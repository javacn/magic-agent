package agent

// output.go - 固定输出格式。
//
// magic-agent 的输出只有两种形态，由 -o 选定，与引擎无关：
//
//	json（默认）：单行 JSON envelope，字段稳定，stdout 专为程序解析设计：
//	  成功 {"engine":"claude","model":"...","session_id":"...",
//	        "attempts":1,"latency_ms":1234,"text":"..."}
//	  失败（打到 stderr，stdout 恒为空，exit != 0）
//	       {"engine":"claude","attempts":3,"error":"...","reason":"..."}
//	text：stdout 只含模型正文 + 尾换行；失败时 stderr 一行 "magic-agent: <error>"。
//
// 失败 envelope 的两个错误字段分工：
//	error  完整错误链（含重试/引擎包装），面向人排查
//	reason 最内层根因（stderr 摘要 / 超时 / 非零退出码等），面向程序分支
//	两者都做 JSON 转义，单行可 jq。
//
// 参数类错误（usageError，exit 2）不走 WriteError：它发生在引擎选择/
// 参数解析之前，此时 -o 尚未解析，语义上没有"输出格式"可言，由
// Execute 统一以纯文本打到 stderr。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// OutputFormat 输出格式。
type OutputFormat string

const (
	FormatText OutputFormat = "text"
	FormatJSON OutputFormat = "json"
)

// ParseFormat 校验输出格式参数。
func ParseFormat(s string) (OutputFormat, error) {
	switch OutputFormat(s) {
	case FormatText, FormatJSON:
		return OutputFormat(s), nil
	default:
		return "", fmt.Errorf("invalid output format %q (want \"text\" or \"json\")", s)
	}
}

// JSONResult json 输出 envelope（成功）。
type JSONResult struct {
	Engine    string `json:"engine"`
	Model     string `json:"model"`
	SessionID string `json:"session_id,omitempty"`
	Attempts  int    `json:"attempts"`
	LatencyMS int64  `json:"latency_ms"`
	Text      string `json:"text"`
	// token 计数（尽力而为，引擎不上报时省略）。
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

// JSONError json 失败 envelope。
//
// Error 是完整错误链；Reason 是根因摘要（优先取最内层 CauseError），
// 无根因时与 Error 相同。两个都保证非空，调用方二选一消费。
type JSONError struct {
	Engine   string `json:"engine"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
	Reason   string `json:"reason"`
}

// CauseError 从错误链中提取最内层的"根因"错误。
// 引擎包装链形如 "claude: all 3 attempts failed: claude CLI: exit status 7 (stderr: some bad error)"，
// 最内层才是 CLI 的真实失败原因（超时 / 非零退出码 / stderr 摘要）。
// 支持 %w 包装与 Cause() error 接口两种形式。
func CauseError(err error) error {
	if err == nil {
		return nil
	}
	type causer interface{ Cause() error }
	// 逐层下钻，直到没有更深一层的 Cause/Unwrap。
	for {
		if c, ok := err.(causer); ok {
			if c.Cause() == nil {
				return err
			}
			err = c.Cause()
			continue
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok || u.Unwrap() == nil {
			return err
		}
		err = u.Unwrap()
	}
}

// reasonOf 提取给 reason 字段的根因文本。
// 优先取 cliCauseError 的 stderr 摘要（最接近真实原因）；
// 否则用最内层错误；都不可得时退回完整错误串。
func reasonOf(err error) string {
	if err == nil {
		return ""
	}
	// 沿链找最近的 cliCauseError（带摘要的优先）。
	var best string
	for e := err; e != nil; {
		if c, ok := e.(*cliCauseError); ok {
			if s := c.Summary(); s != "" {
				return s
			}
			if best == "" {
				best = c.cause.Error()
			}
		}
		if u, ok := e.(interface{ Unwrap() error }); ok && u.Unwrap() != nil {
			e = u.Unwrap()
			continue
		}
		if c, ok := e.(interface{ Cause() error }); ok && c.Cause() != nil {
			e = c.Cause()
			continue
		}
		break
	}
	if best != "" {
		return best
	}
	if c := CauseError(err); c != nil {
		return c.Error()
	}
	return err.Error()
}

// ReasonOf 把 reasonOf 暴露给包外（流式失败事件要用同一个根因文案 ——
// 见 internal/cli 的 writeStreamErrorJSON）。**只此一个入口**，别在包外另写一份
// 「取最内层错误」的逻辑：两处算法一旦漂移，同一次失败在 envelope 与事件流里
// 会写出两句不同的原因。
func ReasonOf(err error) string { return reasonOf(err) }

// WriteOutput 按格式把成功结果写到 w。
func WriteOutput(w io.Writer, format OutputFormat, resp Response) error {
	if format == FormatJSON {
		out := JSONResult{
			Engine:       resp.Engine,
			Model:        resp.Model,
			SessionID:    resp.SessionID,
			Attempts:     resp.Attempts,
			LatencyMS:    resp.Latency.Milliseconds(),
			Text:         resp.Text,
			InputTokens:  resp.InputTokens,
			OutputTokens: resp.OutputTokens,
			TotalTokens:  resp.TotalTokens,
		}
		data, err := json.Marshal(out)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(data))
		return err
	}
	_, err := fmt.Fprintln(w, resp.Text)
	return err
}

// WriteError 按格式把失败信息写到 w。
// json 模式输出带 error + reason 双字段的 envelope；text 模式一行纯文本。
func WriteError(w io.Writer, format OutputFormat, engine string, attempts int, err error) error {
	if err == nil {
		return nil
	}
	if format == FormatJSON {
		out := JSONError{
			Engine:   engine,
			Attempts: attempts,
			Error:    err.Error(),
			Reason:   reasonOf(err),
		}
		data, jerr := json.Marshal(out)
		if jerr != nil {
			return jerr
		}
		_, ferr := fmt.Fprintln(w, string(data))
		return ferr
	}
	_, ferr := fmt.Fprintf(w, "magic-agent: %v\n", err)
	return ferr
}

var _ = errors.Is // 保留 errors 依赖（CauseError 语义参考）
