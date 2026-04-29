//go:build !quic

package quic

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
)

func init() {
	Enabled = false
}

// ServerConfig holds QUIC server configuration.
type ServerConfig struct {
	Addr      string
	Handler   http.Handler
	TLSConfig *tls.Config
}

// Server is a no-op QUIC server placeholder.
type Server struct{}

// NewServer returns a no-op server.
func NewServer(cfg ServerConfig) *Server {
	slog.Debug("quic: server not available, build with -tags quic to enable HTTP/3")
	return &Server{}
}

// ListenAndServe returns an error since QUIC is not enabled.
func (s *Server) ListenAndServe() error {
	return errors.New("quic: not available (build with -tags quic)")
}

// Close is a no-op.
func (s *Server) Close() error { return nil }

// Addr returns empty string.
func (s *Server) Addr() string { return "" }
