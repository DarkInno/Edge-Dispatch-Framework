package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/darkinno/edge-dispatch-framework/internal/auth"
	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
	"github.com/darkinno/edge-dispatch-framework/internal/store"
)

type Scheduler struct {
	pg     *store.PGStore
	signer *auth.Signer
	cfg    *config.ControlPlaneConfig
}

func NewScheduler(pg *store.PGStore, signer *auth.Signer, cfg *config.ControlPlaneConfig) *Scheduler {
	return &Scheduler{pg: pg, signer: signer, cfg: cfg}
}

func (s *Scheduler) Resolve(ctx context.Context, req models.DispatchRequest) (*models.DispatchResponse, error) {
	nodes, err := s.pg.ListActiveNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	filtered := s.filter(ctx, nodes)

	if len(filtered) == 0 {
		slog.Warn("no edge nodes available, degrading to origin")
		return s.NoOriginFallback(req), nil
	}

	type scoredNode struct {
		node  models.Node
		score float64
	}

	scored := make([]scoredNode, len(filtered))
	for i, n := range filtered {
		scored[i] = scoredNode{node: n, score: s.score(n, req.Client)}
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	maxCandidates := s.cfg.MaxCandidates
	if len(scored) > maxCandidates {
		scored = scored[:maxCandidates]
	}

	candidates := make([]models.Candidate, 0, len(scored))
	for _, sn := range scored {
		endpoint := ""
		if len(sn.node.Endpoints) > 0 {
			ep := sn.node.Endpoints[0]
			endpoint = fmt.Sprintf("%s://%s:%d", ep.Scheme, ep.Host, ep.Port)
		}
		candidates = append(candidates, models.Candidate{
			ID:       sn.node.NodeID,
			Endpoint: endpoint,
			Weight:   int(sn.score),
			Meta: models.CandidateMeta{
				Region: sn.node.Region,
				ISP:    sn.node.ISP,
			},
		})
	}

	exp := time.Now().Add(time.Duration(s.cfg.DefaultTTLMs) * time.Millisecond).Unix()
	tokenVal, err := s.signer.Sign(auth.TokenPayload{
		Key: req.Resource.Key,
		Exp: exp,
	})
	if err != nil {
		return nil, fmt.Errorf("sign token: %w", err)
	}

	return &models.DispatchResponse{
		RequestID: uuid.New().String(),
		TTLMs:     s.cfg.DefaultTTLMs,
		Token: models.DispatchToken{
			Type:  "hmac",
			Value: tokenVal,
			Exp:   exp,
		},
		Candidates: candidates,
	}, nil
}

func (s *Scheduler) score(node models.Node, client models.ClientInfo) float64 {
	score := 0.0

	if node.Region == client.Region && client.Region != "" {
		score += 30.0
	}
	if node.ISP == client.ISP && client.ISP != "" {
		score += 20.0
	}

	score += node.Scores.ReachableScore * 0.3
	score += node.Scores.HealthScore * 0.2
	score -= node.Scores.RiskScore * 0.5

	return math.Max(score, 0)
}

func (s *Scheduler) filter(ctx context.Context, nodes []models.Node) []models.Node {
	filtered := make([]models.Node, 0, len(nodes))
	for _, n := range nodes {
		if !n.Capabilities.InboundReachable {
			continue
		}
		if n.Scores.ReachableScore < 10.0 {
			continue
		}
		filtered = append(filtered, n)
	}
	return filtered
}

func (s *Scheduler) NoOriginFallback(req models.DispatchRequest) *models.DispatchResponse {
	if !s.cfg.DegradeToOrigin {
		return &models.DispatchResponse{
			RequestID:  uuid.New().String(),
			TTLMs:      s.cfg.DefaultTTLMs,
			Candidates: nil,
		}
	}

	return &models.DispatchResponse{
		RequestID: uuid.New().String(),
		TTLMs:     s.cfg.DefaultTTLMs,
		Candidates: []models.Candidate{
			{
				ID:       "origin",
				Endpoint: "http://localhost:7070/" + req.Resource.Key,
				Weight:   1,
			},
		},
	}
}
