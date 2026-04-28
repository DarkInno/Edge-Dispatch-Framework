package edgeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/auth"
	"github.com/darkinno/edge-dispatch-framework/internal/config"
)

type serverMetrics struct {
	requests    atomic.Int64
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
	bytesSent   atomic.Int64
	errors      atomic.Int64
}

type Server struct {
	cache    *Cache
	fetcher  *Fetcher
	cfg      *config.EdgeAgentConfig
	signer   *auth.Signer
	metrics  serverMetrics
	httpSrv  *http.Server
	listener net.Listener
}

func NewServer(cache *Cache, fetcher *Fetcher, cfg *config.EdgeAgentConfig) *Server {
	return &Server{
		cache:   cache,
		fetcher: fetcher,
		cfg:     cfg,
		signer:  auth.NewSigner(cfg.NodeToken),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/obj/", s.handleObject)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/metrics", s.handleMetrics)
	return mux
}

func (s *Server) Start(ctx context.Context) error {
	s.httpSrv = &http.Server{
		Handler:      s.Handler(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
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

	// Verify token
	token := r.URL.Query().Get("token")
	if token != "" {
		if _, err := s.signer.Verify(token); err != nil {
			slog.Warn("invalid token", "err", err)
			http.Error(w, "forbidden", http.StatusForbidden)
			s.metrics.errors.Add(1)
			return
		}
	}

	// Extract key from /obj/{key}
	key := strings.TrimPrefix(r.URL.Path, "/obj/")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		s.metrics.errors.Add(1)
		return
	}

	rangeHeader := r.Header.Get("Range")

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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	cacheStats := s.cache.Stats()

	data := map[string]interface{}{
		"requests":    s.metrics.requests.Load(),
		"cache_hits":  s.metrics.cacheHits.Load(),
		"cache_misses": s.metrics.cacheMisses.Load(),
		"bytes_sent":  s.metrics.bytesSent.Load(),
		"errors":      s.metrics.errors.Load(),
		"cache":       cacheStats,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (s *Server) RequestCount() int64  { return s.metrics.requests.Load() }
func (s *Server) CacheHits() int64    { return s.metrics.cacheHits.Load() }
func (s *Server) CacheMisses() int64  { return s.metrics.cacheMisses.Load() }
func (s *Server) BytesSent() int64    { return s.metrics.bytesSent.Load() }
func (s *Server) ErrorCount() int64   { return s.metrics.errors.Load() }

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
