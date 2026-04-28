package controlplane

import (
	"testing"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

type mockContentIndex struct {
	data map[string]struct {
		isHot       bool
		likelyCached bool
	}
}

func (m *mockContentIndex) IsCached(nodeID, key string) (bool, bool) {
	if m == nil {
		return false, false
	}
	if v, ok := m.data[nodeID+":"+key]; ok {
		return v.isHot, v.likelyCached
	}
	return false, false
}

func TestContentAwareScoring(t *testing.T) {
	cfg := &config.ControlPlaneConfig{
		ContentIndex: config.ContentIndexConfig{
			ContentAwareWeight:     10.0,
			HotContentAwareWeight:  25.0,
		},
	}
	s := &Scheduler{cfg: cfg, contentIndex: &mockContentIndex{
		data: map[string]struct {
			isHot       bool
			likelyCached bool
		}{
			"node1:hotfile.txt": {isHot: true, likelyCached: true},
			"node1:cold.txt":    {isHot: false, likelyCached: true},
		},
	}}

	node := models.Node{
		NodeID: "node1",
		Region: "us-east",
		ISP:    "comcast",
		Scores: models.NodeScores{
			ReachableScore: 50,
			HealthScore:    50,
		},
	}
	client := models.ClientInfo{Region: "us-east", ISP: "comcast"}

	// Base score = 30 (region) + 20 (ISP) + 15 (reachable) + 10 (health) = 75
	baseScore := s.scoreKey(node, client, "")

	// Hot key should get +25 bonus
	hotScore := s.scoreKey(node, client, "hotfile.txt")
	if hotScore <= baseScore {
		t.Fatalf("hot key score (%.1f) should be > base score (%.1f)", hotScore, baseScore)
	}
	if hotScore != baseScore+25.0 {
		t.Fatalf("expected hot score %.1f, got %.1f", baseScore+25.0, hotScore)
	}

	// Cold key (bloom only) should get +10 bonus
	coldScore := s.scoreKey(node, client, "cold.txt")
	if coldScore != baseScore+10.0 {
		t.Fatalf("expected cold score %.1f, got %.1f", baseScore+10.0, coldScore)
	}

	// Unknown key gets no bonus
	unknownScore := s.scoreKey(node, client, "unknown.txt")
	if unknownScore != baseScore {
		t.Fatalf("expected unknown score %.1f, got %.1f", baseScore, unknownScore)
	}
}

func TestContentAwareScoringDisabled(t *testing.T) {
	s := &Scheduler{cfg: &config.ControlPlaneConfig{}, contentIndex: nil}

	node := models.Node{
		NodeID: "node1",
		Region: "us-east",
		ISP:    "comcast",
		Scores: models.NodeScores{
			ReachableScore: 50,
			HealthScore:    50,
		},
	}
	client := models.ClientInfo{Region: "us-east", ISP: "comcast"}

	score := s.scoreKey(node, client, "somefile.txt")
	if score != 75.0 {
		t.Fatalf("expected 75.0, got %.1f", score)
	}
}
