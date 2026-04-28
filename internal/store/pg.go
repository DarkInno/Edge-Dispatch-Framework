package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

// PGStore handles PostgreSQL persistent storage.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore creates a new PGStore with optimized connection pool and auto-migration.
func NewPGStore(ctx context.Context, connStr string) (*PGStore, error) {
	config, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse pg config: %w", err)
	}

	// Connection pool tuning (pgxpool v5 official recommendations)
	// MaxConns: default 4, increase for high-concurrency dispatch API
	config.MaxConns = 25
	// MinConns: keep warm connections to avoid cold-start latency
	config.MinConns = 5
	// MaxConnLifetime: recycle connections to prevent stale state
	config.MaxConnLifetime = 30 * time.Minute
	// MaxConnLifetimeJitter: spread connection recycling to avoid thundering herd
	config.MaxConnLifetimeJitter = 5 * time.Minute
	// MaxConnIdleTime: close idle connections faster to free resources
	config.MaxConnIdleTime = 10 * time.Minute
	// HealthCheckPeriod: detect dead connections quickly
	config.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect pg: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping pg: %w", err)
	}
	s := &PGStore{pool: pool}
	if err := s.migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
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

	-- Composite index for ListActiveNodes (covers the WHERE status IN (...) filter)
	CREATE INDEX IF NOT EXISTS idx_nodes_status ON nodes(status) WHERE status IN ('ACTIVE', 'DEGRADED');

	-- Index for probe queries: node_id + probed_at composite for time-range lookups
	CREATE INDEX IF NOT EXISTS idx_probe_results_node_probed_at ON probe_results(node_id, probed_at DESC);

	-- Index for probe success filter (used in GetProbeScores)
	CREATE INDEX IF NOT EXISTS idx_probe_results_node_success ON probe_results(node_id, success, probed_at DESC) WHERE success = true;
	`
	_, err := s.pool.Exec(ctx, schema)
	return err
}

// CreateNode inserts a new node.
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

// GetNode retrieves a node by ID.
func (s *PGStore) GetNode(ctx context.Context, nodeID string) (*models.Node, error) {
	var n models.Node
	var endpointsJSON, capsJSON, scoresJSON []byte
	err := s.pool.QueryRow(ctx, `
		SELECT node_id, tenant_id, name, endpoints, region, isp, asn, capabilities, status, last_seen_at, scores, created_at, updated_at
		FROM nodes WHERE node_id = $1
	`, nodeID).Scan(&n.NodeID, &n.TenantID, &n.Name, &endpointsJSON, &n.Region, &n.ISP, &n.ASN, &capsJSON, &n.Status, &n.LastSeenAt, &scoresJSON, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get node: %w", err)
	}
	json.Unmarshal(endpointsJSON, &n.Endpoints)
	json.Unmarshal(capsJSON, &n.Capabilities)
	json.Unmarshal(scoresJSON, &n.Scores)
	return &n, nil
}

// ListActiveNodes returns all ACTIVE and DEGRADED nodes.
func (s *PGStore) ListActiveNodes(ctx context.Context) ([]models.Node, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT node_id, name, endpoints, region, isp, capabilities, status, scores
		FROM nodes WHERE status IN ('ACTIVE', 'DEGRADED')
	`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()

	var nodes []models.Node
	for rows.Next() {
		var n models.Node
		var endpointsJSON, capsJSON, scoresJSON []byte
		if err := rows.Scan(&n.NodeID, &n.Name, &endpointsJSON, &n.Region, &n.ISP, &capsJSON, &n.Status, &scoresJSON); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		json.Unmarshal(endpointsJSON, &n.Endpoints)
		json.Unmarshal(capsJSON, &n.Capabilities)
		json.Unmarshal(scoresJSON, &n.Scores)
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// UpdateNodeStatus updates a node's status.
func (s *PGStore) UpdateNodeStatus(ctx context.Context, nodeID string, status models.NodeStatus) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET status = $2, updated_at = NOW() WHERE node_id = $1
	`, nodeID, status)
	return err
}

// UpdateNodeScores updates a node's quality scores.
func (s *PGStore) UpdateNodeScores(ctx context.Context, nodeID string, scores models.NodeScores) error {
	scoresJSON, _ := json.Marshal(scores)
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET scores = $2, updated_at = NOW() WHERE node_id = $1
	`, nodeID, scoresJSON)
	return err
}

// UpdateNodeLastSeen updates the last seen timestamp.
func (s *PGStore) UpdateNodeLastSeen(ctx context.Context, nodeID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET last_seen_at = NOW(), updated_at = NOW() WHERE node_id = $1
	`, nodeID)
	return err
}

// SaveProbeResult stores a probe result.
func (s *PGStore) SaveProbeResult(ctx context.Context, pr models.ProbeResult) error {
	endpointJSON, _ := json.Marshal(pr.Endpoint)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO probe_results (node_id, endpoint, success, rtt_ms, error, probed_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, pr.NodeID, endpointJSON, pr.Success, pr.RTTMs, pr.Error, pr.ProbedAt)
	return err
}

// GetProbeScores computes aggregate probe scores for a node in a single query.
func (s *PGStore) GetProbeScores(ctx context.Context, nodeID string) (*models.ProbeScore, error) {
	ps := &models.ProbeScore{NodeID: nodeID}

	// Single query computes all 5 metrics at once (was 5 separate queries)
	err := s.pool.QueryRow(ctx, `
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
		return nil, fmt.Errorf("get probe scores: %w", err)
	}

	return ps, nil
}

// RevokeNode deletes a node from the database.
func (s *PGStore) RevokeNode(ctx context.Context, nodeID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM nodes WHERE node_id = $1`, nodeID)
	return err
}

// PoolStat returns connection pool statistics for monitoring.
func (s *PGStore) PoolStat() pgxpool.Stat {
	return *s.pool.Stat()
}

// Close closes the database connection pool.
func (s *PGStore) Close() {
	s.pool.Close()
}

// RedisStore handles Redis caching and ephemeral state.
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore creates a new RedisStore.
func NewRedisStore(ctx context.Context, addr, password string) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
	})
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("connect redis: %w", err)
	}
	return &RedisStore{client: client}, nil
}

// heartbeatKey returns the Redis key for heartbeat data.
func heartbeatKey(nodeID string) string { return "hb:" + nodeID }

// SaveHeartbeat stores heartbeat data with TTL.
func (r *RedisStore) SaveHeartbeat(ctx context.Context, hb models.HeartbeatRequest, ttl time.Duration) error {
	data, _ := json.Marshal(hb)
	return r.client.Set(ctx, heartbeatKey(hb.NodeID), data, ttl).Err()
}

// GetHeartbeat retrieves heartbeat data.
func (r *RedisStore) GetHeartbeat(ctx context.Context, nodeID string) (*models.HeartbeatRequest, error) {
	data, err := r.client.Get(ctx, heartbeatKey(nodeID)).Bytes()
	if err != nil {
		return nil, err
	}
	var hb models.HeartbeatRequest
	if err := json.Unmarshal(data, &hb); err != nil {
		return nil, err
	}
	return &hb, nil
}

// HasHeartbeat checks if a node has a recent heartbeat.
func (r *RedisStore) HasHeartbeat(ctx context.Context, nodeID string) bool {
	return r.client.Exists(ctx, heartbeatKey(nodeID)).Val() > 0
}

// GetAllHeartbeatNodes returns all node IDs with active heartbeats.
func (r *RedisStore) GetAllHeartbeatNodes(ctx context.Context) ([]string, error) {
	keys, _, err := r.client.Scan(ctx, 0, "hb:*", 1000).Result()
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(keys))
	for i, k := range keys {
		ids[i] = k[3:] // Strip "hb:" prefix
	}
	return ids, nil
}

// Close closes the Redis connection.
func (r *RedisStore) Close() error {
	return r.client.Close()
}
