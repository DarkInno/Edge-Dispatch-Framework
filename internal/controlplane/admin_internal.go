package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

type ctxKey int

const (
	ctxKeyAdminRequestID ctxKey = iota
	ctxKeyAdminActor
)

type adminActor struct {
	UserID    string
	TenantID  string
	ProjectID string
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

type adminAuth struct {
	keyID        string
	secret       []byte
	allowedNets  []*net.IPNet
	nonceTTL     time.Duration
	maxSkew      time.Duration
	mu           sync.Mutex
	seenNonces   map[string]time.Time
	lastCleanup  time.Time
	cleanupEvery time.Duration
}

func newAdminAuth(cfg *config.ControlPlaneConfig) (*adminAuth, error) {
	a := &adminAuth{
		keyID:        cfg.AdminHMACKeyID,
		secret:       []byte(cfg.AdminHMACSecret),
		nonceTTL:     10 * time.Minute,
		maxSkew:      5 * time.Minute,
		seenNonces:   make(map[string]time.Time),
		cleanupEvery: 1 * time.Minute,
	}
	for _, cidr := range cfg.AdminAllowedCIDRs {
		_, ipnet, err := net.ParseCIDR(strings.TrimSpace(cidr))
		if err != nil {
			return nil, err
		}
		a.allowedNets = append(a.allowedNets, ipnet)
	}
	return a, nil
}

func (a *adminAuth) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := net.ParseIP(clientIP(r))
		if len(a.allowedNets) > 0 {
			if ip == nil || !a.ipAllowed(ip) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":{"code":"FORBIDDEN","message":"client ip not allowed"}}`))
				return
			}
		}

		keyID := strings.TrimSpace(r.Header.Get("X-Admin-KeyId"))
		tsRaw := strings.TrimSpace(r.Header.Get("X-Admin-Timestamp"))
		nonce := strings.TrimSpace(r.Header.Get("X-Admin-Nonce"))
		sigHex := strings.TrimSpace(r.Header.Get("X-Admin-Signature"))
		if keyID == "" || tsRaw == "" || nonce == "" || sigHex == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"missing admin auth headers"}}`))
			return
		}
		if keyID != a.keyID {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"code":"FORBIDDEN","message":"invalid key id"}}`))
			return
		}

		ts, err := strconv.ParseInt(tsRaw, 10, 64)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"code":"BAD_REQUEST","message":"invalid timestamp"}}`))
			return
		}
		now := time.Now()
		t := time.Unix(ts, 0)
		if t.After(now.Add(a.maxSkew)) || t.Before(now.Add(-a.maxSkew)) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"timestamp out of range"}}`))
			return
		}

		if !a.checkAndRememberNonce(nonce, now) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"replayed nonce"}}`))
			return
		}

		var body []byte
		if r.Body != nil {
			body, err = io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":{"code":"BAD_REQUEST","message":"failed to read body"}}`))
				return
			}
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		bodyHash := sha256.Sum256(body)
		payload := strings.Join([]string{
			tsRaw,
			nonce,
			r.Method,
			r.URL.Path,
			hex.EncodeToString(bodyHash[:]),
		}, "\n")
		mac := hmac.New(sha256.New, a.secret)
		mac.Write([]byte(payload))
		want := mac.Sum(nil)

		got, err := hex.DecodeString(sigHex)
		if err != nil || !hmac.Equal(got, want) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"invalid signature"}}`))
			return
		}

		actor := adminActor{
			UserID:    strings.TrimSpace(r.Header.Get("X-Actor-UserId")),
			TenantID:  strings.TrimSpace(r.Header.Get("X-Actor-TenantId")),
			ProjectID: strings.TrimSpace(r.Header.Get("X-Actor-ProjectId")),
		}
		if actor.UserID == "" || actor.TenantID == "" || actor.ProjectID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"code":"BAD_REQUEST","message":"missing actor headers"}}`))
			return
		}

		reqID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if reqID == "" {
			reqID = uuid.New().String()
		}
		w.Header().Set("X-Request-Id", reqID)

		ctx := context.WithValue(r.Context(), ctxKeyAdminRequestID, reqID)
		ctx = context.WithValue(ctx, ctxKeyAdminActor, actor)
		r = r.WithContext(ctx)

		rec := &statusRecorder{ResponseWriter: w, status: 200}
		start := time.Now()
		next.ServeHTTP(rec, r)

		slog.Info("admin request",
			"request_id", reqID,
			"actor_user_id", actor.UserID,
			"actor_tenant_id", actor.TenantID,
			"actor_project_id", actor.ProjectID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"remote_ip", clientIP(r),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func (a *adminAuth) ipAllowed(ip net.IP) bool {
	for _, n := range a.allowedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (a *adminAuth) checkAndRememberNonce(nonce string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.lastCleanup.IsZero() {
		a.lastCleanup = now
	}
	if now.Sub(a.lastCleanup) >= a.cleanupEvery {
		cutoff := now.Add(-a.nonceTTL)
		for k, t := range a.seenNonces {
			if t.Before(cutoff) {
				delete(a.seenNonces, k)
			}
		}
		a.lastCleanup = now
	}

	if _, ok := a.seenNonces[nonce]; ok {
		return false
	}
	a.seenNonces[nonce] = now
	return true
}

type disableNodeRequest struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Until   string `json:"until"`
}

func (a *API) handleAdminDisableNode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "nodeID")
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)

	var req disableNodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "reason is required")
		return
	}

	if _, err := a.registry.GetNode(r.Context(), nodeID); err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "node not found")
		return
	}
	if err := a.registry.UpdateStatus(r.Context(), nodeID, models.NodeStatusDisabled); err != nil {
		slog.Error("disable node failed", "node_id", nodeID, "err", err)
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
		return
	}
	if a.scheduler != nil && a.scheduler.nodeCache != nil {
		a.scheduler.nodeCache.Invalidate()
	}
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) handleAdminEnableNode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "nodeID")
	if _, err := a.registry.GetNode(r.Context(), nodeID); err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "node not found")
		return
	}
	if err := a.registry.UpdateStatus(r.Context(), nodeID, models.NodeStatusActive); err != nil {
		slog.Error("enable node failed", "node_id", nodeID, "err", err)
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
		return
	}
	if a.scheduler != nil && a.scheduler.nodeCache != nil {
		a.scheduler.nodeCache.Invalidate()
	}
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) handleAdminRevokeNode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "nodeID")
	if err := a.registry.RevokeNode(r.Context(), nodeID); err != nil {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "node not found")
		return
	}
	if a.scheduler != nil && a.scheduler.nodeCache != nil {
		a.scheduler.nodeCache.Invalidate()
	}
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
