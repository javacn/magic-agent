# anywhere 对照差距分析

> **基线版本**：agents-anywhere `873f4ae`（2026-09-28，`Fix DSH Auto review permission synchronization and UI (#174)`）
> 源码位置：临时目录 `aa-src`（`git clone --depth 1`）
> **对照对象**：`magic-agent`（Go CLI + 插件）、`magic-test`（Electron PC 工作台）、`client-ui-mobile`（Capacitor 移动端）
> **分析日期**：2026-09-28

---

## 0. 规模对照

| 子系统 | 位置 | 文件 | 行数 |
|---|---|---|---|
| connector | `aa-src/connector` | 277 | 74,626 |
| server | `aa-src/server` | 237 | 70,911 |
| android app | `aa-src/android/app/src` | 154 | 41,977 |
| desktop renderer | `aa-src/desktop-workbench/renderer/src` | 264 | 50,446 |
| desktop electron | `aa-src/desktop-workbench/electron` | 50 | 7,805 |
| **合计** | | **982** | **245,765** |

| 我们的项目 | 规模 |
|---|---|
| magic-agent（Go，不含测试） | ~18,600 行（core+plugin） |
| magic-test `app.js` + `app.css` + `index.html` | 21,008 行（30 个分片拼成 `app.js` 14,946） |
| client-ui-mobile `index.html` | 1,595 行（单文件，零构建） |

**量级差 ~12 倍**。但这个数字本身没有意义 —— anywhere 是一个**含服务端的多用户 SaaS**，我们是**单机 CLI + 桌面/移动客户端**。下面按「值不值得补」分层。

---

## 1. 技术原理差距

### 1.1 【P0】事件恢复语义：只有 seq 去重，没有「续读」

这是 anywhere 最硬的一块设计，也是我们最薄的一块。

**anywhere 的模型**（`docs/migrations/event-recovery-v2.md`、`server/agent_server/services/event_recovery.py:60-195`）：

```
GET /api/v2/sessions/{id}/events?after=seq:N
  → 二选一，不存在第三种结果：
     ① 确定性增量（snapshotRequired:false）
     ② 空 + snapshotRequired:true（当 N > current，游标在客户端侧超前了）
```

关键设计：
- 游标是 `seq:<revision>`，revision 上限 `2^53-1`，拒绝前导 0 与非 ASCII（`server/agent_server/core/events.py:13-40`）
- 事件 ID **确定性生成**：`evt_{sequence}_{sha256(type,payload)[:12]}` —— 同 seq 同内容 ID 恒定，所以去重可以只靠 ID（`events.py:43-62`）
- `after == current` 时也要回一条（`session.meta.updated` + `runtime.capability.updated`），因为在线状态/能力**不推进持久化 seq**（`event_recovery.py:92-124`）
- 三次稳定性重试：`start_seq != current_seq` 就放弃转快照（`:126-153`）
- 替换屏障 `timeline_reset_seq`：时间线被重置后旧事件不再有效（`:128-134`）
- 下行去重**分层**：durable 事件按精确 `eventId` 集合（有界 LRU）、capability 按语义指纹、live projection 按 `(type[,catalogType])` **相邻**去重 —— 保留同 seq 的 `A→B→A`（`server/agent_server/api/sessions.py:1099-1134`）
- 上行 ingest 是 **at-least-once**：失败批次留在队首，且**重试不注入新 revision**（`connector/connector/server/ingest.py:77-113`）

**我们的现状**（`internal/cli/events.go:20`、`client-ui-mobile/index.html:1266-1267`）：
- 有 `seq` 单调 +1，`render()` 靠 `state.renderedSeq` 跳过重复
- 进会话先拉 `/desk/session/{id}/messages` 全量 JSONL，再开 SSE
- **没有 resubscribe 端点**。`arkclaw_stream.go:37` 明确不实现 `tasks/resubscribe`；`codebuddy_gateway_stream.go:45` 网关也没有
- SSE 是 `fetch` + `getReader()` 手写解析（`index.html:1399-1422`），**不享受浏览器原生自动重连**；`client-ui/index.html:392-395` 那个 `EventSource` 猴补丁实际是死代码

**差距本质**：我们做的是「重连后重放全量 + 客户端去重」；anywhere 做的是「重连后续读增量，客户端只管带游标问」。后者在长会话下省一个数量级的流量，且天然处理「重连期间事件已过期」。

**补法**：给插件加 `GET /desk/session/{id}/events?after=<seq>`，返回 `{events[], snapshotRequired}`。core 侧已有 `session.ReadSessionLog`，补一个「按 seq 过滤」即可，**不需要新存储**。

### 1.2 【P0】去重缺三道闸

anywhere 有四道（`connector/connector/server/ingest.py:231-259`、`notification_coalescer.py:15-47`、`server/agent_server/core/timeline.py:20-26`、`docs/api/capability-event-deduplication.md`）：

| # | 层 | 手法 |
|---|---|---|
| 1 | Connector 批内 | 同 `(sessionId, itemId)` 只留最后一条 upsert |
| 2 | Connector 发送侧 | assistant 文本 100ms 窗口合并，其余先 flush 再发 |
| 3 | Server timeline | 幂等：只比 `contentHash`，未变直接跳过；item revision 取 `max(incoming, existing+1)` |
| 4 | Server→Web | 分层去重（见 1.1） |

**我们只有第 4 层的最简版**（`state.renderedSeq`）。缺 1-3 的直接后果：多单元工具调用（一条 `tool_use` 装多次调用）时，`seq` 单调但**没有内容幂等**，重放会产生视觉重复。

### 1.3 【P1】能力协商只有两级，没有「有效能力」

**anywhere 三层**：

| 层 | 谁产出 | 语义 |
|---|---|---|
| inventory | Runtime 自己 | 「我**能**做什么」（`catalog.model`、`session.commands`…） |
| protocol capability | Connector 映射 | 「我能**在协议里**说什么」（`connector/server/capabilities.py:13-23` 白名单映射） |
| **effective** | Server 叠加策略 | 「**现在允许**你做什么」= `supported && runtimeAvailable && connectorOnline`；`allowed = runtimeAllowed && session.takeover`（`server/agent_server/services/effective_capabilities.py:223-256`） |

有效能力有**稳定的机器码**错误：`connector_offline` / `session_not_taken_over` / `runtime_capability_unsupported` / `runtime_capability_unavailable`（`:271-291`）—— UI 可以据此说清「为什么不能点」而不只是灰掉。

capability set 的 `revision` 由连接器侧 `ProtocolRevisionClock` 产出，Unix 微秒 + 强制 `max(candidate, last+1)` 保证同机严格单调不回退（`connector/connector/server/protocol_revision.py:7-19`）。

**我们的现状**（`CLIENT-CONTRACT.md:57-65`）：两级且**分得很干净** —— `features[]`（通道级 `feature.session.events` / `feature.session.control`）+ `engines[].capabilities[]`（10 个静态 id）。客户端按静态定界面形态，按运行态（`ok`/`streaming`/`models`）定按钮可用。

**评价**：两级对我们够用 —— 单机没有「多租户策略叠加」这层需求。**但缺稳定错误码**：现在 `takeover` 关掉时，UI 只能知道「不能发」，说不出「因为会话没被接管」。补 3-4 个机器码的成本极低，收益是 UI 能给出可执行的提示。

### 1.4 【P1】凭据分层：我们只有一层半

**anywhere 四层**：

| 层 | 凭据 | 生命周期 | 落点 |
|---|---|---|---|
| L0 | `connector_token` | 长期 | 配置文件 `chmod 0600`（`connector/core/config.py:82-104`） |
| L1 | `accessToken` | 短期 | 内存，**提前 60s 刷新**（`connector/server/auth.py:13,74-83`） |
| L1.5 | ws-ticket | 一次性 | 客户端换取 WS 连接（`docs/api/realtime.md:35-39`） |
| L2 | — | — | 日志脱敏全集 + 状态文件 0600 + 目录 0700（`connector/server/rpc.py:20-40`） |

**失效分级**做得很好：`ConnectorAuthenticationError` 明确标注 "invalid or revoked; **do not retry**"（`auth.py:16-17`），run_forever 直接终止；而网络错/5xx/429 走指数退避。WS 关闭码只有 `4001` **且** reason 含 `invalid/revok/delet/credential` 才判凭据死（`client.py:520-528`）—— 1008 视为可刷新。

**我们**：`Guard` 一个 `X-Magic-Token` 常量令牌（`internal/client/guard.go`），进程内比常数时间。**没有过期、没有刷新、没有失效分级** —— 泄漏即长期泄漏，且换令牌要重启进程。

### 1.5 【P2】revision 语义：anywhere 有四种，我们有一种

| 语义 | anywhere | 我们 |
|---|---|---|
| 会话事件游标 | `seq:<revision>`，PostgreSQL 租约区间 + Redis 头 | `seq` 单调 +1，进程内 |
| capability 集 revision | 连接器微秒时钟 | 无 |
| config revision | Server 驱动更新 | 无 |
| timeline item revision | 内容变更计数 | 无 |

四种**不能混用** —— 这是 anywhere 踩过坑才拆开的（`docs/api/realtime.md:175-190`：稳定通道名不随协议重构改名）。我们现在只有一种，所以还没踩到；但**如果将来加能力协商，必须从一开始就分开命名**。

### 1.6 我们做对的（不要改）

| 我们的设计 | anywhere 怎么做 | 评价 |
|---|---|---|
| 契约版本 v1，不匹配**启动失败**（`internal/client/service.go:109-113`） | `SUPPORTED_PROTOCOL_VERSIONS = ["1.0"]` | 一样。**我们更硬** —— anywhere 当前仅作契约导出未强制 |
| 分层守卫测试（`internal/boundary/boundary_test.go:32-35`） | `test_connector_architecture.py:19-47` 禁导入 `_reference` | 同思路，ours 更轻 |
| 写盘失败只 warn 不抛错（`sessionlog.go:11-17` 三条铁律） | — | **我们独有**。这条铁律是对的：界面卡死比丢日志严重 |
| 异步落盘 goroutine（cap 128） | — | 我们独有，正确 |
| 插件不进 core 依赖闭包 | runtime 不许发 server 通知 | 同思路 |

---

## 2. UI 设计差距（PC 端）

### 2.1 技术栈代差 —— 这是根因

| | anywhere | magic-test |
|---|---|---|
| 框架 | Next.js 16 + React 19 + App Router | 无（vanilla JS） |
| 样式 | **Tailwind CSS v4.3.1**（CSS-first，无 config） | 手写 CSS 5,023 行 |
| 组件 | **shadcn/ui new-york + 55 个组件文件** | 0（`app.js` 全是 innerHTML 模板串） |
| 主题 | `next-themes` + OKLCH 令牌 | `[data-theme]` 覆写 CSS 变量 |
| 图标 | lucide + remixicon + iconify | 内联 SVG 字符串 |
| 变体 | `cva`（class-variance-authority） | 手写 if/else |
| 构建 | yarn + Turbopack | `tools/build-src.cjs` 字符串拼接 |

**结论：不能搬代码，只能移植规格。** 差距不在「少了哪个组件」，在于**没有组件系统** —— 同一个形制要在 3 处各写一遍，改一次漏两处。

### 2.2 令牌：我们在色相上更强，在结构上更弱

| 维度 | anywhere | magic-test | 判定 |
|---|---|---|---|
| 色彩空间 | OKLCH，感知均匀 | HEX（`#D6402A` 朱砂 / `#0D8A6A` 玉） | **我们更有识别度** |
| 明暗映射 | 24 个语义令牌成对定义 | 同样成对，但分散在 `:root` + `[data-theme]` + 40+ 处组件级覆写 | anywhere 集中，我们分散 |
| 圆角 | `--radius: 0.625rem` 派生 6/8/10/14/18/22/26px，浮层**封顶 24px** | `--r-m:10px` / `--r-l:16px` 两档 | 我们**只有两档**，缺小控件档 |
| 阴影 | 三级 `sm`/`lg`/`xl`，**始终叠 1px `ring-foreground/5`** | `--shadow-1/2` 两级 | anywhere 的「阴影+内描边」质感我们没有 |
| 字号 | Tailwind 默认（12/14/16/18/20/24/30/36） | 10-12px 密集工作台 | **这是刻意的**，不要改 |
| 字体 | Geist + Geist Mono + Instrument Serif + 自托管 Caveat | `--serif/--display/--mono` | 相当 |

**最值得抄的一条**：anywhere 的阴影从来不是纯投影，而是 `shadow-sm + ring-1 ring-foreground/5`（暗色 `/10`）—— 模拟半透明边框。这让浮层在两种主题下都有边界，同时不显脏。

**圆角缺档的具体后果**：我们现在只有 10/16px，于是「8px 的代码面板」和「6px 的小徽章」只能都塞 10px 或 16px —— 这就是为什么我上次写 `.qa-cf` 时不得不自己定 8px、`qa-st` 定 4px，**跟主刻度不搭**。补一档 `--r-s:6px` 就能收。

### 2.3 组件：我们缺 11 个基础件

anywhere `src/components/ui/` 55 个文件。我们**完全没有**的：

| 组件 | 用途 | 我们的替代 |
|---|---|---|
| `resizable` | 拖拽分栏 | `15-layout.js` 手写 drag 事件 |
| `scroll-area` | 自绘滚动条 | `app.css:120-122` 自绘 |
| `tooltip` | 悬浮提示 | `title=` 属性（原生、样式不可控） |
| `popover` / `hover-card` | 浮层 | Floating UI 手写定位 |
| `context-menu` | 右键菜单 | 各处 `position:fixed` 手搓 |
| `alert-dialog` | 危险确认 | `.set-mask` 自绘 |
| `drawer` / `sheet` | 抽屉 | 无 |
| `sonner` | toast | `#toast` 单个 div |
| `skeleton` | 加载占位 | 无（直接空着） |
| `empty` | 空态 | 各视图手写 |
| `accordion` | 折叠 | `.qa-tool` / `.qa-mk` 各写一套 |

**优先级**：`resizable`（面板分栏是 PC 端刚需）、`tooltip`（工具卡的 `title` 不可控导致我现在只能把参数 JSON 塞 title）、`skeleton` + `empty`（空态目前是裸文本）。

### 2.4 anywhere 自己的设计债（不要照抄）

调研中发现的**真实缺陷**，抄了会一起继承：

1. `sidebar.tsx:489` 用 `hsl()` 包裹 OKLCH 变量 → **该声明直接失效**
2. `--destructive-foreground` 在 `globals.css:35-36` 映射了令牌但 `:root`/`.dark` **都没定义**
3. `--sidebar-primary` 明暗**不映射**（亮纯黑 / 暗靛蓝 `oklch(0.488 0.243 264.376)`）
4. 等宽字体**三处硬编码**，未复用 `--font-geist-mono`
5. **主应用无 CJK 兜底栈**（只有 onboarding 有 `PingFang SC`）—— 我们有
6. **四套断点体系并存**：Tailwind `md`(768) / CSS `899` / `599` / `359` / `@container 540` / `300`

### 2.5 PC 端交互差距（已在前几轮补掉的除外）

| 项 | anywhere 规格 | 我们的状态 |
|---|---|---|
| 内容列宽 | 消息流 800px / composer 768px 居中 | **未做**，全宽拉伸 |
| 窗口质感 | 隐藏标题栏 + `-webkit-app-region:drag`；macOS `vibrancy:"sidebar"`、Win `backgroundMaterial:"mica"`（build≥22621）；最小 1040×680 | 自绘标题栏 + 自绘红绿灯，无材质 |
| 阻塞审批栈 | 背面 ghost 卡 `-8px` / 缩放 `1-0.014d` / 透明 `1-0.16d`，最多 3 张；顶层 `max-h-[38vh]` | 提问卡内联在消息流 |
| 侧栏行状态 | 三态：待审批绿 pill / 运行中 spinner / 未读点；有指示器时隐藏 hover 操作 | 计数徽标 |
| 懒加载文件树 | 缩进 20px、行高 32px、`role=tree` 全键盘导航、循环目录检测 | **无文件树** |
| 终端多标签 | xterm、双击重命名(180ms)、三击关闭、seq 去重回放 | **无终端** |
| i18n | `messages/en.json` + `zh-CN.json` | 中英混排硬编码 |
| 动态配置表单 | JSON Schema + uiSchema 渲染，Ajv 校验 | 手写 |

---

## 3. 移动端（client-ui-mobile）差距

我们已按 anywhere 的 Compose 版逐字段复刻（气泡 22dp/16.5sp、工具卡 8dp/14dp、代码面板 14dp、composer 22dp+28dp 阴影等）。**这一层的差距最小**。

剩余差距：

| 项 | 差距 |
|---|---|
| **历史缺用户提问** | core 落盘只写引擎事件，**不写用户消息**（`internal/cli/ask.go` 的 `wireLogger`）。渲染器已预留 `kind:"user"` 分支，core 补一条 push 即生效 |
| 事件字段名不一致 | 实时 `type` / 落盘 `kind`（`sessionlog.go:59-68`），渲染器两套都认，但这是**兼容层不是设计** |
| 未知事件 | 刚补的 `default` 分支会兜住，但 anywhere 有 `snapshotRequired` 语义，我们没有 |

---

## 4. 建议路线

### 立刻可做（低风险、高收益）

> **进度（2026-09-28）**：1、2、3 已落地，见下表的「状态」列。

| # | 事项 | 依据 | 状态 |
|---|---|---|---|
| 1 | 插件加 `GET /desk/session/{id}/events?after=<seq>`，返回 `{events[], snapshotRequired}` | §1.1，`session.ReadSessionLog` 加过滤即可 | **已完成** —— `internal/session/sessionlog.go` 的 `ReadSessionLogAfter`（after 排他、maxSeq 覆盖全文件）+ `internal/client/service.go` 的 `handleSessionEvents`。10 项子测试 |
| 2 | core 落盘补 `kind:"user"` | §3，渲染器已就绪 | **已完成** —— `internal/cli/ask.go` 的 `runStreamAsk`（靠 writer 的 pending 缓冲天然保证顺序）。端到端测试 |
| 3 | 补圆角小档 `--r-s:6px`，把 `.qa-cf`(8) / `.qa-st`(4) 收进刻度 | §2.2 | **已完成** —— 补成 4/6/8/10/16 五档；新变体全部改用变量 |
| 4 | 阴影加 `ring-foreground/5` 复合手法 | §2.2 | **部分** —— 令牌已备，但**不适用于本项目**：我们的浮层已有 1px 真边框，叠环成双线。改为提供 `--ring-hair` 供将来无边框浮层使用；同时借 `--elev-*` 修掉一个真实缺陷（暗色下 `#toast` / `.ag-menu` 用的是白底的蓝调投影） |
| 5 | 能力不可用给稳定机器码（`session_not_taken_over` 等 3-4 个） | §1.3 | 待做 |

### 中期（需要设计决策）

| # | 事项 | 决策点 |
|---|---|---|
| 6 | 内容列宽 800/768 居中 | 是否接受宽屏两侧留白 |
| 7 | Electron 窗口材质（vibrancy/Mica）+ 隐藏标题栏 | 是否牺牲部分跨平台一致性 |
| 8 | 阻塞审批栈 | 提问卡从内联改为浮层，需改交互模型 |
| 9 | 引入组件系统（哪怕只是 shadcn 的**规格**而非代码） | 是否值得为 5 万行 CSS 建立约束 |

### 明确不做

| 事项 | 理由 |
|---|---|
| 照搬 OKLCH 令牌 | 会毁掉宣纸/朱砂/玉色的识别度。我们是暖纸工作台，anywhere 是中性灰消费级 App |
| 照搬 14px 字号 | 我们是密集工作台，10-12px 是刻意的 |
| 引入 React/Next | 21,000 行手写代码重写成本远超收益 |
| 建服务端 | 单机场景不需要多租户/租约/Redis |
| 照抄 anywhere 的设计债 | §2.4 六条 |

---

## 5. 一句话结论

**技术原理上，anywhere 真正领先的是「事件可恢复」与「能力可解释」两块** —— 这两块是分布式系统十年踩坑换来的，我们单机场景下不需要全量，但**续读语义**和**稳定错误码**的收益立刻可见。

**UI 上，anywhere 领先的是「有系统」而非「更好看」** —— 它的色彩比我们保守、字号比我们大、组件比我们全，但每样都有刻度。我们缺的不是组件清单，是**约束**：圆角只有两档、阴影只有两级、同一形制在三个地方各写一遍。

**最该先做的三件事**：事件续读端点、core 落盘补用户消息、圆角与阴影补刻度。三件都是半天到一天的量。
