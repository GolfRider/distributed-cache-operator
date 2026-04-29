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
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
)

// Store is the in-memory KV backing the tiny-cache server.
//
// It wraps an LRU+TTL cache from hashicorp/golang-lru. The wrapper exists for
// two reasons: it pins the API surface our handlers consume (so we can swap
// the implementation later without touching handlers), and it lets us
// constrain TTL semantics — callers can't pass a per-entry TTL longer than
// the global default, which keeps eviction bounded.
//
// Concurrency: the underlying LRU is safe for concurrent use. The handlers
// can call Get/Put/Delete from multiple goroutines without external locking.
type Store struct {
	lru        *lru.LRU[string, []byte]
	defaultTTL time.Duration
}

// NewStore creates an LRU+TTL store with the given capacity (max entries)
// and default TTL applied to all entries. Capacity 0 is invalid and will
// panic at construction; callers should validate config before reaching here.
func NewStore(capacity int, defaultTTL time.Duration) *Store {
	if capacity <= 0 {
		panic("cache: NewStore requires capacity > 0")
	}
	if defaultTTL <= 0 {
		// 60s is a reasonable floor; anything shorter is almost certainly a
		// config error, and "no TTL" defeats the cache-is-a-hint contract.
		defaultTTL = 60 * time.Second
	}
	c := lru.NewLRU[string, []byte](capacity, nil, defaultTTL)
	return &Store{lru: c, defaultTTL: defaultTTL}
}

// Get returns the value for key. The bool reports whether the key was
// present and not expired. The returned []byte is owned by the caller —
// internally the LRU stores it by reference, so callers must not mutate
// the returned slice. (Our handlers write the slice to an HTTP response
// and discard it; mutation never happens.)
func (s *Store) Get(key string) ([]byte, bool) {
	return s.lru.Get(key)
}

// Put stores key=value with the store's default TTL. Returns true if a
// previous value was evicted (overwrite or LRU pressure).
func (s *Store) Put(key string, value []byte) bool {
	return s.lru.Add(key, value)
}

// Delete removes key. Returns true if the key was present.
func (s *Store) Delete(key string) bool {
	return s.lru.Remove(key)
}

// Len returns the current number of entries (cheap; used by metrics later).
func (s *Store) Len() int {
	return s.lru.Len()
}
