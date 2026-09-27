package agent

import "sort"

/* 契约版本与能力标识 —— magic-client（桌面 / 移动客户端插件）契约面 v1。
 *
 * 为什么需要（2026-09-27）：客户端只能通过「唯一来源」拿到引擎能力，否则每个
 * 接入方都会自己写一遍 switch，最后各自漂移。实测已经踩过的同类事故：
 * 客户端拿不到能力字段 → 界面空白；桌面上层按版本号挑二进制 → 挑到旧构建。
 *
 * 与既有 `*SupportOf` 表的关系：那些表给的是「落地方式」（flag:-i /
 * stdin:stream-json / part:file / prompt / none…），属于实现细节；能力 id 是
 * 「结论」（这个引擎能不能收附件、能不能续接）。两者由同一批表推导，实现
 * 细节变化时能力 id 不必跟着变 —— 客户端只认结论。
 *
 * 静态与运行态的边界（重要）：
 *   capabilities  静态声明，回答「能做什么」，不启动任何 CLI 就能算出来；
 *   ok / streaming / models / version  运行态，回答「此刻如何」，探测才拿得到。
 * 客户端按 capabilities 决定界面形态，按运行态决定按钮可用性与降级提示。
 */

// ContractVersion 契约面版本。客户端拿它与自己的期望范围比对，超出范围应**明确
// 报错**并提示升级，而不是带着不匹配的能力继续跑。
//
// 递增规则：只增字段（向后兼容）不升版本；一旦有字段改名、语义变化或删除，
// 必须升版本 —— 老客户端会因版本不匹配而拒绝启动，这正是我们想要的结果。
const ContractVersion = 1

// 能力 id。命名规则：`域.能力`，域只有 session / workspace / attachment /
// model / engine 五个，客户端按域分组渲染即可。
const (
	// CapSessionStream 支持 `--stream`：正文或思考增量实时到达（成色见各引擎注释）。
	CapSessionStream = "session.stream"
	// CapSessionAppend 支持常驻会话 + `--append` 追加消息。
	CapSessionAppend = "session.append"
	// CapSessionAsk 支持「需要用户选择」的往返（`{"type":"ask"}` 事件）。
	CapSessionAsk = "session.ask"
	// CapSessionPermission 支持四档权限模型（manual / accept-edits / auto / full）。
	CapSessionPermission = "session.permission"
	// CapWorkspaceSelect 能指定工作目录（原生 flag 或子进程 cwd）。
	CapWorkspaceSelect = "workspace.select"
	// CapAttachmentNative 附件走引擎的原生通道（图片真的进模型上下文）。
	CapAttachmentNative = "attachment.native"
	// CapAttachmentPrompt 附件只有降级通道：把绝对路径写进提示词，靠引擎自己读文件。
	CapAttachmentPrompt = "attachment.prompt"
	// CapModelList 有动态模型清单来源（`--engines` 的 models 字段）。
	CapModelList = "model.list"
	// CapModelCredits 能给出各模型的积分倍率（`model_credits` 字段）。
	CapModelCredits = "model.credits"
	// CapEngineInstall 有一键安装 / 升级命令（`install` 字段）。
	CapEngineInstall = "engine.install"
)

// CapabilityIDs 返回全部已知能力 id（字典序，稳定）。用于校验与文档生成。
func CapabilityIDs() []string {
	out := []string{
		CapAttachmentNative,
		CapAttachmentPrompt,
		CapEngineInstall,
		CapModelCredits,
		CapModelList,
		CapSessionAppend,
		CapSessionAsk,
		CapSessionPermission,
		CapSessionStream,
		CapWorkspaceSelect,
	}
	sort.Strings(out)
	return out
}

// CapabilitiesOf 返回某个引擎**静态声明**的能力 id 集合（字典序，稳定、无重复）。
//
// 未注册的引擎名返回 nil —— 调用方据此区分「没这个引擎」与「这个引擎什么都不会」
// （后者在当前实现里不可能出现：所有内置引擎至少支持 workspace 或附件降级）。
func CapabilitiesOf(engine string) []string {
	e := Lookup(engine)
	if e == nil {
		return nil
	}
	return capabilitiesOfEngine(e)
}

// capabilitiesOfEngine 按引擎实例推导能力（CapabilitiesOf 的实现）。
//
// 单独拆出来是为了让用例能直接拿引擎实例断言，**不必依赖包级注册表的全局状态** ——
// 注册表有个已知脆弱点（Engines() 之前被 Register 过就不注册内置引擎，见 engine.go），
// 而按名筛选测试时它会让「按名字查表」的结果凭空消失。
func capabilitiesOfEngine(e Engine) []string {
	name := e.Name()
	var caps []string
	add := func(id string) { caps = append(caps, id) }

	if AsStreamer(e) != nil {
		add(CapSessionStream)
	}
	if AppendSupportOf(name) {
		add(CapSessionAppend)
	}
	if !unsupported(AskSupportOf(name)) {
		add(CapSessionAsk)
	}
	if !unsupported(PermissionSupportOf(name)) {
		add(CapSessionPermission)
	}
	if !unsupported(WorkspaceSupportOf(name)) {
		add(CapWorkspaceSelect)
	}
	switch AttachmentSupportOf(name) {
	case "prompt":
		add(CapAttachmentPrompt)
	case "", "none":
		// 无附件通道：不加任何附件能力。
	default:
		add(CapAttachmentNative)
	}
	if ModelListerOf(e) != nil {
		add(CapModelList)
	}
	if _, ok := e.(ModelCreditLister); ok {
		add(CapModelCredits)
	}
	if InstallCommandOf(name) != "" {
		add(CapEngineInstall)
	}

	sort.Strings(caps)
	return caps
}

// unsupported 判断一张能力表的取值是否表示「没有这条通道」。
// 空串与 "none" 都算没有 —— 两张表历史上两种写法都出现过，这里统一兜住。
func unsupported(v string) bool {
	return v == "" || v == "none"
}

/* 核心级能力 id：不属于任何单个引擎，而是 core 自己提供的**通道**。
 *
 * 与引擎能力分开命名（`feature.` 前缀）与分开返回：客户端要回答的是两个不同问题 ——
 * 「这个引擎能做什么」（engines[].capabilities）与「这台机器上的 core 支不支持这条通道」
 * （features）。混在一个集合里会让前者随 core 升级而无故变化。
 */

const (
	// FeatureSessionEvents `--stream --events`：事件流带契约版本与行号，并先发一行 ready。
	FeatureSessionEvents = "feature.session.events"
	// FeatureSessionControl `--stream --control`：stdin NDJSON 控制通道（打断 / 收工 / 回审批）。
	FeatureSessionControl = "feature.session.control"
)

// CoreFeatures 返回 core 自身提供的通道能力（字典序，稳定）。
func CoreFeatures() []string {
	return []string{FeatureSessionControl, FeatureSessionEvents}
}
