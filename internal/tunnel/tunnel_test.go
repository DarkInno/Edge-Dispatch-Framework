package tunnel

import (
	"bytes"
	"testing"
)

func TestMessageMarshalUnmarshal(t *testing.T) {
	// Test control message creation
	payload := RegisterPayload{
		NodeID:      "test-node-1",
		NodeToken:   "test-token",
		ServiceAddr: "127.0.0.1:9090",
	}

	msg, err := NewControlMessage(MsgTypeRegister, 1, payload)
	if err != nil {
		t.Fatalf("NewControlMessage failed: %v", err)
	}

	// Verify header
	if msg.Header.Version != 1 {
		t.Errorf("expected version 1, got %d", msg.Header.Version)
	}
	if msg.Header.Type != MsgTypeRegister {
		t.Errorf("expected type %d, got %d", MsgTypeRegister, msg.Header.Type)
	}
	if msg.Header.StreamID != 1 {
		t.Errorf("expected streamID 1, got %d", msg.Header.StreamID)
	}
	if msg.Header.Length == 0 {
		t.Error("expected non-zero payload length")
	}

	// Test unmarshal
	var decoded RegisterPayload
	if err := UnmarshalPayload(msg.Payload, &decoded); err != nil {
		t.Fatalf("UnmarshalPayload failed: %v", err)
	}
	if decoded.NodeID != payload.NodeID {
		t.Errorf("expected NodeID %s, got %s", payload.NodeID, decoded.NodeID)
	}
	if decoded.ServiceAddr != payload.ServiceAddr {
		t.Errorf("expected ServiceAddr %s, got %s", payload.ServiceAddr, decoded.ServiceAddr)
	}
}

func TestReadWriteMessage(t *testing.T) {
	// Create a buffer to simulate connection
	var buf bytes.Buffer

	// Write a message
	payload := ErrorPayload{Code: 42, Message: "test error"}
	msg, _ := NewControlMessage(MsgTypeError, 5, payload)

	if err := WriteMessage(&buf, msg); err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	// Read it back
	readMsg, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}

	// Verify
	if readMsg.Header.Type != MsgTypeError {
		t.Errorf("expected type %d, got %d", MsgTypeError, readMsg.Header.Type)
	}
	if readMsg.Header.StreamID != 5 {
		t.Errorf("expected streamID 5, got %d", readMsg.Header.StreamID)
	}

	var decoded ErrorPayload
	if err := UnmarshalPayload(readMsg.Payload, &decoded); err != nil {
		t.Fatalf("UnmarshalPayload failed: %v", err)
	}
	if decoded.Code != 42 {
		t.Errorf("expected code 42, got %d", decoded.Code)
	}
	if decoded.Message != "test error" {
		t.Errorf("expected message 'test error', got '%s'", decoded.Message)
	}
}

func TestDataMessage(t *testing.T) {
	data := []byte("hello world")
	msg := NewDataMessage(MsgTypeData, 10, data)

	if msg.Header.Type != MsgTypeData {
		t.Errorf("expected type %d, got %d", MsgTypeData, msg.Header.Type)
	}
	if msg.Header.StreamID != 10 {
		t.Errorf("expected streamID 10, got %d", msg.Header.StreamID)
	}
	if msg.Header.Length != uint32(len(data)) {
		t.Errorf("expected length %d, got %d", len(data), msg.Header.Length)
	}
	if !bytes.Equal(msg.Payload, data) {
		t.Errorf("payload mismatch")
	}
}

func TestTunnelManager(t *testing.T) {
	manager := NewTunnelManager(nil)

	// Test register
	session := &TunnelSession{
		TunnelID:    "tunnel-1",
		NodeID:      "node-1",
		ServiceAddr: "127.0.0.1:9090",
		Streams:     make(map[uint32]*Stream),
	}

	if err := manager.Register(session); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// Test duplicate register
	if err := manager.Register(session); err == nil {
		t.Error("expected error for duplicate register")
	}

	// Test get
	got, ok := manager.Get("node-1")
	if !ok || got.TunnelID != "tunnel-1" {
		t.Error("Get returned wrong session")
	}

	// Test IsConnected
	if !manager.IsConnected("node-1") {
		t.Error("expected node-1 to be connected")
	}
	if manager.IsConnected("node-2") {
		t.Error("expected node-2 to not be connected")
	}

	// Test ActiveTunnels
	if manager.ActiveTunnels() != 1 {
		t.Errorf("expected 1 active tunnel, got %d", manager.ActiveTunnels())
	}

	// Test unregister
	manager.Unregister("node-1")
	if manager.IsConnected("node-1") {
		t.Error("expected node-1 to be disconnected after unregister")
	}
	if manager.ActiveTunnels() != 0 {
		t.Errorf("expected 0 active tunnels, got %d", manager.ActiveTunnels())
	}
}

func TestTunnelSession(t *testing.T) {
	session := &TunnelSession{
		TunnelID: "tunnel-1",
		NodeID:   "node-1",
		Streams:  make(map[uint32]*Stream),
	}

	// Test CreateStream
	stream := session.CreateStream(1)
	if stream.ID != 1 {
		t.Errorf("expected stream ID 1, got %d", stream.ID)
	}

	// Test GetStream
	got, ok := session.GetStream(1)
	if !ok || got.ID != 1 {
		t.Error("GetStream returned wrong stream")
	}

	// Test RemoveStream
	session.RemoveStream(1)
	_, ok = session.GetStream(1)
	if ok {
		t.Error("expected stream to be removed")
	}

	// Test Close
	session.CreateStream(2)
	session.CreateStream(3)
	session.Close()
	if len(session.Streams) != 0 {
		t.Errorf("expected 0 streams after close, got %d", len(session.Streams))
	}
}
