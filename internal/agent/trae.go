package agent

// trae.go - Trae CLI 引擎。
//
// 后端：trae-cli（~/.local/bin/trae-cli）。非交互模式：-p，纯文本输出。
// 与 claude/codebuddy 的关键差异：
//
//	- 没有 --model flag；模型默认取 trae 自身配置（~/.trae/trae_cli.yaml
//	  的 model.name）。显式指定模型用 -c "model.name=<name>" 覆盖。
//	- --output-format 有 json，但实测纯文本已足够稳定，这里用 text +
//	  noToolSuffix（拼进 system 部分）压制 agent loop。
//	- 自带 --query-timeout，把外层超时同时传给 CLI，保证语义一致。
//	- 会话续接：--resume=<id>（pflag 可选值风格，必须等号传值）；
//	  没有 --continue，续接最近一次会话用裸 --resume（AUTO 模式，
//	  实测 -p 非交互模式下可用，能接住上一轮记忆）。
//	- 工具：禁用靠 --disallowed-tool 逐个减法；启用靠全放行开关 -y。
//	  trae 没有「只允许白名单这几个工具」的表达能力，白名单模式因此
//	  降级为「默认开启所有工具」（详见 buildArgs 的工具模式映射）。
//	- MaxTokens / Temperature 不支持：`-c <k>=<v>` 会整体覆盖模型配置块
//	  （实测注入 model.max_tokens 后模型名漂移并触发配额错误），因此这两个
//	  参数构造时静默忽略（见 engine_base.go 矩阵②）。
//
// CLI 路径解析顺序：显式 BinPath → MAGIC_AGENT_TRAE_BIN → ~/.local/bin →
// ~/bin → PATH（探测链统一收敛在 engine_base.go 的 cliBase）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TraeEngine 通过 trae-cli 实现 Engine。
type TraeEngine struct {
	// BinPath 显式指定 CLI 路径（空 = 自动探测）。测试注入用。
	BinPath string

	// Model 显式模型（非空时 -c model.name=<model> 覆盖 CLI 默认）。
	Model string
}

// Name 实现 Engine。
func (e *TraeEngine) Name() string { return "trae" }

// DefaultTraeTimeout 单次尝试默认超时。
const DefaultTraeTimeout = 10 * time.Minute

// traeOffDisableTools off 模式下逐个 --disallowed-tool 禁用的实体工具。
//
// 取自 trae-cli 实测的 18 个内置工具里「会读文件 / 会改机器」的那批：
// Agent、ExitPlanMode、EnterPlanMode 是常驻元工具（实测 --disallowed-tool
// 摘不掉，补集禁用后仍保留），Skill、TaskCreate/Get/List/Update、
// BashOutput、KillShell 属会话内辅助、无外部副作用，故不在禁用之列。
//
// 注意工具名必须是真实存在的：编辑与写入在 trae 里叫 Edit / Write ——
// 此前这里写的是并不存在的 `Replace`，等于 off 模式漏禁 Write。
var traeOffDisableTools = []string{"Bash", "Edit", "Write", "Glob", "Grep", "Read"}

// bin 探测 trae-cli 路径（委托 cliBase 统一探测链）。
func (e *TraeEngine) bin() string {
	return traeBase.resolve(e.BinPath)
}

// Detect 实现 Engine。
func (e *TraeEngine) Detect() (bool, string) {
	p := traeBase.resolve(e.BinPath)
	if p == "" {
		return false, traeBase.notFound
	}
	return true, p
}

// TraeDefaultModel 读取 ~/.trae/trae_cli.yaml 顶层 model.name，
// 返回 trae CLI 当前配置的默认模型（如 "My-MiniMax-M3"）。
// 轻量扫描解析（不引入 yaml 依赖）；文件缺失/格式变化返回空串。
func TraeDefaultModel() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".trae", "trae_cli.yaml"))
	if err != nil {
		return ""
	}
	inModelBlock := false
	for _, ln := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(ln)
		if !inModelBlock {
			inModelBlock = ln == trimmed && strings.HasPrefix(trimmed, "model:")
			continue
		}
		if strings.HasPrefix(trimmed, "name:") {
			v := strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
			v = strings.Trim(v, `"'`)
			if v != "" {
				return v
			}
		}
		if ln == trimmed && trimmed != "" {
			inModelBlock = false
		}
	}
	return ""
}

// ListModels 实现 ModelLister：`trae-cli models --json` 是 trae 自己的
// 权威清单（含用户自定义模型，如 ~/.trae/trae_cli.yaml 里的 My-MiniMax-M3）。
// 取 name（= -c model.name=<name> 接受的那个标识），缺失时退回 real_name。
func (e *TraeEngine) ListModels(ctx context.Context) ([]string, error) {
	stdout, err := probeCLI(ctx, "trae", e.bin(), "models", "--json")
	if err != nil {
		return nil, err
	}
	objs, err := jsonModelsArray(stdout)
	if err != nil {
		return nil, err
	}
	models := modelNamesFromObjects(objs, "name", "real_name")
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: trae-cli models --json returned no entries", ErrNoModelSource)
	}
	return models, nil
}

// Complete 实现 Engine：单次调用 trae-cli。
func (e *TraeEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return Response{}, fmt.Errorf("trae CLI not found; set MAGIC_AGENT_TRAE_BIN")
	}

	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, toolsIsOff(req))
	if prompt == "" {
		return Response{}, fmt.Errorf("trae: empty prompt")
	}
	// trae 没有独立 system 注入 flag；无 system prompt 时约束也要生效。
	// 仅 off 模式追加：on/白名单下 noToolSuffix 会与「允许调用工具」冲突。
	if req.SystemPrompt == "" && toolsIsOff(req) {
		prompt += noToolSuffix
	}
	// 附件：trae-cli 没有图片/附件输入通道（--help 无 image/attach/input-format），
	// 只能把绝对路径写进提示词，靠 trae 的读文件工具看 —— 因此需要工具可用。
	prompt = appendAttachmentSection(prompt, req.Attachments)

	args := e.buildArgs(req, prompt)

	// workspace：trae 无工作目录 flag（有 --add-dir 但那是追加额外目录），子进程 cwd 即原生方式
	stdout, stderr, err := runCLIIn(ctx, req.Workspace, bin, args...)
	if err != nil {
		return Response{}, wrapCliError("trae", stdout, stderr, err)
	}

	text := strings.TrimSpace(stdout)
	if text == "" {
		return Response{}, fmt.Errorf("trae CLI returned empty output")
	}

	modelLabel := stripModelPrefix(req.Model)
	if modelLabel == "" {
		modelLabel = e.Model
	}
	if modelLabel == "" {
		if m := TraeDefaultModel(); m != "" {
			modelLabel = m
		} else {
			modelLabel = "cli-default"
		}
	}
	return Response{
		Text:  text,
		Model: modelLabel,
		// 纯文本模式下 CLI 不回 session_id；续接请求时把传入的 id 原样
		// 回传，让调用方可以逐轮透传（新会话仍为空）。
		SessionID: req.SessionID,
		Latency:   time.Since(start),
	}, nil
}

// buildArgs 构造 trae-cli 参数（Complete 与 Stream 共用）。
func (e *TraeEngine) buildArgs(req Request, prompt string) []string {
	args := []string{"-p"}
	switch {
	case req.SessionID != "":
		// 续接指定会话：pflag 可选值风格（--resume string[="AUTO"]），
		// 必须用等号形式 --resume=<id>；空格形式下裸 id 会被吞成 AUTO 默认值。
		args = append(args, "--resume="+req.SessionID)
	case req.Continue:
		// 续接最近一次会话：裸 --resume 触发 AUTO 模式（实测 -p 可用）。
		args = append(args, "--resume")
	}
	// 模型：显式值 > 引擎注入值；都为空则用 CLI 自身默认。
	model := e.Model
	if model == "" {
		model = stripModelPrefix(req.Model)
	}
	if model != "" {
		args = append(args, "-c", "model.name="+model)
	}
	// 统一参数矩阵：req.MaxTokens / req.Temperature 在 trae 上明确不支持、
	// 静默忽略 —— `-c <k>=<v>` 会整体覆盖模型配置块，实测注入
	// model.max_tokens 后模型名漂移并触发配额错误（见 engine_base.go 矩阵②）。
	_ = req.MaxTokens
	_ = req.Temperature
	// 工具模式映射。trae-cli 的权限开关实测语义（2026-09-16 真机验证）：
	//
	//	--allowed-tool    只做「自动批准该工具」，**不裁剪**工具集
	//	                  （实测 --allowed-tool WebFetch 仍下发全部 18 个工具）
	//	--disallowed-tool 真正做减法，从工具集里移除该工具（实测 18 → 4）
	//	-y / --yolo       跳过全部权限检查 ⇒ 全工具可用
	//
	// 由此得出映射：
	//
	//	off        逐个 --disallowed-tool 关掉内置工具（Bash/Edit/Write/Glob/Grep/Read）
	//	on         -y（全放行；非 -p 交互场景下的权限旁路）
	//	allowlist  默认开启所有工具 ⇒ 与 on 同一条全放行路径（-y）
	//
	// ⚠️ 白名单在 trae 上「降级为全放行」是刻意的默认语义，不是实现漏做：
	// trae 无法表达「只允许这几个工具」——想收窄只能对补集逐个 --disallowed-tool，
	// 而补集随 CLI 版本漂移（今天 18 个工具、明天可能加），漏一条就等于白名单失效，
	// 维护成本高于收益。故认定「默认开启所有工具」为白名单模式在 trae 上的契约，
	// 与 codex 的白名单降级（engine_base.go 矩阵④：白名单 → 全放行）保持一致。
	//
	// 返回值里不再出现 --allowed-tool：既然 -y 已全量放行，逐条声明只是
	// 「看起来像收窄」的噪音（这正是此前 allowlist 被 -y 架空的观感来源）。
	// 需要收窄时请用 off 模式（真减法），而不要指望白名单。
	tools := toolsOrDefault(req.Tools)
	switch {
	case tools.IsOff():
		// 真实工具名，逐个减法；`Replace` 不是 trae 的工具（编辑/写入是
		// Edit / Write），此前写成 Replace 等于漏禁 Write。
		for _, t := range traeOffDisableTools {
			args = append(args, "--disallowed-tool", t)
		}
	case tools.IsOn():
		args = append(args, "-y")
	default:
		// 默认开启所有工具：全放行开关是唯一有效机制。
		args = append(args, "-y")
	}
	// 把外层超时透传给 CLI 的 query-timeout（取上限 600s，CLI 单查询上限）。
	if req.Timeout > 0 {
		qt := req.Timeout
		if qt > 600*time.Second {
			qt = 600 * time.Second
		}
		args = append(args, "--query-timeout", qt.String())
	} else {
		args = append(args, "--query-timeout", "600s")
	}
	args = append(args, prompt)
	return args
}

// Stream 实现 Streamer：流式调用 trae-cli。
// trae 的 stream-json 用 delta.content 直出正文增量；
// 模型侧不开 reasoning，无 thinking 通道（Thinking 恒为空）。
func (e *TraeEngine) Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return StreamResult{}, fmt.Errorf("trae CLI not found; set MAGIC_AGENT_TRAE_BIN")
	}

	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, toolsIsOff(req))
	if prompt == "" {
		return StreamResult{}, fmt.Errorf("trae: empty prompt")
	}
	if req.SystemPrompt == "" && toolsIsOff(req) {
		prompt += noToolSuffix
	}
	prompt = appendAttachmentSection(prompt, req.Attachments)

	args := e.buildArgs(req, prompt)
	args = append(args, "--output-format", "stream-json", "--include-partial-messages")

	acc := &streamAccumulator{Engine: e.Name(), OnEvent: onEvent}
	var fin struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		IsError   bool   `json:"is_error"`
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
	}
	seenResult := false

	err := runStreamCLIIn(ctx, req.Workspace, bin, args, func(line string) error {
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
		return StreamResult{}, fmt.Errorf("trae CLI stream ended without result line")
	}
	if fin.IsError {
		return StreamResult{}, fmt.Errorf("trae CLI error (subtype=%s): %s", fin.Subtype, truncateStr(fin.Result, 500))
	}

	text := fin.Result
	if text == "" {
		text = strings.TrimSpace(acc.Text.String())
	}
	if text == "" {
		return StreamResult{}, fmt.Errorf("trae CLI returned empty result")
	}

	modelLabel := stripModelPrefix(req.Model)
	if modelLabel == "" {
		modelLabel = e.Model
	}
	if modelLabel == "" {
		if m := TraeDefaultModel(); m != "" {
			modelLabel = m
		} else {
			modelLabel = "cli-default"
		}
	}
	return StreamResult{
		Response: Response{
			Engine:    e.Name(),
			Text:      text,
			Model:     modelLabel,
			SessionID: fin.SessionID,
			Latency:   time.Since(start),
		},
		Thinking: acc.Thinking.String(),
		// trae 协议不暴露独立工具事件通道；Tools 恒为 nil。
		// 见 stream.go 顶部协议说明。
	}, nil
}
