package config

import (
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
}

// ControlPlaneConfig holds control plane specific configuration.
type ControlPlaneConfig struct {
	ListenAddr     string
	PGURL          string
	RedisAddr      string
	RedisPassword  string
	ProbeInterval  time.Duration
	ProbeTimeout   time.Duration
	HeartbeatTTL   time.Duration
	TokenSecret    string
	MaxCandidates  int
	DefaultTTLMs   int64
	DegradeToOrigin bool
	NodeCacheTTL   time.Duration
	TLSCertFile    string
	TLSKeyFile     string
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
}

// OriginConfig holds origin server configuration.
type OriginConfig struct {
	ListenAddr  string
	DataDir     string
	TLSCertFile string
	TLSKeyFile  string
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
		slog.Warn("WARNING: CP_TOKEN_SECRET is using the default value. Set a strong secret in production.")
	}
	if strings.Contains(c.PGURL, "sslmode=disable") {
		slog.Warn("WARNING: PostgreSQL connection has SSL disabled (sslmode=disable). Enable TLS in production.")
	}
}

func (c *EdgeAgentConfig) warnDefaults() {
	if c.ControlPlaneURL != "" && !strings.Contains(c.ControlPlaneURL, "https") && !strings.HasPrefix(c.ControlPlaneURL, "http://localhost") {
		slog.Warn("WARNING: ControlPlaneURL is using plain HTTP. Use HTTPS in production.")
	}
}
