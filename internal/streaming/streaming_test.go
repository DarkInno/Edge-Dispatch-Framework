package streaming

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

// mockCache implements CacheAccessor for testing.
type mockCache struct {
	mu    sync.RWMutex
	items map[string][]byte
}

func newMockCache() *mockCache {
	return &mockCache{items: make(map[string][]byte)}
}

func (m *mockCache) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.items[key]
	if !ok {
		return nil, 0, fmt.Errorf("not found: %s", key)
	}
	return io.NopCloser(&byteSliceReader{data: data}), int64(len(data)), nil
}

func (m *mockCache) Put(ctx context.Context, key string, data io.Reader, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := make([]byte, size)
	n, err := io.ReadFull(data, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}
	m.items[key] = buf[:n]
	return nil
}

func (m *mockCache) Has(ctx context.Context, key string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.items[key]
	return ok
}

func (m *mockCache) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, key)
	return nil
}

func (m *mockCache) count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.items)
}

type byteSliceReader struct {
	data   []byte
	offset int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func (r *byteSliceReader) Close() error {
	return nil
}

// ─── Tests ────────────────────────────────────────────────────────

func TestSlidingWindowCacheChunk(t *testing.T) {
	mc := newMockCache()
	cfg := &config.StreamingConfig{WindowSize: 5, Enabled: true}
	sw := NewSlidingWindow(mc, cfg)
	ctx := context.Background()

	for i := int64(0); i < 10; i++ {
		data := []byte(fmt.Sprintf("chunk-%d", i))
		if err := sw.CacheChunk(ctx, "stream1", i, data); err != nil {
			t.Fatalf("CacheChunk %d: %v", i, err)
		}
	}

	if mc.count() != 5 {
		t.Errorf("expected 5 items in cache, got %d", mc.count())
	}

	for i := int64(0); i < 5; i++ {
		if sw.HasChunk(ctx, "stream1", i) {
			t.Errorf("chunk %d should have been evicted", i)
		}
	}
	for i := int64(5); i < 10; i++ {
		if !sw.HasChunk(ctx, "stream1", i) {
			t.Errorf("chunk %d should be in cache", i)
		}
	}

	head, evictions := sw.WindowStats("stream1")
	if head != 9 {
		t.Errorf("expected head seq 9, got %d", head)
	}
	if evictions != 5 {
		t.Errorf("expected 5 evictions, got %d", evictions)
	}
}

func TestSlidingWindowGetChunk(t *testing.T) {
	mc := newMockCache()
	cfg := &config.StreamingConfig{WindowSize: 5, Enabled: true}
	sw := NewSlidingWindow(mc, cfg)
	ctx := context.Background()

	sw.CacheChunk(ctx, "stream1", 0, []byte("hello"))

	data, err := sw.GetChunk(ctx, "stream1", 0)
	if err != nil {
		t.Fatalf("GetChunk: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("expected 'hello', got %q", data)
	}
}

func TestSlidingWindowEvictStream(t *testing.T) {
	mc := newMockCache()
	cfg := &config.StreamingConfig{WindowSize: 10, Enabled: true}
	sw := NewSlidingWindow(mc, cfg)
	ctx := context.Background()

	sw.CacheChunk(ctx, "stream1", 0, []byte("a"))
	sw.CacheChunk(ctx, "stream1", 1, []byte("b"))
	sw.CacheChunk(ctx, "stream2", 0, []byte("c"))

	sw.EvictStream(ctx, "stream1")

	if sw.HasChunk(ctx, "stream1", 0) {
		t.Error("stream1 chunk 0 should be evicted")
	}
	if sw.HasChunk(ctx, "stream1", 1) {
		t.Error("stream1 chunk 1 should be evicted")
	}
	if !sw.HasChunk(ctx, "stream2", 0) {
		t.Error("stream2 chunk 0 should still be cached")
	}
}

func TestChunkCacheKey(t *testing.T) {
	key := ChunkCacheKey("live/stream1", 42)
	expected := "stream/live/stream1/seq_0000000042"
	if key != expected {
		t.Errorf("expected %q, got %q", expected, key)
	}

	sk, seq, ok := ParseChunkKey(key)
	if !ok {
		t.Fatal("ParseChunkKey returned not ok")
	}
	if sk != "live/stream1" {
		t.Errorf("expected streamKey 'live/stream1', got %q", sk)
	}
	if seq != 42 {
		t.Errorf("expected seq 42, got %d", seq)
	}
}

func TestInferStreamFromChunkKey(t *testing.T) {
	tests := []struct {
		key       string
		stream    string
		seq       int64
		expectOK  bool
	}{
		{"live/stream1/segment_042.ts", "live/stream1", 42, true},
		{"vod/video/init_001.m4s", "vod/video", 1, true},
		{"vod/video/chunk_100.mp4", "vod/video", 100, true},
		{"stream1/not_a_segment.xml", "stream1", 0, false},
		{"segments_only_42", "", 0, false},
	}

	for _, tt := range tests {
		sk, seq, ok := InferStreamFromChunkKey(tt.key)
		if ok != tt.expectOK {
			t.Errorf("%q: expected ok=%v, got ok=%v", tt.key, tt.expectOK, ok)
		}
		if tt.expectOK {
			if sk != tt.stream {
				t.Errorf("%q: expected stream %q, got %q", tt.key, tt.stream, sk)
			}
			if seq != tt.seq {
				t.Errorf("%q: expected seq %d, got %d", tt.key, tt.seq, seq)
			}
		}
	}
}

func TestIsHLSAndDASHChunk(t *testing.T) {
	if !IsHLSChunk("segment.ts") {
		t.Error("expected .ts to be HLS chunk")
	}
	if !IsDASHChunk("fragment.m4s") {
		t.Error("expected .m4s to be DASH chunk")
	}
	if IsHLSChunk("video.mp4") {
		t.Error("expected .mp4 not to be HLS chunk")
	}
	if IsDASHChunk("audio.ts") {
		t.Error("expected .ts not to be DASH chunk")
	}
}

func TestParseHLSManifest(t *testing.T) {
	m3u8 := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:10.0,
segment_000.ts
#EXTINF:10.0,
segment_001.ts
#EXTINF:10.0,
segment_002.ts
#EXT-X-ENDLIST
`

	m, err := ParseHLSManifest("live/test", []byte(m3u8))
	if err != nil {
		t.Fatalf("ParseHLSManifest: %v", err)
	}
	if m.Type != models.StreamTypeHLS {
		t.Errorf("expected HLS type, got %s", m.Type)
	}
	if m.MedSeq != 0 {
		t.Errorf("expected med seq 0, got %d", m.MedSeq)
	}
	if !m.Endlist {
		t.Error("expected endlist=true")
	}
	if m.TargetDurMs != 10000 {
		t.Errorf("expected target dur 10000ms, got %d", m.TargetDurMs)
	}
	if len(m.Chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(m.Chunks))
	}
	if m.Chunks[0].SeqNum != 0 {
		t.Errorf("expected seq 0, got %d", m.Chunks[0].SeqNum)
	}
}

func TestParseDASHManifest(t *testing.T) {
	mpd := `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" minBufferTime="PT1.500S" profiles="urn:mpeg:dash:profile:isoff-live:2011">
  <Period id="1">
    <AdaptationSet contentType="video">
      <SegmentTemplate timescale="1000" duration="2000" startNumber="1" media="video_$Number$.m4s" initialization="init.mp4" />
    </AdaptationSet>
  </Period>
</MPD>`

	m, err := ParseDASHManifest("live/dash1", []byte(mpd))
	if err != nil {
		t.Fatalf("ParseDASHManifest: %v", err)
	}
	if m.Type != models.StreamTypeDASH {
		t.Errorf("expected DASH type, got %s", m.Type)
	}
	if len(m.Chunks) == 0 {
		t.Fatal("expected chunks, got none")
	}
	if m.Chunks[0].SeqNum != 1 {
		t.Errorf("expected seq 1, got %d", m.Chunks[0].SeqNum)
	}
	if m.TargetDurMs != 2000 {
		t.Errorf("expected target dur 2000ms, got %d", m.TargetDurMs)
	}
}

func TestIdentifyStreamType(t *testing.T) {
	if IdentifyStreamType("playlist.m3u8", "") != models.StreamTypeHLS {
		t.Error("expected HLS from .m3u8 extension")
	}
	if IdentifyStreamType("manifest.mpd", "") != models.StreamTypeDASH {
		t.Error("expected DASH from .mpd extension")
	}
	if IdentifyStreamType("file.txt", "") != "" {
		t.Error("expected empty for unknown extension")
	}
}

func TestStreamingStrategy(t *testing.T) {
	cfg := &config.StreamingConfig{Enabled: true}
	ss := NewStreamingStrategy(cfg)

	ss.ReportStream("node1", "live/stream1", models.StreamTypeHLS)
	if !ss.HasStream("node1", "live/stream1") {
		t.Error("expected node1 to have stream1")
	}
	if ss.HasStream("node1", "nonexistent") {
		t.Error("should not have nonexistent stream")
	}
	if ss.HasStream("node2", "live/stream1") {
		t.Error("node2 should not have stream1")
	}

	score := ss.StreamScore("node1", "live/stream1/segment_042.ts")
	if score != 20.0 {
		t.Errorf("expected score 20.0, got %f", score)
	}

	ss.RemoveStream("node1", "live/stream1", models.StreamTypeHLS)
	if ss.HasStream("node1", "live/stream1") {
		t.Error("expected stream to be removed")
	}
}

func TestStreamingStrategyMultipleStreams(t *testing.T) {
	cfg := &config.StreamingConfig{Enabled: true}
	ss := NewStreamingStrategy(cfg)

	ss.ReportStream("node1", "live/stream1", models.StreamTypeHLS)
	ss.ReportStream("node1", "live/stream2", models.StreamTypeDASH)
	ss.ReportStream("node2", "live/stream1", models.StreamTypeHLS)

	if !ss.HasStream("node1", "live/stream1") {
		t.Error("node1 should have stream1")
	}
	if !ss.HasStream("node1", "live/stream2") {
		t.Error("node1 should have stream2")
	}
	if !ss.HasStream("node2", "live/stream1") {
		t.Error("node2 should have stream1")
	}
}

func TestSlidingWindowMultipleStreams(t *testing.T) {
	mc := newMockCache()
	cfg := &config.StreamingConfig{WindowSize: 3, Enabled: true}
	sw := NewSlidingWindow(mc, cfg)
	ctx := context.Background()

	for i := int64(0); i < 5; i++ {
		sw.CacheChunk(ctx, "stream1", i, []byte(fmt.Sprintf("s1-%d", i)))
	}
	for i := int64(0); i < 4; i++ {
		sw.CacheChunk(ctx, "stream2", i, []byte(fmt.Sprintf("s2-%d", i)))
	}

	head1, ev1 := sw.WindowStats("stream1")
	if head1 != 4 {
		t.Errorf("stream1 head expected 4, got %d", head1)
	}
	if ev1 != 2 {
		t.Errorf("stream1 evictions expected 2, got %d", ev1)
	}

	head2, ev2 := sw.WindowStats("stream2")
	if head2 != 3 {
		t.Errorf("stream2 head expected 3, got %d", head2)
	}
	if ev2 != 1 {
		t.Errorf("stream2 evictions expected 1, got %d", ev2)
	}

	if !sw.HasChunk(ctx, "stream1", 4) {
		t.Error("stream1 chunk 4 should be in cache")
	}
	if !sw.HasChunk(ctx, "stream2", 3) {
		t.Error("stream2 chunk 3 should be in cache")
	}
}

func TestSlidingWindowMetrics(t *testing.T) {
	mc := newMockCache()
	cfg := &config.StreamingConfig{WindowSize: 3, Enabled: true}
	sw := NewSlidingWindow(mc, cfg)
	ctx := context.Background()

	for i := int64(0); i < 6; i++ {
		sw.CacheChunk(ctx, "stream1", i, []byte(fmt.Sprintf("s1-%d", i)))
	}

	metrics := sw.Metrics()
	if metrics.WindowEvictions != 3 {
		t.Errorf("expected 3 total evictions, got %d", metrics.WindowEvictions)
	}
}

func TestParseChunkKeyRoundTrip(t *testing.T) {
	tests := []struct {
		streamKey string
		seqNum    int64
	}{
		{"live/stream1", 0},
		{"live/stream1", 999999},
		{"vod/video", 42},
	}

	for _, tt := range tests {
		key := ChunkCacheKey(tt.streamKey, tt.seqNum)
		sk, seq, ok := ParseChunkKey(key)
		if !ok {
			t.Errorf("round trip failed for %s/%d", tt.streamKey, tt.seqNum)
		}
		if sk != tt.streamKey || seq != tt.seqNum {
			t.Errorf("round trip mismatch: got %s/%d, expected %s/%d", sk, seq, tt.streamKey, tt.seqNum)
		}
	}
}

func TestParseChunkKeyInvalid(t *testing.T) {
	_, _, ok := ParseChunkKey("not_a_stream_key")
	if ok {
		t.Error("expected false for invalid key")
	}
	_, _, ok = ParseChunkKey("stream/no_sequence")
	if ok {
		t.Error("expected false for key without sequence")
	}
}
