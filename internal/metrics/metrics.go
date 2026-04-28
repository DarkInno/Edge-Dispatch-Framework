package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type metricType int

const (
	typeGauge metricType = iota
	typeCounter
)

type metricEntry struct {
	name   string
	help   string
	mtype  metricType
	labels []string
}

type gaugeMetric struct {
	entry  *metricEntry
	mu     sync.Mutex
	values map[string]float64
}

type counterMetric struct {
	entry  *metricEntry
	mu     sync.Mutex
	values map[string]float64
}

type registry struct {
	mu       sync.RWMutex
	gauges   map[string]*gaugeMetric
	counters map[string]*counterMetric
	entries  []*metricEntry
}

var defaultRegistry = &registry{
	gauges:   make(map[string]*gaugeMetric),
	counters: make(map[string]*counterMetric),
}

func NewGauge(name, help string, labels ...string) *GaugeFn {
	entry := &metricEntry{
		name:   name,
		help:   help,
		mtype:  typeGauge,
		labels: labels,
	}
	g := &gaugeMetric{entry: entry, values: make(map[string]float64)}

	defaultRegistry.mu.Lock()
	defaultRegistry.gauges[name] = g
	defaultRegistry.entries = append(defaultRegistry.entries, entry)
	defaultRegistry.mu.Unlock()

	return &GaugeFn{m: g, labels: labels}
}

func NewCounter(name, help string, labels ...string) *CounterFn {
	entry := &metricEntry{
		name:   name,
		help:   help,
		mtype:  typeCounter,
		labels: labels,
	}
	c := &counterMetric{entry: entry, values: make(map[string]float64)}

	defaultRegistry.mu.Lock()
	defaultRegistry.counters[name] = c
	defaultRegistry.entries = append(defaultRegistry.entries, entry)
	defaultRegistry.mu.Unlock()

	return &CounterFn{m: c, labels: labels}
}

type GaugeFn struct {
	m      *gaugeMetric
	labels []string
}

func (g *GaugeFn) Set(value float64, labelVals ...string) {
	key := makeLabelKey(g.labels, labelVals)
	g.m.mu.Lock()
	g.m.values[key] = value
	g.m.mu.Unlock()
}

type CounterFn struct {
	m      *counterMetric
	labels []string
}

func (c *CounterFn) Inc(labelVals ...string) {
	c.Add(1, labelVals...)
}

func (c *CounterFn) Add(delta float64, labelVals ...string) {
	key := makeLabelKey(c.labels, labelVals)
	c.m.mu.Lock()
	c.m.values[key] += delta
	c.m.mu.Unlock()
}

func makeLabelKey(labels, values []string) string {
	if len(labels) != len(values) {
		return "_"
	}
	return strings.Join(values, "\x00")
}

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		defaultRegistry.writeTo(w)
	})
}

func (r *registry) writeTo(w http.ResponseWriter) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var sb strings.Builder
	for _, entry := range r.entries {
		sb.WriteString(fmt.Sprintf("# HELP %s %s\n", entry.name, entry.help))
		typeStr := "gauge"
		if entry.mtype == typeCounter {
			typeStr = "counter"
		}
		sb.WriteString(fmt.Sprintf("# TYPE %s %s\n", entry.name, typeStr))

		switch entry.mtype {
		case typeGauge:
			if g, ok := r.gauges[entry.name]; ok {
				g.writeValues(&sb, entry)
			}
		case typeCounter:
			if c, ok := r.counters[entry.name]; ok {
				c.writeValues(&sb, entry)
			}
		}
	}
	w.Write([]byte(sb.String()))
}

func (g *gaugeMetric) writeValues(sb *strings.Builder, entry *metricEntry) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var keys []string
	for k := range g.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		labelStr := formatLabels(entry.labels, key)
		sb.WriteString(fmt.Sprintf("%s%s %g\n", entry.name, labelStr, g.values[key]))
	}
}

func (c *counterMetric) writeValues(sb *strings.Builder, entry *metricEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var keys []string
	for k := range c.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		labelStr := formatLabels(entry.labels, key)
		sb.WriteString(fmt.Sprintf("%s%s %g\n", entry.name, labelStr, c.values[key]))
	}
}

func formatLabels(labels []string, key string) string {
	if key == "_" || len(labels) == 0 {
		return ""
	}
	values := strings.Split(key, "\x00")
	if len(values) != len(labels) {
		return ""
	}
	var parts []string
	for i, label := range labels {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, label, values[i]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

type AtomicInt64Counter struct {
	val atomic.Int64
}

func (c *AtomicInt64Counter) Value() float64 {
	return float64(c.val.Load())
}

func (c *AtomicInt64Counter) Add(delta int64) {
	c.val.Add(delta)
}

func (c *AtomicInt64Counter) Set(val int64) {
	c.val.Store(val)
}
