#!/usr/bin/env node
"use strict";

// install.js - postinstall 两件事：
//
// 1) magic-agent 二进制兜底：
//    1a) 当前平台已有预编译二进制 -> 直接通过（正常路径）
//    1b) 没有但有 go -> 现场编译（首次 npm install 于非预打包平台时有用）
//    1c) 都没有 -> 只告警，不让安装失败（用户仍可自行 go build）
//
// 2) llm 引擎依赖：安装 simonw/LLM CLI（https://github.com/simonw/LLM）
//    探测顺序（找到就跳过安装）：
//      MAGIC_AGENT_LLM_BIN > ~/.llm-venv/bin/llm > PATH(llm) > brew
//    都没有时建 ~/.llm-venv（pipx 风格隔离，不污染系统 Python）：
//      python3 -m venv ~/.llm-venv && ~/.llm-venv/bin/pip install llm
//    安装失败只告警（-e llm 不可用，其他引擎不受影响）。

const { execFileSync, execSync } = require("child_process");
const fs = require("fs");
const os = require("os");
const path = require("path");

const { currentTarget, binaryPath, pkgRoot, findGo } = require("./lib/platform");

// ── 1) magic-agent 二进制 ──────────────────────────────────────

const target = currentTarget();
const out = binaryPath(target);

if (!fs.existsSync(out)) {
  const go = findGo();
  if (go) {
    const pkg = require(path.join(pkgRoot, "package.json"));
    process.stdout.write(`magic-agent: 当前平台 ${target.dir} 无预编译产物，尝试用 go 现场编译 ... `);
    try {
      execFileSync(
        go,
        ["build", "-trimpath", "-ldflags", `-s -w -X github.com/darren/magic-agent/internal/cli.Version=${pkg.version}`, "-o", out, "./cmd/magic-agent"],
        {
          cwd: pkgRoot,
          stdio: ["ignore", "ignore", "pipe"],
          env: { ...process.env, CGO_ENABLED: "0", GOOS: target.goos, GOARCH: target.goarch },
        }
      );
      fs.chmodSync(out, 0o755);
      console.log("ok");
    } catch (err) {
      console.log(`失败: ${err.message}`);
    }
  } else {
    process.stderr.write(
      `magic-agent: 警告 - 当前平台 ${target.dir} 缺少预编译二进制，且本机未找到 go，命令可能无法运行。\n`
    );
  }
}

// ── 2) llm CLI（simonw/LLM）────────────────────────────────────

function haveLlm() {
  if (process.env.MAGIC_AGENT_LLM_BIN) return process.env.MAGIC_AGENT_LLM_BIN;
  const venv = path.join(os.homedir(), ".llm-venv", "bin", "llm");
  if (fs.existsSync(venv)) return venv;
  try {
    return execFileSync("llm", ["--version"], { stdio: ["ignore", "pipe", "ignore"] }).toString().trim() && "llm";
  } catch (_) {
    /* PATH 里没有 */
  }
  return null;
}

function installLlm() {
  const home = os.homedir();
  const venvBin = path.join(home, ".llm-venv", "bin");

  // 找一个能用的 python3（python3 > python；都不行时放弃）。
  let py = null;
  for (const c of ["python3", "python"]) {
    try {
      execFileSync(c, ["--version"], { stdio: ["ignore", "pipe", "ignore"] });
      py = c;
      break;
    } catch (_) {
      /* 下一个 */
    }
  }
  if (!py) return "未找到可用的 python3";

  try {
    process.stdout.write("magic-agent: 安装 llm CLI (simonw/LLM) 到 ~/.llm-venv ... ");
    const env = { ...process.env };
    // Homebrew Python pip 可能因 PIP_BREAK_SYSTEM_PACKAGES / truststore 报错，
    // venv 内不需要这些；静音升级提示。
    delete env.PIP_BREAK_SYSTEM_PACKAGES;
    execFileSync(py, ["-m", "venv", path.join(home, ".llm-venv")], { stdio: ["ignore", "ignore", "pipe"] });
    execFileSync(
      path.join(venvBin, "pip"),
      ["install", "--quiet", "--disable-pip-version-check", "llm"],
      { stdio: ["ignore", "ignore", "pipe"], env }
    );
    console.log("ok");
    return null;
  } catch (err) {
    console.log(`失败: ${err.message.split("\n")[0]}`);
    return err.message.split("\n")[0];
  }
}

if (!haveLlm()) {
  const why = installLlm();
  if (why) {
    process.stderr.write(
      `magic-agent: 警告 - llm CLI 安装失败（${why}）。-e llm 引擎不可用；claude/codebuddy/trae 不受影响。\n` +
        `手动安装：pip install llm  或  pipx install llm，然后用 MAGIC_AGENT_LLM_BIN 指向可执行文件。\n`
    );
  }
} else {
  // 已有 llm：静默通过（安装输出越少越好）。
}

// postinstall 永不阻断安装。
process.exit(0);
