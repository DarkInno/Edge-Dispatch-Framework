# API 设计（草案）：Node API + Dispatch API（默认 302 调度）

> 说明：
> - 本文为 v0.1 目标 API 的**草案**，用于先定协议与边界，后续实现以此为准迭代。
> - 风格：优先 REST/JSON，便于不同语言快速接入；后续可增加 gRPC 等价接口（Roadmap）。

---

## 1. 通用约定

### 1.1 Base URL
- 控制面：`https://control.example.com`
- 入口（302 调度）：`https://dispatch.example.com`

> 入口与控制面可以是同一个服务，也可以拆分（取决于部署）。

### 1.2 鉴权

#### 节点调用（Node API）
二选一（实现时择优）：
1. **mTLS（推荐）**：节点持有证书，通过 mTLS 访问控制面
2. Bearer Token：`Authorization: Bearer <node_token>`

#### 业务方/客户端调用（Dispatch）
- v0.1 建议允许“开放调度 + URL 签名”两种模式（由使用方选择）：
  - 开放调度：调度接口不鉴权（适合内部/PoC）
  - 调度鉴权：`Authorization: Bearer <app_token>`

### 1.3 错误返回格式
所有 4xx/5xx 建议返回统一结构：

```json
{
  "error": {
    "code": "EDGE_NOT_FOUND",
    "message": "no reachable edge nodes",
    "request_id": "uuid",
    "details": {"hint": "try again later"}
  }
}
```

---

## 2. Node API（节点接入）

### 2.1 注册节点
`POST /v1/nodes/register`

**Request**
```json
{
  "node_name": "edge-bj-01",
  "public_endpoints": [
    {"scheme": "https", "host": "1.2.3.4", "port": 443}
  ],
  "region": "cn-bj",
  "isp": "cmcc",
  "capabilities": {
    "inbound_reachable": true,
    "cache_disk_gb": 500,
    "max_uplink_mbps": 200
  }
}
```

**Response**
```json
{
  "node_id": "n_123",
  "auth": {
    "type": "token",
    "token": "xxxx",
    "exp": 0
  }
}
```

> Roadmap：返回证书/CSR 流程以支持 mTLS。

### 2.2 心跳与运行状态
`POST /v1/nodes/heartbeat`

**Request**
```json
{
  "node_id": "n_123",
  "ts": 1710000000,
  "runtime": {
    "cpu": 0.32,
    "mem_mb": 2048,
    "disk_free_gb": 120,
    "conn": 1800
  },
  "traffic": {
    "egress_mbps": 80,
    "ingress_mbps": 10,
    "err_5xx_rate": 0.002
  },
  "cache": {
    "hit_ratio": 0.67
  }
}
```

**Response**
```json
{"ok": true}
```

### 2.3 内容摘要上报（可选，v0.2）
`POST /v1/nodes/content/summary`

用于让控制面知道“这个节点大概有哪些内容/分片”，典型实现：
- 热内容精确表（key 列表）
- 冷内容 Bloom filter/摘要

---

## 3. Dispatch API（调度接口）

> Dispatch API 有两种使用方式：
> 1) **纯 API 模式**：业务方/SDK 调用 `POST /dispatch/resolve` 获取 Top-K，再由客户端选择并请求边缘  
> 2) **302 入口模式（默认）**：客户端直接访问 `GET /obj/{key}`，由服务返回 302 到边缘

### 3.1 Resolve（纯 API）
`POST /v1/dispatch/resolve`

**Request**
```json
{
  "client": {
    "ip": "203.0.113.9",
    "region": "cn-sh",
    "isp": "ctcc"
  },
  "resource": {
    "type": "object",
    "key": "video/123.mp4",
    "scheme": "https"
  },
  "hints": {
    "need_range": true
  }
}
```

**Response**
```json
{
  "request_id": "uuid",
  "ttl_ms": 30000,
  "token": {
    "type": "hmac",
    "value": "token_xxx",
    "exp": 1710000300
  },
  "candidates": [
    {
      "id": "n_123",
      "endpoint": "https://edge-a.example.com",
      "weight": 100,
      "meta": {"region": "cn-sh", "isp": "ctcc"}
    },
    {
      "id": "n_456",
      "endpoint": "https://edge-b.example.com",
      "weight": 80,
      "meta": {"region": "cn-hz", "isp": "ctcc"}
    }
  ]
}
```

### 3.2 302 入口（默认）
`GET /obj/{key}`

**Query（建议）**
- `token`（可选）：业务方自签名/或由控制面签发
- `range`：客户端可照常使用 HTTP Range Header

**Response**
- `302 Found`
  - `Location: https://edge-a.example.com/obj/{key}?token=...`
  - `X-Alt-Location: https://edge-b... , https://edge-c...`（可选）
  - `Cache-Control: private, max-age=0`（建议不要被公共缓存长期缓存）

### 3.3 示例：curl（演示 302）
```bash
curl -I "https://dispatch.example.com/obj/video/123.mp4"
```

---

## 4. Token/签名（建议约定）

### 4.1 目标
- 防盗链：避免他人拿到边缘真实 URL 后无限复用
- 防滥用：可限制时间窗、速率、（可选）IP 前缀

### 4.2 建议载荷（概念）
- `key`：资源 key
- `exp`：过期时间（短）
- `cidr`：可选，客户端 IP 前缀
- `max_rate/max_conn`：可选

> v0.1 可先用 HMAC(querystring)；v0.2 再引入 JWT/mTLS 等插件化实现。

---

## 5. 错误码建议（最小集合）

| code | HTTP | 说明 |
|---|---:|---|
| INVALID_ARGUMENT | 400 | 参数不合法 |
| UNAUTHORIZED | 401 | 鉴权失败 |
| FORBIDDEN | 403 | 策略拒绝（ACL/黑名单/配额） |
| EDGE_NOT_FOUND | 503 | 无可达边缘节点（可降级到源站） |
| INTERNAL | 500 | 内部错误 |

---

## 6. Roadmap：网关反代与 DNS 适配

### 6.1 网关反代（Gateway）
网关向控制面拿候选，然后执行 L7/L4 转发：
- 新增接口：`POST /v1/gateway/upstream/select`
- 或复用 `resolve`，由网关携带更详细的连接信息

### 6.2 DNS/GSLB
DNS 适配器将 `candidates` 转为 A/AAAA：
- 新增接口：`POST /v1/dns/answer`
- 或复用 `resolve`，增加 `mode=dns`

