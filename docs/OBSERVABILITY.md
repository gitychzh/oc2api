# 可观测性：事件、账本与口径

> 目的：能回答"这个出口为什么在 14:32 被降权、前后是什么状态、哪个出口在失败"，
> 同时**绝不记录**消息内容、prompt、tools、`Authorization` 或任何 key。
>
> 现状：本仓库的观测是**进程内**的（内存环形缓冲 + stdout JSON 日志），不写磁盘
> 事件文件。需要长期留存的部署必须把 stdout 收走。

## 1. 三层观测

| 层 | 位置 | 回答什么 |
|---|---|---|
| **结构化日志** | stdout JSON | 单次事件的全貌：谁、何时、为什么 |
| **尝试记录** | `internal/telemetry` 内存环 | 一次客户端请求试过的每个出口、状态、耗时 |
| **聚合指标** | `internal/telemetry/metrics.go` | 请求量、Token、成功率、延迟分布 |

WebUI 的"诊断"页读的是后两层，"日志"面板读的是第一层的环形缓冲
（`logging.ring_size`，默认 2000 行）。

## 2. 结构化日志约定

每行一个 JSON，`slog` 输出。统一字段：

| 字段 | 含义 |
|---|---|
| `time` | RFC3339 |
| `level` | `DEBUG` / `INFO` / `WARN` / `ERROR` |
| `component` | `gateway` / `upstream` / `proxy` / `stream` / `policy` / `rotation` / `admin` |
| `event` | 稳定的机器可读事件名（见下表） |
| `request_id` | 串联一次客户端请求的所有尝试 |
| `msg` | 人读的一句话 |

**红线**：不含 `messages`、`content`、`prompt`、tools schema、`Authorization`、
API key、完整请求体。`SecretRedactor` 会对日志做最后一道脱敏。

### 2.1 事件名注册表

新增事件必须加进这张表，否则排障时没人知道该 grep 什么。

#### 请求与路由

| `event` | level | 含义 |
|---|---|---|
| `request_canceled` | DEBUG | 客户端在拿到响应前断开（不是上游故障） |
| `deterministic_rejection` | WARN | 上游判定请求本身无效，不再换出口重试 |
| `prompt_gate_refused` | INFO | **本地**字符门拒绝，未联系上游（耗时 < 10ms 可判别） |
| `stream_failed` | WARN | 下游流以错误结束 |
| `stream_collapse_failed` | WARN | 强制流式后合并回单文档失败 |
| `response_conversion_failed` | WARN | 跨协议响应转换失败 |
| `fallback_tier_skipped` | DEBUG | 回退 tier 的 wire 格式与请求不兼容，跳过 |

#### 上游尝试

| `event` | level | 含义 |
|---|---|---|
| `anonymous_attempt_succeeded` | DEBUG | 匿名出口成功 |
| `anonymous_attempt_transport_failed` | DEBUG | 匿名出口传输失败（会被降权） |
| `anonymous_attempt_response_failed` | DEBUG | 匿名出口返回错误状态（换下一个出口/上游） |
| `upstream_error_body` | INFO | 400 类错误体前 240 字符（已脱敏），用于看清这一类错误 |
| `reasoning_reference_retry` | INFO | 引用了上游已不认识的 reasoning item，剥掉后重放一次 |
| `reasoning_reference_retry_failed` | WARN | 上述重放也失败，返回原始错误 |

#### 出口与代理（两档判决）

| `event` | level | 含义 |
|---|---|---|
| `free_tier_ip_blocked` | WARN | 免费层判定出口 IP 滥用 → **cordon**，带 `block_for` |
| `anonymous_breaker_open` | WARN | 匿名车道熔断（连续失败，或全部出口被封锁） |
| `proxy_unavailable` | WARN | 代理不可达 → 键被迁走，带 `zen_keys_moved` / `go_keys_moved` |
| `proxy_restored` | INFO | 代理恢复 → 键迁回、匿名冷却清零 |
| `exit_rotation_started` | INFO | 开始换 IP，带 `exit`、`trigger`、`ip_from` |
| `exit_rotation_succeeded` | INFO | IP 已变，带 `ip_from` / `ip_to` / `elapsed_s` |
| `exit_rotation_budget` | INFO | 该出口当日 token 预算被跨过 |
| `exit_rotation_quota` | WARN | 硬 429 触发换 IP，带 `reason` |
| `exit_rotation_unchanged` | WARN | 重启后 IP 没变 —— **这是失败**，出口保持封锁 |
| `exit_rotation_failed` | WARN | agent 明确拒绝 |
| `exit_rotation_timeout` | WARN | 超时未恢复 |

> **降权（demote）当前不打日志**：它每个请求都可能发生，打日志会淹没有用信息。
> 它的可观测性来自尝试记录里的 `proxy` 字段与 `status`/`error`：同一出口连续
> 出现传输失败或 5xx，就是它正在被降权。

#### 配置与模型

| `event` | level | 含义 |
|---|---|---|
| `runtime_initialization_failed` | ERROR | 启动期构建 runtime 失败 |
| `shutdown_failed` | ERROR | 优雅关闭超时 |
| `server_started` / `server_failed` | INFO / ERROR | 监听器状态 |
| `model_catalog_refreshed` | INFO | 目录刷新（`/api/logs` 可查） |

## 3. 逐请求账本

### 3.1 为什么需要

只有决策事件回答不了"每个出口今天服务了多少请求、成功率多少、P95 多少"。
要求是**每出口 + 当时的状态**的逐请求统计，覆盖全部失败形态。

### 3.2 埋点位置

选出口发生在 `internal/gateway/pool.go`（`anonymousCursor.Next()` 返回节点），
请求收尾在 `upstream.go` 的尝试循环。所以埋点必须在**尝试的生命周期**里完成，
而不是在某个边缘 LB 上。

现状：`internal/telemetry/metrics.go` 的 `UpstreamRequest` / `UpstreamAttempt`
两条环形缓冲已覆盖这一点：

- `UpstreamRequest`：一次客户端请求一行（模型、tier、结局、Token、出口）。
- `UpstreamAttempt`：一次上游尝试一行（出口、状态、耗时、Key、匿名标记）。

保留窗口：最近 1 小时最多 10,000 请求 / 20,000 尝试；接口各返回最多 500 条。

请求级 `outcome` 由 `requestOutcome` 按状态码推出 `success` / `client_error` /
`server_error` / `not_routed`（从未到达上游）/ `unknown`；handler 显式设置时覆盖为
`client_canceled` / `stream_error`。

### 3.3 尝试记录字段（推荐 schema）

```json
{
  "time": "2026-11-26T08:42:15.832Z",
  "request_id": "req_7f43a7e2b3a4",
  "model": "big-pickle",
  "tier": "zen",
  "attempt": 2,
  "key_id": "anonymous",
  "channel": "anonymous",
  "anonymous": true,
  "proxy_node": "http://1.2.3.4:8443",
  "status": 429,
  "duration_ms": 842,
  "success": false,
  "outcome": "retryable_failure"
}
```

| 字段 | 说明 |
|---|---|
| `request_id` | 串联同一次客户端请求的多腿 |
| `proxy_node` | **必须**：选中出口（URL 已脱敏，凭据不出现）；无出口时为 `unavailable` |
| `status` | 无 HTTP 响应（超时/网络）时为 0 |
| `duration_ms` | 单次尝试耗时 |
| `success` | 是否 2xx |
| `outcome` | `success` / `retryable_failure` / `transport_error` / `rejected` |

**协议不在尝试记录里**：它在 `RequestMeta.Protocol`（请求级），因为同一请求的
回退 tier 可能换协议，而尝试记录只回答"这一腿打到了哪个出口、结果如何"。
传输错误的细分（`classifyAttemptError`）只写进请求日志的 `legs` 摘要
（形如 `anon:timeout:30001ms@http://…`），不单独建字段。

> 红线同上：不含消息内容、prompt、tools schema、凭据。

### 3.4 失败分类

`outcome` 只有四档（`internal/gateway/upstream.go` 的 `recordUpstreamAttempt`）：

| 场景 | `outcome` | `status` |
|---|---|---|
| 2xx | `success` | 200–299 |
| 有 HTTP 响应但非 2xx，且非"不可重试" | `retryable_failure` | 4xx / 5xx |
| 无 HTTP 响应（超时、连接失败、TLS EOF） | `transport_error` | 0 |
| 有 HTTP 响应但属不可重试的客户端错误 | `rejected` | 4xx |

**归因时不要只看 status**：429 有四个互斥成因（见 `PROTOCOL_FACTS.md` §1）。
账本只能告诉你"这个出口这一刻 429"，判断是不是真配额必须配合同窗对照。

## 4. 聚合指标口径

### 4.1 Token 计数

- 只统计**上游报告值**，不估算缺失用量。
- 流式请求的上游不报 usage 时，网关补的估算帧标记 `estimated: true`。
  遥测里它与真实账单是**不同口径**，聚合时不得混算。
- Playground 的用量单独归类，不混进生产流量。

### 4.2 请求结果与尝试指标是不同口径

- **请求结果**：客户端最终拿到什么（成功/失败/取消）。
- **尝试指标**：每个出口每次尝试的结果，含最终没被选中的。

一个请求可能产生 5 次尝试记录但只有 1 条请求记录。看成功率时先确认自己在看哪个。

### 4.3 健康检查

`GET /healthz` 不发上游请求、无需 Key：

- 目录未就绪/为空、WARP 不可用、必要代理全部不健康 → **503**
- 可用但过期 → **200** + `status: "degraded"`（过期阈值 = 2 倍刷新间隔，最低 60s）

**就绪不保证 Key 有效、额度充足或推理成功。** 公开响应只含计数与状态，
不含凭据或代理地址。

配了 `egress_agents` 时响应多一个 `rotation` 数组：每个出口的当日 token 用量、
是否已触发、是否正在换 IP、上次结果与 IP。它是排"这个出口今天为什么不动了"的
第一手数据。

## 5. 排障时的查询路径

| 问题 | 看哪里 |
|---|---|
| 客户端 401 | `server_keys`、启用状态、请求头 |
| 启动报缺凭证 | 配置上游 Key / 已有 OAuth / 显式 `anonymous: true` |
| 无模型 / health 503 | 目录状态、原生协议支持、凭证配置、资源可用性 |
| HTTP 200 但生成失败 | SSE 错误事件、请求结果、`x-request-id` 对应的日志 |
| 429 | **按六道门顺序排**（`PROTOCOL_FACTS.md` §1），别直接怀疑配额 |
| 出口一直 429 但没换 IP | 看 `rotation[]`：`triggered=true` 且 `rotations` 不涨 = 换 IP 失败；`last_result` 说原因 |
| 换 IP 后还是 429 | `exit_rotation_unchanged` = ISP 发了同一个地址；或撞的是六道门里换 IP 治不好的那几道 |
| 本地 400 vs 上游 400 | 耗时（ms vs 秒）+ 是否有 `prompt_gate_refused` |
| 修改无效 | 保存/重载实际配置；监听变化需重启 |
| 重启后用量清零 | 计数器仅在内存，日志需另外保存 |

## 6. 明确不做

- **不写磁盘事件文件**：保持单进程无状态依赖。需要长期留存就收集 stdout。
- **不做逐请求持久化账本**：成本高于收益，且容易变成记录用户内容的地方。
- **不在日志里打请求体**（除 `dump_request_bodies` + `debug` 双开，且截断脱敏）。
- **不根据估算 Token 判配额**：估算与账单是两个口径。
