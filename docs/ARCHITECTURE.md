# 架构（ARCHITECTURE）

> 本文描述 `oc2api` 的代码地图、数据流、选路与出口池语义，以及关键决策的理由。
> 协议事实（免费层六道门、429 归因）不在本文复述，见
> [`PROTOCOL_FACTS.md`](PROTOCOL_FACTS.md)。

## 1. 进程拓扑

单进程，两个监听：

```
客户端 (OpenAI / Anthropic 兼容)
   │
   ▼  Authorization: Bearer <server_key>
┌───────────────────────────────────────────────┐
│ :8080  API 监听                                │
│   Gateway.Handler()                            │
│     GET  /v1/models                            │
│     POST /v1/chat/completions   (Chat)         │
│     POST /v1/responses          (Responses)    │
│     POST /v1/messages           (Anthropic)    │
│     POST /v1/systemone          (透传，不转换) │
│     GET  /healthz               (无需认证)     │
└───────────────────┬───────────────────────────┘
                    │ 选路 → 组装 body → 出口池 → 上游
                    ▼
        opencode.ai/zen  |  /zen/go  |  /inference
                    ▲
┌───────────────────┴───────────────────────────┐
│ :8081  WebUI 监听（可关闭）                    │
│   internal/admin  管理 API + 内嵌静态资源      │
└───────────────────────────────────────────────┘
```

两个监听都是可热改的，但 `listen` / `webui.listen` / `webui.enabled` **需要重启**，
其余配置热应用。

## 2. 两条 lane：按客户端代际分，不是按 URL 分

这是本项目最容易看错的一处。`internal/gateway/gateway.go` 的包注释写得最清楚：
车道名来自**它期待哪种 OpenCode 客户端形状**，不是来自 URL 路径。

| lane | origin | 期待的客户端形状 |
|---|---|---|
| **v1** | `https://opencode.ai/zen`（`models.V1Base`，也是 `upstream.zen` 默认值） | OpenCode **1.x**：1.x UA、`x-opencode-request` 头、git-commit 形状的 project、1.x 工具集（`opencode_tools_v1.json`） |
| **v2** | `/inference`（`upstream.anonymous`）与 `/zen/go`（Go 套餐） | OpenCode **2.x** CLI 形状，**拒绝** `x-opencode-request` 头 |

判定函数是 `models.IsV1Upstream(baseURL)`。匿名免费层**只用 v2 的
`/inference`**——1.x 的 `/zen` 车道对免费模型已退役，代码里保留但注释掉了。

> 两条 lane 的差异不是 cosmetic：同一个 body 发错车道会拿到 403（形状门）或
> 429（body 门），而错误形态和配额耗尽完全一样。见 `PROTOCOL_FACTS.md` §1。

## 3. 三个 tier：凭证决定走哪条

| tier | 凭证 | 说明 |
|---|---|---|
| `zen` | `zen_keys` 或 `zen_oauth` | 手动 Zen Key（PAYG）或 Zen OAuth |
| `go` | `go_keys` 或 `go_oauth` | Go 套餐 |
| 匿名 | 共享凭证字面量 `"public"` | `anonymous: true` 才启用 |

`prefer` 决定两者都能服务同一模型时先试谁。

**OAuth 另有硬规则**：请求选择 OAuth 路由后，只使用该 tier 已确认的路由，
**不因失败改走匿名、手动 Key 或其他 tier**。Go OAuth 授权失败、额度耗尽或
429 均不会触发 PAYG。

## 4. 请求生命周期

```
1. authenticate()            校验 server_key → 指纹进 context
2. 读 body（上限 32 MiB）→ JSON 解析
3. withClientEnvironment()   取/剥 client_environment 扩展字段
4. rotation.GroupID(model)   命中 sota/sweet 别名 → handleRotation 分支
5. catalog.Supported(model)  未知模型 404；已知但不可服务 400
6. routeForModel()           建 Route：tier + 每 tier 的原生协议 + 回退计划
7. prepareRouteBodies()      每 tier 一个 body（跨 tier 协议可能不同！）
8. enforcePromptGate()       免费层字符门本地拦截（见 policy.go）
9. identity.DeriveRequestIDs()  派生上游 session/request 身份
10. doUpstream()             出口池选路 → 上游尝试循环
11. 流式：ForwardStream / TranscodeStream；非流式：读全量 → 可选 CollapseStream
```

第 7 步值得强调：**Zen 与 Go 对同一模型的原生协议可以不同**（目录里 MiniMax
在 Zen 是 Chat、在 Go 是 Messages）。所以重试跨 tier 时必须重新编码请求体，
`Route.Protocols` 就是为此存在的 map。

第 8 步是本仓库相对上游 fork 新增的一层：免费层的字符门是字符规则，任何 token
窗都表达不了它，所以必须在本地按字符量。

## 5. 热重载：建新弃旧

`internal/gateway/runtime.go` 的 `RuntimeController`：

```
校验 → 构建候选 runtime（新 Gateway 实例）→ 持久化 → 切换
```

- 任一步失败：**旧 runtime 继续服务**，客户端无感。
- 成功：新 runtime 接管，旧 runtime 的 `cancel()` 停掉它的后台代理检查与模型刷新。
- `ApplyResult.RestartRequired` + `RestartFields` 告诉 UI 哪些字段只重启生效。

`Gateway` 本身**不可变**：应用新配置就是建一个新实例，而不是改旧实例的字段。
这样并发请求永远看到自洽的一份配置。

文件手动改动后必须 `POST /api/config/reload` 或点 UI 的"从磁盘重载"——
**没有自动文件监视器**。

## 6. 出口池语义

这是本仓库与上游 fork 差异最大的部分，也是从实战里长出来的。

### 6.1 三种节点

| 节点 | 说明 |
|---|---|
| 静态代理 | 有健康跟踪、连接复用、可被 Key 绑定 |
| `#pool` / `pool+` 代理 | 供应商负责轮换出口。每次尝试新建连接，不复用 keep-alive/HTTP2，不参与冷却与健康淘汰 |
| `direct` | 直连 |

### 6.2 两种判决，必须分开

| 判决 | 触发 | 效果 | 谁能解除 |
|---|---|---|---|
| **cordon（封锁）** | 只有免费层能下：`429 free_quota_exhausted`、`403 free_tier_rejected`（IP 滥用） | 出口完全退出轮换 | 窗口到期，或观测到出口 IP 变化 |
| **demote（降权）** | 传输失败（任意次数）、5xx | 出口**仍在轮换里**，只是排到选择顺序末尾；当前请求由别的出口答完 | 一次成功立即清零 |

**传输失败永不 cordon。** 这条是从事故里学来的：把 LTE 丢包当成额度判决，会
触发无谓的换 IP 并打散隧道，而真实原因是这条腿暂时不通。

选择顺序：

```
正常健康出口 → 降权出口 → last-resort 出口 → cordon 出口
```

配置项（`performance.*`）：

| 键 | 默认 | 含义 |
|---|---|---|
| `exit_demote_seconds` | 20 | 降权基础窗口，按连续失败翻倍 |
| `exit_demote_max_seconds` | 300 | 降权窗口上限 |
| `free_tier_block_seconds` | 900 | cordon 窗口（IP 判决） |
| `max_anonymous_exits` | 8 | 单请求可走的不同静态出口上限 |

实现：`internal/gateway/pool.go` 的 `proxyTransport.demotedUntil` /
`markDemoted` / `clearDemotion`，以及 `anonymousCursor.Next()` 的两级排序。

### 6.3 匿名车道熔断

连续失败达阈值后暂停**整条**匿名车道，退避按指数增长（默认 6 次触发、1 小时起、
最长 24 小时）。所有出口都被 IP 封锁时也直接熔断，不再重复走一遍 403 路径。

不配置 `models.anonymous_backoff` 则关闭该通道级熔断。

## 7. 协议转换层

`internal/protocol` 用统一的 `bridgeBlock` 桥接模型做三协议互转：

```
Chat / Responses / Anthropic  ──decode──▶  bridge  ──encode──▶  Chat / Responses / Anthropic
```

设计要点：

- **有损是明确的**：跨协议只转已有对应表示的东西。Chat 的 `seed`、JSON 输出
  约束不转成 Anthropic 参数；转不了的内容报校验错误而不是静默丢弃。
- **System One 从不转换**：它是 OpenCode Zen 的结构化决策端点，载荷与消息协议
  无共同形状，只原样透传。在消息端点上收到决策载荷时也原样转发（`gateway.go`
  的 `forwardSystemOne` 分支），这样只认 Chat/Responses 的客户端也能用它。
- **reasoning 历史需要修补**：`moonshot/kimi/deepseek/mimo` 系上游会拒绝没有
  reasoning 历史的 tool-call 轮次，`bridge.go` 的
  `normalizeToolReasoningHistory` 补占位符。
- **流式 usage 需合成**：见 `PROTOCOL_FACTS.md` §9。

## 8. 免费层整形

免费模型只接受 agent 形状的流量，所以网关按需注入官方 system prompt + 官方工具
定义，改写工具名，并在响应里还原客户端名称。

| 组件 | 文件 |
|---|---|
| 官方 prompt 渲染（模型名/日期/环境） | `gateway/opencode_prompt.go` |
| 工具定义注入与改名 | `gateway/toolshape.go` |
| 整形后的 body 编码（客户端 key 顺序） | `gateway/upstream.go` 的 `marshalV2ChatBody` |
| 响应侧工具名还原 | `protocol` 的 `RenameResponseTool` / `RenameStreamReader` |

**整形是有副作用的**：注入的 prompt 与工具 JSON 都算进字符门（§7 of
PROTOCOL_FACTS）。所以整形省下的每一个字符都直接换成客户端的可用对话预算。

## 9. 观测

| 数据 | 位置 | 生命周期 |
|---|---|---|
| 配置及上一版本 | `config.json`、`config.json.bak` | 持久 |
| 模型目录缓存 | `config.json.models.catalog.json` / `.dev.json` | 持久 |
| 可用性、轮转状态 | `config.json.<fingerprint>.availability.json`、`.rotation.json` | 持久 |
| Session、用量、近期请求/尝试、日志环、Playground 结果 | 内存 | 进程重启清空 |
| 结构化日志 | stdout JSON | 需外部收集 |

口径细节见 [`OBSERVABILITY.md`](OBSERVABILITY.md)。

## 10. WARP 出口

`warp.enabled` 默认 `false`。开启后免费匿名/手动 Zen 流量走进程内 Cloudflare
WARP 隧道（userspace WireGuard + gVisor netstack），**不需要 TUN 设备、root 或
改路由表**：UDP socket 承载 WireGuard 包，netstack 在进程内终结 TCP。

- 需要向 Cloudflare `2408` 端口发 UDP。网络阻断该流量时无法初始化。
- `warp.pool_size` 1–32（默认 2），`warp.stack` = `ipv6`/`ipv4`/`dual`。
- `warp.source_ipv6` 要求 /64 或更短前缀，且 Windows 上地址必须**已分配到网卡**
  ——程序不配置主机网络。
- 换出口不保证拿到新额度；不给 `source_ipv6` 时所有出口共享一个公网地址，
  egress-scoped 的免费额度照样是耗尽的。

Go/付费/OAuth 路由**继续用配置的代理**，不走 WARP。

## 11. 关键决策（ADR 摘要）

| 决策 | 结论 | 理由 |
|---|---|---|
| 语言 | Go，单二进制 + 内嵌 WebUI | 无运行时依赖；交叉编译简单；并发模型适合长连接流式 |
| HTTP 客户端 | 自研 `internal/httpx`，uTLS 伪装 Bun | 上游按 TLS 栈判定，标准库必被 429（PROTOCOL_FACTS §3） |
| 配置热重载 | 建新 Gateway 实例，不原地改 | 并发请求永远看到自洽配置；失败可完整回滚 |
| 免费层字符门 | 本地按字符拒，文案复刻上游 | 不烧配额、不等往返，且保持 opencode 的压缩触发形状 |
| 传输失败 vs IP 判决 | 分离为 demote / cordon | 合并会让丢包看起来像配额耗尽，触发无谓换 IP |
| 流式 usage | 只在 Chat 合成，标记 `estimated` | Anthropic/Responses 的 usage 嵌在终止结构里，硬塞会破坏流 |
| 未知模型 | 不发明任何上限 | 给不认识的模型编数字是拒绝合法流量的起点 |
| 测试 | 沿用上游的 test-free | 保持与上游 fork 的可合并性；靠 vet/编译/真实请求验证 |

## 12. 与上游 fork 的边界

本仓库是 `jasonxu114514/opencode2api` 的 fork。本地新增集中在独立文件：

| 文件 | 内容 |
|---|---|
| `internal/models/profile.go` | 字符门常量、每模型实测门、估算器入口 |
| `internal/gateway/policy.go` | 字符门统计与本地 400 |
| `internal/protocol/estimate.go` | 补 usage 的密度系数 |
| `internal/gateway/pool.go`（增量） | 降权判决 |
| `AGENTS.md`、`docs/*` | 本套文档 |

上游改动频繁（免费层校验在持续变化），所以改动尽量落在这几个文件里，减少合并
冲突；必须改上游文件时保持改动小且自洽。
