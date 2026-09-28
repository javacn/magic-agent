# 流式模式（--stream）

各引擎流式的实现路径与实测差异（openclaw ACP 桥、arkclaw A2A SSE、dsh SDK 通道、codebuddy-gateway 帧序）。

claude / codebuddy / trae 走 CLI 原生 `stream-json` NDJSON 协议；llm 走纯文本流式
（内置 thinkSplitter 把混在正文里的思维链标签路由到 thinking 通道）；openclaw 走 ACP 桥；
arkclaw 走 A2A 官方的 SSE（`message/stream`）。增量实时转发，无缓冲等待：

| | claude / codebuddy | trae | llm | openclaw | arkclaw |
|---|---|---|---|---|---|
| 协议 | `stream_event` + `content_block_delta` | `stream_event` + `delta.content` | 纯文本 stdout + 思维链标签 | **ACP**（`openclaw acp`，stdio JSON-RPC） | **A2A SSE**（`message/stream`，`text/event-stream`） |
| 思考过程 | ✅ `thinking_delta`（模型开 reasoning 时） | ❌（模型侧无 reasoning 通道） | ✅ 标签块 → thinking 通道 | ❌（ACP 桥只发正文与工具状态） | ❌（网关不发） |
| 工具事件 | ✅ | ❌（协议不暴露） | ❌ | ✅ `tool_call` / `tool_call_update` | ❌ |
| 收尾 | `result` 行（全文以此为准） | 同左 | EOF（全文 = 增量拼接） | `session/prompt` 响应（stopReason） | 终态帧（`status.state=completed`） |
| 增量粒度 | 逐字 | 逐字 | 逐字 | 逐字 | **整段**：短任务「受理帧 + 整段正文」两帧；长任务每 15s 一个 `working` 心跳（不产生事件），见「arkclaw 流式（A2A SSE）」 |

**text 模式**：正文增量 → stdout 实时打印；思考增量 → stderr（`…` 前缀），
`2>/dev/null` 静音或 `2>&1 | tee` 保留都由你控制。

**json 模式**（默认）：每条增量一行 NDJSON，收尾一行汇总 envelope：

```json
{"type":"thinking","text":"用户在做加法..."}
{"type":"text","text":"2"}
{"type":"result","engine":"claude","model":"...","attempts":1,"latency_ms":1211,"thinking":"...","text":"2"}
```

`jq -c 'select(.type != "result")'` 逐事件消费，或 `tail -1` 取 result 全文。

**失败也走事件流**（2026-09-22 起）：一轮**要么以 `result` 收尾、要么以 `error` 收尾**，不会两样都没有。

```json
{"type":"error","engine":"openclaw","attempts":1,"error":"<完整错误链>","reason":"<最内层根因>"}
```

字段与 `-o json` 的失败 envelope 逐字同源（同一份 `agent.ReasonOf`），只是多一个 `type`；
同一份说明还会写进 stderr（envelope / 一行文本）。**为什么要专门说这条**：以前流式失败
一个字都不写（stdout 空、stderr 只剩前面那些提示行），调用方看到的就是「退出码 1 + 零输出」，
桌面上画成一条空白回答 —— 用户 2026-09-22 报的「openclaw 的引擎没对接好 不显示」正是这个。
只读事件流的消费方现在不必回头解析 stderr 也能拿到原因。

**语义差异**（相对非流式）：流式不做自动重试（增量已实时发出，重放会重复消费），
`-r` 被忽略；超时照常生效（杀整个 CLI 进程组）。

## openclaw 流式（ACP 桥）

openclaw 的 `agent --json` 是一次性 envelope（没有增量、没有工具事件），它自带的流式出口是
**ACP server**：`openclaw acp`（stdio + JSON-RPC 2.0，背后接本地 Gateway，`ws://127.0.0.1:18789`）。
本项目以 ACP client 驱动它，映射关系：

| ACP | → 事件 |
|---|---|
| `session/update{agent_message_chunk}` | `text`（逐字） |
| `session/update{tool_call}` | `tool_use`（name=title/kind、id=toolCallId、args=rawInput） |
| `session/update{tool_call_update}` | `tool_result`（status=completed/failed 时） |
| `session/update{agent_thought_chunk}` | `thinking` —— **桥当前不发**（兼容矩阵：thought streaming unsupported），留着以备将来支持 |

**会话锚点是 Gateway session key**（不是桥给的 ACP sessionId —— 那个每次随机、出进程就没意义）：
新会话自造 `agent:<agent>:acp-<uuid>` 并经 `session/new` 的 `_meta.sessionKey` 交给桥；续接把
`--session` 传回来的 key 原样再用一次即接回同一 Gateway 会话。⚠️ 老版本（非流式路径）返回的是
Gateway **session id**（裸 uuid）→ 会自动用 `openclaw sessions --json` 反查成 key 再续接。

**两条硬限制**（都是上游协议事实，不是本项目偷懒）：

1. **没有思考流** —— 桥只发正文与工具状态；
2. **不能按轮指定模型** —— ACP 未暴露模型选择（会话用 Gateway 侧的默认/钉住模型）。
   所以 `-e openclaw --stream -m <model>` 时**改用非流式内嵌调用**（`--model` 忠实生效），
   并在 stderr 说明 —— 用户选的模型被静默换掉比少一个增量糟糕得多。

## openclaw 非流式的两条路（`--local` 与 Gateway）

非流式那条腿（`Complete`）有两种跑法，**provider 凭据的来源不同**（上游事实）：

| 路径 | 命令 | provider key 从哪来 | 需要什么 |
|---|---|---|---|
| **嵌入式**（默认） | `openclaw agent --local …` | **shell 环境变量**（官方 help 原文：*requires model provider API keys in your shell*） | 无需 Gateway |
| **Gateway** | `openclaw agent …`（不带 `--local`） | `openclaw.json` 的 `models.providers.*.apiKey` | 本地 Gateway 在跑 |

**为什么要两条**：key 只配在 `openclaw.json`、环境变量里没有时，嵌入式那条路必然
`401 invalid api key (2049)` —— 实测本机同一把 key 直连 `api.minimaxi.com` 是 200、
走 Gateway 也正常出正文，只有 `--local` 拿不到它。所以默认仍走嵌入式（不依赖 Gateway），
**一旦失败是凭据类**（`401` / `invalid api key` / `authentication failed` / `unauthorized`）
就**自动改走 Gateway 重试一次**，并在 stderr 留一行痕迹。

边界（刻意收窄，见 `internal/agent/openclaw.go::isOpenClawAuthFailure`）：

- 只在**凭据类**失败上重试 —— 超时 / 会话接不上换条路也一样失败，白多一次往返；
- **只重试一次**；Gateway 也失败时**保留原始失败**（只补一句说明），
  不让「Gateway 没起」把清楚的 401 覆盖成一句连接错误。

两种 envelope 形状**都认**：嵌入式是顶层 `payloads`，Gateway 多包一层
`{runId,status,summary,result:{payloads,…}}`（见 `openclawEnvelope.unwrap`）；
Gateway 明确宣告失败（`status != ok`）时错误里带上它的 `summary`。

**回退与冷却**：ACP 桥多一个外部依赖（Gateway 在跑 + 桥的 scope 已批）。桥不可用时那一轮
**自动回退**到内嵌一次性调用（正文一次给出），并把「到什么时候为止别再试」写进
`~/.magic-agent/openclaw-acp-broken.json`（5 分钟；跨进程生效 —— 上层是「一轮一个 CLI 进程」，
进程内冷却没用）。想立刻重试删掉该文件即可。

**exec 审批**：桥会把需要拍板的执行请求转给 ACP client（`session/request_permission`）。
默认**保守**：`read` / `search` / `fetch` / `think` / `other` 放行，`edit` / `delete` / `move` /
`execute` 一律拒绝并在 stderr 说明；`MAGIC_AGENT_OPENCLAW_ACP_APPROVE=all` 可全放行（等价
`openclaw acp client --approve-all`）。openclaw 自己的 exec-policy / allowlist 仍是第一道闸门。

**首次启用要批一次授权**（ACP 桥以「设备」身份连 Gateway，请求的 scope 比默认高一档）：

```bash
openclaw devices list                    # 看 Pending 那行的 Request id（Device 名显示为 ACP）
openclaw devices approve <requestId>     # 批；--latest 只是**显示**最近一条，不会替你批
openclaw devices list                    # 确认 scopes 里出现 operator.admin
```

实测那台设备的请求是 `operator.admin + operator.read + operator.write`（默认只批了 read/write）。
不批也不会坏：那一轮走回退路径（正文一次给出）。批完想立刻生效：删掉
`~/.magic-agent/openclaw-acp-broken.json`（否则最多等 5 分钟冷却到期）。

**工具事件的两个实测细节**（2026-09-21 真机）：
- 工具名取 ACP 的 `kind`（`read` / `execute` / `edit` …）而不是 `title` —— 后者的实测值是
  「exec: command: echo hi」这种整条命令，当卡片标题又长又每次都变；命令本身在 args 里；
- **同一次工具调用只发一条 `tool_result`**：桥对同一次调用会推多条带内容的更新
  （实测 content 的「hi\n」与 rawOutput 的「hi」各一条）→ 不去重会在界面上出两张重复卡。

**环境变量**：`MAGIC_AGENT_OPENCLAW_ACP_APPROVE`（`all` = 全放行执行审批）、
`MAGIC_AGENT_OPENCLAW_ACP_BREAKER`（冷却标记文件路径，默认 `~/.magic-agent/openclaw-acp-broken.json`）。codebuddy 引擎走同源
`stream-json` 协议（CodeBuddy Code 系）；早期「本环境单次调用长期不返回」
的现象已定位为父会话 `SERVER__PORT` 端口冲突，见「在 WorkBuddy / CodeBuddy
会话内使用」一节。

## arkclaw 流式（A2A SSE）

arkclaw 的流式走 **A2A 官方通道** `message/stream`：POST 同一个端点，`accept: text/event-stream`，
响应是 SSE，逐帧 `data: {...}`（每帧一个完整 JSON-RPC Response）。映射关系：

| SSE 帧 | → 事件 |
|---|---|
| `status.state=working` / `submitted` | 无事件（网关没有增量可发），只刷新 `contextId` |
| `status.state=completed` | `text`（正文；`result.status.message.parts[].text`，兜底 `artifacts[].parts[].text`） |
| `status.state=failed` / `canceled` / `rejected` | 报错（与非流式同一条错误路径） |
| `:` 注释行、空行、`event:` / `id:` / `retry:` 字段 | 跳过（SSE 分隔与心跳） |

**⚠️ 它不是逐字流**（协议事实，不是解析缺陷）：实测真实网关一次 200 字生成只收到 **2 帧**
—— 0.20s 的 `working`（空正文）与 12.42s 的 `completed`（654 字**一次性**到达）。所以
`--stream` 在 arkclaw 上的语义是「任务受理帧 + 正文整段到达」，**不要当成打字机效果**。
要逐字得 claw 侧在生成过程中多发中间 `status-update` / `artifact-update`，属服务端改动。
收益仍然是实的：连接由服务端持续持有并回帧（长任务不再靠单次 HTTP 空等，也不容易被中间
代理的空闲超时掐断），且 claw 侧将来吐增量时客户端零改动即可接住。

**长任务期间每 15s 一个 `working` 心跳**（2026-09-24 实测）：「生成周报」这类带工具循环的
任务在网关侧要 **304s** 才回，期间每 15.2s 稳定来一帧 `working`（正文仍是最后一次性到达）。
⚠️ 这些心跳**目前不产生任何事件** —— 调用方在这几分钟里收不到任何东西，无法区分「在跑」
和「挂了」。界面若要显示「仍在生成」，得先把心跳透出成事件（**尚未实现**：新增事件类型
要同步桌面壳的解析契约，方案待定）。

**超时**：`-t` 对流式与非流式**都生效**（2026-09-24 修）。在此之前流式路径从不设
`req.Timeout`，引擎回落到自己的默认值（当时是 3 分钟）→ 桌面壳传的 `-t 600s` 被静默忽略，
304s 的任务必在 3 分钟被砍，界面上就是**「什么都没显示 · 调用失败 context deadline exceeded」**
（2026-09-24 用户报障）。现在：显式 `-t` 原样透传，未给时用引擎默认（arkclaw 已上调到 10 分钟）。

**为什么不是 WebSocket**（2026-09-22 实测，别再走一遍）：

1. 向同一端点发 `Upgrade: websocket` 握手，网关**不回 `101`**，照常当普通 POST 处理
   （`HTTP 200` + `application/json`；`server: istio-envoy` / `x-powered-by: Express`）；
2. A2A 核心规范只定义三种传输（JSON-RPC over HTTP(S) / gRPC / HTTP+JSON），流式统一走 SSE；
   WebSocket 属官方定义里的「自定义协议绑定」，要**服务端**另行实现；
3. 火山引擎 API 网关（`*.volceapi.com`）的协议类型只有 HTTP/HTTPS 与 HTTP1.1/HTTP2/HTTP2-GRPC，
   没有 WebSocket API 类型，也没有「把已有 HTTP API 升级成 WS」的能力。

**回退**：网关没按 SSE 回（老网关 / 不认 `message/stream`）时，那一轮**按一次性响应处理**
（正文补发成一条 `text` 增量）并在 stderr 说明 —— 不拿「没有 completed 帧」去糊弄调用方。

**去重**：`completed` 帧会把同一份正文同时放在 `status.message` 与 `artifacts` 里，实测如此；
已作为增量发出的正文是它的前缀时只补发剩下部分，不会让调用方看到两份。

**续接**：与非流式同一套 —— `--session <contextId>` 写进**消息对象内部**的 `contextId`，
终帧回填的 `result.contextId` 作为输出的 `session_id`。`-c, --continue` 同样显式拒绝。
