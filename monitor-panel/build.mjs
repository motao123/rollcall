#!/usr/bin/env node
// 把单文件 index.html 拆成 Rollcall 的三份静态资源（dist/monitor.{html,css,js}）。
//
// 为什么要拆：Rollcall 的 CSP 是 `script-src 'self'`，内联 <script> 会被静默拦截
// （页面停在骨架屏且不报错）。所以生产版本必须是外链脚本。
// 单文件 index.html 保留为唯一源：改功能只改它，然后跑本脚本生成产物。
//
// 用法：node monitor-panel/build.mjs [输出目录]
//   默认输出到 ../internal/web/dist/

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const outDir = process.argv[2] || path.join(here, '..', 'internal', 'web', 'dist');
const srcPath = path.join(here, 'index.html');

const src = fs.readFileSync(srcPath, 'utf8');

function pick(re) {
  const m = src.match(re);
  if (!m) throw new Error('未找到段落: ' + re);
  return m[1].trim();
}

const style = pick(/<style>([\s\S]*?)<\/style>/);
const script = pick(/<script>([\s\S]*?)<\/script>/);

// 首屏不闪白：把暗色变量块再复制一份给「系统暗色且用户未显式选择」的场景。
// CSP 禁止内联脚本，无法在 CSS 生效前设置 data-theme，只能靠媒体查询。
const darkBlock = style.match(/html\[data-theme="dark"\] \{([\s\S]*?)\n\}/);
if (!darkBlock) throw new Error('未找到暗色变量块');
const mediaCss =
  '/* 系统暗色且用户尚未显式选择时直接给暗色 —— 首屏不闪白。\n' +
  '   内联脚本被 CSP 禁止，无法在 CSS 生效前改 data-theme，只能靠媒体查询。 */\n' +
  '@media (prefers-color-scheme: dark) {\n' +
  '  :root:not([data-theme="light"]) {' + darkBlock[1] + '\n  }\n}\n\n';
const cssOut = mediaCss + style;

// HTML：内联段落换成外链（用相对路径，这样 file:// 直接打开也能预览）
let htmlOut = src
  .replace(/<style>[\s\S]*?<\/style>\n?/, '<link rel="stylesheet" href="monitor.css">\n')
  .replace(/<script>[\s\S]*?<\/script>/, '<script src="monitor.js" defer></script>');

if (/<style>|<script>/.test(htmlOut)) throw new Error('HTML 中仍有内联 style/script（CSP 会拦截）');

function write(name, text) {
  const p = path.join(outDir, name);
  // 必须是 LF：go:embed 嵌原始字节，CRLF 会让构建 digest 与 CI 不一致
  const body = text.endsWith('\n') ? text : text + '\n';
  fs.writeFileSync(p, body.replace(/\r\n/g, '\n'), 'utf8');
  const buf = fs.readFileSync(p);
  if (buf.includes(Buffer.from([0xef, 0xbf, 0xbd]))) throw new Error(name + ' 含 U+FFFD');
  if (buf.includes(Buffer.from('\r\n'))) throw new Error(name + ' 含 CRLF');
  console.log(`  ${name}  ${buf.length} bytes`);
}

fs.mkdirSync(outDir, { recursive: true });
console.log('生成到 ' + outDir);
write('monitor.html', htmlOut);
write('monitor.css', cssOut);
write('monitor.js', script);
console.log('完成');
