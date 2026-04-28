package streaming

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

// PrefetchManager orchestrates look-ahead prefetching for streaming chunks.
// When a client requests chunk N, the manager queues chunks N+1 through N+prefetchCount
// for background fetching and caching.
type PrefetchManager struct {
	cfg        *config.StreamingConfig
	window     *SlidingWindow
	originURL  string
	nodeToken  string
	client     *http.Client

	mu         sync.RWMutex
	streams    map[string]*streamState
	queue      chan prefetchJob
	stopCh     chan struct{}
	workersWg  sync.WaitGroup

	prefetched atomic.Int64
	failed     atomic.Int64
}

type streamState struct {
	lastSeq      int64
	lastRefresh  time.Time
}

type prefetchJob struct {
	streamKey string
	chunks    []models.ChunkInfo
	priority  int
}

// NewPrefetchManager creates a prefetch manager.
func NewPrefetchManager(
	cfg *config.StreamingConfig,
	window *SlidingWindow,
	originURL string,
	nodeToken string,
) *PrefetchManager {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &PrefetchManager{
		cfg:       cfg,
		window:    window,
		originURL: originURL,
		nodeToken: nodeToken,
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
		streams: make(map[string]*streamState),
		queue:   make(chan prefetchJob, 256),
		stopCh:  make(chan struct{}),
	}
}

// Start begins the background prefetch workers.
func (pm *PrefetchManager) Start(ctx context.Context) {
	if !pm.cfg.Enabled {
		slog.Info("streaming prefetch disabled")
		return
	}
	workers := pm.cfg.PrefetchWorkers
	if workers < 1 {
		workers = 2
	}
	for i := 0; i < workers; i++ {
		pm.workersWg.Add(1)
		go pm.worker(ctx, i)
	}
	slog.Info("prefetch manager started", "workers", workers, "prefetch_count", pm.cfg.PrefetchCount)
}

// Stop gracefully stops the prefetch workers.
func (pm *PrefetchManager) Stop() {
	close(pm.stopCh)
	pm.workersWg.Wait()
	slog.Info("prefetch manager stopped",
		"total_prefetched", pm.prefetched.Load(),
		"total_failed", pm.failed.Load())
}

// OnChunkRequest notifies the prefetch manager of a client chunk request.
// It determines the current position in the stream and queues prefetch for
// the next N chunks.
func (pm *PrefetchManager) OnChunkRequest(streamKey string, seqNum int64) {
	if !pm.cfg.Enabled || pm.cfg.PrefetchCount <= 0 {
		return
	}

	streamKey = validateStreamKey(streamKey)
	if streamKey == "" {
		return
	}

	pm.mu.Lock()
	state, ok := pm.streams[streamKey]
	if !ok {
		state = &streamState{}
		pm.streams[streamKey] = state
	}
	state.lastSeq = seqNum
	state.lastRefresh = time.Now()
	pm.mu.Unlock()

	chunks := make([]models.ChunkInfo, 0, pm.cfg.PrefetchCount)
	for i := int64(1); i <= int64(pm.cfg.PrefetchCount); i++ {
		nextSeq := seqNum + i
		if pm.window.HasChunk(context.Background(), streamKey, nextSeq) {
			continue
		}
		chunks = append(chunks, models.ChunkInfo{
			StreamKey:  streamKey,
			SeqNum:     nextSeq,
			URL:        fmt.Sprintf("%s/obj/%s/%s_segment_%03d.ts", pm.originURL, streamKey, streamKey, nextSeq),
			DurationMs: pm.cfg.ChunkDurationMs,
		})
	}

	if len(chunks) == 0 {
		return
	}

	select {
	case pm.queue <- prefetchJob{streamKey: streamKey, chunks: chunks, priority: 1}:
	default:
		slog.Debug("prefetch queue full, dropping", "streamKey", streamKey)
	}
}

// OnManifestUpdate handles updated manifest data, scheduling prefetch for
// newly discovered chunks.
func (pm *PrefetchManager) OnManifestUpdate(m *models.ManifestInfo) {
	if !pm.cfg.Enabled {
		return
	}
	if len(m.Chunks) == 0 {
		return
	}

	pm.mu.Lock()
	state := pm.streams[m.StreamKey]
	if state != nil {
		state.lastRefresh = time.Now()
	}
	pm.mu.Unlock()

	toFetch := make([]models.ChunkInfo, 0, pm.cfg.PrefetchCount)
	lastSeq := m.Chunks[len(m.Chunks)-1].SeqNum
	for i := int64(1); i <= int64(pm.cfg.PrefetchCount); i++ {
		nextSeq := lastSeq + i
		if pm.window.HasChunk(context.Background(), m.StreamKey, nextSeq) {
			continue
		}
		chunk := models.ChunkInfo{
			StreamKey:  m.StreamKey,
			SeqNum:     nextSeq,
			DurationMs: m.TargetDurMs,
		}
		if len(m.Chunks) > 0 {
			chunk.URL = m.Chunks[0].URL
		}
		toFetch = append(toFetch, chunk)
	}

	if len(toFetch) > 0 {
		select {
		case pm.queue <- prefetchJob{streamKey: m.StreamKey, chunks: toFetch, priority: 2}:
		default:
		}
	}
}

// worker is the background goroutine that processes the prefetch queue.
func (pm *PrefetchManager) worker(ctx context.Context, id int) {
	defer pm.workersWg.Done()
	slog.Debug("prefetch worker started", "worker_id", id)

	for {
		select {
		case <-pm.stopCh:
			return
		case <-ctx.Done():
			return
		case job := <-pm.queue:
			pm.processJob(ctx, job)
		}
	}
}

func (pm *PrefetchManager) processJob(ctx context.Context, job prefetchJob) {
	for _, chunk := range job.chunks {
		if pm.cfg.Enabled && pm.window.HasChunk(ctx, chunk.StreamKey, chunk.SeqNum) {
			continue
		}

		sk := validateStreamKey(chunk.StreamKey)
		if sk == "" {
			pm.failed.Add(1)
			continue
		}
		u, err := url.Parse(pm.originURL)
		if err != nil {
			pm.failed.Add(1)
			continue
		}
		u.Path = path.Join(u.Path, "obj", ChunkCacheKey(sk, chunk.SeqNum))

		reqURL := u.String()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			pm.failed.Add(1)
			continue
		}
		if pm.nodeToken != "" {
			req.Header.Set("Authorization", "Bearer "+pm.nodeToken)
		}

		resp, err := pm.client.Do(req)
		if err != nil {
			slog.Debug("prefetch fetch failed", "url", reqURL, "err", err)
			pm.failed.Add(1)
			continue
		}

		if resp.StatusCode >= 400 {
			resp.Body.Close()
			slog.Debug("prefetch upstream error", "url", reqURL, "status", resp.StatusCode)
			pm.failed.Add(1)
			continue
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			pm.failed.Add(1)
			continue
		}

		if err := pm.window.CacheChunk(ctx, chunk.StreamKey, chunk.SeqNum, data); err != nil {
			slog.Debug("prefetch cache write failed", "err", err)
			pm.failed.Add(1)
			continue
		}

		pm.prefetched.Add(1)
	}
}

// Stats returns current prefetch metrics.
func (pm *PrefetchManager) Stats() (prefetched, failed int64) {
	return pm.prefetched.Load(), pm.failed.Load()
}
