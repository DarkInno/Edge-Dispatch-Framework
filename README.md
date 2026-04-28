# Edge Dispatch Framework

> 开源边缘分发/加速框架：**Anycast（可选）+ Edge 节点 + 中心调度**，默认 302 重定向，支持 DNS/GSLB、网关反代等多种接入方式。

[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-Compose-blue?logo=docker)](docker-compose.yml)

---

## 它能做什么

| 场景 | 说明 | 状态 |
|------|------|------|
| **下载/点播加速** | HTTP 对象分发、Range 断点续传、边缘缓存、回源 | ✅ v0.1 |
| **多入口调度** | 302 重定向、DNS/GSLB、网关反代 | ✅ v0.1~v0.3 |
| **NAT 节点穿透** | 反向隧道，让内网节点也能提供服务 | ✅ v0.3 |
| **内容感知调度** | Bloom Filter + 热内容精确索引，命中率优先 | ✅ v0.2 |
| **直播分片分发** | HLS/DASH 滑动窗口缓存 + 预取 | ✅ v0.4 |
| **HTTP/3 / QUIC** | 基于 UDP 的下一代传输 | 🔜 Roadmap |

## 架构概览

```
┌─────────────────────────────────────────────────────┐
│                   Ingress Layer                      │
│  ┌──────────┐  ┌──────────┐  ┌──────────────────┐  │
│  │ 302 重定向 │  │ DNS/GSLB │  │ Gateway 反向代理  │  │
│  └─────┬────┘  └────┬─────┘  └────────┬─────────┘  │
└────────┼────────────┼─────────────────┼─────────────┘
         │            │                 │
         ▼            ▼                 ▼
┌─────────────────────────────────────────────────────┐
│               Control Plane (控制面)                  │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐            │
│  │ Registry │ │ Scheduler│ │ Prober   │            │
│  │ 注册/鉴权 │ │ Top-K 调度│ │ 可达性探活│            │
│  └──────────┘ └──────────┘ └──────────┘            │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐            │
│  │ Heartbeat│ │ Policy   │ │ Content  │            │
│  │ 心跳/状态 │ │ 策略/限流 │ │ Index 索引│            │
│  └──────────┘ └──────────┘ └──────────┘            │
└──────────────────────┬──────────────────────────────┘
                       │
         ┌─────────────┼─────────────┐
         ▼             ▼             ▼
┌──────────────┐ ┌──────────────┐ ┌──────────────┐
│  Edge Agent  │ │  Edge Agent  │ │  Edge (NAT)  │
│  缓存/回源    │ │  缓存/回源    │ │  通过隧道服务  │
└──────┬───────┘ └──────┬───────┘ └──────┬───────┘
       │                │                │
       ▼                ▼                ▼
┌─────────────────────────────────────────────────────┐
│                   Origin (源站)                      │
└─────────────────────────────────────────────────────┘
```

## 快速开始

### 前置条件

- Docker & Docker Compose
- Go 1.22+（本地开发时需要）

### 一键启动

```bash
# 克隆仓库
git clone https://github.com/DarkInno/Edge-Dispatch-Framework.git
cd Edge-Dispatch-Framework

# 启动全套服务（控制面 + 2 个边缘节点 + 源站 + Redis + PostgreSQL）
docker compose up -d

# 验证服务
curl http://localhost:8080/healthz        # 控制面
curl http://localhost:9090/healthz        # 边缘节点 1
curl http://localhost:9091/healthz        # 边缘节点 2
curl http://localhost:7070/healthz        # 源站
```

### 测试 302 调度

```bash
# 注册一个边缘节点
curl -X POST http://localhost:8080/v1/nodes/register \
  -H 'Content-Type: application/json' \
  -d '{
    "node_name": "edge-local",
    "public_endpoints": [{"scheme": "http", "host": "127.0.0.1", "port": 9090}],
    "region": "cn-sh",
    "isp": "ctcc",
    "capabilities": {"inbound_reachable": true, "cache_disk_gb": 10, "max_uplink_mbps": 100}
  }'

# 请求 302 重定向
curl -I http://localhost:8080/obj/test-file.bin

# 直接请求边缘节点（带 token）
curl http://localhost:9090/obj/test-file.bin?token=<token_from_above>
```

### 启用 NAT 节点（v0.3）

```bash
# 启动网关 + NAT 边缘节点
docker compose up -d gateway edge-agent-nat

# 网关会自动通过隧道代理 NAT 节点的请求
curl http://localhost:8880/obj/test-file.bin
```

### 启用 DNS 适配器（v0.2）

```bash
docker compose up -d dns-adapter

# DNS 查询
dig @127.0.0.1 -p 5353 video123.edge.local
```

## 项目结构

```
.
├── cmd/                          # 服务入口
│   ├── control-plane/            # 控制面
│   ├── edge-agent/               # 边缘节点
│   ├── gateway/                  # 网关反代（v0.3）
│   ├── dns-adapter/              # DNS/GSLB 适配器（v0.2）
│   ├── origin/                   # 示例源站
│   └── stress/                   # 压力测试工具
├── internal/                     # 核心库
│   ├── auth/                     # HMAC Token 签名/验证
│   ├── config/                   # 环境变量配置加载
│   ├── contentindex/             # 内容索引（Bloom Filter + 热内容）
│   ├── controlplane/             # 控制面核心（API、调度、心跳、探活、策略）
│   ├── dns/                      # DNS/GSLB 适配器
│   ├── edgeagent/                # 边缘节点（缓存、回源、上报）
│   ├── gateway/                  # 网关反向代理
│   ├── models/                   # 共享数据模型
│   ├── quic/                     # HTTP/3 QUIC（Roadmap）
│   ├── store/                    # PostgreSQL + Redis 存储层
│   ├── streaming/                # HLS/DASH 流媒体支持（v0.4）
│   └── tunnel/                   # 反向隧道协议（v0.3）
├── docker/                       # Dockerfile
├── docs/                         # 设计文档
├── docker-compose.yml            # 全栈编排
├── Makefile                      # 构建/测试/部署命令
└── requirements.md               # 产品需求文档（PRD）
```

## 配置

所有服务通过环境变量配置，参见 `internal/config/config.go`。

### 控制面（Control Plane）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CP_LISTEN_ADDR` | `:8080` | 监听地址 |
| `CP_PG_URL` | — | PostgreSQL 连接串 |
| `CP_REDIS_ADDR` | `localhost:6379` | Redis 地址 |
| `CP_TOKEN_SECRET` | — | HMAC Token 密钥（**必须设置**） |
| `CP_MAX_CANDIDATES` | `5` | 调度返回的最大候选数 |
| `CP_DEFAULT_TTL_MS` | `30000` | 候选 TTL（毫秒） |
| `CP_DEGRADE_TO_ORIGIN` | `false` | 无可用节点时降级到源站 |

### 边缘节点（Edge Agent）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `EA_LISTEN_ADDR` | `:9090` | 监听地址 |
| `EA_CONTROL_PLANE_URL` | — | 控制面地址 |
| `EA_ORIGIN_URL` | — | 源站地址 |
| `EA_CACHE_DIR` | `/tmp/edf-cache` | 缓存目录 |
| `EA_CACHE_MAX_GB` | `10` | 最大缓存容量（GB） |
| `EA_NAT_MODE` | `false` | NAT 模式（通过隧道连接） |
| `EA_TUNNEL_SERVER_ADDR` | — | 隧道服务器地址 |

### 网关（Gateway, v0.3）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `GW_LISTEN_ADDR` | `:8880` | HTTP 代理监听地址 |
| `GW_TUNNEL_ADDR` | `:7700` | 隧道服务监听地址 |
| `GW_CONTROL_PLANE_URL` | — | 控制面地址 |
| `GW_AUTH_TOKEN` | — | 隧道认证 Token |

### DNS 适配器（v0.2）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `DNS_LISTEN_ADDR` | `:5353` | UDP DNS 监听地址 |
| `DNS_DOMAIN` | `edge.local` | 调度域名后缀 |
| `DNS_TTL_SECONDS` | `30` | DNS 响应 TTL |

## API 接口

### 节点 API

```
POST /v1/nodes/register          # 节点注册
POST /v1/nodes/heartbeat         # 心跳上报
GET  /v1/nodes/{nodeID}          # 查询节点
DELETE /v1/nodes/{nodeID}        # 吊销节点
```

### 调度 API

```
POST /v1/dispatch/resolve        # 纯 API 调度（返回 Top-K 候选）
GET  /obj/{key}                  # 302 入口（重定向到最优边缘节点）
```

### 其他

```
GET  /healthz                    # 健康检查
GET  /metrics                    # 指标（边缘节点 JSON 格式）
```

详细 API 文档见 [docs/API 设计.md](docs/API%20设计.md)。

## 开发

```bash
# 构建所有服务
make build

# 运行测试
make test

# 运行测试（含竞态检测）
make test-race

# 查看测试覆盖率
make test-cover

# 运行 benchmark
make bench

# 集成测试（需要 Docker）
make integration-test

# 代码检查
make lint
```

### 压力测试

```bash
# 调度 API 压测
make stress-dispatch

# 源站直连压测
make stress-direct

# 边缘节点缓存压测
make stress-bench-edge

# 网关代理压测
make stress-gateway
```

## 文档

| 文档 | 说明 |
|------|------|
| [需求文档（PRD）](requirements.md) | 产品需求、功能范围、里程碑 |
| [架构设计](docs/架构设计.md) | 分层架构、数据流、部署拓扑 |
| [设计文档](docs/设计文档.md) | 模块拆分、数据模型、状态机 |
| [API 设计](docs/API%20设计.md) | REST API 接口规范 |
| [性能优化计划](docs/plans/2026-04-29-performance-optimization.md) | 65 项优化任务清单 |

## Roadmap

- [x] **v0.1** — 控制面 + 边缘节点 + 302 调度 + Range 支持
- [x] **v0.2** — 内容索引（Bloom Filter）+ DNS/GSLB 适配器
- [x] **v0.3** — 网关反代 + 反向隧道（NAT 节点支持）
- [x] **v0.4** — HLS/DASH 流媒体 + 滑动窗口缓存 + 预取
- [ ] **v0.5** — HTTP/3 QUIC、Prometheus 指标、Helm Chart

## 贡献

欢迎贡献！请先阅读 [CONTRIBUTING.md](CONTRIBUTING.md)。

## License

[MIT](LICENSE) © 2026 DarkInno
