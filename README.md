# Edge Dispatch Framework

> Open-source edge delivery/acceleration framework: **Anycast (optional) + Edge Nodes + Central Scheduling**, default 302 redirect, supporting DNS/GSLB, gateway reverse proxy, and other access modes.

[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-Compose-blue?logo=docker)](docker-compose.yml)

---

## What It Does

| Scenario | Description | Status |
|------|------|------|
| **Download/VOD Acceleration** | HTTP object delivery, Range byte-range requests, edge caching, origin pull | ✅ |
| **Multi-Ingress Dispatch** | 302 redirect, DNS/GSLB, gateway reverse proxy | ✅ |
| **NAT Node Penetration** | Reverse tunnel, enables intranet nodes to serve traffic | ✅ |
| **Content-Aware Scheduling** | Bloom Filter + hot content exact indexing, hit-rate-first routing | ✅ |
| **Live Stream Segment Delivery** | HLS/DASH sliding-window cache + prefetch | ✅ |
| **Web Admin Console** | Visual management panel for nodes, dispatch, and data monitoring | [Webmanager](https://github.com/im10furry/Edge-Dispatch-Framework-Webmanager) |
| **HTTP/3 / QUIC** | Next-gen UDP-based transport | ✅ |

## Architecture Overview

```
▲                              ▲
│   ┌───────────────────┐      │
│   │  Web Manager (UI) │      │ Admin API
│   │  Admin Console     │      │
│   └────────┬──────────┘      │
│            │                 │
┌────────────┼─────────────────┼─────────────────────────┐
│            ▼                 ▼                         │
│                   Ingress Layer                         │
│  ┌──────────┐  ┌──────────┐  ┌──────────────────┐     │
│  │ 302 Redirect│ │ DNS/GSLB │ │ Gateway Reverse Proxy│  │
│  └─────┬────┘  └────┬─────┘  └────────┬─────────┘     │
└────────┼────────────┼─────────────────┼────────────────┘
         │            │                 │
         ▼            ▼                 ▼
┌────────────────────────────────────────────────────────┐
│               Control Plane                             │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐               │
│  │ Registry │ │ Scheduler│ │ Prober   │               │
│  │ Register  │ │ Top-K    │ │ Liveness │               │
│  │ /Auth     │ │ Dispatch │ │ Probe    │               │
│  └──────────┘ └──────────┘ └──────────┘               │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐               │
│  │ Heartbeat│ │ Policy   │ │ Content  │               │
│  │ /Status  │ │ /RateLim │ │ Index    │               │
│  └──────────┘ └──────────┘ └──────────┘               │
└──────────────────────┬─────────────────────────────────┘
                       │
         ┌─────────────┼─────────────┐
         ▼             ▼             ▼
┌──────────────┐ ┌──────────────┐ ┌──────────────┐
│  Edge Agent  │ │  Edge Agent  │ │  Edge (NAT)  │
│  Cache/Pull  │ │  Cache/Pull  │ │  via Tunnel  │
└──────┬───────┘ └──────┬───────┘ └──────┬───────┘
       │                │                │
       ▼                ▼                ▼
┌────────────────────────────────────────────────────────┐
│                   Origin Server                        │
└────────────────────────────────────────────────────────┘
```
### Web Admin Console

[Edge-Dispatch-Framework-Webmanager](https://github.com/im10furry/Edge-Dispatch-Framework-Webmanager) is the companion Web UI admin panel that provides visual node management, dispatch monitoring, and data presentation via the Control Plane Admin API.

```bash
# Clone and start Webmanager
git clone https://github.com/im10furry/Edge-Dispatch-Framework-Webmanager.git
cd Edge-Dispatch-Framework-Webmanager
# See the project README for details
```

---

## Quick Start (Local Demo)

For quickly experiencing all features on a **single machine**.

### Prerequisites

- Docker & Docker Compose
- Go 1.22+ (only needed for local development)

### One-Click Start

```bash
git clone https://github.com/DarkInno/Edge-Dispatch-Framework.git
cd Edge-Dispatch-Framework

# Start all services (Control Plane + 2 Edge Nodes + Gateway + Origin + PG + Redis)
docker compose up -d

# Wait for services to be ready (~15 seconds)
docker compose ps

# Verify services
curl http://localhost:8080/healthz        # Control Plane
curl http://localhost:9090/healthz        # Edge Node 1
curl http://localhost:9091/healthz        # Edge Node 2
curl http://localhost:7070/healthz        # Origin
curl http://localhost:8880/healthz        # Gateway
```

### Test 302 Dispatch

```bash
# 1. Register an edge node
curl -sS -X POST http://localhost:8080/v1/nodes/register \
  -H 'Content-Type: application/json' \
  -d '{
    "node_name": "edge-shanghai",
    "public_endpoints": [{"scheme": "http", "host": "127.0.0.1", "port": 9090}],
    "region": "cn-sh",
    "isp": "ctcc",
    "capabilities": {"inbound_reachable": true, "cache_disk_gb": 10, "max_uplink_mbps": 100}
  }'

# Returns: {"node_id":"...","token":"..."}
# Note the node_id and token

# 2. Request 302 redirect (Control Plane returns the best edge node address)
curl -sS -I http://localhost:8080/obj/test-file.bin

# 3. Direct request to edge node (with token)
curl -sS http://localhost:9090/obj/test-file.bin?token=<token_from_step_1>
```

---

## Deployment Guide

> In production, the **Control Plane is a centralized service** (deployed on one or more central servers), while **Edge Nodes are distributed services** (deployed on edge servers in each region). They use separate compose files.

### Deployment Architecture

```
                          ┌─────────────────────────────┐
                          │    Control Plane (Central)    │
                          │  ┌──────────┐ ┌──────────┐  │
                          │  │ Postgres │ │  Redis   │  │
                          │  └──────────┘ └──────────┘  │
                          │  ┌──────────┐ ┌──────────┐  │
                          │  │Control   │ │ Gateway  │  │
                          │  │ Plane    │ │ (optional)│  │
                          │  └──────────┘ └──────────┘  │
                          │  ┌──────────┐               │
                          │  │ DNS      │ (optional)    │
                          │  │ Adapter  │               │
                          │  └──────────┘               │
                          └──────┬──────────────────────┘
                                 │ HTTP (edge → cp)
              ┌──────────────────┼──────────────────┐
              ▼                  ▼                  ▼
    ┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
    │   Edge Agent 1  │ │   Edge Agent 2  │ │   Edge Agent N  │
    │   (Region A)    │ │   (Region B)    │ │   (Region ...)  │
    │   docker-compose │ │   docker-compose │ │   docker-compose │
    │   .edge.yml      │ │   .edge.yml      │ │   .edge.yml      │
    └────────┬────────┘ └────────┬────────┘ └────────┬────────┘
             │                   │                   │
             └───────────────────┼───────────────────┘
                                 ▼
                      ┌─────────────────┐
                      │  Origin Server   │
                      │  (standalone)    │
                      └─────────────────┘
```

**Key differences:**
- **Control Plane**: Centralized deployment, manages node registration, scheduling, and heartbeats. Edge nodes connect to the Control Plane via HTTP.
- **Edge Nodes**: Distributed deployment, close to users. Not in the same Docker network as the Control Plane; communicate via public/private IP.

### Prerequisites

- Control Plane server: Docker & Docker Compose, at least 2GB RAM
- Edge Node server: Docker & Docker Compose (no Go environment needed, use Docker build or pre-built images)
- Network: Edge Node servers must be able to reach the Control Plane's `8080` port via HTTP

### Security Preparation

Production deployments **must** replace default secrets, or services will refuse to start:

```bash
# Generate a strong random secret
openssl rand -base64 32
```

Create a `.env` file on the Control Plane server:

```bash
# .env (place in the same directory as the Control Plane compose file)
CP_TOKEN_SECRET=<generated-random-secret>
GW_AUTH_TOKEN=<generated-random-secret>
GW_CP_TOKEN=<generated-random-secret>
PG_PASSWORD=<database-password>

# Required if Admin API is enabled
CP_ADMIN_JWT_SECRET=<generated-random-secret>
CP_ADMIN_ACCESS_KEY=<generated-access-key>
CP_ADMIN_SECRET_KEY=<generated-secret-key>
```

---

### Method 1: Single-Machine Demo (All-in-One)

Experience all features on a single machine. Control Plane and Edge Nodes share the same Docker network.

```bash
docker compose up -d
```

> This is the `docker-compose.yml` mode, intended for local development and feature verification only.

---

### Method 2: Split Deployment (Production Recommended)

#### Step 1: Deploy Control Plane

On the central server:

```bash
git clone https://github.com/DarkInno/Edge-Dispatch-Framework.git
cd Edge-Dispatch-Framework

# Create .env configuration
cat > .env << 'EOF'
CP_TOKEN_SECRET=<your-random-secret>
GW_AUTH_TOKEN=<your-random-token>
PG_PASSWORD=<your-pg-password>
EOF

# Start core Control Plane services
docker compose -f docker-compose.cp.yml up -d

# If you need the Gateway reverse proxy:
docker compose -f docker-compose.cp.yml --profile gateway up -d

# If you need the DNS adapter:
docker compose -f docker-compose.cp.yml --profile dns up -d

# Start all optional components:
docker compose -f docker-compose.cp.yml --profile full up -d

# Verify
curl http://localhost:8080/healthz
```

Control Plane default ports:
| Service | Port | Description |
|------|------|------|
| control-plane | 8080/tcp | Control Plane API (edge nodes communicate via this port) |
| gateway | 8880/tcp | HTTP reverse proxy (optional) |
| gateway-tunnel | 7700/tcp | Reverse tunnel service (optional, needed for NAT nodes) |
| dns-adapter | 53/udp | DNS authoritative server (optional) |
| postgres | 5432/tcp | Database |
| redis | 6379/tcp | Cache |

> Open firewall: Edge nodes must be able to reach the Control Plane's **8080** port.

#### Step 2: Deploy Edge Nodes

On **each edge server**:

```bash
git clone https://github.com/DarkInno/Edge-Dispatch-Framework.git
cd Edge-Dispatch-Framework

# Create .env configuration
cat > .env << 'EOF'
# Required: Control Plane address (replace with actual IP/domain)
EA_CONTROL_PLANE_URL=http://<cp-public-ip>:8080

# Required: Origin server address (pull from origin on cache miss)
EA_ORIGIN_URL=http://<origin-ip>:7070

# Optional: Cache configuration
EA_CACHE_MAX_GB=100
EA_CACHE_DIR=/data/edf-cache
EOF

# Start the edge node
docker compose -f docker-compose.edge.yml up -d

# View logs
docker compose -f docker-compose.edge.yml logs -f

# Verify
curl http://localhost:9090/healthz
```

**NAT Nodes** — use tunnel mode when a node is behind NAT without a public IP:

Add to `.env`:
```bash
EA_NAT_MODE=true
EA_TUNNEL_SERVER_ADDR=<gateway-host>:7700
EA_TUNNEL_AUTH_TOKEN=<gw-auth-token>
```

#### Step 3: Register Edge Node with Control Plane

After the edge node starts, register it with the Control Plane via auto-registration or the manual API:

**Auto-registration**: When an edge node starts for the first time without `EA_NODE_TOKEN`, it automatically calls the Control Plane's registration API.

**Manual registration**:
```bash
curl -X POST http://<cp-host>:8080/v1/nodes/register \
  -H 'Content-Type: application/json' \
  -d '{
    "node_name": "edge-beijing-01",
    "public_endpoints": [{"scheme": "http", "host": "<edge-public-ip>", "port": 9090}],
    "region": "cn-bj",
    "isp": "ctcc",
    "capabilities": {
      "inbound_reachable": true,
      "cache_disk_gb": 100,
      "max_uplink_mbps": 1000
    }
  }'
```

> Record the returned `token` — it can be configured as the `EA_NODE_TOKEN` environment variable on the edge node to persist its identity.

#### Step 4: Verify Dispatch

```bash
# Get the best edge node via Control Plane 302 redirect
curl -sS -I http://<cp-host>:8080/obj/test-file.bin

# Output:
# HTTP/1.1 302 Found
# Location: http://<edge-ip>:9090/obj/test-file.bin?token=...

# Get Top-K candidate nodes via API
curl -sS -X POST http://<cp-host>:8080/v1/dispatch/resolve \
  -H 'Content-Type: application/json' \
  -d '{
    "resource_key": "test-file.bin",
    "client_info": {"region": "cn-bj", "isp": "ctcc", "ip": "1.2.3.4"}
  }'
```

#### Step 5: Deploy Example Origin (Optional)

The `origin` service in this project is an example origin server. In production, replace it with your existing object storage (S3/OSS) or CDN origin.

```bash
docker compose -f docker-compose.edge.yml --profile origin up -d
```

---

### Multi-Node Deployment (Single-Machine Testing)

Simulate multiple edge nodes on a single machine for testing:

```bash
# Node 1
EA_CONTROL_PLANE_URL=http://localhost:8080 \
EA_CACHE_DIR=/tmp/edf-cache-1 \
docker compose -f docker-compose.edge.yml -p edge1 up -d

# Node 2 (different port and cache directory)
EA_CONTROL_PLANE_URL=http://localhost:8080 \
EA_CACHE_DIR=/tmp/edf-cache-2 \
docker compose -f docker-compose.edge.yml -p edge2 up -d

# Manual port mapping
docker run -d --name edge3 \
  -e EA_LISTEN_ADDR=:9092 \
  -e EA_CONTROL_PLANE_URL=http://host.docker.internal:8080 \
  -e EA_ORIGIN_URL=http://host.docker.internal:7070 \
  -e EA_CACHE_DIR=/tmp/edf-cache-3 \
  -p 9092:9092 \
  edf-edge-agent
```

---

### Using Pre-built Images

In production, build images first and push them to a private registry, then edge nodes pull images directly:

```bash
# On the build machine:
docker compose -f docker-compose.edge.yml build
docker tag edf-edge-agent your-registry/edf-edge-agent:latest
docker push your-registry/edf-edge-agent:latest

# On edge nodes, edit docker-compose.edge.yml to replace `build:` with `image:`:
#   image: your-registry/edf-edge-agent:latest
```

---

## Project Structure

```
.
├── cmd/                          # Service entry points
│   ├── control-plane/            # Control Plane
│   ├── edge-agent/               # Edge Node
│   ├── gateway/                  # Gateway Reverse Proxy (v0.3)
│   ├── dns-adapter/              # DNS/GSLB Adapter (v0.2)
│   ├── origin/                   # Example Origin Server
│   └── stress/                   # Stress Testing Tool
├── internal/                     # Core libraries
│   ├── auth/                     # HMAC Token sign/verify
│   ├── config/                   # Environment variable config loading
│   ├── contentindex/             # Content Index (Bloom Filter + hot content)
│   ├── controlplane/             # Control Plane core (API, scheduler, heartbeat, probe, policy)
│   ├── dns/                      # DNS/GSLB Adapter
│   ├── edgeagent/                # Edge Node (cache, origin pull, reporting)
│   ├── gateway/                  # Gateway Reverse Proxy
│   ├── metrics/                  # Prometheus Metrics (v0.5)
│   ├── models/                   # Shared data models
│   ├── quic/                     # HTTP/3 QUIC (placeholder)
│   ├── store/                    # PostgreSQL + Redis storage layer
│   ├── streaming/                # HLS/DASH streaming support (v0.4)
│   └── tunnel/                   # Reverse Tunnel Protocol (v0.3)
├── docker/                       # Dockerfiles
├── Dockerfile                    # Root Dockerfile (multi-stage build)
├── docker-compose.yml            # Local demo (All-in-One)
├── docker-compose.cp.yml         # Control Plane deployment (production)
├── docker-compose.edge.yml       # Edge Node deployment (production)
├── .dockerignore                 # Docker build exclusion rules
├── Makefile                      # Build/test/deploy commands
├── openapi.json                  # OpenAPI 3.0 specification
└── requirements.md               # Product Requirements Document (PRD)
```

## Configuration Reference

All services are configured via environment variables. See `internal/config/config.go`.

### Control Plane

| Variable | Default | Description |
|------|--------|------|
| `CP_LISTEN_ADDR` | `:8080` | Listen address (edge nodes communicate via this port) |
| `CP_PG_URL` | — | PostgreSQL connection string |
| `CP_REDIS_ADDR` | `localhost:6379` | Redis address |
| `CP_TOKEN_SECRET` | — | HMAC Token secret (**must set in production**) |
| `CP_MAX_CANDIDATES` | `5` | Max candidates returned by scheduler |
| `CP_DEFAULT_TTL_MS` | `30000` | Candidate TTL (milliseconds) |
| `CP_DEGRADE_TO_ORIGIN` | `true` | Degrade to origin when no nodes available |
| `CP_PROBE_INTERVAL` | `10s` | Node reachability probe interval |
| `CP_PROBE_TIMEOUT` | `5s` | Probe timeout |
| `CP_HEARTBEAT_TTL` | `30s` | Heartbeat timeout (node marked offline if exceeded) |
| `CP_CI_HOT_KEY_TTL` | `5m` | Hot key TTL |
| `CP_ADMIN_ENABLED` | `false` | Enable Admin API |
| `CP_ADMIN_ACCESS_KEY` | — | Admin API access key |
| `CP_ADMIN_SECRET_KEY` | — | Admin API secret key |
| `CP_ADMIN_JWT_SECRET` | — | JWT signing secret (**required if Admin API enabled**) |
| `CP_ADMIN_JWT_EXPIRY` | `3600` | JWT expiry in seconds |

### Edge Agent

| Variable | Default | Description |
|------|--------|------|
| `EA_LISTEN_ADDR` | `:9090` | Listen address |
| `EA_CONTROL_PLANE_URL` | — | Control Plane address (**required**) |
| `EA_ORIGIN_URL` | — | Origin server address (**required**) |
| `EA_NODE_TOKEN` | — | Node identity token (auto-register on first start) |
| `EA_CACHE_DIR` | `/tmp/edf-cache` | Cache directory |
| `EA_CACHE_MAX_GB` | `10` | Max cache capacity (GB) |
| `EA_HEARTBEAT_INTERVAL` | `10s` | Heartbeat report interval |
| `EA_NAT_MODE` | `false` | NAT mode (connect via tunnel) |
| `EA_TUNNEL_SERVER_ADDR` | — | Tunnel server address |
| `EA_QUIC_ENABLED` | `false` | Enable HTTP/3 QUIC (v0.6) |
| `EA_QUIC_LISTEN_ADDR` | `:9443` | HTTP/3 QUIC listen address |

### Gateway

| Variable | Default | Description |
|------|--------|------|
| `GW_LISTEN_ADDR` | `:8880` | HTTP proxy listen address |
| `GW_TUNNEL_ADDR` | `:7700` | Tunnel service listen address |
| `GW_CONTROL_PLANE_URL` | — | Control Plane address |
| `GW_AUTH_TOKEN` | — | Tunnel auth token (**must set in production**) |
| `GW_CP_TOKEN` | — | Control Plane API auth token (**must set in production**) |
| `GW_QUIC_ENABLED` | `false` | Enable HTTP/3 QUIC (v0.6) |
| `GW_QUIC_LISTEN_ADDR` | `:9443` | HTTP/3 QUIC listen address |

### DNS Adapter

| Variable | Default | Description |
|------|--------|------|
| `DNS_LISTEN_ADDR` | `:5353` | UDP DNS listen address |
| `DNS_DOMAIN` | `edge.local` | Dispatch domain suffix |
| `DNS_TTL_SECONDS` | `30` | DNS response TTL |
| `DNS_TOKEN_SECRET` | — | HMAC token secret (**must set in production, v0.5+**) |
| `DNS_CONTENT_AWARE_SCORE` | `25.0` | Content-aware routing score weight |

## API Reference

### Node API

```
POST /v1/nodes/register          # Register node
POST /v1/nodes/heartbeat         # Report heartbeat
GET  /v1/nodes/{nodeID}          # Query node
DELETE /v1/nodes/{nodeID}        # Revoke node
```

### Dispatch API

```
POST /v1/dispatch/resolve        # Pure API dispatch (returns Top-K candidates)
GET  /obj/{key}                  # 302 entry point (redirect to best edge node)
```

### Admin API (v0.5+)

Requires authentication via Access Key + HMAC signature. Enabled via `CP_ADMIN_ENABLED=true`.

```
POST /internal/admin/v1/nodes/{nodeID}:disable   # Disable a node
POST /internal/admin/v1/nodes/{nodeID}:enable    # Enable a node
POST /internal/admin/v1/nodes/{nodeID}:revoke    # Revoke a node
```

### Other

```
GET  /healthz                    # Health check
GET  /metrics                    # Prometheus metrics (Control Plane / Gateway / Edge Node)
```

## Development

```bash
# Build all services
make build

# Run tests
make test

# Run tests (with race detection)
make test-race

# View test coverage
make test-cover

# Run benchmarks
make bench

# Integration tests (requires Docker)
make integration-test

# Code linting
make lint
```

### Stress Testing

```bash
# Dispatch API stress test
make stress-dispatch

# Direct origin stress test
make stress-direct

# Edge node cache stress test
make stress-bench-edge

# Gateway proxy stress test
make stress-gateway
```

## Documentation

| Document | Description |
|------|------|
| [Changelog](CHANGELOG.md) | Additions, fixes, and changes by version |
| [Contributing](CONTRIBUTING.md) | Development workflow, code standards, commit conventions |
| [OpenAPI Spec](openapi.json) | OpenAPI 3.0 API specification |

## Roadmap

- [x] **v0.1** — Control Plane + Edge Node + 302 Dispatch + Range support
- [x] **v0.2** — Content Index (Bloom Filter) + DNS/GSLB Adapter
- [x] **v0.3** — Gateway Reverse Proxy + Reverse Tunnel (NAT node support)
- [x] **v0.4** — HLS/DASH Streaming + Sliding-window Cache + Prefetch
- [x] **v0.5** — Prometheus Metrics, Hot Key TTL Cleanup, Bug Fixes (v0.1~v0.3)
- [ ] **v0.6** — HTTP/3 QUIC (config + partial impl), Helm Chart Deployment

## Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) first.

## License

[MIT](LICENSE) © 2026 DarkInno
