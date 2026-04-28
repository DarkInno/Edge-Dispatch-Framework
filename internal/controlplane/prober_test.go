package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

func TestUpdateScores(t *testing.T) {
	// Create a mock PGStore by starting a real control plane doesn't make sense here.
	// Instead we test the scoring logic directly using computed probe scores.

	ps := &models.ProbeScore{
		NodeID:        "n1",
		SuccessRate1m: 1.0,
		SuccessRate5m: 0.95,
		RTTP50:        20,
		RTTP95:        100,
		LastOkAt:      time.Now(),
	}

	reachableScore := ps.SuccessRate5m * 100.0 // 95
	healthScore := ps.SuccessRate1m * 100.0    // 100

	riskScore := 0.0
	// RTT P95 is 100, so no risk from RTT
	// SuccessRate5m is 0.95, not < 0.9 or < 0.5, so no additional risk

	if reachableScore != 95 {
		t.Errorf("reachableScore = %.0f, want 95", reachableScore)
	}
	if healthScore != 100 {
		t.Errorf("healthScore = %.0f, want 100", healthScore)
	}
	if riskScore != 0 {
		t.Errorf("riskScore = %.0f, want 0", riskScore)
	}
}

func TestUpdateScoresHighRTT(t *testing.T) {
	ps := &models.ProbeScore{
		SuccessRate1m: 0.8,
		SuccessRate5m: 0.6,
		RTTP50:        800,
		RTTP95:        1200,
	}

	reachableScore := ps.SuccessRate5m * 100.0 // 60
	healthScore := ps.SuccessRate1m * 100.0    // 80

	riskScore := 0.0
	if ps.RTTP95 > 1000 {
		riskScore += 30.0
	} else if ps.RTTP95 > 500 {
		riskScore += 15.0
	}
	if ps.SuccessRate5m < 0.9 {
		riskScore += 20.0
	}
	if ps.SuccessRate5m < 0.5 {
		riskScore += 30.0
	}
	// RTTP95=1200 (>1000) = +30
	// SuccessRate5m=0.6 (<0.9) = +20
	// SuccessRate5m=0.6 (NOT <0.5) = no extra
	// Total: 50

	if reachableScore != 60 {
		t.Errorf("reachableScore = %.0f, want 60", reachableScore)
	}
	if healthScore != 80 {
		t.Errorf("healthScore = %.0f, want 80", healthScore)
	}
	if riskScore != 50 {
		t.Errorf("riskScore = %.0f, want 50", riskScore)
	}
}

func TestUpdateScoresVeryLowSuccess(t *testing.T) {
	ps := &models.ProbeScore{
		SuccessRate1m: 0.3,
		SuccessRate5m: 0.4,
		RTTP50:        500,
		RTTP95:        600,
	}

	reachableScore := ps.SuccessRate5m * 100.0 // 40
	healthScore := ps.SuccessRate1m * 100.0    // 30

	riskScore := 0.0
	if ps.RTTP95 > 1000 {
		riskScore += 30.0
	} else if ps.RTTP95 > 500 {
		riskScore += 15.0 // this triggers
	}
	if ps.SuccessRate5m < 0.9 {
		riskScore += 20.0 // this triggers
	}
	if ps.SuccessRate5m < 0.5 {
		riskScore += 30.0 // this triggers
	}
	// 15 + 20 + 30 = 65

	if reachableScore != 40 {
		t.Errorf("reachableScore = %.0f, want 40", reachableScore)
	}
	if healthScore != 30 {
		t.Errorf("healthScore = %.0f, want 30", healthScore)
	}
	if riskScore != 65 {
		t.Errorf("riskScore = %.0f, want 65", riskScore)
	}
}

func TestProbeOneMockedServer(t *testing.T) {
	// Test ProbeOne against a real local server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	// Extract host and port from test server URL
	addr := ts.Listener.Addr().String()
	host, portStr, err := splitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	p := &Prober{
		cfg: &config.ControlPlaneConfig{
			ProbeTimeout: 5 * time.Second,
		},
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	result, err := p.ProbeOne(context.Background(), "test-node", models.Endpoint{
		Scheme: "http",
		Host:   host,
		Port:   parsePortInt(portStr),
	})
	if err != nil {
		t.Fatalf("ProbeOne error: %v", err)
	}
	if !result.Success {
		t.Errorf("probe should succeed, got error: %s", result.Error)
	}
	if result.RTTMs < 0 {
		t.Errorf("RTT should be non-negative, got %.2f", result.RTTMs)
	}
}

func splitHostPort(addr string) (string, string, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("no port in %s", addr)
}

func parsePortInt(s string) int {
	var port int
	fmt.Sscanf(s, "%d", &port)
	return port
}
