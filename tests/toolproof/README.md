# toolproof — 工具可用性的硬验证脚本

回答「工具到底能不能用」时，不要靠模型自述，也不要只看最终回答文本。这里用**随机 nonce 闭环**证明是否发生了真实网络请求。

## 手法

让 `WebFetch` 抓 `https://httpbin.org/anything?proof=<本地随机串>`，然后解析 CLI `--output-format json` 的原始消息数组，判两条：

1. 存在 `type=="function_call"`（name=`WebFetch`）+ `type=="function_call_result"`（status=`completed`）
2. 该随机 nonce（模型不可能预知）出现在 **`function_call_result.output`** 里

两条同时满足 ⇒ 真实网络请求，非模型编造。

## 脚本

| 脚本 | 用途 |
|---|---|
| `proof_v2.py` | 主验证：magic-agent 走 on/白名单，确认真工具调用；off 模式统计伪标签 |
| `proof_nonce.py` | 早期版本：直接调 CLI，对比 `--tools default -y` / 白名单 / `""` |
| `proof_off_v2.py` | off 模式 ×N，统计「伪造工具返回」率（验证 noToolSuffix 加固效果） |

## 用法

```bash
# 先构建二进制
cd <repo> && go build -o /tmp/ma_t ./cmd/...

# 主验证（需真实网络）
/opt/homebrew/bin/python3 tests/toolproof/proof_v2.py

# off 模式防伪造（8 次）
/opt/homebrew/bin/python3 tests/toolproof/proof_off_v2.py
```

## 两个必须避开的检测陷阱

1. **UTF-8 转义假阴性**：Go `json.Marshal` 把 `<` `>` 转义成 `\u003c` `\u003e`，用正则在对**原始 stdout** 上找 `<tool_calls:...>` 永远匹配不到 → 必须对**解析后的文本**检测。
2. **nonce 假阳性**：nonce 写在 prompt 的 URL 里，模型会把它**回显**进正文/伪标签 → 判真工具调用**只能看 `function_call_result.output`**，绝不能只看「最终回答里有没有 nonce」。
