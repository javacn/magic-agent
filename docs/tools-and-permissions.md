# 工具与权限

`--tools` 工具白名单与四档权限模型的落地细节。

## 工具启用（--tools）

`--tools` 同时管两件事：CLI 侧的工具白名单，以及是否在 system prompt 里
注入 `noToolSuffix`。

| `--tools` | CLI 参数（codebuddy） | noToolSuffix | 效果 |
|---|---|---|---|
| `off`（默认） | `--tools ""` | 注入 | 纯 chat，模型不调工具；后缀额外压制「伪工具调用」与「伪造工具返回」 |
| `on` | `--permission-mode <档位>` | 不注入 | 全工具可用（含 `WebSearch` / `WebFetch`）；档位由 `--permission` 决定 |
| `WebSearch,WebFetch` | `--tools WebSearch,WebFetch --permission-mode <档位>` | 不注入 | 仅白名单工具可用 |

> 工具的**可用范围**与**权限档位**是两件事：`--tools` 决定「有哪些工具」，
> `--permission` 决定「执行时问不问人、有没有沙箱」。详见下一节。
> claude 同理（`--tools ""` / `--permission-mode <档位>`）。

> trae 的白名单降级：trae-cli 的 `--allowed-tool` 只做「自动批准该工具」、
> **不裁剪工具集**（实测 `--allowed-tool WebFetch` 仍下发全部 18 个工具），
> 想收窄只能对补集逐个 `--disallowed-tool`（实测 18 → 4）。补集随 CLI 版本
> 漂移，漏一条白名单就失效，故 trae 的白名单模式定义为**默认开启所有工具**
> （全放行 `-y`，与 `on` 同一条路径），不再输出没有收窄能力的 `--allowed-tool`。
> 需要在 trae 上真正关工具请用 `--tools off`（`--disallowed-tool` 真减法）。
> 另注：trae 没有 `WebSearch` 工具，联网只能靠 `WebFetch`。

> 自带工具循环、**没有命令行级逐工具开关**的引擎（openclaw / dsh / arkclaw）：
> `--tools` 整体忽略（工具集由引擎自己的配置决定，dsh 靠 profile bundle /
> `cordis.patch.yml`，openclaw 靠 agent 配置，arkclaw 由网关侧决定），
> 也不会注入 `noToolSuffix` —— 后缀只会和引擎自己的 agent 行为打架。

> ⚠️ `noToolSuffix` 明文写着「严禁使用任何工具」。它**只在 `off` 模式注入** ——
> 若在 `on`/白名单下也注入，就会出现「CLI 侧工具已开、system prompt 却在
> 压制调用」的自相矛盾，表现为**启用了工具却没有网络搜索**。

`noToolSuffix` 里的约束分两层，缺一不可：

1. **严禁调用工具** —— 否则 off 模式下模型会输出 `<tool_calls:xxxx>` 假标签。
2. **严禁伪造工具返回** —— 否则被要求「输出工具抓到的内容」时，模型会凭空
   编造一份看似真实的返回体（实测约 1/6，见下）。仅禁「调用」堵不住编造。

实测（2026-09-15，本机 codebuddy 2.137.1）：

```bash
magic-agent -e codebuddy --tools on -o text "用 WebSearch 查今天的日期"      # ✅ 真调用了搜索
magic-agent -e codebuddy --tools WebSearch -o text "用 WebSearch 查今天日期" # ✅
magic-agent -e codebuddy --tools off -o text "用 WebSearch 查今天的日期"     # 返回 NO_TOOLS（符合预期）
```

**硬证据（非模型自述）**：让 `WebFetch` 抓取 `https://httpbin.org/anything?proof=<随机 nonce>`，
然后解析 CLI 的 `--output-format json` 原始消息数组：

- 出现 `type=="function_call"`（name=`WebFetch`）+ `type=="function_call_result"`（status=`completed`）；
- 该随机 nonce（模型不可能预知）出现在 **`function_call_result.output`** 里。

满足这两条即证明发生了**真实网络请求**，而非模型编造。

**off 模式防伪造效果**（`--tools off`，重复 8 次）：

| | 编造返回体 | 结果 |
|---|---|---|
| 加固前 | 1/6 | 一次输出伪造的 httpbin JSON |
| 加固后 | 0/8 | 7 次 `NO_TOOLS`，1 次明确拒绝 |

## 四档权限模型（`--permission`，仅 claude / codebuddy）

`--tools` 只管「有哪些工具」，「执行时问不问人、有没有沙箱」由 `--permission` 决定。
四档是把主流 agent CLI 的权限机制收敛成的一条轴 —— **哪些动作自动放行 + 放行不了时由谁裁决**：

| 档位 | 语义 | 审批人 | 沙箱 | claude 参数 |
|---|---|---|---|---|
| `manual` | 只读放行，其余逐项确认 | 用户 | 开 | `--permission-mode default` |
| `accept-edits` | 工作区内编辑放行，命令仍逐条确认 | 用户（仅命令） | 开 | `--permission-mode acceptEdits` |
| `auto` | 沙箱内放行，越界交 LLM Guardian 判定 | LLM | 开 | `--permission-mode auto` |
| `full` | 命令直接在宿主机执行，不触发审批 | 无 | 关 | `--permission-mode bypassPermissions` |

别名：`manual` ← `default`/`ask`/`1`；`accept-edits` ← `edits`/`2`；`auto` ← `guardian`/`3`；
`full` ← `bypass`/`yolo`/`4`（大小写与连字符不敏感）。

```bash
magic-agent -e claude --tools on --permission auto -p "重构这个函数"
magic-agent -e claude --tools on --permission manual -p "先看看再动手"
```

## 默认档是 `full`（刻意）

改造前 claude / codebuddy 在 `--tools` 非 `off` 时**恒传** `--dangerously-skip-permissions` / `-y`，
语义正是第 4 档。所以默认值保持 `full` —— 改成别的档位等于一次**静默的行为变更**
（既有调用方的 agent 会突然开始弹审批 / 被沙箱拦）。做 agent 任务时推荐显式传 `--permission auto`。

`--tools off` 下不调用任何工具，档位无处生效；显式传非默认档会打一行 stderr 提示（不静默）。

## 参数是怎么落地的（两个关键约束）

1. **档位走 `--permission-mode`，不走 settings 的 `permissions.defaultMode`。**
   官方明确 `auto` 与 `bypassPermissions` 写在项目级 `.claude/settings.json` 或
   本地级 `.claude/settings.local.json` 里**不生效**（会被忽略，会话回落到 Manual / 内建默认）。
   `--permission-mode` 是唯一在任意作用域都可靠的入口。

2. **沙箱只能经 `--settings` 注入。** claude 没有 `--sandbox` 命令行参数
   （官方 CLI reference 的 flags 表全文无此参数）。而 `--settings` 的语义是
   「**一个**文件路径或一段内联 JSON」，不是可重复 flag —— 所以 MaxTokens 的 env 注入、
   沙箱配置、autoMode、权限规则必须**合并进同一份 JSON**（`agentSettingsPayload`）。
   实测第 3 档 + 全部可选项时子进程收到的是：

   ```json
   {
     "autoMode": { "environment": ["$defaults", "Source control: github.example.com/acme-corp"] },
     "env": { "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "16000" },
     "permissions": { "ask": ["Bash(git push *)"], "deny": ["Bash(rm -rf *)"] },
     "sandbox": {
       "allowUnsandboxedCommands": false, "autoAllowBashIfSandboxed": true,
       "enabled": true, "excludedCommands": ["docker"], "failIfUnavailable": true
     }
   }
   ```

## 可选项

| flag | 作用 | 生效档位 |
|---|---|---|
| `--sandbox-exclude docker,watchman` | 始终在沙箱外执行的命令（`sandbox.excludedCommands`） | manual / accept-edits / auto |
| `--sandbox-domain api.example.com` | 沙箱网络白名单（`sandbox.network.allowedDomains`） | manual / accept-edits / auto |
| `--auto-mode-env "Source control: github.example.com/acme-corp"` | 第 3 档分类器的**受信边界**（自然语言，不是正则；自动带 `$defaults` 保留内建规则） | auto |
| `--permission-ask 'Bash(git push *)'` | 强制人工审批的规则。命中即弹框，**第 3 档下分类器也无法自动放行** | 全部 |
| `--permission-deny 'Bash(rm -rf *)'` | deny 规则。**在所有档位（含 `full`）都先于 allow/ask 求值且不可被白名单覆盖** | 全部 |

第 1~3 档默认写 `failIfUnavailable: true`（沙箱起不来就**报错退出**，不静默降级成不沙箱运行）
与 `allowUnsandboxedCommands: false`（关闭 `dangerouslyDisableSandbox` 逃逸舱口）。
需要放行的命令请用 `--sandbox-exclude` 显式列出，而不是整体放宽。

> `--sandbox-exclude` 这类「沙箱外执行」的口子比想象中重要：`docker`、`watchman`
> 与沙箱不兼容，不排除会让相关命令直接失败。

## 未接线的引擎会明确报错

`--permission` 只对 claude / codebuddy 生效（`--engines` 的 `permission` 字段为
`flag:--permission-mode`）。传给 trae / llm / codex / openclaw / dsh / arkclaw 时**exit 2 明确报错**，
不会静默忽略 —— 静默忽略一个安全设置是最坏的结果：用户以为自己被保护着，实际没有。

```bash
$ magic-agent -e trae --permission auto -p hi
magic-agent: --permission 暂不支持 trae 引擎（当前仅 claude、codebuddy；各引擎能力见 --engines 的 permission 字段）
$ echo $?   # 2
```

（只在实际传了 `--permission` 时才校验，所以 `-e codex` 这类既有调用不受影响。）

## 测试

- `internal/agent/permission_test.go`：档位解析、settings 载荷逐字段断言、
  各档 argv 映射，外加一个**假 CLI 落 argv** 的端到端（证明参数真的进了子进程）。
- `internal/cli/permission_test.go`：`resolvePermissionTier` 的两道校验、
  flag 默认值契约、`--engines` 能力字段、`prepareAsk` → `agent.Request` 的接线。
