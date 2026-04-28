package edgeagent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/streaming"
	"github.com/darkinno/edge-dispatch-framework/internal/tunnel"
)

// Edge coordinates all edge agent components.
type Edge struct {
	cfg          *config.EdgeAgentConfig
	cache        *Cache
	fetcher      *Fetcher
	server       *Server
	reporter     *Reporter
	tunnelClient *tunnel.Client // nil if not in NAT mode
	prefetchMgr  *streaming.PrefetchManager
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

	edge := &Edge{
		cfg:      cfg,
		cache:    cache,
		fetcher:  fetcher,
		server:   server,
		reporter: reporter,
	}

	// Initialize streaming components (v0.4)
	if cfg.Streaming != nil && cfg.Streaming.Enabled {
		window := streaming.NewSlidingWindow(cache, cfg.Streaming)
		prefetchMgr := streaming.NewPrefetchManager(cfg.Streaming, window, cfg.OriginURL, cfg.NodeToken)
		streamH := streaming.NewHandler(window, prefetchMgr, cfg.Streaming)
		server.WithStreaming(streamH)
		edge.prefetchMgr = prefetchMgr
		slog.Info("streaming mode enabled",
			"window_size", cfg.Streaming.WindowSize,
			"prefetch_count", cfg.Streaming.PrefetchCount,
		)
	}

	// Initialize tunnel client if in NAT mode
	if cfg.NATMode && cfg.TunnelServerAddr != "" {
		tunnelCfg := tunnel.ClientConfig{
			ServerAddr:    cfg.TunnelServerAddr,
			NodeID:        "", // Will be set after registration
			NodeToken:     cfg.TunnelAuthToken,
			ServiceAddr:   cfg.ListenAddr,
			KeepAlive:     30 * time.Second,
			ReconnectWait: 5 * time.Second,
		}
		edge.tunnelClient = tunnel.NewClient(tunnelCfg, slog.Default())
		slog.Info("NAT mode enabled, tunnel client configured",
			"tunnel_server", cfg.TunnelServerAddr,
		)
	}

	return edge, nil
}

// Start runs the edge agent: registers if needed, then starts server and reporter.
func (e *Edge) Start(ctx context.Context) error {
	// Register with control plane if no token
	if e.cfg.NodeToken == "" {
		slog.Info("no node token, registering with control plane")
		if err := e.reporter.Register(ctx); err != nil {
			return fmt.Errorf("register: %w", err)
		}

		// Update tunnel client with node ID after registration
		if e.tunnelClient != nil {
			e.tunnelClient = tunnel.NewClient(tunnel.ClientConfig{
				ServerAddr:    e.cfg.TunnelServerAddr,
				NodeID:        e.reporter.NodeID(),
				NodeToken:     e.cfg.TunnelAuthToken,
				ServiceAddr:   e.cfg.ListenAddr,
				KeepAlive:     30 * time.Second,
				ReconnectWait: 5 * time.Second,
			}, slog.Default())
		}
	}

	// Start tunnel client if in NAT mode
	if e.tunnelClient != nil {
		go func() {
			slog.Info("starting tunnel client")
			if err := e.tunnelClient.Run(); err != nil {
				slog.Error("tunnel client error", "err", err)
			}
		}()
	}

	// Start the reporter (heartbeats)
	if err := e.reporter.Start(ctx); err != nil {
		return fmt.Errorf("start reporter: %w", err)
	}

	// Start prefetch manager (v0.4)
	if e.prefetchMgr != nil {
		e.prefetchMgr.Start(ctx)
	}

	// Start the HTTP server (blocks until ctx cancelled)
	if err := e.server.Start(ctx); err != nil {
		return fmt.Errorf("start server: %w", err)
	}

	slog.Info("edge agent started",
		"nat_mode", e.cfg.NATMode,
		"tunnel_enabled", e.tunnelClient != nil,
	)
	return nil
}

// Shutdown gracefully stops the edge agent.
func (e *Edge) Shutdown(ctx context.Context) error {
	// Stop tunnel client first
	if e.tunnelClient != nil {
		e.tunnelClient.Stop()
	}

	e.reporter.Stop()

	// Stop prefetch manager (v0.4)
	if e.prefetchMgr != nil {
		e.prefetchMgr.Stop()
	}

	if err := e.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown server: %w", err)
	}
	return nil
}

// IsNATMode returns true if this node is operating in NAT mode.
func (e *Edge) IsNATMode() bool {
	return e.cfg.NATMode
}

// TunnelID returns the tunnel ID if connected, empty string otherwise.
func (e *Edge) TunnelID() string {
	if e.tunnelClient != nil {
		return e.tunnelClient.TunnelID()
	}
	return ""
}
