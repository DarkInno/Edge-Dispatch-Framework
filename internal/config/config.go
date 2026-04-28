package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all configuration for any component.
type Config struct {
	ControlPlane ControlPlaneConfig
	EdgeAgent    EdgeAgentConfig
	Origin       OriginConfig
	Gateway      GatewayConfig
}

// GatewayConfig holds gateway configuration (v0.3).
type GatewayConfig struct {
	ListenAddr      string
	TunnelAddr      string
	ControlPlaneURL string
	AuthToken       string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
}

// ControlPlaneConfig holds control plane specific configuration.
type ControlPlaneConfig struct {
	ListenAddr      string
	PGURL           string
	RedisAddr       string
	RedisPassword   string
	ProbeInterval   time.Duration
	ProbeTimeout    time.Duration
	HeartbeatTTL    time.Duration
	TokenSecret     string
	MaxCandidates   int
	DefaultTTLMs    int64
	DegradeToOrigin bool
	NodeCacheTTL    time.Duration
	TLSCertFile     string
	TLSKeyFile      string
	ContentIndex    ContentIndexConfig
	OriginURL       string
	Streaming       *StreamingConfig
}

// EdgeAgentConfig holds edge agent specific configuration.
type EdgeAgentConfig struct {
	ListenAddr       string
	ControlPlaneURL  string
	NodeToken        string
	CacheDir         string
	CacheMaxGB       int64
	OriginURL        string
	HeartbeatInterval time.Duration
	MaxConns         int
	TLSCertFile      string
	TLSKeyFile       string
	// NAT/Tunnel support (v0.3)
	NATMode          bool   // true if this node is behind NAT
	TunnelServerAddr string // Tunnel server address (e.g., "gateway:7700")
	TunnelAuthToken  string // Token for tunnel authentication
	// Streaming support (v0.4)
	Streaming        *StreamingConfig
}

// OriginConfig holds origin server configuration.
type OriginConfig struct {
	ListenAddr  string
	DataDir     string
	TLSCertFile string
	TLSKeyFile  string
}

// DNSAdapterConfig holds DNS/GSLB adapter configuration (v0.2+).
type DNSAdapterConfig struct {
	ListenAddr        string
	ControlPlaneURL   string
	Domain            string
	TTLSeconds        int64
	RefreshInterval   time.Duration
	TokenSecret       string
	ContentAwareScore float64
}

// ContentIndexConfig holds content index configuration (v0.2+).
type ContentIndexConfig struct {
	BloomCapacity       int
	BloomFPRate         float64
	HotKeyTTL           time.Duration
	ContentAwareWeight  float64
	HotContentAwareWeight float64
}

// StreamingConfig holds streaming-specific configuration (v0.4).
type StreamingConfig struct {
	Enabled         bool
	PrefetchCount   int
	WindowSize      int
	PrefetchWorkers int
	ManifestTTL     time.Duration
	ChunkDurationMs int64
	StreamType      string
}

// DefaultStreamingConfig returns sensible defaults for streaming.
func DefaultStreamingConfig() *StreamingConfig {
	return &StreamingConfig{
		Enabled:         boolEnv("STREAM_ENABLED", true),
		PrefetchCount:   intEnv("STREAM_PREFETCH_COUNT", 3),
		WindowSize:      intEnv("STREAM_WINDOW_SIZE", 60),
		PrefetchWorkers: intEnv("STREAM_PREFETCH_WORKERS", 4),
		ManifestTTL:     durationEnv("STREAM_MANIFEST_TTL", 5*time.Second),
		ChunkDurationMs: int64(intEnv("STREAM_CHUNK_DURATION_MS", 2000)),
		StreamType:      getEnv("STREAM_TYPE", ""),
	}
}

// LoadControlPlane loads configuration from environment variables for control plane.
func LoadControlPlane() *ControlPlaneConfig {
	cfg := &ControlPlaneConfig{
		ListenAddr:      getEnv("CP_LISTEN_ADDR", ":8080"),
		PGURL:           getEnv("CP_PG_URL", "postgres://edf:edf@localhost:5432/edf?sslmode=disable"),
		RedisAddr:       getEnv("CP_REDIS_ADDR", "localhost:6379"),
		RedisPassword:   getEnv("CP_REDIS_PASSWORD", ""),
		ProbeInterval:   durationEnv("CP_PROBE_INTERVAL", 10*time.Second),
		ProbeTimeout:    durationEnv("CP_PROBE_TIMEOUT", 5*time.Second),
		HeartbeatTTL:    durationEnv("CP_HEARTBEAT_TTL", 30*time.Second),
		TokenSecret:     getEnv("CP_TOKEN_SECRET", "change-me-in-production"),
		MaxCandidates:   intEnv("CP_MAX_CANDIDATES", 5),
		DefaultTTLMs:    int64(intEnv("CP_DEFAULT_TTL_MS", 30000)),
		DegradeToOrigin: boolEnv("CP_DEGRADE_TO_ORIGIN", true),
		NodeCacheTTL:    durationEnv("CP_NODE_CACHE_TTL", 5*time.Second),
		TLSCertFile:     getEnv("CP_TLS_CERT_FILE", ""),
		TLSKeyFile:      getEnv("CP_TLS_KEY_FILE", ""),
		OriginURL:       getEnv("CP_ORIGIN_URL", "http://localhost:7070"),
		ContentIndex: ContentIndexConfig{
			BloomCapacity:         intEnv("CP_CI_BLOOM_CAPACITY", 10000),
			BloomFPRate:           floatEnv("CP_CI_BLOOM_FP_RATE", 0.01),
			HotKeyTTL:             durationEnv("CP_CI_HOT_KEY_TTL", 5*time.Minute),
			ContentAwareWeight:     floatEnv("CP_CI_CONTENT_AWARE_WEIGHT", 10.0),
			HotContentAwareWeight:  floatEnv("CP_CI_HOT_CONTENT_WEIGHT", 25.0),
		},
		Streaming: DefaultStreamingConfig(),
	}
	cfg.warnDefaults()
	return cfg
}

// LoadEdgeAgent loads configuration for edge agent.
func LoadEdgeAgent() *EdgeAgentConfig {
	cfg := &EdgeAgentConfig{
		ListenAddr:        getEnv("EA_LISTEN_ADDR", ":9090"),
		ControlPlaneURL:   getEnv("EA_CONTROL_PLANE_URL", "http://localhost:8080"),
		NodeToken:         getEnv("EA_NODE_TOKEN", ""),
		CacheDir:          getEnv("EA_CACHE_DIR", "/tmp/edf-cache"),
		CacheMaxGB:        int64(intEnv("EA_CACHE_MAX_GB", 10)),
		OriginURL:         getEnv("EA_ORIGIN_URL", "http://localhost:7070"),
		HeartbeatInterval: durationEnv("EA_HEARTBEAT_INTERVAL", 10*time.Second),
		MaxConns:          intEnv("EA_MAX_CONNS", 1000),
		TLSCertFile:       getEnv("EA_TLS_CERT_FILE", ""),
		TLSKeyFile:        getEnv("EA_TLS_KEY_FILE", ""),
		// NAT/Tunnel support (v0.3)
		NATMode:          boolEnv("EA_NAT_MODE", false),
		TunnelServerAddr: getEnv("EA_TUNNEL_SERVER_ADDR", ""),
		TunnelAuthToken:  getEnv("EA_TUNNEL_AUTH_TOKEN", ""),
		// Streaming support (v0.4)
		Streaming: DefaultStreamingConfig(),
	}
	cfg.warnDefaults()
	return cfg
}

// LoadGateway loads configuration for the gateway (v0.3).
func LoadGateway() *GatewayConfig {
	cfg := &GatewayConfig{
		ListenAddr:      getEnv("GW_LISTEN_ADDR", ":8880"),
		TunnelAddr:      getEnv("GW_TUNNEL_ADDR", ":7700"),
		ControlPlaneURL: getEnv("GW_CONTROL_PLANE_URL", "http://localhost:8080"),
		AuthToken:       getEnv("GW_AUTH_TOKEN", "change-me-in-production"),
		ReadTimeout:     durationEnv("GW_READ_TIMEOUT", 30*time.Second),
		WriteTimeout:    durationEnv("GW_WRITE_TIMEOUT", 60*time.Second),
		IdleTimeout:     durationEnv("GW_IDLE_TIMEOUT", 120*time.Second),
	}
	cfg.warnDefaults()
	return cfg
}

// LoadOrigin loads configuration for the origin server.
func LoadOrigin() *OriginConfig {
	return &OriginConfig{
		ListenAddr:  getEnv("ORIGIN_LISTEN_ADDR", ":7070"),
		DataDir:     getEnv("ORIGIN_DATA_DIR", "/tmp/edf-origin"),
		TLSCertFile: getEnv("ORIGIN_TLS_CERT_FILE", ""),
		TLSKeyFile:  getEnv("ORIGIN_TLS_KEY_FILE", ""),
	}
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func intEnv(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

func floatEnv(key string, defaultVal float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return defaultVal
}

func durationEnv(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultVal
}

func boolEnv(key string, defaultVal bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return defaultVal
}

func (c *ControlPlaneConfig) warnDefaults() {
	if c.TokenSecret == "change-me-in-production" {
		slog.Error("FATAL: CP_TOKEN_SECRET is using the default value. Set a strong secret via CP_TOKEN_SECRET environment variable.", "default", c.TokenSecret)
		fmt.Fprintln(os.Stderr, "FATAL: CP_TOKEN_SECRET must be set to a strong random secret. Refusing to start with default value.")
		os.Exit(1)
	}
	if strings.Contains(c.PGURL, "sslmode=disable") {
		slog.Warn("WARNING: PostgreSQL connection has SSL disabled (sslmode=disable). Enable TLS in production.")
	}
}

func (c *EdgeAgentConfig) warnDefaults() {
	if c.ControlPlaneURL != "" && !strings.Contains(c.ControlPlaneURL, "https") && !strings.HasPrefix(c.ControlPlaneURL, "http://localhost") {
		slog.Warn("WARNING: ControlPlaneURL is using plain HTTP. Use HTTPS in production.")
	}
	if c.NATMode && c.TunnelServerAddr == "" {
		slog.Warn("WARNING: NAT mode enabled but no tunnel server address configured.")
	}
}

func (c *GatewayConfig) warnDefaults() {
	if c.AuthToken == "change-me-in-production" {
		slog.Error("FATAL: GW_AUTH_TOKEN is using the default value. Set a strong secret via GW_AUTH_TOKEN environment variable.", "default", c.AuthToken)
		fmt.Fprintln(os.Stderr, "FATAL: GW_AUTH_TOKEN must be set to a strong random secret. Refusing to start with default value.")
		os.Exit(1)
	}
	if c.ControlPlaneURL != "" && !strings.Contains(c.ControlPlaneURL, "https") && !strings.HasPrefix(c.ControlPlaneURL, "http://localhost") {
		slog.Warn("WARNING: ControlPlaneURL is using plain HTTP. Use HTTPS in production.")
	}
}

// LoadDNSAdapter loads configuration for the DNS/GSLB adapter (v0.2+).
func LoadDNSAdapter() *DNSAdapterConfig {
	return &DNSAdapterConfig{
		ListenAddr:        getEnv("DNS_LISTEN_ADDR", ":5353"),
		ControlPlaneURL:   getEnv("DNS_CONTROL_PLANE_URL", "http://localhost:8080"),
		Domain:            getEnv("DNS_DOMAIN", "edge.local"),
		TTLSeconds:        int64(intEnv("DNS_TTL_SECONDS", 30)),
		RefreshInterval:   durationEnv("DNS_REFRESH_INTERVAL", 10*time.Second),
		TokenSecret:       getEnv("DNS_TOKEN_SECRET", "change-me-in-production"),
		ContentAwareScore: floatEnv("DNS_CONTENT_AWARE_SCORE", 25.0),
	}
}

// LoadContentIndex loads configuration for the content index (v0.2+).
func LoadContentIndex() *ContentIndexConfig {
	return &ContentIndexConfig{
		BloomCapacity:       intEnv("CI_BLOOM_CAPACITY", 10000),
		BloomFPRate:         floatEnv("CI_BLOOM_FP_RATE", 0.01),
		HotKeyTTL:           durationEnv("CI_HOT_KEY_TTL", 5*time.Minute),
		ContentAwareWeight:  floatEnv("CI_CONTENT_AWARE_WEIGHT", 10.0),
		HotContentAwareWeight: floatEnv("CI_HOT_CONTENT_WEIGHT", 25.0),
	}
}
