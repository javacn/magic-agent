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
npm run bump       # 版本号 +1（patch；也可 minor / major，见下）
npm run build      # 交叉编译全部 5 个平台 -> npm/dist/
npm pack --ignore-scripts   # 产出 magic-agent-<version>.tgz
```

> **改动落地四步曲（用户定稿的硬规则：「每次修改完要全局安装更新本地」+「实现了就测试 测试好了全局安装」+ 2026-09-18 再次强调「改好永远全局安装最新的」+ 2026-09-23「安装的时候 magic-agent 也要升级版本号」）**
>
> 1. **实现** —— 改代码，`gofmt` + `go vet ./...` 干净。
> 2. **测试** —— 单测 `go test -count=1 ./...` 全绿（`-count=1` 避免缓存骗人），**并**跑一次真机验收
>    （假 CLI 落 argv/stdin 取证 + 真实引擎冒烟；只跑单测不算「测试好了」）。
> 3. **升版本号**（2026-09-23 新增，用户：「安装的时候 magic-agent 也要升级版本号」）——
>    `npm run bump`（= `node npm/bump.js`，默认 patch；`minor` / `major` 可选，`--dry-run` 只看不写）。
>    **为什么必须有这一步**：版本号是「这份 CLI 是哪次构建」的唯一可读标识，而**上层按版本号挑最新的那份** ——
>    magic-test 的 `tools/build-mac-app.sh` 就是「候选去重后按 `package.json` 的 version 排序取最高」再打进应用包。
>    版本号长期停在同一个值时那个判据形同虚设：本机两份全局安装代码不同、版本相同 → 只能按 PATH 顺序取，
>    **取到旧的那份也看不出来**（2026-09-23 实测踩到：应用包里那份是旧构建，界面里少一个引擎、能力字段也不对）。
> 4. **全局安装（每次改完都要，没有例外）** —— 两份全局前缀都更新，然后 `shasum` 核对：
>
> ```bash
> npm run bump && npm run build && npm pack --ignore-scripts
> SANDBOX="/Users/<you>/Library/Application Support/TRAE SOLO CN/ModularData/ai-agent/vm/tools/npm-global"
> npm install -g --prefix "$SANDBOX" ./magic-agent-<version>.tgz       # 沙箱那份（观物台用）
> npm install -g --prefix /opt/homebrew ./magic-agent-<version>.tgz    # 用户登录 shell 那份
> shasum -a 256 npm/dist/darwin-arm64/magic-agent \
>   "$SANDBOX/lib/node_modules/magic-agent/npm/dist/darwin-arm64/magic-agent" \
>   /opt/homebrew/lib/node_modules/magic-agent/npm/dist/darwin-arm64/magic-agent
> ```
>
> 三份 hash 必须一致；**三份 `package.json` 的 version 也必须一致**（`npm install -g` 会把包内那份一起装进去，
> 上层读的就是它）。⚠️ 只 `cp` 二进制不算装 —— 那样安装目录里的 version 还是旧的，上层的「取版本最高」会判错。
> 只 build 不 install ⇒ `magic-agent` 命令还是旧行为，用户侧表现为「改了没效果」。
> `npm warn allow-scripts`（postinstall 被拦）无害：产物已预编译、`~/.llm-venv` 已存在时它什么都不做。
>
> ⚠️ **别用 `npm config get prefix` 定位沙箱那份**：它随 PATH 变 —— 同一台机器上有时解析成
> `/opt/homebrew`（于是「两份」其实是同一份，沙箱那份悄悄留着旧的，2026-09-18 就这么漏过一次），
> 有时才解析成沙箱前缀。一律用**绝对路径 `--prefix`** 显式装两份，再各自 `shasum` 比对。
>
> ⚠️ **别拿上一轮的 hash 当基准**：实测同源码连编三次 hash 完全相同，但同一台机器上前后两次
> `npm run build` 的产物 hash 可以不同（原因未查明）。所以 `shasum` 只用于验证
> 「**本轮** build + install 的三份一致」；「装的是不是最新」现在可以看**版本号**，
> 但仍建议叠加一次**功能自检**（例如 `magic-agent --engines --no-models` 里有没有新字段）——
> 版本号只证明「这份文件是那轮构建的」，不证明「这轮改动真的生效了」。

`npm run build:current` 只编译当前平台（本地开发更快）。版本号由 `npm build` 从 `package.json` 经 ldflags 注入 `internal/cli.Version`，**无需手改源码**（改的是 `package.json`，`npm run bump` 代劳）。
⚠️ 因此**升版本号后必须重新 build + 装两份全局前缀**，否则 `--version` 还是旧的（版本号是编译期注入的）。
CI 发版走另一条路：`.github/workflows/release.yml` 从 **git tag** 解析版本号再写回 `package.json` —— 与 `npm run bump` 别混用（`bump.js` 刻意不调 `npm version`，避免顺手 commit + 打 tag）。

> 平台不支持或产物缺失时，`postinstall` 会尝试用本机 Go 现场编译；两者都没有则只告警，不阻断安装。
> 同一个 `postinstall` 还会安装 **llm CLI**（simonw/LLM，`-e llm` 引擎的依赖）：已有安装（`MAGIC_AGENT_LLM_BIN` / `~/.llm-venv` / PATH / brew）则跳过；否则建 `~/.llm-venv` 隔离安装，失败只告警不阻断。

依赖：Go 1.26+（仅源码构建需要）。七个 CLI 按需安装，未安装的引擎自动探测失败但不影响其他引擎（`magic-agent --engines` 会为每个引擎给出 `install` 一键安装 / 升级命令，见下）：

| 引擎 | CLI | 探测路径 / 环境变量 |
|------|-----|---------------------|
| claude | Claude Code | `MAGIC_AGENT_CLAUDE_BIN` → `/opt/homebrew/bin/claude` → PATH |
| codebuddy | WorkBuddy 内置 CLI | `MAGIC_AGENT_CODEBUDDY_BIN` → `WorkBuddy.app/.../cli/bin/codebuddy` → PATH |
| trae | trae-cli | `MAGIC_AGENT_TRAE_BIN` → `~/.local/bin/trae-cli` → PATH |
| llm | [simonw/LLM](https://github.com/simonw/LLM) | `MAGIC_AGENT_LLM_BIN` → `~/.llm-venv/bin/llm` → `/opt/homebrew/bin/llm` → PATH |
| codex | `@openai/codex` | `MAGIC_AGENT_CODEX_BIN` → `/opt/homebrew/bin/codex` → PATH |
| openclaw | OpenClaw | `MAGIC_AGENT_OPENCLAW_BIN` → `/opt/homebrew/bin/openclaw` → PATH |
| dsh | [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) | `MAGIC_AGENT_DSH_BIN` → `~/.local/bin/dsh` → `/opt/homebrew/bin/dsh` → PATH → npm 全局前缀（`npm i -g @deepseek-ai/dsh`） |
| arkclaw | 无（远端 A2A 网关） | 配置文件 `~/.config/magic-agent/config.json` 的 `arkclaw` 节，或 `MAGIC_AGENT_ARKCLAW_URL` / `_KEY` / `_CLAW_ID` |
| codebuddy-gateway | 无（已在跑的 CodeBuddy Code HTTP 网关） | 同文件的 `codebuddyGateway` 节，或 `MAGIC_AGENT_CBGW_URL` / `_PASSWORD`（见「codebuddy-gateway 引擎」） |

**探测链的最后一跳是 npm 全局前缀**：`<NPM_CONFIG_PREFIX>/bin`（含小写 `npm_config_prefix`）。
它只做补漏、不改变既有优先级（显式参数 > 环境变量 > 候选路径 > PATH > npm 前缀），
专治「CLI 确实装好了、探测却说 not found」—— 沙箱/容器常把 `npm i -g` 的落点设成一个
**不在 PATH 里**的私有前缀（本机实测：`npm prefix -g` 指向 `…/ai-agent/vm/tools/npm-global`，
只有把该目录留在 PATH 的进程才探测得到）。若你的运行环境两者都没有，给 CLI 在候选位
建个软链即可让任意环境命中，例如 `ln -sf "$(npm prefix -g)/bin/dsh" ~/.local/bin/dsh`。

```bash
$ magic-agent --engines
[{"engine":"claude","ok":true,"bin":"/opt/homebrew/bin/claude","models":["claude-haiku-4-5","MiniMax-M2.7-highspeed","claude-opus-5[1M]",...]},
 {"engine":"codebuddy","ok":true,"bin":"/Applications/WorkBuddy.app/.../cli/bin/codebuddy","models":["auto","hy4-preview","hy3",...]},
 {"engine":"trae","ok":true,"bin":"/Users/you/.local/bin/trae-cli","models":["Doubao-Seed-Evolving","GLM-5.3",...]},
 {"engine":"llm","ok":true,"bin":"/Users/you/.llm-venv/bin/llm","models":["gpt-4o","gpt-4o-mini",...]},
 {"engine":"dsh","ok":true,"bin":"/opt/homebrew/bin/dsh","models_note":"no dynamic model source: dsh 的模型由配置层决定（$DSH_HOME/settings.yaml 的 agent-default-model 段或 Settings → Models），headless 无 --model 参数"},
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
| `install` | **该引擎的「一键安装 / 升级命令」**（shell 一行，可直接执行）。**不可用（`ok:false`）时 = 安装**；**可用时也给 = 升级**（同一条命令对已装好的引擎重跑一遍就是装最新版，2026-09-23 起）。没有可执行安装路径的引擎（codebuddy / codebuddy-ai 是桌面端 GUI 应用、arkclaw 与具名 A2A agent 是远端网关）不给，原因看同行的 `note` |
| `version` | **该引擎当前版本号**（2026-09-23 新增，尽力而为）：跑 `<bin> --version`，取**第一行**第一个 semver。拿不到（桌面端 GUI 应用不认 `--version`、llm 走 venv 脚本、A2A 网关无本机 CLI、`--no-models` 时跳过探测）就**不给**该字段 —— 是「升级到底有没有生效」的唯一依据，调用方**不许拿它当可用性判据** |
| `models` | **该引擎当前支持的模型**（动态探测，见下） |
| `models_note` | 拿不到 `models` 时的原因（无动态来源 / 探测失败） |
| `model_credits` | **模型的积分倍率表**（model → 倍率数字字符串，如 `"fast-model":"0.34"`）。`codebuddy` / `codebuddy-ai` 给出，与 `models` 同源同链；其他引擎不给（客户端没计费口径）。客户端按这张表给模型加"限免/夜间免费"等角标与结算。 |
| `workspace` | 「指定工作目录」的落地方式：`flag:-C`（codex）/ `cwd`（claude、codebuddy、trae、dsh）/ `none`（llm、arkclaw、openclaw、codebuddy-gateway） |
| `streaming` | 是否支持 `--stream`。openclaw 走 `openclaw acp`（见「openclaw 流式（ACP 桥）」）；arkclaw 走 A2A SSE、codebuddy-gateway 走 SSE；openclaw 的 ACP 桥不可用时该字段临时变 `false` |
| `attachments` | 「收附件」的落地方式：`stdin:stream-json`（claude、codebuddy，原生图片）/ `part:file`（arkclaw）/ `flag:-i`（codex）/ `flag:-a`（llm）/ `prompt`（trae、openclaw、dsh、codebuddy-gateway，把路径写进提示词） |
| `append` | 是否支持常驻会话 + 追加消息（`--keep-alive` / `--append`）。`claude` / `codebuddy` / `codebuddy-ai` 默认开；`dsh` 默认关，需显式 `--keep-alive` |
| `ask` | 「需要用户选择」的落地方式：`tool:AskUserQuestion`（claude、codebuddy）/ `none`（其余）。见「需要用户选择」 |
| `permission` | 四档权限模型（`manual` / `accept-edits` / `auto` / `full`）。`claude` / `codebuddy` 走 `--permission-mode`；其他引擎无原生权限档，传 `--permission` 会报错。见「四档权限模型」 |
| `capabilities` | **该引擎的静态能力 id 集合**（字典序、无重复；`--contract` 与 `--engines` 都给）。它回答「**能做什么**」，与上表那些回答「此刻如何」的字段（`ok` / `streaming` / `models` / `version`）严格分开 —— 客户端按 `capabilities` 定界面形态，按运行态字段定按钮可用与降级提示。id 与推导见 `internal/agent/capability.go`：`session.stream` / `session.append` / `session.ask` / `session.permission` / `workspace.select` / `attachment.native` / `attachment.prompt` / `model.list` / `model.credits` / `engine.install` |

`--json` 为兼容旧调用保留（行为相同）。

### 客户端契约（`--contract`）

桌面与移动客户端插件（magic-client）启动时读这一份，用来做契约校验与界面降级：

```bash
magic-agent --contract                                  # {"contractVersion":1,"engines":[...]}（快）
magic-agent --contract | jq -r '.contractVersion'       # 启动时先校验这个
magic-agent --contract | jq '.engines[] | {engine, capabilities}'
```

| 字段 | 含义 |
|------|------|
| `contractVersion` | 契约面版本（当前 `1`）。客户端拿它比对**自己支持的范围**，超出就明确报错并提示升级 —— 不要带着不匹配的能力继续跑 |
| `engines` | 与 `--engines` 的行**完全同构**（同一份构建代码 `emitEngineRows`），每行另含 `capabilities` |

`--engines` 顶层**恒为数组**（老调用方按数组解析，不能改成对象）；契约版本只出现在 `--contract` 的 envelope 里。

⚠️ **`--contract` 默认不探测模型**（实测 0.7 秒级）。模型探测要逐个引擎起真 CLI（10 个引擎 30 秒级），
客户端在启动路径上等不起；要连模型一起拿，显式写 `--no-models=false`。

版本递增规则：**只增字段不升版本**；一旦字段改名、语义变化或被删除，必须升 `contractVersion` ——
老客户端会因版本不匹配而拒绝启动，这正是想要的结果。

### 一键安装 / 升级（`install` 字段）

每行会带一条**可直接执行**的命令，调用方（观物台等）检测到不可用即可拿它自救，
不必再去翻各 CLI 的文档；**可用的行也给同一条命令 —— 重跑一遍就是升级到最新版**
（2026-09-23 用户：「引擎检测除了 a2a 的 其他也要支持有升级」）：

| 引擎 | `install` |
|------|-----------|
| claude | `npm install -g @anthropic-ai/claude-code` |
| codex | `npm install -g @openai/codex` |
| openclaw | `npm install -g openclaw@latest` |
| dsh | `npm i -g @deepseek-ai/dsh` |
| trae | `sh -c "$(curl -L https://trae.cn/trae-cli/install.sh)"` |
| llm | `python3 -m venv ~/.llm-venv && ~/.llm-venv/bin/pip install llm` |
| codebuddy / codebuddy-ai / arkclaw / codebuddy-gateway / 具名 A2A agent | **无该字段**（桌面端 GUI 应用 / 远端或本机网关，没有可执行的安装命令），怎么才能用见同行 `note` |

```bash
# 装齐本机缺的引擎（缺什么装什么；没有 install 字段的会打印空行，可按需过滤）
magic-agent --engines --no-models | jq -r '.[] | select(.ok|not) | "\(.engine)\t\(.install // .note)"'
# dsh	dsh CLI not found (npm i -g @deepseek-ai/dsh, or set MAGIC_AGENT_DSH_BIN)

# 升级：看版本号 → 跑命令 → 再看版本号（变了才算升上去）
magic-agent --engines --no-models | jq -r '.[] | select(.install) | "\(.engine)\t\(.version // "-")\t\(.install)"'
# claude	2.1.146	npm install -g @anthropic-ai/claude-code
# codex	0.154.0	npm install -g @openai/codex
```

命令一律取自各 CLI 的**官方安装方式**（claude 另有 `curl -fsSL https://claude.ai/install.sh | bash` 与
`brew install --cask claude-code`；llm 另有 `pip install llm` / `pipx install llm` / `brew install llm`）。
只给「装得上」的引擎 —— 没有可执行安装路径的引擎**不编一条跑不通的命令**。

> `version` 是**尽力而为**的读数，不是可用性判据：桌面端 GUI 应用（codebuddy）与 venv 脚本（llm）
> 取不到 semver → 字段缺失，此时「升级有没有生效」只能靠**引擎自己的 `--version` / 界面**判断。
> `--no-models` 会跳过版本探测（该档只求快），要版本号就别带这个 flag。

### 模型清单（不硬编码）

`models` 一律从各引擎自己的权威入口现取现算 —— 引擎升级、用户新注册模型后自动跟随，代理侧不维护任何模型名常量：

| 引擎 | 动态来源 |
|------|---------|
| claude | `~/.claude/settings.json`（顶层 `model` + env 里 `ANTHROPIC_*MODEL[*_NAME]`）；claude 无 `models` 子命令 |
| codebuddy | `codebuddy --help` 里 `--model` 描述自带的 `Currently supported: (...)` 清单（含用户自定义 `custom-local:*`） |
| codebuddy-ai | 三级链：客户端合并配置缓存 `~/.workbuddy-ai/cache/acc-product-config-v*.json`（客户端模型选择器同源，27 条 = 23 预制 + 4 `custom-local`，含 `deepseek-v4.1-flash` / `gpt-5.6-*` / `gpt-6-astra`；glob 取 mtime 最新）→ 客户端未运行过时回退「远程配置缓存 ∪ App 包 `product.json`」超集近似 → 回退 `--help`（4 个分层别名）。WorkBuddy 端不启用扩展链，仍走 `--help` |
| trae | `trae-cli models --json`（取 `name`，就是 `-c model.name=<name>` 的取值） |
| llm | `llm models`（llm CLI 自己注册的模型） |
| codex | `codex debug models`（raw model catalog 的 `slug`） |
| openclaw | `openclaw models list --json`（取 `key`，形如 `minimax/MiniMax-M3`） |
| arkclaw | 无清单 —— 模型由网关按 `claw_id` 绑定，改用 `models_note` 说明 |
| codebuddy-gateway | 无清单 —— 模型由**网关进程**自己的配置决定（webhook 协议里没有模型字段），改用 `models_note` 说明 |
| dsh | `$DSH_HOME/settings.yaml` 里**已配置**的模型（`llm-pi-ai.providers.<route>.models`，取 `id`；没声明时回落 `agent-default-model`）；输出 `route/model` 形态 |

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

### openclaw 流式（ACP 桥）

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

### openclaw 非流式的两条路（`--local` 与 Gateway）

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

### arkclaw 流式（A2A SSE）

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

## Flags

| Flag | 默认 | 说明 |
|------|------|------|
| `-e, --engine` | `codebuddy` | 引擎：`claude` \| `codebuddy` \| `codebuddy-ai` \| `codebuddy-gateway` \| `trae` \| `llm` \| `codex` \| `openclaw` \| `dsh` \| `arkclaw`，**外加配置里 `agents` 声明的具名 agent**（如 `MagicAI`，见「具名 A2A agent」） |
| `-m, --model` | 空 | 模型（空 = 引擎默认；codebuddy 默认 `hy3`；llm 用其自身默认模型）。trae 无 `--model`，内部转 `-c model.name=<m>`；dsh 走 SDK 通道时生效（转成 `initialize` 的 `provider`/`model`，取 `route/model` 形态），回退 headless 后不生效并打一行告警（模型在 dsh 自己的配置里） |
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
| `--engines` | 关 | 列出引擎、可用性（JSON 数组：`engine` + `ok`）与各引擎当前支持的模型（`models`，动态探测不硬编码；拿不到时给 `models_note`），并给 `install`（一键安装 / 升级命令，有官方安装方式的引擎都给）与 `version`（当前版本号，尽力而为）；替代原 `engines` 子命令 |
| `--no-models` | 关 | 配合 `--engines`：跳过模型探测**与版本探测**（只列引擎、可用性与 `install`，不启动任何 CLI） |
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
| `trae` / `openclaw` / `dsh` | `prompt` | 无原生通道 → 把**绝对路径**写进提示词末尾的「【附件】」清单，靠引擎自己的读文件工具 |
| `codebuddy-gateway` | `prompt` | 走 `payload.attachments` 原生字段（`urlType: local-path`），但**网关**把它渲染成「附件清单 + 路径」塞进 prompt —— 图不会以图片形式进模型上下文，故如实报 `prompt` |

规则与边界：

| 维度 | 行为 |
|---|---|
| 校验 | 路径必须存在、是**普通文件**、≤32MB（base64 后还要胖 1/3），否则 `exit 2`；重复路径自动去重 |
| 类型识别 | 按**魔数**嗅探（PNG/JPEG/GIF/WebP/BMP/PDF），不信扩展名；认不出按扩展名兜底，最后 `application/octet-stream` |
| 非图片附件 | codex 的 `-i` 只收图片、llm 的 `-a` 也有类型限制 → 无原生通道时统一退化为「路径写进提示词」 |
| 无原生通道的引擎 | **明确提示**不静默：`magic-agent: 警告：trae 引擎没有附件输入通道，N 个附件已改为「把路径写进提示词」…`；`--tools off` 时会追加「引擎读不到这些文件，请改用 `--tools on`」—— 这句只在引擎**真的有 `--tools` 落地通道**时出现（`ToolsSwitchableOf`）：dsh / openclaw 自带工具循环，传 `--tools` 不改变行为，说了等于误导 |
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
| 作用范围 | 全部引擎（claude / codebuddy / trae / llm / codex / openclaw / dsh / arkclaw，含 llm 直连配置模型） |
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
网关：magic-agent 把 prompt 组成一个 JSON-RPC 请求 POST 过去，网关侧跑它自己的 agent，
再把结果包成 A2A Task 返回来。两个方法都接：

- **`message/send`**（默认）：单次请求-响应；
- **`message/stream`**（`--stream`）：A2A 官方的 SSE 事件流。⚠️ 本网关**不吐逐字增量**，
  只有「受理帧 + 整段正文」两帧 —— 详见「arkclaw 流式（A2A SSE）」。

### 配置（url / key / claw_id 放配置文件）

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

### 具名 A2A agent（`agents` 数组）

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

### 用法

```bash
magic-agent --engines                                   # 看 arkclaw 是否已配置齐备
magic-agent -e arkclaw "你好"                            # 单轮 message/send
magic-agent -e arkclaw --session <contextId> "接着说"     # 按 contextId 续接同一上下文
magic-agent -e arkclaw --stream "你好"                   # 流式（A2A SSE；正文仍整段到达，见下）
magic-agent -e arkclaw --json-schema '{"type":"object",...}' "输出 JSON"   # 结构化输出
```

### 行为细节

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

### 协议（两段式）

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

### 帧类型

| `status` | 载荷 | 处理 |
|---|---|---|
| `accepted` | 无正文 | 忽略（**别拿它当「连接成功」的判据**：网关侧用的是普通 RxJS Subject，投递完才连流的客户端收不到这一帧） |
| `streaming` | `content.chunk`（**增量**） | 拼接后按增量转发（⚠️ 实测当前网关不发这类帧，见下） |
| `completed` | `content.markdown`（**整段**，权威值）+ `agent.sessionId` / `agent.toolCalls` | 收尾，只补发差量（不重复发整段） |
| `error` | `error.code` / `error.message` | 立即报错终止 |

### 实测帧序列（2026-09-24，WorkBuddy CLI 2.147.0）

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

### 配置

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

### 用法

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

### 行为细节

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

### 配置模型（不在本 CLI 上切）

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

### 流式（`--stream`）

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

> `codex` / `openclaw` / `dsh` / `arkclaw` 未列入上表：codex 与 openclaw 走各自 CLI 的原生参数（含 codex 的 `CODEX_HOME` 隔离，见下文），dsh 走 `--profile headless "<任务>"`（见「dsh 引擎」），arkclaw 则完全不启进程 —— 它是 HTTP 引擎（非流式走 `message/send`，流式走 `message/stream` 的 SSE），没有二进制、没有进程组可杀，超时由 `context` + `http.Client` 控制。

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

> 自带工具循环、**没有命令行级逐工具开关**的引擎（openclaw / dsh / arkclaw）：
> `--tools` 整体忽略（工具集由引擎自己的配置决定，dsh 靠 profile bundle /
> `cordis.patch.yml`，openclaw 靠 agent 配置，arkclaw 由网关侧决定），
> 也不会注入 `noToolSuffix` —— 后缀只会和引擎自己的 agent 行为打架。

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
`flag:--permission-mode`）。传给 trae / llm / codex / openclaw / dsh / arkclaw 时**exit 2 明确报错**，
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
  openclaw_acp.go           OpenClaw 流式通道（以 ACP client 驱动 `openclaw acp`；含回退与冷却）
  dsh.go                    DeepSeek Harness 引擎（默认 --profile sdk 的 JSON-RPC 通道，回退 --profile headless "<任务>"）
  dsh_sdk.go                dsh SDK 通道：stdio 换行分帧 JSON-RPC 2.0（initialize / session/prompt / shutdown + session.event）
  arkclaw.go                ArkClaw 引擎（A2A JSON-RPC message/send，HTTP，非流式）
  arkclaw_stream.go         ArkClaw 流式通道（A2A message/stream 的 SSE 事件流 + 非 SSE 回退）
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
- 单元测试：`internal/agent/arkclaw_test.go`（31 个用例，httptest 全覆盖成功/401/续接落点/JSON-RPC error/任务失败态/artifacts 兜底/空正文/凭据缺失/Continue 拒绝/JSONSchema 后处理）+ `internal/config/config_test.go`（路径优先级、缺失与空文件、语法错、宽松键名、环境变量覆盖）

## 已验证（2026-09-24，「生成周报」卡住 = 超时被砍，不是流式没支持）

用户报障原文：「arkclaw 生成周报输入 生成周报 没用返回 一直生成中 是没支持流式还是显示异常」
→ 结论：**流式是支持的，界面也确实把失败显示出来了**（截图里是 `● 调用失败 · context deadline exceeded`）；
「什么都没显示」的真因是**任务在网关侧要 304s，而流式路径把超时锁死在 3 分钟**。

| 观测 | 实测 |
|---|---|
| 任务真实耗时 | 绕开 magic-agent 裸打 SSE：0.13s 受理帧，之后**每 15.2s 一帧 `working` 心跳**（15/30/45…285s），**304.24s** 才来 `completed`（586 字，内容是「需要你先扫码授权，我才能拉取 6 人的飞书周报卡片」） |
| 网关有没有 3 分钟上限 | **没有**：裸测跨过 180s / 285s 不断流，心跳照发 |
| 客户端表现 | 不带 `-t` 与带 `-t 600s` **都是整 3:00 挂掉**（`arkclaw: 流式读取中断: context deadline exceeded`），登记表 `state=failed` |
| 历史 4 次「生成周报」 | 09-23 17:56 / 19:12 / 19:32 与 09-24 14:29 全部**整 3:00** failed |
| 桌面壳传的超时 | `magic-test/desktop/agent-cli.cjs` 恒拼 `-t <n>s`，`DEFAULT_TIMEOUT_SEC = 600` → 壳想要 10 分钟，被压成 3 分钟 |
| 失败可见性 | json 模式 stdout 有 `{"type":"error","engine":"MagicAI",…,"reason":"context deadline exceeded"}` + exit 1；text 模式 stderr 一行 —— **不是静默失败** |

**根因与修复**：流式路径从不设 `req.Timeout`（CLI 只在非流式那条路设 `Runner.Timeout`），
`arkclaw_stream.go` 于是回落到 `DefaultArkClawTimeout` = 3 分钟，把壳给的 `-t 600s` 静默压掉。
两处修：① `runStreamAsk` 在 `flagChanged("timeout")` 时把 `-t` 写进 `req.Timeout`；
② `DefaultArkClawTimeout` 3 分钟 → **10 分钟**（与 `-t` 默认对齐）。
回归测试：`TestStreamPassesExplicitTimeoutToEngine`（cli）+ `TestDefaultArkClawTimeoutCoversObservedLatency`（agent）。

**遗留（未做）**：15s 心跳目前不透出成事件 → 长任务的几分钟里界面收不到任何东西，无法区分
「在跑」和「挂了」。要做需新增事件类型并同步桌面壳的解析契约。

## 已验证（2026-09-22，arkclaw 流式 = A2A SSE，非 WebSocket）

需求原文：「arkclaw 的这个也支持改成 ws 协议 看看是否可行」→ 结论：**WS 不可行，正路是 A2A 官方 SSE**。
同一端点（`https://<host>/a2a/jsonrpc?apikey=…&clawId=…`）三项实测：

| 探测 | 请求 | 网关实际返回 |
|---|---|---|
| 基线 | `POST message/send` | `HTTP 200` + `application/json`，往返 **19.8s**，正常返回 Task 与 `contextId` |
| **WS 握手** | 带 `Connection: Upgrade` / `Upgrade: websocket` / `Sec-WebSocket-Version: 13` 的 POST | **无 `101 Switching Protocols`**：被当成普通 POST，照常回 `200 + application/json`（`server: istio-envoy`、`x-powered-by: Express`）→ 服务端没有 WS 升级处理器 |
| 官方流式 | `POST message/stream` + `accept: text/event-stream` | `200` + `content-type: text/event-stream` + `chunked` + `x-accel-buffering: no`，标准 SSE |

- **SSE 帧节奏（关键）**：0.20s 收到 `status.state=working`（空正文）；12.42s 收到 `completed`，**654 字正文一次性到达** —— 共 2 帧，**没有逐字增量**。所以 arkclaw 的 `--stream` 是「受理帧 + 整段正文」，不是打字机（要逐字得 claw 侧改）。
- **重复正文**：`completed` 帧把同一份正文同时放在 `status.message.parts` 与 `artifacts[].parts` 里 → 客户端必须做前缀去重，否则调用方看到两份。
- **协议与网关两层都堵住 WS**：A2A 核心规范只定义 JSON-RPC over HTTP(S) / gRPC / HTTP+JSON，流式统一 SSE，WS 属官方「自定义协议绑定」（需服务端实现）；火山引擎 API 网关协议枚举只有 HTTP/HTTPS 与 HTTP1.1/HTTP2/HTTP2-GRPC，无 WebSocket API 类型、也无法把已有 HTTP API 升级成 WS。
- **端到端实跑**（真实网关，`streaming:true` 后）：`--stream -o text` 19.0s 出正文；`--stream`（json）输出 1 条 `{"type":"text"}` + 收尾 `result`（`session_id` = `contextId`）；`--stream --session <上一轮 contextId>` 追问「我刚才让你数到几」→ 答 `3`（续接生效）；`--stream --continue` 明确报错 exit 1；非流式 `message/send` 回归正常。
- 单元测试：`internal/agent/arkclaw_stream_test.go`（17 个用例，httptest 全覆盖：接口契约/sse 标记、请求形状（method + `accept` + contextId 落点）、心跳与分隔行跳过、working 帧不发事件、completed 发正文、artifacts 重复去重、中间 artifact 增量转发、失败态/JSON-RPC error/401/无终态帧/空正文、非 SSE 回退、JSONSchema 后处理、超时打断读流）；`go build` / `go vet` / `go test ./...` 全绿。

## 已验证（2026-09-21，codebuddy-ai 模型清单来源）

AI 客户端的模型选择器数据源是**客户端自己的合并配置缓存** —— AI App 数据主目录为
`~/.workbuddy-ai/`（daemon 进程参数 `--app_home` 可证），它把「product.json ∪ 网关远程配置 ∪
用户自定义模型」合并后缓存在 `cache/acc-product-config-v3.json`，与 product.json 同构。
此前两版方案（product.json 26 条 / 远程配置缓存 ∪ product.json 74 条）都不是客户端所见，已废弃。

| 来源 | 位置 | 实测 |
|------|------|------|
| ① 客户端合并配置缓存（**首选**） | `~/.workbuddy-ai/cache/acc-product-config-v*.json` 的 `models[].id` | 27 条 = 23 预制 + 4 `custom-local`；`endpoint=www.workbuddy.ai`、daemon 持续刷新（mtime 当天）；含 `deepseek-v4.1-flash`、`deepseek-v4.1-flash-sg`、`gpt-5.6-sol/terra/luna`、`gpt-6-astra` 等静态文件里还没有的模型 |
| ② 远程配置缓存 ∪ product.json | `<config>/local_storage/entry_*.info` 的 per-user `data.models` ∪ `<app>/…/cli/product.json` | 超集近似（74 条，混着国内后端条目），仅在客户端从未运行过（① 不存在）时使用 |
| ③ `--help` | `Currently supported: (...)` | 仅 4 个分层别名，最后回退 |

- `magic-agent --engines` 实测：`codebuddy-ai` 27 个（预制 23 + custom 4），与客户端一致；`deepseek-v4.1-flash` ✓。
- **积分倍率**：`--engines` 对 `codebuddy` / `codebuddy-ai` 额外输出 `model_credits`（model → 规范化倍率数字字符串，如 `"fast-model":"0.34"`，源自客户端 `"x0.34 credits"`）。实现：`agent.ModelCreditLister` 可选接口 + 两个引擎的 `ModelCredits`（共享 `codebuddyCore.modelCredits`，与 models 同链：① acc 缓存 → ② 并集兜底），`--no-models` 时一并跳过。两端 acc 缓存不同目录：`codebuddy-ai` → `~/.workbuddy-ai`（22/23 个预制模型有值，`default-model` 与 custom-local 客户端不给）；`codebuddy` → `~/.workbuddy`（实测 58 个模型 33 条倍率，含 hy3）。**codebuddy 只补倍率、清单仍按 `--help`**（扩展清单链不启用，见下条），倍率表按 model id 与 `--help` 清单对上。
- ① 的文件名带版本号（当前 v3）：glob 全部版本取 mtime 最新，客户端升级版本号后自动跟随；客户端从未运行过时静默落 ②。
- WorkBuddy 端不启用扩展链：其 `--help` 已是完整用户清单（23 条），各缓存反而混有 `completion-gf`/`codewise-*`/`hunyuan-3b` 等内部模型。
- 单元测试：`internal/agent/models_test.go` 的 `TestCodeBuddyListModelsAccConfig`（acc 命中精确返回 / 多版本取最新）/ `TestReadRemoteConfigCacheModels` / `TestParseProductJSONModels` / `TestProductJSONPath` / `TestCodeBuddyListModelsProductJSON`（②③ 回退、WorkBuddy 端忽略扩展来源、显式配置目录不兜底共享缓存）/ `TestCodeBuddyModelCredits`（codebuddy 倍率链：acc 命中不混源 / 缓存∪product 合并 / 全无 → nil / 清单仍按 --help / 不启动 CLI）；`go build` / `go vet` / `go test ./...` 全绿。

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
| dsh | `$DSH_HOME/settings.yaml` | `models_note` 只在读不到时给：模型由 dsh 自己的配置层决定（`agent-default-model` + `llm-pi-ai.providers.<route>.models`，输出 `route/model`）；SDK 通道下这份清单就是 `-m` 的可取值 |

- 全量探测并发执行，整条命令 **4.2s**（最慢单点是 openclaw）；`--engines --no-models` 不启动任何 CLI，退化为原来的快速探测链。
- 单元测试：`internal/agent/models_test.go`（解析器 + 假 CLI 逐引擎断言命令构造与解析 + claude settings.json 的 HOME/`CLAUDE_CONFIG_DIR` + 缺失/失败路径）、`internal/cli/ask_test.go`（`--engines` 的 `models` / `models_note` 装配与 `--no-models` 零探测）；`go build` / `go vet` / `go test ./...` 全绿。

## 已验证（2026-09-21，dsh 引擎 · 真机）

真机环境：dsh 0.1.5-rc.2（npm 全局装），模型走自建 OpenAI 兼容路由
（`llm-pi-ai.providers.modelverse` → `https://api.modelverse.cn/v1`，模型
`deepseek-v4.1-flash`），密钥在 `$DSH_HOME/.credentials.yaml`。

- 配置生效验证：`dsh --profile headless "你是哪个模型"` → `deepseek-v4.1-flash`；
  非流式 `magic-agent -e dsh -p "1+1=?"` → `2`，`--engines` 里 dsh `ok:true`
- **模型清单**（`--engines` 的 `models`）：从 `$DSH_HOME/settings.yaml` 读出已声明的
  `modelverse/<model>`（**只有声明过的才被路由接受**；网关 `GET /v1/models` 的 271 个里
  混着大量图像/视频/音频/检索模型，不能当可用清单用）
- **流式实测**（`--stream`，headless 回退通道）：推理增量在 stderr 逐段到达
  （3.06s 起、约 300ms 一批），正文在 8.29s 收尾一次性给出 —— 与「其余引擎增量走
  stdout」正好相反，故 `runStreamStderrIn` 扫的是 stderr
- 事件形状：`{"type":"thinking","text":"17*23 = 391.\n"}` →
  `{"type":"text","text":"17 × 23 = 391\n\n计算过程：…"}` →
  `{"type":"result","engine":"dsh","model":"modelverse/deepseek-v4.1-flash","attempts":1,"latency_ms":8279}`
- 能力字段：`streaming:true` / `workspace:cwd` / `attachments:prompt` /
  `permission:none` / `append:false` / `ask:none`
- 单元测试：`internal/agent/dsh_test.go`（headless 通道：参数形态、stdout 取正文、
  非推理 stderr 不误当增量、续接拒接、非零退出等）；
  `go build ./...` / `go vet ./...` / `go test ./...` 全绿

## 已验证（2026-09-22，dsh SDK 通道 · 真机）

同一台机器（dsh 0.1.5-rc.2 + modelverse 路由）。**headless 拿不到工具调用**是本轮
的出发点，SDK 通道是解法：

- **工具调用只在会话日志里**：headless 跑「用 bash 执行 echo」时模型确实调了工具，
  但 stdout 只有最终正文、stderr 只有 `dsh: reasoning:` 推理增量，**工具调用一个字都没有**
  （它被写进 `$DSH_HOME/sessions/…/session.v3.jsonl.zstd`，zstd 压缩）
- **SDK 通道实测可用**：`dsh --profile sdk` + stdio 换行分帧 JSON-RPC 2.0；
  握手 `initialize {cwd, provider, model}` → `{"serverInfo":{"name":"deepseek-harness-sdk-runtime","version":"0.0.1"}}`
  （**必须先等 initialize 响应再发 `session/prompt`**，抢跑会被拒）
- **工具调用实时到达**：
  `{"type":"tool/call","data":{"turn":1,"step":1,"callId":"call_00_…","name":"bash","arguments":"{\"command\":\"echo hi\"}"}}`
  → `{"type":"tool/result","data":{"message":{"source":{"kind":"tool","callId":"call_00_…"},"content":[{"type":"tool-result","toolCallId":"call_00_…","content":[{"type":"text","text":"hi\n"}]}]}}}`
  → `{"type":"assistant/message","data":{"message":{"content":[{"type":"text","text":"输出如下：…"}]}}}`
  → `{"type":"turn/end","data":{"turn":1,"reason":{"kind":"completed"}}}`
- **`-m` 在 SDK 通道生效**：`initialize` 带 `provider` / `model`；未声明的模型会被
  明确拒绝（`-32603 pi-ai provider "modelverse" has no configured model "…"`）→
  本引擎把「服务端拒绝」判为真失败、**不回退 headless**（回退就是静默换模型）
- **会话续接做不到**（按 id）：同一进程内对同一 `sessionId` 连续 prompt 可以
  续接，但换新进程拿旧 id 会被 `-32603 session "x" already exists` 拒掉 —— 服务端
  `createSession` 只调 `ctx.agents.create`（源码 `lib/index.js`），从不 `resume`；
  协议也只有 `initialize` / `session/prompt` / `shutdown` 三个方法（master 分支的
  `dsh-sdk-protocol` README 同样只列这三个）。harness 核心其实有 `agents.resume`
  （`dsh-agent` 类型声明：「Load a persisted session and resume an agent on it」），
  但没暴露到 SDK 协议 —— 只有 Web/TUI 那条 host API 用得上（`dsh-api-session-controller`
  的 README：「prompt 和文件引用操作可以解析或恢复普通 Session」）。故 `-s <id>` 仍是
  显式报错，错误信息里直接指向下面这条可用路径
- **多轮上下文走常驻会话（真机验收）**：`--stream --keep-alive` + `--append` 实测接得上 ——
  第 1 轮「记住这个数字：42。只回复 OK。」→ `OK`；`--append <run_id> -p "我刚才让你记住的
  数字是多少？只回复数字。"` → `42`（同一 runtime 进程内对同一 `sessionId` 继续 prompt，
  就是官方 Python SDK 文档说的「reuse a harness, home, and id」）。dsh 的常驻**默认关**，
  要显式 `--keep-alive`
- 端到端（真机 `--stream -o json`）：事件序列 `tool_use` → `tool_result` → `thinking`
  → `text` → `turn_end` → `result`（`tools` 字段带 `Name`/`ID`/`Args`/`Result`），
  `model` 报 `modelverse/deepseek-v4.1-flash`，`latency_ms: 6354`；`-o text` 下
  stderr 能看到 `🔧 bash(call_00_…) {"command":"echo …"}` 与 `↳ <工具输出>`
- 单元测试：`internal/agent/dsh_sdk_test.go`（16 个用例：默认走 SDK、事件全解析、
  跨会话事件过滤、initialize 被拒不回退、SDK 不可用回退、maxTokens / -m 透传、
  空正文报错、显式 profile 与 `MAGIC_AGENT_DSH_PROFILE` 覆盖、路由拆分、
  **常驻会话**：同一 sessionId 连发两轮 + 每轮一条 turn_end + 跨轮累加、轮间 idle
  不掐会话、空白追加不排轮、通道不可用时明确报错）；
  假 CLI 是一段 POSIX sh 的 JSON-RPC 循环（带 turn 号递增），端到端覆盖握手、事件解析与多轮

> 口径说明：headless 的**命令构造与解析**（`--profile headless <任务>`、stdout 取正文、
> 退出码 0/1、无 `--model` / 无 `--resume`）依据官方 `apps/cli/reference`；SDK 通道依据
> `@deepseek-ai/dsh-sdk-protocol` 的 `README.zh.md` / `lib/types/types.d.ts`，以及
> `@deepseek-ai/dsh-llm` 的 `ContentBlock`、`@deepseek-ai/dsh-session` 的 `SessionEventMap`
> —— 字段名逐条对齐官方 d.ts，不是从实测样本猜的。

## 平台支持

三档平台覆盖 npm 包内自带的 5 个预编译产物（darwin-arm64 / darwin-x64 / linux-x64 / linux-arm64 / win32-x64），进程组隔离在各平台均生效：

| 平台 | 产物目录 | 进程组实现 |
|------|---------|-----------|
| macOS arm64 / x64 | `npm/dist/darwin-arm64` / `darwin-x64` | `Setpgid` + `kill(-pgid, SIGKILL)` |
| Linux x64 / arm64 | `npm/dist/linux-x64` / `linux-arm64` | 同上 |
| Windows x64 | `npm/dist/win32-x64` | `CREATE_NEW_PROCESS_GROUP` + `taskkill /T /F` |

## License

MIT
