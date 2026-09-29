# 客户端契约（magic-agent ↔ magic-client ↔ 业务项目）

这一页是**唯一**该被引用的契约说明。它回答两个问题：core 开放了哪些面、插件承诺了哪些面。
实现位置写在每一节里；改契约必须同时改这里。

版本规则：**只增字段不升版本**；一旦字段改名、语义变化或被删除，必须升 `contractVersion`。
客户端（桌面 / 移动）与插件启动时都比对该字段，不匹配就**明确报错**，不带着不匹配的能力跑起来 ——
这类漂移在界面上表现为空白，是最难定位的一类问题。

## 一、core 开放的面（`magic-agent`）

| 面 | 命令 / 通道 | 状态 | 实现 |
| --- | --- | --- | --- |
| 引擎调用 | `-e <engine> -p "<prompt>"`（含超时、重试、退出码） | 稳定 | `internal/cli/ask.go` |
| 探测与能力 | `--engines [--no-models]`：`ok` / `bin` / `install` / `version` / `models` / `capabilities` … | 稳定 | `internal/cli/root.go` |
| 契约 envelope | `--contract`：`{contractVersion, features, engines[]}`，`engines[]` 与 `--engines` **同构** | v1 | `internal/cli/root.go` + `internal/agent/capability.go` |
| 事件流 | `--stream --events`：NDJSON，每行带 `v` / `seq`，首行 `ready` | v1 | `internal/cli/events.go` |
| 控制回传 | `--stream --control`：stdin 收 NDJSON 命令（`ping` / `interrupt` / `stop` / `answer`） | v1 | `internal/cli/control.go` |
| 会话追加 | `--stream --keep-alive` + `--append <id>`；`--sessions` / `--stop <id>` | 稳定 | `internal/cli/session.go` |

### 1.1 事件流形状

未开 `--events` 时是历史形状（逐字节不变）：

```json
{"type":"thinking","text":"…"}
{"type":"text","text":"…"}
{"type":"tool_use","text":"<args JSON>","name":"Bash","id":"toolu_x"}
{"type":"tool_result","text":"<output>","id":"toolu_x"}
{"type":"turn_end","text":"<该轮正文>","session_id":"…"}
{"type":"ask","text":"<一行摘要>","ask":{…}}
{"type":"result","engine":"…","model":"…","session_id":"…","text":"<全文>","thinking":"…","tools":[…]}
{"type":"error","engine":"…","reason":"<根因>","error":"<完整错误链>"}
```

开 `--events` 后**字段集不变**，每行多 `v` / `seq`（`seq` 从 1 单调 +1），
并额外产出四类 core 自己的事件：`ready`（首行，带 `engine` / `model`）、
`pong`、`interrupted`、`stopped`、`answered`、`control_error`。

两条不变量（有测试钉住）：**每轮一定以 `result` 或 `error` 收尾**；
`--events` 与 `--stream` 的字段集只差 `v` / `seq`。

### 1.2 控制命令

```json
{"op":"ping"}                     → {"type":"pong"}
{"op":"interrupt","reason":"…"}   → 打断当前轮；本轮真的结束再发 {"type":"interrupted"}
{"op":"stop"}                     → 收工（关常驻会话）→ {"type":"stopped"}
{"op":"answer","text":"…"}         → 作为下一轮 user 消息注入常驻会话 → {"type":"answered"}
```

`answer` 只在常驻会话生效时可用，且**必须**走这条路：headless 下模型提问后会立刻自行拒绝，
唯一能把答案喂给模型的通道就是下一轮 user 消息。不可用时回 `control_error`，不静默丢。

⚠️ 开了 `--control` 的进程，**stdin 归控制通道**，提示词必须由 `-p` / 位置参数 / `--file` 给。

### 1.3 两级能力

| 集合 | 位置 | 回答的问题 |
| --- | --- | --- |
| `features[]` | `--contract` 顶层 | 这台 core 支不支持某条**通道**（`feature.session.events` / `feature.session.control`） |
| `engines[].capabilities[]` | 每个引擎 | 这个**引擎**能做什么（`session.stream` / `attachment.native` / `engine.install` …） |

静态与运行态严格分开：`capabilities` / `features` 是静态声明，`ok` / `streaming` / `models` /
`version` 是运行态。客户端按前者定界面形态，按后者定控件可用与降级提示。

## 二、插件承诺的面（`magic-client`）

| 面 | 路径 | 状态 |
| --- | --- | --- |
| 契约与引擎清单 | `GET /desk/contract` · `GET /desk/engines[?models=1]` | 已实现 |
| 应用配置 | `GET /desk/profile` | 已实现 |
| 模块清单 | `GET /desk/modules`（从 profile 派生：id / 入口 / 声明的表） | 已实现 |
| 会话登记表 | `GET /desk/sessions`（core 的 `--sessions`，跨进程持久） | 已实现 |
| 项目业务接口 | `ANY /api/*` → 原样转发到 profile 的 `backend.url` | 已实现 |
| 发起一轮 | `POST /desk/ask`（SSE 中继 core 的 NDJSON，前置 `event: open`，收尾 `event: end`） | 已实现 |
| 控制与回审批 | `POST /desk/ask/{id}/control`（与 core 的 `--control` 同协议，插件不解释 op） | 已实现 |
| 运行中列表 | `GET /desk/asks`（**本进程**正在跑的轮次；与 sessions 的区别见下） | 已实现 |
| 静态资源 | `GET /`（`--ui-dir` + profile 的 `ui.entry`） | 已实现 |
| 鉴权 | `X-Magic-Token` 头或 `?token=`；默认只绑 `127.0.0.1`；`--lan` 时绑 `0.0.0.0` 且令牌强制 ≥24 字符（见 2.4） | 已实现 |

### 2.1 扩展接口（业务项目对着写）

| 接口 | 状态 | 由什么承担 |
| --- | --- | --- |
| 模块注册 | 已实现 | profile 的 `modules[]` + `GET /desk/modules`；前端按需加载 `modules[].entry`，模块通过 `window.magicModules[<id>] = { id, title, render(host) }` 注册实现 |
| 数据存储 | 已实现（转发） | `ANY /api/*` → 项目自己的后端。**插件不建存储**：真相在项目侧 |
| 视图与导航 | 已实现（最小） | 基础版 UI 的列表页展示模块，点开可进入对话；完整路由待接 |
| 设置分段 | 已实现（转发） | 走 `/api/*`；项目自己的设置实现（magic-test 的 `config-store.cjs`）照旧 |
| 动作命令 | 已实现（转发） | 走 `/api/*`；项目侧注册动作，前端从对话里调 |
| 产物写入 | 已实现（转发） | 走 `/api/*`（magic-test 的 `/api/media/*` 就是） |
| HTTP 路由 | 已实现 | 同上：模块的路由都在项目后端的 `/api/*` 下 |
| 主题覆盖 | 已实现 | profile 的 `ui.themeVars`，插件注入页面根元素 |
| 事件订阅 | 已实现（只读） | 模块直接订阅 `POST /desk/ask` 的 SSE。**不允许改写对话流** |

两条写死的约定：**事件只读**（模块不能改写对话流，否则基础版行为会随模块漂移）；
**业务不进基础版数据**（模块的数据表由模块自己声明，插件只转发请求，不做领域假设）。

### 2.2 模块运行时契约

模块是普通脚本（不是 ESM），由基础版 UI 动态加载后调用它注册的 `render(host)`。
它能拿到的全部东西就是 `window.magic` 与一个空的宿主节点：

| 项 | 说明 |
| --- | --- |
| `window.magic.api(path, opts)` | 已带令牌的 `fetch` 包装，返回 `Response`。模块**不自己拼令牌** |
| `window.magic.on(type, fn)` | 订阅会话事件（**只读**；`type` 传 `'*'` 收全部）。模块内部异常被隔离，不会打断对话流 |
| `window.magic.profile` | 本应用的 profile |
| `window.magic.version` | 运行时契约版本（当前 `1`） |
| `window.magicModules[id]` | 模块注册自己：`{ id, title, render(host) }` |

三条纪律：模块**只调 `/api/*` 与 `/desk/*`**（不许另找数据源）；只往宿主节点里塞 DOM；
要注入样式就带自己的前缀作用域（如 `rq-`），不许改基础版 UI 的节点与令牌。

还有一条契约要求：**`render(host)` 要在首屏数据到位后再 resolve**。界面上看不出来差别
（DOM 稍后自己更新），但不满足它时测试台只会看到「读取中…」，等于这条自动化检查失效 ——
`media.js` 第一版就栽在这里（`load()` 忘了 return promise）。

模块在进界面之前先过 `tools/module-harness.mjs`：它用最小 DOM 桩加真实形状的假数据，
验「注册成功、渲染不抛异常、渲染结果含预期文案、只调允许的接口」。
第一个实战模块（需求看板）就是靠它抓到问题的 —— 界面上「渲染时抛异常」只表现为一片空白。

### 2.3 `/api/*` 转发规则

| 规则 | 说明 |
| --- | --- |
| 路径 / 方法 / 查询串 / 请求体 | 原样转发 |
| 响应 | 原样回来；`FlushInterval=-1`，SSE 逐条到达不被缓冲 |
| 凭据 | `backend.token` 由**服务端**注入（`Authorization: Bearer`，可用 `tokenHeader` 改），不进浏览器 |
| 插件令牌 | `X-Magic-Token` **不带去后端** —— 那是两套凭据，混传会让后端误以为要校验它 |
| 未配置后端 | `501` + 一条点名 `backend.url` 的说明，不静默 404 |
| 地址非法 | **启动即失败**（比运行时 502 好查） |

### 2.5 局域网接入（`--lan`）

默认只绑 `127.0.0.1`：本地端口对同机所有进程可见，不出鉴权不敢往外露。
手机 / 平板要连就得绑局域网，而这一开，**令牌就是唯一的门锁** —— 所以两者是配套的：

| 行为 | 说明 |
| --- | --- |
| `--lan` | 绑 `0.0.0.0`，启动时把本机**所有**可用局域网地址连令牌一起打印（一台机器常有多个网卡） |
| 令牌下限 | 24 字符；弱令牌**拒绝启动**（自动生成的默认令牌是 32 位十六进制，够用） |
| 无令牌访问 | `401` —— 不因为是内网就放行 |
| 不带 `--lan` | 局域网地址**连不上**（默认就是安全的） |

扫码配对在 v1 就是「扫这段 URL」（URL 自带令牌）。带 TTL 的配对码交换属于 L2（跨网）的活，
需要服务端与令牌存储，**未做**。

### 2.4 `POST /desk/ask` 的请求体

| 字段 | 说明 |
| --- | --- |
| `engine` | 必填；profile 的 `engines` 白名单之外的会被 403 |
| `prompt` | 必填 |
| `model` / `workspace` | 可选，透传给 core 的 `-m` / `-w` |
| `permission` | 可选，四档授权模型（`manual` \| `accept-edits` \| `auto` \| `full`），透传给 core 的 `--permission`。**空 = 不传**，由 core 用它自己的默认档（`agent.DefaultPermissionTier`）—— 刻意没有默认预设，见下 |
| `session` | 可选，续接一个已有会话（传 `/desk/sessions` 里的 id）→ core 的 `--session` |
| `idle` | 可选，常驻会话空闲收工时长；默认 `0` = 本轮结束就收工 |

关于 `permission` 的两条语义，客户端必须照做：

1. **没选就原样不传，不要替 core 补默认值。** core 只在**显式传过** `--permission` 时
   才校验该引擎支不支持（`internal/cli/ask.go::resolvePermissionTier`），替用户预设
   会把「显式指定」与「沿用默认」混成一种，还会让不支持的引擎凭空报错。
2. **档位定义以 `internal/agent/permission.go` 为唯一真相**，前端不要另排一套顺序或文案。
   当前仅 `claude` / `codebuddy` / `codebuddy-ai` 接线，可从 `/desk/engines` 的
   `capabilities` 里读 `permission`；读不到时前端应把选择器**禁用**而不是传过去。
   `full` 等同 `--dangerously-skip-permissions`，两端界面都应把它显式标成危险档。

`asks` 是**本进程内存里**的运行中轮次，`sessions` 是 core 的持久登记表 —— 两者不是一回事，
界面上的「运行中 N」用前者，「最近会话」用后者。

### 2.2 扩展接口（业务项目对着写）

| 接口 | 状态 | 说明 |
| --- | --- | --- |
| 模块注册 | 部分 | profile 的 `modules[]` 已声明；前端按视图激活时机动态加载待接 |
| 视图与导航 | 待接 | 路由与左栏导航项由模块注册 |
| 数据存储 | 待接 | 模块声明自己的表（`dataTables`），插件提供迁移与读写 |
| 设置分段 | 待接 | 复用段式存储（对应 magic-test 的 `config-store.cjs`） |
| 动作命令 | 待接 | 把业务动作注册成命令，可在对话里触发 |
| 产物写入 | 待接 | 往统一产物索引写图片与文件 |
| 事件订阅 | 待接 | 只读订阅会话事件；**不允许改写对话流** |
| HTTP 路由 | 待接 | 模块注册 `/api/*` 下的路由 |
| 主题覆盖 | 待接 | 覆盖主题令牌与图标 |

两条写死的约定：**事件只读**（模块不能改写对话流，否则基础版行为会随模块漂移）；
**业务不进基础版数据**（模块的数据表由模块自己声明，插件不做领域假设）。

## 三、公共对话 UI 组件（对接方直接用它）

插件只提供**基础对话 UI**：消息流 + 输入框 + 引擎/模型/授权三项选择。会话列表、
工作区、业务视图等**不在这里** —— 那些由对接方按自己的界面实现，再把选中的会话交给组件。

**magic-agent 自己不跑任何服务。** 组件也不连后端：数据从哪来由宿主注入一个
`transport` 决定（掌天瓶走 IPC → CLI，别的对接方可能走 HTTP）。所以这里的对接方式是
「宿主实现 transport + 把组件挂进自己的界面」，不存在「先起一个服务再连它」。

### 3.1 两份宿主，一份实现

| 文件 | 角色 |
| --- | --- |
| `client-ui/shared/conversation.js` | **组件本体**（渲染 + 流式 + 控制 + 三项选择器）。PC 与移动端共用，是唯一来源 |
| `client-ui/shared/conversation.css` | 组件样式。结构尺寸共用；颜色走 3.3 的变量契约，由宿主供值 |
| `client-ui/shared/transport-http.js` | 传输层的**可选参考实现**（HTTP 版，打 `/desk/*`）。组件不依赖它 |
| `client-ui/index.html` | PC 宿主：oklch 令牌 + 一条顶栏 + 挂组件 |
| `client-ui-mobile/index.html` | 移动端宿主：AA 安卓端那套十六进制令牌 + 移动形态顶栏 + 挂组件 |

移动端打包时（magic-test 的 `tools/build-mobile-www.cjs`）会把 `client-ui/shared/`
一并复制进 `www/shared/` —— 页面按同级 `shared/` 引用它，缺了这个复制对话区会是空白。

### 3.2 组件 API

```js
const conv = MagicConversation.create({
  el,            // 必填：把对话区 + 输入区挂进去的容器
  transport,     // 必填：见下（组件不自己连后端）
  session,       // 可选：传了就从这段会话开始（会走 transport.readSession）
  engine, model, permission,   // 可选初始值（空 = 不传，core 用它自己的默认）
  engines,       // 可选：给了就用这份清单，不给则调 transport.listEngines()
  titleEl,       // 可选：宿主显示会话标题的元素
  onEvent,       // 可选：每个 core 事件透传一份（只读，不许改写对话流）
  onBusy,        // 可选：一轮开始/结束
});
conv.send('…');
conv.interrupt(); conv.stop(); conv.answer('…');
conv.setSession(sid, s); conv.newSession();
conv.setEngine(n); conv.setModel(n); conv.setPermission(v); conv.setEngines(list);
conv.refreshEngines(); conv.loadHistory(sid, after?); conv.destroy();
```

### 3.2.1 transport 契约（宿主实现，四个方法）

```js
{
  listEngines()                         // → Promise<[{engine, ok, models, capabilities}]>
  ask(payload, {onOpen, onEvent, onEnd})// payload: {engine, model?, permission?, session?, prompt}
                                        // onOpen(runID) 给控制通道的落点
                                        // onEnd(exitCode) **必须**被调一次，否则界面会一直卡在 busy
  control({id, op, text?})              // op: interrupt | stop | answer → Promise
  readSession(id, {after?})             // → Promise<{events, lastSeq?, snapshotRequired?}>
}
```

掌天瓶那一侧的 transport 走 IPC → CLI；`transport-http.js` 是走 `/desk/*` 的现成实现。

### 3.3 主题：颜色变量契约

宿主必须给出下面这组变量的值（组件样式只用不定义）。两套现成的值可抄：
PC 用 `index.html` 里的 oklch 组，移动端用 `client-ui-mobile/index.html` 里的
AA 十六进制组 —— 换掉这组值就换掉整套调性，组件代码一行不用动。

```
基础  --canvas --ink --ink-soft --muted --faint --border --subtle --raised
      --primary-action --on-primary-action --app-shadow
对话  --bubble --bubble-text --code --status-neutral --status-neutral-text --activity
md    --md-body --md-muted --md-border --md-code-bg --link --prompt --shimmer --panel --panel-border
语义  --accent --warning --success --error-surface --error-border --error-text --error-icon
      --running --transitioning --active
几何  --r-card --r --r-full --font --mono
```

Profile 的 `ui.themeVars` 是同一套变量的运行时覆盖。

### 3.4 会话数据：走 CLI，不走服务

magic-agent 把会话的查询与修改都做成 CLI 命令，调用方 exec 即可（magic-test 的
`desktop/agent-cli.cjs` 已经这么调）：

| 操作 | 命令 |
| --- | --- |
| 列出会话 | `magic-agent --sessions` |
| **读会话历史** | `magic-agent --session-log <session_id\|run_id>` → `{session, events}` |
| 增量续读 | 追加 `--after <seq>` → 另带 `count / after / lastSeq / snapshotRequired` |
| 停止会话 | `magic-agent --stop <session_id\|run_id>` |
| 追加消息 | `magic-agent --append <session_id\|run_id> -p "…"` |
| 跑一轮（流式 + 控制） | `magic-agent --stream --events --control -e <engine> -p "…"`，控制命令从 stdin 进 |

`--session-log` 的输出形状与插件同名的 HTTP 接口**逐字段对齐**，两条路可互换。
边界语义：`after == lastSeq` 是**追平**（正常空增量，`snapshotRequired=false`）；
`after > lastSeq` 才要求重拉全量。

### 3.5 两条不变量

- **事件只读**：组件不接受对接方改写对话流，否则基础版行为会随对接方漂移。
- **授权档位以 `internal/agent/permission.go` 为唯一真相**；`full` 等同
  `--dangerously-skip-permissions`，两端界面都必须把它显式标成危险档。

## 四、三层的责任边界

| 层 | 负责 | 明确不做 |
| --- | --- | --- |
| core | CLI 集成：调用、探测与能力、事件流、控制回传、会话追加、契约 | 不做界面、不做 HTTP 服务、不打包前端资源 |
| 插件 | 契约校验、事件中继、基础版 UI、扩展接口、本地服务与壳 | 不自己调引擎、不解析引擎输出、不写业务视图 |
| 业务项目 | 一份 profile + 业务模块 | 不复制通用件、不改基础版 UI 源码 |

可验证的三条：装插件前后主二进制 hash / 体积 / `--engines` 输出一致；插件不 `import` core 的内部包；
插件缺失时 core 一切照旧。
