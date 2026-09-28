# 开发与发布

维护者向：打包发布流程、改动落地四步曲、架构与真机验证记录。

## 打包发布（维护者）

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
