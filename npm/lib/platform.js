"use strict";

// platform.js - node 平台标识 -> Go 交叉编译目标 / 产物路径 的统一映射。
//
// 约定：dist/<goos>-<goarch>/magic-agent[.exe]
//   darwin-arm64  darwin-x64  linux-arm64  linux-x64  win32-x64

const path = require("path");

// 构建目标表（顺序即 build.js 的输出顺序）。
const TARGETS = [
  { dir: "darwin-arm64", goos: "darwin", goarch: "arm64" },
  { dir: "darwin-x64", goos: "darwin", goarch: "amd64" },
  { dir: "linux-x64", goos: "linux", goarch: "amd64" },
  { dir: "linux-arm64", goos: "linux", goarch: "arm64" },
  { dir: "win32-x64", goos: "windows", goarch: "amd64" },
];

const GOOS_BY_NODE = { darwin: "darwin", linux: "linux", win32: "windows" };

/** 当前进程对应的 Go 目标 */
function currentTarget() {
  return {
    dir: `${process.platform}-${process.arch}`,
    goos: GOOS_BY_NODE[process.platform],
    goarch: process.arch === "x64" ? "amd64" : process.arch,
  };
}

function exeName(goos) {
  return goos === "windows" ? "magic-agent.exe" : "magic-agent";
}

/** 包根目录（npm/ 的上一级） */
const pkgRoot = path.resolve(__dirname, "..", "..");

/** 二进制输出目录 */
const distDir = path.join(pkgRoot, "npm", "dist");

/** 某个目标产物的绝对路径 */
function binaryPath(target) {
  return path.join(distDir, target.dir, exeName(target.goos));
}

/**
 * 定位可用的 go 可执行文件。
 * 顺序：MAGIC_AGENT_GO_BIN > PATH > homebrew > /usr/local/go > 常见版本目录。
 */
function findGo() {
  const fs = require("fs");
  const candidates = [];
  if (process.env.MAGIC_AGENT_GO_BIN) candidates.push(process.env.MAGIC_AGENT_GO_BIN);
  candidates.push("go");
  candidates.push("/opt/homebrew/bin/go", "/usr/local/go/bin/go", "/usr/local/bin/go");
  for (const c of candidates) {
    try {
      if (c === "go") {
        const { execFileSync } = require("child_process");
        execFileSync("go", ["version"], { stdio: "ignore" });
        return "go";
      }
      if (fs.existsSync(c)) return c;
    } catch (_) {
      /* 继续找下一个 */
    }
  }
  return null;
}

module.exports = {
  TARGETS,
  currentTarget,
  exeName,
  pkgRoot,
  distDir,
  binaryPath,
  findGo,
};
