package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

func TestAPIHealthz(t *testing.T) {
	cfg := &config.ControlPlaneConfig{
		TokenSecret:    "test-secret",
		MaxCandidates:  5,
		DefaultTTLMs:   30000,
		DegradeToOrigin: false,
	}

	api := NewAPI(nil, nil, nil, cfg)
	ts := httptest.NewServer(api)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

func makeAuthRequest(method, url, body string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-secret")
	return req, nil
}

func TestAPIHandleRegisterBadRequest(t *testing.T) {
	cfg := &config.ControlPlaneConfig{
		TokenSecret:    "test-secret",
		MaxCandidates:  5,
		DefaultTTLMs:   30000,
		DegradeToOrigin: false,
	}

	api := NewAPI(nil, nil, nil, cfg)
	ts := httptest.NewServer(api)
	defer ts.Close()

	req, err := makeAuthRequest("POST", ts.URL+"/v1/nodes/register", "bad")
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/nodes/register: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAPIWriteError(t *testing.T) {
	api := &API{cfg: &config.ControlPlaneConfig{}}
	w := httptest.NewRecorder()
	api.writeError(w, http.StatusForbidden, "BLOCKED", "test message")

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}

	var errResp models.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp.Error.Code != "BLOCKED" {
		t.Errorf("code = %q, want BLOCKED", errResp.Error.Code)
	}
	if errResp.Error.Message != "test message" {
		t.Errorf("message = %q, want 'test message'", errResp.Error.Message)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		headers    map[string]string
		remoteAddr string
		want       string
	}{
		{
			name:       "X-Forwarded-For single",
			headers:    map[string]string{"X-Forwarded-For": "1.2.3.4"},
			remoteAddr: "10.0.0.1:12345",
			want:       "1.2.3.4",
		},
		{
			name:       "X-Forwarded-For multiple",
			headers:    map[string]string{"X-Forwarded-For": "1.2.3.4, 5.6.7.8"},
			remoteAddr: "10.0.0.1:12345",
			want:       "5.6.7.8",
		},
		{
			name:       "X-Real-IP",
			headers:    map[string]string{"X-Real-IP": "5.6.7.8"},
			remoteAddr: "10.0.0.1:12345",
			want:       "5.6.7.8",
		},
		{
			name:       "RemoteAddr only",
			headers:    map[string]string{},
			remoteAddr: "10.0.0.1:12345",
			want:       "10.0.0.1",
		},
		{
			name:       "IPv6 RemoteAddr",
			headers:    map[string]string{},
			remoteAddr: "[::1]:12345",
			want:       "::1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "/", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			req.RemoteAddr = tt.remoteAddr

			got := clientIP(req)
			if got != tt.want {
				t.Errorf("clientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAPIHandleHeartbeatBadRequest(t *testing.T) {
	cfg := &config.ControlPlaneConfig{
		TokenSecret:    "test-secret",
		MaxCandidates:  5,
		DefaultTTLMs:   30000,
		DegradeToOrigin: false,
	}

	api := NewAPI(nil, nil, nil, cfg)
	ts := httptest.NewServer(api)
	defer ts.Close()

	req, err := makeAuthRequest("POST", ts.URL+"/v1/nodes/heartbeat", "bad")
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/nodes/heartbeat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAPIHandleDispatchBadRequest(t *testing.T) {
	cfg := &config.ControlPlaneConfig{
		TokenSecret:    "test-secret",
		MaxCandidates:  5,
		DefaultTTLMs:   30000,
		DegradeToOrigin: false,
	}

	api := NewAPI(nil, nil, nil, cfg)
	ts := httptest.NewServer(api)
	defer ts.Close()

	req, err := makeAuthRequest("POST", ts.URL+"/v1/dispatch/resolve", "bad")
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/dispatch/resolve: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
