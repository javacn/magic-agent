package agent

// permission.go - 四档权限模型（跨引擎统一语义）+ Claude Code 参数映射。
//
// 用户需求（2026-09-18）：「按照4档 使用claudecode的参数映射一下实现集成」。
// 四档来自对五家 agent CLI（Claude Code / Codex / CodeBuddy / TRAE / OpenClaw）
// 权限机制的横向对比：它们的全部用户可选模式都能收敛到同一条轴 ——
// 「哪些动作自动放行，放行不了时由谁裁决」：
//
//	第 1 档 手动审批      manual        沙箱开启，只读放行，其余逐项由「用户」确认
//	第 2 档 自动接受编辑  accept-edits  沙箱开启，工作区内编辑放行，命令仍由「用户」确认
//	第 3 档 自动审批      auto          沙箱开启，沙箱内放行，越界交「LLM Guardian」判定
//	第 4 档 完全访问      full          沙箱关闭，命令直接在宿主机执行，无审批
//
// ── Claude Code 参数映射（本文件的核心） ───────────────────────────────────────
//
//	档位             --permission-mode      settings.sandbox
//	manual           default                enabled=true,  autoAllowBashIfSandboxed=false
//	accept-edits     acceptEdits            enabled=true,  autoAllowBashIfSandboxed=false
//	auto             auto                   enabled=true,  autoAllowBashIfSandboxed=true
//	full             bypassPermissions      enabled=false
//
// 三个关键实现决定（都来自官方文档，不是猜的）：
//
//  1. 档位走 `--permission-mode`，**不走** settings 的 `permissions.defaultMode`。
//     官方明确：`auto` 与 `bypassPermissions` 写在项目级 `.claude/settings.json`
//     或本地级 `.claude/settings.local.json` 里**不生效**（会被忽略，会话回落到
//     Manual / 内建默认）。`--permission-mode` 是唯一在任意作用域都可靠的入口，
//     且它 overrides settings 里的 defaultMode。
//
//  2. 沙箱**只能**走 `--settings`。claude 没有 `--sandbox` 命令行参数
//     （官方 CLI reference 的 flags 表全文无此参数），启用方式只有会话内 `/sandbox`、
//     settings 文件的 `sandbox.enabled`、或 `--settings` 内联 JSON。
//     而 `--settings` 官方语义是「一个文件路径**或**一段内联 JSON」，不是可重复 flag ——
//     所以 MaxTokens 的 env 注入、sandbox、autoMode、permissions 必须**合并进同一份 JSON**。
//     这正是 agentSettingsPayload 存在的理由（早期 maxTokensSettings 只装 env）。
//
//  3. `autoAllowBashIfSandboxed` 是第 1/2 档与第 3 档的分水岭。该键默认值是 true，
//     所以第 1/2 档必须**显式写 false**，否则沙箱内的 Bash 会被自动放行，
//     第 2 档就退化成了「编辑与命令都不问」。
//
// 另外：第 1~3 档会写 `failIfUnavailable: true` 与 `allowUnsandboxedCommands: false`。
// 前者让沙箱起不来时**直接报错退出**，而不是静默降级成不沙箱运行 ——
// 静默降级是安全特性里最坏的结果（用户以为被保护着，其实没有）。
// 后者关闭 `dangerouslyDisableSandbox` 逃逸舱口（官方称 Strict sandbox mode）；
// 需要放行的命令请用 PermissionOptions.ExcludedCommands 显式列出，而不是放宽全局。

import (
	"encoding/json"
	"sort"
	"strings"
)

// PermissionTier 是四档权限模型的档位标识（跨引擎统一语义）。
type PermissionTier string

const (
	// PermissionManual 第 1 档 · 手动审批：沙箱开启，只读放行，其余逐项由用户确认。
	PermissionManual PermissionTier = "manual"
	// PermissionAcceptEdits 第 2 档 · 自动接受编辑：沙箱开启，编辑放行，命令仍需确认。
	PermissionAcceptEdits PermissionTier = "accept-edits"
	// PermissionAuto 第 3 档 · 自动审批：沙箱开启，越界由 LLM Guardian 判定。
	PermissionAuto PermissionTier = "auto"
	// PermissionFull 第 4 档 · 完全访问：沙箱关闭，无审批。
	PermissionFull PermissionTier = "full"
)

// DefaultPermissionTier 默认档位。
//
// 取 full 是为了**保持既有行为**：改造前 claude / codebuddy 在 --tools 非 off 时
// 恒传 `--dangerously-skip-permissions` / `-y`，语义就是第 4 档。默认改成别的档位
// 会是一次静默的行为变更（现有调用方的 agent 会突然开始弹审批 / 被沙箱拦），
// 因此这里不改变默认值，只把「可选」变成「可选且显式」。
// 需要更安全默认值的调用方请显式传 --permission auto（推荐）或 manual。
const DefaultPermissionTier = PermissionFull

// PermissionOptions 是四档模型之上的可选项（都是「加密」而非「放宽」）。
//
// 零值即最严格：严格沙箱、无额外放行、无自定义受信边界。
type PermissionOptions struct {
	// ExcludedCommands 始终在沙箱外执行的命令（claude 的 sandbox.excludedCommands）。
	// 用于 docker / watchman 这类与沙箱不兼容的工具，比整体放宽沙箱精确得多。
	ExcludedCommands []string

	// AllowedDomains 沙箱网络白名单（sandbox.network.allowedDomains）。
	// 留空表示不改动 claude 自身的网络策略。
	AllowedDomains []string

	// AutoModeEnvironment 第 3 档分类器的「受信边界」描述（自然语言，不是正则）。
	// 官方要求写成「像给新工程师介绍基础设施」那样，例如
	// "Source control: github.example.com/acme-corp"。
	// 非空时会以 ["$defaults", ...] 的形式注入 autoMode.environment，
	// 即保留内建默认条目再追加自定义条目。
	// 只在第 3 档生效（其余档位没有分类器）。
	AutoModeEnvironment []string

	// Deny 追加的 deny 规则（permissions.deny）。deny 在所有档位（含第 4 档）
	// 都先于 allow / ask 求值，且不可被白名单例外覆盖 —— 这是唯一能穿透
	// 「完全访问」的硬红线，建议放 rm -rf 之类不可逆操作。
	Deny []string

	// Ask 追加的 ask 规则（permissions.ask）。命中即强制人工审批，
	// 在第 3 档下**分类器也无法自动放行** —— 这就是官方推荐的「人为检查点」，
	// 例如 Bash(git push *) / Bash(gh pr create *)。
	Ask []string

	// AllowUnsandboxedCommands 是否保留「逃逸舱口」（sandbox.allowUnsandboxedCommands）。
	// 默认 false = 严格沙箱：模型无法用 dangerouslyDisableSandbox 把命令挪到沙箱外。
	// 第 4 档无沙箱，本项不生效。
	AllowUnsandboxedCommands bool
}

// permissionOrDefault nil / 空串档位回落默认档（保持既有行为）。
func permissionOrDefault(t PermissionTier) PermissionTier {
	if t == "" {
		return DefaultPermissionTier
	}
	return t
}

// ParsePermissionTier 解析 --permission 取值，返回档位与是否合法。
//
// 接受四档规范名与常见别名（含数字档位），大小写 / 连字符 / 下划线不敏感：
//
//	manual        manual | default | ask | 1
//	accept-edits  accept-edits | acceptedits | edits | 2
//	auto          auto | guardian | llm | classifier | 3
//	full          full | bypass | bypasspermissions | yolo | dangerous | 4
//
// 注意 "default" 归到 manual：这与 claude 自己的命名一致（claude 的
// `--permission-mode default` 就是 UI 上那个 Manual 档），避免同一个词在两处
// 表示不同含义。
func ParsePermissionTier(s string) (PermissionTier, bool) {
	key := strings.ToLower(strings.TrimSpace(s))
	key = strings.NewReplacer("-", "", "_", "", " ", "").Replace(key)
	switch key {
	case "manual", "default", "ask", "1":
		return PermissionManual, true
	case "acceptedits", "acceptedit", "edits", "edit", "2":
		return PermissionAcceptEdits, true
	case "auto", "guardian", "llm", "classifier", "3":
		return PermissionAuto, true
	case "full", "bypass", "bypasspermissions", "yolo", "dangerous", "4":
		return PermissionFull, true
	}
	return "", false
}

// PermissionTiers 返回四档规范名（按档位从严到宽），供 --help / 报错文案使用。
func PermissionTiers() []string {
	return []string{
		string(PermissionManual),
		string(PermissionAcceptEdits),
		string(PermissionAuto),
		string(PermissionFull),
	}
}

// PermissionSupportOf 返回某引擎对四档模型的落地方式（机器可读，进 --engines 输出）：
//
//	"flag:--permission-mode"  claude / codebuddy / codebuddy-ai：CLI 有原生 --permission-mode，
//	                          沙箱与 autoMode 经 --settings 注入（同族 flag 面）
//	"none"                    其余引擎暂未接线（trae 的档位在 yaml 配置里、codex 是
//	                          「沙箱 × 审批」两轴、llm 无工具语义、openclaw 走
//	                          exec.mode 五档、arkclaw 由网关侧决定；dsh 只有
//	                          read-only / workspace-write 两个配置层预设，没有
//	                          「沙箱关闭 / 无审批」那一档，四档无法一一落地；
//	                          codebuddy-gateway 的档位由**网关进程**的 --permission-mode
//	                          决定，且网关会把远程任务的权限强制切到 bypassPermissions
//	                          —— per-call 传四档没有落地通道）
//	                          —— 传 --permission 会**明确报错**，
//	                          而不是静默忽略一个安全设置
func PermissionSupportOf(engine string) string {
	// 具名 agent（如 MagicAI）先归到它协议的家族名，再查表 —— 见 CapabilityFamilyOf。
	engine = CapabilityFamilyOf(engine)
	switch engine {
	case "claude", "codebuddy", "codebuddy-ai":
		return "flag:--permission-mode"
	default:
		return "none"
	}
}

// PermissionSupported 报告某引擎是否已接线四档模型。
func PermissionSupported(engine string) bool {
	return PermissionSupportOf(engine) != "none"
}

// claudePermissionMode 把档位映射到 claude / codebuddy `--permission-mode` 的取值。
//
// 取值必须是 CLI 认的字面量（claude 官方：`default` / `acceptEdits` / `plan` /
// `auto` / `dontAsk` / `bypassPermissions`；codebuddy 同族，另支持 `manual`
// 作为 default 的别名）。这里一律用不带别名的规范值，兼容面最宽。
func claudePermissionMode(t PermissionTier) string {
	switch permissionOrDefault(t) {
	case PermissionManual:
		return "default"
	case PermissionAcceptEdits:
		return "acceptEdits"
	case PermissionAuto:
		return "auto"
	case PermissionFull:
		return "bypassPermissions"
	}
	return "default"
}

// sandboxSettings 构造 settings.sandbox 子对象。
//
//	第 1/2/3 档（manual / accept-edits / auto）—— 返回 `{"enabled": true, ...}`，
//	                        配合 --permission-mode 让模型知道「沙箱里运行」+ 哪些操作免确认
//	第 4 档（full / bypassPermissions）—— **返回 nil，让外层不写 sandbox 段
//	                        整段都不下发**：
//	                        1. bypassPermissions 与 sandbox 互斥（开了也是空跑）；
//	                        2. 实测 2026-10-01：claude CLI 在 tools=default +
//	                           bypassPermissions + `--settings {"sandbox":{"enabled":false}}`
//	                           组合下报「Invalid JSON provided to --settings」、
//	                           exit 0 零输出。bypassPermissions 本身就意味着不沙箱，
//	                           显式写 enabled=false 反而触发那个 bug。
//	                        3. 用户的 ~/.claude/settings.json 若开了 sandbox，
//	                           第 4 档「--settings 优先级更高」这条契约靠的是把整段
//	                           覆盖成空 sandbox，不是不写。两段都不写 = 沙箱启用 + 第 4 档，
//	                           是 bug。
//	                        见 magic-stock `.workbuddy/memory/2026-10-01.md` 的「Invalid JSON
//	                        provided to --settings」一节。
//
// engine 用于区分两家文档确认过的键集：CodeBuddy 文档未列 `failIfUnavailable`，
// 未知键的行为没有保证，故只在 claude 上注入。
func sandboxSettings(tier PermissionTier, opts PermissionOptions, engine string) map[string]any {
	if permissionOrDefault(tier) == PermissionFull {
		// 第 4 档：sandbox 整段都不写（见上方注释）
		return nil
	}
	sb := map[string]any{
		"enabled": true,
		// 分水岭：只有第 3 档让沙箱内的 Bash 免确认。
		// 该键默认值是 true，第 1/2 档必须显式写 false，否则第 2 档会退化成
		// 「编辑与命令都不问」。
		"autoAllowBashIfSandboxed": tier == PermissionAuto,
		// 默认关闭逃逸舱口（Strict sandbox mode）。
		"allowUnsandboxedCommands": opts.AllowUnsandboxedCommands,
	}
	if len(opts.ExcludedCommands) > 0 {
		sb["excludedCommands"] = normalizeList(opts.ExcludedCommands)
	}
	if len(opts.AllowedDomains) > 0 {
		sb["network"] = map[string]any{
			"allowedDomains": normalizeList(opts.AllowedDomains),
		}
	}
	if engine == "claude" {
		// 沙箱起不来就报错退出，不静默降级成不沙箱运行。
		sb["failIfUnavailable"] = true
	}
	return sb
}

// autoModeSettings 构造第 3 档的 settings.autoMode 子对象；非第 3 档或无自定义
// 边界时返回 nil。
//
// 官方限制：分类器只从 `~/.claude/settings.json`、托管设置、`--settings` 读 autoMode，
// **不读**项目内 `.claude/settings.json` 与 `.claude/settings.local.json`
// （防止仓库注入自己的 allow 规则）。我们走 `--settings`，属于被读取的那一档。
func autoModeSettings(tier PermissionTier, opts PermissionOptions) map[string]any {
	if permissionOrDefault(tier) != PermissionAuto {
		return nil
	}
	env := normalizeList(opts.AutoModeEnvironment)
	if len(env) == 0 {
		return nil
	}
	// "$defaults" 是官方占位符：把内建默认条目插到这个位置，
	// 我们的自定义条目追加在后。不写 "$defaults" 等于**完整替换**内建规则，
	// 会丢掉内建安全规则，故必须带上。
	return map[string]any{"environment": append([]string{"$defaults"}, env...)}
}

// permissionRules 构造 settings.permissions 子对象；无规则时返回 nil。
func permissionRules(opts PermissionOptions) map[string]any {
	deny, ask := normalizeList(opts.Deny), normalizeList(opts.Ask)
	if len(deny) == 0 && len(ask) == 0 {
		return nil
	}
	p := map[string]any{}
	if len(deny) > 0 {
		p["deny"] = deny
	}
	if len(ask) > 0 {
		p["ask"] = ask
	}
	return p
}

// agentSettingsPayload 组装 claude / codebuddy `--settings` 的**单份** JSON 载荷。
//
// 为什么必须合并：`--settings` 官方语义是「一个文件路径或一段内联 JSON」，
// 不是可重复 flag。MaxTokens 的 env 注入、sandbox、autoMode、permissions
// 若分多次传，第二次会覆盖第一次（或行为未定义）。所以这里一次性组装。
//
// 工具关闭（--tools off）时只注入 env：没有工具调用，沙箱与权限规则都无意义，
// 而 `failIfUnavailable: true` 反而可能让一次纯 chat 因为沙箱起不来而失败。
//
// 返回 (payload, true) 表示需要传 --settings；("", false) 表示本次无需注入。
func agentSettingsPayload(req Request, engine string) (string, bool) {
	tier := permissionOrDefault(req.Permission)
	opts := req.PermissionOptions
	settings := map[string]any{}

	if env := maxTokensEnv(req.MaxTokens); env != nil {
		settings["env"] = env
	}

	// 沙箱 / autoMode / 权限规则只在真会调用工具时注入。
	if !toolsIsOff(req) {
		if sb := sandboxSettings(tier, opts, engine); sb != nil {
			settings["sandbox"] = sb
		}
		if am := autoModeSettings(tier, opts); am != nil {
			settings["autoMode"] = am
		}
		if rules := permissionRules(opts); rules != nil {
			settings["permissions"] = rules
		}
	}

	if len(settings) == 0 {
		return "", false
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// normalizeList 去空白、去空项、去重并保持稳定顺序。
// 让「同一组选项」永远产出同一份 JSON —— 否则参数断言类测试会随机翻车。
func normalizeList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		for _, part := range strings.Split(raw, ",") {
			v := strings.TrimSpace(part)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// permissionSummary 一行式描述某档位的行为，供 --help 与 -v 输出复用。
func permissionSummary(t PermissionTier) string {
	switch permissionOrDefault(t) {
	case PermissionManual:
		return "沙箱开启，只读放行，其余逐项确认（claude --permission-mode default）"
	case PermissionAcceptEdits:
		return "沙箱开启，编辑放行，命令逐条确认（claude --permission-mode acceptEdits）"
	case PermissionAuto:
		return "沙箱开启，越界由 LLM Guardian 判定（claude --permission-mode auto）"
	case PermissionFull:
		return "沙箱关闭，无审批（claude --permission-mode bypassPermissions）"
	}
	return ""
}

// SortedPermissionTiers 排序后的档位名（给 --engines / 报错文案用，保证稳定输出）。
func SortedPermissionTiers() []string {
	out := PermissionTiers()
	sort.Strings(out)
	return out
}
