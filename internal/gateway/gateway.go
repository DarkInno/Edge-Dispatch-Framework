package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/metrics"
	"github.com/darkinno/edge-dispatch-framework/internal/tunnel"
)

// Config holds gateway configuration.
type Config struct {
	ListenAddr      string        // HTTP listen address
	TunnelAddr      string        // Tunnel server listen address
	ControlPlaneURL string        // Control plane API URL
	AuthToken       string        // Token for tunnel node authentication
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
}

// DefaultConfig returns default gateway configuration.
func DefaultConfig() Config {
	return Config{
		ListenAddr:      ":8880",
		TunnelAddr:      ":7700",
		ControlPlaneURL: "http://localhost:8080",
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    60 * time.Second,
		IdleTimeout:     120 * time.Second,
	}
}

// Gateway is the unified reverse proxy gateway for edge dispatch.
type Gateway struct {
	cfg          Config
	tunnelServer *tunnel.Server
	logger       *slog.Logger
	server       *http.Server
	resolver     NodeResolver
	ctx          context.Context
	cancel       context.CancelFunc
	promMetrics  *gwMetrics
}

type gwMetrics struct {
	requestsTotal *metrics.CounterFn
	errorsTotal   *metrics.CounterFn
	tunnelGauge   *metrics.GaugeFn
}

// NodeResolver resolves node IDs to their endpoints.
type NodeResolver interface {
	// GetNodeEndpoint returns the endpoint URL for a node.
	// Returns empty string if node is NAT (use tunnel).
	GetNodeEndpoint(nodeID string) (string, bool, error)
	// GetBestNode returns the best node for a request.
	GetBestNode(resourceKey string, clientIP string) (string, error)
}

// New creates a new Gateway.
func New(cfg Config, resolver NodeResolver, logger *slog.Logger) *Gateway {
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())

	gw := &Gateway{
		cfg:      cfg,
		resolver: resolver,
		logger:   logger,
		ctx:      ctx,
		cancel:   cancel,
		promMetrics: &gwMetrics{
			requestsTotal: metrics.NewCounter("gateway_requests_total", "Total number of proxy requests"),
			errorsTotal:   metrics.NewCounter("gateway_errors_total", "Total number of proxy errors"),
			tunnelGauge:   metrics.NewGauge("gateway_active_tunnels", "Number of active tunnel connections"),
		},
	}

	// Create tunnel server
	tunnelCfg := tunnel.ServerConfig{
		ListenAddr:  cfg.TunnelAddr,
		AuthToken:   cfg.AuthToken,
		IdleTimeout: 5 * time.Minute,
		MaxTunnels:  100,
	}
	tunnelServer := tunnel.NewServer(tunnelCfg, logger)
	gw.tunnelServer = tunnelServer

	return gw
}

// Start starts the gateway.
func (g *Gateway) Start() error {
	// Start tunnel server
	if err := g.tunnelServer.Start(); err != nil {
		return fmt.Errorf("start tunnel server: %w", err)
	}

	// Create HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/obj/", g.handleObjectRequest)
	mux.HandleFunc("/healthz", g.handleHealth)
	mux.HandleFunc("/metrics", g.handleMetrics)

	g.server = &http.Server{
		Addr:         g.cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  g.cfg.ReadTimeout,
		WriteTimeout: g.cfg.WriteTimeout,
		IdleTimeout:  g.cfg.IdleTimeout,
	}

	ln, err := net.Listen("tcp", g.cfg.ListenAddr)
	if err != nil {
		g.tunnelServer.Stop()
		return fmt.Errorf("listen gateway: %w", err)
	}

	g.logger.Info("gateway started",
		"addr", g.cfg.ListenAddr,
		"tunnel_addr", g.cfg.TunnelAddr,
	)

	go func() {
		if err := g.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			g.logger.Error("gateway serve error", "err", err)
		}
	}()

	return nil
}

// Stop gracefully shuts down the gateway.
func (g *Gateway) Stop() {
	g.cancel()
	if g.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		g.server.Shutdown(ctx)
	}
	g.tunnelServer.Stop()
	g.logger.Info("gateway stopped")
}

// TunnelManager returns the tunnel manager for external access.
func (g *Gateway) TunnelManager() *tunnel.TunnelManager {
	return g.tunnelServer.Manager()
}

func (g *Gateway) handleObjectRequest(w http.ResponseWriter, r *http.Request) {
	// Extract resource key from path: /obj/{key}
	key := strings.TrimPrefix(r.URL.Path, "/obj/")
	key = strings.TrimSpace(key)
	if key == "" || strings.Contains(key, "..") || strings.Contains(key, "\n") || strings.Contains(key, "\r") {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return
	}

	// Get client IP
	clientIP := extractClientIP(r)

	// Resolve best node
	nodeID, err := g.resolver.GetBestNode(key, clientIP)
	if err != nil {
		g.logger.Error("resolve node failed", "err", err, "key", key)
		http.Error(w, "no available node", http.StatusServiceUnavailable)
		return
	}

	// Get node endpoint
	endpoint, isPublic, err := g.resolver.GetNodeEndpoint(nodeID)
	if err != nil {
		g.logger.Error("get node endpoint failed", "err", err, "node_id", nodeID)
		http.Error(w, "node resolution error", http.StatusInternalServerError)
		return
	}

	if isPublic && endpoint != "" {
		// Public node: direct proxy
		g.proxyToPublic(w, r, endpoint, key)
	} else {
		// NAT node: proxy through tunnel
		g.proxyToTunnel(w, r, nodeID, key)
	}
}

// hopByHopHeaders are headers that should not be forwarded by proxies.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"TE":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func stripHopByHopHeaders(headers map[string]string) {
	for h := range hopByHopHeaders {
		delete(headers, h)
	}
}

func sanitizeGatewayKey(key string) string {
	key = strings.TrimSpace(key)
	key = strings.ReplaceAll(key, "\\", "/")
	parts := make([]string, 0)
	for _, p := range strings.Split(key, "/") {
		p = strings.TrimSpace(p)
		if p == "" || p == "." || p == ".." {
			continue
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "/")
}

func (g *Gateway) proxyToPublic(w http.ResponseWriter, r *http.Request, endpoint, key string) {
	targetURL, err := url.Parse(endpoint)
	if err != nil {
		g.logger.Error("parse endpoint", "err", err, "endpoint", endpoint)
		http.Error(w, "invalid endpoint", http.StatusInternalServerError)
		return
	}

	if targetURL.Scheme != "http" && targetURL.Scheme != "https" {
		g.logger.Error("invalid endpoint scheme", "scheme", targetURL.Scheme)
		http.Error(w, "invalid endpoint", http.StatusInternalServerError)
		return
	}

	key = sanitizeGatewayKey(key)
	targetURL.Path = path.Join("/obj/", key)

	// Create reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		req.URL.Path = targetURL.Path
		req.Host = targetURL.Host

		for _, h := range []string{
			"Connection", "Keep-Alive", "Proxy-Authenticate",
			"Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
		} {
			req.Header.Del(h)
		}

		req.Header.Set("X-Forwarded-For", extractClientIP(req))
		req.Header.Set("X-Forwarded-Host", r.Host)
		req.Header.Set("X-Forwarded-Proto", "http")
		if r.TLS != nil {
			req.Header.Set("X-Forwarded-Proto", "https")
		}
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		g.logger.Error("proxy error", "err", err, "endpoint", endpoint)
		http.Error(w, "proxy error", http.StatusBadGateway)
	}

	proxy.ServeHTTP(w, r)
}

func (g *Gateway) proxyToTunnel(w http.ResponseWriter, r *http.Request, nodeID, key string) {
	// Build request for tunnel
	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	stripHopByHopHeaders(headers)
	headers["X-Forwarded-For"] = extractClientIP(r)
	headers["X-Forwarded-Host"] = r.Host

	key = sanitizeGatewayKey(key)
	reqHeader := &tunnel.HTTPRequestHeader{
		Method:  r.Method,
		Path:    "/obj/" + key,
		Headers: headers,
		BodyLen: r.ContentLength,
	}

	// Forward through tunnel
	respHeader, bodyReader, err := g.tunnelServer.ForwardRequest(nodeID, reqHeader, r.Body)
	if err != nil {
		g.logger.Error("tunnel forward failed", "err", err, "node_id", nodeID)
		http.Error(w, "tunnel proxy error", http.StatusBadGateway)
		return
	}

	// Write response
	for k, v := range respHeader.Headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("X-Proxy-Mode", "tunnel")
	w.Header().Set("X-Tunnel-Node", nodeID)
	w.WriteHeader(respHeader.StatusCode)

	if bodyReader != nil {
		io.Copy(w, bodyReader)
	}
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","tunnels":%d}`, g.tunnelServer.Manager().ActiveTunnels())
}

func (g *Gateway) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if g.promMetrics != nil && g.tunnelServer != nil {
		g.promMetrics.tunnelGauge.Set(float64(g.tunnelServer.Manager().ActiveTunnels()))
	}

	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if g.tunnelServer != nil {
			fmt.Fprintf(w, `{"active_tunnels":%d}`, g.tunnelServer.Manager().ActiveTunnels())
		} else {
			w.Write([]byte(`{"active_tunnels":0}`))
		}
		return
	}

	metrics.Handler().ServeHTTP(w, r)
}

// extractClientIP extracts the client IP from the request.
func extractClientIP(r *http.Request) string {
	// Check X-Forwarded-For header
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}

	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
