package controlplane

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

type API struct {
	registry  *Registry
	heartbeat *Heartbeat
	scheduler *Scheduler
	policy    *Policy
	cfg       *config.ControlPlaneConfig
}

func NewAPI(registry *Registry, heartbeat *Heartbeat, scheduler *Scheduler, cfg *config.ControlPlaneConfig) http.Handler {
	api := &API{
		registry:  registry,
		heartbeat: heartbeat,
		scheduler: scheduler,
		policy:    NewPolicy(),
		cfg:       cfg,
	}

	r := chi.NewRouter()
	r.Post("/v1/nodes/register", api.handleRegister)
	r.Post("/v1/nodes/heartbeat", api.handleHeartbeat)
	r.Get("/v1/nodes/{nodeID}", api.handleGetNode)
	r.Delete("/v1/nodes/{nodeID}", api.handleRevokeNode)
	r.Post("/v1/dispatch/resolve", api.handleDispatch)
	r.Get("/obj/{key}", api.handleObjectIngress)
	r.Get("/healthz", api.handleHealthz)

	return r
}

func (a *API) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json", "error", err)
	}
}

func (a *API) writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(models.ErrorResponse{
		Error: models.ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.IndexByte(xff, ','); idx > 0 {
			return strings.TrimSpace(xff[:idx])
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

func (a *API) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req models.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	resp, err := a.registry.Register(r.Context(), req)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "REGISTER_FAILED", err.Error())
		return
	}

	a.writeJSON(w, http.StatusCreated, resp)
}

func (a *API) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req models.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if err := a.heartbeat.ProcessHeartbeat(r.Context(), req); err != nil {
		a.writeError(w, http.StatusInternalServerError, "HEARTBEAT_FAILED", err.Error())
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
		a.writeError(w, http.StatusInternalServerError, "REVOKE_FAILED", err.Error())
		return
	}

	a.writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (a *API) handleDispatch(w http.ResponseWriter, r *http.Request) {
	var req models.DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if req.Client.IP == "" {
		req.Client.IP = clientIP(r)
	}

	if a.policy.IsBlocked(req.Client.IP) {
		a.writeError(w, http.StatusForbidden, "BLOCKED", "client IP is blocked")
		return
	}

	resp, err := a.scheduler.Resolve(r.Context(), req)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "DISPATCH_FAILED", err.Error())
		return
	}

	a.writeJSON(w, http.StatusOK, resp)
}

func (a *API) handleObjectIngress(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")

	req := models.DispatchRequest{
		Client: models.ClientInfo{
			IP: clientIP(r),
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
	redirectURL := fmt.Sprintf("%s/%s", top.Endpoint, key)
	if resp.Token.Value != "" {
		redirectURL += "?token=" + resp.Token.Value
	}
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func (a *API) handleHealthz(w http.ResponseWriter, r *http.Request) {
	a.writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}
