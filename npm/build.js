#!/usr/bin/env node
"use strict";

// build.js - 把 Go 源码交叉编译成 npm 包内的平台二进制。
//
// 用法：
//   node npm/build.js             # 全部目标（prepack 默认走这条）
//   node npm/build.js --current   # 仅当前平台（本地开发更快）
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

const targets = onlyCurrent ? [currentTarget()] : TARGETS;
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
