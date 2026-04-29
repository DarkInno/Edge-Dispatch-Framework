package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// ClientConfig holds tunnel client configuration.
type ClientConfig struct {
	ServerAddr    string        // Tunnel server address (e.g., "gateway:7700")
	NodeID        string        // This node's ID
	NodeToken     string        // Authentication token
	ServiceAddr   string        // Local HTTP service address (e.g., "127.0.0.1:9090")
	KeepAlive     time.Duration // Keepalive interval
	ReconnectWait time.Duration // Wait before reconnecting
	UseTLS        bool          // Use TLS for tunnel connection
	TLSSkipVerify bool          // Skip TLS certificate verification (development only)
}

// DefaultClientConfig returns default client configuration.
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		KeepAlive:     30 * time.Second,
		ReconnectWait: 5 * time.Second,
	}
}

// Client runs on NAT edge nodes to establish reverse tunnels.
type Client struct {
	cfg      ClientConfig
	logger   *slog.Logger
	conn     net.Conn
	tunnelID string
	mu       sync.RWMutex
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewClient creates a new tunnel client.
func NewClient(cfg ClientConfig, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		cfg:    cfg,
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Connect establishes a tunnel connection to the server.
func (c *Client) Connect() error {
	var conn net.Conn
	var err error

	if c.cfg.UseTLS {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
		if c.cfg.TLSSkipVerify {
			tlsCfg.InsecureSkipVerify = true
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		conn, err = tls.DialWithDialer(dialer, "tcp", c.cfg.ServerAddr, tlsCfg)
	} else {
		conn, err = net.DialTimeout("tcp", c.cfg.ServerAddr, 10*time.Second)
	}
	if err != nil {
		return fmt.Errorf("dial tunnel server: %w", err)
	}
	c.conn = conn

	// Send registration
	reg := RegisterPayload{
		NodeID:      c.cfg.NodeID,
		NodeToken:   c.cfg.NodeToken,
		ServiceAddr: c.cfg.ServiceAddr,
	}
	msg, err := NewControlMessage(MsgTypeRegister, 0, reg)
	if err != nil {
		conn.Close()
		return fmt.Errorf("create register message: %w", err)
	}
	if err := WriteMessage(conn, msg); err != nil {
		conn.Close()
		return fmt.Errorf("send register: %w", err)
	}

	// Read registration response
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := ReadMessage(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("read register response: %w", err)
	}
	conn.SetReadDeadline(time.Time{})

	if resp.Header.Type == MsgTypeError {
		var errPayload ErrorPayload
		UnmarshalPayload(resp.Payload, &errPayload)
		conn.Close()
		return fmt.Errorf("register rejected: %s", errPayload.Message)
	}

	if resp.Header.Type != MsgTypeRegisterOK {
		conn.Close()
		return fmt.Errorf("unexpected response type: %d", resp.Header.Type)
	}

	var okPayload RegisterOKPayload
	if err := UnmarshalPayload(resp.Payload, &okPayload); err != nil {
		conn.Close()
		return fmt.Errorf("unmarshal register ok: %w", err)
	}

	c.tunnelID = okPayload.TunnelID
	c.logger.Info("tunnel connected",
		"tunnel_id", c.tunnelID,
		"server", c.cfg.ServerAddr,
		"external_addr", okPayload.ExternalAddr,
	)

	return nil
}

// Run starts the tunnel client loop with automatic reconnection.
func (c *Client) Run() error {
	for {
		select {
		case <-c.ctx.Done():
			return nil
		default:
		}

		if err := c.Connect(); err != nil {
			c.logger.Error("tunnel connect failed", "err", err)
			select {
			case <-c.ctx.Done():
				return nil
			case <-time.After(c.cfg.ReconnectWait):
				continue
			}
		}

		// Run message loop
		c.messageLoop()

		// Connection lost, reconnect
		c.logger.Info("tunnel disconnected, reconnecting...")
		select {
		case <-c.ctx.Done():
			return nil
		case <-time.After(c.cfg.ReconnectWait):
		}
	}
}

// Stop stops the tunnel client.
func (c *Client) Stop() {
	c.cancel()
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Unlock()
}

// TunnelID returns the tunnel ID if connected.
func (c *Client) TunnelID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tunnelID
}

func (c *Client) messageLoop() {
	// Start heartbeat goroutine
	heartbeatDone := make(chan struct{})
	go c.heartbeatLoop(heartbeatDone)
	defer close(heartbeatDone)

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()

		if conn == nil {
			return
		}

		msg, err := ReadMessage(conn)
		if err != nil {
			if err != io.EOF {
				c.logger.Error("read message", "err", err)
			}
			return
		}

		switch msg.Header.Type {
		case MsgTypeHeartbeat:
			// Server acknowledged heartbeat
			continue

		case MsgTypeRequest:
			// Handle forwarded HTTP request
			go c.handleRequest(msg)

		default:
			c.logger.Warn("unknown message type", "type", msg.Header.Type)
		}
	}
}

func (c *Client) heartbeatLoop(done chan struct{}) {
	ticker := time.NewTicker(c.cfg.KeepAlive)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()

			if conn == nil {
				return
			}

			msg, _ := NewControlMessage(MsgTypeHeartbeat, 0, nil)
			if err := WriteMessage(conn, msg); err != nil {
				c.logger.Error("send heartbeat", "err", err)
				return
			}
		}
	}
}

func (c *Client) handleRequest(msg *Message) {
	var reqHeader HTTPRequestHeader
	if err := UnmarshalPayload(msg.Payload, &reqHeader); err != nil {
		c.logger.Error("unmarshal request header", "err", err)
		return
	}

	streamID := msg.Header.StreamID

	// Build HTTP request to local service
	url := fmt.Sprintf("http://%s%s", c.cfg.ServiceAddr, reqHeader.Path)
	req, err := http.NewRequest(reqHeader.Method, url, nil)
	if err != nil {
		c.sendError(streamID, fmt.Sprintf("create request: %v", err))
		return
	}

	for k, v := range reqHeader.Headers {
		req.Header.Set(k, v)
	}

	// Read body if present
	if reqHeader.BodyLen > 0 {
		body, err := c.readRequestBody(streamID, reqHeader.BodyLen)
		if err != nil {
			c.sendError(streamID, fmt.Sprintf("read body: %v", err))
			return
		}
		req.Body = io.NopCloser(body)
		req.ContentLength = reqHeader.BodyLen
	}

	// Execute request against local service
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		c.sendError(streamID, fmt.Sprintf("local request: %v", err))
		return
	}
	defer resp.Body.Close()

	// Send response header
	headers := make(map[string]string)
	for k, v := range resp.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	respHeader := HTTPResponseHeader{
		StreamID:   streamID,
		StatusCode: resp.StatusCode,
		Headers:    headers,
		BodyLen:    resp.ContentLength,
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return
	}

	respMsg, _ := NewControlMessage(MsgTypeResponse, streamID, respHeader)
	if err := WriteMessage(conn, respMsg); err != nil {
		c.logger.Error("send response header", "err", err)
		return
	}

	// Stream response body
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			dataMsg := NewDataMessage(MsgTypeData, streamID, buf[:n])
			if err := WriteMessage(conn, dataMsg); err != nil {
				c.logger.Error("send response data", "err", err)
				return
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			c.logger.Error("read response body", "err", readErr)
			return
		}
	}

	// Send end marker
	endMsg := NewDataMessage(MsgTypeDataEnd, streamID, nil)
	if err := WriteMessage(conn, endMsg); err != nil {
		c.logger.Error("send response end", "err", err)
	}
}

func (c *Client) readRequestBody(streamID uint32, length int64) (io.Reader, error) {
	var buf bytes.Buffer
	remaining := length

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	for remaining > 0 {
		msg, err := ReadMessage(conn)
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}
		if msg.Header.Type != MsgTypeData {
			return nil, fmt.Errorf("expected data message, got %d", msg.Header.Type)
		}
		n, _ := buf.Write(msg.Payload)
		remaining -= int64(n)
	}

	return &buf, nil
}

func (c *Client) sendError(streamID uint32, message string) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return
	}

	errPayload := ErrorPayload{Code: 1, Message: message}
	msg, _ := NewControlMessage(MsgTypeError, streamID, errPayload)
	WriteMessage(conn, msg)
}
