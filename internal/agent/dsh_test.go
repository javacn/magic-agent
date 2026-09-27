package agent

// dsh_test.go - DeepSeek Harness（dsh）引擎单元测试。
//
// 覆盖两条通道：
//
//	SDK（默认）  dsh_sdk_test.go —— initialize 握手、session.event 解析（推理/正文/
//	             工具调用/工具结果/turn 收尾）、模型路由解析、服务端拒绝与回退分流
//	headless     本文件 —— 参数形态（--profile headless <prompt>）、stdout 取正文 /
//	             stderr 推理增量不混入、会话续接显式报错、JSONSchema 后处理、
//	             workspace 走子进程 cwd、无模型清单来源、settings.yaml 解析
//
// ⚠️ 本文件里走 headless 的用例一律显式 `Profile: "headless"`（关掉 SDK 通道）：
// 默认（Profile 空）会先试 SDK，假 CLI 说不了 JSON-RPC 就回退，会多起一次进程
// 并打一行回退告警，把「headless 行为」的断言搅浑。默认路径的行为由
// dsh_sdk_test.go 专门覆盖。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDshLookup(t *testing.T) {
	e := Lookup("dsh")
	if e == nil {
		t.Fatal(`Lookup("dsh") = nil`)
	}
	if e.Name() != "dsh" {
		t.Errorf("Name() = %q want dsh", e.Name())
	}
	if Lookup("DSH") == nil {
		t.Error(`Lookup("DSH") = nil, want case-insensitive match`)
	}
}

func TestDshIsEngine(t *testing.T) {
	var e Engine = &DshEngine{}
	if e.Name() != "dsh" {
		t.Errorf("Name() = %q", e.Name())
	}
}

// dsh 实现了 Streamer（默认 SDK 通道全流式，回退 headless 后流推理）；支持常驻会话
// （SDK 通道下对同一会话继续 prompt），但不支持按 id 续接（-s / -c）。
func TestDshCapabilities(t *testing.T) {
	if s := AsStreamer(&DshEngine{}); s == nil {
		t.Error("dsh 应实现 Streamer（SDK 通道的 session.event / headless 的 stderr 推理增量）")
	}
	if !SupportsStream(&DshEngine{}) {
		t.Error("SupportsStream(dsh) 应为 true")
	}
	if !AppendSupportOf("dsh") {
		t.Error("dsh 应支持常驻会话（--keep-alive/--append）：SDK 通道下同一会话可继续 prompt")
	}
	if AppendDefaultOn("dsh") {
		t.Error("dsh 的常驻会话应默认关（需显式 --keep-alive），否则既有 --stream 调用会挂 5 分钟空闲窗口")
	}
	if got := WorkspaceSupportOf("dsh"); got != "cwd" {
		t.Errorf("WorkspaceSupportOf(dsh) = %q want cwd（调用目录即 workspace 根）", got)
	}
	if got := AttachmentSupportOf("dsh"); got != "prompt" {
		t.Errorf("AttachmentSupportOf(dsh) = %q want prompt", got)
	}
	if got := PermissionSupportOf("dsh"); got != "none" {
		t.Errorf("PermissionSupportOf(dsh) = %q want none（预设无法表达四档）", got)
	}
	if got := AskSupportOf("dsh"); got != "none" {
		t.Errorf("AskSupportOf(dsh) = %q want none", got)
	}
}

func TestDshDetectWithBinPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "dsh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ok, note := (&DshEngine{BinPath: bin}).Detect()
	if !ok {
		t.Fatalf("Detect() = false, note=%q", note)
	}
	if note != bin {
		t.Errorf("Detect() note = %q want %q", note, bin)
	}
}

func TestDshDetectMissing(t *testing.T) {
	// 显式给一个不存在的候选目录 + 清掉环境变量与 PATH，探测应失败。
	t.Setenv("MAGIC_AGENT_DSH_BIN", "")
	t.Setenv("PATH", t.TempDir())
	ok, note := (&DshEngine{}).Detect()
	if ok {
		t.Skip("本机 PATH 上确实有 dsh，跳过「未安装」用例")
	}
	if !strings.Contains(note, "dsh") {
		t.Errorf("note 应说明 dsh 缺失: %q", note)
	}
}

// headless 参数形态：--profile headless <prompt>（启动器 flag 在前，任务文本在后）。
func TestDshBuildArgsHeadless(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "dsh", `echo '最终正文'`)
	e := &DshEngine{BinPath: w.bin, Profile: dshDefaultProfile}
	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "跑一下测试"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "最终正文" {
		t.Errorf("Text = %q want 最终正文", resp.Text)
	}
	if resp.Engine != "dsh" {
		t.Errorf("Engine = %q want dsh", resp.Engine)
	}
	data, _ := os.ReadFile(log)
	args := string(data)
	if !strings.Contains(args, "--profile headless") {
		t.Errorf("缺少 --profile headless: %q", args)
	}
	if !strings.Contains(args, "跑一下测试") {
		t.Errorf("任务文本没作为位置参数传入: %q", args)
	}
	// 启动器 flag 必须排在任务文本之前（dsh 在第一个不认识的 token 处结束 flag 解析）。
	if strings.Index(args, "--profile") > strings.Index(args, "跑一下测试") {
		t.Errorf("--profile 应在任务文本之前: %q", args)
	}
}

// 自定义 profile 名：--profile 取引擎字段。
func TestDshBuildArgsCustomProfile(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "dsh", `echo 'ok'`)
	e := &DshEngine{BinPath: w.bin, Profile: "headless-cn"}
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "--profile headless-cn") {
		t.Errorf("profile 未生效: %q", string(data))
	}
}

// stdout 只取最终正文；stderr 的 "dsh: reasoning:" 推理增量不混进正文。
func TestDshReasoningOnStderrNotInText(t *testing.T) {
	dir := t.TempDir()
	w, _ := argsCaptureCLI(t, dir, "dsh", `echo 'dsh: reasoning: 先看看仓库' >&2
echo '正文：测试全过'`)
	e := &DshEngine{BinPath: w.bin, Profile: dshDefaultProfile}
	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "跑测试"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "正文：测试全过" {
		t.Errorf("Text = %q（不应混入 stderr 的推理增量）", resp.Text)
	}
}

// 非零退出（turn/end 非 completed）→ 报错，且 stderr 末尾的终止原因进错误摘要。
func TestDshNonZeroExitReportsStderr(t *testing.T) {
	dir := t.TempDir()
	w, _ := argsCaptureCLI(t, dir, "dsh", `echo 'dsh: MISSING_CREDENTIAL: 未配置 API key' >&2
exit 1`)
	e := &DshEngine{BinPath: w.bin, Profile: dshDefaultProfile}
	_, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("非零退出应报错")
	}
	if !strings.Contains(err.Error(), "MISSING_CREDENTIAL") {
		t.Errorf("错误里应带 stderr 摘要: %v", err)
	}
}

// 退出码 0 但没有任何正文 → 明确报错（不返回空 Text 让上层猜）。
func TestDshEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	w, _ := argsCaptureCLI(t, dir, "dsh", `true`)
	e := &DshEngine{BinPath: w.bin, Profile: dshDefaultProfile}
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("空输出应报错")
	}
}

// 会话续接：headless 每次都是全新会话 → --session / -c 都明确报错（不静默开新会话）。
func TestDshSessionUnsupported(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "dsh", `echo '不该被执行'`)
	e := &DshEngine{BinPath: w.bin}

	if _, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		SessionID: "sess-1",
	}); err == nil {
		t.Error("--session 应报错")
	} else if !strings.Contains(err.Error(), "不支持会话续接") {
		t.Errorf("错误文案应说明不支持续接: %v", err)
	}
	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Continue: true,
	}); err == nil {
		t.Error("-c/--continue 应报错")
	}
	// 两种情况下都不该真的启动 CLI。
	if _, err := os.Stat(log); err == nil {
		t.Error("续接被拒时不应启动 dsh 进程")
	}
}

// 模型：headless 无 --model 参数 → 不传该 flag，并在 stderr 打一行告警（同 arkclaw）。
func TestDshModelNotForwarded(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "dsh", `echo 'ok'`)
	e := &DshEngine{BinPath: w.bin, Profile: dshDefaultProfile}

	old := stderr
	var buf strings.Builder
	stderr = &buf
	defer func() { stderr = old }()

	if _, err := e.Complete(context.Background(), Request{
		Model:    "deepseek-v4-pro",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "--model") {
		t.Errorf("dsh headless 不应收到 --model: %q", string(data))
	}
	warn := buf.String()
	if !strings.Contains(warn, "dsh") || !strings.Contains(warn, "deepseek-v4-pro") {
		t.Errorf("告警文案未提及 dsh/模型名: %q", warn)
	}
}

// 未传 -m 时不打告警（不无脑刷屏）。
func TestDshNoModelNoWarning(t *testing.T) {
	dir := t.TempDir()
	w, _ := argsCaptureCLI(t, dir, "dsh", `echo 'ok'`)
	e := &DshEngine{BinPath: w.bin, Profile: dshDefaultProfile}

	old := stderr
	var buf strings.Builder
	stderr = &buf
	defer func() { stderr = old }()

	if _, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("未传 -m 不该有告警: %q", buf.String())
	}
}

// JSONSchema：走与 llm/openclaw 相同的输出后处理（抽顶层必填字段的 JSON）。
func TestDshJSONSchemaPostProcess(t *testing.T) {
	dir := t.TempDir()
	w, _ := argsCaptureCLI(t, dir, "dsh", `echo '前置说明
{"title":"标题","score":9}
后置说明'`)
	e := &DshEngine{BinPath: w.bin, Profile: dshDefaultProfile}
	resp, err := e.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "给我个 JSON"}},
		JSONSchema: &JSONSchema{
			Type:       "object",
			Properties: map[string]map[string]any{"title": {"type": "string"}, "score": {"type": "number"}},
			Required:   []string{"title", "score"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.HasPrefix(resp.Text, "{") || !strings.Contains(resp.Text, `"title":"标题"`) {
		t.Errorf("Text 应为抽取后的紧凑 JSON: %q", resp.Text)
	}
}

// workspace：走子进程 cwd（dsh 的「调用目录即 workspace 根」）。
func TestDshWorkspaceUsesCwd(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "pwd.log")
	bin := filepath.Join(dir, "dsh")
	script := "#!/bin/sh\npwd > " + log + "\nprintf '%s\\n' \"$*\" >> " + log + "\necho 'ok'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	e := &DshEngine{BinPath: bin, Profile: dshDefaultProfile}
	if _, err := e.Complete(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		Workspace: ws,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	got := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	// macOS 的 /var 与 /private/var 互为软链，按真实路径比较。
	wantReal, _ := filepath.EvalSymlinks(ws)
	gotReal, _ := filepath.EvalSymlinks(strings.TrimSpace(got[0]))
	if gotReal != wantReal {
		t.Errorf("子进程 cwd = %q want %q", gotReal, wantReal)
	}
}

// 系统提示词无独立通道 → 展平进 prompt 头部（与 trae/openclaw 同路）。
func TestDshSystemPromptFlattened(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "dsh", `echo 'ok'`)
	e := &DshEngine{BinPath: w.bin, Profile: dshDefaultProfile}
	if _, err := e.Complete(context.Background(), Request{
		SystemPrompt: "只输出中文",
		Messages:     []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "只输出中文") {
		t.Errorf("system prompt 应展平进任务文本: %q", string(data))
	}
}

// CLI 不存在：探测不到时 Complete 明确报错。
func TestDshNotFound(t *testing.T) {
	t.Setenv("MAGIC_AGENT_DSH_BIN", "")
	t.Setenv("PATH", t.TempDir())
	if _, err := (&DshEngine{}).Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Skip("本机 PATH 上确实有 dsh，跳过「未安装」用例")
	}
}

// 无模型清单来源：settings.yaml 不存在 → ErrNoModelSource + 原因。
// ⚠️ 必须把 DSH_HOME 指到临时目录，否则会读到开发机真实配置（测试不可复现）。
func TestDshListModelsNoSource(t *testing.T) {
	t.Setenv("DSH_HOME", t.TempDir())
	_, err := (&DshEngine{}).ListModels(context.Background())
	if !errors.Is(err, ErrNoModelSource) {
		t.Fatalf("want ErrNoModelSource, got %v", err)
	}
	if !strings.Contains(err.Error(), "settings.yaml") {
		t.Errorf("原因应指向 settings.yaml: %v", err)
	}
}

// settings.yaml 里没有任何 models 时同样是 ErrNoModelSource。
func TestDshListModelsEmptySettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "settings.yaml"), []byte("llm-pi-ai:\n  providers: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&DshEngine{}).ListModels(context.Background()); !errors.Is(err, ErrNoModelSource) {
		t.Fatalf("want ErrNoModelSource, got %v", err)
	}
}

// 主路径：从 settings.yaml 读出 `provider/model` 列表。
func TestDshListModelsFromSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	cfg := `agent-default-model:
  provider: modelverse
  model: deepseek-v4.1-flash

llm-pi-ai:
  providers:
    modelverse:
      api: openai-completions
      baseURL: https://api.modelverse.cn/v1
      apiKeyEnv: MODELVERSE_API_KEY
      models:
        - id: deepseek-v4.1-flash
          name: DeepSeek V4.1 Flash
        - id: glm-5.3
          name: GLM-5.3
        - id: kimi-k3
    other-gw:
      models:
        - id: some-model
`
	if err := os.WriteFile(filepath.Join(home, "settings.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (&DshEngine{}).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{
		"modelverse/deepseek-v4.1-flash",
		"modelverse/glm-5.3",
		"modelverse/kimi-k3",
		"other-gw/some-model",
	}
	if !equalStrings(got, want) {
		t.Errorf("ListModels() = %v\n want %v", got, want)
	}
}

// 解析器：块式 / 行内项 / 多 provider / 注释 / 去重 / 无 models 时回落默认模型。
func TestParseDshModelRoutes(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want []string
	}{
		{
			name: "块式多模型",
			yaml: "llm-pi-ai:\n  providers:\n    gw:\n      models:\n        - id: a\n        - id: b\n",
			want: []string{"gw/a", "gw/b"},
		},
		{
			name: "条目带 name 子键",
			yaml: "llm-pi-ai:\n  providers:\n    gw:\n      models:\n        - id: a\n          name: A\n        - id: b\n          name: B\n",
			want: []string{"gw/a", "gw/b"},
		},
		{
			name: "多 provider",
			yaml: "llm-pi-ai:\n  providers:\n    gw1:\n      models:\n        - id: a\n    gw2:\n      models:\n        - id: b\n",
			want: []string{"gw1/a", "gw2/b"},
		},
		{
			name: "行内项",
			yaml: "llm-pi-ai:\n  providers:\n    gw:\n      models:\n        - {id: a}\n        - {id: b, name: B}\n",
			want: []string{"gw/a", "gw/b"},
		},
		{
			name: "注释与空行不干扰",
			yaml: "# 顶注\nllm-pi-ai:\n  providers:\n    gw:\n      models:  # 模型清单\n        - id: a  # 甲\n\n        - id: b\n",
			want: []string{"gw/a", "gw/b"},
		},
		{
			name: "去重",
			yaml: "llm-pi-ai:\n  providers:\n    gw:\n      models:\n        - id: a\n        - id: a\n",
			want: []string{"gw/a"},
		},
		{
			name: "models 为空则回落 agent-default-model",
			yaml: "agent-default-model:\n  provider: gw\n  model: only-one\n",
			want: []string{"gw/only-one"},
		},
		{
			name: "行内 agent-default-model",
			yaml: "agent-default-model: {provider: gw, model: m1}\n",
			want: []string{"gw/m1"},
		},
		{
			name: "结构不符 → 空",
			yaml: "providers:\n  gw:\n    models:\n      - id: a\n",
			want: nil,
		},
		{
			name: "空文件 → 空",
			yaml: "",
			want: nil,
		},
		{
			name: "其他段的 models 不误命中",
			yaml: "some-other-plugin:\n  providers:\n    gw:\n      models:\n        - id: nope\n",
			want: nil,
		},
	}
	for _, c := range cases {
		got := parseDshModelRoutes(c.yaml)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !equalStrings(got, c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// 默认模型解析：块式 / 行内式 / 引号 / 缺段 / 段后换键。
func TestParseDshDefaultModel(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "块式",
			yaml: "llm-deepseek:\n  reasoningEffort: max\nagent-default-model:\n  provider: deepseek\n  model: deepseek-chat\n",
			want: "deepseek-chat",
		},
		{
			name: "行内式",
			yaml: "agent-default-model: {provider: deepseek, model: deepseek-v4-pro}\n",
			want: "deepseek-v4-pro",
		},
		{
			name: "带引号",
			yaml: "agent-default-model:\n  model: \"deepseek-chat\"\n",
			want: "deepseek-chat",
		},
		{
			name: "缺段",
			yaml: "llm-deepseek:\n  reasoningEffort: max\n",
			want: "",
		},
		{
			name: "空文件",
			yaml: "",
			want: "",
		},
		{
			name: "注释不误命中",
			yaml: "# agent-default-model:\n#   model: nope\n",
			want: "",
		},
	}
	for _, c := range cases {
		if got := parseDshDefaultModel(c.yaml); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// DshConfiguredModel：从 $DSH_HOME/settings.yaml 读默认模型；读不到返回空串（不报错）。
func TestDshConfiguredModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	if got := DshConfiguredModel(); got != "" {
		t.Errorf("settings.yaml 不存在时应返回空串, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(home, "settings.yaml"),
		[]byte("agent-default-model:\n  provider: deepseek\n  model: deepseek-chat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DshConfiguredModel(); got != "deepseek-chat" {
		t.Errorf("DshConfiguredModel() = %q want deepseek-chat", got)
	}
}

// Response.Model：配置里读不到时回落 "config-default"（标签，不是模型名）。
func TestDshResponseModelFallback(t *testing.T) {
	t.Setenv("DSH_HOME", t.TempDir())
	dir := t.TempDir()
	w, _ := argsCaptureCLI(t, dir, "dsh", `echo 'ok'`)
	resp, err := (&DshEngine{BinPath: w.bin, Profile: dshDefaultProfile}).Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Model != "config-default" {
		t.Errorf("Model = %q want config-default", resp.Model)
	}
}

// DshHome：$DSH_HOME 优先，其次 ~/.dsh。
func TestDshHome(t *testing.T) {
	t.Setenv("DSH_HOME", "/tmp/dsh-home-x")
	if got := DshHome(); got != "/tmp/dsh-home-x" {
		t.Errorf("DshHome() = %q want /tmp/dsh-home-x", got)
	}
	t.Setenv("DSH_HOME", "")
	got := DshHome()
	if got == "" || !strings.HasSuffix(got, ".dsh") {
		t.Errorf("DshHome() = %q want 以 .dsh 结尾", got)
	}
}

// 默认超时要盖住「dsh 自带工具循环」的长任务（不短于 5 分钟）。
func TestDefaultDshTimeoutCoversAgentTasks(t *testing.T) {
	if DefaultDshTimeout < 5*time.Minute {
		t.Errorf("DefaultDshTimeout = %v，agent 任务常常分钟级，应留足余量", DefaultDshTimeout)
	}
	// Runner 按引擎类型取默认超时。
	r := &Runner{Engine: &DshEngine{}}
	if got := r.defaultTimeout(); got != DefaultDshTimeout {
		t.Errorf("Runner.defaultTimeout() = %v want %v", got, DefaultDshTimeout)
	}
}

// ── 流式（--stream）────────────────────────────────────────────
//
// dsh 的增量在 stderr（"dsh: reasoning:" 起头），正文一次性走 stdout。
// 实测依据：2026-09-21，dsh 0.1.5-rc.2（见 dsh.go 的 Stream 注释）。

// dshStreamStub 造一个假 dsh：推理分批写 stderr，正文最后写 stdout。
func dshStreamStub(t *testing.T, dir string) (bin string) {
	t.Helper()
	bin = filepath.Join(dir, "dsh")
	script := `#!/bin/sh
echo "dsh: reasoning:" >&2
echo "第一步：读题" >&2
sleep 0.05
echo "第二步：算 17*20=340" >&2
sleep 0.05
echo "第三步：算 17*3=51" >&2
echo "结果是 391"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// 推理增量逐条 KindThinking；正文收尾一条 KindText。
func TestDshStreamThinkingAndFinalText(t *testing.T) {
	dir := t.TempDir()
	bin := dshStreamStub(t, dir)
	e := &DshEngine{BinPath: bin, Profile: dshDefaultProfile}

	var mu sync.Mutex
	var events []StreamEvent
	res, err := e.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "算 17*23"}},
	}, func(ev StreamEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// 正文：一次性给出，等于 stdout 去空白。
	if res.Text != "结果是 391" {
		t.Errorf("Text = %q want 结果是 391", res.Text)
	}
	// 思考过程：stderr 推理段拼接（标题行不算内容）。
	if !strings.Contains(res.Thinking, "第一步：读题") || !strings.Contains(res.Thinking, "第三步：算 17*3=51") {
		t.Errorf("Thinking 不完整: %q", res.Thinking)
	}
	if strings.Contains(res.Thinking, "dsh: reasoning:") {
		t.Errorf("Thinking 不该包含标题行: %q", res.Thinking)
	}

	// 事件：3 条 thinking + 1 条 text + 1 条 turn_end；thinking 全在 text 之前，
	// 且 text 与 turn_end 是最后两条（正文一次性收尾 → 立刻报「本轮做完」）。
	mu.Lock()
	defer mu.Unlock()
	var thinkings, texts int
	firstTextIdx := -1
	for i, ev := range events {
		switch ev.Kind {
		case KindThinking:
			thinkings++
		case KindText:
			texts++
			if firstTextIdx < 0 {
				firstTextIdx = i
			}
		}
	}
	if thinkings != 3 {
		t.Errorf("KindThinking 事件数 = %d want 3（%v）", thinkings, events)
	}
	if texts != 1 {
		t.Errorf("KindText 事件数 = %d want 1（%v）", texts, events)
	}
	if firstTextIdx != len(events)-2 {
		t.Errorf("KindText 应是倒数第二个事件（正文一次性收尾后补 turn_end），实际 idx=%d/%d",
			firstTextIdx, len(events)-1)
	}
	if last := events[len(events)-1]; last.Kind != KindTurnEnd {
		t.Errorf("最后一个事件应是 KindTurnEnd，实际 %v", last)
	}
}

// 非推理行（启动日志等）不进 Thinking，也不当增量发出。
func TestDshStreamIgnoresNonReasoningStderr(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "dsh")
	script := `#!/bin/sh
echo "openviking: 插件已注册" >&2
echo "dsh: reasoning:" >&2
echo "真正的推理" >&2
echo "正文"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var events []StreamEvent
	res, err := (&DshEngine{BinPath: bin, Profile: dshDefaultProfile}).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(ev StreamEvent) { events = append(events, ev) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.Thinking != "真正的推理\n" {
		t.Errorf("Thinking = %q want %q", res.Thinking, "真正的推理\n")
	}
	for _, ev := range events {
		if ev.Kind == KindThinking && strings.Contains(ev.Text, "openviking") {
			t.Errorf("启动日志被当成推理增量: %v", ev)
		}
	}
}

// 流式路径同样拒续接（与 Complete 同一契约），且不启动进程。
func TestDshStreamRejectsSession(t *testing.T) {
	dir := t.TempDir()
	w, log := argsCaptureCLI(t, dir, "dsh", `echo '不该被执行'`)
	_, err := (&DshEngine{BinPath: w.bin}).Stream(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		SessionID: "s1",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "不支持会话续接") {
		t.Fatalf("流式路径也应拒续接, got %v", err)
	}
	if _, serr := os.Stat(log); serr == nil {
		t.Error("拒续接时不应启动 dsh 进程")
	}
}

// 流式路径的非零退出：错误带 stderr 摘要。
func TestDshStreamNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "dsh")
	script := "#!/bin/sh\necho 'dsh: MISSING_CREDENTIAL: 未配置' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := (&DshEngine{BinPath: bin, Profile: dshDefaultProfile}).Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "MISSING_CREDENTIAL") {
		t.Fatalf("应带 stderr 摘要, got %v", err)
	}
}

// 参数矩阵：MaxTokens / Temperature / Tools / Timeout 在 dsh 上不产生任何 flag。
func TestDshIgnoredParamsProduceNoFlags(t *testing.T) {
	t.Setenv(DshProfileEnv, "") // 隔离外部 profile 覆盖
	e := &DshEngine{}
	temp := 0.7
	args := e.buildArgs(Request{
		MaxTokens:   4096,
		Temperature: &temp,
		Tools:       ToolsOn,
		Timeout:     3 * time.Minute,
	}, "【用户】\nhi")
	want := []string{"--profile", "headless", "【用户】\nhi"}
	if fmt.Sprint(args) != fmt.Sprint(want) {
		t.Errorf("buildArgs = %v want %v", args, want)
	}
}
