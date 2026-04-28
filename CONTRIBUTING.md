# 贡献指南（Contributing）

感谢你对 Edge Dispatch Framework 的兴趣！本文档说明如何参与贡献。

## 开发流程

本项目采用 **"协议先行"** 的方式演进：

1. **文档 PR** — 先在 `docs/` 更新接口/时序/边界设计
2. **代码 PR** — 实现最小可用版本
3. **测试 PR** — 补齐示例与回归用例

## 环境准备

### 前置条件

- Go 1.22+
- Docker & Docker Compose
- Git

### 本地开发

```bash
# 克隆仓库
git clone https://github.com/DarkInno/Edge-Dispatch-Framework.git
cd Edge-Dispatch-Framework

# 安装依赖
go mod download

# 构建所有服务
make build

# 运行测试
make test

# 启动基础设施（PostgreSQL + Redis）
docker compose up -d postgres redis

# 启动控制面
make run-cp

# 另一个终端启动边缘节点
make run-ea

# 另一个终端启动源站
make run-origin
```

### 全栈启动

```bash
docker compose up -d
```

## 分支规范

| 分支类型 | 命名格式 | 说明 |
|---------|---------|------|
| 主分支 | `main` | 稳定版本，保护分支 |
| 开发分支 | `feat/<功能名>` | 新功能开发 |
| 修复分支 | `fix/<问题描述>` | Bug 修复 |
| 文档分支 | `docs/<文档名>` | 文档更新 |
| 优化分支 | `perf/<优化项>` | 性能优化 |

## 提交规范

使用 [Conventional Commits](https://www.conventionalcommits.org/) 格式：

```
<type>(<scope>): <description>

[optional body]

[optional footer]
```

### Type 类型

| Type | 说明 |
|------|------|
| `feat` | 新功能 |
| `fix` | Bug 修复 |
| `docs` | 文档更新 |
| `style` | 代码格式（不影响逻辑） |
| `refactor` | 重构（非新功能、非修复） |
| `perf` | 性能优化 |
| `test` | 测试补充/修正 |
| `chore` | 构建/工具/依赖变更 |
| `ci` | CI/CD 配置 |

### Scope 范围

| Scope | 说明 |
|-------|------|
| `controlplane` | 控制面 |
| `edgeagent` | 边缘节点 |
| `gateway` | 网关 |
| `dns` | DNS 适配器 |
| `tunnel` | 隧道 |
| `streaming` | 流媒体 |
| `auth` | 鉴权 |
| `store` | 存储层 |
| `config` | 配置 |
| `models` | 数据模型 |
| `docker` | Docker 相关 |
| `api` | API 接口 |

### 示例

```
feat(scheduler): add content-aware scoring for bloom filter hits
fix(fetcher): prevent path traversal in origin URL construction
docs(api): update dispatch resolve request/response examples
perf(store): add connection pool read replica support
test(tunnel): add bidirectional streaming integration test
```

## 代码规范

### Go 风格

- 遵循 [Effective Go](https://go.dev/doc/effective_go) 和 [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)
- 使用 `gofmt` 格式化代码
- 使用 `go vet` 静态检查
- 导出函数/类型必须有注释
- 错误处理不要忽略，至少记录日志

### 命名规范

| 类型 | 规范 | 示例 |
|------|------|------|
| 包名 | 小写单词，不带下划线 | `controlplane`、`edgeagent` |
| 导出函数 | PascalCase | `NewScheduler()`、`Resolve()` |
| 私有函数 | camelCase | `scoreKey()`、`filterNodes()` |
| 接口 | 方法名或 `-er` 后缀 | `TunnelManager`、`ContentIndexLookup` |
| 常量 | PascalCase | `NodeStatusActive`、`MsgTypeRegister` |

### 错误处理

```go
// 好：包装错误，提供上下文
if err != nil {
    return fmt.Errorf("save heartbeat: %w", err)
}

// 好：记录日志后继续
if err := h.contentStore.UpsertSummary(ctx, *req.ContentSummary); err != nil {
    slog.Warn("content summary upsert failed", "node_id", req.NodeID, "err", err)
}

// 坏：忽略错误
result, _ := doSomething()
```

### 并发安全

- 共享状态使用 `sync.Mutex` 或 `sync.RWMutex`
- 原子操作使用 `sync/atomic`
- 通道用于 goroutine 间通信
- `context.Context` 用于取消传播

## 测试要求

### 单元测试

```bash
# 运行所有测试
make test

# 运行特定包的测试
go test ./internal/controlplane/... -v

# 运行特定测试
go test ./internal/auth/... -run TestSign -v

# 竞态检测
make test-race

# 覆盖率
make test-cover
```

### 集成测试

集成测试需要 Docker 环境（PostgreSQL + Redis）：

```bash
make integration-test
```

### Benchmark

```bash
make bench
```

### 测试文件命名

- 测试文件：`*_test.go`
- Benchmark 文件：`*_bench_test.go`
- 集成测试：`integration_test.go`（需要 `TEST_PG_URL` 环境变量）

## 提交 PR

### PR 检查清单

- [ ] 代码通过 `make lint`（`go vet`）
- [ ] 代码通过 `make test`
- [ ] 新功能有对应测试
- [ ] 文档已更新（如涉及 API 变更）
- [ ] Commit message 符合规范
- [ ] PR 描述说明变更内容和原因

### PR 描述模板

```markdown
## 变更内容

简要描述本次变更。

## 变更类型

- [ ] 新功能
- [ ] Bug 修复
- [ ] 文档更新
- [ ] 性能优化
- [ ] 重构
- [ ] 测试补充

## 测试

描述如何验证本次变更。

## 关联 Issue

Closes #<issue_number>
```

## 报告 Bug

使用 GitHub Issues 报告 Bug，请包含：

1. **环境信息**：Go 版本、OS、Docker 版本
2. **复现步骤**：最小化复现路径
3. **期望行为**：应该发生什么
4. **实际行为**：实际发生了什么
5. **日志/错误信息**：相关日志输出

## 功能建议

功能建议请通过 GitHub Issues 提交，使用 `enhancement` 标签。建议包含：

1. **问题描述**：当前有什么问题
2. **期望方案**：你希望如何解决
3. **替代方案**：是否考虑过其他方案
4. **版本关联**：建议归属哪个版本（v0.1~v0.5）

## 架构决策记录

重大架构变更请在 `docs/` 下创建 ADR（Architecture Decision Record）：

```markdown
# ADR-NNNN: <决策标题>

## 状态
提议 / 已接受 / 已废弃

## 背景
描述决策背景和上下文。

## 决策
描述做出的决策。

## 影响
描述决策的影响和副作用。
```

## 行为准则

- 尊重每一位参与者
- 以建设性方式提出批评
- 聚焦于技术讨论
- 欢迎不同背景的贡献者

## License

贡献代码默认使用项目相同的 [MIT License](LICENSE)。
