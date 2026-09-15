package agent

// runner.go - 超时 + 重试编排。
//
// Runner 在 Engine 之上叠加跨引擎一致的调用语义：
//
//	- 每次尝试独立超时（context.WithTimeout），超时错误可重试；
//	- 可重试错误分类：网络类（connection refused / reset / EOF / timeout /
//	  signal killed）与限流类（429 / rate limit / 503 overloaded）重试，
//	  其余（参数错、鉴权错、空输出、明确非重试 CLI 错误）快速失败；
//	- 指数退避：attempt N 失败后等待 backoff * 2^(N-1)，上限 maxBackoff；
//	  重试等待可被 ctx 取消打断；
//	- 重试次数语义：retries=2 表示「最多 1 + 2 = 3 次尝试」。
//
// 使用：
//
//	r := &agent.Runner{Engine: e, Timeout: 3*time.Minute, Retries: 2}
//	resp, err := r.Run(ctx, agent.Request{...})

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"time"
)

// stderr 用于测试替换的 stderr 句柄。
var stderr io.Writer = os.Stderr

// Runner 单引擎的调用编排器。
type Runner struct {
	// Engine 目标引擎（必填）。
	Engine Engine

	// Timeout 单次尝试超时；0 = 引擎默认。
	Timeout time.Duration

	// Retries 额外重试次数（总尝试 = 1 + Retries）。
	Retries int

	// Backoff 首次重试前等待；之后指数翻倍。0 = 2s。
	Backoff time.Duration

	// MaxBackoff 退避上限。0 = 30s。
	MaxBackoff time.Duration

	// Verbose 打印重试过程到 stderr。
	Verbose bool
}

// defaultTimeout 按引擎取默认超时。
func (r *Runner) defaultTimeout() time.Duration {
	switch r.Engine.(type) {
	case *ClaudeEngine:
		return DefaultClaudeTimeout
	case *CodeBuddyEngine:
		return DefaultCodeBuddyTimeout
	case *TraeEngine:
		return DefaultTraeTimeout
	case *LLMEngine:
		return DefaultLLMTimeout
	}
	return 5 * time.Minute
}

// isRetryable 判断错误是否值得重试。
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true // 单次尝试超时 → 重新来一遍
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{
		"429", "rate limit", "rate_limit", "overloaded",
		"connection refused", "connection reset", "broken pipe",
		"eof", "timeout", "timed out",
		"signal: killed", "context deadline exceeded",
		"temporarily unavailable", "502", "503", "504",
	} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// Run 执行带超时与重试的调用。ctx 取消/超时整体打断（不算可重试）。
func (r *Runner) Run(ctx context.Context, req Request) (Response, error) {
	if r.Engine == nil {
		return Response{}, fmt.Errorf("runner: engine is nil")
	}
	if req.Engine == "" {
		req.Engine = r.Engine.Name()
	}

	timeout := r.Timeout
	if timeout == 0 {
		timeout = r.defaultTimeout()
	}
	backoff := r.Backoff
	if backoff == 0 {
		backoff = 2 * time.Second
	}
	maxBackoff := r.MaxBackoff
	if maxBackoff == 0 {
		maxBackoff = 30 * time.Second
	}

	totalAttempts := 1 + r.Retries
	var lastErr error

	for attempt := 1; attempt <= totalAttempts; attempt++ {
		// 整体 ctx 已取消 → 直接终止，不再尝试。
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return Response{}, fmt.Errorf("aborted after %d attempts: %w (last error: %v)", attempt-1, err, lastErr)
			}
			return Response{}, err
		}

		req.Timeout = timeout
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		resp, err := r.Engine.Complete(attemptCtx, req)
		cancel()

		if err == nil {
			resp.Attempts = attempt
			if resp.Engine == "" {
				resp.Engine = req.Engine
			}
			return resp, nil
		}
		lastErr = err

		if r.Verbose {
			fmt.Fprintf(stderr, "  ⏳ %s 第 %d/%d 次尝试失败: %v\n", r.Engine.Name(), attempt, totalAttempts, err)
		}

		if !isRetryable(err) {
			return Response{}, fmt.Errorf("%w (attempts=%d, non-retryable)", err, attempt)
		}
		if attempt == totalAttempts {
			break
		}

		// 指数退避 + 少量抖动，等待可被整体 ctx 打断。
		wait := backoff * time.Duration(1<<uint(attempt-1))
		if wait > maxBackoff {
			wait = maxBackoff
		}
		jitter := time.Duration(rand.Int63n(int64(wait)/4 + 1))
		wait += jitter
		if r.Verbose {
			fmt.Fprintf(stderr, "  ⏳ %s 将在 %v 后重试...\n", r.Engine.Name(), wait)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Response{}, fmt.Errorf("aborted during backoff: %w (last error: %v)", ctx.Err(), lastErr)
		case <-timer.C:
		}
	}

	return Response{}, fmt.Errorf("%s: all %d attempts failed: %w", r.Engine.Name(), totalAttempts, lastErr)
}
