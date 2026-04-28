package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
	"github.com/darkinno/edge-dispatch-framework/internal/store"
)

type Prober struct {
	pg               *store.PGStore
	nodeCache        *NodeCache
	cfg              *config.ControlPlaneConfig
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	httpClient       *http.Client
	probeConcurrency int
}

func NewProber(pg *store.PGStore, nodeCache *NodeCache, cfg *config.ControlPlaneConfig) *Prober {
	return &Prober{
		pg:        pg,
		nodeCache: nodeCache,
		cfg:       cfg,
		httpClient: &http.Client{
			Timeout: cfg.ProbeTimeout,
		},
		probeConcurrency: 10,
	}
}

func (p *Prober) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(p.cfg.ProbeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.probeAll(ctx)
			}
		}
	}()
	slog.Info("prober started", "interval", p.cfg.ProbeInterval)
}

func (p *Prober) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	slog.Info("prober stopped")
}

func (p *Prober) probeAll(ctx context.Context) {
	nodes, err := p.nodeCache.GetActiveNodes(ctx)
	if err != nil {
		slog.Error("list active nodes for probe", "error", err)
		return
	}

	sem := make(chan struct{}, p.probeConcurrency)
	var wg sync.WaitGroup
	nodeIDs := make(map[string]bool, len(nodes))

	results := make(chan models.ProbeResult, len(nodes)*2)
	var saveWg sync.WaitGroup
	saveWg.Add(1)
	go func() {
		defer saveWg.Done()
		batch := make([]models.ProbeResult, 0, 32)
		for r := range results {
			batch = append(batch, r)
			if len(batch) >= 32 {
				for _, br := range batch {
					if err := p.pg.SaveProbeResult(ctx, br); err != nil {
						slog.Error("save probe result", "error", err)
					}
				}
				batch = batch[:0]
			}
		}
		for _, br := range batch {
			if err := p.pg.SaveProbeResult(ctx, br); err != nil {
				slog.Error("save probe result", "error", err)
			}
		}
	}()

	for _, node := range nodes {
		for _, ep := range node.Endpoints {
			wg.Add(1)
			go func(nid string, ep models.Endpoint) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				result, err := p.ProbeOne(ctx, nid, ep)
				if err != nil {
					return
				}
				results <- *result
			}(node.NodeID, ep)
		}
		nodeIDs[node.NodeID] = true
	}
	wg.Wait()
	close(results)
	saveWg.Wait()

	type scoreResult struct {
		nodeID string
		scores models.NodeScores
		err    error
	}
	scoreCh := make(chan scoreResult, len(nodeIDs))
	for nid := range nodeIDs {
		go func(nodeID string) {
			scores, err := p.computeScores(ctx, nodeID)
			scoreCh <- scoreResult{nodeID: nodeID, scores: scores, err: err}
		}(nid)
	}
	for range nodeIDs {
		res := <-scoreCh
		if res.err != nil {
			slog.Error("compute scores", "node_id", res.nodeID, "error", res.err)
			continue
		}
		if err := p.pg.UpdateNodeScores(ctx, res.nodeID, res.scores); err != nil {
			slog.Error("update scores", "node_id", res.nodeID, "error", err)
		}
	}
}

func (p *Prober) ProbeOne(ctx context.Context, nodeID string, endpoint models.Endpoint) (*models.ProbeResult, error) {
	addr := net.JoinHostPort(endpoint.Host, fmt.Sprintf("%d", endpoint.Port))
	start := time.Now()

	result := &models.ProbeResult{
		NodeID:   nodeID,
		Endpoint: endpoint,
		ProbedAt: time.Now(),
	}

	conn, err := net.DialTimeout("tcp", addr, p.cfg.ProbeTimeout)
	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("tcp connect: %v", err)
		result.RTTMs = float64(time.Since(start).Milliseconds())
		return result, nil
	}
	conn.Close()

	url := fmt.Sprintf("%s://%s:%d/healthz", endpoint.Scheme, endpoint.Host, endpoint.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("create request: %v", err)
		result.RTTMs = float64(time.Since(start).Milliseconds())
		return result, nil
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		result.Success = false
		result.Error = fmt.Sprintf("http get: %v", err)
		result.RTTMs = float64(time.Since(start).Milliseconds())
		return result, nil
	}
	defer resp.Body.Close()

	result.Success = resp.StatusCode == http.StatusOK
	result.RTTMs = float64(time.Since(start).Milliseconds())
	if !result.Success {
		result.Error = fmt.Sprintf("healthz returned %d", resp.StatusCode)
	}

	return result, nil
}

func (p *Prober) computeScores(ctx context.Context, nodeID string) (models.NodeScores, error) {
	ps, err := p.pg.GetProbeScores(ctx, nodeID)
	if err != nil {
		return models.NodeScores{}, fmt.Errorf("get probe scores: %w", err)
	}

	reachableScore := ps.SuccessRate5m * 100.0
	healthScore := ps.SuccessRate1m * 100.0

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

	return models.NodeScores{
		ReachableScore: reachableScore,
		HealthScore:    healthScore,
		RiskScore:      riskScore,
	}, nil
}
