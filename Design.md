# DESIGN.md

This document records the design decisions behind
`distributed-cache-operator`. It's written incrementally, as decisions are
made — not retrofitted after the fact. 

---

## 1. API shape: spec, status, and the conditions pattern

The `DistributedCache` CRD separates user intent (`spec`) from observed
reality (`status`) strictly. The controller is the only writer of `status`;
users never edit it. Status is treated as a cache of observations about
cluster state — if the controller pod is replaced, its successor must be
able to rebuild status from the world, not from persisted controller memory.
This is why no "scheduling state" or "in-progress operation" lives in
status: anything the controller needs to remember across reconciles is
either derivable from cluster state or stored in a ConfigMap owned by the CR (custom-resource).

### Spec

The spec exposes five fields, grouped to keep the API stable as it grows:

- `replicas` — pointer to `int32`, defaulted to 3, minimum 1. A pointer is
  used so "unset" is distinguishable from "zero"; the minimum is 1 because
  a zero-replica cache is not a meaningful state for this operator (clients
  polling the ring would see an empty member list and fail every lookup),
  and allowing it invites footguns without a real use case.
- `image` — required string. No default. A cache operator that silently
  defaults to some image version is a supply-chain hazard; if the user
  doesn't specify an image, the CR should fail validation at admission.
- `memoryPerPod` — `resource.Quantity`. The standard Kubernetes type for
  `128Mi`, `1Gi`, etc. Maps directly to the pod's memory limit. CPU is
  deliberately not exposed: caches are memory-bound, and adding CPU tuning
  invites bikeshedding without solving a real problem at v1alpha1.
- `drain` — nested struct (`DrainSpec`), currently holding only
  `gracePeriodSeconds`. Nested even with one field because "drain behavior"
  is a concept that will grow; flat naming (`drainGracePeriodSeconds`,
  `drainStrategy`, `drainMaxUnavailable`) accumulates warts and forces
  breaking renames later. Nesting costs nothing today and compounds well.
- `tenant` — optional nested struct (`TenantSpec`), currently holding only
  `isolate`. Same rationale as `drain`. When `isolate: true`, the operator
  owns a namespace, `ResourceQuota`, and `NetworkPolicy` scoped to the CR.
  See section "Tenant isolation" (forthcoming) for semantics.

Fields deliberately not exposed in v1alpha1: `serviceAccount`,
`nodeSelector`, `tolerations`, `affinity`, arbitrary pod template overrides.
These are the long tail of pod customization; exposing them would widen the
API surface without serving the core control-plane story. A future version
would add a `podTemplate` subset or a `PodTemplateSpec` passthrough. See
"Non-goals" (forthcoming).

### Status

Status carries four conditions and a small set of observational fields.
The conditions pattern (`[]metav1.Condition`) is the Kubernetes-wide
standard for resource state; inventing parallel boolean fields (`isReady`,
`isScaling`) fragments the signal and defeats generic tooling.

The four conditions are:

- `Ready` — "can clients use this cache right now?" True only when all
  replicas are Ready and the ring ConfigMap reflects current membership.
- `Progressing` — "is the operator actively changing things?" True during
  scale events, image rolls, or initial rollout. Kept separate from `Ready`
  deliberately: `Ready=True, Progressing=True` means "still healthy but
  rolling through a change," which is a meaningfully different state from
  "steady and good" (`Ready=True, Progressing=False`). Collapsing the two
  loses that distinction and makes CI gating ("wait until the change has
  landed") awkward.
- `Degraded` — "is something stuck that needs human attention?" The
  distinction vs `Ready=False` is persistence: a pod restarting is
  `Ready=False, Degraded=False` (transient, self-heals); a ResourceQuota
  rejection is `Ready=False, Degraded=True` (stuck, page someone). This is
  the condition that wires to alerting.
- `TenantIsolated` — "is tenant isolation actually in effect?" Distinguishes
  `NotRequested` (user didn't ask), `NotImplemented` (API accepted but
  guardrail not wired), and `Degraded` (asked, but a dependency failed).
  Keeping the field in the CRD even in degraded states is deliberate: it
  documents the contract honestly rather than silently dropping it.

The observational fields are intentionally thin:

- `observedGeneration` — records which `metadata.generation` the current
  status reflects. Without this, CI can't distinguish "controller hasn't
  processed my change yet" from "controller processed it and decided
  nothing needed doing." Non-negotiable for any serious operator.
- `readyReplicas` — count of pods currently Ready and in the ring.
- `ring` — the currently published ring (version, ordered members,
  ConfigMap name). `members` is `[]string` rather than a richer struct
  because the authoritative ring is the ConfigMap, consumed by machines;
  the status ring is for humans typing `kubectl describe`, and humans
  want a compact list. Keeping status thin is a principle — it's a
  summary, not a mirror.

### Printer columns

Four printer columns (`Ready`, `Replicas`, `Ring`, `Age`) are registered
via `+kubebuilder:printcolumn` markers so `kubectl get distributedcache`
returns something meaningful by default, rather than just name and age.
This is a small polish that signals operational familiarity.


---

## 2. Reconciler model and the headless Service

### Level-triggered reconciliation

The reconciler is level-triggered, not edge-triggered. It is never told what
*changed*; it is told the namespaced name of a CR and is expected to observe
the world from scratch and make one step's worth of progress, idempotently.
This is non-negotiable for two reasons. First, it is self-healing: a crash
or restart costs nothing because the next reconcile reads fresh state and
continues from observation. Second, it composes — multiple reconcile triggers
(spec edit, owned-resource event, periodic resync) collapse to "go look at
the world again," which is the same code path. Every reconcile must therefore
be safe to run any number of times against the same state.

In practice this discipline shows up in three places: `controllerutil.CreateOrUpdate`
for owned resources (read-mutate-write atomically, no separate create vs
update branch), `meta.SetStatusCondition` for conditions (no-op when nothing
changed, including LastTransitionTime), and the no-cleanup-code rule for
owned resources (Kubernetes garbage collection via owner references handles
it).

### The headless Service

The first owned resource is a headless Service (`clusterIP: None`). A normal
Service load-balances traffic across its endpoints; a headless Service
returns per-pod DNS records (`cache-0.<svc>.<ns>.svc.cluster.local`) and
performs no balancing. Because clients use consistent hashing to route to
specific pods, load-balancing would defeat the design — we *want* the client
addressing pods individually. The Service is therefore a pure DNS-and-identity
primitive, not a traffic primitive.

`PublishNotReadyAddresses` is set to false, deliberately. When the operator
flips a pod to NotReady during drain, that pod's DNS record disappears
within seconds. Clients (after their next ring poll) will already have
removed the pod from their consistent hash; the DNS-level removal is a
belt-and-suspenders signal that the pod is no longer a valid target. See
section 3 ("Drain and the ring-staleness contract", forthcoming) for how
this composes with the operator-published ring.

### Labels, selectors, and the immutability rule

Owned resources carry the four recommended `app.kubernetes.io/*` labels
(name, instance, managed-by, component). These are identity stamps used by
generic Kubernetes tooling — `kubectl get -l`, Prometheus relabeling,
GitOps tools — and following the convention buys ecosystem integration for
free. The reconciler asserts these labels every reconcile via `CreateOrUpdate`'s
mutate function, so manual edits or other controllers' label drift heal
automatically.

The Service's `spec.selector` is intentionally narrower than the Service's
own labels: only `app.kubernetes.io/name` and `app.kubernetes.io/instance`
are used. Selectors are immutable on Services, and including a label that
might evolve over the cluster's lifetime (`version`, `component`) would
either force selector drift or force a Service replacement during otherwise
routine changes. The selector's contract is "the pods this Service routes
to, forever" — so the labels in it must be ones that never change for
the lifetime of the cache cluster.

### Owner references and watches

Every owned resource is created with a controller reference pointing at the
parent CR (`controllerutil.SetControllerReference`). Two consequences. First,
deletion is automatic: when the CR is deleted, Kubernetes' garbage collector
walks owner references and deletes everything stamped as owned, with no
operator code involved. Second, the reverse-watch pattern: `Owns(&corev1.Service{})`
in `SetupWithManager` registers a watch on Services, and any event on a
Service whose owner reference points at a DistributedCache re-enqueues the
*parent* CR for reconciliation. This is the mechanism that turns "manual
delete of an owned resource" into "operator recreates it within seconds"
— the watch graph plus level-triggered reconciliation gives self-healing
without polling.

### Failure as a status condition, not a log line

When an owned-resource reconcile fails, the reconciler sets `Degraded=True`
with a specific reason (e.g. `ServiceReconcileFailed`), best-effort updates
status, and returns the original error so controller-runtime's exponential
backoff retries. This is the discipline that separates an operator that's
actually operable from one that requires log-spelunking to debug: every
failure mode must surface in `kubectl describe` with a stable reason that
monitoring can match on. Log lines are for operators of the operator;
conditions are for users of the CR.


---

## 3. Known operational wart: stuck StatefulSet rollouts

A StatefulSet that has never had any Ready pod cannot complete a
RollingUpdate. The default update strategy waits for each pod to become
Ready before terminating the next one for replacement; if the pods are
crash-looping (e.g. an `ImagePullBackOff` left over from a misconfigured
image), the rollout never advances. The operator has correctly updated the
StatefulSet's pod template, but the StatefulSet refuses to act on it.

The recovery is `kubectl delete pod -l <selector>`: removing the broken
pods lets the StatefulSet create fresh ones from the current template.
This is a property of StatefulSet's update controller, not of this
operator, and it applies to every operator built on StatefulSets. We
considered adding a force-update path that deletes stuck pods automatically
once a Degraded condition has persisted past a threshold, but rejected it
for v1alpha1: the heuristic is hard to get right (when is "stuck" really
stuck vs. recovering?), the failure mode is rare in healthy environments,
and the manual recovery is one command. Documented here so an operator
running this for the first time recognizes the symptom.


---

## 4. The ring: authority, propagation, staleness

Ring topology is owned by the operator and published as a ConfigMap that
clients poll. The operator computes membership by listing pods owned by the
CR, filtering to those that are `PodRunning` with `PodReady=True` and have
no deletion timestamp, sorting by StatefulSet ordinal, and writing the
result as JSON to `<cr-name>-ring`. The ConfigMap is the source of truth
clients consume; the CR's `status.ring` mirrors it for human inspection
via `kubectl describe`.

### Versioning

Each ring carries a monotonically increasing `version`. The version comes
from the existing ConfigMap (read at the start of every reconcile), not
from CR generation: pod readiness changes do not bump CR generation, so
generation would lag membership changes. The bump rule is simple — if
desired members differ from the current members in the ConfigMap, the new
version is `current + 1`; otherwise the version is preserved and the write
is skipped entirely.

This monotonicity is preserved across reconciles, controller restarts, and
optimistic-concurrency conflicts on the ConfigMap (the `resourceVersion`
CAS guarantees only one writer wins per round; the loser re-reads and
re-derives, never producing a regression).

The one case where the version can regress is a corrupt or hand-edited
ConfigMap whose JSON fails to parse. The reconciler treats this as
"rebuild from scratch" rather than "refuse to publish": it is strictly
better for the system to heal than to wedge. A production-grade design
would surface the corruption as a `RingPayloadCorrupt` Degraded condition;
we have not done so to keep v1alpha1 narrow.

### Why ConfigMap and not status

Status is a summary view for humans and CI; it is not designed for the
high-frequency machine reads a sharded cache fleet generates. ConfigMap is
purpose-built for "small structured data clients consume" — it has its own
RBAC envelope, can be mounted as a volume for zero-API-server-load polling,
and its updates produce watch events that are independent of CR events.
Splitting the ring into a dedicated ConfigMap also means a future read-only
client component can be granted RBAC for one ConfigMap rather than the
entire CR including status.

### Propagation: polling, not watching

Clients poll the ConfigMap on a fixed interval (10s in our reference
client). Watching it would be more efficient for change detection, but
polling has two advantages for this design: it is simpler to implement
correctly, and the staleness window it produces (≤ poll interval) becomes
a *known constant* the drain logic can reason about. The operator's
`drain.gracePeriodSeconds` defaults to 30s — three times the poll interval
— precisely so that a pod marked NotReady has time to be observed as
removed by every client before termination. Watching would tighten the
window but eliminate the predictability that makes drain timing safe to
reason about.

### Pod watch is intentionally not registered

`SetupWithManager` registers `Owns(&Service{})` and `Owns(&StatefulSet{})`
but does not watch Pods directly. Pod readiness changes propagate to the
reconciler indirectly: the StatefulSet's status updates when a pod becomes
Ready, our `Owns(&StatefulSet{})` watch fires, and the parent CR is
reconciled. Sub-second latency in practice. The alternative — watching
Pods directly via `Watches(...)` with a custom mapper — is more code and
more watch budget for a marginal latency improvement. Documented as
intentional rather than missing.


---

## 5. Drain and the ring-staleness contract

The operator coordinates pod removal with client ring updates so that no
client routes traffic to a pod that's actively being terminated. This is
the most non-trivial control-plane behavior in the project and the one
that justifies an operator over a hand-rolled StatefulSet + Service.

### The asymmetry: scale-up vs scale-down

Scale-up needs no orchestration. The `computeMembers` filter excludes pods
that aren't yet `PodReady`, so a newly-added pod simply doesn't appear in
the ring until it can serve traffic. There's a window during which the
pod exists but isn't yet a member; clients hash to existing pods only.
Harmless: cache-miss-but-no-error.

Scale-down is timing-critical. Without coordination, the StatefulSet
would terminate pods at t=0s while clients still held the previous ring
for up to one poll interval (10s). During that window, traffic targets
dying pods and clients see connection errors instead of misses. Drain
exists to invert this order: publish the new ring **before** terminating
the pods, observe a grace period that exceeds the client poll interval
(30s default; 3× the 10s poll), then proceed with termination.

### Mechanism

A pod is excluded from the ring as soon as it carries the annotation
`cache.sk1.services.com/draining-since=<unix-ts>`. The annotation is the
entire signal: `computeMembers` filters on it, and once filtered, the
operator republishes the ring without that member. The pod is still
running, still in the Service's endpoint list, still receiving traffic
from clients on the old ring — but the published ring already reflects
its absence. After clients converge (≤ poll interval), no traffic
targets the pod even though it's still alive.

The annotation is preferable to flipping a label that the Service selects
on. Removing a pod from the Service's selector would cause the
StatefulSet to immediately recreate the pod (a missing labeled pod is
something the StatefulSet considers its responsibility to fix), creating
a fight between the operator and the StatefulSet controller. Annotations
don't participate in selectors and don't conflict.

### Two trigger paths, one primitive

Drain runs in two situations: scale-down (`spec.replicas` decreased) and
deletion (`metadata.deletionTimestamp` set). Both flows share the same
primitive — annotate the doomed pods, wait grace, proceed — implemented
as two methods (`drainScaleDown` and `reconcileDeletion`) that consume
the same `computeMembers` filter and emit the same `RingPublished`
events.

Scale-down doesn't actually delete any pods directly; it holds the
StatefulSet at its current replica count until grace elapses, then
allows the StatefulSet's normal scale-down logic to terminate the now-
silent pods. Deletion goes further: after the wait, it removes the
finalizer, allowing the CR to be GC'd along with all owned resources.

This shared primitive also covers a third case automatically: image
rolls. When the StatefulSet replaces a pod for an image update, the
replaced pod's `DeletionTimestamp` is set, which the same
`computeMembers` filter respects. The pod leaves the ring before it
terminates without any image-roll-specific code.

### Timing

The default `drain.gracePeriodSeconds` is 30. The reasoning:

  - Reference client poll interval: 10s (locked design).
  - 30s = 3× poll interval, allowing 2 missed polls before drain
    completes — defense against transient client GC pauses,
    network blips, or controller scheduling delays.
  - StatefulSet `terminationGracePeriodSeconds` is set to
    `drain.gracePeriodSeconds + 10` so the kubelet's hard-kill
    deadline is well after the operator-coordinated drain ends.
    The +10 absorbs the cache pod's own graceful-shutdown deadline.

Reconcile rate during drain is set by `RequeueAfter: 2*time.Second`
because no event will wake the reconciler during the wait — pods aren't
changing, ring isn't changing, spec isn't changing. The operator must
schedule its own re-entry. 2s gives ~15 checks across a 30s grace
window, more than enough to detect completion promptly without
thrashing the API server.

### What this contract gives clients

A client that polls the ring every 10s and respects published membership
strictly is guaranteed not to send traffic to a pod that's actively
terminating, provided the operator's drain succeeded. A client that's
slow, paused, or partitioned for >30s will eventually route to a missing
pod and see connection-refused — but that case is no worse than what
happens when any cache pod fails for any other reason, and is bounded
by client-side retry behavior.

The contract is strictly "no error during *coordinated* removal." It
does not promise anything about pods that crash, are evicted, or are
killed by node failures. Those flow through the same readiness signal
(`/readyz` returning 503 → `PodReady=False` → ring exclusion) but with
no grace window — clients see a brief window of errors before the next
ring publish picks up the change. Acceptable because (a) those events
are rare relative to scheduled drains, and (b) cache-is-a-hint
semantics absorb transient errors.

