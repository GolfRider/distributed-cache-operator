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
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Ring is the published view of cache cluster membership. The operator owns
// it; clients consume it via the ring ConfigMap.
//
// Members is ordered by StatefulSet ordinal (cache-0, cache-1, ...) for
// deterministic ring construction on the client side. Clients use member
// names as input to consistent-hash positioning; the order itself is not
// significant for hash math but matters for human-readable diffs.
type Ring struct {
	Version int64    `json:"version"`
	Members []string `json:"members"`
}

// Equal reports whether two rings represent the same membership at the
// same version. Used to skip writes when membership hasn't changed.
func (r Ring) Equal(other Ring) bool {
	if r.Version != other.Version {
		return false
	}
	if len(r.Members) != len(other.Members) {
		return false
	}
	for i := range r.Members {
		if r.Members[i] != other.Members[i] {
			return false
		}
	}
	return true
}

// MembersEqual reports whether two rings have the same membership,
// ignoring version. Used to decide whether to bump version on reconcile.
func (r Ring) MembersEqual(other Ring) bool {
	if len(r.Members) != len(other.Members) {
		return false
	}
	for i := range r.Members {
		if r.Members[i] != other.Members[i] {
			return false
		}
	}
	return true
}

// computeMembers returns the ordered list of pod names that should be in
// the ring, given a list of pods and their readiness state.
//
// Selection rules:
//   - Pod must be in Running phase. Pending/Succeeded/Failed are excluded.
//   - Pod must have PodReady=True in its conditions. The cache pod itself
//     reports readiness via /readyz; this filter respects that signal.
//   - Pods with a deletion timestamp are excluded immediately, even if
//     still Ready, so the ring removes them as soon as drain begins.
//
// Ordering: by StatefulSet ordinal (cache-0, cache-1, ...). Ordinals are
// parsed from the pod name; pods with non-conformant names are excluded
// (defensive — should not happen with our own StatefulSet).
func computeMembers(pods []corev1.Pod) []string {
	type ordered struct {
		name    string
		ordinal int
	}
	var ready []ordered

	for i := range pods {
		p := &pods[i]
		if !p.DeletionTimestamp.IsZero() {
			continue
		}
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		if !isPodReady(p) {
			continue
		}
		ord, ok := podOrdinal(p.Name)
		if !ok {
			continue
		}
		ready = append(ready, ordered{name: p.Name, ordinal: ord})
	}

	sort.Slice(ready, func(i, j int) bool { return ready[i].ordinal < ready[j].ordinal })

	out := make([]string, len(ready))
	for i, r := range ready {
		out[i] = r.name
	}
	return out
}

// isPodReady checks the pod's conditions for PodReady=True.
// Equivalent to k8s.io/api/core/v1.IsPodReady but without pulling another
// dependency for a four-line check.
func isPodReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podOrdinal extracts the trailing integer from a StatefulSet pod name.
// "orders-cache-7" → (7, true). Pod names that don't fit the
// "<sts-name>-<ordinal>" pattern return false.
func podOrdinal(name string) (int, bool) {
	idx := strings.LastIndex(name, "-")
	if idx == -1 || idx == len(name)-1 {
		return 0, false
	}
	n, err := strconv.Atoi(name[idx+1:])
	if err != nil {
		return 0, false
	}
	return n, true
}
