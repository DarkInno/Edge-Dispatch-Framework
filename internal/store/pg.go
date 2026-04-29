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
		project_id TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL,
		endpoints JSONB NOT NULL DEFAULT '[]',
		region TEXT NOT NULL DEFAULT '',
		isp TEXT NOT NULL DEFAULT '',
		asn TEXT NOT NULL DEFAULT '',
		capabilities JSONB NOT NULL DEFAULT '{}',
		status TEXT NOT NULL DEFAULT 'REGISTERED',
		weight INT NOT NULL DEFAULT 100,
		labels JSONB NOT NULL DEFAULT '{}',
		disable_reason TEXT NOT NULL DEFAULT '',
		maintain_until TIMESTAMPTZ,
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
	CREATE INDEX IF NOT EXISTS idx_nodes_project ON nodes(project_id);
	CREATE INDEX IF NOT EXISTS idx_nodes_created_at ON nodes(created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_probe_results_node_probed_at ON probe_results(node_id, probed_at DESC);
	CREATE INDEX IF NOT EXISTS idx_probe_results_node_success ON probe_results(node_id, success, probed_at DESC) WHERE success = true;
	CREATE INDEX IF NOT EXISTS idx_probe_scores_covering ON probe_results(node_id, probed_at DESC, success, rtt_ms);

	CREATE TABLE IF NOT EXISTS tenants (
		tenant_id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS projects (
		project_id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL REFERENCES tenants(tenant_id),
		name TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS users (
		user_id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL DEFAULT 'default',
		email TEXT NOT NULL UNIQUE,
		display_name TEXT NOT NULL DEFAULT '',
		password_hash TEXT NOT NULL DEFAULT '',
		refresh_token_hash TEXT NOT NULL DEFAULT '',
		roles JSONB NOT NULL DEFAULT '[]',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS admin_policies (
		policy_id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL DEFAULT 'default',
		project_id TEXT NOT NULL DEFAULT 'default',
		name TEXT NOT NULL,
		type TEXT NOT NULL DEFAULT 'dispatch',
		content JSONB NOT NULL DEFAULT '{}',
		version INT NOT NULL DEFAULT 1,
		is_published BOOLEAN NOT NULL DEFAULT false,
		description TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS admin_policy_versions (
		version_id TEXT PRIMARY KEY,
		policy_id TEXT NOT NULL REFERENCES admin_policies(policy_id) ON DELETE CASCADE,
		version INT NOT NULL,
		content JSONB NOT NULL DEFAULT '{}',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS ingresses (
		ingress_id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL DEFAULT 'default',
		project_id TEXT NOT NULL DEFAULT 'default',
		name TEXT NOT NULL,
		type TEXT NOT NULL DEFAULT '302',
		domain TEXT NOT NULL DEFAULT '',
		config JSONB NOT NULL DEFAULT '{}',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS tasks (
		task_id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL DEFAULT 'default',
		project_id TEXT NOT NULL DEFAULT 'default',
		creator_id TEXT NOT NULL DEFAULT '',
		type TEXT NOT NULL DEFAULT 'purge',
		status TEXT NOT NULL DEFAULT 'pending',
		params JSONB NOT NULL DEFAULT '{}',
		result JSONB NOT NULL DEFAULT '{}',
		progress INT NOT NULL DEFAULT 0,
		total_nodes INT NOT NULL DEFAULT 0,
		done_nodes INT NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS audit_events (
		id BIGSERIAL PRIMARY KEY,
		tenant_id TEXT NOT NULL DEFAULT 'default',
		project_id TEXT NOT NULL DEFAULT 'default',
		actor_id TEXT NOT NULL DEFAULT '',
		actor_email TEXT NOT NULL DEFAULT '',
		action TEXT NOT NULL,
		resource_type TEXT NOT NULL DEFAULT '',
		resource_id TEXT NOT NULL DEFAULT '',
		before JSONB,
		after JSONB,
		request_id TEXT NOT NULL DEFAULT '',
		source_ip TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '',
		result TEXT NOT NULL DEFAULT 'success',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_tenants_name ON tenants(name);
	CREATE INDEX IF NOT EXISTS idx_projects_tenant ON projects(tenant_id);
	CREATE INDEX IF NOT EXISTS idx_users_tenant ON users(tenant_id);
	CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);
	CREATE INDEX IF NOT EXISTS idx_users_refresh_hash ON users(refresh_token_hash);
	CREATE INDEX IF NOT EXISTS idx_admin_policies_tenant ON admin_policies(tenant_id);
	CREATE INDEX IF NOT EXISTS idx_admin_policies_project ON admin_policies(project_id);
	CREATE INDEX IF NOT EXISTS idx_admin_policy_versions_policy ON admin_policy_versions(policy_id, version DESC);
	CREATE INDEX IF NOT EXISTS idx_ingresses_tenant ON ingresses(tenant_id);
	CREATE INDEX IF NOT EXISTS idx_ingresses_project ON ingresses(project_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_tenant ON tasks(tenant_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
	CREATE INDEX IF NOT EXISTS idx_audit_events_tenant ON audit_events(tenant_id);
	CREATE INDEX IF NOT EXISTS idx_audit_events_action ON audit_events(action);
	CREATE INDEX IF NOT EXISTS idx_audit_events_created_at ON audit_events(created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_audit_events_actor ON audit_events(actor_id);
	CREATE INDEX IF NOT EXISTS idx_audit_events_resource ON audit_events(resource_type, resource_id);
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
	labelsJSON, _ := json.Marshal(models.NodeLabels{})

	_, err := s.pool.Exec(ctx, `
		INSERT INTO nodes (node_id, tenant_id, project_id, name, endpoints, region, isp, capabilities, status, weight, labels, scores)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'REGISTERED', 100, $9, $10)
	`, nodeID, req.TenantID, req.ProjectID, req.NodeName, endpointsJSON, req.Region, req.ISP, capsJSON, labelsJSON, scoresJSON)
	if err != nil {
		return nil, fmt.Errorf("insert node: %w", err)
	}

	return &models.Node{
		NodeID:       nodeID,
		TenantID:     req.TenantID,
		ProjectID:    req.ProjectID,
		Name:         req.NodeName,
		Endpoints:    req.Endpoints,
		Region:       req.Region,
		ISP:          req.ISP,
		Capabilities: req.Capabilities,
		Status:       models.NodeStatusRegistered,
		Weight:       100,
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
	var endpointsJSON, capsJSON, scoresJSON, labelsJSON []byte
	err := pool.QueryRow(ctx, `
		SELECT node_id, tenant_id, project_id, name, endpoints, region, isp, asn, capabilities, status, weight, labels, disable_reason, maintain_until, last_seen_at, scores, created_at, updated_at
		FROM nodes WHERE node_id = $1
	`, nodeID).Scan(&n.NodeID, &n.TenantID, &n.ProjectID, &n.Name, &endpointsJSON, &n.Region, &n.ISP, &n.ASN, &capsJSON, &n.Status, &n.Weight, &labelsJSON, &n.DisableReason, &n.MaintainUntil, &n.LastSeenAt, &scoresJSON, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		s.logSlow("GetNode", start, nodeID)
		return nil, fmt.Errorf("get node: %w", err)
	}
	json.Unmarshal(endpointsJSON, &n.Endpoints)
	json.Unmarshal(capsJSON, &n.Capabilities)
	json.Unmarshal(scoresJSON, &n.Scores)
	json.Unmarshal(labelsJSON, &n.Labels)
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
		SELECT node_id, tenant_id, project_id, name, endpoints, region, isp, capabilities, status, weight, scores
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
		if err := rows.Scan(&n.NodeID, &n.TenantID, &n.ProjectID, &n.Name, &endpointsJSON, &n.Region, &n.ISP, &capsJSON, &n.Status, &n.Weight, &scoresJSON); err != nil {
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
	ctx    context.Context
	cancel context.CancelFunc
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
	bgCtx, bgCancel := context.WithCancel(context.Background())
	r := &RedisStore{client: client, hook: hook, ctx: bgCtx, cancel: bgCancel}
	go r.cleanupStaleLoop(bgCtx)
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

// MarkNodeRevoked stores a revoked node ID with TTL (24h) so heartbeats can be rejected.
func (r *RedisStore) MarkNodeRevoked(ctx context.Context, nodeID string) error {
	return r.client.Set(ctx, "revoked:"+nodeID, "1", 24*time.Hour).Err()
}

// IsNodeRevoked checks if a node ID has been revoked.
func (r *RedisStore) IsNodeRevoked(ctx context.Context, nodeID string) bool {
	val, err := r.client.Get(ctx, "revoked:"+nodeID).Result()
	if err != nil {
		return false
	}
	return val == "1"
}

// UnrevokeNode removes a revoked node from the revoked set.
func (r *RedisStore) UnrevokeNode(ctx context.Context, nodeID string) error {
	return r.client.Del(ctx, "revoked:"+nodeID).Err()
}

func (r *RedisStore) Close() error {
	if r.cancel != nil {
		r.cancel()
	}
	return r.client.Close()
}

// ─── Tenant CRUD ───────────────────────────────────────────────

func (s *PGStore) CreateTenant(ctx context.Context, tenant *models.Tenant) error {
	tenant.TenantID = "t_" + uuid.New().String()[:8]
	tenant.CreatedAt = time.Now()
	tenant.UpdatedAt = time.Now()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tenants (tenant_id, name, description, created_at, updated_at) VALUES ($1,$2,$3,$4,$5)`,
		tenant.TenantID, tenant.Name, tenant.Description, tenant.CreatedAt, tenant.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert tenant: %w", err)
	}
	return nil
}

func (s *PGStore) GetTenant(ctx context.Context, tenantID string) (*models.Tenant, error) {
	t := &models.Tenant{}
	err := s.readPoolOrPrimary().QueryRow(ctx,
		`SELECT tenant_id, name, description, created_at, updated_at FROM tenants WHERE tenant_id=$1`, tenantID).
		Scan(&t.TenantID, &t.Name, &t.Description, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get tenant: %w", err)
	}
	return t, nil
}

func (s *PGStore) ListTenants(ctx context.Context) ([]*models.Tenant, error) {
	rows, err := s.readPoolOrPrimary().Query(ctx, `SELECT tenant_id, name, description, created_at, updated_at FROM tenants ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	var tenants []*models.Tenant
	for rows.Next() {
		t := &models.Tenant{}
		if err := rows.Scan(&t.TenantID, &t.Name, &t.Description, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan tenant: %w", err)
		}
		tenants = append(tenants, t)
	}
	return tenants, rows.Err()
}

func (s *PGStore) UpdateTenant(ctx context.Context, tenant *models.Tenant) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE tenants SET name=$2, description=$3, updated_at=NOW() WHERE tenant_id=$1`,
		tenant.TenantID, tenant.Name, tenant.Description)
	return err
}

func (s *PGStore) DeleteTenant(ctx context.Context, tenantID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM tenants WHERE tenant_id=$1`, tenantID)
	return err
}

// ─── Project CRUD ──────────────────────────────────────────────

func (s *PGStore) CreateProject(ctx context.Context, project *models.Project) error {
	project.ProjectID = "p_" + uuid.New().String()[:8]
	project.CreatedAt = time.Now()
	project.UpdatedAt = time.Now()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO projects (project_id, tenant_id, name, description, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6)`,
		project.ProjectID, project.TenantID, project.Name, project.Description, project.CreatedAt, project.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	return nil
}

func (s *PGStore) GetProject(ctx context.Context, projectID string) (*models.Project, error) {
	p := &models.Project{}
	err := s.readPoolOrPrimary().QueryRow(ctx,
		`SELECT project_id, tenant_id, name, description, created_at, updated_at FROM projects WHERE project_id=$1`, projectID).
		Scan(&p.ProjectID, &p.TenantID, &p.Name, &p.Description, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	return p, nil
}

func (s *PGStore) ListProjects(ctx context.Context, tenantID string) ([]*models.Project, error) {
	rows, err := s.readPoolOrPrimary().Query(ctx,
		`SELECT project_id, tenant_id, name, description, created_at, updated_at FROM projects WHERE tenant_id=$1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var projects []*models.Project
	for rows.Next() {
		p := &models.Project{}
		if err := rows.Scan(&p.ProjectID, &p.TenantID, &p.Name, &p.Description, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (s *PGStore) UpdateProject(ctx context.Context, project *models.Project) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE projects SET name=$2, description=$3, updated_at=NOW() WHERE project_id=$1`,
		project.ProjectID, project.Name, project.Description)
	return err
}

func (s *PGStore) DeleteProject(ctx context.Context, projectID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM projects WHERE project_id=$1`, projectID)
	return err
}

// ─── User CRUD ─────────────────────────────────────────────────

func (s *PGStore) CreateUser(ctx context.Context, user *models.User, passwordHash string) error {
	user.UserID = "u_" + uuid.New().String()[:8]
	user.CreatedAt = time.Now()
	user.UpdatedAt = time.Now()
	rolesJSON, _ := json.Marshal(user.Roles)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO users (user_id, tenant_id, email, display_name, password_hash, roles, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		user.UserID, user.TenantID, user.Email, user.DisplayName, passwordHash, rolesJSON, user.CreatedAt, user.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

func (s *PGStore) GetUserByEmail(ctx context.Context, email string) (*models.User, string, error) {
	u := &models.User{}
	var passwordHash string
	var rolesJSON []byte
	err := s.readPoolOrPrimary().QueryRow(ctx,
		`SELECT user_id, tenant_id, email, display_name, password_hash, roles, created_at, updated_at FROM users WHERE email=$1`, email).
		Scan(&u.UserID, &u.TenantID, &u.Email, &u.DisplayName, &passwordHash, &rolesJSON, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, "", fmt.Errorf("get user by email: %w", err)
	}
	json.Unmarshal(rolesJSON, &u.Roles)
	return u, passwordHash, nil
}

func (s *PGStore) GetUser(ctx context.Context, userID string) (*models.User, error) {
	u := &models.User{}
	var rolesJSON []byte
	err := s.readPoolOrPrimary().QueryRow(ctx,
		`SELECT user_id, tenant_id, email, display_name, roles, created_at, updated_at FROM users WHERE user_id=$1`, userID).
		Scan(&u.UserID, &u.TenantID, &u.Email, &u.DisplayName, &rolesJSON, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	json.Unmarshal(rolesJSON, &u.Roles)
	return u, nil
}

func (s *PGStore) ListUsers(ctx context.Context, tenantID string) ([]*models.User, error) {
	rows, err := s.readPoolOrPrimary().Query(ctx,
		`SELECT user_id, tenant_id, email, display_name, roles, created_at, updated_at FROM users WHERE tenant_id=$1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var users []*models.User
	for rows.Next() {
		u := &models.User{}
		var rolesJSON []byte
		if err := rows.Scan(&u.UserID, &u.TenantID, &u.Email, &u.DisplayName, &rolesJSON, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		json.Unmarshal(rolesJSON, &u.Roles)
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *PGStore) UpdateUser(ctx context.Context, user *models.User) error {
	rolesJSON, _ := json.Marshal(user.Roles)
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET display_name=$2, roles=$3, updated_at=NOW() WHERE user_id=$1`,
		user.UserID, user.DisplayName, rolesJSON)
	return err
}

func (s *PGStore) DeleteUser(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM users WHERE user_id=$1`, userID)
	return err
}

func (s *PGStore) UpdateUserRefreshToken(ctx context.Context, userID, refreshHash string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET refresh_token_hash=$2, updated_at=NOW() WHERE user_id=$1`,
		userID, refreshHash)
	return err
}

func (s *PGStore) GetUserIDByRefreshToken(ctx context.Context, refreshHash string) (string, error) {
	if refreshHash == "" {
		return "", fmt.Errorf("empty refresh hash")
	}
	var userID string
	err := s.readPoolOrPrimary().QueryRow(ctx,
		`SELECT user_id FROM users WHERE refresh_token_hash=$1`, refreshHash).Scan(&userID)
	if err != nil {
		return "", fmt.Errorf("find user by refresh token: %w", err)
	}
	return userID, nil
}

// ─── Admin Policy CRUD ─────────────────────────────────────────

func (s *PGStore) CreateAdminPolicy(ctx context.Context, policy *models.AdminPolicy) error {
	policy.PolicyID = "pol_" + uuid.New().String()[:8]
	policy.Version = 1
	policy.CreatedAt = time.Now()
	policy.UpdatedAt = time.Now()
	contentJSON, _ := json.Marshal(policy.Content)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO admin_policies (policy_id, tenant_id, project_id, name, type, content, version, is_published, description, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		policy.PolicyID, policy.TenantID, policy.ProjectID, policy.Name, policy.Type, contentJSON, policy.Version, policy.IsPublished, policy.Description, policy.CreatedAt, policy.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert admin policy: %w", err)
	}
	return nil
}

func (s *PGStore) GetAdminPolicy(ctx context.Context, policyID string) (*models.AdminPolicy, error) {
	p := &models.AdminPolicy{}
	var contentJSON []byte
	err := s.readPoolOrPrimary().QueryRow(ctx,
		`SELECT policy_id, tenant_id, project_id, name, type, content, version, is_published, description, created_at, updated_at FROM admin_policies WHERE policy_id=$1`, policyID).
		Scan(&p.PolicyID, &p.TenantID, &p.ProjectID, &p.Name, &p.Type, &contentJSON, &p.Version, &p.IsPublished, &p.Description, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get admin policy: %w", err)
	}
	p.Content = contentJSON
	return p, nil
}

func (s *PGStore) ListAdminPolicies(ctx context.Context, tenantID, projectID string) ([]*models.AdminPolicy, error) {
	query := `SELECT policy_id, tenant_id, project_id, name, type, content, version, is_published, description, created_at, updated_at FROM admin_policies WHERE 1=1`
	args := []interface{}{}
	i := 1
	if tenantID != "" {
		query += fmt.Sprintf(" AND tenant_id=$%d", i)
		args = append(args, tenantID)
		i++
	}
	if projectID != "" {
		query += fmt.Sprintf(" AND project_id=$%d", i)
		args = append(args, projectID)
	}
	query += " ORDER BY created_at DESC"
	rows, err := s.readPoolOrPrimary().Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list admin policies: %w", err)
	}
	defer rows.Close()
	var policies []*models.AdminPolicy
	for rows.Next() {
		p := &models.AdminPolicy{}
		var contentJSON []byte
		if err := rows.Scan(&p.PolicyID, &p.TenantID, &p.ProjectID, &p.Name, &p.Type, &contentJSON, &p.Version, &p.IsPublished, &p.Description, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan policy: %w", err)
		}
		p.Content = contentJSON
		policies = append(policies, p)
	}
	return policies, rows.Err()
}

func (s *PGStore) UpdateAdminPolicy(ctx context.Context, policy *models.AdminPolicy) error {
	contentJSON, _ := json.Marshal(policy.Content)
	_, err := s.pool.Exec(ctx,
		`UPDATE admin_policies SET name=$2, type=$3, content=$4, version=version+1, description=$5, updated_at=NOW() WHERE policy_id=$1`,
		policy.PolicyID, policy.Name, string(policy.Type), contentJSON, policy.Description)
	return err
}

func (s *PGStore) PublishAdminPolicy(ctx context.Context, policyID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE admin_policies SET is_published=true, updated_at=NOW() WHERE policy_id=$1`, policyID)
	return err
}

func (s *PGStore) DeleteAdminPolicy(ctx context.Context, policyID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM admin_policies WHERE policy_id=$1`, policyID)
	return err
}

// ─── Policy Version CRUD ──────────────────────────────────────

func (s *PGStore) SavePolicyVersion(ctx context.Context, pv *models.AdminPolicyVersion) error {
	pv.VersionID = "pv_" + uuid.New().String()[:8]
	pv.CreatedAt = time.Now()
	contentJSON, _ := json.Marshal(pv.Content)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO admin_policy_versions (version_id, policy_id, version, content, created_at) VALUES ($1,$2,$3,$4,$5)`,
		pv.VersionID, pv.PolicyID, pv.Version, contentJSON, pv.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert policy version: %w", err)
	}
	return nil
}

func (s *PGStore) GetPolicyVersions(ctx context.Context, policyID string) ([]*models.AdminPolicyVersion, error) {
	rows, err := s.readPoolOrPrimary().Query(ctx,
		`SELECT version_id, policy_id, version, content, created_at FROM admin_policy_versions WHERE policy_id=$1 ORDER BY version DESC`, policyID)
	if err != nil {
		return nil, fmt.Errorf("list policy versions: %w", err)
	}
	defer rows.Close()
	var versions []*models.AdminPolicyVersion
	for rows.Next() {
		v := &models.AdminPolicyVersion{}
		var contentJSON []byte
		if err := rows.Scan(&v.VersionID, &v.PolicyID, &v.Version, &contentJSON, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan policy version: %w", err)
		}
		v.Content = contentJSON
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

func (s *PGStore) RollbackPolicy(ctx context.Context, policyID string, version int, content json.RawMessage) error {
	contentJSON, _ := json.Marshal(content)
	_, err := s.pool.Exec(ctx,
		`UPDATE admin_policies SET content=$2, version=$3, is_published=true, updated_at=NOW() WHERE policy_id=$1`,
		policyID, contentJSON, version)
	return err
}

// ─── Ingress CRUD ──────────────────────────────────────────────

func (s *PGStore) CreateIngress(ctx context.Context, ingress *models.Ingress) error {
	ingress.IngressID = "ing_" + uuid.New().String()[:8]
	ingress.CreatedAt = time.Now()
	ingress.UpdatedAt = time.Now()
	configJSON, _ := json.Marshal(ingress.Config)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO ingresses (ingress_id, tenant_id, project_id, name, type, domain, config, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		ingress.IngressID, ingress.TenantID, ingress.ProjectID, ingress.Name, ingress.Type, ingress.Domain, configJSON, ingress.CreatedAt, ingress.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert ingress: %w", err)
	}
	return nil
}

func (s *PGStore) GetIngress(ctx context.Context, ingressID string) (*models.Ingress, error) {
	ing := &models.Ingress{}
	var configJSON []byte
	err := s.readPoolOrPrimary().QueryRow(ctx,
		`SELECT ingress_id, tenant_id, project_id, name, type, domain, config, created_at, updated_at FROM ingresses WHERE ingress_id=$1`, ingressID).
		Scan(&ing.IngressID, &ing.TenantID, &ing.ProjectID, &ing.Name, &ing.Type, &ing.Domain, &configJSON, &ing.CreatedAt, &ing.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get ingress: %w", err)
	}
	ing.Config = configJSON
	return ing, nil
}

func (s *PGStore) ListIngresses(ctx context.Context, tenantID, projectID string) ([]*models.Ingress, error) {
	query := `SELECT ingress_id, tenant_id, project_id, name, type, domain, config, created_at, updated_at FROM ingresses WHERE 1=1`
	args := []interface{}{}
	i := 1
	if tenantID != "" {
		query += fmt.Sprintf(" AND tenant_id=$%d", i)
		args = append(args, tenantID)
		i++
	}
	if projectID != "" {
		query += fmt.Sprintf(" AND project_id=$%d", i)
		args = append(args, projectID)
	}
	query += " ORDER BY created_at DESC"
	rows, err := s.readPoolOrPrimary().Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list ingresses: %w", err)
	}
	defer rows.Close()
	var ingresses []*models.Ingress
	for rows.Next() {
		ing := &models.Ingress{}
		var configJSON []byte
		if err := rows.Scan(&ing.IngressID, &ing.TenantID, &ing.ProjectID, &ing.Name, &ing.Type, &ing.Domain, &configJSON, &ing.CreatedAt, &ing.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan ingress: %w", err)
		}
		ing.Config = configJSON
		ingresses = append(ingresses, ing)
	}
	return ingresses, rows.Err()
}

func (s *PGStore) UpdateIngress(ctx context.Context, ingress *models.Ingress) error {
	configJSON, _ := json.Marshal(ingress.Config)
	_, err := s.pool.Exec(ctx,
		`UPDATE ingresses SET name=$2, type=$3, domain=$4, config=$5, updated_at=NOW() WHERE ingress_id=$1`,
		ingress.IngressID, ingress.Name, string(ingress.Type), ingress.Domain, configJSON)
	return err
}

func (s *PGStore) DeleteIngress(ctx context.Context, ingressID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM ingresses WHERE ingress_id=$1`, ingressID)
	return err
}

// ─── Task CRUD ─────────────────────────────────────────────────

func (s *PGStore) CreateTask(ctx context.Context, task *models.Task) error {
	task.TaskID = "task_" + uuid.New().String()[:8]
	task.CreatedAt = time.Now()
	task.UpdatedAt = time.Now()
	paramsJSON, _ := json.Marshal(task.Params)
	resultJSON, _ := json.Marshal(task.Result)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tasks (task_id, tenant_id, project_id, creator_id, type, status, params, result, progress, total_nodes, done_nodes, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		task.TaskID, task.TenantID, task.ProjectID, task.CreatorID, task.Type, task.Status, paramsJSON, resultJSON, task.Progress, task.TotalNodes, task.DoneNodes, task.CreatedAt, task.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

func (s *PGStore) GetTask(ctx context.Context, taskID string) (*models.Task, error) {
	t := &models.Task{}
	var paramsJSON, resultJSON []byte
	err := s.readPoolOrPrimary().QueryRow(ctx,
		`SELECT task_id, tenant_id, project_id, creator_id, type, status, params, result, progress, total_nodes, done_nodes, created_at, updated_at FROM tasks WHERE task_id=$1`, taskID).
		Scan(&t.TaskID, &t.TenantID, &t.ProjectID, &t.CreatorID, &t.Type, &t.Status, &paramsJSON, &resultJSON, &t.Progress, &t.TotalNodes, &t.DoneNodes, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	t.Params = paramsJSON
	t.Result = resultJSON
	return t, nil
}

func (s *PGStore) ListTasks(ctx context.Context, tenantID, statusFilter, taskType string, limit, offset int) ([]*models.Task, int, error) {
	var total int
	countQuery := `SELECT COUNT(*) FROM tasks WHERE 1=1`
	listQuery := `SELECT task_id, tenant_id, project_id, creator_id, type, status, params, result, progress, total_nodes, done_nodes, created_at, updated_at FROM tasks WHERE 1=1`
	args := []interface{}{}
	i := 1
	if tenantID != "" {
		cond := fmt.Sprintf(" AND tenant_id=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, tenantID)
		i++
	}
	if statusFilter != "" {
		cond := fmt.Sprintf(" AND status=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, statusFilter)
		i++
	}
	if taskType != "" {
		cond := fmt.Sprintf(" AND type=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, taskType)
		i++
	}
	if err := s.readPoolOrPrimary().QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count tasks: %w", err)
	}
	listQuery += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", i, i+1)
	args = append(args, limit, offset)
	rows, err := s.readPoolOrPrimary().Query(ctx, listQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	var tasks []*models.Task
	for rows.Next() {
		t := &models.Task{}
		var paramsJSON, resultJSON []byte
		if err := rows.Scan(&t.TaskID, &t.TenantID, &t.ProjectID, &t.CreatorID, &t.Type, &t.Status, &paramsJSON, &resultJSON, &t.Progress, &t.TotalNodes, &t.DoneNodes, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan task: %w", err)
		}
		t.Params = paramsJSON
		t.Result = resultJSON
		tasks = append(tasks, t)
	}
	return tasks, total, rows.Err()
}

func (s *PGStore) UpdateTaskStatus(ctx context.Context, taskID string, status models.TaskStatus, result json.RawMessage, progress, doneNodes int) error {
	resultJSON, _ := json.Marshal(result)
	_, err := s.pool.Exec(ctx,
		`UPDATE tasks SET status=$2, result=$3, progress=$4, done_nodes=$5, updated_at=NOW() WHERE task_id=$1`,
		taskID, status, resultJSON, progress, doneNodes)
	return err
}

// ─── Audit CRUD ────────────────────────────────────────────────

func (s *PGStore) CreateAuditEvent(ctx context.Context, event *models.AuditEvent) error {
	beforeJSON, _ := json.Marshal(event.Before)
	afterJSON, _ := json.Marshal(event.After)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO audit_events (tenant_id, project_id, actor_id, actor_email, action, resource_type, resource_id, before, after, request_id, source_ip, user_agent, result, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		event.TenantID, event.ProjectID, event.ActorID, event.ActorEmail, event.Action, event.ResourceType, event.ResourceID, beforeJSON, afterJSON, event.RequestID, event.SourceIP, event.UserAgent, event.Result, time.Now())
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

func (s *PGStore) QueryAuditEvents(ctx context.Context, query models.AuditQuery) ([]*models.AuditEvent, int, error) {
	if query.Limit <= 0 {
		query.Limit = 50
	}
	countQuery := `SELECT COUNT(*) FROM audit_events WHERE 1=1`
	listQuery := `SELECT id, tenant_id, project_id, actor_id, actor_email, action, resource_type, resource_id, before, after, request_id, source_ip, user_agent, result, created_at FROM audit_events WHERE 1=1`
	args := []interface{}{}
	i := 1
	if query.ActorID != "" {
		cond := fmt.Sprintf(" AND actor_id=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, query.ActorID)
		i++
	}
	if query.Action != "" {
		cond := fmt.Sprintf(" AND action=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, query.Action)
		i++
	}
	if query.ResourceType != "" {
		cond := fmt.Sprintf(" AND resource_type=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, query.ResourceType)
		i++
	}
	if query.ResourceID != "" {
		cond := fmt.Sprintf(" AND resource_id=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, query.ResourceID)
		i++
	}
	if query.Result != "" {
		cond := fmt.Sprintf(" AND result=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, query.Result)
		i++
	}
	if query.TenantID != "" {
		cond := fmt.Sprintf(" AND tenant_id=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, query.TenantID)
		i++
	}
	if query.ProjectID != "" {
		cond := fmt.Sprintf(" AND project_id=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, query.ProjectID)
		i++
	}
	if !query.Since.IsZero() {
		cond := fmt.Sprintf(" AND created_at >= $%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, query.Since)
		i++
	}
	if !query.Until.IsZero() {
		cond := fmt.Sprintf(" AND created_at <= $%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, query.Until)
		i++
	}
	var total int
	if err := s.readPoolOrPrimary().QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit events: %w", err)
	}
	listQuery += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", i, i+1)
	args = append(args, query.Limit, query.Offset)
	rows, err := s.readPoolOrPrimary().Query(ctx, listQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query audit events: %w", err)
	}
	defer rows.Close()
	var events []*models.AuditEvent
	for rows.Next() {
		e := &models.AuditEvent{}
		var beforeJSON, afterJSON []byte
		if err := rows.Scan(&e.ID, &e.TenantID, &e.ProjectID, &e.ActorID, &e.ActorEmail, &e.Action, &e.ResourceType, &e.ResourceID, &beforeJSON, &afterJSON, &e.RequestID, &e.SourceIP, &e.UserAgent, &e.Result, &e.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan audit event: %w", err)
		}
		e.Before = beforeJSON
		e.After = afterJSON
		events = append(events, e)
	}
	return events, total, rows.Err()
}

// ─── Node Admin Operations ─────────────────────────────────────

func (s *PGStore) NodeDisable(ctx context.Context, nodeID, reason, message string, until time.Time) error {
	if until.IsZero() {
		_, err := s.pool.Exec(ctx,
			`UPDATE nodes SET status='DISABLED', disable_reason=$2, updated_at=NOW() WHERE node_id=$1`,
			nodeID, reason)
		if err == nil {
			s.InvalidateNodeCache(nodeID)
		}
		return err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE nodes SET status='DISABLED', disable_reason=$2, maintain_until=$3, updated_at=NOW() WHERE node_id=$1`,
		nodeID, reason, until)
	if err == nil {
		s.InvalidateNodeCache(nodeID)
	}
	return err
}

func (s *PGStore) NodeEnable(ctx context.Context, nodeID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE nodes SET status='ACTIVE', disable_reason='', maintain_until=NULL, updated_at=NOW() WHERE node_id=$1`,
		nodeID)
	if err == nil {
		s.InvalidateNodeCache(nodeID)
	}
	return err
}

func (s *PGStore) ListNodeIDsByTenant(ctx context.Context, tenantID string) ([]string, error) {
	rows, err := s.readPoolOrPrimary().Query(ctx,
		`SELECT node_id FROM nodes WHERE tenant_id=$1`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list node ids by tenant: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *PGStore) ListNodes(ctx context.Context, tenantID, statusFilter, regionFilter, ispFilter, projectID string, limit, offset int) ([]*models.Node, int, error) {
	var total int
	countQuery := `SELECT COUNT(*) FROM nodes WHERE 1=1`
	listQuery := `SELECT node_id, tenant_id, project_id, name, endpoints, region, isp, asn, capabilities, status, weight, labels, disable_reason, maintain_until, last_seen_at, scores, created_at, updated_at FROM nodes WHERE 1=1`
	args := []interface{}{}
	i := 1
	if tenantID != "" {
		cond := fmt.Sprintf(" AND tenant_id=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, tenantID)
		i++
	}
	if projectID != "" {
		cond := fmt.Sprintf(" AND project_id=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, projectID)
		i++
	}
	if statusFilter != "" {
		cond := fmt.Sprintf(" AND status=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, statusFilter)
		i++
	}
	if regionFilter != "" {
		cond := fmt.Sprintf(" AND region=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, regionFilter)
		i++
	}
	if ispFilter != "" {
		cond := fmt.Sprintf(" AND isp=$%d", i)
		countQuery += cond
		listQuery += cond
		args = append(args, ispFilter)
		i++
	}
	if err := s.readPoolOrPrimary().QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count nodes: %w", err)
	}
	listQuery += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", i, i+1)
	args = append(args, limit, offset)
	rows, err := s.readPoolOrPrimary().Query(ctx, listQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()
	var nodes []*models.Node
	for rows.Next() {
		n := &models.Node{}
		var endpointsJSON, capsJSON, scoresJSON, labelsJSON []byte
		if err := rows.Scan(&n.NodeID, &n.TenantID, &n.ProjectID, &n.Name, &endpointsJSON, &n.Region, &n.ISP, &n.ASN, &capsJSON, &n.Status, &n.Weight, &labelsJSON, &n.DisableReason, &n.MaintainUntil, &n.LastSeenAt, &scoresJSON, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan node: %w", err)
		}
		json.Unmarshal(endpointsJSON, &n.Endpoints)
		json.Unmarshal(capsJSON, &n.Capabilities)
		json.Unmarshal(scoresJSON, &n.Scores)
		json.Unmarshal(labelsJSON, &n.Labels)
		nodes = append(nodes, n)
	}
	return nodes, total, rows.Err()
}

// ─── Dashboard ─────────────────────────────────────────────────

func (s *PGStore) GetDashboardData(ctx context.Context, tenantID string) (*models.DashboardMetrics, error) {
	dm := &models.DashboardMetrics{}
	var onlineCount, offlineCount, totalCount int
	s.readPoolOrPrimary().QueryRow(ctx,
		`SELECT COALESCE(SUM(CASE WHEN status='ACTIVE' OR status='DEGRADED' THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN status='OFFLINE' THEN 1 ELSE 0 END),0),
				COUNT(*) FROM nodes WHERE tenant_id=$1`, tenantID).
		Scan(&onlineCount, &offlineCount, &totalCount)
	dm.OnlineNodes = onlineCount
	dm.OfflineNodes = offlineCount
	dm.TotalNodes = totalCount
	return dm, nil
}
