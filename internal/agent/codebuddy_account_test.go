package agent

import (
	"strings"
	"testing"
)

// envMap 把 ["K=V", ...] 折叠成 map（exec 语义：重复 key 取后者）。
func envMap(kv []string) map[string]string {
	out := make(map[string]string, len(kv))
	for _, s := range kv {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// 默认（无任何覆盖）：清空 ACC_PRODUCT_CONFIG_PATH + 指定账号 id + 该引擎的配置目录。
func TestCodebuddyAccountEnv(t *testing.T) {
	t.Setenv("ACC_PRODUCT_CONFIG_V3", "")
	t.Setenv("CODEBUDDY_CONFIG_DIR", "")
	t.Setenv(codebuddyAIAuthIDEnv, "")

	got := envMap(codebuddyAccountEnv("/tmp/cbdir", codebuddyAIAuthID, codebuddyAIAuthIDEnv))

	// 必须清空：父会话指过来的 spilled 配置里的 authentication.id 优先级高于 V3
	if v, ok := got["ACC_PRODUCT_CONFIG_PATH"]; !ok || v != "" {
		t.Errorf("ACC_PRODUCT_CONFIG_PATH 应被显式清空，实际 ok=%v v=%q", ok, v)
	}
	wantID := `{"authentication":{"id":"codebuddy-ai"}}`
	if got["ACC_PRODUCT_CONFIG_V3"] != wantID {
		t.Errorf("ACC_PRODUCT_CONFIG_V3 = %q, want %q", got["ACC_PRODUCT_CONFIG_V3"], wantID)
	}
	if got["CODEBUDDY_CONFIG_DIR"] != "/tmp/cbdir" {
		t.Errorf("CODEBUDDY_CONFIG_DIR = %q, want /tmp/cbdir", got["CODEBUDDY_CONFIG_DIR"])
	}
}

// 账号覆盖只认**各引擎自己的**变量；全局的 ACC_PRODUCT_CONFIG_V3 不参与
// —— 否则全局存在该变量时，两个引擎会静默合并成同一个账号。
func TestCodebuddyAccountEnvOverride(t *testing.T) {
	t.Setenv("CODEBUDDY_CONFIG_DIR", "")
	t.Setenv("ACC_PRODUCT_CONFIG_V3", `{"authentication":{"id":"global-should-be-ignored"}}`)
	t.Setenv(codebuddyAuthIDEnv, "mine")

	got := envMap(codebuddyAccountEnv("/tmp/cbdir", codebuddyAuthID, codebuddyAuthIDEnv))

	if got["ACC_PRODUCT_CONFIG_V3"] != `{"authentication":{"id":"mine"}}` {
		t.Errorf("应使用本引擎的覆盖 id，实际 %q", got["ACC_PRODUCT_CONFIG_V3"])
	}
}

// 用户显式设了 CODEBUDDY_CONFIG_DIR 时**不注入**（让子进程继承用户的值）。
func TestCodebuddyAccountEnvRespectsConfigDirOverride(t *testing.T) {
	t.Setenv("CODEBUDDY_CONFIG_DIR", "/tmp/mine")
	t.Setenv(codebuddyAuthIDEnv, "")

	got := envMap(codebuddyAccountEnv("/tmp/cbdir", codebuddyAuthID, codebuddyAuthIDEnv))

	if v, ok := got["CODEBUDDY_CONFIG_DIR"]; ok {
		t.Errorf("用户已设置 CODEBUDDY_CONFIG_DIR 时不应再注入，实际注入 %q", v)
	}
}

// 两个引擎必须落在**不同**的票据文件上（同一个 CLI、两个账号）。
func TestCodeBuddyEnginesUseDistinctAccounts(t *testing.T) {
	if codebuddyAuthID == codebuddyAIAuthID {
		t.Fatalf("两个引擎的账号标识不能相同：%q", codebuddyAuthID)
	}
	if strings.TrimSpace(codebuddyAuthID) == "" || strings.TrimSpace(codebuddyAIAuthID) == "" {
		t.Fatal("账号标识不能为空串：空串会退化成共享的 auth.info")
	}

	t.Setenv("CODEBUDDY_CONFIG_DIR", "")
	envA := envMap(codebuddyAccountEnv(codebuddyDir(), codebuddyAuthID, codebuddyAuthIDEnv))
	envB := envMap(codebuddyAIExtraEnv())
	if envA["ACC_PRODUCT_CONFIG_V3"] == envB["ACC_PRODUCT_CONFIG_V3"] {
		t.Errorf("两个引擎的账号环境相同（会互相顶号）：%q", envA["ACC_PRODUCT_CONFIG_V3"])
	}
}

// LoginCommand 必须返回「同一个二进制 + 本引擎的账号环境」，供 --login 使用。
func TestCodeBuddyLoginCommandEnv(t *testing.T) {
	t.Setenv("ACC_PRODUCT_CONFIG_V3", "")
	t.Setenv("CODEBUDDY_CONFIG_DIR", "")
	t.Setenv(codebuddyAuthIDEnv, "")
	t.Setenv(codebuddyAIAuthIDEnv, "")

	// BinPath 注入假路径，避免测试依赖本机是否装了 CLI。
	e := &CodeBuddyEngine{BinPath: "/tmp/fake-codebuddy"}
	bin, args, env, err := e.LoginCommand()
	if err != nil {
		t.Fatalf("LoginCommand: %v", err)
	}
	if bin != "/tmp/fake-codebuddy" {
		t.Errorf("bin = %q, want 注入的路径", bin)
	}
	if len(args) != 0 {
		t.Errorf("裸启动应为空参数（交互式界面），实际 %v", args)
	}
	m := envMap(env)
	if !strings.Contains(m["ACC_PRODUCT_CONFIG_V3"], `"id":"`+codebuddyAuthID+`"`) {
		t.Errorf("--login 必须带上 codebuddy 的账号 id，实际 %q", m["ACC_PRODUCT_CONFIG_V3"])
	}

	ai := &CodeBuddyAIEngine{BinPath: "/tmp/fake-codebuddy"}
	_, _, envAI, err := ai.LoginCommand()
	if err != nil {
		t.Fatalf("codebuddy-ai LoginCommand: %v", err)
	}
	if mAI := envMap(envAI); mAI["ACC_PRODUCT_CONFIG_V3"] == m["ACC_PRODUCT_CONFIG_V3"] {
		t.Error("两个引擎 --login 的账号环境相同，会把两个账号登到同一份票据上")
	}
}

// 未探测到 CLI 时 --login 要明确报错（而不是静默启一个空 bin）。
func TestCodeBuddyLoginCommandMissingBin(t *testing.T) {
	e := &CodeBuddyEngine{BinPath: ""}
	if bin := e.bin(); bin != "" {
		t.Skipf("本机 PATH 上存在 codebuddy（%s），跳过未安装分支", bin)
	}
	if _, _, _, err := e.LoginCommand(); err == nil {
		t.Fatal("CLI 不存在时应返回错误")
	}
}
