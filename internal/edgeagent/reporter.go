package edgeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/contentindex"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

type Reporter struct {
	cfg      *config.EdgeAgentConfig
	server   *Server
	cache    *Cache
	client   *http.Client
	nodeID   string
	stopCh   chan struct{}
	stopped  bool
	mu       sync.Mutex
}

func NewReporter(cfg *config.EdgeAgentConfig, server *Server, cache *Cache) *Reporter {
	return &Reporter{
		cfg:    cfg,
		server: server,
		cache:  cache,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		stopCh: make(chan struct{}),
	}
}

func (r *Reporter) SetNodeID(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodeID = id
}

func (r *Reporter) NodeID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nodeID
}

func (r *Reporter) Start(ctx context.Context) error {
	go r.reportLoop(ctx)
	return nil
}

func (r *Reporter) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.stopped {
		r.stopped = true
		close(r.stopCh)
	}
}

func (r *Reporter) reportLoop(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.HeartbeatInterval)
	defer ticker.Stop()

	slog.Info("reporter started", "interval", r.cfg.HeartbeatInterval)

	for {
		select {
		case <-ctx.Done():
			slog.Info("reporter context cancelled")
			return
		case <-r.stopCh:
			slog.Info("reporter stopped")
			return
		case <-ticker.C:
			if err := r.ReportOnce(ctx); err != nil {
				slog.Error("heartbeat failed", "err", err)
			}
		}
	}
}

func (r *Reporter) ReportOnce(ctx context.Context) error {
	r.mu.Lock()
	nid := r.nodeID
	r.mu.Unlock()

	if nid == "" {
		return fmt.Errorf("node not yet registered")
	}

	cs := r.cache.Stats()
	hitRatio := float64(0)
	if cs.Hits+cs.Misses > 0 {
		hitRatio = float64(cs.Hits) / float64(cs.Hits+cs.Misses)
	}

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	delta := r.server.GetMetricsDelta()
	intervalSec := r.cfg.HeartbeatInterval.Seconds()
	var egressMbps float64
	var errRate float64
	if intervalSec > 0 {
		egressMbps = float64(delta.BytesSent*8) / (intervalSec * 1e6)
	}
	if delta.Requests > 0 {
		errRate = float64(delta.Errors) / float64(delta.Requests)
	}
	cpuLoad := float64(runtime.NumGoroutine()) / float64(runtime.NumCPU())

	hb := models.HeartbeatRequest{
		NodeID: nid,
		TS:     time.Now().Unix(),
		Runtime: models.NodeRuntime{
			CPU:        cpuLoad,
			MemMB:      int64(memStats.Alloc / (1024 * 1024)),
			DiskFreeGB: cs.MaxGB - cs.Size/(1024*1024*1024),
			Conn:       r.server.RequestCount(),
		},
		Traffic: models.NodeTraffic{
			EgressMbps:  egressMbps,
			IngressMbps: 0,
			Err5xxRate:  errRate,
		},
		Cache: models.NodeCache{
			HitRatio: hitRatio,
		},
		ContentSummary: r.buildContentSummary(),
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	if err := json.NewEncoder(buf).Encode(hb); err != nil {
		return fmt.Errorf("marshal heartbeat: %w", err)
	}

	url := r.cfg.ControlPlaneURL + "/v1/nodes/heartbeat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("create heartbeat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.cfg.NodeToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.NodeToken)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("send heartbeat: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("heartbeat rejected: %d", resp.StatusCode)
	}

	slog.Debug("heartbeat sent", "node_id", nid, "hit_ratio", fmt.Sprintf("%.2f", hitRatio))
	return nil
}

func (r *Reporter) Register(ctx context.Context) error {
	caps := r.collectCapabilities()

	regReq := models.RegisterRequest{
		NodeName:   "edge-agent",
		Endpoints:  []models.Endpoint{{Scheme: "http", Host: "localhost", Port: parsePort(r.cfg.ListenAddr)}},
		Region:     "unknown",
		ISP:        "unknown",
		Capabilities: caps,
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	if err := json.NewEncoder(buf).Encode(regReq); err != nil {
		return fmt.Errorf("marshal register: %w", err)
	}

	url := r.cfg.ControlPlaneURL + "/v1/nodes/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("create register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.cfg.NodeToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.NodeToken)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("register rejected: %d", resp.StatusCode)
	}

	var regResp models.RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&regResp); err != nil {
		return fmt.Errorf("decode register response: %w", err)
	}

	r.SetNodeID(regResp.NodeID)
	slog.Info("node registered", "node_id", regResp.NodeID)
	return nil
}

func (r *Reporter) collectCapabilities() models.Capabilities {
	return models.Capabilities{
		InboundReachable: true,
		CacheDiskGB:      r.cfg.CacheMaxGB,
		MaxUplinkMbps:    1000,
		SupportsHTTPS:    false,
	}
}

func (r *Reporter) buildContentSummary() *models.ContentSummary {
	allKeys := r.cache.AllKeys()
	if len(allKeys) == 0 {
		return nil
	}

	hotKeys := r.cache.HotKeys(20)

	bf := contentindex.NewBloomFilter(max(len(allKeys), 100), 0.05)
	for _, k := range allKeys {
		bf.AddString(k)
	}

	return &models.ContentSummary{
		NodeID:      r.nodeID,
		HotKeys:     hotKeys,
		BloomFilter: bf.Bytes(),
		TotalKeys:   int64(len(allKeys)),
		UpdatedAt:   time.Now().Unix(),
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func parsePort(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 9090
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 9090
	}
	return port
}
