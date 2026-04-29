package controlplane

import (
	"context"
	"testing"

	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

func TestSchedulerFilterSkipsDisabled(t *testing.T) {
	s := &Scheduler{}
	nodes := []*models.Node{
		{
			NodeID: "n1",
			Status: models.NodeStatusDisabled,
			Capabilities: models.Capabilities{
				InboundReachable: true,
			},
			Scores: models.NodeScores{ReachableScore: 100},
		},
		{
			NodeID: "n2",
			Status: models.NodeStatusActive,
			Capabilities: models.Capabilities{
				InboundReachable: true,
			},
			Scores: models.NodeScores{ReachableScore: 100},
		},
	}

	got := s.filter(context.Background(), nodes)
	if len(got) != 1 || got[0].NodeID != "n2" {
		t.Fatalf("filtered=%v want=[n2]", []string{func() string {
			if len(got) == 0 {
				return ""
			}
			return got[0].NodeID
		}()})
	}
}
