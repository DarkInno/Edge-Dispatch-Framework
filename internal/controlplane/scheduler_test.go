package controlplane

import (
	"testing"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

func TestScore(t *testing.T) {
	s := &Scheduler{}

	tests := []struct {
		name   string
		node   models.Node
		client models.ClientInfo
		want   float64
	}{
		{
			name: "region and ISP match",
			node: models.Node{
				Region: "cn-bj",
				ISP:    "cmcc",
				Scores: models.NodeScores{
					ReachableScore: 100,
					HealthScore:    100,
					RiskScore:      0,
				},
			},
			client: models.ClientInfo{Region: "cn-bj", ISP: "cmcc"},
			want:   100, // 30 + 20 + 30 + 20 - 0 = 100
		},
		{
			name: "region only match",
			node: models.Node{
				Region: "cn-sh",
				Scores: models.NodeScores{
					ReachableScore: 100,
					HealthScore:    100,
					RiskScore:      0,
				},
			},
			client: models.ClientInfo{Region: "cn-sh", ISP: "ctcc"},
			want:   80, // 30 + 0 + 30 + 20 - 0 = 80
		},
		{
			name: "ISP only match",
			node: models.Node{
				Scores: models.NodeScores{
					ReachableScore: 100,
					HealthScore:    100,
					RiskScore:      0,
				},
			},
			client: models.ClientInfo{Region: "cn-bj", ISP: "ctcc"},
			want:   50, // 0 + 0 + 30 + 20 - 0 = 50
		},
		{
			name: "no affinity match",
			node: models.Node{
				Scores: models.NodeScores{
					ReachableScore: 100,
					HealthScore:    100,
					RiskScore:      0,
				},
			},
			client: models.ClientInfo{},
			want:   50, // 0 + 0 + 30 + 20 - 0 = 50
		},
		{
			name: "high risk score",
			node: models.Node{
				Region: "cn-bj",
				ISP:    "cmcc",
				Scores: models.NodeScores{
					ReachableScore: 100,
					HealthScore:    100,
					RiskScore:      60,
				},
			},
			client: models.ClientInfo{Region: "cn-bj", ISP: "cmcc"},
			want:   70, // 30 + 20 + 30 + 20 - 30 = 70
		},
		{
			name: "score clamped at zero",
			node: models.Node{
				Scores: models.NodeScores{
					RiskScore: 200,
				},
			},
			client: models.ClientInfo{},
			want:   0, // 0 + 0 + 0 + 0 - 100 = -100 -> clamp to 0
		},
		{
			name: "partial scores with region",
			node: models.Node{
				Region: "cn-bj",
				Scores: models.NodeScores{
					ReachableScore: 50,
					HealthScore:    80,
					RiskScore:      10,
				},
			},
			client: models.ClientInfo{Region: "cn-bj"},
			want:   56, // 30 + 0 + 15 + 16 - 5 = 56
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.score(tt.node, tt.client)
			if got != tt.want {
				t.Errorf("score() = %.0f, want %.0f", got, tt.want)
			}
		})
	}
}

func TestFilter(t *testing.T) {
	s := &Scheduler{}

	nodes := []*models.Node{
		{
			NodeID: "n1",
			Capabilities: models.Capabilities{
				InboundReachable: true,
			},
			Scores: models.NodeScores{ReachableScore: 100},
		},
		{
			NodeID: "n2",
			Capabilities: models.Capabilities{
				InboundReachable: false,
			},
			Scores: models.NodeScores{ReachableScore: 100},
		},
		{
			NodeID: "n3",
			Capabilities: models.Capabilities{
				InboundReachable: true,
			},
			Scores: models.NodeScores{ReachableScore: 5},
		},
		{
			NodeID: "n4",
			Capabilities: models.Capabilities{
				InboundReachable: true,
			},
			Scores: models.NodeScores{ReachableScore: 10},
		},
	}

	result := s.filter(nil, nodes)

	if len(result) != 2 {
		t.Fatalf("filter() returned %d nodes, want 2", len(result))
	}
	if result[0].NodeID != "n1" || result[1].NodeID != "n4" {
		t.Errorf("filter() got nodes %v, want [n1, n4]", getIDs(result))
	}
}

func TestNoOriginFallback(t *testing.T) {
	t.Run("degrade disabled", func(t *testing.T) {
		s := &Scheduler{
			cfg: &config.ControlPlaneConfig{DegradeToOrigin: false, DefaultTTLMs: 30000},
		}
		resp := s.NoOriginFallback(models.DispatchRequest{})
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if len(resp.Candidates) != 0 {
			t.Errorf("expected 0 candidates when fallback disabled, got %d", len(resp.Candidates))
		}
	})

	t.Run("degrade enabled", func(t *testing.T) {
		s := &Scheduler{
			cfg: &config.ControlPlaneConfig{DegradeToOrigin: true, DefaultTTLMs: 30000},
		}
		resp := s.NoOriginFallback(models.DispatchRequest{
			Resource: models.ResourceInfo{Key: "video/test.mp4"},
		})
		if len(resp.Candidates) != 1 {
			t.Fatalf("expected 1 candidate, got %d", len(resp.Candidates))
		}
		if resp.Candidates[0].ID != "origin" {
			t.Errorf("candidate ID = %q, want 'origin'", resp.Candidates[0].ID)
		}
	})
}

func getIDs(nodes []*models.Node) []string {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.NodeID
	}
	return ids
}
