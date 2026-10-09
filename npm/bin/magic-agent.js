#!/usr/bin/env node
"use strict";

// magic-agent 的 npm bin 转发层：
// 按当前平台挑出预编译的 Go 二进制，原样转发 argv / stdio / 退出码。
//
// 之所以用 spawnSync + stdio:inherit 而不是直接 exec：
//   - 保持真实 fd 0/1/2，Go 侧 process.stdin.isTTY 判定不受影响（管道读 prompt 依赖它）
//   - 退出码、Ctrl-C 信号行为与直接运行二进制一致
//
// 定位顺序（逐级兜底，尽量不让用户卡在「装上了但跑不了」）：
//   1) MAGIC_AGENT_BIN            环境变量显式指定
//   2) npm/dist/<plat>/          预编译产物（npm 包正常路径）
//   3) bin/                      本地开发产物（go build -o bin/...）
//   4) 本机 go 现场编译            与 npm/install.js 的兜底策略保持一致
// 全都落空才报错退出，错误信息里带上「已打包了哪些平台」便于自查。

const { execFileSync, spawnSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const { binaryPath, distDir, currentTarget, exeName, findGo, pkgRoot } = require("../lib/platform");

const target = currentTarget();
const exe = exeName(target.goos);
const isWin = process.platform === "win32";

function fromDist() {
  const p = binaryPath(target);
  return fs.existsSync(p) ? p : null;
}

function fromLocalBin() {
  const p = path.join(pkgRoot, "bin", exe);
  return fs.existsSync(p) ? p : null;
}

/** 最后兜底：本机装了 Go 就现场编译一份。 */
function buildFromSource() {
  const go = findGo();
  if (!go) return null;
  const out = fromLocalBin() || path.join(pkgRoot, "bin", exe);
  process.stderr.write(`magic-agent: 未找到 ${target.dir} 预编译产物，用 go 现场编译 ...\n`);
  try {
    // 版本号从 package.json 注入，与 npm/build.js 保持一致 ——
    // 否则兜底编译出来的产物会报 0.1.0（Go 侧默认值），和正式包对不上。
    let version = "";
    try {
      version = require(path.join(pkgRoot, "package.json")).version || "";
    } catch (_) {
      /* 读不到就不注入，Go 侧会用默认值 */
    }
    const ldflags = version
      ? `-s -w -X github.com/darren/magic-agent/internal/cli.Version=${version}`
      : "-s -w";

    fs.mkdirSync(path.dirname(out), { recursive: true });
    execFileSync(go, ["build", "-trimpath", "-ldflags", ldflags, "-o", out, "./cmd/magic-agent"], {
      cwd: pkgRoot,
      stdio: ["ignore", "ignore", "pipe"],
      env: { ...process.env, CGO_ENABLED: "0", GOOS: target.goos, GOARCH: target.goarch },
    });
    return fs.existsSync(out) ? out : null;
  } catch (err) {
    process.stderr.write(`magic-agent: 现场编译失败: ${String(err.message).split("\n")[0]}\n`);
    return null;
  }
}

function resolve() {
  if (process.env.MAGIC_AGENT_BIN && fs.existsSync(process.env.MAGIC_AGENT_BIN)) {
    return process.env.MAGIC_AGENT_BIN;
  }
  return fromDist() || fromLocalBin() || buildFromSource();
}

const bin = resolve();

if (!bin) {
  // 列出包里实际带了哪些平台，方便判断是「平台没打」还是「包不完整」。
  const packed = fs.existsSync(distDir)
    ? fs
        .readdirSync(distDir)
        .filter((d) => fs.existsSync(path.join(distDir, d, exeName(d.startsWith("win32") ? "windows" : "linux"))))
    : [];
  process.stderr.write(
    `magic-agent: 当前平台 ${target.dir} 没有可用二进制（期望 ${binaryPath(target)}）\n` +
      `已打包的平台：${packed.length ? packed.join(", ") : "（无）"}\n` +
      `解决办法：装 Go 后重跑本命令（会自动现场编译），或手工执行\n` +
      `  npm run build\n` +
      `  go build -o bin/${exe} ./cmd/magic-agent\n` +
      `或用 MAGIC_AGENT_BIN 指向已有的可执行文件。\n`
  );
  process.exit(1);
}

const res = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });

if (res.error) {
  process.stderr.write(`magic-agent: 启动失败: ${res.error.message}\n`);
  process.exit(1);
}
if (res.signal) {
  // 让父进程以相同信号退出，保持 shell 语义（如超时/中断）。
  // Windows 上没有真正的信号投递，process.kill 自杀会丢掉退出码语义，
  // 这种情况退回 1（通用失败），不要假装成功。
  if (isWin) process.exit(1);
  process.kill(process.pid, res.signal);
  process.exit(1);
}
process.exit(res.status === null ? 1 : res.status);
