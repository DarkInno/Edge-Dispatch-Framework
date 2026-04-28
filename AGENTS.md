# AGENTS.md — AI Agent 开发指南

本文件为 AI Agent（如 Codex、Copilot、Claude 等）提供项目上下文和开发规范。

---

## 项目概述

**Edge Dispatch Framework** 是一个开源边缘分发/加速框架，使用 Go 1.22 开发。核心功能是通过中心调度将用户请求分配到最优边缘节点，支持 302 重定向、DNS/GSLB、网关反代等多种接入方式。

**模块路径**：`github.com/darkinno/edge-dispatch-framework`

## 技术栈

| 组件 | 技术 |
|------|------|
| 语言 | Go 1.22 |
| HTTP 路由 | chi/v5 |
| 数据库 | PostgreSQL 16（pgx/v5） |
| 缓存/消息 | Redis 7（go-redis/v9） |
| ID 生成 | google/uuid |
| 容器化 | Docker Compose |
| 测试 | 标准 `testing` 包 |

## 目录结构

```
cmd/                    # 服务入口（main.go）
  control-plane/        # 控制面
  edge-agent/           # 边缘节点
  gateway/              # 网关（v0.3）
  dns-adapter/          # DNS 适配器（v0.2）
  origin/               # 示例源站
  stress/               # 压力测试

internal/               # 核心库（不可导出）
  auth/                 # HMAC Token
  config/               # 环境变量配置
  contentindex/         # Bloom Filter + 热内容索引
  controlplane/         # 控制面核心
  dns/                  # DNS 服务器
  edgeagent/            # 边缘节点核心
  gateway/              # 网关反代
  metrics/              # Prometheus 格式指标（v0.5）
  models/               # 共享数据模型
  quic/                 # HTTP/3（占位）
  store/                # PG + Redis 存储层
  streaming/            # HLS/DASH 流媒体
  tunnel/               # 反向隧道协议

docker/                 # Dockerfile
docs/                   # 设计文档
```

## 构建与测试命令

```bash
# 构建
make build

# 测试
make test                # 全部测试
make test-race           # 竞态检测
make test-cover          # 覆盖率
make bench               # Benchmark

# 集成测试（需要 Docker）
make integration-test

# 代码检查
make lint                # go vet

# Docker
make docker-up           # 启动全栈
make docker-down         # 停止并清理
```

## 代码规范

### 命名

- 包名：小写单词，无下划线（`controlplane`、`edgeagent`）
- 导出函数/类型：PascalCase，必须有注释
- 私有函数：camelCase
- 常量：PascalCase（`NodeStatusActive`）
- 接口：方法名或 `-er` 后缀（`TunnelManager`、`ContentIndexLookup`）

### 错误处理

```go
// 包装错误，提供上下文
if err != nil {
    return fmt.Errorf("save heartbeat: %w", err)
}

// 非关键错误，记录日志继续
if err := store.Upsert(ctx, data); err != nil {
    slog.Warn("upsert failed", "err", err)
}
```

### 并发

- 共享状态：`sync.Mutex` / `sync.RWMutex`
- 原子操作：`sync/atomic`
- 通信：`chan`
- 取消：`context.Context`
- 对象复用：`sync.Pool`

### 配置

所有配置通过环境变量加载，参见 `internal/config/config.go`。新增配置项需要：
1. 在对应 Config struct 添加字段
2. 在 `Load*()` 函数添加环境变量读取
3. 提供合理默认值
4. 安全敏感配置（密钥等）需要在 `warnInsecureDefaults()` 中检查

### API 设计

- REST/JSON 风格
- 路径格式：`/v1/<resource>/<action>`
- 错误返回统一结构：`{"error": {"code": "...", "message": "...", "request_id": "..."}}`
- 请求体大小限制：32KB
- Content-Type 校验

### 数据库

- 使用 pgx/v5 直接连接（无 ORM）
- 连接池配置：50 max / 10 min
- 批量操作使用 `pgx.Batch`
- 迁移在 `store.migrate()` 中执行（SQL DDL）
- 索引命名：`idx_<table>_<columns>`

### Redis

- 心跳存储使用 HSET + gzip 压缩（>256 字节）
- 批量操作使用 Pipeline
- 原子操作使用 Lua 脚本
- 事件使用 Pub/Sub

## 测试规范

### 单元测试

- 文件命名：`*_test.go`
- 函数命名：`Test<FuncName>`、`Benchmark<FuncName>`
- 使用 `t.Helper()` 标记辅助函数
- 使用 `t.Cleanup()` 清理资源
- 使用 `t.Parallel()` 并行执行（无共享状态时）

### 集成测试

- 文件命名：`integration_test.go`
- 需要 `TEST_PG_URL` 和 `TEST_REDIS_ADDR` 环境变量
- 使用 `t.Skip()` 跳过无环境的测试

### 测试数据

- 使用 `models` 包中的类型构造测试数据
- 不要依赖外部文件或网络
- 测试结束后清理数据库记录

## 当前版本状态

| 版本 | 状态 | 关键问题 |
|------|------|---------|
| v0.1 | ✅ 完成 | — |
| v0.2 | ✅ 完成 | — |
| v0.3 | ✅ 完成 | — |
| v0.4 | ⚠️ 部分完成 | HTTP/3 是占位符、DASH 硬编码 50 分段、预取 URL 硬编码 |
| v0.5 | ✅ 基本完成 | HTTP/3 占位、Helm Chart 未实现 |

## 已知 Bug（需优先修复）

- 无已知阻塞 Bug

## 新增功能指南

### 添加新的 API 端点

1. 在 `internal/models/models.go` 添加请求/响应类型
2. 在 `internal/controlplane/api.go` 添加 handler 函数
3. 在 `NewAPI()` 中注册路由
4. 编写单元测试

### 添加新的调度因子

1. 在 `internal/models/models.go` 添加数据字段（如需要）
2. 在 `internal/controlplane/scheduler.go` 的 `scoreKey()` 添加评分逻辑
3. 编写 scheduler 测试

### 添加新的存储表

1. 在 `internal/store/pg.go` 的 `migrate()` 添加 CREATE TABLE
2. 添加 CRUD 方法
3. 编写集成测试

### 添加新的服务

1. 在 `cmd/<service>/main.go` 创建入口
2. 在 `internal/<service>/` 实现核心逻辑
3. 在 `internal/config/config.go` 添加配置 struct 和 loader
4. 在 `docker/` 添加 Dockerfile
5. 在 `docker-compose.yml` 添加服务定义
6. 在 `Makefile` 添加构建/运行目标

## 文档更新

修改功能时需同步更新：
- `docs/API 设计.md` — API 变更
- `docs/设计文档.md` — 架构/模型变更
- `CHANGELOG.md` — 所有变更
- `requirements.md` — 需求变更
- `README.md` — 用户可见变更
