// internal/cache/server.go
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
//
// The server is intentionally minimal — no replication, no persistence, no
// gossip. Coordination (membership, ring publication, drain) is the
// operator's job; this package only knows how to serve gets, puts, and
// deletes against a local LRU.
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

// Config holds tiny-cache server configuration. Constructed by main.go.
type Config struct {
	Listen string
	Logger *slog.Logger
}

// Run starts the tiny-cache server and blocks until ctx is cancelled.
// On cancellation, it triggers a graceful shutdown bounded by the
// shutdown timeout below.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		return errors.New("Config.Logger is required")
	}
	if cfg.Listen == "" {
		return errors.New("Config.Listen is required")
	}

	srv := newServer(cfg)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Run the listener on a goroutine; capture its error via channel so the
	// main path can wait on either ctx.Done or a listener failure.
	listenErr := make(chan error, 1)
	go func() {
		cfg.Logger.Info("tiny-cache listening", "addr", cfg.Listen)
		// ListenAndServe blocks until Shutdown or a hard error.
		err := httpSrv.ListenAndServe()
		// http.ErrServerClosed is the expected error from a clean shutdown;
		// translate to nil so callers don't see it as a failure.
		if errors.Is(err, http.ErrServerClosed) {
			listenErr <- nil
			return
		}
		listenErr <- err
	}()

	// Wait for either a signal-driven shutdown or a listener error.
	select {
	case <-ctx.Done():
		cfg.Logger.Info("shutdown signal received; draining")
	case err := <-listenErr:
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		// Listener closed without error before any signal — unusual but treat
		// as clean exit.
		return nil
	}

	// Mark the server NotReady so the readiness probe fails immediately.
	// Existing connections continue; no new traffic is routed here.
	srv.markNotReady()

	// Bounded graceful shutdown. Existing in-flight requests finish; new
	// connections are refused. After the timeout, ListenAndServe returns
	// regardless and any leftover handlers are abandoned.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// Drain the listener-error channel after shutdown to avoid a goroutine leak.
	if err := <-listenErr; err != nil {
		return err
	}
	return nil
}

// server holds the runtime state of a tiny-cache instance.
//
// `ready` is atomic so the readiness handler can be lock-free; the operator's
// drain logic depends on /readyz responses changing within milliseconds of
// the markNotReady call.
type server struct {
	logger *slog.Logger
	ready  atomic.Bool
}

func newServer(cfg Config) *server {
	s := &server{logger: cfg.Logger}
	s.ready.Store(true)
	return s
}

func (s *server) markNotReady() { s.ready.Store(false) }

// routes registers HTTP handlers. The KV handlers, /metrics, and the actual
// LRU come in a follow-up commit; this skeleton has only the probes so we
// can verify the build and signal handling end-to-end.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	// /healthz: liveness — process is up, mux is responding.
	// Always 200 unless the process has crashed (in which case the kernel
	// returns RST and the probe fails at the TCP level).
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// /readyz: readiness — the process is willing to serve traffic.
	// Flips to 503 when shutdown begins. The operator's drain logic relies
	// on this signal: kubelet observes 503, marks the pod NotReady, the
	// Service removes the pod's DNS record, clients stop targeting it.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})

	return mux
}
