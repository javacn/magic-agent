# magic-agent

专业的 agent CLI 代理工具 —— 把 **claude / codebuddy / trae / llm** 四家 CLI 的非交互调用统一成一条命令，提供一致的引擎/模型切换、超时与重试、固定输出格式与稳定退出码。适合脚本化编排与上层工具（如 magic-video）集成。

从 [magic-video](../magic-video) 的 `base/llm/codebuddy.go` / `base/llm/trae.go` / `base/engine.go` 剥离而来，独立演进。其中 `-e llm` 包装 [simonw/LLM](https://github.com/simonw/LLM) —— 用它屏蔽背后全部模型差异（OpenAI / Anthropic / MiniMax / ollama / 开源端点…），模型注册、密钥、端点全部由 llm 自管。

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
> 同一个 `postinstall` 还会安装 **llm CLI**（simonw/LLM，`-e llm` 引擎的依赖）：已有安装（`MAGIC_AGENT_LLM_BIN` / `~/.llm-venv` / PATH / brew）则跳过；否则建 `~/.llm-venv` 隔离安装，失败只告警不阻断。

依赖：Go 1.26+（仅源码构建需要）。四个 CLI 按需安装，未安装的引擎自动探测失败但不影响其他引擎：

| 引擎 | CLI | 探测路径 / 环境变量 |
|------|-----|---------------------|
| claude | Claude Code | `MAGIC_AGENT_CLAUDE_BIN` → `/opt/homebrew/bin/claude` → PATH |
| codebuddy | WorkBuddy 内置 CLI | `MAGIC_AGENT_CODEBUDDY_BIN` → `WorkBuddy.app/.../cli/bin/codebuddy` → PATH |
| trae | trae-cli | `MAGIC_AGENT_TRAE_BIN` → `~/.local/bin/trae-cli` → PATH |
| llm | [simonw/LLM](https://github.com/simonw/LLM) | `MAGIC_AGENT_LLM_BIN` → `~/.llm-venv/bin/llm` → `/opt/homebrew/bin/llm` → PATH |

```bash
$ magic-agent --engines
ENGINE     STATUS  CLI
claude     ✓       /opt/homebrew/bin/claude
codebuddy  ✓       /Applications/WorkBuddy.app/.../cli/bin/codebuddy
trae       ✓       /Users/you/.local/bin/trae-cli
llm        ✓       /Users/you/.llm-venv/bin/llm
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

# llm 引擎（包装 simonw/LLM）
magic-agent -e llm "问题"                # llm 的默认模型
magic-agent -e llm -m minimax-m3 "问题"  # 显式模型（llm models 里注册过的）

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

claude / codebuddy / trae 走 CLI 原生 `stream-json` NDJSON 协议；llm 走纯文本流式
（内置 thinkSplitter 把混在正文里的思维链标签路由到 thinking 通道）。增量实时转发，无缓冲等待：

| | claude / codebuddy | trae | llm |
|---|---|---|---|
| 协议 | `stream_event` + `content_block_delta` | `stream_event` + `delta.content` | 纯文本 stdout + 思维链标签 |
| 思考过程 | ✅ `thinking_delta`（模型开 reasoning 时） | ❌（模型侧无 reasoning 通道） | ✅ 标签块 → thinking 通道 |
| 收尾 | `result` 行（全文以此为准） | 同左 | EOF（全文 = 增量拼接） |

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
`-r` 被忽略；超时照常生效（杀整个 CLI 进程组）。codebuddy 引擎走同源
`stream-json` 协议（CodeBuddy Code 系）；早期「本环境单次调用长期不返回」
的现象已定位为父会话 `SERVER__PORT` 端口冲突，见「在 WorkBuddy / CodeBuddy
会话内使用」一节。

## Flags

| Flag | 默认 | 说明 |
|------|------|------|
| `-e, --engine` | `codebuddy` | 引擎：`claude` \| `codebuddy` \| `trae` \| `llm` |
| `-m, --model` | 空 | 模型（空 = 引擎默认；codebuddy 默认 `hy3`；llm 用其自身默认模型）。trae 无 `--model`，内部转 `-c model.name=<m>` |
| `-s, --system` | 空 | 系统提示词（claude/codebuddy 走 `--append-system-prompt`，trae 拼进 prompt） |
| `-p, --prompt` | 空 | 提示词 |
| `-f, --file` | 空 | 从文件读 prompt（`-` = stdin）；与位置参数可组合，文件在前 |
| `--tools` | `off` | `off`（纯 chat）\| `on`（agent 模式）\| 逗号分隔白名单（如 `WebSearch,WebFetch`） |
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

## llm 引擎（simonw/LLM 包装）

`-e llm` 不直连任何 HTTP 端点，而是转调 [simonw/LLM](https://github.com/simonw/LLM) CLI，
用一个工具屏蔽全部模型差异。模型注册、密钥、端点管理全部交给 llm：

```bash
llm models                          # 列出可用模型
llm keys set openai                 # 存 OpenAI 密钥
llm keys set minimax                # 存 MiniMax 密钥
```

### 注册自定义模型（OpenAI 兼容端点）

llm 只内置大厂模型；MiniMax 等需要手动注册。编辑
`~/Library/Application Support/io.datasette.llm/extra-openai-models.yaml`（Linux: `~/.config/io.datasette.llm/`）：

```yaml
- model_id: minimax-m3
  model_name: MiniMax-M3
  api_base: https://api.minimaxi.com/v1
  api_key_name: minimax     # 引用 llm keys set 存的密钥名
```

注册后即可 `magic-agent -e llm -m minimax-m3 "问题"`。

### 调用映射

| magic-agent | llm CLI |
|---|---|
| `-e llm "问题"` | `llm prompt -n "问题" --no-stream` |
| `-e llm -m <model>` | `llm prompt -n -m <model> ...` |
| `-e llm -s <system>` | `llm prompt -n -s <system> ...` |
| `--stream -e llm` | `llm prompt -n ...`（默认流式，纯文本 stdout） |

流式输出是纯文本（非 NDJSON）。MiniMax 等推理模型会把思维链以标签形式混在
正文里，magic-agent 内置状态机把标签块路由到 thinking 通道（实测 llm 的
`-R/--hide-reasoning` 挡不住 MiniMax 的标签，所以剥离必须自己做）。

### 安装

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

### 密钥与数据位置

| 内容 | 位置 |
|---|---|
| 密钥 | `~/Library/Application Support/io.datasette.llm/keys.json`（Linux: `~/.config/io.datasette.llm/`） |
| 自定义模型 | 同目录 `extra-openai-models.yaml` |
| 会话日志 | 同目录 `logs.db`（llm 自身功能，magic-agent 不读写） |

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

| | | claude | codebuddy | trae | llm |
|---|---|---|---|---|---|
| 非交互模式 | | `-p --output-format json` | `--print --output-format json` | `-p`（纯文本） | `llm prompt -n --no-stream` |
| 模型指定 | | `--model <m>` | `--model <m>` | `-c model.name=<m>`（无 --model flag） | `-m <m>` |
| 默认模型 | | CLI 配置 | `hy3`（可 `-m` 覆盖） | `~/.trae/trae_cli.yaml` 的 `model.name` | llm 自身的默认模型 |
| system 注入 | | `--append-system-prompt` | `--append-system-prompt` | 拼进 prompt 头 | `-s <system>` |
| 工具禁用 | | `--tools ""` | `--tools ""` | `--disallowed-tool`（Bash/Edit/… 逐个） | 不适用（纯 chat） |
| 工具启用 | | 不传 `--tools` + `-y` | `-y` | `--allowed-tool <n>` + `-y` | 不适用（纯 chat） |
| 超时联动 | | 进程组 kill | 进程组 kill | 另透传 `--query-timeout`（上限 600s） | 进程组 kill |

所有引擎都以独立**进程组**运行：超时/取消时 `kill(-pgid)` 杀掉整个进程树，CLI 内部 spawn 的 node worker 不会残留（有回归测试保障）。

## 工具启用（--tools）

`--tools` 同时管两件事：CLI 侧的工具白名单，以及是否在 system prompt 里
注入 `noToolSuffix`。

| `--tools` | CLI 参数（codebuddy） | noToolSuffix | 效果 |
|---|---|---|---|
| `off`（默认） | `--tools ""` | 注入 | 纯 chat，模型不调工具；后缀额外压制「伪工具调用」与「伪造工具返回」 |
| `on` | `-y` | 不注入 | 全工具可用（含 `WebSearch` / `WebFetch`） |
| `WebSearch,WebFetch` | `--tools WebSearch,WebFetch -y` | 不注入 | 仅白名单工具可用 |

> ⚠️ `noToolSuffix` 明文写着「严禁使用任何工具」。它**只在 `off` 模式注入** ——
> 若在 `on`/白名单下也注入，就会出现「CLI 侧工具已开、system prompt 却在
> 压制调用」的自相矛盾，表现为**启用了工具却没有网络搜索**。

`noToolSuffix` 里的约束分两层，缺一不可：

1. **严禁调用工具** —— 否则 off 模式下模型会输出 `<tool_calls:xxxx>` 假标签。
2. **严禁伪造工具返回** —— 否则被要求「输出工具抓到的内容」时，模型会凭空
   编造一份看似真实的返回体（实测约 1/6，见下）。仅禁「调用」堵不住编造。

实测（2026-09-15，本机 codebuddy 2.137.1）：

```bash
magic-agent -e codebuddy --tools on -o text "用 WebSearch 查今天的日期"      # ✅ 真调用了搜索
magic-agent -e codebuddy --tools WebSearch -o text "用 WebSearch 查今天日期" # ✅
magic-agent -e codebuddy --tools off -o text "用 WebSearch 查今天的日期"     # 返回 NO_TOOLS（符合预期）
```

**硬证据（非模型自述）**：让 `WebFetch` 抓取 `https://httpbin.org/anything?proof=<随机 nonce>`，
然后解析 CLI 的 `--output-format json` 原始消息数组：

- 出现 `type=="function_call"`（name=`WebFetch`）+ `type=="function_call_result"`（status=`completed`）；
- 该随机 nonce（模型不可能预知）出现在 **`function_call_result.output`** 里。

满足这两条即证明发生了**真实网络请求**，而非模型编造。

**off 模式防伪造效果**（`--tools off`，重复 8 次）：

| | 编造返回体 | 结果 |
|---|---|---|
| 加固前 | 1/6 | 一次输出伪造的 httpbin JSON |
| 加固后 | 0/8 | 7 次 `NO_TOOLS`，1 次明确拒绝 |

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
  tags.go                   思维链标签常量（分段拼接防 tokenizer 改写）+ 剥离
  llmengine.go              llm 引擎：包装 simonw/LLM CLI + thinkSplitter 流式标签路由
  runner.go                 超时 + 重试编排（错误分类、指数退避、可取消）
  env.go                    子进程环境构造（剔除 SERVER__* 等父进程专属变量）
  runcmd.go                 进程组感知执行（平台无关调度）
  runcmd_unix.go            Setpgid + kill(-pgid)（darwin/linux）
  runcmd_windows.go         CREATE_NEW_PROCESS_GROUP + taskkill /T /F
  output.go                 固定 text/json 输出
npm/
  bin/magic-agent.js        npm bin 转发层（spawnSync + stdio inherit）
  lib/platform.js           平台 -> Go 目标 / 产物路径映射
  build.js                  交叉编译 5 平台 + 版本号注入
  install.js                postinstall：二进制兜底 + llm CLI（simonw/LLM）安装
  dist/                     构建产物（gitignore）
tests/                      （预留）跨包集成测试
```

测试：`go test ./...`（fake CLI 脚本 + httptest server，不依赖真实安装；真实引擎冒烟见下方）。

## 已验证（2026-09-15，本机）

- claude 引擎：真实调用成功（text + json + stdin 管道）
- trae 引擎：真实调用成功（默认模型 + query-timeout 映射）
- codebuddy 引擎：CLI 探测/参数构造正确；本环境该 CLI 单次调用 20 分钟不返回（与 magic-video 时代一致），超时 + 进程组清理验证通过（超时后 0 残留进程）
- **llm 引擎**（真实调用，包装 simonw/LLM CLI）：
  - Complete：`-m minimax-m3` → `{"engine":"llm","model":"minimax-m3",...,"text":"2"}`（标签思维链剥离生效）
  - Stream：`--stream` → thinking/text 增量正确分流，标签行的换行不污染正文
  - llm 安装：postinstall 在干净 HOME 下成功建 `~/.llm-venv` 并安装 llm（0.27.1）
- npm 分发：5 平台交叉编译通过；全局 `npm install -g ./magic-agent-0.1.0.tgz` 后 `magic-agent` 可直接调用；stdin 管道、json 输出、退出码 0/1/2、超时杀进程组均验证通过（`go test ./...` 全绿）

## 平台支持

| 平台 | 产物目录 | 进程组实现 |
|------|---------|-----------|
| macOS arm64 / x64 | `npm/dist/darwin-arm64` / `darwin-x64` | `Setpgid` + `kill(-pgid, SIGKILL)` |
| Linux x64 / arm64 | `npm/dist/linux-x64` / `linux-arm64` | 同上 |
| Windows x64 | `npm/dist/win32-x64` | `CREATE_NEW_PROCESS_GROUP` + `taskkill /T /F` |

## License

MIT
