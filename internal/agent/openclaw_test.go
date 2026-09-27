package agent

// openclaw_test.go - OpenClaw 引擎单元测试。
//
// 覆盖：注册表、buildArgs 关键 flag 映射、envelope 解析(plugin 日志混入场景)、
// Continue 走 sessions --json 查 sessionId、max-tokens/temperature/tools 静默忽略、
// CLI 缺失报错。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenClawLookup(t *testing.T) {
	e := Lookup("openclaw")
	if e == nil {
		t.Fatal("Lookup(\"openclaw\") = nil")
	}
	if e.Name() != "openclaw" {
		t.Errorf("Name() = %q want openclaw", e.Name())
	}
}

func TestOpenClawBuildArgsNewSession(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "openclaw", `echo '{"payloads":[{"text":"hi"}],"meta":{"agentMeta":{"sessionId":"s1","model":"m1"}}}'`)
	e := &OpenClawEngine{BinPath: w.bin, Agent: "main"}
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	for _, want := range []string{
		"agent",
		"--local",
		"--agent main",
		"--json",
		"hi", // FlattenPrompt 后正文是 "【用户】\nhi",至少含 "hi"
		"--timeout 600",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("want %q in args, got %q", want, args)
		}
	}
	if !strings.Contains(args, "--message ") {
		t.Errorf("--message flag 应被使用: %q", args)
	}
}

func TestOpenClawBuildArgsModel(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "openclaw", `echo '{"payloads":[{"text":"hi"}]}'`)
	e := &OpenClawEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		Model:    "openclaw/deepseek-v4-pro",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "--model deepseek-v4-pro") {
		t.Errorf("model flag missing: %q", string(data))
	}
}

func TestOpenClawResumeSession(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "openclaw", `echo '{"payloads":[{"text":"ok"}],"meta":{"agentMeta":{"sessionId":"sess-1"}}}'`)
	e := &OpenClawEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		SessionID: "sess-1",
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "--session-id sess-1") {
		t.Errorf("--session-id sess-1 not passed: %q", string(data))
	}
}

func TestOpenClawContinueResolvesLatest(t *testing.T) {
	// 第一次调用:sessions --json 拿最近一次 sessionId
	// 第二次调用:agent --session-id <id>
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	cli := filepath.Join(dir, "openclaw")
	script := `#!/bin/sh
# 第一次调用是 sessions,其余都是 agent
if [ "$1" = "sessions" ]; then
  echo '{"path":"/x","count":1,"totalCount":1,"limitApplied":1,"hasMore":false,"sessions":[{"key":"k1","updatedAt":1,"sessionId":"sess-from-sessions","model":"m1"}]}'
  exit 0
fi
printf '%s\n' "$*" > ` + log + `
echo '{"payloads":[{"text":"ok"}],"meta":{"agentMeta":{"sessionId":"sess-from-sessions","model":"m1"}}}'
`
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &OpenClawEngine{BinPath: cli}
	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Continue: true,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.SessionID != "sess-from-sessions" {
		t.Errorf("SessionID = %q want sess-from-sessions", resp.SessionID)
	}
	data, _ := os.ReadFile(log)
	// agent 调用时 args 应包含 --session-id sess-from-sessions
	if !strings.Contains(string(data), "--session-id sess-from-sessions") {
		t.Errorf("agent 阶段未传续接 id: %q", string(data))
	}
}

func TestOpenClawParseStdoutWithPluginLogs(t *testing.T) {
	// 模拟 plugin 日志污染 stdout 的场景
	stdout := strings.Join([]string{
		`[plugins] openviking: resolveAgentId ...`,
		`[plugins] openviking: diag ...`,
		`[model-fallback/decision] model fallback decision: ...`,
		`{"payloads":[{"text":"pong","mediaUrl":null}],"meta":{"durationMs":1234,"agentMeta":{"sessionId":"sid-1","provider":"papergames","model":"deepseek-v4-pro","usage":{"input":100,"output":10,"cacheRead":0,"cacheWrite":0,"total":110}},"aborted":false}}`,
		``, // 末尾空行
	}, "\n")
	e := &OpenClawEngine{}
	resp, err := e.parseStdout(stdout)
	if err != nil {
		t.Fatalf("parseStdout: %v", err)
	}
	if resp.Text != "pong" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.SessionID != "sid-1" {
		t.Errorf("SessionID = %q", resp.SessionID)
	}
	if resp.Model != "deepseek-v4-pro" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.OutputTokens != 10 {
		t.Errorf("OutputTokens = %d", resp.OutputTokens)
	}
	if resp.TotalTokens != 110 {
		t.Errorf("TotalTokens = %d", resp.TotalTokens)
	}
}

func TestOpenClawParseStdoutShortOutput(t *testing.T) {
	// 整段就是 JSON 的"短输出"路径
	stdout := `{"payloads":[{"text":"短回答"}],"meta":{"agentMeta":{"sessionId":"sid-2","model":"m2","usage":{"input":1,"output":1,"total":2}}}}`
	e := &OpenClawEngine{}
	resp, err := e.parseStdout(stdout)
	if err != nil {
		t.Fatalf("parseStdout: %v", err)
	}
	if resp.Text != "短回答" {
		t.Errorf("Text = %q", resp.Text)
	}
}

func TestOpenClawParseStdoutNoEnvelope(t *testing.T) {
	e := &OpenClawEngine{}
	if _, err := e.parseStdout("only logs, no JSON\nmore logs\n"); err == nil {
		t.Error("expected error when envelope missing")
	}
}

func TestOpenClawMaxTokensIgnored(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "openclaw", `echo '{"payloads":[{"text":"ok"}]}'`)
	e := &OpenClawEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: 4096,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(strings.ToLower(string(data)), "max_tokens") {
		t.Errorf("MaxTokens 应静默忽略: %q", string(data))
	}
}

func TestOpenClawTemperatureIgnored(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "openclaw", `echo '{"payloads":[{"text":"ok"}]}'`)
	e := &OpenClawEngine{BinPath: w.bin}
	temp := 0.7
	if _, err := e.Complete(context.Background(), Request{
		Messages:    []Message{{Role: "user", Content: "hi"}},
		Temperature: &temp,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(strings.ToLower(string(data)), "temperature") {
		t.Errorf("temperature 应静默忽略: %q", string(data))
	}
}

func TestOpenClawToolsIgnored(t *testing.T) {
	// ToolsMode 任意值都不应改变 args(openclaw 内嵌 agent,自身决定工具使用)
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "openclaw", `echo '{"payloads":[{"text":"ok"}]}'`)
	e := &OpenClawEngine{BinPath: w.bin}
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    ToolsOn, // 即便 on 也不应改 args
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	for _, forbid := range []string{"--tools", "--disallowed-tool", "--allowed-tool", "--dangerously-skip-permissions"} {
		if strings.Contains(args, forbid) {
			t.Errorf("openclaw 不应透传工具 flag %s: %q", forbid, args)
		}
	}
}

func TestOpenClawNotFound(t *testing.T) {
	e := &OpenClawEngine{BinPath: "/nonexistent/openclaw"}
	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for missing CLI")
	}
}

func TestOpenClawWriteMessageArgShortUsesInline(t *testing.T) {
	dir := t.TempDir()
	args, cleanup, err := writeMessageArg(dir, "短文本")
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		t.Error("短文本不该返回 cleanup")
	}
	if len(args) != 2 || args[0] != "--message" || args[1] != "短文本" {
		t.Errorf("短文本应走 --message 内联, got %v", args)
	}
}

func TestOpenClawWriteMessageArgLongUsesFile(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 5000) // > 4KB 阈值
	args, cleanup, err := writeMessageArg(dir, long)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Error("长文本必须返回 cleanup")
	} else {
		cleanup()
	}
	if len(args) != 2 || args[0] != "--message-file" {
		t.Errorf("长文本应走 --message-file, got %v", args)
	}
	if !strings.HasPrefix(args[1], dir) {
		t.Errorf("file path 应在 tmpDir 下, got %q", args[1])
	}
}

func TestOpenClawEnvelopeRoundTrip(t *testing.T) {
	// 通过 JSON 序列化/反序列化验证 envelope 结构稳定
	raw := `{"payloads":[{"text":"abc","mediaUrl":null}],"meta":{"durationMs":1,"agentMeta":{"sessionId":"s","model":"m","usage":{"input":2,"output":3,"total":5}},"aborted":false}}`
	var env openclawEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	resp := envelopeToResponse(env)
	if resp.Text != "abc" || resp.SessionID != "s" || resp.Model != "m" {
		t.Errorf("resp = %+v", resp)
	}
	if resp.InputTokens != 2 || resp.OutputTokens != 3 || resp.TotalTokens != 5 {
		t.Errorf("tokens = %+v", resp)
	}
}

/* ── 嵌入式凭据失败 → 回退 Gateway（2026-09-23）──
 *
 * 现场（用户报障「openclaw 的引擎没对接好 不显示」的下半场）：`--local` 嵌入式路径的
 * provider key 只从 **shell 环境变量**取，`openclaw.json` 里配好的 key 在这条路上用不上
 * → 「模型只配在 openclaw.json」的用户必然 `401 invalid api key (2049)`。
 * 桩复刻这个形状：**带 `--local` 就 401、不带就正常出 envelope**。 */

// openclawAuthStub 造一个「--local 必 401、Gateway 路径正常」的桩，返回 bin 与调用日志路径。
// ⚠️ 日志把 argv 里的换行压成空格（`tr '\n' ' '`）：prompt 经 FlattenPrompt 后自带换行，
// 不压平会让「一次调用」在日志里占两行 —— 按行数断言就必然假红（实测踩过）。
func openclawAuthStub(t *testing.T, dir string) (bin, log string) {
	t.Helper()
	log = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "openclaw")
	script := `#!/bin/sh
printf '%s\n' "$*" | tr '\n' ' ' >> ` + log + `
echo >> ` + log + `
case "$*" in
  *--local*)
    echo 'FailoverError: Authentication failed (provider returned HTTP 401). detail=401 invalid api key (2049)' >&2
    exit 1
    ;;
esac
echo '{"payloads":[{"text":"收到"}],"meta":{"agentMeta":{"sessionId":"gw-1","model":"minimax/MiniMax-M3"}}}'
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestOpenClawAuthFailureFallsBackToGateway(t *testing.T) {
	dir := t.TempDir()
	bin, log := openclawAuthStub(t, dir)
	e := &OpenClawEngine{BinPath: bin, Agent: "main"}
	resp, err := e.Complete(context.Background(), Request{
		Model:    "minimax/MiniMax-M3",
		Messages: []Message{{Role: "user", Content: "只回两个字：收到"}},
	})
	if err != nil {
		t.Fatalf("Complete 应回退 Gateway 后成功, got err = %v", err)
	}
	if resp.Text != "收到" {
		t.Errorf("Text = %q want 收到", resp.Text)
	}
	if resp.SessionID != "gw-1" {
		t.Errorf("SessionID = %q want gw-1（取自 Gateway 那次 envelope）", resp.SessionID)
	}
	data, _ := os.ReadFile(log)
	var calls []string
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		// 桩用 `tr '\n' ' '` 压平 argv，末尾会留一个空格（被换掉的那个换行）→ 统一去掉再比。
		calls = append(calls, strings.TrimRight(strings.TrimRight(ln, "\r"), " "))
	}
	if len(calls) != 2 {
		t.Fatalf("应恰好两次调用（嵌入式 → Gateway），got %d: %q", len(calls), calls)
	}
	if !strings.Contains(calls[0], "--local") {
		t.Errorf("第 1 次应走嵌入式（带 --local）: %q", calls[0])
	}
	if strings.Contains(calls[1], "--local") {
		t.Errorf("第 2 次应走 Gateway（去掉 --local）: %q", calls[1])
	}
	// 两次的**其余参数必须完全一致**（只差一个 flag）—— 否则回退会把模型 / 会话丢掉。
	if strings.Replace(calls[0], " --local", "", 1) != calls[1] {
		t.Errorf("回退时除 --local 外不该有别的差异:\n  嵌入式: %q\n  Gateway: %q", calls[0], calls[1])
	}
}

func TestOpenClawNonAuthFailureDoesNotRetry(t *testing.T) {
	// 非凭据类失败（超时 / 会话接不上）**不许**重试：换条路也一样失败，白多一次往返。
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, "openclaw")
	script := `#!/bin/sh
printf '%s\n' "$*" | tr '\n' ' ' >> ` + log + `
echo >> ` + log + `
echo 'openclaw: 会话已过期，无法续接' >&2
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &OpenClawEngine{BinPath: bin}
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("应报错")
	}
	data, _ := os.ReadFile(log)
	if n := len(strings.Split(strings.TrimSpace(string(data)), "\n")); n != 1 {
		t.Errorf("非凭据类失败只该调一次, got %d 次: %q", n, string(data))
	}
}

/* ── Gateway envelope 多包一层 result（2026-09-23）──
 * 实测：`--local` 是顶层 `payloads`，Gateway 是 `{runId,status,summary,result:{payloads,meta}}`。
 * 只认前者的话，回退 Gateway 明明拿到了正文却报「未找到有效 JSON envelope」。 */
func TestOpenClawGatewayEnvelopeShape(t *testing.T) {
	// 复刻实测形状：pretty JSON + 前面混 plugin 日志 + 后面还有别的输出
	stdout := `[plugins] openviking: loaded plugin config
{
  "runId": "e0dda575-3a50-482d-9ab0-e6df7f9db86a",
  "status": "ok",
  "summary": "completed",
  "result": {
    "payloads": [ { "text": "收到", "mediaUrl": null } ],
    "meta": { "durationMs": 1479, "agentMeta": {
      "sessionId": "ff91a857-0d63-4ebc-ad81-6f03dfdc8427",
      "provider": "minimax", "model": "MiniMax-M3" } }
  }
}
[state-migrations] 尾部噪声`
	e := &OpenClawEngine{}
	resp, err := e.parseStdout(stdout)
	if err != nil {
		t.Fatalf("Gateway 形状应能解析, got err = %v", err)
	}
	if resp.Text != "收到" {
		t.Errorf("Text = %q want 收到", resp.Text)
	}
	if resp.SessionID != "ff91a857-0d63-4ebc-ad81-6f03dfdc8427" {
		t.Errorf("SessionID = %q（应取自 result.meta.agentMeta）", resp.SessionID)
	}
	if resp.Model != "MiniMax-M3" {
		t.Errorf("Model = %q want MiniMax-M3", resp.Model)
	}
}

func TestOpenClawGatewayStatusErrorSurfaces(t *testing.T) {
	// Gateway 明确失败（status != ok）→ 错误里要带 status/summary，不许只说「找不到 envelope」
	stdout := `{"runId":"x","status":"error","summary":"provider 鉴权失败"}`
	if _, err := (&OpenClawEngine{}).parseStdout(stdout); err == nil {
		t.Fatal("应报错")
	} else if !strings.Contains(err.Error(), "status=error") || !strings.Contains(err.Error(), "provider 鉴权失败") {
		t.Errorf("错误应带 status 与 summary, got %q", err.Error())
	}
}

func TestIsOpenClawAuthFailure(t *testing.T) {
	yes := []string{
		"FailoverError: Authentication failed (provider returned HTTP 401).",
		"401 invalid api key (2049)",
		"error: Unauthorized",
		"your provider token may have expired — re-authenticate this provider",
	}
	for _, s := range yes {
		if !isOpenClawAuthFailure("", s) {
			t.Errorf("应判为凭据失败: %q", s)
		}
	}
	no := []string{
		"openclaw: 会话已过期，无法续接",
		"context deadline exceeded",
		"gateway connect failed: connection refused",
		"usage: input=1401 output=22", // 1401 里的 401 不是状态码
	}
	for _, s := range no {
		if isOpenClawAuthFailure("", s) {
			t.Errorf("不该判为凭据失败: %q", s)
		}
	}
}
