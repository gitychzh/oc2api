# 模型参数：与上游目录和实测边界对齐

> 网关是"替上游说话"的一方，所以它报出和执行的上限必须是上游的上限。
> 本文记录每个免费模型的上限**出处**与**实测边界**，改之前先读这里。
>
> 数值分两类，不能混用：
> - **保证值**（目录声明 / 实测确认的 floor）：网关按它执行和对外宣称。
> - **实测值**（有时能通过的更大值）：只作记录，**不得对外宣称**。
>   上游是异构后端池，越过保证值之后能否通过取决于落在哪个后端。

## 1. 出处

| 事实 | 来源 |
|---|---|
| 模型目录（限额、能力、推理档位） | `https://models.opencode.ai/api.json`（provider `opencode`） |
| token 估算 `round(len/4)` | OpenCode CLI `packages/core/src/util/token.ts`（`CHARS_PER_TOKEN = 4`） |
| 自动压缩阈值 | CLI `packages/core/src/session/compaction.ts`（`calculateCeiling`） |
| 输出预算 | CLI `packages/core/src/session/model-request.ts`（`outputLimit`） |
| 免费层字符门 | 本仓库实测二分，见 [`PROTOCOL_FACTS.md`](PROTOCOL_FACTS.md) §7 |

网关侧的落地位置：`internal/models/profile.go`（字符门与估算器）、
`internal/protocol/estimate.go`（补 usage 的密度系数）。

## 2. 两个估算器，别用错

| 估算器 | 公式 | 用途 |
|---|---|---|
| 客户端估算 | `round(chars / 4)` | 复刻 CLI 的上下文占用显示 |
| 上游控制面估算 | `round(chars * 0.33966)` | 免费层字符门的真实口径（§4） |
| 上游真实计费 | 后端 tokenizer BPE | `usage.prompt_tokens`，自然中文 0.67~0.89 tok/char |

三者数值差别很大：同样 92,675 字符，客户端估 23,169，控制面估 31,473，
真实计费 62,807~71,238。**报错时用控制面口径，计费用真实计费口径**。

## 3. 保证值表（网关按此执行）

| id | context | output | 字符门 | reasoning efforts | 备注 |
|---|---|---|---|---|---|
| `big-pickle` | 262,139 | 32,000 | 94,200 | —（无档位） | 目录写 200000/160000，**实测已过期**；见 §5 |
| `deepseek-v4.1-flash-free` | 200,000 | 32,000 | 94,200 | low/high/max | |
| `glm-5.3-flash-free` | 200,000 | 32,000 | 94,200 | low/high/max | |
| `minimax-m3.1-flash-free` | 1,000,000 | 512,000 | 94,200 | low/medium/high/xhigh/max | |
| `hy4-flash-free` | 1,048,576（input 983,040） | 65,536 | **不启用** | none/high | **不受**字符门约束，见 §6 |
| `mimo-v2.6-flash-free` | 200,000 | 32,000 | 未测 | —（无档位） | 不支持 structured output |
| `longcat-2.5-free` | 1,000,000 | 262,144 | 未测 | —（仅 toggle） | 不支持 temperature |
| `iceman-free` | 262,144 | 131,072 | 未测 | —（无档位） | 流式不吐独立 reasoning delta |
| `inkling-2-small-free` | 1,048,576 | 131,072 | 未测 | none/minimal/low/medium/high/max | `none` 可关思考 |
| `beam-free` | 1,048,576 | 131,072 | 未测 | low/medium/high/xhigh/max | |
| `flint-40b-free` | 262,144 | 65,536 | 未测 | low/medium/high/xhigh/max | |
| `nemotron-3.5-super-vl-free` | 1,048,576 | 131,072 | 未测 | none/minimal/low/medium/high/xhigh | 无 max 档 |
| `nemotron-3.5-lightning-free` | 262,144 | 262,144 | 未测 | —（无档位） | |

**"未测"= 网关不启用字符门**，只按 token 窗判。给没测过的模型套 94,200 会拒掉
上游本来接受的请求。

## 4. 免费层字符门（四模型同门）

`round(chars * 0.33966) > 32000` 即 400，约 **94,200 字符**，与模型、语言、
内容类型都无关，且**含注入的内置工具 JSON**。

2026-11-17 与 11-18 两次二分，四个 ID 的 chars 边界**完全相同**：
92,675 过 / 94,442 拒。

推论，实际影响配置：

- 工具越多，可用对话越少。46 个工具 38,913 字符 ⇒ 对话只剩约 55K 字符。
- 注入**全部 builtin 名 + 空 schema**（~1,350 字符）比注入完整定义（~25,000
  字符）省约 24K 字符，直接换成对话预算。形状门只数名字，不校验 schema。

## 5. big-pickle 的窗口：目录是错的

目录（以及 CLI 内嵌快照）都写 `{context 200000, input 160000}`，**两个数都偏小**。

- 2026-10-29：所有拒绝都引用 `maximum context length of (262139)`，约束是
  `prompt + max_tokens <= 262,139`。通过点实测到 233,240 全 200，294,840 → 400。
- 2026-11-14（家庭 IP）：222,818 / 262,139 / 283,110 / 325,576 / 368,043
  prompt tokens 全部 200。

**保证值取 262,139**。越过它之后是抽奖：

| 目标 | 通过率 |
|---|---|
| 268,000 | 2/6 |
| 300,000 | 4/6 |
| 315,000 | 0/6 |

响应 id 可见三类后端（`chatcmpl-…` / `mai-api-…` / 纯 hex）。宣称 303K 会让约
三分之二的请求落到小窗口后端后失败，且先烧了配额。

## 6. hy4-flash-free：不走字符门的新一代

2026-11-16 上游新增，canonical `tencent/hy4-flash`。

**关键差异：它不受 `at most 32000 prompt tokens` 估算门约束。** 它走真实
1,048,576-token 窗口：

| 测量 | 结果 |
|---|---|
| 自然中文最大 200 | **1,281,215 chars → 867,947 真实 tokens** |
| 首次 400 | 1,290,595 chars |
| 4-byte U+20000 | 94,009 chars → 384,264 tok（密度 4.086 tok/char） |

它的 400 错误体用的是 `chars × 1.007` 粗估，与四模型的 `0.33966` 不同口径。
**套同一个字符门系数会把 `about` 报错量级**，所以 `promptGates` 对它留空。

对外按约 1.28M 中文字符 / 868K tokens 宣称，不得沿用旧四模型的 94.2K chars。

`reasoning efforts [none, high]`：`none` 仅客户端手动可选，默认推理开启，
**网关从不关思考**。`max_tokens` 请求 120,000 实测 200（上游不在请求阶段硬拒
output，仅实际生成时约束）。

## 7. 思考档位：只有声明了 efforts 的模型才该转发

`capabilities.reasoning.effort` 只有模型**允许用户选强度**时才发布。

- `big-pickle` 只有 `reasoning: {supported: true}`，**没有** efforts 数组 →
  客户端渲染不出调节项，模型按自身上限推理。
- `deepseek-v4.1-flash-free` / `glm-5.3-flash-free` 声明了
  `effort: ["low","high","max"]`，客户端才有调节项。

**给不声明档位的模型转发 `reasoning_effort`，等于对客户端撒谎**，还可能被上游
当成降低强度的指令。本仓库的 `reasoning.effort_by_model` 与
`internal/protocol` 的 effort 处理遵循这条。

客户端唯一有效的思考档位字段是 **`reasoning_effort`**（snake_case）。Anthropic
侧的对应物是 `output_config.effort` 与 `thinking.budget_tokens`，桥接层会归一化。

## 8. 输出预算

CLI 的 `outputLimit`：

```
requested = min(limit.output || 32000, 256000)
room      = context - measured - ceil(estimate * 1.15)
budget    = min(requested, max(1024, room))
```

本仓库在 2.x 车道补 `max_completion_tokens`（默认 32000），客户端显式给了
`max_tokens` 或 `max_completion_tokens` 则以客户端为准。

## 9. 目录暴露

- `/v1/models`：OpenAI 标准形状（`id/object/created/owned_by`）**契约冻结**。
  自动发现类客户端只认这个形状。`internal/gateway/health.go` 额外在顶层补
  `context_window` / `context_length` / `max_input` / `max_output` / `reasoning` /
  `reasoning_efforts` / `compat`，因为多数 harness 从顶层读。
- 未知上游 id：**一律不加任何限额字段**。给不认识的模型编数字，是网关开始拒绝
  合法流量的起点。

## 10. 回归验证

改动模型参数后：

1. `go vet ./...` + `go build` 通过。
2. 真实上游拉一次 `/v1/models`，确认目标模型仍在免费目录里（`-free` 后缀或
   `big-pickle`）。
3. 对该模型发一条**短**的非流式请求，看 `usage.prompt_tokens` 是否有值。
4. 若要验证字符门边界，用二分法从 90,000 字符起，**每步只烧一发**。
