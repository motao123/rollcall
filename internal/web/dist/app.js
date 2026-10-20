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
  search: '',
  data: {},
  panelLink: '',
  authRequired: false,
  authenticated: false,
  user: null,
  timers: [],
};

/* ---------------- 工具 ---------------- */

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

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

/** 坐标轴用的紧凑速率格式：293K / 1.2M / 45B */
function rate(v) {
  v = Number(v) || 0;
  if (v >= 1048576) return (v / 1048576).toFixed(1) + 'M';
  if (v >= 1024) return Math.round(v / 1024) + 'K';
  return Math.round(v) + 'B';
}

function dur(sec) {  sec = Number(sec) || 0;
  if (sec < 60) return sec + 's';
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d}天${h}时`;
  if (h > 0) return `${h}时${m}分`;
  return m + '分';
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

function clock(ts) {
  const d = new Date(ts * 1000);
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
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

/** 大图：带网格、坐标、面积填充 */
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

  return `<svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" style="width:100%;height:${h}px;display:block">
    <defs><linearGradient id="${id}" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0%" stop-color="${color}" stop-opacity="0.20"/>
      <stop offset="100%" stop-color="${color}" stop-opacity="0"/>
    </linearGradient></defs>
    ${grid}
    <path d="${area}" fill="url(#${id})"/>
    <path d="${line}" fill="none" stroke="${color}" stroke-width="1.8" stroke-linejoin="round" stroke-linecap="round"/>
  </svg>`;
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
    <div class="kpi-value mono">${value}${unit ? `<span class="kpi-unit">${unit}</span>` : ''}</div>
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

  return `<article class="node-card ${cardCls} ${n.alerts > 0 ? 'alerting' : ''}" data-node="${esc(n.id)}">
    <div class="nc-head">
      <span class="dot ${st.tone}"></span>
      <span class="nc-name">${esc(n.name)}</span>
      <span class="nc-tag">${esc(n.provider || '—')}</span>
      ${expireBadge}${agentBadge}
      <span class="nc-status">${st.text}</span>
    </div>
    <div class="nc-bars">
      ${barRow('CPU', cpu, 75, 90)}
      ${barRow('内存', mem, 80, 92)}
      ${barRow('磁盘', disk, 80, 90)}
    </div>
    <div class="nc-spark">${sparkline(spark, st.tone === 'ok' ? 'var(--accent)' : 'var(--fg-3)')}</div>
    <div class="nc-foot">
      <span class="mono">${esc(n.region || '—')}</span>
      <span class="sep">·</span>
      <span class="mono" title="累计入站">${hasData ? '↓' + bytes(n.latest.netRx) : '—'}</span>
      <span class="sep">·</span>
      <span class="mono" title="累计出站">${hasData ? '↑' + bytes(n.latest.netTx) : '—'}</span>
      <span class="sep">·</span>
      <span class="mono">${hasData ? dur(n.latest.uptime) : '—'}</span>
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

async function viewOverview() {
  const [ov, nodes] = await Promise.all([api('/api/overview'), api('/api/nodes')]);
  state.data.nodes = nodes;
  // 静默轮询用它判断"数据是否真的变了"，避免每次轮询都整体重绘：
  // 重绘会重建搜索框、打断输入，也会让 KPI 数字闪一下。
  state.liveSig = JSON.stringify([ov, nodes]);
  setAlertBadge(ov.activeAlert || 0);

  const expiring = ov.expiring || [];
  const lists = nodes.filter((n) => !state.search || n.name.toLowerCase().includes(state.search));
  const alerting = nodes.filter((n) => n.alerts > 0);
  const countParts = [`在线 ${ov.online}`];
  if (ov.stale) countParts.push(`数据延迟 ${ov.stale}`);
  if (ov.offline) countParts.push(`离线 ${ov.offline}`);
  if (ov.noData) countParts.push(`待接入 ${ov.noData}`);

  // 服务商分布：按 provider 聚合（空值归「未分类」）
  const byProvider = new Map();
  for (const n of lists) {
    const p = n.provider || '未分类';
    if (!byProvider.has(p)) byProvider.set(p, []);
    byProvider.get(p).push(n);
  }
  const providerRows = [...byProvider.entries()]
    .map(([p, ns]) => ({
      p, ns,
      online: ns.filter((n) => n.online).length,
      cpu: ns.filter((n) => hasNodeData(n)).reduce((a, n) => a + (n.latest.cpuPct || 0), 0) / Math.max(1, ns.filter((n) => hasNodeData(n)).length),
    }))
    .sort((a, b) => b.ns.length - a.ns.length);

  $('#view').innerHTML = `
    <div class="kpis">
      ${kpi('节点总数', ov.nodes, '', countParts.join(' · '))}
      ${kpi('活跃告警', ov.activeAlert, '', ov.activeAlert > 0 ? '需关注' : '全部正常')}
      ${kpi('平均 CPU', ov.avgCpu.toFixed(1), '%', '在线节点均值')}
      ${kpi('平均内存', ov.avgMem.toFixed(1), '%', '在线节点均值')}
      ${kpi('服务探测', `${ov.services - ov.svcDown}/${ov.services}`, '', ov.svcDown > 0 ? `${ov.svcDown} 个不可用` : '全部可用')}
      ${kpi('月度成本', '$' + ov.monthlyCost.toFixed(1), '', `${expiring.length} 台 30 天内到期`)}
      ${kpi('累计流量', bytes(ov.trafficIn || 0), '↓', '↑ ' + bytes(ov.trafficOut || 0))}
    </div>

    ${alerting.length ? `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">正在告警</h2>
        <span class="section-meta">${alerting.length} 台节点异常</span>
      </div>
      <div class="panel">
        ${alerting.map((n) => {
          const sev = n.state === 'offline' || n.state === 'no_data' ? 'critical' : 'warn';
          const detail = n.state === 'stale'
            ? `数据延迟 · 最后上报 ${ago(n.lastSeen)}`
            : !n.online
              ? `已失联 · 最后上报 ${ago(n.lastSeen)}`
              : `CPU ${(n.latest.cpuPct || 0).toFixed(0)}% · 内存 ${pct(n.latest.memUsed, n.latest.memTotal).toFixed(0)}% · 磁盘 ${pct(n.latest.diskUsed, n.latest.diskTotal).toFixed(0)}%`;
          return `
          <div class="alert-item" style="cursor:pointer" data-node="${esc(n.id)}">
            <div class="alert-bar ${sev}"></div>
            <div class="alert-main">
              <div class="alert-title">${esc(n.name)}<span class="badge ${sev === 'critical' ? 'crit' : 'warn'}">${n.alerts} 条</span></div>
              <div class="alert-msg">${detail}</div>
            </div>
          </div>`;
        }).join('')}
      </div>
    </section>` : ''}

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">节点状态</h2>
        <span class="section-meta">${lists.length} 台 · 按服务商分组</span>
        <button class="btn primary" id="wizBtn" style="padding:6px 14px;font-size:12.5px">＋ 接入节点</button>
      </div>
      <div class="prov-strip">
        ${providerRows.map((r) => `
          <button class="prov-chip" data-prov="${esc(r.p)}" title="CPU 均值 ${r.cpu.toFixed(0)}%">
            <span class="prov-name">${esc(r.p)}</span>
            <span class="prov-cnt">${r.online}/${r.ns.length}</span>
          </button>`).join('')}
      </div>
      <div class="table-wrap"><table class="node-table">
        <thead><tr>
          <th>状态</th><th>节点</th><th>位置</th><th>系统</th>
          <th class="num">CPU</th><th class="num">内存</th><th class="num">磁盘</th>
          <th class="num">负载</th><th class="num">流量 ↓ / ↑</th><th class="num">在线</th><th>24h CPU</th>
        </tr></thead>
        <tbody>
          ${providerRows.map((r) => `
            <tr class="prov-row" id="provrow-${esc(r.p)}"><td colspan="11">
              <span class="prov-title">${esc(r.p)}</span>
              <span class="prov-meta">${r.ns.length} 台 · 在线 ${r.online} · CPU 均值 ${r.cpu.toFixed(0)}%</span>
            </td></tr>
            ${r.ns.map((n) => {
              const hasData = hasNodeData(n);
              const st = nodeState(n);
              const cpu = hasData ? n.latest.cpuPct || 0 : null;
              const mem = hasData ? pct(n.latest.memUsed, n.latest.memTotal) : null;
              const disk = hasData ? pct(n.latest.diskUsed, n.latest.diskTotal) : null;
              const days = n.expireAt ? Math.ceil((new Date(n.expireAt) - Date.now()) / 86400000) : null;
              return `<tr data-node="${esc(n.id)}" style="cursor:pointer">
            <td><span class="dot ${st.tone}"></span> <span class="state-text">${st.text}</span></td>
            <td class="strong">${esc(n.name)}${days != null && days <= 30 ? ` <span class="badge ${days < 0 ? 'crit' : days <= 7 ? 'crit' : 'warn'}" title="到期 ${esc(n.expireAt)}">${days < 0 ? '已过期' : days + '天'}</span>` : ''}${n.agentOutdated ? ` <span class="badge warn" title="Agent ${esc(n.agentVersion || '')} 低于控制端">可升级</span>` : ''}</td>
            <td>${esc(n.region || '—')}</td>
            <td style="font-size:11.5px">${esc(n.host?.os || '—')}</td>
            <td class="num">${tbar(cpu)}</td>
            <td class="num">${tbar(mem, 80, 92)}</td>
            <td class="num">${tbar(disk, 80, 90)}</td>
            <td class="num mono">${hasData ? (n.latest.load1 || 0).toFixed(2) : '—'}</td>
            <td class="num mono" style="font-size:11px">${hasData ? bytes(n.latest.netRx) + ' / ' + bytes(n.latest.netTx) : '—'}</td>
            <td class="num mono">${hasData ? dur(n.latest.uptime) : '—'}</td>
            <td><div class="tspark">${sparkline((n.spark || []).slice(-40).map((p) => p.v), st.tone === 'ok' ? 'var(--accent)' : 'var(--fg-3)', 26)}</div></td>
          </tr>`;
            }).join('')}
          `).join('') || `<tr><td colspan="11"><div class="empty">没有匹配的节点</div></td></tr>`}
        </tbody>
      </table></div>
    </section>
  `;

  const wiz = $('#wizBtn');
  if (wiz) wiz.onclick = showAgentWizard;
  $$('.prov-chip').forEach((c) => {
    c.onclick = () => {
      const g = document.getElementById('provrow-' + c.dataset.prov);
      if (g) g.scrollIntoView({ behavior: 'smooth', block: 'center' });
    };
  });
  bindNodeClicks();
}

async function viewNodes() {
  const nodes = state.data.nodes || await api('/api/nodes');
  state.data.nodes = nodes;
  const list = nodes.filter((n) => !state.search || n.name.toLowerCase().includes(state.search));

  $('#view').innerHTML = `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">全部节点</h2>
        <span class="section-meta">${list.length} 台</span>
        <button class="btn" id="batchBtn" style="padding:6px 14px;font-size:12.5px">批量操作</button>
        <button class="btn primary" id="wizBtn2" style="padding:6px 14px;font-size:12.5px">＋ 接入节点</button>
      </div>
      <div id="batchBar" hidden class="batch-bar">
        <span style="font-size:12px;color:var(--fg-2)">已选 <b id="batchCount" class="mono">0</b> 台</span>
        <span style="flex:1"></span>
        <button class="btn tiny" data-batch="group">改分组</button>
        <button class="btn tiny" data-batch="provider">改服务商</button>
        <button class="btn tiny" data-batch="region">改地区</button>
        <button class="btn tiny" data-batch="expire">改到期日</button>
        <button class="btn tiny" data-batch="currency">改币种</button>
        <button class="btn tiny danger" data-batch="delete">删除</button>
      </div>
      <div class="table-wrap"><table class="nodes-table">
        <thead><tr>
          <th style="width:26px"><input type="checkbox" id="selAll" aria-label="全选"></th>
          <th>状态</th><th>节点</th><th>服务商</th><th>位置</th><th>系统</th>
          <th class="num">CPU</th><th class="num">内存</th><th class="num">磁盘</th>
          <th class="num">负载</th><th class="num">运行时长</th><th class="num">流量</th>
        </tr></thead>
        <tbody>${list.map((n) => {
          const hasData = hasNodeData(n);
          const st = nodeState(n);
          const cpu = hasData ? n.latest.cpuPct || 0 : null;
          const mem = hasData ? pct(n.latest.memUsed, n.latest.memTotal) : null;
          const disk = hasData ? pct(n.latest.diskUsed, n.latest.diskTotal) : null;
          const dash = '—';
          return `<tr data-node="${esc(n.id)}" style="cursor:pointer">
            <td><input type="checkbox" class="row-sel" data-id="${esc(n.id)}" aria-label="选择 ${esc(n.name)}"></td>
            <td><span class="dot ${st.tone}"></span> <span class="state-text">${st.text}</span></td>
            <td class="strong">${esc(n.name)}${n.agentOutdated ? ` <span class="badge warn" title="Agent ${esc(n.agentVersion || '')} 低于控制端">可升级</span>` : ''}</td>
            <td>${esc(n.provider || '—')}</td>
            <td>${esc(n.region || '—')}</td>
            <td style="font-size:11.5px">${esc(n.host?.os || '—')}</td>
            <td class="num mono" style="color:${cpu > 85 ? 'var(--crit)' : 'inherit'}">${cpu == null ? dash : cpu.toFixed(0) + '%'}</td>
            <td class="num mono">${mem == null ? dash : mem.toFixed(0) + '%'}</td>
            <td class="num mono" style="color:${disk > 88 ? 'var(--crit)' : 'inherit'}">${disk == null ? dash : disk.toFixed(0) + '%'}</td>
            <td class="num mono">${hasData ? (n.latest.load1 || 0).toFixed(2) : dash}</td>
            <td class="num mono">${hasData ? dur(n.latest.uptime) : dash}</td>
            <td class="num mono">${hasData ? bytes(n.latest.netRx) : dash}</td>
          </tr>`;
        }).join('') || `<tr><td colspan="12"><div class="empty">没有匹配的节点</div></td></tr>`}</tbody>
      </table></div>
    </section>`;
  bindNodeClicks();
  const wiz2 = document.getElementById('wizBtn2');
  if (wiz2) wiz2.onclick = showAgentWizard;
  bindBatchOps(list);

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
        if (!confirm(`确认删除 ${ids.length} 台节点？\n\n${names.join('、')}\n\n历史数据会一并删除，且无法恢复。`)) return;
      } else {
        const p = PROMPTS[action];
        const v = prompt(`${p.label}\n（将应用到 ${ids.length} 台节点）`, p.def);
        if (v === null) return;
        try {
          await adminApi('/api/admin/nodes/batch', {
            method: 'POST',
            body: JSON.stringify({ ids, action, value: v.trim() }),
          });
          toast(`已更新 ${ids.length} 台节点`);
          state.data.nodes = null;
          viewNodes();
        } catch (err) { toast('批量操作失败：' + err.message, 'err'); }
        return;
      }
      try {
        const res = await adminApi('/api/admin/nodes/batch', {
          method: 'POST',
          body: JSON.stringify({ ids, action: 'delete' }),
        });
        toast(`已删除 ${res.changed} 台节点`);
        state.data.nodes = null;
        viewNodes();
      } catch (err) { toast('删除失败：' + err.message, 'err'); }
    };
  });
}

async function viewNodeDetail(id) {
  const [detail, series] = await Promise.all([
    api('/api/nodes/' + id),
    api(`/api/nodes/${id}/series?range=${state.range}`),
  ]);
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
  const loadVals = pts.map((p) => p.load1);
  const last = detail.latest || {};
  const traffic = detail.traffic || {};
  const trafficUsed = (traffic.usedRx || 0) + (traffic.usedTx || 0);
  const quotaBytes = (traffic.quotaGb || 0) * 1024 * 1024 * 1024;
  const quotaPctVal = quotaBytes > 0 ? (trafficUsed / quotaBytes) * 100 : 0;
  const mounts = last.mounts || [];
  const nics = last.nics || [];

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
          <h2 style="font-size:17px;font-weight:650;margin:0;letter-spacing:-.02em">${esc(n.name)}</h2>
          <span class="nc-tag">${esc(n.provider || '—')}</span>
          <span class="nc-tag">${esc(n.group || '—')}</span>
          <span class="badge ${st.tone === 'off' ? 'mute' : st.tone}">${st.text}</span>
        </div>
        <p style="font-size:12px;color:var(--fg-3);margin:3px 0 0">
          ${esc(host.hostname || '—')} · ${esc(host.os || '—')} · ${esc(host.kernel || '')} · ${host.cores || '?'} 核
        </p>
      </div>
      <div class="range-tabs">
        ${['1h', '6h', '24h', '7d', '30d', '365d'].map((r) => `<button class="${state.range === r ? 'on' : ''}" data-range="${r}">${r}</button>`).join('')}
      </div>
    </div>

    <div class="detail-grid">
      <div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">CPU 使用率</span>
            <span class="chart-val mono" style="color:${hasData && (last.cpuPct || 0) > 85 ? 'var(--crit)' : 'var(--fg)'}">${hasData ? (last.cpuPct || 0).toFixed(1) + '%' : '—'}</span></div>
          ${areaChart(cpuVals, 'var(--accent)')}
        </div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">内存使用率</span>
            <span class="chart-val mono">${hasData ? `${pct(memUsed, memTotal).toFixed(1)}%<span style="font-size:11px;color:var(--fg-3);font-weight:400"> · ${bytes(memUsed)} / ${bytes(memTotal)}</span>` : '—'}</span></div>
          ${areaChart(memVals, 'var(--accent-2)')}
        </div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">磁盘使用率</span>
            <span class="chart-val mono">${hasData ? `${pct(diskUsed, diskTotal).toFixed(1)}%<span style="font-size:11px;color:var(--fg-3);font-weight:400"> · ${bytes(diskUsed)} / ${bytes(diskTotal)}</span>` : '—'}</span></div>
          ${areaChart(diskVals, 'var(--warn)')}
        </div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">网络吞吐速率</span>
            <span class="chart-val mono" style="font-size:13px">${hasData ? `↓${bytes(last.netRx)} ↑${bytes(last.netTx)} <span style="color:var(--fg-3);font-weight:400">累计</span>` : '—'}</span></div>
          ${areaChart(netVals, 'var(--ok)', { height: 110, fmt: (v) => rate(v) })}
        </div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">1 分钟负载</span>
            <span class="chart-val mono">${hasData ? (last.load1 || 0).toFixed(2) : '—'}</span></div>
          ${areaChart(loadVals, 'var(--fg-2)', { height: 100 })}
        </div>
      </div>

      <div>
        <div class="panel panel-pad" style="margin-bottom:12px">
          <div class="section-title" style="margin-bottom:8px">系统信息</div>
          <div class="info-list">
            <div class="info-row"><span class="info-k">主机名</span><span class="info-v mono">${esc(host.hostname || '—')}</span></div>
            <div class="info-row"><span class="info-k">操作系统</span><span class="info-v">${esc(host.os || '—')}</span></div>
            <div class="info-row"><span class="info-k">内核</span><span class="info-v mono">${esc(host.kernel || '—')}</span></div>
            <div class="info-row"><span class="info-k">架构</span><span class="info-v mono">${esc(host.arch || '—')}</span></div>
            <div class="info-row"><span class="info-k">CPU</span><span class="info-v" title="${esc(host.cpuModel || '')}">${esc((host.cpuModel || '—').slice(0, 22))}</span></div>
            <div class="info-row"><span class="info-k">虚拟化</span><span class="info-v mono">${esc(host.virt || '—')}</span></div>
            <div class="info-row"><span class="info-k">负载 1/5/15</span><span class="info-v mono">${hasData ? `${(last.load1 || 0).toFixed(2)} / ${(last.load5 || 0).toFixed(2)} / ${(last.load15 || 0).toFixed(2)}` : '—'}</span></div>
            <div class="info-row"><span class="info-k">运行时长</span><span class="info-v mono">${hasData ? dur(last.uptime) : '—'}</span></div>
            <div class="info-row"><span class="info-k">TCP 连接</span><span class="info-v mono">${hasData ? last.tcpConn || 0 : '—'}</span></div>
            <div class="info-row"><span class="info-k">进程数</span><span class="info-v mono">${hasData ? last.procs || 0 : '—'}</span></div>
            <div class="info-row"><span class="info-k">Agent 版本</span><span class="info-v mono">${esc(n.agentVersion || '—')}${detail.agentOutdated ? ' <span class="badge warn">可升级</span>' : ''}</span></div>
            <div class="info-row"><span class="info-k">最后上报</span><span class="info-v">${ago(detail.lastSeen)}</span></div>
          </div>
        </div>

        <div class="panel panel-pad">
          <div class="section-title" style="margin-bottom:8px">资产</div>
          <div class="info-list">
            <div class="info-row"><span class="info-k">服务商</span><span class="info-v">${esc(n.provider || '—')}</span></div>
            <div class="info-row"><span class="info-k">位置</span><span class="info-v">${esc(n.region || '—')}</span></div>
            <div class="info-row"><span class="info-k">分组</span><span class="info-v">${esc(n.group || '—')}</span></div>
            <div class="info-row"><span class="info-k">到期日</span><span class="info-v mono">${esc(n.expireAt || '—')}</span></div>
            <div class="info-row"><span class="info-k">年付价</span><span class="info-v mono">${n.price ? (n.currency === 'CNY' ? '¥' : '$') + n.price : '—'}</span></div>
          </div>
        </div>

        <div class="panel panel-pad" style="margin-top:12px">
          <div class="section-title" style="margin-bottom:8px">流量配额</div>
          ${quotaBytes > 0 ? `
            <div style="display:flex;justify-content:space-between;font-size:12px;margin-bottom:6px">
              <span class="mono">${bytes(trafficUsed)}</span>
              <span class="field-hint">配额 ${traffic.quotaGb} GB · 每月 ${traffic.resetDay || 1} 日重置</span>
            </div>
            <div class="meter"><i style="width:${Math.min(100, quotaPctVal).toFixed(1)}%;background:${quotaPctVal >= 100 ? 'var(--crit)' : quotaPctVal >= 90 ? 'var(--warn)' : 'var(--ok)'}"></i></div>
            <div class="field-hint" style="margin-top:6px">已用 ${quotaPctVal.toFixed(1)}%${traffic.cycleStart ? ' · 周期起点 ' + clock(traffic.cycleStart) : ''}</div>
          ` : `
            <div class="field-hint">未设置月度配额。可在 <a href="#/settings">设置 → 节点管理 → 编辑</a> 中填写，超过阈值时由「流量配额使用率」规则告警。</div>
          `}
        </div>

        <div class="panel panel-pad" style="margin-top:12px">
          <div class="section-title" style="margin-bottom:8px">采集明细</div>
          <div class="info-list">
            <div class="info-row"><span class="info-k">磁盘 I/O（累计）</span><span class="info-v mono" style="font-size:11.5px">读 ${bytes(last.diskReadBytes || 0)}<br>写 ${bytes(last.diskWriteBytes || 0)}</span></div>
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
  $$('.range-tabs button', $('#view')).forEach((b) => {
    b.onclick = () => { state.range = b.dataset.range; viewNodeDetail(id); };
  });
}

async function viewServices() {
  const svcs = await api('/api/services');
  $('#view').innerHTML = `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">服务探测</h2>
        <span class="section-meta">${svcs.length} 个目标 · 由服务端主动探测</span>
      </div>
      <div class="table-wrap"><table class="services-table">
        <thead><tr><th>状态</th><th>名称</th><th>类型</th><th>目标</th>
          <th class="num">延迟</th><th class="num">丢包 / 抖动</th><th class="num">证书</th><th class="num">24h 可用率</th><th>最近 2 小时</th></tr></thead>
        <tbody>${svcs.map((s) => {
          const spark = (s.spark || []).slice(-120).map((p) => p[1]);
          const up = s.uptime || 0;
          const cd = s.certDays;
          const certCell = cd === 0
            ? '<span class="badge crit">已过期</span>'
            : cd > 0
              ? `<span class="badge ${cd <= 14 ? 'crit' : cd <= 30 ? 'warn' : 'mute'}">${cd} 天</span>`
              : '—';
          const loss = Number(s.lossPct || 0);
          const jitter = Number(s.jitterMs || 0);
          return `<tr>
            <td><span class="dot ${s.ok ? 'ok' : 'crit'}"></span></td>
            <td class="strong">${esc(s.name)}</td>
            <td><span class="badge mute">${esc(s.type.toUpperCase())}</span></td>
            <td class="mono" style="font-size:11.5px;max-width:240px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(s.target)}</td>
            <td class="num mono">${s.latencyMs ? s.latencyMs + 'ms' : '—'}</td>
            <td class="num mono" style="font-size:11.5px">${loss > 0 ? `<span style="color:${loss >= 10 ? 'var(--crit)' : 'var(--warn)'}">${loss.toFixed(1)}%</span>` : '0%'} / ${jitter ? jitter.toFixed(0) + 'ms' : '—'}</td>
            <td class="num">${certCell}</td>
            <td class="num"><span class="badge ${up >= 99 ? 'ok' : up >= 95 ? 'warn' : 'crit'}">${up.toFixed(1)}%</span></td>
            <td style="width:150px;padding:6px 14px">
              <div style="height:22px">${sparkline(spark, s.ok ? 'var(--ok)' : 'var(--crit)', 22)}</div>
            </td>
          </tr>`;
        }).join('') || `<tr><td colspan="9"><div class="empty">尚未配置服务探测目标</div></td></tr>`}</tbody>
      </table></div>
    </section>`;
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

  const metricMap = {};
  (meta.metrics || []).forEach((m) => { metricMap[m.value] = m; });
  const sevLabel = { critical: '严重', warn: '警告', info: '信息' };

  const silences = d.silences || [];
  const silenceMap = {};
  silences.forEach((s) => { silenceMap[s.scope] = s; });
  const globalSil = silenceMap.global;
  const nq = d.notify || {};
  const silLabel = (s) => `至 ${clock(s.until)}${s.reason ? ' · ' + s.reason : ''}`;

  $('#view').innerHTML = `
    <div class="kpis" style="margin-bottom:20px">
      ${kpi('活跃告警', d.active, '', d.active ? '需要处理' : '全部恢复')}
      ${kpi('启用规则', (rules || []).filter((r) => r.enabled).length, '', `共 ${(rules || []).length} 条`)}
      ${kpi('历史记录', items.length, '', '最近 200 条')}
      ${kpi('待投递通知', nq.pending || 0, '条', `失败 ${nq.failed || 0} · 静默拦截 ${nq.suppressed || 0}`)}
    </div>

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
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">告警规则</h2>
        <span class="section-meta">阈值、持续时间与级别均可在此调整</span>
      </div>
      <div class="table-wrap"><table class="alerts-table">
        <thead><tr><th>规则</th><th>条件</th><th class="num">持续</th><th>级别</th><th class="num">启用</th><th class="num">操作</th></tr></thead>
        <tbody>${(rules || []).map((r) => {
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
            <td class="num mono">${durText(r.duration)}</td>
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
      <div style="margin-top:10px">
        <button class="btn primary" id="addRuleBtn">＋ 新增规则</button>
      </div>
    </section>

    <section class="section">
      <div class="section-head"><h2 class="section-title">告警记录</h2></div>
      <div class="panel">
        ${items.map((a) => {
          const active = !a.resolvedAt;
          return `<div class="alert-item">
            <div class="alert-bar ${a.severity}"></div>
            <div class="alert-main">
              <div class="alert-title">
                ${esc(a.ruleName)}
                <span class="badge ${active ? (a.severity === 'critical' ? 'crit' : 'warn') : 'mute'}">${active ? '进行中' : '已恢复'}</span>
                <span style="font-size:11px;color:var(--fg-3);font-weight:400">${esc(a.nodeName)}</span>
              </div>
              <div class="alert-msg">${esc(a.message)}</div>
              <div class="alert-time mono">${clock(a.startedAt)} 开始${a.resolvedAt ? ` · ${clock(a.resolvedAt)} 恢复` : ''}</div>
            </div>
          </div>`;
        }).join('') || `<div class="empty">暂无告警记录</div>`}
      </div>
    </section>`;

  // ---- 事件绑定 ----
  $$('[data-rule-toggle]').forEach((el) => {
    el.onchange = async () => {
      if (!canManage) { toast('请先在「设置」页配置管理令牌', 'err'); el.checked = !el.checked; return; }
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
      if (!canManage) { toast('请先在「设置」页配置管理令牌', 'err'); return; }
      if (!confirm('确认删除该规则？其进行中的告警会一并关闭。')) return;
      try {
        await adminApi('/api/admin/rules/' + b.dataset.ruleDel, { method: 'DELETE' });
        toast('规则已删除'); viewAlerts();
      } catch (e) { toast('删除失败：' + e.message, 'err'); }
    };
  });

  $$('[data-rule-edit]').forEach((b) => {
    b.onclick = () => {
      if (!canManage) { toast('请先在「设置」页配置管理令牌', 'err'); return; }
      const r = (rules || []).find((x) => x.id === b.dataset.ruleEdit);
      if (r) openRuleDialog(r, meta, nodes);
    };
  });

  $('#addRuleBtn').onclick = () => {
    if (!canManage) { toast('请先在「设置」页配置管理令牌', 'err'); return; }
    openRuleDialog(null, meta, nodes);
  };

  const globalSilenceBtn = $('#globalSilenceBtn');
  if (globalSilenceBtn) {
    globalSilenceBtn.onclick = () => {
      if (!canManage) { toast('请先在「设置」页配置管理令牌', 'err'); return; }
      openSilenceDialog('global', '全局通知', globalSil);
    };
  }
  const globalUnsilenceBtn = $('#globalUnsilenceBtn');
  if (globalUnsilenceBtn) {
    globalUnsilenceBtn.onclick = async () => {
      if (!canManage) { toast('请先在「设置」页配置管理令牌', 'err'); return; }
      try {
        await adminApi('/api/admin/silences/global', { method: 'DELETE' });
        toast('全局静默已取消');
        viewAlerts();
      } catch (e) { toast('取消失败：' + e.message, 'err'); }
    };
  }
  $$('[data-rule-silence]').forEach((b) => {
    b.onclick = () => {
      if (!canManage) { toast('请先在「设置」页配置管理令牌', 'err'); return; }
      const r = (rules || []).find((x) => x.id === b.dataset.ruleSilence);
      if (r) openSilenceDialog(r.id, r.name, silenceMap[r.id]);
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
        <select name="nodeIds" multiple size="4" style="min-height:86px;padding:6px;border:1px solid var(--line);border-radius:7px;background:var(--panel);color:var(--fg);font-size:12.5px">${nodeOptions}</select>
        <span class="field-hint">按住 Ctrl / Cmd 多选</span>
      </label>
      <label class="field" id="groupScope">
        <span class="field-label">定向分组（逗号分隔，留空=全部）</span>
        <input name="groups" value="${esc((r.groups || []).join(', '))}" placeholder="prod, test">
        <span class="field-hint">节点与分组建「或」关系；两者都留空表示全部节点</span>
      </label>
      <label class="field" id="svcScope" style="display:none">
        <span class="field-label">定向服务（可多选，留空=全部服务）</span>
        <select name="serviceIds" multiple size="4" style="min-height:86px;padding:6px;border:1px solid var(--line);border-radius:7px;background:var(--panel);color:var(--fg);font-size:12.5px">${svcOptions}</select>
        <span class="field-hint">仅服务类指标生效；服务未配置时保持为空</span>
      </label>
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

async function viewAssets() {
  const [d, renewals] = await Promise.all([
    api('/api/assets'),
    api('/api/renewals').catch(() => []),
  ]);
  const rows = d.rows || [];
  const renewRows = Array.isArray(renewals) ? renewals : [];
  const total = d.monthlyTotal || 0;

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
      ${kpi('30 天内到期', rows.filter((r) => r.days > 0 && r.days <= 30).length, '台', '需续费')}
      ${kpi('未来 90 天事项', upcoming.length, '条', '到期与续费')}
    </div>
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">续费日历</h2>
        <span class="section-meta">未来 12 个月 · 到期日与续费记录</span>
        <button class="btn primary" id="addRenewBtn" style="padding:6px 14px;font-size:12.5px">＋ 登记续费</button>
      </div>
      <div class="renew-grid">${monthHTML}</div>
    </section>
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">资产台账</h2>
        <span class="section-meta">按到期时间排序</span>
      </div>
      <div class="table-wrap"><table class="assets-table">
        <thead><tr><th>节点</th><th>服务商</th><th>位置</th><th>系统</th>
          <th>到期日</th><th class="num">剩余</th><th class="num">续费</th><th class="num">月摊</th></tr></thead>
        <tbody>${rows.map((r) => {
          const d2 = r.days;
          const cls = !d2 ? 'mute' : d2 <= 7 ? 'crit' : d2 <= 30 ? 'warn' : 'ok';
          return `<tr>
            <td class="strong">${esc(r.name)}</td>
            <td>${esc(r.provider || '—')}</td>
            <td>${esc(r.region || '—')}</td>
            <td style="font-size:11.5px">${esc(r.os || '—')}</td>
            <td class="mono">${esc(r.expireAt || '未登记')}</td>
            <td class="num"><span class="badge ${cls}">${d2 ? d2 + ' 天' : '—'}</span></td>
            <td class="num mono">${r.price ? (r.currency === 'CNY' ? '¥' : '$') + r.price : '—'}</td>
            <td class="num mono">$${(r.monthly || 0).toFixed(1)}</td>
          </tr>`;
        }).join('') || `<tr><td colspan="8"><div class="empty">台账为空</div></td></tr>`}</tbody>
      </table></div>
    </section>`;

  $('#addRenewBtn').onclick = () => {
    const opts = rows.map((r) => `<option value="${esc(r.id)}">${esc(r.name)}</option>`).join('');
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

  $$('[data-del-renew]').forEach((b) => {
    b.onclick = async (e) => {
      e.stopPropagation();
      if (!confirm('删除这条续费记录？')) return;
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
  const rows = d.nodes || [];
  const sum = d.summary || {};
  const groups = [...new Set(nodes.map((n) => n.group).filter(Boolean))].sort();

  const groupRows = (sum.groups || []).map((g) => `<tr>
    <td class="strong">${esc(g.group)}</td>
    <td class="num mono">${g.online}/${g.nodes}</td>
    <td class="num">${g.avgCpu.toFixed(1)}%</td>
    <td class="num">${g.avgMem.toFixed(1)}%</td>
    <td class="num mono">$${g.cost.toFixed(1)}</td>
    <td class="num mono">${rate(g.trafficIn)}/s</td>
  </tr>`).join('') || `<tr><td colspan="6"><div class="empty">没有可聚合的节点</div></td></tr>`;

  const nodeRows = rows.map((n) => {
    const quota = n.traffic ? n.traffic.quotaGb || 0 : 0;
    const used = n.traffic ? (n.traffic.usedRx || 0) + (n.traffic.usedTx || 0) : 0;
    const qp = quota > 0 ? (used / (quota * 1024 * 1024 * 1024)) * 100 : 0;
    const st = nodeState({ state: n.state, online: n.state === 'online', alerts: 0 });
    const days = n.expireAt ? Math.ceil((new Date(n.expireAt + 'T00:00:00') - Date.now()) / 86400000) : null;
    return `<tr data-node="${esc(n.id)}" style="cursor:pointer">
      <td><button class="fav-btn ${n.favorite ? 'on' : ''}" data-fav-node="${esc(n.id)}" data-fav="${n.favorite ? '1' : '0'}" title="${n.favorite ? '取消收藏' : '收藏'}">${n.favorite ? '★' : '☆'}</button></td>
      <td class="strong">${esc(n.name)}${days != null && days <= 30 ? ` <span class="badge ${days <= 7 ? 'crit' : 'warn'}">${days < 0 ? '已过期' : days + '天'}</span>` : ''}</td>
      <td><span class="dot ${st.tone}"></span> ${esc(st.text)}</td>
      <td>${esc(n.group || '—')}</td>
      <td class="num">${n.state === 'no_data' ? '—' : tbar(n.cpuPct, 80, 90)}</td>
      <td class="num">${n.state === 'no_data' ? '—' : tbar(n.memPct, 80, 92)}</td>
      <td class="num">${n.state === 'no_data' ? '—' : tbar(n.diskPct, 80, 90)}</td>
      <td class="num mono">${n.state === 'no_data' ? '—' : n.load1.toFixed(2)}</td>
      <td class="num mono" style="font-size:11.5px">${n.state === 'no_data' ? '—' : '↓' + rate(n.netRx) + ' ↑' + rate(n.netTx)}</td>
      <td class="num mono" style="font-size:11.5px">${bytes(used)}${quota > 0 ? `<div class="field-hint">${qp.toFixed(0)}% / ${quota} GB</div>` : ''}</td>
      <td class="num mono">${n.price ? (n.currency === 'CNY' ? '¥' : '$') + n.price : '—'}</td>
    </tr>`;
  }).join('') || `<tr><td colspan="11"><div class="empty">没有匹配的节点</div></td></tr>`;

  $('#view').innerHTML = `
    <div class="kpis" style="margin-bottom:20px">
      ${kpi('参与对比', sum.nodes || 0, '台', `在线 ${sum.online || 0} · 不可用 ${sum.unavailable || 0}`)}
      ${kpi('平均 CPU', (sum.avgCpu || 0).toFixed(1), '%', `峰值 ${(sum.maxCpu || 0).toFixed(1)}%`)}
      ${kpi('平均内存', (sum.avgMem || 0).toFixed(1), '%', `峰值 ${(sum.maxMem || 0).toFixed(1)}%`)}
      ${kpi('收藏节点', sum.favorites || 0, '台', '收藏优先展示')}
      ${kpi('月度成本', '$' + (sum.cost || 0).toFixed(1), '', '在线节点合计')}
    </div>
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">对比范围</h2>
        <span class="section-meta">${rows.length} 台节点</span>
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
        <h2 class="section-title">分组聚合</h2>
        <span class="section-meta">仅在线节点参与均值</span>
      </div>
      <div class="table-wrap"><table class="assets-table">
        <thead><tr><th>分组</th><th class="num">在线 / 总数</th><th class="num">平均 CPU</th><th class="num">平均内存</th><th class="num">月度成本</th><th class="num">实时下行</th></tr></thead>
        <tbody>${groupRows}</tbody>
      </table></div>
    </section>
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">节点对比</h2>
        <span class="section-meta">点击星标收藏，点击行查看详情</span>
      </div>
      <div class="table-wrap"><table class="node-table">
        <thead><tr><th></th><th>节点</th><th>状态</th><th>分组</th>
          <th class="num">CPU</th><th class="num">内存</th><th class="num">磁盘</th><th class="num">负载</th>
          <th class="num">实时流量</th><th class="num">周期用量</th><th class="num">年付价</th></tr></thead>
        <tbody>${nodeRows}</tbody>
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
    <p class="field-hint">名称：${esc((meta && meta.name) || '')} · 范围：${esc(((meta && meta.scopes) || []).join(', '))}</p>
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
      : up >= 60 ? `${Math.floor(up / 60)} 分` : `${Math.round(up)} 秒`;

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
        <div class="field-hint" style="margin-top:8px">
          每 60 秒一次；编码与压缩在锁外进行，只有深拷贝会短暂持有读锁。
          上次成功：${Number(s.lastSuccessAgoSeconds) || 0} 秒前。
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
  let queue = { stats: {}, jobs: [] };
  let tokens = [];
  let health = null;
  let loadErr = '';
  try {
    const [n, svc, q, th, tk, hl] = await Promise.all([
      adminApi('/api/admin/nodes'),
      adminApi('/api/admin/services'),
      adminApi('/api/admin/notify/jobs'),
      adminApi('/api/admin/theme'),
      adminApi('/api/admin/tokens').catch(() => []),
      adminApi('/api/admin/health').catch(() => null),
    ]);
    health = hl;
    nodes = n; services = svc; queue = q || queue; tokens = Array.isArray(tk) ? tk : [];
    themeCfg = th || {};
    const st = await adminApi('/api/admin/settings');
    cfg = st.notify || {};
    chans = st.channels || [];
    defs = st.defs || [];
    state.panelLink = st.panelLink || '';
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
    <td class="mono" style="font-size:11.5px">${clock(tk.createdAt)}</td>
    <td class="mono" style="font-size:11.5px">${tk.expiresAt ? clock(tk.expiresAt) : '永不过期'}</td>
    <td class="num"><button class="btn tiny danger" data-del-token="${esc(tk.id)}">删除</button></td>
  </tr>`).join('') || `<tr><td colspan="6"><div class="empty">尚未创建 API 令牌</div></td></tr>`;

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
      <p class="field-hint" style="margin:8px 0 0">
        账号资料与密码可在此直接修改，保存时需验证当前密码；修改后当前浏览器自动续签，其他设备需重新登录。
        忘记密码时可在服务端执行 <code>-reset-admin-password -admin-password '新密码'</code> 启动一次来重置。
      </p>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">安全与自动化</h2>
        <span class="section-meta">两步验证与范围化 API 令牌</span>
      </div>
      <div class="panel panel-pad" style="display:flex;gap:14px;align-items:center;flex-wrap:wrap;margin-bottom:12px">
        <span class="badge ${totpOn ? 'ok' : 'mute'}">${totpOn ? '两步验证已启用' : '两步验证未启用'}</span>
        <span class="section-meta">登录时在密码之外额外校验身份验证器动态码（TOTP，30 秒轮换）。</span>
        <button class="btn ${totpOn ? 'danger' : 'primary'}" id="totpBtn" type="button" style="margin-left:auto">${totpOn ? '停用两步验证' : '启用两步验证'}</button>
      </div>
      <div class="panel panel-pad">
        <div class="ch-add">
          <input id="tokenName" type="text" maxlength="64" placeholder="令牌名称，例如 CI 只读" style="flex:0 0 220px">
          <select id="tokenScope">
            <option value="read">只读（read）</option>
            <option value="admin">管理（admin）</option>
          </select>
          <input id="tokenDays" type="number" min="0" max="3650" placeholder="有效天数（0=永久）" style="flex:0 0 170px">
          <button class="btn primary" id="createTokenBtn" type="button">创建令牌</button>
        </div>
        <p class="field-hint" style="margin:8px 0 10px">
          使用 <code>Authorization: Bearer rct_…</code> 调用 API；令牌只保存 SHA-256 摘要，明文仅在创建时显示一次。
          管理范围可执行写操作，请仅授予可信自动化。
        </p>
        <div class="table-wrap"><table class="queue-table">
          <thead><tr><th>名称</th><th>前缀</th><th>范围</th><th>创建时间</th><th>过期时间</th><th class="num">操作</th></tr></thead>
          <tbody>${tokenRows}</tbody>
        </table></div>
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
            <div class="sub-title" style="margin-bottom:8px">导出备份</div>
            <div class="form-grid">
              ${field('备份密码', 'backupPwd', '', { type: 'password', hint: '至少 8 个字符；恢复时需要同一密码' })}
            </div>
            <div class="form-actions" style="justify-content:flex-start">
              <button class="btn primary" id="backupBtn" type="button">下载加密备份</button>
            </div>
          </div>
          <div>
            <div class="sub-title" style="margin-bottom:8px">导入备份</div>
            <div class="form-grid">
              ${field('备份密码', 'restorePwd', '', { type: 'password' })}
              <label class="field">
                <span class="field-label">备份文件（.rcbk）</span>
                <input type="file" id="restoreFile" accept=".rcbk,text/plain">
              </label>
              ${checkbox('我确认导入会替换当前全部配置', 'restoreConfirm', false)}
            </div>
            <div class="form-actions" style="justify-content:flex-start">
              <button class="btn danger" id="restoreBtn" type="button">导入并覆盖配置</button>
            </div>
          </div>
        </div>
        <p class="field-hint" style="margin:10px 0 0">
          备份包含节点、探测、规则、静默、续费与设置（含节点令牌）；导入不会修改监控历史与告警记录。
          导入后通知配置与节点令牌立即生效，请妥善保管备份文件与密码。
        </p>
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
        <p class="field-hint" style="margin:10px 0 0">
          强调色、圆角与紧凑密度对仪表盘和公开状态页生效；默认模式只是访客的初始值，右上角手动切换过则优先本机选择。
        </p>
      </div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">节点管理</h2>
        <span class="section-meta">${nodes.length} 台${serverVersion ? ' · 控制端 ' + esc(serverVersion) : ''}</span>
      </div>
      <div class="table-wrap" style="margin-bottom:12px"><table class="settings-nodes-table">
        <thead><tr><th>名称</th><th>服务商</th><th>位置</th><th>分组</th><th>到期</th><th>Agent</th><th class="num">操作</th></tr></thead>
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
          ${field('备注', 'note', '', { placeholder: '可选' })}
          ${field('排序权重', 'weight', '', { type: 'number', placeholder: '0' })}
          ${checkbox('收藏（对比页优先展示）', 'favorite', false)}
          <div class="form-actions"><button class="btn primary" type="submit">创建并获取令牌</button></div>
        </form>
      </details>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">服务探测</h2>
        <span class="section-meta">${services.length} 个目标</span>
      </div>
      <div class="table-wrap" style="margin-bottom:12px"><table class="settings-services-table">
        <thead><tr><th>名称</th><th>类型</th><th>目标</th><th class="num">间隔</th><th class="num">操作</th></tr></thead>
        <tbody>${services.map((s) => `<tr>
          <td class="strong">${esc(s.name)}</td>
          <td><span class="badge mute">${esc(s.type.toUpperCase())}</span></td>
          <td class="mono" style="font-size:11.5px">${esc(s.target)}</td>
          <td class="num mono">${s.interval}s</td>
          <td class="num"><button class="btn tiny danger" data-del-svc="${esc(s.id)}">删除</button></td>
        </tr>`).join('') || `<tr><td colspan="5"><div class="empty">暂无探测目标</div></td></tr>`}</tbody>
      </table></div>
      <details class="panel panel-pad">
        <summary class="sum">＋ 新增探测目标</summary>
        <form id="svcForm" class="form-grid">
          ${field('名称', 'name', '', { required: true, placeholder: '例如 imotao.com' })}
          ${selectField('类型', 'type', 'https', [{ v: 'https', t: 'HTTPS' }, { v: 'http', t: 'HTTP' }, { v: 'tcp', t: 'TCP' }, { v: 'dns', t: 'DNS' }])}
          ${field('目标', 'target', '', { required: true, placeholder: 'https://example.com / 1.2.3.4:3306 / example.com@1.1.1.1' })}
          ${field('间隔（秒）', 'interval', '60', { type: 'number', hint: 'DNS 目标可写 name 或 name@server' })}
          ${selectField('归属节点', 'nodeId', '', [{ v: '', t: '不关联' }].concat(nodes.map((n) => ({ v: n.id, t: n.name }))))}
          <div class="form-actions"><button class="btn primary" type="submit">添加</button></div>
        </form>
      </details>
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
      <div class="table-wrap"><table class="queue-table">
        <thead><tr><th>状态</th><th>通知</th><th>规则 / 节点</th><th class="num">尝试</th><th>创建时间</th><th>最近错误</th><th class="num">操作</th></tr></thead>
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
        <h2 class="section-title">数据导出</h2>
        <span class="section-meta">CSV 带 BOM，Excel 可直接打开；指标导出含 24 小时全量采样</span>
      </div>
      <div class="panel panel-pad">
        <div class="form-actions" style="padding-top:0">
          <button class="btn" data-exp="assets">资产台账 CSV</button>
          <button class="btn" data-exp="alerts">告警历史 CSV</button>
          <select id="expNode" style="padding:7px 10px;border:1px solid var(--line);border-radius:7px;background:var(--panel);color:var(--fg);font-size:12.5px">
            ${nodes.map((n) => `<option value="${esc(n.id)}">${esc(n.name)}</option>`).join('')}
          </select>
          <select id="expRange" style="padding:7px 10px;border:1px solid var(--line);border-radius:7px;background:var(--panel);color:var(--fg);font-size:12.5px">
            <option value="1h">最近 1 小时</option>
            <option value="6h">最近 6 小时</option>
            <option value="24h" selected>最近 24 小时</option>
            <option value="7d">最近 7 天</option>
            <option value="30d">最近 30 天</option>
            <option value="365d">最近 365 天</option>
          </select>
          <button class="btn" data-exp="metrics">指标 CSV</button>
        </div>
        <p class="field-hint" style="margin:10px 0 0">
          Prometheus 抓取端点：<code style="font-size:11.5px;background:var(--panel-2);padding:1px 5px;border-radius:4px">GET /metrics</code>
          （Basic Auth 密码填管理令牌，用户名任意）
        </p>
      </div>
    </section>`;

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
          expiresDays: Number($('#tokenDays').value) || 0,
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
      if (!confirm('删除该 API 令牌？使用它的自动化会立即失去访问权限。')) return;
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
    if (!confirm('导入会替换当前全部节点、探测、规则与设置，且无法撤销。继续？')) return;
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
      if (!confirm('确认删除该节点？其监控数据也会一并移除。')) return;
      try {
        await adminApi('/api/admin/nodes/' + b.dataset.delNode, { method: 'DELETE' });
        toast('节点已删除'); viewSettings();
      } catch (e) { toast('删除失败：' + e.message, 'err'); }
    };
  });

  $$('[data-del-svc]').forEach((b) => {
    b.onclick = async () => {
      if (!confirm('确认删除该探测目标？')) return;
      try {
        await adminApi('/api/admin/services/' + b.dataset.delSvc, { method: 'DELETE' });
        toast('探测目标已删除'); viewSettings();
      } catch (e) { toast('删除失败：' + e.message, 'err'); }
    };
  });

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
        note: d.note, weight: Number(d.weight) || 0, favorite: !!d.favorite,
      }) });
      toast('节点已创建');
      showAgentConfig(r);
      viewSettings();
    } catch (err) { toast('创建失败：' + err.message, 'err'); }
  };

  $('#svcForm').onsubmit = async (e) => {
    e.preventDefault();
    const d = formData(e.target);
    try {
      await adminApi('/api/admin/services', { method: 'POST', body: JSON.stringify({
        name: d.name, type: d.type, target: d.target,
        interval: Number(d.interval) || 60, nodeId: d.nodeId || '', enabled: true,
      }) });
      toast('探测目标已添加'); viewSettings();
    } catch (err) { toast('添加失败：' + err.message, 'err'); }
  };

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

// showAgentConfig 展示（或轮换后重新展示）节点接入指引
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
    <p class="modal-sub" style="margin-top:10px">脚本会自动识别架构（amd64/arm64）；明文 HTTP 场景需显式加 --allow-insecure（上方命令已按当前地址自动补上）才会继续安装。约 30 秒后面板即可看到该节点。</p>
    ${insecure ? '<p class="modal-sub" style="color:var(--warn)">注意：控制端当前是明文 HTTP，节点令牌会在链路上明文传输。长期使用建议上 TLS 或保持隧道。</p>' : ''}
    <details style="margin-top:12px">
      <summary class="modal-sub" style="cursor:pointer">手动安装（备选）</summary>
      <pre class="code" style="margin-top:8px">${esc(manual)}</pre>
      <pre class="code">${esc(cfg)}</pre>
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
    if (!confirm('确认轮换该节点的令牌？旧令牌会立即失效，现有 Agent 将停止上报，直到更新配置。')) return;
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
function openDialog(title, bodyHTML) {
  const mask = document.createElement('div');
  mask.className = 'modal-mask';
  mask.innerHTML = `<div class="modal"><div class="modal-title">${esc(title)}</div>${bodyHTML}</div>`;
  document.body.appendChild(mask);
  const close = () => mask.remove();
  mask.onclick = (e) => { if (e.target === mask) close(); };
  $$('[data-close]', mask).forEach((b) => { b.onclick = close; });
  return {
    close,
    querySelector: (s) => mask.querySelector(s),
  };
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

  const visBoxes = STATUS_VIS_ITEMS.map(([k, label]) =>
    checkbox(label, 'vis_' + k, !!vis[k])).join('');
  const blockBoxes = STATUS_BLOCK_ITEMS.map(([k, label]) =>
    checkbox(label, 'blk_' + k, !!blocks[k])).join('');

  const mtRows = (mtRes.items || []).map((m) => {
    const scope = (m.nodeIds && m.nodeIds.length)
      ? m.nodeIds.map((id) => { const n = nodes.find((x) => x.id === id); return n ? n.name : id; }).join('、')
      : '全部节点';
    const active = Date.now() / 1000 >= m.startAt && Date.now() / 1000 < m.endAt;
    return `<tr>
      <td class="strong">${esc(m.title)}</td>
      <td>${esc(scope)}</td>
      <td class="mono" style="font-size:11.5px">${esc(fullTs(m.startAt))}<br>${esc(fullTs(m.endAt))}</td>
      <td>${active ? '<span class="dot info"></span> 进行中' : m.startAt > Date.now() / 1000 ? '未开始' : '已结束'}</td>
      <td class="num"><button class="btn danger" data-del-mt="${esc(m.id)}" style="padding:3px 10px;font-size:11.5px">删除</button></td>
    </tr>`;
  }).join('');

  const incRows = (incRes.items || []).map((it) => {
    const scope = (it.nodeIds && it.nodeIds.length)
      ? it.nodeIds.map((id) => { const n = nodes.find((x) => x.id === id); return n ? n.name : id; }).join('、')
      : '全局';
    return `<tr>
      <td class="strong">${esc(it.title)}<div style="font-size:11px;color:var(--fg-3);font-weight:400">${esc((it.body || '').slice(0, 60))}</div></td>
      <td><span class="dot ${it.level === 'critical' ? 'crit' : it.level === 'warn' ? 'warn' : 'ok'}"></span> ${esc(it.level)}</td>
      <td>${esc(scope)}</td>
      <td class="mono" style="font-size:11.5px">${esc(fullTs(it.startAt))}${it.endAt ? '<br>→ ' + esc(fullTs(it.endAt)) : '<br>进行中'}</td>
      <td class="num"><button class="btn danger" data-del-inc="${esc(it.id)}" style="padding:3px 10px;font-size:11.5px">删除</button></td>
    </tr>`;
  }).join('');

  $('#view').innerHTML = `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">展示配置</h2>
        <span class="section-meta">公开页 /status</span>
        <a class="btn" href="/status" target="_blank" rel="noopener" style="padding:6px 14px;font-size:12.5px">打开公开页 ↗</a>
      </div>
      <form id="spForm" class="panel panel-pad">
        <div class="grid-2">
          ${field('站点标题', 'title', cfg.title || '', { required: true, hint: '浏览器标签页与页面主标题' })}
          ${selectField('可用率日柱天数', 'historyDays', String(cfg.historyDays || 90),
            [{ v: '30', t: '30 天' }, { v: '60', t: '60 天' }, { v: '90', t: '90 天' }])}
        </div>
        <div class="field" style="margin-top:10px">
          <span class="field-label">置顶公告</span>
          <textarea name="annText" rows="2" placeholder="例如：今晚 23:00 主控迁移，期间可能短暂不可用。">${esc(ann.text || '')}</textarea>
        </div>
        <div class="grid-2" style="margin-top:10px">
          ${checkbox('启用公告', 'annEnabled', !!ann.enabled)}
          ${selectField('公告级别', 'annLevel', ann.level || 'info',
            [{ v: 'info', t: '信息' }, { v: 'warn', t: '警告' }, { v: 'critical', t: '严重' }])}
        </div>
        <div style="margin-top:14px">
          <div class="field-label">公开字段（未勾选的字段不会出现在公开页，关闭是安全的默认值）</div>
          <div class="check-grid">${visBoxes}</div>
        </div>
        <div style="margin-top:14px">
          <div class="field-label">显示区块</div>
          <div class="check-grid">${blockBoxes}</div>
        </div>
        <div style="margin-top:14px;display:flex;gap:10px;align-items:center">
          <button class="btn primary" type="submit">保存配置</button>
          <span id="spSaved" style="font-size:12px;color:var(--ok)"></span>
        </div>
      </form>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">维护窗口</h2>
        <span class="section-meta">窗口内不产生新告警，公开页显示"维护中"</span>
        <button class="btn primary" id="mtAdd" style="padding:6px 14px;font-size:12.5px">＋ 新建维护窗口</button>
      </div>
      <div class="table-wrap"><table class="maint-table">
        <thead><tr><th>标题</th><th>范围</th><th>起止</th><th>状态</th><th class="num">操作</th></tr></thead>
        <tbody>${mtRows || '<tr><td colspan="5"><div class="empty">没有维护窗口</div></td></tr>'}</tbody>
      </table></div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">人工事件</h2>
        <span class="section-meta">与告警一起出现在公开页时间线</span>
        <button class="btn primary" id="incAdd" style="padding:6px 14px;font-size:12.5px">＋ 登记事件</button>
      </div>
      <div class="table-wrap"><table class="maint-table">
        <thead><tr><th>标题</th><th>级别</th><th>影响范围</th><th>时间</th><th class="num">操作</th></tr></thead>
        <tbody>${incRows || '<tr><td colspan="5"><div class="empty">没有人工事件</div></td></tr>'}</tbody>
      </table></div>
    </section>`;

  // ---- 保存展示配置 ----
  $('#spForm').onsubmit = async (ev) => {
    ev.preventDefault();
    const f = ev.target;
    const vis = {};
    STATUS_VIS_ITEMS.forEach(([k]) => { vis[k] = f.querySelector(`[name="vis_${k}"]`).checked; });
    const blocks = {};
    STATUS_BLOCK_ITEMS.forEach(([k]) => { blocks[k] = f.querySelector(`[name="blk_${k}"]`).checked; });
    const body = {
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
  $$('[data-del-mt]').forEach((b) => {
    b.onclick = async () => {
      if (!confirm('删除该维护窗口？窗口内正在进行的维护不会再显示。')) return;
      try {
        await adminApi('/api/admin/maintenance/' + encodeURIComponent(b.dataset.delMt), { method: 'DELETE' });
        toast('已删除');
        viewStatusPage();
      } catch (err) { toast('删除失败：' + err.message, 'err'); }
    };
  });

  // ---- 人工事件 ----
  $('#incAdd').onclick = () => openIncidentDialog(nodes);
  $$('[data-del-inc]').forEach((b) => {
    b.onclick = async () => {
      if (!confirm('删除该事件？它会同时从公开页时间线消失。')) return;
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

// nodePicker 生成"影响范围"多选框列表（全部节点 + 分组归类展示）。
function nodePicker(nodes, name) {
  if (!nodes.length) return '<div class="empty">暂无节点</div>';
  return `<div class="check-grid">${nodes.map((n) =>
    `<label class="check"><input type="checkbox" name="${name}" value="${esc(n.id)}"><span>${esc(n.name)}</span></label>`
  ).join('')}</div>`;
}
function pickedIDs(form, name) {
  return $$(`input[name="${name}"]:checked`, form).map((c) => c.value);
}

function openMaintenanceDialog(nodes) {
  const dlg = openDialog('新建维护窗口', `
    <form id="mtForm">
      ${field('标题', 'title', '', { required: true, placeholder: '例如：主控迁移' })}
      ${field('说明（可选）', 'reason', '', { placeholder: '访客会在公开页看到这段说明' })}
      <div class="field"><span class="field-label">影响范围（不选 = 全部节点）</span>${nodePicker(nodes, 'mtNode')}</div>
      <div class="grid-2">
        ${field('开始（留空 = 立即）', 'startAt', '', { type: 'datetime-local' })}
        ${field('持续（分钟，默认 60）', 'minutes', '', { type: 'number', placeholder: '60' })}
      </div>
      <div style="display:flex;gap:10px;justify-content:flex-end;margin-top:14px">
        <button class="btn" type="button" data-close>取消</button>
        <button class="btn primary" type="submit">创建</button>
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
          title: f.title.value.trim(),
          reason: f.reason.value.trim(),
          nodeIds: pickedIDs(f, 'mtNode'),
          startAt: start,
          minutes,
        }),
      });
      close();
      toast('维护窗口已创建');
      viewStatusPage();
    } catch (err) { toast('创建失败：' + err.message, 'err'); }
  };
}

function openIncidentDialog(nodes) {
  const dlg = openDialog('登记事件', `
    <form id="incForm">
      ${field('标题', 'title', '', { required: true, placeholder: '例如：上游出口抖动' })}
      <div class="field"><span class="field-label">详情（可选）</span>
        <textarea name="body" rows="3" placeholder="发生了什么、影响什么、怎么处理"></textarea></div>
      <div class="grid-2">
        ${selectField('级别', 'level', 'info',
          [{ v: 'info', t: '信息' }, { v: 'warn', t: '警告' }, { v: 'critical', t: '严重' }])}
        ${field('结束时间（留空 = 进行中）', 'endAt', '', { type: 'datetime-local' })}
      </div>
      <div class="field"><span class="field-label">受影响节点（不选 = 全局）</span>${nodePicker(nodes, 'incNode')}</div>
      <div style="display:flex;gap:10px;justify-content:flex-end;margin-top:14px">
        <button class="btn" type="button" data-close>取消</button>
        <button class="btn primary" type="submit">登记</button>
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
          title: f.title.value.trim(),
          body: f.body.value.trim(),
          level: f.level.value,
          nodeIds: pickedIDs(f, 'incNode'),
          endAt: end,
        }),
      });
      close();
      toast('事件已登记');
      viewStatusPage();
    } catch (err) { toast('登记失败：' + err.message, 'err'); }
  };
}

// showNodeEdit 编辑节点台账，避免为了改名/改价删除节点、丢失历史。
function showNodeEdit(n) {
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
      ${field('备注', 'note', n.note, { placeholder: '可选' })}
      ${field('排序权重', 'weight', n.weight, { type: 'number', placeholder: '0' })}
      ${checkbox('收藏（对比页优先展示）', 'favorite', !!n.favorite)}
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
          note: d.note, weight: Number(d.weight) || 0, favorite: !!d.favorite,
        }),
      });
      close();
      toast('节点已更新');
      viewSettings();
    } catch (err) {
      toast('保存失败：' + err.message, 'err');
    }
  };
}

/* ---------------- 路由 ---------------- */

const ROUTES = {
  overview: { title: '概览', sub: '机群健康状态与关键指标', fn: viewOverview },
  nodes: { title: '节点', sub: '全部受监控服务器', fn: viewNodes },
  compare: { title: '节点对比', sub: '跨节点指标对比与分组聚合', fn: viewCompare },
  services: { title: '服务', sub: '网站与端口可用性探测', fn: viewServices },
  alerts: { title: '告警', sub: '规则触发与历史记录', fn: viewAlerts },
  assets: { title: '台账', sub: '资产、到期与成本', fn: viewAssets },
  statuspage: { title: '状态页', sub: '公开页 /status 的展示、维护窗口与事件', fn: viewStatusPage },
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
                   placeholder="admin 或 name@example.com" required spellcheck="false"
                   autocomplete="username" autocapitalize="none" aria-describedby="loginError">
          </span>
        </label>
        <label class="login-field">
          <span class="login-label">密码</span>
          <span class="login-input-wrap">
            <input class="login-input" id="gatePassword" name="password" type="password"
                   placeholder="请输入密码" required autocomplete="current-password"
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
                   placeholder="身份验证器中的 6 位数字" inputmode="numeric"
                   pattern="[0-9]{6}" maxlength="6" autocomplete="one-time-code"
                   aria-describedby="loginError">
          </span>
        </label>
        <button class="login-btn" id="gateSubmit" type="submit">登 录</button>
      </form>
      <div class="login-hint">
        账号、邮箱与密码由服务端统一管理；忘记密码时可用
        <code>-reset-admin-password</code> 启动一次来重置。
      </div>
      <div class="login-foot">
        <a href="/status" target="_blank" rel="noopener">公开状态页 ↗</a>
        <button type="button" id="loginTheme">深浅色</button>
      </div>
    </div>`;

  const userInp = layer.querySelector('#gateIdentifier');
  const passInp = layer.querySelector('#gatePassword');
  const err = layer.querySelector('#loginError');
  const card = layer.querySelector('#loginCard');
  const btn = layer.querySelector('#gateSubmit');
  userInp.focus();

  layer.querySelector('#gateEye').onclick = () => {
    passInp.type = passInp.type === 'password' ? 'text' : 'password';
    passInp.focus();
  };
  layer.querySelector('#loginTheme').onclick = () => $('#themeBtn').click();

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

  $('#liveText').textContent = '实时';
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
  if (state.rendering) return;
  if (state.authRequired) {
    showLoginGate();
    return;
  }
  // 任何一次成功渲染都意味着鉴权已通过：确保登录层退场。
  state.rendering = true;
  closeLoginLayer();
  const hash = location.hash.replace(/^#\/?/, '') || 'overview';
  const [route, param] = hash.split('/');
  state.route = route;
  state.param = param || null;

  const meta = ROUTES[route];
  if (meta) {
    $('#pageTitle').textContent = meta.title;
    $('#pageSub').textContent = meta.sub;
    setActiveNav(route);
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
    $('#liveText').textContent = '实时';
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
    $('#liveText').textContent = '离线';
  } finally {
    state.rendering = false;
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
  // Agent 默认 30 秒上报一次，30 秒轮询会让"刚上报的数据"最多等一分钟才出现，
  // 看起来就像没在刷新。
  const LIVE_ROUTES = ['overview', 'nodes'];
  const busy = () => {
    // 有弹窗、或正在输入时不打断用户
    if (document.querySelector('dialog[open], .modal, .modal-backdrop')) return true;
    const el = document.activeElement;
    return !!el && /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName);
  };
  const tickLive = async () => {
    if (document.hidden || state.authRequired || !state.authenticated) return;
    if (LIVE_ROUTES.indexOf(state.route) < 0) return;
    if (state.route === 'nodes' && state.param) return; // 详情页单独节奏
    if (busy() || state.rendering) return;
    try {
      const [ov, nodes] = await Promise.all([api('/api/overview'), api('/api/nodes')]);
      const sig = JSON.stringify([ov, nodes]);
      if (sig === state.liveSig) return; // 数据没变，不重绘
      render();
    } catch (e) {
      // 轮询失败保持旧数据，等下一轮；真正的会话失效由 api() 内的 401 分支处理
    }
  };
  setInterval(tickLive, 5000);

  // 切回标签页/窗口获得焦点时立即刷新一次，不用等下一个 5 秒
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) tickLive();
  });
  window.addEventListener('focus', tickLive);

  // 节点详情页：图表较重，15 秒一次整体重绘（原来 20 秒）。
  // 注意它不能用上面的 tickLive —— 那条路径会跳过详情页。
  setInterval(() => {
    if (document.hidden || state.authRequired || !state.authenticated) return;
    if (!(state.route === 'nodes' && state.param)) return;
    if (busy() || state.rendering) return;
    render();
  }, 15000);

  // 其它页面（设置、规则、令牌等含表单）保持 30 秒整体重绘，避免打断编辑
  setInterval(() => {
    if (document.hidden || state.authRequired) return;
    if (LIVE_ROUTES.indexOf(state.route) >= 0) return;
    if (busy()) return;
    render();
  }, 30000);
}

boot();

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
