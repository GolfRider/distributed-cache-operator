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
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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
	networkingv1 "k8s.io/api/networking/v1"
)

// Condition types reported by the DistributedCache reconciler.
const (
	ConditionAvailable      = "Available"
	ConditionProgressing    = "Progressing"
	ConditionDegraded       = "Degraded"
	ConditionTenantIsolated = "TenantIsolated"
)

// Condition reasons. Stable strings — part of the API contract.
const (
	ReasonReconciled           = "Reconciled"
	ReasonNoReadyPods          = "NoReadyPods"
	ReasonInsufficientReplicas = "InsufficientReplicas"
	ReasonRingNotPublished     = "RingNotPublished"
	ReasonReplicasConverging   = "ReplicasConverging"
	ReasonNotRequested         = "NotRequested"
	ReasonCellFailed           = "CellReconcileFailed"
	ReasonServiceFailed        = "ServiceReconcileFailed"
	ReasonStatefulSetFailed    = "StatefulSetReconcileFailed"
	ReasonRingFailed           = "RingReconcileFailed"
)

// Recommended Kubernetes labels.
const (
	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelComponent = "app.kubernetes.io/component"
	appName        = "distributed-cache"
	managedByValue = "distributed-cache-operator"
)

const cachePort int32 = 8080

// Cellular design constants.
const (
	cellNamespacePrefix = "cache-"
	drainFinalizer      = "cache.sk1.services.com/drain"
	drainAnnotation     = "cache.sk1.services.com/draining-since"
)

// targetNamespace returns the namespace where this DistributedCache's owned
// resources live. When tenant.isolate is true, the operator manages a
// dedicated cell namespace named "cache-<cr-name>"; otherwise resources
// live alongside the CR.
func targetNamespace(cache *cachev1alpha1.DistributedCache) string {
	if cache.Spec.Tenant != nil && cache.Spec.Tenant.Isolate {
		return cellNamespacePrefix + cache.Name
	}
	return cache.Namespace
}

// isCrossNamespaceChild reports whether owned resources live in a different
// namespace than the parent CR. Cross-namespace ownerReferences are
// forbidden by the API server.
func isCrossNamespaceChild(cache *cachev1alpha1.DistributedCache) bool {
	return targetNamespace(cache) != cache.Namespace
}

type DistributedCacheReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=cache.sk1.services.com,resources=distributedcaches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cache.sk1.services.com,resources=distributedcaches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cache.sk1.services.com,resources=distributedcaches/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *DistributedCacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

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
		"deleting", !cache.DeletionTimestamp.IsZero(),
		"target_ns", targetNamespace(&cache),
	)

	if !cache.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, &cache)
	}

	if controllerutil.AddFinalizer(&cache, drainFinalizer) {
		if err := r.Update(ctx, &cache); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Cell creation must run before owned resources, since they live
	// in the cell namespace.
	if err := r.reconcileCell(ctx, &cache); err != nil {
		r.markDegraded(&cache, ReasonCellFailed, err)
		_ = r.Status().Update(ctx, &cache)
		return ctrl.Result{}, fmt.Errorf("reconcile cell: %w", err)
	}

	if err := r.reconcileService(ctx, &cache); err != nil {
		if err := r.reconcileService(ctx, &cache); err != nil {
			r.markDegraded(&cache, ReasonServiceFailed, err)
			_ = r.Status().Update(ctx, &cache)
			return ctrl.Result{}, fmt.Errorf("reconcile service: %w", err)
		}
	}

	doomedComplete, err := r.drainScaleDown(ctx, &cache)
	if err != nil {
		r.markDegraded(&cache, ReasonStatefulSetFailed, err)
		_ = r.Status().Update(ctx, &cache)
		return ctrl.Result{}, fmt.Errorf("drain scale-down: %w", err)
	}

	if err := r.reconcileStatefulSet(ctx, &cache, doomedComplete); err != nil {
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

	cache.Status.Ring = cachev1alpha1.RingStatus{
		Version:       ring.Version,
		Members:       ring.Members,
		ConfigMapName: ringConfigMapName(&cache),
	}
	cache.Status.ReadyReplicas = int32(len(ring.Members))
	cache.Status.ObservedGeneration = cache.Generation
	r.setObservedConditions(&cache, ring)

	if err := r.Status().Update(ctx, &cache); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	if !doomedComplete {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *DistributedCacheReconciler) markDegraded(
	cache *cachev1alpha1.DistributedCache,
	reason string,
	err error,
) {
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type:               ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            err.Error(),
		ObservedGeneration: cache.Generation,
	})
}

// setControllerReferenceIfSameNamespace sets the controlling owner ref
// only when the child is in the same namespace as the parent CR.
// Cross-namespace ownerRefs are forbidden by the API server.
func (r *DistributedCacheReconciler) setControllerReferenceIfSameNamespace(
	cache *cachev1alpha1.DistributedCache,
	child client.Object,
) error {
	if isCrossNamespaceChild(cache) {
		return nil
	}
	return controllerutil.SetControllerReference(cache, child, r.Scheme)
}

func podLabels(cache *cachev1alpha1.DistributedCache) map[string]string {
	return map[string]string{
		labelName:     appName,
		labelInstance: cache.Name,
	}
}

// reconcileCell ensures the cellular isolation primitives exist when
// tenant.isolate is true. Creates the cell Namespace, ResourceQuota, and
// NetworkPolicy. Idempotent; returns nil and skips when isolation is off.
//
// Order of operations matters: Namespace first (others live in it), then
// quota and netpol in either order. We run them sequentially so a failure
// surfaces with a precise reason.
func (r *DistributedCacheReconciler) reconcileCell(ctx context.Context, cache *cachev1alpha1.DistributedCache) error {
	if cache.Spec.Tenant == nil || !cache.Spec.Tenant.Isolate {
		return nil
	}

	if err := r.reconcileCellNamespace(ctx, cache); err != nil {
		return fmt.Errorf("namespace: %w", err)
	}
	if err := r.reconcileResourceQuota(ctx, cache); err != nil {
		return fmt.Errorf("resourcequota: %w", err)
	}
	if err := r.reconcileNetworkPolicy(ctx, cache); err != nil {
		return fmt.Errorf("networkpolicy: %w", err)
	}
	return nil
}

// reconcileCellNamespace creates the cell namespace if missing. The
// namespace is *not* owned by the CR (cluster-scoped resources can't have
// namespaced owners); the operator's finalizer cleans it up explicitly on
// CR deletion.
//
// Labels on the namespace identify it as operator-managed and link back
// to the CR, which is how the finalizer finds it.
func (r *DistributedCacheReconciler) reconcileCellNamespace(ctx context.Context, cache *cachev1alpha1.DistributedCache) error {
	log := logf.FromContext(ctx)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: targetNamespace(cache),
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, ns, func() error {
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		ns.Labels[labelName] = appName
		ns.Labels[labelInstance] = cache.Name
		ns.Labels[labelManagedBy] = managedByValue
		ns.Labels[labelComponent] = "cell"
		// Cell-origin pointer: which CR (in which namespace) owns this cell.
		// Used by the finalizer cleanup and by anyone trying to trace a
		// cell back to its declaration.
		ns.Labels["cache.sk1.services.com/cell-of"] = cache.Namespace + "." + cache.Name
		return nil
	})
	if err != nil {
		return fmt.Errorf("CreateOrUpdate Namespace: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		log.Info("reconciled cell Namespace", "operation", op, "name", ns.Name)
		r.Recorder.Eventf(cache, corev1.EventTypeNormal, "CellNamespaceReconciled",
			"Cell Namespace %s %s", ns.Name, op)
	}
	return nil
}

// reconcileResourceQuota creates a ResourceQuota in the cell namespace,
// sized from spec.replicas × spec.memoryPerPod plus 25% headroom for
// transient overshoot during scale events. Bounds total pods to spec.replicas.
//
// Two-cache cells get fully independent quotas because they live in
// different namespaces — that's the cellular guarantee.
func (r *DistributedCacheReconciler) reconcileResourceQuota(ctx context.Context, cache *cachev1alpha1.DistributedCache) error {
	log := logf.FromContext(ctx)

	replicas := int32(0)
	if cache.Spec.Replicas != nil {
		replicas = *cache.Spec.Replicas
	}
	totalMem := cache.Spec.MemoryPerPod.DeepCopy()
	totalMem.Set(totalMem.Value() * int64(replicas))
	// 25% headroom: Add(0.25 × totalMem). Quantity arithmetic doesn't
	// support multiplication directly; computing as bytes and reconstructing.
	headroom := totalMem.Value() / 4
	totalWithHeadroom := totalMem.DeepCopy()
	totalWithHeadroom.Set(totalMem.Value() + headroom)

	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cell-quota",
			Namespace: targetNamespace(cache),
		},
	}

	// Cap total pods at the requested replica count plus a small
	// allowance for rolling updates (StatefulSet may briefly run +1
	// during a rolling pod replacement).
	podCap := replicas + 1

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, quota, func() error {
		if quota.Labels == nil {
			quota.Labels = map[string]string{}
		}
		quota.Labels[labelName] = appName
		quota.Labels[labelInstance] = cache.Name
		quota.Labels[labelManagedBy] = managedByValue
		quota.Labels[labelComponent] = "cell"

		quota.Spec.Hard = corev1.ResourceList{
			corev1.ResourcePods:           *resourceQuantity(int64(podCap)),
			corev1.ResourceLimitsMemory:   totalWithHeadroom,
			corev1.ResourceRequestsMemory: totalWithHeadroom,
		}
		// Cross-namespace child: skip ownerRef.
		return r.setControllerReferenceIfSameNamespace(cache, quota)
	})
	if err != nil {
		return fmt.Errorf("CreateOrUpdate ResourceQuota: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		log.Info("reconciled cell ResourceQuota",
			"operation", op,
			"namespace", quota.Namespace,
			"pods", podCap,
			"memory", totalWithHeadroom.String(),
		)
		r.Recorder.Eventf(cache, corev1.EventTypeNormal, "QuotaReconciled",
			"ResourceQuota %s/%s %s (memory=%s)",
			quota.Namespace, quota.Name, op, totalWithHeadroom.String())
	}
	return nil
}

// reconcileNetworkPolicy creates a NetworkPolicy that denies all ingress
// to the cell namespace except from pods within the same namespace.
// This is the network half of the cellular boundary.
//
// Egress is allowed by default; cells need to talk to the API server,
// DNS, etc. Restricting egress would require additional rules for those
// dependencies, which is out of scope for v1alpha1.
//
// Note: enforcement requires a CNI that honors NetworkPolicy. The Calico
// installation in `make kind-up` provides this; the default kindnet CNI
// does not enforce these policies (the resources are still created, just
// not enforced). See DESIGN.md.
func (r *DistributedCacheReconciler) reconcileNetworkPolicy(ctx context.Context, cache *cachev1alpha1.DistributedCache) error {
	log := logf.FromContext(ctx)

	netpol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cell-isolation",
			Namespace: targetNamespace(cache),
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, netpol, func() error {
		if netpol.Labels == nil {
			netpol.Labels = map[string]string{}
		}
		netpol.Labels[labelName] = appName
		netpol.Labels[labelInstance] = cache.Name
		netpol.Labels[labelManagedBy] = managedByValue
		netpol.Labels[labelComponent] = "cell"

		netpol.Spec = networkingv1.NetworkPolicySpec{
			// Empty PodSelector matches all pods in the namespace.
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
			},
			// Allow ingress only from pods in the same namespace.
			// PodSelector with no matchLabels matches all pods in the
			// referenced namespace (which is the policy's own namespace
			// since no namespaceSelector is given).
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{},
				}},
			}},
		}
		return r.setControllerReferenceIfSameNamespace(cache, netpol)
	})
	if err != nil {
		return fmt.Errorf("CreateOrUpdate NetworkPolicy: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		log.Info("reconciled cell NetworkPolicy",
			"operation", op, "namespace", netpol.Namespace)
		r.Recorder.Eventf(cache, corev1.EventTypeNormal, "NetworkPolicyReconciled",
			"NetworkPolicy %s/%s %s", netpol.Namespace, netpol.Name, op)
	}
	return nil
}

// resourceQuantity is a small helper to build a *resource.Quantity from
// an int64 count. Used for ResourceQuota fields that take counts.
func resourceQuantity(n int64) *resource.Quantity {
	q := resource.NewQuantity(n, resource.DecimalSI)
	return q
}

func (r *DistributedCacheReconciler) reconcileService(ctx context.Context, cache *cachev1alpha1.DistributedCache) error {
	log := logf.FromContext(ctx)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cache.Name,
			Namespace: targetNamespace(cache),
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

		return r.setControllerReferenceIfSameNamespace(cache, svc)
	})
	if err != nil {
		return fmt.Errorf("CreateOrUpdate Service: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		log.Info("reconciled headless Service", "operation", op, "name", svc.Name, "namespace", svc.Namespace)
		r.Recorder.Eventf(cache, corev1.EventTypeNormal, "ServiceReconciled",
			"Headless Service %s/%s %s", svc.Namespace, svc.Name, op)
	}

	return nil
}

func (r *DistributedCacheReconciler) reconcileStatefulSet(
	ctx context.Context,
	cache *cachev1alpha1.DistributedCache,
	canScaleDown bool,
) error {
	log := logf.FromContext(ctx)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cache.Name,
			Namespace: targetNamespace(cache),
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		if sts.Labels == nil {
			sts.Labels = map[string]string{}
		}
		sts.Labels[labelName] = appName
		sts.Labels[labelInstance] = cache.Name
		sts.Labels[labelManagedBy] = managedByValue
		sts.Labels[labelComponent] = "cache"

		desiredReplicas := cache.Spec.Replicas
		if !canScaleDown && sts.Spec.Replicas != nil && desiredReplicas != nil && *desiredReplicas < *sts.Spec.Replicas {
			desiredReplicas = sts.Spec.Replicas
		}
		sts.Spec.Replicas = desiredReplicas

		if sts.CreationTimestamp.IsZero() {
			sts.Spec.ServiceName = cache.Name
			sts.Spec.Selector = &metav1.LabelSelector{
				MatchLabels: podLabels(cache),
			}
			sts.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
		}

		sts.Spec.Template.Labels = podLabels(cache)

		grace := int64(cache.Spec.Drain.GracePeriodSeconds) + 10
		sts.Spec.Template.Spec.TerminationGracePeriodSeconds = &grace

		container := corev1.Container{
			Name:  "cache",
			Image: cache.Spec.Image,
			Ports: []corev1.ContainerPort{{
				Name:          "http",
				ContainerPort: cachePort,
				Protocol:      corev1.ProtocolTCP,
			}},
			Resources: corev1.ResourceRequirements{
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

		return r.setControllerReferenceIfSameNamespace(cache, sts)
	})
	if err != nil {
		return fmt.Errorf("CreateOrUpdate StatefulSet: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		log.Info("reconciled StatefulSet", "operation", op, "name", sts.Name, "namespace", sts.Namespace)
		r.Recorder.Eventf(cache, corev1.EventTypeNormal, "StatefulSetReconciled",
			"StatefulSet %s/%s %s", sts.Namespace, sts.Name, op)
	}

	return nil
}

func ringConfigMapName(cache *cachev1alpha1.DistributedCache) string {
	return cache.Name + "-ring"
}

const ringConfigMapKey = "ring.json"

func (r *DistributedCacheReconciler) reconcileRing(ctx context.Context, cache *cachev1alpha1.DistributedCache) (Ring, error) {
	log := logf.FromContext(ctx)

	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(targetNamespace(cache)),
		client.MatchingLabels(podLabels(cache)),
	); err != nil {
		return Ring{}, fmt.Errorf("list pods: %w", err)
	}

	desiredMembers := computeMembers(podList.Items)

	current := Ring{Version: 0, Members: nil}
	var cm corev1.ConfigMap
	cmKey := types.NamespacedName{Namespace: targetNamespace(cache), Name: ringConfigMapName(cache)}
	if err := r.Get(ctx, cmKey, &cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return Ring{}, fmt.Errorf("get ring ConfigMap: %w", err)
		}
	} else {
		if data, ok := cm.Data[ringConfigMapKey]; ok {
			var parsed Ring
			if err := json.Unmarshal([]byte(data), &parsed); err == nil {
				current = parsed
			} else {
				log.Info("ring ConfigMap had unparsable payload; rebuilding", "err", err)
			}
		}
	}

	desired := Ring{Members: desiredMembers}
	if desired.MembersEqual(current) {
		desired.Version = current.Version
	} else {
		desired.Version = current.Version + 1
	}

	if desired.Equal(current) && len(cm.Data) > 0 {
		log.V(1).Info("ring unchanged; skipping ConfigMap write",
			"version", desired.Version, "members", len(desired.Members))
		return desired, nil
	}

	payload, err := json.Marshal(desired)
	if err != nil {
		return Ring{}, fmt.Errorf("marshal ring: %w", err)
	}

	cm = corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ringConfigMapName(cache),
			Namespace: targetNamespace(cache),
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

		return r.setControllerReferenceIfSameNamespace(cache, &cm)
	})
	if err != nil {
		return Ring{}, fmt.Errorf("CreateOrUpdate ring ConfigMap: %w", err)
	}

	if op != controllerutil.OperationResultNone {
		log.Info("published ring",
			"operation", op,
			"version", desired.Version,
			"members", desired.Members,
			"namespace", cm.Namespace,
		)
		r.Recorder.Eventf(cache, corev1.EventTypeNormal, "RingPublished",
			"Ring %s/%s %s (version=%d, members=%v)",
			cm.Namespace, cm.Name, op, desired.Version, desired.Members)
	}

	return desired, nil
}

func (r *DistributedCacheReconciler) drainScaleDown(
	ctx context.Context,
	cache *cachev1alpha1.DistributedCache,
) (bool, error) {
	log := logf.FromContext(ctx)

	var sts appsv1.StatefulSet
	stsKey := types.NamespacedName{Namespace: targetNamespace(cache), Name: cache.Name}
	if err := r.Get(ctx, stsKey, &sts); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("get StatefulSet: %w", err)
	}

	currentReplicas := int32(0)
	if sts.Spec.Replicas != nil {
		currentReplicas = *sts.Spec.Replicas
	}
	desiredReplicas := int32(0)
	if cache.Spec.Replicas != nil {
		desiredReplicas = *cache.Spec.Replicas
	}

	if desiredReplicas >= currentReplicas {
		return true, nil
	}

	doomedNames := make(map[string]bool, currentReplicas-desiredReplicas)
	for i := desiredReplicas; i < currentReplicas; i++ {
		doomedNames[fmt.Sprintf("%s-%d", cache.Name, i)] = true
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(targetNamespace(cache)),
		client.MatchingLabels(podLabels(cache)),
	); err != nil {
		return false, fmt.Errorf("list pods: %w", err)
	}

	grace := time.Duration(cache.Spec.Drain.GracePeriodSeconds) * time.Second
	now := time.Now()
	allReady := true

	for i := range podList.Items {
		p := &podList.Items[i]
		if !doomedNames[p.Name] {
			continue
		}

		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		if _, has := p.Annotations[drainAnnotation]; !has {
			p.Annotations[drainAnnotation] = strconv.FormatInt(now.Unix(), 10)
			if err := r.Update(ctx, p); err != nil {
				return false, fmt.Errorf("annotate pod %s: %w", p.Name, err)
			}
			log.Info("marked pod for drain", "pod", p.Name)
			r.Recorder.Eventf(cache, corev1.EventTypeNormal, "DrainStarted",
				"Pod %s marked for drain (grace=%s)", p.Name, grace)
			allReady = false
			continue
		}

		ts, err := strconv.ParseInt(p.Annotations[drainAnnotation], 10, 64)
		if err != nil {
			p.Annotations[drainAnnotation] = strconv.FormatInt(now.Unix(), 10)
			if err := r.Update(ctx, p); err != nil {
				return false, fmt.Errorf("rewrite drain annotation on %s: %w", p.Name, err)
			}
			allReady = false
			continue
		}
		drainStart := time.Unix(ts, 0)
		if now.Sub(drainStart) < grace {
			allReady = false
		}
	}

	return allReady, nil
}

func (r *DistributedCacheReconciler) reconcileDeletion(
	ctx context.Context,
	cache *cachev1alpha1.DistributedCache,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(cache, drainFinalizer) {
		return ctrl.Result{}, nil
	}

	// Phase 1: drain pods. Same logic as before, scoped to targetNamespace.
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(targetNamespace(cache)),
		client.MatchingLabels(podLabels(cache)),
	); err != nil {
		// If the cell namespace itself is missing (e.g. force-deleted),
		// list returns no error but empty list — fine, skip the drain.
		return ctrl.Result{}, fmt.Errorf("list pods for deletion drain: %w", err)
	}

	grace := time.Duration(cache.Spec.Drain.GracePeriodSeconds) * time.Second
	now := time.Now()
	allDrained := true

	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		if _, has := p.Annotations[drainAnnotation]; !has {
			p.Annotations[drainAnnotation] = strconv.FormatInt(now.Unix(), 10)
			if err := r.Update(ctx, p); err != nil {
				return ctrl.Result{}, fmt.Errorf("annotate %s: %w", p.Name, err)
			}
			log.Info("deletion drain: annotated pod", "pod", p.Name)
			allDrained = false
			continue
		}
		ts, err := strconv.ParseInt(p.Annotations[drainAnnotation], 10, 64)
		if err != nil {
			allDrained = false
			continue
		}
		if now.Sub(time.Unix(ts, 0)) < grace {
			allDrained = false
		}
	}

	// Best-effort ring republish so clients see "no members" before
	// teardown. May fail if the cell namespace is mid-termination — log
	// and continue.
	if _, err := r.reconcileRing(ctx, cache); err != nil {
		log.V(1).Info("ring republish during deletion (non-fatal)", "err", err)
	}

	if !allDrained {
		log.V(1).Info("waiting for drain to complete", "grace", grace)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Phase 2: in cellular mode, delete the cell namespace.
	// Owner references handle children when CR and children share a
	// namespace; in cellular mode the children are cross-namespace and
	// owner refs aren't set, so we must explicitly tear down the cell.
	if isCrossNamespaceChild(cache) {
		done, err := r.deleteCellNamespace(ctx, cache)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("delete cell namespace: %w", err)
		}
		if !done {
			// Namespace exists and is terminating; come back shortly.
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	// Phase 3: drain and cell teardown complete. Release the CR.
	controllerutil.RemoveFinalizer(cache, drainFinalizer)
	if err := r.Update(ctx, cache); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	r.Recorder.Eventf(cache, corev1.EventTypeNormal, "DrainComplete",
		"All pods drained; cell torn down; finalizer removed.")
	log.Info("deletion drain complete; finalizer removed")

	return ctrl.Result{}, nil
}

// deleteCellNamespace ensures the cell namespace is torn down. Returns
// (done bool, err error):
//
//   - done=true  → namespace is gone (or never existed); safe to release CR.
//   - done=false → namespace exists; either we just issued the delete
//     (it's terminating), or it's still terminating from a
//     prior reconcile. Caller should requeue.
//
// Deleting a namespace cascades to every namespaced resource inside it —
// StatefulSet, Service, ConfigMap, ResourceQuota, NetworkPolicy, Pods —
// so this single operation tears down the entire cell.
func (r *DistributedCacheReconciler) deleteCellNamespace(
	ctx context.Context,
	cache *cachev1alpha1.DistributedCache,
) (bool, error) {
	log := logf.FromContext(ctx)

	ns := &corev1.Namespace{}
	nsName := targetNamespace(cache)
	if err := r.Get(ctx, types.NamespacedName{Name: nsName}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			// Already gone (or never created). Done.
			return true, nil
		}
		return false, fmt.Errorf("get cell namespace: %w", err)
	}

	if ns.DeletionTimestamp.IsZero() {
		// Hasn't been deleted yet; issue the delete.
		if err := r.Delete(ctx, ns); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, fmt.Errorf("delete cell namespace: %w", err)
		}
		log.Info("issued cell namespace deletion", "namespace", nsName)
		r.Recorder.Eventf(cache, corev1.EventTypeNormal, "CellTearingDown",
			"Cell namespace %s deletion issued; cascading to children.", nsName)
		return false, nil
	}

	// Already terminating; wait.
	log.V(1).Info("cell namespace terminating", "namespace", nsName)
	return false, nil
}

func (r *DistributedCacheReconciler) setObservedConditions(cache *cachev1alpha1.DistributedCache, ring Ring) {
	desired := int32(0)
	if cache.Spec.Replicas != nil {
		desired = *cache.Spec.Replicas
	}
	ready := int32(len(ring.Members))
	gen := cache.Generation

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

	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type:               ConditionDegraded,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonReconciled,
		Message:            "No degraded conditions observed.",
		ObservedGeneration: gen,
	})

	tenantCond := metav1.Condition{
		Type:               ConditionTenantIsolated,
		ObservedGeneration: gen,
	}
	if cache.Spec.Tenant != nil && cache.Spec.Tenant.Isolate {
		// Reaching this point means reconcileCell ran without error;
		// otherwise we'd have returned early with Degraded.
		tenantCond.Status = metav1.ConditionTrue
		tenantCond.Reason = ReasonReconciled
		tenantCond.Message = fmt.Sprintf("Cell namespace %s with quota and NetworkPolicy active.", targetNamespace(cache))
	} else {
		tenantCond.Status = metav1.ConditionFalse
		tenantCond.Reason = ReasonNotRequested
		tenantCond.Message = "spec.tenant.isolate is unset or false."
	}
	meta.SetStatusCondition(&cache.Status.Conditions, tenantCond)
}

func (r *DistributedCacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("distributedcache-controller")
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.DistributedCache{}).
		Owns(&corev1.Service{}).
		Owns(&appsv1.StatefulSet{}).
		Named("distributedcache").
		Complete(r)
}
