package agent

// output.go - 固定输出格式。
//
// magic-agent 的 stdout 输出只有两种形态，由调用方选定，与引擎无关：
//
//	text（默认）：只打印 Response.Text，一行尾换行。适合管道 / 脚本拼接。
//	json：单行 JSON envelope，字段稳定：
//	  {"engine":"claude","model":"...","session_id":"...",
//	   "attempts":1,"latency_ms":1234,"text":"..."}
//	失败时 json 模式输出到 stderr 的 envelope 带 "error" 字段（exit code != 0），
//	stdout 不产生半截 JSON。

import (
	"encoding/json"
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

// JSONResult json 输出 envelope。
type JSONResult struct {
	Engine    string `json:"engine"`
	Model     string `json:"model"`
	SessionID string `json:"session_id,omitempty"`
	Attempts  int    `json:"attempts"`
	LatencyMS int64  `json:"latency_ms"`
	Text      string `json:"text"`
}

// JSONError json 失败 envelope。
type JSONError struct {
	Engine    string `json:"engine"`
	Attempts  int    `json:"attempts"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error"`
}

// WriteOutput 按格式把成功结果写到 w。
func WriteOutput(w io.Writer, format OutputFormat, resp Response) error {
	if format == FormatJSON {
		out := JSONResult{
			Engine:    resp.Engine,
			Model:     resp.Model,
			SessionID: resp.SessionID,
			Attempts:  resp.Attempts,
			LatencyMS: resp.Latency.Milliseconds(),
			Text:      resp.Text,
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

// WriteError 按格式把失败信息写到 w（json 模式输出 envelope）。
func WriteError(w io.Writer, format OutputFormat, engine string, attempts int, err error) error {
	if format == FormatJSON {
		out := JSONError{Engine: engine, Attempts: attempts, Error: err.Error()}
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
