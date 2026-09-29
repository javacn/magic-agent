#!/usr/bin/env node
"use strict";

// build.js - 把 Go 源码交叉编译成 npm 包内的平台二进制。
//
// 用法：
//   node npm/build.js                        # 全部目标（prepack 默认走这条）
//   node npm/build.js --current              # 仅当前平台（本地开发更快）
//   node npm/build.js --target=linux-arm64   # 仅指定平台（CI 矩阵用：与 runner 架构解耦）
//
// 版本号从 package.json 注入到 internal/cli.Version。

const { execFileSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const { TARGETS, currentTarget, pkgRoot, binaryPath, findGo } = require("./lib/platform");

const pkg = require(path.join(pkgRoot, "package.json"));
const onlyCurrent = process.argv.includes("--current");

const go = findGo();
if (!go) {
  console.error("magic-agent: 找不到 go 可执行文件，无法编译。可设置 MAGIC_AGENT_GO_BIN 指定路径。");
  process.exit(1);
}

// --target=<dir>：显式指定目标。CI 里每个矩阵 job 只编译自己那一个平台，
// 用的是 runner 上的 go 交叉编译（CGO_ENABLED=0，纯 Go 无需 QEMU），
// 所以「目标平台」与「runner 自身架构」无关 —— 不能拿 --current 充当。
const targetArg = (process.argv.find((a) => a.startsWith("--target=")) || "").slice("--target=".length);
let targets;
if (targetArg) {
  const t = TARGETS.find((x) => x.dir === targetArg);
  if (!t) {
    console.error(
      `magic-agent: 未知目标 ${JSON.stringify(targetArg)}（可用：${TARGETS.map((x) => x.dir).join(" | ")}）`
    );
    process.exit(2);
  }
  targets = [t];
} else {
  targets = onlyCurrent ? [currentTarget()] : TARGETS;
}
const ldflags = `-s -w -X github.com/darren/magic-agent/internal/cli.Version=${pkg.version}`;

console.log(`magic-agent: 使用 ${go} 编译 v${pkg.version}，共 ${targets.length} 个目标`);
let ok = 0;

for (const t of targets) {
  const out = binaryPath(t);
  fs.mkdirSync(path.dirname(out), { recursive: true });
  process.stdout.write(`  ${t.dir.padEnd(14)} ... `);
  try {
    execFileSync(
      go,
      ["build", "-trimpath", "-ldflags", ldflags, "-o", out, "./cmd/magic-agent"],
      {
        cwd: pkgRoot,
        stdio: ["ignore", "ignore", "pipe"],
        env: { ...process.env, CGO_ENABLED: "0", GOOS: t.goos, GOARCH: t.goarch },
      }
    );
    if (t.goos !== "windows") fs.chmodSync(out, 0o755);
    const kb = Math.round(fs.statSync(out).size / 1024);
    console.log(`ok (${kb} KB)`);
    ok++;
  } catch (err) {
    const msg = (err.stderr || Buffer.from("")).toString().trim().split("\n").slice(-3).join(" | ");
    console.log(`失败: ${err.message}${msg ? " -- " + msg : ""}`);
  }
}

console.log(`magic-agent: 完成 ${ok}/${targets.length} 个目标 -> npm/dist/`);
if (ok === 0) process.exit(1);
