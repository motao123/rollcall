#!/usr/bin/env node
/**
 * check-contrast.mjs — 前端配色对比度自检（WCAG 2.1）
 *
 * 为什么需要：样式是手写 CSS，深浅双主题 + 语义色（且「使用率/可用率」配色
 * 方向相反），靠肉眼审查漏过过真实事故（v1.35.1 公开页进度条透明）。
 *
 * 做法：解析 internal/web/dist/style.css 里 :root 与 html[data-theme="dark"]
 * 两套变量，按「文字色 × 背景色」的实际组合断言对比度阈值。
 *
 * 退出码：0 = 通过（允许 warn），1 = 存在 fail 级不达标组合。
 *
 * 用法：node tools/check-contrast.mjs [cssPath]
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const DEFAULT_CSS = path.join(HERE, '..', 'internal', 'web', 'dist', 'style.css');

/* ---------------- WCAG 对比度 ---------------- */

const srgb = (c) => {
  const v = c / 255;
  return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
};

const parseHex = (hex) => {
  const h = hex.replace('#', '').trim();
  const full = h.length === 3 ? h.split('').map((c) => c + c).join('') : h;
  if (!/^[0-9a-fA-F]{6}$/.test(full)) return null;
  return [
    parseInt(full.slice(0, 2), 16),
    parseInt(full.slice(2, 4), 16),
    parseInt(full.slice(4, 6), 16),
  ];
};

const luminance = (rgb) =>
  0.2126 * srgb(rgb[0]) + 0.7152 * srgb(rgb[1]) + 0.0722 * srgb(rgb[2]);

export const contrast = (a, b) => {
  const la = luminance(a);
  const lb = luminance(b);
  const [hi, lo] = la > lb ? [la, lb] : [lb, la];
  return (hi + 0.05) / (lo + 0.05);
};

/* ---------------- 变量抽取 ---------------- */

/** 从一段 CSS 文本里抽出 --name: #hex 形式的变量 */
function varsIn(blockText) {
  const out = {};
  const re = /--([a-z0-9-]+)\s*:\s*(#[0-9a-fA-F]{3,8})\s*;/g;
  let m;
  while ((m = re.exec(blockText))) {
    const rgb = parseHex(m[2]);
    if (rgb) out[m[1]] = { hex: m[2], rgb };
  }
  return out;
}

/** 截取以 selector 开头的第一个规则块的内部文本 */
function ruleBody(css, selector) {
  const idx = css.indexOf(selector);
  if (idx < 0) return '';
  const open = css.indexOf('{', idx);
  if (open < 0) return '';
  let depth = 0;
  for (let i = open; i < css.length; i++) {
    if (css[i] === '{') depth++;
    else if (css[i] === '}') {
      depth--;
      if (depth === 0) return css.slice(open + 1, i);
    }
  }
  return '';
}

/* ---------------- 断言组合 ---------------- */

// fail：正文与次要正文；语义色作为文字；这些是阅读必需
const FAIL_RULES = [
  ['fg', 'bg', 4.5, '正文 / 页面底色'],
  ['fg', 'panel', 4.5, '正文 / 卡片'],
  ['fg', 'panel-2', 4.5, '正文 / 次级面板'],
  ['fg-2', 'bg', 4.5, '次要正文 / 页面底色'],
  ['fg-2', 'panel', 4.5, '次要正文 / 卡片'],
  ['fg-2', 'panel-2', 4.5, '次要正文 / 次级面板'],
  ['ok', 'panel', 4.5, '成功色文字 / 卡片'],
  ['warn', 'panel', 4.5, '警告色文字 / 卡片'],
  ['crit', 'panel', 4.5, '危险色文字 / 卡片'],
];

// warn：辅助信息（占位符、表头、图注），AA 对小字不强制 4.5；低于 3 才提示
const WARN_RULES = [
  ['fg-3', 'bg', 3, '辅助文字 / 页面底色'],
  ['fg-3', 'panel', 3, '辅助文字 / 卡片'],
  ['accent', 'panel', 3, '强调色文字（链接）/ 卡片'],
  ['accent', 'bg', 3, '强调色文字（链接）/ 页面底色'],
];

function run(cssPath) {
  const css = fs.readFileSync(cssPath, 'utf8');
  const themes = [
    { name: 'light', vars: varsIn(ruleBody(css, ':root')) },
    { name: 'dark', vars: varsIn(ruleBody(css, 'html[data-theme="dark"]')) },
  ];

  let fails = 0;
  let warns = 0;

  for (const t of themes) {
    if (Object.keys(t.vars).length === 0) {
      console.error(`✗ 主题 ${t.name}: 未解析到任何颜色变量（选择器或变量格式变了？）`);
      fails++;
      continue;
    }
    const rows = [];
    const check = (rules, level) => {
      for (const [fgKey, bgKey, min, label] of rules) {
        const fg = t.vars[fgKey];
        const bg = t.vars[bgKey];
        if (!fg || !bg) {
          rows.push({ level: 'skip', label, ratio: '-', note: `缺变量 --${fgKey} 或 --${bgKey}` });
          continue;
        }
        const ratio = contrast(fg.rgb, bg.rgb);
        const bad = ratio < min;
        if (bad) level === 'fail' ? fails++ : warns++;
        rows.push({
          level: bad ? level : 'pass',
          label,
          ratio: ratio.toFixed(2),
          note: bad ? `${fg.hex} on ${bg.hex}（需 ≥${min}）` : `${fg.hex} on ${bg.hex}`,
        });
      }
    };
    check(FAIL_RULES, 'fail');
    check(WARN_RULES, 'warn');

    console.log(`\n=== ${t.name} ===`);
    for (const r of rows) {
      const mark = r.level === 'pass' ? '✓' : r.level === 'fail' ? '✗' : r.level === 'warn' ? '!' : '-';
      console.log(`${mark} ${r.ratio.padStart(6)}  ${r.label.padEnd(26)} ${r.note}`);
    }
  }

  console.log(`\n结果：${fails === 0 ? '通过' : '未通过'}（fail=${fails} warn=${warns}）`);
  if (fails > 0) {
    console.error('\n存在低于 AA 阈值的正文/语义色组合，请调整 style.css 变量或阈值。');
    return 1;
  }
  return 0;
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const cssPath = process.argv[2] || DEFAULT_CSS;
  if (!fs.existsSync(cssPath)) {
    console.error(`找不到样式文件：${cssPath}`);
    process.exit(1);
  }
  process.exit(run(cssPath));
}
