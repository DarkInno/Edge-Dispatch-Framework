package controlplane

import (
	"container/heap"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/auth"
	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
	"github.com/darkinno/edge-dispatch-framework/internal/streaming"
)

type scoredNode struct {
	node     *models.Node
	score    float64
	endpoint string
}

type scoredHeap struct {
	nodes []scoredNode
	max   int
}

func (h *scoredHeap) Len() int           { return len(h.nodes) }
func (h *scoredHeap) Less(i, j int) bool { return h.nodes[i].score < h.nodes[j].score }
func (h *scoredHeap) Swap(i, j int)      { h.nodes[i], h.nodes[j] = h.nodes[j], h.nodes[i] }

func (h *scoredHeap) Push(x any) {
	h.nodes = append(h.nodes, x.(scoredNode))
}

func (h *scoredHeap) Pop() any {
	old := h.nodes
	n := len(old)
	x := old[n-1]
	h.nodes = old[:n-1]
	return x
}

func (h *scoredHeap) push(sn scoredNode) {
	if h.max > 0 && len(h.nodes) < h.max {
		heap.Push(h, sn)
		return
	}
	if h.max > 0 && sn.score > h.nodes[0].score {
		h.nodes[0] = sn
		heap.Fix(h, 0)
	}
}

func (h *scoredHeap) sorted() []scoredNode {
	result := make([]scoredNode, 0, len(h.nodes))
	for len(h.nodes) > 0 {
		result = append(result, heap.Pop(h).(scoredNode))
	}
	return result
}

var requestSeq atomic.Uint64

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
	FindNodesWithKey(key string) (hotNodes, bloomNodes map[string]bool)
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

	var streamKey string
	var hotNodes, bloomNodes map[string]bool
	var streamNodes map[string]bool

	if s.contentIndex != nil && rKey != "" {
		hotNodes, bloomNodes = s.contentIndex.FindNodesWithKey(rKey)
	}
	if s.streamStrat != nil && rKey != "" {
		streamKey, _, _ = streaming.InferStreamFromChunkKey(rKey)
		if streamKey != "" {
			streamNodes = s.streamStrat.FindNodesWithStream(streamKey)
		}
	}

	maxCandidates := s.cfg.MaxCandidates
	h := &scoredHeap{max: maxCandidates}
	for _, n := range filtered {
		sn := scoredNode{node: n, score: s.scoreKeyFast(n, req.Client, rKey, hotNodes, bloomNodes, streamNodes)}
		if n.Capabilities.InboundReachable && len(n.Endpoints) > 0 {
			sn.endpoint = n.Endpoints[0].URL()
		}
		h.push(sn)
	}

	scored := h.sorted()
	candidates := make([]models.Candidate, 0, len(scored))
	for i := len(scored) - 1; i >= 0; i-- {
		sn := scored[i]
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
		RequestID: fmt.Sprintf("%x-%x", time.Now().UnixNano(), requestSeq.Add(1)),
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
	return s.scoreKeyFast(node, client, resourceKey, nil, nil, nil)
}

func (s *Scheduler) scoreKeyFast(node *models.Node, client models.ClientInfo, resourceKey string, hotNodes, bloomNodes, streamNodes map[string]bool) float64 {
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

	if resourceKey != "" {
		if hotNodes != nil || bloomNodes != nil {
			if hotNodes[node.NodeID] {
				score += s.cfg.ContentIndex.HotContentAwareWeight
			} else if bloomNodes[node.NodeID] {
				score += s.cfg.ContentIndex.ContentAwareWeight
			}
		} else if s.contentIndex != nil {
			isHot, likelyCached := s.contentIndex.IsCached(node.NodeID, resourceKey)
			if isHot {
				score += s.cfg.ContentIndex.HotContentAwareWeight
			} else if likelyCached {
				score += s.cfg.ContentIndex.ContentAwareWeight
			}
		}

		if streamNodes != nil {
			if streamNodes[node.NodeID] {
				score += 20.0
			}
		} else if s.streamStrat != nil {
			score += s.streamStrat.StreamScore(node.NodeID, resourceKey)
		}
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
	if !strings.Contains(key, "..") && !strings.Contains(key, "\\") {
		return strings.TrimSpace(key)
	}
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
			RequestID:  fmt.Sprintf("%x-%x", time.Now().UnixNano(), requestSeq.Add(1)),
			TTLMs:      s.cfg.DefaultTTLMs,
			Candidates: nil,
		}
	}

	originURL := s.cfg.OriginURL
	if originURL == "" {
		originURL = "http://localhost:7070"
	}

	return &models.DispatchResponse{
		RequestID: fmt.Sprintf("%x-%x", time.Now().UnixNano(), requestSeq.Add(1)),
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
