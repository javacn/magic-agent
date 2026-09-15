package agent

// stream.go - 流式调用抽象（含思考过程）。
//
// Streamer 是 Engine 的流式形态：CLI 以 stream-json 逐行输出，
// magic-agent 实时解析并把两类增量转发给回调：
//
//	KindThinking — 模型思考过程（reasoning / thinking delta）
//	KindText     — 正文增量
//
// 流式模式语义与 Complete 不同，刻意不共享 Runner：
//	- 超时仍然生效（每次尝试独立进程组，超时杀整组）；
//	- 不做自动重试——增量已实时发往 stdout，重放会造成重复消费；
//	  需要重试语义的调用方用非流式 Run。
//
// 三家 CLI 的流式协议（2026-09-15 实测）：
//
//	claude / codebuddy（同源 CodeBuddy Code 系）：
//	  --output-format stream-json --include-partial-messages --verbose
//	  NDJSON 行 {"type":"stream_event","event":{"type":"content_block_delta",
//	  "delta":{"type":"thinking_delta","thinking":"..."}}} / text_delta
//	  收尾 {"type":"result","subtype":"success","result":"全文",...}
//
//	trae：
//	  -p --output-format stream-json --include-partial-messages
//	  NDJSON 行 {"type":"stream_event","delta":{"role":"assistant",
//	  "content":"增量"}}；无 thinking 通道（模型侧不开 reasoning）
//	  收尾 {"type":"result","subtype":"success","result":"全文",...}

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// StreamEventKind 增量类型。
type StreamEventKind string

const (
	// KindThinking 思考过程增量。
	KindThinking StreamEventKind = "thinking"
	// KindText 正文增量。
	KindText StreamEventKind = "text"
)

// StreamEvent 一条流式增量。
type StreamEvent struct {
	Kind StreamEventKind
	Text string
}

// StreamResult 流式调用的收尾汇总。
type StreamResult struct {
	Response
	// Thinking 完整思考过程（各增量拼接）。
	Thinking string
}

// Streamer 流式引擎接口。实现负责单次流式尝试；
// 超时由 StreamRunner 外层控制。
type Streamer interface {
	// Stream 发起一次流式调用，实时把增量写入 onEvent，
	// 返回收尾结果（含全文与思考过程）。
	Stream(ctx context.Context, req Request, onEvent func(StreamEvent)) (StreamResult, error)
}

// AsStreamer 把 Engine 升级为 Streamer（不支持流式的引擎返回 nil）。
func AsStreamer(e Engine) Streamer {
	s, _ := e.(Streamer)
	return s
}

// streamArgsFor 返回引擎的流式支持标记（用于 SupportsStream 快速判定）。
// 返回 nil 表示该引擎不支持流式。
func streamArgsFor(e Engine) []string {
	switch e.(type) {
	case *ClaudeEngine:
		return []string{"stream-json"}
	case *CodeBuddyEngine:
		return []string{"stream-json"}
	case *TraeEngine:
		return []string{"stream-json"}
	case *LLMEngine:
		return []string{"sse"} // openai-completions SSE / CLI 委托；ollama 运行时报错
	}
	return nil
}

// SupportsStream 报告引擎是否支持流式。
func SupportsStream(e Engine) bool {
	return streamArgsFor(e) != nil && AsStreamer(e) != nil
}

// runStreamCLI 启动 CLI 流式进程，逐行回调，收尾返回全文与思考。
// lineHandler 返回非 nil error 时立即杀进程组并中止（解析致命错误）。
func runStreamCLI(ctx context.Context, bin string, args []string, lineHandler func(line string) error) error {
	cmd := newStreamCmd(bin, args)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &streamStderrBuf{}
	if err := cmd.Start(); err != nil {
		return err
	}

	// 看门狗：ctx 结束 → 杀整组；否则等进程自然退出。
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdoutPipe)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // 16MB 行上限（result 行可能很大）
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				if err := lineHandler(line); err != nil {
					killProcessGroup(cmd)
					<-done // 等回收 goroutine（下方写入）
					return
				}
			}
		}
		if err := sc.Err(); err != nil {
			done <- fmt.Errorf("read stream: %w", err)
			return
		}
		done <- nil
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case err := <-done:
		<-waitErr // 回收
		return err
	case err := <-waitErr:
		// 进程先退（正常退出时 reader 也会很快结束；先等 reader 把余量读完）
		if err == nil {
			// 正常退出 → 等 reader 收尾（有限等待防挂死）。
			select {
			case rerr := <-done:
				<-waitErr
				return rerr
			case <-time.After(10 * time.Second):
				killProcessGroup(cmd)
				return fmt.Errorf("stream reader did not finish after process exit")
			}
		}
		killProcessGroup(cmd)
		<-done
		<-waitErr
		return fmt.Errorf("cli exited: %w", err)
	case <-ctx.Done():
		killProcessGroup(cmd)
		<-done
		<-waitErr
		return fmt.Errorf("process group killed: %w", ctx.Err())
	}
}

// streamAccumulator 聚合增量与收尾 result 行。
type streamAccumulator struct {
	Text     strings.Builder
	Thinking strings.Builder
	OnEvent  func(StreamEvent)
}

// handleNDJSONLine 解析一行 NDJSON 流事件，返回是否为 result 收尾行。
// 兼容 claude/codebuddy（content_block_delta）与 trae（delta.content）两种族谱。
func (a *streamAccumulator) handleNDJSONLine(line string) (isResult bool, err error) {
	var probe struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if jerr := json.Unmarshal([]byte(line), &probe); jerr != nil {
		return false, nil // 非 JSON 行（杂讯）忽略
	}

	switch probe.Type {
	case "result":
		return true, nil // 全文在 result.result，由调用方自行解析

	case "stream_event":
		// claude / codebuddy: event.content_block_delta.delta.{thinking_delta,text_delta}
		var se struct {
			Event struct {
				Type         string `json:"type"`
				ContentBlock struct {
					Type string `json:"type"`
				} `json:"content_block"`
				Delta struct {
					Type     string `json:"type"`
					Thinking string `json:"thinking"`
					Text     string `json:"text"`
				} `json:"delta"`
			} `json:"event"`
			// trae: delta.content 直出增量
			Delta2 struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"delta"`
		}
		if jerr := json.Unmarshal([]byte(line), &se); jerr != nil {
			return false, nil
		}
		if d := se.Event.Delta; d.Thinking != "" {
			a.emit(KindThinking, d.Thinking)
		} else if d.Text != "" && se.Event.Type == "content_block_delta" {
			// 仅当 content_block 类型是 text 时才算正文增量——
			// thinking 块的 text 字段不会出现，但 input_json_delta
			//（工具参数流）的 partial_json 字段名不同，天然不误触。
			if se.Event.ContentBlock.Type == "" || se.Event.ContentBlock.Type == "text" {
				a.emit(KindText, d.Text)
			}
		}
		if c := se.Delta2.Content; c != "" && se.Delta2.Role == "assistant" {
			a.emit(KindText, c)
		}

	case "assistant":
		// 非 partial 的整条 assistant 消息（trae 在 stream_event 之外还会
		// 发一份聚合行；claude 也会）。增量已在 stream_event 覆盖，
		// 这里不重复 emit，只做兜底：全文以 result 行为准。
	}
	return false, nil
}

// emit 聚合并转发一条增量。
func (a *streamAccumulator) emit(kind StreamEventKind, text string) {
	switch kind {
	case KindThinking:
		a.Thinking.WriteString(text)
	case KindText:
		a.Text.WriteString(text)
	}
	if a.OnEvent != nil {
		a.OnEvent(StreamEvent{Kind: kind, Text: text})
	}
}
