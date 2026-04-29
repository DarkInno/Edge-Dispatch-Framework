package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

var uuidBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 12)
		return &b
	},
}

func fastNodeID() string {
	u := uuid.New()
	bp := uuidBufPool.Get().(*[]byte)
	b := (*bp)[:0]
	b = hex.AppendEncode(b, u[:6])
	*bp = b
	uuidBufPool.Put(bp)
	return "n_" + string(b)
}

type nodeCacheEntry struct {
	node      *models.Node
	expiresAt time.Time
}

type PGStore struct {
	pool       *pgxpool.Pool
	readPool   *pgxpool.Pool
	nodeCache  sync.Map
	cacheTTL   time.Duration
	slowThresh time.Duration
}

func NewPGStore(ctx context.Context, connStr string, readConnStr ...string) (*PGStore, error) {
	config, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse pg config: %w", err)
	}

	config.MaxConns = 50
	config.MinConns = 10
	config.MaxConnLifetime = 30 * time.Minute
	config.MaxConnLifetimeJitter = 5 * time.Minute
	config.MaxConnIdleTime = 5 * time.Minute
	config.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect pg: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping pg: %w", err)
	}

	s := &PGStore{
		pool:       pool,
		cacheTTL:   30 * time.Second,
		slowThresh: 200 * time.Millisecond,
	}

	if len(readConnStr) > 0 && readConnStr[0] != "" {
		readConfig, err := pgxpool.ParseConfig(readConnStr[0])
		if err != nil {
			return nil, fmt.Errorf("parse read replica config: %w", err)
		}
		readConfig.MaxConns = 30
		readConfig.MinConns = 5
		readConfig.MaxConnLifetime = 30 * time.Minute
		readConfig.MaxConnIdleTime = 5 * time.Minute
		readConfig.HealthCheckPeriod = 30 * time.Second
		readPool, err := pgxpool.NewWithConfig(ctx, readConfig)
		if err != nil {
			return nil, fmt.Errorf("connect read replica: %w", err)
		}
		if err := readPool.Ping(ctx); err != nil {
			return nil, fmt.Errorf("ping read replica: %w", err)
		}
		s.readPool = readPool
	}

	if err := s.migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	go s.startMaintenance(ctx)

	return s, nil
}

func (s *PGStore) migrate(ctx context.Context) error {
	schema := `
	CREATE TABLE IF NOT EXISTS nodes (
		node_id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL DEFAULT 'default',
		name TEXT NOT NULL,
		endpoints JSONB NOT NULL DEFAULT '[]',
		region TEXT NOT NULL DEFAULT '',
		isp TEXT NOT NULL DEFAULT '',
		asn TEXT NOT NULL DEFAULT '',
		capabilities JSONB NOT NULL DEFAULT '{}',
		status TEXT NOT NULL DEFAULT 'REGISTERED',
		last_seen_at TIMESTAMPTZ,
		scores JSONB NOT NULL DEFAULT '{}',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS probe_results (
		id BIGSERIAL PRIMARY KEY,
		node_id TEXT NOT NULL,
		endpoint JSONB NOT NULL,
		success BOOLEAN NOT NULL,
		rtt_ms DOUBLE PRECISION,
		error TEXT,
		probed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_nodes_status ON nodes(status) WHERE status IN ('ACTIVE', 'DEGRADED');
	CREATE INDEX IF NOT EXISTS idx_nodes_active_covering ON nodes(status, node_id) INCLUDE (name, endpoints, region, isp, capabilities, scores) WHERE status IN ('ACTIVE', 'DEGRADED');
	CREATE INDEX IF NOT EXISTS idx_nodes_tenant_status ON nodes(tenant_id, status);
	CREATE INDEX IF NOT EXISTS idx_probe_results_node_probed_at ON probe_results(node_id, probed_at DESC);
	CREATE INDEX IF NOT EXISTS idx_probe_results_node_success ON probe_results(node_id, success, probed_at DESC) WHERE success = true;
	CREATE INDEX IF NOT EXISTS idx_probe_scores_covering ON probe_results(node_id, probed_at DESC, success, rtt_ms);
	`
	_, err := s.pool.Exec(ctx, schema)
	return err
}

func (s *PGStore) readPoolOrPrimary() *pgxpool.Pool {
	if s.readPool != nil {
		return s.readPool
	}
	return s.pool
}

func (s *PGStore) logSlow(query string, start time.Time, detail string) {
	elapsed := time.Since(start)
	if elapsed >= s.slowThresh {
		slog.Warn("slow query",
			"query", query,
			"duration_ms", elapsed.Milliseconds(),
			"detail", detail,
		)
	}
}

func (s *PGStore) InvalidateNodeCache(nodeID string) {
	s.nodeCache.Delete(nodeID)
}

func (s *PGStore) CreateNode(ctx context.Context, req models.RegisterRequest, token string) (*models.Node, error) {
	nodeID := "n_" + uuid.New().String()[:12]
	endpointsJSON, _ := json.Marshal(req.Endpoints)
	capsJSON, _ := json.Marshal(req.Capabilities)
	scoresJSON, _ := json.Marshal(models.NodeScores{})

	_, err := s.pool.Exec(ctx, `
		INSERT INTO nodes (node_id, name, endpoints, region, isp, capabilities, status, scores)
		VALUES ($1, $2, $3, $4, $5, $6, 'REGISTERED', $7)
	`, nodeID, req.NodeName, endpointsJSON, req.Region, req.ISP, capsJSON, scoresJSON)
	if err != nil {
		return nil, fmt.Errorf("insert node: %w", err)
	}

	return &models.Node{
		NodeID:       nodeID,
		Name:         req.NodeName,
		Endpoints:    req.Endpoints,
		Region:       req.Region,
		ISP:          req.ISP,
		Capabilities: req.Capabilities,
		Status:       models.NodeStatusRegistered,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}, nil
}

func (s *PGStore) GetNode(ctx context.Context, nodeID string) (*models.Node, error) {
	if v, ok := s.nodeCache.Load(nodeID); ok {
		entry := v.(nodeCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return entry.node, nil
		}
		s.nodeCache.Delete(nodeID)
	}

	start := time.Now()
	pool := s.readPoolOrPrimary()

	var n models.Node
	var endpointsJSON, capsJSON, scoresJSON []byte
	err := pool.QueryRow(ctx, `
		SELECT node_id, tenant_id, name, endpoints, region, isp, asn, capabilities, status, last_seen_at, scores, created_at, updated_at
		FROM nodes WHERE node_id = $1
	`, nodeID).Scan(&n.NodeID, &n.TenantID, &n.Name, &endpointsJSON, &n.Region, &n.ISP, &n.ASN, &capsJSON, &n.Status, &n.LastSeenAt, &scoresJSON, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		s.logSlow("GetNode", start, nodeID)
		return nil, fmt.Errorf("get node: %w", err)
	}
	json.Unmarshal(endpointsJSON, &n.Endpoints)
	json.Unmarshal(capsJSON, &n.Capabilities)
	json.Unmarshal(scoresJSON, &n.Scores)
	s.logSlow("GetNode", start, nodeID)

	s.nodeCache.Store(nodeID, nodeCacheEntry{
		node:      &n,
		expiresAt: time.Now().Add(s.cacheTTL),
	})

	return &n, nil
}

func (s *PGStore) ListActiveNodes(ctx context.Context) ([]*models.Node, error) {
	start := time.Now()
	pool := s.readPoolOrPrimary()

	rows, err := pool.Query(ctx, `
		SELECT node_id, name, endpoints, region, isp, capabilities, status, scores
		FROM nodes WHERE status IN ('ACTIVE', 'DEGRADED')
	`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()

	nodes := make([]*models.Node, 0, 16)
	for rows.Next() {
		n := new(models.Node)
		var endpointsJSON, capsJSON, scoresJSON []byte
		if err := rows.Scan(&n.NodeID, &n.Name, &endpointsJSON, &n.Region, &n.ISP, &capsJSON, &n.Status, &scoresJSON); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		json.Unmarshal(endpointsJSON, &n.Endpoints)
		json.Unmarshal(capsJSON, &n.Capabilities)
		json.Unmarshal(scoresJSON, &n.Scores)
		nodes = append(nodes, n)
	}
	s.logSlow("ListActiveNodes", start, fmt.Sprintf("count=%d", len(nodes)))
	return nodes, rows.Err()
}

func (s *PGStore) UpdateNodeStatus(ctx context.Context, nodeID string, status models.NodeStatus) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE nodes SET status = $2, updated_at = NOW() WHERE node_id = $1
	`, nodeID, status)
	if err == nil {
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		s.InvalidateNodeCache(nodeID)
	}
	return err
}

func (s *PGStore) CountByStatus(ctx context.Context, status string) (int, error) {
	var count int
	err := s.readPoolOrPrimary().QueryRow(ctx, `
		SELECT COUNT(*) FROM nodes WHERE status = $1
	`, status).Scan(&count)
	return count, err
}

func (s *PGStore) BatchUpdateNodeStatus(ctx context.Context, updates []struct {
	NodeID string
	Status models.NodeStatus
}) error {
	if len(updates) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, u := range updates {
		batch.Queue(`UPDATE nodes SET status = $2, updated_at = NOW() WHERE node_id = $1`, u.NodeID, u.Status)
	}

	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()

	for _, u := range updates {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("batch update node status %s: %w", u.NodeID, err)
		}
		s.InvalidateNodeCache(u.NodeID)
	}
	return nil
}

func (s *PGStore) UpdateNodeScores(ctx context.Context, nodeID string, scores models.NodeScores) error {
	scoresJSON, _ := json.Marshal(scores)
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET scores = $2, updated_at = NOW() WHERE node_id = $1
	`, nodeID, scoresJSON)
	if err == nil {
		s.InvalidateNodeCache(nodeID)
	}
	return err
}

func (s *PGStore) UpdateNodeLastSeen(ctx context.Context, nodeID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET last_seen_at = NOW(), updated_at = NOW() WHERE node_id = $1
	`, nodeID)
	if err == nil {
		s.InvalidateNodeCache(nodeID)
	}
	return err
}

func (s *PGStore) SaveProbeResult(ctx context.Context, pr models.ProbeResult) error {
	endpointJSON, _ := json.Marshal(pr.Endpoint)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO probe_results (node_id, endpoint, success, rtt_ms, error, probed_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, pr.NodeID, endpointJSON, pr.Success, pr.RTTMs, pr.Error, pr.ProbedAt)
	return err
}

func (s *PGStore) BatchSaveProbeResults(ctx context.Context, results []models.ProbeResult) error {
	if len(results) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, pr := range results {
		endpointJSON, _ := json.Marshal(pr.Endpoint)
		batch.Queue(`
			INSERT INTO probe_results (node_id, endpoint, success, rtt_ms, error, probed_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, pr.NodeID, endpointJSON, pr.Success, pr.RTTMs, pr.Error, pr.ProbedAt)
	}

	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()

	for range results {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("batch save probe result: %w", err)
		}
	}
	return nil
}

func (s *PGStore) GetProbeScores(ctx context.Context, nodeID string) (*models.ProbeScore, error) {
	start := time.Now()
	pool := s.readPoolOrPrimary()

	ps := &models.ProbeScore{NodeID: nodeID}

	err := pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN probed_at > NOW() - INTERVAL '1 minute' AND success THEN 1 ELSE 0 END)::float
				/ NULLIF(SUM(CASE WHEN probed_at > NOW() - INTERVAL '1 minute' THEN 1 ELSE 0 END), 0), 0) AS success_rate_1m,
			COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END)::float
				/ NULLIF(COUNT(*), 0), 0) AS success_rate_5m,
			COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY CASE WHEN success THEN rtt_ms END), 0) AS rtt_p50,
			COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY CASE WHEN success THEN rtt_ms END), 0) AS rtt_p95,
			COALESCE(MAX(CASE WHEN success THEN probed_at END), '1970-01-01') AS last_ok_at
		FROM probe_results
		WHERE node_id = $1 AND probed_at > NOW() - INTERVAL '5 minutes'
	`, nodeID).Scan(
		&ps.SuccessRate1m,
		&ps.SuccessRate5m,
		&ps.RTTP50,
		&ps.RTTP95,
		&ps.LastOkAt,
	)
	if err != nil {
		s.logSlow("GetProbeScores", start, nodeID)
		return nil, fmt.Errorf("get probe scores: %w", err)
	}

	s.logSlow("GetProbeScores", start, nodeID)
	return ps, nil
}

func (s *PGStore) RevokeNode(ctx context.Context, nodeID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM nodes WHERE node_id = $1`, nodeID)
	if err == nil {
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		s.InvalidateNodeCache(nodeID)
	}
	return err
}

func (s *PGStore) CleanupOldProbeResults(ctx context.Context, olderThan time.Duration) (int64, error) {
	start := time.Now()
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM probe_results WHERE probed_at < NOW() - $1::interval
	`, fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return 0, fmt.Errorf("cleanup old probe results: %w", err)
	}
	s.logSlow("CleanupOldProbeResults", start, fmt.Sprintf("deleted=%d", tag.RowsAffected()))
	return tag.RowsAffected(), nil
}

func (s *PGStore) VacuumProbeResults(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `VACUUM ANALYZE probe_results`)
	return err
}

func (s *PGStore) startMaintenance(ctx context.Context) {
	vacuumTicker := time.NewTicker(6 * time.Hour)
	archiveTicker := time.NewTicker(1 * time.Hour)
	defer vacuumTicker.Stop()
	defer archiveTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-vacuumTicker.C:
			if err := s.VacuumProbeResults(ctx); err != nil {
				slog.Error("vacuum probe_results failed", "error", err)
			} else {
				slog.Info("vacuum probe_results completed")
			}
		case <-archiveTicker.C:
			deleted, err := s.CleanupOldProbeResults(ctx, 7*24*time.Hour)
			if err != nil {
				slog.Error("archive old probe results failed", "error", err)
			} else if deleted > 0 {
				slog.Info("archived old probe results", "deleted", deleted)
			}
		}
	}
}

func (s *PGStore) Pool() *pgxpool.Pool {
	return s.pool
}

func (s *PGStore) ReadPool() *pgxpool.Pool {
	if s.readPool != nil {
		return s.readPool
	}
	return s.pool
}

func (s *PGStore) PoolStat() pgxpool.Stat {
	return *s.pool.Stat()
}

func (s *PGStore) Close() {
	s.pool.Close()
	if s.readPool != nil {
		s.readPool.Close()
	}
}

const hbCompressThreshold = 256

var heartbeatCheckScript = redis.NewScript(`
local exists = redis.call('EXISTS', KEYS[1])
if exists == 0 then
	return 0
end
redis.call('HSET', KEYS[1], 'lseen', ARGV[1])
redis.call('EXPIRE', KEYS[1], ARGV[2])
return 1
`)

type slowLogHook struct {
	mu        sync.Mutex
	threshold time.Duration
}

func (h *slowLogHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *slowLogHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		dur := time.Since(start)
		h.mu.Lock()
		th := h.threshold
		h.mu.Unlock()
		if dur > th {
			log.Printf("redis slow pipeline: %d cmds took %v", len(cmds), dur)
		}
		return err
	}
}

func (h *slowLogHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		dur := time.Since(start)
		h.mu.Lock()
		th := h.threshold
		h.mu.Unlock()
		if dur > th {
			log.Printf("redis slow cmd: %s took %v", cmd.Name(), dur)
		}
		return err
	}
}

type RedisStore struct {
	client *redis.Client
	hook   *slowLogHook
}

func NewRedisStore(ctx context.Context, addr, password string) (*RedisStore, error) {
	hook := &slowLogHook{threshold: 100 * time.Millisecond}
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		PoolSize:     50,
		MinIdleConns: 10,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolTimeout:  4 * time.Second,
	})
	client.AddHook(hook)
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("connect redis: %w", err)
	}
	r := &RedisStore{client: client, hook: hook}
	go r.cleanupStaleLoop(context.Background())
	return r, nil
}

func hKey(nodeID string) string { return "h:" + nodeID }

func marshalHB(hb models.HeartbeatRequest) ([]byte, error) {
	raw, err := json.Marshal(hb)
	if err != nil {
		return nil, err
	}
	if len(raw) < hbCompressThreshold {
		return raw, nil
	}
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

const maxHBDecompressedSize = 10 << 20

func unmarshalHB(data []byte) (*models.HeartbeatRequest, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty heartbeat data")
	}
	var reader io.Reader = bytes.NewReader(data)
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		gr, err := gzip.NewReader(reader)
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		reader = io.LimitReader(gr, maxHBDecompressedSize)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxHBDecompressedSize {
		return nil, fmt.Errorf("heartbeat data too large: %d bytes", len(raw))
	}
	var hb models.HeartbeatRequest
	if err := json.Unmarshal(raw, &hb); err != nil {
		return nil, err
	}
	return &hb, nil
}

func (r *RedisStore) SaveHeartbeat(ctx context.Context, hb models.HeartbeatRequest, ttl time.Duration) error {
	key := hKey(hb.NodeID)
	data, err := marshalHB(hb)
	if err != nil {
		return err
	}
	tsBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(tsBuf, uint64(hb.TS))
	pipe := r.client.Pipeline()
	pipe.HSet(ctx, key, map[string]interface{}{
		"d":     data,
		"ts":    tsBuf,
		"lseen": tsBuf,
	})
	pipe.Expire(ctx, key, ttl)
	_, err = pipe.Exec(ctx)
	return err
}

func (r *RedisStore) PipelineSaveHeartbeats(ctx context.Context, hbs []models.HeartbeatRequest, ttl time.Duration) error {
	pipe := r.client.Pipeline()
	for i := range hbs {
		key := hKey(hbs[i].NodeID)
		data, err := marshalHB(hbs[i])
		if err != nil {
			return err
		}
		tsBuf := make([]byte, 8)
		binary.BigEndian.PutUint64(tsBuf, uint64(hbs[i].TS))
		pipe.HSet(ctx, key, map[string]interface{}{
			"d":     data,
			"ts":    tsBuf,
			"lseen": tsBuf,
		})
		pipe.Expire(ctx, key, ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (r *RedisStore) GetHeartbeat(ctx context.Context, nodeID string) (*models.HeartbeatRequest, error) {
	key := hKey(nodeID)
	data, err := r.client.HGet(ctx, key, "d").Bytes()
	if err != nil {
		return nil, err
	}
	hb, err := unmarshalHB(data)
	if err != nil {
		return nil, err
	}
	r.client.Expire(ctx, key, 60*time.Second)
	return hb, nil
}

func (r *RedisStore) GetAllHeartbeats(ctx context.Context, nodeIDs []string) (map[string]*models.HeartbeatRequest, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	keys := make([]string, len(nodeIDs))
	for i, id := range nodeIDs {
		keys[i] = hKey(id)
	}
	pipe := r.client.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.HGetAll(ctx, key)
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	result := make(map[string]*models.HeartbeatRequest, len(nodeIDs))
	for i, cmd := range cmds {
		vals, err := cmd.Result()
		if err != nil || len(vals) == 0 {
			continue
		}
		d, ok := vals["d"]
		if !ok {
			continue
		}
		hb, err := unmarshalHB([]byte(d))
		if err != nil {
			continue
		}
		result[nodeIDs[i]] = hb
		r.client.Expire(ctx, keys[i], 60*time.Second)
	}
	return result, nil
}

func (r *RedisStore) HasHeartbeat(ctx context.Context, nodeID string) bool {
	key := hKey(nodeID)
	nowBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(nowBuf, uint64(time.Now().Unix()))
	result, err := heartbeatCheckScript.Run(ctx, r.client, []string{key}, nowBuf, 60).Int64()
	if err != nil {
		return r.client.Exists(ctx, key).Val() > 0
	}
	return result == 1
}

func (r *RedisStore) GetAllHeartbeatNodes(ctx context.Context) ([]string, error) {
	keys, _, err := r.client.Scan(ctx, 0, "h:*", 1000).Result()
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(keys))
	for i, k := range keys {
		ids[i] = k[2:]
	}
	return ids, nil
}

func (r *RedisStore) PublishEvent(ctx context.Context, channel string, event models.Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return r.client.Publish(ctx, channel, data).Err()
}

func (r *RedisStore) cleanupStaleLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.cleanupStale(ctx)
		}
	}
}

func (r *RedisStore) cleanupStale(ctx context.Context) {
	var cursor uint64
	for {
		keys, nextCursor, err := r.client.Scan(ctx, cursor, "h:*", 100).Result()
		if err != nil {
			return
		}
		cursor = nextCursor
		for _, key := range keys {
			tsBytes, err := r.client.HGet(ctx, key, "ts").Bytes()
			if err != nil {
				continue
			}
			if len(tsBytes) < 8 {
				continue
			}
			ts := int64(binary.BigEndian.Uint64(tsBytes))
			if time.Now().Unix()-ts > 300 {
				r.client.Del(ctx, key)
			}
		}
		if cursor == 0 {
			break
		}
	}
}

func (r *RedisStore) Close() error {
	return r.client.Close()
}
