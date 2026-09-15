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
//	- 工具用 --disallowed-tool 逐个禁用（Bash/Edit/Replace/Glob/Grep/Read）。
//
// CLI 路径解析：MAGIC_AGENT_TRAE_BIN → ~/.local/bin/trae-cli →
// ~/bin/trae-cli → PATH。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

// defaultTraeBin 探测 trae-cli 路径。
func (e *TraeEngine) bin() string {
	if e.BinPath != "" {
		return e.BinPath
	}
	if env := os.Getenv("MAGIC_AGENT_TRAE_BIN"); env != "" {
		return env
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, p := range []string{
			filepath.Join(home, ".local/bin/trae-cli"),
			filepath.Join(home, "bin/trae-cli"),
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if p, err := exec.LookPath("trae-cli"); err == nil {
		return p
	}
	return ""
}

// Detect 实现 Engine。
func (e *TraeEngine) Detect() (bool, string) {
	p := e.bin()
	if p == "" {
		return false, "trae-cli not found (install trae-cli or set MAGIC_AGENT_TRAE_BIN)"
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

// Complete 实现 Engine：单次调用 trae-cli。
func (e *TraeEngine) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	bin := e.bin()
	if bin == "" {
		return Response{}, fmt.Errorf("trae CLI not found; set MAGIC_AGENT_TRAE_BIN")
	}

	prompt := FlattenPrompt(req.SystemPrompt, req.Messages, true)
	if prompt == "" {
		return Response{}, fmt.Errorf("trae: empty prompt")
	}
	// trae 没有独立 system 注入 flag；无 system prompt 时约束也要生效，
	// 直接追加到 prompt 末尾。
	if req.SystemPrompt == "" {
		prompt += noToolSuffix
	}

	args := []string{"-p"}
	// 模型：显式值 > 引擎注入值；都为空则用 CLI 自身默认。
	model := e.Model
	if model == "" {
		model = stripModelPrefix(req.Model)
	}
	if model != "" {
		args = append(args, "-c", "model.name="+model)
	}
	// 工具模式映射：trae 用 --allowed-tool / --disallowed-tool 表达。
	//	off        逐个 disallow 内置工具（Bash/Edit/Replace/Glob/Grep/Read）
	//	on         -y（yolo，跳过权限检查，全工具可用）
	//	allowlist  --allowed-tool <names> + -y
	tools := toolsOrDefault(req.Tools)
	switch {
	case tools.IsOff():
		for _, t := range []string{"Bash", "Edit", "Replace", "Glob", "Grep", "Read"} {
			args = append(args, "--disallowed-tool", t)
		}
	case tools.IsOn():
		args = append(args, "-y")
	default:
		for _, t := range tools.Allowlist() {
			args = append(args, "--allowed-tool", t)
		}
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

	stdout, stderr, err := runCLI(ctx, bin, args...)
	if err != nil {
		errMsg := truncateStr(strings.TrimSpace(stderr), 500)
		if errMsg == "" {
			errMsg = err.Error()
		}
		return Response{}, fmt.Errorf("trae CLI: %w (stderr: %s)", err, errMsg)
	}

	text := strings.TrimSpace(stdout)
	if text == "" {
		return Response{}, fmt.Errorf("trae CLI returned empty output")
	}

	modelLabel := model
	if modelLabel == "" {
		if m := TraeDefaultModel(); m != "" {
			modelLabel = m
		} else {
			modelLabel = "cli-default"
		}
	}
	return Response{
		Text:    text,
		Model:   modelLabel,
		Latency: time.Since(start),
	}, nil
}
