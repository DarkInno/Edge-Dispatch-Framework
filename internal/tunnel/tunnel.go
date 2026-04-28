package tunnel

import (
	"fmt"
	"log/slog"
	"sync"
)

// TunnelManager manages tunnel connections between gateway and NAT nodes.
type TunnelManager struct {
	mu       sync.RWMutex
	tunnels  map[string]*TunnelSession // nodeID -> session
	logger   *slog.Logger
}

// TunnelSession represents an active tunnel connection with a NAT node.
type TunnelSession struct {
	TunnelID     string
	NodeID       string
	ServiceAddr  string // Local HTTP address on the NAT node
	ExternalAddr string // Public tunnel address
	Conn         TunnelConn
	Streams      map[uint32]*Stream
	mu           sync.RWMutex
	closed       bool
}

// Stream represents a multiplexed stream within a tunnel.
type Stream struct {
	ID       uint32
	Request  chan *Message
	Response chan *Message
	Done     chan struct{}
}

// TunnelConn is the interface for tunnel connections.
type TunnelConn interface {
	WriteMessage(msg *Message) error
	ReadMessage() (*Message, error)
	Close() error
}

// NewTunnelManager creates a new tunnel manager.
func NewTunnelManager(logger *slog.Logger) *TunnelManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &TunnelManager{
		tunnels: make(map[string]*TunnelSession),
		logger:  logger,
	}
}

// Register adds a new tunnel session for a NAT node.
func (m *TunnelManager) Register(session *TunnelSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.tunnels[session.NodeID]; exists {
		return fmt.Errorf("tunnel already exists for node %s", session.NodeID)
	}

	m.tunnels[session.NodeID] = session
	m.logger.Info("tunnel registered",
		"node_id", session.NodeID,
		"tunnel_id", session.TunnelID,
		"external_addr", session.ExternalAddr,
	)
	return nil
}

// Unregister removes a tunnel session.
func (m *TunnelManager) Unregister(nodeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if session, ok := m.tunnels[nodeID]; ok {
		session.Close()
		delete(m.tunnels, nodeID)
		m.logger.Info("tunnel unregistered", "node_id", nodeID)
	}
}

// Get returns the tunnel session for a node.
func (m *TunnelManager) Get(nodeID string) (*TunnelSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.tunnels[nodeID]
	return session, ok
}

// IsConnected checks if a node has an active tunnel.
func (m *TunnelManager) IsConnected(nodeID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.tunnels[nodeID]
	return ok && !session.closed
}

// ActiveTunnels returns the count of active tunnels.
func (m *TunnelManager) ActiveTunnels() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.tunnels)
}

// Close shuts down all tunnel sessions.
func (m *TunnelManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for nodeID, session := range m.tunnels {
		session.Close()
		m.logger.Info("tunnel closed", "node_id", nodeID)
	}
	m.tunnels = make(map[string]*TunnelSession)
}

// NewTunnelSession creates a new tunnel session.
func NewTunnelSession(tunnelID, nodeID, serviceAddr string, conn TunnelConn) *TunnelSession {
	return &TunnelSession{
		TunnelID:    tunnelID,
		NodeID:      nodeID,
		ServiceAddr: serviceAddr,
		Conn:        conn,
		Streams:     make(map[uint32]*Stream),
	}
}

// CreateStream creates a new multiplexed stream.
func (s *TunnelSession) CreateStream(streamID uint32) *Stream {
	s.mu.Lock()
	defer s.mu.Unlock()

	stream := &Stream{
		ID:       streamID,
		Request:  make(chan *Message, 1),
		Response: make(chan *Message, 1),
		Done:     make(chan struct{}),
	}
	s.Streams[streamID] = stream
	return stream
}

// GetStream returns a stream by ID.
func (s *TunnelSession) GetStream(streamID uint32) (*Stream, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stream, ok := s.Streams[streamID]
	return stream, ok
}

// RemoveStream removes a stream.
func (s *TunnelSession) RemoveStream(streamID uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stream, ok := s.Streams[streamID]; ok {
		close(stream.Done)
		delete(s.Streams, streamID)
	}
}

// Close shuts down the tunnel session and all streams.
func (s *TunnelSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true

	for _, stream := range s.Streams {
		close(stream.Done)
	}
	s.Streams = make(map[uint32]*Stream)

	if s.Conn != nil {
		s.Conn.Close()
	}
}
