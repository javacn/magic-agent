package agent

// runner_test.go - Runner 超时/重试语义测试 + output 格式测试。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// counterEngine 记录调用次数的可控假引擎。
type counterEngine struct {
	name     string
	failures int    // 前N次失败
	errText  string // 失败时的错误信息
	calls    int
	sleep    time.Duration // 每次调用耗时
}

func (c *counterEngine) Name() string           { return c.name }
func (c *counterEngine) Detect() (bool, string) { return true, "fake" }

func (c *counterEngine) Complete(ctx context.Context, req Request) (Response, error) {
	c.calls++
	if c.sleep > 0 {
		select {
		case <-time.After(c.sleep):
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
	}
	if c.calls <= c.failures {
		return Response{}, errors.New(c.errText)
	}
	return Response{Text: fmt.Sprintf("ok#%d", c.calls), Latency: time.Millisecond}, nil
}

func TestRunnerSuccessFirstTry(t *testing.T) {
	e := &counterEngine{name: "fake"}
	r := &Runner{Engine: e, Retries: 3}
	resp, err := r.Run(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if e.calls != 1 || resp.Attempts != 1 {
		t.Errorf("calls=%d attempts=%d, want 1/1", e.calls, resp.Attempts)
	}
	if resp.Text != "ok#1" {
		t.Errorf("Text=%q", resp.Text)
	}
}

func TestRunnerRetriesOnRetryableError(t *testing.T) {
	e := &counterEngine{name: "fake", failures: 2, errText: "HTTP 429: rate limit exceeded"}
	r := &Runner{Engine: e, Retries: 3, Backoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	resp, err := r.Run(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if e.calls != 3 || resp.Attempts != 3 {
		t.Errorf("calls=%d attempts=%d, want 3/3", e.calls, resp.Attempts)
	}
	if resp.Text != "ok#3" {
		t.Errorf("Text=%q", resp.Text)
	}
}

func TestRunnerFailsFastOnNonRetryable(t *testing.T) {
	e := &counterEngine{name: "fake", failures: 5, errText: "invalid model: nope"}
	r := &Runner{Engine: e, Retries: 3, Backoff: time.Millisecond}
	_, err := r.Run(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	if e.calls != 1 {
		t.Errorf("calls=%d, want 1 (fail fast)", e.calls)
	}
	if !strings.Contains(err.Error(), "non-retryable") {
		t.Errorf("error should be marked non-retryable: %v", err)
	}
}

func TestRunnerExhaustsRetries(t *testing.T) {
	e := &counterEngine{name: "fake", failures: 99, errText: "connection refused"}
	r := &Runner{Engine: e, Retries: 2, Backoff: time.Millisecond}
	_, err := r.Run(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error")
	}
	if e.calls != 3 { // 1 + 2 retries
		t.Errorf("calls=%d, want 3", e.calls)
	}
	if !strings.Contains(err.Error(), "all 3 attempts failed") {
		t.Errorf("error should mention total attempts: %v", err)
	}
}

func TestRunnerTimeoutPerAttempt(t *testing.T) {
	e := &counterEngine{name: "fake", sleep: 200 * time.Millisecond, errText: ""}
	r := &Runner{Engine: e, Timeout: 50 * time.Millisecond, Retries: 1, Backoff: time.Millisecond}
	start := time.Now()
	_, err := r.Run(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if e.calls != 2 { // 超时可重试 → 第二次也超时
		t.Errorf("calls=%d, want 2", e.calls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took too long: %v", elapsed)
	}
}

func TestRunnerCtxCancelInterrupts(t *testing.T) {
	e := &counterEngine{name: "fake", failures: 99, errText: "connection refused"}
	r := &Runner{Engine: e, Retries: 10, Backoff: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := r.Run(ctx, Request{})
	if err == nil {
		t.Fatal("expected cancel error")
	}
	if e.calls > 2 {
		t.Errorf("calls=%d, cancel should interrupt early", e.calls)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("cancel took too long: %v", elapsed)
	}
}

func TestIsRetryable(t *testing.T) {
	retryable := []string{
		"HTTP 429: too many requests",
		"rate_limit exceeded",
		"connection refused",
		"connection reset by peer",
		"unexpected EOF",
		"request timeout",
		"context deadline exceeded",
		"signal: killed",
		"HTTP 503: overloaded",
	}
	for _, msg := range retryable {
		if !isRetryable(errors.New(msg)) {
			t.Errorf("%q should be retryable", msg)
		}
	}
	nonRetryable := []string{
		"invalid model: nope",
		"unknown engine",
		"empty prompt",
		"CLI returned empty output",
		"authentication failed",
	}
	for _, msg := range nonRetryable {
		if isRetryable(errors.New(msg)) {
			t.Errorf("%q should NOT be retryable", msg)
		}
	}
}

// ── 输出格式 ──────────────────────────────────────────────────

func TestWriteOutputText(t *testing.T) {
	var buf bytes.Buffer
	resp := Response{Engine: "claude", Text: "hello\nworld", Attempts: 1}
	if err := WriteOutput(&buf, FormatText, resp); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hello\nworld\n" {
		t.Errorf("text output = %q", buf.String())
	}
}

func TestWriteOutputJSON(t *testing.T) {
	var buf bytes.Buffer
	resp := Response{
		Engine:    "codebuddy",
		Model:     "hy3",
		SessionID: "s1",
		Attempts:  2,
		Latency:   1500 * time.Millisecond,
		Text:      `带"引号"的文本`,
	}
	if err := WriteOutput(&buf, FormatJSON, resp); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`"engine":"codebuddy"`,
		`"model":"hy3"`,
		`"session_id":"s1"`,
		`"attempts":2`,
		`"latency_ms":1500`,
		`"text":"带\"引号\"的文本"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("json output missing %s: %s", want, out)
		}
	}
	if !strings.HasSuffix(out, "\n") {
		t.Error("json output should end with newline")
	}
}

func TestWriteErrorJSON(t *testing.T) {
	var buf bytes.Buffer
	err := fmt.Errorf("boom")
	if werr := WriteError(&buf, FormatJSON, "trae", 3, err); werr != nil {
		t.Fatal(werr)
	}
	out := buf.String()
	if !strings.Contains(out, `"engine":"trae"`) || !strings.Contains(out, `"attempts":3`) || !strings.Contains(out, `"error":"boom"`) {
		t.Errorf("json error envelope wrong: %s", out)
	}
}

func TestParseFormat(t *testing.T) {
	if _, err := ParseFormat("text"); err != nil {
		t.Error("text should parse")
	}
	if _, err := ParseFormat("json"); err != nil {
		t.Error("json should parse")
	}
	if _, err := ParseFormat("yaml"); err == nil {
		t.Error("yaml should fail")
	}
}

func TestRunnerNilEngine(t *testing.T) {
	r := &Runner{}
	if _, err := r.Run(context.Background(), Request{}); err == nil {
		t.Fatal("expected nil engine error")
	}
}

func TestRunnerDefaultTimeoutByEngine(t *testing.T) {
	// 各引擎默认超时
	cases := []struct {
		e    Engine
		want time.Duration
	}{
		{&ClaudeEngine{}, DefaultClaudeTimeout},
		{&CodeBuddyEngine{}, DefaultCodeBuddyTimeout},
		{&TraeEngine{}, DefaultTraeTimeout},
		{&counterEngine{name: "x"}, 5 * time.Minute},
	}
	for _, c := range cases {
		r := &Runner{Engine: c.e}
		if got := r.defaultTimeout(); got != c.want {
			t.Errorf("%s default timeout = %v want %v", c.e.Name(), got, c.want)
		}
	}
}
