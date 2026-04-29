package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/gateway"
)

func main() {
	// Setup structured logging
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	slog.Info("starting Edge Dispatch Gateway v0.3")

	// Load configuration
	cfg := config.LoadGateway()

	// Create control plane client for node resolution
	resolver := gateway.NewControlPlaneClient(cfg.ControlPlaneURL, cfg.CPToken, logger)

	// Create and start gateway
	gw := gateway.New(
		gateway.Config{
			ListenAddr:      cfg.ListenAddr,
			TunnelAddr:      cfg.TunnelAddr,
			ControlPlaneURL: cfg.ControlPlaneURL,
			AuthToken:       cfg.AuthToken,
			ReadTimeout:     cfg.ReadTimeout,
			WriteTimeout:    cfg.WriteTimeout,
			IdleTimeout:     cfg.IdleTimeout,
		},
		resolver,
		logger,
	)

	if err := gw.Start(); err != nil {
		slog.Error("failed to start gateway", "err", err)
		os.Exit(1)
	}

	// Print startup info
	fmt.Printf(`
╔══════════════════════════════════════════════════════════════╗
║           Edge Dispatch Gateway v0.3 Started                ║
╠══════════════════════════════════════════════════════════════╣
║  HTTP Proxy:    %-42s ║
║  Tunnel Server: %-42s ║
║  Control Plane: %-42s ║
╚══════════════════════════════════════════════════════════════╝
`, cfg.ListenAddr, cfg.TunnelAddr, cfg.ControlPlaneURL)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutting down gateway...")
	gw.Stop()
	slog.Info("gateway stopped")
}

func init() {
	// Suppress unused import warning
	_ = context.Background
}
