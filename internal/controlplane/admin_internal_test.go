package controlplane

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
)

func signAdmin(secret, ts, nonce, method, path string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	payload := strings.Join([]string{
		ts,
		nonce,
		method,
		path,
		hex.EncodeToString(bodyHash[:]),
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func newSignedAdminRequest(t *testing.T, cfg *config.ControlPlaneConfig, method, path string, body []byte, nonce string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, "http://example.com"+path, bytes.NewReader(body))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := signAdmin(cfg.AdminHMACSecret, ts, nonce, method, path, body)

	req.Header.Set("X-Admin-KeyId", cfg.AdminHMACKeyID)
	req.Header.Set("X-Admin-Timestamp", ts)
	req.Header.Set("X-Admin-Nonce", nonce)
	req.Header.Set("X-Admin-Signature", sig)
	req.Header.Set("X-Actor-UserId", "u1")
	req.Header.Set("X-Actor-TenantId", "t1")
	req.Header.Set("X-Actor-ProjectId", "p1")
	return req
}

func TestAdminAuthMissingHeaders(t *testing.T) {
	cfg := &config.ControlPlaneConfig{
		AdminHMACKeyID:  "k1",
		AdminHMACSecret: "s1",
	}
	a, err := newAdminAuth(cfg)
	if err != nil {
		t.Fatalf("newAdminAuth: %v", err)
	}

	h := a.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/internal/admin/v1/nodes/n1:disable", nil)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d", w.Code, http.StatusUnauthorized)
	}
}

func TestAdminAuthOK(t *testing.T) {
	cfg := &config.ControlPlaneConfig{
		AdminHMACKeyID:  "k1",
		AdminHMACSecret: "s1",
	}
	a, err := newAdminAuth(cfg)
	if err != nil {
		t.Fatalf("newAdminAuth: %v", err)
	}

	h := a.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	body := []byte(`{"reason":"maintenance","message":"m"}`)
	req := newSignedAdminRequest(t, cfg, http.MethodPost, "/internal/admin/v1/nodes/n1:disable", body, "nonce-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d want=%d", w.Code, http.StatusNoContent)
	}
	if got := w.Header().Get("X-Request-Id"); got == "" {
		t.Fatalf("expected X-Request-Id set")
	}
}

func TestAdminAuthReplayNonce(t *testing.T) {
	cfg := &config.ControlPlaneConfig{
		AdminHMACKeyID:  "k1",
		AdminHMACSecret: "s1",
	}
	a, err := newAdminAuth(cfg)
	if err != nil {
		t.Fatalf("newAdminAuth: %v", err)
	}

	h := a.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	body := []byte(`{"reason":"maintenance","message":"m"}`)
	req1 := newSignedAdminRequest(t, cfg, http.MethodPost, "/internal/admin/v1/nodes/n1:disable", body, "nonce-1")
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, req1)
	if w1.Code != http.StatusNoContent {
		t.Fatalf("first status=%d want=%d", w1.Code, http.StatusNoContent)
	}

	req2 := newSignedAdminRequest(t, cfg, http.MethodPost, "/internal/admin/v1/nodes/n1:disable", body, "nonce-1")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("second status=%d want=%d", w2.Code, http.StatusUnauthorized)
	}
}
