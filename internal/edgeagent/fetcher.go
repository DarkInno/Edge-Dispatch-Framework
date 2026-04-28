package edgeagent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
}

func NewFetcher(cfg *config.EdgeAgentConfig) *Fetcher {
	return &Fetcher{
		originURL: cfg.OriginURL,
		nodeToken: cfg.NodeToken,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (f *Fetcher) Fetch(ctx context.Context, key string, ranges ...string) (*FetchResult, error) {
	return f.doFetch(ctx, key, "", 0)
}

func (f *Fetcher) FetchWithRange(ctx context.Context, key string, rangeHeader string) (*FetchResult, error) {
	return f.doFetch(ctx, key, rangeHeader, 0)
}

func (f *Fetcher) doFetch(ctx context.Context, key string, rangeHeader string, retries int) (*FetchResult, error) {
	url := f.originURL + "/obj/" + key

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	if f.nodeToken != "" {
		req.Header.Set("Authorization", "Bearer "+f.nodeToken)
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		if retries < 2 {
			slog.Warn("fetch retry", "url", url, "err", err, "retry", retries+1)
			time.Sleep(time.Duration(retries+1) * 500 * time.Millisecond)
			return f.doFetch(ctx, key, rangeHeader, retries+1)
		}
		return nil, fmt.Errorf("fetch origin: %w", err)
	}

	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("origin returned %d for %s", resp.StatusCode, key)
	}

	result := &FetchResult{
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
		ContentType:   resp.Header.Get("Content-Type"),
		ETag:          resp.Header.Get("ETag"),
		StatusCode:    resp.StatusCode,
	}

	return result, nil
}
