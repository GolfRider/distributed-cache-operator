/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cachev1alpha1 "github.com/GolfRider/distributed-cache-operator/api/v1alpha1"
)

// Condition types reported by the DistributedCache reconciler.
// Exported so tests, clients, and humans can reference the same constants.
const (
	ConditionAvailable      = "Available"
	ConditionProgressing    = "Progressing"
	ConditionDegraded       = "Degraded"
	ConditionTenantIsolated = "TenantIsolated"
)

// Condition reasons. Treat these as part of the API contract: monitoring and
// alerting will key on these strings, so renames are breaking changes.
const (
	ReasonReconciled           = "Reconciled"
	ReasonNoReadyPods          = "NoReadyPods"
	ReasonInsufficientReplicas = "InsufficientReplicas"
	ReasonRingNotPublished     = "RingNotPublished"
	ReasonReplicasConverging   = "ReplicasConverging"
	ReasonNotRequested         = "NotRequested"
	ReasonNotImplemented       = "NotImplemented"
	ReasonServiceFailed        = "ServiceReconcileFailed"
	ReasonStatefulSetFailed    = "StatefulSetReconcileFailed"
	ReasonRingFailed           = "RingReconcileFailed"
)

// Standard recommended labels propagated to every owned resource.
// See https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
const (
	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelComponent = "app.kubernetes.io/component"
	appName        = "distributed-cache"
	managedByValue = "distributed-cache-operator"
)

// Cache pod HTTP port. The cache binary listens here for kv operations,
// /readyz, /healthz, and /metrics.
const cachePort int32 = 8080

// DistributedCacheReconciler reconciles a DistributedCache object.
//
// The reconciler is the source of truth for ring topology: it observes pod
// readiness, computes membership, and publishes the ring as a ConfigMap that
// clients poll. See DESIGN.md for the broader control-plane story.
type DistributedCacheReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=cache.sk1.services.com,resources=distributedcaches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cache.sk1.services.com,resources=distributedcaches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cache.sk1.services.com,resources=distributedcaches/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives a DistributedCache toward its desired state. It is
// level-triggered: every invocation re-observes the world from scratch and
// makes one step's worth of progress, idempotently.
// Reconcile drives a DistributedCache toward its desired state.
func (r *DistributedCacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Fetch the CR.
	var cache cachev1alpha1.DistributedCache
	if err := r.Get(ctx, req.NamespacedName, &cache); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("DistributedCache not found; assuming deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get DistributedCache: %w", err)
	}

	log.Info("reconciling",
		"generation", cache.Generation,
		"replicas", cache.Spec.Replicas,
		"image", cache.Spec.Image,
	)

	// 2. Reconcile owned resources in dependency order: Service first
	//    (StatefulSet's serviceName references it), then StatefulSet
	//    (creates pods), then ring ConfigMap (depends on pod readiness).
	if err := r.reconcileService(ctx, &cache); err != nil {
		r.markDegraded(&cache, ReasonServiceFailed, err)
		_ = r.Status().Update(ctx, &cache)
		return ctrl.Result{}, fmt.Errorf("reconcile service: %w", err)
	}

	if err := r.reconcileStatefulSet(ctx, &cache); err != nil {
		r.markDegraded(&cache, ReasonStatefulSetFailed, err)
		_ = r.Status().Update(ctx, &cache)
		return ctrl.Result{}, fmt.Errorf("reconcile statefulset: %w", err)
	}

	ring, err := r.reconcileRing(ctx, &cache)
	if err != nil {
		r.markDegraded(&cache, ReasonRingFailed, err)
		_ = r.Status().Update(ctx, &cache)
		return ctrl.Result{}, fmt.Errorf("reconcile ring: %w", err)
	}

	// 3. Mirror ring into status. Status is a summary view for humans;
	//    the authoritative ring lives in the ConfigMap.
	cache.Status.Ring = cachev1alpha1.RingStatus{
		Version:       ring.Version,
		Members:       ring.Members,
		ConfigMapName: ringConfigMapName(&cache),
	}
	cache.Status.ReadyReplicas = int32(len(ring.Members))
	cache.Status.ObservedGeneration = cache.Generation

	// 4. Compute conditions from observed state.
	r.setObservedConditions(&cache, ring)

	// 5. Persist status.
	if err := r.Status().Update(ctx, &cache); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	return ctrl.Result{}, nil
}

// markDegraded sets the Degraded condition with the given reason and the
// error's message. Used in error paths to make failures visible in
// `kubectl describe` rather than only in operator logs.
func (r *DistributedCacheReconciler) markDegraded(cache *cachev1alpha1.DistributedCache, reason string, err error) {
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type:               ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            err.Error(),
		ObservedGeneration: cache.Generation,
	})
}

// podLabels returns the labels that uniquely identify pods for a given
// DistributedCache. Used both as labels stamped on pods (via the StatefulSet
// pod template) and as the selector on the Service. Matching on both sides
// is what makes the Service's DNS resolution find the pods.
func podLabels(cache *cachev1alpha1.DistributedCache) map[string]string {
	return map[string]string{
		labelName:     appName,
		labelInstance: cache.Name,
	}
}

// reconcileService ensures a headless Service exists for this cache.
// See DESIGN.md §2 for the headless-Service rationale.
func (r *DistributedCacheReconciler) reconcileService(ctx context.Context, cache *cachev1alpha1.DistributedCache) error {
	log := logf.FromContext(ctx)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cache.Name,
			Namespace: cache.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels[labelName] = appName
		svc.Labels[labelInstance] = cache.Name
		svc.Labels[labelManagedBy] = managedByValue
		svc.Labels[labelComponent] = "cache"

		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.Selector = podLabels(cache)
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       cachePort,
			TargetPort: intstr.FromInt32(cachePort),
			Protocol:   corev1.ProtocolTCP,
		}}
		svc.Spec.PublishNotReadyAddresses = false

		return controllerutil.SetControllerReference(cache, svc, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("CreateOrUpdate Service: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		log.Info("reconciled headless Service", "operation", op, "name", svc.Name)
		r.Recorder.Eventf(cache, corev1.EventTypeNormal, "ServiceReconciled",
			"Headless Service %s/%s %s", svc.Namespace, svc.Name, op)
	}

	return nil
}

// reconcileStatefulSet ensures a StatefulSet exists for this cache, with
// the right replicas, image, resources, and probes. Each pod gets a stable
// DNS name (cache-0.<svc>, cache-1.<svc>, ...) which the consistent-hash
// ring math depends on.
func (r *DistributedCacheReconciler) reconcileStatefulSet(ctx context.Context, cache *cachev1alpha1.DistributedCache) error {
	log := logf.FromContext(ctx)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cache.Name,
			Namespace: cache.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		// Labels on the StatefulSet itself.
		if sts.Labels == nil {
			sts.Labels = map[string]string{}
		}
		sts.Labels[labelName] = appName
		sts.Labels[labelInstance] = cache.Name
		sts.Labels[labelManagedBy] = managedByValue
		sts.Labels[labelComponent] = "cache"

		// Replicas, mutable.
		sts.Spec.Replicas = cache.Spec.Replicas

		// Immutable fields: only set on creation. The API server rejects
		// updates that change selector, serviceName, or volumeClaimTemplates,
		// so guarding by CreationTimestamp prevents our reconciler from
		// generating unfixable Update errors after the StatefulSet exists.
		if sts.CreationTimestamp.IsZero() {
			sts.Spec.ServiceName = cache.Name // pairs with the headless Service
			sts.Spec.Selector = &metav1.LabelSelector{
				MatchLabels: podLabels(cache),
			}
			sts.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
			// OrderedReady is the default; Parallel lets pods come up
			// concurrently. For a stateless cache there's no startup
			// ordering requirement, and Parallel makes scale-up faster.
		}

		// Pod template — mutable; updates trigger a rolling restart.
		sts.Spec.Template.Labels = podLabels(cache)

		// TerminationGracePeriodSeconds: must give the operator enough time
		// to publish a ring removal *and* the pod enough time to finish
		// in-flight requests. The drain logic waits drain.gracePeriodSeconds
		// after marking the pod NotReady; we add 10s of headroom for the
		// cache's own graceful shutdown.
		grace := int64(cache.Spec.Drain.GracePeriodSeconds) + 10
		sts.Spec.Template.Spec.TerminationGracePeriodSeconds = &grace

		// Container.
		container := corev1.Container{
			Name:  "cache",
			Image: cache.Spec.Image,
			Ports: []corev1.ContainerPort{{
				Name:          "http",
				ContainerPort: cachePort,
				Protocol:      corev1.ProtocolTCP,
			}},
			Resources: corev1.ResourceRequirements{
				// Requests == Limits: predictable Guaranteed QoS class.
				// The cache is memory-bound; we don't pin CPU on purpose
				// (see DESIGN.md §1 on why CPU isn't in the spec).
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: cache.Spec.MemoryPerPod,
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: cache.Spec.MemoryPerPod,
				},
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/readyz",
						Port: intstr.FromInt32(cachePort),
					},
				},
				PeriodSeconds:    2,
				TimeoutSeconds:   1,
				FailureThreshold: 3,
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/healthz",
						Port: intstr.FromInt32(cachePort),
					},
				},
				PeriodSeconds:    10,
				TimeoutSeconds:   2,
				FailureThreshold: 3,
			},
		}
		sts.Spec.Template.Spec.Containers = []corev1.Container{container}

		return controllerutil.SetControllerReference(cache, sts, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("CreateOrUpdate StatefulSet: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		log.Info("reconciled StatefulSet", "operation", op, "name", sts.Name)
		r.Recorder.Eventf(cache, corev1.EventTypeNormal, "StatefulSetReconciled",
			"StatefulSet %s/%s %s", sts.Namespace, sts.Name, op)
	}

	return nil
}

// ringConfigMapName returns the name of the ConfigMap that holds the
// authoritative ring for this cache. We append "-ring" to the CR name so
// the ConfigMap is distinguishable from the cache's own resources (Service
// and StatefulSet share the CR name); a quick `kubectl get cm` shows
// at a glance which ConfigMaps are operator-managed.
func ringConfigMapName(cache *cachev1alpha1.DistributedCache) string {
	return cache.Name + "-ring"
}

const ringConfigMapKey = "ring.json"

// reconcileRing computes the desired ring from current pod state, compares
// against the existing ring ConfigMap, bumps the version on membership
// change, and persists the result. Returns the ring that was published so
// the caller can mirror it into status.
//
// The version is monotonic across reconciles: it comes from the existing
// ConfigMap (the source of truth) and only ever increases. If the
// ConfigMap is missing, version starts at 1 on the first publish.
func (r *DistributedCacheReconciler) reconcileRing(ctx context.Context, cache *cachev1alpha1.DistributedCache) (Ring, error) {
	log := logf.FromContext(ctx)

	// Step 1: list owned pods.
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(cache.Namespace),
		client.MatchingLabels(podLabels(cache)),
	); err != nil {
		return Ring{}, fmt.Errorf("list pods: %w", err)
	}

	// Step 2: compute desired membership from pod state.
	desiredMembers := computeMembers(podList.Items)

	// Step 3: read the existing ring ConfigMap. NotFound is the expected
	// path on first reconcile; treat as an empty ring at version 0.
	current := Ring{Version: 0, Members: nil}
	var cm corev1.ConfigMap
	cmKey := types.NamespacedName{Namespace: cache.Namespace, Name: ringConfigMapName(cache)}
	if err := r.Get(ctx, cmKey, &cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return Ring{}, fmt.Errorf("get ring ConfigMap: %w", err)
		}
		// Not found: leave `current` as the zero ring.
	} else {
		// Found: parse the stored ring. A malformed ConfigMap is treated
		// as version 0 — we'd rather rebuild than refuse to publish.
		if data, ok := cm.Data[ringConfigMapKey]; ok {
			var parsed Ring
			if err := json.Unmarshal([]byte(data), &parsed); err == nil {
				current = parsed
			} else {
				log.Info("ring ConfigMap had unparsable payload; rebuilding", "err", err)
			}
		}
	}

	// Step 4: decide the new version.
	desired := Ring{Members: desiredMembers}
	if desired.MembersEqual(current) {
		desired.Version = current.Version
	} else {
		desired.Version = current.Version + 1
	}

	// Short-circuit: if the existing ConfigMap already encodes the desired
	// ring exactly, skip the write entirely. This is the no-op reconcile
	// path; with status-update events also triggering reconciles, this
	// matters for keeping API server load bounded.
	if desired.Equal(current) && len(cm.Data) > 0 {
		log.V(1).Info("ring unchanged; skipping ConfigMap write",
			"version", desired.Version, "members", len(desired.Members))
		return desired, nil
	}

	// Step 5 + 6: marshal and write.
	payload, err := json.Marshal(desired)
	if err != nil {
		return Ring{}, fmt.Errorf("marshal ring: %w", err)
	}

	cm = corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ringConfigMapName(cache),
			Namespace: cache.Namespace,
		},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, &cm, func() error {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[labelName] = appName
		cm.Labels[labelInstance] = cache.Name
		cm.Labels[labelManagedBy] = managedByValue
		cm.Labels[labelComponent] = "ring"

		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[ringConfigMapKey] = string(payload)

		return controllerutil.SetControllerReference(cache, &cm, r.Scheme)
	})
	if err != nil {
		return Ring{}, fmt.Errorf("CreateOrUpdate ring ConfigMap: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		log.Info("published ring",
			"operation", op,
			"version", desired.Version,
			"members", desired.Members,
		)
		r.Recorder.Eventf(cache, corev1.EventTypeNormal, "RingPublished",
			"Ring %s/%s %s (version=%d, members=%v)",
			cm.Namespace, cm.Name, op, desired.Version, desired.Members)
	}

	return desired, nil
}

// setObservedConditions writes the four conditions based on observed cluster
// state. This replaces the earlier "Initializing" placeholder once the ring
// is being computed; from this point onward, conditions reflect reality.
//
// The matrix:
//
//	Available=True only when ready replicas == desired and ring is published.
//	Progressing=True when a transition is in flight (replica mismatch, or
//	              generation hasn't been observed yet — though we always
//	              update observedGeneration in the same write).
//	Degraded=False on the happy path; callers set it directly via
//	              markDegraded() in error paths and we don't clear it
//	              from those reasons here.
//	TenantIsolated reflects whether the user requested isolation.
func (r *DistributedCacheReconciler) setObservedConditions(cache *cachev1alpha1.DistributedCache, ring Ring) {
	desired := int32(0)
	if cache.Spec.Replicas != nil {
		desired = *cache.Spec.Replicas
	}
	ready := int32(len(ring.Members))
	gen := cache.Generation

	// Available: enough ready pods AND a published ring. Either alone
	// is insufficient — pods Ready with no ring means clients can't
	// route; ring published with too few pods means a real outage.
	availableCond := metav1.Condition{
		Type:               ConditionAvailable,
		ObservedGeneration: gen,
	}
	switch {
	case ready == 0:
		availableCond.Status = metav1.ConditionFalse
		availableCond.Reason = ReasonNoReadyPods
		availableCond.Message = "No cache pods are Ready."
	case ready < desired:
		availableCond.Status = metav1.ConditionFalse
		availableCond.Reason = ReasonInsufficientReplicas
		availableCond.Message = fmt.Sprintf("%d/%d replicas Ready.", ready, desired)
	case ring.Version == 0:
		availableCond.Status = metav1.ConditionFalse
		availableCond.Reason = ReasonRingNotPublished
		availableCond.Message = "Ring ConfigMap has not been published yet."
	default:
		availableCond.Status = metav1.ConditionTrue
		availableCond.Reason = ReasonReconciled
		availableCond.Message = fmt.Sprintf("%d/%d replicas Ready; ring version %d.", ready, desired, ring.Version)
	}
	meta.SetStatusCondition(&cache.Status.Conditions, availableCond)

	// Progressing: True while we're transitioning toward spec.
	progressingCond := metav1.Condition{
		Type:               ConditionProgressing,
		ObservedGeneration: gen,
	}
	if ready != desired {
		progressingCond.Status = metav1.ConditionTrue
		progressingCond.Reason = ReasonReplicasConverging
		progressingCond.Message = fmt.Sprintf("Working toward %d replicas (currently %d Ready).", desired, ready)
	} else {
		progressingCond.Status = metav1.ConditionFalse
		progressingCond.Reason = ReasonReconciled
		progressingCond.Message = "Steady state."
	}
	meta.SetStatusCondition(&cache.Status.Conditions, progressingCond)

	// Degraded: clear it on the happy path (we got here without errors).
	// Error paths set it via markDegraded() before returning.
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type:               ConditionDegraded,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonReconciled,
		Message:            "No degraded conditions observed.",
		ObservedGeneration: gen,
	})

	// TenantIsolated: same logic as before, no change.
	tenantCond := metav1.Condition{
		Type:               ConditionTenantIsolated,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: gen,
	}
	if cache.Spec.Tenant != nil && cache.Spec.Tenant.Isolate {
		tenantCond.Reason = ReasonNotImplemented
		tenantCond.Message = "Tenant isolation accepted by API but not yet wired."
	} else {
		tenantCond.Reason = ReasonNotRequested
		tenantCond.Message = "spec.tenant.isolate is unset or false."
	}
	meta.SetStatusCondition(&cache.Status.Conditions, tenantCond)
}

// SetupWithManager wires the reconciler into the manager. The Owns calls
// register watches on owned resources: any change to a child Service or
// StatefulSet we own re-enqueues the parent CR for reconciliation.
func (r *DistributedCacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("distributedcache-controller")
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.DistributedCache{}).
		Owns(&corev1.Service{}).
		Owns(&appsv1.StatefulSet{}).
		Named("distributedcache").
		Complete(r)
}
