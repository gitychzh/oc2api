# AGENTS.md — 本仓库维护约定（改代码前必读）

> **文档地图（同一事实只在一处维护）**：
> - 本文件 = **协议事实与安全红线**（硬约束）+ 代码分层速查
> - [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) = 代码地图、数据流、选路与池语义、ADR
> - [`docs/PROTOCOL_FACTS.md`](docs/PROTOCOL_FACTS.md) = 免费层六道门、429 归因红线、实测边界
> - [`docs/MODEL_PROFILE.md`](docs/MODEL_PROFILE.md) = 每个免费模型的上限、思考档位、实测边界
> - [`docs/OBSERVABILITY.md`](docs/OBSERVABILITY.md) = 事件流、逐请求账本、指标口径
> - [`docs/management.zh-CN.md`](docs/management.zh-CN.md) = 管理台与运行指南（原有）
> - [`README.zh-CN.md`](README.zh-CN.md) = 对外说明

## 项目概述

`oc2api`（上游 `jasonxu114514/opencode2api`）：Go 写的 OpenCode Zen / Zen Go
网关。客户端用 OpenAI Chat Completions、OpenAI Responses 或 Anthropic Messages
进来，网关选一条可用上游链路并在协议不同时双向转换。管理 WebUI 编译进二进制，
运行时不需要 Node 或数据库。

## 硬约束（不可违反）

### 1. 免费层是**多层门**，各门独立

顺序为：**形状 → 传输栈 → session → body → 模型 → 真实配额**。看到 403/429/401
先按这个顺序排，不要跳到"配额用完"。逐门细节与实测边界见
[`docs/PROTOCOL_FACTS.md`](docs/PROTOCOL_FACTS.md)。

一句话版本：

- **形状门（403）**：免费层只接 agent 形状的流量——至少 4 个 distinct 工具名、
  `stream: true`。schema/description 内容不校验。
- **传输栈门（429，伪装成配额）**：上游按 TLS/HTTP 栈判定。生产出网必须走
  `internal/httpx/bun.go` 的 uTLS+Bun 伪装栈；标准库 transport 会被判 429。
  **换 IP 对这条无解。**
- **session 单次使用**：`x-opencode-session` 被消费后重放会回同款 429。网关每次
  上游试探都必须重新 mint（见 `internal/identity`）。
- **body 门（快照新鲜度）**：注入的官方 prompt 必须每请求现场渲染——`Today's
  date:` 停在抓包当天就会 429。`internal/gateway/opencode_prompt.go` 的
  `opencodeEnvironment` 负责刷新模型名与日期，**不得改成固定字符串或缓存**。
- **模型门（401）**：免费层只服务免费裸 ID。目录里免费判据只有 `-free` 后缀与
  `big-pickle`；`cost` 字段在匿名视图里对付费条目也全是 0，不能用来判免费。
- **prompt 字符门（400）**：四个已二分模型的真实上限是
  `round(chars * 0.33966) <= 32000`，约 **94,200 字符**，与模型/语言/内容类型
  无关，且**含注入的内置工具 JSON**。这是字符门，不是 token 窗。网关在本地按它
  拒绝（`internal/gateway/policy.go`），不烧一次出口租约。

### 2. 429 归因红线

`429 FreeUsageLimitError` 有**四个互斥成因且响应完全同形**：①传输栈
②body 快照 ③session 复用 ④真实 IP 配额。**换出口只对 ④ 有效**。把 ①②③ 当 ④
处理会触发无谓的换 IP 并打散隧道。

排障顺序固定：**传输 → prompt 新鲜度 → session → 才怀疑 IP 配额**。

### 3. 回环与上游限定

- `upstream.zen` / `upstream.go` / `upstream.anonymous` 只允许 `opencode.ai`。
- 管理监听默认只应绑 loopback；对外暴露必须加 HTTPS 反代并限制来源。
- `internal/config` 强制校验；新增上游入口必须先过这道校验。

### 4. 日志红线

绝不记录消息内容、prompt、tools 详情、`Authorization`、API Key 或任何 key。
请求观测只记路由、时延、状态码、异常分类等非敏感字段。
`logging.dump_request_bodies` 默认关闭，且需 `debug` 级别才输出。
`internal/config` 的 `SecretRedactor` 必须覆盖所有新增的敏感字段。

### 5. 凭据不入仓库

`config.json`、`config.json.bak`、OAuth Token、代理密码一律不进仓库（已在
`.gitignore`）。只有管理员密码是 Argon2id 哈希，其余都是明文——分享源码时排除
运行文件。

### 6. 未知模型不发明上限

给不认识的模型编数字，是网关开始拒绝合法流量的起点。`internal/models/profile.go`
的 `promptGates` 只收录真正二分过的 ID；没测过的留空 = 不启用字符门。

## 工程化门禁（必须全绿）

```bash
go vet ./...
go build -trimpath ./cmd/opencode2api
gofmt -l ./cmd ./internal ./webui     # 必须输出为空
for f in webui/js/*.js webui/js/pages/*.js; do
  node --input-type=module --check < "$f" || exit 1
done
```

CI（`.github/workflows/checks.yml`）在 ubuntu 与 windows 上跑 `go vet` +
`go build`，另有 gofmt 与 WebUI 语法检查。门禁**不得**写成"未安装则跳过"。

> 注意：上游 fork 刻意保持 **test-free**（提交 `chore: drop _test.go files to
> keep the released tree test-free`）。本仓库沿用这一约定，改动靠 `go vet`、
> 编译和真实上游请求验证，不新增 `_test.go`。

## 代码分层速查

| 路径 | 职责 |
|---|---|
| `cmd/opencode2api` | 入口：加载配置 → RuntimeManager → API + WebUI 两个 http.Server |
| `internal/gateway` | 核心。`Gateway`（一配置一实例，热重载=建新弃旧）、`runtime.go` 原子切换、`upstream.go` 实际上游请求、`pool.go` 节点/代理池、`policy.go` 本地字符门、`toolshape.go` 免费层整形、`abuse.go` 风控分类、`warp.go` |
| `internal/protocol` | 三协议互转：`bridgeBlock` 桥接模型 + SSE emitter/parser；`estimate.go` 补 usage 的密度系数 |
| `internal/models` | 模型发现（`models.opencode.ai/api.json` + zen/go 文档表）、`Route` 路由计划、价格、可用性、`profile.go` 实测参数 |
| `internal/config` | 校验/规范化/持久化（`//` 注释、未知字段拒绝、敏感值脱敏、Argon2id） |
| `internal/httpx` | 自研 HTTP 客户端：`bun.go` 用 uTLS 伪装 Bun 运行时的 TLS 指纹 |
| `internal/warp` | 进程内 Cloudflare WARP（userspace WireGuard + gVisor netstack），不需要 TUN/root |
| `internal/oauth` / `identity` / `rotation` / `telemetry` / `admin` / `jsonutil` | 设备码 OAuth、会话身份派生、SOTA/Sweet 轮转、监控日志、管理 API、JSON 工具 |
| `webui` | 原生 JS 无构建，`embed.FS` 打进二进制，8 个页面，4 语言 |

## 开发规范

- **最小改动**：新增协议 → `internal/protocol` 转换器 + 现有路由；新增上游模式 →
  `internal/gateway` 里加一条 lane，并在 `models.IsV1Upstream` 判定处登记。
- **保持一致**：改动需与周边结构、命名、注释风格一致；注释解释**为什么**，不复述
  代码在做什么。
- **文档同步**：影响协议事实时同步 `docs/PROTOCOL_FACTS.md`；影响模型参数时同步
  `docs/MODEL_PROFILE.md`；影响拓扑或端口时同步 `docs/ARCHITECTURE.md`。
- **可观测**：新增失败分类必须能在 `internal/telemetry` 的尝试记录里区分，
  口径见 [`docs/OBSERVABILITY.md`](docs/OBSERVABILITY.md)。
- **Conventional Commits** + 语义化版本；发布用 `-ldflags "-X main.version=vX.Y.Z"`。

## 与上游 fork 的关系

本仓库是 `jasonxu114514/opencode2api` 的 fork，GPL-3.0-or-later。上游改动频繁
（免费层校验在持续变化），所以：

- 本地改动尽量集中在**独立文件**（`policy.go`、`profile.go`、`estimate.go`、
  本文档与 `docs/` 新文件），减少与上游合并时的冲突面。
- 需要改上游文件时，改动保持小且自洽，并在提交信息里写明理由。
- 上游 tag/分支合并前，先跑一遍上面的门禁，再用真实上游请求验证免费层仍可用。
