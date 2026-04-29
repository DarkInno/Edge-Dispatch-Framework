package models

import (
	"encoding/json"
	"time"
)

// NodeStatus represents the lifecycle state of an edge node.
type NodeStatus string

const (
	NodeStatusRegistered  NodeStatus = "REGISTERED"
	NodeStatusActive      NodeStatus = "ACTIVE"
	NodeStatusDegraded    NodeStatus = "DEGRADED"
	NodeStatusQuarantined NodeStatus = "QUARANTINED"
	NodeStatusOffline     NodeStatus = "OFFLINE"
	NodeStatusDisabled    NodeStatus = "DISABLED"
	NodeStatusMaintenance NodeStatus = "MAINTENANCE"
)

var internedNodeStatuses = map[string]NodeStatus{
	"REGISTERED":  NodeStatusRegistered,
	"ACTIVE":      NodeStatusActive,
	"DEGRADED":    NodeStatusDegraded,
	"QUARANTINED": NodeStatusQuarantined,
	"OFFLINE":     NodeStatusOffline,
	"DISABLED":    NodeStatusDisabled,
	"MAINTENANCE": NodeStatusMaintenance,
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
	Labels       NodeLabels   `json:"labels,omitempty"`
	DisableReason string      `json:"disable_reason,omitempty"`
	MaintainUntil *time.Time  `json:"maintain_until,omitempty"`
	NodeID       string       `json:"node_id"`
	TenantID     string       `json:"tenant_id,omitempty"`
	ProjectID    string       `json:"project_id,omitempty"`
	Name         string       `json:"name"`
	Region       string       `json:"region"`
	ISP          string       `json:"isp"`
	ASN          string       `json:"asn,omitempty"`
	Status       NodeStatus   `json:"status"`
	Weight       int          `json:"weight"`
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
	NATType         string `json:"nat_type,omitempty"` // "", "none", "full_cone", "restricted", "port_restricted", "symmetric"
	TunnelRequired  bool  `json:"tunnel_required"`     // true if node requires tunnel for inbound
}

// NodeScores holds computed quality scores.
type NodeScores struct {
	ReachableScore float64 `json:"reachable_score"`
	HealthScore    float64 `json:"health_score"`
	RiskScore      float64 `json:"risk_score"`
}

// NodeLabels holds key-value labels for a node.
type NodeLabels map[string]string

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
	ContentSummary *ContentSummary `json:"content_summary,omitempty"`
}

// RegisterRequest is the payload for node registration.
type RegisterRequest struct {
	NodeName     string       `json:"node_name"`
	Endpoints    []Endpoint   `json:"public_endpoints"`
	Region       string       `json:"region"`
	ISP          string       `json:"isp"`
	TenantID     string       `json:"tenant_id,omitempty"`
	ProjectID    string       `json:"project_id,omitempty"`
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
	Region    string `json:"region,omitempty"`
	ISP       string `json:"isp,omitempty"`
	ProxyMode string `json:"proxy_mode,omitempty"` // "direct" or "tunnel"
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
	NodeID   string      `json:"node_id"`
	Endpoint string      `json:"endpoint,omitempty"` // Direct endpoint for public nodes
	IsPublic bool        `json:"is_public"`          // true = direct proxy, false = tunnel
	TunnelID string      `json:"tunnel_id,omitempty"` // Tunnel ID for NAT nodes
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
	NodeID      string   `json:"node_id"`
	HotKeys     []string `json:"hot_keys,omitempty"`
	BloomFilter []byte   `json:"bloom_filter,omitempty"`
	BloomK      uint32   `json:"bloom_k,omitempty"`
	TotalKeys   int64    `json:"total_keys"`
	UpdatedAt   int64    `json:"updated_at"`
}

// ContentIndexEntry is a stored entry in the content index (v0.2+).
type ContentIndexEntry struct {
	NodeID      string `json:"node_id"`
	ContentKey  string `json:"content_key"`
	IsHot       bool   `json:"is_hot"`
	LastSeenAt  int64  `json:"last_seen_at"`
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

// ─── v0.4 Streaming Types ───────────────────────────────────────────

// StreamType indicates the streaming protocol format.
type StreamType string

const (
	StreamTypeHLS  StreamType = "hls"
	StreamTypeDASH StreamType = "dash"
)

// ChunkInfo contains metadata about a single streaming chunk (segment/fragment).
type ChunkInfo struct {
	StreamKey  string  `json:"stream_key"`
	SeqNum     int64   `json:"seq_num"`
	URL        string  `json:"url"`
	DurationMs int64   `json:"duration_ms"`
	Size       int64   `json:"size,omitempty"`
}

// ManifestInfo holds parsed playlist/manifest data.
type ManifestInfo struct {
	StreamKey    string      `json:"stream_key"`
	Type         StreamType  `json:"type"`
	Chunks       []ChunkInfo `json:"chunks"`
	TargetDurMs  int64       `json:"target_dur_ms,omitempty"`
	Endlist      bool        `json:"endlist,omitempty"`
	MedSeq       int64       `json:"med_seq,omitempty"`
	UpdatedAt    int64       `json:"updated_at"`
}

// PrefetchRequest describes a batch of chunks to prefetch.
type PrefetchRequest struct {
	StreamKey    string      `json:"stream_key"`
	Chunks       []ChunkInfo `json:"chunks"`
	Priority     int         `json:"priority,omitempty"`
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

// ─── v0.5 Admin Types ───────────────────────────────────────────

// NodeAdminPatch is the admin patchable fields for a node.
type NodeAdminPatch struct {
	Labels        map[string]string `json:"labels,omitempty"`
	Weight        *int              `json:"weight,omitempty"`
	ProjectID     *string           `json:"project_id,omitempty"`
	TenantID      *string           `json:"tenant_id,omitempty"`
	Disabled      *bool             `json:"disabled,omitempty"`
	DisableReason *string           `json:"disable_reason,omitempty"`
	DisableUntil  *time.Time        `json:"disable_until,omitempty"`
}

// Tenant represents a tenant in the multi-tenant system.
type Tenant struct {
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	TenantID    string    `json:"tenant_id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
}

// Project represents a project within a tenant.
type Project struct {
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ProjectID   string    `json:"project_id"`
	TenantID    string    `json:"tenant_id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
}

// UserRole defines user permissions.
type UserRole string

const (
	UserRoleTenantOwner     UserRole = "tenant_owner"
	UserRoleTenantAdmin     UserRole = "tenant_admin"
	UserRoleProjectOperator UserRole = "project_operator"
	UserRoleProjectViewer   UserRole = "project_viewer"
)

// User represents an admin console user.
type User struct {
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	UserID      string       `json:"user_id"`
	TenantID    string       `json:"tenant_id"`
	Email       string       `json:"email"`
	DisplayName string       `json:"display_name"`
	Roles       []RoleBinding `json:"roles,omitempty"`
}

// RoleBinding assigns a role to a user scoped to a tenant/project.
type RoleBinding struct {
	Role      UserRole `json:"role"`
	TenantID  string   `json:"tenant_id,omitempty"`
	ProjectID string   `json:"project_id,omitempty"`
}

// AdminPolicyType defines types of scheduling policies.
type AdminPolicyType string

const (
	AdminPolicyTypeDispatch AdminPolicyType = "dispatch"
	AdminPolicyTypeBlock    AdminPolicyType = "block"
)

// AdminPolicy represents a versioned policy resource.
type AdminPolicy struct {
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Content     json.RawMessage `json:"content"`
	PolicyID    string          `json:"policy_id"`
	TenantID    string          `json:"tenant_id,omitempty"`
	ProjectID   string          `json:"project_id,omitempty"`
	Name        string          `json:"name"`
	Type        AdminPolicyType `json:"type"`
	Version     int             `json:"version"`
	IsPublished bool            `json:"is_published"`
	Description string          `json:"description,omitempty"`
}

// AdminPolicyVersion is a historical version of a policy.
type AdminPolicyVersion struct {
	CreatedAt time.Time       `json:"created_at"`
	Content   json.RawMessage `json:"content"`
	VersionID string          `json:"version_id"`
	PolicyID  string          `json:"policy_id"`
	Version   int             `json:"version"`
}

// IngressType defines the type of ingress entry point.
type IngressType string

const (
	IngressType302     IngressType = "302"
	IngressTypeDNS     IngressType = "dns"
	IngressTypeGateway IngressType = "gateway"
)

// Ingress represents a managed ingress entry point.
type Ingress struct {
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Config    json.RawMessage `json:"config"`
	TenantID  string          `json:"tenant_id,omitempty"`
	ProjectID string          `json:"project_id,omitempty"`
	IngressID string          `json:"ingress_id"`
	Name      string          `json:"name"`
	Type      IngressType     `json:"type"`
	Domain    string          `json:"domain"`
}

// TaskType defines cache operation task types.
type TaskType string

const (
	TaskTypePrewarm TaskType = "prewarm"
	TaskTypePurge   TaskType = "purge"
	TaskTypeBlock   TaskType = "block"
)

// TaskStatus defines task lifecycle states.
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusCancelled TaskStatus = "cancelled"
)

// Task represents an async cache operation task.
type Task struct {
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	Params     json.RawMessage `json:"params"`
	Result     json.RawMessage `json:"result,omitempty"`
	TaskID     string          `json:"task_id"`
	TenantID   string          `json:"tenant_id,omitempty"`
	ProjectID  string          `json:"project_id,omitempty"`
	CreatorID  string          `json:"creator_id"`
	Type       TaskType        `json:"type"`
	Status     TaskStatus      `json:"status"`
	Progress   int             `json:"progress"`
	TotalNodes int             `json:"total_nodes"`
	DoneNodes  int             `json:"done_nodes"`
}

// TaskParams is the params for cache operations.
type TaskParams struct {
	ObjectKey   string   `json:"object_key,omitempty"`
	KeyPrefix   string   `json:"key_prefix,omitempty"`
	TargetNodes []string `json:"target_nodes,omitempty"`
	TargetScope string   `json:"target_scope"` // "all", "region", "node_list"
	Concurrency int      `json:"concurrency,omitempty"`
	RateLimit   int      `json:"rate_limit,omitempty"`
}

// AuditAction represents an auditable action.
type AuditAction string

const (
	AuditActionNodeDisable    AuditAction = "node.disable"
	AuditActionNodeEnable     AuditAction = "node.enable"
	AuditActionNodeRevoke     AuditAction = "node.revoke"
	AuditActionNodePatch      AuditAction = "node.patch"
	AuditActionPolicyCreate   AuditAction = "policy.create"
	AuditActionPolicyUpdate   AuditAction = "policy.update"
	AuditActionPolicyPublish  AuditAction = "policy.publish"
	AuditActionPolicyRollback AuditAction = "policy.rollback"
	AuditActionIngressCreate  AuditAction = "ingress.create"
	AuditActionIngressUpdate  AuditAction = "ingress.update"
	AuditActionIngressDelete  AuditAction = "ingress.delete"
	AuditActionCachePrewarm   AuditAction = "cache.prewarm"
	AuditActionCachePurge     AuditAction = "cache.purge"
	AuditActionObjectBlock    AuditAction = "object.block"
	AuditActionTaskCancel     AuditAction = "task.cancel"
	AuditActionUserCreate     AuditAction = "user.create"
	AuditActionUserUpdate     AuditAction = "user.update"
	AuditActionLogin          AuditAction = "auth.login"
	AuditActionLogout         AuditAction = "auth.logout"
)

// AuditEvent represents an audit log entry.
type AuditEvent struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"tenant_id,omitempty"`
	ProjectID    string          `json:"project_id,omitempty"`
	ActorID      string          `json:"actor_id"`
	ActorEmail   string          `json:"actor_email,omitempty"`
	Action       AuditAction     `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id,omitempty"`
	Before       json.RawMessage `json:"before,omitempty"`
	After        json.RawMessage `json:"after,omitempty"`
	RequestID    string          `json:"request_id"`
	SourceIP     string          `json:"source_ip"`
	UserAgent    string          `json:"user_agent,omitempty"`
	Result       string          `json:"result"` // "success", "failure"
	CreatedAt    time.Time       `json:"created_at"`
}

// AuditQuery represents audit log query filters.
type AuditQuery struct {
	ActorID      string    `json:"actor_id,omitempty"`
	Action       string    `json:"action,omitempty"`
	ResourceType string    `json:"resource_type,omitempty"`
	ResourceID   string    `json:"resource_id,omitempty"`
	Result       string    `json:"result,omitempty"`
	TenantID     string    `json:"tenant_id,omitempty"`
	ProjectID    string    `json:"project_id,omitempty"`
	Since        time.Time `json:"since,omitempty"`
	Until        time.Time `json:"until,omitempty"`
	Limit        int       `json:"limit,omitempty"`
	Offset       int       `json:"offset,omitempty"`
}

// LoginRequest is the admin console login payload.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginResponse is the admin console login response.
type LoginResponse struct {
	Token        string        `json:"token"`
	RefreshToken string        `json:"refresh_token"`
	User         User          `json:"user"`
	ExpiresAt    int64         `json:"expires_at"`
	Roles        []RoleBinding `json:"roles"`
}

// PaginatedResponse wraps list responses with pagination info.
type PaginatedResponse struct {
	Data    any  `json:"data"`
	Total   int  `json:"total"`
	Limit   int  `json:"limit"`
	Offset  int  `json:"offset"`
	HasMore bool `json:"has_more"`
}

// DashboardMetrics holds aggregated dashboard data.
type DashboardMetrics struct {
	QPS           float64          `json:"qps"`
	TotalRequests int64            `json:"total_requests"`
	SuccessRate   float64          `json:"success_rate"`
	ErrorRate     float64          `json:"error_rate"`
	HitRate       float64          `json:"hit_rate"`
	OriginRatio   float64          `json:"origin_ratio"`
	P95Latency    float64          `json:"p95_latency_ms"`
	OnlineNodes   int              `json:"online_nodes"`
	OfflineNodes  int              `json:"offline_nodes"`
	TotalNodes    int              `json:"total_nodes"`
	RecentAlerts  []DashboardAlert `json:"recent_alerts,omitempty"`
}

// DashboardAlert is an alert shown on the dashboard.
type DashboardAlert struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"` // "node_offline", "origin_spike", "error_spike"
	Message   string    `json:"message"`
	NodeID    string    `json:"node_id,omitempty"`
}

// AdminConfig represents system settings.
type AdminConfig struct {
	OIDCEnabled      bool     `json:"oidc_enabled"`
	OIDCProviderURL  string   `json:"oidc_provider_url,omitempty"`
	OIDCClientID     string   `json:"oidc_client_id,omitempty"`
	LocalAuthEnabled bool     `json:"local_auth_enabled"`
	ReadOnlyMode     bool     `json:"read_only_mode"`
	IPAllowlist      []string `json:"ip_allowlist,omitempty"`
	GrafanaURL       string   `json:"grafana_url,omitempty"`
	PrometheusURL    string   `json:"prometheus_url,omitempty"`
	LokiURL          string   `json:"loki_url,omitempty"`
}
