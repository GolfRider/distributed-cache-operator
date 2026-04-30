# distributed-cache-operator

A Kubernetes operator that manages sharded, in-memory cache clusters with
client-side consistent hashing. The operator is the source of truth for ring
topology and coordinates graceful scale-down and deletion.

---

At Fanatics I built and operated the in-memory resolution/decision platform
underpinning 10+ production applications across 1,000+ retail sites —
content resolution, A/B experimentation with PlanOut namespace support,
and feature flags merged into one hot-path service, ~100M+ requests per
day on a 15-node Go cluster. Each request walked nested in-memory data
structures — not a flat lookup but a computed resolution across content
envelopes, with ranking, scheduling, overriding, filtering, and
label-matching semantics analogous to Kubernetes resource selection.
Publications were generated from upstream databases as snapshot files,
pushed to S3, signaled through a Consul watch, and downloaded by every
node; a Consul-elected leader then choreographed an atomic version flip
across the cluster so all nodes promoted to the new publication within
milliseconds of each other. We added delta publication later, so
refreshes shipped only the changed records — pushing publication
frequency into the hundreds per day without latency cost. It was
strongly consistent, highly available, and deeply data-plane:
replication, leader-elected version choreography, snapshot/delta
layering, multi-publication isolation, ranking and experiment-assignment
logic, all inside the service process.

Where I engineered coordination directly into the data plane at Fanatics — Consul-driven leader election, atomic version promotion across the cluster
— this project engineers coordination out of the data plane and into a Kubernetes-native operator.
Same problem space — many nodes, shared membership, controlled transitions — opposite design choice. 
Both are correct for their constraints; the contrast is the point.

---

## What it does

A `DistributedCache` custom resource describes a sharded cache cluster. The
operator reconciles three owned resources for it:

- A **headless Service** giving pods stable per-pod DNS records.
- A **StatefulSet** running `tiny-cache` (a small Go binary; LRU + TTL,
  HTTP API, peer-unaware) with the requested replicas, image, and memory.
- A **ring ConfigMap** that publishes membership: an ordered, monotonically
  versioned JSON list of pod names that clients consume to compute consistent
  hash positions.

The operator also enforces a **drain contract** on scale-down and deletion:
pods are removed from the published ring before they are terminated, with a
configurable grace period so clients (polling on a fixed interval) converge on
the new topology before any pod actually goes away.

The **data plane** is intentionally **peer-unaware** — there's no replication, no
gossip, no rebalancing. Coordination lives entirely in the operator and the
ring ConfigMap. Cache pods are workers; the cluster exists in the operator's
view, the API server's storage, and the clients' compute.

## CRD shape

```yaml
apiVersion: cache.sk1.services.com/v1alpha1
kind: DistributedCache
metadata:
  name: orders-cache
spec:
  replicas: 3
  image: tiny-cache:dev
  memoryPerPod: 128Mi
  drain:
    gracePeriodSeconds: 30
  tenant:
    isolate: false
```

`kubectl get` output is configured via printer columns:

```
NAME           AVAILABLE   REPLICAS   RING   AGE
orders-cache   True        3          7      4h12m
```

Status carries four conditions: `Available`, `Progressing`, `Degraded`,
`TenantIsolated`. Reasons are stable, machine-readable strings (`Reconciled`,
`InsufficientReplicas`, `RingNotPublished`, `StatefulSetReconcileFailed`,
etc.) suitable for monitoring keys.

## 60-second getting started

Requirements: Docker, kind, kubectl, Go 1.24+, kubebuilder.

```bash
make kind-up             # create kind cluster, install CRD, build & load images
make run                 # in one terminal, runs the operator from your laptop
kubectl apply -f config/samples/cache_v1alpha1_distributedcache.yaml
```

Within ~10 seconds you should see:

```bash
kubectl get dc
# NAME           AVAILABLE   REPLICAS   RING   AGE
# orders-cache   True        3          1      11s

kubectl get pods -l app.kubernetes.io/instance=orders-cache
# orders-cache-0   1/1   Running   0   11s
# orders-cache-1   1/1   Running   0   11s
# orders-cache-2   1/1   Running   0   11s

kubectl get cm orders-cache-ring -o jsonpath='{.data.ring\.json}{"\n"}'
# {"version":1,"members":["orders-cache-0","orders-cache-1","orders-cache-2"]}
```

To run a reference client inside the cluster and see consistent-hash routing:

```bash
kubectl apply -f config/samples/cli-job.yaml
kubectl wait --for=condition=complete --timeout=60s job/cache-cli-demo
kubectl logs job/cache-cli-demo
# ring version: 1
# service FQDN: orders-cache.default.svc.cluster.local
# sample lookups:
#   foo             → orders-cache-0.orders-cache.default.svc.cluster.local
#   bar             → orders-cache-1.orders-cache.default.svc.cluster.local
#   baz             → orders-cache-2.orders-cache.default.svc.cluster.local
#   user-1234       → orders-cache-2.orders-cache.default.svc.cluster.local
#   session-abcd    → orders-cache-2.orders-cache.default.svc.cluster.local
#   orders-99       → orders-cache-1.orders-cache.default.svc.cluster.local
```

To tear down: `make kind-down`.

## Architecture

Three communicating components, none of which talk to each other directly:

```
                  ┌──────────────────────┐
                  │      API server      │
                  │       (+ etcd)       │
                  └──────────────────────┘
                    ▲                  ▲
        watch+write │                  │ watch+read
                    │                  │
         ┌──────────┴───────┐    ┌─────┴─────────┐
         │  control plane   │    │  data plane   │
         │   (operator)     │    │ (cache pods,  │
         │                  │    │   clients)    │
         └──────────────────┘    └───────────────┘
```

The operator watches `DistributedCache`, owned `Service`, owned `StatefulSet`,
and the ring `ConfigMap`. It writes ring membership to the ConfigMap and
status conditions back to the CR. Clients watch (or poll) the ring ConfigMap
and route requests directly to the pods named in it. **Operator outage
freezes ring updates but does not interrupt the data plane** — clients keep
using the last ring they saw.

See [DESIGN.md](DESIGN.md) for the full design rationale, including the ring
versioning rules, drain timing contract, and an explicit non-goals list.

## Operations

A few common scenarios:

**Scale up.** Edit `spec.replicas`. New pods come up; once `PodReady=True`,
they appear in the ring at version+1. Existing keys may relocate to the new
pod (cache miss, no error). The transition window is one client poll
interval.

**Scale down.** Edit `spec.replicas`. Pods scheduled for removal are
annotated with `cache.sk1.services.com/draining-since` and immediately
excluded from the ring (version+1). The StatefulSet is held at its current
size until `drain.gracePeriodSeconds` elapses; then the doomed pods are
terminated. Clients converge on the new ring during the grace window, so no
traffic is routed to terminating pods.

**Pod crash.** Pod readiness flips to False; the StatefulSet's status updates;
the operator reconciles; the ring is republished without that pod. Clients
converge on the next poll. A brief window of cache-miss for keys that hashed
to the dead pod; no errors expected.

**Operator crash or restart.** No effect on the data plane. The ring
ConfigMap stays at its last published version. Pods continue serving traffic.
On restart, the operator re-reads the world and resumes reconciling.

**CR deletion.** The drain finalizer holds CR deletion until all pods have
been drained for `drain.gracePeriodSeconds`. Then the finalizer is removed
and Kubernetes garbage-collects the StatefulSet, Service, ring ConfigMap, and
all pods.

## Non-goals

Explicitly out of scope for v1alpha1, with one-line redirects:

- **Multi-region / cross-cluster failover** — separate concern; for that, look
  at solutions like CockroachDB or stateful service meshes that operate
  across clusters.
- **Replication of cache contents** — if you need HA of hot keys, your data
  plane needs replication; this is the Fanatics design and a different project.
- **Persistence** — it's a cache. If you need durability, use a database.
- **Rebalancing on scale events** — orphans self-limit via TTL + LRU + pod
  churn. If exact-fit ownership matters, you want a system with explicit
  shard ownership semantics (Cassandra, CockroachDB).
- **Out-of-cluster clients** — pod FQDNs only resolve in-cluster. An external
  proxy could be added but is intentionally not part of this surface.
- **Admission webhooks** — CRD validation markers are sufficient for the
  invariants we care about.
- **HPA / KEDA integration** — a layered concern; a future controller could
  watch ring metrics and edit `spec.replicas`.
- **CRD version conversion** — only `v1alpha1` is shipped; we do not commit
  to API stability or migration support.
- **PodDisruptionBudget** — natural extension; not done because the drain
  finalizer covers the same intent for scale events, and node-disruption
  events would require additional logic.
- **Dashboards / UI** — `kubectl describe`, the printer columns, and the
  Kubernetes events from the operator are the supported observability
  surface.

## Project layout

```
api/v1alpha1/                CRD types and generated deepcopy
cmd/
  manager/                   operator binary
  tiny-cache/                cache binary
  cache-cli/                 reference CLI
internal/
  controller/                reconciler, ring math, drain logic
  cache/                     tiny-cache server
  cacheclient/               reference client library
config/
  crd/bases/                 generated CRD YAML
  rbac/                      generated RBAC YAML
  samples/                   example CRs and Jobs
Dockerfile                   operator image
Dockerfile.tiny-cache        tiny-cache image
Dockerfile.cache-cli         cache-cli image
DESIGN.md                    design rationale and non-goals
```

## Development

```bash
make help                    # list all targets
make manifests               # regenerate CRD YAML from Go markers
make generate                # regenerate deepcopy code
make build                   # build the operator binary
make run                     # run the operator from your laptop against the current kubectl context
make docker-build-tiny-cache # build the tiny-cache image
make kind-load-tiny-cache    # build & load tiny-cache into the kind cluster
make kind-up                 # bring up a fresh kind cluster end-to-end
make kind-down               # delete the kind cluster
```

To run unit tests:

```bash
go test ./...
```

To regenerate everything after changing CRD types:

```bash
make generate manifests
go build ./...
```

## What this project is not

A production cache. It is a portfolio artifact demonstrating a control-plane
design — the kind of operator that wraps a peer-unaware, symmetric data
plane in a Kubernetes-native API, with a deliberate split between
coordination (operator) and execution (pods). The data plane is small on
purpose; coordination has been moved out of it entirely. Reading the
operator's reconciler and the design document is the point of the repo,
more than running the cache itself.