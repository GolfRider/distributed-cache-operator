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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	ReasonReconciled     = "Reconciled"
	ReasonInitializing   = "Initializing"
	ReasonNotRequested   = "NotRequested"
	ReasonNotImplemented = "NotImplemented"
	ReasonServiceFailed  = "ServiceReconcileFailed"
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
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives a DistributedCache toward its desired state. It is
// level-triggered: every invocation re-observes the world from scratch and
// makes one step's worth of progress, idempotently.
func (r *DistributedCacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Fetch the CR. NotFound is the normal path when a CR has been deleted;
	//    return nil so controller-runtime stops requeueing this name.
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

	// 2. Reconcile owned resources.
	if err := r.reconcileService(ctx, &cache); err != nil {
		// Record the failure into a condition so users see it in
		// `kubectl describe` rather than only in operator logs.
		meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
			Type:               ConditionDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonServiceFailed,
			Message:            fmt.Sprintf("headless Service reconcile failed: %v", err),
			ObservedGeneration: cache.Generation,
		})
		// Best-effort status persist before returning the error;
		// if even the status write fails, propagate the original error.
		_ = r.Status().Update(ctx, &cache)
		return ctrl.Result{}, fmt.Errorf("reconcile service: %w", err)
	}

	// 3. Compute conditions for the steady "observed" state. Once owned
	//    resources are healthy in later steps, Available will flip True.
	cache.Status.ObservedGeneration = cache.Generation
	r.setInitializingConditions(&cache)

	// 4. Persist status via the /status subresource.
	if err := r.Status().Update(ctx, &cache); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	return ctrl.Result{}, nil
}

// reconcileService ensures a headless Service exists for this cache and has
// the right shape. The Service has clusterIP=None so its DNS record returns
// per-pod IPs (cache-0.<svc>, cache-1.<svc>, ...) — clients address pods
// directly by name, since consistent hashing means load-balancing would be
// counterproductive.
func (r *DistributedCacheReconciler) reconcileService(ctx context.Context, cache *cachev1alpha1.DistributedCache) error {
	log := logf.FromContext(ctx)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cache.Name,
			Namespace: cache.Namespace,
		},
	}

	// CreateOrUpdate handles three cases idempotently:
	//   - Service does not exist: the mutate fn shapes a fresh object, then Create.
	//   - Service exists, matches desired shape: no API write performed.
	//   - Service exists, differs: mutate fn updates only the fields we set,
	//     then Update — preserving server-set fields we don't touch.
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		// Labels: idempotent merge. We assert the keys we own; any keys
		// added by other tools (Istio, kustomize commonLabels) are preserved.
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels[labelName] = appName
		svc.Labels[labelInstance] = cache.Name
		svc.Labels[labelManagedBy] = managedByValue
		svc.Labels[labelComponent] = "cache"

		// Spec: shape the headless Service.
		// ClusterIPNone is the magic value that disables load-balancing and
		// makes DNS return per-pod records.
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.Selector = map[string]string{
			labelName:     appName,
			labelInstance: cache.Name,
		}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       8080,
			TargetPort: intstr.FromInt32(8080),
			Protocol:   corev1.ProtocolTCP,
		}}
		// PublishNotReadyAddresses=false: when a pod goes NotReady, its DNS
		// record is removed. The drain logic relies on this — flipping a pod
		// to NotReady is how the operator removes it from client traffic
		// before the pod actually terminates.
		svc.Spec.PublishNotReadyAddresses = false

		// SetControllerReference writes ownerReferences pointing at the CR.
		// This makes the Service garbage-collected on CR deletion and
		// enables our Owns(&Service{}) watch to re-enqueue the parent on
		// any Service event.
		return controllerutil.SetControllerReference(cache, svc, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("CreateOrUpdate Service: %w", err)
	}

	// Emit a Kubernetes event only when something actually changed.
	// OperationResultNone means the Service was already in desired state.
	if op != controllerutil.OperationResultNone {
		log.Info("reconciled headless Service", "operation", op, "name", svc.Name)
		r.Recorder.Eventf(cache, corev1.EventTypeNormal, "ServiceReconciled",
			"Headless Service %s/%s %s", svc.Namespace, svc.Name, op)
	}

	return nil
}

// setInitializingConditions sets the four conditions to a coherent
// "we observed the object but haven't finished bringing up the data plane"
// state. As we add StatefulSet and ring reconciliation, this helper will
// be replaced with logic that reflects real readiness.
func (r *DistributedCacheReconciler) setInitializingConditions(cache *cachev1alpha1.DistributedCache) {
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type:               ConditionAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonInitializing,
		Message:            "Reconciler observed the resource; data plane not yet running.",
		ObservedGeneration: cache.Generation,
	})
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type:               ConditionProgressing,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonInitializing,
		Message:            "Bringing up owned resources.",
		ObservedGeneration: cache.Generation,
	})
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type:               ConditionDegraded,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonReconciled,
		Message:            "No degraded conditions observed.",
		ObservedGeneration: cache.Generation,
	})

	tenantStatus := metav1.Condition{
		Type:               ConditionTenantIsolated,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cache.Generation,
	}
	if cache.Spec.Tenant != nil && cache.Spec.Tenant.Isolate {
		tenantStatus.Reason = ReasonNotImplemented
		tenantStatus.Message = "Tenant isolation accepted by API but not yet wired."
	} else {
		tenantStatus.Reason = ReasonNotRequested
		tenantStatus.Message = "spec.tenant.isolate is unset or false."
	}
	meta.SetStatusCondition(&cache.Status.Conditions, tenantStatus)
}

// SetupWithManager wires the reconciler into the manager. The Owns calls
// register watches on owned resources: any change to a child Service we own
// re-enqueues the parent CR for reconciliation, which is how owned-state
// drift heals automatically.
func (r *DistributedCacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("distributedcache-controller")
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.DistributedCache{}).
		Owns(&corev1.Service{}).
		Named("distributedcache").
		Complete(r)
}
