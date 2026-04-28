package controlplane

import (
	"context"
	"sync"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/models"
	"github.com/darkinno/edge-dispatch-framework/internal/store"
)

type NodeCache struct {
	pg       *store.PGStore
	mu       sync.RWMutex
	nodes    []*models.Node
	cachedAt time.Time
	ttl      time.Duration
}

func NewNodeCache(pg *store.PGStore, ttl time.Duration) *NodeCache {
	return &NodeCache{pg: pg, ttl: ttl}
}

func (c *NodeCache) GetActiveNodes(ctx context.Context) ([]*models.Node, error) {
	c.mu.RLock()
	if time.Since(c.cachedAt) < c.ttl {
		nodes := c.nodes
		c.mu.RUnlock()
		return nodes, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if time.Since(c.cachedAt) < c.ttl {
		return c.nodes, nil
	}

	nodes, err := c.pg.ListActiveNodes(ctx)
	if err != nil {
		return nil, err
	}
	c.nodes = nodes
	c.cachedAt = time.Now()
	return nodes, nil
}

func (c *NodeCache) Invalidate() {
	c.mu.Lock()
	c.cachedAt = time.Time{}
	c.mu.Unlock()
}
