package agent

// dsh.go - DeepSeek Harness（DSH）引擎。
//
// 后端：`dsh`（DeepSeek Harness，npm 包 @deepseek-ai/dsh，MIT，2026-08 开源；
// 本文件依据其官方 CLI 行为参考 apps/cli/reference 与 SDK 协议包撰写，非猜测）。
//
// **两条通道**，默认走 SDK，不可用时回退 headless：
//
//	① SDK（默认）   `dsh --profile sdk` + stdio 换行分帧 JSON-RPC 2.0（见 dsh_sdk.go）
//	② headless（回退）`dsh --profile headless "<任务文本>"`
//
// 为什么默认 SDK：headless **没有任何工具调用通道**。它的官方定位就是「推理走 stderr、
// 最终正文走 stdout、然后退出」—— 实测模型用了 bash/glob/read，CLI 只字未提，工具调用
// 只写进 $DSH_HOME/sessions/…/session.v3.jsonl.zstd（zstd 压缩）。而 SDK profile 把
// 会话事件实时推出来（tool/call + tool/result + 逐 step 的正文），且 initialize 可指定
// provider/model/maxTokens。SDK 通道不可用时（旧版 dsh 没有该 profile）才回退 headless。
//
// headless 调用形态与关键事实（官方 reference 明确写出，逐条对应本文件的实现）：
//
//   - 启动器 flags 必须在 app 参数之前，且**在第一个它不认识的 token 处结束**；
//     headless 的 app 参数**只有任务文本本身**（位置参数），没有 --prompt。
//     故回退路径固定产出 `--profile headless <prompt>`（FlattenPrompt 的正文恒以
//     「【用户】/【系统指令】」开头，不会撞上启动器 flag 名，无需 `--` 边界符）。
//   - 一次调用 = 一个**全新的持久化 Agent**：提交任务 → 等静默 → flush 会话 →
//     取该区间最后一条非空助手文本。**没有续接参数**（无 --resume / --continue）。
//   - stdout **只打印最终助手正文**（纯文本，无 JSON、无 --output-format）；
//     stderr 是 "dsh: reasoning:" 前缀的推理增量 + 终止性错误原因。
//   - 退出码：turn/end 为 completed → 0，否则 1。
//   - 工作目录：**调用时所在目录就是默认 workspace 根**（base 系 profile 语义）
//     → 与 trae/claude 同路，用子进程 cwd 落地（WorkspaceSupportOf = "cwd"）。
//     SDK 通道同理：initialize 的 cwd 就是「每个 SDK 建的会话 header 上记的目录」，
//     同时也仍用子进程 cwd 兜底（工具执行的相对路径跟着它走）。
//   - 权限：新会话默认 workspace-write 预设（写入限制在工作区 + 平台临时目录），
//     read-only 为另一预设，进程回退模式由环境变量 DSH_PERMISSION_MODE 决定。
//     **没有命令行级 approval flag**，且这两个预设无法表达本项目四档模型里的
//     「沙箱关闭 / 无审批」（第 4 档）→ 四档模型在 dsh 上**不接线**
//     （PermissionSupportOf = "none"），显式传 --permission 会在 CLI 层 exit 2
//     报错，而不是静默降级 —— 安全设置不做静默降级。
//   - 工具：DSH 自带 agent 工具循环（base bundle 的 read/write/edit + Bash 等），
//     增删工具靠 profile bundle / cordis.patch.yml 配置，**无命令行级逐工具开关**
//     → Request.Tools 整体忽略（与 openclaw 的「内嵌 agent」同构，见矩阵注 ③）。
//   - 超时：CLI 无 --timeout flag（超时是 provider 级 settings 的
//     streamIdleTimeoutMs 等）；外层超时由 Runner 的 ctx 施加，超时会杀掉
//     整个进程组（runcmd.go），不留孤儿。
//
// 参数映射（统一参数矩阵的 dsh 列）：
//
//	Model        SDK：支持（initialize 的 provider/model，取 `route/model` 形态）
//	             headless 回退：不支持（静默忽略 + 一行告警：换模型要改配置层）
//	SessionID    两条通道都不支持 → **显式报错**，不静默开新会话
//	             （headless 每次全新会话；SDK 的 sessionId 是「未知 id 惰性建会话」，
//	              已存在的 id 会被 -32603 `session already exists` 拒掉，协议也没有
//	              resume/load 方法 → 对「每次调用都是新进程」的 magic-agent 一样续不上）
//	Continue     同上（无「最近一次会话」概念）→ 显式报错
//	MaxTokens    SDK：支持（initialize 的 maxTokens，官方语义「限制 SDK 创建的 agent
//	             及其进程内后代的每次对话模型输出」）；headless 回退：静默忽略
//	Temperature  不支持（无对应键）→ 静默忽略
//	Tools        内嵌 agent（忽略，见上）
//	Permission   未接线（none）→ 显式传则 CLI 层 exit 2
//	SystemPrompt 无独立 system 注入 flag → 展平进 prompt 头部（与 trae/openclaw 同路）
//	Timeout      进程级超时（CLI 无 flag）
//	JSONSchema   无原生约束 → 走与 llm/openclaw/arkclaw 同一条「输出后处理抽 JSON」
//	Workspace    子进程 cwd（SDK 通道另把 cwd 交给 initialize）
//	Attachments  无原生输入通道 → 绝对路径写进提示词（与 trae/openclaw 同路）
//
// 流式：实现了 Streamer，两通道能力不同（如实接线，不假装）：
//
//	SDK 通道（默认）  推理 → KindThinking；正文 → 逐 step KindText（真流式）；
//	                  工具 → KindToolUse / KindToolResult；收尾一条 KindTurnEnd。
//	headless 回退     推理增量走 stderr（"dsh: reasoning:" 起头，约 300ms 一批）
//	                  → 逐条 KindThinking；正文在 turn 结束时一次性 stdout
//	                  → 收尾一条 KindText。**工具调用在这条路上拿不到**（协议如此）。
//
// CLI 路径解析顺序：显式 BinPath → MAGIC_AGENT_DSH_BIN → 常见安装位 → PATH
//（探测链统一收敛在 engine_base.go 的 cliBase）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DshEngine 通过 DeepSeek Harness CLI 实现 Engine（默认 SDK 通道，见 dsh_sdk.go）。
type DshEngine struct {
	// BinPath 显式指定 CLI 路径（空 = 自动探测）。测试注入用。
	BinPath string

	// Profile 显式 profile 名（空 = 看 DshProfileEnv，再空 = 默认走 SDK 通道）。
	//
	// 显式给 "headless"（或企业内预置的自定义 profile）即**关闭 SDK 通道**，退回
	// 单 profile 的一次性调用 —— 留这个字段是为了让调用方/测试能指向自定义 profile
	//（例如预置了插件与模型的 profile），语义仍是「一次性任务」入口。
	Profile string
}

// Name 实现 Engine。
func (e *DshEngine) Name() string { return "dsh" }

// DefaultDshTimeout 单次尝试默认超时。
//
// dsh 自带 agent 工具循环（读文件 / 跑命令），一轮任务常常是分钟级；
// 取 10 分钟与 trae/openclaw 对齐。CLI 侧没有 --timeout，超时完全由外层
// ctx 施加（超时会杀掉 dsh 及其 node worker 的整个进程组）。
const DefaultDshTimeout = 10 * time.Minute

// dshDefaultProfile headless 是一次性（非交互）入口。
const dshDefaultProfile = "headless"

// DshProfileEnv 覆盖 dsh profile 的环境变量（优先级低于 DshEngine.Profile）。
//
// 存在的理由：SDK 通道是**默认**，但它是预发布协议（官方自述「无兼容承诺」）。
// 万一某版 dsh 的 SDK 行为有变，用户要能不改代码就切回 headless：
//
//	MAGIC_AGENT_DSH_PROFILE=headless magic-agent -e dsh "..."
//
// 取值与 Profile 字段同义："sdk" = SDK 通道，其余（如 "headless"、自定义 profile 名）
// = 单 profile 的一次性调用。
const DshProfileEnv = "MAGIC_AGENT_DSH_PROFILE"

// effectiveProfile 生效的 profile 名：显式字段 → 环境变量 → 默认（SDK 通道）。
func (e *DshEngine) effectiveProfile() string {
	if p := strings.TrimSpace(e.Profile); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv(DshProfileEnv)); p != "" {
		return p
	}
	return dshSDKProfile
}

// profile 返回**回退路径**要用的 profile 名（headless / 自定义 profile）。
//
// 与 effectiveProfile 分开是必要的：SDK 通道握手失败时我们要退回 `--profile headless`，
// 而不是把 `--profile sdk` 当 app 参数再喂一遍（那是 SDK 启动器 profile，不是一次性入口）。
func (e *DshEngine) profile() string {
	if p := e.effectiveProfile(); p != dshSDKProfile {
		return p
	}
	return dshDefaultProfile
}

// bin 探测 dsh CLI 路径（委托 cliBase 统一探测链）。
func (e *DshEngine) bin() string {
	return dshBase.resolve(e.BinPath)
}

// Detect 实现 Engine。
func (e *DshEngine) Detect() (bool, string) {
	p := dshBase.resolve(e.BinPath)
	if p == "" {
		return false, dshBase.notFound
	}
	return true, p
}

// ListModels 实现 ModelLister：返回 `$DSH_HOME/settings.yaml` 里**已配置**的模型。
//
// 为什么不查 CLI：dsh 启动器只有 profile / plugin / config-dump 三种模式，没有
// `dsh models` 之类的只读子命令（headless 也没有 --model 参数）。
//
// 为什么不查网关的 `GET {baseURL}/models`：官方明确「settings.yaml 才是决定一条路由
// 服务什么的东西」—— 网关列出的上百个模型并不都能用，只有 `models:` 里声明过的才会被
// 路由接受。故**配置层才是权威**，也顺带避免了每次探测都发网络请求 + 需要解密凭据。
//
// 输出形态 `provider/model`（与 openclaw 的 `provider/model` 同构）：dsh 的模型只在
// 所属路由内有意义，同名模型可能出现在两条路由上，裸 id 会歧义。
//
// 这份清单**就是** `-m` 的可取值 —— 但只在 SDK 通道下生效（转成 initialize 的
// provider/model 两个必填参数，见 dshResolveRoute）。回退 headless 后 `-m` 无作用
// （无 --model 参数），引擎会打一行告警说明该改配置层。
func (e *DshEngine) ListModels(_ context.Context) ([]string, error) {
	home := DshHome()
	if home == "" {
		return nil, fmt.Errorf("%w: 无法定位 $DSH_HOME（设置 DSH_HOME 或检查 HOME）", ErrNoModelSource)
	}
	data, err := os.ReadFile(filepath.Join(home, "settings.yaml"))
	if err != nil {
		return nil, fmt.Errorf("%w: 读不到 %s（模型由该文件的 agent-default-model / llm-pi-ai 段决定）: %v",
			ErrNoModelSource, filepath.Join(home, "settings.yaml"), err)
	}
	models := parseDshModelRoutes(string(data))
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: %s 里没有已配置的模型（在 llm-pi-ai.providers.<id>.models 下列出，并设 agent-default-model）",
			ErrNoModelSource, filepath.Join(home, "settings.yaml"))
	}
	return models, nil
}

// sdkEnabled 是否走 SDK 通道（effectiveProfile == "sdk"）。
//
// 默认走；显式 Profile 或 MAGIC_AGENT_DSH_PROFILE 给别的值即关闭（见 effectiveProfile）。
func (e *DshEngine) sdkEnabled() bool {
	return e.effectiveProfile() == dshSDKProfile
}

// errDshNoSessionResume 会话续接在 dsh 上不可用（两条通道都不行，理由不同）。
//
// 明确报错而不是静默开新会话 —— 后者会让调用方以为上一轮上下文还在。
var errDshNoSessionResume = errors.New(
	"dsh: 不支持会话续接（-s/--session）—— headless 每次调用都是全新会话；" +
		"SDK 通道的 sessionId 只在**同一个 runtime 进程内**可续（官方 Python SDK 文档：" +
		"reuse a harness, home, and id），换进程拿旧 id 会被 -32603 `session already exists` 拒掉，" +
		"协议也没有 resume 方法。要真正的多轮上下文请用常驻会话：" +
		"`magic-agent -e dsh --stream --keep-alive \"首轮\"`，再用 " +
		"`magic-agent --append <run_id|session_id> -p \"追加\"` 接着问")

// dshModelLabel 本轮实际用的模型标签（`provider/model`，与 ListModels 同形态）。
func dshModelLabel(res dshSDKTurnResult) string {
	if res.Provider == "" || res.Model == "" {
		return ""
	}
	return res.Provider + "/" + res.Model
}

// Complete 实现 Engine：优先 `dsh --profile sdk`（工具调用 + 正文流式 + 可指定模型），
// 该 profile 不可用时回退 `dsh --profile headless "<prompt>"`。
//
// 执行流程：
//  1. 拒掉续接参数（两条通道都续不上，静默忽略会让调用方误以为上下文接上了）
//  2. 展平 prompt（无独立 system 通道）+ 附件路径兜底
//  3. SDK 通道：initialize + session/prompt，收 turn/end 时的正文
//     （通道不可用 → 回退；服务端明确拒绝 → 直接报错，不吞）
//  4. 回退路径：runCLIIn（workspace 走子进程 cwd），stdout 即最终正文
//  5. JSONSchema 模式下再做严格抽取替换 Text
func (e *DshEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return Response{}, fmt.Errorf("dsh CLI not found; set MAGIC_AGENT_DSH_BIN")
	}

	if req.SessionID != "" || req.Continue {
		return Response{}, errDshNoSessionResume
	}

	// 无独立 system 注入 flag：与 trae / openclaw 同路，展平进 prompt 头部。
	// 这里不追加 noToolSuffix —— dsh 是自带工具循环的 agent harness，
	// Request.Tools 在它上面没有落地通道（见文件头矩阵），后缀只会是噪音。
	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if prompt == "" {
		return Response{}, fmt.Errorf("dsh: empty prompt")
	}
	// 附件：dsh 没有附件输入参数，只能把绝对路径写进提示词
	//（与 trae/openclaw 同一兜底；需要 dsh 侧的读文件工具能看到它）。
	prompt = appendAttachmentSection(prompt, req.Attachments)

	if e.sdkEnabled() {
		res, sdkErr := dshSDKTurn(ctx, bin, req.Workspace, req, prompt, nil)
		if sdkErr == nil {
			resp := Response{
				Engine:  e.Name(),
				Text:    strings.TrimSpace(res.Text),
				Model:   dshModelLabel(res),
				Latency: time.Since(start),
			}
			// JSONSchema 后处理（与 llm / openclaw / arkclaw 同路径）。
			if req.JSONSchema != nil {
				if extracted, ok := extractJSONObjectStrict(resp.Text, req.JSONSchema); ok {
					resp.Text = extracted
				}
			}
			if resp.Text == "" {
				return resp, fmt.Errorf("dsh SDK returned empty result")
			}
			return resp, nil
		}
		// 服务端明确拒绝（模型没配等）→ 真失败，不换通道重试（换了就是静默改模型）。
		if !errors.Is(sdkErr, errDshSDKUnavailable) {
			return Response{}, sdkErr
		}
		// 通道不可用 → 回退 headless，并把代价说清楚。
		// 但常驻会话（req.Append）在 headless 上没有落地通道 → 宁可报错也不静默降级
		//（静默降级 = 追加消息被丢掉，调用方却以为接上了）。
		if req.Append != nil {
			return Response{}, fmt.Errorf(
				"dsh: 常驻会话（--keep-alive/--append）需要 SDK 通道，但它不可用：%v", sdkErr)
		}
		fmt.Fprintf(stderr, "  ⚠ dsh: SDK 通道不可用，本轮回退 headless（拿不到工具调用、正文不流式）：%v\n", sdkErr)
	}

	// 走到这里就是要用 headless 执行了 —— 它没有 --model 参数，模型由配置层决定。
	// -m 在 headless 上不是透传是「无作用」：告警一次（与 arkclaw 一致），让用户知道
	// 该去改 dsh 自己的配置。显式 Profile: "headless" 也走这里，同样告警。
	if req.Model != "" {
		fmt.Fprintf(stderr, "  ⚠ dsh: headless 通道下 --model %s 不生效（无 --model 参数）；"+
			"换模型请改 $DSH_HOME/settings.yaml 的 agent-default-model 段，或用 dsh 的 Settings → Models\n", req.Model)
	}

	args := e.buildArgs(req, prompt)

	// workspace：dsh 的「调用时所在目录即默认 workspace 根」正是子进程 cwd 的语义。
	stdout, stderrText, runErr := runCLIIn(ctx, req.Workspace, bin, args...)

	resp := Response{
		Engine: e.Name(),
		// headless 的 stdout 就是最终助手正文（推理增量在 stderr，不混入）。
		Text: strings.TrimSpace(stdout),
		// 模型名尽力而为：读 dsh 配置里的默认模型（读不到给 "config-default"）。
		Model:   DshConfiguredModel(),
		Latency: time.Since(start),
	}
	if resp.Model == "" {
		resp.Model = "config-default"
	}
	// JSONSchema 后处理（与 llm / openclaw / arkclaw 同路径）。
	if req.JSONSchema != nil {
		if extracted, ok := extractJSONObjectStrict(resp.Text, req.JSONSchema); ok {
			resp.Text = extracted
		}
	}
	if runErr != nil {
		// 非零退出 = turn/end 不是 completed；stderr 末尾通常就是终止原因，
		// wrapCliError 会把它摘成可读摘要。
		return resp, wrapCliError("dsh", stdout, stderrText, runErr)
	}
	if resp.Text == "" {
		return resp, fmt.Errorf("dsh CLI returned empty result")
	}
	return resp, nil
}

// buildArgs 构造 dsh 回退路径（headless / 自定义 profile）的参数。
//
// 形态（启动器 flags 在前，app 参数在后）：
//
//	dsh --profile headless "<prompt>"
//
// prompt 走**位置参数**（headless 唯一的 app 参数，无 --prompt / stdin 通道）。
// 代价是超长 prompt 会撞 ARG_MAX —— 与 trae 的 -p 同一个已知边界，
// 需要喂超长文本时请先落成文件、让 dsh 用自己的读文件工具看（配合 -a 路径兜底同理）。
//
// ⚠️ 只有回退路径用得到它：SDK 通道的 prompt 走 session/prompt 的 contentBlocks
// （见 dsh_sdk.go），既不占命令行也不受 ARG_MAX 限制。
func (e *DshEngine) buildArgs(req Request, prompt string) []string {
	// 统一参数矩阵：MaxTokens / Temperature 在 headless 上无对应 flag（静默忽略）；
	// Tools 由 dsh 自己的工具循环决定（忽略）；Timeout 由外层 ctx 施加（忽略）。
	_ = req.MaxTokens
	_ = req.Temperature
	_ = req.Tools
	_ = req.Timeout
	return []string{"--profile", e.profile(), prompt}
}

// ── 流式（--stream）────────────────────────────────────────────
//
// dsh 有两条流式形态，能力不同，如实接线：
//
//	SDK 通道（默认，见 dsh_sdk.go）
//	  session.event 实时推送：
//	    assistant/message 的 reasoning 块 → 逐条 KindThinking
//	    assistant/message 的 text 块      → 逐 step KindText（真流式，不等 turn 结束）
//	    tool/call                        → KindToolUse（name + arguments + callId）
//	    tool/result                      → KindToolResult（按 callId 关联）
//	    turn/end                         → 收尾一条 KindTurnEnd
//
//	headless 回退（实测 2026-09-21，dsh 0.1.5-rc.2）
//	  stderr：`dsh: reasoning:` 标题行之后，推理增量**逐段到达**（约 300ms 一批）
//	  stdout：最终助手正文在 turn 结束时**一次性**到达
//	  → 推理 → 逐条 KindThinking；正文 → 一条 KindText（收尾时）
//	  ⚠️ 这条路上**没有任何工具调用通道**（官方 headless 的定位就是「stream reasoning
//	     to stderr, print the final assistant message, and exit」）—— 不是解析缺陷。

// dshReasoningPrefix stderr 上推理增量的起始标记（官方 headless 的固定前缀）。
const dshReasoningPrefix = "dsh: reasoning:"

// Stream 实现 Streamer：默认走 SDK（推理/正文/工具调用全流式），
// SDK 不可用时回退 headless（只有推理流式 + 收尾正文）。
func (e *DshEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return StreamResult{}, fmt.Errorf("dsh CLI not found; set MAGIC_AGENT_DSH_BIN")
	}
	// 与 Complete 同一条契约：dsh 两条通道都续不上，明确报错而不是静默开新会话。
	if req.SessionID != "" || req.Continue {
		return StreamResult{}, errDshNoSessionResume
	}

	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if prompt == "" {
		return StreamResult{}, fmt.Errorf("dsh: empty prompt")
	}
	prompt = appendAttachmentSection(prompt, req.Attachments)

	if e.sdkEnabled() {
		res, sdkErr := dshSDKTurn(ctx, bin, req.Workspace, req, prompt, onEvent)
		if sdkErr == nil {
			resp := Response{
				Engine: e.Name(),
				Text:   strings.TrimSpace(res.Text),
				Model:  dshModelLabel(res),
				// 报出本轮会话 id：常驻会话里 `--append <session_id>` 能用它定位
				//（会话登记表按 run_id 或 session_id 查找）。注意它**不能**当 -s 用 ——
				// 跨进程续接拿不到（见 errDshNoSessionResume）。
				SessionID: res.SessionID,
				Latency:   time.Since(start),
			}
			if req.JSONSchema != nil {
				if extracted, ok := extractJSONObjectStrict(resp.Text, req.JSONSchema); ok {
					resp.Text = extracted
				}
			}
			// ⚠️ 不再在这里补发 KindTurnEnd：SDK 通道**每一轮**都由 dshSDKTurn 在
			// turn/end 时发过（常驻会话里每轮一条，常驻看门狗靠它判断轮次边界）。
			// 再发一条会把「最后一轮」重复上报一次。
			if resp.Text == "" {
				return StreamResult{Response: resp, Thinking: res.Thinking, Tools: res.Tools},
					fmt.Errorf("dsh SDK returned empty result")
			}
			return StreamResult{Response: resp, Thinking: res.Thinking, Tools: res.Tools}, nil
		}
		// 服务端明确拒绝 → 真失败，不换通道（换了就是静默改模型/换语义）。
		if !errors.Is(sdkErr, errDshSDKUnavailable) {
			return StreamResult{}, sdkErr
		}
		// 常驻会话在 headless 上没有落地通道 → 宁可报错也不静默丢追加。
		if req.Append != nil {
			return StreamResult{}, fmt.Errorf(
				"dsh: 常驻会话（--keep-alive/--append）需要 SDK 通道，但它不可用：%v", sdkErr)
		}
		fmt.Fprintf(stderr, "  ⚠ dsh: SDK 通道不可用，本轮回退 headless（拿不到工具调用、正文不流式）：%v\n", sdkErr)
	}

	// 同 Complete：headless 通道没有 --model 参数，-m 在这里是「无作用」，告警一次。
	if req.Model != "" {
		fmt.Fprintf(stderr, "  ⚠ dsh: headless 通道下 --model %s 不生效（无 --model 参数）；"+
			"换模型请改 $DSH_HOME/settings.yaml 的 agent-default-model 段，或用 dsh 的 Settings → Models\n", req.Model)
	}

	args := e.buildArgs(req, prompt)

	var thinking strings.Builder
	inReasoning := false
	stdout, stderrText, runErr := runStreamStderrIn(ctx, req.Workspace, bin, args, func(line string) error {
		// 标题行本身不是内容；它出现即进入推理区段。
		if strings.HasPrefix(line, dshReasoningPrefix) {
			inReasoning = true
			return nil
		}
		if !inReasoning {
			return nil // 非推理行（例如启动日志）不当作增量
		}
		thinking.WriteString(line)
		thinking.WriteByte('\n')
		if onEvent != nil {
			onEvent(StreamEvent{Kind: KindThinking, Text: line + "\n"})
		}
		return nil
	})

	resp := Response{
		Engine:    e.Name(),
		Text:      strings.TrimSpace(stdout),
		Model:     DshConfiguredModel(),
		SessionID: "", // dsh 不回可续接的会话 id（见 errDshNoSessionResume）
		Latency:   time.Since(start),
	}
	if resp.Model == "" {
		resp.Model = "config-default"
	}
	if req.JSONSchema != nil {
		if extracted, ok := extractJSONObjectStrict(resp.Text, req.JSONSchema); ok {
			resp.Text = extracted
		}
	}
	if runErr != nil {
		return StreamResult{Response: resp, Thinking: thinking.String()},
			wrapCliError("dsh", stdout, stderrText, runErr)
	}
	if resp.Text == "" {
		return StreamResult{Response: resp, Thinking: thinking.String()},
			fmt.Errorf("dsh CLI returned empty result")
	}

	// 正文一次性给出（headless 不流式正文，见本节顶部说明）。
	if onEvent != nil {
		onEvent(StreamEvent{Kind: KindText, Text: resp.Text})
		// 同样补一轮结束事件：回退路径下宿主也该知道「本轮做完了」。
		onEvent(StreamEvent{Kind: KindTurnEnd, Text: resp.Text})
	}
	return StreamResult{Response: resp, Thinking: thinking.String()}, nil
}

// ── 配置层：DSH_HOME、模型清单与默认模型（尽力而为）────────────────

// DshHome 返回 dsh 的家目录：$DSH_HOME（非空时）→ ~/.dsh。
// profile / settings.yaml / .credentials.yaml / logs 都在它下面。
func DshHome() string {
	if v := strings.TrimSpace(os.Getenv("DSH_HOME")); v != "" {
		return expandHome(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".dsh")
}

// DshConfiguredModel 尽力而为地报出 dsh 当前配置的默认模型
// （$DSH_HOME/settings.yaml 的 agent-default-model 段）。
//
// 为什么需要它：headless 不回模型名（stdout 只有正文），而调用方（如观物台）
// 通常要展示「这一轮是谁答的」。读不到就返回空串（调用方回落 "config-default"）——
// 与 TraeDefaultModel 的「文件缺失 / 格式变化返回空串」同一约定，绝不因此阻断调用。
func DshConfiguredModel() string {
	home := DshHome()
	if home == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, "settings.yaml"))
	if err != nil {
		return ""
	}
	return parseDshDefaultModel(string(data))
}

// parseDshDefaultModel 轻量扫描 settings.yaml（不引入 yaml 依赖），取
// agent-default-model 段里的 model 值。兼容两种写法：
//
//	agent-default-model:            # 块式（与 llm-deepseek: 等段同一风格）
//	  provider: deepseek
//	  model: deepseek-chat
//
//	agent-default-model: {provider: deepseek, model: deepseek-chat}   # 行内式
//
// 段内取**第一个** model: 键；注释行与空行跳过；遇到下一个顶层键即结束。
// 结构不符（例如键名变化）时返回空串 —— 这是启发式读取，不是契约解析。
func parseDshDefaultModel(yamlText string) string {
	const key = "agent-default-model:"
	inBlock := false
	for _, ln := range strings.Split(yamlText, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		topLevel := ln == trimmed // 无缩进 = 顶层键
		if !inBlock {
			if !topLevel || !strings.HasPrefix(trimmed, key) {
				continue
			}
			if rest := strings.TrimSpace(strings.TrimPrefix(trimmed, key)); rest != "" {
				// 行内式：只在同一行里找 model 值。
				return yamlScalarAfter(rest, "model:")
			}
			inBlock = true
			continue
		}
		if topLevel {
			return "" // 段结束
		}
		if strings.HasPrefix(trimmed, "model:") {
			if v := yamlScalarAfter(trimmed, "model:"); v != "" {
				return v
			}
		}
	}
	return ""
}

// yamlScalarAfter 从一段 yaml 文本里取 `key` 后的标量值（去引号、去行内注释与
// 行内映射的收尾符号）。找不到返回空串。
func yamlScalarAfter(s, key string) string {
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	v := strings.TrimSpace(s[i+len(key):])
	if j := strings.IndexAny(v, ",}#"); j >= 0 {
		v = v[:j]
	}
	v = strings.TrimSpace(strings.Trim(v, `"'`))
	return v
}

// ── settings.yaml 里的模型清单（缩进感知的轻量扫描）──────────────────
//
// 只认这一种结构（官方文档给出的形状，见 README「配置模型」）：
//
//	agent-default-model:
//	  provider: <route>
//	  model: <id>
//
//	llm-pi-ai:
//	  providers:
//	    <route>:
//	      api: openai-completions
//	      models:
//	        - id: <model-id>
//	        - id: <model-id>
//	          name: <显示名>
//
// 返回去重后的 `route/model` 列表（保持出现顺序）。识别不出结构时返回空切片
// —— 这是**启发式读取**，不是契约解析：settings.yaml 加新键/换布局时不报错，
// 只是列表可能变短，由调用方把「为什么空」写进 models_note。
//
// 不引入 yaml 依赖的理由与本包既有做法一致（见 TraeDefaultModel / parseDshDefaultModel）：
// 只读几个固定键，为它拉一个 YAML 库不划算。

// yamlFrame 一层缩进作用域（key 为不带冒号的键名）。
type yamlFrame struct {
	indent int
	key    string
}

// parseDshModelRoutes 从 settings.yaml 文本里抽 `route/model` 列表。
func parseDshModelRoutes(yamlText string) []string {
	var (
		stack []yamlFrame
		out   []string
		seen  = map[string]bool{}
	)
	add := func(route, model string) {
		model = strings.TrimSpace(model)
		route = strings.TrimSpace(route)
		if model == "" || route == "" {
			return
		}
		v := route + "/" + model
		if seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}

	for _, raw := range strings.Split(yamlText, "\n") {
		line := stripYAMLComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		trimmed := strings.TrimSpace(line)

		// 先按缩进退栈：任何 indent >= 当前行的帧都已结束。
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}

		switch {
		case strings.HasPrefix(trimmed, "- "):
			// 列表项：只在 `llm-pi-ai > providers > <route> > models` 之下才算模型。
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			if route, ok := dshModelsRoute(stack); ok {
				if id, ok := yamlMapScalar(item, "id"); ok {
					add(route, id)
				}
			}
		case strings.HasSuffix(trimmed, ":"):
			key := strings.TrimSpace(strings.TrimSuffix(trimmed, ":"))
			if key == "" {
				continue
			}
			stack = append(stack, yamlFrame{indent: indent, key: key})
		}
	}

	// 兜底：即使没声明 models，也把 agent-default-model 指的那个报出来，
	// 让 --engines 至少能看到「当前用的是哪个模型」。
	if len(out) == 0 {
		if route, model := parseDshDefaultModelRoute(yamlText); route != "" && model != "" {
			add(route, model)
		}
	}
	return out
}

// dshModelsRoute 判断当前缩进栈是否正好是
// `llm-pi-ai > providers > <route> > models`，是则返回 <route>。
func dshModelsRoute(stack []yamlFrame) (string, bool) {
	if len(stack) < 4 {
		return "", false
	}
	n := len(stack)
	if stack[0].key != "llm-pi-ai" ||
		stack[n-3].key != "providers" ||
		stack[n-1].key != "models" {
		return "", false
	}
	route := stack[n-2].key
	if route == "providers" || route == "models" {
		return "", false
	}
	return route, true
}

// yamlMapScalar 从 `key: value` 或行内 `{key: value, ...}` 片段里取 key 的值。
func yamlMapScalar(s, key string) (string, bool) {
	s = strings.TrimSpace(strings.Trim(s, "{}"))
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == key {
			v = strings.TrimSpace(strings.Trim(v, `"'`))
			if v != "" {
				return v, true
			}
		}
	}
	return "", false
}

// parseDshDefaultModelRoute 取 agent-default-model 段的 (provider, model)。
// 与 parseDshDefaultModel 同源，但保留 provider（模型清单要 `route/model` 形态）。
func parseDshDefaultModelRoute(yamlText string) (string, string) {
	const key = "agent-default-model:"
	inBlock := false
	var provider string
	for _, raw := range strings.Split(yamlText, "\n") {
		line := stripYAMLComment(raw)
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		topLevel := line == trimmed
		if !inBlock {
			if !topLevel || !strings.HasPrefix(trimmed, key) {
				continue
			}
			if rest := strings.TrimSpace(strings.TrimPrefix(trimmed, key)); rest != "" {
				// 行内式：{provider: x, model: y}
				p, _ := yamlMapScalar(rest, "provider")
				m, _ := yamlMapScalar(rest, "model")
				return p, m
			}
			inBlock = true
			continue
		}
		if topLevel {
			break // 段结束
		}
		if p, ok := yamlMapScalar(trimmed, "provider"); ok && provider == "" {
			provider = p
		}
		if m, ok := yamlMapScalar(trimmed, "model"); ok {
			return provider, m
		}
	}
	return provider, ""
}

// stripYAMLComment 去掉 yaml 行尾注释（不处理引号内的 #，够本用途）。
func stripYAMLComment(line string) string {
	if i := strings.Index(line, "#"); i >= 0 {
		return line[:i]
	}
	return line
}
