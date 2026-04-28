package edgeagent

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type CacheStats struct {
	Size      int64 `json:"size"`
	MaxGB     int64 `json:"max_gb"`
	ItemCount int64 `json:"item_count"`
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
}

type cacheMeta struct {
	Size       int64 `json:"size"`
	LastAccess int64 `json:"last_access"` // unix nano
}

type Cache struct {
	dir         string
	maxGB       int64
	maxBytes    int64
	mu          sync.RWMutex
	hits        atomic.Int64
	misses      atomic.Int64
	size        atomic.Int64
	count       atomic.Int64
	accessTimes map[string]int64
	sizes       map[string]int64
	accessMu    sync.RWMutex
}

func NewCache(dir string, maxGB int64) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	c := &Cache{
		dir:         dir,
		maxGB:       maxGB,
		maxBytes:    maxGB * 1024 * 1024 * 1024,
		accessTimes: make(map[string]int64),
		sizes:       make(map[string]int64),
	}

	var totalSize int64
	var totalCount int64
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		metaPath := filepath.Join(dir, entry.Name(), "meta.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var m cacheMeta
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		totalSize += m.Size
		totalCount++
		c.accessTimes[entry.Name()] = m.LastAccess
		c.sizes[entry.Name()] = m.Size
	}
	c.size.Store(totalSize)
	c.count.Store(totalCount)

	return c, nil
}

func (c *Cache) keyPath(key string) string {
	h := md5.Sum([]byte(key))
	return filepath.Join(c.dir, fmt.Sprintf("%x", h))
}

func (c *Cache) contentPath(key string) string {
	return filepath.Join(c.keyPath(key), "content")
}

func (c *Cache) metaPath(key string) string {
	return filepath.Join(c.keyPath(key), "meta.json")
}

func (c *Cache) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	c.mu.RLock()
	kp := c.keyPath(key)
	c.mu.RUnlock()

	cp := filepath.Join(kp, "content")
	f, err := os.Open(cp)
	if err != nil {
		if os.IsNotExist(err) {
			c.misses.Add(1)
			return nil, 0, fmt.Errorf("cache miss: %s", key)
		}
		return nil, 0, fmt.Errorf("open cache: %w", err)
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("stat cache: %w", err)
	}

	c.accessMu.Lock()
	c.accessTimes[filepath.Base(kp)] = time.Now().UnixNano()
	c.accessMu.Unlock()

	c.hits.Add(1)
	return f, fi.Size(), nil
}

func (c *Cache) Put(ctx context.Context, key string, data io.Reader, size int64) error {
	c.mu.Lock()
	kp := c.keyPath(key)
	c.mu.Unlock()

	if err := os.MkdirAll(kp, 0o755); err != nil {
		return fmt.Errorf("mkdir cache entry: %w", err)
	}

	cp := filepath.Join(kp, "content")
	f, err := os.Create(cp)
	if err != nil {
		return fmt.Errorf("create cache file: %w", err)
	}
	defer f.Close()

	written, err := io.Copy(f, data)
	if err != nil {
		os.Remove(cp)
		return fmt.Errorf("write cache: %w", err)
	}
	_ = f.Sync()

	now := time.Now().UnixNano()
	meta := cacheMeta{Size: written, LastAccess: now}
	metaBytes, _ := json.Marshal(meta)
	mp := filepath.Join(kp, "meta.json")
	if err := os.WriteFile(mp, metaBytes, 0o644); err != nil {
		return fmt.Errorf("write meta: %w", err)
	}

	entryName := filepath.Base(kp)
	c.accessMu.Lock()
	c.accessTimes[entryName] = now
	c.sizes[entryName] = written
	c.accessMu.Unlock()

	c.count.Add(1)
	c.size.Add(written)

	currentSize := c.size.Load()
	if currentSize > c.maxBytes {
		c.mu.Lock()
		c.evict(ctx, currentSize-c.maxBytes)
		c.mu.Unlock()
	}

	return nil
}

func (c *Cache) Has(ctx context.Context, key string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, err := os.Stat(c.contentPath(key))
	return err == nil
}

func (c *Cache) Delete(ctx context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	kp := c.keyPath(key)
	mp := filepath.Join(kp, "meta.json")

	var metaSize int64
	if data, err := os.ReadFile(mp); err == nil {
		var m cacheMeta
		if json.Unmarshal(data, &m) == nil {
			metaSize = m.Size
		}
	}

	if err := os.RemoveAll(kp); err != nil {
		return fmt.Errorf("delete cache entry: %w", err)
	}

	entryName := filepath.Base(kp)
	c.accessMu.Lock()
	delete(c.accessTimes, entryName)
	delete(c.sizes, entryName)
	c.accessMu.Unlock()

	c.count.Add(-1)
	c.size.Add(-metaSize)
	return nil
}

func (c *Cache) Stats() CacheStats {
	return CacheStats{
		Size:      c.size.Load(),
		MaxGB:     c.maxGB,
		ItemCount: c.count.Load(),
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
	}
}

func (c *Cache) Evict(ctx context.Context, targetBytes int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evict(ctx, targetBytes)
}

func (c *Cache) evict(ctx context.Context, targetBytes int64) error {
	type entry struct {
		key        string
		lastAccess int64
		size       int64
	}

	c.accessMu.Lock()
	var entries []entry
	for k, v := range c.accessTimes {
		sz, ok := c.sizes[k]
		if !ok {
			continue
		}
		entries = append(entries, entry{
			key:        k,
			lastAccess: v,
			size:       sz,
		})
	}
	c.accessMu.Unlock()

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].lastAccess < entries[j].lastAccess
	})

	var freed int64
	for _, e := range entries {
		if freed >= targetBytes {
			break
		}
		entryPath := filepath.Join(c.dir, e.key)
		if err := os.RemoveAll(entryPath); err != nil {
			slog.Error("evict remove failed", "key", e.key, "err", err)
			continue
		}
		freed += e.size

		c.accessMu.Lock()
		delete(c.accessTimes, e.key)
		delete(c.sizes, e.key)
		c.accessMu.Unlock()

		c.count.Add(-1)
		c.size.Add(-e.size)
	}

	slog.Info("cache eviction completed", "freed_bytes", freed, "target_bytes", targetBytes)
	return nil
}

func (c *Cache) Close() error {
	return nil
}
