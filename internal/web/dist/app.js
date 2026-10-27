/* ============================================================
   Rollcall 前端（零依赖，纯原生 JS）
   - 无框架、无图表库：曲线用内联 SVG 自绘
   - 无外部请求：全部同源 /api
   ============================================================ */
'use strict';

const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => Array.from(r.querySelectorAll(s));

const state = {
  route: 'overview',
  param: null,
  range: '1h',
  ovRange: '24h',
  ovTopMetric: 'cpu',
  search: '',
  alertPage: 1,
  auditPage: 1,
  auditFilters: { range: '7d', action: '', q: '' },
  nodeFilter: { provider: '', group: '', state: '' },
  alertPage: 1,
  data: {},
  panelLink: '',
  timezone: '',
  timezoneOffsetMin: null,
  authRequired: false,
  authenticated: false,
  user: null,
  rendering: false,
  renderPending: false,
  livePending: false,
  liveNodeSig: null,
  detailSig: '',
  detailCache: null,
  timers: [],
};

const VALID_RANGES = ['1h', '6h', '24h', '7d', '30d', '365d'];
// 概览「正在告警」分页：机群规模大时异常节点可能远超一屏。
const ALERT_PAGE_SIZE = 10;

/* ---------------- 工具 ---------------- */

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

async function copyText(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch (e) { /* 回退到 execCommand */ }
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
  ta.remove();
  return ok;
}

function pct(used, total) {
  if (!total) return 0;
  return (used / total) * 100;
}

function bytes(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  const u = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return n.toFixed(n >= 100 ? 0 : n >= 10 ? 1 : 2) + ' ' + u[i];
}

function fixed(v, n = 0) {
  return (Number(v) || 0).toFixed(n);
}

/** 坐标轴用的紧凑速率格式：293K / 1.2M / 45B */
function rate(v) {
  v = Number(v) || 0;
  if (v >= 1048576) return (v / 1048576).toFixed(1) + 'M';
  if (v >= 1024) return Math.round(v / 1024) + 'K';
  return Math.round(v) + 'B';
}

// 流量口径文案。服务商计费方式不同：收发合计 / 只算下行 / 只算上行 / 取较大者。
// 服务端返回的 mode 已归一化，这里只需映射成中文。
const TRAFFIC_MODES = [
  { v: 'sum', t: '收发合计（默认）' },
  { v: 'rx', t: '只计下行' },
  { v: 'tx', t: '只计上行' },
  { v: 'max', t: '取收发较大者' },
];
function trafficModeLabel(mode) {
  const m = TRAFFIC_MODES.find((x) => x.v === (mode || 'sum'));
  return m ? m.t : '收发合计';
}

function dur(sec) {  sec = Number(sec) || 0;
  if (sec < 60) return sec + ' 秒';
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d > 0) return d + ' 天' + (h > 0 ? ' ' + h + ' 小时' : '');
  if (h > 0) return h + ' 小时' + (m > 0 ? ' ' + m + ' 分' : '');
  return m + ' 分钟';
}

function ago(ts) {
  if (!ts) return '从未';
  const s = Math.floor(Date.now() / 1000) - ts;
  if (s < 60) return s + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}

// 节点状态：后端 state 与 online 同时下发；旧数据缺少 state 时退回 online。
function nodeState(n) {
  const s = n.state || (n.online ? 'online' : 'offline');
  const labels = {
    online: { text: '在线', tone: 'ok' },
    stale: { text: '数据延迟', tone: 'warn' },
    offline: { text: '离线', tone: 'off' },
    no_data: { text: '待接入', tone: 'off' },
  };
  const base = labels[s] || labels.offline;
  if (s === 'online' && n.alerts > 0) return { text: `${n.alerts} 告警`, tone: 'crit' };
  return base;
}

function hasNodeData(n) {
  return (n.state || (n.online ? 'online' : 'offline')) !== 'no_data';
}

// ISO 3166-1 alpha-2 → 地区旗帜 emoji（区域指示符拼字）。
// country 未登记时返回空串，调用方无需再判空。
function flagEmoji(code) {
  if (!code || !/^[A-Za-z]{2}$/.test(code)) return '';
  const cc = code.toUpperCase();
  return String.fromCodePoint(...[...cc].map((c) => 0x1f1e6 + c.charCodeAt(0) - 65));
}

// OS 家族短标签：卡片/表格里代替完整发行版字符串（悬停可见全名）。
// 顺序即优先级：先匹配具体发行版，兜底取首词。
function osBadge(os) {
  const s = (os || '').toLowerCase();
  if (!s) return '';
  const table = [
    ['windows', 'Windows'], ['darwin|mac os|macos', 'macOS'],
    ['debian', 'Debian'], ['ubuntu', 'Ubuntu'], ['centos', 'CentOS'],
    ['rocky', 'Rocky'], ['almalinux|alma ', 'Alma'], ['fedora', 'Fedora'],
    ['alpine', 'Alpine'], ['arch', 'Arch'], ['opensuse|suse', 'openSUSE'],
    ['oracle', 'Oracle'], ['deepin', 'Deepin'], ['openwrt', 'OpenWrt'],
    ['freebsd', 'FreeBSD'], ['nixos', 'NixOS'],
  ];
  for (const [re, label] of table) {
    if (new RegExp(re).test(s)) return label;
  }
  return esc((os || '').split(/[ /]/)[0].slice(0, 10));
}

function clock(ts) {
  const d = new Date(ts * 1000);
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// 顶栏同步时间：每次成功拉取（含静默轮询）都刷新，
// 让"页面是否还在自动更新"可见，而不是只能靠观察数字变化。
function markSynced() {
  const now = new Date();
  const p = (n) => String(n).padStart(2, '0');
  const hms = `${p(now.getHours())}:${p(now.getMinutes())}:${p(now.getSeconds())}`;
  const text = $('#liveText');
  const badge = $('#liveBadge');
  const time = $('#liveTime');
  if (text) text.textContent = '实时';
  if (badge) {
    badge.classList.remove('live-off');
    badge.title = '上次同步 ' + hms;
  }
  if (time) time.textContent = ' · ' + hms;
}

function markOffline() {
  const text = $('#liveText');
  const badge = $('#liveBadge');
  const time = $('#liveTime');
  if (text) text.textContent = '离线';
  if (badge) badge.classList.add('live-off');
  if (time) time.textContent = '';
}

// 本地日期（YYYY-MM-DD）。toISOString 是 UTC，东八区凌晨会落到前一天。
function localDate(d = new Date()) {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

function tone(v, warnAt, critAt) {
  if (v >= critAt) return 'crit';
  if (v >= warnAt) return 'warn';
  return 'ok';
}

// 顶栏搜索是全局的：各列表页只需要声明参与匹配的字段。
function matchesSearch(parts) {
  if (!state.search) return true;
  return parts.some((v) => String(v ?? '').toLowerCase().includes(state.search));
}

function sectionActions(meta, buttons = '') {
  return `<div class="section-actions">
    ${meta ? `<span class="section-meta">${meta}</span>` : ''}
    ${buttons}
  </div>`;
}

/* ---------------- 请求 ---------------- */

async function api(path) {
  // 没有超时的 fetch 会永久挂起：轮询回调 await 住之后再也不返回，
  // 表现为"页面卡在旧数据上，手动刷新才恢复"。
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), 12000);
  let r;
  try {
    r = await fetch(path, {
      headers: { Accept: 'application/json' },
      credentials: 'same-origin',
      cache: 'no-store',
      signal: ctl.signal,
    });
  } finally {
    clearTimeout(timer);
  }
  if (r.status === 401) {
    authLost();
    const e = new Error('登录状态已失效');
    e.status = 401;
    throw e;
  }
  if (!r.ok) throw new Error(r.status + ' ' + r.statusText);
  return r.json();
}

// 会话失效统一入口：停掉轮询并弹出登录页。authRequired 同时是
// "登录页已显示" 的标记，重复触发不再重绘，避免抹掉用户正在输入的内容。
function authLost() {
  state.authenticated = false;
  state.user = null;
  updateUserMenu();
  if (state.authRequired) return;
  state.authRequired = true;
  setTimeout(() => { if (state.authRequired) showLoginGate(); }, 0);
}

/* ---------------- SVG 绘图 ---------------- */

function scale(vals, h, pad = 2) {
  let min = Math.min(...vals), max = Math.max(...vals);
  if (!isFinite(min) || !isFinite(max)) { min = 0; max = 1; }
  if (max - min < 0.001) { max = min + 1; }
  return (v) => h - pad - ((v - min) / (max - min)) * (h - pad * 2);
}

function pathFrom(values, w, h, pad = 2) {
  if (!values.length) return '';
  const y = scale(values, h, pad);
  const step = values.length > 1 ? w / (values.length - 1) : w;
  return values.map((v, i) => `${i ? 'L' : 'M'}${(i * step).toFixed(1)},${y(v).toFixed(1)}`).join(' ');
}

/** 迷你曲线：带渐变填充 */
function sparkline(values, color, h = 34) {
  if (!values || values.length < 2) {
    return `<svg viewBox="0 0 100 ${h}" preserveAspectRatio="none"><line x1="0" y1="${h / 2}" x2="100" y2="${h / 2}" stroke="var(--line-2)" stroke-width="1" stroke-dasharray="2 3"/></svg>`;
  }
  const w = 100;
  const d = pathFrom(values, w, h, 3);
  const area = `${d} L${w},${h} L0,${h} Z`;
  const id = 'g' + Math.random().toString(36).slice(2, 8);
  return `<svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">
    <defs><linearGradient id="${id}" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0%" stop-color="${color}" stop-opacity="0.24"/>
      <stop offset="100%" stop-color="${color}" stop-opacity="0"/>
    </linearGradient></defs>
    <path d="${area}" fill="url(#${id})"/>
    <path d="${d}" fill="none" stroke="${color}" stroke-width="1.6" stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/>
  </svg>`;
}

/* ---------------- 图表交互 ----------------
   曲线是 innerHTML 渲染出来的，事件不能内联（CSP 禁止）。
   图表数据存入 registry，由绑定在 #view 上的委托监听统一处理悬停。 */
const chartRegistry = new Map();
let chartSeq = 0;
let activeChart = null;

function chartAxisTime(ts, spanSec) {
  const d = new Date(ts * 1000);
  const p = (n) => String(n).padStart(2, '0');
  if (spanSec > 180 * 86400) return `${d.getFullYear()}-${p(d.getMonth() + 1)}`;
  if (spanSec > 2 * 86400) return `${p(d.getMonth() + 1)}-${p(d.getDate())}`;
  return `${p(d.getHours())}:${p(d.getMinutes())}`;
}

function chartTipTime(ts) {
  const d = new Date(ts * 1000);
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function hideChartTips() {
  $$('.chart-tip').forEach((t) => { t.hidden = true; });
  if (activeChart) {
    activeChart.querySelector('.chart-cross')?.setAttribute('opacity', '0');
    activeChart = null;
  }
}

function bindChartHover() {
  const view = $('#view');
  if (!view || view.dataset.chartBound) return;
  view.dataset.chartBound = '1';
  view.addEventListener('pointermove', (e) => {
    const wrap = e.target.closest ? e.target.closest('.chart-wrap') : null;
    if (!wrap) { hideChartTips(); return; }
    const c = chartRegistry.get(wrap.dataset.chartId);
    if (!c || !c.pts.length) return;
    const box = wrap.getBoundingClientRect();
    const svg = wrap.querySelector('svg');
    if (!svg) return;
    // viewBox 的绘图区换算到屏幕坐标后再取最近点，
    // 避免 preserveAspectRatio=none 时直接用 offsetX 造成偏移。
    const left = box.left + box.width * (c.padL / c.w);
    const right = box.left + box.width * ((c.padL + c.iw) / c.w);
    const x = Math.max(left, Math.min(right, e.clientX));
    const ratio = (x - left) / Math.max(1, right - left);
    const idx = Math.round(ratio * (c.pts.length - 1));
    const pt = c.pts[idx];
    if (activeChart && activeChart !== wrap) {
      activeChart.querySelector('.chart-cross')?.setAttribute('opacity', '0');
    }
    activeChart = wrap;
    const cross = wrap.querySelector('.chart-cross');
    if (cross) {
      cross.setAttribute('opacity', '1');
      cross.querySelector('line').setAttribute('x1', pt.x);
      cross.querySelector('line').setAttribute('x2', pt.x);
      cross.querySelector('circle').setAttribute('cx', pt.x);
      cross.querySelector('circle').setAttribute('cy', pt.y);
    }
    const tip = wrap.querySelector('.chart-tip');
    if (tip) {
      const time = c.times ? chartTipTime(c.times[idx]) : '';
      const value = c.fmt(pt.v);
      tip.innerHTML = `${time ? `<span class="tip-time">${esc(time)}</span>` : ''}<span class="tip-val mono">${esc(c.label ? c.label + ' ' : '')}${esc(value)}</span>`;
      tip.hidden = false;
      const px = Math.max(70, Math.min(box.width - 70, e.clientX - box.left));
      tip.style.left = px + 'px';
    }
  });
  view.addEventListener('pointerleave', hideChartTips);
}

/** 大图：带网格、坐标、面积填充；可传入 times/label 启用横轴与悬停读数 */
function areaChart(values, color, opts = {}) {
  const w = 640, h = opts.height || 132, padL = 34, padR = 6, padT = 8, padB = 18;
  if (!values || values.length < 2) {
    return `<div class="empty" style="padding:34px 0">暂无数据</div>`;
  }
  const iw = w - padL - padR, ih = h - padT - padB;
  let min = Math.min(...values), max = Math.max(...values);
  if (max - min < 0.001) { max = min + 1; }
  // 留 10% 顶部余量，纵轴取整
  const span = max - min;
  min = Math.max(0, min - span * 0.12);
  max = max + span * 0.12;
  const y = (v) => padT + ih - ((v - min) / (max - min)) * ih;
  const step = values.length > 1 ? iw / (values.length - 1) : iw;
  const pts = values.map((v, i) => `${(padL + i * step).toFixed(1)},${y(v).toFixed(1)}`);
  const line = 'M' + pts.join(' L');
  const area = `${line} L${(padL + iw).toFixed(1)},${padT + ih} L${padL},${padT + ih} Z`;
  const id = 'a' + Math.random().toString(36).slice(2, 8);

  // 4 条横向网格
  const fmt = opts.fmt || ((v) => v.toFixed(0));
  let grid = '';
  for (let i = 0; i <= 3; i++) {
    const gv = min + ((max - min) / 3) * i;
    const gy = y(gv);
    grid += `<line x1="${padL}" y1="${gy.toFixed(1)}" x2="${w - padR}" y2="${gy.toFixed(1)}" stroke="var(--line)" stroke-width="1"/>
             <text x="${padL - 6}" y="${(gy + 3).toFixed(1)}" text-anchor="end" font-size="9" fill="var(--fg-3)" font-family="ui-monospace,monospace">${fmt(gv)}</text>`;
  }

  const times = Array.isArray(opts.times) && opts.times.length === values.length ? opts.times : null;
  let xAxis = '';
  if (times) {
    const spanSec = times[times.length - 1] - times[0];
    const picks = [...new Set([0, Math.floor((values.length - 1) / 3), Math.floor(2 * (values.length - 1) / 3), values.length - 1])];
    xAxis = `<div class="chart-x">${picks.map((i) => `<span>${esc(chartAxisTime(times[i], spanSec))}</span>`).join('')}</div>`;
  }
  const cid = 'c' + (++chartSeq);
  chartRegistry.set(cid, {
    padL, iw, w, label: opts.label || '', fmt, times,
    pts: values.map((v, i) => ({ x: +(padL + i * step).toFixed(1), y: +y(v).toFixed(1), v })),
  });

  return `<div class="chart-wrap" data-chart-id="${cid}">
    <svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" style="width:100%;height:${h}px;display:block">
      <defs><linearGradient id="${id}" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stop-color="${color}" stop-opacity="0.20"/>
        <stop offset="100%" stop-color="${color}" stop-opacity="0"/>
      </linearGradient></defs>
      ${grid}
      <path d="${area}" fill="url(#${id})"/>
      <path d="${line}" fill="none" stroke="${color}" stroke-width="1.8" stroke-linejoin="round" stroke-linecap="round"/>
      <g class="chart-cross" opacity="0" pointer-events="none">
        <line x1="0" y1="${padT}" x2="0" y2="${padT + ih}" stroke="var(--fg-3)" stroke-width="1" stroke-dasharray="3 3"/>
        <circle cx="0" cy="0" r="3" fill="${color}" stroke="var(--panel)" stroke-width="1.5"/>
      </g>
    </svg>
    <div class="chart-tip" hidden></div>
    ${xAxis}
  </div>`;
}

/* ---------------- 表格增强：排序 / 列显隐 / 分页 ----------------
   后台表格数量多，逐个页面改标记成本高且容易漏。
   这里对 #view 内所有表格统一增强：表头可排序，右侧「列」菜单控制显隐，
   带 data-page-size 的长表自动分页。排序偏好存内存，列显隐存 localStorage。 */
const tablePrefs = {};

function tableKey(table, idx) {
  return `${state.route}:${state.param || ''}:${idx}`;
}

function sortCellValue(td) {
  if (!td) return { s: '' };
  const dv = td.dataset.v;
  if (dv !== undefined && dv !== '') {
    const n = Number(dv);
    if (Number.isFinite(n)) return { n };
  }
  const text = (td.innerText || '').replace(/\s+/g, ' ').trim();
  const m = text.match(/^-?[\d,]+(?:\.\d+)?/);
  if (m) {
    const n = Number(m[0].replace(/,/g, ''));
    if (Number.isFinite(n)) return { n, s: text };
  }
  return { s: text };
}

function applyTableSort(table, idx, dir, resetPage = true) {
  const tbody = table.tBodies[0];
  if (!tbody) return;
  const all = Array.from(tbody.rows);
  const rows = all.filter((tr) => !tr.querySelector('.empty'));
  const emptyRows = all.filter((tr) => tr.querySelector('.empty'));
  const decorated = rows.map((tr, i) => ({ tr, v: sortCellValue(tr.children[idx]), i }));
  decorated.sort((a, b) => {
    const an = a.v.n, bn = b.v.n;
    if (an != null && bn != null && an !== bn) return dir === 'asc' ? an - bn : bn - an;
    if (an != null && bn == null) return -1;
    if (an == null && bn != null) return 1;
    const c = a.v.s.localeCompare(b.v.s, 'zh-CN', { numeric: true });
    return dir === 'asc' ? c : -c;
  });
  decorated.forEach((d) => tbody.appendChild(d.tr));
  emptyRows.forEach((tr) => tbody.appendChild(tr));
  const ths = $$('thead th', table);
  ths.forEach((th) => { delete th.dataset.sort; });
  if (ths[idx]) ths[idx].dataset.sort = dir;
  table.dataset.sortIdx = String(idx);
  table.dataset.sortDir = dir;
  const key = table.dataset.tkey;
  if (key) {
    tablePrefs[key] = tablePrefs[key] || {};
    tablePrefs[key].sortIdx = idx;
    tablePrefs[key].dir = dir;
  }
  if (resetPage) table.dataset.page = '1';
  applyTablePagination(table);
}

function toggleTableSort(table, idx) {
  const same = table.dataset.sortIdx === String(idx);
  const dir = same && table.dataset.sortDir === 'asc' ? 'desc' : 'asc';
  applyTableSort(table, idx, dir, true);
}

function setColumnHidden(table, idx, hidden) {
  $$('tr', table).forEach((tr) => {
    const cell = tr.children[idx];
    if (cell) cell.style.display = hidden ? 'none' : '';
  });
}

function bindColumnPicker(table, ths, eligible, hidden) {
  const wrap = table.closest('.table-wrap');
  if (!wrap || eligible.length < 2) return;
  const details = document.createElement('details');
  details.className = 'col-picker';
  const items = eligible.map(([i, label]) => `<label class="col-item">
      <input type="checkbox" data-col="${i}" ${hidden.includes(label) ? '' : 'checked'}>
      <span>${esc(label)}</span>
    </label>`).join('');
  // 按钮文案用「自定义列」而不是单字「列」：单字过于简略，读者无法从按钮
  // 判断它是「选择显示哪些列」的入口（与腾讯云「自定义列」等主流命名一致）。
  // 齿轮图标 + title 进一步说明用途。
  details.innerHTML = `<summary class="btn tiny col-picker-btn" title="选择要显示的列"><svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" aria-hidden="true"><path d="M4 6h16M7 12h10M10 18h4"/></svg>自定义列</summary><div class="col-menu">${items}</div>`;
  wrap.parentNode.insertBefore(details, wrap);
  details.addEventListener('change', (e) => {
    const box = e.target.closest('input[data-col]');
    if (!box) return;
    const i = Number(box.dataset.col);
    setColumnHidden(table, i, !box.checked);
    const key = table.dataset.tkey;
    const pref = tablePrefs[key] || (tablePrefs[key] = {});
    const label = (ths[i].innerText || '').trim();
    pref.hidden = (pref.hidden || []).filter((x) => x !== label);
    if (!box.checked) pref.hidden.push(label);
    try { localStorage.setItem('rollcall.cols.' + key, JSON.stringify(pref.hidden)); } catch (_) { /* 隐私模式忽略 */ }
  });
}

function applyTablePagination(table) {
  const size = Number(table.dataset.pageSize) || 0;
  const wrap = table.closest('.table-wrap');
  if (!size || !wrap) return;
  const tbody = table.tBodies[0];
  if (!tbody) return;
  const rows = Array.from(tbody.rows).filter((tr) => !tr.querySelector('.empty'));
  const pages = Math.ceil(rows.length / size);
  if (pages <= 1) {
    rows.forEach((tr) => { tr.style.display = ''; });
    if (table.__pager) { table.__pager.remove(); table.__pager = null; }
    return;
  }
  let page = Number(table.dataset.page) || 1;
  page = Math.max(1, Math.min(pages, page));
  table.dataset.page = String(page);
  rows.forEach((tr, i) => {
    tr.style.display = i >= (page - 1) * size && i < page * size ? '' : 'none';
  });
  let pager = table.__pager;
  if (!pager || !pager.isConnected) {
    pager = document.createElement('div');
    pager.className = 'table-pager';
    wrap.after(pager);
    table.__pager = pager;
    pager.addEventListener('click', (e) => {
      const btn = e.target.closest('button[data-page]');
      if (!btn) return;
      const cur = Number(table.dataset.page) || 1;
      table.dataset.page = String(cur + Number(btn.dataset.page));
      applyTablePagination(table);
    });
  }
  const sig = `${page}/${pages}/${rows.length}`;
  if (pager.dataset.sig === sig) return;
  pager.dataset.sig = sig;
  pager.innerHTML = `<button class="btn tiny" data-page="-1" ${page <= 1 ? 'disabled' : ''}>上一页</button>
    <span class="pager-info">第 <b class="mono">${page}</b> / ${pages} 页 · 共 ${rows.length} 条</span>
    <button class="btn tiny" data-page="1" ${page >= pages ? 'disabled' : ''}>下一页</button>`;
}

function enhanceTables() {
  const tables = $$('#view .table-wrap table');
  tables.forEach((table, idx) => {
    if (table.dataset.enhanced === '1') { applyTablePagination(table); return; }
    table.dataset.enhanced = '1';
    const key = tableKey(table, idx);
    table.dataset.tkey = key;
    const pref = tablePrefs[key] || (tablePrefs[key] = {});
    const ths = $$('thead th', table);
    const eligible = [];
    ths.forEach((th, i) => {
      const label = (th.innerText || '').trim();
      if (!label || label === '操作' || label === 'Actions' || th.querySelector('input')) return;
      eligible.push([i, label]);
      th.classList.add('sortable');
      th.title = '点击排序';
      th.addEventListener('click', () => toggleTableSort(table, i));
    });
    // 列显隐按表头文本持久化：列顺序调整后依然能命中。
    let hidden = pref.hidden || [];
    try {
      const saved = localStorage.getItem('rollcall.cols.' + key);
      if (saved) hidden = JSON.parse(saved) || [];
    } catch (_) { /* 忽略损坏的本地配置 */ }
    pref.hidden = hidden;
    eligible.forEach(([i, label]) => {
      if (hidden.includes(label)) setColumnHidden(table, i, true);
    });
    bindColumnPicker(table, ths, eligible, hidden);
    if (pref.sortIdx != null && ths[pref.sortIdx]) {
      applyTableSort(table, pref.sortIdx, pref.dir || 'asc', false);
    }
    applyTablePagination(table);
  });
}

/* ---------------- 组件 ---------------- */

function barRow(label, pctVal, warnAt = 75, critAt = 90) {
  if (pctVal == null) {
    return `<div class="nb-row">
      <span class="nb-label">${label}</span>
      <span class="nb-track"></span>
      <span class="nb-val mono">—</span>
    </div>`;
  }
  const t = tone(pctVal, warnAt, critAt);
  const color = t === 'crit' ? 'var(--crit)' : t === 'warn' ? 'var(--warn)' : 'var(--accent)';
  return `<div class="nb-row">
    <span class="nb-label">${label}</span>
    <span class="nb-track"><span class="nb-fill" style="width:${Math.min(100, pctVal).toFixed(1)}%;background:${color}"></span></span>
    <span class="nb-val mono">${pctVal.toFixed(0)}%</span>
  </div>`;
}

function kpi(label, value, unit, foot) {
  return `<div class="kpi">
    <div class="kpi-label">${label}</div>
    <div class="kpi-value"><span class="mono">${value}</span>${unit ? `<span class="kpi-unit">${unit}</span>` : ''}</div>
    ${foot ? `<div class="kpi-foot">${foot}</div>` : ''}
  </div>`;
}

// tbar 表格单元格里的迷你用量条：数值 + 底部细条，一眼可比。
function tbar(v, warnAt = 75, critAt = 90) {
  if (v == null) return '<span class="mono" style="color:var(--fg-3)">—</span>';
  const tone = v >= critAt ? 'crit' : v >= warnAt ? 'warn' : 'ok';
  const color = tone === 'crit' ? 'var(--crit)' : tone === 'warn' ? 'var(--warn)' : 'var(--accent)';
  return `<span class="tbar"><span class="tbar-fill ${tone}" style="width:${Math.min(100, v).toFixed(1)}%;background:${color}"></span></span><span class="mono tbar-val">${v.toFixed(0)}%</span>`;
}

function nodeCard(n) {
  const hasData = hasNodeData(n);
  const cpu = hasData ? n.latest.cpuPct || 0 : null;
  const mem = hasData ? pct(n.latest.memUsed, n.latest.memTotal) : null;
  const disk = hasData ? pct(n.latest.diskUsed, n.latest.diskTotal) : null;
  const spark = (n.spark || []).map((p) => p.v);
  const st = nodeState(n);
  const cardCls = n.state === 'offline' ? 'offline' : n.state === 'stale' ? 'stale' : '';
  const days = n.expireAt ? Math.ceil((new Date(n.expireAt) - Date.now()) / 86400000) : null;
  const expireBadge = days != null && days <= 30
    ? `<span class="badge ${days < 0 ? 'crit' : days <= 7 ? 'crit' : 'warn'}" title="到期 ${esc(n.expireAt)}">${days < 0 ? '已过期' : days + ' 天'}</span>` : '';
  const agentBadge = n.agentOutdated
    ? `<span class="badge warn" title="Agent ${esc(n.agentVersion || '')} 低于控制端">可升级</span>` : '';
  const flag = flagEmoji(n.country);
  const priceBadge = n.price
    ? `<span class="badge" title="年付续费价">${n.currency === 'CNY' ? '¥' : '$'}${n.price}</span>` : '';
  const osLabel = osBadge(n.host?.os);

  return `<article class="node-card ${cardCls} ${n.alerts > 0 ? 'alerting' : ''}" data-node="${esc(n.id)}">
    <div class="nc-head">
      <span class="dot ${st.tone}"></span>
      <span class="nc-name">${esc(n.name)}</span>
      ${flag ? `<span class="nc-flag" title="${esc(n.country)}">${flag}</span>` : ''}
      <span class="nc-tag">${esc(n.provider || '—')}</span>
      ${priceBadge}${expireBadge}${agentBadge}
      <span class="nc-status">${st.text}</span>
    </div>
    <div class="nc-bars">
      ${barRow('CPU', cpu, 75, 90)}
      ${barRow('内存', mem, 80, 92)}
      ${barRow('磁盘', disk, 80, 90)}
    </div>
    <div class="nc-spark">${sparkline(spark, st.tone === 'ok' ? 'var(--accent)' : 'var(--fg-3)')}</div>
    <div class="nc-foot">
      ${osLabel ? `<span title="${esc(n.host?.os || '')}">${osLabel}</span><span class="sep">·</span>` : ''}
      <span>${esc(n.region || '—')}</span>
      <span class="sep">·</span>
      <span class="mono" title="累计入站">${hasData ? '↓' + bytes(n.latest.netRx) : '—'}</span>
      <span class="sep">·</span>
      <span class="mono" title="累计出站">${hasData ? '↑' + bytes(n.latest.netTx) : '—'}</span>
      <span class="sep">·</span>
      <span>${hasData ? dur(n.latest.uptime) : '—'}</span>
    </div>
  </article>`;
}

/* ---------------- 视图 ---------------- */

function setAlertBadge(n) {
  const el = $('#navAlertCount');
  if (!el) return;
  el.hidden = !n;
  el.textContent = n;
}

/* ---------------- live 局部更新 ---------------- */

// 单节点签名：轮询拿到新数据后只重绘真正变化的行。
function nodeLiveSig(n) { return JSON.stringify(n); }

// 记录“当前 DOM 渲染的是哪份数据”，局部更新据此找出变化的节点。
// sigs 可由调用方传入，避免同一轮里对每个节点重复序列化。
function rememberLiveNodes(nodes, sigs) {
  state.liveNodeSig = sigs || new Map((nodes || []).map((n) => [n.id, nodeLiveSig(n)]));
}

function overviewGroups(nodes) {
  const lists = nodes.filter((n) => matchesSearch([n.name, n.provider, n.region, n.group]));
  const byProvider = new Map();
  for (const n of lists) {
    const p = n.provider || '未分类';
    if (!byProvider.has(p)) byProvider.set(p, []);
    byProvider.get(p).push(n);
  }
  return [...byProvider.entries()]
    .map(([p, ns]) => ({
      p, ns,
      online: ns.filter((n) => n.online).length,
      cpu: ns.filter((n) => hasNodeData(n)).reduce((a, n) => a + (n.latest.cpuPct || 0), 0) / Math.max(1, ns.filter((n) => hasNodeData(n)).length),
    }))
    .sort((a, b) => b.ns.length - a.ns.length);
}

function overviewKpisHTML(ov) {
  const expiring = ov.expiring || [];
  const countParts = [`在线 ${ov.online}`];
  if (ov.stale) countParts.push(`数据延迟 ${ov.stale}`);
  if (ov.offline) countParts.push(`离线 ${ov.offline}`);
  if (ov.noData) countParts.push(`待接入 ${ov.noData}`);
  return `
      ${kpi('节点总数', ov.nodes, '', countParts.join(' · '))}
      ${kpi('活跃告警', ov.activeAlert, '', ov.activeAlert > 0 ? '需关注' : '全部正常')}
      ${kpi('平均 CPU', ov.avgCpu.toFixed(1), '%', '在线节点均值')}
      ${kpi('平均内存', ov.avgMem.toFixed(1), '%', '在线节点均值')}
      ${kpi('服务探测', `${ov.services - ov.svcDown}/${ov.services}`, '', ov.svcDown > 0 ? `${ov.svcDown} 个不可用` : '全部可用')}
      ${kpi('实时速率', '↓' + rate(ov.rateIn || 0) + '/s', '', '↑ ' + rate(ov.rateOut || 0) + '/s')}
      ${kpi('本周期流量', bytes(ov.periodUsed || 0), '', (ov.periodQuota || 0) > 0 ? `${((ov.periodUsed || 0) / ov.periodQuota * 100).toFixed(1)}% / ${bytes(ov.periodQuota)}` : '未设配额')}
      ${kpi('月度成本', '$' + ov.monthlyCost.toFixed(1), '', `${expiring.length} 台 30 天内到期`)}
      ${kpi('剩余价值', '$' + (ov.remainingValue || 0).toFixed(0), '', '未到期资产按剩余天数折算')}`;
}

function alertItemInner(n) {
  const sev = n.state === 'offline' || n.state === 'no_data' ? 'critical' : 'warn';
  const detail = n.state === 'stale'
    ? `数据延迟 · 最后上报 ${ago(n.lastSeen)}`
    : !n.online
      ? `已失联 · 最后上报 ${ago(n.lastSeen)}`
      : `CPU ${(n.latest.cpuPct || 0).toFixed(0)}% · 内存 ${pct(n.latest.memUsed, n.latest.memTotal).toFixed(0)}% · 磁盘 ${pct(n.latest.diskUsed, n.latest.diskTotal).toFixed(0)}%`;
  return `<div class="alert-bar ${sev}"></div>
            <div class="alert-main">
              <div class="alert-title">${esc(n.name)}<span class="badge ${sev === 'critical' ? 'crit' : 'warn'}">${n.alerts} 条</span></div>
              <div class="alert-msg">${detail}</div>
            </div>`;
}

function provChipsHTML(rows) {
  return rows.map((r) => `
          <button class="prov-chip" data-prov="${esc(r.p)}" title="CPU 均值 ${r.cpu.toFixed(0)}%">
            <span class="prov-name">${esc(r.p)}</span>
            <span class="prov-cnt">${r.online}/${r.ns.length}</span>
          </button>`).join('');
}

function provRowInner(r) {
  return `<span class="prov-title">${esc(r.p)}</span>
              <span class="prov-meta">${r.ns.length} 台 · 在线 ${r.online} · CPU 均值 ${r.cpu.toFixed(0)}%</span>`;
}

function bindProvChips() {
  $$('.prov-chip').forEach((c) => {
    c.onclick = () => {
      const g = document.getElementById('provrow-' + c.dataset.prov);
      if (g) g.scrollIntoView({ behavior: 'smooth', block: 'center' });
    };
  });
}

// 概览表格行：单元格带 data-cell，局部更新时按单元格替换。
function overviewRowCells(n) {
  const hasData = hasNodeData(n);
  const st = nodeState(n);
  const cpu = hasData ? n.latest.cpuPct || 0 : null;
  const mem = hasData ? pct(n.latest.memUsed, n.latest.memTotal) : null;
  const disk = hasData ? pct(n.latest.diskUsed, n.latest.diskTotal) : null;
  const days = n.expireAt ? Math.ceil((new Date(n.expireAt) - Date.now()) / 86400000) : null;
  return `<td data-cell="state"><span class="dot ${st.tone}"></span> <span class="state-text">${st.text}</span></td>
            <td class="strong" data-cell="name">${esc(n.name)}${days != null && days <= 30 ? ` <span class="badge ${days < 0 ? 'crit' : days <= 7 ? 'crit' : 'warn'}" title="到期 ${esc(n.expireAt)}">${days < 0 ? '已过期' : days + ' 天'}</span>` : ''}${n.agentOutdated ? ` <span class="badge warn" title="Agent ${esc(n.agentVersion || '')} 低于控制端">可升级</span>` : ''}</td>
            <td data-cell="region">${esc(n.region || '—')}</td>
            <td style="font-size:11.5px" data-cell="os">${esc(n.host?.os || '—')}</td>
            <td class="num" data-cell="cpu">${tbar(cpu)}</td>
            <td class="num" data-cell="mem">${tbar(mem, 80, 92)}</td>
            <td class="num" data-cell="disk">${tbar(disk, 80, 90)}</td>
            <td class="num mono" data-cell="load">${hasData ? (n.latest.load1 || 0).toFixed(2) : '—'}</td>
            <td class="num mono" style="font-size:11px" data-cell="traffic">${hasData ? bytes(n.latest.netRx) + ' / ' + bytes(n.latest.netTx) : '—'}</td>
            <td class="num" data-cell="uptime">${hasData ? dur(n.latest.uptime) : '—'}</td>
            <td data-cell="spark"><div class="tspark">${sparkline((n.spark || []).slice(-40).map((p) => p.v), st.tone === 'ok' ? 'var(--accent)' : 'var(--fg-3)', 26)}</div></td>`;
}

// 节点列表行：勾选列与操作列由外层保留，只替换数据列。
function nodesRowCells(n) {
  const hasData = hasNodeData(n);
  const st = nodeState(n);
  const cpu = hasData ? n.latest.cpuPct || 0 : null;
  const mem = hasData ? pct(n.latest.memUsed, n.latest.memTotal) : null;
  const disk = hasData ? pct(n.latest.diskUsed, n.latest.diskTotal) : null;
  const dash = '—';
  const flag = flagEmoji(n.country);
  const osLabel = osBadge(n.host?.os);
  return `<td data-cell="state"><span class="dot ${st.tone}"></span> <span class="state-text">${st.text}</span></td>
            <td class="strong" data-cell="name">${esc(n.name)}${n.agentOutdated ? ` <span class="badge warn badge-click" data-node-upgrade="${esc(n.id)}" title="Agent ${esc(n.agentVersion || '')} 低于控制端 ${esc(state.serverVersion || '')} —— 点击获取该机器的升级命令">可升级</span>` : ''}</td>
            <td data-cell="provider">${esc(n.provider || '—')}</td>
            <td data-cell="region">${flag ? `<span title="${esc(n.country)}">${flag}</span> ` : ''}${esc(n.region || '—')}</td>
            <td class="os-cell" data-cell="os">${osLabel ? `<span class="os-badge" title="${esc(n.host?.os || '')}">${osLabel}</span>` : ''}${esc(n.host?.os || '—')}</td>
            <td class="num mono${cpu > 85 ? ' text-crit' : ''}" data-cell="cpu">${cpu == null ? dash : cpu.toFixed(0) + '%'}</td>
            <td class="num mono" data-cell="mem">${mem == null ? dash : mem.toFixed(0) + '%'}</td>
            <td class="num mono${disk > 88 ? ' text-crit' : ''}" data-cell="disk">${disk == null ? dash : disk.toFixed(0) + '%'}</td>
            <td class="num mono" data-cell="load">${hasData ? (n.latest.load1 || 0).toFixed(2) : dash}</td>
            <td class="num" data-cell="uptime">${hasData ? dur(n.latest.uptime) : dash}</td>
            <td class="num mono" data-cell="traffic">${hasData ? bytes(n.latest.netRx) : dash}</td>`;
}

function filteredNodes(nodes) {
  const f = state.nodeFilter;
  return nodes.filter((n) => {
    const st = n.state || (n.online ? 'online' : 'offline');
    if (f.provider && (n.provider || '') !== f.provider) return false;
    if (f.group && (n.group || '') !== f.group) return false;
    if (f.state && st !== f.state) return false;
    return matchesSearch([n.name, n.provider, n.region, n.group, n.host?.os]);
  });
}

// 按 data-cell 原位替换行内单元格。结构对不上时返回 false，交给整页重绘兜底。
function applyLiveCells(row, html) {
  if (!row) return false;
  const tpl = document.createElement('template');
  tpl.innerHTML = '<table><tbody><tr>' + html + '</tr></tbody></table>';
  const src = tpl.content.querySelector('tr');
  if (!src) return false;
  for (const td of src.children) {
    const cell = row.querySelector('[data-cell="' + td.dataset.cell + '"]');
    if (!cell) return false;
    cell.className = td.className;
    cell.innerHTML = td.innerHTML;
  }
  return true;
}

// patchLive 只更新当前列表页的节点行与概览数字。
// 节点增删、筛选结果、分组顺序或告警集合变化时返回 false，由调用方整页重绘。
function patchLive(ov, nodes) {
  if (!state.liveNodeSig || state.rendering) return false;
  const byId = new Map(nodes.map((n) => [n.id, n]));
  const sigs = new Map(nodes.map((n) => [n.id, nodeLiveSig(n)]));

  if (state.route === 'nodes') {
    // 服务商/分组下拉是渲染时生成的：集合变了就整页重绘，避免筛选项与实际节点脱节。
    const providers = [...new Set(nodes.map((n) => n.provider).filter(Boolean))].sort();
    const groups = [...new Set(nodes.map((n) => n.group).filter(Boolean))].sort();
    const provOpts = $$('#filterProvider option').slice(1).map((o) => o.value);
    const groupOpts = $$('#filterGroup option').slice(1).map((o) => o.value);
    if (provOpts.join('\n') !== providers.join('\n') || groupOpts.join('\n') !== groups.join('\n')) return false;

    const list = filteredNodes(nodes);
    const rows = $$('.nodes-table tbody tr[data-node]');
    if (rows.length !== list.length) return false;
    const rowById = new Map(rows.map((tr) => [tr.dataset.node, tr]));
    for (let i = 0; i < rows.length; i++) if (rows[i].dataset.node !== list[i].id) return false;
    // 筛选时增删的节点可能不改变可见行，但「x / y 台」的总数会变。
    const countMeta = document.getElementById('nodesCountMeta');
    if (countMeta) countMeta.textContent = `${list.length}/${nodes.length} 台`;
    for (const n of list) {
      if (state.liveNodeSig.get(n.id) === sigs.get(n.id)) continue;
      if (!applyLiveCells(rowById.get(n.id), nodesRowCells(n))) return false;
    }
    rememberLiveNodes(nodes, sigs);
    return true;
  }

  if (state.route !== 'overview') return false;

  const groups = overviewGroups(nodes);
  const order = groups.flatMap((r) => r.ns.map((n) => n.id));
  const rows = $$('.node-table tbody tr[data-node]');
  if (rows.length !== order.length) return false;
  const rowById = new Map(rows.map((tr) => [tr.dataset.node, tr]));
  for (let i = 0; i < rows.length; i++) if (rows[i].dataset.node !== order[i]) return false;

  const alerting = nodes.filter((n) => n.alerts > 0);
  // 分页后 DOM 只呈现当前页切片：与切片比对而不是全量；
  // 总数变化但当前页仍有效时，仅刷新计数行而不是整页重绘。
  const alertPages = Math.max(1, Math.ceil(alerting.length / ALERT_PAGE_SIZE));
  if (!Number.isInteger(state.alertPage) || state.alertPage < 1 || state.alertPage > alertPages) return false;
  const pageSlice = alerting.slice((state.alertPage - 1) * ALERT_PAGE_SIZE, state.alertPage * ALERT_PAGE_SIZE);
  const alertEls = $$('.alert-item[data-node]');
  if (pageSlice.length !== alertEls.length) return false;
  for (let i = 0; i < alertEls.length; i++) if (alertEls[i].dataset.node !== pageSlice[i].id) return false;
  const alertMeta = $('#alertMeta');
  if (alertMeta) alertMeta.textContent = `${alerting.length} 台节点异常${alertPages > 1 ? ` · 第 ${state.alertPage}/${alertPages} 页` : ''}`;

  const kpis = $('.kpis');
  if (!kpis) return false;
  kpis.innerHTML = overviewKpisHTML(ov);
  setAlertBadge(ov.activeAlert || 0);

  const strip = $('.prov-strip');
  if (strip) {
    strip.innerHTML = provChipsHTML(groups);
    bindProvChips();
  }
  for (const r of groups) {
    const head = document.getElementById('provrow-' + r.p);
    if (!head || !head.firstElementChild) return false;
    head.firstElementChild.innerHTML = provRowInner(r);
  }

  const alertById = new Map(alerting.map((n) => [n.id, n]));
  for (const el of alertEls) el.innerHTML = alertItemInner(alertById.get(el.dataset.node));

  for (const id of order) {
    const n = byId.get(id);
    if (state.liveNodeSig.get(id) === sigs.get(id)) continue;
    if (!applyLiveCells(rowById.get(id), overviewRowCells(n))) return false;
  }
  rememberLiveNodes(nodes, sigs);
  return true;
}

const TOP_METRICS = [
  ['cpu', 'CPU', (v) => fixed(v, 1) + '%'],
  ['mem', '内存', (v) => fixed(v, 1) + '%'],
  ['disk', '磁盘', (v) => fixed(v, 1) + '%'],
  ['net', '网速', (v) => bytes(v) + '/s'],
];

function topListHTML(trend, metric) {
  const list = (trend && trend.top && trend.top[metric]) || [];
  if (!list.length) return '<div class="empty">暂无数据</div>';
  const fmt = (TOP_METRICS.find((m) => m[0] === metric) || TOP_METRICS[0])[2];
  const max = Math.max(...list.map((x) => x.value), 1);
  return list.map((n, i) => `
    <div class="top-row row-click" data-node="${esc(n.id)}">
      <span class="top-rank">${i + 1}</span>
      <span class="top-name" title="${esc(n.name)}">${esc(n.name)}</span>
      <span class="top-bar"><i style="width:${Math.max(2, (n.value / max) * 100).toFixed(1)}%"></i></span>
      <span class="top-val mono">${esc(fmt(n.value))}</span>
    </div>`).join('');
}

// fleetTrendHTML 计算机群级趋势与资源 Top。
// 数据来自 /api/overview/trend：服务端已按 5m/1h 桶聚合，
// 前端只负责画图与切换 Top 指标，不再自己合并各节点序列。
function fleetTrendHTML(trend) {
  if (!trend || !Array.isArray(trend.points) || trend.points.length < 2) return '';
  const times = trend.points.map((p) => p.t);
  const cpu = trend.points.map((p) => p.cpu);
  const mem = trend.points.map((p) => p.mem);
  const last = trend.points[trend.points.length - 1];
  const ranges = [['24h', '24 小时'], ['7d', '7 天']];
  const metric = state.ovTopMetric || 'cpu';
  return `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">机群趋势</h2>
        <div class="seg" role="group" aria-label="趋势时间范围">
          ${ranges.map(([k, t]) => `<button class="btn sm ${state.ovRange === k ? 'primary' : ''}" data-ov-range="${k}">${t}</button>`).join('')}
        </div>
      </div>
      <div class="fleet-grid">
        <div class="panel chart-panel">
          <div class="panel-head">
            <span>平均 CPU</span>
            <span class="mono">${fixed(last.cpu, 1)}%</span>
          </div>
          ${areaChart(cpu, 'var(--accent)', { times, label: 'CPU', height: 120, fmt: (v) => fixed(v, 1) + '%' })}
        </div>
        <div class="panel chart-panel">
          <div class="panel-head">
            <span>平均内存</span>
            <span class="mono">${fixed(last.mem, 1)}%</span>
          </div>
          ${areaChart(mem, 'var(--ok)', { times, label: '内存', height: 120, fmt: (v) => fixed(v, 1) + '%' })}
        </div>
      </div>
      <div class="panel top-panel">
        <div class="panel-head">
          <span>资源占用 Top 8</span>
          <div class="seg" role="group" aria-label="Top 指标">
            ${TOP_METRICS.map(([k, t]) => `<button class="btn sm ${metric === k ? 'primary' : ''}" data-ov-top="${k}">${t}</button>`).join('')}
          </div>
        </div>
        <div id="topList">${topListHTML(trend, metric)}</div>
      </div>
    </section>`;
}

async function viewOverview() {
  const [ov, nodes, trend] = await Promise.all([
    api('/api/overview'), api('/api/nodes'),
    api('/api/overview/trend?range=' + encodeURIComponent(state.ovRange)).catch(() => null),
  ]);
  state.data.nodes = nodes;
  state.data.trend = trend;
  // 静默轮询用它判断"数据是否真的变了"，避免每次轮询都整体重绘：
  // 重绘会重建搜索框、打断输入，也会让 KPI 数字闪一下。
  state.liveSig = JSON.stringify([ov, nodes]);
  state.livePending = false;
  setAlertBadge(ov.activeAlert || 0);

  const providerRows = overviewGroups(nodes);
  const lists = providerRows.flatMap((r) => r.ns);
  const alerting = nodes.filter((n) => n.alerts > 0);
  // 分页：页码越界（数据变化后）回到第 1 页，保证渲染的切片始终有效。
  const alertPages = Math.max(1, Math.ceil(alerting.length / ALERT_PAGE_SIZE));
  if (!Number.isInteger(state.alertPage) || state.alertPage < 1 || state.alertPage > alertPages) state.alertPage = 1;
  const alertPageItems = alerting.slice((state.alertPage - 1) * ALERT_PAGE_SIZE, state.alertPage * ALERT_PAGE_SIZE);
  const alertMetaText = `${alerting.length} 台节点异常${alertPages > 1 ? ` · 第 ${state.alertPage}/${alertPages} 页` : ''}`;

  $('#view').innerHTML = `
    <div class="kpis">${overviewKpisHTML(ov)}</div>

    ${alerting.length ? `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">正在告警</h2>
        <span class="section-meta" id="alertMeta">${alertMetaText}</span>
      </div>
      <div class="panel">
        ${alertPageItems.map((n) => `
          <div class="alert-item" style="cursor:pointer" data-node="${esc(n.id)}">${alertItemInner(n)}</div>`).join('')}
        ${alertPages > 1 ? `
        <div style="display:flex;align-items:center;gap:10px;margin-top:10px">
          <button class="btn tiny" id="alertPrev" type="button"${state.alertPage <= 1 ? ' disabled' : ''}>‹ 上一页</button>
          <span class="section-meta">${state.alertPage} / ${alertPages}</span>
          <button class="btn tiny" id="alertNext" type="button"${state.alertPage >= alertPages ? ' disabled' : ''}>下一页 ›</button>
        </div>` : ''}
      </div>
    </section>` : ''}

    ${fleetTrendHTML(trend)}

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">节点状态</h2>
        ${sectionActions(`${lists.length} 台 · 按服务商分组`, '<button class="btn sm primary" id="wizBtn">＋ 接入节点</button>')}
      </div>
      <div class="prov-strip">${provChipsHTML(providerRows)}</div>
      <div class="table-wrap"><table class="node-table" data-page-size="10">
        <thead><tr>
          <th>状态</th><th>节点</th><th>位置</th><th>系统</th>
          <th class="num">CPU</th><th class="num">内存</th><th class="num">磁盘</th>
          <th class="num">负载</th><th class="num">流量 ↓/↑</th><th class="num">在线</th><th>24 小时 CPU</th>
        </tr></thead>
        <tbody>
          ${providerRows.map((r) => `
            <tr class="prov-row" id="provrow-${esc(r.p)}"><td colspan="11">${provRowInner(r)}</td></tr>
            ${r.ns.map((n) => `<tr data-node="${esc(n.id)}" style="cursor:pointer">${overviewRowCells(n)}</tr>`).join('')}
          `).join('') || `<tr><td colspan="11"><div class="empty">没有匹配的节点</div></td></tr>`}
        </tbody>
      </table></div>
    </section>
  `;

  const wiz = $('#wizBtn');
  if (wiz) wiz.onclick = showAgentWizard;
  const aprev = $('#alertPrev');
  if (aprev) aprev.onclick = () => { if (state.alertPage > 1) { state.alertPage--; viewOverview(); } };
  const anext = $('#alertNext');
  if (anext) anext.onclick = () => { state.alertPage++; viewOverview(); };
  $$('[data-ov-range]').forEach((b) => {
    b.onclick = () => {
      if (state.ovRange === b.dataset.ovRange) return;
      state.ovRange = b.dataset.ovRange;
      viewOverview();
    };
  });
  $$('[data-ov-top]').forEach((b) => {
    b.onclick = () => {
      state.ovTopMetric = b.dataset.ovTop;
      $$('[data-ov-top]').forEach((x) => x.classList.toggle('primary', x === b));
      const box = $('#topList');
      if (box) box.innerHTML = topListHTML(trend, state.ovTopMetric);
      bindNodeClicks();
    };
  });
  bindProvChips();
  bindNodeClicks();
  rememberLiveNodes(nodes);
}

async function viewNodes() {
  // 筛选条件入 URL（#/nodes?provider=..&group=..&state=..）：刷新/分享可还原，
  // 也便于把「某分组当前离线的机器」这条链接发给未来的自己。
  // 路由只取 path 段，带查询串不会误入节点详情。
  const hq = location.hash.split('?')[1];
  if (hq) {
    const qp = new URLSearchParams(hq);
    if (qp.has('provider')) state.nodeFilter.provider = qp.get('provider') || '';
    if (qp.has('group')) state.nodeFilter.group = qp.get('group') || '';
    if (qp.has('state')) state.nodeFilter.state = qp.get('state') || '';
  }
  const nodes = state.data.nodes || await api('/api/nodes');
  state.data.nodes = nodes;
  const providers = [...new Set(nodes.map((n) => n.provider).filter(Boolean))].sort();
  const groups = [...new Set(nodes.map((n) => n.group).filter(Boolean))].sort();
  const f = state.nodeFilter;
  // 台账里已不存在的选项自动失效，避免筛选后永远为空。
  if (f.provider && !providers.includes(f.provider)) f.provider = '';
  if (f.group && !groups.includes(f.group)) f.group = '';
  const list = filteredNodes(nodes);
  const opt = (v, label, cur) => `<option value="${esc(v)}"${v === cur ? ' selected' : ''}>${esc(label)}</option>`;
  // 状态 chips：计数基于「仅按服务商/分组过滤」的集合（ facet 口径，
  // 点进某个状态前就知道该状态有几台）；零数量的状态置灰不可点。
  const stateBase = nodes.filter((n) => (!f.provider || n.provider === f.provider) && (!f.group || n.group === f.group));
  const stateCount = (s) => stateBase.filter((n) => (n.state || (n.online ? 'online' : 'offline')) === s).length;
  const stateChips = [
    ['', '全部', stateBase.length],
    ['online', '在线', stateCount('online')],
    ['stale', '数据延迟', stateCount('stale')],
    ['offline', '离线', stateCount('offline')],
    ['no_data', '待接入', stateCount('no_data')],
  ].map(([v, label, cnt]) =>
    `<button type="button" class="state-chip${f.state === v ? ' on' : ''}" data-state-chip="${v}"${cnt === 0 && v !== '' ? ' disabled' : ''}>${label} <span class="mono">${cnt}</span></button>`
  ).join('');
  // 视图模式：表格 / 卡片。首次访问按屏幕宽度给默认（移动端卡片更易读），
  // 之后记住选择。卡片模式没有复选框，批量操作条只在表格模式渲染。
  const viewMode = localStorage.getItem('rollcall-nodes-view')
    || (window.matchMedia('(max-width: 720px)').matches ? 'cards' : 'table');

  $('#view').innerHTML = `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">全部节点</h2>
        ${sectionActions(`<span id="nodesCountMeta">${list.length}/${nodes.length} 台</span>`, '<button class="btn sm primary" id="wizBtn2">＋ 接入节点</button>')}
      </div>
      <div class="filter-bar">
        <select id="filterProvider" aria-label="按服务商筛选">${opt('', '全部服务商', f.provider)}${providers.map((p) => opt(p, p, f.provider)).join('')}</select>
        <select id="filterGroup" aria-label="按分组筛选">${opt('', '全部分组', f.group)}${groups.map((g) => opt(g, g, f.group)).join('')}</select>
        <div class="state-chips" role="group" aria-label="按状态筛选">${stateChips}</div>
        <button class="btn sm" id="filterReset" type="button">重置筛选</button>
        <span class="state-chips" role="group" aria-label="视图模式">
          <button type="button" class="state-chip${viewMode === 'table' ? ' on' : ''}" data-view-mode="table" title="表格视图（支持批量操作）">表格</button>
          <button type="button" class="state-chip${viewMode === 'cards' ? ' on' : ''}" data-view-mode="cards" title="卡片视图（移动端推荐）">卡片</button>
        </span>
        ${(f.provider || f.group || f.state || state.search) ? `<span class="section-meta">已启用筛选</span>` : ''}
      </div>
      ${viewMode === 'cards'
        ? `<div class="node-grid">${list.map(nodeCard).join('') || '<div class="empty" style="grid-column:1/-1">没有匹配的节点</div>'}</div>
           <p class="field-hint" style="margin-top:8px">批量操作请切换到表格视图</p>`
        : `
      <div id="batchBar" hidden class="batch-bar">
        <span style="font-size:12px;color:var(--fg-2)">已选 <b id="batchCount" class="mono">0</b> 台</span>
        <span style="flex:1"></span>
        <button class="btn tiny" data-batch="group">改分组</button>
        <button class="btn tiny" data-batch="provider">改服务商</button>
        <button class="btn tiny" data-batch="region">改地区</button>
        <button class="btn tiny" data-batch="expire">改到期日</button>
        <button class="btn tiny" data-batch="currency">改币种</button>
        <button class="btn tiny" data-batch="weight">改权重</button>
        <button class="btn tiny" data-batch="favorite">改收藏</button>
        <button class="btn tiny" data-batch="exempt">告警豁免</button>
        <button class="btn tiny" data-batch="traffic">改流量</button>
        <button class="btn tiny" data-batch="tags">改标签</button>
        <button class="btn tiny" data-batch="upgrade">加入升级清单</button>
        <button class="btn tiny danger" data-batch="delete">删除</button>
      </div>
      <div class="table-wrap"><table class="nodes-table" data-page-size="10">
        <thead><tr>
          <th class="col-check"><input type="checkbox" id="selAll" aria-label="全选"></th>
          <th title="在线 / 离线：15 分钟无上报判为离线">状态</th><th>节点</th><th>服务商</th><th title="机房所在地区">位置</th><th title="操作系统与内核">系统</th>
          <th class="num" title="当前 CPU 使用率">CPU</th><th class="num" title="当前内存使用率">内存</th><th class="num" title="磁盘空间使用率">磁盘</th>
          <th class="num" title="1 / 5 / 15 分钟平均负载">负载</th><th class="num" title="自上次开机起的运行时长">运行时长</th><th class="num" title="实时网速与计费周期用量">流量</th><th class="num">操作</th>
        </tr></thead>
        <tbody>${list.map((n) => `<tr class="row-click" data-node="${esc(n.id)}">
            <td><input type="checkbox" class="row-sel" data-id="${esc(n.id)}" aria-label="选择 ${esc(n.name)}"></td>
            ${nodesRowCells(n)}
            <td class="num"><div class="row-actions">
              ${n.agentOutdated ? `<button class="btn tiny primary" data-node-upgrade="${esc(n.id)}" title="获取该机器的 Agent 升级命令">升级</button>` : ''}
              <button class="btn tiny" data-node-edit="${esc(n.id)}">编辑</button>
              <button class="btn tiny" data-node-agent="${esc(n.id)}">接入</button>
            </div></td>
          </tr>`).join('') || `<tr><td colspan="13"><div class="empty">没有匹配的节点</div></td></tr>`}</tbody>
      </table></div>`}
    </section>`;
  bindNodeClicks();
  const wiz2 = document.getElementById('wizBtn2');
  if (wiz2) wiz2.onclick = showAgentWizard;
  // 筛选变化后把条件写回 URL（replaceState 不产生多余历史记录）。
  const syncNodesHash = () => {
    const ff = state.nodeFilter;
    const qp = new URLSearchParams();
    if (ff.provider) qp.set('provider', ff.provider);
    if (ff.group) qp.set('group', ff.group);
    if (ff.state) qp.set('state', ff.state);
    const q = qp.toString();
    history.replaceState(null, '', location.pathname + location.search + '#/nodes' + (q ? '?' + q : ''));
  };
  const bindFilter = (id, key) => {
    const el = document.getElementById(id);
    if (el) el.onchange = () => { state.nodeFilter[key] = el.value; syncNodesHash(); viewNodes(); };
  };
  bindFilter('filterProvider', 'provider');
  bindFilter('filterGroup', 'group');
  $$('[data-state-chip]').forEach((b) => {
    b.onclick = () => { state.nodeFilter.state = b.dataset.stateChip || ''; syncNodesHash(); viewNodes(); };
  });
  $$('[data-view-mode]').forEach((b) => {
    b.onclick = () => {
      localStorage.setItem('rollcall-nodes-view', b.dataset.viewMode);
      viewNodes();
    };
  });
  const reset = document.getElementById('filterReset');
  if (reset) reset.onclick = () => {
    state.nodeFilter = { provider: '', group: '', state: '' };
    syncNodesHash();
    viewNodes();
  };
  $$('[data-node-upgrade]').forEach((b) => {
    b.onclick = (e) => {
      e.stopPropagation();
      const id = b.dataset.nodeUpgrade;
      const n = (state.data.nodes || []).find((x) => x.id === id);
      if (n) showNodeUpgrade(n);
    };
  });
  $$('[data-node-edit]').forEach((b) => {
    b.onclick = async (e) => {
      e.stopPropagation();
      try {
        await editNodeById(b.dataset.nodeEdit, viewNodes);
      } catch (err) { toast('读取节点失败：' + err.message, 'err'); }
    };
  });
  $$('[data-node-agent]').forEach((b) => {
    b.onclick = async (e) => {
      e.stopPropagation();
      try {
        const n = await adminApi('/api/admin/nodes/' + encodeURIComponent(b.dataset.nodeAgent));
        showAgentConfig(n);
      } catch (err) { toast('读取接入信息失败：' + err.message, 'err'); }
    };
  });
  bindBatchOps(list);
  rememberLiveNodes(nodes);
}

// bindBatchOps 节点多选与批量操作。
// 行点击仍是"进入详情"，勾选列单独处理 —— 复选框的 click 不再冒泡触发行跳转。
function bindBatchOps(list) {
  const bar = document.getElementById('batchBar');
  const count = document.getElementById('batchCount');
  if (!bar) return;
  const checked = () => $$('.row-sel').filter((c) => c.checked).map((c) => c.dataset.id);
  const sync = () => {
    const ids = checked();
    bar.hidden = ids.length === 0;
    if (count) count.textContent = String(ids.length);
  };
  $$('.row-sel').forEach((c) => {
    c.onclick = (e) => { e.stopPropagation(); sync(); };
  });
  const selAll = document.getElementById('selAll');
  if (selAll) {
    selAll.onclick = (e) => {
      e.stopPropagation();
      $$('.row-sel').forEach((c) => { c.checked = selAll.checked; });
      sync();
    };
  }
  const reload = () => { state.data.nodes = null; viewNodes(); };
  const PROMPTS = {
    group: { label: '新分组名', ph: '例如：prod', def: '' },
    provider: { label: '服务商', ph: '例如：racknerd', def: '' },
    region: { label: '地区', ph: '例如：香港', def: '' },
    expire: { label: '到期日（YYYY-MM-DD，留空清除）', ph: '2027-01-31', def: '' },
    currency: { label: '币种（USD / CNY）', ph: 'USD', def: 'USD' },
  };
  $$('[data-batch]').forEach((b) => {
    b.onclick = async (e) => {
      e.stopPropagation();
      const ids = checked();
      if (!ids.length) { toast('请先勾选节点', 'err'); return; }
      const action = b.dataset.batch;
      if (action === 'delete') {
        const names = ids.map((id) => { const n = list.find((x) => x.id === id); return n ? n.name : id; });
        if (!(await modalConfirm('批量删除节点',
          `确认删除 ${ids.length} 台节点？\n\n${names.join('、')}\n\n历史数据会一并删除，且无法恢复。`,
          { danger: true, okText: '删除' }))) return;
        try {
          const res = await adminApi('/api/admin/nodes/batch', {
            method: 'POST',
            body: JSON.stringify({ ids, action: 'delete' }),
          });
          toast(`已删除 ${res.changed} 台节点`);
          reload();
        } catch (err) { toast('删除失败：' + err.message, 'err'); }
        return;
      }
      if (action === 'upgrade') { openUpgradeBatchDialog(list, ids, reload); return; }
      if (action === 'traffic') { openBatchTrafficDialog(ids, reload); return; }
      if (action === 'tags') { openBatchTagsDialog(ids, reload); return; }
      if (action === 'favorite') { openBatchFavoriteDialog(ids, reload); return; }
      if (action === 'exempt') { openBatchExemptDialog(ids, reload); return; }
      if (action === 'weight') {
        const v = await modalPrompt('修改排序权重', '排序权重（-1000–1000，数字越小越靠前）', {
          type: 'number', value: '0', min: -1000, max: 1000,
          hint: `将应用到 ${ids.length} 台节点`,
        });
        if (v === null) return;
        const weight = Number(v);
        if (!Number.isInteger(weight) || weight < -1000 || weight > 1000) {
          toast('请输入 -1000 到 1000 之间的整数', 'err'); return;
        }
        try {
          await adminApi('/api/admin/nodes/batch', {
            method: 'POST',
            body: JSON.stringify({ ids, action: 'weight', weight }),
          });
          toast(`已更新 ${ids.length} 台节点`);
          reload();
        } catch (err) { toast('批量操作失败：' + err.message, 'err'); }
        return;
      }
      const p = PROMPTS[action];
      if (!p) return;
      const v = await modalPrompt('批量修改', p.label, {
        value: p.def, placeholder: p.ph, hint: `将应用到 ${ids.length} 台节点`,
      });
      if (v === null) return;
      try {
        await adminApi('/api/admin/nodes/batch', {
          method: 'POST',
          body: JSON.stringify({ ids, action, value: v }),
        });
        toast(`已更新 ${ids.length} 台节点`);
        reload();
      } catch (err) { toast('批量操作失败：' + err.message, 'err'); }
    };
  });
}

// 批量对话框共用提交逻辑：成功后关闭并刷新节点列表。
function batchDialog(ids, title, bodyHTML, after, buildBody) {
  const dlg = openDialog(title + '（' + ids.length + ' 台）', `
    <form id="batchForm" class="form-stack">
      ${bodyHTML}
      <div class="form-actions">
        <button class="btn" type="button" data-close>取消</button>
        <button class="btn primary" type="submit">应用到 ${ids.length} 台</button>
      </div>
    </form>`);
  const f = dlg.querySelector('#batchForm');
  f.onsubmit = async (ev) => {
    ev.preventDefault();
    try {
      await adminApi('/api/admin/nodes/batch', {
        method: 'POST',
        body: JSON.stringify(Object.assign({ ids }, buildBody(f))),
      });
      dlg.close();
      toast('已更新 ' + ids.length + ' 台节点');
      (after || (() => {}))();
    } catch (err) { toast('批量操作失败：' + err.message, 'err'); }
  };
}

// openTrafficAdjustDialog 人工校正本周期已用流量。
// 场景：月中才配好重置日、或机器从别处迁移过来，服务端没见过本周期已跑掉的量，
// 不补录就永远和服务商面板对不上。填一次即可，之后的增量会继续累加。
// loginStateHtml 渲染 Agent 自报的 wtmp 采集状态。
//
// 刻意不写"正常/异常"这种判定：这批节点上实测到的两种情况（wtmp 多日未写、
// 或只写 LOGIN_PROCESS 没有登录记录）在数据上无法与"最近确实没人登录"区分。
// 摆事实 + 一句自查提示，比给一个可能是错的结论有用。
function loginStateHtml(n) {
  const st = n.loginState || '';
  if (!st) {
    return '<span class="badge mute">未上报</span> Agent 较旧（v1.19.1+ 才报状态）';
  }
  if (st === 'unavailable') {
    return '<span class="badge crit">不可用</span> 读不到 /var/log/wtmp，本机不会有登录告警';
  }
  const age = Number(n.loginAge == null ? -1 : n.loginAge);
  const last = Number(n.loginLastTs || 0);
  const now = Math.floor(Date.now() / 1000);
  const wrote = age >= 0 ? `最后写入 ${ago(now - age)}前` : '写入时间未知';
  const seen = last > 0
    ? `最后一条登录记录 ${ago(last)}前`
    : '窗口内没有登录记录';
  // 登录记录超过一天没更新、或压根没有：给出自查提示（不判定为故障）
  const suspicious = last === 0 || (now - last) > 86400;
  const badge = suspicious ? '<span class="badge warn">待确认</span>' : '<span class="badge ok">有记录</span>';
  const hint = suspicious
    ? '若你刚登录过这台机器而这里没更新，说明本机 sshd 没把登录写进 wtmp'
    : '';
  return `${badge} ${wrote} · ${seen}${hint ? ` <span class="muted">${hint}</span>` : ''}`;
}

function openTrafficAdjustDialog(nodeId, currentUsed, after) {
  const gb = (currentUsed / (1024 ** 3)).toFixed(2);
  const d = openDialog('校正本周期已用流量', `
    <p class="modal-text">按服务商面板上的数字填一次即可；写入后 Agent 的增量会继续在它之上累加。</p>
    <div class="field">
      <span class="field-label">本期已用（GB）</span>
      <input id="adjGb" type="number" step="0.01" min="0" value="${gb}">
      <p class="field-hint">当前记录 ${bytes(currentUsed)} · 填 0 表示本周期归零重算</p>
    </div>
    <div class="form-actions">
      <button class="btn" type="button" data-close>取消</button>
      <button class="btn primary" type="button" id="adjOk">写入</button>
    </div>`);
  d.querySelector('#adjOk').onclick = async () => {
    const val = Number(d.querySelector('#adjGb').value);
    if (!Number.isFinite(val) || val < 0) { toast('请填写非负数字', 'err'); return; }
    try {
      const r = await adminApi(`/api/admin/nodes/${encodeURIComponent(nodeId)}/traffic-adjust`, {
        method: 'POST', body: JSON.stringify({ usedGb: val }),
      });
      d.close();
      toast(`已校正为 ${bytes(r.after)}`);
      if (after) after();
    } catch (err) {
      toast('校正失败：' + err.message, 'err');
    }
  };
}

function openBatchTrafficDialog(ids, after) {
  batchDialog(ids, '批量设置流量配额', `
    <div class="grid-2">
      ${field('月度配额（GB，0 = 不限制）', 'trafficQuotaGb', '', { type: 'number', placeholder: '0' })}
      ${field('重置日', 'trafficResetDay', '1', { type: 'number', hint: '每月 1–28 日' })}
    </div>
    ${selectField('计费口径', 'trafficMode', 'sum', TRAFFIC_MODES.map((x) => ({ v: x.v, t: x.t })))}
    <div class="field-hint">按商家计费方式选择：双边计费用「收发合计」，只算单向用对应单边口径。配额归零表示不限制。</div>`,
  after, (f) => ({
    trafficQuotaGb: Number(f.trafficQuotaGb.value) || 0,
    trafficResetDay: Number(f.trafficResetDay.value) || 0,
    trafficMode: f.trafficMode.value || 'sum',
  }));
}

function openBatchTagsDialog(ids, after) {
  batchDialog(ids, '批量修改标签', `
    ${selectField('操作', 'tagMode', 'add', [
      { v: 'add', t: '追加标签' }, { v: 'remove', t: '移除标签' }, { v: 'replace', t: '替换全部标签' },
    ])}
    ${field('标签', 'tags', '', { placeholder: 'cn2, 高防', hint: '逗号分隔，最多 8 个；移除时不存在的标签会被忽略' })}`,
  after, (f) => ({
    tagMode: f.tagMode.value,
    tags: splitTags(f.tags.value),
  }));
}

function openBatchFavoriteDialog(ids, after) {
  batchDialog(ids, '批量设置收藏', `
    ${selectField('收藏状态', 'favorite', '1', [
      { v: '1', t: '设为收藏（对比页优先展示）' }, { v: '0', t: '取消收藏' },
    ])}`,
  after, (f) => ({ favorite: f.favorite.value === '1' }));
}

function openBatchExemptDialog(ids, after) {
  batchDialog(ids, '批量设置告警豁免', `
    ${selectField('豁免状态', 'exempt', '1', [
      { v: '1', t: '开启豁免（不再评估任何指标规则，既有告警关闭）' }, { v: '0', t: '关闭豁免（恢复正常评估）' },
    ])}`,
  after, (f) => ({ alertExempt: f.exempt.value === '1' }));
}

async function viewNodeDetail(id) {
  const key = id + '|' + state.range;
  let detail, series;
  // 轮询已经拉到的新数据直接复用，避免"比对一次、渲染再拉一次"。
  // 超过 5 秒的缓存不再使用：可能期间用户切换过节点/区间，重新拉更稳。
  const cached = state.detailCache;
  if (cached && cached.key === key && Date.now() - cached.at < 5000) {
    detail = cached.detail;
    series = cached.series;
    state.detailCache = null;
  } else {
    const pair = await Promise.all([
      api('/api/nodes/' + id),
      api(`/api/nodes/${id}/series?range=${state.range}`),
      // 详情页的上一台/下一台需要完整节点顺序；首次直接打开详情时补拉一次。
      state.data.nodes ? Promise.resolve(null) : api('/api/nodes').catch(() => []),
    ]);
    detail = pair[0];
    series = pair[1];
    if (pair[2]) state.data.nodes = pair[2];
  }
  state.detailSig = JSON.stringify([detail, series]);
  const n = detail.node, host = detail.host || {};
  const pts = series.points || [];
  const resolution = series.resolution || 'raw';
  const hasData = (detail.state || (detail.online ? 'online' : 'offline')) !== 'no_data';
  const st = nodeState({ state: detail.state, online: detail.online, alerts: 0 });
  // 聚合区间返回桶内平均值与增量；原始区间仍是累计计数器。
  const bucketSec = { '1m': 60, '5m': 300, '1h': 3600 }[resolution] || 0;
  const cpuVals = pts.map((p) => p.cpuPct);
  const memVals = pts.map((p) => (typeof p.memPct === 'number' ? p.memPct : pct(p.memUsed, p.memTotal)));
  const diskVals = pts.map((p) => (typeof p.diskPct === 'number' ? p.diskPct : pct(p.diskUsed, p.diskTotal)));
  // 网络用「速率」而非累计值：累计曲线在长时间窗口里几乎是一条直线，看不出波动
  const netVals = [];
  if (bucketSec > 0) {
    for (const p of pts) netVals.push((p.netRxDelta + p.netTxDelta) / bucketSec);
  } else {
    for (let i = 1; i < pts.length; i++) {
      const dt = (pts[i].ts - pts[i - 1].ts) || 1;
      const d = (pts[i].netRx + pts[i].netTx) - (pts[i - 1].netRx + pts[i - 1].netTx);
      netVals.push(Math.max(0, d / dt));
    }
  }
  // 温度：hwmon 传感器的最高温。节点没有传感器时全程无值，整块不渲染。
  // 过渡期（Agent 刚升级）前几个点可能缺温度：用第一个有效值补齐起点、
  // 缺失点沿用上一个已知值 —— 温度是缓变量，这样画不会出现"跌到 0"的假象。
  let tempVals = null;
  const firstTempPt = pts.find((p) => typeof p.tempC === 'number' && p.tempC > 0);
  if (firstTempPt) {
    let cur = firstTempPt.tempC;
    tempVals = pts.map((p) => {
      if (typeof p.tempC === 'number' && p.tempC > 0) cur = p.tempC;
      return cur;
    });
  }
  const hasTemp = !!(tempVals && tempVals.filter((v) => v > 0).length >= 2);
  const lastTemp = hasTemp ? tempVals[tempVals.length - 1] : 0;

  // 磁盘 I/O 速率与网络同理，但聚合桶只落了网络增量、没有磁盘 I/O 增量，
  // 因此只在原始区间（1h）可算；长范围明确说明而不是画一条假曲线。
  const diskIoVals = [];
  if (bucketSec === 0) {
    for (let i = 1; i < pts.length; i++) {
      const dt = (pts[i].ts - pts[i - 1].ts) || 1;
      const d = ((pts[i].diskReadBytes || 0) + (pts[i].diskWriteBytes || 0))
        - ((pts[i - 1].diskReadBytes || 0) + (pts[i - 1].diskWriteBytes || 0));
      diskIoVals.push(Math.max(0, d / dt));
    }
  }
  const times = pts.map((p) => p.ts);
  const netTimes = bucketSec > 0 ? times : times.slice(1);
  const loadVals = pts.map((p) => p.load1);
  // 图表脚注：窗口内均值与峰值。点太少不算，避免 1 个样本冒充"均值"。
  // p95Of：对原始样本做精确分位（线性插值），与后端 cpuhist 的语义一致。
  // raw/1m 区间样本量小（≤2880），排序成本可忽略。
  const p95Of = (vals) => {
    if (!vals || !vals.length) return 0;
    const a = vals.filter((v) => typeof v === 'number' && isFinite(v)).slice().sort((x, y) => x - y);
    if (!a.length) return 0;
    const pos = 0.95 * (a.length - 1);
    const lo = Math.floor(pos), hi = Math.ceil(pos);
    return a[lo] + (a[hi] - a[lo]) * (pos - lo);
  };
  // 区间 p95：聚合区间（5m/1h）用服务端桶内分位数，取各桶最大值 ——
  // 关心的是"最差时段的尾部水位"，而不是全区间的平均尾部（会被空闲期拉低）。
  // 老桶（升级前落盘）没有直方图，cpuP95 为 0，自动回退到自算。
  const p95FromBuckets = (list) => {
    const vs = (list || []).map((p) => p && p.cpuP95).filter((v) => typeof v === 'number' && v > 0);
    return vs.length ? Math.max(...vs) : 0;
  };
  const winStat = (vals, fmt, p95) => (vals && vals.length >= 2
    ? `<div class="chart-foot mono"${p95 ? ' title="p95 取区间内最差时段的桶内 95% 分位（直方图近似，误差 ≤ 档宽一半）"' : ''}>均值 ${fmt(vals.reduce((a, b) => a + b, 0) / vals.length)} · 峰值 ${fmt(Math.max(...vals))}${p95 ? ` · <span style="color:var(--fg-2)">p95 ${fmt(p95)}</span>` : ''}</div>`
    : '');
  const last = detail.latest || {};
  const traffic = detail.traffic || {};
  // 服务端已按节点配置的计费口径合成了 used（收发合计 / 只计单边 / 取较大者），
  // 这里不能再自己把收发相加——单边计费的节点会显示成双倍用量。
  // 保留 rx+tx 兜底：升级瞬间浏览器可能还连着旧版服务端。
  const trafficUsed = traffic.used != null ? traffic.used : (traffic.usedRx || 0) + (traffic.usedTx || 0);
  const trafficMode = trafficModeLabel(traffic.mode);
  const quotaBytes = (traffic.quotaGb || 0) * 1024 * 1024 * 1024;
  const quotaPctVal = quotaBytes > 0 ? (trafficUsed / quotaBytes) * 100 : 0;
  const mounts = last.mounts || [];
  const nics = last.nics || [];
  const navList = state.data.nodes || [];
  const navIdx = navList.findIndex((x) => x.id === id);
  const prevNode = navIdx > 0 ? navList[navIdx - 1] : null;
  const nextNode = navIdx >= 0 && navIdx < navList.length - 1 ? navList[navIdx + 1] : null;

  const memUsed = last.memUsed || 0, memTotal = last.memTotal || 1;
  const diskUsed = last.diskUsed || 0, diskTotal = last.diskTotal || 1;

  $('#view').innerHTML = `
    <div class="detail-head">
      <button class="back-btn" id="backToNodes">
        <svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M15 18l-6-6 6-6"/></svg>
        返回
      </button>
      <div style="flex:1">
        <div style="display:flex;align-items:center;gap:9px;flex-wrap:wrap">
          <span class="dot ${st.tone}"></span>
          <h2 style="font-size:17px;font-weight:700;margin:0;letter-spacing:0">${esc(n.name)}</h2>
          <span class="nc-tag">${esc(n.provider || '—')}</span>
          <span class="nc-tag">${esc(n.group || '—')}</span>
          <span class="badge ${st.tone === 'off' ? 'mute' : st.tone}">${st.text}</span>
        </div>
        <p style="font-size:12px;color:var(--fg-3);margin:3px 0 0">
          ${esc(host.hostname || '—')} · ${esc(host.os || '—')} · ${esc(host.kernel || '')} · ${host.cores || '?'} 核
        </p>
      </div>
      <div class="detail-actions">
        ${navList.length > 1 ? `<div class="detail-nav">
          <button class="btn sm" id="prevNode" type="button" ${prevNode ? '' : 'disabled'} title="${prevNode ? esc(prevNode.name) : ''}" aria-label="上一台节点">←</button>
          <span class="field-hint" style="white-space:nowrap">${navIdx + 1} / ${navList.length}</span>
          <button class="btn sm" id="nextNode" type="button" ${nextNode ? '' : 'disabled'} title="${nextNode ? esc(nextNode.name) : ''}" aria-label="下一台节点">→</button>
        </div>` : ''}
        <div class="range-tabs">
          ${[['1h', '1 小时'], ['6h', '6 小时'], ['24h', '24 小时'], ['7d', '7 天'], ['30d', '30 天'], ['365d', '365 天']]
            .map(([r, label]) => `<button class="${state.range === r ? 'on' : ''}" data-range="${r}">${label}</button>`).join('')}
        </div>
        ${resolution !== 'raw' ? `<span class="section-meta" title="长范围不返回全部原始点">${({ '1m': '已按 1 分钟桶聚合，每点为桶内均值（网络为增量）', '5m': '已按 5 分钟桶聚合，每点为桶内均值（网络为增量）', '1h': '已按 1 小时桶聚合，每点为桶内均值（网络为增量）' })[resolution] || ''}</span>` : ''}
        <button class="btn sm" id="nodeCopyBtn" type="button">复制摘要</button>
        <button class="btn sm" id="nodeEditBtn" type="button">编辑资产</button>
        <button class="btn sm" id="nodeAgentBtn" type="button">接入/令牌</button>
      </div>
    </div>

    <div class="detail-grid">
      <div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">CPU 使用率</span>
            <span class="chart-val mono" style="color:${hasData && (last.cpuPct || 0) > 85 ? 'var(--crit)' : 'var(--fg)'}">${hasData ? (last.cpuPct || 0).toFixed(1) + '%' : '—'}</span></div>
          ${areaChart(cpuVals, 'var(--accent)', { times, label: 'CPU', fmt: (v) => v.toFixed(1) + '%' })}
          ${winStat(cpuVals, (v) => v.toFixed(1) + '%', p95FromBuckets(pts) || p95Of(cpuVals))}
        </div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">内存使用率</span>
            <span class="chart-val mono">${hasData ? `${pct(memUsed, memTotal).toFixed(1)}%<span style="font-size:11px;color:var(--fg-3);font-weight:400"> · ${bytes(memUsed)} / ${bytes(memTotal)}</span>` : '—'}</span></div>
          ${areaChart(memVals, 'var(--accent-2)', { times, label: '内存', fmt: (v) => v.toFixed(1) + '%' })}
          ${winStat(memVals, (v) => v.toFixed(1) + '%')}
        </div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">磁盘使用率</span>
            <span class="chart-val mono">${hasData ? `${pct(diskUsed, diskTotal).toFixed(1)}%<span style="font-size:11px;color:var(--fg-3);font-weight:400"> · ${bytes(diskUsed)} / ${bytes(diskTotal)}</span>` : '—'}</span></div>
          ${areaChart(diskVals, 'var(--warn)', { times, label: '磁盘', fmt: (v) => v.toFixed(1) + '%' })}
          ${winStat(diskVals, (v) => v.toFixed(1) + '%')}
        </div>
        ${hasTemp ? `<div class="chart-card">
          <div class="chart-head"><span class="chart-title">温度（hwmon 最高）</span>
            <span class="chart-val mono" style="color:${lastTemp > 85 ? 'var(--crit)' : 'var(--fg)'}">${lastTemp.toFixed(1)}℃</span></div>
          ${areaChart(tempVals, 'var(--warn)', { times, label: '温度', fmt: (v) => v.toFixed(0) + '℃' })}
          ${winStat(tempVals, (v) => v.toFixed(1) + '℃')}
        </div>` : ''}
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">网络吞吐速率</span>
            <span class="chart-val" style="font-size:13px">${hasData ? `<span class="mono">↓${bytes(last.netRx)} ↑${bytes(last.netTx)}</span> <span style="color:var(--fg-3);font-weight:400;letter-spacing:0">累计</span>` : '—'}</span></div>
          ${areaChart(netVals, 'var(--ok)', { height: 110, times: netTimes, label: '速率', fmt: (v) => rate(v) + '/s' })}
          ${winStat(netVals, (v) => rate(v) + '/s')}
        </div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">磁盘 I/O 速率</span>
            <span class="chart-val" style="font-size:13px">${hasData && (last.diskReadBytes || last.diskWriteBytes) ? `<span class="mono">读 ${bytes(last.diskReadBytes || 0)} · 写 ${bytes(last.diskWriteBytes || 0)}</span> <span style="color:var(--fg-3);font-weight:400;letter-spacing:0">累计</span>` : '—'}</span></div>
          ${bucketSec === 0
            ? areaChart(diskIoVals, 'var(--accent)', { height: 110, times: netTimes, label: '磁盘 I/O', fmt: (v) => rate(v) + '/s' }) + winStat(diskIoVals, (v) => rate(v) + '/s')
            : '<div class="empty" style="padding:34px 0">磁盘 I/O 只保留 1 小时原始数据，长范围未聚合；切换到「1 小时」查看</div>'}
        </div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">1 分钟负载</span>
            <span class="chart-val mono">${hasData ? (last.load1 || 0).toFixed(2) : '—'}</span></div>
          ${areaChart(loadVals, 'var(--fg-2)', { height: 100, times, label: '负载', fmt: (v) => v.toFixed(2) })}
          ${winStat(loadVals, (v) => v.toFixed(2))}
        </div>
      </div>

      <div class="panel-stack">
        <div class="panel panel-pad">
          <div class="panel-head"><span class="panel-title">系统信息</span></div>
          <div class="info-list">
            <div class="info-row"><span class="info-k">主机名</span><span class="info-v mono">${esc(host.hostname || '—')}</span></div>
            <div class="info-row"><span class="info-k">操作系统</span><span class="info-v">${esc(host.os || '—')}</span></div>
            <div class="info-row"><span class="info-k">内核</span><span class="info-v mono">${esc(host.kernel || '—')}</span></div>
            <div class="info-row"><span class="info-k">架构</span><span class="info-v mono">${esc(host.arch || '—')}</span></div>
            <div class="info-row"><span class="info-k">CPU</span><span class="info-v" title="${esc(host.cpuModel || '')}">${esc((host.cpuModel || '—').slice(0, 22))}</span></div>
            <div class="info-row"><span class="info-k">虚拟化</span><span class="info-v mono">${esc(host.virt || '—')}</span></div>
            <div class="info-row"><span class="info-k">负载 1/5/15</span><span class="info-v mono">${hasData ? `${(last.load1 || 0).toFixed(2)} / ${(last.load5 || 0).toFixed(2)} / ${(last.load15 || 0).toFixed(2)}` : '—'}</span></div>
            <div class="info-row"><span class="info-k">运行时长</span><span class="info-v">${hasData ? dur(last.uptime) : '—'}</span></div>
            <div class="info-row"><span class="info-k">TCP 连接</span><span class="info-v mono">${hasData ? last.tcpConn || 0 : '—'}</span></div>
            ${hasData && (last.stealPct > 0 || last.iowaitPct > 0) ? `<div class="info-row"><span class="info-k">CPU steal / iowait</span><span class="info-v mono" title="steal 高通常意味着宿主机超售把 CPU 时间片偷走了">${(last.stealPct || 0).toFixed(1)}% / ${(last.iowaitPct || 0).toFixed(1)}%</span></div>` : ''}
            <div class="info-row"><span class="info-k">进程数</span><span class="info-v mono">${hasData ? last.procs || 0 : '—'}</span></div>
            <div class="info-row"><span class="info-k">Agent 版本</span><span class="info-v mono">${esc(n.agentVersion || '—')}${detail.agentOutdated ? ' <span class="badge warn">可升级</span>' : ''}</span></div>
            <div class="info-row"><span class="info-k">上报来源</span><span class="info-v mono">${esc(n.lastIP || '—')}</span></div>
            <div class="info-row"><span class="info-k">上报签名</span><span class="info-v">${n.signed
              ? `已签名${n.clockSkew ? ` <span class="badge ${Math.abs(n.clockSkew) > 30 ? 'warn' : 'mute'}">时钟偏差 ${n.clockSkew > 0 ? '+' : ''}${n.clockSkew}s</span>` : ''}`
              : '<span class="badge mute">未签名</span>'}</span></div>
            <div class="info-row"><span class="info-k">最后上报</span><span class="info-v">${ago(detail.lastSeen)}</span></div>
            <div class="info-row"><span class="info-k">登录采集</span><span class="info-v">${loginStateHtml(n)}</span></div>
            <div class="info-row"><span class="info-k">最近登录</span><span class="info-v">${n.lastLogin
              ? `${esc(n.lastLogin.user)} @ <span class="mono">${esc(n.lastLogin.host || '本地控制台')}</span>
                 <span class="badge mute">${esc(n.lastLogin.line || '未知终端')}</span> ${ago(n.lastLogin.ts)}`
              : '—'}</span></div>
          </div>
        </div>

        <div class="panel panel-pad">
          <div class="panel-head">
            <span class="panel-title">资产</span>
            <button class="btn tiny" id="assetEditBtn" type="button">编辑</button>
          </div>
          <div class="info-list">
            <div class="info-row"><span class="info-k">服务商</span><span class="info-v">${esc(n.provider || '—')}</span></div>
            <div class="info-row"><span class="info-k">位置</span><span class="info-v">${esc(n.region || '—')}</span></div>
            <div class="info-row"><span class="info-k">分组</span><span class="info-v">${esc(n.group || '—')}</span></div>
            <div class="info-row"><span class="info-k">到期日</span><span class="info-v mono">${esc(n.expireAt || '—')}</span></div>
            <div class="info-row"><span class="info-k">年付价</span><span class="info-v mono">${n.price ? (n.currency === 'CNY' ? '¥' : '$') + n.price : '—'}</span></div>
          </div>
        </div>

        <div class="panel panel-pad">
          <div class="panel-head">
            <span class="panel-title">流量配额</span>
            <div class="row-actions">
              <button class="btn tiny" id="quotaAdjustBtn" type="button" title="把本周期已用量直接改成服务商面板上的数字">校正本期</button>
              <button class="btn tiny" id="quotaEditBtn" type="button">设置</button>
            </div>
          </div>
          ${quotaBytes > 0 ? `
            <div style="display:flex;justify-content:space-between;font-size:12px;margin-bottom:6px">
              <span class="mono">${bytes(trafficUsed)}</span>
              <span class="field-hint">配额 ${traffic.quotaGb} GB · 每月 ${traffic.resetDay || 1} 日重置</span>
            </div>
            <div class="meter"><i style="width:${Math.min(100, quotaPctVal).toFixed(1)}%;background:${quotaPctVal >= 100 ? 'var(--crit)' : quotaPctVal >= 90 ? 'var(--warn)' : 'var(--ok)'}"></i></div>
            <div class="field-hint" style="margin-top:6px">已用 ${quotaPctVal.toFixed(1)}% · 口径 ${esc(trafficMode)}${traffic.cycleStart ? ' · 周期起点 ' + clock(traffic.cycleStart) : ''}</div>
            ${traffic.incomplete ? '<div class="field-hint" style="color:var(--warn);margin-top:4px">⚠ 本周期经历过网卡集合变更（如 WARP/多 WAN 切换），计数可能不准——用「校正本期」按服务商面板数字修正。</div>' : ''}
          ` : `
            <div class="field-hint">未设置月度配额。可在 <a href="#/settings">设置 → 节点管理 → 编辑</a> 中填写，超过阈值时由「流量配额使用率」规则告警。</div>
          `}
        </div>

        <div class="panel panel-pad">
          <div class="panel-head"><span class="panel-title">采集明细</span></div>
          <div class="info-list">
            <div class="info-row"><span class="info-k">磁盘 I/O（累计）</span><span class="info-v" style="font-size:11.5px">读 <span class="mono">${bytes(last.diskReadBytes || 0)}</span><br>写 <span class="mono">${bytes(last.diskWriteBytes || 0)}</span></span></div>
          </div>
          <div class="sub-title" style="margin:12px 0 6px">挂载点</div>
          ${mounts.length ? mounts.map((m) => `
            <div class="mount-row">
              <span class="mount-path mono" title="${esc(m.path)}">${esc(m.path)}</span>
              <div class="meter" style="flex:1"><i style="width:${Math.min(100, pct(m.used, m.total)).toFixed(1)}%;background:${pct(m.used, m.total) >= 90 ? 'var(--crit)' : pct(m.used, m.total) >= 80 ? 'var(--warn)' : 'var(--ok)'}"></i></div>
              <span class="mount-val mono">${pct(m.used, m.total).toFixed(0)}%</span>
            </div>`).join('') : '<div class="field-hint">Agent 尚未上报挂载点（需 v1.10+）</div>'}
          <div class="sub-title" style="margin:12px 0 6px">网卡</div>
          ${nics.length ? nics.map((x) => `
            <div class="info-row"><span class="info-k mono">${esc(x.name)}</span><span class="info-v mono" style="font-size:11.5px">↓ ${bytes(x.rx)} / ↑ ${bytes(x.tx)}</span></div>`).join('') : '<div class="field-hint">Agent 尚未上报网卡明细</div>'}
        </div>
      </div>
    </div>`;

  $('#backToNodes').onclick = () => { location.hash = '#/nodes'; };
  const openDetailEdit = async () => {
    try {
      const an = await adminApi('/api/admin/nodes/' + encodeURIComponent(id));
      showNodeEdit(an, render);
    } catch (e) { toast('读取资产信息失败：' + e.message, 'err'); }
  };
  const nodeEditBtn = $('#nodeEditBtn');
  const assetEditBtn = $('#assetEditBtn');
  const quotaAdjustBtn = $('#quotaAdjustBtn');
  if (quotaAdjustBtn) {
    quotaAdjustBtn.onclick = () => openTrafficAdjustDialog(n.id, trafficUsed, () => viewNodeDetail(id));
  }
  const quotaEditBtn = $('#quotaEditBtn');
  if (nodeEditBtn) nodeEditBtn.onclick = openDetailEdit;
  if (assetEditBtn) assetEditBtn.onclick = openDetailEdit;
  if (quotaEditBtn) quotaEditBtn.onclick = openDetailEdit;
  const nodeAgentBtn = $('#nodeAgentBtn');
  if (nodeAgentBtn) nodeAgentBtn.onclick = async () => {
    try {
      const an = await adminApi('/api/admin/nodes/' + encodeURIComponent(id));
      showAgentConfig(an);
    } catch (e) { toast('读取接入信息失败：' + e.message, 'err'); }
  };
  const prevBtn = $('#prevNode');
  if (prevBtn) prevBtn.onclick = () => { if (prevNode) location.hash = '#/nodes/' + prevNode.id + '?range=' + state.range; };
  const nextBtn = $('#nextNode');
  if (nextBtn) nextBtn.onclick = () => { if (nextNode) location.hash = '#/nodes/' + nextNode.id + '?range=' + state.range; };
  const copyBtn = $('#nodeCopyBtn');
  if (copyBtn) copyBtn.onclick = async () => {
    const cpuVal = hasData ? (last.cpuPct || 0).toFixed(1) + '%' : '—';
    const memVal = hasData ? pct(memUsed, memTotal).toFixed(1) + '%' : '—';
    const diskVal = hasData ? pct(diskUsed, diskTotal).toFixed(1) + '%' : '—';
    const loadVal = hasData ? `${(last.load1 || 0).toFixed(2)} / ${(last.load5 || 0).toFixed(2)} / ${(last.load15 || 0).toFixed(2)}` : '—';
    const lines = [
      `【${n.name}】${st.text} · 24h 可用率 ${detail.uptime24 != null ? detail.uptime24.toFixed(1) + '%' : '—'}`,
      `主机：${host.hostname || '—'} · ${host.os || '—'} · ${host.arch || '—'} · ${host.cores || '?'} 核`,
      `CPU ${cpuVal} · 内存 ${memVal} · 磁盘 ${diskVal}`,
      `负载 ${loadVal} · 运行 ${hasData ? dur(last.uptime) : '—'}`,
      `网络 ↓ ${rate(last.netRx)}/s ↑ ${rate(last.netTx)}/s`,
      `Agent ${n.agentVersion || '—'} · 最后上报 ${ago(detail.lastSeen)}`,
      `服务商 ${n.provider || '—'} · 位置 ${n.region || '—'} · 分组 ${n.group || '—'}`,
      `到期 ${n.expireAt || '—'} · 年付 ${n.price ? (n.currency === 'CNY' ? '¥' : '$') + n.price : '—'}`,
    ];
    const ok = await copyText(lines.join('\n'));
    if (ok) toast('节点摘要已复制');
    else toast('复制失败，请手动选择文本', 'err');
  };
  $$('.range-tabs button', $('#view')).forEach((b) => {
    b.onclick = () => {
      state.range = b.dataset.range;
      state.detailCache = null;
      // replaceState 不触发 hashchange，手动重渲染，避免整页闪回概览。
      history.replaceState(null, '', '#/nodes/' + encodeURIComponent(id) + '?range=' + state.range);
      viewNodeDetail(id);
    };
  });
}

// 同目标并排：把同一主机的多条探测放在一起，便于判断是目标整体异常，
// 还是某一条协议/路径的探测单独失败。探测统一由控制服务器发起，
// “归属节点”只是展示归组，不能当成该节点的本地网络视角。
function serviceCompareSection(data) {
  if (!data) return '';
  const groups = data.groups || [];
  const note = data.note || '所有探测均由控制服务器发起。';
  let body;
  if (!groups.length) {
    const singles = Number(data.singles || 0);
    const hint = singles > 0
      ? `当前 ${singles} 个目标只有一条探测。为同一主机配置多条探测（例如 HTTPS + TCP）后，这里会并排展示。`
      : '为同一主机配置多条探测（例如 HTTPS + TCP）后，这里会并排展示。';
    body = `<div class="empty" style="padding:28px 20px">${esc(hint)}</div>`;
  } else {
    body = `<div class="table-wrap"><table class="services-table svc-compare-table">
      <thead><tr><th>探测</th><th>归属节点</th><th>类型</th><th>目标</th><th>状态</th>
        <th class="num" title="最近一次探测的往返延迟">延迟</th><th class="num" title="ICMP 探测的丢包率与延迟抖动">丢包 / 抖动</th><th class="num" title="HTTPS 目标的证书剩余天数">证书</th><th class="num" title="最近 24 小时探测成功率">24 小时可用率</th></tr></thead>
      <tbody>${groups.map(serviceCompareGroupHTML).join('')}</tbody>
    </table></div>`;
  }
  return `<section class="section">
    <div class="section-head">
      <h2 class="section-title">同目标并排</h2>
      ${sectionActions(`${groups.length} 组 · 同一主机的多条探测`)}
    </div>
    <p class="section-note">${esc(note)}</p>
    ${body}
  </section>`;
}

function serviceCompareGroupHTML(g) {
  const verdicts = {
    up: ['ok', '全部正常'],
    partial: ['warn', '部分异常'],
    down: ['crit', '全部失败'],
    unknown: ['mute', '暂无数据'],
    disabled: ['mute', '全部停用'],
  };
  const [tone, label] = verdicts[g.verdict] || ['mute', g.verdict];
  const counts = !g.enabled
    ? `共 ${g.total} 条探测`
    : g.probed
      ? `${g.failing}/${g.probed} 条失败 · 共 ${g.total} 条探测`
      : `等待首次探测 · 共 ${g.total} 条探测`;
  const head = `<tr class="svc-group-row"><td colspan="9">
    <span class="mono strong">${esc(g.host)}</span>
    <span class="badge ${tone}">${esc(label)}</span>
    <span class="svc-group-meta">${counts}</span>
  </td></tr>`;
  return head + (g.services || []).map((s) => {
    const enabled = s.enabled !== false;
    let status;
    if (!enabled) status = '<span class="badge mute">已停用</span>';
    else if (!s.has) status = '<span class="badge mute">无数据</span>';
    else status = `<span class="dot ${s.ok ? 'ok' : 'crit'}"></span>${s.ok ? '正常' : '失败'}`;
    const cert = s.certDays === 0
      ? '<span class="badge crit">已过期</span>'
      : s.certDays > 0
        ? `<span class="badge ${s.certDays <= 14 ? 'crit' : s.certDays <= 30 ? 'warn' : 'mute'}">${s.certDays} 天</span>`
        : '—';
    const loss = Number(s.lossPct || 0);
    const jitter = Number(s.jitterMs || 0);
    const up = Number(s.uptime || 0);
    const node = s.nodeId ? esc(s.nodeName || s.nodeId) : '<span class="text-faint">未关联</span>';
    const upCell = s.has || up > 0
      ? `<span class="badge ${up >= 99 ? 'ok' : up >= 95 ? 'warn' : 'crit'}">${up.toFixed(1)}%</span>`
      : '—';
    return `<tr>
      <td class="strong">${esc(s.name)}</td>
      <td>${node}</td>
      <td><span class="badge mute">${esc(String(s.type || '').toUpperCase())}</span></td>
      <td class="mono target-cell" title="${esc(s.target)}">${esc(s.target)}</td>
      <td>${status}</td>
      <td class="num mono">${s.has && s.latencyMs ? s.latencyMs + 'ms' : '—'}</td>
      <td class="num mono cell-compact">${loss > 0 ? `<span class="${loss >= 10 ? 'text-crit' : 'text-warn'}">${loss.toFixed(1)}%</span>` : '0%'} / ${jitter ? jitter.toFixed(0) + 'ms' : '—'}</td>
      <td class="num">${cert}</td>
      <td class="num">${upCell}</td>
    </tr>`;
  }).join('');
}

async function viewServices() {
  const [svcs, nodes, svcCompare] = await Promise.all([
    api('/api/services'),
    api('/api/nodes').catch(() => []),
    api('/api/services/compare').catch(() => null),
  ]);
  state.data.nodes = nodes;
  const list = svcs.filter((s) => matchesSearch([s.name, s.target, s.type, s.enabled === false ? '已停用' : '']));
  $('#view').innerHTML = `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">服务探测</h2>
        ${sectionActions(`${list.length}/${svcs.length} 个目标 · 由服务端主动探测`, '<button class="btn sm primary" id="svcAdd">＋ 新增探测</button>')}
      </div>
      <div class="table-wrap"><table class="services-table" data-page-size="10">
        <thead><tr><th>状态</th><th>名称</th><th>类型</th><th>目标</th>
          <th class="num" title="最近一次探测的往返延迟">延迟</th><th class="num" title="ICMP 探测的丢包率与延迟抖动">丢包 / 抖动</th><th class="num" title="HTTPS 目标的证书剩余天数">证书</th><th class="num" title="最近 24 小时探测成功率">24 小时可用率</th><th>最近 2 小时</th><th class="num">操作</th></tr></thead>
        <tbody>${list.map((s) => {
          const spark = (s.spark || []).slice(-120).map((p) => p[1]);
          const up = s.uptime || 0;
          const cd = s.certDays;
          const enabled = s.enabled !== false;
          const certCell = cd === 0
            ? '<span class="badge crit">已过期</span>'
            : cd > 0
              ? `<span class="badge ${cd <= 14 ? 'crit' : cd <= 30 ? 'warn' : 'mute'}">${cd} 天</span>`
              : '—';
          const loss = Number(s.lossPct || 0);
          const jitter = Number(s.jitterMs || 0);
          return `<tr>
            <td><span class="dot ${!enabled ? 'off' : s.ok ? 'ok' : 'crit'}"></span>${enabled ? '' : ' <span class="badge mute">已停用</span>'}</td>
            <td class="strong">${esc(s.name)}</td>
            <td><span class="badge mute">${esc(s.type.toUpperCase())}</span></td>
            <td class="mono target-cell" title="${esc(s.target)}">${esc(s.target)}</td>
            <td class="num mono">${s.latencyMs ? s.latencyMs + 'ms' : '—'}</td>
            <td class="num mono cell-compact">${loss > 0 ? `<span class="${loss >= 10 ? 'text-crit' : 'text-warn'}">${loss.toFixed(1)}%</span>` : '0%'} / ${jitter ? jitter.toFixed(0) + 'ms' : '—'}</td>
            <td class="num">${certCell}</td>
            <td class="num"><span class="badge ${up >= 99 ? 'ok' : up >= 95 ? 'warn' : 'crit'}">${up.toFixed(1)}%</span></td>
            <td class="spark-cell"><div class="spark-box">${sparkline(spark, s.ok ? 'var(--ok)' : 'var(--crit)', 22)}</div></td>
            <td class="num"><div class="row-actions">
              <button class="btn tiny" data-svc-test="${esc(s.id)}">检测</button>
              <button class="btn tiny" data-svc-edit="${esc(s.id)}">编辑</button>
              <button class="btn tiny" data-svc-toggle="${esc(s.id)}">${enabled ? '停用' : '启用'}</button>
              <button class="btn tiny danger" data-svc-del="${esc(s.id)}">删除</button>
            </div></td>
          </tr>`;
        }).join('') || `<tr><td colspan="10"><div class="empty">尚未配置服务探测目标</div></td></tr>`}</tbody>
      </table></div>
    </section>
    ${serviceCompareSection(svcCompare)}`;

  $('#svcAdd').onclick = () => openServiceDialog(null, nodes);
  $$('[data-svc-test]').forEach((b) => {
    b.onclick = () => {
      const svc = svcs.find((x) => x.id === b.dataset.svcTest);
      if (svc) testServiceNow(svc, b);
    };
  });
  $$('[data-svc-edit]').forEach((b) => {
    b.onclick = () => {
      const svc = svcs.find((x) => x.id === b.dataset.svcEdit);
      if (svc) openServiceDialog(svc, nodes);
    };
  });
  $$('[data-svc-toggle]').forEach((b) => {
    b.onclick = async () => {
      const svc = svcs.find((x) => x.id === b.dataset.svcToggle);
      if (!svc) return;
      b.disabled = true;
      try {
        await adminApi('/api/admin/services/' + encodeURIComponent(svc.id), {
          method: 'PUT',
          body: JSON.stringify({
            name: svc.name, type: svc.type, target: svc.target,
            interval: svc.interval, nodeId: svc.nodeId || '', enabled: svc.enabled === false,
          }),
        });
        toast(svc.enabled === false ? '探测已启用' : '探测已停用');
        viewServices();
      } catch (e) {
        toast('操作失败：' + e.message, 'err');
        b.disabled = false;
      }
    };
  });
  $$('[data-svc-del]').forEach((b) => {
    b.onclick = async () => {
      if (!(await modalConfirm('删除探测目标', '确认删除该探测目标？其历史探测数据会一并删除。',
        { danger: true, okText: '删除' }))) return;
      b.disabled = true;
      try {
        await adminApi('/api/admin/services/' + encodeURIComponent(b.dataset.svcDel), { method: 'DELETE' });
        toast('探测目标已删除');
        viewServices();
      } catch (e) {
        toast('删除失败：' + e.message, 'err');
        b.disabled = false;
      }
    };
  });
}

// openServiceDialog 新增或编辑探测目标。后端用 PUT 更新，保留探测历史。
async function testServiceNow(svc, btn) {
  const label = btn.textContent;
  btn.disabled = true;
  btn.textContent = '检测中…';
  try {
    const r = await adminApi('/api/admin/services/' + encodeURIComponent(svc.id) + '/test', { method: 'POST' });
    if (!r.ok) {
      toast(`${svc.name} 检测失败：${r.detail || '目标不可达'}`, 'err');
      return;
    }
    const bits = ['连通正常'];
    if (r.latencyMs) bits.push(r.latencyMs + ' ms');
    if (r.status) bits.push('HTTP ' + r.status);
    if (r.certDays >= 0) bits.push(r.certDays === 0 ? '证书已过期' : '证书 ' + r.certDays + ' 天');
    toast(`${svc.name}：${bits.join(' · ')}`);
  } catch (e) {
    toast('检测失败：' + e.message, 'err');
  } finally {
    btn.disabled = false;
    btn.textContent = label;
  }
}

function openServiceDialog(svc, nodes, after) {
  const editing = !!svc;
  const dlg = openDialog(editing ? '编辑探测目标' : '新增探测目标', `
    <p class="modal-sub">HTTP/HTTPS 会同时校验证书；TCP/DNS 只判断连通性；ICMP 测的是「机器在不在线 + 往返延迟」（仅 IPv4，需控制端内核允许非特权 ping）。间隔最小 10 秒。</p>
    <form id="svcDialogForm" class="form-grid">
      ${field('名称', 'name', svc?.name || '', { required: true, placeholder: '例如 example.com' })}
      ${selectField('类型', 'type', svc?.type || 'https', [{ v: 'https', t: 'HTTPS' }, { v: 'http', t: 'HTTP' }, { v: 'tcp', t: 'TCP' }, { v: 'dns', t: 'DNS' }, { v: 'icmp', t: 'ICMP（ping）' }])}
      ${field('目标', 'target', svc?.target || '', { required: true, placeholder: 'https://example.com / 1.2.3.4:3306 / 1.1.1.1（ICMP 只填主机）' })}
      ${field('间隔（秒）', 'interval', String(svc?.interval || 60), { type: 'number', hint: '10–86400 秒' })}
      ${selectField('归属节点', 'nodeId', svc?.nodeId || '', [{ v: '', t: '不关联' }].concat(nodes.map((n) => ({ v: n.id, t: n.name }))))}
      ${checkbox('启用探测', 'enabled', editing ? svc.enabled !== false : true)}
      <div class="form-actions">
        <button class="btn" type="button" data-close>取消</button>
        <button class="btn primary" type="submit">${editing ? '保存修改' : '添加'}</button>
      </div>
    </form>`);
  const form = dlg.querySelector('#svcDialogForm');
  form.onsubmit = async (e) => {
    e.preventDefault();
    const d = formData(form);
    const body = JSON.stringify({
      name: d.name, type: d.type, target: d.target,
      interval: Number(d.interval) || 60, nodeId: d.nodeId || '', enabled: !!d.enabled,
    });
    const btn = form.querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      if (editing) {
        await adminApi('/api/admin/services/' + encodeURIComponent(svc.id), { method: 'PUT', body });
        toast('探测目标已更新');
      } else {
        await adminApi('/api/admin/services', { method: 'POST', body });
        toast('探测目标已添加');
      }
      dlg.close();
      (after || viewServices)();
    } catch (err) {
      toast('保存失败：' + err.message, 'err');
      btn.disabled = false;
    }
  };
  form.querySelector('input[name=name]').focus();
}

async function viewAlerts() {
  const canManage = state.authenticated;
  const [d, rules, meta, nodes] = await Promise.all([
    api('/api/alerts'),
    api('/api/rules').catch(() => []),
    adminApi('/api/admin/rule-meta').catch(() => ({ metrics: [], severities: [], durations: [] })),
    canManage ? adminApi('/api/admin/nodes').catch(() => []) : Promise.resolve([]),
  ]);
  const items = d.items || [];
  setAlertBadge(d.active || 0);
  const rulesAll = rules || [];
  const filteredRules = rulesAll.filter((r) => matchesSearch([r.name, r.metric, r.severity, ...(r.groups || [])]));
  const filteredItems = items.filter((a) => matchesSearch([a.ruleName, a.nodeName, a.message, a.severity]));

  const metricMap = {};
  (meta.metrics || []).forEach((m) => { metricMap[m.value] = m; });
  const sevLabel = { critical: '严重', warn: '警告', info: '信息' };

  const silences = d.silences || [];
  const silenceMap = {};
  silences.forEach((s) => { silenceMap[s.scope] = s; });
  const globalSil = silenceMap.global;
  const nq = d.notify || {};
  const silLabel = (s) => `至 ${clock(s.until)}${s.reason ? ' · ' + s.reason : ''}`;

  // 告警记录是卡片列表而不是表格，通用表格分页覆盖不到，单独分页。
  const alertPageSize = 10;
  const alertPages = Math.max(1, Math.ceil(filteredItems.length / alertPageSize));
  const alertPage = Math.max(1, Math.min(alertPages, state.alertPage || 1));
  state.alertPage = alertPage;
  const pageItems = filteredItems.slice((alertPage - 1) * alertPageSize, alertPage * alertPageSize);

  $('#view').innerHTML = `
    <div class="kpis" style="margin-bottom:20px">
      ${kpi('活跃告警', d.active, '', d.active ? '需要处理' : '全部恢复')}
      ${kpi('启用规则', (rules || []).filter((r) => r.enabled).length, '', `共 ${(rules || []).length} 条`)}
      ${kpi('历史记录', items.length, '', '最近 200 条')}
      ${kpi('待投递通知', nq.pending || 0, '条', `失败 ${nq.failed || 0} · 静默拦截 ${nq.suppressed || 0}`)}
    </div>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">告警规则</h2>
        ${sectionActions(`${filteredRules.length}/${rulesAll.length} 条 · 阈值、持续时间与级别均可调整`, '<button class="btn sm" id="ruleTplBtn">规则模板</button> <button class="btn sm primary" id="addRuleBtn">＋ 新增规则</button>')}
      </div>
      <div class="table-wrap"><table class="alerts-table" data-page-size="10">
        <thead><tr><th>规则</th><th>条件</th><th class="num">持续</th><th>级别</th><th class="num">启用</th><th class="num">操作</th></tr></thead>
        <tbody>${filteredRules.map((r) => {
          const m = metricMap[r.metric];
          const unit = m ? m.unit : '';
          const cond = r.metric === 'offline'
            ? `超过 ${r.duration > 0 ? durText(r.duration) : '即刻'} 未上报`
            : `${esc(m ? m.label : r.metric)} ${r.op === 'lt' ? '低于' : '高于'} ${r.threshold}${unit}`;
          const scope = ruleScopeText(r, nodes, meta.services);
          const sil = silenceMap[r.id];
          return `<tr>
            <td class="strong">${esc(r.name)}${sil ? ` <span class="badge warn" title="${esc(silLabel(sil))}">通知静默中</span>` : ''}</td>
            <td style="font-size:12px">${cond}${scope ? `<div class="field-hint">${esc(scope)}</div>` : ''}</td>
            <td class="num">${durText(r.duration)}</td>
            <td><span class="badge ${r.severity === 'critical' ? 'crit' : r.severity === 'warn' ? 'warn' : 'mute'}">${sevLabel[r.severity] || r.severity}</span></td>
            <td class="num">
              <label class="switch"><input type="checkbox" data-rule-toggle="${esc(r.id)}"${r.enabled ? ' checked' : ''}><span></span></label>
            </td>
            <td class="num" style="white-space:nowrap">
              <button class="btn tiny" data-rule-silence="${esc(r.id)}">${sil ? '静默设置' : '静默'}</button>
              <button class="btn tiny" data-rule-edit="${esc(r.id)}">编辑</button>
              <button class="btn tiny danger" data-rule-del="${esc(r.id)}">删除</button>
            </td>
          </tr>`;
        }).join('') || `<tr><td colspan="6"><div class="empty">暂无规则</div></td></tr>`}</tbody>
      </table></div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">告警记录</h2>
        ${sectionActions(`${filteredItems.length}/${items.length} 条 · 最近 200 条`)}
      </div>
      <div class="panel">
        ${pageItems.map((a) => {
          const active = !a.resolvedAt;
          return `<div class="alert-item">
            <div class="alert-bar ${a.severity}"></div>
            <div class="alert-main">
              <div class="alert-title">
                ${esc(a.ruleName)}
                <span class="badge ${active ? (a.severity === 'critical' ? 'crit' : 'warn') : 'mute'}">${active ? '进行中' : '已恢复'}</span>
                ${a.nodeId
                  ? `<a class="alert-link" href="#/nodes/${encodeURIComponent(a.nodeId)}">${esc(a.nodeName)}</a>`
                  : `<span class="alert-node">${esc(a.nodeName)}</span>`}
                ${a.serviceName ? `<span class="alert-node">· ${esc(a.serviceName)}</span>` : ''}
              </div>
              <div class="alert-msg">${esc(a.message)}</div>
              <div class="alert-time">${clock(a.startedAt)} 开始${a.resolvedAt ? ` · ${clock(a.resolvedAt)} 恢复` : ''}</div>
            </div>
          </div>`;
        }).join('') || `<div class="empty">${items.length ? '没有匹配的告警记录' : '暂无告警记录'}</div>`}
        ${alertPages > 1 ? `<div class="table-pager" style="padding:0 14px 14px">
          <button class="btn tiny" data-alert-page="-1" ${alertPage <= 1 ? 'disabled' : ''}>上一页</button>
          <span class="pager-info">第 <b class="mono">${alertPage}</b> / ${alertPages} 页 · 共 ${filteredItems.length} 条</span>
          <button class="btn tiny" data-alert-page="1" ${alertPage >= alertPages ? 'disabled' : ''}>下一页</button>
        </div>` : ''}
      </div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">通知投递</h2>
        <span class="section-meta">告警始终记录；静默仅拦截通知</span>
      </div>
      <div class="panel panel-pad" style="display:flex;gap:14px;align-items:center;flex-wrap:wrap">
        <span class="badge ${globalSil ? 'warn' : 'ok'}">${globalSil ? '全局静默 ' + esc(silLabel(globalSil)) : '全局通知正常'}</span>
        <span class="section-meta">队列：待投递 ${nq.pending || 0} · 已送达 ${nq.sent || 0} · 失败 ${nq.failed || 0} · 重试 ${nq.retries || 0}</span>
        <span class="section-meta">${nq.lastSuccess ? '最近成功 ' + clock(nq.lastSuccess) : '尚无成功记录'}</span>
        ${canManage ? `<div style="margin-left:auto;display:flex;gap:8px">
          <button class="btn tiny" id="globalSilenceBtn">${globalSil ? '调整全局静默' : '全局静默'}</button>
          ${globalSil ? '<button class="btn tiny danger" id="globalUnsilenceBtn">取消静默</button>' : ''}
        </div>` : ''}
      </div>
    </section>`;

  // ---- 事件绑定 ----
  $$('[data-rule-toggle]').forEach((el) => {
    el.onchange = async () => {
      if (!canManage) { toast('当前会话没有管理权限', 'err'); el.checked = !el.checked; return; }
      try {
        await adminApi('/api/admin/rules/' + el.dataset.ruleToggle, {
          method: 'PUT', body: JSON.stringify({ enabled: el.checked }),
        });
        toast(el.checked ? '规则已启用' : '规则已停用');
      } catch (e) {
        el.checked = !el.checked;
        toast('操作失败：' + e.message, 'err');
      }
    };
  });

  $$('[data-rule-del]').forEach((b) => {
    b.onclick = async () => {
      if (!canManage) { toast('当前会话没有管理权限', 'err'); return; }
      if (!(await modalConfirm('删除告警规则', '确认删除该规则？其进行中的告警会一并关闭。',
        { danger: true, okText: '删除' }))) return;
      try {
        await adminApi('/api/admin/rules/' + b.dataset.ruleDel, { method: 'DELETE' });
        toast('规则已删除'); viewAlerts();
      } catch (e) { toast('删除失败：' + e.message, 'err'); }
    };
  });

  $$('[data-rule-edit]').forEach((b) => {
    b.onclick = () => {
      if (!canManage) { toast('当前会话没有管理权限', 'err'); return; }
      const r = (rules || []).find((x) => x.id === b.dataset.ruleEdit);
      if (r) openRuleDialog(r, meta, nodes);
    };
  });

  $('#addRuleBtn').onclick = () => {
    if (!canManage) { toast('请先在「设置」页配置管理令牌', 'err'); return; }
    openRuleDialog(null, meta, nodes);
  };

  const ruleTplBtn = $('#ruleTplBtn');
  if (ruleTplBtn) {
    ruleTplBtn.onclick = () => {
      if (!canManage) { toast('请先在「设置」页配置管理令牌', 'err'); return; }
      openRuleTemplateDialog(meta, rules);
    };
  }

  const globalSilenceBtn = $('#globalSilenceBtn');
  if (globalSilenceBtn) {
    globalSilenceBtn.onclick = () => {
      if (!canManage) { toast('当前会话没有管理权限', 'err'); return; }
      openSilenceDialog('global', '全局通知', globalSil);
    };
  }
  const globalUnsilenceBtn = $('#globalUnsilenceBtn');
  if (globalUnsilenceBtn) {
    globalUnsilenceBtn.onclick = async () => {
      if (!canManage) { toast('当前会话没有管理权限', 'err'); return; }
      try {
        await adminApi('/api/admin/silences/global', { method: 'DELETE' });
        toast('全局静默已取消');
        viewAlerts();
      } catch (e) { toast('取消失败：' + e.message, 'err'); }
    };
  }
  $$('[data-rule-silence]').forEach((b) => {
    b.onclick = () => {
      if (!canManage) { toast('当前会话没有管理权限', 'err'); return; }
      const r = (rules || []).find((x) => x.id === b.dataset.ruleSilence);
      if (r) openSilenceDialog(r.id, r.name, silenceMap[r.id]);
    };
  });
  $$('[data-alert-page]').forEach((b) => {
    b.onclick = () => {
      state.alertPage = alertPage + Number(b.dataset.alertPage);
      viewAlerts();
    };
  });
}

// openSilenceDialog 设置或取消某个范围（global / 规则）的通知静默。
function openSilenceDialog(scope, label, current) {
  const presets = [
    { v: '60', t: '1 小时' },
    { v: '360', t: '6 小时' },
    { v: '1440', t: '24 小时' },
    { v: '10080', t: '7 天' },
  ];
  const dlg = document.createElement('div');
  dlg.className = 'modal-mask';
  dlg.innerHTML = `<div class="modal">
    <div class="modal-title">${current ? '调整通知静默' : '通知静默'} · ${esc(label)}</div>
    <p class="modal-sub">静默期间告警仍会正常记录与恢复，只是不再外发通知；到期后自动恢复投递。</p>
    ${current ? `<p class="modal-sub" style="color:var(--warn)">当前静默至 ${clock(current.until)}${current.reason ? ' · ' + esc(current.reason) : ''}</p>` : ''}
    <form id="silenceForm" class="form-grid">
      ${selectField('静默时长', 'minutes', '60', presets)}
      ${field('备注', 'reason', '', { placeholder: '例如：机房维护、迁移窗口' })}
      <div class="form-actions">
        <button class="btn primary" type="submit">${current ? '重新静默' : '开始静默'}</button>
        ${current ? '<button class="btn danger" type="button" id="silenceRemove">取消静默</button>' : ''}
        <button class="btn" type="button" id="silenceCancel">关闭</button>
      </div>
    </form>
  </div>`;
  document.body.appendChild(dlg);

  const close = () => dlg.remove();
  dlg.onclick = (e) => { if (e.target === dlg) close(); };
  $('#silenceCancel', dlg).onclick = close;

  const path = '/api/admin/silences/' + encodeURIComponent(scope);
  const removeBtn = $('#silenceRemove', dlg);
  if (removeBtn) {
    removeBtn.onclick = async () => {
      removeBtn.disabled = true;
      try {
        await adminApi(path, { method: 'DELETE' });
        toast('静默已取消');
        close();
        viewAlerts();
      } catch (e) {
        toast('取消失败：' + e.message, 'err');
        removeBtn.disabled = false;
      }
    };
  }

  $('#silenceForm', dlg).onsubmit = async (e) => {
    e.preventDefault();
    const d = formData(e.target);
    const btn = e.target.querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      await adminApi(path, {
        method: 'PUT',
        body: JSON.stringify({ minutes: Number(d.minutes) || 60, reason: d.reason || '' }),
      });
      toast('静默已生效');
      close();
      viewAlerts();
    } catch (err) {
      toast('设置失败：' + err.message, 'err');
      btn.disabled = false;
    }
  };
}

// ruleScopeText 把规则的定向范围转成可读文案。
function ruleScopeText(r, nodes, services) {
  const ids = r.nodeIds || [], groups = r.groups || [], svcIDs = r.serviceIds || [];
  const parts = [];
  if (ids.length || groups.length) {
    const nameOf = {};
    (nodes || []).forEach((n) => { nameOf[n.id] = n.name; });
    const sub = [];
    if (ids.length) sub.push('节点：' + ids.map((id) => nameOf[id] || id).join('、'));
    if (groups.length) sub.push('分组：' + groups.join('、'));
    parts.push(sub.join('；'));
  }
  if (svcIDs.length) {
    const svcName = {};
    (services || []).forEach((s) => { svcName[s.id] = s.name; });
    parts.push('服务：' + svcIDs.map((id) => svcName[id] || id).join('、'));
  }
  return parts.join('；');
}

function durText(sec) {
  if (!sec || sec <= 0) return '—';
  if (sec < 60) return sec + ' 秒';
  if (sec < 3600) return (sec / 60) + ' 分钟';
  return (sec / 3600) + ' 小时';
}

// openRuleDialog 打开规则新建/编辑弹窗
function openRuleDialog(rule, meta, nodes) {
  const editing = !!rule;
  const metrics = meta.metrics || [];
  const severities = meta.severities || [];
  const r = rule || { name: '', metric: 'cpu', op: 'gt', threshold: 90, duration: 300, severity: 'warn', enabled: true, nodeIds: [], groups: [], serviceIds: [] };
  const scopedIDs = r.nodeIds || [];
  const knownIDs = new Set((nodes || []).map((n) => n.id));
  const nodeOptions = (nodes || []).map((n) =>
    `<option value="${esc(n.id)}"${scopedIDs.includes(n.id) ? ' selected' : ''}>${esc(n.name)}</option>`)
    .concat(scopedIDs.filter((id) => !knownIDs.has(id)).map((id) =>
      `<option value="${esc(id)}" selected>${esc(id)}（节点已不存在）</option>`)).join('');
  const services = meta.services || [];
  const scopedSvc = r.serviceIds || [];
  const svcOptions = services.map((s) =>
    `<option value="${esc(s.id)}"${scopedSvc.includes(s.id) ? ' selected' : ''}>${esc(s.name)}</option>`).join('');
  // 通知渠道：按 ID 选。留空=全部启用渠道，与旧行为一致。
  const chanList = meta.channelList || [];
  const scopedChan = r.channels || [];
  const knownChan = new Set(chanList.map((c) => c.id));
  const chanOptions = chanList.map((c) =>
    `<option value="${esc(c.id)}"${scopedChan.includes(c.id) ? ' selected' : ''}>${esc(c.name)}（${esc(c.type)}${c.enabled ? '' : '，已停用'}）</option>`)
    .concat(scopedChan.filter((id) => !knownChan.has(id)).map((id) =>
      `<option value="${esc(id)}" selected>${esc(id)}（渠道已不存在）</option>`)).join('');
  const chanField = chanList.length
    ? `<label class="field" id="chanScope">
        <span class="field-label">通知渠道（可多选，留空=全部）</span>
        <select name="channels" multiple size="4">${chanOptions}</select>
        <span class="field-hint">只发到选中的渠道；渠道在「设置 → 通知」里配置</span>
      </label>`
    : `<label class="field" id="chanScope">
        <span class="field-label">通知渠道</span>
        <span class="field-hint">还没有配置通知渠道，规则将发送到全部渠道（当前为空）。
          先到「设置 → 通知」添加 Telegram / 企业微信 / Webhook 等渠道。</span>
      </label>`;

  const dlg = document.createElement('div');
  dlg.className = 'modal-mask';
  dlg.innerHTML = `<div class="modal">
    <div class="modal-title">${editing ? '编辑规则' : '新增规则'}</div>
    <p class="modal-sub">阈值需持续满足指定时长才会触发，避免瞬时抖动误报。</p>
    <form id="ruleForm" class="form-grid">
      ${field('规则名称', 'name', r.name, { required: true, placeholder: '例如 CPU 持续高负载' })}
      ${selectField('监控指标', 'metric', r.metric, metrics.map((m) => ({ v: m.value, t: m.label + (m.unit ? '（' + m.unit + '）' : '') })))}
      <label class="field" id="opWrap">
        <span class="field-label">比较方式</span>
        <select name="op">
          <option value="gt"${r.op === 'gt' ? ' selected' : ''}>高于阈值时触发</option>
          <option value="lt"${r.op === 'lt' ? ' selected' : ''}>低于阈值时触发</option>
        </select>
      </label>
      <label class="field" id="thWrap">
        <span class="field-label">阈值 <span id="thUnit"></span></span>
        <input type="number" name="threshold" value="${r.threshold}" step="any" min="0">
        <span class="field-hint" id="thHint"></span>
      </label>
      ${selectField('持续时间', 'duration', String(r.duration), (meta.durations || []).map((x) => ({ v: String(x.value), t: x.label })))}
      ${selectField('告警级别', 'severity', r.severity, severities.map((x) => ({ v: x.value, t: x.label })))}
      <label class="field" id="nodeScope">
        <span class="field-label">定向节点（可多选，留空=全部）</span>
        <select name="nodeIds" multiple size="4">${nodeOptions}</select>
        <span class="field-hint">按住 Ctrl / Cmd 多选</span>
      </label>
      <label class="field" id="groupScope">
        <span class="field-label">定向分组（逗号分隔，留空=全部）</span>
        <input name="groups" value="${esc((r.groups || []).join(', '))}" placeholder="prod, test">
        <span class="field-hint">节点与分组建「或」关系；两者都留空表示全部节点</span>
      </label>
      <label class="field" id="svcScope" style="display:none">
        <span class="field-label">定向服务（可多选，留空=全部服务）</span>
        <select name="serviceIds" multiple size="4">${svcOptions}</select>
        <span class="field-hint">仅服务类指标生效；服务未配置时保持为空</span>
      </label>
      ${chanField}
      ${checkbox('启用该规则', 'enabled', r.enabled)}
      <div class="form-actions">
        <button class="btn primary" type="submit">${editing ? '保存修改' : '创建规则'}</button>
        <button class="btn" type="button" id="ruleCancel">取消</button>
      </div>
    </form>
  </div>`;
  document.body.appendChild(dlg);

  const close = () => dlg.remove();
  dlg.onclick = (e) => { if (e.target === dlg) close(); };
  $('#ruleCancel', dlg).onclick = close;

  const metricSel = dlg.querySelector('[name=metric]');
  const opSel = dlg.querySelector('[name=op]');
  const thWrap = $('#thWrap', dlg);
  const opWrap = $('#opWrap', dlg);
  const thUnit = $('#thUnit', dlg);
  const thHint = $('#thHint', dlg);
  const thInput = dlg.querySelector('[name=threshold]');
  const svcWrap = $('#svcScope', dlg);
  const nodeWrap = $('#nodeScope', dlg);
  const groupWrap = $('#groupScope', dlg);
  const serviceMetrics = new Set(['service_down', 'latency', 'loss', 'jitter', 'cert_days']);

  // offline / service_down 规则没有阈值与比较符，需隐藏；
  // 服务类指标只显示服务定向，节点/分组定向只对节点指标有意义。
  let firstSync = true;
  function syncMetricFields() {
    const m = (meta.metrics || []).find((x) => x.value === metricSel.value);
    const noVal = !!(m && m.noVal);
    const isSvc = serviceMetrics.has(metricSel.value);
    thWrap.style.display = noVal ? 'none' : '';
    opWrap.style.display = noVal ? 'none' : '';
    svcWrap.style.display = isSvc ? '' : 'none';
    nodeWrap.style.display = isSvc ? 'none' : '';
    groupWrap.style.display = isSvc ? 'none' : '';
    if (m) {
      thUnit.textContent = m.unit ? '(' + m.unit + ')' : '';
      thHint.textContent = m.hint || '';
      if (m.min !== undefined) thInput.min = m.min;
      if (m.max !== undefined) thInput.max = m.max;
      if (m.step !== undefined) thInput.step = m.step;
      if (!firstSync && !noVal && m.op) opSel.value = m.op;
    }
    thInput.disabled = noVal;
    firstSync = false;
  }
  metricSel.onchange = syncMetricFields;
  syncMetricFields();

  $('#ruleForm', dlg).onsubmit = async (e) => {
    e.preventDefault();
    const d = formData(e.target);
    const isOffline = d.metric === 'offline';
    const isSvcDown = d.metric === 'service_down';
    const noVal = isOffline || isSvcDown;
    const nodeSel = e.target.querySelector('[name=nodeIds]');
    const svcSel = e.target.querySelector('[name=serviceIds]');
    const payload = {
      name: d.name,
      metric: d.metric,
      op: isOffline ? 'gt' : d.op,
      duration: Number(d.duration) || 0,
      severity: d.severity,
      enabled: d.enabled,
      nodeIds: nodeSel ? Array.from(nodeSel.selectedOptions).map((o) => o.value) : [],
      groups: String(d.groups || '').split(',').map((s) => s.trim()).filter(Boolean),
      serviceIds: svcSel ? Array.from(svcSel.selectedOptions).map((o) => o.value) : [],
    };
    const chanSel = e.target.querySelector('[name=channels]');
    if (chanSel) payload.channels = Array.from(chanSel.selectedOptions).map((o) => o.value);
    if (!noVal) payload.threshold = Number(d.threshold) || 0;

    const btn = e.target.querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      if (editing) {
        await adminApi('/api/admin/rules/' + rule.id, { method: 'PUT', body: JSON.stringify(payload) });
        toast('规则已更新');
      } else {
        await adminApi('/api/admin/rules', { method: 'POST', body: JSON.stringify(payload) });
        toast('规则已创建');
      }
      close();
      viewAlerts();
    } catch (err) {
      toast('保存失败：' + err.message, 'err');
      btn.disabled = false;
    }
  };
}

// ruleCondText 把一条规则/模板的条件写成一行可读文案，模板列表与规则表共用同一口径。
function ruleCondText(r, metricMap) {
  const m = metricMap ? metricMap[r.metric] : null;
  const unit = m ? m.unit : '';
  if (r.metric === 'offline') return `超过 ${r.duration > 0 ? durText(r.duration) : '即刻'} 未上报`;
  const op = r.op === 'lt' ? '低于' : '高于';
  return `${m ? m.label : r.metric} ${op} ${r.threshold}${unit}`;
}

// openRuleTemplateDialog 规则模板：内置一组常见故障的预设，一键建成正式规则。
// 模板带固定 ID，因此"补齐缺失"是幂等的；已存在的会显示为「已启用」。
function openRuleTemplateDialog(meta, rules) {
  const tpls = (meta && meta.templates) || [];
  const existing = new Set((rules || []).map((r) => r.id));
  const metricMap = {};
  (meta.metrics || []).forEach((m) => { metricMap[m.value] = m; });
  const sevLabel = { critical: '严重', warn: '警告', info: '信息' };
  const missing = tpls.filter((t) => !existing.has(t.id)).length;

  const rows = tpls.map((t) => {
    const on = existing.has(t.id);
    return `<div style="display:flex;gap:12px;align-items:flex-start;padding:11px 0;border-top:1px solid var(--line)">
      <div style="flex:1;min-width:0">
        <div style="display:flex;gap:8px;align-items:center;flex-wrap:wrap">
          <span class="strong">${esc(t.name)}</span>
          <span class="badge ${t.severity === 'critical' ? 'crit' : t.severity === 'warn' ? 'warn' : 'mute'}">${sevLabel[t.severity] || esc(t.severity)}</span>
          <span style="font-size:11.5px;color:var(--fg-3)">${esc(ruleCondText(t, metricMap))}${t.duration > 0 ? ' · 持续 ' + durText(t.duration) : ''}</span>
        </div>
        <div class="field-hint" style="margin-top:3px">${esc(t.desc || '')}</div>
      </div>
      ${on
        ? '<span class="badge ok" style="flex-shrink:0">已启用</span>'
        : `<button class="btn tiny primary" type="button" data-tpl-create="${esc(t.id)}" style="flex-shrink:0">创建</button>`}
    </div>`;
  }).join('');

  const dlg = openDialog('规则模板', `
    <p class="modal-sub">这些是最常见的故障场景，阈值与持续时长都按经验设好；创建后仍可照常编辑阈值、级别与定向范围。模板使用固定 ID，重复套用不会产生同名规则。</p>
    <div style="max-height:52vh;overflow:auto">${rows || '<div class="empty">服务端未下发模板（可能是旧版本）</div>'}</div>
    <div class="form-actions" style="margin-top:12px">
      <button class="btn" type="button" data-close>关闭</button>
      ${missing > 0 ? `<button class="btn primary" type="button" id="tplCreateAll">一键补齐缺失的 ${missing} 条</button>` : ''}
    </div>`);

  // createOne 建一条模板规则；返回 'ok' | 'exists' | 'fail'。
  async function createOne(t) {
    const payload = {
      id: t.id, name: t.name, metric: t.metric, op: t.op,
      duration: t.duration, severity: t.severity, enabled: true,
    };
    // offline / service_down 没有阈值，后端也会忽略；其余必须带上。
    if (t.metric !== 'offline' && t.metric !== 'service_down') payload.threshold = t.threshold;
    try {
      await adminApi('/api/admin/rules', { method: 'POST', body: JSON.stringify(payload) });
      return 'ok';
    } catch (err) {
      // 409 = 同 ID 已存在（例如另一个浏览器刚刚建过），按"已存在"处理即可。
      return /已存在|409/.test(err.message || '') ? 'exists' : 'fail';
    }
  }

  dlg.querySelectorAll('[data-tpl-create]').forEach((b) => {
    b.onclick = async () => {
      if (!state.authenticated) { toast('当前会话没有管理权限', 'err'); return; }
      const t = tpls.find((x) => x.id === b.dataset.tplCreate);
      if (!t) return;
      b.disabled = true;
      const r = await createOne(t);
      if (r === 'fail') { b.disabled = false; toast('创建失败：' + t.name, 'err'); return; }
      toast(r === 'exists' ? '该规则已存在' : '已创建：' + t.name);
      dlg.close();
      viewAlerts();
    };
  });

  const all = dlg.querySelector('#tplCreateAll');
  if (all) {
    all.onclick = async () => {
      if (!state.authenticated) { toast('当前会话没有管理权限', 'err'); return; }
      all.disabled = true;
      let ok = 0, skip = 0, fail = 0;
      for (const t of tpls) {
        if (existing.has(t.id)) { skip++; continue; }
        const r = await createOne(t);
        if (r === 'ok') ok++;
        else if (r === 'exists') skip++;
        else fail++;
      }
      dlg.close();
      viewAlerts();
      const head = ok ? `已创建 ${ok} 条规则` : '没有新建规则';
      toast(fail ? `${head}，${fail} 条失败` : `${head}${skip ? `（${skip} 条已存在）` : ''}`, fail ? 'err' : '');
    };
  }
}

async function viewAssets() {
  const [d, renewals] = await Promise.all([
    api('/api/assets'),
    api('/api/renewals').catch(() => []),
  ]);
  const rowsAll = d.rows || [];
  const rows = rowsAll.filter((r) => matchesSearch([r.name, r.provider, r.region, r.os, r.expireAt]));
  const renewRows = Array.isArray(renewals) ? renewals : [];
  const total = d.monthlyTotal || 0;
  const remainingTotal = d.remainingTotal || 0;

  // 续费日历：资产到期日与人工登记的续费记录合并到同一条时间线。
  const events = [];
  rows.forEach((r) => {
    if (r.expireAt) events.push({ date: r.expireAt, node: r.name, kind: 'expire' });
  });
  renewRows.forEach((x) => {
    events.push({
      date: x.at, node: x.nodeName || x.nodeId, kind: 'renew',
      id: x.id, amount: x.amount, currency: x.currency, note: x.note,
    });
  });
  events.sort((a, b) => a.date.localeCompare(b.date));

  const now = new Date();
  const months = [];
  for (let i = 0; i < 12; i++) {
    const dt = new Date(now.getFullYear(), now.getMonth() + i, 1);
    const key = dt.getFullYear() + '-' + String(dt.getMonth() + 1).padStart(2, '0');
    months.push({
      key,
      label: dt.getFullYear() + ' 年 ' + (dt.getMonth() + 1) + ' 月',
      items: events.filter((e) => e.date.slice(0, 7) === key),
    });
  }
  const upcoming = events.filter((e) => {
    const days = Math.ceil((new Date(e.date + 'T00:00:00') - new Date(now.getFullYear(), now.getMonth(), now.getDate())) / 86400000);
    return days >= 0 && days <= 90;
  });

  const monthHTML = months.map((m) => `
    <div class="renew-month">
      <div class="renew-month-h">${esc(m.label)}<span>${m.items.length || ''}</span></div>
      <div class="renew-items">
        ${m.items.map((e) => e.kind === 'renew'
          ? `<div class="renew-item renew-paid">
              <span class="renew-date mono">${esc(e.date.slice(8))}</span>
              <span class="renew-name">${esc(e.node)}<span class="renew-note">续费 ${e.currency === 'CNY' ? '¥' : '$'}${e.amount}</span></span>
              <button class="icon-btn renew-del" data-del-renew="${esc(e.id)}" title="删除续费记录">×</button>
            </div>`
          : `<div class="renew-item">
              <span class="renew-date mono">${esc(e.date.slice(8))}</span>
              <span class="renew-name">${esc(e.node)}<span class="renew-note">到期</span></span>
            </div>`).join('') || '<div class="renew-empty">—</div>'}
      </div>
    </div>`).join('');

  $('#view').innerHTML = `
    <div class="kpis" style="margin-bottom:20px">
      ${kpi('资产总数', rows.length, '台', '含监控与未监控')}
      ${kpi('月度成本', '$' + total.toFixed(1), '', '按年付折算')}
      ${kpi('剩余价值', '$' + remainingTotal.toFixed(0), '', '未到期资产按剩余天数折算')}
      ${kpi('30 天内到期', rows.filter((r) => r.days > 0 && r.days <= 30).length, '台', '需续费')}
      ${kpi('未来 90 天事项', upcoming.length, '条', '到期与续费')}
    </div>
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">资产台账</h2>
        ${sectionActions(`${rows.length}/${rowsAll.length} 台 · 按到期时间排序`)}
      </div>
      <div class="table-wrap"><table class="assets-table" data-page-size="10">
        <thead><tr><th>节点</th><th>服务商</th><th>位置</th><th>系统</th>
          <th>到期日</th><th class="num" title="距到期剩余天数">剩余</th><th class="num" title="年付折算的续费价格">续费</th><th class="num" title="周期价格按月摊销">月摊</th><th class="num" title="按剩余天数折算的未消耗价值">剩余价值</th><th class="num">操作</th></tr></thead>
        <tbody>${rows.map((r) => {
          const d2 = r.days;
          const cls = !d2 ? 'mute' : d2 <= 7 ? 'crit' : d2 <= 30 ? 'warn' : 'ok';
          return `<tr>
            <td class="strong"><a class="row-link" href="#/nodes/${encodeURIComponent(r.id)}">${esc(r.name)}</a></td>
            <td>${esc(r.provider || '—')}</td>
            <td>${esc(r.region || '—')}</td>
            <td class="os-cell">${esc(r.os || '—')}</td>
            <td>${esc(r.expireAt || '未登记')}</td>
            <td class="num"><span class="badge ${cls}">${d2 ? d2 + ' 天' : '—'}</span></td>
            <td class="num mono">${r.price ? (r.currency === 'CNY' ? '¥' : '$') + r.price : '—'}</td>
            <td class="num mono">$${(r.monthly || 0).toFixed(1)}</td>
            <td class="num mono">${r.remaining ? '$' + r.remaining.toFixed(1) : '—'}</td>
            <td class="num row-actions">
              <button class="btn tiny" data-asset-renew="${esc(r.id)}">续费</button>
              <button class="btn tiny" data-asset-edit="${esc(r.id)}">编辑</button>
            </td>
          </tr>`;
        }).join('') || `<tr><td colspan="10"><div class="empty">${rowsAll.length ? '没有匹配的资产' : '台账为空'}</div></td></tr>`}</tbody>
      </table></div>
    </section>
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">续费日历</h2>
        ${sectionActions('未来 12 个月 · 到期日与续费记录', '<button class="btn sm primary" id="addRenewBtn">＋ 登记续费</button>')}
      </div>
      <div class="renew-grid">${monthHTML}</div>
    </section>`;

  const openRenew = (preselect) => {
    const opts = rowsAll.map((r) => `<option value="${esc(r.id)}"${r.id === preselect ? ' selected' : ''}>${esc(r.name)}</option>`).join('');
    const dlg = openDialog('登记续费', `
      <p class="modal-sub">续费记录只用于日历与成本回溯，不会自动修改节点的到期日。</p>
      <form id="renewForm" class="form-grid">
        <label class="field"><span class="field-label">节点 *</span>
          <select name="nodeId" required>${opts}</select></label>
        ${field('续费日期', 'at', localDate(), { type: 'date', required: true })}
        ${field('金额', 'amount', '', { type: 'number', placeholder: '11.00' })}
        ${selectField('币种', 'currency', 'USD', [{ v: 'USD', t: 'USD ($)' }, { v: 'CNY', t: 'CNY (¥)' }])}
        ${field('备注', 'note', '', { placeholder: '例如：年付续费、优惠码' })}
        <div class="form-actions">
          <button class="btn" type="button" data-close>取消</button>
          <button class="btn primary" type="submit">保存</button>
        </div>
      </form>`);
    const f = dlg.querySelector('#renewForm');
    f.onsubmit = async (e) => {
      e.preventDefault();
      const v = formData(f);
      try {
        await adminApi('/api/admin/renewals', { method: 'POST', body: JSON.stringify({
          nodeId: v.nodeId, at: v.at, amount: Number(v.amount) || 0,
          currency: v.currency, note: v.note,
        }) });
        dlg.close();
        toast('续费记录已登记');
        viewAssets();
      } catch (err) { toast('登记失败：' + err.message, 'err'); }
    };
  };

  $('#addRenewBtn').onclick = () => openRenew('');
  $$('[data-asset-renew]').forEach((b) => {
    b.onclick = () => openRenew(b.dataset.assetRenew);
  });
  $$('[data-asset-edit]').forEach((b) => {
    b.onclick = async () => {
      try {
        await editNodeById(b.dataset.assetEdit, viewAssets);
      } catch (err) { toast('读取资产信息失败：' + err.message, 'err'); }
    };
  });

  $$('[data-del-renew]').forEach((b) => {
    b.onclick = async (e) => {
      e.stopPropagation();
      if (!(await modalConfirm('删除续费记录', '删除这条续费记录？', { danger: true, okText: '删除' }))) return;
      try {
        await adminApi('/api/admin/renewals/' + encodeURIComponent(b.dataset.delRenew), { method: 'DELETE' });
        toast('续费记录已删除');
        viewAssets();
      } catch (err) { toast('删除失败：' + err.message, 'err'); }
    };
  });
}


/* ---------------- 节点对比 ---------------- */

async function viewCompare() {
  if (state.compareGroup == null) state.compareGroup = '';
  if (state.compareFav == null) state.compareFav = false;
  const qs = new URLSearchParams();
  if (state.compareGroup) qs.set('group', state.compareGroup);
  if (state.compareFav) qs.set('favorite', '1');
  const [d, nodes] = await Promise.all([
    api('/api/compare' + (qs.toString() ? '?' + qs.toString() : '')),
    api('/api/nodes'),
  ]);
  const rowsAll = d.nodes || [];
  const rows = rowsAll.filter((n) => matchesSearch([n.name, n.group, n.region, n.country, n.state]));
  const sum = d.summary || {};
  const groups = [...new Set(nodes.map((n) => n.group).filter(Boolean))].sort();

  const groupRows = (sum.groups || []).map((g) => `<tr>
    <td class="strong">${esc(g.group)}</td>
    <td class="num mono">${g.online}/${g.nodes}</td>
    <td class="num">${g.avgCpu.toFixed(1)}%</td>
    <td class="num">${g.avgMem.toFixed(1)}%</td>
    <td class="num mono">$${g.cost.toFixed(1)}</td>
    <td class="num mono">${g.remaining ? '$' + g.remaining.toFixed(1) : '—'}</td>
    <td class="num mono">${rate(g.trafficIn)}/s</td>
  </tr>`).join('') || `<tr><td colspan="7"><div class="empty">没有可聚合的节点</div></td></tr>`;

  const nodeRows = rows.map((n) => {
    const quota = n.traffic ? n.traffic.quotaGb || 0 : 0;
    // 与详情页一致：用量取服务端按口径合好的 used，旧版服务端才回退相加。
    const used = n.traffic ? (n.traffic.used != null ? n.traffic.used : (n.traffic.usedRx || 0) + (n.traffic.usedTx || 0)) : 0;
    const qp = quota > 0 ? (used / (quota * 1024 * 1024 * 1024)) * 100 : 0;
    const st = nodeState({ state: n.state, online: n.state === 'online', alerts: 0 });
    const days = n.expireAt ? Math.ceil((new Date(n.expireAt + 'T00:00:00') - Date.now()) / 86400000) : null;
    return `<tr class="row-click" data-node="${esc(n.id)}">
      <td><button class="fav-btn ${n.favorite ? 'on' : ''}" data-fav-node="${esc(n.id)}" data-fav="${n.favorite ? '1' : '0'}" title="${n.favorite ? '取消收藏' : '收藏'}">${n.favorite ? '★' : '☆'}</button></td>
      <td class="strong">${esc(n.name)}${days != null && days <= 30 ? ` <span class="badge ${days <= 7 ? 'crit' : 'warn'}">${days < 0 ? '已过期' : days + ' 天'}</span>` : ''}</td>
      <td><span class="dot ${st.tone}"></span> ${esc(st.text)}</td>
      <td>${esc(n.group || '—')}</td>
      <td class="num">${n.state === 'no_data' ? '—' : tbar(n.cpuPct, 80, 90)}</td>
      <td class="num">${n.state === 'no_data' ? '—' : tbar(n.memPct, 80, 92)}</td>
      <td class="num">${n.state === 'no_data' ? '—' : tbar(n.diskPct, 80, 90)}</td>
      <td class="num mono">${n.state === 'no_data' ? '—' : n.load1.toFixed(2)}</td>
      <td class="num mono cell-compact">${n.state === 'no_data' ? '—' : '↓' + rate(n.netRx) + ' ↑' + rate(n.netTx)}</td>
      <td class="num mono cell-compact">${bytes(used)}${quota > 0 ? `<div class="field-hint">${qp.toFixed(0)}% / ${quota} GB</div>` : ''}</td>
      <td class="num mono">${n.price ? (n.currency === 'CNY' ? '¥' : '$') + n.price : '—'}</td>
    </tr>`;
  }).join('') || `<tr><td colspan="11"><div class="empty">没有匹配的节点</div></td></tr>`;

  $('#view').innerHTML = `
    <div class="kpis" style="margin-bottom:20px">
      ${kpi('参与对比', sum.nodes || 0, '台', `在线 ${sum.online || 0} · 不可用 ${sum.unavailable || 0}`)}
      ${kpi('平均 CPU', (sum.avgCpu || 0).toFixed(1), '%', `峰值 ${(sum.maxCpu || 0).toFixed(1)}%`)}
      ${kpi('平均内存', (sum.avgMem || 0).toFixed(1), '%', `峰值 ${(sum.maxMem || 0).toFixed(1)}%`)}
      ${kpi('收藏节点', sum.favorites || 0, '台', '收藏优先展示')}
      ${kpi('月度成本', '$' + (sum.cost || 0).toFixed(1), '', '全部节点合计')}
      ${kpi('剩余价值', '$' + (sum.remaining || 0).toFixed(0), '', '未到期资产按剩余天数折算')}
    </div>
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">对比范围</h2>
        ${sectionActions(`${rows.length}/${rowsAll.length} 台节点`)}
      </div>
      <div class="panel panel-pad compare-filter">
        <label class="field" style="margin:0">
          <span class="field-label">分组</span>
          <select id="compareGroup">
            <option value="">全部分组</option>
            ${groups.map((g) => `<option value="${esc(g)}"${state.compareGroup === g ? ' selected' : ''}>${esc(g)}</option>`).join('')}
          </select>
        </label>
        <label class="check inline"><input type="checkbox" id="compareFav"${state.compareFav ? ' checked' : ''}><span>只看收藏</span></label>
      </div>
    </section>
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">节点对比</h2>
        <span class="section-meta">点击星标收藏，点击行查看详情</span>
      </div>
      <div class="table-wrap"><table class="node-table" data-page-size="10">
        <thead><tr><th></th><th>节点</th><th>状态</th><th>分组</th>
          <th class="num">CPU</th><th class="num">内存</th><th class="num">磁盘</th><th class="num">负载</th>
          <th class="num">实时流量</th><th class="num">周期用量</th><th class="num">年付价</th></tr></thead>
        <tbody>${nodeRows}</tbody>
      </table></div>
    </section>
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">分组聚合</h2>
        <span class="section-meta">仅在线节点参与均值</span>
      </div>
      <div class="table-wrap"><table class="assets-table">
        <thead><tr><th>分组</th><th class="num">在线 / 总数</th><th class="num">平均 CPU</th><th class="num">平均内存</th><th class="num">月度成本</th><th class="num">剩余价值</th><th class="num">实时下行</th></tr></thead>
        <tbody>${groupRows}</tbody>
      </table></div>
    </section>`;

  $('#compareGroup').onchange = (e) => { state.compareGroup = e.target.value; viewCompare(); };
  $('#compareFav').onchange = (e) => { state.compareFav = e.target.checked; viewCompare(); };

  $$('[data-fav-node]').forEach((b) => {
    b.onclick = async (e) => {
      e.stopPropagation();
      const id = b.dataset.favNode;
      const next = b.dataset.fav !== '1';
      try {
        const n = await adminApi('/api/admin/nodes/' + encodeURIComponent(id));
        await adminApi('/api/admin/nodes/' + encodeURIComponent(id), {
          method: 'PUT',
          body: JSON.stringify({
            name: n.name, provider: n.provider, region: n.region, group: n.group,
            country: n.country || '', expireAt: n.expireAt, price: n.price, currency: n.currency,
            note: n.note, weight: n.weight, favorite: next,
            trafficQuotaGb: n.trafficQuotaGb || 0, trafficResetDay: n.trafficResetDay || 0,
            // PUT 是整表替换：这里漏掉 trafficMode 会把用户配置的计费口径
            // 悄悄打回默认值，收藏一次就"用量翻倍/减半"。
            trafficMode: n.trafficMode || '',
          }),
        });
        toast(next ? '已收藏' : '已取消收藏');
        viewCompare();
      } catch (err) { toast('操作失败：' + err.message, 'err'); }
    };
  });

  $$('[data-node]').forEach((tr) => {
    tr.onclick = () => { location.hash = '#/nodes/' + tr.dataset.node; };
  });
}

/* ---------------- 设置 / 管理 ---------------- */

// 主题偏好仍保存在本机；管理会话由服务端 HttpOnly Cookie 承载，
// 前端不再保存任何凭证。
const LS_THEME = 'rollcall-theme';
const LS_THEME_LEGACY = 'probe-theme';

// 服务端下发的外观配置（仅固定字段）。用户本机手动切换模式仍优先，
// 强调色 / 圆角 / 密度则始终跟随运营配置。
let serverTheme = null;

function lsGet(key, legacy) {
  return localStorage.getItem(key) || localStorage.getItem(legacy) || '';
}
function lsSet(key, legacy, v) {
  localStorage.setItem(key, v);
  localStorage.removeItem(legacy); // 迁移后清掉旧 key，避免两份不一致
}

// 登录后所有管理请求都靠同源 Cookie；写操作额外带 X-Requested-With，
// 与服务端的 CSRF 校验配套。
async function adminApi(path, opts = {}) {
  const method = (opts.method || 'GET').toUpperCase();
  const headers = { Accept: 'application/json', ...(opts.headers || {}) };
  if (opts.body != null) headers['Content-Type'] = 'application/json';
  if (method !== 'GET' && method !== 'HEAD') headers['X-Requested-With'] = 'XMLHttpRequest';
  const r = await fetch(path, {
    ...opts,
    headers,
    credentials: 'same-origin',
  });
  const txt = await r.text();
  let data = null;
  try { data = txt ? JSON.parse(txt) : null; } catch (e) { data = { raw: txt }; }
  if (r.status === 401) authLost();
  if (!r.ok) {
    const err = new Error((data && (data.error || data.message)) || ('HTTP ' + r.status));
    err.status = r.status;
    throw err;
  }
  return data;
}

let toastTimer = null;
function toast(msg, kind = 'ok') {
  let el = document.getElementById('toast');
  if (!el) {
    el = document.createElement('div');
    el.id = 'toast';
    el.className = 'toast';
    document.body.appendChild(el);
  }
  el.textContent = msg;
  el.className = 'toast show ' + kind;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.className = 'toast'; }, 3200);
}

function field(label, name, value, opts = {}) {
  const type = opts.type || 'text';
  const ph = opts.placeholder ? ` placeholder="${esc(opts.placeholder)}"` : '';
  const req = opts.required ? ' required' : '';
  const hint = opts.hint ? `<span class="field-hint">${esc(opts.hint)}</span>` : '';
  return `<label class="field">
    <span class="field-label">${esc(label)}${opts.required ? ' *' : ''}</span>
    <input type="${type}" name="${name}" value="${esc(value ?? '')}"${ph}${req} autocomplete="off">
    ${hint}
  </label>`;
}

function checkbox(label, name, checked) {
  return `<label class="check">
    <input type="checkbox" name="${name}"${checked ? ' checked' : ''}>
    <span>${esc(label)}</span>
  </label>`;
}

function selectField(label, name, value, options) {
  return `<label class="field">
    <span class="field-label">${esc(label)}</span>
    <select name="${name}">
      ${options.map((o) => `<option value="${esc(o.v)}"${o.v === value ? ' selected' : ''}>${esc(o.t)}</option>`).join('')}
    </select>
  </label>`;
}

function formData(form) {
  const o = {};
  new FormData(form).forEach((v, k) => { o[k] = typeof v === 'string' ? v.trim() : v; });
  form.querySelectorAll('input[type=checkbox]').forEach((c) => { o[c.name] = c.checked; });
  return o;
}

/* ---------------- 管理员账号 ---------------- */

function refreshAccountCard() {
  const u = state.user || {};
  const name = u.username || 'admin';
  const avatar = $('.account-avatar');
  if (avatar) avatar.textContent = (name.trim()[0] || 'A').toUpperCase();
  const nameEl = $('.account-name');
  if (nameEl) nameEl.textContent = name;
  const info = $('.account-info .field-hint');
  if (info) info.textContent = `${u.email || '未设置邮箱'} · 会话有效期 7 天`;
  updateUserMenu();
}

function applyAccountResponse(res) {
  if (res && res.user) state.user = res.user;
  refreshAccountCard();
}

function showAccountDialog() {
  const u = state.user || {};
  const dlg = openDialog('编辑管理员账号', `
    <p class="modal-sub">保存后当前浏览器会自动续签会话，其他已登录设备需要重新登录。</p>
    <form class="form-grid" id="accountForm">
      <label class="field">
        <span class="field-label">用户名 *</span>
        <input type="text" name="username" value="${esc(u.username || '')}" required minlength="2" maxlength="32"
          pattern="[A-Za-z0-9][A-Za-z0-9._-]{1,31}" autocomplete="username">
        <span class="field-hint">2–32 位字母、数字、点、下划线或连字符</span>
      </label>
      <label class="field">
        <span class="field-label">邮箱</span>
        <input type="email" name="email" value="${esc(u.email || '')}" maxlength="254" autocomplete="email">
        <span class="field-hint">可选；填写后可用邮箱登录</span>
      </label>
      <label class="field">
        <span class="field-label">当前密码 *</span>
        <input type="password" name="currentPassword" required autocomplete="current-password">
        <span class="field-hint">验证身份后才能保存</span>
      </label>
      ${String(u.totpEnabled) === 'true' ? `
      <label class="field">
        <span class="field-label">动态验证码 *</span>
        <input type="text" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6"
          autocomplete="one-time-code" placeholder="身份验证器中的 6 位数字" required>
        <span class="field-hint">已启用两步验证，修改账号需要动态码（每个码只能用一次）</span>
      </label>` : ''}
      <div class="form-actions">
        <button class="btn" type="button" data-close>取消</button>
        <button class="btn primary" type="submit">保存账号</button>
      </div>
    </form>`);
  const form = dlg.querySelector('#accountForm');
  form.onsubmit = async (e) => {
    e.preventDefault();
    const d = formData(form);
    const btn = form.querySelector('button[type=submit]');
    btn.disabled = true;
    btn.textContent = '保存中…';
    try {
      const res = await adminApi('/api/admin/account', {
        method: 'POST',
        body: JSON.stringify({
          username: d.username, email: d.email,
          currentPassword: d.currentPassword, code: d.code || '',
        }),
      });
      applyAccountResponse(res);
      dlg.close();
      toast('账号已更新');
    } catch (err) {
      btn.disabled = false;
      btn.textContent = '保存账号';
      toast('保存失败：' + err.message, 'err');
    }
  };
  form.querySelector('input[name=username]').focus();
}

function showPasswordDialog() {
  const dlg = openDialog('修改管理员密码', `
    <p class="modal-sub">密码至少 12 个字符。修改成功后其他设备上的会话立即失效，当前浏览器自动续签。</p>
    <form class="form-grid" id="passwordForm">
      <label class="field">
        <span class="field-label">当前密码 *</span>
        <input type="password" name="currentPassword" required autocomplete="current-password">
      </label>
      <label class="field">
        <span class="field-label">新密码 *</span>
        <input type="password" name="newPassword" required minlength="12" autocomplete="new-password">
        <span class="field-hint">至少 12 个字符</span>
      </label>
      <label class="field">
        <span class="field-label">确认新密码 *</span>
        <input type="password" name="confirmPassword" required minlength="12" autocomplete="new-password">
      </label>
      ${String((state.user || {}).totpEnabled) === 'true' ? `
      <label class="field">
        <span class="field-label">动态验证码 *</span>
        <input type="text" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6"
          autocomplete="one-time-code" placeholder="身份验证器中的 6 位数字" required>
        <span class="field-hint">已启用两步验证，修改密码需要动态码（每个码只能用一次）</span>
      </label>` : ''}
      <div class="form-actions">
        <button class="btn" type="button" data-close>取消</button>
        <button class="btn primary" type="submit">修改密码</button>
      </div>
    </form>`);
  const form = dlg.querySelector('#passwordForm');
  form.onsubmit = async (e) => {
    e.preventDefault();
    const d = formData(form);
    if (d.newPassword !== d.confirmPassword) {
      toast('两次输入的新密码不一致', 'err');
      return;
    }
    const btn = form.querySelector('button[type=submit]');
    btn.disabled = true;
    btn.textContent = '提交中…';
    try {
      const res = await adminApi('/api/admin/account/password', {
        method: 'POST',
        body: JSON.stringify({
          currentPassword: d.currentPassword, newPassword: d.newPassword, code: d.code || '',
        }),
      });
      applyAccountResponse(res);
      dlg.close();
      toast('密码已修改，其他设备需重新登录');
    } catch (err) {
      btn.disabled = false;
      btn.textContent = '修改密码';
      toast('修改失败：' + err.message, 'err');
    }
  };
  form.querySelector('input[name=currentPassword]').focus();
}

/* ---------------- 两步验证 ---------------- */

function showTOTPSetupDialog() {
  const dlg = openDialog('启用两步验证', `
    <p class="modal-sub">使用 Google Authenticator、1Password、Microsoft Authenticator 等扫描或手动录入密钥。启用后登录需要额外输入 6 位动态码。</p>
    <form id="totpSetupForm" class="form-grid">
      <label class="field">
        <span class="field-label">当前密码 *</span>
        <input type="password" name="password" required autocomplete="current-password">
        <span class="field-hint">生成密钥前需要验证身份</span>
      </label>
      <div class="form-actions">
        <button class="btn primary" type="button" id="totpGen">生成密钥</button>
        <button class="btn" type="button" data-close>取消</button>
      </div>
    </form>
    <div id="totpStep2" hidden>
      <div class="panel panel-pad" style="margin-top:12px">
        <div class="field-label">手动录入密钥</div>
        <pre class="code" id="totpSecret" style="word-break:break-all"></pre>
        <p class="field-hint" style="margin:6px 0 0">在身份验证器中选择「手动输入」，类型选 TOTP，粘贴上面的密钥。</p>
        <details style="margin-top:8px">
          <summary class="field-hint" style="cursor:pointer">显示 otpauth 链接</summary>
          <pre class="code" id="totpURL" style="word-break:break-all"></pre>
        </details>
      </div>
      <form id="totpEnableForm" class="form-grid" style="margin-top:10px">
        <label class="field">
          <span class="field-label">动态验证码 *</span>
          <input type="text" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" placeholder="6 位数字" required autocomplete="one-time-code">
          <span class="field-hint">输入身份验证器中当前显示的 6 位数字</span>
        </label>
        <div class="form-actions">
          <button class="btn primary" type="submit">验证并启用</button>
        </div>
      </form>
    </div>`);
  const form = dlg.querySelector('#totpSetupForm');
  let password = '';
  dlg.querySelector('#totpGen').onclick = async (e) => {
    const pwd = form.password.value;
    if (!pwd) { toast('请输入当前密码', 'err'); return; }
    e.target.disabled = true;
    try {
      const r = await adminApi('/api/admin/account/totp', {
        method: 'POST', body: JSON.stringify({ action: 'setup', currentPassword: pwd }),
      });
      password = pwd;
      dlg.querySelector('#totpSecret').textContent = r.secret || '';
      dlg.querySelector('#totpURL').textContent = r.otpauth || '';
      dlg.querySelector('#totpStep2').hidden = false;
      form.hidden = true;
      dlg.querySelector('#totpEnableForm [name=code]').focus();
    } catch (err) {
      toast('生成失败：' + err.message, 'err');
      e.target.disabled = false;
    }
  };
  dlg.querySelector('#totpEnableForm').onsubmit = async (e) => {
    e.preventDefault();
    const code = e.target.code.value.trim();
    const btn = e.target.querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      const r = await adminApi('/api/admin/account/totp', {
        method: 'POST', body: JSON.stringify({ action: 'enable', currentPassword: password, code }),
      });
      applyAccountResponse(r);
      dlg.close();
      toast('两步验证已启用，下次登录需输入动态码');
      viewSettings();
    } catch (err) {
      toast('启用失败：' + err.message, 'err');
      btn.disabled = false;
    }
  };
}

function showTOTPDisableDialog() {
  const dlg = openDialog('停用两步验证', `
    <p class="modal-sub" style="color:var(--warn)">停用后仅凭密码即可登录，安全性会下降。请输入当前密码与动态验证码确认。</p>
    <form id="totpDisableForm" class="form-grid">
      <label class="field">
        <span class="field-label">当前密码 *</span>
        <input type="password" name="password" required autocomplete="current-password">
      </label>
      <label class="field">
        <span class="field-label">动态验证码 *</span>
        <input type="text" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" required autocomplete="one-time-code">
      </label>
      <div class="form-actions">
        <button class="btn danger" type="submit">确认停用</button>
        <button class="btn" type="button" data-close>取消</button>
      </div>
    </form>`);
  const form = dlg.querySelector('#totpDisableForm');
  form.onsubmit = async (e) => {
    e.preventDefault();
    const v = formData(form);
    const btn = form.querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      const r = await adminApi('/api/admin/account/totp', {
        method: 'POST', body: JSON.stringify({ action: 'disable', currentPassword: v.password, code: v.code }),
      });
      applyAccountResponse(r);
      dlg.close();
      toast('两步验证已停用');
      viewSettings();
    } catch (err) {
      toast('停用失败：' + err.message, 'err');
      btn.disabled = false;
    }
  };
}

function showRawToken(token, meta) {
  const dlg = openDialog('API 令牌已创建', `
    <p class="modal-sub">明文只显示这一次，关闭后无法再次查看。请立即复制并保存到密钥管理系统。</p>
    <pre class="code" id="rawTokenText" style="word-break:break-all"></pre>
    <p class="field-hint">名称：${esc((meta && meta.name) || '')} · 范围：${esc(((meta && meta.scopes) || []).join(', '))} · 路径：${esc(((meta && meta.paths) || []).join(', ') || '全部')}</p>
    <div class="form-actions">
      <button class="btn primary" id="copyTokenBtn" type="button">复制令牌</button>
      <button class="btn" type="button" data-close>关闭</button>
    </div>`);
  dlg.querySelector('#rawTokenText').textContent = token;
  dlg.querySelector('#copyTokenBtn').onclick = async (e) => {
    try {
      await navigator.clipboard.writeText(token);
      e.target.textContent = '已复制 ✓';
    } catch (err) {
      const range = document.createRange();
      range.selectNodeContents(dlg.querySelector('#rawTokenText'));
      const sel = getSelection(); sel.removeAllRanges(); sel.addRange(range);
      toast('已选中令牌文本，请 Ctrl+C 复制');
    }
  };
}

/* ---------------- 实例健康 ---------------- */

let healthTimer = null;

// SSE 连接（见 tickLive 附近的 __ensureStream）
let liveStream = null;

// healthBody 渲染卡片主体。所有数值都可缺省：老服务端没有该接口时
// 前端不应整页报错，只是不显示这块。
function healthBody(h) {
  if (!h || !h.nodes) return `<div class="empty">实例状态暂时不可用</div>`;
  const s = h.save || {}, mem = h.memory || {}, rep = h.reports || {};
  const nodes = h.nodes || {}, nt = h.notify || {};

  const up = Number(h.uptimeSeconds) || 0;
  const upText = up >= 86400
    ? `${Math.floor(up / 86400)} 天 ${Math.floor((up % 86400) / 3600)} 小时`
    : up >= 3600 ? `${Math.floor(up / 3600)} 小时 ${Math.floor((up % 3600) / 60)} 分`
      : up >= 60 ? `${Math.floor(up / 60)} 分钟` : `${Math.round(up)} 秒`;

  const expected = Number(rep.expected) || 0;
  const got = Number(rep.last60s) || 0;
  // 低于预期的 60% 才算"掉队"：上报本来就有抖动，卡太严会长期显示黄色。
  const reportOk = expected === 0 || got >= expected * 0.6;

  const total = Math.max(1, (s.copyMs || 0) + (s.marshalMs || 0) + (s.encodeMs || 0) + (s.writeMs || 0));
  // 亚毫秒的段显示 <0.1 而不是 0.0：后者会被误读成"没花时间"
  const msText = (v) => {
    const n = Number(v) || 0;
    if (n <= 0) return '0 ms';
    return n < 0.05 ? '<0.1 ms' : n.toFixed(1) + ' ms';
  };
  const seg = (label, ms, color) => `
    <div style="display:flex;align-items:center;gap:8px;margin:5px 0">
      <span style="width:56px;color:var(--fg-3);font-size:11.5px">${label}</span>
      <span style="flex:1;height:6px;background:var(--panel-2);border-radius:3px;overflow:hidden">
        <span style="display:block;height:100%;width:${Math.max(1, (Number(ms) || 0) / total * 100).toFixed(1)}%;background:${color}"></span>
      </span>
      <span class="mono" style="width:62px;text-align:right;font-size:11.5px">${msText(ms)}</span>
    </div>`;

  return `
    <div class="health-grid">
      <div>
        <div style="display:flex;gap:8px;flex-wrap:wrap;margin-bottom:10px">
          <span class="badge mute">${esc(h.version || 'dev')}</span>
          <span class="badge mute">运行 ${upText}</span>
          <span class="badge ${nodes.online === nodes.total ? 'ok' : 'warn'}">在线 ${nodes.online}/${nodes.total}</span>
          <span class="badge mute">限速条目 ${Number(h.limiterEntries) || 0}</span>
        </div>
        <div class="field-hint" style="display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:6px 14px">
          <span class="health-stat">内存（堆）：<b class="mono">${bytes(mem.heapAllocBytes)}</b></span>
          <span class="health-stat">协程：<b class="mono">${Number(h.goroutines) || 0}</b></span>
          <span class="health-stat">上报（60s）：<b class="mono">${got}</b> / 预期 <b class="mono">${expected}</b></span>
          <span class="health-stat">速率：<b class="mono">${(Number(rep.perSecond) || 0).toFixed(2)}</b> 次/秒</span>
          <span class="health-stat">通知队列：<b class="mono">${Number(nt.pending) || 0}</b> 待投递</span>
          <span class="health-stat">已送达：<b class="mono">${Number(nt.sent) || 0}</b></span>
        </div>
        <div style="margin-top:10px">
          <span class="badge ${reportOk ? 'ok' : 'warn'}">${reportOk ? '采集正常' : '采集可能掉队'}</span>
          <span class="badge ${(Number(s.failures) || 0) === 0 ? 'ok' : 'crit'}">快照失败 ${Number(s.failures) || 0}</span>
        </div>
      </div>
      <div>
        <div style="display:flex;justify-content:space-between;align-items:baseline;margin-bottom:6px">
          <span class="sub-title">最近一次快照</span>
          <span class="mono" style="font-size:11.5px;color:var(--fg-3)">
            ${(Number(s.lastDurationMs) || 0).toFixed(0)} ms · ${bytes(s.bytes)}
          </span>
        </div>
        ${seg('深拷贝', s.copyMs, 'var(--accent)')}
        ${seg('JSON', s.marshalMs, 'var(--accent-2)')}
        ${seg('gzip', s.encodeMs, 'var(--ok)')}
        ${seg('写盘', s.writeMs, 'var(--fg-3)')}
        ${Number(s.seriesMs) > 0 ? seg('历史分片', s.seriesMs, 'var(--crit)') : ''}
        <div class="field-hint" style="margin-top:8px">
          配置与当前态每 60 秒一次，历史分片每 5 分钟一次（各自独立计费）；
          编码与压缩都在锁外。上次成功：${Number(s.lastSuccessAgoSeconds) || 0} 秒前。
        </div>
      </div>
    </div>`;
}

async function refreshHealthCard() {
  const el = document.getElementById('healthBody');
  // 切走页面后定时器还在跑，这里必须自检，否则会往不存在的节点写。
  if (!el || state.route !== 'settings') return;
  try {
    const h = await adminApi('/api/admin/health');
    el.innerHTML = healthBody(h);
  } catch (e) {
    // 轮询失败不覆盖页面：卡片保留上一次的数据，避免闪成错误态。
  }
}

// 能力矩阵：设置页尾部的静态自述。只陈述事实、与 README 功能清单同步维护，
// 不从接口取数——它是文档，不是状态面板；口径与竞品对照页一致（● ◐ ○ –）。
const CAP_MATRIX = [
  ['采集与指标', [
    ['基础资源指标（CPU / 内存 / Swap / 磁盘 / 负载 / TCP / 进程 / 运行时长 / 网络）', 'yes', '#/nodes', '节点详情 1h–365d 时序图'],
    ['多挂载点 / 每网卡 / 磁盘 I/O 分项采集', 'yes', '#/nodes', '节点详情可展开'],
    ['平台范围：仅 Linux 有指标采集', 'part', '', 'Windows/macOS 可接入但无数据（占位实现）'],
    ['SSH 登录记录（Agent 读 wtmp，只报增量）', 'part', '#/nodes', '依赖节点 sshd 实际写入 wtmp'],
    ['GPU / 温度 / 容器级指标', 'no', '', '未采集'],
    ['统计网卡自动过滤（bond 从属 / veth / docker / cni）', 'yes', '', '网卡集合不可手动指定'],
  ]],
  ['服务探测', [
    ['HTTP(S) / TCP / DNS / ICMP 四种探测', 'yes', '#/services', '异常自动置顶，同目标并排对比'],
    ['证书到期 / 丢包率 / 抖动指标与阈值告警', 'yes', '#/alerts', '规则模板一键套用'],
    ['探测由控制端统一发起', 'yes', '#/services', '节点侧探测刻意不做（Agent 单向边界）'],
  ]],
  ['告警与通知', [
    ['资源 / 离线 / 流量配额 / 到期 / 延迟 / 丢包 / 抖动规则', 'yes', '#/alerts', '逐规则静默与全局免打扰'],
    ['10 通知通道 + 本地插件', 'yes', '#/settings', 'Outbox 持久化重试，逐通道测试'],
    ['IP 变更 / 后台登录 / 多台离线合并通知', 'yes', '#/settings', ''],
  ]],
  ['历史与容量', [
    ['分层保留：24h 原始 / 7d 1m / 30d 5m / 365d 1h', 'yes', '#/nodes', '长范围自动聚合，不拉全量原始点'],
    ['加密配置备份 + 自动调度', 'yes', '#/settings', 'AES-256-GCM'],
  ]],
  ['公开状态页', [
    ['总开关 + 访问令牌 + 逐台隐藏 + 公开备注', 'yes', '#/statuspage', '默认开启；显式关闭时匿名一律 404'],
    ['24h 色块 / 历史趋势 / 地图 / 活动流 / 维护窗口 / 事件', 'yes', '#/statuspage', '板块可拖拽排序'],
    ['主题包（公开页皮肤）', 'part', '#/settings', '脚本与面板同源执行，安装等同授予管理员权限'],
  ]],
  ['安全', [
    ['Agent 只读（无 os/exec、无监听端口）', 'yes', '', 'go list -deps 可验证'],
    ['上报签名（时间戳 + nonce + HMAC-SHA256，可强制）', 'yes', '#/settings', '强制开启后未签名节点被拒绝'],
    ['Ed25519 发行验签 + Agent 签名自更新 + 自动回滚', 'yes', '#/settings', ''],
    ['TOTP / 登录失败限速 / 管理操作审计', 'yes', '#/audit', ''],
    ['Passkey / 安全入口 / 多用户', 'no', '', '未提供；多用户为刻意不做'],
  ]],
  ['接口与集成', [
    ['作用域 API 令牌 / Prometheus /metrics / CSV 导出', 'yes', '#/settings', ''],
    ['只读 MCP 服务器（rollcall-mcp，stdio）', 'yes', '#/settings', '7 个只读工具，scopes=read 令牌'],
  ]],
  ['部署', [
    ['单二进制零外部依赖 / Linux 四架构', 'yes', '', 'amd64 / arm64 / arm / 386'],
    ['Docker（Dockerfile 自行构建）', 'yes', '', 'docker build --build-arg VERSION=vX.Y.Z'],
    ['一键升级 / 回滚（验签 + 健康检查 + 失败自动回滚）', 'yes', '', 'deploy/rollcall-server-upgrade.sh'],
  ]],
];

function capMatrixHTML() {
  const sym = { yes: ['●', 'cap-yes'], part: ['◐', 'cap-part'], no: ['○', 'cap-no'], na: ['–', 'cap-na'] };
  const body = CAP_MATRIX.map(([group, rows]) => {
    const trs = rows.map(([name, st, link, desc]) => {
      const s = sym[st] || sym.no;
      const label = link ? `<a class="row-link" href="${esc(link)}">${esc(name)}</a>` : esc(name);
      return `<tr><td class="strong">${label}</td>
        <td class="num"><span class="cap-dot ${s[1]}" title="${st === 'yes' ? '具备' : st === 'part' ? '部分或有前提' : '未提供'}">${s[0]}</span></td>
        <td class="cap-desc">${esc(desc || '—')}</td></tr>`;
    }).join('');
    return `<tr class="cap-group"><td colspan="3">${esc(group)}</td></tr>${trs}`;
  }).join('');
  return `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">能力矩阵</h2>
        <span class="section-meta">静态自述 · 与 README 功能清单同步维护</span>
      </div>
      <div class="panel panel-pad">
        <div class="cap-legend">
          <span><span class="cap-dot cap-yes">●</span> 具备</span>
          <span><span class="cap-dot cap-part">◐</span> 部分或有前提</span>
          <span><span class="cap-dot cap-no">○</span> 未提供</span>
        </div>
        <div class="table-wrap"><table class="assets-table">
          <thead><tr><th>能力</th><th class="num">状态</th><th>说明 / 去哪看</th></tr></thead>
          <tbody>${body}</tbody>
        </table></div>
      </div>
    </section>`;
}

async function viewSettings() {
  if (!state.authenticated) {
    // 与首屏登录共用同一个全屏登录层：验证通过后 render() 会回到当前路由
    // （hash 仍是 #/settings），自然落回设置页。
    state.authRequired = true;
    showLoginGate();
    return;
  }

  let nodes = [], services = [], cfg = null, chans = [], defs = [];
  let themeCfg = null;
  let themePacks = { active: '', dir: '', packs: [] };
  let queue = { stats: {}, jobs: [] };
  let tokens = [];
  let health = null;
  let loadErr = '';
  let autoBk = null;
  let upgrades = { items: [], serverVersion: '' };
  let storage = null;
  let repSec = null;
  try {
    const [n, svc, q, th, tk, hl, tp, ab, au, sg, rs] = await Promise.all([
      adminApi('/api/admin/nodes'),
      adminApi('/api/admin/services'),
      adminApi('/api/admin/notify/jobs'),
      adminApi('/api/admin/theme'),
      adminApi('/api/admin/tokens').catch(() => []),
      adminApi('/api/admin/health').catch(() => null),
      adminApi('/api/admin/theme-packs').catch(() => null),
      adminApi('/api/admin/backup/auto').catch(() => null),
      adminApi('/api/admin/agent-upgrades').catch(() => null),
      adminApi('/api/admin/storage').catch(() => null),
      adminApi('/api/admin/report-security').catch(() => null),
    ]);
    repSec = rs;
    health = hl;
    autoBk = ab;
    storage = sg;
    if (au) upgrades = au;
    nodes = n; services = svc; queue = q || queue; tokens = Array.isArray(tk) ? tk : [];
    themeCfg = th || {};
    if (tp) themePacks = tp;
    const st = await adminApi('/api/admin/settings');
    cfg = st.notify || {};
    chans = st.channels || [];
    defs = st.defs || [];
    state.panelLink = st.panelLink || '';
    state.timezone = st.timezone || '';
    state.timezoneOffsetMin = (st.timezoneOffsetMin === undefined ? null : st.timezoneOffsetMin);
    state.serverVersion = st.version || '';
  } catch (e) {
    if (e.status === 401) return;
    loadErr = e.message;
  }

  if (loadErr) {
    $('#view').innerHTML = `
      <div class="panel panel-pad">
        <div style="color:var(--crit);font-weight:600;font-size:13px;margin-bottom:4px">管理接口不可用</div>
        <div style="font-size:12px;color:var(--fg-2)">${esc(loadErr)}</div>
        <div class="form-actions" style="margin-top:14px">
          <button class="btn" id="retryBtn">重新加载</button>
          <button class="btn danger" id="logoutBtn">退出登录</button>
        </div>
      </div>`;
    $('#retryBtn').onclick = viewSettings;
    $('#logoutBtn').onclick = doLogout;
    return;
  }

  const t = cfg.telegram || {}, w = cfg.webhook || {}, m = cfg.email || {};
  const hourOpts = Array.from({ length: 24 }, (_, i) => ({ v: String(i), t: String(i).padStart(2, '0') + ':00' }));
  const quietStart = cfg.quietStart == null ? -1 : Number(cfg.quietStart);
  const quietEnd = cfg.quietEnd == null ? 7 : Number(cfg.quietEnd);
  const qJobs = queue.jobs || [], qStats = queue.stats || {};
  const qBadge = {
    pending: ['warn', '待投递'], failed: ['crit', '失败'],
    suppressed: ['mute', '已拦截'], sent: ['ok', '已送达'],
  };
  const account = state.user || {};
  const accountName = account.username || 'admin';
  const accountEmail = account.email || '未设置邮箱';
  const accountInitial = accountName.slice(0, 1).toUpperCase();
  const serverVersion = state.serverVersion || '';
  const panelBase = (state.panelLink || location.origin).replace(/\/+$/, '');
  const totpOn = String(account.totpEnabled) === 'true';
  const tokenRows = tokens.map((tk) => `<tr>
    <td class="strong">${esc(tk.name)}</td>
    <td class="mono" style="font-size:11.5px">${esc(tk.prefix)}…</td>
    <td>${(tk.scopes || []).map((x) => `<span class="badge ${x === 'admin' ? 'warn' : 'mute'}">${esc(x)}</span>`).join(' ')}</td>
    <td class="${(tk.paths && tk.paths.length) ? 'mono' : ''}" style="font-size:11.5px">${(tk.paths && tk.paths.length) ? tk.paths.map((p) => esc(p)).join('<br>') : '<span class="text-faint">全部</span>'}</td>
    <td class="mono" style="font-size:11.5px">${clock(tk.createdAt)}</td>
    <td style="font-size:11.5px">${tk.expiresAt ? `<span class="mono">${clock(tk.expiresAt)}</span>` : '永不过期'}</td>
    <td style="font-size:11.5px">${tk.lastUsedAt
      ? `<span class="mono">${clock(tk.lastUsedAt)}</span>${tk.lastUsedIp ? `<br><span class="text-faint mono">${esc(tk.lastUsedIp)}</span>` : ''}`
      : '<span class="text-faint">从未使用</span>'}</td>
    <td class="num"><button class="btn tiny danger" data-del-token="${esc(tk.id)}">删除</button></td>
  </tr>`).join('') || `<tr><td colspan="8"><div class="empty">尚未创建 API 令牌</div></td></tr>`;
  const packList = Array.isArray(themePacks.packs) ? themePacks.packs : [];
  const packOpts = [{ v: '', t: '内置（默认）' }].concat(
    packList.filter((p) => !p.error).map((p) => ({
      v: p.name,
      t: p.title && p.title !== p.name ? p.title + ' · ' + p.name : p.name,
    }))
  );
  const badPacks = packList.filter((p) => p.error);
  const themeDirHint = themePacks.dir || '<数据目录>/themes';
  const ab = autoBk || { enabled: false };
  const abLatest = (ab.files || [])[0];
  const upList = Array.isArray(upgrades.items) ? upgrades.items : [];
  const upState = { done: ['ok', '已完成'], pending: ['warn', '待升级'], missing: ['mute', '已删除'] };
  const upgradeCards = upList.map((b) => {
    const pct = b.total ? Math.round((b.done / b.total) * 100) : 0;
    const rows = (b.targets || []).map((t) => {
      const [cls, label] = upState[t.state] || ['mute', t.state];
      return `<tr>
        <td class="strong">${esc(t.name || t.id)}</td>
        <td class="mono">${esc(t.before || '—')}</td>
        <td class="mono">${esc(t.current || '—')}</td>
        <td><span class="badge ${cls}">${label}</span></td>
      </tr>`;
    }).join('') || `<tr><td colspan="4"><div class="empty">清单为空</div></td></tr>`;
    return `<div class="panel panel-pad" style="margin-bottom:10px">
      <div class="panel-head">
        <span class="panel-title">目标 ${esc(b.targetVersion || '—')}
          <span class="badge ${pct === 100 ? 'ok' : 'warn'}">${b.done}/${b.total} 完成</span>
          ${b.note ? `<span class="field-hint" style="margin-left:8px">${esc(b.note)}</span>` : ''}
        </span>
        <button class="btn tiny danger" data-upgrade-del="${esc(b.id)}">删除</button>
      </div>
      <div class="meter" style="margin:8px 0 4px"><i style="width:${pct}%;background:${pct === 100 ? 'var(--ok)' : 'var(--warn)'}"></i></div>
      <div class="field-hint" style="margin-bottom:10px">创建于 ${esc(clock(b.createdAt))} · ${b.total} 台 · 按各节点最近上报的 Agent 版本推导进度</div>
      <details>
        <summary class="sum">查看节点明细</summary>
        <div class="table-wrap" style="margin-top:8px"><table class="maint-table" data-page-size="10">
          <thead><tr><th>节点</th><th>创建时版本</th><th>当前版本</th><th>状态</th></tr></thead>
          <tbody>${rows}</tbody>
        </table></div>
      </details>
    </div>`;
  }).join('') || `<div class="empty" style="padding:18px">还没有升级批次。升级时先建一份清单，节点上报新版本后会自动标记完成。</div>`;

  // 存储与容量：目录占用、历史点数、保留期与审计/备份体积。
  const stg = storage || {};
  const stgEntries = Array.isArray(stg.entries) ? stg.entries : [];
  const stgTotal = stg.totalBytes || 0;
  const stgHist = stg.history || { nodes: [], totals: {}, retentionDays: {} };
  const stgTotals = stgHist.totals || {};
  const stgDays = stgHist.retentionDays || {};
  const stgAudit = stg.audit || {};
  const storageBars = stgEntries.map((e) => {
    const pct = stgTotal > 0 ? Math.max(1, (e.bytes / stgTotal) * 100) : 0;
    return `<div class="kv-row">
      <span class="kv-key mono" style="font-size:11.5px">${esc(e.name)}</span>
      <span class="kv-val" style="display:flex;align-items:center;gap:8px;justify-content:flex-end">
        <span class="nb-track" style="width:90px"><span class="nb-fill" style="width:${pct.toFixed(1)}%;background:var(--accent)"></span></span>
        <span class="mono" style="font-size:11.5px">${bytes(e.bytes)} · ${e.files} 个文件</span>
      </span>
    </div>`;
  }).join('') || '<div class="empty">数据目录为空或不可读</div>';
  const histNodeRows = (stgHist.nodes || []).map((n) => `<tr>
      <td class="strong">${esc(n.name)}</td>
      <td class="num mono">${(n.points && n.points['1m']) || 0}</td>
      <td class="num mono">${(n.points && n.points['5m']) || 0}</td>
      <td class="num mono">${(n.points && n.points['1h']) || 0}</td>
    </tr>`).join('') || `<tr><td colspan="4"><div class="empty">暂无历史数据</div></td></tr>`;

  $('#view').innerHTML = `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">实例健康</h2>
        <span class="section-meta">运行状态与快照成本 · 每 5 秒刷新</span>
      </div>
      <div class="panel panel-pad" id="healthBody">${healthBody(health)}</div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">节点管理</h2>
        <span class="section-meta">${nodes.length} 台${serverVersion ? ' · 控制端 ' + esc(serverVersion) : ''}</span>
      </div>
      <div class="table-wrap" style="margin-bottom:12px"><table class="settings-nodes-table" data-page-size="10">
        <thead><tr><th>名称</th><th>服务商</th><th>位置</th><th>分组</th><th>到期</th><th title="节点上报的 Agent 版本">Agent</th><th class="num">操作</th></tr></thead>
        <tbody>${nodes.map((n) => `<tr>
          <td class="strong">${esc(n.name)}</td>
          <td>${esc(n.provider || '—')}</td>
          <td>${esc(n.region || '—')}</td>
          <td>${esc(n.group || '—')}</td>
          <td class="mono">${esc(n.expireAt || '—')}</td>
          <td>${n.agentVersion
            ? `<span class="mono">${esc(n.agentVersion)}</span>${n.agentOutdated ? ` <span class="badge warn" title="低于控制端 ${esc(serverVersion)}">可升级</span>` : ''}`
            : '<span style="color:var(--fg-3)">—</span>'}</td>
          <td class="num"><button class="btn tiny" data-agent-node="${esc(n.id)}">接入</button>
            <button class="btn tiny" data-edit-node="${esc(n.id)}">编辑</button>
            <button class="btn tiny danger" data-del-node="${esc(n.id)}">删除</button></td>
        </tr>`).join('') || `<tr><td colspan="7"><div class="empty">暂无节点</div></td></tr>`}</tbody>
      </table></div>
      <p class="field-hint" style="margin:0 0 12px">
        Agent 版本低于控制端时会标记「可升级」。在目标节点执行
        <code>curl -fsSL ${esc(panelBase)}/agent/install.sh | sudo sh -s -- ${installInsecureFlag(panelBase)}--upgrade</code>
        即可只替换二进制、保留现有配置（需服务端启用 <code>-agent-dir</code>）。
      </p>
      <details class="panel panel-pad">
        <summary class="sum">⚙ 面板对外地址（域名反代）</summary>
        <p class="field-hint" style="margin:8px 0">
          用域名反代到 <code>ip:端口</code> 时填这里：<b>告警通知里的链接</b>与
          <b>Agent 接入命令里的地址</b>都会用它。留空表示用当前浏览器访问的地址。
          只支持站点根路径（<code>https://monitor.example.com</code>），不支持
          <code>/rollcall/</code> 这类子路径部署。
        </p>
        <form id="panelLinkForm" class="form-grid">
          <label class="field">
            <span class="field-label">对外地址</span>
            <input type="text" name="link" value="${esc(state.panelLink || '')}"
              placeholder="https://monitor.example.com" autocomplete="off">
            <span class="field-hint">
              生效前提：反代要传 <code>X-Forwarded-Proto</code>，且服务端用
              <code>-trusted-proxies</code> 信任该代理地址；否则记不到真实客户端 IP，
              登录 Cookie 也不会带 Secure。
            </span>
          </label>
          <div class="form-actions"><button class="btn primary" type="submit">保存地址</button></div>
        </form>
        <p class="field-hint" id="panelLinkHint" style="margin:6px 0 0"></p>
        <p class="field-hint" style="margin:8px 0 0">
          服务器时区：<code>${esc(timezoneLabel())}</code>
          —— 日报/周报的发送时刻、免打扰窗口、流量周期的每月起始日都按它算。
          服务器（尤其容器）默认是 <code>UTC</code>：此时「每天 9 点」会在另一个时刻触发。
          用启动参数 <code>-timezone Asia/Shanghai</code> 或环境变量 <code>TZ</code> 指定。
        </p>
      </details>
      <details class="panel panel-pad">
        <summary class="sum">⇪ 批量导入（迁移用）</summary>
        <p class="field-hint" style="margin:8px 0">
          一行一台，逗号或制表符分隔：<code>名称,服务商,地区,国家,分组</code>（后四项可留空）。
          重名或格式不对的行会被跳过并逐行列出，不会打断整批。导入后每台都会生成一行接入命令。
        </p>
        <form id="nodeImportForm">
          <label class="field">
            <span class="field-label">节点列表</span>
            <textarea name="list" rows="5" placeholder="香港-01,racknerd,圣何塞,US,prod
东京-02,绿云,东京,JP,prod"></textarea>
          </label>
          <div class="form-actions"><button class="btn primary" type="submit">导入并生成接入命令</button></div>
        </form>
      </details>
      <details class="panel panel-pad">
        <summary class="sum">＋ 新增节点</summary>
        <form id="nodeForm" class="form-grid">
          ${field('名称', 'name', '', { required: true, placeholder: '例如 prod-01', hint: '会显示在公开状态页，勿填主机名/IP' })}
          ${field('服务商', 'provider', '', { placeholder: 'racknerd / 腾讯云 …' })}
          ${field('位置', 'region', '', { placeholder: '美国 / 香港 …' })}
          ${field('国家代码', 'country', '', { placeholder: 'US / JP / DE', hint: '两位 ISO 代码，用于公开页分布图' })}
          ${field('分组', 'group', '', { placeholder: 'prod / test' })}
          ${field('到期日', 'expireAt', '', { placeholder: '2027-06-01' })}
          ${field('年付价', 'price', '', { type: 'number', placeholder: '11.00' })}
          ${selectField('币种', 'currency', 'USD', [{ v: 'USD', t: 'USD ($)' }, { v: 'CNY', t: 'CNY (¥)' }])}
          ${field('月度流量配额（GB）', 'trafficQuotaGb', '', { type: 'number', placeholder: '0 = 不限制' })}
          ${field('流量重置日', 'trafficResetDay', '1', { type: 'number', hint: '每月 1–28 日' })}
          ${selectField('流量计费口径', 'trafficMode', 'sum', TRAFFIC_MODES.map((x) => ({ v: x.v, t: x.t })))}
          ${field('备注', 'note', '', { placeholder: '可选' })}
          ${field('标签', 'tags', '', { placeholder: 'cn2, 高防', hint: '逗号分隔，最多 8 个' })}
          ${field('排序权重', 'weight', '', { type: 'number', placeholder: '0' })}
          ${checkbox('收藏（对比页优先展示）', 'favorite', false)}
          <div class="form-actions"><button class="btn primary" type="submit">创建并获取令牌</button></div>
        </form>
      </details>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">Agent 升级批次</h2>
        ${sectionActions('只记录清单与基线版本，不远程执行升级', '<button class="btn sm primary" id="upBatchAdd">＋ 新建升级清单</button>')}
      </div>
      <p class="section-note">节点升级完成后会随下一次上报写入新版本，这里按「当前版本 ≠ 创建时版本」自动标记完成。控制端当前版本 ${esc(upgrades.serverVersion || serverVersion || '—')}。</p>
      <div id="upgradeList">${upgradeCards}</div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">服务探测</h2>
        ${sectionActions(`${services.length} 个目标 · 与「服务」页共用同一份配置`, '<button class="btn sm primary" id="settingsSvcAdd">＋ 新增探测</button>')}
      </div>
      <div class="table-wrap"><table class="settings-services-table">
        <thead><tr><th>状态</th><th>名称</th><th>类型</th><th>目标</th><th class="num">间隔</th><th class="num">操作</th></tr></thead>
        <tbody>${services.map((s) => {
          const on = s.enabled !== false;
          return `<tr>
          <td><span class="dot ${on ? 'ok' : 'off'}"></span>${on ? '' : ' <span class="badge mute">已停用</span>'}</td>
          <td class="strong">${esc(s.name)}</td>
          <td><span class="badge mute">${esc(s.type.toUpperCase())}</span></td>
          <td class="mono target-cell" title="${esc(s.target)}">${esc(s.target)}</td>
          <td class="num mono">${s.interval}s</td>
          <td class="num"><div class="row-actions">
            <button class="btn tiny" data-svc-setting-test="${esc(s.id)}">检测</button>
            <button class="btn tiny" data-svc-setting-edit="${esc(s.id)}">编辑</button>
            <button class="btn tiny" data-svc-setting-toggle="${esc(s.id)}">${on ? '停用' : '启用'}</button>
            <button class="btn tiny danger" data-del-svc="${esc(s.id)}">删除</button>
          </div></td>
        </tr>`;
        }).join('') || `<tr><td colspan="6"><div class="empty">暂无探测目标</div></td></tr>`}</tbody>
      </table></div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">告警通知</h2>
        <span class="section-meta">${chans.length ? '已启用：' + chans.join(' / ') : '尚未配置通道'}</span>
      </div>

      <div class="panel panel-pad" style="margin-bottom:12px">
        <div class="sub-title" style="margin-bottom:10px">基础</div>
        <div class="form-grid">
          ${checkbox('启用通知', 'nEnabled', !!cfg.enabled)}
          ${selectField('最低严重度', 'nMinSeverity', cfg.minSeverity || 'warn',
            [{ v: 'info', t: '信息及以上' }, { v: 'warn', t: '警告及以上' }, { v: 'critical', t: '仅严重' }])}
          ${field('重复提醒间隔（秒）', 'nRepeat', cfg.repeatInterval || 0,
            { type: 'number', hint: '0 = 只通知一次，不重复' })}
          ${field('离线宽限下限（秒）', 'nOfflineGrace', cfg.offlineGraceSec || 0,
            { type: 'number', hint: '0 = 不干预；「节点失联」规则至少持续这么久才告警（规则设得更长以规则为准）' })}
        </div>
        <div class="sub-title" style="margin:16px 0 10px">降噪、摘要与到期提醒</div>
        <div class="form-grid">
          ${selectField('免打扰开始', 'quietStart', String(quietStart),
            [{ v: '-1', t: '关闭免打扰' }].concat(hourOpts))}
          ${selectField('免打扰结束', 'quietEnd', String(quietEnd), hourOpts)}
          ${field('摘要聚合窗口（分钟）', 'digestMins', cfg.digestMins || 0,
            { type: 'number', hint: '0 = 逐条发送；5–1440 内的告警合并为一条摘要' })}
          ${field('到期提醒提前天数', 'expiryDays', cfg.expiryDays == null ? 7 : cfg.expiryDays,
            { type: 'number', hint: '-1 关闭；0 表示仅到期当天提醒' })}
        </div>
        <p class="field-hint" style="margin:6px 0 0">
          免打扰期间严重告警与恢复通知仍会发送；普通告警延后到窗口结束。窗口跨午夜时（例如 22 → 7）自动按次日计算。
        </p>
        <div class="sub-title" style="margin:16px 0 10px">消息模板</div>
        <div class="form-grid">
          <label class="field">
            <span class="field-label">标题模板</span>
            <input name="titleFormat" value="${esc(cfg.titleFormat || '')}" maxlength="500" placeholder="留空使用默认标题">
            <span class="field-hint">可用变量：{status} {node} {title} {rule} {value} {severity} {time}</span>
          </label>
          <label class="field">
            <span class="field-label">正文模板</span>
            <input name="bodyFormat" value="${esc(cfg.bodyFormat || '')}" maxlength="2000" placeholder="留空使用默认正文">
            <span class="field-hint">可用变量：{status} {node} {title} {rule} {value} {detail} {time} {link}</span>
          </label>
        </div>
        <div class="sub-title" style="margin:16px 0 10px">运行报告</div>
        <div class="form-grid">
          ${checkbox('每日运行报告', 'reportDaily', !!cfg.reportDaily)}
          ${checkbox('每周运行报告（周一）', 'reportWeekly', !!cfg.reportWeekly)}
          ${checkbox('每月运行报告（1 日）', 'reportMonthly', !!cfg.reportMonthly)}
          ${selectField('报告发送时间', 'reportHour', String(cfg.reportHour == null ? 9 : cfg.reportHour), hourOpts)}
        </div>
        <p class="field-hint" style="margin:6px 0 0">报告经由通知通道发送，包含节点状态、均值、活跃告警、流量、成本与 30 天内到期资产。</p>
      </div>

      <div id="chList"></div>

      <div class="panel panel-pad">
        <div class="ch-add">
          <select id="chType">
            ${defs.map((d) => `<option value="${esc(d.type)}">${esc(d.label)}</option>`).join('')}
          </select>
          <button class="btn" id="addCh" type="button">＋ 添加通道</button>
          <span style="flex:1"></span>
          <button class="btn" id="testAll" type="button">测试全部</button>
          <button class="btn primary" id="saveNotify" type="button">保存通知配置</button>
        </div>
        <p class="field-hint" id="chDesc" style="margin:8px 0 0">${esc((defs[0] || {}).desc || '')}</p>
      </div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">通知队列</h2>
        <span class="section-meta">待投递 ${qStats.pending || 0} · 失败 ${qStats.failed || 0} · 已送达 ${qStats.sent || 0} · 重试 ${qStats.retries || 0} · 拒收 ${qStats.rejected || 0}</span>
      </div>
      <div class="table-wrap"><table class="queue-table" data-page-size="10">
        <thead><tr><th>状态</th><th>通知</th><th>规则 / 节点</th><th class="num" title="已投递尝试次数">尝试</th><th>创建时间</th><th>最近错误</th><th class="num">操作</th></tr></thead>
        <tbody>${qJobs.map((j) => {
          const [cls, label] = qBadge[j.status] || ['mute', j.status];
          const canRetry = j.status === 'failed' || j.status === 'suppressed';
          return `<tr>
            <td><span class="badge ${cls}">${label}</span></td>
            <td class="strong" style="max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${esc(j.title)}">${esc(j.title)}</td>
            <td style="font-size:11.5px">${esc(j.ruleName || j.ruleId || '—')}<div class="field-hint">${esc(j.nodeName || j.nodeId || '—')}${j.resolved ? ' · 恢复通知' : ''}</div></td>
            <td class="num mono">${j.attempts || 0}</td>
            <td class="mono" style="font-size:11.5px">${clock(j.createdAt)}</td>
            <td style="max-width:240px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${esc(j.lastError || '')}">${esc(j.lastError || '—')}</td>
            <td class="num">${canRetry ? `<button class="btn tiny" data-retry-job="${esc(j.id)}">重试</button>` : '<span class="state-text">—</span>'}</td>
          </tr>`;
        }).join('') || `<tr><td colspan="7"><div class="empty">队列为空</div></td></tr>`}</tbody>
      </table></div>
      <p class="field-hint" style="margin:8px 0 0">
        失败或被静默拦截的任务可在修复通道后手动重试，重试会重置尝试次数；此处仅显示最近 100 条，累计值见 /metrics。
      </p>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">管理员账号</h2>
        <span class="section-meta">密码仅以 PBKDF2 哈希保存在服务端</span>
      </div>
      <div class="panel panel-pad account-panel">
        <span class="account-avatar" aria-hidden="true">${esc(accountInitial)}</span>
        <div class="account-info">
          <div class="account-name">${esc(accountName)}</div>
          <div class="field-hint">${esc(accountEmail)} · 会话有效期 7 天</div>
        </div>
        <div class="account-actions">
          <button class="btn" id="editAccountBtn" type="button">编辑账号</button>
          <button class="btn" id="changePasswordBtn" type="button">修改密码</button>
          <a class="btn" href="/status" target="_blank" rel="noopener">公开状态页 ↗</a>
          <button class="btn danger" id="logoutBtn" type="button">退出登录</button>
        </div>
      </div>
      <p class="field-hint section-hint">
        账号资料与密码可在此直接修改，保存时需验证当前密码；修改后当前浏览器自动续签，其他设备需重新登录。
        忘记密码时需在服务器上停止服务，执行 <code>rollcall-server -reset-admin-password</code> 重置一次后再启动（见 README「重置管理员密码」）。
      </p>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">安全与自动化</h2>
        <span class="section-meta">两步验证与范围化 API 令牌</span>
      </div>
      <div class="panel panel-pad inline-panel">
        <span class="badge ${totpOn ? 'ok' : 'mute'}">${totpOn ? '两步验证已启用' : '两步验证未启用'}</span>
        <span class="section-meta">登录时在密码之外额外校验身份验证器动态码（TOTP，30 秒轮换）。</span>
        <button class="btn ${totpOn ? 'danger' : 'primary'}" id="totpBtn" type="button">${totpOn ? '停用两步验证' : '启用两步验证'}</button>
      </div>
      <div class="panel panel-pad inline-panel">
        <span class="badge ${repSec && repSec.requireSignature ? 'ok' : 'mute'}">${repSec && repSec.requireSignature ? '已要求签名上报' : '未强制签名上报'}</span>
        <span class="section-meta">
          上报带时间戳与 HMAC-SHA256 签名，服务端只接受 ±${repSec ? repSec.windowSeconds : 90} 秒内的请求，重复 nonce 直接拒绝。
          关闭时未签名的旧 Agent 仍可上报${repSec && repSec.signedCount ? `（当前 ${repSec.signedCount} 台已签名` : ''}${repSec && (repSec.unsignedNodes || []).length ? `，未签名：${esc((repSec.unsignedNodes || []).join('、'))}）` : repSec && repSec.signedCount ? '）' : ''}。
        </span>
        <button class="btn ${repSec && repSec.requireSignature ? 'danger' : 'primary'}" id="repSigBtn" type="button">${repSec && repSec.requireSignature ? '取消强制' : '要求签名上报'}</button>
      </div>
      <div class="panel panel-pad">
        <div class="toolbar token-form">
          <input id="tokenName" type="text" maxlength="64" placeholder="令牌名称，例如 CI 只读" class="w-name">
          <select id="tokenScope">
            <option value="read">只读（read）</option>
            <option value="admin">管理（admin）</option>
          </select>
          <input id="tokenPaths" type="text" maxlength="400" placeholder="路径限定（可选，逗号分隔）" class="w-paths">
          <select id="tokenTTL">
            <option value="30">30 分钟</option>
            <option value="120">2 小时</option>
            <option value="1440">24 小时</option>
            <option value="10080">7 天</option>
            <option value="43200" selected>30 天</option>
            <option value="0">永不过期</option>
          </select>
          <button class="btn primary" id="createTokenBtn" type="button">创建令牌</button>
        </div>
        <p class="field-hint section-hint">
          使用 <code>Authorization: Bearer rct_…</code> 调用 API；令牌只保存 SHA-256 摘要，明文仅在创建时显示一次。
          管理范围可执行写操作，请仅授予可信自动化。路径限定可把令牌收窄到
          <code>/api/overview</code>、<code>/api/nodes/n1</code> 等前缀（留空=全部）。
        </p>
        <div class="table-wrap"><table class="queue-table" data-page-size="10">
          <thead><tr><th>名称</th><th>前缀</th><th>范围</th><th>路径限定</th><th>创建时间</th><th>过期时间</th><th>最近使用</th><th class="num">操作</th></tr></thead>
          <tbody>${tokenRows}</tbody>
        </table></div>
      </div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">外观主题</h2>
        <span class="section-meta">仅固定字段，不下发 CSS 文本</span>
      </div>
      <div class="panel panel-pad">
        <form id="themeForm" class="form-grid">
          ${selectField('默认模式', 'mode', themeCfg.mode || 'auto',
            [{ v: 'auto', t: '跟随系统' }, { v: 'light', t: '浅色' }, { v: 'dark', t: '深色' }])}
          <label class="field">
            <span class="field-label">浅色强调色</span>
            <input type="color" name="accent" value="${esc(themeCfg.accent || THEME_DEFAULT_ACCENT)}">
          </label>
          <label class="field">
            <span class="field-label">深色强调色</span>
            <input type="color" name="accentDark" value="${esc(themeCfg.accentDark || THEME_DEFAULT_ACCENT_DARK)}">
          </label>
          <label class="field">
            <span class="field-label">圆角（px）</span>
            <input type="number" name="radius" min="0" max="24" value="${themeCfg.radius == null ? 10 : esc(themeCfg.radius)}">
          </label>
          ${checkbox('紧凑布局', 'themeCompact', !!themeCfg.compact)}
          <div class="form-actions">
            <button class="btn" type="button" id="themePreview">预览</button>
            <button class="btn" type="button" id="themeReset">恢复默认</button>
            <button class="btn primary" type="submit">保存外观</button>
          </div>
        </form>
        <p class="field-hint section-hint">
          强调色、圆角与紧凑密度对仪表盘和公开状态页生效；默认模式只是访客的初始值，右上角手动切换过则优先本机选择。
        </p>
      </div>
      <div class="panel panel-pad" style="margin-top:10px">
        <form id="themePackForm" class="form-grid">
          ${selectField('公开状态页主题包', 'statusTheme', themePacks.active || '', packOpts)}
          <div class="form-actions">
            <button class="btn" type="button" id="themePackRefresh">重新扫描</button>
            <button class="btn primary" type="submit">应用主题包</button>
          </div>
        </form>
        <p class="field-hint section-hint">
          主题包放在 <code>${esc(themeDirHint)}</code>：每个目录一个 <code>theme.json</code>，
          可含 <code>status.css</code> 与 <code>status.js</code>（至少一个），仅作用于公开状态页。
          <strong>主题脚本与站点同源运行，安装主题等同授予管理员权限，只使用可信来源。</strong>
        </p>
        ${themePacks.warning ? `<p class="field-hint" style="color:var(--warn)">${esc(themePacks.warning)}</p>` : ''}
        ${badPacks.length ? `<p class="field-hint" style="color:var(--warn)">未通过校验：${badPacks.map((p) => esc(p.name) + '（' + esc(p.error) + '）').join('；')}</p>` : ''}
      </div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">加密备份与恢复</h2>
        <span class="section-meta">AES-256-GCM · 仅配置文件，不含监控历史</span>
      </div>
      <div class="panel panel-pad">
        <div class="grid-2">
          <div>
            <div class="sub-title">导出备份</div>
            <div class="form-grid">
              ${field('备份密码', 'backupPwd', '', { type: 'password', hint: '至少 8 个字符；恢复时需要同一密码' })}
            </div>
            <div class="form-actions start">
              <button class="btn primary" id="backupBtn" type="button">下载加密备份</button>
            </div>
          </div>
          <div>
            <div class="sub-title">导入备份</div>
            <div class="form-grid">
              ${field('备份密码', 'restorePwd', '', { type: 'password' })}
              <label class="field">
                <span class="field-label">备份文件（.rcbk）</span>
                <input type="file" id="restoreFile" accept=".rcbk,text/plain">
              </label>
              ${checkbox('我确认导入会覆盖备份中包含的配置项', 'restoreConfirm', false)}
            </div>
            <div class="form-actions start">
              <button class="btn danger" id="restoreBtn" type="button">导入并覆盖配置</button>
            </div>
          </div>
        </div>
        <p class="field-hint section-hint">
          备份包含节点、探测、规则、静默、续费与设置（含节点令牌）；导入不会修改监控历史与告警记录。
          导入后通知配置与节点令牌立即生效，请妥善保管备份文件与密码。
        </p>
      </div>
      <div class="panel panel-pad" style="margin-top:10px">
        <div class="section-head" style="margin-bottom:10px">
          <div class="sub-title">自动备份</div>
          <span class="badge ${ab.enabled ? 'ok' : 'mute'}">${ab.enabled ? '已启用' : '未启用'}</span>
          ${sectionActions(ab.enabled ? `每 ${dur(ab.intervalSeconds || 86400)} · 保留 ${ab.keep || 0} 份` : '',
            ab.enabled ? '<button class="btn tiny primary" id="autoBackupRun" type="button">立即备份</button>' : '')}
        </div>
        ${ab.enabled ? `
          <div class="grid-2" style="gap:18px">
            <div>
              <div class="kv-row"><span class="kv-key">最近运行</span><span class="kv-val">${ab.lastAt ? clock(ab.lastAt) : '尚未运行'}</span></div>
              <div class="kv-row"><span class="kv-key">下次运行</span><span class="kv-val">${ab.nextAt ? clock(ab.nextAt) : '—'}</span></div>
              <div class="kv-row"><span class="kv-key">累计运行</span><span class="kv-val">${ab.runs || 0} 次 · 失败 ${ab.failures || 0} 次</span></div>
              <div class="kv-row"><span class="kv-key">最近文件</span><span class="kv-val mono" style="font-size:11.5px">${esc(ab.lastFile || '—')}</span></div>
            </div>
            <div>
              <div class="kv-row"><span class="kv-key">备份目录</span><span class="kv-val mono" style="font-size:11.5px">${esc(ab.dir || '—')}</span></div>
              <div class="kv-row"><span class="kv-key">已有备份</span><span class="kv-val">${(ab.files || []).length} 份 · ${bytes(ab.totalBytes || 0)}</span></div>
              <div class="kv-row"><span class="kv-key">最近校验</span><span class="kv-val mono" style="font-size:11.5px">${esc(ab.lastChecksum || '—')}${ab.lastSize ? ' · ' + bytes(ab.lastSize) : ''}</span></div>
              <div class="kv-row"><span class="kv-key">最新文件</span><span class="kv-val">${abLatest ? clock(abLatest.modTime) : '—'}</span></div>
            </div>
          </div>
          ${ab.lastError ? `<p class="field-hint" style="color:var(--crit)">最近一次失败：${esc(ab.lastError)}</p>` : ''}
          <p class="field-hint section-hint">
            自动备份使用服务端配置的备份密码加密，写入前会解密回读比对，确认文件可用后才计为成功；失败会进入通知队列。
            调整间隔与保留份数请修改服务端启动参数 <code class="code-inline">-backup-interval</code> / <code class="code-inline">-backup-keep</code>。
          </p>` : `
          <p class="field-hint section-hint">${esc(ab.reason || '未启用：启动时需提供备份密码（-backup-password-file 或 ROLLCALL_BACKUP_PASSWORD）。')}</p>`}
      </div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">存储与容量</h2>
        <span class="section-meta">数据目录 ${esc(stg.dataDir || '—')} · 合计 ${bytes(stgTotal)}</span>
      </div>
      <div class="panel panel-pad">
        <div class="grid-2" style="gap:18px">
          <div>
            <div class="sub-title" style="margin-bottom:8px">目录占用</div>
            ${storageBars}
          </div>
          <div>
            <div class="sub-title" style="margin-bottom:8px">历史与保留</div>
            <div class="kv-row"><span class="kv-key">1 分钟粒度</span><span class="kv-val">${stgTotals['1m'] || 0} 点 · 保留 ${stgDays['1m'] || 0} 天</span></div>
            <div class="kv-row"><span class="kv-key">5 分钟粒度</span><span class="kv-val">${stgTotals['5m'] || 0} 点 · 保留 ${stgDays['5m'] || 0} 天</span></div>
            <div class="kv-row"><span class="kv-key">1 小时粒度</span><span class="kv-val">${stgTotals['1h'] || 0} 点 · 保留 ${stgDays['1h'] || 0} 天</span></div>
            <div class="kv-row"><span class="kv-key">审计日志</span><span class="kv-val">${stgAudit.count || 0} 条 · ${bytes(stgAudit.bytes || 0)} · 保留 ${stgAudit.retentionDays || 0} 天</span></div>
            <div class="kv-row"><span class="kv-key">自动备份</span><span class="kv-val">${stg.backup && stg.backup.enabled ? `${stg.backup.files || 0} 份 · ${bytes(stg.backup.totalBytes || 0)}` : '未启用'}</span></div>
          </div>
        </div>
        <details style="margin-top:10px">
          <summary class="sum">按节点查看历史点数</summary>
          <div class="table-wrap" style="margin-top:8px"><table class="maint-table" data-page-size="10">
            <thead><tr><th>节点</th><th class="num">1 分钟</th><th class="num">5 分钟</th><th class="num">1 小时</th></tr></thead>
            <tbody>${histNodeRows}</tbody>
          </table></div>
        </details>
      </div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">数据导出</h2>
        <span class="section-meta">CSV 带 BOM，Excel 可直接打开；指标导出含 24 小时全量采样</span>
      </div>
      <div class="panel panel-pad">
        <div class="toolbar">
          <button class="btn" data-exp="assets">资产台账 CSV</button>
          <button class="btn" data-exp="alerts">告警历史 CSV</button>
          <select id="expNode">
            ${nodes.map((n) => `<option value="${esc(n.id)}">${esc(n.name)}</option>`).join('')}
          </select>
          <select id="expRange">
            <option value="1h">最近 1 小时</option>
            <option value="6h">最近 6 小时</option>
            <option value="24h" selected>最近 24 小时</option>
            <option value="7d">最近 7 天</option>
            <option value="30d">最近 30 天</option>
            <option value="365d">最近 365 天</option>
          </select>
          <button class="btn" data-exp="metrics">指标 CSV</button>
        </div>
        <div class="toolbar" style="margin-top:9px">
          <select id="expArchiveRange" aria-label="归档时间范围">
            <option value="24h" selected>归档：最近 24 小时（5 分钟粒度）</option>
            <option value="7d">归档：最近 7 天（1 小时粒度）</option>
            <option value="30d">归档：最近 30 天（1 小时粒度）</option>
            <option value="365d">归档：最近一年（1 小时粒度）</option>
          </select>
          <button class="btn" data-exp="history">全机群历史归档 CSV</button>
        </div>
        <p class="field-hint section-hint">
          Prometheus 抓取端点：<code class="code-inline">GET /metrics</code>
          （Basic Auth 密码填管理令牌，用户名任意）；接口延迟、5xx 与 SSE 连接数均已输出。
        </p>
      </div>
    </section>
    ${capMatrixHTML()}`;

  // 实例健康轮询：每次进入设置页重置定时器，避免离开后堆积多个 interval。
  if (healthTimer) clearInterval(healthTimer);
  healthTimer = setInterval(refreshHealthCard, 5000);

  // 事件绑定
  $('#logoutBtn').onclick = doLogout;
  $('#editAccountBtn').onclick = showAccountDialog;
  $('#changePasswordBtn').onclick = showPasswordDialog;

  /* ---- 两步验证 ---- */
  $('#totpBtn').onclick = () => {
    if (String((state.user || {}).totpEnabled) === 'true') showTOTPDisableDialog();
    else showTOTPSetupDialog();
  };

  /* ---- 上报签名策略 ---- */
  const repSigBtn = $('#repSigBtn');
  if (repSigBtn) {
    repSigBtn.onclick = async () => {
      const on = !(repSec && repSec.requireSignature);
      const pending = (repSec && repSec.unsignedNodes) || [];
      // 开启会把没签名的节点踢下线（它们会一直 401），必须先说清楚。
      if (on && pending.length) {
        const ok = await modalConfirm('这些节点会立刻失联',
          `以下 ${pending.length} 台节点还没有签名上报，开启后会立刻被 401 拒绝、在面板上显示离线：${pending.join('、')}。建议先把它们升到 v1.16.0 再开启。`,
          { danger: true, okText: '仍然开启' });
        if (!ok) return;
      }
      repSigBtn.disabled = true;
      try {
        await adminApi('/api/admin/report-security', {
          method: 'PUT', body: JSON.stringify({ requireSignature: on }),
        });
        toast(on ? '已要求签名上报' : '已取消强制签名');
        viewSettings();
      } catch (e) {
        toast('保存失败：' + e.message, 'err');
      } finally {
        repSigBtn.disabled = false;
      }
    };
  }

  /* ---- API 令牌 ---- */
  $('#createTokenBtn').onclick = async () => {
    const name = $('#tokenName').value.trim();
    if (!name) { toast('请填写令牌名称', 'err'); return; }
    const btn = $('#createTokenBtn');
    btn.disabled = true;
    try {
      const r = await adminApi('/api/admin/tokens', {
        method: 'POST',
        body: JSON.stringify({
          name,
          scopes: [$('#tokenScope').value],
          paths: $('#tokenPaths').value.split(',').map((s) => s.trim()).filter(Boolean),
          expiresMinutes: Number($('#tokenTTL').value) || 0,
        }),
      });
      showRawToken(r.token, r.meta);
      viewSettings();
    } catch (e) {
      toast('创建失败：' + e.message, 'err');
    } finally {
      btn.disabled = false;
    }
  };
  $$('[data-del-token]').forEach((b) => {
    b.onclick = async () => {
      if (!(await modalConfirm('删除 API 令牌', '删除该 API 令牌？使用它的自动化会立即失去访问权限。',
        { danger: true, okText: '删除' }))) return;
      try {
        await adminApi('/api/admin/tokens/' + encodeURIComponent(b.dataset.delToken), { method: 'DELETE' });
        toast('令牌已删除');
        viewSettings();
      } catch (e) { toast('删除失败：' + e.message, 'err'); }
    };
  });

  /* ---- 加密备份 ---- */
  $('#backupBtn').onclick = async () => {
    const pwd = $('#view [name="backupPwd"]').value;
    if (pwd.length < 8) { toast('备份密码至少 8 个字符', 'err'); return; }
    const btn = $('#backupBtn');
    btn.disabled = true;
    try {
      const r = await adminApi('/api/admin/backup', { method: 'POST', body: JSON.stringify({ password: pwd }) });
      // 保存 base64 文本而不是原始字节：导入端按文本读取后原样回传，
      // 文本格式也便于通过密码管理器/配置仓库传递。
      const blob = new Blob([r.data + '\n'], { type: 'text/plain' });
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = r.filename || 'rollcall-backup.rcbk';
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 4000);
      toast('备份已生成（校验 ' + r.checksum + '）');
    } catch (e) {
      toast('导出失败：' + e.message, 'err');
    } finally {
      btn.disabled = false;
    }
  };
  $('#restoreBtn').onclick = async () => {
    const pwd = $('#view [name="restorePwd"]').value;
    const file = $('#restoreFile').files[0];
    const confirmBox = $('#view [name="restoreConfirm"]');
    if (!file) { toast('请选择备份文件', 'err'); return; }
    if (!pwd) { toast('请输入备份密码', 'err'); return; }
    if (!confirmBox.checked) { toast('请先勾选确认覆盖配置', 'err'); return; }
    if (!(await modalConfirm('恢复配置备份',
      '导入会覆盖备份中的节点、探测、规则与设置（本机独有的设置项保留），且无法撤销。继续？',
      { danger: true, okText: '继续导入' }))) return;
    const btn = $('#restoreBtn');
    btn.disabled = true;
    try {
      const data = (await file.text()).trim();
      const r = await adminApi('/api/admin/restore', {
        method: 'POST',
        body: JSON.stringify({ password: pwd, data, confirm: true }),
      });
      toast('导入完成：' + r.nodes + ' 节点 / ' + r.services + ' 服务 / ' + r.rules + ' 规则');
      viewSettings();
    } catch (e) {
      toast('导入失败：' + e.message, 'err');
    } finally {
      btn.disabled = false;
    }
  };

  /* ---- 自动备份 ---- */
  const autoBkRun = $('#autoBackupRun');
  if (autoBkRun) {
    autoBkRun.onclick = async () => {
      autoBkRun.disabled = true;
      try {
        const r = await adminApi('/api/admin/backup/auto/run', { method: 'POST' });
        const res = (r && r.result) || {};
        toast('自动备份完成：' + (res.file || '已写入') + (res.checksum ? '（' + res.checksum + '）' : ''));
        viewSettings();
      } catch (e) {
        toast('自动备份失败：' + e.message, 'err');
        autoBkRun.disabled = false;
      }
    };
  }

  /* ---- 外观主题 ---- */
  const themeForm = $('#themeForm');
  const readThemeForm = () => {
    const d = formData(themeForm);
    return {
      mode: d.mode,
      accent: d.accent,
      accentDark: d.accentDark,
      radius: d.radius === '' ? null : Number(d.radius),
      compact: !!d.themeCompact,
    };
  };
  const previewTheme = () => {
    serverTheme = readThemeForm();
    applyThemeVars(document.documentElement.dataset.theme || 'light');
  };
  $('#themePreview').onclick = () => {
    previewTheme();
    toast('已在本机预览，保存后对所有访客生效');
  };
  $('#themeReset').onclick = () => {
    themeForm.querySelector('[name=mode]').value = 'auto';
    themeForm.querySelector('[name=accent]').value = THEME_DEFAULT_ACCENT;
    themeForm.querySelector('[name=accentDark]').value = THEME_DEFAULT_ACCENT_DARK;
    themeForm.querySelector('[name=radius]').value = '10';
    themeForm.querySelector('[name=themeCompact]').checked = false;
    previewTheme();
    toast('已恢复默认预览，保存后生效');
  };
  themeForm.onsubmit = async (e) => {
    e.preventDefault();
    try {
      const body = readThemeForm();
      const r = await adminApi('/api/admin/theme', { method: 'PUT', body: JSON.stringify(body) });
      serverTheme = r.theme || body;
      applyThemeVars(document.documentElement.dataset.theme || 'light');
      toast('外观配置已保存');
    } catch (err) { toast('保存失败：' + err.message, 'err'); }
  };

  const themePackForm = $('#themePackForm');
  $('#themePackRefresh').onclick = () => viewSettings();
  themePackForm.onsubmit = async (e) => {
    e.preventDefault();
    const name = themePackForm.querySelector('[name=statusTheme]').value;
    try {
      await adminApi('/api/admin/theme-packs', { method: 'PUT', body: JSON.stringify({ name }) });
      toast(name ? '已应用主题包' : '已恢复内置主题');
      viewSettings();
    } catch (err) { toast('应用失败：' + err.message, 'err'); }
  };

  $$('[data-retry-job]').forEach((b) => {
    b.onclick = async () => {
      b.disabled = true;
      try {
        await adminApi('/api/admin/notify/jobs/' + encodeURIComponent(b.dataset.retryJob) + '/retry', { method: 'POST' });
        toast('已重新入队');
        viewSettings();
      } catch (e) {
        toast('重试失败：' + e.message, 'err');
        b.disabled = false;
      }
    };
  });

  // CSV 导出：同源会话 Cookie 鉴权，下载为文件
  $$('[data-exp]').forEach((b) => {
    b.onclick = async () => {
      let url = '/api/export/' + b.dataset.exp + '.csv';
      if (b.dataset.exp === 'metrics') {
        const nid = $('#expNode') ? $('#expNode').value : '';
        const rng = $('#expRange') ? $('#expRange').value : '24h';
        if (!nid) { toast('没有可导出的节点', 'err'); return; }
        url += '?node=' + encodeURIComponent(nid) + '&range=' + rng;
      } else if (b.dataset.exp === 'history') {
        const rng = $('#expArchiveRange') ? $('#expArchiveRange').value : '24h';
        url += '?range=' + rng;
      }
      b.disabled = true;
      try {
        const r = await fetch(url, { credentials: 'same-origin', headers: { Accept: 'text/csv' } });
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const blob = await r.blob();
        const cd = r.headers.get('Content-Disposition') || '';
        const m = cd.match(/filename="([^"]+)"/);
        const a = document.createElement('a');
        a.href = URL.createObjectURL(blob);
        a.download = m ? m[1] : (b.dataset.exp + '.csv');
        document.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(() => URL.revokeObjectURL(a.href), 4000);
        toast('已开始下载');
      } catch (e) {
        toast('导出失败：' + e.message, 'err');
      } finally {
        b.disabled = false;
      }
    };
  });

  $$('[data-agent-node]').forEach((b) => {
    b.onclick = async () => {
      try {
        const n = await adminApi('/api/admin/nodes/' + encodeURIComponent(b.dataset.agentNode));
        showAgentConfig(n);
      } catch (e) { toast('读取接入信息失败：' + e.message, 'err'); }
    };
  });

  $$('[data-edit-node]').forEach((b) => {
    b.onclick = () => {
      const n = nodes.find((x) => x.id === b.dataset.editNode);
      if (n) showNodeEdit(n);
    };
  });

  $$('[data-del-node]').forEach((b) => {
    b.onclick = async () => {
      if (!(await modalConfirm('删除节点', '确认删除该节点？其监控数据也会一并移除。',
        { danger: true, okText: '删除' }))) return;
      try {
        await adminApi('/api/admin/nodes/' + b.dataset.delNode, { method: 'DELETE' });
        toast('节点已删除'); viewSettings();
      } catch (e) { toast('删除失败：' + e.message, 'err'); }
    };
  });

  $$('[data-del-svc]').forEach((b) => {
    b.onclick = async () => {
      if (!(await modalConfirm('删除探测目标', '确认删除该探测目标？', { danger: true, okText: '删除' }))) return;
      try {
        await adminApi('/api/admin/services/' + b.dataset.delSvc, { method: 'DELETE' });
        toast('探测目标已删除'); viewSettings();
      } catch (e) { toast('删除失败：' + e.message, 'err'); }
    };
  });

  $('#panelLinkForm').onsubmit = async (e) => {
    e.preventDefault();
    const input = e.target.querySelector('input[name=link]');
    const btn = e.target.querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      const r = await adminApi('/api/admin/settings/panel-link', {
        method: 'POST',
        body: JSON.stringify({ link: (input.value || '').trim() }),
      });
      state.panelLink = r.panelLink || '';
      input.value = state.panelLink;
      toast(state.panelLink ? '已保存：' + state.panelLink : '已清空，改用当前访问地址');
    } catch (err) {
      // 后端会把「不支持子路径」「缺少 scheme」等原因原样返回，直接显示比笼统报错有用
      toast('保存失败：' + err.message, 'err');
    } finally {
      btn.disabled = false;
    }
  };

  $('#nodeImportForm').onsubmit = async (e) => {
    e.preventDefault();
    const raw = (e.target.querySelector('textarea[name=list]').value || '');
    const rows = raw.split(/\r?\n/).map((x) => x.trim()).filter((x) => x && !x.startsWith('#'));
    if (!rows.length) { toast('没有可导入的行', 'err'); return; }
    // 列序固定：名称,服务商,地区,国家,分组；多余列忽略。
    const nodes = rows.map((line) => {
      const c = line.split(/[,\t]/).map((x) => x.trim());
      return { name: c[0] || '', provider: c[1] || '', region: c[2] || '',
        country: (c[3] || '').toUpperCase(), group: c[4] || '' };
    });
    const btn = e.target.querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      const r = await adminApi('/api/admin/nodes/import', { method: 'POST', body: JSON.stringify({ nodes }) });
      showImportResult(r);
    } catch (err) {
      toast('导入失败：' + err.message, 'err');
    } finally {
      btn.disabled = false;
    }
  };

  $('#nodeForm').onsubmit = async (e) => {
    e.preventDefault();
    const d = formData(e.target);
    try {
      const r = await adminApi('/api/admin/nodes', { method: 'POST', body: JSON.stringify({
        name: d.name, provider: d.provider, region: d.region, group: d.group,
        country: (d.country || '').toUpperCase(),
        expireAt: d.expireAt, price: Number(d.price) || 0, currency: d.currency,
        trafficQuotaGb: Number(d.trafficQuotaGb) || 0,
        trafficResetDay: Number(d.trafficResetDay) || 0,
        trafficMode: d.trafficMode || 'sum',
        tags: splitTags(d.tags),
        note: d.note, weight: Number(d.weight) || 0, favorite: !!d.favorite,
      }) });
      toast('节点已创建');
      showAgentConfig(r);
      viewSettings();
    } catch (err) { toast('创建失败：' + err.message, 'err'); }
  };

  const upAdd = $('#upBatchAdd');
  if (upAdd) upAdd.onclick = () => openUpgradeBatchDialog(nodes);
  $$('[data-upgrade-del]').forEach((b) => {
    b.onclick = async () => {
      if (!(await modalConfirm('删除升级清单', '删除这条升级清单记录？不会影响节点本身。',
        { danger: true, okText: '删除' }))) return;
      try {
        await adminApi('/api/admin/agent-upgrades/' + encodeURIComponent(b.dataset.upgradeDel), { method: 'DELETE' });
        toast('升级清单已删除');
        viewSettings();
      } catch (e) { toast('删除失败：' + e.message, 'err'); }
    };
  });

  $('#settingsSvcAdd').onclick = () => openServiceDialog(null, nodes, viewSettings);
  $$('[data-svc-setting-test]').forEach((b) => {
    b.onclick = () => {
      const svc = services.find((x) => x.id === b.dataset.svcSettingTest);
      if (svc) testServiceNow(svc, b);
    };
  });
  $$('[data-svc-setting-edit]').forEach((b) => {
    b.onclick = () => {
      const svc = services.find((x) => x.id === b.dataset.svcSettingEdit);
      if (svc) openServiceDialog(svc, nodes, viewSettings);
    };
  });
  $$('[data-svc-setting-toggle]').forEach((b) => {
    b.onclick = async () => {
      const svc = services.find((x) => x.id === b.dataset.svcSettingToggle);
      if (!svc) return;
      b.disabled = true;
      try {
        await adminApi('/api/admin/services/' + encodeURIComponent(svc.id), {
          method: 'PUT',
          body: JSON.stringify({
            name: svc.name, type: svc.type, target: svc.target,
            interval: svc.interval, nodeId: svc.nodeId || '', enabled: svc.enabled === false,
          }),
        });
        toast(svc.enabled === false ? '探测已启用' : '探测已停用');
        viewSettings();
      } catch (e) {
        toast('操作失败：' + e.message, 'err');
        b.disabled = false;
      }
    };
  });

  /* ---- 通道动态渲染 ---- */
  let chState = JSON.parse(JSON.stringify(cfg.channels || []));

  const defMap = {};
  defs.forEach((d) => { defMap[d.type] = d; });

  function channelCard(ch) {
    const d = defMap[ch.type];
    if (!d) return '';
    return `<div class="ch-card" data-id="${esc(ch.id)}" data-type="${esc(ch.type)}">
      <div class="ch-head">
        <span class="badge mute">${esc(d.label)}</span>
        <input class="ch-name" value="${esc(ch.name || d.label)}" placeholder="通道名称">
        <label class="check inline"><input type="checkbox" class="ch-en"${ch.enabled ? ' checked' : ''}><span>启用</span></label>
        <button class="btn tiny" data-act="test" type="button">测试</button>
        <button class="btn tiny danger" data-act="del" type="button">删除</button>
      </div>
      <div class="form-grid ch-fields">
        ${d.fields.map((f) => `<label class="field">
          <span class="field-label">${esc(f.label)}${f.required ? ' *' : ''}</span>
          <input type="${f.type === 'number' ? 'number' : (f.secret ? 'password' : 'text')}"
                 class="ch-opt" data-key="${esc(f.key)}"
                 value="${esc((ch.options || {})[f.key] || f.default || '')}"
                 placeholder="${esc(f.placeholder || '')}" autocomplete="off">
          ${f.hint ? `<span class="field-hint">${esc(f.hint)}</span>` : ''}
        </label>`).join('')}
      </div>
    </div>`;
  }

  function renderChannels() {
    const box = $('#chList');
    if (!chState.length) {
      box.innerHTML = `<div class="panel panel-pad" style="text-align:center;color:var(--fg-3);font-size:12.5px;margin-bottom:12px">
        还没有通知通道。从下方选择类型后添加，支持企业微信 / 钉钉 / 飞书 / Telegram / Bark / Server酱 / PushPlus / Gotify / 邮件 / Webhook。
      </div>`;
      return;
    }
    box.innerHTML = chState.map(channelCard).join('');
    $$('.ch-card', box).forEach((card) => {
      const id = card.dataset.id;
      card.querySelector('[data-act=del]').onclick = () => {
        chState = chState.filter((x) => x.id !== id);
        renderChannels();
      };
      card.querySelector('[data-act=test]').onclick = async (ev) => {
        const btn = ev.target;
        btn.disabled = true; btn.textContent = '发送中…';
        try {
          collectChannels();
          await apiSaveSettings(true); // 先保存再测，确保后端用的是最新配置
          const r = await adminApi('/api/admin/test-notify', {
            method: 'POST', body: JSON.stringify({ channelId: id }),
          });
          const res = r.results || [];
          const ok = res.filter((x) => x.ok);
          const bad = res.filter((x) => !x.ok);
          if (ok.length) toast('测试通知已送达：' + ok.map((x) => x.channel).join(', '));
          if (bad.length) toast(bad.map((x) => x.channel + ' — ' + x.error).join('；'), 'err');
        } catch (e) {
          toast('测试失败：' + e.message, 'err');
        } finally {
          btn.disabled = false; btn.textContent = '测试';
        }
      };
    });
  }

  // 把 DOM 里编辑的内容写回 chState
  function collectChannels() {
    $$('.ch-card', $('#chList')).forEach((card) => {
      const id = card.dataset.id;
      const ch = chState.find((x) => x.id === id);
      if (!ch) return;
      ch.name = card.querySelector('.ch-name').value.trim();
      ch.enabled = card.querySelector('.ch-en').checked;
      ch.options = ch.options || {};
      card.querySelectorAll('.ch-opt').forEach((inp) => {
        ch.options[inp.dataset.key] = inp.value.trim();
      });
    });
  }

  async function apiSaveSettings(silent) {
    collectChannels();
    const v = (name) => {
      const el = document.querySelector('#view [name="' + name + '"]');
      return el ? el.value : '';
    };
    const c = (name) => {
      const el = document.querySelector('#view [name="' + name + '"]');
      return !!(el && el.checked);
    };
    const body = {
      enabled: c('nEnabled'),
      minSeverity: v('nMinSeverity'),
      repeatInterval: Number(v('nRepeat')) || 0,
      quietStart: Number(v('quietStart')),
      quietEnd: Number(v('quietEnd')),
      digestMins: Number(v('digestMins')) || 0,
      expiryDays: Number(v('expiryDays')),
      titleFormat: v('titleFormat').trim(),
      bodyFormat: v('bodyFormat').trim(),
      reportDaily: c('reportDaily'),
      reportWeekly: c('reportWeekly'),
      reportMonthly: c('reportMonthly'),
      reportHour: Number(v('reportHour')),
      offlineGraceSec: Math.max(0, Number(v('nOfflineGrace')) || 0),
      channels: chState,
    };
    const r = await adminApi('/api/admin/settings', { method: 'POST', body: JSON.stringify(body) });
    if (!silent) {
      toast('通知配置已保存' + (r.channels && r.channels.length ? '（已启用：' + r.channels.join(' / ') + '）' : ''));
      viewSettings();
    }
    return r;
  }

  renderChannels();

  $('#chType').onchange = (e) => {
    const d = defMap[e.target.value];
    $('#chDesc').textContent = d ? d.desc : '';
  };
  $('#chDesc').textContent = (defMap[$('#chType').value] || {}).desc || '';

  $('#addCh').onclick = () => {
    const type = $('#chType').value;
    const d = defMap[type];
    if (!d) return;
    collectChannels();
    chState.push({
      id: 'ch_' + Math.random().toString(36).slice(2, 10),
      type,
      name: d.label,
      enabled: true,
      options: {},
    });
    renderChannels();
    toast('已添加「' + d.label + '」，填写参数后保存');
  };

  $('#saveNotify').onclick = async () => {
    try { await apiSaveSettings(false); } catch (e) { toast('保存失败：' + e.message, 'err'); }
  };

  $('#testAll').onclick = async () => {
    const btn = $('#testAll');
    btn.disabled = true; btn.textContent = '发送中…';
    try {
      await apiSaveSettings(true);
      const r = await adminApi('/api/admin/test-notify', { method: 'POST', body: '{}' });
      const res = r.results || [];
      const ok = res.filter((x) => x.ok);
      const bad = res.filter((x) => !x.ok);
      if (ok.length) toast('已送达 ' + ok.length + ' 个通道：' + ok.map((x) => x.channel).join(', '));
      if (bad.length) toast(bad.map((x) => x.channel + ' — ' + x.error).join('；'), 'err');
    } catch (e) {
      toast('测试失败：' + e.message, 'err');
    } finally {
      btn.disabled = false; btn.textContent = '测试全部';
    }
  };
}

function isLoopbackHost(hostname) {
  return hostname === '127.0.0.1' || hostname === 'localhost' || hostname === '::1' || hostname === '[::1]';
}

// 安装脚本对非回环明文 HTTP 强制要求 --allow-insecure（下载即 root 执行）。
function installInsecureFlag(base) {
  let host = '';
  try { host = new URL(base).hostname; } catch (e) { return ''; }
  return base.startsWith('http://') && !isLoopbackHost(host) ? '--allow-insecure ' : '';
}

function agentServerURL() {
  const configured = (state.panelLink || '').trim().replace(/\/+$/, '');
  return configured || location.origin;
}

// showNodeUpgrade：给出一台机器的 Agent 升级命令。
//
// 为什么不是「点一下就远程升级」：本产品的硬边界是 Agent 只读、单向、
// 服务端不下发指令 —— 攻击面从任意 RCE 降为「只能伪造指标」。升级因此
// 只能给出在目标机器上执行的命令（与「接入」同理）。
// 命令走控制端自己的 install.sh --upgrade：脚本内 uname -m 自动识别架构、
// 下载后校验 SHA-256、替换二进制并重启，配置文件与令牌保持不变。
function showNodeUpgrade(n) {
  const base = agentServerURL();
  const cmd = `curl -fsSL ${base}/agent/install.sh | sh -s -- --upgrade`;
  const cur = n.agentVersion || '未上报';
  // 目标版本优先取节点自带的 serverVersion（/api/nodes 每个节点都带，
  // web.go 的节点级字段）；state.serverVersion 只在设置页加载过，
  // 直接打开节点页时为空，会让弹窗只显示「当前 vX（控制端版本）」而缺目标版本。
  const target = n.serverVersion || state.serverVersion || '';
  const dlg = openDialog('升级 Agent · ' + n.name, `
    <p class="modal-sub">当前 <span class="mono">${esc(cur)}</span>${target ? ` → 目标 <span class="mono">${esc(target)}</span>` : ''}（控制端版本）。</p>
    <div class="field">
      <span class="field-label">在 ${esc(n.name)} 上以 root 执行</span>
      <pre class="code" style="max-height:120px;overflow:auto;word-break:break-all">${esc(cmd)}</pre>
    </div>
    <div class="field-hint">脚本会：识别架构 → 从控制端下载 Agent → 校验 SHA-256 → 替换 /usr/local/bin/rollcall-agent → 重启服务。配置与上报令牌不受影响。</div>
    <div class="field-hint">多台设备建议先用「加入升级清单」建立批次（只做进度跟踪），再逐台执行命令；清单会按 Agent 版本自动计算完成数。</div>
    <div class="form-actions">
      <button class="btn" type="button" data-close>关闭</button>
      <button class="btn primary" type="button" id="upCmdCopy">复制命令</button>
    </div>`);
  const copyBtn = dlg.querySelector('#upCmdCopy');
  if (copyBtn) {
    copyBtn.onclick = async () => {
      try {
        await navigator.clipboard.writeText(cmd);
        toast('已复制升级命令');
      } catch (err) {
        toast('复制失败，请手动选中命令文本', 'err');
      }
    };
  }
}

// showAgentConfig 展示（或轮换后重新展示）节点接入指引
// showImportResult 展示批量导入结果：每台的接入命令 + 被跳过的行。
// 用弹窗而不是页面内嵌块：导入后要刷新节点列表，内嵌块会被重渲染清掉；
// 而这批命令正是用户接下来要逐台粘贴的东西，不能刷一下就没。
// timezoneLabel 把服务器时区显示成一眼能懂的形式。
// time.Local.String() 在 TZ 未设置时是 "Local"，单看它无法判断实际时区，
// 所以带上偏移："Local (UTC+0)" / "Asia/Shanghai (UTC+8)"。
function timezoneLabel() {
  const name = state.timezone || '跟随系统';
  const off = state.timezoneOffsetMin;
  if (off === null || off === undefined) return name;
  const sign = off < 0 ? '-' : '+';
  const abs = Math.abs(off);
  const h = Math.floor(abs / 60);
  const m = abs % 60;
  return `${name} (UTC${sign}${h}${m ? ':' + String(m).padStart(2, '0') : ''})`;
}

function showImportResult(r) {
  const created = r.created || [];
  const skipped = r.skipped || [];
  const cmds = created.map((c) => `# ${c.name}
${c.command}`).join('\n');
  const dlg = document.createElement('div');
  dlg.className = 'modal-mask';
  dlg.innerHTML = `<div class="modal" style="max-width:720px">
    <div class="modal-title">批量导入结果</div>
    <p class="modal-sub">已创建 <b>${created.length}</b> 个节点${skipped.length ? `，跳过 <b>${skipped.length}</b> 行` : ''}。
      把下面的命令逐条贴到对应服务器的 root shell（每台一条，已含该节点自己的令牌）。</p>
    ${created.length ? `<pre class="code" style="max-height:240px;overflow:auto">${esc(cmds)}</pre>
      <div class="form-actions" style="justify-content:flex-start">
        <button class="btn tiny" id="copyImportCmds" type="button">复制全部命令</button>
      </div>` : ''}
    ${skipped.length ? `<p class="modal-sub" style="color:var(--warn)">跳过的行：<br>${skipped
      .map((x) => `第 ${x.index + 1} 行 ${esc(x.name || '(空)')} — ${esc(x.reason)}`).join('<br>')}</p>` : ''}
    <div class="form-actions"><button class="btn primary" id="importDone" type="button">完成</button></div>
  </div>`;
  document.body.appendChild(dlg);
  const close = () => { dlg.remove(); viewSettings(); };
  dlg.querySelector('#importDone').onclick = close;
  dlg.onclick = (e) => { if (e.target === dlg) close(); };
  const cb = dlg.querySelector('#copyImportCmds');
  if (cb) {
    cb.onclick = async () => {
      await navigator.clipboard.writeText(cmds);
      toast('已复制 ' + created.length + ' 条命令');
    };
  }
}

function showAgentConfig(node) {
  const server = agentServerURL();
  let host = '';
  try { host = new URL(server).hostname; } catch (e) { host = ''; }
  const insecure = server.startsWith('http://') && !isLoopbackHost(host);
  // 明文 HTTP 下安装脚本要求显式 --allow-insecure，否则拒绝以 root 执行下载的二进制。
  const oneCmd = `curl -fsSL ${server}/agent/install.sh | sh -s -- ${insecure ? '--allow-insecure ' : ''}${server} ${node.id} ${node.token}`;
  const cfg = JSON.stringify({
    server,
    nodeId: node.id,
    token: node.token,
    interval: 30,
  }, null, 2);

  const manual = `# 手动安装（脚本不可用时的备选路径）
install -m 0755 rollcall-agent /usr/local/bin/rollcall-agent
install -d -m 0700 /etc/rollcall
# 将下面的 JSON 写入 /etc/rollcall/agent.json（权限 0600）
# systemd 单元模板：curl -fsSLO https://raw.githubusercontent.com/motao123/rollcall/main/deploy/rollcall-agent.service
systemctl enable --now rollcall-agent`;

  const dlg = document.createElement('div');
  dlg.className = 'modal-mask';
  dlg.innerHTML = `<div class="modal" style="max-width:640px">
    <div class="modal-title">节点接入 · ${esc(node.name || node.id)}</div>
    <p class="modal-sub">在目标服务器的 root shell 里执行这一行，Agent 会自动下载、注册为 systemd 服务并开始上报：</p>
    <div class="onecmd">
      <pre class="code" id="oneCmdText">${esc(oneCmd)}</pre>
      <button class="btn primary" id="copyCmd" type="button">复制命令</button>
    </div>
    <p class="modal-sub" id="agentDirCheck" hidden></p>
    <p class="modal-sub" id="tlsPinLine" hidden></p>
    <p class="modal-sub" style="margin-top:10px">脚本会自动识别架构（amd64/arm64）；明文 HTTP 场景需显式加 --allow-insecure（上方命令已按当前地址自动补上）才会继续安装。约 30 秒后面板即可看到该节点。</p>
    ${insecure ? '<p class="modal-sub" style="color:var(--warn)">注意：控制端当前是明文 HTTP，节点令牌会在链路上明文传输。长期使用建议上 TLS 或保持隧道。</p>' : ''}
    <details style="margin-top:12px">
      <summary class="modal-sub" style="cursor:pointer">手动安装（备选）</summary>
      <pre class="code" style="margin-top:8px">${esc(manual)}</pre>
      <pre class="code" id="manualCfg">${esc(cfg)}</pre>
    </details>
    <p class="modal-sub" style="color:var(--warn)">令牌等同于该节点的上报凭据：轮换后旧令牌立即失效，需同步更新 agent.json 并重启 Agent。</p>
    <div class="form-actions">
      <button class="btn danger" id="rotateTok" type="button">轮换令牌</button>
      <button class="btn primary" id="dlgOk">完成</button>
    </div>
  </div>`;
  document.body.appendChild(dlg);
  const close = () => dlg.remove();
  dlg.querySelector('#dlgOk').onclick = close;
  dlg.onclick = (e) => { if (e.target === dlg) close(); };
  // 异步探测 Agent 分发是否启用：未配置 -agent-dir 时，上面这条命令会在
  // 下载步失败。与其让用户跑完再读报错，弹窗直接给指引（含正确顺序）。
  // 探测用任意合法 arch 即可——未启用时与 arch 无关地 404。
  fetch(`${server}/agent/download.sha256?arch=amd64`, { cache: 'no-store' })
    .then((r) => {
      if (r.ok) return;
      const p = dlg.querySelector('#agentDirCheck');
      if (!p) return;
      p.hidden = false;
      p.style.color = 'var(--warn)';
      p.textContent = '⚠ 服务端未启用 Agent 分发（启动参数 -agent-dir 未配置），上面的命令会下载失败。在服务端启动参数追加 -agent-dir <目录>、把 rollcall-agent-linux-<arch> 放进该目录并重启服务端后，这条命令即可使用；此前可先用下方「手动安装」。';
    })
    .catch(() => {});
  // 控制端证书指纹：Agent 装了它就会校验服务端身份，而不是盲信任何自签证书。
  // 显示出来是为了让安装者能人工核对——首次安装时链路若已被控制，
  // 脚本取到的指纹也会被替换，那是 TOFU 的固有盲区，只能靠人看。
  fetch(`${server}/agent/tls-pin`, { cache: 'no-store' })
    .then((r) => (r.ok ? r.text() : ''))
    .then((t) => {
      const pin = (t || '').trim();
      if (!pin) return;
      const line = dlg.querySelector('#tlsPinLine');
      const pre = dlg.querySelector('#manualCfg');
      if (line) {
        line.hidden = false;
        line.innerHTML = `服务端证书指纹 <code class="mono">${esc(pin)}</code>` +
          `<br>安装脚本会自动写进 agent.json；Agent 会拒绝指纹不符的控制端。换证书后需同步更新各节点配置。`;
      }
      if (pre) {
        pre.textContent = JSON.stringify({
          server, nodeId: node.id, token: node.token, interval: 30, serverCertPin: pin,
        }, null, 2);
      }
    })
    .catch(() => {});
  dlg.querySelector('#copyCmd').onclick = async (ev) => {
    try {
      await navigator.clipboard.writeText(oneCmd);
      ev.target.textContent = '已复制 ✓';
      setTimeout(() => { ev.target.textContent = '复制命令'; }, 1800);
    } catch (e) {
      // 非 https / 无剪贴板权限时退化为选中文本
      const range = document.createRange();
      range.selectNodeContents(dlg.querySelector('#oneCmdText'));
      const sel = getSelection(); sel.removeAllRanges(); sel.addRange(range);
      toast('已选中命令文本，请 Ctrl+C 复制');
    }
  };
  dlg.querySelector('#rotateTok').onclick = async (ev) => {
    if (!(await modalConfirm('轮换节点令牌',
      '确认轮换该节点的令牌？旧令牌会立即失效，现有 Agent 将停止上报，直到更新配置。',
      { danger: true, okText: '轮换' }))) return;
    ev.target.disabled = true;
    try {
      const updated = await adminApi('/api/admin/nodes/' + encodeURIComponent(node.id) + '/rotate-token', { method: 'POST' });
      close();
      showAgentConfig(updated);
      toast('令牌已轮换，请更新节点配置');
    } catch (err) {
      ev.target.disabled = false;
      toast('轮换失败：' + err.message, 'err');
    }
  };
}

/* ---------------- 状态页管理 ---------------- *
 *
 * 这里改的每一项都会立刻反映到公开状态页（/status）：
 * 标题、公告、字段可见性、区块开关、维护窗口、人工事件。
 * 维护窗口同时作用于告警引擎 —— 窗口内的节点不再产生新告警。
 */

// openDialog 统一的模态框脚手架。
// 返回 { close, querySelector }：close 关闭并解绑，
// querySelector 委托到弹窗根节点，调用方不用再自己 create/remove。
// onClose 在任意方式关闭后触发（含点击遮罩），供确认框回传"取消"。
function openDialog(title, bodyHTML, onClose) {
  const mask = document.createElement('div');
  mask.className = 'modal-mask';
  mask.innerHTML = `<div class="modal"><div class="modal-title">${esc(title)}</div>${bodyHTML}</div>`;
  document.body.appendChild(mask);
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    mask.remove();
    if (onClose) onClose();
  };
  mask.onclick = (e) => { if (e.target === mask) close(); };
  $$('[data-close]', mask).forEach((b) => { b.onclick = close; });
  return {
    close,
    querySelector: (s) => mask.querySelector(s),
    querySelectorAll: (s) => mask.querySelectorAll(s),
  };
}

// modalConfirm 用界面内弹窗代替原生 confirm：
// 原生 confirm 在部分浏览器里会被"阻止此页面再显示对话框"静默吞掉，
// 导致删除操作既不执行也没有任何反馈。
function modalConfirm(title, text, opts = {}) {
  return new Promise((resolve) => {
    let answered = false;
    const d = openDialog(title, `
      <p class="modal-text">${esc(text)}</p>
      <div class="form-actions">
        <button class="btn" type="button" data-cancel>取消</button>
        <button class="btn ${opts.danger ? 'danger' : 'primary'}" type="button" data-ok>${esc(opts.okText || '确认')}</button>
      </div>`, () => { if (!answered) resolve(false); });
    d.querySelector('[data-ok]').onclick = () => { answered = true; d.close(); resolve(true); };
    d.querySelector('[data-cancel]').onclick = () => { answered = true; d.close(); resolve(false); };
    d.querySelector('[data-ok]').focus();
  });
}

// modalPrompt 对应原生 prompt；返回 null 表示取消，空字符串仅在 allowEmpty 时出现。
function modalPrompt(title, label, opts = {}) {
  return new Promise((resolve) => {
    let answered = false;
    const attrs = [
      `type="${esc(opts.type || 'text')}"`,
      `value="${esc(opts.value || '')}"`,
      `placeholder="${esc(opts.placeholder || '')}"`,
      opts.min != null ? `min="${esc(String(opts.min))}"` : '',
      opts.max != null ? `max="${esc(String(opts.max))}"` : '',
    ].filter(Boolean).join(' ');
    const d = openDialog(title, `
      <div class="modal-form">
        <label class="field-label" for="modalPromptInput">${esc(label)}</label>
        <input class="input" id="modalPromptInput" ${attrs}>
        ${opts.hint ? `<div class="hint">${esc(opts.hint)}</div>` : ''}
      </div>
      <div class="form-actions">
        <button class="btn" type="button" data-cancel>取消</button>
        <button class="btn primary" type="button" data-ok>${esc(opts.okText || '确定')}</button>
      </div>`, () => { if (!answered) resolve(null); });
    const input = d.querySelector('#modalPromptInput');
    const submit = () => {
      const v = input.value.trim();
      if (!v && !opts.allowEmpty) { toast('请输入内容', 'err'); input.focus(); return; }
      answered = true;
      d.close();
      resolve(v);
    };
    d.querySelector('[data-ok]').onclick = submit;
    d.querySelector('[data-cancel]').onclick = () => { answered = true; d.close(); resolve(null); };
    input.onkeydown = (e) => { if (e.key === 'Enter') { e.preventDefault(); submit(); } };
    input.focus();
    input.select();
  });
}

const STATUS_VIS_ITEMS = [
  ['metrics', 'CPU / 内存 / 磁盘使用率'],
  ['load', '负载（1/5/15 分钟）'],
  ['uptime', '连续运行时间'],
  ['traffic', '实时网速与累计流量'],
  ['procs', '进程数'],
  ['tcpConn', 'TCP 连接数'],
  ['swap', 'Swap 用量'],
  ['region', '地区'],
  ['provider', '服务商'],
  ['tags', '节点标签（资产台账中的自由标签）'],
  ['billing', '续费价格与币种（资产类，慎开）'],
  ['expire', '到期日（资产类，慎开）'],
  ['quota', '流量配额与周期用量'],
  ['country', '国家/地区（用于「地区分布」区块）'],
  ['hostname', '真实主机名（可用于指纹识别，慎开）'],
  ['os', '操作系统'],
  ['kernel', '内核版本'],
  ['arch', 'CPU 架构'],
  ['cpuModel', 'CPU 型号'],
  ['cores', '核心数'],
  ['virt', '虚拟化类型'],
];
const STATUS_BLOCK_ITEMS = [
  ['summary', '顶部总览卡片'],
  ['services', '服务探测区'],
  ['incidents', '事件时间线'],
  ['history', 'N 天可用率日柱'],
  ['map', '地区分布（需开启「国家/地区」）'],
  ['activity', '最近活动流'],
  ['nodeDetail', '允许访客点进节点详情'],
];

const INC_UPDATE_LABELS = {
  investigating: '调查中',
  identified: '已定位',
  monitoring: '观察中',
  resolved: '已恢复',
};

async function viewStatusPage() {
  const [cfgRes, nodes, mtRes, incRes] = await Promise.all([
    adminApi('/api/admin/status-page'),
    api('/api/nodes'),
    adminApi('/api/admin/maintenance'),
    adminApi('/api/admin/incidents'),
  ]);
  const cfg = cfgRes.config || {};
  const vis = cfg.visibility || {};
  const blocks = cfg.blocks || {};
  const ann = cfg.announcement || {};
  state.data.nodes = nodes;

  // 逐台可见性：勾选=公开。令牌/隐藏节点都在同一个表单里保存。
  const hiddenSet = {};
  (cfg.hiddenNodes || []).forEach((id) => { hiddenSet[id] = true; });
  const noteMap = cfg.publicNotes || {};
  const spNodeRows = nodes.length
    ? nodes.map((n) => `<tr>
        <td class="strong">${esc(n.name)}</td>
        <td><input type="checkbox" data-hide-node="${esc(n.id)}" ${hiddenSet[n.id] ? '' : 'checked'}></td>
        <td><input type="text" maxlength="120" placeholder="可选，公开可见" data-note-node="${esc(n.id)}" value="${esc(noteMap[n.id] || '')}"></td>
      </tr>`).join('')
    : '<tr><td colspan="3"><div class="empty">还没有节点</div></td></tr>';

  const visBoxes = STATUS_VIS_ITEMS.map(([k, label]) =>
    checkbox(label, 'vis_' + k, !!vis[k])).join('');
  const blockBoxes = STATUS_BLOCK_ITEMS.map(([k, label]) =>
    checkbox(label, 'blk_' + k, !!blocks[k])).join('');

  const nowSec = Date.now() / 1000;
  const mts = mtRes.items || [];
  const incs = incRes.items || [];
  const sevText = { info: '信息', warn: '警告', critical: '严重' };

  const mtRows = mts.map((m) => {
    const scope = (m.nodeIds && m.nodeIds.length)
      ? m.nodeIds.map((id) => { const n = nodes.find((x) => x.id === id); return n ? n.name : id; }).join('、')
      : '全部节点';
    const active = nowSec >= m.startAt && nowSec < m.endAt;
    const status = active
      ? '<span class="badge ok">进行中</span>'
      : nowSec < m.startAt ? '<span class="badge mute">未开始</span>' : '<span class="badge mute">已结束</span>';
    return `<tr>
      <td class="strong">${esc(m.title)}${m.reason ? `<div class="cell-sub">${esc(m.reason)}</div>` : ''}</td>
      <td>${esc(scope)}</td>
      <td class="mono cell-compact">${esc(fullTs(m.startAt))}<br>${esc(fullTs(m.endAt))}</td>
      <td>${status}</td>
      <td class="num"><div class="row-actions">
        ${active ? `<button class="btn tiny" data-end-mt="${esc(m.id)}">结束</button>` : ''}
        <button class="btn tiny" data-edit-mt="${esc(m.id)}">编辑</button>
        <button class="btn tiny danger" data-del-mt="${esc(m.id)}">删除</button>
      </div></td>
    </tr>`;
  }).join('');

  const incRows = incs.map((it) => {
    const scope = (it.nodeIds && it.nodeIds.length)
      ? it.nodeIds.map((id) => { const n = nodes.find((x) => x.id === id); return n ? n.name : id; }).join('、')
      : '全局';
    const ongoing = !it.endAt;
    const updates = it.updates || [];
    const latest = updates.length ? updates[updates.length - 1] : null;
    return `<tr>
      <td class="strong">${esc(it.title)}${it.body ? `<div class="cell-sub">${esc(it.body.slice(0, 60))}</div>` : ''}
        ${latest ? `<div class="cell-sub">最新：${esc(INC_UPDATE_LABELS[latest.status] || latest.status)} · ${esc(latest.text.slice(0, 40))}</div>` : ''}</td>
      <td><span class="badge ${it.level === 'critical' ? 'crit' : it.level === 'warn' ? 'warn' : 'mute'}">${sevText[it.level] || esc(it.level)}</span></td>
      <td>${esc(scope)}</td>
      <td class="mono cell-compact">${esc(fullTs(it.startAt))}${it.endAt ? '<br>→ ' + esc(fullTs(it.endAt)) : '<br><span class="text-warn">进行中</span>'}</td>
      <td class="num"><div class="row-actions">
        <button class="btn tiny" data-update-inc="${esc(it.id)}">进展${updates.length ? ` (${updates.length})` : ''}</button>
        ${ongoing && nowSec >= it.startAt ? `<button class="btn tiny" data-end-inc="${esc(it.id)}">结束</button>` : ''}
        <button class="btn tiny" data-edit-inc="${esc(it.id)}">编辑</button>
        <button class="btn tiny danger" data-del-inc="${esc(it.id)}">删除</button>
      </div></td>
    </tr>`;
  }).join('');

  $('#view').innerHTML = `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">展示配置</h2>
        ${sectionActions('公开页 /status', '<a class="btn sm" href="/status" target="_blank" rel="noopener">打开公开页 ↗</a>')}
      </div>
      <form id="spForm" class="panel panel-pad form-stack">
        <div class="field">
          <span class="field-label">访问门槛</span>
          <div class="grid-2">
            ${checkbox('启用公开页（关闭后 /status 与 /api/status* 一律 404）', 'spEnabled', !!cfg.enabled)}
            <div>
              <span class="field-label">访问令牌（可选；留空表示知道地址即可访问）</span>
              <div class="row-actions" style="gap:8px">
                <input id="spToken" name="spToken" type="text" maxlength="64" value="${esc(cfg.token || '')}"
                  placeholder="16–64 位字母 / 数字 / - / _">
                <button type="button" class="btn sm" id="spTokenGen">生成</button>
              </div>
              <p class="field-hint">令牌会出现在链接里（?t=…）；怀疑外泄点「生成」即立刻换链，旧链接立即失效。</p>
            </div>
          </div>
        </div>
        <div class="field">
          <span class="field-label">逐台可见性</span>
          <p class="field-hint">取消「对外公开」即从公开页隐藏（连详情页一起 404）；备注只在公开页显示，与台账里的内部备注互不影响。</p>
          <div class="table-wrap"><table class="maint-table" data-page-size="10">
            <thead><tr><th>节点</th><th style="width:96px">对外公开</th><th>公开备注</th></tr></thead>
            <tbody>${spNodeRows}</tbody>
          </table></div>
        </div>
        <div class="grid-2">
          ${field('站点标题', 'title', cfg.title || '', { required: true, hint: '浏览器标签页与页面主标题' })}
          ${selectField('可用率日柱天数', 'historyDays', String(cfg.historyDays || 90),
            [{ v: '30', t: '30 天' }, { v: '60', t: '60 天' }, { v: '90', t: '90 天' }])}
        </div>
        <div class="field">
          <span class="field-label">置顶公告</span>
          <textarea name="annText" rows="2" placeholder="例如：今晚 23:00 主控迁移，期间可能短暂不可用。">${esc(ann.text || '')}</textarea>
        </div>
        <div class="grid-2">
          ${checkbox('启用公告', 'annEnabled', !!ann.enabled)}
          ${selectField('公告级别', 'annLevel', ann.level || 'info',
            [{ v: 'info', t: '信息' }, { v: 'warn', t: '警告' }, { v: 'critical', t: '严重' }])}
        </div>
        <div>
          <div class="field-label">公开字段（未勾选的字段不会出现在公开页，关闭是安全的默认值）</div>
          <div class="check-grid">${visBoxes}</div>
        </div>
        <div>
          <div class="field-label">显示区块</div>
          <div class="check-grid">${blockBoxes}</div>
        </div>
        <div class="form-footer">
          <button class="btn primary" type="submit">保存配置</button>
          <span id="spSaved" class="form-note"></span>
        </div>
      </form>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">维护窗口</h2>
        ${sectionActions('窗口内不产生新告警，公开页显示「维护中」', '<button class="btn sm primary" id="mtAdd">＋ 新建维护窗口</button>')}
      </div>
      <div class="table-wrap"><table class="maint-table" data-page-size="10">
        <thead><tr><th>标题</th><th>范围</th><th>起止</th><th>状态</th><th class="num">操作</th></tr></thead>
        <tbody>${mtRows || '<tr><td colspan="5"><div class="empty">没有维护窗口</div></td></tr>'}</tbody>
      </table></div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">人工事件</h2>
        ${sectionActions('与告警一起出现在公开页时间线', '<button class="btn sm primary" id="incAdd">＋ 登记事件</button>')}
      </div>
      <div class="table-wrap"><table class="maint-table" data-page-size="10">
        <thead><tr><th>标题</th><th>级别</th><th>影响范围</th><th>时间</th><th class="num">操作</th></tr></thead>
        <tbody>${incRows || '<tr><td colspan="5"><div class="empty">没有人工事件</div></td></tr>'}</tbody>
      </table></div>
    </section>`;

  // ---- 生成访问令牌（立即保存并生效）----
  const genBtn = $('#spTokenGen');
  if (genBtn) {
    genBtn.onclick = async () => {
      try {
        const r = await adminApi('/api/admin/status-page/token', { method: 'POST' });
        $('#spToken').value = r.token || '';
        toast('已生成新令牌，旧链接立即失效');
      } catch (err) {
        toast('生成失败：' + err.message, 'err');
      }
    };
  }

  // ---- 保存展示配置 ----
  $('#spForm').onsubmit = async (ev) => {
    ev.preventDefault();
    const f = ev.target;
    const vis = {};
    STATUS_VIS_ITEMS.forEach(([k]) => { vis[k] = f.querySelector(`[name="vis_${k}"]`).checked; });
    const blocks = {};
    STATUS_BLOCK_ITEMS.forEach(([k]) => { blocks[k] = f.querySelector(`[name="blk_${k}"]`).checked; });
    const hiddenNodes = [];
    f.querySelectorAll('[data-hide-node]').forEach((el) => {
      if (!el.checked) hiddenNodes.push(el.dataset.hideNode);
    });
    const publicNotes = {};
    f.querySelectorAll('[data-note-node]').forEach((el) => {
      const v = el.value.trim();
      if (v) publicNotes[el.dataset.noteNode] = v;
    });
    const body = {
      enabled: f.querySelector('[name="spEnabled"]').checked,
      token: f.spToken.value.trim(),
      hiddenNodes,
      publicNotes,
      title: f.title.value.trim(),
      historyDays: parseInt(f.historyDays.value, 10) || 90,
      announcement: {
        enabled: f.querySelector('[name="annEnabled"]').checked,
        level: f.annLevel.value,
        text: f.annText.value.trim(),
      },
      visibility: vis,
      blocks,
    };
    try {
      await adminApi('/api/admin/status-page', { method: 'PUT', body: JSON.stringify(body) });
      $('#spSaved').textContent = '已保存 ✓';
      setTimeout(() => { const el = $('#spSaved'); if (el) el.textContent = ''; }, 2500);
      toast('公开页配置已更新');
    } catch (err) {
      toast('保存失败：' + err.message, 'err');
    }
  };

  // ---- 维护窗口 ----
  $('#mtAdd').onclick = () => openMaintenanceDialog(nodes);
  $$('[data-edit-mt]').forEach((b) => {
    b.onclick = () => {
      const m = mts.find((x) => x.id === b.dataset.editMt);
      if (m) openMaintenanceDialog(nodes, m);
    };
  });
  $$('[data-end-mt]').forEach((b) => {
    b.onclick = async () => {
      const m = mts.find((x) => x.id === b.dataset.endMt);
      if (!m) return;
      if (!(await modalConfirm('结束维护窗口', `立即结束维护窗口「${m.title}」？`, { okText: '结束' }))) return;
      try {
        await adminApi('/api/admin/maintenance', {
          method: 'POST',
          body: JSON.stringify({
            id: m.id, title: m.title, reason: m.reason, nodeIds: m.nodeIds || [],
            startAt: m.startAt, endAt: Math.max(m.startAt + 1, Math.floor(Date.now() / 1000)),
          }),
        });
        toast('维护窗口已结束');
        viewStatusPage();
      } catch (err) { toast('结束失败：' + err.message, 'err'); }
    };
  });
  $$('[data-del-mt]').forEach((b) => {
    b.onclick = async () => {
      if (!(await modalConfirm('删除维护窗口', '删除该维护窗口？窗口内正在进行的维护不会再显示。',
        { danger: true, okText: '删除' }))) return;
      try {
        await adminApi('/api/admin/maintenance/' + encodeURIComponent(b.dataset.delMt), { method: 'DELETE' });
        toast('已删除');
        viewStatusPage();
      } catch (err) { toast('删除失败：' + err.message, 'err'); }
    };
  });

  // ---- 人工事件 ----
  $('#incAdd').onclick = () => openIncidentDialog(nodes);
  $$('[data-update-inc]').forEach((b) => {
    b.onclick = () => {
      const it = incs.find((x) => x.id === b.dataset.updateInc);
      if (it) openIncidentUpdateDialog(it);
    };
  });
  $$('[data-edit-inc]').forEach((b) => {
    b.onclick = () => {
      const it = incs.find((x) => x.id === b.dataset.editInc);
      if (it) openIncidentDialog(nodes, it);
    };
  });
  $$('[data-end-inc]').forEach((b) => {
    b.onclick = async () => {
      const it = incs.find((x) => x.id === b.dataset.endInc);
      if (!it) return;
      if (!(await modalConfirm('结束事件', `将事件「${it.title}」标记为已结束？`, { okText: '标记结束' }))) return;
      try {
        await adminApi('/api/admin/incidents', {
          method: 'POST',
          body: JSON.stringify({
            id: it.id, title: it.title, body: it.body, level: it.level,
            nodeIds: it.nodeIds || [], startAt: it.startAt,
            endAt: Math.max(it.startAt + 1, Math.floor(Date.now() / 1000)),
          }),
        });
        toast('事件已结束');
        viewStatusPage();
      } catch (err) { toast('结束失败：' + err.message, 'err'); }
    };
  });
  $$('[data-del-inc]').forEach((b) => {
    b.onclick = async () => {
      if (!(await modalConfirm('删除事件', '删除该事件？它会同时从公开页时间线消失。',
        { danger: true, okText: '删除' }))) return;
      try {
        await adminApi('/api/admin/incidents/' + encodeURIComponent(b.dataset.delInc), { method: 'DELETE' });
        toast('已删除');
        viewStatusPage();
      } catch (err) { toast('删除失败：' + err.message, 'err'); }
    };
  });
}

function fullTs(ts) {
  const d = new Date(ts * 1000);
  const p = (n) => (n < 10 ? '0' : '') + n;
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// dtLocal 把 Unix 秒转成 datetime-local 输入框需要的本地时间格式。
function dtLocal(ts) {
  if (!ts) return '';
  const d = new Date(ts * 1000);
  const p = (n) => (n < 10 ? '0' : '') + n;
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

// nodePicker 生成"影响范围"多选框列表（全部节点 + 分组归类展示）。
function nodePicker(nodes, name, selected = []) {
  if (!nodes.length) return '<div class="empty">暂无节点</div>';
  const picked = new Set(selected);
  return `<div class="check-grid">${nodes.map((n) =>
    `<label class="check"><input type="checkbox" name="${name}" value="${esc(n.id)}"${picked.has(n.id) ? ' checked' : ''}><span>${esc(n.name)}</span></label>`
  ).join('')}</div>`;
}
function pickedIDs(form, name) {
  return $$(`input[name="${name}"]:checked`, form).map((c) => c.value);
}

function openMaintenanceDialog(nodes, mt) {
  const editing = !!mt;
  const dlg = openDialog(editing ? '编辑维护窗口' : '新建维护窗口', `
    <form id="mtForm" class="form-stack">
      ${field('标题', 'title', mt?.title || '', { required: true, placeholder: '例如：主控迁移' })}
      ${field('说明（可选）', 'reason', mt?.reason || '', { placeholder: '访客会在公开页看到这段说明' })}
      <div class="field"><span class="field-label">影响范围（不选 = 全部节点）</span>${nodePicker(nodes, 'mtNode', mt?.nodeIds || [])}</div>
      <div class="grid-2">
        ${field('开始（留空 = 立即）', 'startAt', editing ? dtLocal(mt.startAt) : '', { type: 'datetime-local' })}
        ${field('持续（分钟，默认 60）', 'minutes', editing ? String(Math.max(1, Math.round((mt.endAt - mt.startAt) / 60))) : '', { type: 'number', placeholder: '60' })}
      </div>
      <div class="form-actions">
        <button class="btn" type="button" data-close>取消</button>
        <button class="btn primary" type="submit">${editing ? '保存修改' : '创建'}</button>
      </div>
    </form>`);
  const f = dlg.querySelector('#mtForm');
  f.onsubmit = async (ev) => {
    ev.preventDefault();
    const start = f.startAt.value ? Math.floor(new Date(f.startAt.value).getTime() / 1000) : 0;
    const minutes = parseInt(f.minutes.value, 10) || 0;
    try {
      await adminApi('/api/admin/maintenance', {
        method: 'POST',
        body: JSON.stringify({
          id: editing ? mt.id : '',
          title: f.title.value.trim(),
          reason: f.reason.value.trim(),
          nodeIds: pickedIDs(f, 'mtNode'),
          startAt: start,
          minutes,
        }),
      });
      dlg.close();
      toast(editing ? '维护窗口已更新' : '维护窗口已创建');
      viewStatusPage();
    } catch (err) { toast((editing ? '保存失败：' : '创建失败：') + err.message, 'err'); }
  };
}

function openIncidentDialog(nodes, inc) {
  const editing = !!inc;
  const dlg = openDialog(editing ? '编辑事件' : '登记事件', `
    <form id="incForm" class="form-stack">
      ${field('标题', 'title', inc?.title || '', { required: true, placeholder: '例如：上游出口抖动' })}
      <div class="field"><span class="field-label">详情（可选）</span>
        <textarea name="body" rows="3" placeholder="发生了什么、影响什么、怎么处理">${esc(inc?.body || '')}</textarea></div>
      <div class="grid-2">
        ${selectField('级别', 'level', inc?.level || 'info',
          [{ v: 'info', t: '信息' }, { v: 'warn', t: '警告' }, { v: 'critical', t: '严重' }])}
        ${field('结束时间（留空 = 进行中）', 'endAt', inc?.endAt ? dtLocal(inc.endAt) : '', { type: 'datetime-local' })}
      </div>
      <div class="field"><span class="field-label">受影响节点（不选 = 全局）</span>${nodePicker(nodes, 'incNode', inc?.nodeIds || [])}</div>
      <div class="form-actions">
        <button class="btn" type="button" data-close>取消</button>
        <button class="btn primary" type="submit">${editing ? '保存修改' : '登记'}</button>
      </div>
    </form>`);
  const f = dlg.querySelector('#incForm');
  f.onsubmit = async (ev) => {
    ev.preventDefault();
    const end = f.endAt.value ? Math.floor(new Date(f.endAt.value).getTime() / 1000) : 0;
    try {
      await adminApi('/api/admin/incidents', {
        method: 'POST',
        body: JSON.stringify({
          id: editing ? inc.id : '',
          title: f.title.value.trim(),
          body: f.body.value.trim(),
          level: f.level.value,
          nodeIds: pickedIDs(f, 'incNode'),
          startAt: editing ? inc.startAt : 0,
          endAt: end,
        }),
      });
      dlg.close();
      toast(editing ? '事件已更新' : '事件已登记');
      viewStatusPage();
    } catch (err) { toast((editing ? '保存失败：' : '登记失败：') + err.message, 'err'); }
  };
}

// openIncidentUpdateDialog 给已有事件追加一条公开进展。
// 与"编辑事件"分开：编辑改的是标题/级别等元数据，进展是追加记录，
// 后者会保留时间线，访客能看到事件的处理过程。
function openIncidentUpdateDialog(inc) {
  const updates = inc.updates || [];
  const history = updates.length
    ? `<div class="info-list" style="margin-bottom:4px">${updates.slice().reverse().map((u) => `
        <div class="kv-row"><span class="kv-key mono">${esc(fullTs(u.ts))}</span>
        <span class="kv-val"><b>${esc(INC_UPDATE_LABELS[u.status] || u.status)}</b> · ${esc(u.text)}</span></div>`).join('')}</div>`
    : '<div class="field-hint">还没有进展记录。</div>';
  const dlg = openDialog('事件进展 · ' + inc.title, `
    <form id="incUpdForm" class="form-stack">
      <div class="field-hint">已发布记录（${updates.length}）</div>
      ${history}
      <div class="grid-2">
        ${selectField('状态', 'status', updates.length ? 'monitoring' : 'investigating',
          Object.entries(INC_UPDATE_LABELS).map(([v, t]) => ({ v, t })))}
      </div>
      <div class="field"><span class="field-label">进展说明</span>
        <textarea name="text" rows="3" required maxlength="500"
          placeholder="例如：已定位上游线路问题，正在切换备用出口。">${esc('')}</textarea></div>
      <div class="field-hint">发布后立即出现在公开事件时间线；状态选择「已恢复」时会同时把事件标记为结束。</div>
      <div class="form-actions">
        <button class="btn" type="button" data-close>取消</button>
        <button class="btn primary" type="submit">发布进展</button>
      </div>
    </form>`);
  const f = dlg.querySelector('#incUpdForm');
  f.onsubmit = async (ev) => {
    ev.preventDefault();
    const text = f.text.value.trim();
    if (!text) { toast('请填写进展说明', 'err'); return; }
    try {
      await adminApi('/api/admin/incidents/' + encodeURIComponent(inc.id) + '/updates', {
        method: 'POST',
        body: JSON.stringify({ status: f.status.value, text }),
      });
      dlg.close();
      toast('进展已发布');
      viewStatusPage();
    } catch (err) { toast('发布失败：' + err.message, 'err'); }
  };
}

// openUpgradeBatchDialog 创建 Agent 升级清单。
// 服务端不远程执行升级，清单只记录"升级前的版本"用于推导进度；
// 目标版本默认填控制端版本，便于统一升级到同一版本。
function openUpgradeBatchDialog(nodes, presetIDs, after) {
  if (!nodes.length) { toast('还没有节点可以加入清单', 'err'); return; }
  const picked = new Set(presetIDs || []);
  const current = state.serverVersion || '';
  const rows = nodes.map((n) => `<label class="check">
      <input type="checkbox" name="upNode" value="${esc(n.id)}"${picked.has(n.id) ? ' checked' : ''}>
      <span>${esc(n.name)} <span class="mono" style="color:var(--fg-3);font-size:11px">${esc(n.agentVersion || '未上报')}</span></span>
    </label>`).join('');
  const dlg = openDialog('新建 Agent 升级清单', `
    <form id="upBatchForm" class="form-stack">
      <div class="grid-2">
        ${field('目标版本', 'targetVersion', current, { placeholder: current || '例如 v1.14.0', hint: '留空则使用控制端版本' })}
        ${field('备注', 'note', '', { placeholder: '例如：周末维护窗口内升级' })}
      </div>
      <div class="field">
        <span class="field-label">选择节点（${nodes.length} 台）</span>
        <div class="row-actions" style="margin-bottom:8px">
          <button class="btn tiny" type="button" id="upPickOutdated">仅选可升级</button>
          <button class="btn tiny" type="button" id="upPickAll">全选</button>
          <button class="btn tiny" type="button" id="upPickNone">清空</button>
        </div>
        <div class="check-grid">${rows}</div>
      </div>
      <div class="field-hint">清单创建后按「节点当前 Agent 版本 ≠ 创建时版本」自动计算完成数；节点删除后标记为「已删除」。</div>
      <div class="form-actions">
        <button class="btn" type="button" data-close>取消</button>
        <button class="btn primary" type="submit">创建清单</button>
      </div>
    </form>`);
  const f = dlg.querySelector('#upBatchForm');
  const setPick = (fn) => {
    $$('input[name="upNode"]', f).forEach((c) => {
      const n = nodes.find((x) => x.id === c.value);
      c.checked = fn(n);
    });
  };
  dlg.querySelector('#upPickOutdated').onclick = () => setPick((n) => !!n && !!n.agentVersion && n.agentVersion !== current);
  dlg.querySelector('#upPickAll').onclick = () => setPick(() => true);
  dlg.querySelector('#upPickNone').onclick = () => setPick(() => false);
  f.onsubmit = async (ev) => {
    ev.preventDefault();
    const ids = pickedIDs(f, 'upNode');
    if (!ids.length) { toast('至少选择一个节点', 'err'); return; }
    try {
      await adminApi('/api/admin/agent-upgrades', {
        method: 'POST',
        body: JSON.stringify({
          nodeIds: ids,
          targetVersion: f.targetVersion.value.trim(),
          note: f.note.value.trim(),
        }),
      });
      dlg.close();
      toast('升级清单已创建（' + ids.length + ' 台）');
      (after || viewSettings)();
    } catch (err) { toast('创建失败：' + err.message, 'err'); }
  };
}

// showNodeEdit 编辑节点台账，避免为了改名/改价删除节点、丢失历史。
// editNodeById 拉取完整节点台账后打开编辑弹窗，完成后刷新调用方页面。
async function editNodeById(id, after) {
  const n = await adminApi('/api/admin/nodes/' + encodeURIComponent(id));
  showNodeEdit(n, after);
}

// splitTags 解析逗号分隔的标签输入（中英文逗号都接受）。
function splitTags(s) {
  return String(s || '').split(/[,，]/).map((t) => t.trim()).filter(Boolean).slice(0, 8);
}

function showNodeEdit(n, after) {
  const dlg = document.createElement('div');
  dlg.className = 'modal-mask';
  dlg.innerHTML = `<div class="modal">
    <div class="modal-title">编辑节点</div>
    <p class="modal-sub">节点名称会显示在公开状态页，请勿填写主机名、IP 或可识别信息。</p>
    <form id="editNodeForm" class="form-grid">
      ${field('名称', 'name', n.name, { required: true, placeholder: '例如 prod-01' })}
      ${field('服务商', 'provider', n.provider, { placeholder: 'racknerd / 腾讯云 …' })}
      ${field('位置', 'region', n.region, { placeholder: '美国 / 香港 …' })}
      ${field('国家代码', 'country', n.country, { placeholder: 'US / JP / DE', hint: '两位 ISO 代码，公开页分布图使用' })}
      ${field('分组', 'group', n.group, { placeholder: 'prod / test' })}
      ${field('到期日', 'expireAt', n.expireAt, { placeholder: '2027-06-01' })}
      ${field('年付价', 'price', n.price, { type: 'number', placeholder: '11.00' })}
      ${selectField('币种', 'currency', n.currency || 'USD', [{ v: 'USD', t: 'USD ($)' }, { v: 'CNY', t: 'CNY (¥)' }])}
      ${field('月度流量配额（GB）', 'trafficQuotaGb', n.trafficQuotaGb || '', { type: 'number', placeholder: '0 = 不限制' })}
      ${field('流量重置日', 'trafficResetDay', n.trafficResetDay == null ? 1 : n.trafficResetDay, { type: 'number', hint: '每月 1–28 日' })}
      ${selectField('流量计费口径', 'trafficMode', n.trafficMode || 'sum', TRAFFIC_MODES.map((x) => ({ v: x.v, t: x.t })))}
      <div class="field-hint" style="grid-column:1/-1;margin-top:-6px">按商家计费方式选择：双边计费用「收发合计」，只算单向用对应单边口径。切换后本周期已用量会立即按新口径重算。</div>
      ${field('备注', 'note', n.note, { placeholder: '可选' })}
      ${field('标签', 'tags', (n.tags || []).join(', '), { placeholder: '例如：cn2, 高防, 备机', hint: '逗号分隔，最多 8 个；公开页开启「节点标签」后可见' })}
      ${field('排序权重', 'weight', n.weight, { type: 'number', placeholder: '0' })}
      ${checkbox('收藏（对比页优先展示）', 'favorite', !!n.favorite)}
      ${checkbox('告警豁免（不再评估该节点的任何指标规则）', 'alertExempt', !!n.alertExempt)}
      <div class="form-actions">
        <button class="btn" type="button" id="editCancel">取消</button>
        <button class="btn primary" type="submit">保存</button>
      </div>
    </form>
  </div>`;
  document.body.appendChild(dlg);
  const close = () => dlg.remove();
  dlg.querySelector('#editCancel').onclick = close;
  dlg.onclick = (e) => { if (e.target === dlg) close(); };
  dlg.querySelector('#editNodeForm').onsubmit = async (e) => {
    e.preventDefault();
    const d = formData(e.target);
    try {
      await adminApi('/api/admin/nodes/' + encodeURIComponent(n.id), {
        method: 'PUT',
        body: JSON.stringify({
          name: d.name, provider: d.provider, region: d.region, group: d.group,
          country: (d.country || '').toUpperCase(),
          expireAt: d.expireAt, price: Number(d.price) || 0, currency: d.currency,
          trafficQuotaGb: Number(d.trafficQuotaGb) || 0,
          trafficResetDay: Number(d.trafficResetDay) || 0,
          trafficMode: d.trafficMode || 'sum',
          tags: splitTags(d.tags),
          note: d.note, weight: Number(d.weight) || 0, favorite: !!d.favorite,
          alertExempt: !!d.alertExempt,
        }),
      });
      close();
      toast('节点已更新');
      (after || viewSettings)();
    } catch (err) {
      toast('保存失败：' + err.message, 'err');
    }
  };
}

/* ---------------- 审计日志 ---------------- */

const AUDIT_ACTIONS = [
  ['', '全部动作'],
  ['auth.login', '登录成功'],
  ['auth.login_failed', '登录失败'],
  ['auth.logout', '退出登录'],
  ['nodes.create', '新增节点'],
  ['nodes.update', '编辑节点'],
  ['nodes.delete', '删除节点'],
  ['nodes.batch', '批量操作节点'],
  ['agent-upgrades.create', '创建升级清单'],
  ['agent-upgrades.delete', '删除升级清单'],
  ['services.create', '新增服务'],
  ['services.update', '编辑服务'],
  ['services.delete', '删除服务'],
  ['rules.create', '新增告警规则'],
  ['rules.update', '编辑告警规则'],
  ['rules.delete', '删除告警规则'],
  ['settings.update', '修改设置'],
  ['statuspage.update', '修改状态页'],
  ['incidents.create', '发布事件'],
  ['incidents.update', '更新事件进展'],
  ['maintenance.create', '创建维护窗口'],
  ['tokens.create', '创建令牌'],
  ['tokens.delete', '删除令牌'],
  ['backup.export', '导出备份'],
  ['backup.run', '手动自动备份'],
  ['backup.restore', '导入备份'],
  ['export.action', '导出数据'],
  ['notify.test', '测试通知'],
];

async function viewAudit() {
  if (!state.authenticated) {
    state.authRequired = true;
    showLoginGate();
    return;
  }
  const f = state.auditFilters;
  const limit = 20;
  const params = new URLSearchParams({
    limit: String(limit),
    offset: String(Math.max(0, (state.auditPage - 1) * limit)),
    range: f.range,
  });
  if (f.action) params.set('action', f.action);
  if (state.search) params.set('q', state.search);
  const d = await adminApi('/api/admin/audit?' + params.toString());
  const entries = d.entries || [];
  const stats = d.stats || {};
  const total = d.total || 0;
  const pages = Math.max(1, Math.ceil(total / limit));
  const page = Math.max(1, Math.min(pages, state.auditPage));
  state.auditPage = page;

  const statusBadge = (code) => {
    const cls = code >= 500 ? 'crit' : code >= 400 ? 'warn' : 'ok';
    return `<span class="badge ${cls}">${code || '—'}</span>`;
  };
  const roleLabel = { session: '会话', 'api-token': 'API 令牌', 'admin-token': '管理令牌', basic: 'Basic' };
  const fmtTime = (ts) => new Date(ts * 1000).toLocaleString('zh-CN', { hour12: false });

  $('#view').innerHTML = `
    <div class="kpis" style="margin-bottom:20px">
      ${kpi('匹配记录', total, '条', `当前筛选 · 每页 ${limit} 条`)}
      ${kpi('内存保留', stats.count || 0, '条', '最近操作可直接检索')}
      ${kpi('保留期', stats.retentionDays || 90, '天', '过期自动清理')}
      ${kpi('审计文件', bytes(stats.bytes || 0), '', stats.earliest ? '最早 ' + clock(stats.earliest) : '尚无记录')}
    </div>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">管理操作</h2>
        ${sectionActions('只记录元数据，不保存请求体与密码')}
      </div>
      <div class="panel panel-pad" style="margin-bottom:12px">
        <div class="toolbar">
          <select id="auditRange">
            ${[['24h', '最近 24 小时'], ['7d', '最近 7 天'], ['30d', '最近 30 天'], ['90d', '最近 90 天'], ['', '全部时间']]
              .map(([v, t]) => `<option value="${v}"${v === f.range ? ' selected' : ''}>${t}</option>`).join('')}
          </select>
          <select id="auditAction">
            ${AUDIT_ACTIONS.map(([v, t]) => `<option value="${esc(v)}"${v === f.action ? ' selected' : ''}>${esc(t)}</option>`).join('')}
          </select>
          <span class="section-meta">${state.search ? '关键词：' + esc(state.search) : '可在右上角搜索操作人、路径或 IP'}</span>
        </div>
      </div>
      <div class="table-wrap">
        <table class="audit-table" data-page-size="10">
          <thead><tr><th>时间</th><th>动作</th><th>操作人</th><th>方式</th><th>方法</th><th>路径</th><th class="num">状态</th><th>IP</th></tr></thead>
          <tbody>${entries.map((e) => `<tr>
            <td class="mono" style="font-size:11.5px;white-space:nowrap">${esc(fmtTime(e.ts))}</td>
            <td class="strong">${esc(e.action)}${e.detail ? `<div class="field-hint">${esc(e.detail)}</div>` : ''}</td>
            <td>${esc(e.actor || '—')}</td>
            <td>${roleLabel[e.role] ? `<span class="badge mute">${roleLabel[e.role]}</span>` : '—'}</td>
            <td class="mono" style="font-size:11px">${esc(e.method || '—')}</td>
            <td class="mono" style="font-size:11.5px">${esc(e.path || '—')}</td>
            <td class="num">${statusBadge(e.status)}</td>
            <td class="mono" style="font-size:11.5px">${esc(e.ip || '—')}</td>
          </tr>`).join('') || `<tr><td colspan="8"><div class="empty">没有匹配的审计记录</div></td></tr>`}</tbody>
        </table>
      </div>
      ${pages > 1 ? `<div class="table-pager">
        <button class="btn tiny" data-audit-page="-1" ${page <= 1 ? 'disabled' : ''}>上一页</button>
        <span class="pager-info">第 <b class="mono">${page}</b> / ${pages} 页 · 共 ${total} 条</span>
        <button class="btn tiny" data-audit-page="1" ${page >= pages ? 'disabled' : ''}>下一页</button>
      </div>` : ''}
      <p class="field-hint section-hint">
        审计日志保存在数据目录的 <code>audit.jsonl</code>（权限 0600），保留 ${stats.retentionDays || 90} 天。
        登录失败、令牌增删、配置修改与数据导出都会记录；不记录请求体，避免密码与令牌泄露。
      </p>
    </section>`;

  $('#auditRange').onchange = (e) => {
    state.auditFilters.range = e.target.value;
    state.auditPage = 1;
    viewAudit();
  };
  $('#auditAction').onchange = (e) => {
    state.auditFilters.action = e.target.value;
    state.auditPage = 1;
    viewAudit();
  };
  $$('[data-audit-page]').forEach((b) => {
    b.onclick = () => {
      state.auditPage = page + Number(b.dataset.auditPage);
      viewAudit();
    };
  });
}

/* ---------------- 路由 ---------------- */

const ROUTES = {
  overview: { title: '概览', sub: '机群健康状态与关键指标', fn: viewOverview, search: '搜索节点 / 服务商…' },
  nodes: { title: '节点', sub: '全部受监控服务器', fn: viewNodes, search: '搜索名称 / 服务商 / 地区…' },
  compare: { title: '节点对比', sub: '跨节点指标对比与分组聚合', fn: viewCompare, search: '搜索节点 / 分组…' },
  services: { title: '服务', sub: '网站与端口可用性探测', fn: viewServices, search: '搜索服务 / 目标…' },
  alerts: { title: '告警', sub: '规则触发与历史记录', fn: viewAlerts, search: '搜索规则 / 节点 / 告警…' },
  assets: { title: '台账', sub: '资产、到期与成本', fn: viewAssets, search: '搜索资产 / 服务商 / 到期日…' },
  statuspage: { title: '状态页', sub: '公开页 /status 的展示、维护窗口与事件', fn: viewStatusPage },
  audit: { title: '审计日志', sub: '管理操作、登录与导出记录', fn: viewAudit, search: '搜索操作人 / 路径 / IP…' },
  settings: { title: '设置', sub: '节点、探测与通知配置', fn: viewSettings },
};

function bindNodeClicks() {
  $$('[data-node]', $('#view')).forEach((el) => {
    el.onclick = () => { location.hash = '#/nodes/' + el.dataset.node; };
  });
}

function setActiveNav(route) {
  $$('.nav-item').forEach((a) => {
    const active = a.dataset.route === route;
    a.classList.toggle('active', active);
    if (active) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
}

// 登录层关闭：恢复后台骨架并清空登录页内容。
// ★ 必须同时清 innerHTML：留着旧表单会残留事件绑定与已输入的令牌。
function closeLoginLayer() {
  document.body.classList.remove('auth-gate');
  const layer = document.getElementById('loginLayer');
  if (layer) { layer.hidden = true; layer.innerHTML = ''; }
}

// showLoginGate 渲染全屏账号密码登录页。
// 会话 Cookie 由服务端签发（HttpOnly），前端不保存任何凭证。
function showLoginGate() {
  if (window.__closeStream) window.__closeStream();
  state.authRequired = true;
  const layer = document.getElementById('loginLayer');
  if (!layer) return;
  if (!layer.hidden && layer.querySelector('#gateForm')) {
    // 已经显示时只聚焦，避免抹掉用户正在输入的账号密码。
    const cur = layer.querySelector('#gateIdentifier');
    if (cur) cur.focus();
    return;
  }
  document.body.classList.add('auth-gate');
  layer.hidden = false;
  layer.innerHTML = `
    <div class="login-card" id="loginCard">
      <div class="login-brand">
        <div class="login-mark" aria-hidden="true">
          <svg viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M3 15l4-6 3.5 4.5L14 9l7 8"/></svg>
        </div>
        <div>
          <div class="login-brand-name">Rollcall</div>
          <div class="login-brand-sub">Fleet Monitor</div>
        </div>
      </div>
      <h1 class="login-title">登录管理控制台</h1>
      <p class="login-sub">使用管理员账号或邮箱登录，管理节点、告警与状态页。</p>
      <div class="login-error" id="loginError" role="alert"></div>
      <form id="gateForm" autocomplete="on">
        <label class="login-field">
          <span class="login-label">用户名或邮箱</span>
          <span class="login-input-wrap">
            <input class="login-input" id="gateIdentifier" name="identifier" type="text"
                   required spellcheck="false"
                   autocomplete="username" autocapitalize="none" aria-describedby="loginError">
          </span>
        </label>
        <label class="login-field">
          <span class="login-label">密码</span>
          <span class="login-input-wrap">
            <input class="login-input" id="gatePassword" name="password" type="password"
                   required autocomplete="current-password"
                   aria-describedby="loginError">
            <button type="button" class="login-eye" id="gateEye" title="显示 / 隐藏密码" aria-label="显示或隐藏密码">
              <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12s3.5-6.5 10-6.5S22 12 22 12s-3.5 6.5-10 6.5S2 12 2 12z"/><circle cx="12" cy="12" r="2.6"/></svg>
            </button>
          </span>
        </label>
        <label class="login-field" id="gateTotpWrap" hidden>
          <span class="login-label">动态验证码</span>
          <span class="login-input-wrap">
            <input class="login-input" id="gateTotp" name="totp" type="text"
                   inputmode="numeric"
                   pattern="[0-9]{6}" maxlength="6" autocomplete="one-time-code"
                   aria-describedby="loginError">
          </span>
        </label>
        <button class="login-btn" id="gateSubmit" type="submit">登 录</button>
      </form>
      <div class="login-foot">
        <a href="/status" target="_blank" rel="noopener">公开状态页 ↗</a>
        <button type="button" class="login-theme-btn" id="loginTheme" title="切换深浅色" aria-label="切换深浅色">
          <svg id="loginThemeIcon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12.8A9 9 0 1111.2 3a7 7 0 009.8 9.8z"/></svg>
        </button>
      </div>
    </div>`;

  const userInp = layer.querySelector('#gateIdentifier');
  const passInp = layer.querySelector('#gatePassword');
  const err = layer.querySelector('#loginError');
  const card = layer.querySelector('#loginCard');
  const btn = layer.querySelector('#gateSubmit');
  // 复用主界面的主题图标与文案，登录页与首页保持一致。
  applyTheme(document.documentElement.dataset.theme || 'light', false);
  userInp.focus();

  layer.querySelector('#gateEye').onclick = () => {
    passInp.type = passInp.type === 'password' ? 'text' : 'password';
    passInp.focus();
  };
  layer.querySelector('#loginTheme').onclick = () => {
    applyTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
  };

  const showErr = (msg) => {
    err.textContent = msg;
    err.classList.add('show');
    // 重新触发抖动动画：先移除类、强制 reflow、再加回
    card.classList.remove('shake');
    void card.offsetWidth;
    card.classList.add('shake');
  };

  $('#gateForm').onsubmit = async (e) => {
    e.preventDefault();
    const d = formData(e.target);
    const identifier = String(d.identifier || '').trim();
    const password = String(d.password || '');
    const totp = String(d.totp || '').trim();
    if (!identifier || !password) {
      showErr('请输入用户名/邮箱和密码。');
      return;
    }
    btn.disabled = true;
    btn.innerHTML = '<span class="spin" aria-hidden="true"></span>正在登录…';
    err.classList.remove('show');
    try {
      const r = await fetch('/api/auth/login', {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
          Accept: 'application/json',
          'Content-Type': 'application/json',
          'X-Requested-With': 'XMLHttpRequest',
        },
        body: JSON.stringify({ identifier, password, totp }),
      });
      let data = null;
      try { data = await r.json(); } catch (ex) { /* 错误页可能不是 JSON */ }
      if (r.status === 401) {
        const wrap = layer.querySelector('#gateTotpWrap');
        if (data && (data.totpRequired || !wrap.hidden)) {
          wrap.hidden = false;
          const totpInp = layer.querySelector('#gateTotp');
          totpInp.focus();
          totpInp.select();
          showErr((data && data.error) || '动态验证码错误。');
          return;
        }
        showErr('用户名或密码错误，请核对后重试。');
        passInp.value = '';
        passInp.focus();
        return;
      }
      if (r.status === 429) {
        showErr('尝试次数过多，请稍后再试。');
        return;
      }
      if (r.status === 503) {
        showErr('面板登录未启用，请联系服务端管理员。');
        return;
      }
      if (!r.ok) {
        showErr((data && data.error) || ('服务端返回 HTTP ' + r.status + '，请稍后再试。'));
        return;
      }
      state.authenticated = true;
      state.authRequired = false;
      state.user = (data && data.user) || { username: identifier, email: '' };
      state.data = {};
      closeLoginLayer();
      updateUserMenu();
      toast('已登录');
      render();
    } catch (ex) {
      showErr('无法连接服务端：' + ex.message);
    } finally {
      btn.disabled = false;
      btn.textContent = '登 录';
    }
  };

  markOffline();
}

async function doLogout() {
  try {
    await fetch('/api/auth/logout', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { Accept: 'application/json', 'X-Requested-With': 'XMLHttpRequest' },
    });
  } catch (e) { /* 网络失败也要在本地退出 */ }
  state.authenticated = false;
  state.authRequired = true;
  state.user = null;
  state.data = {};
  updateUserMenu();
  closeUserPop();
  toast('已退出登录');
  render();
}

async function render() {
  // 渲染是异步的（含最多 12 秒的请求超时）。期间的导航/刷新请求先记下来，
  // 等当前渲染结束后补跑，不能直接丢掉——否则点击导航后页面停在旧视图。
  if (state.rendering) {
    state.renderPending = true;
    return;
  }
  if (state.authRequired) {
    showLoginGate();
    return;
  }
  // 任何一次成功渲染都意味着鉴权已通过：确保登录层退场。
  state.rendering = true;
  closeLoginLayer();
  // 幂等：连接已存在时内部直接返回，不会重复建连。
  if (window.__ensureStream) window.__ensureStream();
  const hash = location.hash.replace(/^#\/?/, '') || 'overview';
  // 节点详情支持 #/nodes/<id>?range=7d：刷新/分享后保留所选区间。
  const qIdx = hash.indexOf('?');
  const pathPart = qIdx >= 0 ? hash.slice(0, qIdx) : hash;
  const hashQuery = new URLSearchParams(qIdx >= 0 ? hash.slice(qIdx + 1) : '');
  const [route, param] = pathPart.split('/');
  state.route = route;
  state.param = param || null;
  if (route === 'nodes' && param) {
    const r = hashQuery.get('range');
    if (VALID_RANGES.includes(r)) state.range = r;
  }

  const meta = ROUTES[route];
  if (meta) {
    $('#pageTitle').textContent = meta.title;
    $('#pageSub').textContent = meta.sub;
    setActiveNav(route);
  }
  // 搜索只对支持筛选的列表页展示，避免在设置/状态页输入后毫无反应。
  const searchBox = $('.topbar .search');
  if (searchBox) {
    searchBox.hidden = (route === 'nodes' && param) || !(meta && meta.search);
    if (meta && meta.search) $('#searchInput').placeholder = meta.search;
  }

  try {
    if (route === 'nodes' && param) {
      $('#pageTitle').textContent = '节点详情';
      $('#pageSub').textContent = '历史趋势与系统信息';
      setActiveNav('nodes');
      await viewNodeDetail(param);
    } else if (meta) {
      await meta.fn();
    } else {
      location.hash = '#/overview';
    }
    markSynced();
  } catch (e) {
    if (e && e.status === 401) {
      showLoginGate();
      return;
    }
    $('#view').innerHTML = `<div class="panel panel-pad" style="text-align:center;padding:48px 20px">
      <div style="font-size:13px;color:var(--crit);font-weight:600;margin-bottom:6px">数据加载失败</div>
      <div style="font-size:12px;color:var(--fg-3)">${esc(e.message)}</div>
      <div style="font-size:11.5px;color:var(--fg-3);margin-top:10px">请确认服务端已启动（默认 :19080）</div>
    </div>`;
    markOffline();
  } finally {
    state.rendering = false;
    if (state.renderPending) {
      state.renderPending = false;
      render();
    }
  }
}

/* ---------------- 主题 ---------------- */

const THEME_DEFAULT_ACCENT = '#2f5d8a';
const THEME_DEFAULT_ACCENT_DARK = '#6ea8d8';

function themeRgb(hex) {
  const m = /^#([0-9a-f]{6})$/i.exec(hex || '');
  if (!m) return null;
  const n = parseInt(m[1], 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

function themeMix(rgb, amount) {
  return rgb.map((v) => Math.round(v + (255 - v) * amount));
}

// 只写入白名单变量；服务端从不下发 CSS 文本，这里也不拼接到 <style>。
function applyThemeVars(mode) {
  const root = document.documentElement;
  const t = serverTheme;
  const accent = t ? (mode === 'dark' ? (t.accentDark || THEME_DEFAULT_ACCENT_DARK) : (t.accent || THEME_DEFAULT_ACCENT)) : '';
  const rgb = themeRgb(accent);
  if (rgb) {
    root.style.setProperty('--accent', accent);
    root.style.setProperty('--accent-2', '#' + themeMix(rgb, mode === 'dark' ? 0.22 : 0.16)
      .map((v) => v.toString(16).padStart(2, '0')).join(''));
    root.style.setProperty('--accent-soft', `rgba(${rgb.join(', ')}, ${mode === 'dark' ? 0.12 : 0.09})`);
  } else {
    root.style.removeProperty('--accent');
    root.style.removeProperty('--accent-2');
    root.style.removeProperty('--accent-soft');
  }
  if (t && t.radius !== null && t.radius !== undefined && t.radius !== '') {
    root.style.setProperty('--r', Number(t.radius) + 'px');
  } else {
    root.style.removeProperty('--r');
  }
  if (t && t.compact) root.dataset.density = 'compact';
  else delete root.dataset.density;
}

async function loadServerTheme() {
  try {
    const r = await fetch('/api/theme', { headers: { Accept: 'application/json' } });
    if (!r.ok) return;
    serverTheme = await r.json();
    applyThemeVars(document.documentElement.dataset.theme || 'light');
  } catch (e) { /* 外观是增强功能，拉取失败不影响主流程 */ }
}

function applyTheme(t, persist = true) {
  document.documentElement.dataset.theme = t;
  if (persist) lsSet(LS_THEME, LS_THEME_LEGACY, t);
  applyThemeVars(t);
  const icon = $('#themeIcon');
  const text = $('.sidebar-foot .btn-text');
  if (t === 'dark') {
    icon.innerHTML = '<circle cx="12" cy="12" r="4.2"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>';
    if (text) text.textContent = '浅色模式';
  } else {
    icon.innerHTML = '<path d="M21 12.8A9 9 0 1111.2 3a7 7 0 009.8 9.8z"/>';
    if (text) text.textContent = '深色模式';
  }
  const loginIcon = $('#loginThemeIcon');
  if (loginIcon) loginIcon.innerHTML = icon.innerHTML;
  const loginBtn = $('#loginTheme');
  if (loginBtn) {
    const label = t === 'dark' ? '切换到浅色模式' : '切换到深色模式';
    loginBtn.title = label;
    loginBtn.setAttribute('aria-label', label);
  }
}

/* ---------------- 启动 ---------------- */

function updateUserMenu() {
  const u = state.user || {};
  const name = u.username || 'admin';
  const initial = (name.trim()[0] || 'A').toUpperCase();
  const avatar = $('#userAvatar');
  if (avatar) avatar.textContent = initial;
  const label = $('#userName');
  if (label) label.textContent = name;
  const popName = $('#userPopName');
  if (popName) popName.textContent = name;
  const popEmail = $('#userPopEmail');
  if (popEmail) popEmail.textContent = u.email || '未设置邮箱';
}

function closeUserPop() {
  const pop = $('#userPop');
  if (pop) pop.classList.remove('open');
  const btn = $('#userBtn');
  if (btn) btn.setAttribute('aria-expanded', 'false');
}

// 首屏先查会话：未登录直接显示登录页，避免先渲染一屏 401 骨架。
async function initSession() {
  try {
    const r = await fetch('/api/auth/session', {
      headers: { Accept: 'application/json' },
      credentials: 'same-origin',
    });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const d = await r.json();
    state.authenticated = !!d.authenticated;
    state.user = d.user || null;
  } catch (e) {
    state.authenticated = false;
    state.user = null;
  }
  state.authRequired = !state.authenticated;
  updateUserMenu();
}

async function boot() {
  const params = new URLSearchParams(location.search);

  const qTheme = params.get('theme');
  const lsTheme = lsGet(LS_THEME, LS_THEME_LEGACY);
  const hadThemePref = qTheme === 'dark' || qTheme === 'light' || !!lsTheme;
  applyTheme(qTheme === 'dark' || qTheme === 'light' ? qTheme
    : (lsTheme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')));

  loadServerTheme().then(() => {
    // 服务端模式只是默认值：用户手动切换过就尊重本机选择。
    if (!hadThemePref && serverTheme &&
        (serverTheme.mode === 'light' || serverTheme.mode === 'dark')) {
      applyTheme(serverTheme.mode, false);
    }
  });

  $('#themeBtn').onclick = () => {
    applyTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
  };

  $('#refreshBtn').onclick = () => { state.data = {}; render(); };

  const userBtn = $('#userBtn');
  if (userBtn) {
    userBtn.onclick = (e) => {
      e.stopPropagation();
      const pop = $('#userPop');
      const open = !pop.classList.contains('open');
      pop.classList.toggle('open', open);
      userBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
    };
    document.addEventListener('click', (e) => {
      const menu = $('#userMenu');
      if (menu && !menu.contains(e.target)) closeUserPop();
    });
  }
  const topLogout = $('#userLogoutBtn');
  if (topLogout) topLogout.onclick = doLogout;

  let st;
  $('#searchInput').addEventListener('input', (e) => {
    state.search = e.target.value.trim().toLowerCase();
    clearTimeout(st);
    st = setTimeout(render, 180);
  });

  document.addEventListener('keydown', (e) => {
    if (e.target.tagName === 'INPUT') {
      if (e.key === 'Escape') e.target.blur();
      return;
    }
    if (e.key === '/') { e.preventDefault(); $('#searchInput').focus(); }
    if (e.key === 't' || e.key === 'T') $('#themeBtn').click();
    if (e.key === 'r' || e.key === 'R') $('#refreshBtn').click();
    if (e.key === 'g') { location.hash = '#/overview'; }
  });

  window.addEventListener('hashchange', render);

  await initSession();
  document.body.classList.remove('auth-checking');
  render();

  // ---- 数据页静默轮询 ----
  // 概览/节点列表每 5 秒比对一次数据：只有内容变化才重绘。
  // Agent 默认 30 秒上报一次；5 秒一轮能把"刚上报的数据"尽快显示出来。
  // 每次成功拉取都会刷新顶栏的同步时间——即使指标没变，用户也能看到页面在更新。
  const LIVE_ROUTES = ['overview', 'nodes'];
  const modalOpen = () => !!document.querySelector('dialog[open], .modal-mask');
  // 表单页重绘会重建输入区，正在编辑时先不打断。
  const formBusy = () => {
    if (modalOpen()) return true;
    const el = document.activeElement;
    return !!el && /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName);
  };
  // 节点列表的勾选状态存在 DOM 里，重绘会清空批量操作的选择。
  const listBusy = () => modalOpen() || !!document.querySelector('#view .row-sel:checked');
  const tickLive = async () => {
    if (document.hidden || state.authRequired || !state.authenticated) return;
    if (LIVE_ROUTES.indexOf(state.route) < 0) return;
    if (state.route === 'nodes' && state.param) return; // 详情页单独节奏
    if (state.rendering) return;
    try {
      const [ov, nodes] = await Promise.all([api('/api/overview'), api('/api/nodes')]);
      markSynced();
      const sig = JSON.stringify([ov, nodes]);
      if (sig !== state.liveSig) {
        state.liveSig = sig;
        // 节点列表视图直接复用这次拉到的数据；否则 viewNodes 会读到旧缓存，
        // 每 5 秒用同一份旧数据重绘（并丢掉本次刷新）。
        state.data.nodes = nodes;
        state.livePending = true;
      }
      if (!state.livePending) return; // 数据没变，不重绘
      if (modalOpen()) return;
      // 列表页优先原位更新变化的行；结构变化（增删节点、筛选结果、告警集合）才整页重绘。
      // 局部更新只替换数据列，勾选与操作列不动，已勾选时也能安全刷新。
      if (patchLive(ov, nodes)) {
        state.livePending = false;
        return;
      }
      // 退回整页重绘：会重建勾选列并清空批量选择，等用户取消勾选后再画。
      if (listBusy()) return;
      state.livePending = false;
      render();
    } catch (e) {
      // 轮询失败保持旧数据，等下一轮；真正的会话失效由 api() 内的 401 分支处理
    }
  };

  // 节点详情页每 5 秒比对一次数据，图表只在内容变化时才重绘。
  // 注意它不能用上面的 tickLive —— 那条路径会跳过详情页。
  let detailFetching = false;
  const tickDetail = async () => {
    if (document.hidden || state.authRequired || !state.authenticated) return;
    if (!(state.route === 'nodes' && state.param)) return;
    if (state.rendering || detailFetching || modalOpen()) return;
    const id = state.param;
    const range = state.range;
    detailFetching = true;
    try {
      const pair = await Promise.all([
        api('/api/nodes/' + id),
        api(`/api/nodes/${id}/series?range=${range}`),
      ]);
      markSynced();
      const sig = JSON.stringify(pair);
      if (sig === state.detailSig) return;
      // 交给 viewNodeDetail 复用，避免重绘时再请求一次。
      state.detailCache = { key: id + '|' + range, detail: pair[0], series: pair[1], at: Date.now() };
      render();
    } catch (e) {
      // 与列表轮询一致：失败保留旧图，下一轮再试
    } finally {
      detailFetching = false;
    }
  };

  // SSE 负责"变更即推"，这里的兜底轮询只处理"时间流逝型"状态变化
  // （在线 → 掉线，没有任何写操作，无法被事件触发），30 秒足够。
  setInterval(tickLive, 30000);
  setInterval(tickDetail, 5000);

  // ---- SSE：服务端主动通知"数据变了" ----
  // 为什么不是 WebSocket：EventSource 是浏览器原生能力，自动重连、
  // 带同源 Cookie、穿透反代，服务端用标准库就能实现（零依赖）。
  // 收到 change 后复用 tickLive 的签名比对逻辑，数据没变不会重绘，
  // 因此不会出现"每次上报都闪一下"。
  window.__ensureStream = function () {
    if (typeof EventSource === 'undefined') return;
    if (liveStream && liveStream.readyState !== 2) return;
    try {
      liveStream = new EventSource('/api/stream');
    } catch (e) {
      liveStream = null;
      return;
    }
    liveStream.addEventListener('change', function () { tickLive(); });
    liveStream.addEventListener('hello', function () { state.streamAlive = true; });
    liveStream.addEventListener('ping', function () { state.streamAlive = true; });
    liveStream.onerror = function () {
      // readyState=2 表示浏览器已放弃重连（例如 401/服务端不支持）。
      // 这种情况下不要死循环重连，等下一次成功渲染再重建。
      state.streamAlive = false;
      if (liveStream && liveStream.readyState === 2) {
        liveStream.close();
        liveStream = null;
      }
    };
  };
  window.__closeStream = function () {
    if (liveStream) {
      liveStream.close();
      liveStream = null;
    }
    state.streamAlive = false;
  };
  // 暴露给运维/调试：不开 DevTools 也能从控制台手动触发一次刷新。
  window.__tickLive = tickLive;
  // 启动时就尝试建连。不能只依赖 render()：render 是异步的，
  // 若它先于本段定义执行（脚本尚未跑到这里），那一次调用会被跳过，
  // 而之后没有数据变化就不会再 render —— SSE 整场都不会建立。
  setTimeout(function () { if (window.__ensureStream) window.__ensureStream(); }, 0);

  // 切回标签页/窗口获得焦点时立即刷新一次，不用等下一个 5 秒
  const tickNow = () => { tickLive(); tickDetail(); };
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) tickNow();
  });
  window.addEventListener('focus', tickNow);

  // 其它页面（设置、规则、令牌等含表单）保持 30 秒整体重绘，避免打断编辑
  setInterval(() => {
    if (document.hidden || state.authRequired) return;
    if (LIVE_ROUTES.indexOf(state.route) >= 0) return;
    if (formBusy()) return;
    render();
  }, 30000);
}

boot();

// 图表与表格增强：都依赖 #view 内新渲染出的 DOM，
// 用 MutationObserver 比在每个视图函数尾部手动调用更不容易漏。
bindChartHover();
new MutationObserver(() => {
  requestAnimationFrame(() => {
    bindChartHover();
    enhanceTables();
  });
}).observe($('#view'), { childList: true, subtree: true });

// showAgentWizard 一站式接入向导：填台账 → 创建节点 → 直接给出安装命令。
// 以前要「设置页建节点 → 复制 JSON → 手拼 systemd」，现在三步并一步。
function showAgentWizard() {
  if (!state.authenticated) { showLoginGate(); return; }
  const dlg = document.createElement('div');
  dlg.className = 'modal-mask';
  dlg.innerHTML = `<div class="modal" style="max-width:560px">
    <div class="modal-title">接入新节点</div>
    <p class="modal-sub">填写台账信息并创建，随后会生成一条在目标服务器上执行的安装命令。</p>
    <form id="wizardForm" class="form-grid">
      ${field('名称', 'name', '', { required: true, placeholder: '例如 racknerd-纽约' })}
      ${field('服务商', 'provider', '', { placeholder: 'racknerd / 腾讯云 …' })}
      ${field('位置', 'region', '', { placeholder: '美国 / 香港 …' })}
      ${field('分组', 'group', 'prod', { placeholder: 'prod / test / probe' })}
      ${field('到期日', 'expireAt', '', { placeholder: '2027-06-01' })}
      ${field('年付价', 'price', '', { type: 'number', placeholder: '11.00' })}
      ${selectField('币种', 'currency', 'USD', [{ v: 'USD', t: 'USD ($)' }, { v: 'CNY', t: 'CNY (¥)' }])}
      <div class="form-actions">
        <button class="btn" type="button" id="wizCancel">取消</button>
        <button class="btn primary" type="submit">创建并生成命令</button>
      </div>
    </form>
  </div>`;
  document.body.appendChild(dlg);
  const close = () => dlg.remove();
  dlg.querySelector('#wizCancel').onclick = close;
  dlg.onclick = (e) => { if (e.target === dlg) close(); };
  dlg.querySelector('#wizardForm').onsubmit = async (e) => {
    e.preventDefault();
    const d = formData(e.target);
    const btn = e.target.querySelector('button[type=submit]');
    btn.disabled = true;
    try {
      const r = await adminApi('/api/admin/nodes', { method: 'POST', body: JSON.stringify({
        name: d.name, provider: d.provider, region: d.region, group: d.group,
        expireAt: d.expireAt, price: Number(d.price) || 0, currency: d.currency,
      }) });
      close();
      showAgentConfig(r);
      // 刷新当前视图，让新节点立刻出现在列表
      if (typeof render === 'function') render();
    } catch (err) {
      btn.disabled = false;
      toast('创建失败：' + err.message, 'err');
    }
  };
}
