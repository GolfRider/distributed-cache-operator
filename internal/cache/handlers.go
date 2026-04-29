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

package cache

import (
	"io"
	"net/http"
	"strings"
)

// Maximum value size in bytes. Caps the per-entry memory footprint to
// prevent a single large PUT from blowing the pod's memory budget. Tunable
// later if needed; 1MB is a comfortable default for typical cache usage.
const maxValueBytes int64 = 1 << 20 // 1 MiB

// kvHandler dispatches /kv/{key} requests to the right method handler.
// We use Go's stdlib mux which doesn't natively extract path parameters,
// so we extract the key inline. Refactor to chi/mux later if the routing
// gets richer; for three endpoints this is fine.
func (s *server) kvHandler(w http.ResponseWriter, r *http.Request) {
	const prefix = "/kv/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, prefix)
	if key == "" || strings.Contains(key, "/") {
		// Empty or nested keys are rejected. Nested paths (/kv/a/b) are
		// reserved for future namespacing and are an error today.
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, r, key)
	case http.MethodPut:
		s.handlePut(w, r, key)
	case http.MethodDelete:
		s.handleDelete(w, r, key)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request, key string) {
	value, ok := s.store.Get(key)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(value)
}

func (s *server) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	// Cap the read at maxValueBytes+1 so we can detect oversize bodies
	// without buffering arbitrarily-large payloads.
	limited := io.LimitReader(r.Body, maxValueBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxValueBytes {
		http.Error(w, "value too large", http.StatusRequestEntityTooLarge)
		return
	}
	s.store.Put(key, body)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request, key string) {
	existed := s.store.Delete(key)
	if !existed {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
