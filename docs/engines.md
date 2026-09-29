# 各引擎细节

codebuddy / codebuddy-ai / arkclaw / codebuddy-gateway / llm / dsh / codex
各引擎的配置、协议与行为细节。

## arkclaw 引擎（A2A JSON-RPC 网关）

`-e arkclaw` 不走本机 CLI，而是一个 [A2A](https://a2a-protocol.org/)（Agent-to-Agent）
网关：magic-agent 把 prompt 组成一个 JSON-RPC 请求 POST 过去，网关侧跑它自己的 agent，
再把结果包成 A2A Task 返回来。两个方法都接：

- **`message/send`**（默认）：单次请求-响应；
- **`message/stream`**（`--stream`）：A2A 官方的 SSE 事件流。⚠️ 本网关**不吐逐字增量**，
  只有「受理帧 + 整段正文」两帧 —— 详见「arkclaw 流式（A2A SSE）」。

## 配置（url / key / claw_id 放配置文件）

凭据不写死在代码里、也不必每次敲 flag，统一放本地配置文件：

```bash
mkdir -p ~/.magic-agent
cat > ~/.magic-agent/config.json <<'EOF'
{
  "systemPrompt": "你是一个中文助手，始终用中文回答所有问题。",
  "arkclaw": {
    "url": "https://<host>/a2a/jsonrpc",
    "key": "<apikey>",
    "claw_id": "ci-xxxxxxxxxxxxxxxxxxxx"
  },
  "agents": [
    { "name": "MagicAI", "url": "https://<host>/a2a/jsonrpc?apikey=<apikey>&clawId=ci-xxxxxxxxxxxxxxxxxxxx" }
  ]
}
EOF
```

仓库内 `config.example.json` 是同一份骨架。路径与取值优先级：

| 优先级 | 来源 | 说明 |
|---|---|---|
| 1 | 环境变量 `MAGIC_AGENT_ARKCLAW_URL` / `_KEY` / `_CLAW_ID` | 覆盖文件值，便于临时切换端点 |
| 2 | 配置文件 | 探测链：`MAGIC_AGENT_CONFIG` → **`~/.magic-agent/config.json`（存在时优先）** → `$XDG_CONFIG_HOME/magic-agent/config.json` → `~/.config/magic-agent/config.json`（历史位置） |

> ⚠️ **`~/.magic-agent/config.json` 只在它存在时才优先**（2026-09-23 用户定稿：「应该放在 `~/.magic-agent/` 下」）。
> 这样新老两种放法都安全：文件放在新位置就生效，没建文件时老位置照旧 ——
> 否则老配置会被静默忽略，那正是「配置改了没生效」这类报障的来源。

键名做了宽松兼容：`url`/`endpoint`、`key`/`apikey`/`api_key`、`claw_id`/`clawId`/`clawID` 任选其一。文件不存在或为空**不算错误**（等同未配置，纯环境变量用法同样可用）；JSON 语法错会明确报出路径。

## 具名 A2A agent（`agents` 数组）

`arkclaw` 那个引擎**只有一个**（模型还由 `claw_id` 在网关侧绑死），想同时用两个 claw、
或只是给同一个 claw 一个好认的名字，就写在 `agents` 里：

```json
"agents": [
  { "name": "MagicAI", "url": "https://<host>/a2a/jsonrpc?apikey=…&clawId=…" },
  { "name": "AnotherClaw", "url": "https://<host>/a2a/jsonrpc", "key": "…", "claw_id": "ci-…" }
]
```

- 每个条目注册成一个**独立引擎**，名字就是 `name` → `magic-agent -e MagicAI "你好"`（大小写不敏感），
  `--engines` 里也多一行 → 上层（观物台等）的引擎下拉**一行都不用改**；
- 凭据**两种写法等价**：内嵌在 URL 的 query 里（`?apikey=…&clawId=…`），或分开写 `key` / `claw_id`。
  拼接用 `q.Set`，URL 里已有的参数会保留，写重也不怕；
- **能力表按「协议家族」归属**，不按名字：具名 agent 自动继承 arkclaw 那一整行
  （`workspace:none` / `attachments:part:file` / `permission:none` …，见 `CapabilityFamilyOf`）——
  否则新名字会落进 default 档，界面会白白警告「图贴了但模型看不到」；
- 边界：`name` 为空、或与已有引擎重名（不分大小写）→ **该条目被跳过**（不报错、不影响其它条目）；
- 与 `arkclaw` 节互不影响：`arkclaw` 是历史保留的单实例，`agents` 是新增的多实例通道。

## 用法

```bash
magic-agent --engines                                   # 看 arkclaw 是否已配置齐备
magic-agent -e arkclaw "你好"                            # 单轮 message/send
magic-agent -e arkclaw --session <contextId> "接着说"     # 按 contextId 续接同一上下文
magic-agent -e arkclaw --stream "你好"                   # 流式（A2A SSE；正文仍整段到达，见下）
magic-agent -e arkclaw --json-schema '{"type":"object",...}' "输出 JSON"   # 结构化输出
```

## 行为细节

| 维度 | 行为 |
|---|---|
| 鉴权 | `apikey` / `clawId` 作为 URL query 参数发送；失败时网关回 `HTTP 401` + `text/plain` 正文（**非 JSON**），错误信息里带上状态码与正文 |
| 续接 | `--session <contextId>` → 写入**消息对象内部**的 `contextId`；返回时用 `result.contextId` 回填输出的 `session_id`。实测放到外层 `params.contextId` 会被网关忽略并另开上下文 |
| `-c, --continue` | **不支持**：A2A 没有「查询最近上下文」的接口，显式报错并提示改用 `--session <contextId>`，而不是静默新开会话 |
| `--stream` | **支持（A2A SSE，`message/stream`）**，但网关不吐逐字增量：实测只有「受理帧 + 整段正文」两帧，正文仍是一次性到达；网关没按 SSE 回时自动退回一次性响应并在 stderr 说明。见「arkclaw 流式（A2A SSE）」 |
| `-m` / `--max-tokens` / `--temperature` / `--tools` | 静默忽略：模型与工具循环由网关侧的 claw 决定 |
| `-s, --system` | 展平进 message 正文头部（协议无独立 system 角色） |
| `--json-schema` | 支持，走与 `llm` 相同的输出后处理抽 JSON 路径 |
| 默认超时 | **10 分钟**（2026-09-24 由 3 分钟上调）：短问答 8~23s，但**带工具循环的长任务要 5 分钟** —— 实测「生成周报」在网关侧 304s 才回；3 分钟会把正文全砍掉 |
| `-t, --timeout` | **流式与非流式都生效**（2026-09-24 修）：以前流式路径从不设 `req.Timeout`，引擎回落到自己的默认值 → `-t 600s` 被静默忽略，桌面壳上表现为「什么都没显示 · 调用失败 context deadline exceeded」 |
| 取文 | `result.status.message.parts[].text` → 兜底 `result.artifacts[].parts[].text` |

## codebuddy / codebuddy-ai 引擎（同一个独立 CLI、两个账号）

`-e codebuddy` 与 `-e codebuddy-ai` 跑的是**同一个** CodeBuddy Code CLI ——
即 `npm i -g @tencent-ai/codebuddy-code` 装出来的那份，**不是**桌面 App 内置的。

| | codebuddy | codebuddy-ai |
|---|---|---|
| 二进制 | 同一个（env `MAGIC_AGENT_CODEBUDDY_BIN`） | 同一个（env `MAGIC_AGENT_CODEBUDDY_AI_BIN`） |
| 账号 id | `codebuddy` | `codebuddy-ai` |
| 票据文件 | `…/CodeBuddyExtension/Data/Public/auth/codebuddy.info` | 同目录 `codebuddy-ai.info` |
| 配置目录 | `~/.codebuddy` | `~/.codebuddy-ai` |
| 默认模型 | `hy3` | 不传（交 CLI 自选） |

**为什么换掉 App 内置 CLI（2026-09-28 实测）**：桌面 App 的凭据由 App 通过 sidecar
通道用**受管密钥**静态加密（`CODEBUDDY_SIDECAR_CREDENTIAL_BOOTSTRAP_SOCKET`），
密钥只发给 App 自己的子进程 —— 独立起的 CLI 解不开，at-rest 登记表里
`auth/<id>.info` 的 read/write 全是 `missing-key`，引擎每次只拿到
「Authentication required」的空输出。独立安装的 CLI 自带可读写的凭据库，
而且带完整 TUI（能 `/login`）。

**一个 CLI 怎么挂两个账号**：票据路径是 `sharedDataPath/auth/<authentication.id>.info`，
而 `authentication.id` 的取值链里**环境变量 `ACC_PRODUCT_CONFIG_V3` 优先于 CLI 包内的
`product.json`**。给两个子进程注入不同的 id，就等于各用各的账号、互不顶号。
实现见 `internal/agent/codebuddy.go` 的 `codebuddyAccountEnv`。

⚠️ 同时必须**清空** `ACC_PRODUCT_CONFIG_PATH`：父会话（WorkBuddy App / 内嵌会话）
会把它指到自己的 acc-product-config，那份里的 `authentication.id` 优先级高于 V3，
会把账号选回 App 的默认账号（实测踩过）。

### 登录

```bash
magic-agent --login codebuddy        # 拉起交互式会话，在里面执行 /login
magic-agent --login codebuddy-ai     # 另一个账号，各登各的
```

`--login` 由 CLI 层 exec 引擎自己给出的命令（`agent.LoginRunner`：bin + 账号环境），
**账号环境与普通调用同源**，所以不会把两个账号登到同一份票据上。

临时换账号：`MAGIC_AGENT_CODEBUDDY_AUTH_ID` / `MAGIC_AGENT_CODEBUDDY_AI_AUTH_ID`。

### 检测与安装

- 探测链：`MAGIC_AGENT_CODEBUDDY_BIN`（/ `_AI_BIN`）→ PATH 上的 `codebuddy`
  → npm 全局 bin 目录；**不再探测 App 包内路径**（那是导致 missing-key 的那一份）。
- `--engines` 的 `install` 字段：`npm install -g @tencent-ai/codebuddy-code`
  （同一条命令对已装好的引擎重跑 = 升级）。

### 自定义模型端点（`models.json` 自动同步）

桌面 App 里配好的自定义模型（`custom-local:*`）在独立 CLI 下会失败：

```
Custom model <id> has no endpoint url configured.
Set the "url" field for this model in your model settings (models.json) and try again.
```

原因：CLI 的自定义模型端点**只从 `<配置目录>/models.json` 读**，而桌面 App 是在启动 CLI 时
把端点（`url` / `apiKey`）注入子进程的 —— 独立进程没有这份注入。

引擎在**探测模型时顺带同步**（`listModels` → `syncCustomModelEndpoints`）：

| 引擎 | 端点来源（只读） | 落点 |
|---|---|---|
| `codebuddy` | `~/.workbuddy/cache/acc-product-config-v*.json` | `~/.codebuddy/models.json` |
| `codebuddy-ai` | `~/.workbuddy-ai/cache/acc-product-config-v*.json` | `~/.codebuddy-ai/models.json` |

清单与端点**同源**（都是该引擎的 acc 缓存），所以不会出现「列得出来、选不了」。
调用侧还有一道兜底：`-m custom-local:*` 时再补一次（用户可能刚在 App 里配好就直接调用）。

约束（见 `internal/agent/custom_models.go`）：只**新增**缺失的 id，不删不改既有条目、
不动未识别字段（`models.json` 归 CLI 所有）；跳过 `disabled` 条目；无新增不落盘；
App 缓存只读；文件权限 `0600`（含第三方 `apiKey`）。用户显式设了
`CODEBUDDY_CONFIG_DIR` 时，落点跟着走。

### 提问（AskUserQuestion）：不支持

`--engines` 里 codebuddy / codebuddy-ai 的 `ask` 字段是 **`"none"`** —— 这两个引擎**产生不了决策卡**。

官方文档把 `AskUserQuestion` 列为内置工具（Requires Permission: Yes），但**可用性有宿主门槛**：
CLI 只把它交给第一方宿主（官方 App / IDE），第三方宿主驱动的通道下**模型根本看不到它**。
2026-09-28 实测七种配置，全部拿不到 —— 模型一律 `ToolSearch` 搜不到、自述"注册表里没有"，
最后退化成**文字提问**（这正是「正文说'有两个决定要你拍'、UI 却没有卡片」的原因）：

| 配置 | 结果 |
|---|---|
| `-p` 文本 / `--print --output-format json` | ❌ |
| `--tools default` / 显式白名单 / `--allowedTools AskUserQuestion` | ❌ |
| `-y`(bypassPermissions) / `auto` / `default` | ❌ |
| SDK initialize 声明 `capabilities.elicitation.form` | ❌ |
| `CODEBUDDY_HOST_CAPABILITIES=elicitation.form` | ❌ |
| `--acp`（initialize + session/new + session/prompt） | ❌ |
| `--acp` + `clientCapabilities.elicitation.form=true` | ❌ |

佐证：该工具**只在 CLI init 行的 75 个工具注册表里出现**，不进模型的可用工具列表，`ToolSearch`
也索引不到；而 `elicitation.form` 解锁的是 `AskUserForStructuredInput`（文档明说"否则不暴露给模型"），
不是 `AskUserQuestion`。CLI dist 里提问走的是「工具审批/中断」通道（ACP 的
`handleToolApproval → requestPermission`、SDK 的 `perm_`/`elic_` 前缀），属第一方宿主面。

**所以能力表如实报 `none`**：此前按"claude/codebuddy 同族协议"推断成支持，会让上层 UI
承诺一张永远不会出现的决策卡——比"不支持"更糟。**需要决策卡请用 `claude` 引擎。**

（`EncodeAskAnswer` 对 codebuddy 族的**答案编码形状**仍然保留，见 `ask.go` 的
`askAnswerShapeKnown`：形状官方文档写了，将来该通道打开即可直接复用。）

## codebuddy-gateway 引擎（webhook + SSE）

`-e codebuddy-gateway` 接的是**已经在跑的** CodeBuddy Code HTTP 网关 —— 也就是
`codebuddy --serve`（或交互会话里的 `/gateway` 远程控制）暴露出来的那套服务，官方文档见
[远程控制（Remote Control）](https://www.codebuddy.ai/docs/zh/cli/remote-control)。
magic-agent 把 prompt 以 webhook 投递进去，再收 SSE 增量。

它和 `codebuddy`（本机 CLI）解决的是不同问题：

| | `codebuddy` | `codebuddy-gateway` |
|---|---|---|
| 执行位置 | 每次 spawn 一个本机 CLI 进程 | 已在跑的网关进程里（可以是另一台机器 / 容器） |
| 登录与配置 | 每个进程各自读本机配置 | 复用网关进程**已经登录、已经配好模型与 MCP** 的环境 |
| 会话续接 | `--session <session_id>`（CLI 侧） | `--session <conversation_id>`（网关侧会话锚点） |
| 流式 | `stream-json` 逐字 | SSE（协议支持 `streaming` 增量帧；**实测当前只推终帧**，见下） |
| 冷启动 | 有（起进程 + 加载扩展） | 无（进程常驻） |

## 协议（两段式）

```text
① 投递  POST <base>/api/v1/webhooks/generic
        authorization: Bearer <password>
        x-codebuddy-request: 1
        {"version":"1.0","id":"<msgId>","type":"message",
         "source":{"platform":"generic","sender":{"id":"magic-agent"},
                   "conversation":{"id":"<会话锚点>","type":"direct"}},
         "payload":{"text":"<prompt>","attachments":[…]}}
        → 202 {"data":{"runId":"<uuid>","status":"accepted"}}     （实测 3~13ms 返回）

② 收流  GET  <base>/api/v1/runs/<runId>/stream
        accept: text/event-stream
        → SSE，逐帧 data: {"version":"1.0","replyTo":"<msgId>","status":"…",…}
```

**关键事实：投递响应里没有正文。** 网关是「受理即返回」的异步模型，正文只在 SSE 通道上出现
（`streaming` 的增量 + `completed` 的整段）。所以本引擎的**非流式路径也必须读 SSE**，只是不往
外发增量 —— 这不是「为了流式而流式」，而是那条通道是唯一拿得到正文的路。

附带结论：`platform` 只能用 `generic`。`wecom` / `wechat-kf` 适配器会把回复**推给平台**，
而 `generic` 适配器根本没有实现 `sendReply`（实测回调一次都不会触发），正文只留在 run 流里，
正好由本引擎消费。

## 帧类型

| `status` | 载荷 | 处理 |
|---|---|---|
| `accepted` | 无正文 | 忽略（**别拿它当「连接成功」的判据**：网关侧用的是普通 RxJS Subject，投递完才连流的客户端收不到这一帧） |
| `streaming` | `content.chunk`（**增量**） | 拼接后按增量转发（⚠️ 实测当前网关不发这类帧，见下） |
| `completed` | `content.markdown`（**整段**，权威值）+ `agent.sessionId` / `agent.toolCalls` | 收尾，只补发差量（不重复发整段） |
| `error` | `error.code` / `error.message` | 立即报错终止 |

## 实测帧序列（2026-09-24，WorkBuddy CLI 2.147.0）

裸探针直接投递 + 读 SSE（不经本引擎解析），一次 410 字生成的完整序列：

```text
+  0.014s 投递 HTTP 202 → runId=16e0eb8e-…
+ 10.067s 流已建立  content-type=text/event-stream
+ 10.067s event: message
+ 10.067s data: {"status":"completed","content":{"markdown":"<410 字整段>"}}
+ 10.067s event: done
+ 10.068s data: {}
```

两条结论：

1. **没有 `streaming` 增量帧** —— 当前成色与 arkclaw 一样是「受理 + 整段正文」两帧，不是打字机。
   内核里 `convertEventToOutbound` 确实写了 `text_delta → status=streaming` 的映射，但它要求会话
   事件是 `type=model` 且带 `data.delta.type=text_delta`，这一条在本版本的 `--serve` 路径上没出现。
   本引擎**照协议**实现了增量帧的解析与去重，网关将来吐 chunk 时零改动即可接住。
2. **网关在首帧到达前不 flush 响应头** —— 流响应头到 +10.067s 才出现（= 首帧生成完）。所以「等待」
   全都发生在 `http.Client.Do` 里，`-t` 必须覆盖**整个生成过程**而不是只覆盖建连（默认 10 分钟即按此定）。

## 配置

```json
{
  "codebuddyGateway": {
    "url": "http://127.0.0.1:8399"
  }
}
```

| 字段 | 必需 | 说明 |
|---|---|---|
| `url` | ✓ | 网关根地址（**不带** `/api/v1`）。别名 `endpoint` |
| `password` | | 网关访问口令，`--serve` 默认开密码认证并在启动日志里打印；`--auth none` 时可省略。别名 `token` / `key` / `secret` |
| `platform` | | 平台标识，空 = `generic` |
| `sender` | | 发送者标识（网关按它限流与归属），空 = `magic-agent` |
| `conversation` | | 固定会话锚点；空 = 每次调用新开一个（不传 `--session` 时不串上下文） |

节名别名：`codebuddyGateway` \| `codebuddy_gateway` \| `codebuddy-gateway` \| `cbgw`。
环境变量覆盖：`MAGIC_AGENT_CBGW_URL` / `MAGIC_AGENT_CBGW_PASSWORD`。

## 用法

```bash
# 先把网关跑起来（两种形态都行）
codebuddy --serve --port 8399                     # 独立服务（口令打印在启动日志里）
# 或在交互会话里执行 /gateway                      # 远程控制形态（带 Tunnel / 二维码）

magic-agent --engines                                    # 看 codebuddy-gateway 是否已配置齐备
magic-agent -e codebuddy-gateway "你好"                   # 单轮
magic-agent -e codebuddy-gateway --session conv-1 "接着说"  # 按 conversation id 续接同一会话
magic-agent -e codebuddy-gateway --stream -o text "你好"    # 逐字流式（真打字机）
magic-agent -e codebuddy-gateway --json-schema '{"type":"object",...}' "输出 JSON"
```

## 行为细节

| 维度 | 行为 |
|---|---|
| 鉴权 | `Authorization: Bearer <password>`（网关也接受 `x-access-token` 与 cookie；口令为空时不带该头） |
| 安全头 | 所有请求带 `x-codebuddy-request: 1` —— 网关的请求校验中间件要求，缺了回 `403 Missing required header` |
| 续接 | `--session <conversation_id>` → 写入 `source.conversation.id`；网关按它 `getOrCreateSession`，多轮上下文接得上 |
| `-c, --continue` | **不支持**：网关没有「查询最近会话」的接口，显式报错并提示改用 `--session <conversation_id>` |
| `--stream` | **支持（SSE）**，但实测当前网关**只推终帧**（正文整段到达，与 arkclaw 同成色）；增量帧解析已按协议实现 |
| 输出 `session_id` | 给的是**会话锚点**（`conversation.id`），不是终帧里的 `agent.sessionId` —— 后者是网关内部 UUID，拿它当 `--session` 会静默新开会话 |
| `-m, --model` | **不生效**（模型由网关进程自己的配置决定），打一行告警后忽略 |
| `-w` / `--tools` / `--permission` | **无落地通道**：工作目录、工具集、权限档位都由网关进程的启动参数决定（网关还会把远程任务的权限强制切到 `bypassPermissions`，因为远程场景无法交互式审批）→ 能力表报 `none` |
| `-s, --system` | 展平进 prompt 头部（协议无独立 system 角色） |
| `--max-tokens` / `--temperature` | 静默忽略（协议无对应字段） |
| `--json-schema` | 支持，走与 `llm` 相同的输出后处理抽 JSON 路径 |
| 附件 | 走 `payload.attachments`（`urlType: local-path`），由网关渲染成路径清单进 prompt；**跨机时路径对网关侧无意义** |
| 默认超时 | **10 分钟**（与 CLI 的 `-t` 默认一致；网关侧 `runTimeoutMs` 默认 30 分钟，`-t` 可覆盖） |
| 网关没按 SSE 回 | **如实报错**，不假装有增量、不拿半句当结果 |
| 未配置时 | `--engines` 报 `ok:false` 并在 `note` 里点明缺 `url`、配置文件位置与环境变量名；`install` 字段**留空**（要的是「把网关跑起来」，不是安装命令） |

## llm 引擎（simonw/LLM 包装）

`-e llm` 不直连任何 HTTP 端点，而是转调 [simonw/LLM](https://github.com/simonw/LLM) CLI，
用一个工具屏蔽全部模型差异。模型注册、密钥、端点管理全部交给 llm：

```bash
llm models                          # 列出可用模型
llm keys set openai                 # 存 OpenAI 密钥
llm keys set minimax                # 存 MiniMax 密钥
```

## 注册自定义模型（OpenAI 兼容端点）

llm 只内置大厂模型；MiniMax 等需要手动注册。编辑
`~/Library/Application Support/io.datasette.llm/extra-openai-models.yaml`（Linux: `~/.config/io.datasette.llm/`）：

```yaml
- model_id: minimax-m3
  model_name: MiniMax-M3
  api_base: https://api.minimaxi.com/v1
  api_key_name: minimax     # 引用 llm keys set 存的密钥名
```

注册后即可 `magic-agent -e llm -m minimax-m3 "问题"`。

## 调用映射

| magic-agent | llm CLI |
|---|---|
| `-e llm "问题"` | `llm prompt -n "问题" --no-stream` |
| `-e llm -m <model>` | `llm prompt -n -m <model> ...` |
| `-e llm -s <system>` | `llm prompt -n -s <system> ...` |
| `--stream -e llm` | `llm prompt -n ...`（默认流式，纯文本 stdout） |

流式输出是纯文本（非 NDJSON）。MiniMax 等推理模型会把思维链以标签形式混在
正文里，magic-agent 内置状态机把标签块路由到 thinking 通道（实测 llm 的
`-R/--hide-reasoning` 挡不住 MiniMax 的标签，所以剥离必须自己做）。

## 安装

`npm install` 时 postinstall 自动装（已有则跳过）：

- 探测顺序：`MAGIC_AGENT_LLM_BIN` → `~/.llm-venv/bin/llm` → PATH → brew
- 都没有时：`python3 -m venv ~/.llm-venv && pip install llm`（隔离安装，不污染系统 Python）
- 安装失败只告警，不阻断；claude/codebuddy/trae 不受影响

手动安装任选：

```bash
pip install llm            # 或 pipx install llm / brew install llm
export MAGIC_AGENT_LLM_BIN=$(which llm)   # 装在非默认位置时指定
```

> macOS Homebrew Python 3.14 的 pip 有 truststore bug（`invalid literal for int()`
> 报错），建议用 venv / pipx 方式安装。

## 密钥与数据位置

| 内容 | 位置 |
|---|---|
| 密钥 | `~/Library/Application Support/io.datasette.llm/keys.json`（Linux: `~/.config/io.datasette.llm/`） |
| 自定义模型 | 同目录 `extra-openai-models.yaml` |
| 会话日志 | 同目录 `logs.db`（llm 自身功能，magic-agent 不读写） |

## dsh 引擎（DeepSeek Harness）

`-e dsh` 调本机 [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)（简称 DSH，
命令行 `dsh`，npm 包 `@deepseek-ai/dsh`，MIT）。**两条通道，默认 SDK，不可用时回退 headless**：

| 通道 | 命令 | 能力 |
|---|---|---|
| **SDK**（默认） | `dsh --profile sdk` + stdio 换行分帧 JSON-RPC 2.0 | 工具调用（`tool/call` / `tool/result`）、正文逐 step 流式、`-m` 换模型、`--max-tokens` |
| headless（回退） | `dsh --profile headless "<任务>"` | 只有最终正文（stdout）+ 推理增量（stderr）；**没有工具调用通道** |

为什么默认 SDK：headless 的官方定位就是「reasoning 走 stderr、最终正文走 stdout、然后退出」——
实测模型用了 `bash` / `glob` / `read`，CLI 的 stdout/stderr **只字未提**，工具调用只写进
`$DSH_HOME/sessions/…/session.v3.jsonl.zstd`（zstd 压缩）。SDK profile 把会话事件实时推给
客户端，工具调用才拿得到。SDK 起不来（例如旧版 dsh 没有这个 profile）时自动回退 headless，
并在 stderr 说明「本轮回退，拿不到工具调用、正文不流式」。

```bash
npm i -g @deepseek-ai/dsh      # 或 npx @deepseek-ai/dsh web（Web UI）
export MAGIC_AGENT_DSH_BIN=$(which dsh)   # 装在非默认位置时指定

magic-agent -e dsh "把 tests 跑一遍并总结失败原因"
magic-agent -e dsh -w ~/proj "重构这个模块"     # workspace = 子进程 cwd（dsh 原生语义）
magic-agent -e dsh -a shot.png "看截图报错"     # 附件无原生通道 → 路径写进提示词
magic-agent -e dsh --stream "看看这个仓库"       # 推理 / 正文 / 工具调用都实时出（见下）
magic-agent -e dsh -m modelverse/deepseek-v4-pro-0813 "换个模型答"   # SDK 通道下 -m 生效
magic-agent -e dsh --stream --keep-alive "记住 42"                   # 常驻会话（dsh 默认关，要显式传）
magic-agent --append <run_id> -p "刚才那个数是多少"                    # 追问：上下文接得上
MAGIC_AGENT_DSH_PROFILE=headless magic-agent -e dsh "..."            # 强制回退通道（排障用）
magic-agent --engines                          # 看 dsh 是否已装（bin 探测 + install）
```

## 配置模型（不在本 CLI 上切）

dsh 的模型由**配置层**决定：`$DSH_HOME/settings.yaml`（默认 `~/.dsh/settings.yaml`）。
这里以接一个 OpenAI 兼容网关（Modelverse）为例：

```yaml
# ~/.dsh/settings.yaml
agent-default-model:
  provider: modelverse
  model: deepseek-v4.1-flash

llm-pi-ai:
  providers:
    modelverse:
      displayName: Modelverse
      api: openai-completions          # 网关协议（OpenAI Chat Completions）
      baseURL: https://api.modelverse.cn/v1
      apiKeyEnv: MODELVERSE_API_KEY    # 只是「引用」，密钥本体不写在这里
      models:
        - id: deepseek-v4.1-flash
```

密钥本体写进 dsh 的凭据存储（`chmod 600`；也可用 `$DSH_HOME/.env` 或启动环境变量）：

```yaml
# ~/.dsh/.credentials.yaml
version: 1
refs:
  MODELVERSE_API_KEY: <你的 key>
```

解析优先级（官方文档）：**启动环境 > 存储文件 > 项目 `.env` > `$DSH_HOME/.env`**。
配完直接 `dsh --profile headless "1+1=?"` 验证；`magic-agent --engines` 里 dsh 的
`streaming` 应为 `true`、`models` 应列出你在 `models:` 下声明的那些（形如
`modelverse/glm-5.3`）。注意 `dsh --dump-config` **不反映** settings 层（它只 dump
插件组合树），验证要看实际请求。

> **只有 `models:` 里声明过的模型才被路由接受** —— 网关 `GET /v1/models` 返回的
> 上百个模型里，没声明的一律 `UNKNOWN_MODEL`。所以 `--engines` 的 dsh `models`
> 取自配置层（不是网关），它同时就是「你真正能用的那批」。要加模型：在 `models:`
> 下追加 `- id: <model-id>` 即可，无需重启。

> `-m` 的可取值就是上面这份清单（`route/model` 形态）—— **但只在 SDK 通道下生效**：
> 它转成 `initialize` 的 `provider` / `model` 两个必填参数（裸 id 按配置层那条路由解释）。
> 回退 headless 后 `-m` 不生效（无 `--model` 参数），会打一行告警。

> 自建路由需要同时给 `api`、`baseURL` 与非空 `models` 列表（官方要求）；
> 只改 `agent-default-model` 而不声明 provider 会以 `UNKNOWN_MODEL` / `MISSING_CREDENTIAL` 失败。

## 流式（`--stream`）

两条通道的成色不同：

**SDK 通道（默认）**：`session.event` 实时推送，推理 / 正文 / 工具调用都真流式。

| 会话事件 | 映射到 | 说明 |
|---|---|---|
| `assistant/message` 的 `reasoning` 块 | `thinking` | 推理增量 |
| `assistant/message` 的 `text` 块 | `text` | **逐 step 到达**（不必等整轮结束） |
| `tool/call` | `tool_use` | 带 `name` / `arguments`（原样 JSON 字符串）/ `callId` |
| `tool/result` | `tool_result` | 按 `callId` 关联回上一条 `tool_use` |
| `turn/end` | `turn_end` | 本轮结束 |

```bash
magic-agent -e dsh --stream -o text "用 bash 执行 echo hi 并贴出输出"
# …推理…🔧 bash(call_00_xx) {"command":"echo hi","description":"…"}
#    ↳ hi
magic-agent -e dsh --stream "用 bash 执行 echo hi 并贴出输出"    # json：NDJSON 事件流
# {"type":"tool_use","text":"{\"command\":\"echo hi\"…}","name":"bash","id":"call_00_xx"}
# {"type":"tool_result","text":"hi\n","id":"call_00_xx"}
# {"type":"thinking","text":"…"}
# {"type":"text","text":"输出如下：…"}
# {"type":"turn_end","text":"输出如下：…"}
# {"type":"result","engine":"dsh","model":"modelverse/deepseek-v4.1-flash","attempts":1,"latency_ms":6354,
#  "text":"输出如下：…","tools":[{"Name":"bash","ID":"call_00_xx","Args":"{…}","Result":"hi\n"}]}
```

**headless 回退**：方向与其余引擎相反（实测 dsh 0.1.5-rc.2）—— 推理增量走 **stderr**
（`dsh: reasoning:` 标题后逐段到达，约 300ms 一批），最终正文在 turn 结束时**一次性**
打到 stdout。所以只流推理，正文收尾一条 `text` 事件；**这条路上没有工具事件**。

```bash
magic-agent -e dsh --stream -o text "算 17*23"
# {"type":"thinking","text":"17*23 = 391.\n"}
# {"type":"text","text":"17 × 23 = 391\n\n计算过程：…"}
```

回退路径实际发出的命令是 `dsh --profile headless "<任务>"`：启动器 flag 在前，**任务文本是
headless 唯一的 app 参数**（没有 `--prompt`，也不走 stdin），因此超长提示词受
ARG_MAX 限制 —— 要喂长文本请先落成文件、让 dsh 自己的读文件工具去看（`-a` 的
路径兜底同理）。SDK 通道的 prompt 走 `session/prompt` 的 `contentBlocks`，不占命令行。

| 维度 | 行为 |
|---|---|
| 取文 | SDK 通道：正文来自 `assistant/message` 的 `text` 块（逐 step 累积）；回退 headless：**stdout 就是最终助手正文**（纯文本，无 JSON / 无 `--output-format`），stderr 的 `dsh: reasoning:` 增量**不混入正文** |
| 工具调用 | **只有 SDK 通道有**（`tool/call` / `tool/result` → `tool_use` / `tool_result` 事件与 `result.tools`）。headless 的 stdout/stderr 里没有工具事件 —— 这是官方定位，不是解析缺陷 |
| 退出码 | SDK 通道：一轮由 `turn/end` 收尾（`reason.kind`）；回退 headless：引擎侧 `turn/end` 为 `completed` → 0，否则 1，非零退出时把 stderr 末尾的终止原因摘进错误信息 |
| `-m, --model` | **SDK 通道生效**：转成 `initialize` 的 `provider` / `model`（取 `route/model` 形态，可用值见 `--engines` 的 `models`）；**回退 headless 不生效**（无 `--model` 参数），会打一行告警。两种情况下输出的 `model` 都反映本轮实际用的路由 |
| 会话续接 | **多轮上下文走常驻会话**：`-e dsh --stream --keep-alive "首轮"` + `--append <run_id> -p "追问"`（SDK 通道下同一个 runtime 进程活着，每条追加都排进同一 `sessionId` → 官方推荐的续接方式，官方 Python SDK 文档：「reuse a harness, home, and id only to continue the same durable conversation」）。⚠️ dsh 的常驻**默认关**（与 claude/codebuddy 不同），要显式 `--keep-alive`：它的 SDK 通道本来是一次一轮的形态，默认挂 5 分钟空闲窗口会让既有 `--stream` 调用以为命令卡住了。<br>**`-s/--session` 按 id 续接不支持**：headless 每次调用都是全新会话；SDK 的 `sessionId` 只在**同一个 runtime 进程内**可续，换进程拿旧 id 会被 `-32603 session "x" already exists` 拒掉（服务端 `createSession` 只调 `ctx.agents.create`，从不 resume；协议也只有 `initialize` / `session/prompt` / `shutdown` 三个方法）。harness 核心其实有 `agents.resume`（「Load a persisted session and resume an agent on it」），但 SDK 协议没暴露它 —— 只有 Web/TUI 那条 host API 用得上。会话本身持久化在 `$DSH_HOME`，可在 dsh 的 TUI/Web 界面里续接 |
| `-w, --workspace` | **支持**（`cwd`）：dsh 的官方语义就是「调用时所在目录即默认 workspace 根」；SDK 通道另把该目录交给 `initialize` 的 `cwd` |
| `--stream` | **支持**：SDK 通道全流式（见上）；回退 headless 后只流推理，正文收尾一次性给出 |
| `--keep-alive` / `--append` | **支持（dsh 默认关，需显式 `--keep-alive`）**：靠 SDK 通道对同一会话继续 prompt 实现多轮上下文（机制与 claude/codebuddy 的 stream-json stdin 不同，但 CLI 侧同一套 `--append` 入口）。SDK 通道不可用时**明确报错**，不会拿 headless 顶替（那会把追加消息静默丢掉） |
| `-a, --attach` | `prompt` 通道：把**绝对路径**写进提示词末尾的「【附件】」清单，靠 dsh 自己的读文件工具看 |
| `-s, --system` | 展平进任务文本头部（无独立 system 注入 flag） |
| `--json-schema` | 支持，走与 `llm` / `openclaw` 相同的输出后处理抽 JSON 路径 |
| `--max-tokens` | **SDK 通道生效**（`initialize` 的 `maxTokens`，官方语义「限制 SDK 创建的 agent 及其进程内后代的每次对话模型输出」）；回退 headless 静默忽略（无对应 flag） |
| `--temperature` | 静默忽略（无对应 flag） |
| `--tools` | **忽略**：dsh 自带 agent 工具循环（base bundle 的 `read` / `write` / `edit` + Bash），增删工具靠 profile bundle / `cordis.patch.yml` 配置，没有命令行级逐工具开关 |
| `--permission` | **未接线**：新会话默认 `workspace-write` 预设（写入限工作区 + 平台临时目录），另有 `read-only` 预设、进程回退由环境变量 `DSH_PERMISSION_MODE` 决定；这两个预设表达不了四档模型里的「沙箱关闭 / 无审批」，故不猜映射 —— 显式传 `--permission` 会 `exit 2` 报错 |
| 默认超时 | 10 分钟（dsh 自带工具循环，一轮任务常常是分钟级；超时由外层进程组 kill 兜底） |
| 凭据 | 由 dsh 自管：`DEEPSEEK_API_KEY` 等环境变量或 `$DSH_HOME/.credentials.yaml`；magic-agent 不读写 |

> SDK 通道是**默认**，但它是预发布协议（官方自述「无协议版本协商、无兼容承诺」）。
> 排障时可一键切回旧通道，不用改代码：`MAGIC_AGENT_DSH_PROFILE=headless magic-agent -e dsh "..."`。

> DSH 目前是 **developer preview**（官方声明会有破坏性变更）。本引擎按官方
> `apps/cli/reference`（headless）与 `@deepseek-ai/dsh-sdk-protocol` 的
> `README.zh.md` / `lib/types/types.d.ts`（SDK）实现：headless 的 app 参数表只有任务文本、
> stdout 只给最终正文、退出码 0/1；SDK 只依赖 `initialize` / `session/prompt` / `shutdown`
> 三个方法与 `session.event` / `session.status` 两个通知（未知 `event.type` 一律忽略）——
> dsh 升级后若这些契约变化，需要同步调整。

## codex 引擎：CODEX_HOME 隔离与 web_search 覆写

**现象**：codex 引擎调用失败（`Error: timed out waiting for cloud config bundle after 15s`），
同样的 `codex exec` 在终端里手动执行也可能复现（取决于网络环境）。

**根因**（2026-09-16 实测，codex-cli 0.154.0）：`~/.codex/auth.json` 存在
ChatGPT 登录态（`auth_mode: "chatgpt"`）时，codex 启动会拉取 **cloud config
bundle**（企业云端配置），目标域名 `auth.openai.com`。若系统 DNS 解析该域名
失败/超时，codex 在 15s 后报错退出。实测本机 `curl` 裸解析 8s 无响应，但
`--resolve` 指定 Cloudflare IP 后 0.9s 返回 200 —— 纯 DNS 路径问题。且
`-c cloud_config.enabled=false` 等 `-c` 覆写**无法绕过**（0.154 无此配置键）。
关键矛盾：config.toml 用的是 custom provider（本地代理 `127.0.0.1:15721`），
根本不需要 ChatGPT 登录态 —— **auth.json 是唯一触发点**。

**修复**：magic-agent 为 codex 子进程自动注入隔离的 CODEX_HOME，镜像目录
`~/.magic-agent/codex-home/`：

- 同步 `config.toml`、`AGENTS.md`，以及 config.toml 引用的相对路径数据文件
  （如 cc-switch 的 `model_catalog_json`）——引用提取按「无 `/` + 带扩展名」
  过滤，model 名/枚举值/token 不会被误当文件名；
- **绝不同步 `auth.json`**（无登录态 → 不拉 cloud config → exec 直连
  custom provider 正常返回）；
- 幂等同步：镜像文件写入后回写源 mtime，`size+mtime` 一致即跳过，源变更
  才重同步（文件均为 KB 级，开销可忽略）；
- 同步失败不阻断调用，静默回退无隔离的旧行为；镜像内 sessions 由 codex
  自行落盘，resume 续接天然工作在同一镜像内，自洽。

行为矩阵：

| 环境 | 行为 |
|---|---|
| 默认 | 自动镜像 `~/.codex` → `~/.magic-agent/codex-home/` 并注入 |
| 外部已设 `CODEX_HOME` | 尊重用户环境，不做隔离/同步 |
| `MAGIC_AGENT_CODEX_HOME=<dir>` | 直接用该目录（跳过自动同步） |
| `~/.codex/config.toml` 不存在 | 不启用隔离（保持原行为） |

## web_search 覆写（MAGIC_AGENT_CODEX_WEBSEARCH）

config.toml 常配 `web_search = "disabled"`（无联网），联网类查询（如「当日
热点新闻」）需要放开。设环境变量后 magic-agent 注入 `-c web_search=<v>`：

```bash
MAGIC_AGENT_CODEX_WEBSEARCH=live magic-agent -e codex --tools on -o text "今天的科技新闻"
```

- 值域：`disabled` \| `cached` \| `indexed` \| `live`（0.154 实测合法值，
  **没有** `enabled`，传了会报 `unknown variant`）；
- 仅非 resume 调用注入（`exec resume` 子命令不接受额外 `-c` 之外的参数
  变更，续接时延用首次会话设置）；
- 未设置该变量时保持 config.toml 原值，不做任何改写。

验证（2026-09-16，本机）：

```bash
magic-agent -e codex -o text -t 60s "1+1=?"                  # 7.5s → "2"
MAGIC_AGENT_CODEX_WEBSEARCH=live magic-agent -e codex --tools on \
  -o text -t 120s "今天的科技新闻"                            # → 9 条真实当日新闻
```
