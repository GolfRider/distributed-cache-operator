package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cachev1alpha1 "github.com/GolfRider/distributed-cache-operator/api/v1alpha1"
)

// Condition types — exported so clients and tests can reference the same constants.
const (
	ConditionAvailable      = "Available"
	ConditionProgressing    = "Progressing"
	ConditionDegraded       = "Degraded"
	ConditionTenantIsolated = "TenantIsolated"
)

// Condition reasons.
const (
	ReasonReconciled     = "Reconciled"
	ReasonInitializing   = "Initializing"
	ReasonNotRequested   = "NotRequested"
	ReasonNotImplemented = "NotImplemented"
)

// DistributedCacheReconciler reconciles a DistributedCache object.
type DistributedCacheReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=cache.sk1.services.com,resources=distributedcaches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cache.sk1.services.com,resources=distributedcaches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cache.sk1.services.com,resources=distributedcaches/finalizers,verbs=update

// Reconcile observes a DistributedCache and moves the cluster one step closer
// to the desired state. The function is called level-triggered: it never
// receives deltas, only the namespaced name of the CR to inspect.
func (r *DistributedCacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Fetch the CR.
	var cache cachev1alpha1.DistributedCache
	if err := r.Get(ctx, req.NamespacedName, &cache); err != nil {
		// IsNotFound is the expected path when a CR has been deleted; the
		// reconciler is invoked one last time with the now-gone object's name.
		// Owner references take care of GC for owned resources, so we just exit.
		if apierrors.IsNotFound(err) {
			log.V(1).Info("DistributedCache not found; assuming deleted")
			return ctrl.Result{}, nil
		}
		// Any other error is a transient API failure — return it to trigger
		// controller-runtime's exponential backoff retry.
		return ctrl.Result{}, fmt.Errorf("get DistributedCache: %w", err)
	}

	log.Info("reconciling", "generation", cache.Generation, "replicas", cache.Spec.Replicas)

	// 2. Compute the new condition set. For now this is a placeholder set
	//    that says "we observed the object but haven't done anything yet."
	//    Each branch in the real reconciler will set these more precisely.
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type:               ConditionAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonInitializing,
		Message:            "Reconciler observed the resource; owned resources not yet created.",
		ObservedGeneration: cache.Generation,
	})
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type:               ConditionProgressing,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonInitializing,
		Message:            "Initial reconcile in progress.",
		ObservedGeneration: cache.Generation,
	})
	meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
		Type:               ConditionDegraded,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonReconciled,
		Message:            "No degraded conditions observed.",
		ObservedGeneration: cache.Generation,
	})
	if cache.Spec.Tenant != nil && cache.Spec.Tenant.Isolate {
		meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
			Type:               ConditionTenantIsolated,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonNotImplemented,
			Message:            "Tenant isolation accepted by API but not yet wired.",
			ObservedGeneration: cache.Generation,
		})
	} else {
		meta.SetStatusCondition(&cache.Status.Conditions, metav1.Condition{
			Type:               ConditionTenantIsolated,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonNotRequested,
			Message:            "spec.tenant.isolate is unset or false.",
			ObservedGeneration: cache.Generation,
		})
	}

	// 3. Record observedGeneration so clients can tell the controller has
	//    caught up to the latest spec, even if no other status field changed.
	cache.Status.ObservedGeneration = cache.Generation

	// 4. Persist status. Status writes go through the /status subresource,
	//    not the main endpoint — using r.Update here would silently drop the
	//    status changes.
	if err := r.Status().Update(ctx, &cache); err != nil {
		// Conflict means another writer touched the object between our Get and
		// Update. Return the error and let controller-runtime requeue; the next
		// reconcile reads fresh state and tries again.
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager. The watch graph
// declared here defines what events trigger a reconcile.
func (r *DistributedCacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.DistributedCache{}).
		Named("distributedcache").
		Complete(r)
}
