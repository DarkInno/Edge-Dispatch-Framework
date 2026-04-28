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

// NewPGStore creates a new PGStore with auto-migration.
func NewPGStore(ctx context.Context, connStr string) (*PGStore, error) {
	pool, err := pgxpool.New(ctx, connStr)
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

	CREATE INDEX IF NOT EXISTS idx_probe_results_node_id ON probe_results(node_id);
	CREATE INDEX IF NOT EXISTS idx_probe_results_probed_at ON probe_results(probed_at);
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

// GetProbeScores computes aggregate probe scores for a node.
func (s *PGStore) GetProbeScores(ctx context.Context, nodeID string) (*models.ProbeScore, error) {
	ps := &models.ProbeScore{NodeID: nodeID}

	// 1-minute success rate
	s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END)::float / NULLIF(COUNT(*), 0), 0)
		FROM probe_results WHERE node_id = $1 AND probed_at > NOW() - INTERVAL '1 minute'
	`, nodeID).Scan(&ps.SuccessRate1m)

	// 5-minute success rate
	s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN success THEN 1 ELSE 0 END)::float / NULLIF(COUNT(*), 0), 0)
		FROM probe_results WHERE node_id = $1 AND probed_at > NOW() - INTERVAL '5 minutes'
	`, nodeID).Scan(&ps.SuccessRate5m)

	// P50/P95 RTT from recent probes
	s.pool.QueryRow(ctx, `
		SELECT COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY rtt_ms), 0)
		FROM probe_results WHERE node_id = $1 AND probed_at > NOW() - INTERVAL '5 minutes' AND success = true
	`, nodeID).Scan(&ps.RTTP50)

	s.pool.QueryRow(ctx, `
		SELECT COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY rtt_ms), 0)
		FROM probe_results WHERE node_id = $1 AND probed_at > NOW() - INTERVAL '5 minutes' AND success = true
	`, nodeID).Scan(&ps.RTTP95)

	s.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(probed_at), '1970-01-01') FROM probe_results WHERE node_id = $1 AND success = true
	`, nodeID).Scan(&ps.LastOkAt)

	return ps, nil
}

// RevokeNode deletes a node from the database.
func (s *PGStore) RevokeNode(ctx context.Context, nodeID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM nodes WHERE node_id = $1`, nodeID)
	return err
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
