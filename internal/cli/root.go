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
//	-e, --engine <name>      引擎：claude | codebuddy | codebuddy-ai | codebuddy-gateway | trae | llm | codex | openclaw | dsh | arkclaw（默认 codebuddy）
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
//	--keep-alive         常驻会话（claude/codebuddy 默认开，dsh 需显式传；需配合 --stream）：
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
//	dsh                 同上（dsh 官方语义就是「调用时所在目录即默认 workspace 根」）
//	openclaw            **不支持**（workspace 与 agent 绑定；实测子进程 cwd 被忽略）
//	llm / arkclaw       **不支持**（无文件系统语义：llm 是 chat CLI；arkclaw 由网关按 claw_id 绑定）
//	codebuddy-gateway   **不支持**（网关在它自己的 cwd 里跑 agent，协议里没有工作目录字段）
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
		Short:   "专业的 agent CLI 代理工具 - 统一 claude / codebuddy / trae / llm / codex / openclaw / dsh / arkclaw 引擎",
		Version: Version,
		Long: `magic-agent - agent CLI 代理工具

把 claude / codebuddy / trae 等 CLI 与 openclaw / dsh / arkclaw
等 agent 后端的非交互调用统一成一条命令：支持切换引擎与模型、超时与
重试、固定 text/json 输出格式、稳定退出码。适合脚本化编排与上层工具集成。

引擎：
  claude     Claude Code CLI（-p --output-format json）
  codebuddy  CodeBuddy Code CLI（**独立安装**：npm i -g @tencent-ai/codebuddy-code；默认 hy3，可切 glm-5.3 等）
  codebuddy-ai 同一个 CodeBuddy Code CLI 的**第二个账号**（默认不传 --model，交给 CLI 自选）
             两个引擎跑同一个二进制，靠 authentication.id 分开账号，互不顶号；
             登录：magic-agent --login codebuddy / --login codebuddy-ai
  trae       Trae CLI（使用 trae 自身配置的默认模型）
  llm        simonw/LLM CLI（模型与密钥由 llm models / llm keys 自管）
  codex      Codex CLI（使用 codex 自身配置的模型与凭据）
  openclaw   OpenClaw CLI（本机 openclaw 可执行文件）
  dsh        DeepSeek Harness（dsh --profile headless "<任务>"；模型在 dsh 自己的配置里）
  arkclaw    ArkClaw A2A 网关（HTTP JSON-RPC message/send；凭据读配置文件）
  codebuddy-gateway
             CodeBuddy Code HTTP 网关（把 prompt 投给已在跑的 codebuddy --serve /
             /gateway 远程控制，走 webhook 投递 + SSE 收流；凭据读配置文件，见下）

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
  magic-agent -e dsh "把 tests 跑一遍并总结失败原因"   # DeepSeek Harness（headless）
  magic-agent -e dsh -w ~/proj "重构这个模块"          # dsh 的 workspace = 子进程 cwd
  magic-agent --engines                           # 列出引擎、可用性与各引擎支持的模型（JSON 数组）
  magic-agent --engines --no-models               # 只列引擎与可用性（跳过模型探测，不启动 CLI）
  magic-agent --engines --json                    # 等价（--json 为兼容保留）

四档权限模型（--permission，仅 claude / codebuddy / codebuddy-ai 生效）：
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
  说明：--permission 传给未接线的引擎（trae / llm / codex / openclaw / dsh / arkclaw）会
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
    dsh       $DSH_HOME/settings.yaml 里已配置的模型（llm-pi-ai.providers.<route>.models，
              输出 route/model）；headless 无 --model，换模型要改该文件
  探测会真的启动这些 CLI 的只读子命令（最慢 openclaw ~2.5s），故并发执行、
  单引擎上限 30s；拿不到清单不报错，原因写进该行的 models_note。
  --no-models 整段跳过探测（只列引擎与可用性，不启动任何 CLI）。

引擎不可用时给「一键安装命令」（--engines 的 install 字段，shell 一行，可直接执行）：
  claude    npm install -g @anthropic-ai/claude-code
  codex     npm install -g @openai/codex
  openclaw  npm install -g openclaw@latest
  dsh       npm i -g @deepseek-ai/dsh
  trae      sh -c "$(curl -L https://trae.cn/trae-cli/install.sh)"
  llm       python3 -m venv ~/.llm-venv && ~/.llm-venv/bin/pip install llm
  命令一律取自各 CLI 的官方安装方式；**没有可执行安装路径的引擎不给该字段**
  （codebuddy / codebuddy-ai 是桌面端 GUI 应用，arkclaw 靠配置文件），
  它们的「怎么才能用」写在同行的 note 里。
    magic-agent --engines --no-models | jq -r '.[] | select(.ok|not) | .install'

桌面 / 移动客户端（magic-client 插件）的契约面（定义见 internal/agent/capability.go）：
  --contract 输出带版本的 envelope：{"contractVersion":N,"engines":[...]}，
  engines 与 --engines 的行**完全同构**，且每行多一个 capabilities 字段
  （静态能力 id，字典序）。capabilities 回答「能做什么」，ok / streaming /
  models 回答「此刻如何」：客户端按前者定界面形态，按后者决定按钮可用与降级提示。
  ⚠️ --contract 默认**不探测模型**（快）；要连模型一起拿写 --no-models=false。
    magic-agent --contract                                # 契约版本 + 各引擎能力（快）
    magic-agent --contract | jq '.engines[] | {engine, capabilities}'
    magic-agent --contract | jq -r '.contractVersion'     # 客户端启动时先校验这个

流式事件与控制通道（桌面 / 移动客户端据此驱动界面与回传动作）：
  --stream             正文 / 思考增量逐行 NDJSON（老形状，逐字节不变，老消费者继续用）
  --stream --events    同上，但每行多 v / seq，并先发一行 ready —— **客户端消费形态**
  --stream --control   从 stdin 读 NDJSON 控制命令，让调用方能打断与回审批：
    {"op":"ping"}                        探活 → pong
    {"op":"interrupt"}                   打断当前轮 → 本轮真的结束再发 interrupted
    {"op":"stop"}                        收工（关常驻会话）→ stopped
    {"op":"answer","text":"<答案>"}       把答案作为下一轮 user 消息追进常驻会话 → answered
                                         ⚠️ headless 下原地作答模型拿不到答案（见 agent.KindAsk）
                                         ⚠️ 依赖常驻会话生效；不可用时回一条 control_error
    magic-agent --stream --events --control -p "跑一遍测试" < cmds.ndjson
    echo '{"op":"interrupt"}' | magic-agent --stream --events --control -p "长任务"

dsh 引擎（DeepSeek Harness，本机 dsh CLI）：
  默认走 SDK 通道：dsh --profile sdk（stdio + 换行分帧 JSON-RPC，实时收会话事件）；
  该 profile 不可用时自动回退 headless（dsh --profile headless "<任务>"，stdout 只给最终正文）。
  magic-agent -e dsh "把测试跑一遍并总结失败原因"
  magic-agent -e dsh -w ~/proj "重构这个模块"      # workspace = 子进程 cwd（dsh 原生语义）
  magic-agent -e dsh -a shot.png "看截图报错"      # 附件无原生通道 → 路径写进提示词
  magic-agent -e dsh --stream "看看这个仓库"        # 推理 / 正文 / 工具调用都实时出
  MAGIC_AGENT_DSH_PROFILE=headless magic-agent -e dsh "..."   # 强制回退通道（排障用）
  magic-agent --engines                            # 看 dsh 是否已装（bin 探测）
  说明：
    · 模型：SDK 通道下 -m 生效（转成 initialize 的 provider/model，取 route/model 形态，
      可用值见 --engines 的 models 字段）；回退 headless 后 -m 不生效（无 --model 参数），
      模型由 dsh 自己的配置决定（$DSH_HOME/settings.yaml 的 agent-default-model 段 +
      llm-pi-ai.providers，密钥放 $DSH_HOME/.credentials.yaml 或 .env），此时会打一行告警。
    · 工具调用只在 SDK 通道上有：headless 的 stdout/stderr 里都没有工具事件（官方定位就是
      「推理走 stderr、正文走 stdout、然后退出」）→ 要工具事件请确保走 SDK 通道。
    · --stream：SDK 通道下推理 → thinking、正文 → 逐 step text、工具 → tool_use/tool_result、
      收尾一条 turn_end；回退 headless 后只流推理（stderr 的 "dsh: reasoning:" 增量），
      正文在 turn 结束时一次性给出。
    · 多轮上下文：用**常驻会话**（SDK 通道下同一进程内对同一会话继续 prompt，官方推荐的续接方式）：
        magic-agent -e dsh --stream --keep-alive "记住 42"        # 首轮（dsh 的常驻默认关，要显式传）
        magic-agent --append <run_id|session_id> -p "刚才那个数是多少"  # 追问，上下文接得上
      不支持按 id 续接（-s/--session）：headless 每次调用都是全新会话；SDK 的 sessionId
      只在**同一个 runtime 进程内**可续，换进程拿旧 id 会被 -32603 "session already exists"
      拒掉（协议只有 initialize / session/prompt / shutdown，没有 resume 方法）。
    · --permission 未接线（dsh 的权限预设是配置层概念，无法表达四档里的「沙箱关闭」档）。
    · --tools 由 dsh 自己的工具循环决定（无命令行级逐工具开关），传入即忽略。
    · --json-schema 支持（走与 llm/openclaw 相同的输出后处理抽 JSON）。
    · CLI 路径解析：MAGIC_AGENT_DSH_BIN → 常见安装位 → PATH → npm 全局前缀
      （npm i -g @deepseek-ai/dsh；最后一跳读 NPM_CONFIG_PREFIX，专治「装好了却报 not found」）。

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
  magic-agent -e arkclaw --stream "问题"          # 流式（A2A SSE；见下）
  magic-agent --engines                          # 看 arkclaw 是否已配置齐备

  说明：--stream 走 A2A 官方的 message/stream（SSE），但**网关不吐逐字增量** —— 实测只有
        「受理帧 + 整段正文」两帧，正文仍一次性到达（要逐字得 claw 侧改）；网关没按 SSE 回时
        自动退回一次性响应。不要改成 WebSocket：实测网关不回 101 升级响应，且 A2A 规范里
        WS 属自定义绑定、火山网关也没有 WS API 类型。
        不支持 -c/--continue（A2A 无「查询最近上下文」接口，请显式传 --session）；
        --tools / --max-tokens / --temperature 由网关侧决定，静默忽略；
        --json-schema 支持（走与 llm 相同的输出后处理抽 JSON）。

codebuddy-gateway 引擎（CodeBuddy Code HTTP 网关，走 webhook + SSE；凭据放配置文件）：
  接的是**已经在跑的** CodeBuddy Code 网关 —— codebuddy --serve（或交互会话里的 /gateway
  远程控制）暴露的那套 HTTP 服务。magic-agent 把 prompt 以 webhook 投递进去，再收 SSE 增量。
  为什么值得单开一个引擎：复用那个**已经登录、已经配好模型与 MCP** 的网关进程（不必每次
  spawn 一个 CLI），并且执行发生在网关那台机器上（Tunnel 形态下网关可以在另一台机器/容器）。
  配置文件（同 arkclaw 那节所在文件）：
    {
      "codebuddyGateway": {
        "url": "http://127.0.0.1:8399"
      }
    }
  url = 网关根地址（不带 /api/v1）；--auth password 的网关再给 password（启动日志里打印的
  那个口令）。节名别名：codebuddyGateway | codebuddy_gateway | codebuddy-gateway | cbgw；
  字段别名：url|endpoint、password|token|key|secret。环境变量覆盖：
  MAGIC_AGENT_CBGW_URL / MAGIC_AGENT_CBGW_PASSWORD。
    magic-agent -e codebuddy-gateway "问题"                    # 单轮
    magic-agent -e codebuddy-gateway --session conv-1 "接着说"   # 按 conversation id 续接
    magic-agent -e codebuddy-gateway --stream -o text "问题"     # 流式（见下方「成色」说明）
    magic-agent --engines | jq '.[] | select(.engine=="codebuddy-gateway")'

  说明：网关是「受理即返回」的异步模型 —— 投递只回 202 + runId，**正文只在 SSE 上出现**，
        所以非流式（不带 --stream）也读 SSE，只是不往外发增量。
        --session <conversation_id> 就是网关的会话锚点，多轮上下文接得上；不传则每次新会话
        （输出里的 session_id 给的就是这个锚点，不是网关内部的 session UUID）。
        不支持 -c/--continue（网关无「查询最近会话」接口，显式报错）。
        -m/--model 不生效（模型由网关进程自己的配置决定，会打一行告警）；
        --tools / --permission / -w 同样没有落地通道（由网关进程的 --agent /
        --permission-mode / 启动目录决定）→ 能力表报 none，传了会在 CLI 层提示或报错。
        -a/--attach 走 payload.attachments 原生字段，但网关把它渲染成「附件清单 + 路径」
        塞进 prompt（图不会以图片形式进模型上下文）→ 能力表如实报 prompt。
        ⚠️ --stream 的成色：实测（2026-09-24）该网关只推「终帧（整段正文）」，**不发增量帧**
        —— 所以它和 arkclaw 一样是「整段到达」，不是打字机。增量帧的解析已按协议实现，
        网关将来吐 content.chunk 时客户端零改动即可接住。
        网关没按 SSE 回（老版本）时如实报错，不假装有增量。

常驻会话与追加需求（--append；claude/codebuddy 默认开，dsh 需显式 --keep-alive）：
  引擎能在**同一个进程**里接着收 user 消息，所以：
    magic-agent -e claude --stream -p "先把仓库跑一遍测试" -o text          # 默认就是常驻会话
    magic-agent -e dsh --stream --keep-alive -p "记住 42" -o text           # dsh 要显式开
    magic-agent --append <run_id|session_id> -p "追加：顺便把 lint 也跑了"   # 任务跑着时随时追加
    magic-agent -e claude --stream --keep-alive=false -p "..."              # 关掉常驻（turn 结束即退出）
  两条落地通道（CLI 侧同一套 --append 入口）：
    claude/codebuddy  --input-format stream-json 的 stdin 持续喂 user 消息；
    dsh               SDK 通道下对同一 sessionId 继续 session/prompt（官方推荐的续接方式）
  常驻会话会开一个 unix socket 追加入口（路径写在会话登记表的 append 字段里，权限 0600），
  --append 连上去把消息交给它，再由它写进引擎 stdin（dsh 则是排进同一会话）成为下一轮。
  追加进来的轮次照常以事件流输出（每轮收尾发一个 turn_end 事件，便于调用方知道「这轮完了」）。
  收工：最后一轮结束后 --idle（默认 5m）内没人追加 → 优雅退出；--idle 0 = 本轮结束就收工
  （追加窗口只在任务运行期间）；也可随时 --stop。
  常驻生效期间不套 -t 默认超时（长任务+追加不受 600s 限制），显式给 -t 才套。
  不生效的形态会自动忽略（非 --stream 调用、不支持追加的引擎）；显式传 --keep-alive 却
  不满足条件则明确报错（exit 2）。Windows 无 unix socket，暂不支持。
  注意：常驻进程会在最后一轮结束后继续存活 --idle 那么久 —— 调用方若要「一轮完成」
  的信号，请读事件流里的 turn_end（而不是等进程退出），或传 --keep-alive=false。
  dsh 的常驻**默认关**：它的 SDK 通道本来是一次一轮的形态，默认挂 5 分钟空闲窗口会让既有
  --stream 调用以为命令卡住了；要开就显式 --keep-alive。

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
    dsh        prompt             同上（headless 无附件参数；dsh 自己的工具能读文件）
    codebuddy-gateway
               prompt             走 payload.attachments 原生字段，但网关把它渲染成路径清单
                                  塞进 prompt（图不会以图片形式进模型上下文）
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
			/* 只在流式下成立的开关：给了却不在流式路径上就报错，**不静默忽略**
			   —— 静默忽略是本项目反复踩过的坑（见 README「-t 600s 被静默忽略」那条）。 */
			if !opts.stream && (opts.events || opts.control) {
				return &usageError{fmt.Errorf("--events / --control 只在流式下成立：请配合 --stream 使用")}
			}
			if opts.control && opts.file == "-" {
				return &usageError{fmt.Errorf("--control 与「从 stdin 读提示词（-f -）」冲突：stdin 已被控制通道占用")}
			}
			switch {
			case flagChanged(cmd, "login"):
				// 用「传过没有」而不是「值非空」判断：`--login ""` 也要走参数校验（提示要引擎名）
				return runLogin(cmd, opts)
			case opts.listSessions:
				return runSessions(cmd, opts)
			case flagChanged(cmd, "session-log"):
				// 与 --stop 同理：用「传过没有」判断，`--session-log ""` 要走参数校验（报要 id），
				// 而不是掉进下面的「提问」分支报 empty prompt。
				return runSessionLog(cmd, opts)
			case flagChanged(cmd, "after"):
				// --after 只在 --session-log 下成立：给了却没给 --session-log 就**明说**，
				// 不静默忽略（静默忽略是本项目反复踩的坑）。
				return &usageError{fmt.Errorf("--after 只在 --session-log 下成立：请配合 --session-log <id> 使用")}
			case flagChanged(cmd, "stop"):
				// 用「传过没有」而不是「值非空」判断：`--stop ""` 也要走参数校验
				//（exit 2 提示要 id），而不是掉进「提问」分支报 empty prompt。
				return runStop(cmd, opts)
			case flagChanged(cmd, "append"):
				return runAppend(cmd, args, opts)
			case opts.contract:
				return runContract(cmd, opts)
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

// probeEngineCredits 单引擎积分倍率探测的入口（与 probeEngineModels 同理，可测替换）。
var probeEngineCredits = func(ctx context.Context, l agent.ModelCreditLister) map[string]string {
	return l.ModelCredits(ctx)
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
//		engine       引擎名
//		ok           引擎是否可用
//		bin          可用时给二进制路径（无本机 CLI 的引擎给端点 URL）
//		note         不可用原因
//		install      不可用时给「一键安装命令」（shell 一行，可直接执行）；没有可执行安装
//		             路径的引擎（codebuddy/codebuddy-ai 是 GUI 应用、arkclaw 靠配置文件）不给
//		models       该引擎当前支持的模型标识（动态探测，见 agent.ModelLister）
//		models_note  拿不到 models 时的原因（无动态来源 / 探测失败）
//		model_credits 各模型积分倍率（model → "0.34"；仅实现 agent.ModelCreditLister
//		             的引擎给出，与 models 同源同链；缺省省略该字段）
//		workspace    该引擎「指定工作目录」的落地方式：flag:-C（codex 原生）/ cwd（子进程 cwd）/
//		             none（llm、arkclaw、openclaw 不支持，-w 会被忽略）
//	     streaming    该引擎是否支持 `--stream`（false = 调用方应改用非流式）。9 个引擎都是 true，
//	                  但成色不同：dsh 只流**推理增量**（正文仍一次性给出）；
//	                  openclaw 走 `openclaw acp`（逐字正文增量 + 工具事件，⚠️ 多一个外部依赖
//	                  「Gateway 在跑」——桥起不来时该轮回退非流式、且该字段会在冷却期内变 false，
//	                  见 internal/agent/openclaw_acp.go）；
//	                  arkclaw 走 A2A 官方 SSE（`message/stream`），⚠️ 但网关不吐逐字增量，
//	                  实测只有「受理帧 + 整段正文」，见 internal/agent/arkclaw_stream.go
//
// 模型**一律现取现算、不硬编码**：各引擎用自己 CLI/配置文件里的权威入口
// （trae-cli models / llm models / codex debug models / openclaw models list /
// codebuddy --help / claude settings.json）。探测要真的启动 CLI（只读子命令），
// 故并发执行并套 agent.DefaultModelProbeTimeout；--no-models 可整段跳过（只列引擎，快）。
// --json 为兼容旧调用保留（本函数不再区分，恒为 JSON）。
func runEngines(cmd *cobra.Command, opts *askOptions) error {
	return emitEngineRows(cmd, opts, false)
}

// runContract 输出带版本的契约 envelope（契约版本 + 引擎数组）。
//
// 客户端启动时读它做契约校验：版本超出自己支持的范围就明确报错，而不是带着
// 不匹配的能力继续跑 —— 「core 加了字段、客户端按旧字段解析」这类漂移在界面上
// 表现为空白，是这类架构最常见的事故（本项目已踩过两次同类问题）。
//
// ⚠️ 默认**不探测模型**：契约只描述「引擎能做什么」，模型清单属于运行态，且每个
// 引擎要起一次真 CLI 进程（10 个引擎约 30s 级）。要连模型一起拿，显式写
// `--contract --no-models=false`。
func runContract(cmd *cobra.Command, opts *askOptions) error {
	o := *opts
	if !flagChanged(cmd, "no-models") {
		o.noModels = true
	}
	return emitEngineRows(cmd, &o, true)
}

// emitEngineRows 构建并输出引擎行 —— `--engines` 与 `--contract` 的唯一实现。
// envelope=true 时外层套一层带 contractVersion 的结构。
func emitEngineRows(cmd *cobra.Command, opts *askOptions, envelope bool) error {
	engines := agent.Engines()
	type row struct {
		Engine string `json:"engine"`
		OK     bool   `json:"ok"`
		Bin    string `json:"bin"`
		Note   string `json:"note,omitempty"`
		// Install 该引擎的「一键安装 / 升级命令」（shell 一行，可直接执行）。
		// ⚠️ 2026-09-23 起**不再只给不可用的行**（用户：「引擎检测除了 a2a 的 其他也要支持有升级」）：
		// 各引擎的安装命令本身就是「装最新版」（`npm i -g <pkg>` / 官方 curl 脚本 / pip），
		// 对**已装好的引擎重跑一遍 = 升级到最新版**，所以可用时也照给。
		// 调用方据此决定：ok:false → 按钮叫「安装」；ok:true → 按钮叫「升级」。
		// 没有可执行安装路径的引擎（codebuddy/codebuddy-ai 是桌面端 GUI 应用、
		// arkclaw / 具名 A2A agent 靠配置文件）**不给**该字段 —— 原因在 note 里，
		// 不编一条跑不通的命令（见 agent.InstallCommandOf）。
		Install string `json:"install,omitempty"`
		// Version 该引擎 CLI 的**当前版本**（尽力而为；读不到就省略）。
		// 用途：让「升级」可验证 —— 调用方跑完升级命令重探一次，比对新旧版本号，
		// 如实说「已升级：旧 → 新」或「版本未变（多半已是最新）」。
		// 读法见 agent.EngineVersionOf（跑 `<bin> --version`，取第一个 x.y.z）。
		// 两类引擎天然没有：A2A 网关（本机没二进制）与桌面端 GUI 应用（CLI 无 --version）。
		Version    string   `json:"version,omitempty"`
		Models     []string `json:"models,omitempty"`
		ModelsNote string   `json:"models_note,omitempty"`
		// ModelCredits 各模型的积分倍率（machine-readable，model → 倍率数字字符串，
		// 如 "fast-model":"0.34"，源于客户端 "x0.34 credits"；与 models 同源同链探测，
		// 仅实现了 agent.ModelCreditLister 的引擎给出 —— 目前 codebuddy / codebuddy-ai）。
		// 客户端不给倍率的模型（custom-local）不出现在 map 里。
		ModelCredits map[string]string `json:"model_credits,omitempty"`
		// Workspace 该引擎「指定工作目录」的落地方式（machine-readable，供调用方决定是否下发）：
		//   flag:-C 原生 flag（codex）；cwd 子进程 cwd（claude/codebuddy/trae）；
		//   none    无落地通道（llm/arkclaw/openclaw/codebuddy-gateway，参数会被忽略）
		Workspace string `json:"workspace,omitempty"`
		// Streaming 是否实现 Streamer（即 `--stream` 可用）。false 时调用方应改用非流式
		//（观物台的 desk:ask 据此决定要不要加 --stream）。当前 10 个引擎都实现了 Streamer，
		// 但「流式」的成色不同：dsh 只有 thinking 增量、正文收尾一次性给出；
		// arkclaw 走 A2A 官方 SSE，实测网关也只发「受理帧 + 整段正文」（见 agent/arkclaw_stream.go）；
		// codebuddy-gateway 同样走 SSE，但它同时是**非流式路径的唯一取文通道**
		//（投递响应里没有正文）。⚠️ 实测该网关目前只推终帧、不发增量帧 ——
		// 成色与 arkclaw 一样是「整段到达」（见 agent/codebuddy_gateway_stream.go）；
		// openclaw 的 ACP 桥不可用（Gateway 没起 / scope 未批）时这里会临时变 false（冷却期内）。
		Streaming bool `json:"streaming"`
		// Attachments 该引擎「收附件（截图等）」的落地方式（machine-readable，供调用方决定
		// 是否下发 -a/--attach，以及要不要提示用户降级）：
		//   flag:-i           codex 原生 --image
		//   stdin:stream-json claude / codebuddy 原生 stream-json input（图片走 content block）
		//   flag:-a           llm CLI 原生 --attachment（直连模型走 image_url content parts）
		//   part:file         arkclaw 走 A2A 原生 file part（base64 inline）
		//   prompt            无原生通道：把绝对路径写进提示词，靠引擎读文件
		//                     （trae/openclaw/dsh/codebuddy-gateway）
		Attachments string `json:"attachments,omitempty"`
		// Append 是否支持「常驻会话 + 追加消息」（`--stream --keep-alive` 与 `--append`）。
		// claude / codebuddy 为 true（stream-json 输入可持续喂 user 消息）；
		// dsh 也为 true，但**默认关**（需显式 --keep-alive，见 agent.AppendDefaultOn）。
		Append bool `json:"append"`
		// Ask 该引擎「需要用户选择」的落地方式（machine-readable，见 agent.AskSupportOf）：
		//   tool:AskUserQuestion 有该工具 + can_use_tool 协议（claude / codebuddy）
		//   none                 无该能力（其余引擎，含 codebuddy-gateway —— 协议里没有作答通道）
		// 调用方据此决定要不要处理 `--stream` 输出里的 `{"type":"ask"}` 事件。
		Ask string `json:"ask,omitempty"`
		// Permission 该引擎对四档权限模型（manual / accept-edits / auto / full）的
		// 落地方式（machine-readable，见 agent.PermissionSupportOf）：
		//   flag:--permission-mode  claude / codebuddy 有原生 --permission-mode
		//   none                    其余引擎未接线 —— 传 --permission 会 exit 2 报错
		//                           （codebuddy-gateway 的档位在网关进程启动参数上，
		//                            且网关会把远程任务的权限强制切到 bypassPermissions）
		// 调用方据此决定是否下发 --permission 与那几个沙箱/规则类 flags。
		Permission string `json:"permission,omitempty"`
		// Capabilities 该引擎**静态声明**的能力 id 集合（字典序，稳定、无重复）。
		// 与运行态分开：ok / streaming / models / version 说的是「此刻如何」，
		// capabilities 说的是「能做什么」，二者的推导见 internal/agent/capability.go。
		// 客户端按 capabilities 决定界面形态，按运行态决定按钮可用与降级提示。
		Capabilities []string `json:"capabilities"`
	}
	rows := make([]row, len(engines))
	var wg sync.WaitGroup
	for i, e := range engines {
		ok, note := e.Detect()
		r := row{Engine: e.Name(), OK: ok, Note: note,
			Capabilities: agent.CapabilitiesOf(e.Name()),
			Workspace:    agent.WorkspaceSupportOf(e.Name()),
			// 能力（实现了 Streamer）× 可用性（流式通道此刻没在冷却中）：
			// 调用方据此决定要不要传 `--stream`，界面据此决定要不要画流式标记。
			// 典型场景：openclaw 的 ACP 桥未授权 → 冷却中 → 这里报 false → 调用方直接走非流式，
			// 界面也不显示流式（用户 2026-09-21：「没批的时候要能检测不显示流式即可」）。
			Streaming:   agent.AsStreamer(e) != nil && agent.StreamAvailable(e.Name()),
			Attachments: agent.AttachmentSupportOf(e.Name()),
			Append:      agent.AppendSupportOf(e.Name()),
			Ask:         agent.AskSupportOf(e.Name()),
			Permission:  agent.PermissionSupportOf(e.Name())}
		if ok {
			r.Bin, r.Note = note, ""
		}
		/* 安装 / 升级命令：**可用时也给**（同一条命令重跑一遍 = 升级到最新版）。
		   没有可执行安装路径的引擎留空 —— 见 InstallCommandOf 的说明，不编命令。 */
		r.Install = agent.InstallCommandOf(e.Name())
		rows[i] = r
		/* ⚠️ `--no-models` 是「能力探测」的快路径（调用方只问有哪些能力），
		   所以**版本与模型一起跳过** —— 不为它多起 N 个进程。 */
		if opts.noModels {
			continue
		}
		/* 当前版本（可读才有；A2A 网关 / GUI 应用天然没有，见 EngineVersionOf）。
		   与模型探测同一档并发跑：它只是 `<bin> --version` 一行输出。 */
		if ok && r.Bin != "" {
			wg.Add(1)
			go func(i int, bin string) {
				defer wg.Done()
				rows[i].Version = agent.EngineVersionOf(cmd.Context(), bin)
			}(i, r.Bin)
		}
		if !ok {
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
				// 积分倍率与 models 同链同源（同一批缓存文件），探测失败/缺省
				// 不致命：该字段整体省略（omitempty），不影响 models。
				if cl, ok := l.(agent.ModelCreditLister); ok {
					rows[i].ModelCredits = probeEngineCredits(ctx, cl)
				}
			}
		}(i, lister)
	}
	wg.Wait()
	if !envelope {
		return printJSON(cmd.OutOrStdout(), rows)
	}
	/* 匿名结构体就地引用函数内的 row 类型：不必把 row 提到包级，
	   也就不会把上面那张字段注释表搬来搬去。 */
	return printJSON(cmd.OutOrStdout(), struct {
		ContractVersion int      `json:"contractVersion"`
		Features        []string `json:"features"`
		Engines         []row    `json:"engines"`
	}{ContractVersion: agent.ContractVersion, Features: agent.CoreFeatures(), Engines: rows})
}
