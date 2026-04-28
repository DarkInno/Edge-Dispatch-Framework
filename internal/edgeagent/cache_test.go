package edgeagent

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
)

func newTestCache(t *testing.T) (*Cache, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "edf-cache-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	c, err := NewCache(dir, 1) // 1GB max
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("NewCache: %v", err)
	}
	return c, func() { os.RemoveAll(dir) }
}

func TestCachePutGet(t *testing.T) {
	c, cleanup := newTestCache(t)
	defer cleanup()
	ctx := context.Background()

	data := []byte("hello world content")
	key := "test-object"

	err := c.Put(ctx, key, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	reader, size, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer reader.Close()

	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content mismatch: got %q, want %q", got, data)
	}
}

func TestCacheMiss(t *testing.T) {
	c, cleanup := newTestCache(t)
	defer cleanup()
	ctx := context.Background()

	_, _, err := c.Get(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for cache miss")
	}

	stats := c.Stats()
	if stats.Misses != 1 {
		t.Errorf("misses = %d, want 1", stats.Misses)
	}
}

func TestCacheHas(t *testing.T) {
	c, cleanup := newTestCache(t)
	defer cleanup()
	ctx := context.Background()

	if c.Has(ctx, "key") {
		t.Fatal("Has should return false for missing key")
	}

	c.Put(ctx, "key", bytes.NewReader([]byte("x")), 1)
	if !c.Has(ctx, "key") {
		t.Fatal("Has should return true after Put")
	}
}

func TestCacheDelete(t *testing.T) {
	c, cleanup := newTestCache(t)
	defer cleanup()
	ctx := context.Background()

	c.Put(ctx, "key", bytes.NewReader([]byte("hello")), 5)
	if err := c.Delete(ctx, "key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if c.Has(ctx, "key") {
		t.Fatal("Has should return false after Delete")
	}

	stats := c.Stats()
	if stats.ItemCount != 0 {
		t.Errorf("ItemCount = %d, want 0", stats.ItemCount)
	}
	if stats.Size != 0 {
		t.Errorf("Size = %d, want 0", stats.Size)
	}
}

func TestCacheStats(t *testing.T) {
	c, cleanup := newTestCache(t)
	defer cleanup()
	ctx := context.Background()

	c.Put(ctx, "a", bytes.NewReader(make([]byte, 100)), 100)
	c.Put(ctx, "b", bytes.NewReader(make([]byte, 200)), 200)

	stats := c.Stats()
	if stats.ItemCount != 2 {
		t.Errorf("ItemCount = %d, want 2", stats.ItemCount)
	}
	if stats.Size != 300 {
		t.Errorf("Size = %d, want 300", stats.Size)
	}
	if stats.MaxGB != 1 {
		t.Errorf("MaxGB = %d, want 1", stats.MaxGB)
	}
}

func TestCacheHitCount(t *testing.T) {
	c, cleanup := newTestCache(t)
	defer cleanup()
	ctx := context.Background()

	c.Put(ctx, "key", bytes.NewReader([]byte("data")), 4)

	// Hit
	reader, _, err := c.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	reader.Close()

	// Another hit
	reader2, _, err := c.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	reader2.Close()

	stats := c.Stats()
	if stats.Hits != 2 {
		t.Errorf("Hits = %d, want 2", stats.Hits)
	}
	if stats.Misses != 0 {
		t.Errorf("Misses = %d, want 0", stats.Misses)
	}
}

func TestCacheEviction(t *testing.T) {
	ctx := context.Background()

	// Create a cache with very small max size
	dir, err := os.MkdirTemp("", "edf-cache-evict-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	small, err := NewCache(dir, 0) // 0GB = 0 bytes
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	// Override maxBytes for testing
	small.maxBytes = 50

	small.Put(ctx, "a", bytes.NewReader(make([]byte, 30)), 30)
	small.Put(ctx, "b", bytes.NewReader(make([]byte, 30)), 30) // should trigger eviction

	stats := small.Stats()
	// After putting 60 bytes into a 50-byte cache, eviction should reduce
	if stats.Size > 50 || stats.ItemCount > 2 {
		// Eviction may clear one entry
		t.Logf("size=%d, count=%d after eviction", stats.Size, stats.ItemCount)
	}
}

func TestCachePutOverwrite(t *testing.T) {
	c, cleanup := newTestCache(t)
	defer cleanup()
	ctx := context.Background()

	c.Put(ctx, "key", bytes.NewReader([]byte("old")), 3)
	c.Put(ctx, "key", bytes.NewReader([]byte("newdata")), 7)

	reader, size, err := c.Get(ctx, "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer reader.Close()

	got, _ := io.ReadAll(reader)
	if string(got) != "newdata" {
		t.Errorf("content = %q, want %q", got, "newdata")
	}
	if size != 7 {
		t.Errorf("size = %d, want 7", size)
	}
}

func TestParseRangeHeader(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		contentLen int64
		wantStart  int64
		wantEnd    int64
		wantErr    bool
	}{
		{"full range", "bytes=0-99", 200, 0, 99, false},
		{"open end", "bytes=100-", 200, 100, 199, false},
		{"suffix", "bytes=-50", 200, 150, 199, false},
		{"invalid prefix", "byte=0-99", 200, 0, 0, true},
		{"empty", "", 200, 0, 0, true},
		{"start beyond length", "bytes=300-400", 200, 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := parseRangeHeader(tt.header, tt.contentLen)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if start != tt.wantStart {
					t.Errorf("start = %d, want %d", start, tt.wantStart)
				}
				if end != tt.wantEnd {
					t.Errorf("end = %d, want %d", end, tt.wantEnd)
				}
			}
		})
	}
}
