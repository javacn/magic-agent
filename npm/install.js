#!/usr/bin/env node
"use strict";

// install.js - postinstall 兜底：
//   1) 当前平台已有预编译二进制 -> 直接通过（正常路径）
//   2) 没有但有 go -> 现场编译（首次 npm install 于非预打包平台时有用）
//   3) 都没有 -> 只告警，不让安装失败（用户仍可自行 go build）

const { execFileSync } = require("child_process");
const fs = require("fs");

const { currentTarget, binaryPath, pkgRoot, findGo } = require("./lib/platform");

const target = currentTarget();
const out = binaryPath(target);

if (fs.existsSync(out)) {
  process.exit(0);
}

const go = findGo();
if (go) {
  const pkg = require(require("path").join(pkgRoot, "package.json"));
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

// postinstall 永不阻断安装。
process.exit(0);
