# 协议事实：免费层的六道门与 429 归因

> 本文是**协议事实的唯一权威**。`AGENTS.md` 只给一句话版本，这里给实测证据、
> 边界数值和排障顺序。改动涉及免费层行为时，先改这里再改代码。
>
> 全部结论来自活体复测，不是从文档抄的。日期标注在每条后面；上游校验在持续
> 变化，**旧结论过期前必须重新实测**，不能靠"以前能过"推断。

## 1. 六道门与它们的顺序

免费层不是一道门，是六道**独立**的门。命中顺序固定为：

```
形状 → 传输栈 → session → body → 模型 → 真实配额
```

| 门 | 失败形态 | 换出口有用？ | 换 body/重 mint 有用？ |
|---|---|---|---|
| 形状 | 403 `FreeTierError` | 否 | 是 |
| 传输栈 | 429（伪装成配额） | **否** | 否（只能换 TLS 栈） |
| session | 429（伪装成配额） | 否 | 是（重新 mint） |
| body | 429（伪装成配额） | 否 | 是（现场渲染 prompt） |
| 模型 | 401 `Missing API key` | 否 | 是（换免费裸 ID） |
| 真实配额 | 429 `FreeUsageLimitError` | **是** | 否 |

**这张表就是排障顺序**。看到一个 429 就往"配额"想，是这类网关最常见的误判。

## 2. 形状门（403）

免费层只接受 **agent 形状**的流量：

- 至少 **4 个 distinct 工具**，且 `function.name` 属于内置集
  （`bash/edit/glob/grep/read/skill/task/todowrite/webfetch/websearch/write`，
  本仓库 2.x 车道用 `edit/execute/glob/grep/question/read/shell/skill/subagent/
  webfetch/websearch/write/patch`）。
- 必须 `stream: true`。

**schema/description 内容不校验**：4 个真名 + 空 schema 也能 200（实测
2026-10-29）。UA 版本、头名、`b3`/`traceparent`/`x-opencode-client` 的值都不敏感。

反过来：1/2/3 个真名、掺入改名工具、`stream:false` → 403。

实现：`internal/gateway/toolshape.go` 的 `renameAndCompleteTools`，资产是
`opencode_tools.json`（2.x 车道）与 `opencode_tools_v1.json`（1.x 车道），都是
从官方客户端抓的真实定义。

> 省 prompt 的推论：既然只数名字，注入**全部 builtin 名 + 空 schema**（每项
> ~110 字符）比注入完整定义省下约 24K 字符。这在 94.2K 字符门（§6）下直接换成
> 可用对话预算。

## 3. 传输栈门（429）

上游按 **TLS/HTTP 栈**判定。同一新出口 IP、同一分钟内的字节级对照：

| 栈 | 结果 |
|---|---|
| Bun（boringssl / HTTP2） | **200** |
| httpx（OpenSSL / HTTP1.1） | **429** |

头级单变量全部通过，所以这不是 header 问题，是握手指纹问题。

**生产出网必须走 Bun 伪装栈**：`internal/httpx/bun.go` 用 uTLS 复刻
boringssl 的 ClientHello，并走 HTTP/2。标准库 `net/http` 的 transport 会被判
429，**换 IP 对这条无解**——换了 IP 还是 OpenSSL 指纹，还是 429。

这是本仓库为什么要自带一个 HTTP 客户端、而不是直接用 `http.DefaultTransport`
的唯一原因。

## 4. session 门（429）

`x-opencode-session` 被上游按**单次使用**判定：

- 同一 IP、headers/body 字节完全相同，新 mint 的 session → 200。
- 立刻重放同一个已消费的 session → 429。
- 该 IP 随后仍能服务新 mint 的请求（≥3 发实测）。

所以**每次上游试探都必须重新 mint**。实现见 `internal/identity`：`Session` 是
对话亲和键，`ChildSession` 保留客户端自己的子 session id（subagent 场景），
`Request`/`Message` 分别对应两条车道的 `x-opencode-request`。

### session 编码（结构判定，不是装饰）

`x-opencode-session` 必须 **descending**：

```
~((ms << 12) + counter) & 0xFFFFFFFFFFFF
```

同一毫秒内 counter < 64（真实客户端首条用 counter=1）。**ascending 恒定 403**。

`x-opencode-request`（`msg_`）仍 ascending。1.19.4 实发双头
`x-opencode-session` + `x-opencode-session-id`（同值），**不发** legacy
`x-session-id`，不重复 `x-opencode-request-id`。

`x-opencode-project`：1.19.5 无工作区时发字面量 `global`。**固定 40-hex 是跨 IP
可追踪身份，不得作为默认值**——那等于给上游一个稳定的追踪锚点。

## 5. body 门（429）

上游校验请求体是否"**本请求的现场渲染**"。同 IP、同 Bun 栈、同样新 mint session
的 2×2 对照：

| 变量 | 结果 |
|---|---|
| 官方客户端原样 body | 200 |
| 网关 body，prompt 里的日期是抓包当天 | 429 |
| 同上，日期刷成当天 | 200 |

UA、工具集、AGENTS 附录单独都不是判别项。**`Today's date:` 停在抓包当天就
429**，同段还含模型名与工作区路径。

因此 `internal/gateway/opencode_prompt.go` 的 `opencodeEnvironment` 每请求重写：

- `You are powered by the model named X` / `The exact model ID is opencode/X`
  —— 用**别名映射后的实际上游 ID**，不是客户端请求的别名。
- `Today's date: <UTC 当天>`。
- 工作区路径。

**严禁原样冻结发送、缓存渲染结果或手工简化模板**。旧日期、旧模型名、网关自己的
路径都会被回同形 429。`max_tokens` 无关，用户消息可任意短。

未知的客户端目录/工作区/平台/Git 状态一律标 `unknown`，**不得用网关主机环境
替代**——那会把网关自己的路径送进上游。

## 6. 模型门（401）与免费判据

免费层只服务**免费裸 ID**。付费裸 ID 同 headers/body 回
`401 AuthError: Missing API key`，且 failover 链不会因 401 触发。

**免费判据只有一个**：`-free` 后缀，或 `big-pickle`。

反例（都踩过）：

- 匿名目录的 `cost` 字段是**嵌套对象**，而且**付费条目也全是 0**，不能用来判免费。
- 目录总数 ≠ 免费模型数。匿名视图实测 86 条里只有 12 条免费。

目录本身可以匿名查（`GET {upstream.anonymous}/v1/models`，`Bearer public`，
不经网关），GET 不烧额度。

## 7. prompt 字符门（400）——最容易被误判的一道

这是免费层对**单请求输入**的硬顶，比模型窗口更早生效。

### 7.1 事实

上游对每次请求独立校验估算体积，超限即 400：

```
This model's maximum context length is 200000 tokens and at most 32000 prompt
tokens are accepted. This request is about 32054 prompt tokens.
```

- **模型最大上下文**（200000 / 262139 / 1000000）是窗口声明。
- **`at most 32000 prompt tokens`** 是免费层对单请求输入的真顶。
- `about N` 是上游控制面的估算器：**固定 `round(chars * 0.33966)`**，与内容
  语言/类型**无关**。自然中文、英文 filler、纯 4-byte 符号串，实测
  `about/chars` 全部落在 0.334~0.339。

换算成体积：**约 94.2K 字符**就是硬顶，与模型无关。

### 7.2 四个模型同门（2026-11-17 / 11-18 两次二分一致）

| 模型 | 最大 200 的 chars | 对应真实 prompt tokens | 首次 400 的 chars |
|---|---|---|---|
| big-pickle | 92,675 | 90,769 | 94,442 |
| deepseek-v4.1-flash-free | 92,675 | 71,238 | 94,442 |
| glm-5.3-flash-free | 92,675 | 70,947 | 94,442 |
| minimax-m3.1-flash-free | 92,675 | 71,236 | 94,442 |

四个模型的 chars 边界**完全相同**，再次印证门是字符数而非模型差异。

### 7.3 为什么有的模型能"过 300K token"

用 4-byte 罕见字符（U+20000 / U+2A700，真实密度 4.09~4.21 tok/char，而估算器只
按 0.339 计）可以把真实 tokens 藏在 94K chars 以内，于是能在不超估算门的前提下把
`usage.prompt_tokens` 顶到 384K。

**但那是测量技巧的产物，不是可用容量。** 常规文本永远先撞 94.2K 字符门。对外给
客户端配窗口时只能按估算门口径，**不得宣称 384K**。

### 7.4 网关的应对

`internal/gateway/policy.go` 在**发出请求之前**按同一个字符口径量一遍
（含注入的 prompt 与工具 schema——它们是真字符），超了就本地 400。

为什么本地拒而不是透传：

1. 本地拒绝**不消耗配额**，也不占用出口桶。
2. 上游同样会拒，但要等一整轮往返。
3. 错误形状必须保持上游同款（`code=context_length_exceeded`），因为 opencode
   正是靠这个形状识别"context too long"并启动压缩（`reason: "overflow"`）。
   换成网关自定义形状会让它把同一个超长请求再发一次。

文案在保留上游原句之后追加 `[opencode2api]` 段，给出真实字符账（总字符 / tools /
messages / 超出量），并把"compacts below"改成**可达**的 32000 —— 上游那句
170000 是给 opencode 的压缩点，对免费层不可达，客户端照它设阈值永远压不下来。

### 7.5 判别本地 400 与上游 400

两条就够：

- **耗时**：本地 2–4 ms，上游往返 3–20 s。
- **日志**：本地命中会写 `event=prompt_gate_refused`；上游 4xx 原样透传。

### 7.6 边界

`promptGates` 只收录真正二分过的 4 个 ID。没测过的留空 = 不启用字符门，仍只按
token 窗判。`hy4-flash-free` 特别：它**不受**这道估算门约束（实测自然中文
1,281,215 chars → 867,947 真实 tokens 仍 200），硬顶是真实 1,048,576 token 窗。
给它套 94.2K 字符门会拒掉上游本来接受的请求。

## 8. 真实配额（429 `FreeUsageLimitError`）

按**出口 IP / UTC 日**计量成功调用，UTC0 重置。额度小，数十发 200 量级即见烧穿。

**传输失败、TLS EOF、超时、403 不烧额度**，不得据此判断配额。

### 8.1 `Retry-After` 指向 UTC0 不是充分判据

实测反例（2026-11-14）：同一出口 IP 在 429 波后**不换 IP 即恢复 200**；且三个
不同运营商 IP 在同一秒级窗口一起 429。上游存在**共享池维度的阵发节流**，其 429
同样携带 UTC0 形 `Retry-After`，约 1 小时内自愈。

### 8.1.1 换 IP 是唯一能重置额度的手段，但只在 ④ 有效

额度按出口 IP 计，所以换 IP 能重置它。网关用 `egress_agents` 实现：每个出口旁的
egress-agent 在自己端口上既是 CONNECT 代理，又提供 `/healthz`（当前出口 IP）与
`/rotate`（重启路由器）。实现与触发条件见 `docs/ARCHITECTURE.md` §6.2.1。

**必须强调**：这张表里只有 ④ 能被新 IP 治好。①②③ 换多少 IP 都没用，而它们的
429 响应与 ④ 完全同形。这就是为什么网关只在两种窄条件下换 IP——token 预算跨过，
或 `Retry-After` 缺失/>1 小时——而不是见到 429 就动。

### 8.2 判据：同桶同窗的官方客户端对照

唯一有效判据是**在同一出口、同一时间窗跑一次官方 opencode CLI**：

- CLI 200 → 网关侧问题（修代码）。
- CLI 429 → 上游真节流（换出口重置额度）。

### 8.3 补充判据：多桶同窗硬 429

**≥2 个不同出口在 10 秒内一起硬 429**，本身就是共享池阵发的直接证据——独立的
per-IP 配额不可能在同一秒级窗口命中多家运营商。此时不应按"额度耗尽"处理，而应
短静默。

## 9. 流式回答不带 usage

同一请求的两种传输给的东西不一样：

| 传输 | 内容帧 | 终止 | `usage` |
|---|---|---|---|
| `stream: false` | 一条完整 message | — | **有**，真实 BPE 计数 |
| `stream: true` | 内容/思考增量 | `finish_reason` | **没有** |

`stream_options.include_usage` 已经被网关强制写进上游 body，但对这批后端无效
（实测 2026-11-18：8 个内容帧 + 终止帧，没有任何 `choices: []` 的 usage 帧）。

后果：凡靠 usage 计上下文的客户端会整场会话读到 0 token，看不见自己离 §7 那道门
还有多远。

**网关补一帧**：`internal/protocol/stream.go` 的 `streamUsageTracker` 在终止帧
之前补一帧 OpenAI 形状的 usage（`choices: []`、`estimated: true`）：

- prompt 侧 = `EstimatePromptTokens`：CJK 0.70 tok/char、ASCII 0.28 tok/char、
  +300 结构开销。对两次实测分别高 3.8% 与低 8.5%——同一个池的 tokenizer/后端
  组合会把密度挪 ±13%，这个系数取的是中点。
- completion 侧 = 流内实际生成字符数 × 0.6（含 reasoning 与 tool arguments）。
- **上游真给了 usage 就一律不合成。**
- **只对 Chat 合成**：Anthropic 与 Responses 的 usage 嵌在终止结构里
  （`message_delta` / `response.completed`），硬塞一帧会让严格客户端拒绝流。

它是**估算，不是账单**：`estimated: true` 就是给读者看的。要精确账单只有
`stream: false` 一条路。

## 10. 复现与验证

改动涉及免费层时，最小验证集：

```bash
# 1. 本地门禁
go vet ./... && go build -trimpath ./cmd/opencode2api
gofmt -l ./cmd ./internal ./webui     # 必须为空

# 2. 起服务（需要至少一个上游凭证，或显式 anonymous: true）
./opencode2api -config config.json

# 3. 目录与免费模型
curl -s http://127.0.0.1:8080/v1/models -H 'Authorization: Bearer YOUR_LOCAL_KEY' | head

# 4. 流式 + 工具调用（形状门的正例）
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_LOCAL_KEY' -H 'Content-Type: application/json' \
  -d '{"model":"MODEL_ID","stream":true,"messages":[{"role":"user","content":"hi"}],
       "tools":[{"type":"function","function":{"name":"read","parameters":{}}},
                {"type":"function","function":{"name":"write","parameters":{}}},
                {"type":"function","function":{"name":"glob","parameters":{}}},
                {"type":"function","function":{"name":"grep","parameters":{}}}]}'

# 5. 字符门（应本地 400，耗时 < 10ms，日志有 prompt_gate_refused）
python -c "print('中'*95000)" > /tmp/big.txt   # 用真实客户端把它塞进 messages
```

**每次 200 的活体调用都消耗该出口 IP 当日额度**——实验要少而精，先原样复现
（clone-first），再改单变量。
