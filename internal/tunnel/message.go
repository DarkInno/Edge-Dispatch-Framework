package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// MessageType defines the type of tunnel message.
type MessageType uint8

const (
	// Control messages
	MsgTypeRegister   MessageType = 0x01 // NAT node registers with tunnel server
	MsgTypeRegisterOK MessageType = 0x02 // Registration successful
	MsgTypeHeartbeat  MessageType = 0x03 // Keep-alive heartbeat
	MsgTypeError      MessageType = 0x04 // Error response

	// Data messages
	MsgTypeRequest  MessageType = 0x10 // Forward HTTP request to NAT node
	MsgTypeResponse MessageType = 0x11 // HTTP response from NAT node
	MsgTypeData     MessageType = 0x12 // Stream data chunk
	MsgTypeDataEnd  MessageType = 0x13 // Stream end marker
)

// MessageHeader is the fixed-size header for all tunnel messages.
type MessageHeader struct {
	Version  uint8       // Protocol version (1)
	Type     MessageType // Message type
	StreamID uint32      // Stream identifier for multiplexing
	Length   uint32      // Payload length (0 for no payload)
}

const HeaderSize = 10 // version(1) + type(1) + streamID(4) + length(4)

// Message is a complete tunnel message with optional payload.
type Message struct {
	Header  MessageHeader
	Payload []byte
}

// RegisterPayload is sent by NAT edge nodes to register with the tunnel server.
type RegisterPayload struct {
	NodeID      string `json:"node_id"`
	NodeToken   string `json:"node_token"`
	ServiceAddr string `json:"service_addr"` // Local HTTP service address (e.g., "127.0.0.1:9090")
}

// RegisterOKPayload is returned on successful registration.
type RegisterOKPayload struct {
	TunnelID    string `json:"tunnel_id"`
	ExternalAddr string `json:"external_addr"` // Public address for this tunnel
}

// ErrorPayload contains error details.
type ErrorPayload struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// HTTPRequestHeader forwards an HTTP request to a NAT node.
type HTTPRequestHeader struct {
	StreamID uint32            `json:"stream_id"`
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Headers  map[string]string `json:"headers"`
	BodyLen  int64             `json:"body_len"`
}

// HTTPResponseHeader returns the response from a NAT node.
type HTTPResponseHeader struct {
	StreamID   uint32            `json:"stream_id"`
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	BodyLen    int64             `json:"body_len"`
}

// WriteMessage writes a complete message to the writer.
func WriteMessage(w io.Writer, msg *Message) error {
	// Write header
	buf := make([]byte, HeaderSize)
	buf[0] = msg.Header.Version
	buf[1] = uint8(msg.Header.Type)
	binary.BigEndian.PutUint32(buf[2:6], msg.Header.StreamID)
	binary.BigEndian.PutUint32(buf[6:10], msg.Header.Length)

	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	// Write payload if present
	if msg.Header.Length > 0 && len(msg.Payload) > 0 {
		if _, err := w.Write(msg.Payload[:msg.Header.Length]); err != nil {
			return fmt.Errorf("write payload: %w", err)
		}
	}

	return nil
}

// ReadMessage reads a complete message from the reader.
func ReadMessage(r io.Reader) (*Message, error) {
	// Read header
	buf := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}

	msg := &Message{
		Header: MessageHeader{
			Version:  buf[0],
			Type:     MessageType(buf[1]),
			StreamID: binary.BigEndian.Uint32(buf[2:6]),
			Length:   binary.BigEndian.Uint32(buf[6:10]),
		},
	}

	// Read payload if present
	if msg.Header.Length > 0 {
		msg.Payload = make([]byte, msg.Header.Length)
		if _, err := io.ReadFull(r, msg.Payload); err != nil {
			return nil, fmt.Errorf("read payload: %w", err)
		}
	}

	return msg, nil
}

// MarshalPayload marshals a payload to JSON bytes.
func MarshalPayload(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// UnmarshalPayload unmarshals JSON bytes to a target struct.
func UnmarshalPayload(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

// NewControlMessage creates a control message with JSON payload.
func NewControlMessage(msgType MessageType, streamID uint32, payload interface{}) (*Message, error) {
	data, err := MarshalPayload(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	return &Message{
		Header: MessageHeader{
			Version:  1,
			Type:     msgType,
			StreamID: streamID,
			Length:   uint32(len(data)),
		},
		Payload: data,
	}, nil
}

// NewDataMessage creates a data message with raw bytes.
func NewDataMessage(msgType MessageType, streamID uint32, data []byte) *Message {
	return &Message{
		Header: MessageHeader{
			Version:  1,
			Type:     msgType,
			StreamID: streamID,
			Length:   uint32(len(data)),
		},
		Payload: data,
	}
}
