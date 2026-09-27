#!/usr/bin/env node
"use strict";

// bump.js - 把 package.json 的版本号 +1（改动落地流程里的「升版本号」那一步）。
//
// 为什么需要它（用户 2026-09-23：「安装的时候 magic-agent 也要升级版本号」）：
//   版本号经 ldflags 注入 internal/cli.Version，是「这份 CLI 是哪次构建」的**唯一可读标识**，
//   而**上层会按版本号挑最新的那份** —— magic-test 的 tools/build-mac-app.sh 就是
//   「候选去重后按 package.json 的 version 排序取最高」，再 `ditto` 进应用包。
//   版本号长期停在同一个值时那个判据形同虚设：本机两份全局安装（沙箱 / homebrew）
//   代码不同、版本相同 → 只能按 PATH 顺序取，**取到旧的那份也看不出来**
//   （2026-09-23 实测踩到：应用包里那份是旧构建，界面里少一个引擎、能力字段也不对）。
//
// 用法：
//   node npm/bump.js              # patch（默认）：0.2.0 -> 0.2.1
//   node npm/bump.js minor        # 0.2.1 -> 0.3.0
//   node npm/bump.js major        # 0.3.0 -> 1.0.0
//   node npm/bump.js --dry-run    # 只打印，不写文件
//
// ⚠️ 刻意**不用 `npm version`**：那条命令会顺手 `git commit` + 打 tag，而本流程只要
//    「文件里的版本号 +1」。发版打 tag 是 CI 的事 —— `.github/workflows/release.yml`
//    从 tag 名解析版本号再写回 package.json，两者别混。
// ⚠️ 自增后**必须重新 build + 装两份全局前缀**（README「改动落地四步曲」）：
//    只改 package.json 不重装，CLI 的 `--version` 还是旧的（版本号是编译期注入的）。

const fs = require("fs");
const path = require("path");

const pkgPath = path.join(__dirname, "..", "package.json");

const argv = process.argv.slice(2);
const dryRun = argv.includes("--dry-run");
const level = (argv.find((a) => !a.startsWith("--")) || "patch").toLowerCase();

if (!["patch", "minor", "major"].includes(level)) {
  console.error(`magic-agent: 未知的版本级别 ${JSON.stringify(level)}（可用：patch | minor | major）`);
  process.exit(2);
}

let pkg;
try {
  pkg = JSON.parse(fs.readFileSync(pkgPath, "utf8"));
} catch (e) {
  console.error(`magic-agent: 读不了 ${pkgPath}：${e.message}`);
  process.exit(1);
}

const cur = String(pkg.version || "");
const parts = cur.split(".").map((n) => Number.parseInt(n, 10));
if (parts.length !== 3 || parts.some((n) => !Number.isFinite(n) || n < 0)) {
  console.error(`magic-agent: version ${JSON.stringify(cur)} 不是 x.y.z 形态，拒绝自增（请先手工修好）`);
  process.exit(2);
}

let [maj, min, pat] = parts;
if (level === "major") { maj += 1; min = 0; pat = 0; }
else if (level === "minor") { min += 1; pat = 0; }
else { pat += 1; }
const next = `${maj}.${min}.${pat}`;

if (dryRun) {
  console.log(`magic-agent: ${cur} -> ${next}（--dry-run，未写文件）`);
  process.exit(0);
}

pkg.version = next;
// 与仓库里这份 package.json 的原格式一致：2 空格缩进 + 末尾换行
//（改格式会让每次自增都产生一整篇 diff，review 时看不出真正的改动）。
fs.writeFileSync(pkgPath, JSON.stringify(pkg, null, 2) + "\n");
console.log(`magic-agent: 版本号 ${cur} -> ${next}  (${path.relative(process.cwd(), pkgPath) || pkgPath})`);
console.log("  下一步：npm run build && npm pack --ignore-scripts，再按 README「改动落地四步曲」装两份全局前缀");
