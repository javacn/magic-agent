# magic-agent

专业的 agent CLI 代理工具 —— 把 **claude / codebuddy / trae** 三家 CLI 的非交互调用统一成一条命令，提供一致的引擎/模型切换、超时与重试、固定输出格式与稳定退出码。适合脚本化编排与上层工具（如 magic-video）集成。

从 [magic-video](../magic-video) 的 `base/llm/codebuddy.go` / `base/llm/trae.go` / `base/engine.go` 剥离而来，独立演进。

## 安装

```bash
go build -o bin/magic-agent ./cmd/magic-agent
```

依赖：Go 1.26+。三个 CLI 按需安装，未安装的引擎自动探测失败但不影响其他引擎：

| 引擎 | CLI | 探测路径 / 环境变量 |
|------|-----|---------------------|
| claude | Claude Code | `/opt/homebrew/bin/claude` → PATH → `MAGIC_AGENT_CLAUDE_BIN` |
| codebuddy | WorkBuddy 内置 CLI | `WorkBuddy.app/.../cli/bin/codebuddy` → PATH → `MAGIC_AGENT_CODEBUDDY_BIN` |
| trae | trae-cli | `~/.local/bin/trae-cli` → PATH → `MAGIC_AGENT_TRAE_BIN` |

```bash
$ magic-agent engines
ENGINE     STATUS  CLI
claude     ✓       /opt/homebrew/bin/claude
codebuddy  ✓       /Applications/WorkBuddy.app/.../cli/bin/codebuddy
trae       ✓       /Users/you/.local/bin/trae-cli
```

## 用法

```bash
# 基本提问（默认 claude 引擎）
magic-agent ask "用一句话解释什么是熵"

# 切引擎、切模型
magic-agent ask -e codebuddy -m hy3 "写一首俳句"
magic-agent ask -e trae "总结这篇文档"          # trae 用自身配置的默认模型
magic-agent ask -e trae -m My-MiniMax-M3 "..."  # -c model.name= 覆盖

# 超时 + 重试（单次尝试 3 分钟，最多额外重试 2 次，指数退避）
magic-agent ask -e claude -t 3m -r 2 --verbose "复杂的分析任务"

# 系统提示词
magic-agent ask -e claude -s "你是严谨的翻译官，只输出译文" "Hello, world"

# 管道输入
cat doc.md | magic-agent ask -e claude -f - "总结上文"
magic-agent ask -e claude -f context.md "基于这个文件回答：……"

# 固定 JSON 输出（单行 envelope，适合 jq / 程序解析）
magic-agent ask -e claude -o json "1+1=?"
# {"engine":"claude","model":"","session_id":"...","attempts":1,"latency_ms":534,"text":"2"}

# 引擎可用性（支持 --json）
magic-agent engines --json
```

## Flags（ask）

| Flag | 说明 |
|------|------|
| `-e, --engine` | 引擎：`claude` \| `codebuddy` \| `trae`（默认 claude） |
| `-m, --model` | 模型（空 = 引擎默认）。trae 无 `--model`，内部转 `-c model.name=<m>` |
| `-s, --system` | 系统提示词（claude/codebuddy 走 `--append-system-prompt`，trae 拼进 prompt） |
| `-f, --file` | 从文件读 prompt（`-` = stdin）；与位置参数可组合，文件在前 |
| `-t, --timeout` | 单次尝试超时（如 `90s` / `3m`；默认 claude/codebuddy 5m、trae 10m） |
| `-r, --retries` | 失败重试次数（默认 0；总尝试 = 1 + retries） |
| `--backoff` | 首次重试退避（默认 2s，指数翻倍，上限 30s，带抖动） |
| `-o, --output` | `text`（默认）\| `json` |
| `-v, --verbose` | 重试过程打印到 stderr |

prompt 输入优先级：位置参数 > `--file` > stdin 管道（stdin 非 TTY 且无其他输入时自动读）。

## 输出格式

**text**（默认）：stdout 只含模型正文 + 尾换行。

**json**：stdout 单行 envelope：

```json
{"engine":"claude","model":"claude-sonnet-4-6","session_id":"...","attempts":1,"latency_ms":534,"text":"..."}
```

失败时 stdout 为空，stderr 输出错误 envelope，退出码非 0：

```json
{"engine":"codebuddy","attempts":3,"error":"codebuddy: all 3 attempts failed: ..."}
```

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功 |
| 1 | 调用失败（引擎错误 / 超时耗尽 / 重试耗尽） |
| 2 | 参数或输入错误（未知引擎、非法格式、空 prompt 等） |

## 重试语义

- **可重试**：网络类（connection refused/reset、EOF、timeout、signal killed）、限流类（429、rate limit、503 overloaded）、单次尝试超时。
- **快速失败**：参数错、鉴权错、空输出、CLI 明确报错等不可恢复错误——不浪费重试。
- 退避：`backoff * 2^(n-1)` + 抖动，上限 30s；整体 context 取消会立即打断等待。

## 引擎差异说明

| | claude | codebuddy | trae |
|---|---|---|---|
| 非交互模式 | `-p --output-format json` | `--print --output-format json` | `-p`（纯文本） |
| 模型指定 | `--model <m>` | `--model <m>`（hy3 等） | `-c model.name=<m>`（无 --model flag） |
| 默认模型 | CLI 配置 | CLI 配置 | `~/.trae/trae_cli.yaml` 的 `model.name` |
| system 注入 | `--append-system-prompt` | `--append-system-prompt` | 拼进 prompt 头 |
| 工具禁用 | `--tools ""` | `--tools ""` | `--disallowed-tool`（Bash/Edit/… 逐个） |
| 超时联动 | 进程组 kill | 进程组 kill | 另透传 `--query-timeout`（上限 600s） |

所有引擎都以独立**进程组**运行：超时/取消时 `kill(-pgid)` 杀掉整个进程树，CLI 内部 spawn 的 node worker 不会残留（有回归测试保障）。

## 架构

```
cmd/magic-agent/main.go     入口
internal/cli/               cobra 命令层（ask / engines / version、参数校验、退出码）
internal/agent/
  engine.go                 Engine 接口 + Request/Response + 注册表
  prompt.go                 多轮消息扁平化 + noToolSuffix 约束
  claude.go                 Claude Code 引擎
  codebuddy.go              CodeBuddy 引擎（envelope 多形态解析 + 回显剥离）
  trae.go                   Trae 引擎（模型覆盖 + query-timeout 映射）
  runner.go                 超时 + 重试编排（错误分类、指数退避、可取消）
  runcmd.go                 进程组感知执行（超时杀整树，防孤儿）
  output.go                 固定 text/json 输出
tests/                      （预留）跨包集成测试
```

测试：`go test ./...`（fake CLI 脚本，不依赖真实安装；真实引擎冒烟见下方）。

## 已验证（2026-09-15，本机）

- claude 引擎：真实调用成功（text + json + stdin 管道）
- trae 引擎：真实调用成功（默认模型 + query-timeout 映射）
- codebuddy 引擎：CLI 探测/参数构造正确；本环境该 CLI 单次调用 20 分钟不返回（与 magic-video 时代一致），超时 + 进程组清理验证通过（超时后 0 残留进程）

## License

MIT
