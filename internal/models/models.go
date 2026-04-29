package models

import "time"

// NodeStatus represents the lifecycle state of an edge node.
type NodeStatus string

const (
	NodeStatusRegistered  NodeStatus = "REGISTERED"
	NodeStatusActive      NodeStatus = "ACTIVE"
	NodeStatusDegraded    NodeStatus = "DEGRADED"
	NodeStatusQuarantined NodeStatus = "QUARANTINED"
	NodeStatusDisabled    NodeStatus = "DISABLED"
	NodeStatusOffline     NodeStatus = "OFFLINE"
)

var internedNodeStatuses = map[string]NodeStatus{
	"REGISTERED":  NodeStatusRegistered,
	"ACTIVE":      NodeStatusActive,
	"DEGRADED":    NodeStatusDegraded,
	"QUARANTINED": NodeStatusQuarantined,
	"DISABLED":    NodeStatusDisabled,
	"OFFLINE":     NodeStatusOffline,
}

func InternNodeStatus(s string) NodeStatus {
	if v, ok := internedNodeStatuses[s]; ok {
		return v
	}
	return NodeStatus(s)
}

// Node represents an edge node registered with the control plane.
type Node struct {
	LastSeenAt   time.Time    `json:"last_seen_at"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
	Endpoints    []Endpoint   `json:"endpoints"`
	Capabilities Capabilities `json:"capabilities"`
	Scores       NodeScores   `json:"scores,omitempty"`
	NodeID       string       `json:"node_id"`
	TenantID     string       `json:"tenant_id,omitempty"`
	Name         string       `json:"name"`
	Region       string       `json:"region"`
	ISP          string       `json:"isp"`
	ASN          string       `json:"asn,omitempty"`
	Status       NodeStatus   `json:"status"`
}

// Endpoint represents a service endpoint of an edge node.
type Endpoint struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
}

// Capabilities describes the node's hardware and network capabilities.
type Capabilities struct {
	InboundReachable bool   `json:"inbound_reachable"`
	CacheDiskGB      int64  `json:"cache_disk_gb"`
	MaxUplinkMbps    int64  `json:"max_uplink_mbps"`
	SupportsHTTPS    bool   `json:"supports_https"`
	NATType          string `json:"nat_type,omitempty"` // "", "none", "full_cone", "restricted", "port_restricted", "symmetric"
	TunnelRequired   bool   `json:"tunnel_required"`    // true if node requires tunnel for inbound
}

// NodeScores holds computed quality scores.
type NodeScores struct {
	ReachableScore float64 `json:"reachable_score"`
	HealthScore    float64 `json:"health_score"`
	RiskScore      float64 `json:"risk_score"`
}

// NodeRuntime contains the current runtime metrics reported via heartbeat.
type NodeRuntime struct {
	CPU        float64 `json:"cpu"`
	MemMB      int64   `json:"mem_mb"`
	DiskFreeGB int64   `json:"disk_free_gb"`
	Conn       int64   `json:"conn"`
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
	NodeID         string          `json:"node_id"`
	TS             int64           `json:"ts"`
	Runtime        NodeRuntime     `json:"runtime"`
	Traffic        NodeTraffic     `json:"traffic"`
	Cache          NodeCache       `json:"cache"`
	ContentSummary *ContentSummary `json:"content_summary,omitempty"`
}

// RegisterRequest is the payload for node registration.
type RegisterRequest struct {
	NodeName     string       `json:"node_name"`
	Endpoints    []Endpoint   `json:"public_endpoints"`
	Region       string       `json:"region"`
	ISP          string       `json:"isp"`
	Capabilities Capabilities `json:"capabilities"`
}

// RegisterResponse is returned after successful registration.
type RegisterResponse struct {
	NodeID string   `json:"node_id"`
	Auth   NodeAuth `json:"auth"`
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
	RequestID  string        `json:"request_id"`
	TTLMs      int64         `json:"ttl_ms"`
	Token      DispatchToken `json:"token"`
	Candidates []Candidate   `json:"candidates"`
}

// DispatchToken is a short-lived access token for edge nodes.
type DispatchToken struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	Exp   int64  `json:"exp"`
}

// Candidate represents a selected edge node for content delivery.
type Candidate struct {
	ID       string        `json:"id"`
	Endpoint string        `json:"endpoint"`
	Weight   int           `json:"weight"`
	Meta     CandidateMeta `json:"meta,omitempty"`
}

// CandidateMeta holds additional node metadata for the client.
type CandidateMeta struct {
	Region     string `json:"region,omitempty"`
	ISP        string `json:"isp,omitempty"`
	ProxyMode  string `json:"proxy_mode,omitempty"`  // "direct" or "tunnel"
	GatewayURL string `json:"gateway_url,omitempty"` // Gateway URL for tunnel nodes
}

// GatewayRequest represents a request from the gateway to the control plane.
type GatewayRequest struct {
	ResourceKey string     `json:"resource_key"`
	ClientIP    string     `json:"client_ip"`
	ClientInfo  ClientInfo `json:"client_info"`
}

// GatewayResponse is returned by the control plane to the gateway.
type GatewayResponse struct {
	NodeID   string `json:"node_id"`
	Endpoint string `json:"endpoint,omitempty"`  // Direct endpoint for public nodes
	IsPublic bool   `json:"is_public"`           // true = direct proxy, false = tunnel
	TunnelID string `json:"tunnel_id,omitempty"` // Tunnel ID for NAT nodes
}

// TunnelStatus represents the status of a tunnel connection.
type TunnelStatus struct {
	TunnelID     string    `json:"tunnel_id"`
	NodeID       string    `json:"node_id"`
	ExternalAddr string    `json:"external_addr"`
	ConnectedAt  time.Time `json:"connected_at"`
	LastPingAt   time.Time `json:"last_ping_at"`
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
	NodeID   string    `json:"node_id"`
	Endpoint Endpoint  `json:"endpoint"`
	Success  bool      `json:"success"`
	RTTMs    float64   `json:"rtt_ms"`
	Error    string    `json:"error,omitempty"`
	ProbedAt time.Time `json:"probed_at"`
}

// ProbeScore aggregates probe results for a node.
type ProbeScore struct {
	NodeID        string    `json:"node_id"`
	SuccessRate1m float64   `json:"success_rate_1m"`
	SuccessRate5m float64   `json:"success_rate_5m"`
	RTTP50        float64   `json:"rtt_p50"`
	RTTP95        float64   `json:"rtt_p95"`
	LastOkAt      time.Time `json:"last_ok_at"`
}

// ContentSummary is a compact representation of cached content (v0.2+).
type ContentSummary struct {
	NodeID      string   `json:"node_id"`
	HotKeys     []string `json:"hot_keys,omitempty"`
	BloomFilter []byte   `json:"bloom_filter,omitempty"`
	BloomK      uint32   `json:"bloom_k,omitempty"`
	TotalKeys   int64    `json:"total_keys"`
	UpdatedAt   int64    `json:"updated_at"`
}

// ContentIndexEntry is a stored entry in the content index (v0.2+).
type ContentIndexEntry struct {
	NodeID     string `json:"node_id"`
	ContentKey string `json:"content_key"`
	IsHot      bool   `json:"is_hot"`
	LastSeenAt int64  `json:"last_seen_at"`
}

// DNSQuery represents a DNS resolution request from the GSLB adapter (v0.2+).
type DNSQuery struct {
	FQDN      string     `json:"fqdn"`
	Client    ClientInfo `json:"client"`
	QueryType uint16     `json:"query_type"`
}

// DNSResponse represents a DNS resolution response (v0.2+).
type DNSResponse struct {
	FQDN       string      `json:"fqdn"`
	Records    []DNSRecord `json:"records"`
	TTLSeconds int64       `json:"ttl_seconds"`
	Token      string      `json:"token,omitempty"`
}

// DNSRecord represents a single DNS resource record.
type DNSRecord struct {
	Type     string `json:"type"`
	Address  string `json:"address"`
	Weight   int    `json:"weight"`
	Priority int    `json:"priority"`
}

// EdgeAgentReport enriches the heartbeat with content summary (v0.2+).
type EdgeAgentReport struct {
	HeartbeatRequest
	ContentSummary *ContentSummary `json:"content_summary,omitempty"`
}

// Event types for the event stream.
const (
	EventNodeOnline        = "node.online"
	EventNodeOffline       = "node.offline"
	EventNodeDegraded      = "node.degraded"
	EventNodeQuarantined   = "node.quarantined"
	EventNodeRecovered     = "node.recovered"
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

// ─── v0.4 Streaming Types ───────────────────────────────────────────

// StreamType indicates the streaming protocol format.
type StreamType string

const (
	StreamTypeHLS  StreamType = "hls"
	StreamTypeDASH StreamType = "dash"
)

// ChunkInfo contains metadata about a single streaming chunk (segment/fragment).
type ChunkInfo struct {
	StreamKey  string `json:"stream_key"`
	SeqNum     int64  `json:"seq_num"`
	URL        string `json:"url"`
	DurationMs int64  `json:"duration_ms"`
	Size       int64  `json:"size,omitempty"`
}

// ManifestInfo holds parsed playlist/manifest data.
type ManifestInfo struct {
	StreamKey   string      `json:"stream_key"`
	Type        StreamType  `json:"type"`
	Chunks      []ChunkInfo `json:"chunks"`
	TargetDurMs int64       `json:"target_dur_ms,omitempty"`
	Endlist     bool        `json:"endlist,omitempty"`
	MedSeq      int64       `json:"med_seq,omitempty"`
	UpdatedAt   int64       `json:"updated_at"`
}

// PrefetchRequest describes a batch of chunks to prefetch.
type PrefetchRequest struct {
	StreamKey string      `json:"stream_key"`
	Chunks    []ChunkInfo `json:"chunks"`
	Priority  int         `json:"priority,omitempty"`
}

// PrefetchResult reports the outcome of prefetch operations.
type PrefetchResult struct {
	StreamKey string `json:"stream_key"`
	Fetched   int    `json:"fetched"`
	Failed    int    `json:"failed"`
	Total     int    `json:"total"`
}

// StreamingMetrics tracks streaming-specific stats.
type StreamingMetrics struct {
	ChunkRequests   int64   `json:"chunk_requests"`
	ChunkCacheHits  int64   `json:"chunk_cache_hits"`
	ChunkPrefetched int64   `json:"chunk_prefetched"`
	WindowEvictions int64   `json:"window_evictions"`
	ManifestFetches int64   `json:"manifest_fetches"`
	AvgLatencyMs    float64 `json:"avg_latency_ms,omitempty"`
}
