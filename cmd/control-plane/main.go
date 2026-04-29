package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/auth"
	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/contentindex"
	"github.com/darkinno/edge-dispatch-framework/internal/controlplane"
	"github.com/darkinno/edge-dispatch-framework/internal/store"
)

func main() {
	debug.SetGCPercent(80)
	debug.SetMemoryLimit(1 << 30)

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
	nodeCache := controlplane.NewNodeCache(pgStore, cfg.NodeCacheTTL)
	prober := controlplane.NewProber(pgStore, nodeCache, cfg)
	scheduler := controlplane.NewScheduler(nodeCache, signer, cfg)

	// Content index (v0.2+)
	ciStore, err := contentindex.NewStore(ctx, pgStore.Pool(), &cfg.ContentIndex)
	if err != nil {
		logger.Error("failed to create content index store", "error", err)
		os.Exit(1)
	}
	defer ciStore.Close()
	heartbeat.SetContentStore(ciStore)
	scheduler.SetContentIndex(ciStore.Index())

	// Load existing content index data into memory
	if err := ciStore.LoadAll(ctx); err != nil {
		logger.Warn("failed to load content index from db", "error", err)
	}

	// Create API handler (returns http.Handler)
	handler := controlplane.NewAPI(registry, heartbeat, scheduler, cfg)

	// Initialize Admin API (v0.5)
	var adminHandler http.Handler
	if cfg.Admin.Enabled {
		adminHandler, err = controlplane.NewAdminAPI(pgStore, redisStore, registry, &cfg.Admin)
		if err != nil {
			logger.Error("failed to create admin API", "error", err)
			os.Exit(1)
		}
		if adminHandler != nil {
			logger.Info("admin API enabled", "jwt_expiry_seconds", cfg.Admin.JWTExpirySeconds)
		}
	}

	// Start background tasks
	ctxBg, cancelBg := context.WithCancel(context.Background())
	defer cancelBg()

	prober.Start(ctxBg)

	// Start HTTP server
	combinedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/internal/admin/v1") && adminHandler != nil {
			adminHandler.ServeHTTP(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           combinedHandler,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	go func() {
		logger.Info("http server listening", "addr", cfg.ListenAddr)
		var err error
		if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			err = srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
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
