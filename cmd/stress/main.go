package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type config struct {
	controlPlaneURL string
	originURL       string
	concurrency     int
	workers         int
	duration        time.Duration
	rampUp          time.Duration
	objects         int
	mode            string
	edgeEndpoint    string
	noBodyRead      bool
	key             string
	signKey         string
	authToken       string
	natMode         bool
}

type stats struct {
	totalReqs      atomic.Int64
	successReqs    atomic.Int64
	failedReqs     atomic.Int64
	totalBytes     atomic.Int64
	totalLatencyUs atomic.Int64
	minLatency     atomic.Int64
	maxLatency     atomic.Int64
	statusCodes    sync.Map
	latencySamples []int64
	mu             sync.Mutex
}

func (s *stats) record(status int, latency time.Duration, bytesRead int64) {
	s.totalReqs.Add(1)
	if status >= 200 && status < 400 {
		s.successReqs.Add(1)
	} else {
		s.failedReqs.Add(1)
	}
	s.totalBytes.Add(bytesRead)
	latUs := latency.Microseconds()
	s.totalLatencyUs.Add(latUs)

	for {
		min := s.minLatency.Load()
		if min == 0 || latUs < min {
			if s.minLatency.CompareAndSwap(min, latUs) {
				break
			}
		} else {
			break
		}
	}
	for {
		max := s.maxLatency.Load()
		if latUs > max {
			if s.maxLatency.CompareAndSwap(max, latUs) {
				break
			}
		} else {
			break
		}
	}

	key := fmt.Sprintf("%d", status)
	val, _ := s.statusCodes.LoadOrStore(key, new(atomic.Int64))
	val.(*atomic.Int64).Add(1)

	if s.totalReqs.Load()%50 == 0 {
		s.mu.Lock()
		s.latencySamples = append(s.latencySamples, latUs)
		s.mu.Unlock()
	}
}

func (s *stats) print(dur time.Duration) {
	total := s.totalReqs.Load()
	success := s.successReqs.Load()
	failed := s.failedReqs.Load()
	bytesTotal := s.totalBytes.Load()
	totalLatUs := s.totalLatencyUs.Load()

	fmt.Println()
	fmt.Println("══════════════════════════════════════════════════")
	fmt.Println("  STRESS TEST RESULTS")
	fmt.Println("══════════════════════════════════════════════════")
	fmt.Printf("  Duration:          %v\n", dur)
	fmt.Printf("  Total Requests:    %d\n", total)
	if total > 0 {
		qps := float64(total) / dur.Seconds()
		fmt.Printf("  QPS (req/sec):     %.0f\n", qps)
	}
	fmt.Printf("  Successful:        %d (%.1f%%)\n", success, pct(success, total))
	fmt.Printf("  Failed:            %d (%.1f%%)\n", failed, pct(failed, total))

	if total > 0 {
		avgLatMs := float64(totalLatUs) / float64(total) / 1000
		fmt.Printf("  Avg Latency:       %.2f ms\n", avgLatMs)
		fmt.Printf("  Min Latency:       %.2f ms\n", float64(s.minLatency.Load())/1000)
		fmt.Printf("  Max Latency:       %.2f ms\n", float64(s.maxLatency.Load())/1000)
	}

	s.mu.Lock()
	samples := make([]int64, len(s.latencySamples))
	copy(samples, s.latencySamples)
	s.mu.Unlock()

	if len(samples) > 0 {
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		p50 := samples[len(samples)*50/100]
		p95 := samples[len(samples)*95/100]
		p99 := samples[len(samples)*99/100]
		fmt.Printf("  P50 Latency:       %.2f ms\n", float64(p50)/1000)
		fmt.Printf("  P95 Latency:       %.2f ms\n", float64(p95)/1000)
		fmt.Printf("  P99 Latency:       %.2f ms\n", float64(p99)/1000)
	}

	totalMB := float64(bytesTotal) / (1024 * 1024)
	fmt.Printf("  Total Data:        %.2f MB\n", totalMB)
	if dur.Seconds() > 0 {
		throughputMBps := totalMB / dur.Seconds()
		fmt.Printf("  Throughput:        %.2f MB/s\n", throughputMBps)
	}

	fmt.Println("  --- Status Codes ---")
	s.statusCodes.Range(func(k, v interface{}) bool {
		code := k.(string)
		count := v.(*atomic.Int64).Load()
		fmt.Printf("    %s: %d\n", code, count)
		return true
	})
	fmt.Println("══════════════════════════════════════════════════")
}

func pct(v, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(v) / float64(total) * 100
}

func main() {
	cfg := &config{}

	flag.StringVar(&cfg.mode, "mode", "direct", "test mode: direct|dispatch|bench-dispatch|bench-origin|bench-edge")
	flag.StringVar(&cfg.controlPlaneURL, "cp", "http://localhost:8080", "control plane URL")
	flag.StringVar(&cfg.originURL, "origin", "http://localhost:7070", "origin URL")
	flag.StringVar(&cfg.edgeEndpoint, "edge", "http://localhost:9090", "edge agent URL")
	flag.IntVar(&cfg.concurrency, "c", 10, "concurrent workers")
	flag.IntVar(&cfg.workers, "w", 0, "worker pool size (0 = same as -c, limits active TCP connections)")
	flag.DurationVar(&cfg.duration, "d", 30*time.Second, "test duration")
	flag.DurationVar(&cfg.rampUp, "rampup", 0, "ramp-up time (0 = no ramp)")
	flag.IntVar(&cfg.objects, "objects", 100, "number of unique objects")
	flag.BoolVar(&cfg.noBodyRead, "no-body", false, "skip reading response body (measure TTFB)")
	flag.StringVar(&cfg.key, "key", "", "override object key for requests")
	flag.StringVar(&cfg.signKey, "sign-key", "", "HMAC signing key for edge agent token generation")
	flag.StringVar(&cfg.authToken, "auth-token", "", "Bearer auth token for control-plane API")
	flag.BoolVar(&cfg.natMode, "nat", false, "use NAT mode (prefix key with nat/)")
	flag.Parse()

	log.SetFlags(0)
	log.SetOutput(os.Stdout)

	st := &stats{}

	fmt.Println("══════════════════════════════════════════════════")
	fmt.Println("  Edge Dispatch Framework - Stress Test")
	fmt.Println("══════════════════════════════════════════════════")
	fmt.Printf("  Mode:          %s\n", cfg.mode)
	eff := cfg.workers
	if eff <= 0 {
		eff = cfg.concurrency
	}
	fmt.Printf("  Concurrency:   %d\n", cfg.concurrency)
	fmt.Printf("  Workers:       %d\n", eff)
	fmt.Printf("  Duration:      %v\n", cfg.duration)
	fmt.Printf("  Ramp-up:       %v\n", cfg.rampUp)
	fmt.Printf("  Objects:       %d\n", cfg.objects)
	fmt.Printf("  Skip Body:     %v\n", cfg.noBodyRead)
	fmt.Println("══════════════════════════════════════════════════")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				total := st.totalReqs.Load()
				success := st.successReqs.Load()
				failed := st.failedReqs.Load()
				fmt.Printf("  [progress] reqs=%d ok=%d fail=%d\n", total, success, failed)
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	startTime := time.Now()

	switch cfg.mode {
	case "direct":
		runDirect(ctx, cfg, st)
	case "dispatch":
		runDispatch(ctx, cfg, st)
	case "bench-dispatch":
		runBenchDispatch(ctx, cfg, st)
	case "bench-origin":
		runBenchOrigin(ctx, cfg, st)
	case "bench-edge":
		runBenchEdge(ctx, cfg, st)
	case "bench-gateway":
		cfg.natMode = true
		runBenchEdge(ctx, cfg, st)
	case "bench-nat":
		cfg.natMode = true
		runDispatch(ctx, cfg, st)
	default:
		log.Fatalf("unknown mode: %s", cfg.mode)
	}

	st.print(time.Since(startTime))
}

func newTransport(host string) *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 1000,
		MaxConnsPerHost:     1000,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
		ForceAttemptHTTP2:   false,
	}
}

type tokenPayload struct {
	Key      string `json:"key"`
	Exp      int64  `json:"exp"`
	IPPrefix string `json:"ip_prefix,omitempty"`
}

func signToken(secret, key string) string {
	if secret == "" {
		return ""
	}
	payload := tokenPayload{
		Key: key,
		Exp: time.Now().Add(1 * time.Hour).Unix(),
	}
	b, _ := json.Marshal(payload)
	encoded := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func drainBody(resp *http.Response, skip bool) int64 {
	if skip {
		return 0
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	return n
}

func runDirect(ctx context.Context, cfg *config, st *stats) {
	transport := newTransport("")
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	keys := make([]string, cfg.objects)
	for i := range keys {
		keys[i] = fmt.Sprintf("video/stress-%04d.mp4", i)
	}

	runWorkers(ctx, cfg, st, func(w int) func() {
		return func() {
			key := keys[w%len(keys)]
			url := cfg.originURL + "/obj/" + key
			req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)

			start := time.Now()
			resp, err := client.Do(req)
			latency := time.Since(start)
			if err != nil {
				if !isCanceled(err) {
					st.record(0, latency, 0)
				}
				return
			}
			n := drainBody(resp, cfg.noBodyRead)
			resp.Body.Close()
			st.record(resp.StatusCode, latency, n)
		}
	})
}

func runDispatch(ctx context.Context, cfg *config, st *stats) {
	transport := newTransport("")
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	keys := make([]string, cfg.objects)
	for i := range keys {
		keys[i] = fmt.Sprintf("video/stress-%04d.mp4", i)
	}
	baseURL := cfg.controlPlaneURL
	if cfg.natMode {
		baseURL = cfg.edgeEndpoint // use gateway URL for NAT routing
	}

	runWorkers(ctx, cfg, st, func(w int) func() {
		return func() {
			key := keys[w%len(keys)]
			url := baseURL + "/obj/" + key
			req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
			if cfg.authToken != "" {
				req.Header.Set("Authorization", "Bearer "+cfg.authToken)
			}

			start := time.Now()
			resp, err := client.Do(req)
			latency := time.Since(start)
			if err != nil {
				if !isCanceled(err) {
					st.record(0, latency, 0)
				}
				return
			}
			n := drainBody(resp, cfg.noBodyRead)
			resp.Body.Close()
			st.record(resp.StatusCode, latency, n)
		}
	})
}

func runBenchDispatch(ctx context.Context, cfg *config, st *stats) {
	transport := newTransport("")
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	keys := make([]string, cfg.objects)
	for i := range keys {
		keys[i] = fmt.Sprintf("bench/video-%04d.mp4", i)
	}

	bodyPool := sync.Pool{
		New: func() interface{} {
			return make([]byte, 0, 256)
		},
	}

	runWorkers(ctx, cfg, st, func(w int) func() {
		return func() {
			key := keys[w%len(keys)]
			dispReq := map[string]interface{}{
				"client":   map[string]string{"ip": "10.0.0.1", "region": "cn-sh", "isp": "ctcc"},
				"resource": map[string]string{"type": "object", "key": key},
			}
			buf := bodyPool.Get().([]byte)
			buf = buf[:0]
			buf, _ = json.Marshal(dispReq)

			start := time.Now()
			var resp *http.Response
			var err error
			if cfg.authToken != "" {
				req, _ := http.NewRequestWithContext(ctx, "POST", cfg.controlPlaneURL+"/v1/dispatch/resolve", strings.NewReader(string(buf)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+cfg.authToken)
				resp, err = client.Do(req)
			} else {
				resp, err = client.Post(
					cfg.controlPlaneURL+"/v1/dispatch/resolve",
					"application/json",
					strings.NewReader(string(buf)),
				)
			}
			latency := time.Since(start)
			bodyPool.Put(buf)

			if err != nil {
				st.record(0, latency, 0)
				return
			}
			n := drainBody(resp, cfg.noBodyRead)
			resp.Body.Close()
			st.record(resp.StatusCode, latency, n)
		}
	})
}

func runBenchOrigin(ctx context.Context, cfg *config, st *stats) {
	transport := newTransport("")
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	runWorkers(ctx, cfg, st, func(w int) func() {
		return func() {
			var key string
			if cfg.key != "" {
				key = cfg.key
			} else {
				key = fmt.Sprintf("bench/stress-%04d-%d.mp4", w%cfg.objects, time.Now().UnixNano()%100)
			}
			url := cfg.originURL + "/obj/" + key
			req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)

			start := time.Now()
			resp, err := client.Do(req)
			latency := time.Since(start)
			if err != nil {
				st.record(0, latency, 0)
				return
			}
			n := drainBody(resp, cfg.noBodyRead)
			resp.Body.Close()
			st.record(resp.StatusCode, latency, n)
		}
	})
}

func runBenchEdge(ctx context.Context, cfg *config, st *stats) {
	transport := newTransport("")
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	keys := make([]string, cfg.objects)
	for i := range keys {
		keys[i] = fmt.Sprintf("video/stress-%04d.mp4", i)
	}
	edgeURL := cfg.edgeEndpoint
	if cfg.natMode {
		edgeURL = cfg.controlPlaneURL // route through gateway for NAT
	}

	runWorkers(ctx, cfg, st, func(w int) func() {
		return func() {
			key := keys[w%len(keys)]
			url := edgeURL + "/obj/" + key
			if cfg.signKey != "" {
				token := signToken(cfg.signKey, key)
				url += "?token=" + token
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
			if cfg.authToken != "" {
				req.Header.Set("Authorization", "Bearer "+cfg.authToken)
			}

			start := time.Now()
			resp, err := client.Do(req)
			latency := time.Since(start)
			if err != nil {
				if !isCanceled(err) {
					st.record(0, latency, 0)
				}
				return
			}
			n := drainBody(resp, cfg.noBodyRead)
			resp.Body.Close()
			st.record(resp.StatusCode, latency, n)
		}
	})
}

func runWorkers(ctx context.Context, cfg *config, st *stats, fn func(workerID int) func()) {
	var wg sync.WaitGroup

	poolSize := cfg.workers
	if poolSize <= 0 {
		poolSize = cfg.concurrency
	}

	for i := 0; i < poolSize; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			if cfg.rampUp > 0 {
				delay := time.Duration(float64(cfg.rampUp) * float64(workerID) / float64(poolSize))
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
			}

			workFn := fn(workerID)
			deadline := time.After(cfg.duration)
			for {
				select {
				case <-ctx.Done():
					return
				case <-deadline:
					return
				default:
					workFn()
				}
			}
		}(i)
	}

	wg.Wait()
}

func isCanceled(err error) bool {
	return strings.Contains(err.Error(), "context canceled")
}
