package controlplane

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
	"github.com/darkinno/edge-dispatch-framework/internal/store"
)

type EventCallback func(eventType string, nodeID string, message string)

type Heartbeat struct {
	pg      *store.PGStore
	redis   *store.RedisStore
	cfg     *config.ControlPlaneConfig
	OnEvent EventCallback
}

func NewHeartbeat(pg *store.PGStore, redis *store.RedisStore, cfg *config.ControlPlaneConfig) *Heartbeat {
	return &Heartbeat{pg: pg, redis: redis, cfg: cfg}
}

func (h *Heartbeat) emit(eventType string, nodeID string, message string) {
	if h.OnEvent != nil {
		h.OnEvent(eventType, nodeID, message)
	}
	slog.Info("heartbeat event", "event_type", eventType, "node_id", nodeID, "message", message)
}

func (h *Heartbeat) ProcessHeartbeat(ctx context.Context, req models.HeartbeatRequest) error {
	if err := h.redis.SaveHeartbeat(ctx, req, h.cfg.HeartbeatTTL); err != nil {
		return fmt.Errorf("save heartbeat: %w", err)
	}

	if err := h.pg.UpdateNodeLastSeen(ctx, req.NodeID); err != nil {
		return fmt.Errorf("update last seen: %w", err)
	}

	node, err := h.pg.GetNode(ctx, req.NodeID)
	if err != nil {
		return fmt.Errorf("get node: %w", err)
	}

	if node.Status == models.NodeStatusRegistered {
		if err := h.pg.UpdateNodeStatus(ctx, req.NodeID, models.NodeStatusActive); err != nil {
			return fmt.Errorf("activate node: %w", err)
		}
		h.emit(models.EventNodeOnline, req.NodeID, "node activated")
	}

	return nil
}

func (h *Heartbeat) CheckNodeHealth(ctx context.Context, nodeID string) error {
	if h.redis.HasHeartbeat(ctx, nodeID) {
		return nil
	}

	node, err := h.pg.GetNode(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("get node: %w", err)
	}

	switch node.Status {
	case models.NodeStatusActive:
		if err := h.pg.UpdateNodeStatus(ctx, nodeID, models.NodeStatusDegraded); err != nil {
			return fmt.Errorf("degrade node: %w", err)
		}
		h.emit(models.EventNodeDegraded, nodeID, "heartbeat lost, node degraded")
	case models.NodeStatusDegraded:
		if err := h.pg.UpdateNodeStatus(ctx, nodeID, models.NodeStatusOffline); err != nil {
			return fmt.Errorf("offline node: %w", err)
		}
		h.emit(models.EventNodeOffline, nodeID, "heartbeat lost, node offline")
	}

	return nil
}
