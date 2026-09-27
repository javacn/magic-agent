package agent

// codebuddy.go - CodeBuddy（WorkBuddy / WorkBuddy AI）CLI 引擎。
//
// 两个后端共用本文件的实现核心（codebuddyCore），只差引擎名 / 探测链 / 默认模型：
//
//	codebuddy      WorkBuddy.app 内置 CLI（后端网关 copilot.tencent.com）
//	codebuddy-ai   WorkBuddy AI.app 内置 CLI（后端网关 www.workbuddy.ai）
//
// 两者同为 CodeBuddy Code v2.x：非交互协议与 claude 同源、flag 面一致、Bearer 令牌
// 各自独立。模型注册表按后端不同：
//
//	WorkBuddy  hy3 / glm / kimi / deepseek 等国内模型（--help 动态下发，23 条）
//	AI 端      客户端所见 ~23 个预制模型（分层别名 + gpt-5.x/5.6 + deepseek-v4.1-flash
//	           等；--help 只有 4 个分层别名，完整清单见 listModels 的三级来源链）
//
// codebuddy-ai 默认不强制 --model（交 CLI 自身默认，旧静态清单里的 kimi-k3-1 等
// 实测被国际后端 400 拒绝）。
//
// 配置目录隔离：codebuddy-ai 通过 CODEBUDDY_CONFIG_DIR=~/.codebuddy-ai 使用
// 独立凭据库（见 codebuddyAIDir 注释）—— 共享 ~/.codebuddy 会被桌面 App
// daemon 切换账号，导致登录态被顶、间歇性 401。
//
// 非交互模式输出 envelope 实测有两种形态，解析需兼容：
//
//	1. 单对象：{"type":"result","result":"<md>",...}
//	2. 数组：[{...stream messages...}]，扫描 type=="result" 或
//	   逐条拼 assistant text content
//
// 关键 flags：
//
//	--print --output-format json     单结果 envelope
//	--model <m>                      裸模型名（hy3 / glm-5.3 / ...）
//	--tools ""                       禁用全部工具
//	-y                               启用工具（--dangerously-skip-permissions）
//	-r, --resume <id>                续接指定会话（Request.SessionID 非空时传）。
//	-c, --continue                   续接最近一次会话（Request.Continue）。
//	                                 默认新会话不传 --no-session-persistence，
//	                                 否则首轮会话不落盘、session_id 无法续接
//	                                （与 claude 同款 "cannot be resumed" 语义）
//	--append-system-prompt <s>       注入 system prompt（仅 off 模式附 noToolSuffix）
//	--settings <json>                注入 {"env":{"CLAUDE_CODE_MAX_OUTPUT_TOKENS":"<n>"}}
//	                                 实现 MaxTokens（codebuddy 无原生 max-tokens flag，
//	                                 与 claude 同款 --settings 通道，见矩阵①）
//
// CLI 路径解析：显式 BinPath → MAGIC_AGENT_CODEBUDDY_BIN / MAGIC_AGENT_CODEBUDDY_AI_BIN
// → 各自 App 内置路径 → PATH（探测链统一收敛在 engine_base.go 的 cliBase）。
//
// ⚠️ 在 WorkBuddy 会话内调用时，父进程注入的 SERVER__PORT 会让 CLI 抢
// 父会话已监听的端口 → EADDRINUSE → 永久挂起。子进程环境由 env.go
// 的 denylist 统一剔除该类变量。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codebuddyCore 两个 codebuddy 后端共享的实现核心。
// CodeBuddyEngine / CodeBuddyAIEngine 都是它的薄壳（见文件头）。
type codebuddyCore struct {
	// name 引擎名：进错误消息、Response.Engine、agentSettingsPayload 的引擎键。
	name string
	// base 可执行文件探测链（engine_base.go 的 cliBase）。
	base cliBase
	// binPath 显式指定 CLI 路径（空 = 走 base 探测链）。测试注入用。
	binPath string
	// model 默认模型（空 = 不传 --model，交 CLI 自身默认）。
	model string
	// extraEnv 追加给子进程的环境变量（Go exec：重复 key 取最后 → 覆盖继承值）。
	// codebuddy-ai 用它把 CODEBUDDY_CONFIG_DIR 指向独立配置目录（见 codebuddyAIExtraEnv）。
	extraEnv []string
	// extendedModelSources 启用扩展模型清单来源（仅 codebuddy-ai）：
	// ① AI 桌面客户端合并配置缓存 acc-product-config（客户端模型选择器同源，
	// 见 codebuddyAIAccConfigPath）→ ② 远程配置缓存 ∪ App 包 product.json
	// → ③ --help。WorkBuddy 端不启用。
	extendedModelSources bool
	// modelCacheDirs 远程配置缓存的 local_storage 目录（按优先级），两个用途：
	// extendedModelSources=true 时是扩展清单链 ②；一律用于 modelCredits 的兜底 ②
	// （见 codebuddyAIModelCacheDirs / codebuddyModelCacheDirs）。
	modelCacheDirs []string
	// accConfigPath 该引擎对应桌面客户端的合并产品配置缓存路径（构造时算好：
	// codebuddyAIAccConfigPath / codebuddyAccConfigPath）。两个用途：
	// extendedModelSources=true 时是扩展清单链 ①；一律用于 modelCredits 的首选 ①。
	accConfigPath string
}

// bin 探测 CLI 路径（委托 cliBase 统一探测链）。
func (c codebuddyCore) bin() string {
	return c.base.resolve(c.binPath)
}

// detect 返回（是否可用，说明）。
func (c codebuddyCore) detect() (bool, string) {
	p := c.base.resolve(c.binPath)
	if p == "" {
		return false, c.base.notFound
	}
	if c.model == "" {
		return true, p
	}
	return true, p + " (default model: " + c.model + ")"
}

// extendedModelCatalog 扩展来源链（仅 codebuddy-ai 启用）：返回（模型清单, 积分倍率表, 是否命中）。
//
// ① 客户端合并配置缓存 acc-product-config（客户端模型选择器同源，清单与倍率同文件）
// ② 远程配置缓存 ∪ App 包 product.json（超集近似，含国内后端条目）
//
// 命中 ① 时 ② 不再叠加 —— ① 就是客户端所见，别无二义。倍率表与清单同源同链。
func (c codebuddyCore) extendedModelCatalog(bin string) ([]string, map[string]string, bool) {
	if ids, credits := readProductJSONCatalog(c.accConfigPath); len(ids) > 0 {
		return ids, credits, true
	}
	var ids []string
	credits := map[string]string{}
	for _, dir := range c.modelCacheDirs {
		i, cr := remoteConfigCacheCatalog(dir)
		ids = append(ids, i...)
		for k, v := range cr {
			if _, dup := credits[k]; !dup {
				credits[k] = v
			}
		}
	}
	i, cr := readProductJSONCatalog(productJSONPath(bin))
	ids = append(ids, i...)
	for k, v := range cr {
		if _, dup := credits[k]; !dup {
			credits[k] = v
		}
	}
	ids = dedupeModels(ids)
	if len(ids) == 0 {
		return nil, nil, false
	}
	return ids, credits, true
}

// modelCredits 返回各模型的**积分倍率表**（model → 规范化数字字符串，如 "0.34"，
// 源自客户端 "x0.34 credits"）。来源链与 extendedModelCatalog 完全一致
// （① acc 缓存 → ② 远程配置缓存 ∪ ③ App 包 product.json），但**只取倍率、
// 不改变模型清单来源**：codebuddy-ai 的清单本来就走同一条扩展链；codebuddy 的
// 清单仍按 --help（见 listModels），倍率单独补齐 —— 两张表按 model id 对上，
// acc 缓存里多出的条目（清单里没有的模型）自然不会被任何清单引用。
func (c codebuddyCore) modelCredits(bin string) map[string]string {
	// ① 客户端合并配置缓存（与 extendedModelCatalog 同源同语义：**有模型即钉死**，
	// 哪怕一条倍率都没有也不穿透 ②③ 去混别处的倍率 —— 客户端不给就是不显示）
	if ids, credits := readProductJSONCatalog(c.accConfigPath); len(ids) > 0 {
		if len(credits) == 0 {
			return nil
		}
		return credits
	}
	// ② 远程配置缓存（按优先级，先到先得）∪ ③ App 包 product.json
	credits := map[string]string{}
	for _, dir := range c.modelCacheDirs {
		_, cr := remoteConfigCacheCatalog(dir)
		for k, v := range cr {
			if _, dup := credits[k]; !dup {
				credits[k] = v
			}
		}
	}
	_, cr := readProductJSONCatalog(productJSONPath(bin))
	for k, v := range cr {
		if _, dup := credits[k]; !dup {
			credits[k] = v
		}
	}
	if len(credits) == 0 {
		return nil
	}
	return credits
}

// listModels 实现 ModelLister 的共享逻辑。
//
// 两个来源策略，按引擎择一：
//
//	codebuddy      --help 里 --model 描述自带的 "Currently supported: (...)" 清单
//	codebuddy-ai  客户端合并配置缓存 acc-product-config（客户端同源）
//	              → 客户端未运行过时回退「远程配置缓存 ∪ product.json」
//	              → 再回退 --help（只有 4 个分层别名）
//
// 实测（2026-09-21）：AI 端各单一来源都不等于客户端所见 —— acc 缓存 27 条
// （23 预制 + 4 custom-local，含 deepseek-v4.1-flash / gpt-5.6-*）才是客户端
// 模型选择器的真实数据源；远程配置缓存（52 条）混着国内后端条目但没有 gpt；
// product.json（26 条）没有 deepseek-v4.1-flash；--help 只有 4 个分层别名。
//
// ⚠️ --help 路径必须带 extraEnv：清单由登录后端下发，跟随配置目录。不带隔离
// 环境时 codebuddy-ai 会读到共享 ~/.codebuddy 里 WorkBuddy 后端的清单。
func (c codebuddyCore) listModels(ctx context.Context) ([]string, error) {
	bin := c.bin()
	if bin == "" {
		return nil, fmt.Errorf("%s CLI not found", c.name)
	}
	if c.extendedModelSources {
		// ① 客户端合并配置缓存（客户端模型选择器同源）→ ② 远程配置缓存 ∪
		// App 包 product.json（超集近似）→ 都没有时落 ③ --help（见函数头）。
		if ids, _, ok := c.extendedModelCatalog(bin); ok {
			return ids, nil
		}
	}
	stdout, stderr, err := runCLIEnvIn(ctx, "", c.extraEnv, bin, "--help")
	if err != nil {
		return nil, wrapCliError(c.name, stdout, stderr, err)
	}
	models := parseCodebuddyHelpModels(stdout)
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: --help has no \"Currently supported\" list", ErrNoModelSource)
	}
	return models, nil
}

// complete 单次调用的共享逻辑。
func (c codebuddyCore) complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	bin := c.bin()
	if bin == "" {
		return Response{}, fmt.Errorf("%s CLI not found; set %s", c.name, c.base.envVar)
	}

	// 附件（截图）：与 claude 同族，--print 文本模式收不了图，走
	// --input-format stream-json（图片 content block 经 stdin）。
	if hasImageAttachment(req.Attachments) {
		return c.completeWithAttachments(ctx, req, start)
	}

	args := c.buildArgs(req)

	// workspace：codebuddy 与 claude 同族（无工作目录 flag，--add-dir 只加额外目录）
	stdout, stderr, err := runCLIEnvIn(ctx, req.Workspace, c.extraEnv, bin, args...)
	if err != nil {
		return Response{}, wrapCliError(c.name, stdout, stderr, err)
	}

	raw := strings.TrimSpace(stdout)
	if raw == "" {
		return Response{}, fmt.Errorf("%s CLI returned empty output", c.name)
	}

	text := extractCodeBuddyResult(raw)

	// 兜底：个别版本会把 user 请求回显（<user_query>…</user_query>）当正文。
	if cleaned := stripUserQueryEcho(text); cleaned != text {
		text = cleaned
	}
	if strings.TrimSpace(text) == "" {
		return Response{}, fmt.Errorf("%s CLI 返回内容仅为请求回显（无模型正文）", c.name)
	}

	return Response{
		Text:      text,
		Model:     req.Model,
		SessionID: extractCodeBuddySession(raw),
		Latency:   time.Since(start),
	}, nil
}

// completeWithAttachments 走 stream-json 输入通道跑一次带附件的调用
// （codebuddy 与 claude 同族协议），把事件归约成单次 Response。
func (c codebuddyCore) completeWithAttachments(ctx context.Context, req Request, start time.Time) (Response, error) {
	bin := c.bin()
	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
	if prompt == "" {
		return Response{}, fmt.Errorf("%s: empty prompt", c.name)
	}
	args := streamJSONArgs(c.buildArgsBase(req), prompt, false)
	stdin, err := streamJSONUserLine(prompt, req.Attachments)
	if err != nil {
		return Response{}, err
	}
	acc := &streamAccumulator{Engine: c.name}
	var fin streamJSONResult
	seen, err := runStreamJSONIn(ctx, req.Workspace, c.extraEnv, bin, args, strings.NewReader(stdin), acc, &fin)
	if err != nil {
		return Response{}, wrapCliError(c.name, "", "", err)
	}
	if !seen {
		return Response{}, fmt.Errorf("%s CLI stream ended without result line", c.name)
	}
	if fin.IsError {
		return Response{}, fmt.Errorf("%s CLI error (subtype=%s): %s", c.name, fin.Subtype, truncateStr(fin.Result, 500))
	}
	text := stripUserQueryEcho(finalizeStreamText(fin, acc))
	if strings.TrimSpace(text) == "" {
		return Response{}, fmt.Errorf("%s CLI 返回内容仅为请求回显（无模型正文）", c.name)
	}
	return Response{
		Text:      text,
		Model:     fin.Model,
		SessionID: fin.SessionID,
		Latency:   time.Since(start),
	}, nil
}

// buildArgs 构造 codebuddy CLI 参数（Complete 与 Stream 共用）。
func (c codebuddyCore) buildArgs(req Request) []string {
	return append(c.buildArgsBase(req), FlattenPrompt(req.SystemPrompt, req.Messages, false))
}

// buildArgsBase 构造参数（不含末尾的位置参数 prompt）——附件场景复用：
// 那条路提示词走 stdin，不能再作为命令行参数传。
func (c codebuddyCore) buildArgsBase(req Request) []string {
	args := []string{"--print", "--output-format", "json"}
	switch {
	case req.SessionID != "":
		// 续接指定会话（commander 风格空格传值）。默认新会话不传
		// --no-session-persistence：不落盘的会话无法续接。
		args = append(args, "--resume", req.SessionID)
	case req.Continue:
		// 续接最近一次会话（无需 id）。
		args = append(args, "--continue")
	}
	// 工具模式决定两件事：CLI 侧的工具白名单，以及 system prompt 是否
	// 追加 noToolSuffix。
	//
	// ⚠️ noToolSuffix 只在 off 模式注入。它明文写着「严禁使用任何工具」，
	// 与 on / 白名单模式的目标直接冲突 —— 之前无条件注入，导致
	// `--tools on` 表面开了工具、system prompt 却在压制模型调用，
	// 表现为「工具启用无效」。enable 模式下必须让 system prompt 干净。
	tools := toolsOrDefault(req.Tools)
	switch {
	case tools.IsOff():
		// 关工具：不仅 CLI 侧禁掉，system prompt 也要堵住模型「伪工具调用」
		// （实测 off 且不注入时，模型会输出 <tool_calls:xxxx> 这类假标签）。
		args = append(args, "--tools", "")
		args = append(args, "--append-system-prompt", noToolSuffix)
	case tools.IsOn():
		// 四档权限档位：codebuddy 与 claude 同族，--permission-mode 取值完全一致
		//（default / acceptEdits / auto / bypassPermissions），故复用同一映射。
		// 改造前这里恒传 -y（= --dangerously-skip-permissions，第 4 档）。
		args = append(args, "--permission-mode", claudePermissionMode(req.Permission))
	default:
		args = append(args, "--tools", strings.Join(tools.Allowlist(), ","),
			"--permission-mode", claudePermissionMode(req.Permission))
	}
	if m := stripModelPrefix(req.Model); m != "" {
		args = append(args, "--model", m)
	} else if c.model != "" {
		// 默认模型（codebuddy=hy3；codebuddy-ai 故意留空 = CLI 自身默认，
		// 见文件头的模型注册表漂移说明）；空串 = 不传 --model。
		args = append(args, "--model", c.model)
	}
	// 统一参数矩阵：`--settings` 只接受一份载荷，故 MaxTokens 的 env 注入与
	// 四档模型的 sandbox / autoMode / permissions 合并进同一个 JSON。
	// 注意 codebuddy 的沙箱键集与 claude 略有差异（文档未列 failIfUnavailable），
	// 由 sandboxSettings 按引擎区分（codebuddy / codebuddy-ai 同族同语义）。
	if payload, ok := agentSettingsPayload(req, c.name); ok {
		args = append(args, "--settings", payload)
	}
	return args
}

// stream 流式调用的共享逻辑。
// 协议与 claude 同源（CodeBuddy Code 系 stream-json）。
func (c codebuddyCore) stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()
	bin := c.bin()
	if bin == "" {
		return StreamResult{}, fmt.Errorf("%s CLI not found; set %s", c.name, c.base.envVar)
	}

	args := c.buildArgs(req)
	if req.Append != nil || hasImageAttachment(req.Attachments) {
		// 附件 / 常驻会话 → stream-json 输入通道：提示词改走 stdin（见 streamjson.go）。
		prompt := FlattenPrompt(req.SystemPrompt, req.Messages, false)
		var stdin io.Reader
		if req.Append != nil {
			pipe, perr := streamJSONInputPipe(prompt, req.Attachments, req.Append)
			if perr != nil {
				return StreamResult{}, perr
			}
			stdin = pipe
		} else {
			line, serr := streamJSONUserLine(prompt, req.Attachments)
			if serr != nil {
				return StreamResult{}, serr
			}
			stdin = strings.NewReader(line)
		}
		args = streamJSONArgs(c.buildArgsBase(req), prompt, true)
		acc := &streamAccumulator{Engine: c.name, OnEvent: onEvent}
		var fin streamJSONResult
		seen, err := runStreamJSONIn(ctx, req.Workspace, c.extraEnv, bin, args, stdin, acc, &fin)
		if err != nil {
			return StreamResult{}, err
		}
		if !seen {
			return StreamResult{}, fmt.Errorf("%s CLI stream ended without result line", c.name)
		}
		if fin.IsError {
			return StreamResult{}, fmt.Errorf("%s CLI error (subtype=%s): %s", c.name, fin.Subtype, truncateStr(fin.Result, 500))
		}
		text := stripUserQueryEcho(finalizeStreamText(fin, acc))
		if strings.TrimSpace(text) == "" {
			return StreamResult{}, fmt.Errorf("%s CLI 返回内容仅为请求回显（无模型正文）", c.name)
		}
		return StreamResult{
			Response: Response{
				Engine:    c.name,
				Text:      text,
				Model:     fin.Model,
				SessionID: fin.SessionID,
				Latency:   time.Since(start),
			},
			Thinking: acc.Thinking.String(),
			Tools:    acc.Tools,
		}, nil
	}
	for i := range args {
		if args[i] == "--output-format" {
			args[i+1] = "stream-json"
			break
		}
	}
	args = append(args, "--include-partial-messages", "--verbose")

	acc := &streamAccumulator{Engine: c.name, OnEvent: onEvent}
	var fin struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		IsError   bool   `json:"is_error"`
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
		Model     string `json:"model"`
	}
	seenResult := false

	err := runStreamCLIEnvIn(ctx, req.Workspace, c.extraEnv, bin, args, func(line string) error {
		isResult, perr := acc.handleNDJSONLine(line)
		if perr != nil {
			return perr
		}
		if isResult {
			_ = json.Unmarshal([]byte(line), &fin)
			seenResult = true
		}
		return nil
	})
	if err != nil {
		return StreamResult{}, err
	}
	if !seenResult {
		return StreamResult{}, fmt.Errorf("%s CLI stream ended without result line", c.name)
	}
	if fin.IsError {
		return StreamResult{}, fmt.Errorf("%s CLI error (subtype=%s): %s", c.name, fin.Subtype, truncateStr(fin.Result, 500))
	}

	text := stripUserQueryEcho(fin.Result)
	if strings.TrimSpace(text) == "" {
		text = strings.TrimSpace(acc.Text.String())
	}
	if strings.TrimSpace(text) == "" {
		return StreamResult{}, fmt.Errorf("%s CLI 返回内容仅为请求回显（无模型正文）", c.name)
	}
	return StreamResult{
		Response: Response{
			Engine:    c.name,
			Text:      text,
			Model:     fin.Model,
			SessionID: fin.SessionID,
			Latency:   time.Since(start),
		},
		Thinking: acc.Thinking.String(),
		Tools:    acc.Tools,
	}, nil
}

// ── codebuddy：WorkBuddy.app 后端 ─────────────────────────────

// CodeBuddyEngine 通过 WorkBuddy.app 内置的 codebuddy CLI 实现 Engine。
type CodeBuddyEngine struct {
	// BinPath 显式指定 CLI 路径（空 = 自动探测）。测试注入用。
	BinPath string
}

// Name 实现 Engine。
func (e *CodeBuddyEngine) Name() string { return "codebuddy" }

// DefaultCodeBuddyTimeout 单次尝试默认超时（codebuddy / codebuddy-ai 共用）。
const DefaultCodeBuddyTimeout = 5 * time.Minute

// DefaultCodeBuddyModel codebuddy 引擎的默认模型。
// 未显式 -m 指定时使用 hy3（与 magic-video 的 DefaultCreativeModel 一致）。
const DefaultCodeBuddyModel = "hy3"

// core 返回共享实现核心。
// 扩展清单链（extendedModelSources）不启用：codebuddy 的模型清单按 --help
// （见 listModels 的说明）；积分倍率走 modelCredits（来源见 codebuddyAccConfigPath）。
func (e *CodeBuddyEngine) core() codebuddyCore {
	return codebuddyCore{
		name:           "codebuddy",
		base:           codebuddyBase,
		binPath:        e.BinPath,
		model:          DefaultCodeBuddyModel,
		accConfigPath:  codebuddyAccConfigPath(),
		modelCacheDirs: codebuddyModelCacheDirs(),
	}
}

// bin 探测 codebuddy CLI 路径。
func (e *CodeBuddyEngine) bin() string { return e.core().bin() }

// Detect 实现 Engine。
func (e *CodeBuddyEngine) Detect() (bool, string) { return e.core().detect() }

// ListModels 实现 ModelLister。
func (e *CodeBuddyEngine) ListModels(ctx context.Context) ([]string, error) {
	return e.core().listModels(ctx)
}

// ModelCredits 实现 ModelCreditLister：各模型的积分倍率（规范化数字字符串，
// 如 "0.34"，源自客户端 "x0.34 credits"）。**只取倍率表，清单不动** ——
// codebuddy 的模型清单仍按 --help（见 listModels）；倍率来源链与 codebuddy-ai
// 一致：acc 缓存（~/.workbuddy）→ 远程配置缓存 ∪ product.json。清单里某模型
// 没有倍率数据时自然查不到（UI 不显示）；完全无数据返回 nil（CLI 端省略字段）。
func (e *CodeBuddyEngine) ModelCredits(ctx context.Context) map[string]string {
	bin := e.bin()
	if bin == "" {
		return nil
	}
	return e.core().modelCredits(bin)
}

// Complete 实现 Engine：单次调用 codebuddy CLI。
func (e *CodeBuddyEngine) Complete(ctx context.Context, req Request) (Response, error) {
	return e.core().complete(ctx, req)
}

// Stream 实现 Streamer。
func (e *CodeBuddyEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	return e.core().stream(ctx, req, onEvent)
}

// buildArgs 构造 codebuddy CLI 参数。
func (e *CodeBuddyEngine) buildArgs(req Request) []string { return e.core().buildArgs(req) }

// buildArgsBase 构造参数（不含末尾的位置参数 prompt）。
func (e *CodeBuddyEngine) buildArgsBase(req Request) []string { return e.core().buildArgsBase(req) }

// ── codebuddy-ai：WorkBuddy AI.app 后端 ───────────────────────

// DefaultCodeBuddyAIModel codebuddy-ai 引擎的默认模型。
//
// 故意留空：WorkBuddy AI（国际后端）的模型注册表与静态 --help 清单有漂移
// （--help 里的 kimi-k3-1 / deepseek-v4-pro 实测被后端 400 拒绝），强制指定
// hy3 有同款风险，故默认不传 --model、交 CLI 自身默认；-m 仍可显式指定。
const DefaultCodeBuddyAIModel = ""

// CodeBuddyAIEngine 通过 WorkBuddy AI.app 内置的 codebuddy CLI 实现 Engine。
// 协议 / flag 面与 CodeBuddyEngine 完全一致，仅后端网关与 Bearer 令牌不同。
type CodeBuddyAIEngine struct {
	// BinPath 显式指定 CLI 路径（空 = 自动探测）。测试注入用。
	BinPath string
}

// Name 实现 Engine。
func (e *CodeBuddyAIEngine) Name() string { return "codebuddy-ai" }

// core 返回共享实现核心。
func (e *CodeBuddyAIEngine) core() codebuddyCore {
	return codebuddyCore{
		name:                 "codebuddy-ai",
		base:                 codebuddyAIBase,
		binPath:              e.BinPath,
		model:                DefaultCodeBuddyAIModel,
		extraEnv:             codebuddyAIExtraEnv(),
		extendedModelSources: true,
		modelCacheDirs:       codebuddyAIModelCacheDirs(),
		accConfigPath:        codebuddyAIAccConfigPath(),
	}
}

// workbuddyAccConfigPath 返回 WorkBuddy 系桌面客户端的合并产品配置缓存路径
// （appHome = 客户端数据目录名：".workbuddy-ai" / ".workbuddy"）。
//
// 桌面客户端把「product.json ∪ 网关远程配置 ∪ 用户自定义模型」合并后的运行时
// 配置缓存在 <数据目录>/cache/acc-product-config-v*.json，与 product.json 同构
// （models[].id + models[].credits）—— 这是客户端模型选择器的真实数据源。
// 文件名带版本号（当前 v3）：glob 全部版本取 mtime 最新；客户端从未运行过时
// 文件不存在，返回 ""（调用方回退各自的下一条来源）。
//
// 两端同构不同目录：codebuddy-ai → ~/.workbuddy-ai（WorkBuddy AI App）；
// codebuddy → ~/.workbuddy（WorkBuddy App，实测 2026-09-22 同样有 v3 缓存：
// 58 个模型 33 条倍率，含 hy3 x0.00 —— codebuddy 倍率的首选来源）。
func workbuddyAccConfigPath(appHome string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	hits, err := filepath.Glob(filepath.Join(home, appHome, "cache", "acc-product-config-v*.json"))
	if err != nil || len(hits) == 0 {
		return ""
	}
	best := ""
	var bestMod time.Time
	for _, p := range hits {
		fi, statErr := os.Stat(p)
		if statErr != nil {
			continue
		}
		if best == "" || fi.ModTime().After(bestMod) {
			best, bestMod = p, fi.ModTime()
		}
	}
	return best
}

// codebuddyAIAccConfigPath 返回 AI 桌面客户端（~/.workbuddy-ai）的合并产品配置
// 缓存路径。实测 2026-09-21：27 条 = 23 预制 + 4 custom-local，含
// deepseek-v4.1-flash 与 gpt-5.6-sol/terra/luna、gpt-6-astra 等静态 product.json
// 里还没有的新模型；endpoint=www.workbuddy.ai、daemon 持续刷新。
func codebuddyAIAccConfigPath() string {
	return workbuddyAccConfigPath(".workbuddy-ai")
}

// codebuddyAccConfigPath 返回 WorkBuddy 桌面客户端（非 AI，~/.workbuddy）的
// 合并产品配置缓存路径 —— codebuddy 引擎积分倍率的首选来源。
func codebuddyAccConfigPath() string {
	return workbuddyAccConfigPath(".workbuddy")
}

// codebuddyAIModelCacheDirs 返回 codebuddy-ai 模型清单的远程配置缓存目录（按优先级）。
//
// 隔离目录优先；共享 ~/.codebuddy 兜底 —— 桌面 App daemon 仍在共享目录运行，
// 会把网关下发的远程配置持续刷在那里（实测 2026-09-21：隔离 CLI 自身拉
// /v3/config 全部 400，deepseek-v4.1-flash 所在的远程配置缓存只存在于共享目录）。
// 只读缓存文件，不触碰凭据。用户显式设置 CODEBUDDY_CONFIG_DIR 时只信该目录。
func codebuddyAIModelCacheDirs() []string {
	dirs := []string{filepath.Join(codebuddyAIDir(), "local_storage")}
	if os.Getenv("CODEBUDDY_CONFIG_DIR") != "" {
		return dirs
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	shared := filepath.Join(home, ".codebuddy", "local_storage")
	if shared != dirs[0] {
		dirs = append(dirs, shared)
	}
	return dirs
}

// codebuddyModelCacheDirs 返回 codebuddy（非 ai）倍率兜底的远程配置缓存目录。
// 引擎自身不注入 CODEBUDDY_CONFIG_DIR；用户显式设置时只信该目录（与 CLI 的
// 实际读取一致），否则共享 ~/.codebuddy（桌面 daemon 持续把网关下发的远程
// 配置刷在那里，实测 2026-09-21）。只读缓存文件，不触碰凭据。
func codebuddyModelCacheDirs() []string {
	if dir := os.Getenv("CODEBUDDY_CONFIG_DIR"); dir != "" {
		return []string{filepath.Join(dir, "local_storage")}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return []string{filepath.Join(home, ".codebuddy", "local_storage")}
}

// codebuddyAIDir codebuddy-ai 引擎的专用配置目录（~/.codebuddy-ai）。
//
// 为什么不共用 ~/.codebuddy（2026-09-21 实测根因）：两个桌面 App（WorkBuddy /
// WorkBuddy AI）的 daemon 与两个 CLI 共享同一份凭据库，且各自会切换「活动账号」
// —— 谁后写谁生效。CLI 登录写入的会话会被 WorkBuddy AI 桌面 daemon 顶掉
// （实测顶成另一账号，其 refresh token 401），造成引擎间歇性 401 / 空输出。
// 独立目录让 codebuddy-ai 的登录态只归本引擎，与桌面 App 彻底隔离。
//
// 后端网关不受影响：endpoint 定义在 CLI 安装目录的 product.json（随 App 分发），
// 与配置目录无关（实测 product.json line 11）。
func codebuddyAIDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".codebuddy-ai")
}

// codebuddyAIExtraEnv 返回 codebuddy-ai 子进程的隔离环境变量。
// 用户已显式设置 CODEBUDDY_CONFIG_DIR 时尊重之（不覆盖）。
func codebuddyAIExtraEnv() []string {
	if os.Getenv("CODEBUDDY_CONFIG_DIR") != "" {
		return nil
	}
	return []string{"CODEBUDDY_CONFIG_DIR=" + codebuddyAIDir()}
}

// bin 探测 codebuddy-ai CLI 路径。
func (e *CodeBuddyAIEngine) bin() string { return e.core().bin() }

// Detect 实现 Engine。
func (e *CodeBuddyAIEngine) Detect() (bool, string) { return e.core().detect() }

// ListModels 实现 ModelLister。
func (e *CodeBuddyAIEngine) ListModels(ctx context.Context) ([]string, error) {
	return e.core().listModels(ctx)
}

// ModelCredits 实现 ModelCreditLister：各模型的积分倍率（规范化数字字符串，
// 如 "0.34"，源自客户端 "x0.34 credits"）。与 ListModels 同源同链
// （core().modelCredits，即 extendedModelCatalog 的倍率半边）；客户端不给倍率
// 的模型（custom-local）不在返回值里；引擎不可用 / 无倍率数据时返回 nil。
func (e *CodeBuddyAIEngine) ModelCredits(ctx context.Context) map[string]string {
	bin := e.bin()
	if bin == "" {
		return nil
	}
	return e.core().modelCredits(bin)
}

// Complete 实现 Engine：单次调用 codebuddy-ai CLI。
func (e *CodeBuddyAIEngine) Complete(ctx context.Context, req Request) (Response, error) {
	return e.core().complete(ctx, req)
}

// Stream 实现 Streamer。
func (e *CodeBuddyAIEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	return e.core().stream(ctx, req, onEvent)
}

// buildArgs 构造 codebuddy-ai CLI 参数。
func (e *CodeBuddyAIEngine) buildArgs(req Request) []string { return e.core().buildArgs(req) }

// buildArgsBase 构造参数（不含末尾的位置参数 prompt）。
func (e *CodeBuddyAIEngine) buildArgsBase(req Request) []string { return e.core().buildArgsBase(req) }

// ── 输出解析（两个后端共用）──────────────────────────────────

// stripUserQueryEcho 剥离 <user_query>…</user_query> 请求回显。
func stripUserQueryEcho(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "<user_query>") {
		return s
	}
	end := strings.Index(t, "</user_query>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(t[end+len("</user_query>"):])
}

// extractCodeBuddyResult 解析 codebuddy 输出，兼容三种形态：
//
//  1. 单对象 envelope {"type":"result","result":"..."}
//  2. 数组：优先 type=="result"，否则拼接全部 assistant text
//  3. 解析失败原样返回
func extractCodeBuddyResult(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}

	// Shape 1: 单对象。
	var single struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if trimmed[0] == '{' {
		if err := json.Unmarshal([]byte(trimmed), &single); err == nil {
			if single.Result != "" {
				return single.Result
			}
		}
	}

	// Shape 2: stream 消息数组。
	if trimmed[0] == '[' {
		var msgs []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &msgs); err == nil {
			for _, raw := range msgs {
				var m struct {
					Type   string `json:"type"`
					Result string `json:"result"`
				}
				if json.Unmarshal(raw, &m) == nil && m.Type == "result" && m.Result != "" {
					return m.Result
				}
			}
			var sb strings.Builder
			for _, raw := range msgs {
				var probe map[string]json.RawMessage
				if json.Unmarshal(raw, &probe) != nil {
					continue
				}
				if r, ok := probe["role"]; ok && string(r) == `"assistant"` {
					if text := extractAssistantText(probe); text != "" {
						if sb.Len() > 0 {
							sb.WriteString("\n")
						}
						sb.WriteString(text)
					}
				}
				if r, ok := probe["type"]; ok && string(r) == `"assistant"` {
					if text := extractAssistantText(probe); text != "" {
						if sb.Len() > 0 {
							sb.WriteString("\n")
						}
						sb.WriteString(text)
					}
				}
			}
			if sb.Len() > 0 {
				return sb.String()
			}
		}
	}

	// Shape 3: 原样返回。
	return raw
}

// extractCodeBuddySession 从 codebuddy 输出里挖 session_id（尽力而为），
// 兼容单对象 envelope 与 stream 消息数组两种形态；找不到返回空串。
func extractCodeBuddySession(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	// Shape 1: 单对象 envelope。
	if trimmed[0] == '{' {
		var single struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(trimmed), &single) == nil {
			return single.SessionID
		}
		return ""
	}
	// Shape 2: stream 消息数组 —— 优先 type=="result" 行，其次任意带
	// session_id 的行。
	if trimmed[0] == '[' {
		var msgs []json.RawMessage
		if json.Unmarshal([]byte(trimmed), &msgs) != nil {
			return ""
		}
		fallback := ""
		for _, m := range msgs {
			var probe struct {
				Type      string `json:"type"`
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(m, &probe) != nil || probe.SessionID == "" {
				continue
			}
			if probe.Type == "result" {
				return probe.SessionID
			}
			if fallback == "" {
				fallback = probe.SessionID
			}
		}
		return fallback
	}
	return ""
}

// extractAssistantText 从单条 stream 消息里挖 assistant 可见文本。
func extractAssistantText(msg map[string]json.RawMessage) string {
	var sb strings.Builder
	var containers []json.RawMessage
	// "message" 可能是 {"content":[...]}（包一层），也可能直接是 [...]。
	if v, ok := msg["message"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(v, &inner); err == nil {
			if c, ok := inner["content"]; ok {
				containers = append(containers, c)
			}
		} else {
			containers = append(containers, v)
		}
	}
	if v, ok := msg["content"]; ok {
		containers = append(containers, v)
	}
	for _, c := range containers {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(c, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			t := b.Type
			if t == "" || t == "text" || t == "output_text" {
				if b.Text != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(b.Text)
				}
			}
		}
	}
	return sb.String()
}
