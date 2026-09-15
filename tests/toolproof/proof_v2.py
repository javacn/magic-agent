#!/usr/bin/env python3
"""v2：精确区分「真实工具调用」与模型编造的「伪工具调用标签」。

v1 (verify_ma.py) 的两个检测缺陷：
  1. 伪标签检测对原始 stdout 做 —— Go json.Marshal 把 '<' '>' 转义成
     \\u003c / \\u003e，正则匹配不到 → 假阴性。必须对解析后的 text 检测。
  2. off 模式 nonce 判定是假阳性 —— nonce 写在 prompt 的 URL 里，模型把
     它回显进 <tool_calls> 伪标签，并非真实的网络返回。

本脚本 output 分三类：
  - REAL  : JSON 里存在 type=="function_call" 对象 + function_call_result
            status=completed，且 nonce 出现在 function_call_result.output
  - PSEUDO: 文本正文里出现 <tool_call(s):...> 标签（模型编造）
  - NONE  : 明确回答 NO_TOOLS 或纯文本无标签
"""
import json, os, re, secrets, subprocess, time

MA = "/tmp/ma_t"
CB = "/Applications/WorkBuddy.app/Contents/Resources/app.asar.unpacked/cli/bin/codebuddy"
ENV_MA = dict(os.environ)                       # 真实继承环境（SERVER__PORT 仍在）
ENV_MA["MAGIC_AGENT_CODEBUDDY_BIN"] = CB
ENV_CB = {k: v for k, v in os.environ.items() if not k.startswith("SERVER__")}
ENV_CB["PATH"] = "/opt/homebrew/bin:" + ENV_CB.get("PATH", "/usr/bin:/bin")

NONCE = secrets.token_hex(12)
URL = f"https://httpbin.org/anything?proof={NONCE}"
PROMPT = (f"Use your WebFetch tool to GET this exact URL: {URL}\n"
          "Then output the JSON response body you received. "
          "If you have no WebFetch tool, output exactly NO_TOOLS.")
print(f"nonce = {NONCE}")
print(f"url   = {URL}\n")

PSEUDO_RE = re.compile(r"<tool_calls?:")


def ma_off(i):
    t0 = time.time()
    r = subprocess.run(
        [MA, "-e", "codebuddy", "-m", "hy3", "-o", "json", "-t", "150s",
         "--tools", "off", PROMPT],
        env=ENV_MA, capture_output=True, text=True, timeout=170,
        stdin=subprocess.DEVNULL)
    raw = r.stdout
    txt = raw
    try:
        j = json.loads(raw)
        if isinstance(j, dict):
            txt = j.get("text", raw)
    except Exception:
        pass
    return {
        "secs": round(time.time() - t0, 1),
        "no_tools": "NO_TOOLS" in txt,
        "pseudo": bool(PSEUDO_RE.search(txt)),
        "empty": not txt.strip(),
        "head": txt[:110].replace("\n", "\\n"),
    }


print("=== off 模式 x6：统计「伪工具调用」出现率 ===")
stats = {"none": 0, "pseudo": 0, "empty": 0}
for i in range(6):
    d = ma_off(i)
    if d["empty"]:
        stats["empty"] += 1
    elif d["pseudo"]:
        stats["pseudo"] += 1
    else:
        stats["none"] += 1
    print(f"  #{i+1} {d['secs']:>5}s  NO_TOOLS={d['no_tools']!s:<5} "
          f"伪标签={d['pseudo']!s:<5} 空={d['empty']!s:<5}  head={d['head']!r}")
print(f"  → 汇总: 干净={stats['none']}  伪标签={stats['pseudo']}  空={stats['empty']}\n")

print('=== 直接 CLI 真工具调用证据 (--tools WebFetch,WebSearch -y) ===')
t0 = time.time()
r = subprocess.run(
    [CB, "--print", "--output-format", "json", "--no-session-persistence",
     "--model", "hy3", "--tools", "WebFetch,WebSearch", "-y", PROMPT],
    env=ENV_CB, capture_output=True, text=True, timeout=170,
    stdin=subprocess.DEVNULL)
try:
    arr = json.loads(r.stdout)
except Exception as e:
    print(f"  parse error: {e}; raw head={r.stdout[:200]!r}")
    arr = []
calls = [x for x in arr if isinstance(x, dict) and x.get("type") == "function_call"]
results = [x for x in arr if isinstance(x, dict) and x.get("type") == "function_call_result"]
tool_out = json.dumps([x.get("output") for x in results], ensure_ascii=False)
print(f"  耗时 {time.time()-t0:.1f}s")
print(f"  function_call 名称 : {[c.get('name') for c in calls]}")
print(f"  结果 status        : {[x.get('status') for x in results]}")
print(f"  ★ nonce 在工具真实返回里 : {NONCE in tool_out}")
print(f"  ★ 工具返回片段     : {tool_out[:180]!r}")
