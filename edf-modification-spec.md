# Edge-Dispatch-Framework（EDF）需要改哪些：修改说明文档

> 目标：在不破坏 EDF “调度核心”定位的前提下，使其能够被 **公网 Admin Console** 安全地“可修改/可运维”。  
> 原则：**不让 Control Plane 直接裸奔公网**；Admin Console 通过 **Internal Admin APIs**（仅内网 + 强鉴权）对 EDF 施加变更。

---

## 1. 结论（最小可行改动）

EDF 仓库需要做三类改动：

1) **控制面新增 Internal Admin API（必须）**  
用于节点运维（disable/enable/revoke）、策略发布/回滚、缓存运维任务编排等。

2) **补齐可运维所需的“可观测字段与指标”（强烈建议）**  
让控制台能展示“为什么离线/为什么没命中/当前命中率/回源比/错误率”等。

3) **支持多租户/项目隔离的标识与约束（平台化必需）**  
最少要让 Node、Ingress、Policy、Object key 等与 `tenant_id / project_id` 关联（或支持外部映射）。

> 不推荐方案：Admin API 直接读写 EDF 的 PG/Redis。虽然 EDF 可以不改，但会导致强耦合、升级风险与安全边界问题。

---

## 2. 改动范围总览（按模块）

### 2.1 Control Plane（控制面）

**新增：**
- `internal/admin` 路由组（仅内网访问）
- internal admin 鉴权中间件（mTLS 或 HMAC）
- 管理操作的审计钩子（结构化日志 + request_id）
- 若要做平台化：资源按 tenant/project 分区或打标签

**可能调整：**
- 节点模型：补充版本、标签、权重、维护状态、禁用原因
- 调度逻辑：尊重“禁用/维护中”的节点；按 project/tenant 过滤节点池
- 存储层：为多租户加索引/约束

### 2.2 Edge Agent（边缘节点）

**建议新增：**
- `/healthz`、`/metrics` 指标补齐（若已有则增强标签维度）
- 预热/驱逐等运维任务执行端点（若由边缘直接执行）
- 支持接收“运维指令”（pull 或 push）：
  - pull：边缘定期从 CP 拉取 tasks
  - push：CP 下发并由边缘 ack（更复杂）

### 2.3 Gateway / DNS Adapter（可选增强）
- 将 ingress 配置纳入“可管理资源”：域名、路由规则、TTL、健康检查
- 管理面可重载配置（热更新/滚动更新）

---

## 3. 必须新增：Internal Admin API（建议规格）

> 对外 API（公网）属于 Admin Plane；这里说的是 EDF 内部 **CP 专用** internal 管理接口。  
> 建议统一前缀：`/internal/admin/v1`

### 3.1 鉴权与访问控制（必须）

必须满足三件事：
- **网络隔离**：仅允许 admin-api 所在网段访问（安全组/iptables）
- **强鉴权**（二选一，推荐 mTLS）：
  1. **mTLS**：CP 作为 server 验 client cert（admin-api 证书）
  2. **HMAC 签名**：`X-Admin-KeyId / X-Admin-Timestamp / X-Admin-Nonce / X-Admin-Signature`
- **审计上下文**：要求 admin-api 透传 actor 信息：
  - `X-Actor-UserId`
  - `X-Actor-TenantId`
  - `X-Actor-ProjectId`
  - `X-Request-Id`（可由 admin-api 生成）

### 3.2 Nodes 运维接口（必需）

1) 禁用/维护节点
- `POST /internal/admin/v1/nodes/{node_id}:disable`
  - body：`{ "reason": "maintenance|abuse|manual|other", "message": "...", "until": "RFC3339 optional" }`

2) 启用节点
- `POST /internal/admin/v1/nodes/{node_id}:enable`

3) 吊销节点 token（强制失效）
- `POST /internal/admin/v1/nodes/{node_id}:revoke`

4) 修改节点属性（标签/权重/备注/项目归属）
- `PATCH /internal/admin/v1/nodes/{node_id}`
  - body：`{ "labels": {...}, "weight": 100, "project_id": "...", "tenant_id": "..." }`

**CP 侧行为要求：**
- 调度时过滤 `disabled=true` 的节点
- 吊销后 edge-agent 心跳应被拒绝/重新注册

### 3.3 Policies 策略发布/回滚（平台化强需求）

> 如果 EDF 当前策略都写死在 env/config，建议引入“策略配置资源化”的最小实现：策略 JSON 存储 + 版本 + 生效点。

- `POST /internal/admin/v1/policies/{policy_id}:publish`
- `POST /internal/admin/v1/policies/{policy_id}:rollback?to=version`
- `GET  /internal/admin/v1/policies/{policy_id}/versions`

**CP 侧行为要求：**
- 发布是原子生效（至少对调度路径原子切换）
- 回滚可快速切换到旧版本

### 3.4 CacheOps：预热/驱逐/封禁（对象级运维）

两种实施模式（二选一）：

**模式 A（推荐，可扩展）：CP 编排任务，Edge 执行**
- `POST /internal/admin/v1/cache/prewarm` → CP 创建 task
- `POST /internal/admin/v1/cache/purge` → CP 创建 task
- `POST /internal/admin/v1/objects/block` → CP 更新 blocklist
- `GET  /internal/admin/v1/tasks/{task_id}`

**模式 B（简单但弱）：CP 直接对某个边缘发指令**
- 需要明确“目标节点集合”并处理失败重试，不推荐早期就做 push。

---

## 4. 多租户/项目（平台化）改动建议

### 4.1 最小需求

需要能在 EDF 侧辨识并隔离资源：
- Node 属于哪个 tenant/project
- Ingress（域名/入口）属于哪个 tenant/project
- Policy 属于哪个 tenant/project
- Object key 的权限归属（至少按项目维度）

### 4.2 推荐落地方式

**方案 1（推荐）：EDF 资源模型增加 `tenant_id`、`project_id` 字段**
- 数据库 schema 增加字段 + 索引
- API 输入输出透传
- 调度时按项目过滤节点池

**方案 2：外部映射（不改 schema，兼容性强但功能弱）**
- admin-api 在外部维护映射表：node_id → project_id
- CP internal 接口在执行时带上 project_id，CP 只做逻辑校验
- 缺点：CP 很难做强隔离与查询聚合

---

## 5. 可观测性增强（建议）

### 5.1 指标（Prometheus）

建议 CP/Edge/Gateway 均添加统一标签维度（至少）：
- `tenant_id`、`project_id`（可选但平台化强烈需要）
- `node_id`、`region`、`isp`
- `ingress_type`（302/dns/gw）
- `result`（hit/miss/origin/degraded）

关键指标建议（示例）：
- 调度：QPS、候选数分布、无可用节点次数、降级次数
- 命中：hit/miss、回源比、Range 请求比例
- 错误：5xx、超时、probe 失败、心跳超时

### 5.2 结构化日志与追踪
- 所有 internal admin 写操作：输出审计结构化日志（与 admin-api 的审计 event_id 对齐）
- 调度链路：引入 `request_id`（入口生成/透传），便于控制台排障

---

## 6. 配置与兼容性策略（开源友好）

### 6.1 兼容性
- 新增 internal admin APIs 不影响现有对外 API
- 多租户字段可做“可选字段”：未启用平台化时默认空/`default`

### 6.2 Feature Flags（建议）
- `CP_ENABLE_ADMIN_INTERNAL_API=true`
- `CP_REQUIRE_MTLS_FOR_ADMIN=true`
- `CP_ENABLE_MULTITENANCY=true`

---

## 7. 测试与验收（建议清单）

### 7.1 安全
- internal admin APIs：无证书/签名访问必失败
- 重放攻击：HMAC 模式需 nonce/timestamp 校验
- actor header 不可伪造：仅在通过 admin auth 后才信任

### 7.2 功能
- disable 后调度不再返回该节点
- revoke 后节点心跳被拒/强制重新注册
- policy publish 原子生效；rollback 可恢复
- cache prewarm/purge 任务可追踪、可重试、可中止（可选）

---

## 8. 建议的 PR 切分（便于开源审阅）

1) PR1：新增 internal admin API 基础框架（路由组 + 鉴权中间件 + 审计日志）
2) PR2：Nodes 运维（disable/enable/revoke + 调度过滤）
3) PR3：多租户字段（schema + 过滤逻辑 + 基础查询）
4) PR4：Policies 版本化（如需要）
5) PR5：CacheOps 任务框架（tasks + edge 执行接口）
6) PR6：指标/日志增强

