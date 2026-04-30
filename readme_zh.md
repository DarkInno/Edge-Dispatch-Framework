# Edge Dispatch Framework

> 开源边缘分发/加速框架：**Anycast（可选）+ Edge 节点 + 中心调度**，默认 302 重定向，支持 DNS/GSLB、网关反代等多种接入方式。

[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-Compose-blue?logo=docker)](docker-compose.yml)

---

## 它能做什么

| 场景 | 说明 | 状态 |
|------|------|------|
| **下载/点播加速** | HTTP 对象分发、Range 断点续传、边缘缓存、回源 | ✅ |
| **多入口调度** | 302 重定向、DNS/GSLB、网关反代 | ✅ |
| **NAT 节点穿透** | 反向隧道，让内网节点也能提供服务 | ✅ |
| **内容感知调度** | Bloom Filter + 热内容精确索引，命中率优先 | ✅ |
| **直播分片分发** | HLS/DASH 滑动窗口缓存 + 预取 | ✅ |
| **Web 管理控制台** | 可视化管理面板，监控节点、调度、数据 | [Webmanager](https://github.com/im10furry/Edge-Dispatch-Framework-Webmanager) |
| **HTTP/3 / QUIC** | 基于 UDP 的下一代传输 | ✅ |

## 架构概览

```
▲                              ▲
│   ┌───────────────────┐      │
│   │  Web Manager (UI) │      │ Admin API
│   │  管理控制台/监控    │      │
│   └────────┬──────────┘      │
│            │                 │
┌────────────┼─────────────────┼─────────────────────────┐
│            ▼                 ▼                         │
│                   Ingress Layer                         │
│  ┌──────────┐  ┌──────────┐  ┌──────────────────┐     │
│  │ 302 重定向 │  │ DNS/GSLB │  │ Gateway 反向代理  │     │
│  └─────┬────┘  └────┬─────┘  └────────┬─────────┘     │
└────────┼────────────┼─────────────────┼────────────────┘
         │            │                 │
         ▼            ▼                 ▼
┌────────────────────────────────────────────────────────┐
│               Control Plane (控制面)                     │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐               │
│  │ Registry │ │ Scheduler│ │ Prober   │               │
│  │ 注册/鉴权 │ │ Top-K 调度│ │ 可达性探活│               │
│  └──────────┘ └──────────┘ └──────────┘               │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐               │
│  │ Heartbeat│ │ Policy   │ │ Content  │               │
│  │ 心跳/状态 │ │ 策略/限流 │ │ Index 索引│               │
│  └──────────┘ └──────────┘ └──────────┘               │
└──────────────────────┬─────────────────────────────────┘
                       │
         ┌─────────────┼─────────────┐
         ▼             ▼             ▼
┌──────────────┐ ┌──────────────┐ ┌──────────────┐
│  Edge Agent  │ │  Edge Agent  │ │  Edge (NAT)  │
│  缓存/回源    │ │  缓存/回源    │ │  通过隧道服务  │
└──────┬───────┘ └──────┬───────┘ └──────┬───────┘
       │                │                │
       ▼                ▼                ▼
┌────────────────────────────────────────────────────────┐
│                   Origin (源站)                         │
└────────────────────────────────────────────────────────┘
```
### Web 管理控制台

[Edge-Dispatch-Framework-Webmanager](https://github.com/im10furry/Edge-Dispatch-Framework-Webmanager) 是配套的 Web UI 管理面板，通过控制面 Admin API 提供可视化的节点管理、调度监控和数据展示。

```bash
# 克隆并启动 Webmanager
git clone https://github.com/im10furry/Edge-Dispatch-Framework-Webmanager.git
cd Edge-Dispatch-Framework-Webmanager
# 详见项目 README
```

---

## 快速开始（本地演示）

适用于在**单台机器**上快速体验全部功能。

### 前置条件

- Docker & Docker Compose
- Go 1.22+（仅本地开发时需要）

### 一键启动

```bash
git clone https://github.com/DarkInno/Edge-Dispatch-Framework.git
cd Edge-Dispatch-Framework

# 启动全套服务（控制面 + 2 个边缘节点 + 网关 + 源站 + PG + Redis）
docker compose up -d

# 等待服务就绪（约 15 秒）
docker compose ps

# 验证服务
curl http://localhost:8080/healthz        # 控制面
curl http://localhost:9090/healthz        # 边缘节点 1
curl http://localhost:9091/healthz        # 边缘节点 2
curl http://localhost:7070/healthz        # 源站
curl http://localhost:8880/healthz        # 网关
```

### 测试 302 调度

```bash
# 1. 注册一个边缘节点
curl -sS -X POST http://localhost:8080/v1/nodes/register \
  -H 'Content-Type: application/json' \
  -d '{
    "node_name": "edge-shanghai",
    "public_endpoints": [{"scheme": "http", "host": "127.0.0.1", "port": 9090}],
    "region": "cn-sh",
    "isp": "ctcc",
    "capabilities": {"inbound_reachable": true, "cache_disk_gb": 10, "max_uplink_mbps": 100}
  }'

# 返回: {"node_id":"...","token":"..."}
# 记下 node_id 和 token

# 2. 请求 302 重定向（控制面返回最优边缘节点地址）
curl -sS -I http://localhost:8080/obj/test-file.bin

# 3. 直接请求边缘节点（带 token）
curl -sS http://localhost:9090/obj/test-file.bin?token=<token_from_step_1>
```

---

## 部署指南

> 生产环境中，**控制面是中心化服务**（部署在一台/多台中心服务器），**边缘节点是分布式服务**（部署在各区域的边缘服务器上），二者使用不同的 compose 文件。

### 部署架构

```
                          ┌─────────────────────────────┐
                          │    Control Plane (中心节点)    │
                          │  ┌──────────┐ ┌──────────┐  │
                          │  │ Postgres │ │  Redis   │  │
                          │  └──────────┘ └──────────┘  │
                          │  ┌──────────┐ ┌──────────┐  │
                          │  │Control   │ │ Gateway  │  │
                          │  │ Plane    │ │ (可选)    │  │
                          │  └──────────┘ └──────────┘  │
                          │  ┌──────────┐               │
                          │  │ DNS      │ (可选)         │
                          │  │ Adapter  │               │
                          │  └──────────┘               │
                          └──────┬──────────────────────┘
                                 │ HTTP (edge → cp)
              ┌──────────────────┼──────────────────┐
              ▼                  ▼                  ▼
    ┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
    │   Edge Agent 1  │ │   Edge Agent 2  │ │   Edge Agent N  │
    │   (区域 A)       │ │   (区域 B)       │ │   (区域 ...)     │
    │   docker-compose │ │   docker-compose │ │   docker-compose │
    │   .edge.yml      │ │   .edge.yml      │ │   .edge.yml      │
    └────────┬────────┘ └────────┬────────┘ └────────┬────────┘
             │                   │                   │
             └───────────────────┼───────────────────┘
                                 ▼
                      ┌─────────────────┐
                      │  Origin (源站)   │
                      │  (独立部署)      │
                      └─────────────────┘
```

**关键区别：**
- **控制面**：集中部署，管理节点注册、调度、心跳。边缘节点通过 HTTP 连接控制面。
- **边缘节点**：分布式部署，靠近用户。与控制面不在同一个 Docker 网络中，通过公网/内网 IP 通信。

### 前置条件

- 控制面服务器：Docker & Docker Compose，至少 2GB 内存
- 边缘节点服务器：Docker & Docker Compose（不需要 Go 环境，使用 Docker 构建或预构建镜像）
- 网络：边缘节点服务器需要能够通过 HTTP 访问控制面的 `8080` 端口

### 安全准备

生产部署**必须**替换默认密钥，否则服务会拒绝启动：

```bash
# 生成强随机密钥
openssl rand -base64 32
```

在控制面服务器上创建 `.env` 文件：

```bash
# .env (放在与控制面 compose 文件同目录)
CP_TOKEN_SECRET=<生成的随机密钥>
GW_AUTH_TOKEN=<生成的随机密钥>
PG_PASSWORD=<数据库密码>
```

---

### 方式一：单机演示（All-in-One）

在一台机器上体验全部功能。控制面和边缘节点在同一个 Docker 网络中。

```bash
docker compose up -d
```

> 这是 `docker-compose.yml` 的模式，仅用于本地开发和功能验证。

---

### 方式二：分离部署（生产推荐）

#### 第一步：部署控制面

在中心服务器上：

```bash
git clone https://github.com/DarkInno/Edge-Dispatch-Framework.git
cd Edge-Dispatch-Framework

# 创建 .env 配置
cat > .env << 'EOF'
CP_TOKEN_SECRET=<your-random-secret>
GW_AUTH_TOKEN=<your-random-token>
PG_PASSWORD=<your-pg-password>
EOF

# 启动控制面核心服务
docker compose -f docker-compose.cp.yml up -d

# 如果需要 Gateway 反向代理:
docker compose -f docker-compose.cp.yml --profile gateway up -d

# 如果需要 DNS 适配器:
docker compose -f docker-compose.cp.yml --profile dns up -d

# 启动全部可选组件:
docker compose -f docker-compose.cp.yml --profile full up -d

# 验证
curl http://localhost:8080/healthz
```

控制面默认端口：
| 服务 | 端口 | 说明 |
|------|------|------|
| control-plane | 8080/tcp | 控制面 API（边缘节点通过此端口通信） |
| gateway | 8880/tcp | HTTP 反向代理（可选） |
| gateway-tunnel | 7700/tcp | 反向隧道服务（可选，NAT 节点需要） |
| dns-adapter | 53/udp | DNS 权威服务器（可选） |
| postgres | 5432/tcp | 数据库 |
| redis | 6379/tcp | 缓存 |

> 开放防火墙：边缘节点需要能访问控制面的 **8080** 端口。

#### 第二步：部署边缘节点

在**每台边缘服务器**上：

```bash
git clone https://github.com/DarkInno/Edge-Dispatch-Framework.git
cd Edge-Dispatch-Framework

# 创建 .env 配置
cat > .env << 'EOF'
# 必填：控制面地址（替换为实际 IP/域名）
EA_CONTROL_PLANE_URL=http://<cp-public-ip>:8080

# 必填：源站地址（缓存未命中时回源）
EA_ORIGIN_URL=http://<origin-ip>:7070

# 可选：缓存配置
EA_CACHE_MAX_GB=100
EA_CACHE_DIR=/data/edf-cache
EOF

# 启动边缘节点
docker compose -f docker-compose.edge.yml up -d

# 查看日志
docker compose -f docker-compose.edge.yml logs -f

# 验证
curl http://localhost:9090/healthz
```

**NAT 节点** — 节点在 NAT 后无公网 IP 时使用隧道模式：

在 `.env` 中添加：
```bash
EA_NAT_MODE=true
EA_TUNNEL_SERVER_ADDR=<gateway-host>:7700
EA_TUNNEL_AUTH_TOKEN=<gw-auth-token>
```

#### 第三步：注册边缘节点到控制面

边缘节点启动后，通过边缘节点的自动注册或手动 API 将其注册到控制面：

**自动注册**：边缘节点首次启动时，如果没有 `EA_NODE_TOKEN`，会自动调用控制面的注册 API。

**手动注册**：
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

> 返回的 `token` 需要记录下来，后续可以配置到边缘节点的 `EA_NODE_TOKEN` 环境变量中以保持身份。

#### 第四步：验证调度

```bash
# 通过控制面 302 重定向获取最优边缘节点
curl -sS -I http://<cp-host>:8080/obj/test-file.bin

# 输出:
# HTTP/1.1 302 Found
# Location: http://<edge-ip>:9090/obj/test-file.bin?token=...

# 通过 API 获取 Top-K 候选节点
curl -sS -X POST http://<cp-host>:8080/v1/dispatch/resolve \
  -H 'Content-Type: application/json' \
  -d '{
    "resource_key": "test-file.bin",
    "client_info": {"region": "cn-bj", "isp": "ctcc", "ip": "1.2.3.4"}
  }'
```

#### 第五步：部署示例源站（可选）

本项目的 `origin` 服务是一个示例源站。生产环境中通常替换为已有的对象存储（S3/OSS）或 CDN 源站。

```bash
docker compose -f docker-compose.edge.yml --profile origin up -d
```

---

### 部署多节点（单机测试）

在单台机器上模拟多个边缘节点用于测试：

```bash
# 节点 1
EA_CONTROL_PLANE_URL=http://localhost:8080 \
EA_CACHE_DIR=/tmp/edf-cache-1 \
docker compose -f docker-compose.edge.yml -p edge1 up -d

# 节点 2 (使用不同端口和缓存目录)
EA_CONTROL_PLANE_URL=http://localhost:8080 \
EA_CACHE_DIR=/tmp/edf-cache-2 \
docker compose -f docker-compose.edge.yml -p edge2 up -d

# 手动指定端口映射
docker run -d --name edge3 \
  -e EA_LISTEN_ADDR=:9092 \
  -e EA_CONTROL_PLANE_URL=http://host.docker.internal:8080 \
  -e EA_ORIGIN_URL=http://host.docker.internal:7070 \
  -e EA_CACHE_DIR=/tmp/edf-cache-3 \
  -p 9092:9092 \
  edf-edge-agent
```

---

### 使用预构建镜像

生产环境中建议先构建镜像并推送到私有 Registry，边缘节点直接拉取镜像：

```bash
# 在构建机上:
docker compose -f docker-compose.edge.yml build
docker tag edf-edge-agent your-registry/edf-edge-agent:latest
docker push your-registry/edf-edge-agent:latest

# 在边缘节点上，修改 docker-compose.edge.yml 将 build: 替换为 image::
#   image: your-registry/edf-edge-agent:latest
```

---

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
│   ├── metrics/                  # Prometheus 指标（v0.5）
│   ├── models/                   # 共享数据模型
│   ├── quic/                     # HTTP/3 QUIC（占位）
│   ├── store/                    # PostgreSQL + Redis 存储层
│   ├── streaming/                # HLS/DASH 流媒体支持（v0.4）
│   └── tunnel/                   # 反向隧道协议（v0.3）
├── docker/                       # Dockerfile
├── docker-compose.yml            # 本地演示（All-in-One）
├── docker-compose.cp.yml         # 控制面部署（生产用）
├── docker-compose.edge.yml       # 边缘节点部署（生产用）
├── Makefile                      # 构建/测试/部署命令
└── requirements.md               # 产品需求文档（PRD）
```

## 配置参考

所有服务通过环境变量配置，参见 `internal/config/config.go`。

### 控制面（Control Plane）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `CP_LISTEN_ADDR` | `:8080` | 监听地址（边缘节点通过此端口通信） |
| `CP_PG_URL` | — | PostgreSQL 连接串 |
| `CP_REDIS_ADDR` | `localhost:6379` | Redis 地址 |
| `CP_TOKEN_SECRET` | — | HMAC Token 密钥（**生产必须设置**） |
| `CP_MAX_CANDIDATES` | `5` | 调度返回的最大候选数 |
| `CP_DEFAULT_TTL_MS` | `30000` | 候选 TTL（毫秒） |
| `CP_DEGRADE_TO_ORIGIN` | `true` | 无可用节点时降级到源站 |
| `CP_PROBE_INTERVAL` | `10s` | 节点可达性探测间隔 |
| `CP_PROBE_TIMEOUT` | `5s` | 探测超时 |
| `CP_HEARTBEAT_TTL` | `30s` | 心跳超时（超过则标记下线） |
| `CP_CI_HOT_KEY_TTL` | `5m` | 热键 TTL |

### 边缘节点（Edge Agent）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `EA_LISTEN_ADDR` | `:9090` | 监听地址 |
| `EA_CONTROL_PLANE_URL` | — | 控制面地址（**必填**） |
| `EA_ORIGIN_URL` | — | 源站地址（**必填**） |
| `EA_NODE_TOKEN` | — | 节点身份 Token（首次自动注册） |
| `EA_CACHE_DIR` | `/tmp/edf-cache` | 缓存目录 |
| `EA_CACHE_MAX_GB` | `10` | 最大缓存容量（GB） |
| `EA_HEARTBEAT_INTERVAL` | `10s` | 心跳上报间隔 |
| `EA_NAT_MODE` | `false` | NAT 模式（通过隧道连接） |
| `EA_TUNNEL_SERVER_ADDR` | — | 隧道服务器地址 |

### 网关（Gateway）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `GW_LISTEN_ADDR` | `:8880` | HTTP 代理监听地址 |
| `GW_TUNNEL_ADDR` | `:7700` | 隧道服务监听地址 |
| `GW_CONTROL_PLANE_URL` | — | 控制面地址 |
| `GW_AUTH_TOKEN` | — | 隧道认证 Token（**生产必须设置**） |

### DNS 适配器

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
GET  /metrics                    # Prometheus 指标（控制面/网关/边缘节点）
```

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
| [更新日志](CHANGELOG.md) | 按版本记录的新增、修复、变更 |
| [贡献指南](CONTRIBUTING.md) | 开发流程、代码规范、提交规范 |

## Roadmap

- [x] **v0.1** — 控制面 + 边缘节点 + 302 调度 + Range 支持
- [x] **v0.2** — 内容索引（Bloom Filter）+ DNS/GSLB 适配器
- [x] **v0.3** — 网关反代 + 反向隧道（NAT 节点支持）
- [x] **v0.4** — HLS/DASH 流媒体 + 滑动窗口缓存 + 预取
- [x] **v0.5** — Prometheus 指标、热键 TTL 清理、Bug 修复（v0.1~v0.3）
- [ ] **v0.6** — HTTP/3 QUIC、Helm Chart 部署

## 贡献

欢迎贡献！请先阅读 [CONTRIBUTING.md](CONTRIBUTING.md)。

## License

[MIT](LICENSE) © 2026 DarkInno
