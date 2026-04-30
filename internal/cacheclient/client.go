/*
Copyright 2026.
... (Apache header)
*/

// Package cacheclient is a reference client for distributed-cache-operator.
//
// It polls the operator-published ring ConfigMap and routes key lookups
// to specific cache pods using consistent hashing. The client is the only
// component that holds ring state at runtime — cache pods are peer-unaware.
//
// In-cluster only. The pod DNS names returned by Lookup are reachable from
// pods in the same Kubernetes cluster but not from outside.
package cacheclient

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Ring is the on-the-wire shape published by the operator. Mirrored from
// the operator's internal type to avoid cross-package import.
type Ring struct {
	Version int64    `json:"version"`
	Members []string `json:"members"`
}

// vnodeCount is virtual nodes per pod. Higher → smoother key distribution
// but more memory and slower ring construction. 128 is the standard
// trade-off used by most consistent-hash implementations.
const vnodeCount = 128

// hashRing is the resolved ring used for lookups. Built once per ring
// version published by the operator.
type hashRing struct {
	version int64
	// hashes is sorted; positions[i] tells which pod owns hashes[i].
	hashes    []uint32
	positions []string
}

// Client tracks the latest ring and serves Lookup queries. Safe for
// concurrent use; Lookup is lock-free via atomic.Pointer.
type Client struct {
	k8s           client.Client
	ringConfigMap types.NamespacedName
	pollInterval  time.Duration
	ring          atomic.Pointer[hashRing]
	serviceFQDN   string
}

// Config configures a Client. RingConfigMap is the namespaced name of the
// operator-published ConfigMap (e.g. {Namespace: "default", Name:
// "orders-cache-ring"}). ServiceFQDN is the FQDN of the headless Service
// (e.g. "orders-cache.default.svc.cluster.local"); Lookup returns
// "<pod-name>.<ServiceFQDN>".
type Config struct {
	K8s           client.Client
	RingConfigMap types.NamespacedName
	ServiceFQDN   string
	PollInterval  time.Duration
}

// New creates a Client. Run must be called to start polling; Lookup
// returns ErrRingEmpty until the first successful poll.
func New(cfg Config) *Client {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 10 * time.Second
	}
	return &Client{
		k8s:           cfg.K8s,
		ringConfigMap: cfg.RingConfigMap,
		pollInterval:  cfg.PollInterval,
		serviceFQDN:   cfg.ServiceFQDN,
	}
}

// Run polls the ring ConfigMap until ctx is cancelled. Errors during a
// poll are logged via the slog default; the loop continues so transient
// API failures don't take the client down.
func (c *Client) Run(ctx context.Context) error {
	// First poll synchronously so callers can rely on a populated ring
	// before they start serving traffic. Bound to the poll interval so a
	// hung API server doesn't block startup forever.
	pollCtx, cancel := context.WithTimeout(ctx, c.pollInterval)
	_ = c.pollOnce(pollCtx)
	cancel()

	t := time.NewTicker(c.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			pollCtx, cancel := context.WithTimeout(ctx, c.pollInterval)
			_ = c.pollOnce(pollCtx)
			cancel()
		}
	}
}

// pollOnce reads the ConfigMap and rebuilds the ring if version changed.
func (c *Client) pollOnce(ctx context.Context) error {
	var cm corev1.ConfigMap
	if err := c.k8s.Get(ctx, c.ringConfigMap, &cm); err != nil {
		return fmt.Errorf("get ring ConfigMap: %w", err)
	}
	raw, ok := cm.Data["ring.json"]
	if !ok {
		return fmt.Errorf("ring ConfigMap missing ring.json")
	}
	var r Ring
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return fmt.Errorf("unmarshal ring: %w", err)
	}

	current := c.ring.Load()
	if current != nil && current.version == r.Version {
		return nil // unchanged
	}

	built := buildRing(r)
	c.ring.Store(built)
	return nil
}

// buildRing constructs the consistent-hash ring from a published Ring.
// Each pod gets vnodeCount virtual positions; positions are sorted so
// Lookup can binary-search.
func buildRing(r Ring) *hashRing {
	hashes := make([]uint32, 0, len(r.Members)*vnodeCount)
	positions := make([]string, 0, len(r.Members)*vnodeCount)
	for _, member := range r.Members {
		for i := 0; i < vnodeCount; i++ {
			h := hashString(fmt.Sprintf("%s#%d", member, i))
			hashes = append(hashes, h)
			positions = append(positions, member)
		}
	}
	// Sort by hash, carrying positions along.
	idx := make([]int, len(hashes))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return hashes[idx[a]] < hashes[idx[b]] })

	sortedHashes := make([]uint32, len(hashes))
	sortedPositions := make([]string, len(positions))
	for i, j := range idx {
		sortedHashes[i] = hashes[j]
		sortedPositions[i] = positions[j]
	}
	return &hashRing{
		version:   r.Version,
		hashes:    sortedHashes,
		positions: sortedPositions,
	}
}

// hashString returns a uint32 hash of s. SHA-1 truncated; sufficient
// distribution for ring positioning, no security assumption.
func hashString(s string) uint32 {
	h := sha1.Sum([]byte(s))
	return binary.BigEndian.Uint32(h[:4])
}

// ErrRingEmpty is returned by Lookup before the first poll succeeds or
// when the ring has zero members.
var ErrRingEmpty = fmt.Errorf("ring is empty")

// Lookup returns the FQDN of the pod that owns the given key.
// Lock-free: reads the atomic ring snapshot once.
func (c *Client) Lookup(key string) (string, error) {
	r := c.ring.Load()
	if r == nil || len(r.hashes) == 0 {
		return "", ErrRingEmpty
	}
	h := hashString(key)
	// Binary-search for the first hash >= h; wrap around if past the end.
	i := sort.Search(len(r.hashes), func(i int) bool { return r.hashes[i] >= h })
	if i == len(r.hashes) {
		i = 0
	}
	pod := r.positions[i]
	return fmt.Sprintf("%s.%s", pod, c.serviceFQDN), nil
}

// RingVersion returns the version of the currently-loaded ring, or 0 if
// none. Useful for testing and debugging.
func (c *Client) RingVersion() int64 {
	r := c.ring.Load()
	if r == nil {
		return 0
	}
	return r.version
}
