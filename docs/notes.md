# 协议与运行环境笔记

需要用户选择的统一格式、在 WorkBuddy / CodeBuddy 会话内使用时的注意事项。

## 需要用户选择（统一格式）

模型发起 `AskUserQuestion`（或工具调用待授权）时，claude / codebuddy 在 wire 上是**两种完全不同的形状**。
`internal/agent/ask.go` 把它们归一化成一份统一格式，`--stream` 输出里多一类 `ask` 事件：

```json
{"type":"ask","text":"需要用户选择：午餐吃拉面还是盖饭？ [拉面 | 盖饭]",
 "name":"AskUserQuestion","id":"call_function_fwvghez7y84d_1",
 "ask":{"engine":"claude","kind":"question","source":"tool_use","tool_name":"AskUserQuestion",
        "tool_use_id":"call_function_fwvghez7y84d_1",
        "questions":[{"id":"q0","text":"午餐吃拉面还是盖饭？","header":"午餐选择",
                      "options":[{"label":"拉面","description":"…"},{"label":"盖饭","description":"…"}],
                      "multi_select":false}],
        "tool_input":{"questions":[…]}}}
```

`text` 模式（`-o text`）下它是一行 stderr：`❓ 需要用户选择：…`（`2>/dev/null` 可静音，stdout 正文保持干净）。

## 两种 wire 形状（实测）

| 形状 | 出处 | 谁作答 |
|---|---|---|
| `tool_use` | claude / codebuddy 的 assistant 消息里一个普通 `tool_use` 块（`name=AskUserQuestion`） | **CLI 自己** —— 见下面的坑 |
| `control_request` | SDK 层 `subtype=can_use_tool`（宿主自己实现了 `canUseTool` 回调时才有） | **宿主**，回 `control_response` |

> ⚠️ **headless 下 claude 会自行拒绝**（2026-09-18 真机，claude 2.1.146）：
> 模型调 `AskUserQuestion` 后，CLI 紧接着回一条 `is_error` 的 tool_result
> （内容实测为 `Answer questions?`），**模型拿不到用户答案**，只能顺着往下编或改口。
> 本机 WorkBuddy 内置 codebuddy 更是连该工具都不在工具表里（工具表走 `ToolSearch` /
> `DeferExecuteTool` 的延迟工具集），当前不会出现该形态。
> 所以 magic-agent 目前只能**观察**到这次提问，真正作答得靠下面第三条通道。

## 三条通道（`ask.go` 的 API）

| 场景 | 用法 |
|---|---|
| 宿主实现了 `canUseTool`（拿到的是 `control_request`） | `EncodeAskAnswer(engine, req, ans)` → SDK 层 `{"behavior":…}`；要 wire 信封再用 `EncodeAskControlResponse` |
| **实际可用的作答通道**：把答案补进正在跑的会话 | `EncodeAskFollowUp(req, ans)` → 一段纯文本，走 `--append` 通道追加一条 user 消息（对全部引擎可用） |
| 只想识别 / 上报 | 读 `--stream` 的 `ask` 事件（`StreamEvent.Ask`），或直接调 `ParseAskLine(engine, line)` |

## 怎么接住用户的回答（2026-09-18 两条实测）

| 尝试 | 结果 |
|---|---|
| **抢答**：看到 tool_use 的瞬间往 stdin 写一条带答案的 `tool_result` | ❌ **无效**。CLI 自己的 `is_error` 拒绝几乎同时落地（实测：我们 2.59s 写入，CLI 2.60s 回 `Answer questions?`），模型只看到拒绝，本轮收尾成「用户取消了选择」。headless 下**没有**回填 tool_result 的窗口 |
| **追加**：本轮结束后把答案作为**新的一条 user 消息**写进 stdin | ✅ **有效**。第 1 轮 result =「用户没有回答这个问题。」→ 追加「【用户选择】…→ 拉面」→ 第 2 轮 result =「好的，午餐吃拉面。」 |

所以现阶段的闭环是：

```bash
# ① 起常驻会话（默认即常驻），宿主读事件流
magic-agent --stream -e claude --tools on -o json "帮我决定午餐"
#    → {"type":"ask","session_id":"44a06ef5-…","id":"call_…",
#       "ask":{"questions":[{"id":"q0","text":"午餐吃拉面还是盖饭？",
#                            "options":[{"label":"拉面 🍜"},{"label":"盖饭 🍚"}]}]}}

# ② 宿主把问题渲染给用户，拿到选择后追加回去（session_id 就在 ask 事件里，直接可用）
magic-agent --append 44a06ef5-… -p "【用户选择】
- 午餐吃拉面还是盖饭？ → 拉面"
```

`ask` 事件带 `session_id` 是刻意的：json 模式下 stderr 保持干净，宿主拿不到启动提示里的 run_id，
没有它就无法寻址到该会话（与 `turn_end` 带 `SessionID` 同一取舍）。

> 想做到「**当轮**原生作答」（模型同一轮就拿到答案，不必多跑一轮）需要宿主自己当决策方：
> claude 走 Agent SDK 的 `canUseTool` 回调（`EncodeAskAnswer` 的输出就是它的返回值格式），
> 或升级到支持 `--permission-prompt-tool` 的 CLI 版本 —— 本机 claude 2.1.146 的 `--help` 里
> **没有**该参数。magic-agent 现在跑的是 CLI 子进程，不在这条路上。

答案格式两族**完全一致**（官方文档 claude / codebuddy 同构）：

```jsonc
// 允许：updatedInput 必填，且必须原样回传 questions 数组
{"behavior":"allow","updatedInput":{"questions":[/* 原样 */],"answers":{"午餐吃拉面还是盖饭？":"拉面"}}}
// 拒绝
{"behavior":"deny","message":"User declined"}     // codebuddy 另支持 "interrupt":true
```

三个最容易写错、`ask.go` 已经用代码钉死的点：

| 点 | 行为 |
|---|---|
| `answers` 的 key | 必须是**问题原文**（不是 `header`、不是 id）；问题原文重复时**直接报错**（文本 key 无法区分，静默合并会答错题） |
| 多选 | 多个 `label` 用 `", "` 连接（官方示例写法） |
| `allow` 时 `updatedInput` | 必填；实现方式是「原始 input + answers」而不是字段级重建，保证 `questions` 一定原样回传 |

其余校验（不静默降级）：漏答问题 / 选项 label 不在候选里 / 单选却给了多个 label / 问题 id 不存在 /
引擎不支持（`AskSupportOf == "none"`）→ 一律报错；拒绝时 `message` 为空回 `"User declined"`。

> 其它引擎（trae / llm / codex / openclaw / dsh / arkclaw）实测均无 `AskUserQuestion`，也无 `can_use_tool`。
> trae / Cursor / iFlow / Qwen 走的是 **ACP 的 `session/request_permission`**（另一族协议：选项带
> `optionId` + `kind=allow_once/reject_once…`），本项目尚未接入，故 `ask` 字段如实报 `none`。

真机验收（2026-09-18）：

```bash
magic-agent --engines --no-models | jq -c '.[] | {engine, ask}'
# claude / codebuddy → "tool:AskUserQuestion"；其余 → "none"

magic-agent --stream --keep-alive=false -e claude --tools on -o json \
  "请调用 AskUserQuestion 工具问我：午餐吃拉面还是盖饭。只问一次"
# → 事件流里恰好一条 {"type":"ask", …}（partial 与聚合两条路都识别，去重后只报一次）
```

## 在 WorkBuddy / CodeBuddy 会话内使用（重要）

**现象**：在 WorkBuddy 的会话里调用 codebuddy 引擎，单次调用**永久不返回**
（既无 stdout 也无 stderr，直到超时被杀）。同样的命令在本机终端里数秒即回。

**根因**：父会话把自身内置 HTTP 服务的监听端口通过 `SERVER__HOST` /
`SERVER__PORT` 注入给子进程。codebuddy CLI 读到这两个变量后会在**同一端口**
再起一个服务，撞上父进程已监听 → `EADDRINUSE` → 该错误在启动流程里是
unhandled rejection，CLI 既不退出也不产出任何输出 → 永久挂起。

```
Unhandled rejection Error: listen EADDRINUSE: address already in use 127.0.0.1:58311
```

**处理**：magic-agent 在 `internal/agent/env.go` 里维护子进程环境 denylist，
`SERVER__*` 前缀（以及父会话的 `CODEBUDDY_SESSION_ID` / `CLAUDE_SESSION_ID`
等会话标识）一律不传给子 CLI。**在 WorkBuddy 会话内无需任何额外配置**。

验证（同一环境、`SERVER__PORT` 仍在）：

| | 修复前 | 修复后 |
|---|---|---|
| `--tools off` | 挂起 120s+ | 4.0s → `NO_TOOLS` |
| `--tools on` | 挂起 100s（超时被杀） | 16.6s → 正常回答 |
| `--tools WebSearch` | 挂起 | 6.2s → 正常回答 |

若在**其他**宿主环境遇到类似挂起，可用同一思路排查：把该宿主注入的
「监听端口/会话标识」类变量从子进程环境里剔除。
