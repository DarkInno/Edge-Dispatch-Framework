package streaming

import (
	"sync"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

// StreamingStrategy extends content-aware scheduling with streaming-specific
// logic. It tracks which cache nodes hold recent chunks for each stream,
// enabling the control plane to prefer nodes that already have the stream
// in their sliding window.
type StreamingStrategy struct {
	mu           sync.RWMutex
	nodeStreams  map[string]map[models.StreamType][]string
	cfg          *config.StreamingConfig
}

func NewStreamingStrategy(cfg *config.StreamingConfig) *StreamingStrategy {
	return &StreamingStrategy{
		nodeStreams: make(map[string]map[models.StreamType][]string),
		cfg:         cfg,
	}
}

// ReportStream notifies the strategy that a node has a stream in cache.
func (ss *StreamingStrategy) ReportStream(nodeID string, streamKey string, st models.StreamType) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if _, ok := ss.nodeStreams[nodeID]; !ok {
		ss.nodeStreams[nodeID] = make(map[models.StreamType][]string)
	}

	streams := ss.nodeStreams[nodeID][st]
	seen := false
	for _, s := range streams {
		if s == streamKey {
			seen = true
			break
		}
	}
	if !seen {
		ss.nodeStreams[nodeID][st] = append(streams, streamKey)
	}
}

// RemoveStream removes a stream from a node's report (e.g., window eviction).
func (ss *StreamingStrategy) RemoveStream(nodeID string, streamKey string, st models.StreamType) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	nodeStreams, ok := ss.nodeStreams[nodeID]
	if !ok {
		return
	}
	streams, ok := nodeStreams[st]
	if !ok {
		return
	}
	for i, s := range streams {
		if s == streamKey {
			nodeStreams[st] = append(streams[:i], streams[i+1:]...)
			break
		}
	}
}

// StreamScore returns a bonus score for a node serving a given stream.
// Higher score means the node is more likely to have the stream in cache.
func (ss *StreamingStrategy) StreamScore(nodeID string, key string) float64 {
	if ss.cfg == nil || !ss.cfg.Enabled {
		return 0
	}
	streamKey, _, ok := InferStreamFromChunkKey(key)
	if !ok {
		return 0
	}

	ss.mu.RLock()
	defer ss.mu.RUnlock()

	nodeStreams, ok := ss.nodeStreams[nodeID]
	if !ok {
		return 0
	}

	for _, st := range []models.StreamType{models.StreamTypeHLS, models.StreamTypeDASH} {
		for _, s := range nodeStreams[st] {
			if s == streamKey {
				return 20.0
			}
		}
	}
	return 0
}

// HasStream checks if a node is known to have a stream.
func (ss *StreamingStrategy) HasStream(nodeID string, streamKey string) bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	nodeStreams, ok := ss.nodeStreams[nodeID]
	if !ok {
		return false
	}
	for _, streams := range nodeStreams {
		for _, s := range streams {
			if s == streamKey {
				return true
			}
		}
	}
	return false
}

// FindNodesWithStream returns a set of node IDs that have the given stream
// in their cache. Called once per dispatch instead of N times.
func (ss *StreamingStrategy) FindNodesWithStream(streamKey string) map[string]bool {
	if ss.cfg == nil || !ss.cfg.Enabled {
		return nil
	}

	ss.mu.RLock()
	defer ss.mu.RUnlock()

	result := make(map[string]bool)
	for nodeID, nodeStreams := range ss.nodeStreams {
		for _, streams := range nodeStreams {
			for _, s := range streams {
				if s == streamKey {
					result[nodeID] = true
					break
				}
			}
		}
	}
	return result
}
