package edgeagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkinno/edge-dispatch-framework/internal/config"
)

type FetchResult struct {
	Body          io.ReadCloser
	ContentLength int64
	ContentType   string
	ETag          string
	StatusCode    int
}

type Fetcher struct {
	originURL string
	client    *http.Client
	nodeToken string
	cb        *circuitBreaker
	deduper   *deduper
}

func NewFetcher(cfg *config.EdgeAgentConfig) *Fetcher {
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     100,
		IdleConnTimeout:     120 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
		ExpectContinueTimeout:  1 * time.Second,
		DisableCompression:     false,
		ForceAttemptHTTP2:      true,
	}

	return &Fetcher{
		originURL: cfg.OriginURL,
		nodeToken: cfg.NodeToken,
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
		cb:      newCircuitBreaker(5, 30*time.Second),
		deduper: newDeduper(),
	}
}

func (f *Fetcher) Fetch(ctx context.Context, key string, ranges ...string) (*FetchResult, error) {
	return f.doFetch(ctx, key, "")
}

func (f *Fetcher) FetchWithRange(ctx context.Context, key string, rangeHeader string) (*FetchResult, error) {
	return f.doFetch(ctx, key, rangeHeader)
}

func (f *Fetcher) doFetch(ctx context.Context, key string, rangeHeader string) (*FetchResult, error) {
	dedupKey := key
	if rangeHeader != "" {
		dedupKey = key + "|" + rangeHeader
	}

	resultCh, errCh, wait := f.deduper.Do(ctx, dedupKey)
	if !wait {
		return nil, ctx.Err()
	}
	if resultCh != nil {
		select {
		case r := <-resultCh:
			return r, <-errCh
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	fr, err := f.executeFetch(ctx, key, rangeHeader)
	f.deduper.Broadcast(dedupKey, fr, err)
	return fr, err
}

func (f *Fetcher) executeFetch(ctx context.Context, key string, rangeHeader string) (*FetchResult, error) {
	return f.executeFetchWithRetry(ctx, key, rangeHeader, 0)
}

func sanitizeKey(key string) string {
	key = strings.TrimSpace(key)
	key = strings.ReplaceAll(key, "\\", "/")
	parts := make([]string, 0)
	for _, p := range strings.Split(key, "/") {
		p = strings.TrimSpace(p)
		if p == "" || p == "." || p == ".." {
			continue
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "/")
}

func (f *Fetcher) buildOriginURL(key string) (string, error) {
	base, err := url.Parse(f.originURL)
	if err != nil {
		return "", fmt.Errorf("parse origin URL: %w", err)
	}
	base.Path = path.Join(base.Path, "obj", key)
	return base.String(), nil
}

func (f *Fetcher) executeFetchWithRetry(ctx context.Context, key string, rangeHeader string, retries int) (*FetchResult, error) {
	if err := f.cb.Allow(); err != nil {
		return nil, err
	}

	key = sanitizeKey(key)
	originURL, err := f.buildOriginURL(key)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, originURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	if f.nodeToken != "" {
		req.Header.Set("Authorization", "Bearer "+f.nodeToken)
	}
	if rangeHeader != "" {
		if !validRangeHeader(rangeHeader) {
			return nil, fmt.Errorf("invalid range header")
		}
		req.Header.Set("Range", rangeHeader)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		f.cb.RecordFailure()
		if retries < 2 {
			slog.Warn("fetch retry", "url", originURL, "err", err, "retry", retries+1)
			time.Sleep(time.Duration(retries+1) * 500 * time.Millisecond)
			return f.executeFetchWithRetry(ctx, key, rangeHeader, retries+1)
		}
		return nil, fmt.Errorf("fetch origin: %w", err)
	}

	if resp.StatusCode >= 400 {
		resp.Body.Close()
		f.cb.RecordFailure()
		return nil, fmt.Errorf("origin returned %d for %s", resp.StatusCode, key)
	}

	f.cb.RecordSuccess()

	result := &FetchResult{
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
		ContentType:   resp.Header.Get("Content-Type"),
		ETag:          resp.Header.Get("ETag"),
		StatusCode:    resp.StatusCode,
	}

	return result, nil
}

type circuitBreaker struct {
	state       int32
	failures    int32
	threshold   int32
	recovery    time.Duration
	lastFailure atomic.Int64
	successes   int32
	mu          sync.Mutex
}

func validRangeHeader(rh string) bool {
	if rh == "" {
		return false
	}
	if strings.Contains(rh, "\n") || strings.Contains(rh, "\r") {
		return false
	}
	if !strings.HasPrefix(rh, "bytes=") {
		return false
	}
	return true
}

func newCircuitBreaker(threshold int32, recovery time.Duration) *circuitBreaker {
	return &circuitBreaker{
		threshold: threshold,
		recovery:  recovery,
	}
}

func (cb *circuitBreaker) Allow() error {
	state := atomic.LoadInt32(&cb.state)
	if state == 0 {
		return nil
	}
	if state == 1 {
		last := time.Unix(0, cb.lastFailure.Load())
		if time.Since(last) >= cb.recovery {
			cb.mu.Lock()
			if atomic.CompareAndSwapInt32(&cb.state, 1, 2) {
				cb.successes = 0
			}
			cb.mu.Unlock()
			return nil
		}
		return errors.New("circuit breaker open")
	}
	return nil
}

func (cb *circuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures++
	cb.lastFailure.Store(time.Now().UnixNano())
	if cb.failures >= cb.threshold {
		atomic.StoreInt32(&cb.state, 1)
	}
}

func (cb *circuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	state := atomic.LoadInt32(&cb.state)
	if state == 2 {
		cb.successes++
		if cb.successes >= 3 {
			atomic.StoreInt32(&cb.state, 0)
			cb.failures = 0
		}
	} else if state == 0 {
		cb.failures = 0
	}
}

type deduperEntry struct {
	resultCh chan *FetchResult
	errCh    chan error
}

type deduper struct {
	mu      sync.Mutex
	pending map[string]*deduperEntry
}

func newDeduper() *deduper {
	return &deduper{pending: make(map[string]*deduperEntry)}
}

func (d *deduper) Do(ctx context.Context, key string) (chan *FetchResult, chan error, bool) {
	d.mu.Lock()
	if entry, ok := d.pending[key]; ok {
		d.mu.Unlock()
		select {
		case <-entry.resultCh:
			return nil, nil, false
		case <-ctx.Done():
			return nil, nil, false
		default:
			return entry.resultCh, entry.errCh, true
		}
	}
	entry := &deduperEntry{
		resultCh: make(chan *FetchResult, 1),
		errCh:    make(chan error, 1),
	}
	d.pending[key] = entry
	d.mu.Unlock()
	return nil, nil, true
}

func (d *deduper) Broadcast(key string, result *FetchResult, err error) {
	d.mu.Lock()
	entry, ok := d.pending[key]
	if ok {
		delete(d.pending, key)
	}
	d.mu.Unlock()
	if ok {
		entry.resultCh <- result
		entry.errCh <- err
	}
}
