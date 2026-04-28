package tunnel

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// ServerConfig holds tunnel server configuration.
type ServerConfig struct {
	ListenAddr    string        // Address to listen for tunnel connections (e.g., ":7700")
	AuthToken     string        // Token to validate NAT node registration
	IdleTimeout   time.Duration // Close idle tunnels after this duration
	MaxTunnels    int           // Maximum concurrent tunnels
	TLSCertFile   string        // TLS certificate file
	TLSKeyFile    string        // TLS key file
}

// DefaultServerConfig returns default server configuration.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		ListenAddr:  ":7700",
		IdleTimeout: 5 * time.Minute,
		MaxTunnels:  100,
	}
}

// Server accepts tunnel connections from NAT edge nodes.
type Server struct {
	cfg      ServerConfig
	manager  *TunnelManager
	listener net.Listener
	logger   *slog.Logger
	nextID    atomic.Uint64
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewServer creates a new tunnel server.
func NewServer(cfg ServerConfig, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		cfg:     cfg,
		manager: NewTunnelManager(logger),
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Start begins accepting tunnel connections.
func (s *Server) Start() error {
	var ln net.Listener
	var err error
	if s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != "" {
		cert, loadErr := tls.LoadX509KeyPair(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		if loadErr != nil {
			return fmt.Errorf("load tls cert: %w", loadErr)
		}
		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		ln, err = tls.Listen("tcp", s.cfg.ListenAddr, tlsCfg)
	} else {
		ln, err = net.Listen("tcp", s.cfg.ListenAddr)
	}
	if err != nil {
		return fmt.Errorf("listen tunnel: %w", err)
	}
	s.listener = ln
	s.logger.Info("tunnel server started", "addr", s.cfg.ListenAddr)

	go s.acceptLoop()
	return nil
}

// Stop gracefully shuts down the tunnel server.
func (s *Server) Stop() {
	s.cancel()
	if s.listener != nil {
		s.listener.Close()
	}
	s.manager.Close()
	s.logger.Info("tunnel server stopped")
}

// Manager returns the tunnel manager.
func (s *Server) Manager() *TunnelManager {
	return s.manager
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				return
			default:
				s.logger.Error("accept error", "err", err)
				continue
			}
		}
		go s.handleConnection(conn)
	}
}

func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()

	// Set initial read timeout for registration
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	msg, err := ReadMessage(conn)
	if err != nil {
		s.logger.Error("read register message", "err", err)
		return
	}

	if msg.Header.Type != MsgTypeRegister {
		s.sendError(conn, 0, "expected register message")
		return
	}

	var reg RegisterPayload
	if err := UnmarshalPayload(msg.Payload, &reg); err != nil {
		s.sendError(conn, 0, "invalid register payload")
		return
	}

	// Validate registration
	if s.cfg.AuthToken != "" && reg.NodeToken != s.cfg.AuthToken {
		s.sendError(conn, 0, "invalid token")
		return
	}

	if s.manager.ActiveTunnels() >= s.cfg.MaxTunnels {
		s.sendError(conn, 0, "max tunnels reached")
		return
	}

	// Create tunnel session
	tunnelID := uuid.New().String()
	session := NewTunnelSession(tunnelID, reg.NodeID, reg.ServiceAddr, &connAdapter{conn: conn})
	session.ExternalAddr = fmt.Sprintf("tunnel://%s", conn.RemoteAddr().String())

	if err := s.manager.Register(session); err != nil {
		s.sendError(conn, 0, err.Error())
		return
	}
	defer s.manager.Unregister(reg.NodeID)

	// Send registration OK
	okPayload := RegisterOKPayload{
		TunnelID:     tunnelID,
		ExternalAddr: session.ExternalAddr,
	}
	resp, _ := NewControlMessage(MsgTypeRegisterOK, 0, okPayload)
	if err := WriteMessage(conn, resp); err != nil {
		s.logger.Error("send register ok", "err", err)
		return
	}

	// Clear read deadline
	conn.SetReadDeadline(time.Time{})

	s.logger.Info("tunnel established",
		"node_id", reg.NodeID,
		"tunnel_id", tunnelID,
		"remote", conn.RemoteAddr(),
	)

	// Main message loop
	s.messageLoop(session, conn)
}

func (s *Server) messageLoop(session *TunnelSession, conn net.Conn) {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		msg, err := ReadMessage(conn)
		if err != nil {
			if err != io.EOF {
				s.logger.Error("read message", "err", err, "node_id", session.NodeID)
			}
			return
		}

		switch msg.Header.Type {
		case MsgTypeHeartbeat:
			// Respond with heartbeat
			resp, _ := NewControlMessage(MsgTypeHeartbeat, 0, nil)
			if err := session.Conn.WriteMessage(resp); err != nil {
				s.logger.Error("send heartbeat", "err", err)
				return
			}

		case MsgTypeResponse:
			// Forward response to waiting stream
			if stream, ok := session.GetStream(msg.Header.StreamID); ok {
				stream.Response <- msg
			}

		case MsgTypeData:
			// Forward data to waiting stream
			if stream, ok := session.GetStream(msg.Header.StreamID); ok {
				stream.Response <- msg
			}

		case MsgTypeDataEnd:
			// Stream complete
			session.RemoveStream(msg.Header.StreamID)

		default:
			s.logger.Warn("unknown message type", "type", msg.Header.Type, "node_id", session.NodeID)
		}
	}
}

func (s *Server) sendError(conn net.Conn, streamID uint32, message string) {
	errPayload := ErrorPayload{Code: 1, Message: message}
	msg, _ := NewControlMessage(MsgTypeError, streamID, errPayload)
	WriteMessage(conn, msg)
}

// ForwardRequest sends an HTTP request through the tunnel to the NAT node.
func (s *Server) ForwardRequest(nodeID string, req *HTTPRequestHeader, body io.Reader) (*HTTPResponseHeader, io.Reader, error) {
	session, ok := s.manager.Get(nodeID)
	if !ok {
		return nil, nil, fmt.Errorf("no tunnel for node %s", nodeID)
	}

	// Create a new stream
	streamID := uint32(s.nextID.Add(1))
	stream := session.CreateStream(streamID)
	defer session.RemoveStream(streamID)

	// Send request header
	reqMsg, err := NewControlMessage(MsgTypeRequest, streamID, req)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	if err := session.Conn.WriteMessage(reqMsg); err != nil {
		return nil, nil, fmt.Errorf("send request: %w", err)
	}

	// Send body if present
	if body != nil {
		buf := make([]byte, 32*1024)
		for {
			n, readErr := body.Read(buf)
			if n > 0 {
				dataMsg := NewDataMessage(MsgTypeData, streamID, buf[:n])
				if err := session.Conn.WriteMessage(dataMsg); err != nil {
					return nil, nil, fmt.Errorf("send body: %w", err)
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					break
				}
				return nil, nil, fmt.Errorf("read body: %w", readErr)
			}
		}
		// Send end marker
		endMsg := NewDataMessage(MsgTypeDataEnd, streamID, nil)
		if err := session.Conn.WriteMessage(endMsg); err != nil {
			return nil, nil, fmt.Errorf("send body end: %w", err)
		}
	}

	// Wait for response
	select {
	case respMsg := <-stream.Response:
		if respMsg.Header.Type == MsgTypeError {
			var errPayload ErrorPayload
			UnmarshalPayload(respMsg.Payload, &errPayload)
			return nil, nil, fmt.Errorf("remote error: %s", errPayload.Message)
		}
		var respHeader HTTPResponseHeader
		if err := UnmarshalPayload(respMsg.Payload, &respHeader); err != nil {
			return nil, nil, fmt.Errorf("unmarshal response: %w", err)
		}
		// Create reader for response body
		bodyReader := &tunnelBodyReader{
			stream:  stream,
			remain:  respHeader.BodyLen,
		}
		return &respHeader, bodyReader, nil

	case <-time.After(30 * time.Second):
		return nil, nil, fmt.Errorf("tunnel request timeout")
	}
}

// tunnelBodyReader reads response body from tunnel stream.
type tunnelBodyReader struct {
	stream  *Stream
	remain  int64
	buf     []byte
}

func (r *tunnelBodyReader) Read(p []byte) (int, error) {
	if r.remain <= 0 && len(r.buf) == 0 {
		return 0, io.EOF
	}

	// Return buffered data first
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}

	// Read next data message
	select {
	case msg := <-r.stream.Response:
		switch msg.Header.Type {
		case MsgTypeData:
			r.remain -= int64(len(msg.Payload))
			n := copy(p, msg.Payload)
			if n < len(msg.Payload) {
				r.buf = msg.Payload[n:]
			}
			return n, nil
		case MsgTypeDataEnd:
			return 0, io.EOF
		default:
			return 0, fmt.Errorf("unexpected message type: %d", msg.Header.Type)
		}
	case <-r.stream.Done:
		return 0, io.EOF
	case <-time.After(30 * time.Second):
		return 0, fmt.Errorf("tunnel body read timeout")
	}
}

// connAdapter wraps net.Conn to implement TunnelConn.
type connAdapter struct {
	conn net.Conn
	mu   sync.Mutex
}

func (a *connAdapter) WriteMessage(msg *Message) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return WriteMessage(a.conn, msg)
}

func (a *connAdapter) ReadMessage() (*Message, error) {
	return ReadMessage(a.conn)
}

func (a *connAdapter) Close() error {
	return a.conn.Close()
}

// HTTPHandler returns an http.Handler that proxies requests through tunnels.
func (s *Server) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract node ID from request (e.g., from query param or header)
		nodeID := r.Header.Get("X-Tunnel-Node-ID")
		if nodeID == "" {
			nodeID = r.URL.Query().Get("node_id")
		}
		if nodeID == "" {
			http.Error(w, "missing node_id", http.StatusBadRequest)
			return
		}

		// Build request header
		headers := make(map[string]string)
		for k, v := range r.Header {
			if len(v) > 0 {
				headers[k] = v[0]
			}
		}

		reqHeader := &HTTPRequestHeader{
			Method:  r.Method,
			Path:    r.URL.String(),
			Headers: headers,
			BodyLen: r.ContentLength,
		}

		// Forward through tunnel
		respHeader, bodyReader, err := s.ForwardRequest(nodeID, reqHeader, r.Body)
		if err != nil {
			s.logger.Error("tunnel forward failed", "err", err, "node_id", nodeID)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		// Write response
		for k, v := range respHeader.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(respHeader.StatusCode)
		if bodyReader != nil {
			io.Copy(w, bodyReader)
		}
	})
}
