#!/usr/bin/env node
"use strict";

// magic-agent 的 npm bin 转发层：
// 按当前平台挑出预编译的 Go 二进制，原样转发 argv / stdio / 退出码。
//
// 之所以用 spawnSync + stdio:inherit 而不是直接 exec：
//   - 保持真实 fd 0/1/2，Go 侧 process.stdin.isTTY 判定不受影响（管道读 prompt 依赖它）
//   - 退出码、Ctrl-C 信号行为与直接运行二进制一致

const { spawnSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const { binaryPath, distDir, currentTarget, exeName } = require("../lib/platform");

const target = currentTarget();
const bin = binaryPath(target);

if (!fs.existsSync(bin)) {
  const found = fs.existsSync(distDir)
    ? fs.readdirSync(distDir).filter((d) => fs.existsSync(path.join(distDir, d, exeName(d.startsWith("win32") ? "windows" : "linux"))))
    : [];
  process.stderr.write(
    `magic-agent: 当前平台 ${target.dir} 没有可用二进制（期望 ${bin}）\n` +
      `已打包的平台：${found.length ? found.join(", ") : "（无）"}\n` +
      `解决办法：在有 Go 的环境执行 \`npm run build\`，或用 \`go build -o bin/magic-agent ./cmd/magic-agent\` 自行编译后直接调用。\n`
  );
  process.exit(1);
}

const res = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });

if (res.error) {
  process.stderr.write(`magic-agent: 启动失败: ${res.error.message}\n`);
  process.exit(1);
}
if (res.signal) {
  // 让父进程以相同信号退出，保持 shell 语义（如超时/中断）
  process.kill(process.pid, res.signal);
  process.exit(1);
}
process.exit(res.status === null ? 1 : res.status);
