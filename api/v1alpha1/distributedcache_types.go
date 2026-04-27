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

package v1alpha1

import (
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DistributedCacheSpec defines the desired state of a DistributedCache.
//
// A DistributedCache represents a sharded, in-memory, client-side-hashed
// cache cluster. The operator reconciles the underlying StatefulSet,
// headless Service, and ring ConfigMap; the cache pods themselves are
// intentionally peer-unaware (see DESIGN.md §2).
type DistributedCacheSpec struct {
	// Replicas is the desired number of cache pods.
	//
	// The minimum is 1: a zero-replica cache is not a meaningful state
	// for this operator (clients polling the ring would see an empty
	// member list and fail every lookup). Defaulted to 3 to match the
	// typical "small HA" footprint.
	//
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Image is the container image for the cache pods. Required.
	//
	// No default is provided deliberately: a cache operator that silently
	// defaults to some image version is a supply-chain footgun.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// MemoryPerPod is the memory limit applied to each cache pod.
	// Uses the standard resource.Quantity format (e.g. "128Mi", "1Gi").
	//
	// CPU is not exposed in v1alpha1: caches are memory-bound and
	// adding a CPU knob widens the API without solving a real problem.
	//
	// +kubebuilder:validation:Required
	MemoryPerPod resource.Quantity `json:"memoryPerPod"`

	// Drain controls how the operator sequences cache pod terminations
	// during scale-down and deletion. See DESIGN.md §3 ("Drain and the
	// ring-staleness contract").
	//
	// +optional
	Drain DrainSpec `json:"drain,omitempty"`

	// Tenant, when set, opts this DistributedCache into multi-tenant
	// isolation: the operator owns a Namespace, ResourceQuota, and
	// NetworkPolicy scoped to this CR. See DESIGN.md §4
	// ("Tenant isolation").
	//
	// +optional
	Tenant *TenantSpec `json:"tenant,omitempty"`
}

// DrainSpec controls scale-down and deletion sequencing.
type DrainSpec struct {
	// GracePeriodSeconds is how long the operator waits after marking
	// a pod NotReady (and publishing a new ring excluding it) before
	// allowing the StatefulSet to terminate the pod.
	//
	// This value must be larger than the client ring-poll interval
	// (10s by convention) plus a safety margin; the default of 30s
	// gives 3× the poll interval. See DESIGN.md §3.
	//
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	// +optional
	GracePeriodSeconds int32 `json:"gracePeriodSeconds,omitempty"`
}

// TenantSpec controls multi-tenant isolation.
type TenantSpec struct {
	// Isolate, when true, causes the operator to create and own a
	// dedicated Namespace, ResourceQuota, and NetworkPolicy for this
	// DistributedCache. When false or unset, the cache is created in
	// the namespace of the CR without additional isolation.
	//
	// +optional
	Isolate bool `json:"isolate,omitempty"`
}

// DistributedCacheStatus defines the observed state of a DistributedCache.
//
// Status is written exclusively by the controller. The /status subresource
// (enabled via the +kubebuilder:subresource:status marker on the root type)
// enforces this at the API-server level: updates to spec and status go
// through different endpoints with different RBAC.
//
// Status is treated as a cache of observations: if the controller is
// replaced, its successor reconstructs status from cluster state, not
// from persisted controller memory. See DESIGN.md §1.
type DistributedCacheStatus struct {
	// Conditions represent the latest available observations of the
	// DistributedCache's state.
	//
	// Known condition types:
	//   - "Available":      the cache has sufficient ready pods and a
	//                       published ring; clients can use it.
	//   - "Progressing":    a scale event, image roll, or initial
	//                       rollout is in flight. Independent of
	//                       Available — Available=True, Progressing=True
	//                       means "healthy, still rolling."
	//   - "Degraded":       a stuck condition that needs human attention
	//                       (e.g. invalid image, quota exceeded).
	//                       Distinguished from Available=False, which
	//                       may be transient and self-healing.
	//   - "TenantIsolated": tenant-isolation guardrails are in effect.
	//                       Reason field distinguishes NotRequested,
	//                       NotImplemented, and Degraded.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration reflects the .metadata.generation that this
	// status was computed from. CI and clients gate on
	// observedGeneration == metadata.generation before trusting other
	// status fields, to distinguish "controller hasn't processed my
	// change yet" from "controller processed it and decided nothing
	// needed doing."
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ReadyReplicas is the number of cache pods currently Ready and
	// included in the published ring. May lag spec.replicas during
	// scale events; combined with the Progressing condition, this is
	// how clients reason about transition state.
	//
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Ring summarizes the currently published ring topology. The
	// authoritative ring lives in a ConfigMap (named in
	// Ring.ConfigMapName); this status field is a compact summary
	// for humans running `kubectl describe`. See DESIGN.md §2.
	//
	// +optional
	Ring RingStatus `json:"ring,omitempty"`
}

// RingStatus summarizes the published ring topology.
type RingStatus struct {
	// Version is a monotonically increasing counter, incremented by
	// the operator on every membership change. Clients use this to
	// detect ring updates between polls; the value is also written
	// to the ring ConfigMap.
	//
	// +optional
	Version int64 `json:"version,omitempty"`

	// Members is the ordered list of pod names currently in the ring,
	// sorted by ordinal (cache-0, cache-1, ...). Pods being drained
	// are excluded as soon as the operator publishes their removal,
	// even before they are terminated.
	//
	// +optional
	// +listType=atomic
	Members []string `json:"members,omitempty"`

	// ConfigMapName is the name of the ConfigMap holding the
	// authoritative ring data. Same namespace as the DistributedCache.
	//
	// +optional
	ConfigMapName string `json:"configMapName,omitempty"`
}

// DistributedCache is a sharded, in-memory cache cluster managed by
// distributed-cache-operator. The operator reconciles the underlying
// StatefulSet, headless Service, and ring ConfigMap, and is the source
// of truth for ring topology. See DESIGN.md.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=dc;dcache,categories=cache
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Available",type="string",JSONPath=".status.conditions[?(@.type=='Available')].status"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Ring",type="integer",JSONPath=".status.ring.version"
// +kubebuilder:printcolumn:name="Image",type="string",priority=1,JSONPath=".spec.image"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type DistributedCache struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DistributedCache.
	// +required
	Spec DistributedCacheSpec `json:"spec"`

	// status defines the observed state of DistributedCache.
	// +optional
	Status DistributedCacheStatus `json:"status,omitzero"`
}

// DistributedCacheList contains a list of DistributedCache.
//
// +kubebuilder:object:root=true
type DistributedCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DistributedCache `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DistributedCache{}, &DistributedCacheList{})
}
