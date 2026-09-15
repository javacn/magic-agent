# magic-agent

专业的 agent CLI 代理工具 —— 把 **claude / codebuddy / trae** 三家 CLI 的非交互调用统一成一条命令，提供一致的引擎/模型切换、超时与重试、固定输出格式与稳定退出码。适合脚本化编排与上层工具（如 magic-video）集成。

从 [magic-video](../magic-video) 的 `base/llm/codebuddy.go` / `base/llm/trae.go` / `base/engine.go` 剥离而来，独立演进。

## 安装

### 方式一：npm（推荐，无需 Go）

包内自带 **darwin-arm64 / darwin-x64 / linux-x64 / linux-arm64 / win32-x64** 五个平台的预编译二进制，安装时按当前平台自动选取。

```bash
# 全局安装（推荐）
npm install -g magic-agent

# 或从本地 tgz 安装（离线 / 内网）
npm install -g ./magic-agent-0.1.0.tgz

# 或装进当前项目
npm install magic-agent && ./node_modules/.bin/magic-agent version
```

`magic-agent` 会出现在 PATH 中（全局装时）。

**装到哪个 npm 前缀很重要**：全局安装落在 `npm config get prefix` 指向的 `bin/` 下。如果 `which magic-agent` 找不到，多半是装到了别的 node 环境（比如某个沙箱/工具链自带的 node）。确认并修正：

```bash
npm config get prefix          # 看你的真实前缀，通常 /opt/homebrew
which -a npm node              # 确认用的是哪个 npm
```

用你登录 shell 里那个 npm 安装（例如 `/opt/homebrew/bin/npm install -g ./magic-agent-0.1.0.tgz`），装完新开一个终端即可用。

### 方式二：从源码构建

```bash
go build -o bin/magic-agent ./cmd/magic-agent
```

### 打包发布（维护者）

```bash
npm run build      # 交叉编译全部 5 个平台 -> npm/dist/
npm pack           # 产出 magic-agent-<version>.tgz
```

`npm run build:current` 只编译当前平台（本地开发更快）。版本号由 `npm build` 从 `package.json` 经 ldflags 注入 `internal/cli.Version`，无需手改源码。

> 平台不支持或产物缺失时，`postinstall` 会尝试用本机 Go 现场编译；两者都没有则只告警，不阻断安装。

依赖：Go 1.26+（仅源码构建需要）。三个 CLI 按需安装，未安装的引擎自动探测失败但不影响其他引擎：

| 引擎 | CLI | 探测路径 / 环境变量 |
|------|-----|---------------------|
| claude | Claude Code | `/opt/homebrew/bin/claude` → PATH → `MAGIC_AGENT_CLAUDE_BIN` |
| codebuddy | WorkBuddy 内置 CLI | `WorkBuddy.app/.../cli/bin/codebuddy` → PATH → `MAGIC_AGENT_CODEBUDDY_BIN` |
| trae | trae-cli | `~/.local/bin/trae-cli` → PATH → `MAGIC_AGENT_TRAE_BIN` |
| llm | 无（读 `~/.magic-agent/models.json`） | 扁平模型数组；配置缺失即不可用；`MAGIC_AGENT_MODELS` 可换路径 |

```bash
$ magic-agent --engines
ENGINE     STATUS  CLI
claude     ✓       /opt/homebrew/bin/claude
codebuddy  ✓       /Applications/WorkBuddy.app/.../cli/bin/codebuddy
trae       ✓       /Users/you/.local/bin/trae-cli
llm        ✓       /Users/you/.magic-agent/models.json (3 models, default=MiniMax-M3)
```

## 用法

无子命令设计：所有功能都走根命令 + flags。

```bash
# 版本
magic-agent --version

# 基本提问：直接给 prompt（默认 json 输出）
magic-agent -p "用一句话解释什么是熵"
magic-agent "1+1=?"            # 位置参数等价

# 人看用 text
magic-agent -o text -p "用一句话解释什么是熵"

# 切引擎、切模型
magic-agent -e codebuddy "写一首俳句"            # codebuddy 默认 hy3
magic-agent -e codebuddy -m glm-5.3 "写一首俳句"
magic-agent -e trae "总结这篇文档"               # trae 用自身配置的默认模型
magic-agent -e trae -m My-MiniMax-M3 "..."      # -c model.name= 覆盖

# 直连 LLM（读 ~/.magic-agent/models.json，扁平数组，见下节）
magic-agent -e llm -m MiniMax-M3 "问题"          # 按 id 指定
magic-agent -e llm -m minimax-nothink "问题"     # 同模型不同变体（关思维链）
magic-agent -e llm "问题"                        # 用数组首条

# 工具开关（默认 off = 纯 chat；on = agent 模式；或白名单）
magic-agent -e claude --tools on -p "看看当前目录有什么"
magic-agent -e claude --tools Bash,Read -p "读一下 README"

# 超时 + 重试（单次尝试 3 分钟，最多额外重试 2 次，指数退避）
magic-agent -e claude -t 3m -r 2 --verbose -o text "复杂的分析任务"

# 系统提示词
magic-agent -e claude -s "你是严谨的翻译官，只输出译文" -o text "Hello, world"

# 管道输入
cat doc.md | magic-agent -e claude -o text -f - "总结上文"
magic-agent -e claude -f context.md "基于这个文件回答：……"

# 固定 JSON 输出（单行 envelope，适合 jq / 程序解析）
magic-agent -e claude "1+1=?"
# {"engine":"claude","model":"","session_id":"...","attempts":1,"latency_ms":534,"text":"2"}

# 流式输出（正文/思考实时增量；--no-thinking 关思考）
magic-agent --stream -e claude -o text "复杂问题"          # 正文→stdout，思考→stderr
cat doc.md | magic-agent --stream -e claude - "总结"        # json NDJSON 事件流 + result 收尾行
magic-agent --stream --no-thinking -e claude "问题"        # 只要正文增量

# 引擎可用性
magic-agent --engines          # text 表格
magic-agent --engines --json   # 单行 JSON 数组（jq 友好）
```

## 流式模式（--stream）

三家引擎都走 CLI 原生 `stream-json` 协议，增量实时转发，无缓冲等待：

| | claude / codebuddy | trae |
|---|---|---|
| 协议 | `stream_event` + `content_block_delta` | `stream_event` + `delta.content` |
| 思考过程 | ✅ `thinking_delta`（模型开 reasoning 时） | ❌（模型侧无 reasoning 通道） |
| 收尾 | `result` 行（全文以此为准） | 同左 |

**text 模式**：正文增量 → stdout 实时打印；思考增量 → stderr（`…` 前缀），
`2>/dev/null` 静音或 `2>&1 | tee` 保留都由你控制。

**json 模式**（默认）：每条增量一行 NDJSON，收尾一行汇总 envelope：

```json
{"type":"thinking","text":"用户在做加法..."}
{"type":"text","text":"2"}
{"type":"result","engine":"claude","model":"...","attempts":1,"latency_ms":1211,"thinking":"...","text":"2"}
```

`jq -c 'select(.type != "result")'` 逐事件消费，或 `tail -1` 取 result 全文。

**语义差异**（相对非流式）：流式不做自动重试（增量已实时发出，重放会重复消费），
`-r` 被忽略；超时照常生效（杀整个 CLI 进程组）。codebuddy 引擎本环境单次调用
长期不返回（与非流式行为一致），流式实现按同源协议提供。

## Flags

| Flag | 默认 | 说明 |
|------|------|------|
| `-e, --engine` | `codebuddy` | 引擎：`claude` \| `codebuddy` \| `trae` \| `llm` |
| `-m, --model` | 空 | 模型（空 = 引擎默认；codebuddy 默认 `hy3`；llm 引擎读 models.json）。trae 无 `--model`，内部转 `-c model.name=<m>` |
| `-s, --system` | 空 | 系统提示词（claude/codebuddy 走 `--append-system-prompt`，trae 拼进 prompt） |
| `-p, --prompt` | 空 | 提示词 |
| `-f, --file` | 空 | 从文件读 prompt（`-` = stdin）；与位置参数可组合，文件在前 |
| `--tools` | `off` | `off`（纯 chat）\| `on`（agent 模式）\| 逗号分隔白名单（如 `Bash,Read`） |
| `-t, --timeout` | `600s` | 单次尝试超时（如 `90s` / `3m`） |
| `-r, --retries` | `0` | 失败重试次数（总尝试 = 1 + retries） |
| `--backoff` | `2s` | 首次重试退避（指数翻倍，上限 30s，带抖动） |
| `-o, --output` | `json` | 输出格式：`json` \| `text` |
| `--stream` | 关 | 流式输出：增量实时打到 stdout（text 模式思考走 stderr） |
| `--no-thinking` | 关 | 流式模式下不转发思考过程增量 |
| `--engines` | 关 | 列出引擎与本机 CLI 可用性（替代原 `engines` 子命令） |
| `--json` | 关 | `--engines` 的 JSON 输出开关 |
| `-v, --verbose` | `false` | 重试过程打印到 stderr |

prompt 输入优先级：`-p/--prompt` > 位置参数 > `--file` > stdin 管道（stdin 非 TTY 且无其他输入时自动读）。

## llm 引擎与 models.json

`-e llm` 直接按配置调 LLM，不依赖任何外部 agent CLI，也不依赖任何第三方 LLM 工具。
配置读取顺序（先命中先用）：

1. `$MAGIC_AGENT_MODELS` 指定的路径
2. `~/.magic-agent/models.json`

配置文件是一个**扁平 JSON 数组**，每条模型自带完整的 `url` + `apiKey`：

```json
[
  {
    "id": "MiniMax-M3",
    "name": "MiniMax-M3",
    "vendor": "MiniMax",
    "url": "https://api.minimaxi.com/v1/chat/completions",
    "apiKey": "sk-...",
    "supportsToolCall": true,
    "supportsImages": true,
    "supportsReasoning": true,
    "maxInputTokens": 1000000,
    "maxOutputTokens": 524288,
    "timeout": 600
  },
  {
    "id": "minimax-nothink",
    "model": "MiniMax-M3",
    "name": "MiniMax-M3 (no thinking)",
    "vendor": "MiniMax",
    "url": "https://api.minimaxi.com/v1/chat/completions",
    "apiKey": "sk-...",
    "timeout": 600,
    "extraBody": { "thinking": { "type": "disabled" } }
  }
]
```

### 字段

本格式与 WorkBuddy / CodeBuddy CLI 的 `models.json`（`LanguageModel`）**互为超集** ——
对方的条目直接拿来也能读，本工具只是多了三个扩展字段（`timeout` / `extraBody` / `temperature`）。

| 字段 | 必填 | 说明 |
|------|------|------|
| `id` | ✓ | 调用方标识（`-m` 用）。文件内需唯一（忽略大小写） |
| `model` | | 发往 API 的真实模型名；空 = 用 `id`。见下方「同模型多变体」 |
| `name` / `vendor` | | 展示用 |
| `url` | ✓ | 完整 API 端点。支持 `${ENV_VAR}` |
| `apiKey` | | Bearer 密钥。支持 `${ENV_VAR}`；本地端点可留空 |
| `useCustomProtocol` | | `true` = `url` 原样透传，不补 `/chat/completions` |
| `supportsToolCall` / `supportsImages` / `supportsReasoning` | | 能力标记（仅展示/校验，不参与请求构造） |
| `maxInputTokens` / `maxOutputTokens` | | 上下文与输出上限（仅展示） |
| `timeout` | | 单次 HTTP 超时（秒）；0 = 默认 300s。**magic-agent 扩展** |
| `extraBody` | | 原样并入请求体顶层。**magic-agent 扩展**，见下 |
| `temperature` | | 采样温度；不写则不发该字段。**magic-agent 扩展** |

### 默认模型与 `-m`

```
不给 -m           # 用数组首条（顺序即优先级）
-m MiniMax-M3     # 按 id 指定（忽略大小写，命中后回填规范大小写）
```

没有 `default` 包装字段：**数组首条即默认**。

### 同模型多变体：`id` 与 `model`

API 只认线上模型名，但你可能需要同一个模型出现多次（不同 key、不同开关）。
这时用 `id` 区分调用方视角、用 `model` 指定线上模型名：

```jsonc
{ "id": "MiniMax-M3",       "url": "...", "apiKey": "k1" },            // model 省略 → 发 "MiniMax-M3"
{ "id": "minimax-peter",    "model": "MiniMax-M3", "url": "...", "apiKey": "k2" },
{ "id": "minimax-nothink",  "model": "MiniMax-M3", "url": "...", "extraBody": {"thinking":{"type":"disabled"}} }
```

`magic-agent -e llm -m minimax-nothink` 发往 API 的 `model` 仍是 `MiniMax-M3`，
但输 envelope 的 `model` 字段是 `minimax-nothink`，便于区分路由来源。

### URL 补全规则

| `url` 写法 | 实际请求 |
|---|---|
| `https://api.minimaxi.com/v1/chat/completions` | 原样（已带完整路径） |
| `https://ark.cn-beijing.volces.com/api/coding/v3` | 自动补成 `.../api/coding/v3/chat/completions` |
| 任意写法 + `"useCustomProtocol": true` | 原样透传，不补全 |

规则与 WorkBuddy 一致：`url` 是接口完整路径时直接用，是 base 地址时补 `/chat/completions`。

### 环境变量引用

`apiKey` 与 `url` 支持 `${VAR}` 语法，便于把密钥留在环境里而非文件内：

```json
{ "id": "gpt-4o", "url": "https://api.openai.com/v1/chat/completions", "apiKey": "${OPENAI_API_KEY}" }
```

变量不存在时**保留占位符原样**（不静默置空），让问题在发请求时以 401/404 暴露。

### extraBody 是必须保留的字段

推理模型（如 MiniMax-M3）不关思维链时，长文本生成会被推理吃光 token 预算、**正文为空**。
`extraBody` 原样并入请求体顶层，是关掉它的唯一手段：

```json
"extraBody": { "thinking": { "type": "disabled" } }
```

HTTP 客户端另外固定发 `reasoning_split: true`，让 thinking 走 `reasoning_content`，
并对 content 里残留的思维链标签做兜底剥离。

配置里含 API key，建议 `chmod 600 ~/.magic-agent/models.json`。

## 输出格式

**json**（默认）：stdout 单行 envelope：

```json
{"engine":"claude","model":"claude-sonnet-4-6","session_id":"...","attempts":1,"latency_ms":534,"text":"..."}
```

**text**（`-o text`）：stdout 只含模型正文 + 尾换行。

**失败时**：stdout 恒为空（不产生半截内容），错误打到 stderr，**格式与 `-o` 联动**：

```bash
# -o json（默认）：单行错误 envelope，error 与 reason 双字段
$ magic-agent -e claude -t 2s "写一篇万字长文"
{"engine":"claude","attempts":1,
 "error":"claude: all 1 attempts failed: claude CLI: process group killed: context deadline exceeded",
 "reason":"process group killed: context deadline exceeded"}

# 参数类错误（exit 2）同样输出 envelope（attempts=0 表示尚未执行任何尝试）
$ magic-agent -e nope "hi"
{"engine":"nope","attempts":0,"error":"unknown engine \"nope\" ...","reason":"unknown engine \"nope\" ..."}

# -o text：单行纯文本
$ magic-agent -o text -e nope "hi"
magic-agent: unknown engine "nope" (available: claude, codebuddy, trae, llm)
```

字段说明：

| 字段 | 说明 |
|------|------|
| `error` | 完整错误链（含引擎/重试包装），面向人排查 |
| `reason` | 最内层根因（stderr 摘要 / 超时 / 非零退出码），面向程序分支判断 |
| `attempts` | 实际执行次数；`0` = 参数校验阶段即失败 |

调用方约定：**stderr 整体可按 JSON 解析**（json 模式下只有这一行 envelope），jq 直接 `jq -r .reason` 取根因。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功 |
| 1 | 调用失败（引擎错误 / 超时耗尽 / 重试耗尽） |
| 2 | 参数或输入错误（未知引擎、非法格式、空 prompt、已删除的子命令形态等） |

## 重试语义

- **可重试**：网络类（connection refused/reset、EOF、timeout、signal killed）、限流类（429、rate limit、503 overloaded）、单次尝试超时。
- **快速失败**：参数错、鉴权错、空输出、CLI 明确报错等不可恢复错误——不浪费重试。
- 退避：`backoff * 2^(n-1)` + 抖动，上限 30s；整体 context 取消会立即打断等待。

## 引擎差异说明

| | claude | codebuddy | trae | llm |
|---|---|---|---|---|
| 非交互模式 | `-p --output-format json` | `--print --output-format json` | `-p`（纯文本） | HTTP `/chat/completions` |
| 模型指定 | `--model <m>` | `--model <m>` | `-c model.name=<m>`（无 --model flag） | `-m <id>`（models.json 的 id） |
| 默认模型 | CLI 配置 | `hy3`（可 `-m` 覆盖） | `~/.trae/trae_cli.yaml` 的 `model.name` | models.json 的**首条** |
| system 注入 | `--append-system-prompt` | `--append-system-prompt` | 拼进 prompt 头 | HTTP 走 messages[0] |
| 工具禁用 | `--tools ""` | `--tools ""` | `--disallowed-tool`（Bash/Edit/… 逐个） | 不适用（纯 chat） |
| 超时联动 | 进程组 kill | 进程组 kill | 另透传 `--query-timeout`（上限 600s） | HTTP client timeout |

所有引擎都以独立**进程组**运行：超时/取消时 `kill(-pgid)` 杀掉整个进程树，CLI 内部 spawn 的 node worker 不会残留（有回归测试保障）。

## 架构

```
cmd/magic-agent/main.go     入口
internal/cli/               cobra 命令层（无子命令、参数校验、退出码、流式分流）
internal/agent/
  engine.go                 Engine 接口 + Request/Response + 注册表（含 llm）
  stream.go                 Streamer 接口 + StreamEvent + NDJSON 流解析
  prompt.go                 多轮消息扁平化 + noToolSuffix 约束
  claude.go                 Claude Code 引擎（非流式 + 流式）
  codebuddy.go              CodeBuddy 引擎（envelope 多形态解析 + 回显剥离 + 流式）
  trae.go                   Trae 引擎（模型覆盖 + query-timeout 映射 + 流式）
  models.go                 models.json 加载（扁平数组）+ 模型解析 + Endpoint/ENV 展开
  openai_client.go          OpenAI 兼容 HTTP 客户端（extraBody 透传、思维链剥离）
  llmengine.go              llm 引擎：按 models.json 直连 HTTP 端点
  runner.go                 超时 + 重试编排（错误分类、指数退避、可取消）
  runcmd.go                 进程组感知执行（平台无关调度）
  runcmd_unix.go            Setpgid + kill(-pgid)（darwin/linux）
  runcmd_windows.go         CREATE_NEW_PROCESS_GROUP + taskkill /T /F
  output.go                 固定 text/json 输出
npm/
  bin/magic-agent.js        npm bin 转发层（spawnSync + stdio inherit）
  lib/platform.js           平台 -> Go 目标 / 产物路径映射
  build.js                  交叉编译 5 平台 + 版本号注入
  install.js                postinstall 兜底（缺产物时现场编译）
  dist/                     构建产物（gitignore）
tests/                      （预留）跨包集成测试
```

测试：`go test ./...`（fake CLI 脚本 + httptest server，不依赖真实安装；真实引擎冒烟见下方）。

## 已验证（2026-09-15，本机）

- claude 引擎：真实调用成功（text + json + stdin 管道）
- trae 引擎：真实调用成功（默认模型 + query-timeout 映射）
- codebuddy 引擎：CLI 探测/参数构造正确；本环境该 CLI 单次调用 20 分钟不返回（与 magic-video 时代一致），超时 + 进程组清理验证通过（超时后 0 残留进程）
- **llm 引擎**（真实调用）：
  - HTTP 直连：`-m MiniMax-M3` → `{"engine":"llm","model":"MiniMax-M3",...,"text":"..."}`
  - 大小写不敏感：`-m MINIMAX-M3` → 命中并回填规范 id
  - 默认模型（不给 `-m`）→ 走数组**首条**
  - 同模型变体：`-m minimax-nothink` → 发往 API 的 `model` 仍为 `MiniMax-M3`，并带上 `extraBody` 关思维链
  - URL 补全：`url` 只给 base 时不重复/不漏拼 `/chat/completions`
  - 错误 envelope：`reason` 为根因摘要，含可用 id 清单与配置路径
- npm 分发：5 平台交叉编译通过；全局 `npm install -g ./magic-agent-0.1.0.tgz` 后 `magic-agent` 可直接调用；stdin 管道、json 输出、退出码 0/1/2、超时杀进程组均验证通过（`go test ./...` 全绿）

## 平台支持

| 平台 | 产物目录 | 进程组实现 |
|------|---------|-----------|
| macOS arm64 / x64 | `npm/dist/darwin-arm64` / `darwin-x64` | `Setpgid` + `kill(-pgid, SIGKILL)` |
| Linux x64 / arm64 | `npm/dist/linux-x64` / `linux-arm64` | 同上 |
| Windows x64 | `npm/dist/win32-x64` | `CREATE_NEW_PROCESS_GROUP` + `taskkill /T /F` |

## License

MIT
