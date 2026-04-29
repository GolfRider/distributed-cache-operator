/*
Copyright 2026.
... (same header)
*/

// Command tiny-cache runs a single-node, in-memory KV cache server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GolfRider/distributed-cache-operator/internal/cache"
)

func main() {
	var (
		listen     = flag.String("listen", ":8080", "address to listen on for HTTP traffic")
		capacity   = flag.Int("capacity", 100_000, "max number of cache entries")
		defaultTTL = flag.Duration("default-ttl", 5*time.Minute, "default TTL applied to PUTs")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cfg := cache.Config{
		Listen:     *listen,
		Logger:     logger,
		Capacity:   *capacity,
		DefaultTTL: *defaultTTL,
	}

	if err := cache.Run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "tiny-cache: %v\n", err)
		os.Exit(1)
	}
}
