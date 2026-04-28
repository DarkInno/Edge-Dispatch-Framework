package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/auth"
	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/controlplane"
	"github.com/darkinno/edge-dispatch-framework/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	logger.Info("starting control plane")

	cfg := config.LoadControlPlane()

	ctx := context.Background()

	// Initialize PostgreSQL store
	logger.Info("connecting to postgres", "url", cfg.PGURL)
	pgStore, err := store.NewPGStore(ctx, cfg.PGURL)
	if err != nil {
		logger.Error("failed to connect to postgres", "error", err)
		os.Exit(1)
	}
	defer pgStore.Close()

	// Initialize Redis store
	logger.Info("connecting to redis", "addr", cfg.RedisAddr)
	redisStore, err := store.NewRedisStore(ctx, cfg.RedisAddr, cfg.RedisPassword)
	if err != nil {
		logger.Error("failed to connect to redis", "error", err)
		os.Exit(1)
	}
	defer redisStore.Close()

	// Create a shared signer for token operations
	signer := auth.NewSigner(cfg.TokenSecret)

	// Create control plane components
	registry := controlplane.NewRegistry(pgStore, redisStore)
	heartbeat := controlplane.NewHeartbeat(pgStore, redisStore, cfg)
	prober := controlplane.NewProber(pgStore, cfg)
	scheduler := controlplane.NewScheduler(pgStore, signer, cfg)

	// Create API handler (returns http.Handler)
	handler := controlplane.NewAPI(registry, heartbeat, scheduler, cfg)

	// Start background tasks
	ctxBg, cancelBg := context.WithCancel(context.Background())
	defer cancelBg()

	prober.Start(ctxBg)

	// Start HTTP server
	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("http server listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	logger.Info("received shutdown signal", "signal", sig.String())

	cancelBg()
	prober.Stop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http server forced shutdown", "error", err)
	}

	logger.Info("control plane stopped")
}
