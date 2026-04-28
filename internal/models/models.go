package models

import "time"

// NodeStatus represents the lifecycle state of an edge node.
type NodeStatus string

const (
	NodeStatusRegistered  NodeStatus = "REGISTERED"
	NodeStatusActive      NodeStatus = "ACTIVE"
	NodeStatusDegraded    NodeStatus = "DEGRADED"
	NodeStatusQuarantined NodeStatus = "QUARANTINED"
	NodeStatusOffline     NodeStatus = "OFFLINE"
)

// Node represents an edge node registered with the control plane.
type Node struct {
	NodeID       string      `json:"node_id"`
	TenantID     string      `json:"tenant_id,omitempty"`
	Name         string      `json:"name"`
	Endpoints    []Endpoint  `json:"endpoints"`
	Region       string      `json:"region"`
	ISP          string      `json:"isp"`
	ASN          string      `json:"asn,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
	Status       NodeStatus  `json:"status"`
	LastSeenAt   time.Time   `json:"last_seen_at"`
	Scores       NodeScores  `json:"scores,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

// Endpoint represents a service endpoint of an edge node.
type Endpoint struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
}

// Capabilities describes the node's hardware and network capabilities.
type Capabilities struct {
	InboundReachable bool  `json:"inbound_reachable"`
	CacheDiskGB      int64 `json:"cache_disk_gb"`
	MaxUplinkMbps    int64 `json:"max_uplink_mbps"`
	SupportsHTTPS    bool  `json:"supports_https"`
}

// NodeScores holds computed quality scores.
type NodeScores struct {
	ReachableScore float64 `json:"reachable_score"`
	HealthScore    float64 `json:"health_score"`
	RiskScore      float64 `json:"risk_score"`
}

// NodeRuntime contains the current runtime metrics reported via heartbeat.
type NodeRuntime struct {
	CPU       float64 `json:"cpu"`
	MemMB     int64   `json:"mem_mb"`
	DiskFreeGB int64  `json:"disk_free_gb"`
	Conn      int64   `json:"conn"`
}

// NodeTraffic contains traffic metrics.
type NodeTraffic struct {
	EgressMbps  float64 `json:"egress_mbps"`
	IngressMbps float64 `json:"ingress_mbps"`
	Err5xxRate  float64 `json:"err_5xx_rate"`
}

// NodeCache contains cache performance metrics.
type NodeCache struct {
	HitRatio float64 `json:"hit_ratio"`
}

// HeartbeatRequest is the payload sent by edge agents.
type HeartbeatRequest struct {
	NodeID  string      `json:"node_id"`
	TS      int64       `json:"ts"`
	Runtime NodeRuntime `json:"runtime"`
	Traffic NodeTraffic `json:"traffic"`
	Cache   NodeCache   `json:"cache"`
}

// RegisterRequest is the payload for node registration.
type RegisterRequest struct {
	NodeName   string       `json:"node_name"`
	Endpoints  []Endpoint   `json:"public_endpoints"`
	Region     string       `json:"region"`
	ISP        string       `json:"isp"`
	Capabilities Capabilities `json:"capabilities"`
}

// RegisterResponse is returned after successful registration.
type RegisterResponse struct {
	NodeID string     `json:"node_id"`
	Auth   NodeAuth   `json:"auth"`
}

// NodeAuth contains authentication credentials for a node.
type NodeAuth struct {
	Type  string `json:"type"`
	Token string `json:"token"`
	Exp   int64  `json:"exp"`
}

// DispatchRequest is the payload for the dispatch resolve API.
type DispatchRequest struct {
	Client   ClientInfo   `json:"client"`
	Resource ResourceInfo `json:"resource"`
	Hints    ClientHints  `json:"hints"`
}

// ClientInfo contains client metadata inferred from the request.
type ClientInfo struct {
	IP     string `json:"ip"`
	Region string `json:"region,omitempty"`
	ISP    string `json:"isp,omitempty"`
}

// ResourceInfo describes the requested resource.
type ResourceInfo struct {
	Type   string `json:"type"`
	Key    string `json:"key"`
	Scheme string `json:"scheme,omitempty"`
}

// ClientHints provides extra context for scheduling.
type ClientHints struct {
	NeedRange bool `json:"need_range,omitempty"`
}

// DispatchResponse is the response from the dispatch API.
type DispatchResponse struct {
	RequestID  string       `json:"request_id"`
	TTLMs      int64        `json:"ttl_ms"`
	Token      DispatchToken `json:"token"`
	Candidates []Candidate  `json:"candidates"`
}

// DispatchToken is a short-lived access token for edge nodes.
type DispatchToken struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	Exp   int64  `json:"exp"`
}

// Candidate represents a selected edge node for content delivery.
type Candidate struct {
	ID       string   `json:"id"`
	Endpoint string   `json:"endpoint"`
	Weight   int      `json:"weight"`
	Meta     CandidateMeta `json:"meta,omitempty"`
}

// CandidateMeta holds additional node metadata for the client.
type CandidateMeta struct {
	Region string `json:"region,omitempty"`
	ISP    string `json:"isp,omitempty"`
}

// ErrorResponse is the standard error format.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains structured error information.
type ErrorDetail struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	RequestID string            `json:"request_id"`
	Details   map[string]string `json:"details,omitempty"`
}

// ProbeResult holds the result of a single probe attempt.
type ProbeResult struct {
	NodeID       string    `json:"node_id"`
	Endpoint     Endpoint  `json:"endpoint"`
	Success      bool      `json:"success"`
	RTTMs        float64   `json:"rtt_ms"`
	Error        string    `json:"error,omitempty"`
	ProbedAt     time.Time `json:"probed_at"`
}

// ProbeScore aggregates probe results for a node.
type ProbeScore struct {
	NodeID         string  `json:"node_id"`
	SuccessRate1m  float64 `json:"success_rate_1m"`
	SuccessRate5m  float64 `json:"success_rate_5m"`
	RTTP50         float64 `json:"rtt_p50"`
	RTTP95         float64 `json:"rtt_p95"`
	LastOkAt       time.Time `json:"last_ok_at"`
}

// ContentSummary is a compact representation of cached content (v0.2+).
type ContentSummary struct {
	NodeID    string   `json:"node_id"`
	HotKeys   []string `json:"hot_keys,omitempty"`
	BloomFilter []byte  `json:"bloom_filter,omitempty"`
}

// Event types for the event stream.
const (
	EventNodeOnline      = "node.online"
	EventNodeOffline     = "node.offline"
	EventNodeDegraded    = "node.degraded"
	EventNodeQuarantined = "node.quarantined"
	EventNodeRecovered   = "node.recovered"
	EventSchedulerDegraded = "scheduler.degraded"
)

// Event represents a system event.
type Event struct {
	Type      string    `json:"type"`
	NodeID    string    `json:"node_id,omitempty"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data,omitempty"`
}
