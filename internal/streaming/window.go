package streaming

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

// CacheAccessor abstracts cache operations to avoid circular imports.
type CacheAccessor interface {
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
	Put(ctx context.Context, key string, data io.Reader, size int64) error
	Has(ctx context.Context, key string) bool
	Delete(ctx context.Context, key string) error
}

// SlidingWindow wraps a CacheAccessor with per-stream sliding window semantics.
// Each stream maintains a window of the most recent N chunks; older chunks
// are evicted as new chunks are cached.
type SlidingWindow struct {
	cache *CacheAccessor
	cfg   *config.StreamingConfig

	mu      sync.RWMutex
	windows map[string]*streamWindow
}

type streamWindow struct {
	seqs      []int64
	headSeq   int64
	maxSize   int
	evictions int64
}

func NewSlidingWindow(cache CacheAccessor, cfg *config.StreamingConfig) *SlidingWindow {
	return &SlidingWindow{
		cache:   &cache,
		cfg:     cfg,
		windows: make(map[string]*streamWindow),
	}
}

func (sw *SlidingWindow) CacheChunk(ctx context.Context, streamKey string, seqNum int64, chunkData []byte) error {
	sw.mu.Lock()
	win, ok := sw.windows[streamKey]
	if !ok {
		win = &streamWindow{maxSize: sw.cfg.WindowSize, seqs: make([]int64, 0, sw.cfg.WindowSize)}
		sw.windows[streamKey] = win
	}
	sw.mu.Unlock()

	cacheKey := ChunkCacheKey(streamKey, seqNum)

	contentLen := int64(len(chunkData))
	if contentLen > 0 {
		buf := &byteReader{data: chunkData}
		if err := (*sw.cache).Put(ctx, cacheKey, buf, contentLen); err != nil {
			return fmt.Errorf("cache put: %w", err)
		}
	}

	sw.mu.Lock()
	win.seqs = append(win.seqs, seqNum)
	win.headSeq = seqNum

	if len(win.seqs) > win.maxSize {
		toEvict := len(win.seqs) - win.maxSize
		for i := 0; i < toEvict; i++ {
			oldSeq := win.seqs[i]
			oldKey := ChunkCacheKey(streamKey, oldSeq)
			if err := (*sw.cache).Delete(ctx, oldKey); err != nil {
				slog.Debug("sliding window evict", "key", oldKey, "err", err)
			}
			win.evictions++
		}
		win.seqs = win.seqs[toEvict:]
	}
	sw.mu.Unlock()

	return nil
}

func (sw *SlidingWindow) GetChunk(ctx context.Context, streamKey string, seqNum int64) ([]byte, error) {
	cacheKey := ChunkCacheKey(streamKey, seqNum)
	reader, _, err := (*sw.cache).Get(ctx, cacheKey)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read chunk: %w", err)
	}
	return data, nil
}

func (sw *SlidingWindow) HasChunk(ctx context.Context, streamKey string, seqNum int64) bool {
	return (*sw.cache).Has(ctx, ChunkCacheKey(streamKey, seqNum))
}

func (sw *SlidingWindow) WindowStats(streamKey string) (headSeq, evictions int64) {
	sw.mu.RLock()
	defer sw.mu.RUnlock()
	if win, ok := sw.windows[streamKey]; ok {
		return win.headSeq, win.evictions
	}
	return 0, 0
}

func (sw *SlidingWindow) EvictStream(ctx context.Context, streamKey string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	win, ok := sw.windows[streamKey]
	if !ok {
		return
	}
	for _, seq := range win.seqs {
		key := ChunkCacheKey(streamKey, seq)
		(*sw.cache).Delete(ctx, key)
	}
	delete(sw.windows, streamKey)
}

func (sw *SlidingWindow) TotalEvictions() int64 {
	sw.mu.RLock()
	defer sw.mu.RUnlock()
	var total int64
	for _, w := range sw.windows {
		total += w.evictions
	}
	return total
}

func (sw *SlidingWindow) Metrics() models.StreamingMetrics {
	return models.StreamingMetrics{
		WindowEvictions: sw.TotalEvictions(),
	}
}

// ChunkCacheKey generates a consistent cache key for a streaming chunk.
func ChunkCacheKey(streamKey string, seqNum int64) string {
	return fmt.Sprintf("stream/%s/seq_%010d", streamKey, seqNum)
}

// ParseChunkKey attempts to extract streamKey and seqNum from a cache key.
func ParseChunkKey(key string) (streamKey string, seqNum int64, ok bool) {
	const prefix = "stream/"
	if !strings.HasPrefix(key, prefix) {
		return "", 0, false
	}
	rest := strings.TrimPrefix(key, prefix)
	idx := strings.LastIndex(rest, "/seq_")
	if idx < 0 {
		return "", 0, false
	}
	streamKey = rest[:idx]
	seqStr := rest[idx+len("/seq_"):]
	var err error
	seqNum, err = strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return streamKey, seqNum, true
}

func IsHLSChunk(key string) bool {
	return strings.HasSuffix(strings.ToLower(key), ".ts") ||
		strings.HasSuffix(strings.ToLower(key), ".aac") ||
		strings.HasSuffix(strings.ToLower(key), ".vtt")
}

func IsDASHChunk(key string) bool {
	return strings.HasSuffix(strings.ToLower(key), ".m4s") ||
		strings.HasSuffix(strings.ToLower(key), ".mp4") ||
		strings.HasSuffix(strings.ToLower(key), ".cmfv")
}

func validateStreamKey(key string) string {
	key = strings.TrimSpace(key)
	key = strings.ReplaceAll(key, "\\", "/")
	parts := make([]string, 0)
	for _, p := range strings.Split(key, "/") {
		p = strings.TrimSpace(p)
		if p == "" || p == "." || p == ".." {
			continue
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "/")
}

func InferStreamFromChunkKey(key string) (streamKey string, seqNum int64, ok bool) {
	lastSlash := strings.LastIndex(key, "/")
	if lastSlash < 0 {
		return "", 0, false
	}
	rawKey := key[:lastSlash]
	streamKey = validateStreamKey(rawKey)
	if streamKey == "" {
		return "", 0, false
	}
	filename := key[lastSlash+1:]

	dotIdx := strings.LastIndex(filename, ".")
	if dotIdx >= 0 {
		filename = filename[:dotIdx]
	}

	seqNum = extractSeqNum(filename)
	if seqNum >= 0 {
		return streamKey, seqNum, true
	}
	return "", 0, false
}

func extractSeqNum(name string) int64 {
	parts := strings.Split(name, "_")
	for i := len(parts) - 1; i >= 0; i-- {
		if n, err := strconv.ParseInt(parts[i], 10, 64); err == nil {
			return n
		}
	}
	return -1
}

type byteReader struct {
	data   []byte
	offset int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}
