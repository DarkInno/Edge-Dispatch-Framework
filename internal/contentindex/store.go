package contentindex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store handles persistent storage and in-memory caching of content location data.
type Store struct {
	pool    *pgxpool.Pool
	index   *ContentIndex
	cfg     *config.ContentIndexConfig
	mu      sync.RWMutex
	stopCh  chan struct{}
	stopped bool
}

// NewStore creates a content index store with auto-migration.
func NewStore(ctx context.Context, pool *pgxpool.Pool, cfg *config.ContentIndexConfig) (*Store, error) {
	s := &Store{
		pool:   pool,
		index:  NewContentIndex(),
		cfg:    cfg,
		stopCh: make(chan struct{}),
	}
	if err := s.migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate content index: %w", err)
	}
	return s, nil
}

// StartCleanup begins a background goroutine that periodically removes stale hot keys.
func (s *Store) StartCleanup(ctx context.Context) {
	interval := s.cfg.HotKeyTTL
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go s.cleanupLoop(ctx, interval)
	slog.Info("content index cleanup started", "interval", interval)
}

func (s *Store) cleanupLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.expireStaleHotKeys(ctx)
		}
	}
}

func (s *Store) expireStaleHotKeys(ctx context.Context) {
	cutoff := time.Now().Unix() - int64(s.cfg.HotKeyTTL.Seconds())
	if cutoff <= 0 {
		return
	}

	result, err := s.pool.Exec(ctx, `
		DELETE FROM content_index
		WHERE is_hot = true AND last_seen_at < $1
	`, cutoff)
	if err != nil {
		slog.Warn("expire stale hot keys failed", "err", err)
		return
	}

	if result.RowsAffected() > 0 {
		slog.Debug("expired stale hot keys", "count", result.RowsAffected())
		if err := s.LoadAll(ctx); err != nil {
			slog.Warn("reload content index after cleanup failed", "err", err)
		}
	}
}

// Stop stops the cleanup goroutine.
func (s *Store) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stopCh)
	}
}

func (s *Store) migrate(ctx context.Context) error {
	schema := `
	CREATE TABLE IF NOT EXISTS content_index (
		node_id TEXT NOT NULL,
		content_key TEXT NOT NULL,
		is_hot BOOLEAN NOT NULL DEFAULT false,
		last_seen_at BIGINT NOT NULL,
		PRIMARY KEY (node_id, content_key)
	);

	CREATE INDEX IF NOT EXISTS idx_content_index_node_hot ON content_index(node_id, is_hot) WHERE is_hot = true;
	CREATE INDEX IF NOT EXISTS idx_content_index_key ON content_index(content_key);
	CREATE INDEX IF NOT EXISTS idx_content_index_last_seen ON content_index(node_id, last_seen_at DESC);

	CREATE TABLE IF NOT EXISTS content_bloom (
		node_id TEXT PRIMARY KEY,
		bloom_data BYTEA NOT NULL,
		bloom_k INT NOT NULL,
		total_keys BIGINT NOT NULL DEFAULT 0,
		updated_at BIGINT NOT NULL
	);
	`
	_, err := s.pool.Exec(ctx, schema)
	return err
}

// UpsertSummary stores a content summary from an edge node.
func (s *Store) UpsertSummary(ctx context.Context, summary models.ContentSummary) error {
	now := time.Now().Unix()

	// Store bloom filter data
	bloomK := uint32(0)
	if summary.BloomK > 0 && summary.BloomK <= 100 {
		bloomK = summary.BloomK
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO content_bloom (node_id, bloom_data, bloom_k, total_keys, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (node_id) DO UPDATE SET
			bloom_data = EXCLUDED.bloom_data,
			bloom_k = EXCLUDED.bloom_k,
			total_keys = EXCLUDED.total_keys,
			updated_at = EXCLUDED.updated_at
	`, summary.NodeID, summary.BloomFilter, bloomK, summary.TotalKeys, now)
	if err != nil {
		return fmt.Errorf("upsert bloom: %w", err)
	}

	// Replace hot keys for this node
	_, err = s.pool.Exec(ctx, `DELETE FROM content_index WHERE node_id = $1 AND is_hot = true`, summary.NodeID)
	if err != nil {
		return fmt.Errorf("delete old hot keys: %w", err)
	}

	for _, key := range summary.HotKeys {
		_, err := s.pool.Exec(ctx, `
			INSERT INTO content_index (node_id, content_key, is_hot, last_seen_at)
			VALUES ($1, $2, true, $3)
			ON CONFLICT (node_id, content_key) DO UPDATE SET last_seen_at = EXCLUDED.last_seen_at
		`, summary.NodeID, key, now)
		if err != nil {
			slog.Warn("insert hot key failed", "node_id", summary.NodeID, "key", key, "err", err)
		}
	}

	// Update in-memory index
	bloomData := ContentSummaryData{
		HotKeys:   summary.HotKeys,
		TotalKeys: summary.TotalKeys,
	}
	if len(summary.BloomFilter) > 0 {
		bloomData.BloomBytes = summary.BloomFilter
		if summary.BloomK > 0 && summary.BloomK <= 100 {
			bloomData.BloomK = summary.BloomK
		}
	}
	s.index.Update(summary.NodeID, bloomData)

	slog.Debug("content summary updated", "node_id", summary.NodeID, "hot_keys", len(summary.HotKeys), "total_keys", summary.TotalKeys)
	return nil
}

// LoadBloom reconstructs a Bloom filter from stored data.
func (s *Store) LoadBloom(ctx context.Context, nodeID string) (*BloomFilter, error) {
	var bloomData []byte
	var bloomK uint32
	err := s.pool.QueryRow(ctx, `
		SELECT bloom_data, bloom_k FROM content_bloom WHERE node_id = $1
	`, nodeID).Scan(&bloomData, &bloomK)
	if err != nil {
		return nil, err
	}
	return NewBloomFilterFromBytes(bloomData, bloomK), nil
}

// GetHotKeys returns hot cached keys for a node.
func (s *Store) GetHotKeys(ctx context.Context, nodeID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT content_key FROM content_index
		WHERE node_id = $1 AND is_hot = true
		ORDER BY last_seen_at DESC
		LIMIT 1000
	`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return keys, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// FindNodesWithKey returns node IDs that likely have the given content key.
func (s *Store) FindNodesWithKey(ctx context.Context, key string) ([]string, error) {
	// Check hot keys first
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT node_id FROM content_index
		WHERE content_key = $1 AND is_hot = true
	`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []string
	for rows.Next() {
		var nid string
		if err := rows.Scan(&nid); err != nil {
			return nodes, err
		}
		nodes = append(nodes, nid)
	}
	if err := rows.Err(); err != nil {
		return nodes, err
	}

	// Check bloom filters if no hot match
	if len(nodes) == 0 {
		bloomRows, err := s.pool.Query(ctx, `SELECT node_id, bloom_data, bloom_k FROM content_bloom`)
		if err != nil {
			return nodes, err
		}
		defer bloomRows.Close()

		for bloomRows.Next() {
			var nid string
			var data []byte
			var k uint32
			if err := bloomRows.Scan(&nid, &data, &k); err != nil {
				continue
			}
			if k == 0 || k > 100 {
				continue
			}
			bf := NewBloomFilterFromBytes(data, k)
			if bf.ContainsString(key) {
				nodes = append(nodes, nid)
			}
		}
	}

	return nodes, nil
}

// RemoveNode removes all content index data for a node.
func (s *Store) RemoveNode(ctx context.Context, nodeID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM content_index WHERE node_id = $1`, nodeID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM content_bloom WHERE node_id = $1`, nodeID)
	if err != nil {
		return err
	}
	s.index.RemoveNode(nodeID)
	return nil
}

// LoadAll loads all content index data into memory.
func (s *Store) LoadAll(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT cb.node_id, cb.bloom_data, cb.bloom_k, cb.total_keys,
			ARRAY_REMOVE(ARRAY_AGG(ci.content_key ORDER BY ci.last_seen_at DESC), NULL) as hot_keys
		FROM content_bloom cb
		LEFT JOIN content_index ci ON cb.node_id = ci.node_id AND ci.is_hot = true
		GROUP BY cb.node_id, cb.bloom_data, cb.bloom_k, cb.total_keys
	`)
	if err != nil {
		return fmt.Errorf("load content index: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var nodeID string
		var bloomData []byte
		var bloomK uint32
		var totalKeys int64
		var hotKeysJSON []byte

		if err := rows.Scan(&nodeID, &bloomData, &bloomK, &totalKeys, &hotKeysJSON); err != nil {
			slog.Warn("scan content index row", "err", err)
			continue
		}

		var hotKeys []string
		if hotKeysJSON != nil {
			json.Unmarshal(hotKeysJSON, &hotKeys)
		}

		s.index.Update(nodeID, ContentSummaryData{
			HotKeys:    hotKeys,
			BloomBytes: bloomData,
			BloomK:     bloomK,
			TotalKeys:  totalKeys,
		})
	}

	return rows.Err()
}

// Index returns the in-memory content index.
func (s *Store) Index() *ContentIndex {
	return s.index
}

// Close cleans up resources.
func (s *Store) Close() {}
