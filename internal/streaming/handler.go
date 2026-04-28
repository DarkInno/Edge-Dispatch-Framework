package streaming

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

// Handler ties together the streaming components and provides HTTP-level
// integration for the edge agent. It intercepts streaming requests to
// orchestrate sliding window caching, manifest parsing, and prefetch.
type Handler struct {
	window   *SlidingWindow
	prefetch *PrefetchManager
	cfg      *config.StreamingConfig

	metrics streamingMetrics
}

type streamingMetrics struct {
	chunkRequests   atomic.Int64
	chunkCacheHits  atomic.Int64
	chunkCacheMisses atomic.Int64
	prefetched      atomic.Int64
	manifestFetches atomic.Int64
	latencySum      atomic.Int64
}

// NewHandler creates a streaming handler.
func NewHandler(
	window *SlidingWindow,
	prefetch *PrefetchManager,
	cfg *config.StreamingConfig,
) *Handler {
	return &Handler{
		window:   window,
		prefetch: prefetch,
		cfg:      cfg,
	}
}

// IsStreamingRequest determines if a request targets streaming content.
func (h *Handler) IsStreamingRequest(key string) bool {
	return IsHLSChunk(key) || IsDASHChunk(key) ||
		keyHasExt(key, ".m3u8") || keyHasExt(key, ".mpd")
}

// IsManifestRequest determines if a request targets a manifest/playlist file.
func (h *Handler) IsManifestRequest(key string) bool {
	return keyHasExt(key, ".m3u8") || keyHasExt(key, ".mpd")
}

// HandleStreamingRequest processes a streaming content request through
// the sliding window cache, with prefetch on cache hit.
// Returns (data, contentLen, fromCache, error).
func (h *Handler) HandleStreamingRequest(ctx context.Context, key string, fetchFn func(ctx context.Context, key string) (io.ReadCloser, int64, string, error)) ([]byte, int64, bool, error) {
	start := time.Now()

	streamKey, seqNum, ok := InferStreamFromChunkKey(key)
	if !ok {
		return nil, 0, false, fmt.Errorf("not a streaming chunk: %s", key)
	}

	h.metrics.chunkRequests.Add(1)

	if data, err := h.window.GetChunk(ctx, streamKey, seqNum); err == nil {
		h.metrics.chunkCacheHits.Add(1)
		h.metrics.latencySum.Add(time.Since(start).Milliseconds())
		h.prefetch.OnChunkRequest(streamKey, seqNum)
		slog.Debug("streaming cache hit", "key", key, "seq", seqNum)
		return data, int64(len(data)), true, nil
	}

	h.metrics.chunkCacheMisses.Add(1)

	body, _, _, err := fetchFn(ctx, key)
	if err != nil {
		return nil, 0, false, err
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read upstream: %w", err)
	}

	if len(data) > 0 {
		if err := h.window.CacheChunk(ctx, streamKey, seqNum, data); err != nil {
			slog.Debug("cache chunk error", "err", err)
		}
	}

	h.prefetch.OnChunkRequest(streamKey, seqNum)
	h.metrics.latencySum.Add(time.Since(start).Milliseconds())

	return data, int64(len(data)), false, nil
}

// HandleManifestRequest fetches and parses a streaming manifest, then
// triggers prefetch for discovered chunks.
func (h *Handler) HandleManifestRequest(ctx context.Context, key string, fetchFn func(ctx context.Context, key string) (io.ReadCloser, int64, string, error)) (*models.ManifestInfo, []byte, error) {
	if !h.cfg.Enabled {
		return nil, nil, fmt.Errorf("streaming not enabled")
	}

	h.metrics.manifestFetches.Add(1)

	body, _, contentType, err := fetchFn(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}

	st := IdentifyStreamType(key, contentType)

	var manifest *models.ManifestInfo
	switch st {
	case models.StreamTypeHLS:
		manifest, err = ParseHLSManifest(key, data)
	case models.StreamTypeDASH:
		manifest, err = ParseDASHManifest(key, data)
	default:
		return nil, data, nil
	}

	if err != nil {
		return nil, data, fmt.Errorf("parse manifest: %w", err)
	}

	if manifest != nil {
		h.prefetch.OnManifestUpdate(manifest)
	}

	return manifest, data, nil
}

// Metrics returns streaming metrics for the edge agent metrics endpoint.
func (h *Handler) Metrics() models.StreamingMetrics {
	prefetched, _ := h.prefetch.Stats()
	return models.StreamingMetrics{
		ChunkRequests:   h.metrics.chunkRequests.Load(),
		ChunkCacheHits:  h.metrics.chunkCacheHits.Load(),
		ChunkPrefetched: prefetched,
		WindowEvictions: h.window.TotalEvictions(),
		ManifestFetches: h.metrics.manifestFetches.Load(),
		AvgLatencyMs:    avgLatency(h.metrics.latencySum.Load(), h.metrics.chunkRequests.Load()),
	}
}

// ServeStreamingResponse writes streaming chunk data to an HTTP response.
func ServeStreamingResponse(w http.ResponseWriter, data []byte, fromCache bool) {
	w.Header().Set("Content-Type", "application/octet-stream")
	if fromCache {
		w.Header().Set("X-Cache", "HIT")
	} else {
		w.Header().Set("X-Cache", "MISS")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func avgLatency(sum, count int64) float64 {
	if count == 0 {
		return 0
	}
	return float64(sum) / float64(count)
}

func keyHasExt(key, ext string) bool {
	lower := len(key)
	if lower < len(ext) {
		return false
	}
	return key[lower-len(ext):] == ext
}
