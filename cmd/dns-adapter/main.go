package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/dns"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg := config.LoadDNSAdapter()
	slog.Info("starting dns-adapter", "domain", cfg.Domain, "listen", cfg.ListenAddr)

	srv := dns.NewServer(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutting down dns-adapter")
		cancel()
	}()

	if err := srv.Start(ctx); err != nil {
		slog.Error("dns-adapter failed", "err", err)
		os.Exit(1)
	}

	slog.Info("dns-adapter stopped")
}
