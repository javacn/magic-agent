# magic-agent

统一的 agent CLI 代理 —— 把 **claude / codebuddy / trae / llm / codex / openclaw / dsh** 七家 CLI 与 **arkclaw**（A2A JSON-RPC 网关）、**codebuddy-gateway**（CodeBuddy Code HTTP 网关）的非交互调用收成一条命令，提供一致的引擎/模型切换、超时与重试、固定输出格式与稳定退出码。适合脚本化编排与上层工具集成。

## 能做什么

| 能力 | 说明 |
|------|------|
| **多引擎统一调用** | 一条命令串起 claude / codebuddy / trae / llm / codex / openclaw / dsh / arkclaw / codebuddy-gateway 这十种后端，参数语义统一 |
| **引擎与模型清单** | `--engines` 给出当前可用的引擎、模型列表与积分倍率；`install` 字段给出一键装/升级命令 |
| **流式 + 追加需求** | `--stream` 走各引擎原生协议；`--keep-alive` / `--append` 支持常驻会话里中途补需求 |
| **统一 wire** | 多模态附件、用户选择（`AskUserQuestion`）、四档权限模型都收成同一套协议，CLI 端与上层 UI 共用收口 |
| **客户端契约面** | `--contract` 给固定 schema（`contractVersion` + `engines[].capabilities`），桌面 / 移动端插件启动时按它做能力降级 |

## 安装

```bash
npm install -g magic-agent       # 全局安装（推荐，无需 Go）
npm install -g ./magic-agent-<version>.tgz   # 从本地 tgz 安装（离线 / 内网）
go build -o bin/magic-agent ./cmd/magic-agent   # 从源码构建（Go 1.26+）
```

平台产物缺失时 `postinstall` 会用本机 Go 现场编译；两者都没有只告警，不阻断安装。同一个 `postinstall` 还会按需安装 **llm CLI**（`-e llm` 的依赖，隔离在 `~/.llm-venv`）。

维护者发布流程（升版本号 / 交叉编译 / 装两份全局前缀）见 [docs/development.md](docs/development.md)。

## 引擎依赖

七个 CLI 按需安装，未安装的引擎自动探测失败但不影响其他引擎：

| 引擎 | CLI | 探测路径 / 环境变量 |
|------|-----|---------------------|
| claude | Claude Code | `MAGIC_AGENT_CLAUDE_BIN` → `/opt/homebrew/bin/claude` → PATH |
| codebuddy | WorkBuddy 内置 CLI | `MAGIC_AGENT_CODEBUDDY_BIN` → `WorkBuddy.app/.../cli/bin/codebuddy` → PATH |
| trae | trae-cli | `MAGIC_AGENT_TRAE_BIN` → `~/.local/bin/trae-cli` → PATH |
| llm | [simonw/LLM](https://github.com/simonw/LLM) | `MAGIC_AGENT_LLM_BIN` → `~/.llm-venv/bin/llm` → `/opt/homebrew/bin/llm` → PATH |
| codex | `@openai/codex` | `MAGIC_AGENT_CODEX_BIN` → `/opt/homebrew/bin/codex` → PATH |
| openclaw | OpenClaw | `MAGIC_AGENT_OPENCLAW_BIN` → `/opt/homebrew/bin/openclaw` → PATH |
| dsh | [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) | `MAGIC_AGENT_DSH_BIN` → `~/.local/bin/dsh` → PATH → npm 全局前缀 |
| arkclaw | 无（远端 A2A 网关） | 配置文件的 `arkclaw` 节，或 `MAGIC_AGENT_ARKCLAW_URL` / `_KEY` / `_CLAW_ID` |
| codebuddy-gateway | 无（已在跑的 CodeBuddy Code HTTP 网关） | 配置文件的 `codebuddyGateway` 节，或 `MAGIC_AGENT_CBGW_URL` / `_PASSWORD` |

探测链最后一跳是 **npm 全局前缀** `<NPM_CONFIG_PREFIX>/bin`，专治「CLI 装好了、探测却说 not found」（容器常把 `npm i -g` 的落点设成不在 PATH 里的私有前缀）。优先级不变：显式参数 > 环境变量 > 候选路径 > PATH > npm 前缀。

## 引擎与模型清单（`--engines`）

```bash
magic-agent --engines                # JSON 数组：引擎 + 可用性 + 模型 + 版本 + 一键安装命令
magic-agent --engines --no-models    # 跳过模型与版本探测（不启动任何 CLI，快）
```

每行的字段：

| 字段 | 含义 |
|------|------|
| `engine` | 引擎名 |
| `ok` | 该引擎是否可用（CLI 存在 / 网关配置齐备） |
| `bin` | 可用时给二进制路径；无本机 CLI 的引擎给端点 URL |
| `note` | 不可用原因（`ok:false` 时） |
| `install` | 一键安装 / 升级命令（shell 一行）。`ok:false` 时拿它安装，已装的引擎重跑就是升级到最新版。桌面端 GUI 应用与网关类引擎没有该字段 |
| `version` | 引擎当前版本号（尽力而为，拿不到就不给）。是「升级有没有生效」的依据，**不是**可用性判据 |
| `models` | 该引擎当前支持的模型（动态探测，来源见下） |
| `models_note` | 拿不到 `models` 时的原因 |
| `model_credits` | 积分倍率表（model → `"0.34"`）。`codebuddy` / `codebuddy-ai` 给出；客户端按它给模型加「限免 / 夜间免费」角标与结算 |
| `workspace` | 指定工作目录的落地方式（`flag:-C` / `cwd` / `none`） |
| `streaming` | 是否支持 `--stream` |
| `attachments` | 收附件的落地方式（原生图片 / 路径写进提示词） |
| `append` | 是否支持常驻会话 + 追加消息 |
| `ask` | 「需要用户选择」的落地方式 |
| `permission` | 四档权限模型的落地方式 |
| `capabilities` | 该引擎的**静态能力 id 集合**，回答「能做什么」；与 `ok` / `models` 那些回答「此刻如何」的字段严格分开。取值见 `internal/agent/capability.go` |

`--json` 为兼容旧调用保留（行为相同）。

### 一键安装 / 升级（`install` 字段）

| 引擎 | `install` |
|------|-----------|
| claude | `npm install -g @anthropic-ai/claude-code` |
| codex | `npm install -g @openai/codex` |
| openclaw | `npm install -g openclaw@latest` |
| dsh | `npm i -g @deepseek-ai/dsh` |
| trae | `sh -c "$(curl -L https://trae.cn/trae-cli/install.sh)"` |
| llm | `python3 -m venv ~/.llm-venv && ~/.llm-venv/bin/pip install llm` |
| codebuddy / codebuddy-ai / arkclaw / codebuddy-gateway / 具名 A2A agent | **无该字段**（桌面端 GUI 应用 / 网关，没有可执行的安装命令），怎么才能用见同行 `note` |

```bash
# 装齐本机缺的引擎
magic-agent --engines --no-models | jq -r '.[] | select(.ok|not) | "\(.engine)\t\(.install // .note)"'
# 升级：看版本号 → 跑命令 → 再看版本号（变了才算升上去）
magic-agent --engines --no-models | jq -r '.[] | select(.install) | "\(.engine)\t\(.version // "-")\t\(.install)"'
```

### 模型清单来源（不硬编码）

`models` 一律从各引擎自己的权威入口现取现算，代理侧不维护任何模型名常量：

| 引擎 | 动态来源 |
|------|---------|
| claude | `~/.claude/settings.json`（顶层 `model` + env 里 `ANTHROPIC_*MODEL[*_NAME]`） |
| codebuddy | 客户端模型选择器清单 ∩ 客户端合并配置缓存；回退 `codebuddy --help` 的 `Currently supported` |
| codebuddy-ai | 同上（独立配置目录 `~/.workbuddy-ai`） |
| trae | `trae-cli models --json` |
| llm | `llm models` |
| codex | `codex debug models` |
| openclaw | `openclaw models list --json` |
| dsh | `$DSH_HOME/settings.yaml` 里已配置的模型，输出 `route/model` |
| arkclaw / codebuddy-gateway | 无清单（模型由网关侧决定），改用 `models_note` 说明 |

探测只跑**只读子命令**（不发起推理、不消耗额度），但会真的启动进程，因此并发执行、单引擎上限 30s；任一引擎失败只写进该行 `models_note`。

### 客户端契约（`--contract`）

```bash
magic-agent --contract                              # {"contractVersion":1,"engines":[...]}
magic-agent --contract | jq -r '.contractVersion'   # 启动时先校验这个
```

| 字段 | 含义 |
|------|------|
| `contractVersion` | 契约面版本（当前 `1`）。客户端超出自己支持的范围就明确报错并提示升级，不要带着不匹配的能力继续跑 |
| `engines` | 与 `--engines` 的行完全同构，每行另含 `capabilities` |

版本规则：**只增字段不升版本**；字段改名 / 语义变化 / 删除时必须升 `contractVersion`。`--contract` 默认**不探测模型**（模型探测要起真 CLI，30 秒级，客户端启动路径等不起），要连模型一起拿就写 `--no-models=false`。

## 用法

无子命令设计：所有功能都走根命令 + flags。

```bash
# 基本提问（默认 json 输出；位置参数等价于 -p）
magic-agent -p "用一句话解释什么是熵"
magic-agent -o text -p "用一句话解释什么是熵"

# 切引擎、切模型
magic-agent -e codebuddy "写一首俳句"           # codebuddy 默认 hy3
magic-agent -e codebuddy -m glm-5.3 "写一首俳句"
magic-agent -e trae -m My-MiniMax-M3 "..."     # trae 无 --model，内部转 -c model.name=
magic-agent -e llm -m minimax-m3 "问题"        # llm 用自己注册的模型

# 工具与权限（--tools 默认 off = 纯 chat）
magic-agent -e claude --tools on -p "看看当前目录有什么"
magic-agent -e claude --tools Bash,Read -p "读一下 README"
magic-agent -e claude --tools on --permission auto -p "重构这个函数"

# 超时 + 重试 + 系统提示词
magic-agent -e claude -t 3m -r 2 --verbose -o text "复杂的分析任务"
magic-agent -e claude -s "你是严谨的翻译官，只输出译文" -o text "Hello, world"

# 管道输入 / 文件
cat doc.md | magic-agent -e claude -o text -f - "总结上文"
magic-agent -e claude -p ./prompt.md "补充一句"    # 提示词给文件路径

# 附件（截图 + 提示词）
magic-agent -p "这张图什么颜色" -a shot.png
magic-agent -p "对比两张图" -a a.png -a b.png

# 流式输出
magic-agent --stream -e claude -o text "复杂问题"
magic-agent --stream --no-thinking -e claude "问题"

# 会话
magic-agent --sessions              # 会话登记表
magic-agent --stop <session_id>     # 停掉这条会话的引擎进程组
magic-agent --append <run_id> -p "追加：顺便把 lint 也跑了"
```

## Flags

| Flag | 默认 | 说明 |
|------|------|------|
| `-e, --engine` | `codebuddy` | 引擎：`claude` \| `codebuddy` \| `codebuddy-ai` \| `codebuddy-gateway` \| `trae` \| `llm` \| `codex` \| `openclaw` \| `dsh` \| `arkclaw`，外加配置里 `agents` 声明的具名 agent |
| `-m, --model` | 空 | 模型（空 = 引擎默认；codebuddy 默认 `hy3`） |
| `-s, --system` | 空 | 系统提示词；空 = 用配置里的 `systemPrompt`。值可以是文件路径或 `@文件` |
| `-p, --prompt` | 空 | 提示词；值可以是文件路径或 `@文件` |
| `-f, --file` | 空 | 从文件读 prompt（`-` = stdin） |
| `-a, --attach` | 空 | 附件（截图 / 图片），可重复或逗号分隔 |
| `--tools` | `off` | `off`（纯 chat）\| `on`（agent 模式）\| 逗号分隔白名单 |
| `-t, --timeout` | `600s` | 单次尝试超时（如 `90s` / `3m`） |
| `-r, --retries` | `0` | 失败重试次数（总尝试 = 1 + retries） |
| `--backoff` | `2s` | 首次重试退避（指数翻倍，上限 30s，带抖动） |
| `-o, --output` | `json` | 输出格式：`json` \| `text` |
| `--stream` | 关 | 流式输出（text 模式思考走 stderr） |
| `--no-thinking` | 关 | 流式模式下不转发思考过程增量 |
| `--engines` | 关 | 列出引擎、可用性、模型、版本与一键安装命令 |
| `--no-models` | 关 | 配合 `--engines`：跳过模型与版本探测 |
| `--json` | 关 | 兼容保留（行为同 `--engines` 的默认 JSON 输出） |
| `--stop` | 空 | 停止指定会话 / 运行（传 `session_id` 或 `run_id`） |
| `--sessions` | 关 | 列出会话登记表 |
| `--keep-alive` | **开** | 常驻会话（仅 `claude`/`codebuddy` 的 `--stream` 调用生效） |
| `--append` | 空 | 向常驻会话追加一条消息（传 `session_id` 或 `run_id`） |
| `--idle` | `5m` | 常驻会话空闲收工时长（`0` = 本轮结束就收工） |
| `-v, --verbose` | `false` | 重试过程打印到 stderr |

prompt 输入优先级：`-p/--prompt` > 位置参数 > `--file` > stdin 管道（stdin 非 TTY 且无其他输入时自动读）。

## 流式模式（`--stream`）

`--stream` 走各引擎的原生协议：claude / codebuddy / trae 走 CLI 的原生 `stream-json` NDJSON，llm 走纯文本流式，openclaw 走 ACP 桥，arkclaw 与 codebuddy-gateway 走 SSE。增量实时转发，无缓冲等待。

思考通道与正文分开：`-o text` 下正文进 stdout、思考进 stderr；`-o json` 下是 NDJSON 事件流（`thinking` / `text` / `tool_use` / `tool_result` / `ask` / `turn_end`），末尾一条 `result` 收尾行。

各引擎的流式成色不同（其中 openclaw 的 ACP 桥、arkclaw 的 A2A SSE、dsh 的 SDK 通道差异最大），细节见 [docs/streaming.md](docs/streaming.md)。

## 常驻会话与追加需求（`--keep-alive` 默认开）

`claude` / `codebuddy` 的 `--stream` 调用**默认即常驻**：长任务跑着的时候，可以在另一个进程追加需求。

```bash
magic-agent -e claude --stream -p "先把仓库跑一遍测试" -o text   # ① 起会话
magic-agent --append <run_id> -p "追加：顺便把 lint 也跑了"       # ② 中途追加
magic-agent --stop <run_id>                                      # ③ 收工（或空闲 --idle 自动退出）

magic-agent -e claude --stream --keep-alive=false -p "..."       # 要「一轮结束就退出」的老行为
```

| 维度 | 行为 |
|---|---|
| 支持范围 | 只有 `claude` / `codebuddy` 的 `--stream` 调用；其它情况自动忽略（显式传 `--keep-alive` 却不满足条件 → `exit 2` 报错） |
| 追加语义 | 消息写进引擎 stdin 成为**下一轮 user 消息**：本轮跑完后接着处理，不打断当前轮 |
| 收工 | 最后一轮结束后 `--idle`（默认 5m）内无人追加则优雅收尾；`--idle 0` = 本轮结束就收工 |
| 超时 | 常驻生效期间不套 `-t` 的默认 600s（长任务 + 追加不该被砍），显式给 `-t` 才生效 |

> ⚠️ **默认常驻带来的行为变化**：进程会在最后一轮结束后继续存活 `--idle` 等待追加。调用方若要「一轮完成」的信号，请读事件流里的 **`turn_end`** 事件（而不是等进程退出），或启动时传 `--keep-alive=false`。

## 停止指定会话

```bash
magic-agent --sessions                 # 列出会话登记表（新→旧）
magic-agent --stop <session_id>        # 按会话 id 停
magic-agent --stop <run_id>            # 按运行 id 停（新会话还没拿到 session_id 时）
```

引擎 CLI 在**独立进程组**里跑，所以调用方对 magic-agent 发信号带不走它 —— 界面显示「已停止」而引擎还在后台跑。因此每次调用都落一条记录（`~/.magic-agent/sessions/<run_id>.json`，`MAGIC_AGENT_SESSIONS` 可改目录，记录保留 24 小时），`--stop` 按记录里的 pid 杀整个进程组（先 `SIGTERM`，2s 内不退再 `SIGKILL`）。

| 情况 | 退出码 |
|---|---|
| 在跑 → 已停 / 进程已不在 / 会话已结束（幂等，重复点「停止」不算错） | 0 |
| id 不存在 | 1 |
| `--stop` 后没给 id | 2 |

## 附件（文件＋提示词）

**提示词照旧，附件是并列的第二份输入**：「截图 + 提示词」是主要用法。

```bash
magic-agent -p "这张图什么颜色" -a ~/Desktop/shot.png
magic-agent -p "对比这两张图的差异" -a a.png -a b.png      # 可重复，也可逗号分隔
```

每个引擎尽量走**自己的原生附件通道**（`--engines` 的 `attachments` 字段给出机器可读答案）：

| 引擎 | 落地方式 |
|---|---|
| `claude` / `codebuddy` | 原生 `stream-json`：图片作为 image content block 经 stdin 传入 |
| `codex` | 原生 `-i/--image`（只收图片） |
| `llm` | 原生 `-a`（委托 llm CLI 时）/ OpenAI 兼容 content parts（直连配置模型时） |
| `arkclaw` | A2A 原生 `file` part（base64 inline） |
| `trae` / `openclaw` / `dsh` / `codebuddy-gateway` | 无原生通道 → 把**绝对路径**写进提示词清单，靠引擎自己的读文件工具 |

校验：路径必须存在、是普通文件、≤32MB；重复路径自动去重；类型按**魔数**嗅探（不信扩展名）。无原生通道的引擎会**明确提示**不静默；提示词在有原生通道时原样不改写。

> 能力上限取决于模型：链路只保证把附件按协议送到位，模型是否真能看图要看后端。实测同一张纯红 PNG，`-e llm` 直连 MiniMax-M3 答对，而走某网关的 claude CLI 把图片块吞掉了。

## 需要用户选择（`AskUserQuestion`）

模型发起 `AskUserQuestion`（或工具调用待授权）时，claude / codebuddy 在 wire 上是两种完全不同的形状。magic-agent 把它们归一化成一份统一格式，`--stream` 输出里多一类 `ask` 事件（消息里带 `questions` / `options`），上层 UI 拿到后渲染给用户，再用 `--append` 把选择传回去。

```bash
magic-agent --engines --no-models | jq -r '.[] | "\(.engine)\t\(.ask)"'
# claude / codebuddy → "tool:AskUserQuestion"；其余 → "none"
```

wire 形状、三条通道与接住回答的写法见 [docs/notes.md](docs/notes.md)。

## 提示词可以是文件路径

长提示词写在文件里，不必每次 `-f` 或 `cat |`：`-p` / 位置参数 / `-s` 的值**命中一个已存在的普通文件**时，直接按文件内容用。

```bash
magic-agent -p ./prompt.md                 # 按文件内容提问
magic-agent -p ./prompt.md "补充一句"       # 文件内容在前、追加文本在后
magic-agent -p @notes/task.md              # @ 强制按文件读（读不到 → exit 2）
magic-agent -s ./reviewer.md "审一下这段"   # 系统提示词也能给文件
```

自动识别的判据刻意保守，避免把正常提示词误判成路径：必须是**单行**、≤4096 字节、`stat` 出来是**普通文件**。所以多行提示词、粘贴的长文都不受影响。`~` 前缀会展开。

## 默认系统提示词与配置

不想每次调用都敲 `-s`，就把默认系统提示词写进配置文件 —— 对所有引擎生效：

```bash
mkdir -p ~/.config/magic-agent && cat > ~/.config/magic-agent/config.json <<'EOF'
{
  "systemPrompt": "你是一个中文助手，始终用中文回答所有问题。",
  "arkclaw": { "url": "…", "key": "…", "claw_id": "…" }
}
EOF
```

| 维度 | 行为 |
|---|---|
| 生效条件 | 调用时未给 `-s` → 注入配置里的 `systemPrompt`；给了 `-s` 则 `-s` 优先，不叠加 |
| 作用范围 | 全部引擎 |
| 键名兼容 | `systemPrompt` \| `system_prompt` \| `system` 任选其一 |
| 临时覆盖 | 环境变量 `MAGIC_AGENT_SYSTEM_PROMPT` 优先于文件值 |
| 配置文件路径 | 默认 `~/.config/magic-agent/config.json`（`MAGIC_AGENT_CONFIG` 覆盖，`XDG_CONFIG_HOME` 优先于 `~/.config`） |
| 配置写坏时 | **不阻断调用**（本次不注入），`-v` 打印原因，不静默 |

完整配置项见 [config.example.json](config.example.json)。

## 输出格式

**json**（默认）：stdout 单行 envelope。

```json
{"engine":"claude","model":"claude-sonnet-4-6","session_id":"...","attempts":1,"latency_ms":534,"text":"..."}
```

**text**（`-o text`）：stdout 只含模型正文 + 尾换行。

**失败时**：stdout 恒为空（不产生半截内容），错误打到 stderr，格式与 `-o` 联动。json 模式下 stderr 也是一行 envelope，可直接 `jq -r .reason` 取根因。

| 字段 | 说明 |
|------|------|
| `error` | 完整错误链（含引擎 / 重试包装），面向人排查 |
| `reason` | 最内层根因（stderr 摘要 / 超时 / 非零退出码），面向程序分支判断 |
| `attempts` | 实际执行次数；`0` = 参数校验阶段即失败 |

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功 |
| 1 | 调用失败（引擎错误 / 超时耗尽 / 重试耗尽） |
| 2 | 参数或输入错误（未知引擎、非法格式、空 prompt 等） |

## 重试语义

- **可重试**：网络类（connection refused / reset、EOF、timeout、signal killed）、限流类（429、rate limit、503 overloaded）、单次尝试超时。
- **快速失败**：参数错、鉴权错、空输出、CLI 明确报错等不可恢复错误 —— 不浪费重试。
- 退避：`backoff * 2^(n-1)` + 抖动，上限 30s；整体 context 取消会立即打断等待。

所有引擎都以独立**进程组**运行：超时 / 取消时 `kill(-pgid)` 杀掉整个进程树，CLI 内部 spawn 的 node worker 不会残留。

## 平台支持

三档平台覆盖 npm 包内自带的 5 个预编译产物，进程组隔离在各平台均生效：

| 平台 | 产物目录 |
|------|---------|
| macOS arm64 / x64 | `npm/dist/darwin-arm64` / `darwin-x64` |
| Linux x64 / arm64 | `npm/dist/linux-x64` / `linux-arm64` |
| Windows x64 | `npm/dist/win32-x64` |

## 更多文档

| 文档 | 内容 |
|------|------|
| [docs/engines.md](docs/engines.md) | arkclaw / codebuddy-gateway / llm / dsh / codex 五个引擎的配置、协议与行为细节 |
| [docs/streaming.md](docs/streaming.md) | 各引擎流式的实现路径与实测差异（openclaw ACP 桥、arkclaw A2A SSE、dsh SDK 通道） |
| [docs/tools-and-permissions.md](docs/tools-and-permissions.md) | `--tools` 工具白名单与四档权限模型的落地细节 |
| [docs/notes.md](docs/notes.md) | 需要用户选择的统一格式、在 WorkBuddy / CodeBuddy 会话内使用时的注意事项 |
| [docs/development.md](docs/development.md) | 打包发布流程、改动落地四步曲、架构与真机验证记录 |
| [CLIENT-CONTRACT.md](CLIENT-CONTRACT.md) | 客户端契约面（magic-client 插件对接用） |

## License

MIT
