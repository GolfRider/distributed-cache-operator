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

// Package cache implements the tiny-cache data-plane server: a single-node,
// peer-unaware, in-memory KV cache with HTTP API.
package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Config holds tiny-cache server configuration.
type Config struct {
	Listen     string
	Logger     *slog.Logger
	Capacity   int           // max entries in the LRU
	DefaultTTL time.Duration // applied to every PUT
}

// Run starts the tiny-cache server and blocks until ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		return errors.New("Config.Logger is required")
	}
	if cfg.Listen == "" {
		return errors.New("Config.Listen is required")
	}
	if cfg.Capacity <= 0 {
		return errors.New("Config.Capacity must be > 0")
	}

	srv := newServer(cfg)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	listenErr := make(chan error, 1)
	go func() {
		cfg.Logger.Info("tiny-cache listening",
			"addr", cfg.Listen,
			"capacity", cfg.Capacity,
			"default_ttl", cfg.DefaultTTL,
		)
		err := httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			listenErr <- nil
			return
		}
		listenErr <- err
	}()

	select {
	case <-ctx.Done():
		cfg.Logger.Info("shutdown signal received; draining")
	case err := <-listenErr:
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		return nil
	}

	srv.markNotReady()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	if err := <-listenErr; err != nil {
		return err
	}
	return nil
}

// server holds the runtime state of a tiny-cache instance.
type server struct {
	logger *slog.Logger
	store  *Store
	ready  atomic.Bool
}

func newServer(cfg Config) *server {
	s := &server{
		logger: cfg.Logger,
		store:  NewStore(cfg.Capacity, cfg.DefaultTTL),
	}
	s.ready.Store(true)
	return s
}

func (s *server) markNotReady() { s.ready.Store(false) }

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})

	// KV endpoints. Stdlib mux dispatches by path prefix; the handler
	// extracts the key and dispatches by method.
	mux.HandleFunc("/kv/", s.kvHandler)

	return mux
}
