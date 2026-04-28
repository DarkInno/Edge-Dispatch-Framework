package edgeagent

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/auth"
	"github.com/darkinno/edge-dispatch-framework/internal/config"
	"github.com/darkinno/edge-dispatch-framework/internal/streaming"
)

func extractClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.LastIndex(xff, ","); idx > 0 {
			return strings.TrimSpace(xff[idx+1:])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

var bufPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

type serverMetrics struct {
	requests    atomic.Int64
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
	bytesSent   atomic.Int64
	errors      atomic.Int64
}

type Server struct {
	cache     *Cache
	fetcher   *Fetcher
	cfg       *config.EdgeAgentConfig
	signer    *auth.Signer
	metrics   serverMetrics
	httpSrv   *http.Server
	listener  net.Listener
	streamH   *streaming.Handler
}

func NewServer(cache *Cache, fetcher *Fetcher, cfg *config.EdgeAgentConfig) *Server {
	return &Server{
		cache:   cache,
		fetcher: fetcher,
		cfg:     cfg,
		signer:  auth.NewSigner(cfg.NodeToken),
	}
}

// WithStreaming attaches a streaming handler for HLS/DASH support (v0.4).
func (s *Server) WithStreaming(handler *streaming.Handler) *Server {
	s.streamH = handler
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/obj/", s.handleObject)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/metrics", s.handleMetrics)
	return withCommonHeaders(withGzipCompression(mux))
}

var commonRespHeaders = map[string]string{
	"Connection":        "keep-alive",
	"Keep-Alive":        "timeout=120, max=1000",
	"X-Content-Type-Options": "nosniff",
}

func withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range commonRespHeaders {
			w.Header().Set(k, v)
		}
		next.ServeHTTP(w, r)
	})
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
	wroteHeader bool
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	ct := w.ResponseWriter.Header().Get("Content-Type")
	if !isCompressible(ct) {
		w.Writer = w.ResponseWriter
		w.ResponseWriter.Header().Del("Content-Encoding")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.Writer.Write(b)
}

func (w *gzipResponseWriter) Flush() {
	if f, ok := w.Writer.(http.Flusher); ok {
		f.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func isCompressible(ct string) bool {
	if ct == "" {
		return true
	}
	return strings.HasPrefix(ct, "application/json") ||
		strings.HasPrefix(ct, "text/") ||
		strings.HasPrefix(ct, "application/javascript") ||
		strings.HasPrefix(ct, "application/xml")
}

func withGzipCompression(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gz, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		defer gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		next.ServeHTTP(&gzipResponseWriter{Writer: gz, ResponseWriter: w}, r)
	})
}

func (s *Server) Start(ctx context.Context) error {
	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	var ln net.Listener
	var err error
	if s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("load tls cert: %w", err)
		}
		tlsCfg := &tls.Config{
			Certificates:     []tls.Certificate{cert},
			MinVersion:       tls.VersionTLS12,
			SessionTicketsDisabled: false,
		}
		s.httpSrv.TLSConfig = tlsCfg
		ln, err = tls.Listen("tcp", s.cfg.ListenAddr, tlsCfg)
	} else {
		ln, err = net.Listen("tcp", s.cfg.ListenAddr)
	}
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.ListenAddr, err)
	}
	s.listener = ln

	go func() {
		slog.Info("edge agent server starting", "addr", s.cfg.ListenAddr)
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
		}
	}()

	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv != nil {
		slog.Info("shutting down edge agent server")
		return s.httpSrv.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request) {
	s.metrics.requests.Add(1)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		s.metrics.errors.Add(1)
		return
	}

	// Verify token with IP binding (mandatory when signer is configured)
	token := r.Header.Get("Authorization")
	if token != "" && strings.HasPrefix(token, "Bearer ") {
		token = strings.TrimPrefix(token, "Bearer ")
	} else {
		token = r.URL.Query().Get("token")
	}
	if token == "" {
		slog.Warn("missing authentication token", "client_ip", extractClientIP(r))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		s.metrics.errors.Add(1)
		return
	}
	clientIP := extractClientIP(r)
	if _, err := s.signer.VerifyWithIP(token, clientIP); err != nil {
		slog.Warn("invalid token", "err", err, "client_ip", clientIP)
		http.Error(w, "forbidden", http.StatusForbidden)
		s.metrics.errors.Add(1)
		return
	}

	// Extract key from /obj/{key}
	key := strings.TrimPrefix(r.URL.Path, "/obj/")
	key = strings.TrimSpace(key)
	if key == "" || key == "." || strings.Contains(key, "..") {
		http.Error(w, "invalid key", http.StatusBadRequest)
		s.metrics.errors.Add(1)
		return
	}

	rangeHeader := r.Header.Get("Range")

	// Intercept streaming requests (v0.4)
	if s.streamH != nil && s.streamH.IsStreamingRequest(key) {
		s.handleStreamingRequest(w, r, key)
		return
	}

	// Try cache first
	reader, contentLen, err := s.cache.Get(r.Context(), key)
	if err != nil {
		// Cache miss — fetch from origin
		s.metrics.cacheMisses.Add(1)
		slog.Debug("cache miss", "key", key)

		var fr *FetchResult
		if rangeHeader != "" {
			fr, err = s.fetcher.FetchWithRange(r.Context(), key, rangeHeader)
		} else {
			fr, err = s.fetcher.Fetch(r.Context(), key)
		}
		if err != nil {
			slog.Error("fetch failed", "key", key, "err", err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			s.metrics.errors.Add(1)
			return
		}
		defer fr.Body.Close()

		if fr.ContentLength > 0 {
			s.serveFromFetchResult(w, r, fr)
			s.metrics.bytesSent.Add(fr.ContentLength)
		} else {
			n, _ := io.Copy(w, fr.Body)
			s.metrics.bytesSent.Add(n)
		}
		return
	}
	defer reader.Close()

	s.metrics.cacheHits.Add(1)

	// Handle Range request from cache
	if rangeHeader != "" {
		s.serveRangeFromCache(w, r, reader, contentLen, key)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(contentLen, 10))
		w.WriteHeader(http.StatusOK)
		n, _ := io.Copy(w, reader)
		s.metrics.bytesSent.Add(n)
	}
}

func (s *Server) serveFromFetchResult(w http.ResponseWriter, r *http.Request, fr *FetchResult) {
	if fr.ContentType != "" {
		w.Header().Set("Content-Type", fr.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if fr.ETag != "" {
		w.Header().Set("ETag", fr.ETag)
	}
	if fr.ContentLength > 0 && fr.StatusCode != http.StatusPartialContent {
		w.Header().Set("Content-Length", strconv.FormatInt(fr.ContentLength, 10))
	}
	w.WriteHeader(fr.StatusCode)
	io.Copy(w, fr.Body)
}

func (s *Server) serveRangeFromCache(w http.ResponseWriter, r *http.Request, reader io.ReadCloser, contentLen int64, key string) {
	start, end, err := parseRangeHeader(r.Header.Get("Range"), contentLen)
	if err != nil {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(contentLen, 10))
		w.WriteHeader(http.StatusOK)
		n, _ := io.Copy(w, reader)
		s.metrics.bytesSent.Add(n)
		return
	}

	// Seek to start position
	if seeker, ok := reader.(io.ReadSeeker); ok {
		if _, err := seeker.Seek(start, io.SeekStart); err != nil {
			slog.Error("seek failed", "key", key, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			s.metrics.errors.Add(1)
			return
		}
	}

	rangeLen := end - start + 1
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, contentLen))
	w.Header().Set("Content-Length", strconv.FormatInt(rangeLen, 10))
	w.WriteHeader(http.StatusPartialContent)
	io.CopyN(w, reader, rangeLen)
	s.metrics.bytesSent.Add(rangeLen)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	json.NewEncoder(buf).Encode(map[string]string{"status": "ok"})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	cacheStats := s.cache.Stats()

	data := map[string]interface{}{
		"requests":     s.metrics.requests.Load(),
		"cache_hits":   s.metrics.cacheHits.Load(),
		"cache_misses": s.metrics.cacheMisses.Load(),
		"bytes_sent":   s.metrics.bytesSent.Load(),
		"errors":       s.metrics.errors.Load(),
		"cache":        cacheStats,
	}

	if s.streamH != nil {
		data["streaming"] = s.streamH.Metrics()
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	json.NewEncoder(buf).Encode(data)
	w.Header().Set("Content-Type", "application/json")
	w.Write(buf.Bytes())
}

func (s *Server) RequestCount() int64  { return s.metrics.requests.Load() }
func (s *Server) CacheHits() int64    { return s.metrics.cacheHits.Load() }
func (s *Server) CacheMisses() int64  { return s.metrics.cacheMisses.Load() }
func (s *Server) BytesSent() int64    { return s.metrics.bytesSent.Load() }
func (s *Server) ErrorCount() int64   { return s.metrics.errors.Load() }

// handleStreamingRequest processes HLS/DASH chunk requests through the
// sliding window cache and prefetch pipeline (v0.4).
func (s *Server) handleStreamingRequest(w http.ResponseWriter, r *http.Request, key string) {
	if s.streamH.IsManifestRequest(key) {
		s.handleManifestRequest(w, r, key)
		return
	}

	fetchFn := func(ctx context.Context, k string) (io.ReadCloser, int64, string, error) {
		fr, err := s.fetcher.Fetch(ctx, k)
		if err != nil {
			return nil, 0, "", err
		}
		return fr.Body, fr.ContentLength, fr.ContentType, nil
	}

	data, _, fromCache, err := s.streamH.HandleStreamingRequest(r.Context(), key, fetchFn)
	if err != nil {
		slog.Error("streaming request failed", "key", key, "err", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		s.metrics.errors.Add(1)
		return
	}

	if fromCache {
		s.metrics.cacheHits.Add(1)
	} else {
		s.metrics.cacheMisses.Add(1)
	}
	s.metrics.bytesSent.Add(int64(len(data)))

	streaming.ServeStreamingResponse(w, data, fromCache)
}

func (s *Server) handleManifestRequest(w http.ResponseWriter, r *http.Request, key string) {
	fetchFn := func(ctx context.Context, k string) (io.ReadCloser, int64, string, error) {
		fr, err := s.fetcher.Fetch(ctx, k)
		if err != nil {
			return nil, 0, "", err
		}
		return fr.Body, fr.ContentLength, fr.ContentType, nil
	}

	_, data, err := s.streamH.HandleManifestRequest(r.Context(), key, fetchFn)
	if err != nil {
		slog.Error("manifest request failed", "key", key, "err", err)
	}

	if data != nil {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write(data)
	} else {
		http.Error(w, "upstream error", http.StatusBadGateway)
		s.metrics.errors.Add(1)
	}
}

func parseRangeHeader(rh string, contentLen int64) (int64, int64, error) {
	if rh == "" {
		return 0, 0, fmt.Errorf("empty range header")
	}
	const prefix = "bytes="
	if !strings.HasPrefix(rh, prefix) {
		return 0, 0, fmt.Errorf("invalid range prefix")
	}
	r := strings.TrimPrefix(rh, prefix)

	parts := strings.SplitN(r, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range format")
	}

	var start, end int64
	var err error

	if parts[0] == "" {
		// suffix range: bytes=-500 (last 500 bytes)
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid suffix range")
		}
		start = contentLen - suffix
		if start < 0 {
			start = 0
		}
		end = contentLen - 1
	} else {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid range start")
		}
		if parts[1] == "" {
			end = contentLen - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid range end")
			}
		}
	}

	if start < 0 || start >= contentLen || end < start {
		return 0, 0, fmt.Errorf("range not satisfiable")
	}
	if end >= contentLen {
		end = contentLen - 1
	}

	return start, end, nil
}
