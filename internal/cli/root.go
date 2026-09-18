package cli

// root.go - magic-agent CLI 入口与全局 flags。
//
// 命令结构（无子命令，统一根命令直用）：
//
//	magic-agent [flags] [prompt...]   提问（唯一主流程）
//	magic-agent --engines             列出引擎、可用性与各引擎支持的模型（JSON 数组）
//	magic-agent --version             版本信息
//
// 关键 flags：
//
//	-e, --engine <name>      引擎：claude | codebuddy | trae | llm | codex | openclaw | arkclaw（默认 codebuddy）
//	-m, --model <name>       模型（空 = 引擎默认；llm 引擎读 models.json 的 id）
//	-s, --system <prompt>    系统提示词（空 = 用配置文件的 systemPrompt 默认值；值可为文件路径 / @文件）
//	-p, --prompt <text>      提示词（值可为文件路径 / @文件 → 按文件内容用）
//	-f, --file <path>        从文件读 prompt（- 读 stdin）
//	-a, --attach <path>      附件（截图/图片），可重复/逗号分隔；与提示词一起发给引擎
//	-w, --workspace <dir>    工作目录：在该目录里执行引擎（详见下方「工作目录」）
//	-t, --timeout <dur>      单次尝试超时（如 3m；默认 600s）
//	-r, --retries <n>        失败重试次数（默认 0）
//	    --backoff <dur>      首次重试退避（默认 2s，指数翻倍，上限 30s）
//	-o, --output <format>    输出：json（默认）| text
//	    --tools <mode>       off（默认）| on | 逗号分隔白名单
//	    --permission <tier>  四档权限档位：manual | accept-edits | auto | full
//	                         （默认 full = 保持既有行为；仅 claude/codebuddy 生效）
//	    --sandbox-exclude   始终在沙箱外执行的命令（docker,watchman…）
//	    --sandbox-domain    沙箱网络白名单域名
//	    --auto-mode-env     第 3 档分类器的受信边界（自然语言）
//	    --permission-deny   追加 deny 规则（所有档位含 full 都生效）
//	    --permission-ask    追加 ask 规则（命中即强制人工审批）
//	    --max-tokens <n>     输出 token 上限（claude/codebuddy 经 --settings 注入，llm 透传 -o，trae 忽略）
//	    --temperature <t>    采样温度（仅 llm 引擎透传 -o temperature，其余忽略）
//	--engines            列出引擎、可用性与支持的模型（JSON 数组；--json 兼容保留）
//	--no-models          配合 --engines：跳过模型探测（只列引擎，快）
//	--stop <id>          停止指定会话/运行（session_id 或 run_id），杀掉它的引擎进程组
//	--sessions           列出会话登记表（JSON 数组：run_id / session_id / pid / engine / state）
//	--keep-alive         常驻会话（默认开，仅 claude/codebuddy 的 --stream 调用生效）：
//	                     首轮结束后不退出、等 --append 追加；--keep-alive=false 关闭
//	--append <id>        向常驻会话追加一条消息（内容用 -p/位置参数给）
//	--idle <dur>         常驻会话空闲收工时长（默认 5m；0 = 本轮结束就收工）
//	-v, --verbose            重试过程打到 stderr
//
// 退出码：0 成功；1 调用失败；2 参数/输入错误。
//
// 工作目录（-w/--workspace <dir>）：让引擎在指定目录里干活。**尽量用各 CLI 的原生能力**：
//
//	codex               原生 `-C/--cd <dir>`（working root）+ 子进程 cwd
//	claude/codebuddy    无工作目录 flag（--add-dir 只追加额外可访问目录）→ 子进程 cwd
//	trae                同上（有 --add-dir）
//	openclaw            **不支持**（workspace 与 agent 绑定；实测子进程 cwd 被忽略）
//	llm / arkclaw       **不支持**（无文件系统语义：llm 是 chat CLI；arkclaw 由网关按 claw_id 绑定）
//	                    → 参数被忽略，且会打一行 stderr 提示，不静默
//
// 目录必须存在且是目录（否则 exit 2 报错）。机器可读的能力表见 `--engines` 的 workspace 字段
//（flag:-C / cwd / none）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/darren/magic-agent/internal/agent"
)

// Version 版本号。
var Version = "0.1.0"

// NewRootCommand 构建 CLI 根命令（无子命令，全部走根命令 flags）。
//
//	.magic-agent -p "问题"            （-p/--prompt 提示词）
//	magic-agent "问题"                （位置参数同样有效）
//	magic-agent -e codebuddy -m hy3 "问题"
//	magic-agent --engines             （列引擎，JSON 数组输出；--json 兼容）
//
// 注意 -p 在 claude 语境里是 --print，这里统一让位给 --prompt。
func NewRootCommand() *cobra.Command {
	opts := newAskOptions()
	root := &cobra.Command{
		Use:     "magic-agent [flags] [prompt...]",
		Short:   "专业的 agent CLI 代理工具 - 统一 claude / codebuddy / trae / llm / codex / openclaw / arkclaw 引擎",
		Version: Version,
		Long: `magic-agent - agent CLI 代理工具

把 claude / codebuddy（WorkBuddy）/ trae 等 CLI 与 openclaw / arkclaw
等 agent 后端的非交互调用统一成一条命令：支持切换引擎与模型、超时与
重试、固定 text/json 输出格式、稳定退出码。适合脚本化编排与上层工具集成。

引擎：
  claude     Claude Code CLI（-p --output-format json）
  codebuddy  CodeBuddy / WorkBuddy 内置 CLI（默认 hy3，可切 glm-5.3 等）
  trae       Trae CLI（使用 trae 自身配置的默认模型）
  llm        simonw/LLM CLI（模型与密钥由 llm models / llm keys 自管）
  codex      Codex CLI（使用 codex 自身配置的模型与凭据）
  openclaw   OpenClaw CLI（本机 openclaw 可执行文件）
  arkclaw    ArkClaw A2A 网关（HTTP JSON-RPC message/send；凭据读配置文件）

示例：
  magic-agent -p "用一句话解释什么是熵"            # 直接提问（默认 json 输出）
  magic-agent "问题"                               # 位置参数等价
  magic-agent -e codebuddy "写一首俳句"            # codebuddy 默认 hy3
  magic-agent -e codebuddy -m glm-5.3 "写一首俳句"
  magic-agent -e trae -t 10m "总结这篇文档"        # 默认关工具；--tools on 可开
  magic-agent -e claude --tools Bash,Read "看看这个目录"  # 工具白名单
  cat doc.md | magic-agent -e claude -f - "总结上文"
  magic-agent -e claude -p ./prompt.md "补充一句"        # 提示词给文件路径 → 读文件内容
  magic-agent -e claude -p @notes/task.md               # @ 强制按文件读（读不到直接报错）
  magic-agent -e claude -s ./reviewer.md "审一下这段"     # 系统提示词也可给文件
  magic-agent -e claude -r 2 "1+1=?"               # 失败重试 2 次
  magic-agent --session <id> "接着刚才继续"         # 会话续接（id 取上次输出 session_id）
  magic-agent -c "接着刚才继续"                     # 续接最近一次会话（免传 id）
  magic-agent -e llm -m minimax-m3 --max-tokens 32000 "写一集剧本"
  magic-agent --engines                           # 列出引擎、可用性与各引擎支持的模型（JSON 数组）
  magic-agent --engines --no-models               # 只列引擎与可用性（跳过模型探测，不启动 CLI）
  magic-agent --engines --json                    # 等价（--json 为兼容保留）

四档权限模型（--permission，仅 claude / codebuddy 生效）：
  把「哪些动作自动放行 + 放行不了时由谁裁决」收敛成四档，映射到 Claude Code 的参数：

    manual        第1档 沙箱开启，只读放行，其余逐项由「用户」确认
                        → --permission-mode default
    accept-edits  第2档 沙箱开启，工作区内编辑放行，命令仍由「用户」确认
                        → --permission-mode acceptEdits
    auto          第3档 沙箱开启，沙箱内放行，越界交「LLM Guardian」判定
                        → --permission-mode auto
    full          第4档 沙箱关闭，命令直接在宿主机执行，无审批
                        → --permission-mode bypassPermissions

  **默认是 full**（保持既有行为：改造前 claude/codebuddy 开工具时恒传
  --dangerously-skip-permissions / -y，语义正是第 4 档）。做 agent 任务时
  推荐显式传 --permission auto —— 沙箱 + LLM 判定，安全与效率兼顾。
  沙箱配置没有 CLI flag（claude 无 --sandbox），统一经 --settings 注入；
  MaxTokens 的 env 注入与它在同一份 JSON 里合并（--settings 不是可重复 flag）。
  第 1~3 档默认写 failIfUnavailable=true（沙箱起不来就报错，不静默降级）与
  allowUnsandboxedCommands=false（关闭逃逸舱口）；需要放行的命令用 --sandbox-exclude 列出。

    magic-agent -e claude --tools on --permission auto -p "重构这个函数"
    magic-agent -e claude --tools on --permission accept-edits -p "改注释"
    magic-agent -e claude --tools on --permission manual -p "先看看再动手"
    magic-agent -e claude --tools on --permission auto \
      --auto-mode-env "Source control: github.example.com/acme-corp" \
      --permission-ask 'Bash(git push *)' -p "提交并推送"    # 推送前强制人工检查点
    magic-agent -e claude --tools on --permission full \
      --permission-deny 'Bash(rm -rf *)' -p "清理临时文件"    # 全权但保留硬红线
  说明：--permission 传给未接线的引擎（trae / llm / codex / openclaw / arkclaw）会
  exit 2 明确报错，不会静默忽略 —— 静默忽略一个安全设置，等于让用户以为自己被保护着。
  机器可读的能力表见 --engines 每行的 permission 字段。

llm 引擎（包装 simonw/LLM CLI，模型/密钥由它自管）：
  magic-agent -e llm -m minimax-m3 "问题"        # 按 llm CLI 注册名指定
  magic-agent -e llm --max-tokens 32000 "问题"   # 透传 -o max_tokens
  magic-agent -e llm --session <id> "接着说"     # --session → --cid 续接（id 取上轮输出 session_id）
  magic-agent -e llm "问题"                      # 用 llm 的默认模型
  magic-agent --engines                          # 看 llm 已注册的模型清单

模型清单（--engines 的 models 字段，一律不硬编码）：
  每个可用引擎的模型清单都从它自己的权威入口现取现算，CLI 升级/新注册模型后自动跟随：
    claude    ~/.claude/settings.json（顶层 model + env 里 ANTHROPIC_*MODEL[*_NAME]）
    codebuddy codebuddy --help 里 --model 自带的 "Currently supported: (...)" 清单
    trae      trae-cli models --json
    llm       llm models
    codex     codex debug models
    openclaw  openclaw models list --json
    arkclaw   无清单（模型由网关按 claw_id 绑定）→ 输出 models_note 说明
  探测会真的启动这些 CLI 的只读子命令（最慢 openclaw ~2.5s），故并发执行、
  单引擎上限 30s；拿不到清单不报错，原因写进该行的 models_note。
  --no-models 整段跳过探测（只列引擎与可用性，不启动任何 CLI）。

arkclaw 引擎（A2A JSON-RPC 网关，不走本机 CLI；凭据放配置文件）：
  配置文件 ~/.config/magic-agent/config.json（路径可用 MAGIC_AGENT_CONFIG 覆盖）：
    {
      "systemPrompt": "你是一个中文助手，始终用中文回答所有问题。",
      "arkclaw": {
        "url": "https://<host>/a2a/jsonrpc",
        "key": "<apikey>",
        "claw_id": "ci-xxxxxxxx"
      }
    }
  环境变量可覆盖文件值：MAGIC_AGENT_ARKCLAW_URL / MAGIC_AGENT_ARKCLAW_KEY / MAGIC_AGENT_ARKCLAW_CLAW_ID
  magic-agent -e arkclaw "问题"                  # 单轮 message/send
  magic-agent -e arkclaw --session <ctx> "接着说"  # 按 contextId 续接同一上下文
  magic-agent --engines                          # 看 arkclaw 是否已配置齐备

  说明：不支持 --stream（A2A 流式是独立的 message/stream，本项目只接 message/send）；
        不支持 -c/--continue（A2A 无「查询最近上下文」接口，请显式传 --session）；
        --tools / --max-tokens / --temperature 由网关侧决定，静默忽略；
        --json-schema 支持（走与 llm 相同的输出后处理抽 JSON）。

常驻会话与追加需求（--keep-alive 默认开 / --append，仅 claude/codebuddy 的 --stream 调用）：
  引擎 CLI 的 stream-json 输入可以在**同一个进程**里持续收 user 消息，所以：
    magic-agent -e claude --stream -p "先把仓库跑一遍测试" -o text          # 默认就是常驻会话
    magic-agent --append <run_id|session_id> -p "追加：顺便把 lint 也跑了"   # 任务跑着时随时追加
    magic-agent -e claude --stream --keep-alive=false -p "..."              # 关掉常驻（turn 结束即退出）
  常驻会话会开一个 unix socket 追加入口（路径写在会话登记表的 append 字段里，权限 0600），
  --append 连上去把消息交给它，再由它写进引擎 stdin 成为下一轮 user 消息。
  追加进来的轮次照常以事件流输出（每轮收尾发一个 turn_end 事件，便于调用方知道「这轮完了」）。
  收工：最后一轮结束后 --idle（默认 5m）内没人追加 → 优雅退出（stdin EOF）；--idle 0 = 本轮结束就收工
  （追加窗口只在任务运行期间）；也可随时 --stop。
  常驻生效期间不套 -t 默认超时（长任务+追加不受 600s 限制），显式给 -t 才套。
  不生效的形态会自动忽略（非 --stream 调用、不支持追加的引擎）；显式传 --keep-alive 却
  不满足条件则明确报错（exit 2）。Windows 无 unix socket，暂不支持。
  注意：默认常驻后进程会在最后一轮结束后继续存活 --idle 那么久 —— 调用方若要「一轮完成」
  的信号，请读事件流里的 turn_end（而不是等进程退出），或传 --keep-alive=false。

停止指定会话（--stop / --sessions）：
  每次调用都会往会话登记表落一条记录（~/.magic-agent/sessions/<run_id>.json，MAGIC_AGENT_SESSIONS 可改目录），
  里面记着**引擎子进程的 pid** 与引擎/模型/工作目录/会话 id。为什么需要它：
    · 引擎 CLI 在独立进程组里跑（Setpgid），调用方 kill 掉 magic-agent 的进程组**带不走它**；
    · 进程句柄只活在调用方内存里，应用重启后之前 detach 出去的会话就再也停不掉。
  有了记录，任何进程都能按会话停：
    magic-agent --sessions                        # 列出全部（run_id / session_id / pid / engine / state）
    magic-agent --stop <session_id>               # 按会话 id 停（续接场景最常用）
    magic-agent --stop <run_id>                   # 按运行 id 停（新会话还没拿到 session_id 时）
  行为：先 SIGTERM，2s 内不退再 SIGKILL（连它的 node worker 一起带走），然后回写 state=stopped。
  幂等：进程已不在 / 会话已结束 → stopped=false + reason，退出码仍是 0；id 不存在 → 退出码 1（多半写错了）。
  另：magic-agent 自己收到 SIGINT/SIGTERM 时也会先带走自己的引擎子进程再退出，不留孤儿。

附件（-a/--attach：文件＋提示词，比如截图加提示词）：
  每个引擎尽量用**自己的原生方式**收附件，能力见 --engines 的 attachments 字段：
    codex      flag:-i            原生 -i/--image（图片；resume 轮退化为提示词里的路径）
    claude     stdin:stream-json  原生 --input-format stream-json，图片作为 content block 走 stdin
    codebuddy  stdin:stream-json  同上（同族 CLI）
    llm        flag:-a            直连走 OpenAI content parts（image_url data URL）；
                                  委托 llm CLI 时走原生 -a/--attachment
    arkclaw    part:file          A2A 原生 file part（base64 inline；远端看不到本机路径）
    trae       prompt             无原生通道 → 把**绝对路径**写进提示词（需要 --tools 非 off）
    openclaw   prompt             同上
  路径在 CLI 层校验（存在 + 普通文件 + 32MB 上限），出错 exit 2；附件与提示词是并列关系，
  提示词本身照旧（不会因为给了附件就被改写）。图片按**魔数**识别（不信扩展名），
  非图片附件在没有原生通道的引擎上同样退化为路径。
    magic-agent -p "这张图什么颜色" -a ~/Desktop/shot.png
    magic-agent -p "对比这两张图" -a a.png -a b.png
    magic-agent -p "看截图报错" --tools on -a shot.png -o text

提示词可以是文件路径（-p / 位置参数 / -s 都适用）：
  值恰好命中一个**已存在的普通文件** → 按文件内容作为提示词，并打一行 stderr 提示（不静默）；
  写 "@<path>" 则**强制**按文件读（读不到 / 不是普通文件 → exit 2 报错）。
  自动识别很保守：单行、长度 ≤ 4096 字节、stat 出来是普通文件（目录不算），
  所以 -p "解释一下 README.md"、多行提示词都不受影响；-f/--file 仍是不加猜测的显式写法。
    magic-agent -p ./prompt.md "补充一句"      # 文件内容在前，追加内容在后
    magic-agent ./tasks/task.md               # 位置参数同样支持
    magic-agent -p @notes/task.md             # 强制按文件读（不存在就报错）
    magic-agent -s ./reviewer.md "审一下"      # 系统提示词也一样（含配置里的 systemPrompt）

默认系统提示词（同一份配置文件，对所有引擎生效）：
  "systemPrompt" 是**全局默认**：调用时没给 -s/--system 就用它，给了则 -s 优先。
  键名宽松兼容（systemPrompt / system_prompt / system 任选），取值可用环境变量
  MAGIC_AGENT_SYSTEM_PROMPT 临时覆盖；未配置 → 不注入，行为与以前完全一致。
    magic-agent "1+1=?"                      # 用配置里的默认 systemPrompt
    magic-agent -s "只输出译文" "Hello"       # -s 优先，默认值本轮不生效`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          rejectRemovedSubcommands,
		// 禁用 cobra 内置 completion 子命令：本 CLI 无子命令形态。
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case opts.listSessions:
				return runSessions(cmd, opts)
			case flagChanged(cmd, "stop"):
				// 用「传过没有」而不是「值非空」判断：`--stop ""` 也要走参数校验
				//（exit 2 提示要 id），而不是掉进「提问」分支报 empty prompt。
				return runStop(cmd, opts)
			case flagChanged(cmd, "append"):
				return runAppend(cmd, args, opts)
			case opts.engines:
				return runEngines(cmd, opts)
			case opts.stream:
				return runStreamAsk(cmd, args, opts)
			}
			return runAsk(cmd, args, opts)
		},
	}
	bindAskFlags(root, opts)
	// 无子命令设计：禁用 cobra 自动注入的 completion 命令，
	// 保证 "magic-agent completion" 也只是被当作 prompt 而非隐藏子命令。
	root.CompletionOptions.DisableDefaultCmd = true
	// --version 输出固定 "magic-agent <ver>"，不带 cobra 默认前缀。
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	return root
}

// Execute 运行 CLI，返回进程退出码。
//
// 错误输出约定（stderr，与 -o 联动）：
//   - 引擎执行失败：runAsk 已用 WriteError 输出（reportedError 标记），此处跳过
//   - 其余错误（参数错 / 引擎预检失败 / 子命令形态）：此处统一输出 ——
//     -o json 时输出同结构 envelope（attempts=0，engine 为 -e 原值），
//     text 时一行 "magic-agent: <err>"。stdout 恒不产生半截内容。
func Execute() int {
	// 被 SIGINT/SIGTERM 终止时先带走引擎子进程（它在自己的进程组里，调用方杀不到）。
	installSignalStop()
	root := NewRootCommand()
	err := root.Execute()
	if err == nil {
		return exitOK
	}
	// 已由 WriteError 格式化输出过（json envelope / text 行）。
	if errors.Is(err, errReportedSentinel) {
		return exitCodeOf(err)
	}

	// 按 -o 的实际生效值（含默认）决定 stderr 格式。
	format := agent.FormatJSON
	if f := root.PersistentFlags().Lookup("output"); f != nil && f.Value.String() != "" {
		if v, perr := agent.ParseFormat(f.Value.String()); perr == nil {
			format = v
		}
	}
	engineName := ""
	if f := root.PersistentFlags().Lookup("engine"); f != nil {
		engineName = f.Value.String()
	}
	// 参数类错误尚未执行任何尝试；attempts=0 便于调用方区分阶段。
	_ = agent.WriteError(os.Stderr, format, engineName, 0, err)
	return exitCodeOf(err)
}

// errReportedSentinel reportedError 的内部标记（errors.Is 用）。
var errReportedSentinel = errors.New("already reported")

// reportedError 包装"已输出过失败信息"的错误：
// WriteError 已把 envelope/文本写到 stderr，Execute 不再打印第二遍。
type reportedError struct{ err error }

func (r *reportedError) Error() string { return r.err.Error() }

// Is 让 errors.Is(err, errReportedSentinel) 命中。
func (r *reportedError) Is(target error) bool { return target == errReportedSentinel }

// Unwrap 保留退出码推断（usageError 判定）沿链下钻。
func (r *reportedError) Unwrap() error { return r.err }

// 退出码约定。
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// usageError 标记参数/输入错误（exit 2）。
type usageError struct{ err error }

func (u *usageError) Error() string { return u.err.Error() }

// removedSubcommands 已删除的子命令名（v0.1 曾有 ask/engines/version
// 子命令形态；现统一为根命令 flags）。出现在首位参数时直接拒绝，
// 避免被误当成 prompt 发给引擎。
var removedSubcommands = map[string]string{
	"ask":        "--prompt / 位置参数",
	"engines":    "--engines",
	"version":    "--version",
	"completion": "(no shell completion)",
}

// rejectRemovedSubcommands 首个位置参数命中已删子命令名时报 usage 错误。
func rejectRemovedSubcommands(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		if hint, ok := removedSubcommands[strings.ToLower(args[0])]; ok {
			return &usageError{fmt.Errorf("unknown command %q: subcommands were removed, use %s instead", args[0], hint)}
		}
	}
	return cobra.ArbitraryArgs(cmd, args)
}

// exitCodeOf 从错误推断退出码。
func exitCodeOf(err error) int {
	if _, ok := err.(*usageError); ok {
		return exitUsage
	}
	return exitFail
}

// probeEngineModels 单引擎模型探测的入口。
//
// 提为包级变量只为测试替换：单元测试不应该真的去启动用户机器上的 CLI
// （各引擎探测命令的正确性在 internal/agent 用假 CLI 覆盖）。
// 生产路径就是 lister.ListModels(ctx)。
var probeEngineModels = func(ctx context.Context, l agent.ModelLister) ([]string, error) {
	return l.ListModels(ctx)
}

// flagChanged 报告某个 flag 是否被显式传过（用于「传了就走这条分支」的开关型参数）。
func flagChanged(cmd *cobra.Command, name string) bool {
	f := cmd.Flags().Lookup(name)
	return f != nil && f.Changed
}

// runEngines 列出引擎、CLI 探测结果与各引擎当前支持的模型（原 engines 子命令的 flag 形态）。
//
// 固定输出单行 JSON 数组（jq 友好），每项：
//
//	engine       引擎名
//	ok           引擎是否可用
//	bin          可用时给二进制路径（无本机 CLI 的引擎给端点 URL）
//	note         不可用原因
//	models       该引擎当前支持的模型标识（动态探测，见 agent.ModelLister）
//	models_note  拿不到 models 时的原因（无动态来源 / 探测失败）
//	workspace    该引擎「指定工作目录」的落地方式：flag:-C（codex 原生）/ cwd（子进程 cwd）/
//	             none（llm、arkclaw、openclaw 不支持，-w 会被忽略）
//	streaming    该引擎是否支持 `--stream`（false = 调用方应改用非流式；codex/openclaw/arkclaw）
//
// 模型**一律现取现算、不硬编码**：各引擎用自己 CLI/配置文件里的权威入口
// （trae-cli models / llm models / codex debug models / openclaw models list /
// codebuddy --help / claude settings.json）。探测要真的启动 CLI（只读子命令），
// 故并发执行并套 agent.DefaultModelProbeTimeout；--no-models 可整段跳过（只列引擎，快）。
// --json 为兼容旧调用保留（本函数不再区分，恒为 JSON）。
func runEngines(cmd *cobra.Command, opts *askOptions) error {
	engines := agent.Engines()
	type row struct {
		Engine     string   `json:"engine"`
		OK         bool     `json:"ok"`
		Bin        string   `json:"bin"`
		Note       string   `json:"note,omitempty"`
		Models     []string `json:"models,omitempty"`
		ModelsNote string   `json:"models_note,omitempty"`
		// Workspace 该引擎「指定工作目录」的落地方式（machine-readable，供调用方决定是否下发）：
		//   flag:-C 原生 flag（codex）；cwd 子进程 cwd（claude/codebuddy/trae）；
		//   none    无文件系统语义（llm/arkclaw，参数会被忽略）
		Workspace string `json:"workspace,omitempty"`
		// Streaming 是否实现 Streamer（即 `--stream` 可用）。false 时调用方应改用非流式
		//（观物台的 desk:ask 据此决定要不要加 --stream；codex/openclaw/arkclaw 为 false）。
		Streaming bool `json:"streaming"`
		// Attachments 该引擎「收附件（截图等）」的落地方式（machine-readable，供调用方决定
		// 是否下发 -a/--attach，以及要不要提示用户降级）：
		//   flag:-i           codex 原生 --image
		//   stdin:stream-json claude / codebuddy 原生 stream-json input（图片走 content block）
		//   flag:-a           llm CLI 原生 --attachment（直连模型走 image_url content parts）
		//   part:file         arkclaw 走 A2A 原生 file part（base64 inline）
		//   prompt            无原生通道：把绝对路径写进提示词，靠引擎读文件（trae/openclaw）
		Attachments string `json:"attachments,omitempty"`
		// Append 是否支持「常驻会话 + 追加消息」（`--stream --keep-alive` 与 `--append`）。
		// 只有 claude / codebuddy 为 true（stream-json 输入可持续喂 user 消息）。
		Append bool `json:"append"`
		// Ask 该引擎「需要用户选择」的落地方式（machine-readable，见 agent.AskSupportOf）：
		//   tool:AskUserQuestion 有该工具 + can_use_tool 协议（claude / codebuddy）
		//   none                 无该能力（其余引擎）
		// 调用方据此决定要不要处理 `--stream` 输出里的 `{"type":"ask"}` 事件。
		Ask string `json:"ask,omitempty"`
		// Permission 该引擎对四档权限模型（manual / accept-edits / auto / full）的
		// 落地方式（machine-readable，见 agent.PermissionSupportOf）：
		//   flag:--permission-mode  claude / codebuddy 有原生 --permission-mode
		//   none                    其余引擎未接线 —— 传 --permission 会 exit 2 报错
		// 调用方据此决定是否下发 --permission 与那几个沙箱/规则类 flags。
		Permission string `json:"permission,omitempty"`
	}
	rows := make([]row, len(engines))
	var wg sync.WaitGroup
	for i, e := range engines {
		ok, note := e.Detect()
		r := row{Engine: e.Name(), OK: ok, Note: note,
			Workspace:   agent.WorkspaceSupportOf(e.Name()),
			Streaming:   agent.AsStreamer(e) != nil,
			Attachments: agent.AttachmentSupportOf(e.Name()),
			Append:      agent.AppendSupportOf(e.Name()),
			Ask:         agent.AskSupportOf(e.Name()),
			Permission:  agent.PermissionSupportOf(e.Name())}
		if ok {
			r.Bin, r.Note = note, ""
		}
		rows[i] = r
		if !ok || opts.noModels {
			continue
		}
		lister := agent.ModelListerOf(e)
		if lister == nil {
			rows[i].ModelsNote = "no dynamic model source"
			continue
		}
		wg.Add(1)
		go func(i int, l agent.ModelLister) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(cmd.Context(), agent.DefaultModelProbeTimeout)
			defer cancel()
			models, err := probeEngineModels(ctx, lister)
			switch {
			case err != nil:
				// 失败不致命：原因进 models_note，models 留空。
				rows[i].ModelsNote = err.Error()
			case len(models) == 0:
				rows[i].ModelsNote = "no models reported"
			default:
				rows[i].Models = models
			}
		}(i, lister)
	}
	wg.Wait()
	return printJSON(cmd.OutOrStdout(), rows)
}
