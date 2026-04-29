package controlplane

import (
	"testing"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

func BenchmarkScore(b *testing.B) {
	s := &Scheduler{cfg: &config.ControlPlaneConfig{}}
	node := models.Node{
		Region: "cn-bj",
		ISP:    "cmcc",
		Scores: models.NodeScores{
			ReachableScore: 85,
			HealthScore:    90,
			RiskScore:      5,
		},
	}
	client := models.ClientInfo{Region: "cn-bj", ISP: "cmcc"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.score(&node, client)
	}
}

func BenchmarkFilter(b *testing.B) {
	s := &Scheduler{cfg: &config.ControlPlaneConfig{}}
	nodes := make([]*models.Node, 100)
	for i := range nodes {
		nodes[i] = &models.Node{
			NodeID: "n" + string(rune(i)),
			Capabilities: models.Capabilities{
				InboundReachable: i%3 != 0,
			},
			Scores: models.NodeScores{ReachableScore: float64(i % 20)},
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.filter(nil, nodes)
	}
}
