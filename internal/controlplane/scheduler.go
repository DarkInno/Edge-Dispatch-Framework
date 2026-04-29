package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/darkinno/edge-dispatch-framework/internal/auth"
	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
	"github.com/darkinno/edge-dispatch-framework/internal/streaming"
)

type scoredNode struct {
	node     *models.Node
	score    float64
	endpoint string // pre-computed primary endpoint URL
}

type scoredNodes []scoredNode

func (s scoredNodes) Len() int           { return len(s) }
func (s scoredNodes) Less(i, j int) bool { return s[i].score > s[j].score }
func (s scoredNodes) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

var filteredPool = sync.Pool{
	New: func() any {
		s := make([]*models.Node, 0, 64)
		return &s
	},
}

type Scheduler struct {
	nodeCache    *NodeCache
	contentIndex ContentIndexLookup
	signer       *auth.Signer
	cfg          *config.ControlPlaneConfig
	tunnelMgr    TunnelManager
	streamStrat  *streaming.StreamingStrategy
}

// TunnelManager interface for checking tunnel connectivity.
type TunnelManager interface {
	IsConnected(nodeID string) bool
}

// ContentIndexLookup is the interface for content-aware scheduling.
type ContentIndexLookup interface {
	IsCached(nodeID, key string) (isHot bool, likelyCached bool)
}

func NewScheduler(nodeCache *NodeCache, signer *auth.Signer, cfg *config.ControlPlaneConfig) *Scheduler {
	return &Scheduler{nodeCache: nodeCache, signer: signer, cfg: cfg}
}

// SetContentIndex enables content-aware scheduling.
func (s *Scheduler) SetContentIndex(ci ContentIndexLookup) {
	s.contentIndex = ci
}

// SetTunnelManager sets the tunnel manager for NAT node support (v0.3).
func (s *Scheduler) SetTunnelManager(tm TunnelManager) {
	s.tunnelMgr = tm
}

// SetStreamingStrategy sets the streaming strategy for chunk-aware scoring (v0.4).
func (s *Scheduler) SetStreamingStrategy(ss *streaming.StreamingStrategy) {
	s.streamStrat = ss
}

func (s *Scheduler) Resolve(ctx context.Context, req models.DispatchRequest) (*models.DispatchResponse, error) {
	nodes, err := s.nodeCache.GetActiveNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	filtered := s.filter(ctx, nodes)

	if len(filtered) == 0 {
		slog.Warn("no edge nodes available, degrading to origin")
		return s.NoOriginFallback(req), nil
	}

	rKey := req.Resource.Key
	scored := make([]scoredNode, 0, len(filtered))
	for _, n := range filtered {
		sn := scoredNode{node: n, score: s.scoreKey(n, req.Client, rKey)}
		if n.Capabilities.InboundReachable && len(n.Endpoints) > 0 {
			sn.endpoint = n.Endpoints[0].URL()
		}
		scored = append(scored, sn)
	}

	sort.Sort(scoredNodes(scored))

	maxCandidates := s.cfg.MaxCandidates
	if len(scored) > maxCandidates {
		scored = scored[:maxCandidates]
	}

	candidates := make([]models.Candidate, 0, len(scored))
	for _, sn := range scored {
		proxyMode := "direct"
		if sn.node.Capabilities.TunnelRequired {
			proxyMode = "tunnel"
		}

		candidates = append(candidates, models.Candidate{
			ID:       sn.node.NodeID,
			Endpoint: sn.endpoint,
			Weight:   int(sn.score),
			Meta: models.CandidateMeta{
				Region:    sn.node.Region,
				ISP:       sn.node.ISP,
				ProxyMode: proxyMode,
			},
		})
	}

	exp := time.Now().Add(time.Duration(s.cfg.DefaultTTLMs) * time.Millisecond).Unix()
	ipPrefix := ""
	if req.Client.IP != "" {
		ipPrefix = auth.ComputeIPPrefix(req.Client.IP)
	}
	tokenVal, err := s.signer.Sign(auth.TokenPayload{
		Key:      req.Resource.Key,
		Exp:      exp,
		IPPrefix: ipPrefix,
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

func (s *Scheduler) score(node *models.Node, client models.ClientInfo) float64 {
	return s.scoreKey(node, client, "")
}

func (s *Scheduler) scoreKey(node *models.Node, client models.ClientInfo, resourceKey string) float64 {
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

	if node.Capabilities.TunnelRequired {
		score -= 15.0
	}

	if s.contentIndex != nil && resourceKey != "" {
		isHot, likelyCached := s.contentIndex.IsCached(node.NodeID, resourceKey)
		if isHot {
			score += s.cfg.ContentIndex.HotContentAwareWeight
		} else if likelyCached {
			score += s.cfg.ContentIndex.ContentAwareWeight
		}
	}

	if s.streamStrat != nil && resourceKey != "" {
		score += s.streamStrat.StreamScore(node.NodeID, resourceKey)
	}

	if score < 0 {
		return 0
	}
	return score
}

func (s *Scheduler) filter(ctx context.Context, nodes []*models.Node) []*models.Node {
	filteredPtr := filteredPool.Get().(*[]*models.Node)
	filtered := (*filteredPtr)[:0]

	for _, n := range nodes {
		if n.Status == models.NodeStatusDisabled || n.Status == models.NodeStatusMaintenance {
			continue
		}

		isPublic := n.Capabilities.InboundReachable
		isNAT := n.Capabilities.TunnelRequired

		if isPublic {
			if n.Scores.ReachableScore < 10.0 {
				continue
			}
			filtered = append(filtered, n)
		} else if isNAT && s.tunnelMgr != nil {
			if s.tunnelMgr.IsConnected(n.NodeID) {
				filtered = append(filtered, n)
			}
		}
	}

	result := make([]*models.Node, len(filtered))
	copy(result, filtered)
	*filteredPtr = filtered
	filteredPool.Put(filteredPtr)

	return result
}

func sanitizeResourceKey(key string) string {
	key = strings.TrimSpace(key)
	key = strings.ReplaceAll(key, "\\", "/")
	for _, p := range strings.Split(key, "/") {
		p = strings.TrimSpace(p)
		if p == "." || p == ".." {
			return ""
		}
	}
	return key
}

func (s *Scheduler) NoOriginFallback(req models.DispatchRequest) *models.DispatchResponse {
	if !s.cfg.DegradeToOrigin {
		return &models.DispatchResponse{
			RequestID:  uuid.New().String(),
			TTLMs:      s.cfg.DefaultTTLMs,
			Candidates: nil,
		}
	}

	originURL := s.cfg.OriginURL
	if originURL == "" {
		originURL = "http://localhost:7070"
	}

	return &models.DispatchResponse{
		RequestID: uuid.New().String(),
		TTLMs:     s.cfg.DefaultTTLMs,
		Candidates: []models.Candidate{
			{
				ID:       "origin",
				Endpoint: originURL,
				Weight:   1,
			},
		},
	}
}
