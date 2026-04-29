package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
	"github.com/darkinno/edge-dispatch-framework/internal/store"
)

type EventCallback func(eventType string, nodeID string, message string)

// ContentSummaryHandler processes content summaries from edge node heartbeats.
type ContentSummaryHandler interface {
	UpsertSummary(context.Context, models.ContentSummary) error
}

type Heartbeat struct {
	pg           *store.PGStore
	redis        *store.RedisStore
	cfg          *config.ControlPlaneConfig
	contentStore ContentSummaryHandler
	OnEvent      EventCallback
	batchCh      chan batchHeartbeat
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

type batchHeartbeat struct {
	ctx context.Context
	req models.HeartbeatRequest
	err chan error
}

func NewHeartbeat(pg *store.PGStore, redis *store.RedisStore, cfg *config.ControlPlaneConfig) *Heartbeat {
	return &Heartbeat{
		pg:      pg,
		redis:   redis,
		cfg:     cfg,
		batchCh: make(chan batchHeartbeat, 256),
	}
}

func (h *Heartbeat) Start(ctx context.Context) {
	ctx, h.cancel = context.WithCancel(ctx)
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		batch := make([]batchHeartbeat, 0, 32)
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case bh := <-h.batchCh:
				batch = append(batch, bh)
				drain:
				for len(batch) < cap(batch) {
					select {
					case bh := <-h.batchCh:
						batch = append(batch, bh)
					default:
						break drain
					}
				}
				h.processBatch(batch)
				batch = batch[:0]
				timer.Reset(50 * time.Millisecond)
			case <-timer.C:
				timer.Reset(50 * time.Millisecond)
			}
		}
	}()
}

func (h *Heartbeat) Stop() {
	if h.cancel != nil {
		h.cancel()
	}
	h.wg.Wait()
}

func (h *Heartbeat) processBatch(batch []batchHeartbeat) {
	for _, bh := range batch {
		err := h.processOne(bh.ctx, bh.req)
		bh.err <- err
	}
}

func (h *Heartbeat) processOne(ctx context.Context, req models.HeartbeatRequest) error {
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

	// Don't re-activate disabled or maintenance nodes
	if node.Status == models.NodeStatusDisabled || node.Status == models.NodeStatusMaintenance {
		return nil
	}

	if node.Status == models.NodeStatusRegistered {
		if err := h.pg.UpdateNodeStatus(ctx, req.NodeID, models.NodeStatusActive); err != nil {
			return fmt.Errorf("activate node: %w", err)
		}
		h.emit(models.EventNodeOnline, req.NodeID, "node activated")
	}

	if h.contentStore != nil && req.ContentSummary != nil {
		req.ContentSummary.NodeID = req.NodeID
		if err := h.contentStore.UpsertSummary(ctx, *req.ContentSummary); err != nil {
			slog.Warn("content summary upsert failed", "node_id", req.NodeID, "err", err)
		}
	}

	return nil
}

// SetContentStore enables content summary processing from edge heartbeats (v0.2+).
func (h *Heartbeat) SetContentStore(cs ContentSummaryHandler) {
	h.contentStore = cs
}

func (h *Heartbeat) emit(eventType string, nodeID string, message string) {
	if h.OnEvent != nil {
		h.OnEvent(eventType, nodeID, message)
	}
	slog.Info("heartbeat event", "event_type", eventType, "node_id", nodeID, "message", message)
}

func (h *Heartbeat) ProcessHeartbeat(ctx context.Context, req models.HeartbeatRequest) error {
	// Reject heartbeats from revoked nodes immediately
	if h.redis.IsNodeRevoked(ctx, req.NodeID) {
		slog.Warn("heartbeat rejected: node is revoked", "node_id", req.NodeID)
		return fmt.Errorf("node %s has been revoked", req.NodeID)
	}

	bh := batchHeartbeat{
		ctx: ctx,
		req: req,
		err: make(chan error, 1),
	}
	h.batchCh <- bh
	return <-bh.err
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
