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

