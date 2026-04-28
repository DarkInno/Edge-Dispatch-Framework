package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/edgeagent"
)

func main() {
	debug.SetGCPercent(60)
	debug.SetMemoryLimit(512 << 20)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	logger.Info("starting edge agent")

	cfg := config.LoadEdgeAgent()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create Edge instance
	edge, err := edgeagent.New(cfg)
	if err != nil {
		logger.Error("failed to create edge agent", "error", err)
		os.Exit(1)
	}

	// Handle graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-quit
		logger.Info("received shutdown signal", "signal", sig.String())
		cancel()
	}()

	if err := edge.Start(ctx); err != nil {
		logger.Error("edge agent failed", "error", err)
		os.Exit(1)
	}

	logger.Info("edge agent stopped")
}
