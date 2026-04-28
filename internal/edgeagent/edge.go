package edgeagent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
)

// Edge coordinates all edge agent components.
type Edge struct {
	cfg      *config.EdgeAgentConfig
	cache    *Cache
	fetcher  *Fetcher
	server   *Server
	reporter *Reporter
}

// New creates a new Edge agent, initializing all sub-components.
func New(cfg *config.EdgeAgentConfig) (*Edge, error) {
	cache, err := NewCache(cfg.CacheDir, cfg.CacheMaxGB)
	if err != nil {
		return nil, fmt.Errorf("create cache: %w", err)
	}

	fetcher := NewFetcher(cfg)
	server := NewServer(cache, fetcher, cfg)
	reporter := NewReporter(cfg, server, cache)

	return &Edge{
		cfg:      cfg,
		cache:    cache,
		fetcher:  fetcher,
		server:   server,
		reporter: reporter,
	}, nil
}

// Start runs the edge agent: registers if needed, then starts server and reporter.
func (e *Edge) Start(ctx context.Context) error {
	// Register with control plane if no token
	if e.cfg.NodeToken == "" {
		slog.Info("no node token, registering with control plane")
		if err := e.reporter.Register(ctx); err != nil {
			return fmt.Errorf("register: %w", err)
		}
	}

	// Start the reporter (heartbeats)
	if err := e.reporter.Start(ctx); err != nil {
		return fmt.Errorf("start reporter: %w", err)
	}

	// Start the HTTP server (blocks until ctx cancelled)
	if err := e.server.Start(ctx); err != nil {
		return fmt.Errorf("start server: %w", err)
	}

	slog.Info("edge agent started")
	return nil
}

// Shutdown gracefully stops the edge agent.
func (e *Edge) Shutdown(ctx context.Context) error {
	e.reporter.Stop()
	if err := e.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown server: %w", err)
	}
	return nil
}
