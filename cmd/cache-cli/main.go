/*
Copyright 2026.
... (Apache header)
*/

// Command cache-cli is a small demo client for distributed-cache-operator.
// It uses the cacheclient library to resolve a key to a pod, then issues
// HTTP requests directly against that pod's tiny-cache server.
//
// Usage:
//
//	cache-cli --ns default --name orders-cache get foo
//	cache-cli --ns default --name orders-cache put foo "hello"
//	cache-cli --ns default --name orders-cache delete foo
//	cache-cli --ns default --name orders-cache info
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/GolfRider/distributed-cache-operator/internal/cacheclient"
)

func main() {
	var (
		ns           = flag.String("ns", "default", "namespace of the DistributedCache")
		name         = flag.String("name", "", "name of the DistributedCache")
		kubeconfig   = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig")
		port         = flag.Int("port", 8080, "tiny-cache HTTP port")
		pollInterval = flag.Duration("poll", 2*time.Second, "ring poll interval (lower than default for CLI use)")
	)
	flag.Parse()

	if *name == "" {
		fatalf("--name is required")
	}
	if flag.NArg() < 1 {
		fatalf("usage: cache-cli [flags] {get|put|delete|info} <key> [value]")
	}
	op := flag.Arg(0)

	cfg, err := loadRestConfig(*kubeconfig)
	if err != nil {
		fatalf("kube config: %v", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fatalf("k8s client: %v", err)
	}

	ringCM := types.NamespacedName{Namespace: *ns, Name: *name + "-ring"}
	svcFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", *name, *ns)

	c := cacheclient.New(cacheclient.Config{
		K8s:           k8s,
		RingConfigMap: ringCM,
		ServiceFQDN:   svcFQDN,
		PollInterval:  *pollInterval,
	})

	// Run the poller in the background so the ring is fresh when we Lookup.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	// Wait for first poll to succeed (bounded).
	deadline := time.Now().Add(5 * time.Second)
	for c.RingVersion() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	if op == "info" {
		fmt.Printf("ring version: %d\n", c.RingVersion())
		fmt.Printf("service FQDN: %s\n", svcFQDN)
		// Demonstrate distribution by hashing a few keys
		fmt.Println("sample lookups:")
		for _, k := range []string{"foo", "bar", "baz", "user-1234", "session-abcd", "orders-99"} {
			pod, err := c.Lookup(k)
			if err != nil {
				fmt.Printf("  %-15s → ERROR %v\n", k, err)
				continue
			}
			fmt.Printf("  %-15s → %s\n", k, pod)
		}
		return
	}

	if flag.NArg() < 2 {
		fatalf("usage: cache-cli [flags] {get|put|delete} <key> [value]")
	}
	key := flag.Arg(1)

	pod, err := c.Lookup(key)
	if err != nil {
		fatalf("lookup: %v", err)
	}
	url := fmt.Sprintf("http://%s:%d/kv/%s", pod, *port, key)

	switch op {
	case "get":
		resp, err := http.Get(url)
		if err != nil {
			fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("→ %s\n%d %s\n%s\n", url, resp.StatusCode, http.StatusText(resp.StatusCode), body)
	case "put":
		if flag.NArg() < 3 {
			fatalf("usage: cache-cli [flags] put <key> <value>")
		}
		req, _ := http.NewRequest(http.MethodPut, url, bytes.NewBufferString(flag.Arg(2)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fatalf("PUT: %v", err)
		}
		defer resp.Body.Close()
		fmt.Printf("→ %s\n%d %s\n", url, resp.StatusCode, http.StatusText(resp.StatusCode))
	case "delete":
		req, _ := http.NewRequest(http.MethodDelete, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fatalf("DELETE: %v", err)
		}
		defer resp.Body.Close()
		fmt.Printf("→ %s\n%d %s\n", url, resp.StatusCode, http.StatusText(resp.StatusCode))
	default:
		fatalf("unknown op: %s", op)
	}

	_ = corev1.SchemeGroupVersion // silence unused import in some paths
}

func fatalf(f string, args ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(2)
}

// loadRestConfig returns a Kubernetes REST config, preferring in-cluster
// service account credentials when available and falling back to a
// kubeconfig file when running outside a cluster.
func loadRestConfig(kubeconfigPath string) (*rest.Config, error) {
	// In-cluster: kubelet mounts /var/run/secrets/kubernetes.io/serviceaccount/
	// and sets KUBERNETES_SERVICE_HOST. rest.InClusterConfig succeeds when both
	// are present.
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}

	// Out-of-cluster: use the supplied path or default to ~/.kube/config.
	if kubeconfigPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("home dir: %w", err)
		}
		kubeconfigPath = home + "/.kube/config"
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", kubeconfigPath, err)
	}
	return cfg, nil
}
