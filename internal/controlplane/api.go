package controlplane

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/metrics"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

const maxJSONBodySize = 32 << 10

var bufPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

type API struct {
	registry  *Registry
	heartbeat *Heartbeat
	scheduler *Scheduler
	policy    *Policy
	cfg       *config.ControlPlaneConfig
	metrics   *apiMetrics
}

type apiMetrics struct {
	dispatchTotal    *metrics.CounterFn
	dispatchErrors   *metrics.CounterFn
	heartbeatTotal   *metrics.CounterFn
	registerTotal    *metrics.CounterFn
	nodeStatusGauge  *metrics.GaugeFn
	rateLimitedTotal *metrics.CounterFn
}

type rateLimiter struct {
	rate     int64
	burst    int64
	tokens   atomic.Int64
	lastTime atomic.Int64
}

func newRateLimiter(rate, burst int64) *rateLimiter {
	rl := &rateLimiter{rate: rate, burst: burst}
	rl.tokens.Store(burst)
	rl.lastTime.Store(time.Now().UnixNano())
	return rl
}

func (rl *rateLimiter) Allow() bool {
	now := time.Now().UnixNano()
	last := rl.lastTime.Load()
	elapsed := now - last
	newTokens := elapsed * rl.rate / int64(time.Second)
	if newTokens <= 0 {
		tokens := rl.tokens.Load()
		if tokens <= 0 {
			return false
		}
		rl.tokens.Add(-1)
		return true
	}
	for {
		old := rl.tokens.Load()
		nv := old + newTokens
		if nv > rl.burst {
			nv = rl.burst
		}
		if nv <= 0 {
			return false
		}
		if rl.tokens.CompareAndSwap(old, nv-1) {
			rl.lastTime.Store(now)
			return true
		}
	}
}

func withRateLimit(rl *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !rl.Allow() {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":{"code":"RATE_LIMITED","message":"too many requests"}}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func withAPICOMmonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

type apiKeyAuth struct {
	validKeys map[string]bool
	enabled   bool
}

func newAPIKeyAuth(secret string) *apiKeyAuth {
	a := &apiKeyAuth{validKeys: make(map[string]bool)}
	if secret != "" {
		a.enabled = true
		a.validKeys[secret] = true
	}
	return a
}

func (a *apiKeyAuth) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.enabled {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"missing or invalid authorization"}}`))
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		if !a.validKeys[token] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"code":"FORBIDDEN","message":"invalid api key"}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func NewAPI(registry *Registry, heartbeat *Heartbeat, scheduler *Scheduler, cfg *config.ControlPlaneConfig) http.Handler {
	api := &API{
		registry:  registry,
		heartbeat: heartbeat,
		scheduler: scheduler,
		policy:    NewPolicy(),
		cfg:       cfg,
		metrics:   newAPIMetrics(),
	}

	r := chi.NewRouter()
	rl := newRateLimiter(1000, 2000)
	r.Use(withRateLimit(rl))
	r.Use(withAPICOMmonHeaders)

	auth := newAPIKeyAuth(cfg.TokenSecret)
	r.Group(func(r chi.Router) {
		r.Use(auth.middleware)
		r.Post("/v1/nodes/register", api.handleRegister)
		r.Post("/v1/nodes/heartbeat", api.handleHeartbeat)
		r.Get("/v1/nodes/{nodeID}", api.handleGetNode)
		r.Delete("/v1/nodes/{nodeID}", api.handleRevokeNode)
		r.Post("/v1/dispatch/resolve", api.handleDispatch)
		r.Get("/obj/{key}", api.handleObjectIngress)
	})
	r.Get("/healthz", api.handleHealthz)
	r.Get("/metrics", api.handleMetrics)

	return r
}

func newAPIMetrics() *apiMetrics {
	return &apiMetrics{
		dispatchTotal:    metrics.NewCounter("controlplane_dispatch_requests_total", "Total number of dispatch requests"),
		dispatchErrors:   metrics.NewCounter("controlplane_dispatch_errors_total", "Total number of dispatch errors"),
		heartbeatTotal:   metrics.NewCounter("controlplane_heartbeat_total", "Total number of heartbeat requests"),
		registerTotal:    metrics.NewCounter("controlplane_register_total", "Total number of node registrations"),
		nodeStatusGauge:  metrics.NewGauge("controlplane_node_status", "Current node status count", "status"),
		rateLimitedTotal: metrics.NewCounter("controlplane_rate_limited_total", "Total number of rate-limited requests"),
	}
}

func (a *API) writeJSON(w http.ResponseWriter, status int, v any) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	if err := json.NewEncoder(buf).Encode(v); err != nil {
		slog.Error("write json", "error", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (a *API) writeError(w http.ResponseWriter, status int, code, message string) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	if err := json.NewEncoder(buf).Encode(models.ErrorResponse{
		Error: models.ErrorDetail{
			Code:    code,
			Message: message,
		},
	}); err != nil {
		slog.Error("write error", "error", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.LastIndex(xff, ","); idx > 0 {
			return strings.TrimSpace(xff[idx+1:])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	if host := r.RemoteAddr; host != "" {
		if len(host) > 0 && host[0] == '[' {
			if idx := strings.IndexByte(host, ']'); idx > 0 {
				return host[1:idx]
			}
			return host[1:]
		}
		if idx := strings.LastIndexByte(host, ':'); idx > 0 {
			return host[:idx]
		}
		return host
	}
	return ""
}

var ipCache sync.Map

func cachedClientIP(r *http.Request) string {
	raw := r.Header.Get("X-Forwarded-For")
	if raw == "" {
		raw = r.Header.Get("X-Real-IP")
	}
	if raw == "" {
		raw = r.RemoteAddr
	}
	if raw == "" {
		return ""
	}
	if v, ok := ipCache.Load(raw); ok {
		return v.(string)
	}
	ip := clientIP(r)
	ipCache.Store(raw, ip)
	return ip
}

func (a *API) handleRegister(w http.ResponseWriter, r *http.Request) {
	a.metrics.registerTotal.Inc()
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)

	var req models.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if err.Error() == "http: request body too large" {
			a.writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE", "request body exceeds 32KB limit")
			return
		}
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	req.NodeName = strings.TrimSpace(req.NodeName)
	req.Region = strings.TrimSpace(req.Region)
	req.ISP = strings.TrimSpace(req.ISP)
	if req.NodeName == "" {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "node_name is required")
		return
	}

	resp, err := a.registry.Register(r.Context(), req)
	if err != nil {
		slog.Error("register failed", "err", err)
		a.writeError(w, http.StatusInternalServerError, "REGISTER_FAILED", "internal server error")
		return
	}

	a.writeJSON(w, http.StatusCreated, resp)
}

func (a *API) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)

	var req models.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			a.writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE", "request body exceeds 32KB limit")
			return
		}
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	req.NodeID = strings.TrimSpace(req.NodeID)
	if req.NodeID == "" {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "node_id is required")
		return
	}

	a.metrics.heartbeatTotal.Inc()

	if err := a.heartbeat.ProcessHeartbeat(r.Context(), req); err != nil {
		slog.Error("heartbeat failed", "err", err)
		a.writeError(w, http.StatusInternalServerError, "HEARTBEAT_FAILED", "internal server error")
		return
	}

	a.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) handleGetNode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "nodeID")

	node, err := a.registry.GetNode(r.Context(), nodeID)
	if err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "node not found")
		return
	}

	a.writeJSON(w, http.StatusOK, node)
}

func (a *API) handleRevokeNode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "nodeID")

	if err := a.registry.RevokeNode(r.Context(), nodeID); err != nil {
		slog.Error("revoke node failed", "err", err)
		a.writeError(w, http.StatusInternalServerError, "REVOKE_FAILED", "internal server error")
		return
	}

	a.writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (a *API) handleDispatch(w http.ResponseWriter, r *http.Request) {
	a.metrics.dispatchTotal.Inc()
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		a.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)

	var req models.DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			a.writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE", "request body exceeds 32KB limit")
			return
		}
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	req.Client.IP = strings.TrimSpace(req.Client.IP)
	if req.Client.IP == "" {
		req.Client.IP = cachedClientIP(r)
	}
	req.Resource.Key = strings.TrimSpace(req.Resource.Key)
	if req.Resource.Key == "" {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "resource key is required")
		return
	}

	if a.policy.IsBlocked(req.Client.IP) {
		a.writeError(w, http.StatusForbidden, "BLOCKED", "client IP is blocked")
		return
	}

	resp, err := a.scheduler.Resolve(r.Context(), req)
	if err != nil {
		a.metrics.dispatchErrors.Inc()
		slog.Error("dispatch failed", "err", err)
		a.writeError(w, http.StatusInternalServerError, "DISPATCH_FAILED", "internal server error")
		return
	}

	if len(resp.Candidates) == 0 {
		a.metrics.dispatchErrors.Inc()
	}

	a.writeJSON(w, http.StatusOK, resp)
}

func (a *API) handleObjectIngress(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	key = sanitizeResourceKey(key)

	req := models.DispatchRequest{
		Client: models.ClientInfo{
			IP: cachedClientIP(r),
		},
		Resource: models.ResourceInfo{
			Type:   "object",
			Key:    key,
			Scheme: "https",
		},
	}

	resp, err := a.scheduler.Resolve(r.Context(), req)
	if err != nil || len(resp.Candidates) == 0 {
		a.writeError(w, http.StatusServiceUnavailable, "NO_EDGE", "no edge nodes available")
		return
	}

	top := resp.Candidates[0]
	redirectURL := fmt.Sprintf("%s/obj/%s", top.Endpoint, key)
	if resp.Token.Value != "" {
		redirectURL += "?token=" + url.QueryEscape(resp.Token.Value)
	}
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func (a *API) handleHealthz(w http.ResponseWriter, r *http.Request) {
	a.writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if a.metrics != nil {
		a.metrics.nodeStatusGauge.Set(float64(a.registry.CountByStatus(string(models.NodeStatusActive))), "active")
		a.metrics.nodeStatusGauge.Set(float64(a.registry.CountByStatus(string(models.NodeStatusDegraded))), "degraded")
		a.metrics.nodeStatusGauge.Set(float64(a.registry.CountByStatus(string(models.NodeStatusOffline))), "offline")
		a.metrics.nodeStatusGauge.Set(float64(a.registry.CountByStatus(string(models.NodeStatusRegistered))), "registered")
		a.metrics.nodeStatusGauge.Set(float64(a.registry.CountByStatus(string(models.NodeStatusQuarantined))), "quarantined")
	}
	metrics.Handler().ServeHTTP(w, r)
}
