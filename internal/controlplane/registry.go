package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/darkinno/edge-dispatch-framework/internal/models"
	"github.com/darkinno/edge-dispatch-framework/internal/store"
)

type Registry struct {
	pg    *store.PGStore
	redis *store.RedisStore
}

func NewRegistry(pg *store.PGStore, redis *store.RedisStore) *Registry {
	return &Registry{pg: pg, redis: redis}
}

func (r *Registry) Register(ctx context.Context, req models.RegisterRequest) (*models.RegisterResponse, error) {
	token := uuid.New().String()

	node, err := r.pg.CreateNode(ctx, req, token)
	if err != nil {
		return nil, fmt.Errorf("create node: %w", err)
	}

	slog.Info("node registered", "node_id", node.NodeID, "name", node.Name)

	return &models.RegisterResponse{
		NodeID: node.NodeID,
		Auth: models.NodeAuth{
			Type:  "bearer",
			Token: token,
			Exp:   time.Now().Add(365 * 24 * time.Hour).Unix(),
		},
	}, nil
}

func (r *Registry) GetNode(ctx context.Context, nodeID string) (*models.Node, error) {
	return r.pg.GetNode(ctx, nodeID)
}

func (r *Registry) RevokeNode(ctx context.Context, nodeID string) error {
	slog.Warn("revoking node", "node_id", nodeID)
	return r.pg.RevokeNode(ctx, nodeID)
}

func (r *Registry) ListActiveNodes(ctx context.Context) ([]*models.Node, error) {
	return r.pg.ListActiveNodes(ctx)
}

func (r *Registry) UpdateStatus(ctx context.Context, nodeID string, status models.NodeStatus) error {
	slog.Info("updating node status", "node_id", nodeID, "status", status)
	return r.pg.UpdateNodeStatus(ctx, nodeID, status)
}

func (r *Registry) CountByStatus(status string) int {
	ctx := context.Background()
	count, err := r.pg.CountByStatus(ctx, status)
	if err != nil {
		slog.Warn("count by status failed", "status", status, "err", err)
		return 0
	}
	return count
}
