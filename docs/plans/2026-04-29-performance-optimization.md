# Performance Optimization Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Optimize CPU, memory, database, Redis, and HTTP performance with 50+ optimization points

**Architecture:** Multi-layer optimization covering connection pooling, caching, batch operations, memory reuse, and query optimization

**Tech Stack:** Go 1.22, PostgreSQL, Redis, pgx, go-redis

---

## CPU Optimizations (15 points)

### Task 1: JSON encoder pool reuse
**Files:** `internal/controlplane/api.go`, `internal/edgeagent/server.go`

### Task 2: String builder pool for responses
**Files:** `internal/controlplane/api.go`

### Task 3: Pre-allocated slices in scheduler
**Files:** `internal/controlplane/scheduler.go`

### Task 4: Optimized node scoring with math/bits
**Files:** `internal/controlplane/scheduler.go`

### Task 5: Batch probe result processing
**Files:** `internal/controlplane/prober.go`

### Task 6: Incremental score updates
**Files:** `internal/controlplane/prober.go`

### Task 7: Fast path for cache hits
**Files:** `internal/edgeagent/cache.go`

### Task 8: Reduced allocation in hot paths
**Files:** `internal/edgeagent/server.go`

### Task 9: Compiled regex for range parsing
**Files:** `internal/edgeagent/server.go`

### Task 10: Optimized IP extraction
**Files:** `internal/controlplane/api.go`, `internal/edgeagent/server.go`

### Task 11: Batch heartbeat processing
**Files:** `internal/controlplane/heartbeat.go`

### Task 12: Parallel node filtering
**Files:** `internal/controlplane/scheduler.go`

### Task 13: Efficient bloom filter operations
**Files:** `internal/contentindex/`

### Task 14: Reduced logging overhead
**Files:** All files

### Task 15: Optimized UUID generation
**Files:** `internal/store/pg.go`, `internal/controlplane/registry.go`

## Memory Optimizations (15 points)

### Task 16: Extended sync.Pool usage
**Files:** `internal/edgeagent/cache.go`

### Task 17: LRU cache with pre-allocated entries
**Files:** `internal/edgeagent/cache.go`

### Task 18: Slice capacity pre-allocation
**Files:** All files with make([]T, 0)

### Task 19: String interning for common values
**Files:** `internal/models/models.go`

### Task 20: Struct field reordering for alignment
**Files:** `internal/models/models.go`

### Task 21: Streaming JSON encoding
**Files:** `internal/controlplane/api.go`

### Task 22: Buffer pool for HTTP bodies
**Files:** `internal/controlplane/api.go`, `internal/edgeagent/server.go`

### Task 23: Reduced map allocations
**Files:** `internal/edgeagent/cache.go`

### Task 24: Efficient error handling
**Files:** All files

### Task 25: Connection object reuse
**Files:** `internal/store/pg.go`

### Task 26: Probe result batch buffer
**Files:** `internal/controlplane/prober.go`

### Task 27: Node cache memory optimization
**Files:** `internal/controlplane/nodecache.go`

### Task 28: Heartbeat data compression
**Files:** `internal/store/pg.go`

### Task 29: Content summary optimization
**Files:** `internal/edgeagent/reporter.go`

### Task 30: GC tuning parameters
**Files:** `cmd/control-plane/main.go`, `cmd/edge-agent/main.go`

## Database Optimizations (15 points)

### Task 31: Connection pool tuning
**Files:** `internal/store/pg.go`

### Task 32: Batch insert for probe results
**Files:** `internal/store/pg.go`

### Task 33: Prepared statements
**Files:** `internal/store/pg.go`

### Task 34: Query result caching
**Files:** `internal/store/pg.go`

### Task 35: Partition probe_results table
**Files:** `internal/store/pg.go`

### Task 36: Materialized view for scores
**Files:** `internal/store/pg.go`

### Task 37: Index optimization
**Files:** `internal/store/pg.go`

### Task 38: Batch node status updates
**Files:** `internal/store/pg.go`

### Task 39: Async score computation
**Files:** `internal/controlplane/prober.go`

### Task 40: Connection multiplexing
**Files:** `internal/store/pg.go`

### Task 41: Query plan caching
**Files:** `internal/store/pg.go`

### Task 42: Vacuum and analyze scheduling
**Files:** `internal/store/pg.go`

### Task 43: Read replica support
**Files:** `internal/store/pg.go`

### Task 44: Slow query logging
**Files:** `internal/store/pg.go`

### Task 45: Data archival strategy
**Files:** `internal/store/pg.go`

## Redis Optimizations (10 points)

### Task 46: Pipeline batch operations
**Files:** `internal/store/pg.go`

### Task 47: Lua scripts for atomic ops
**Files:** `internal/store/pg.go`

### Task 48: Memory-efficient data structures
**Files:** `internal/store/pg.go`

### Task 49: Connection pool optimization
**Files:** `internal/store/pg.go`

### Task 50: Lazy expiration strategy
**Files:** `internal/store/pg.go`

### Task 51: Pub/Sub for real-time events
**Files:** `internal/store/pg.go`

### Task 52: Data compression
**Files:** `internal/store/pg.go`

### Task 53: Cluster support
**Files:** `internal/store/pg.go`

### Task 54: Slow query monitoring
**Files:** `internal/store/pg.go`

### Task 55: Key prefix optimization
**Files:** `internal/store/pg.go`

## HTTP/Network Optimizations (10 points)

### Task 56: HTTP/2 transport
**Files:** `internal/edgeagent/fetcher.go`

### Task 57: Connection reuse optimization
**Files:** `internal/edgeagent/fetcher.go`

### Task 58: Response compression
**Files:** `internal/edgeagent/server.go`

### Task 59: Keep-alive optimization
**Files:** `internal/edgeagent/server.go`

### Task 60: Request timeout tuning
**Files:** All HTTP handlers

### Task 61: Rate limiting
**Files:** `internal/controlplane/api.go`

### Task 62: Circuit breaker pattern
**Files:** `internal/edgeagent/fetcher.go`

### Task 63: Request deduplication
**Files:** `internal/edgeagent/fetcher.go`

### Task 64: Response streaming
**Files:** `internal/edgeagent/server.go`

### Task 65: Header optimization
**Files:** All HTTP handlers

---

## Execution Order

Execute tasks in parallel batches:
- Batch 1: Tasks 1-15 (CPU)
- Batch 2: Tasks 16-30 (Memory)
- Batch 3: Tasks 31-45 (Database)
- Batch 4: Tasks 46-55 (Redis)
- Batch 5: Tasks 56-65 (HTTP)
