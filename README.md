# magic-agent

专业的 agent CLI 代理工具 —— 把 **claude / codebuddy / trae / llm / codex / openclaw** 六家 CLI 与 **arkclaw**（A2A JSON-RPC 网关）的非交互调用统一成一条命令，提供一致的引擎/模型切换、超时与重试、固定输出格式与稳定退出码。适合脚本化编排与上层工具（如 magic-video）集成。

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

> **改动落地三步曲（用户定稿的硬规则：「每次修改完要全局安装更新本地」+「实现了就测试 测试好了全局安装」+ 2026-09-18 再次强调「改好永远全局安装最新的」）**
>
> 1. **实现** —— 改代码，`gofmt` + `go vet ./...` 干净。
> 2. **测试** —— 单测 `go test -count=1 ./...` 全绿（`-count=1` 避免缓存骗人），**并**跑一次真机验收
>    （假 CLI 落 argv/stdin 取证 + 真实引擎冒烟；只跑单测不算「测试好了」）。
> 3. **全局安装（每次改完都要，没有例外）** —— 两份全局前缀都更新，然后 `shasum` 核对：
>
> ```bash
> npm run build && npm pack --ignore-scripts
> SANDBOX="/Users/<you>/Library/Application Support/TRAE SOLO CN/ModularData/ai-agent/vm/tools/npm-global"
> npm install -g --prefix "$SANDBOX" ./magic-agent-<version>.tgz       # 沙箱那份（观物台用）
> npm install -g --prefix /opt/homebrew ./magic-agent-<version>.tgz    # 用户登录 shell 那份
> shasum -a 256 npm/dist/darwin-arm64/magic-agent \
>   "$SANDBOX/lib/node_modules/magic-agent/npm/dist/darwin-arm64/magic-agent" \
>   /opt/homebrew/lib/node_modules/magic-agent/npm/dist/darwin-arm64/magic-agent
> ```
>
> 三份 hash 必须一致；版本号恒为 `0.2.0`，**不能只看版本号判新旧**。
> 只 build 不 install ⇒ `magic-agent` 命令还是旧行为，用户侧表现为「改了没效果」。
> `npm warn allow-scripts`（postinstall 被拦）无害：产物已预编译、`~/.llm-venv` 已存在时它什么都不做。
>
> ⚠️ **别用 `npm config get prefix` 定位沙箱那份**：它随 PATH 变 —— 同一台机器上有时解析成
> `/opt/homebrew`（于是「两份」其实是同一份，沙箱那份悄悄留着旧的，2026-09-18 就这么漏过一次），
> 有时才解析成沙箱前缀。一律用**绝对路径 `--prefix`** 显式装两份，再各自 `shasum` 比对。
>
> ⚠️ **别拿上一轮的 hash 当基准**：实测同源码连编三次 hash 完全相同，但同一台机器上前后两次
> `npm run build` 的产物 hash 可以不同（原因未查明）。所以 `shasum` 只用于验证
> 「**本轮** build + install 的三份一致」；「装的是不是最新」要靠**功能自检**
> （例如 `magic-agent --engines --no-models` 里有没有新字段）。

`npm run build:current` 只编译当前平台（本地开发更快）。版本号由 `npm build` 从 `package.json` 经 ldflags 注入 `internal/cli.Version`，无需手改源码。

> 平台不支持或产物缺失时，`postinstall` 会尝试用本机 Go 现场编译；两者都没有则只告警，不阻断安装。
> 同一个 `postinstall` 还会安装 **llm CLI**（simonw/LLM，`-e llm` 引擎的依赖）：已有安装（`MAGIC_AGENT_LLM_BIN` / `~/.llm-venv` / PATH / brew）则跳过；否则建 `~/.llm-venv` 隔离安装，失败只告警不阻断。

依赖：Go 1.26+（仅源码构建需要）。六个 CLI 按需安装，未安装的引擎自动探测失败但不影响其他引擎：

| 引擎 | CLI | 探测路径 / 环境变量 |
|------|-----|---------------------|
| claude | Claude Code | `MAGIC_AGENT_CLAUDE_BIN` → `/opt/homebrew/bin/claude` → PATH |
| codebuddy | WorkBuddy 内置 CLI | `MAGIC_AGENT_CODEBUDDY_BIN` → `WorkBuddy.app/.../cli/bin/codebuddy` → PATH |
| trae | trae-cli | `MAGIC_AGENT_TRAE_BIN` → `~/.local/bin/trae-cli` → PATH |
| llm | [simonw/LLM](https://github.com/simonw/LLM) | `MAGIC_AGENT_LLM_BIN` → `~/.llm-venv/bin/llm` → `/opt/homebrew/bin/llm` → PATH |
| codex | `@openai/codex` | `MAGIC_AGENT_CODEX_BIN` → `/opt/homebrew/bin/codex` → PATH |
| openclaw | OpenClaw | `MAGIC_AGENT_OPENCLAW_BIN` → `/opt/homebrew/bin/openclaw` → PATH |
| arkclaw | 无（远端 A2A 网关） | 配置文件 `~/.config/magic-agent/config.json` 的 `arkclaw` 节，或 `MAGIC_AGENT_ARKCLAW_URL` / `_KEY` / `_CLAW_ID` |

```bash
$ magic-agent --engines
[{"engine":"claude","ok":true,"bin":"/opt/homebrew/bin/claude","models":["claude-haiku-4-5","MiniMax-M2.7-highspeed","claude-opus-5[1M]",...]},
 {"engine":"codebuddy","ok":true,"bin":"/Applications/WorkBuddy.app/.../cli/bin/codebuddy","models":["auto","hy4-preview","hy3",...]},
 {"engine":"trae","ok":true,"bin":"/Users/you/.local/bin/trae-cli","models":["Doubao-Seed-Evolving","GLM-5.3",...]},
 {"engine":"llm","ok":true,"bin":"/Users/you/.llm-venv/bin/llm","models":["gpt-4o","gpt-4o-mini",...]},
 {"engine":"arkclaw","ok":true,"bin":"https://<host>/a2a/jsonrpc","models_note":"no dynamic model source: arkclaw binds the model by claw_id at the gateway (change claw_id to switch)"}]

$ magic-agent --engines --no-models     # 只列引擎与可用性，不启动任何 CLI（快）
```

`--engines` 固定输出 JSON 数组（jq / 程序友好），每行的字段：

| 字段 | 含义 |
|------|------|
| `engine` | 引擎名 |
| `ok` | 该引擎是否可用（CLI 存在 / 网关配置齐备） |
| `bin` | 可用时给二进制路径；无本机 CLI 的引擎（arkclaw）给端点 URL |
| `note` | 不可用原因（`ok:false` 时） |
| `models` | **该引擎当前支持的模型**（动态探测，见下） |
| `models_note` | 拿不到 `models` 时的原因（无动态来源 / 探测失败） |
| `workspace` | 「指定工作目录」的落地方式：`flag:-C`（codex）/ `cwd`（claude、codebuddy、trae）/ `none`（llm、arkclaw、openclaw） |
| `streaming` | 是否支持 `--stream`（`false`：openclaw、arkclaw） |
| `attachments` | 「收附件」的落地方式：`flag:-i`（codex）/ `stdin:stream-json`（claude、codebuddy）/ `flag:-a`（llm）/ `part:file`（arkclaw）/ `prompt`（trae、openclaw，只能把路径写进提示词） |
| `append` | 是否支持常驻会话 + 追加消息（`--keep-alive` / `--append`）：**只有 claude、codebuddy 为 `true`** |
| `ask` | 「需要用户选择」的落地方式：`tool:AskUserQuestion`（claude、codebuddy）/ `none`（其余）。见「需要用户选择」 |
| `permission` | 四档权限模型（`manual` / `accept-edits` / `auto` / `full`）的落地方式：`flag:--permission-mode`（claude、codebuddy）/ `none`（其余引擎传 `--permission` 会 exit 2 报错）。见「四档权限模型」 |

`--json` 为兼容旧调用保留（行为相同）。

### 模型清单（不硬编码）

`models` 一律从各引擎自己的权威入口现取现算 —— 引擎升级、用户新注册模型后自动跟随，代理侧不维护任何模型名常量：

| 引擎 | 动态来源 |
|------|---------|
| claude | `~/.claude/settings.json`（顶层 `model` + env 里 `ANTHROPIC_*MODEL[*_NAME]`）；claude 无 `models` 子命令 |
| codebuddy | `codebuddy --help` 里 `--model` 描述自带的 `Currently supported: (...)` 清单（含用户自定义 `custom-local:*`） |
| trae | `trae-cli models --json`（取 `name`，就是 `-c model.name=<name>` 的取值） |
| llm | `llm models`（llm CLI 自己注册的模型） |
| codex | `codex debug models`（raw model catalog 的 `slug`） |
| openclaw | `openclaw models list --json`（取 `key`，形如 `minimax/MiniMax-M3`） |
| arkclaw | 无清单 —— 模型由网关按 `claw_id` 绑定，改用 `models_note` 说明 |

探测只跑各 CLI 的**只读子命令**（不发起推理、不消耗额度），但确实会启动进程（最慢 `openclaw models list` ~2.5s），
因此并发执行、单引擎上限 30s；任一引擎失败只写进该行 `models_note`，不影响其他行。`--no-models` 可整段跳过。

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

# 四档权限（仅 claude / codebuddy；默认 full = 保持既有行为）
magic-agent -e claude --tools on --permission auto -p "重构这个函数"
magic-agent -e claude --tools on --permission manual -p "先看看再动手"

# 超时 + 重试（单次尝试 3 分钟，最多额外重试 2 次，指数退避）
magic-agent -e claude -t 3m -r 2 --verbose -o text "复杂的分析任务"

# 系统提示词
magic-agent -e claude -s "你是严谨的翻译官，只输出译文" -o text "Hello, world"

# 管道输入
cat doc.md | magic-agent -e claude -o text -f - "总结上文"
magic-agent -e claude -f context.md "基于这个文件回答：……"

# 提示词直接给文件路径（-p / 位置参数 / -s 都支持，见「提示词可以是文件路径」）
magic-agent -e claude -p ./prompt.md "补充一句"
magic-agent -e claude -p @notes/task.md

# 附件（文件＋提示词）：截图 / 图片随提示词一起发，见「附件」
magic-agent -p "这张图什么颜色" -a shot.png
magic-agent -p "对比两张图" -a a.png -a b.png

# 固定 JSON 输出（单行 envelope，适合 jq / 程序解析）
magic-agent -e claude "1+1=?"
# {"engine":"claude","model":"","session_id":"...","attempts":1,"latency_ms":534,"text":"2"}

# 流式输出（正文/思考实时增量；--no-thinking 关思考）
magic-agent --stream -e claude -o text "复杂问题"          # 正文→stdout，思考→stderr
cat doc.md | magic-agent --stream -e claude - "总结"        # json NDJSON 事件流 + result 收尾行
magic-agent --stream --no-thinking -e claude "问题"        # 只要正文增量

# 引擎可用性
magic-agent --engines            # JSON 数组：engine 名字 + ok 是否可用
magic-agent --engines --json     # 等价（--json 为兼容保留）

# 停止指定会话（见「停止指定会话」）
magic-agent --sessions           # 会话登记表（run_id / session_id / pid / engine / state）
magic-agent --stop <session_id>  # 停掉这条会话的引擎进程组
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
| `-e, --engine` | `codebuddy` | 引擎：`claude` \| `codebuddy` \| `trae` \| `llm` \| `codex` \| `openclaw` \| `arkclaw` |
| `-m, --model` | 空 | 模型（空 = 引擎默认；codebuddy 默认 `hy3`；llm 用其自身默认模型）。trae 无 `--model`，内部转 `-c model.name=<m>` |
| `-s, --system` | 空 | 系统提示词（claude/codebuddy 走 `--append-system-prompt`，trae 拼进 prompt）。空 = 用配置文件里的默认 `systemPrompt`，见「默认系统提示词」。值也可以是文件路径或 `@文件`，见「提示词可以是文件路径」 |
| `-p, --prompt` | 空 | 提示词（值可以是文件路径或 `@文件` → 按文件内容用，见下） |
| `-f, --file` | 空 | 从文件读 prompt（`-` = stdin）；与位置参数可组合，文件在前 |
| `-a, --attach` | 空 | 附件（截图/图片等），可重复或用逗号分隔；与提示词**并列**发送，见「附件」 |
| `--tools` | `off` | `off`（纯 chat）\| `on`（agent 模式）\| 逗号分隔白名单（如 `WebSearch,WebFetch`） |
| `-t, --timeout` | `600s` | 单次尝试超时（如 `90s` / `3m`） |
| `-r, --retries` | `0` | 失败重试次数（总尝试 = 1 + retries） |
| `--backoff` | `2s` | 首次重试退避（指数翻倍，上限 30s，带抖动） |
| `-o, --output` | `json` | 输出格式：`json` \| `text` |
| `--stream` | 关 | 流式输出：增量实时打到 stdout（text 模式思考走 stderr） |
| `--no-thinking` | 关 | 流式模式下不转发思考过程增量 |
| `--engines` | 关 | 列出引擎、可用性（JSON 数组：`engine` + `ok`）与各引擎当前支持的模型（`models`，动态探测不硬编码；拿不到时给 `models_note`）；替代原 `engines` 子命令 |
| `--no-models` | 关 | 配合 `--engines`：跳过模型探测（只列引擎与可用性，不启动任何 CLI） |
| `--json` | 关 | 兼容保留（`--engines` 已默认 JSON，本 flag 行为相同） |
| `--stop` | 空 | 停止指定会话/运行：传 `session_id` 或 `run_id`，杀掉它的引擎进程组，见「停止指定会话」 |
| `--sessions` | 关 | 列出会话登记表（JSON 数组：`run_id` / `session_id` / `pid` / `engine` / `state` / 起止时间） |
| `--keep-alive` | **开** | 常驻会话（默认开，仅 `claude`/`codebuddy` 的 `--stream` 调用生效）：等 `--append` 追加；`--keep-alive=false` 关闭，见「常驻会话」 |
| `--append` | 空 | 向常驻会话追加一条消息：传 `session_id` 或 `run_id`，内容用 `-p`/位置参数给 |
| `--idle` | `5m` | 常驻会话空闲收工时长（`0` = 本轮结束就收工） |
| `-v, --verbose` | `false` | 重试过程打印到 stderr |

prompt 输入优先级：`-p/--prompt` > 位置参数 > `--file` > stdin 管道（stdin 非 TTY 且无其他输入时自动读）。

## 常驻会话与追加需求（`--keep-alive` 默认开 / `--append`）

长任务跑着的时候还想补一句需求？**默认就支持** —— `claude` / `codebuddy` 的 `--stream` 调用会自动成为常驻会话：

```bash
# ① 起会话（默认即常驻；空闲 5m 自动收工）
magic-agent -e claude --stream -p "先把仓库跑一遍测试" -o text

# ② 任务跑着时（或结束后 5m 内）在另一个进程追加需求
magic-agent --append <run_id> -p "追加：顺便把 lint 也跑了"
magic-agent --sessions                                  # 看哪些会话可追加（append 字段非空）

# ③ 收工：空闲 --idle 自动退出，或随时 --stop
magic-agent --stop <run_id>

# 想要「一轮结束就退出」的老行为：
magic-agent -e claude --stream --keep-alive=false -p "..."
magic-agent -e claude --stream --idle 0 -p "..."        # 追加窗口只在任务运行期间
```

| 维度 | 行为 |
|---|---|
| 默认 | `--keep-alive` **默认开**（用户 2026-09-18 定稿：「keep-alive要是默认的」） |
| 支持范围 | **只有 `claude` / `codebuddy` 的 `--stream` 调用**（`--engines` 的 `append` 字段给出机器可读答案）。它们的 `--input-format stream-json` 允许**同一进程**持续收 user 消息；官方文档的说法是「allows providing guidance to the model while it [is working]」（Claude Code Headless 文档） |
| 不生效的形态 | 非 `--stream` 调用、不支持追加的引擎 → **自动忽略**（原有行为不变）；显式传 `--keep-alive` 却不满足条件 → `exit 2` 明确报错 |
| 通道 | 常驻会话开一个 **unix domain socket**（`~/.magic-agent/sessions/<run_id>.sock`，文件 0600 / 目录 0700 → 仅同用户可追加，不需要 token），路径写在会话记录的 `append` 字段里 |
| 追加语义 | 消息被写进引擎 stdin，成为**下一轮 user 消息**：本轮跑完后接着处理，**不打断**当前轮（实测：第 1 轮正在数数时追加，第 2 轮 result 正确复述了第 1 轮埋的暗号） |
| 输出 | 追加轮次照常走事件流；每轮收尾多发一个 `{"type":"turn_end","text":"<该轮正文>"}` 事件 |
| 超时 | 常驻生效期间**不套** `-t` 的默认 600s（长任务 + 追加不该被砍）；显式给 `-t` 才生效 |
| 收工 | 最后一轮结束后 `--idle`（默认 5m）内无人追加 → 关闭 stdin → 引擎优雅收尾 → 输出正常 result 信封；`--idle 0` = 本轮结束就收工；也可 `--stop` 立刻杀掉 |
| 提示 | 常驻开启的提示只打在 `-o text`（人看）或显式传 `--keep-alive` 时；`-o json` 机器调用方的 stderr 保持干净 |
| 错误路径 | `--append` 到不存在的 id → `exit 1`；到「没启用常驻」或已结束的会话 → `exit 1` 并给出出路（前者提示用 `--stream`（默认即常驻）启动，后者提示 `--session <id>` 续接）；空内容 / 负 `--idle` → `exit 2` |
| Windows | 没有 unix domain socket → 常驻不生效并明确报错（不静默降级） |
| 与 `--session` 的分工 | `--append` = **同一进程内**接着干（会话还活着）；`--session <id>` = 上一轮**结束后**用新进程续接（`--resume`） |

> ⚠️ **默认常驻带来的行为变化**：进程会在最后一轮结束后继续存活 `--idle`（默认 5 分钟）等追加。
> 调用方若要「一轮完成」的信号，请读事件流里的 **`turn_end`** 事件（而不是等进程退出），
> 或启动时传 `--keep-alive=false`。观物台的 `desk:ask` 目前按「进程退出 = 一轮完成」判断，需按此调整。

> 其它引擎的原生追加通道（本次未接）：`codex queue --thread <id> --message <text>`、
> `openclaw agent --session-id <id> --message <text>`、A2A 用同一 `taskId` 再发 `message/send`
>（A2A 文档：Clients optionally attach the taskId to a subsequent message to indicate that it
> continues that specific task）。
> `llm` 没有中途追加，只能 `--cid` 续接。

## 停止指定会话

```bash
magic-agent --sessions                 # 列出会话登记表（新→旧）
magic-agent --stop <session_id>        # 按会话 id 停（续接场景最常用）
magic-agent --stop <run_id>            # 按运行 id 停（新会话还没拿到 session_id 时）
magic-agent --stop <id> -o text        # 人看的一行结果
```

**为什么需要一张登记表**：引擎 CLI 在**独立进程组**里跑（`Setpgid`），所以调用方对 magic-agent 发信号 /
`kill(-magic-agent-pid)` 都**带不走它** —— 界面显示「已停止」，引擎还在后台跑。更麻烦的是进程句柄只活在
调用方内存里，应用重启后之前 detach 出去的会话就再也停不掉了。于是每次调用都落一条记录：

| 项 | 说明 |
|---|---|
| 位置 | `~/.magic-agent/sessions/<run_id>.json`（`MAGIC_AGENT_SESSIONS` 可改目录；一条一个文件，无锁） |
| 内容 | `run_id` / `session_id`（新会话在收尾时回填）/ **引擎子进程 pid** / `kill_group` / engine / model / workspace / 提示词开头 / state / 起止时间 |
| pid 怎么来的 | 子进程 `Start()` 后由 spawn 钩子回写（`agent.WithSpawnHook`）；HTTP 直连这类没有子进程的引擎先填自身 pid 兜底 |
| 保留 | 24 小时（按 `updated_at`），过期在每次读写时顺手清理 —— 留着是为了让「停一条早就结束的会话」能答「已结束」而不是「没找到」 |

`state` 取值：`running`（在跑）/ `done` / `failed` / `stopped`（被 `--stop` 停掉）/ `gone`（记录是 running 但进程已不在）。

停止行为与退出码：

| 情况 | 行为 | 退出码 |
|---|---|---|
| 在跑 | 先 `SIGTERM`，2s 内不退再 `SIGKILL`（进程组一起，含 CLI 内部的 node worker），回写 `state=stopped` | 0 |
| 进程已不在 | `stopped=false` + `reason`，状态改 `gone`（幂等，重复点「停止」不算错） | 0 |
| 会话已结束 | `stopped=false` + `reason=已结束（state=…）` | 0 |
| id 不存在 | 报错（多半写错了，`--sessions` 可列全部） | 1 |
| `--stop` 后没给 id | 参数错 | 2 |

```json
{"type":"stop","id":"s-1","stopped":true,"run_id":"run-…","session_id":"s-1","pid":4321,"engine":"claude","state":"stopped"}
```

> **另一条保障**：magic-agent 自己收到 `SIGINT` / `SIGTERM` 时，会先杀掉自己的引擎子进程再退出
> （退出码 130）—— 否则调用方 kill 掉 wrapper 只会留下一个还在跑的引擎孤儿。

## 附件（文件＋提示词）

**提示词照旧，附件是并列的第二份输入** —— 「截图 + 提示词」是主要用法：

```bash
magic-agent -p "这张图什么颜色" -a ~/Desktop/shot.png
magic-agent -p "对比这两张图的差异" -a a.png -a b.png      # 可重复
magic-agent -p "看截图里的报错" --tools on -a shot.png -o text
magic-agent -p "看图" -a a.png,b.png                      # 逗号分隔也行
```

每个引擎尽量走**自己的原生附件通道**（`--engines` 的 `attachments` 字段给出机器可读答案）：

| 引擎 | 落地方式 | 说明 |
|---|---|---|
| `codex` | `flag:-i` | 原生 `-i/--image`（**只收图片**）。`exec resume` 不接受 `-i`，续接轮退化为提示词里的路径 |
| `claude` | `stdin:stream-json` | 原生 `--input-format stream-json`：图片作为 message 的 image content block 经 **stdin** 传入；该模式要求 `--output-format` 也是 stream-json，故非流式（Complete）在有图时也跑流式协议再归约（不对调用方发事件） |
| `codebuddy` | `stdin:stream-json` | 同上（同族 CLI） |
| `llm` | `flag:-a` | **直连配置模型**：OpenAI 兼容 content parts（`image_url` + `data:` URL）；**委托 llm CLI**：原生 `-a/--attachment` |
| `arkclaw` | `part:file` | A2A 原生 `file` part（base64 inline）—— 远端 agent 看不到本机路径，**必须**inline |
| `trae` / `openclaw` | `prompt` | 无原生通道 → 把**绝对路径**写进提示词末尾的「【附件】」清单，靠引擎自己的读文件工具 |

规则与边界：

| 维度 | 行为 |
|---|---|
| 校验 | 路径必须存在、是**普通文件**、≤32MB（base64 后还要胖 1/3），否则 `exit 2`；重复路径自动去重 |
| 类型识别 | 按**魔数**嗅探（PNG/JPEG/GIF/WebP/BMP/PDF），不信扩展名；认不出按扩展名兜底，最后 `application/octet-stream` |
| 非图片附件 | codex 的 `-i` 只收图片、llm 的 `-a` 也有类型限制 → 无原生通道时统一退化为「路径写进提示词」 |
| 无原生通道的引擎 | **明确提示**不静默：`magic-agent: 警告：trae 引擎没有附件输入通道，N 个附件已改为「把路径写进提示词」…`；`--tools off` 时会追加「引擎读不到这些文件，请改用 `--tools on`」 |
| 提示词不被改写 | 有原生通道时提示词原样（图片不进正文）；只有退化路径才在正文追加「【附件】」清单 |
| 大小上限的实际约束 | 图片走 base64 进请求体/命令行，几十 MB 的图基本会被引擎或网络先拒；建议截图直接用（几百 KB ~ 几 MB） |

> **能力上限取决于模型**：链路只保证把附件按协议送到位。实测（2026-09-17）同一张纯红 PNG：
> `-e llm` 直连 MiniMax-M3 → 答「红色」✓；而本机 `claude` CLI（后端也接 MiniMax-M3 网关）
> 收到 stream-json 的 image block 后答「灰色」✗ —— 该网关把图片块吞掉了。
> 换用真 Anthropic 端点或 codex/llm 时应正常。

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

### 两种 wire 形状（实测）

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

### 三条通道（`ask.go` 的 API）

| 场景 | 用法 |
|---|---|
| 宿主实现了 `canUseTool`（拿到的是 `control_request`） | `EncodeAskAnswer(engine, req, ans)` → SDK 层 `{"behavior":…}`；要 wire 信封再用 `EncodeAskControlResponse` |
| **实际可用的作答通道**：把答案补进正在跑的会话 | `EncodeAskFollowUp(req, ans)` → 一段纯文本，走 `--append` 通道追加一条 user 消息（对全部引擎可用） |
| 只想识别 / 上报 | 读 `--stream` 的 `ask` 事件（`StreamEvent.Ask`），或直接调 `ParseAskLine(engine, line)` |

#### 怎么接住用户的回答（2026-09-18 两条实测）

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

> 其它引擎（trae / llm / codex / openclaw / arkclaw）实测均无 `AskUserQuestion`，也无 `can_use_tool`。
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

## 提示词可以是文件路径

长提示词写在文件里，不必每次 `-f` 或 `cat |`：`-p` / 位置参数 / `-s` 的值**命中一个已存在的普通文件**时，直接按文件内容用。

```bash
magic-agent -p ./prompt.md                    # 按文件内容提问
magic-agent -p ./prompt.md "补充一句"          # 文件内容在前、追加文本在后（与 -f 语义一致）
magic-agent ./tasks/task.md                   # 位置参数同样支持
magic-agent -p @notes/task.md                 # @ 强制按文件读（读不到 → exit 2 报错）
magic-agent -s ./reviewer.md "审一下这段"      # 系统提示词也能给文件
```

| 写法 | 行为 |
|---|---|
| `-p <存在的文件>` / 位置参数 / `-s <存在的文件>` | 读文件内容当提示词，并在 **stderr** 打一行「命中文件」提示（不静默）；内容和路径都不是文件时原样当文本 |
| `@<path>`（`-p` / 位置参数 / `-s` / 配置 `systemPrompt` 均可） | **强制**按文件读：不存在 / 不是普通文件 / 读不了 → `exit 2` 报错，绝不退化成把路径当提示词 |
| `-f, --file <path>` | 与以前一致：显式从文件读（`-` = stdin），不做任何猜测 |

自动识别的判据刻意保守，避免把正常提示词误判成路径：必须是**单行**、长度 ≤ 4096 字节、`stat` 出来是**普通文件**（目录不算）。所以 `magic-agent -p "解释一下 README.md"`、多行提示词、粘贴进来的长文都不受影响。`~` 前缀会展开。

配置里的默认 `systemPrompt` 同样支持这两种写法：

```json
{ "systemPrompt": "@~/.magic-agent/prompts/cn.md" }
```

> 配置里的 `@路径` 读不到时**不注入**（`-v` 打印原因），不阻断本次调用 —— 与「配置文件坏掉不阻断调用」同一取舍。

## 默认系统提示词（配置文件 `systemPrompt`）

不想每次调用都敲 `-s`，就把默认系统提示词写进配置文件 —— **对所有引擎生效**：

```bash
mkdir -p ~/.config/magic-agent
cat > ~/.config/magic-agent/config.json <<'EOF'
{
  "systemPrompt": "你是一个中文助手，始终用中文回答所有问题。",
  "arkclaw": { "url": "…", "key": "…", "claw_id": "…" }
}
EOF
```

| 维度 | 行为 |
|---|---|
| 生效条件 | 调用时**未给** `-s/--system`（或只给了空白）→ 注入配置里的 `systemPrompt`；给了 `-s` 则 `-s` 优先，**不叠加** |
| 作用范围 | 全部引擎（claude / codebuddy / trae / llm / codex / openclaw / arkclaw，含 llm 直连配置模型） |
| 键名兼容 | `systemPrompt` \| `system_prompt` \| `system` 任选其一 |
| 未配置 | 不注入，行为与以前完全一致（文件不存在 / 键为空 / 纯空白都算未配置） |
| 临时覆盖 | 环境变量 `MAGIC_AGENT_SYSTEM_PROMPT` 优先于文件值 |
| 配置文件路径 | 默认 `~/.config/magic-agent/config.json`（`MAGIC_AGENT_CONFIG` 覆盖路径，支持 `~`；`XDG_CONFIG_HOME` 优先于 `~/.config`） |
| 配置写坏时 | **不阻断调用**（本次不注入），`-v` 会在 stderr 打印读取失败原因，不静默 |

```bash
magic-agent "1+1=?"                       # 用配置里的默认 systemPrompt
magic-agent -s "只输出译文" "Hello, world"  # -s 优先，默认值本轮不生效
```

## arkclaw 引擎（A2A JSON-RPC 网关）

`-e arkclaw` 不走本机 CLI，而是一个 [A2A](https://a2a-protocol.org/)（Agent-to-Agent）
网关：magic-agent 把 prompt 组成一个 JSON-RPC `message/send` 请求 POST 过去，
网关侧跑它自己的 agent，再把结果包成 A2A Task 返回来。**只接 `message/send`，
不实现流式**（A2A 的流式是独立的 `message/stream` SSE，与其余引擎的
Streamer 契约不是一回事）。

### 配置（url / key / claw_id 放配置文件）

凭据不写死在代码里、也不必每次敲 flag，统一放本地配置文件：

```bash
mkdir -p ~/.config/magic-agent
cat > ~/.config/magic-agent/config.json <<'EOF'
{
  "arkclaw": {
    "url": "https://<host>/a2a/jsonrpc",
    "key": "<apikey>",
    "claw_id": "ci-xxxxxxxxxxxxxxxxxxxx"
  }
}
EOF
```

仓库内 `config.example.json` 是同一份骨架（含 `systemPrompt` 与 `arkclaw` 两节）。路径与取值优先级：

| 优先级 | 来源 | 说明 |
|---|---|---|
| 1 | 环境变量 `MAGIC_AGENT_ARKCLAW_URL` / `_KEY` / `_CLAW_ID` | 覆盖文件值，便于临时切换端点 |
| 2 | 配置文件 `arkclaw` 节 | 默认 `~/.config/magic-agent/config.json`；路径可用 `MAGIC_AGENT_CONFIG` 覆盖（支持 `~` 展开）；`XDG_CONFIG_HOME` 优先于 `~/.config` |

键名做了宽松兼容：`url`/`endpoint`、`key`/`apikey`/`api_key`、`claw_id`/`clawId`/`clawID` 任选其一。文件不存在或为空**不算错误**（等同未配置，纯环境变量用法同样可用）；JSON 语法错会明确报出路径。

### 用法

```bash
magic-agent --engines                                   # 看 arkclaw 是否已配置齐备
magic-agent -e arkclaw "你好"                            # 单轮 message/send
magic-agent -e arkclaw --session <contextId> "接着说"     # 按 contextId 续接同一上下文
magic-agent -e arkclaw --json-schema '{"type":"object",...}' "输出 JSON"   # 结构化输出
```

### 行为细节

| 维度 | 行为 |
|---|---|
| 鉴权 | `apikey` / `clawId` 作为 URL query 参数发送；失败时网关回 `HTTP 401` + `text/plain` 正文（**非 JSON**），错误信息里带上状态码与正文 |
| 续接 | `--session <contextId>` → 写入**消息对象内部**的 `contextId`；返回时用 `result.contextId` 回填输出的 `session_id`。实测放到外层 `params.contextId` 会被网关忽略并另开上下文 |
| `-c, --continue` | **不支持**：A2A 没有「查询最近上下文」的接口，显式报错并提示改用 `--session <contextId>`，而不是静默新开会话 |
| `--stream` | **不支持**：非流式引擎，CLI 会明确报 `engine "arkclaw" does not support streaming` |
| `-m` / `--max-tokens` / `--temperature` / `--tools` | 静默忽略：模型与工具循环由网关侧的 claw 决定 |
| `-s, --system` | 展平进 message 正文头部（协议无独立 system 角色） |
| `--json-schema` | 支持，走与 `llm` 相同的输出后处理抽 JSON 路径 |
| 默认超时 | 3 分钟（实测单轮网关往返 8s ~ 23s，留足余量） |
| 取文 | `result.status.message.parts[].text` → 兜底 `result.artifacts[].parts[].text` |

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
| 工具禁用 | | `--tools ""` | `--tools ""` | `--disallowed-tool`（Bash/Edit/Write/Glob/Grep/Read 逐个） | 不适用（纯 chat） |
| 工具启用 | | 不传 `--tools` + `-y` | `-y` | `-y`（全放行；白名单同此，见下） | 不适用（纯 chat） |
| 超时联动 | | 进程组 kill | 进程组 kill | 另透传 `--query-timeout`（上限 600s） | 进程组 kill |

所有引擎都以独立**进程组**运行：超时/取消时 `kill(-pgid)` 杀掉整个进程树，CLI 内部 spawn 的 node worker 不会残留（有回归测试保障）。

> `codex` / `openclaw` / `arkclaw` 未列入上表：codex 与 openclaw 走各自 CLI 的原生参数（含 codex 的 `CODEX_HOME` 隔离，见下文），arkclaw 则完全不启进程 —— 它是 HTTP 引擎，没有二进制、没有流式、也没有进程组可杀，超时由 `context` + `http.Client` 控制。

## 工具启用（--tools）

`--tools` 同时管两件事：CLI 侧的工具白名单，以及是否在 system prompt 里
注入 `noToolSuffix`。

| `--tools` | CLI 参数（codebuddy） | noToolSuffix | 效果 |
|---|---|---|---|
| `off`（默认） | `--tools ""` | 注入 | 纯 chat，模型不调工具；后缀额外压制「伪工具调用」与「伪造工具返回」 |
| `on` | `--permission-mode <档位>` | 不注入 | 全工具可用（含 `WebSearch` / `WebFetch`）；档位由 `--permission` 决定 |
| `WebSearch,WebFetch` | `--tools WebSearch,WebFetch --permission-mode <档位>` | 不注入 | 仅白名单工具可用 |

> 工具的**可用范围**与**权限档位**是两件事：`--tools` 决定「有哪些工具」，
> `--permission` 决定「执行时问不问人、有没有沙箱」。详见下一节。
> claude 同理（`--tools ""` / `--permission-mode <档位>`）。

> trae 的白名单降级：trae-cli 的 `--allowed-tool` 只做「自动批准该工具」、
> **不裁剪工具集**（实测 `--allowed-tool WebFetch` 仍下发全部 18 个工具），
> 想收窄只能对补集逐个 `--disallowed-tool`（实测 18 → 4）。补集随 CLI 版本
> 漂移，漏一条白名单就失效，故 trae 的白名单模式定义为**默认开启所有工具**
> （全放行 `-y`，与 `on` 同一条路径），不再输出没有收窄能力的 `--allowed-tool`。
> 需要在 trae 上真正关工具请用 `--tools off`（`--disallowed-tool` 真减法）。
> 另注：trae 没有 `WebSearch` 工具，联网只能靠 `WebFetch`。

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

## 四档权限模型（`--permission`，仅 claude / codebuddy）

`--tools` 只管「有哪些工具」，「执行时问不问人、有没有沙箱」由 `--permission` 决定。
四档是把主流 agent CLI 的权限机制收敛成的一条轴 —— **哪些动作自动放行 + 放行不了时由谁裁决**：

| 档位 | 语义 | 审批人 | 沙箱 | claude 参数 |
|---|---|---|---|---|
| `manual` | 只读放行，其余逐项确认 | 用户 | 开 | `--permission-mode default` |
| `accept-edits` | 工作区内编辑放行，命令仍逐条确认 | 用户（仅命令） | 开 | `--permission-mode acceptEdits` |
| `auto` | 沙箱内放行，越界交 LLM Guardian 判定 | LLM | 开 | `--permission-mode auto` |
| `full` | 命令直接在宿主机执行，不触发审批 | 无 | 关 | `--permission-mode bypassPermissions` |

别名：`manual` ← `default`/`ask`/`1`；`accept-edits` ← `edits`/`2`；`auto` ← `guardian`/`3`；
`full` ← `bypass`/`yolo`/`4`（大小写与连字符不敏感）。

```bash
magic-agent -e claude --tools on --permission auto -p "重构这个函数"
magic-agent -e claude --tools on --permission manual -p "先看看再动手"
```

### 默认档是 `full`（刻意）

改造前 claude / codebuddy 在 `--tools` 非 `off` 时**恒传** `--dangerously-skip-permissions` / `-y`，
语义正是第 4 档。所以默认值保持 `full` —— 改成别的档位等于一次**静默的行为变更**
（既有调用方的 agent 会突然开始弹审批 / 被沙箱拦）。做 agent 任务时推荐显式传 `--permission auto`。

`--tools off` 下不调用任何工具，档位无处生效；显式传非默认档会打一行 stderr 提示（不静默）。

### 参数是怎么落地的（两个关键约束）

1. **档位走 `--permission-mode`，不走 settings 的 `permissions.defaultMode`。**
   官方明确 `auto` 与 `bypassPermissions` 写在项目级 `.claude/settings.json` 或
   本地级 `.claude/settings.local.json` 里**不生效**（会被忽略，会话回落到 Manual / 内建默认）。
   `--permission-mode` 是唯一在任意作用域都可靠的入口。

2. **沙箱只能经 `--settings` 注入。** claude 没有 `--sandbox` 命令行参数
   （官方 CLI reference 的 flags 表全文无此参数）。而 `--settings` 的语义是
   「**一个**文件路径或一段内联 JSON」，不是可重复 flag —— 所以 MaxTokens 的 env 注入、
   沙箱配置、autoMode、权限规则必须**合并进同一份 JSON**（`agentSettingsPayload`）。
   实测第 3 档 + 全部可选项时子进程收到的是：

   ```json
   {
     "autoMode": { "environment": ["$defaults", "Source control: github.example.com/acme-corp"] },
     "env": { "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "16000" },
     "permissions": { "ask": ["Bash(git push *)"], "deny": ["Bash(rm -rf *)"] },
     "sandbox": {
       "allowUnsandboxedCommands": false, "autoAllowBashIfSandboxed": true,
       "enabled": true, "excludedCommands": ["docker"], "failIfUnavailable": true
     }
   }
   ```

### 可选项

| flag | 作用 | 生效档位 |
|---|---|---|
| `--sandbox-exclude docker,watchman` | 始终在沙箱外执行的命令（`sandbox.excludedCommands`） | manual / accept-edits / auto |
| `--sandbox-domain api.example.com` | 沙箱网络白名单（`sandbox.network.allowedDomains`） | manual / accept-edits / auto |
| `--auto-mode-env "Source control: github.example.com/acme-corp"` | 第 3 档分类器的**受信边界**（自然语言，不是正则；自动带 `$defaults` 保留内建规则） | auto |
| `--permission-ask 'Bash(git push *)'` | 强制人工审批的规则。命中即弹框，**第 3 档下分类器也无法自动放行** | 全部 |
| `--permission-deny 'Bash(rm -rf *)'` | deny 规则。**在所有档位（含 `full`）都先于 allow/ask 求值且不可被白名单覆盖** | 全部 |

第 1~3 档默认写 `failIfUnavailable: true`（沙箱起不来就**报错退出**，不静默降级成不沙箱运行）
与 `allowUnsandboxedCommands: false`（关闭 `dangerouslyDisableSandbox` 逃逸舱口）。
需要放行的命令请用 `--sandbox-exclude` 显式列出，而不是整体放宽。

> `--sandbox-exclude` 这类「沙箱外执行」的口子比想象中重要：`docker`、`watchman`
> 与沙箱不兼容，不排除会让相关命令直接失败。

### 未接线的引擎会明确报错

`--permission` 只对 claude / codebuddy 生效（`--engines` 的 `permission` 字段为
`flag:--permission-mode`）。传给 trae / llm / codex / openclaw / arkclaw 时**exit 2 明确报错**，
不会静默忽略 —— 静默忽略一个安全设置是最坏的结果：用户以为自己被保护着，实际没有。

```bash
$ magic-agent -e trae --permission auto -p hi
magic-agent: --permission 暂不支持 trae 引擎（当前仅 claude、codebuddy；各引擎能力见 --engines 的 permission 字段）
$ echo $?   # 2
```

（只在实际传了 `--permission` 时才校验，所以 `-e codex` 这类既有调用不受影响。）

### 测试

- `internal/agent/permission_test.go`：档位解析、settings 载荷逐字段断言、
  各档 argv 映射，外加一个**假 CLI 落 argv** 的端到端（证明参数真的进了子进程）。
- `internal/cli/permission_test.go`：`resolvePermissionTier` 的两道校验、
  flag 默认值契约、`--engines` 能力字段、`prepareAsk` → `agent.Request` 的接线。

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

### web_search 覆写（MAGIC_AGENT_CODEX_WEBSEARCH）

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

## 架构

```
cmd/magic-agent/main.go     入口
internal/cli/               cobra 命令层（无子命令、参数校验、退出码、流式分流）
internal/agent/
  engine.go                 Engine 接口 + Request/Response + 注册表（含 llm）
  stream.go                 Streamer 接口 + StreamEvent + NDJSON 流解析
  streamjson.go             claude/codebuddy 共用：stream-json 输入（附件走 stdin）+ 事件归约
  attachments.go            附件公共件：类型、魔数嗅探、能力表、兜底路径清单
  ask.go                    「需要用户选择」统一格式：两种 wire 形状 → 一份归一化结构，
                            再渲染回各引擎原生答案（含 headless 自动拒绝后的兜底通道）
  prompt.go                 多轮消息扁平化 + noToolSuffix 约束
  claude.go                 Claude Code 引擎（非流式 + 流式）
  codebuddy.go              CodeBuddy 引擎（envelope 多形态解析 + 回显剥离 + 流式）
  trae.go                   Trae 引擎（模型覆盖 + query-timeout 映射 + 流式）
  tags.go                   思维链标签常量（分段拼接防 tokenizer 改写）+ 剥离
  llmengine.go              llm 引擎：包装 simonw/LLM CLI + thinkSplitter 流式标签路由
  codex.go                  Codex 引擎（CODEX_HOME 隔离 + web_search 覆写）
  codex_home.go             codex 配置镜像与同步
  openclaw.go               OpenClaw 引擎（本机 CLI + payloads envelope 解析）
  arkclaw.go                ArkClaw 引擎（A2A JSON-RPC message/send，HTTP，非流式）
  engine_base.go            引擎公共基座（参数矩阵、二进制探测、默认值）
  runner.go                 超时 + 重试编排（错误分类、指数退避、可取消）
  env.go                    子进程环境构造（剔除 SERVER__* 等父进程专属变量）
  runcmd.go                 进程组感知执行（平台无关调度）
  runcmd_unix.go            Setpgid + kill(-pgid)（darwin/linux）
  runcmd_windows.go         CREATE_NEW_PROCESS_GROUP + taskkill /T /F
  output.go                 固定 text/json 输出
internal/config/
  config.go                 本地配置文件读取（~/.config/magic-agent/config.json）+ 环境变量覆盖
internal/session/
  session.go                会话登记表（run_id/session_id/pid/state）+ 按会话停止（SIGTERM→SIGKILL）
  session_append.go         常驻会话的追加入口（unix socket）+ 客户端 AppendMessage
config.example.json         配置文件骨架（systemPrompt + arkclaw 节）
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

## 已验证（2026-09-16，arkclaw 真实网关）

- **端点探测**：`POST <url>?apikey=<key>&clawId=<clawId>`，`message/send` 单轮往返实测 **7.95s / 22.69s**（agent 侧带工具循环时更久）→ 默认超时定 3 分钟
- **响应形状**：`result.status.message.parts[].text` 取到正文；`result.contextId` 可回填续接
- **续接定位（关键结论）**：`contextId` 必须放 **`params.message.contextId`（消息对象内部）**。放外层 `params.contextId` 时返回全新 contextId，agent 答「当前会话里没有更早的消息」；放对位置后 contextId 保持不变，agent 正确回忆上轮暗号
- **鉴权失败形状**：`HTTP 401` + `text/plain` 正文 `External authentication failed.`（**非 JSON**）→ 解析先看状态码再看 body，避免含糊的 `invalid character` 报错
- 单元测试：`internal/agent/arkclaw_test.go`（29 个用例，httptest 全覆盖成功/401/续接落点/JSON-RPC error/任务失败态/artifacts 兜底/空正文/凭据缺失/Continue 拒绝/JSONSchema 后处理）+ `internal/config/config_test.go`（路径优先级、缺失与空文件、语法错、宽松键名、环境变量覆盖）

## 已验证（2026-09-16，`--engines` 模型清单探测）

本机实跑 `magic-agent --engines`（7 个引擎全部 `ok:true`），逐引擎核对「清单来源 = `-m` 能收的标识」：

| 引擎 | 探测命令 | 实测结果 |
|------|---------|---------|
| claude | 读 `~/.claude/settings.json` | 5 个（`claude-haiku-4-5` / `MiniMax-M2.7-highspeed` / `claude-opus-5[1M]` / `MiniMax-M3` / `claude-sonnet-5[1M]`） |
| codebuddy | `--help` 的 `Currently supported:` | 23 个（`auto`/`hy4-preview`/`hy3`/…/`custom-local:gpt-6-astra`） |
| trae | `trae-cli models --json` | 26 个（含用户自定义 `My-MiniMax-M3`，`name` 而非 `real_name` 才是 `-c model.name=` 的取值） |
| llm | `llm models` | 64 个（OpenAI/Responses/OpenRouter…） |
| codex | `codex debug models` | 1 个（`MiniMax-M3`，来自 `config.toml` 的 `model_catalog_json`） |
| openclaw | `openclaw models list --json` | 28 个（`minimax/MiniMax-M3`、`papergames/deepseek-v4-pro`…） |
| arkclaw | — | `models_note`：模型由网关按 `claw_id` 绑定 |

- 全量探测并发执行，整条命令 **4.2s**（最慢单点是 openclaw）；`--engines --no-models` 不启动任何 CLI，退化为原来的快速探测链。
- 单元测试：`internal/agent/models_test.go`（解析器 + 假 CLI 逐引擎断言命令构造与解析 + claude settings.json 的 HOME/`CLAUDE_CONFIG_DIR` + 缺失/失败路径）、`internal/cli/ask_test.go`（`--engines` 的 `models` / `models_note` 装配与 `--no-models` 零探测）；`go build` / `go vet` / `go test ./...` 全绿。

## 平台支持

三档平台覆盖 npm 包内自带的 5 个预编译产物（darwin-arm64 / darwin-x64 / linux-x64 / linux-arm64 / win32-x64），进程组隔离在各平台均生效：

| 平台 | 产物目录 | 进程组实现 |
|------|---------|-----------|
| macOS arm64 / x64 | `npm/dist/darwin-arm64` / `darwin-x64` | `Setpgid` + `kill(-pgid, SIGKILL)` |
| Linux x64 / arm64 | `npm/dist/linux-x64` / `linux-arm64` | 同上 |
| Windows x64 | `npm/dist/win32-x64` | `CREATE_NEW_PROCESS_GROUP` + `taskkill /T /F` |

## License

MIT
