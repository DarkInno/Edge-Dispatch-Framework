package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/models"
)

// Server is a DNS/GSLB adapter that resolves domain queries to edge node IPs.
type Server struct {
	cfg          *config.DNSAdapterConfig
	nodeCache    map[string]models.Candidate
	cacheMu      sync.RWMutex
	cacheExpires time.Time
	client       *http.Client
	conn         *net.UDPConn
	stopCh       chan struct{}
}

// NewServer creates a DNS adapter server.
func NewServer(cfg *config.DNSAdapterConfig) *Server {
	return &Server{
		cfg:    cfg,
		client: &http.Client{Timeout: 5 * time.Second},
		stopCh: make(chan struct{}),
	}
}

// Start begins listening for DNS queries.
func (s *Server) Start(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("resolve udp addr: %w", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}
	s.conn = conn

	slog.Info("DNS adapter listening", "addr", s.cfg.ListenAddr, "domain", s.cfg.Domain)

	// Start node cache refresh loop
	go s.refreshLoop(ctx)

	// Start DNS query handler
	buf := make([]byte, 512)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopCh:
			return nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, clientAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			slog.Warn("dns read error", "err", err)
			continue
		}

		go s.handleQuery(clientAddr, buf[:n])
	}
}

// Shutdown gracefully stops the DNS server.
func (s *Server) Shutdown() error {
	close(s.stopCh)
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

func (s *Server) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.RefreshInterval)
	defer ticker.Stop()

	s.refreshCache(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshCache(ctx)
		}
	}
}

func (s *Server) refreshCache(ctx context.Context) {
	resp, err := s.dispatchResolve(ctx, models.DispatchRequest{
		Client: models.ClientInfo{
			IP:     "0.0.0.0",
			Region: "",
			ISP:    "",
		},
		Resource: models.ResourceInfo{
			Type: "dns",
			Key:  s.cfg.Domain,
		},
	})
	if err != nil {
		slog.Warn("dns cache refresh failed", "err", err)
		return
	}

	s.cacheMu.Lock()
	s.nodeCache = make(map[string]models.Candidate, len(resp.Candidates))
	for _, c := range resp.Candidates {
		s.nodeCache[c.ID] = c
	}
	s.cacheExpires = time.Now().Add(time.Duration(s.cfg.TTLSeconds) * time.Second)
	s.cacheMu.Unlock()

	slog.Debug("dns cache refreshed", "candidates", len(resp.Candidates))
}

func (s *Server) dispatchResolve(ctx context.Context, req models.DispatchRequest) (*models.DispatchResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.ControlPlaneURL+"/v1/dispatch/resolve",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("dispatch request: %w", err)
	}
	defer resp.Body.Close()

	var dr models.DispatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return nil, fmt.Errorf("decode dispatch: %w", err)
	}

	return &dr, nil
}

func (s *Server) handleQuery(clientAddr *net.UDPAddr, query []byte) {
	// Parse DNS query header
	if len(query) < 12 {
		return
	}

	// Extract transaction ID
	txID := query[:2]

	// Parse the question section to extract domain name
	domain, qtype, err := parseQuestion(query)
	if err != nil {
		slog.Debug("dns parse question failed", "err", err)
		return
	}

	// Check if domain matches our configured domain
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	if !strings.HasSuffix(domain, s.cfg.Domain) && domain != s.cfg.Domain {
		// Not our domain, could forward but for now just NXDOMAIN
		s.sendNXDOMAIN(txID, query, clientAddr)
		return
	}

	// Extract subdomain as resource key
	resourceKey := strings.TrimSuffix(domain, "."+s.cfg.Domain)
	if resourceKey == s.cfg.Domain {
		resourceKey = ""
	}
	resourceKey = strings.TrimSuffix(resourceKey, ".")

	s.cacheMu.RLock()
	cached := s.nodeCache
	cacheOk := time.Now().Before(s.cacheExpires)
	s.cacheMu.RUnlock()

	var candidates []models.Candidate
	if cacheOk && resourceKey == "" {
		// Use general cache for base domain queries
		candidates = make([]models.Candidate, 0, len(cached))
		for _, c := range cached {
			candidates = append(candidates, c)
		}
	} else {
		// Resolve through dispatch API for subdomain/content-specific queries
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		resp, err := s.dispatchResolve(ctx, models.DispatchRequest{
			Client: models.ClientInfo{
				IP: clientAddr.IP.String(),
			},
			Resource: models.ResourceInfo{
				Type: "object",
				Key:  resourceKey,
			},
		})
		if err != nil {
			slog.Warn("dns dispatch failed", "domain", domain, "err", err)
			s.sendNXDOMAIN(txID, query, clientAddr)
			return
		}
		candidates = resp.Candidates
	}

	if len(candidates) == 0 {
		s.sendNXDOMAIN(txID, query, clientAddr)
		return
	}

	// Build DNS response with A records for candidates
	s.sendAResponse(txID, query, clientAddr, candidates, qtype)
}

func parseQuestion(query []byte) (string, uint16, error) {
	if len(query) < 16 {
		return "", 0, fmt.Errorf("query too short")
	}

	// Skip header (12 bytes), parse question
	offset := 12
	var parts []string
	for {
		if offset >= len(query) {
			return "", 0, fmt.Errorf("offset overflow")
		}
		length := int(query[offset])
		offset++
		if length == 0 {
			break
		}
		if offset+length > len(query) {
			return "", 0, fmt.Errorf("label overflow")
		}
		parts = append(parts, string(query[offset:offset+length]))
		offset += length
	}

	if offset+4 > len(query) {
		return "", 0, fmt.Errorf("no qtype/qclass")
	}

	qtype := uint16(query[offset])<<8 | uint16(query[offset+1])
	return strings.Join(parts, "."), qtype, nil
}

func (s *Server) sendAResponse(txID []byte, query []byte, clientAddr *net.UDPAddr, candidates []models.Candidate, qtype uint16) {
	resp := buildDNSResponse(txID, query, candidates, s.cfg.TTLSeconds, qtype)
	_, err := s.conn.WriteToUDP(resp, clientAddr)
	if err != nil {
		slog.Warn("dns write response failed", "err", err)
	}
}

func (s *Server) sendNXDOMAIN(txID []byte, query []byte, clientAddr *net.UDPAddr) {
	resp := buildNXDOMAIN(txID, query)
	_, err := s.conn.WriteToUDP(resp, clientAddr)
	if err != nil {
		slog.Warn("dns write nxdomain failed", "err", err)
	}
}

func buildDNSResponse(txID []byte, query []byte, candidates []models.Candidate, ttl int64, qtype uint16) []byte {
	buf := newDNSBuffer()

	// Header
	copy(buf[:2], txID)
	buf[2] = 0x81 // QR=1, OPCODE=0, AA=0, TC=0, RD=1
	buf[3] = 0x80 // RA=1, Z=0, RCODE=0

	// Copy question section from query
	copySection(query, buf, 12, len(query)-12)

	buf[4] = 0x00 // QDCOUNT high
	buf[5] = 0x01 // QDCOUNT low

	ansCount := uint16(0)
	for _, c := range candidates {
		ip := extractIP(c.Endpoint)
		if ip == nil {
			continue
		}

		ansCount++
		// Name pointer to question
		buf = append(buf, 0xC0, 0x0C)

		recordType := uint16(1) // A record default
		if qtype == 28 {
			recordType = 28 // AAAA
		}

		// TYPE = A (1) or AAAA (28)
		buf = append(buf, byte(recordType>>8), byte(recordType))
		// CLASS = IN (1)
		buf = append(buf, 0x00, 0x01)
		// TTL
		buf = append(buf, byte(ttl>>24), byte(ttl>>16), byte(ttl>>8), byte(ttl))

		if recordType == 1 {
			// RDLENGTH = 4, RDATA = IPv4
			buf = append(buf, 0x00, 0x04)
			buf = append(buf, ip.To4()...)
		} else {
			// RDLENGTH = 16, RDATA = IPv6
			buf = append(buf, 0x00, 0x10)
			buf = append(buf, ip.To16()...)
		}
	}

	// Set ANCOUNT
	buf[6] = byte(ansCount >> 8)
	buf[7] = byte(ansCount)

	return buf
}

func buildNXDOMAIN(txID []byte, query []byte) []byte {
	buf := newDNSBuffer()

	copy(buf[:2], txID)
	buf[2] = 0x81
	buf[3] = 0x83 // RCODE=3 (NXDOMAIN)

	qLen := copySection(query, buf, 12, len(query)-12)
	_ = qLen

	buf[4] = 0x00 // QDCOUNT
	buf[5] = 0x01
	buf[6] = 0x00 // ANCOUNT
	buf[7] = 0x00

	return buf
}

func newDNSBuffer() []byte {
	return make([]byte, 12)
}

func copySection(src []byte, dst []byte, srcStart int, length int) int {
	if srcStart+length > len(src) {
		length = len(src) - srcStart
	}
	copied := 0
	for i := 0; i < length; i++ {
		dst = append(dst, src[srcStart+i])
		copied++
	}
	return copied
}

func extractIP(endpoint string) net.IP {
	// Strip scheme prefix
	if idx := strings.Index(endpoint, "://"); idx >= 0 {
		endpoint = endpoint[idx+3:]
	}
	// Strip port suffix
	if idx := strings.LastIndex(endpoint, ":"); idx > 0 {
		endpoint = endpoint[:idx]
	}
	// Strip brackets from IPv6
	endpoint = strings.Trim(endpoint, "[]")

	return net.ParseIP(endpoint)
}
