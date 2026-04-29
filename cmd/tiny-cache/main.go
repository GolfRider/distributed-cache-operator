// cmd/tiny-cache/main.go
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

// Command tiny-cache runs a single-node, in-memory KV cache server.
// It is the data plane managed by distributed-cache-operator: peer-unaware,
// symmetric, no replication, no persistence. Coordination lives in the
// control plane; this binary is intentionally minimal.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/GolfRider/distributed-cache-operator/internal/cache"
)

func main() {
	var (
		listen = flag.String("listen", ":8080", "address to listen on for HTTP traffic")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Cancel the root context on SIGTERM/SIGINT. The server's Run method
	// observes this and triggers graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cfg := cache.Config{
		Listen: *listen,
		Logger: logger,
	}

	if err := cache.Run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "tiny-cache: %v\n", err)
		os.Exit(1)
	}
}
