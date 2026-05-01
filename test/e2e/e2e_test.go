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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	networkingv1 "k8s.io/api/networking/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cachev1alpha1 "github.com/GolfRider/distributed-cache-operator/api/v1alpha1"
)

const (
	testNamespace = "default"
	testCRName    = "e2e-cache"
	testTimeout   = 4 * time.Minute
	testInterval  = 1 * time.Second
)

var _ = Describe("DistributedCache lifecycle", Ordered, func() {
	var (
		k8s client.Client
		ctx context.Context
	)

	BeforeAll(func() {
		ctx = context.Background()

		// Build a client from the user's current kubeconfig. The test runner
		// is expected to be pointed at the kind cluster where the CRD is
		// installed and the operator is running.
		cfg, err := clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		Expect(err).NotTo(HaveOccurred(), "load kubeconfig")

		scheme := runtime.NewScheme()
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		utilruntime.Must(cachev1alpha1.AddToScheme(scheme))

		k8s, err = client.New(cfg, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred(), "construct k8s client")

		// Best-effort cleanup of any prior run.
		_ = k8s.Delete(ctx, &cachev1alpha1.DistributedCache{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testCRName},
		})
		Eventually(func() bool {
			err := k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName},
				&cachev1alpha1.DistributedCache{})
			return apierrors.IsNotFound(err)
		}, testTimeout, testInterval).Should(BeTrue(), "previous CR should be cleaned up")
	})

	It("creates owned resources and reaches Available=True", func() {
		replicas := int32(3)
		cr := &cachev1alpha1.DistributedCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testCRName,
				Namespace: testNamespace,
			},
			Spec: cachev1alpha1.DistributedCacheSpec{
				Replicas:     &replicas,
				Image:        "tiny-cache:dev",
				MemoryPerPod: resource.MustParse("64Mi"),
				Drain: cachev1alpha1.DrainSpec{
					GracePeriodSeconds: 5,
				},
			},
		}
		Expect(k8s.Create(ctx, cr)).To(Succeed())

		// Wait for Available=True.
		Eventually(func(g Gomega) {
			fresh := &cachev1alpha1.DistributedCache{}
			g.Expect(k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName}, fresh)).To(Succeed())
			cond := meta.FindStatusCondition(fresh.Status.Conditions, "Available")
			g.Expect(cond).NotTo(BeNil(), "Available condition should be set")
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
				"Available should be True (reason=%s, message=%s)", cond.Reason, cond.Message)
		}, testTimeout, testInterval).Should(Succeed())

		// Owned resources exist.
		Expect(k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName},
			&corev1.Service{})).To(Succeed())
		Expect(k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName},
			&appsv1.StatefulSet{})).To(Succeed())

		// Ring ConfigMap has the expected members.
		ringCM := &corev1.ConfigMap{}
		Expect(k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName + "-ring"},
			ringCM)).To(Succeed())
		ring := readRing(ringCM)
		Expect(ring.Members).To(HaveLen(3))
		Expect(ring.Version).To(BeNumerically(">=", int64(1)))
	})

	It("scales up and reflects new ring version", func() {
		// Capture current ring version.
		var startVersion int64
		{
			cm := &corev1.ConfigMap{}
			Expect(k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName + "-ring"}, cm)).To(Succeed())
			startVersion = readRing(cm).Version
		}

		// Scale to 5.
		fresh := &cachev1alpha1.DistributedCache{}
		Expect(k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName}, fresh)).To(Succeed())
		five := int32(5)
		fresh.Spec.Replicas = &five
		Expect(k8s.Update(ctx, fresh)).To(Succeed())

		// Wait for ring to grow and version to advance.
		Eventually(func(g Gomega) {
			cm := &corev1.ConfigMap{}
			g.Expect(k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName + "-ring"}, cm)).To(Succeed())
			ring := readRing(cm)
			g.Expect(ring.Members).To(HaveLen(5))
			g.Expect(ring.Version).To(BeNumerically(">", startVersion))
		}, testTimeout, testInterval).Should(Succeed())
	})

	It("scales down with drain and converges", func() {
		fresh := &cachev1alpha1.DistributedCache{}
		Expect(k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName}, fresh)).To(Succeed())
		three := int32(3)
		fresh.Spec.Replicas = &three
		Expect(k8s.Update(ctx, fresh)).To(Succeed())

		// During drain, doomed pods should be annotated. We don't sleep
		// to "see" the annotation — we Eventually the final state.
		Eventually(func(g Gomega) {
			cm := &corev1.ConfigMap{}
			g.Expect(k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName + "-ring"}, cm)).To(Succeed())
			g.Expect(readRing(cm).Members).To(HaveLen(3))
		}, testTimeout, testInterval).Should(Succeed())

		// StatefulSet should eventually be at 3 replicas.
		Eventually(func(g Gomega) {
			sts := &appsv1.StatefulSet{}
			g.Expect(k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName}, sts)).To(Succeed())
			g.Expect(sts.Spec.Replicas).NotTo(BeNil())
			g.Expect(*sts.Spec.Replicas).To(Equal(int32(3)))
		}, testTimeout, testInterval).Should(Succeed())
	})

	It("deletes the CR and garbage-collects owned resources", func() {
		Expect(k8s.Delete(ctx, &cachev1alpha1.DistributedCache{
			ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		})).To(Succeed())

		// Finalizer holds deletion until drain completes; eventually CR is gone.
		Eventually(func() bool {
			err := k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName},
				&cachev1alpha1.DistributedCache{})
			return apierrors.IsNotFound(err)
		}, testTimeout, testInterval).Should(BeTrue(), "CR should be deleted after drain")

		// Owned resources GC'd.
		Eventually(func() bool {
			err := k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName},
				&appsv1.StatefulSet{})
			return apierrors.IsNotFound(err)
		}, testTimeout, testInterval).Should(BeTrue(), "StatefulSet should be GC'd")
		Eventually(func() bool {
			err := k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName},
				&corev1.Service{})
			return apierrors.IsNotFound(err)
		}, testTimeout, testInterval).Should(BeTrue(), "Service should be GC'd")
		Eventually(func() bool {
			err := k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testCRName + "-ring"},
				&corev1.ConfigMap{})
			return apierrors.IsNotFound(err)
		}, testTimeout, testInterval).Should(BeTrue(), "ring ConfigMap should be GC'd")
	})
})

const (
	cellTestCRName = "e2e-cell-cache"
	cellNamespace  = "cache-" + cellTestCRName
)

var _ = Describe("DistributedCache cellular isolation", Ordered, func() {
	var (
		k8s client.Client
		ctx context.Context
	)

	BeforeAll(func() {
		ctx = context.Background()

		cfg, err := clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		Expect(err).NotTo(HaveOccurred(), "load kubeconfig")

		scheme := runtime.NewScheme()
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		utilruntime.Must(cachev1alpha1.AddToScheme(scheme))

		k8s, err = client.New(cfg, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred(), "construct k8s client")

		// Best-effort cleanup of any prior run.
		_ = k8s.Delete(ctx, &cachev1alpha1.DistributedCache{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: cellTestCRName},
		})
		Eventually(func() bool {
			err := k8s.Get(ctx,
				types.NamespacedName{Namespace: testNamespace, Name: cellTestCRName},
				&cachev1alpha1.DistributedCache{})
			return apierrors.IsNotFound(err)
		}, testTimeout, testInterval).Should(BeTrue(), "previous CR should be cleaned up")

		// Also verify the cell namespace from a prior run is gone, otherwise
		// our new cell creation would race against its termination.
		Eventually(func() bool {
			err := k8s.Get(ctx,
				types.NamespacedName{Name: cellNamespace},
				&corev1.Namespace{})
			return apierrors.IsNotFound(err)
		}, testTimeout, testInterval).Should(BeTrue(), "previous cell namespace should be cleaned up")
	})

	It("creates a cell namespace with quota, network policy, and data plane", func() {
		replicas := int32(3)
		cr := &cachev1alpha1.DistributedCache{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cellTestCRName,
				Namespace: testNamespace,
			},
			Spec: cachev1alpha1.DistributedCacheSpec{
				Replicas:     &replicas,
				Image:        "tiny-cache:dev",
				MemoryPerPod: resource.MustParse("64Mi"),
				Drain: cachev1alpha1.DrainSpec{
					GracePeriodSeconds: 5,
				},
				Tenant: &cachev1alpha1.TenantSpec{
					Isolate: true,
				},
			},
		}
		Expect(k8s.Create(ctx, cr)).To(Succeed())

		// Wait for both Available=True AND TenantIsolated=True.
		// The compound condition guarantees the cell is fully wired,
		// not just present in spec.
		Eventually(func(g Gomega) {
			fresh := &cachev1alpha1.DistributedCache{}
			g.Expect(k8s.Get(ctx,
				types.NamespacedName{Namespace: testNamespace, Name: cellTestCRName}, fresh)).To(Succeed())

			available := meta.FindStatusCondition(fresh.Status.Conditions, "Available")
			g.Expect(available).NotTo(BeNil(), "Available condition should exist")
			g.Expect(available.Status).To(Equal(metav1.ConditionTrue),
				"Available should be True (reason=%s, message=%s)",
				available.Reason, available.Message)

			tenant := meta.FindStatusCondition(fresh.Status.Conditions, "TenantIsolated")
			g.Expect(tenant).NotTo(BeNil(), "TenantIsolated condition should exist")
			g.Expect(tenant.Status).To(Equal(metav1.ConditionTrue),
				"TenantIsolated should be True (reason=%s, message=%s)",
				tenant.Reason, tenant.Message)
			g.Expect(tenant.Reason).To(Equal("Reconciled"))
		}, testTimeout, testInterval).Should(Succeed())
	})

	It("places owned resources in the cell namespace, not the CR namespace", func() {
		// StatefulSet, Service, ConfigMap should be in cellNamespace.
		Expect(k8s.Get(ctx,
			types.NamespacedName{Namespace: cellNamespace, Name: cellTestCRName},
			&appsv1.StatefulSet{})).To(Succeed())
		Expect(k8s.Get(ctx,
			types.NamespacedName{Namespace: cellNamespace, Name: cellTestCRName},
			&corev1.Service{})).To(Succeed())
		Expect(k8s.Get(ctx,
			types.NamespacedName{Namespace: cellNamespace, Name: cellTestCRName + "-ring"},
			&corev1.ConfigMap{})).To(Succeed())

		// And NOT in the CR's namespace (default).
		err := k8s.Get(ctx,
			types.NamespacedName{Namespace: testNamespace, Name: cellTestCRName},
			&appsv1.StatefulSet{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"StatefulSet should not exist in CR namespace when isolated")
	})

	It("creates ResourceQuota and NetworkPolicy in the cell namespace", func() {
		quota := &corev1.ResourceQuota{}
		Expect(k8s.Get(ctx,
			types.NamespacedName{Namespace: cellNamespace, Name: "cell-quota"},
			quota)).To(Succeed())

		// Quota carries memory and pod caps.
		Expect(quota.Spec.Hard).To(HaveKey(corev1.ResourcePods))
		Expect(quota.Spec.Hard).To(HaveKey(corev1.ResourceLimitsMemory))

		netpol := &networkingv1.NetworkPolicy{}
		Expect(k8s.Get(ctx,
			types.NamespacedName{Namespace: cellNamespace, Name: "cell-isolation"},
			netpol)).To(Succeed())

		// NetworkPolicy denies ingress except from same namespace.
		Expect(netpol.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeIngress))
	})

	It("tears down the cell namespace on CR deletion", func() {
		Expect(k8s.Delete(ctx, &cachev1alpha1.DistributedCache{
			ObjectMeta: metav1.ObjectMeta{Name: cellTestCRName, Namespace: testNamespace},
		})).To(Succeed())

		// CR is gone after finalizer drain + namespace deletion.
		Eventually(func() bool {
			err := k8s.Get(ctx,
				types.NamespacedName{Namespace: testNamespace, Name: cellTestCRName},
				&cachev1alpha1.DistributedCache{})
			return apierrors.IsNotFound(err)
		}, testTimeout, testInterval).Should(BeTrue(), "CR should be deleted after cellular drain")

		// The cell namespace is gone (cascade-deleted everything inside it).
		Eventually(func() bool {
			err := k8s.Get(ctx,
				types.NamespacedName{Name: cellNamespace},
				&corev1.Namespace{})
			return apierrors.IsNotFound(err)
		}, testTimeout, testInterval).Should(BeTrue(), "cell namespace should be GC'd")
	})
})

// readRing parses the ring JSON from a ConfigMap. Mirrors the operator's
// internal Ring struct to avoid an import cycle.
func readRing(cm *corev1.ConfigMap) struct {
	Version int64    `json:"version"`
	Members []string `json:"members"`
} {
	var r struct {
		Version int64    `json:"version"`
		Members []string `json:"members"`
	}
	raw, ok := cm.Data["ring.json"]
	Expect(ok).To(BeTrue(), "ring ConfigMap should have ring.json key")
	Expect(json.Unmarshal([]byte(raw), &r)).To(Succeed(), "ring JSON should parse")
	_ = fmt.Sprintf // silence unused-import in some versions
	return r
}
