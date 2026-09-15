#!/usr/bin/env python3
"""决定性验证：工具是否真的执行了网络请求。

手法：让 WebFetch 去抓一个"回显任意参数"的 URL，参数用本地生成的随机
nonce。随机 nonce 模型不可能预先知道 —— 若返回内容含该 nonce，则证明
发生了真实网络请求，且不是模型编造。
"""
import json, os, subprocess, secrets, time

CB = "/Applications/WorkBuddy.app/Contents/Resources/app.asar.unpacked/cli/bin/codebuddy"
ENV = {k: v for k, v in os.environ.items() if not k.startswith("SERVER__")}
ENV["PATH"] = "/opt/homebrew/bin:" + ENV.get("PATH", "/usr/bin:/bin")

def _url_of(call):
    a = call.get("arguments")
    if isinstance(a, str):
        try:
            a = json.loads(a)
        except Exception:
            return f"<unparsed:{a[:80]}>"
    if isinstance(a, dict):
        return a.get("url")
    return None


NONCE = secrets.token_hex(12)
URL = f"https://httpbin.org/anything?proof={NONCE}"
print(f"随机 nonce : {NONCE}")
print(f"目标 URL   : {URL}")

# 先 curl 实测，确认服务端确实回显该 nonce
curl = subprocess.run(["curl", "-s", "--max-time", "15", URL],
                      capture_output=True, text=True).stdout
curl_has_nonce = NONCE in curl
print(f"curl 回显 nonce: {curl_has_nonce}\n")


def call(name, tools_extra, timeout=170):
    prompt = (f"Use your WebFetch tool to GET this exact URL: {URL}\n"
              f"Then output the full raw response body verbatim. "
              f"Do not summarize. If you have no WebFetch tool, output NO_TOOLS.")
    args = [CB, "--print", "--output-format", "json", "--no-session-persistence",
            "--model", "hy3"] + tools_extra + [prompt]
    t0 = time.time()
    try:
        r = subprocess.run(args, env=ENV, capture_output=True, text=True,
                           timeout=timeout, stdin=subprocess.DEVNULL)
        d = json.loads(r.stdout)
        res = [x for x in d if isinstance(x, dict) and x.get("type") == "result"]
        calls = [x for x in d if isinstance(x, dict) and x.get("type") == "function_call"]
        results = [x for x in d if isinstance(x, dict) and x.get("type") == "function_call_result"]
        final = res[0].get("result", "") if res else ""
        tool_out = json.dumps([x.get("output") for x in results], ensure_ascii=False)
        return {
            "name": name, "secs": round(time.time() - t0, 1),
            "tool_names": [c.get("name") for c in calls],
            "tool_urls": [_url_of(c) for c in calls],
            "tool_status": [x.get("status") for x in results],
            "nonce_in_tool_output": NONCE in tool_out,
            "nonce_in_final": NONCE in final,
            "final": final[:200],
            "no_tools": "NO_TOOLS" in final,
        }
    except subprocess.TimeoutExpired:
        return {"name": name, "secs": round(time.time() - t0, 1), "error": "TIMEOUT"}
    except Exception as e:
        return {"name": name, "secs": round(time.time() - t0, 1), "error": str(e)[:200]}


cases = [
    ("① --tools default -y （启用）", ["--tools", "default", "-y"]),
    ("② --tools WebFetch -y（白名单）", ["--tools", "WebFetch", "-y"]),
    ("③ --tools \"\"（关闭）", ["--tools", ""]),
]
for name, extra in cases:
    r = call(name, extra)
    print(f"===== {name}   ({r.get('secs')}s)")
    if "error" in r:
        print(f"  ERROR: {r['error']}\n")
        continue
    print(f"  function_call 工具名: {r['tool_names']}")
    print(f"  实际请求的 URL     : {r['tool_urls']}")
    print(f"  工具执行 status    : {r['tool_status']}")
    print(f"  ★ nonce 出现在工具返回里: {r['nonce_in_tool_output']}")
    print(f"  ★ nonce 出现在最终回答里: {r['nonce_in_final']}")
    print(f"  NO_TOOLS?          : {r['no_tools']}")
    print(f"  final: {r['final']!r}\n")
