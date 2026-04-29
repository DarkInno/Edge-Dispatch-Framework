package controlplane

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/darkinno/edge-dispatch-framework/internal/models"
	"github.com/darkinno/edge-dispatch-framework/internal/store"
)

type NodeCache struct {
	pg       *store.PGStore
	mu       sync.Mutex // only protects the refresh path
	nodes    atomic.Value // stores []*models.Node
	cachedAt atomic.Int64 // stores time.Now().UnixNano()
	ttl      int64
	sf       singleflight.Group
}

func NewNodeCache(pg *store.PGStore, ttl time.Duration) *NodeCache {
	c := &NodeCache{pg: pg, ttl: int64(ttl)}
	c.nodes.Store([]*models.Node(nil))
	return c
}

func (c *NodeCache) GetActiveNodes(ctx context.Context) ([]*models.Node, error) {
	if nodes, ok := c.nodes.Load().([]*models.Node); ok && nodes != nil {
		if time.Since(time.Unix(0, c.cachedAt.Load())) < time.Duration(c.ttl) {
			return nodes, nil
		}
	}

	v, err, _ := c.sf.Do("active-nodes", func() (any, error) {
		// Double-check cache after winning singleflight
		if nodes, ok := c.nodes.Load().([]*models.Node); ok && nodes != nil {
			if time.Since(time.Unix(0, c.cachedAt.Load())) < time.Duration(c.ttl) {
				return nodes, nil
			}
		}

		nodes, err := c.pg.ListActiveNodes(ctx)
		if err != nil {
			return nil, err
		}
		c.nodes.Store(nodes)
		c.cachedAt.Store(time.Now().UnixNano())
		return nodes, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]*models.Node), nil
}

func (c *NodeCache) Invalidate() {
	c.nodes.Store([]*models.Node(nil))
	c.cachedAt.Store(0)
}
