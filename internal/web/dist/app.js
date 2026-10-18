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

function clock(ts) {
  const d = new Date(ts * 1000);
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function tone(v, warnAt, critAt) {
  if (v >= critAt) return 'crit';
  if (v >= warnAt) return 'warn';
  return 'ok';
}

/* ---------------- 请求 ---------------- */

async function api(path) {
  const headers = { Accept: 'application/json' };
  const tok = adminToken();
  if (tok) headers['X-Admin-Token'] = tok;
  const r = await fetch(path, { headers });
  if (r.status === 401) {
    state.authRequired = true;
    const e = new Error('需要管理令牌');
    e.status = 401;
    throw e;
  }
  if (!r.ok) throw new Error(r.status + ' ' + r.statusText);
  state.authRequired = false;
  return r.json();
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

function nodeCard(n) {
  const cpu = n.latest.cpuPct || 0;
  const mem = pct(n.latest.memUsed, n.latest.memTotal);
  const disk = pct(n.latest.diskUsed, n.latest.diskTotal);
  const spark = (n.spark || []).map((p) => p.v);
  const statusTone = !n.online ? 'off' : n.alerts > 0 ? 'crit' : 'ok';
  const statusText = !n.online ? '离线' : n.alerts > 0 ? `${n.alerts} 告警` : '在线';

  return `<article class="node-card ${n.online ? '' : 'offline'} ${n.alerts > 0 ? 'alerting' : ''}" data-node="${esc(n.id)}">
    <div class="nc-head">
      <span class="dot ${statusTone}"></span>
      <span class="nc-name">${esc(n.name)}</span>
      <span class="nc-tag">${esc(n.provider || '—')}</span>
      <span class="nc-status">${statusText}</span>
    </div>
    <div class="nc-bars">
      ${barRow('CPU', cpu, 75, 90)}
      ${barRow('内存', mem, 80, 92)}
      ${barRow('磁盘', disk, 80, 90)}
    </div>
    <div class="nc-spark">${sparkline(spark, statusTone === 'off' ? 'var(--fg-3)' : 'var(--accent)')}</div>
    <div class="nc-foot">
      <span class="mono">${esc(n.region || '—')}</span>
      <span class="sep">·</span>
      <span class="mono">↑${bytes(n.latest.netTx)}</span>
      <span class="sep">·</span>
      <span class="mono">${dur(n.latest.uptime)}</span>
    </div>
  </article>`;
}

/* ---------------- 视图 ---------------- */

async function viewOverview() {
  const [ov, nodes] = await Promise.all([api('/api/overview'), api('/api/nodes')]);
  state.data.nodes = nodes;

  const expiring = ov.expiring || [];
  const lists = nodes.filter((n) => !state.search || n.name.toLowerCase().includes(state.search));
  const alerting = nodes.filter((n) => n.alerts > 0);

  $('#view').innerHTML = `
    <div class="kpis">
      ${kpi('节点总数', ov.nodes, '', `在线 ${ov.online} · 离线 ${ov.offline}`)}
      ${kpi('活跃告警', ov.activeAlert, '', ov.activeAlert > 0 ? '需关注' : '全部正常')}
      ${kpi('平均 CPU', ov.avgCpu.toFixed(1), '%', '在线节点均值')}
      ${kpi('平均内存', ov.avgMem.toFixed(1), '%', '在线节点均值')}
      ${kpi('服务探测', `${ov.services - ov.svcDown}/${ov.services}`, '', ov.svcDown > 0 ? `${ov.svcDown} 个不可用` : '全部可用')}
      ${kpi('月度成本', '$' + ov.monthlyCost.toFixed(1), '', `${expiring.length} 台 30 天内到期`)}
    </div>

    ${alerting.length ? `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">正在告警</h2>
        <span class="section-meta">${alerting.length} 台节点异常</span>
      </div>
      <div class="panel">
        ${alerting.map((n) => {
          const sev = !n.online ? 'critical' : 'warn';
          const detail = !n.online
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
        <span class="section-meta">${lists.length} 台 · 点击查看详情</span>
      </div>
      <div class="node-grid">
        ${lists.length ? lists.map(nodeCard).join('') : `<div class="empty">没有匹配的节点</div>`}
      </div>
    </section>

    ${expiring.length ? `
    <section class="section">
      <div class="section-head"><h2 class="section-title">即将到期</h2></div>
      <div class="table-wrap"><table>
        <thead><tr><th>节点</th><th>服务商</th><th>到期日</th><th class="num">剩余</th><th class="num">续费</th></tr></thead>
        <tbody>${expiring.map((n) => {
          const d = Math.ceil((new Date(n.expireAt) - Date.now()) / 86400000);
          return `<tr>
            <td class="strong">${esc(n.name)}</td>
            <td>${esc(n.provider)}</td>
            <td class="mono">${esc(n.expireAt)}</td>
            <td class="num"><span class="badge ${d <= 7 ? 'crit' : 'warn'}">${d} 天</span></td>
            <td class="num mono">${n.currency === 'CNY' ? '¥' : '$'}${n.price}</td>
          </tr>`;
        }).join('')}</tbody>
      </table></div>
    </section>` : ''}
  `;

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
      </div>
      <div class="table-wrap"><table>
        <thead><tr>
          <th>状态</th><th>节点</th><th>服务商</th><th>位置</th><th>系统</th>
          <th class="num">CPU</th><th class="num">内存</th><th class="num">磁盘</th>
          <th class="num">负载</th><th class="num">运行时长</th><th class="num">流量</th>
        </tr></thead>
        <tbody>${list.map((n) => {
          const cpu = n.latest.cpuPct || 0;
          const mem = pct(n.latest.memUsed, n.latest.memTotal);
          const disk = pct(n.latest.diskUsed, n.latest.diskTotal);
          return `<tr data-node="${esc(n.id)}" style="cursor:pointer">
            <td><span class="dot ${!n.online ? 'off' : n.alerts ? 'crit' : 'ok'}"></span></td>
            <td class="strong">${esc(n.name)}</td>
            <td>${esc(n.provider || '—')}</td>
            <td>${esc(n.region || '—')}</td>
            <td style="font-size:11.5px">${esc(n.host?.os || '—')}</td>
            <td class="num mono" style="color:${cpu > 85 ? 'var(--crit)' : 'inherit'}">${cpu.toFixed(0)}%</td>
            <td class="num mono">${mem.toFixed(0)}%</td>
            <td class="num mono" style="color:${disk > 88 ? 'var(--crit)' : 'inherit'}">${disk.toFixed(0)}%</td>
            <td class="num mono">${(n.latest.load1 || 0).toFixed(2)}</td>
            <td class="num mono">${dur(n.latest.uptime)}</td>
            <td class="num mono">${bytes(n.latest.netRx)}</td>
          </tr>`;
        }).join('') || `<tr><td colspan="11"><div class="empty">没有匹配的节点</div></td></tr>`}</tbody>
      </table></div>
    </section>`;
  bindNodeClicks();
}

async function viewNodeDetail(id) {
  const [detail, series] = await Promise.all([
    api('/api/nodes/' + id),
    api(`/api/nodes/${id}/series?range=${state.range}`),
  ]);
  const n = detail.node, host = detail.host || {};
  const pts = series.points || [];
  const resolution = series.resolution || 'raw';
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
          <span class="dot ${detail.online ? 'ok' : 'off'}"></span>
          <h2 style="font-size:17px;font-weight:650;margin:0;letter-spacing:-.02em">${esc(n.name)}</h2>
          <span class="nc-tag">${esc(n.provider || '—')}</span>
          <span class="nc-tag">${esc(n.group || '—')}</span>
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
            <span class="chart-val mono" style="color:${(last.cpuPct || 0) > 85 ? 'var(--crit)' : 'var(--fg)'}">${(last.cpuPct || 0).toFixed(1)}%</span></div>
          ${areaChart(cpuVals, 'var(--accent)')}
        </div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">内存使用率</span>
            <span class="chart-val mono">${pct(memUsed, memTotal).toFixed(1)}%<span style="font-size:11px;color:var(--fg-3);font-weight:400"> · ${bytes(memUsed)} / ${bytes(memTotal)}</span></span></div>
          ${areaChart(memVals, 'var(--accent-2)')}
        </div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">磁盘使用率</span>
            <span class="chart-val mono">${pct(diskUsed, diskTotal).toFixed(1)}%<span style="font-size:11px;color:var(--fg-3);font-weight:400"> · ${bytes(diskUsed)} / ${bytes(diskTotal)}</span></span></div>
          ${areaChart(diskVals, 'var(--warn)')}
        </div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">网络吞吐速率</span>
            <span class="chart-val mono" style="font-size:13px">↓${bytes(last.netRx)} ↑${bytes(last.netTx)} <span style="color:var(--fg-3);font-weight:400">累计</span></span></div>
          ${areaChart(netVals, 'var(--ok)', { height: 110, fmt: (v) => rate(v) })}
        </div>
        <div class="chart-card">
          <div class="chart-head"><span class="chart-title">1 分钟负载</span>
            <span class="chart-val mono">${(last.load1 || 0).toFixed(2)}</span></div>
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
            <div class="info-row"><span class="info-k">负载 1/5/15</span><span class="info-v mono">${(last.load1 || 0).toFixed(2)} / ${(last.load5 || 0).toFixed(2)} / ${(last.load15 || 0).toFixed(2)}</span></div>
            <div class="info-row"><span class="info-k">运行时长</span><span class="info-v mono">${dur(last.uptime)}</span></div>
            <div class="info-row"><span class="info-k">TCP 连接</span><span class="info-v mono">${last.tcpConn || 0}</span></div>
            <div class="info-row"><span class="info-k">进程数</span><span class="info-v mono">${last.procs || 0}</span></div>
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
      <div class="table-wrap"><table>
        <thead><tr><th>状态</th><th>名称</th><th>类型</th><th>目标</th>
          <th class="num">延迟</th><th class="num">证书</th><th class="num">24h 可用率</th><th>最近 2 小时</th></tr></thead>
        <tbody>${svcs.map((s) => {
          const spark = (s.spark || []).slice(-120).map((p) => p[1]);
          const up = s.uptime || 0;
          const cd = s.certDays;
          const certCell = cd === 0
            ? '<span class="badge crit">已过期</span>'
            : cd > 0
              ? `<span class="badge ${cd <= 14 ? 'crit' : cd <= 30 ? 'warn' : 'mute'}">${cd} 天</span>`
              : '—';
          return `<tr>
            <td><span class="dot ${s.ok ? 'ok' : 'crit'}"></span></td>
            <td class="strong">${esc(s.name)}</td>
            <td><span class="badge mute">${esc(s.type.toUpperCase())}</span></td>
            <td class="mono" style="font-size:11.5px;max-width:240px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(s.target)}</td>
            <td class="num mono">${s.latencyMs ? s.latencyMs + 'ms' : '—'}</td>
            <td class="num">${certCell}</td>
            <td class="num"><span class="badge ${up >= 99 ? 'ok' : up >= 95 ? 'warn' : 'crit'}">${up.toFixed(1)}%</span></td>
            <td style="width:150px;padding:6px 14px">
              <div style="height:22px">${sparkline(spark, s.ok ? 'var(--ok)' : 'var(--crit)', 22)}</div>
            </td>
          </tr>`;
        }).join('') || `<tr><td colspan="8"><div class="empty">尚未配置服务探测目标</div></td></tr>`}</tbody>
      </table></div>
    </section>`;
}

async function viewAlerts() {
  const canManage = !!adminToken();
  const [d, rules, meta, nodes] = await Promise.all([
    api('/api/alerts'),
    api('/api/rules').catch(() => []),
    adminApi('/api/admin/rule-meta').catch(() => ({ metrics: [], severities: [], durations: [] })),
    canManage ? adminApi('/api/admin/nodes').catch(() => []) : Promise.resolve([]),
  ]);
  const items = d.items || [];
  $('#navAlertCount').hidden = d.active === 0;
  $('#navAlertCount').textContent = d.active;

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
      <div class="table-wrap"><table>
        <thead><tr><th>规则</th><th>条件</th><th class="num">持续</th><th>级别</th><th class="num">启用</th><th class="num">操作</th></tr></thead>
        <tbody>${(rules || []).map((r) => {
          const m = metricMap[r.metric];
          const unit = m ? m.unit : '';
          const cond = r.metric === 'offline'
            ? `超过 ${r.duration > 0 ? durText(r.duration) : '即刻'} 未上报`
            : `${esc(m ? m.label : r.metric)} ${r.op === 'lt' ? '低于' : '高于'} ${r.threshold}${unit}`;
          const scope = ruleScopeText(r, nodes);
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
function ruleScopeText(r, nodes) {
  const ids = r.nodeIds || [], groups = r.groups || [];
  if (!ids.length && !groups.length) return '';
  const nameOf = {};
  (nodes || []).forEach((n) => { nameOf[n.id] = n.name; });
  const parts = [];
  if (ids.length) parts.push('节点：' + ids.map((id) => nameOf[id] || id).join('、'));
  if (groups.length) parts.push('分组：' + groups.join('、'));
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
  const r = rule || { name: '', metric: 'cpu', op: 'gt', threshold: 90, duration: 300, severity: 'warn', enabled: true, nodeIds: [], groups: [] };
  const scopedIDs = r.nodeIds || [];
  const knownIDs = new Set((nodes || []).map((n) => n.id));
  const nodeOptions = (nodes || []).map((n) =>
    `<option value="${esc(n.id)}"${scopedIDs.includes(n.id) ? ' selected' : ''}>${esc(n.name)}</option>`)
    .concat(scopedIDs.filter((id) => !knownIDs.has(id)).map((id) =>
      `<option value="${esc(id)}" selected>${esc(id)}（节点已不存在）</option>`)).join('');

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
      <label class="field">
        <span class="field-label">定向节点（可多选，留空=全部）</span>
        <select name="nodeIds" multiple size="4" style="min-height:86px;padding:6px;border:1px solid var(--line);border-radius:7px;background:var(--panel);color:var(--fg);font-size:12.5px">${nodeOptions}</select>
        <span class="field-hint">按住 Ctrl / Cmd 多选</span>
      </label>
      <label class="field">
        <span class="field-label">定向分组（逗号分隔，留空=全部）</span>
        <input name="groups" value="${esc((r.groups || []).join(', '))}" placeholder="prod, test">
        <span class="field-hint">节点与分组建「或」关系；两者都留空表示全部节点</span>
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
  const thWrap = $('#thWrap', dlg);
  const opWrap = $('#opWrap', dlg);
  const thUnit = $('#thUnit', dlg);
  const thHint = $('#thHint', dlg);
  const thInput = dlg.querySelector('[name=threshold]');

  // offline 规则没有阈值与比较符，需隐藏
  function syncMetricFields() {
    const m = (meta.metrics || []).find((x) => x.value === metricSel.value);
    const noVal = !!(m && m.noVal);
    thWrap.style.display = noVal ? 'none' : '';
    opWrap.style.display = noVal ? 'none' : '';
    if (m) {
      thUnit.textContent = m.unit ? '(' + m.unit + ')' : '';
      thHint.textContent = m.hint || '';
      if (m.min !== undefined) thInput.min = m.min;
      if (m.max !== undefined) thInput.max = m.max;
      if (m.step !== undefined) thInput.step = m.step;
    }
    thInput.disabled = noVal;
  }
  metricSel.onchange = syncMetricFields;
  syncMetricFields();

  $('#ruleForm', dlg).onsubmit = async (e) => {
    e.preventDefault();
    const d = formData(e.target);
    const isOffline = d.metric === 'offline';
    const nodeSel = e.target.querySelector('[name=nodeIds]');
    const payload = {
      name: d.name,
      metric: d.metric,
      op: isOffline ? 'gt' : d.op,
      duration: Number(d.duration) || 0,
      severity: d.severity,
      enabled: d.enabled,
      nodeIds: nodeSel ? Array.from(nodeSel.selectedOptions).map((o) => o.value) : [],
      groups: String(d.groups || '').split(',').map((s) => s.trim()).filter(Boolean),
    };
    if (!isOffline) payload.threshold = Number(d.threshold) || 0;

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
  const d = await api('/api/assets');
  const rows = d.rows || [];
  const total = d.monthlyTotal || 0;

  $('#view').innerHTML = `
    <div class="kpis" style="margin-bottom:20px">
      ${kpi('资产总数', rows.length, '台', '含监控与未监控')}
      ${kpi('月度成本', '$' + total.toFixed(1), '', '按年付折算')}
      ${kpi('30 天内到期', rows.filter((r) => r.days > 0 && r.days <= 30).length, '台', '需续费')}
      ${kpi('已过期/无日期', rows.filter((r) => !r.days || r.days <= 0).length, '台', '需补录')}
    </div>
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">资产台账</h2>
        <span class="section-meta">按到期时间排序</span>
      </div>
      <div class="table-wrap"><table>
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
}


/* ---------------- 设置 / 管理 ---------------- */

// localStorage key 随项目改名从 probe-* 迁到 rollcall-*。
// 读取时回退旧 key，避免升级后用户已保存的管理令牌凭空消失、需要重新粘贴。
const LS_TOKEN = 'rollcall-admin-token';
const LS_TOKEN_LEGACY = 'probe-admin-token';
const LS_THEME = 'rollcall-theme';
const LS_THEME_LEGACY = 'probe-theme';

function lsGet(key, legacy) {
  return localStorage.getItem(key) || localStorage.getItem(legacy) || '';
}
function lsSet(key, legacy, v) {
  localStorage.setItem(key, v);
  localStorage.removeItem(legacy); // 迁移后清掉旧 key，避免两份不一致
}
function lsDel(key, legacy) {
  localStorage.removeItem(key);
  localStorage.removeItem(legacy);
}

const adminToken = () => lsGet(LS_TOKEN, LS_TOKEN_LEGACY);

async function adminApi(path, opts = {}) {
  const r = await fetch(path, {
    ...opts,
    headers: { 'Content-Type': 'application/json', 'X-Admin-Token': adminToken(), ...(opts.headers || {}) },
  });
  const txt = await r.text();
  let data = null;
  try { data = txt ? JSON.parse(txt) : null; } catch (e) { data = { raw: txt }; }
  if (r.status === 401) state.authRequired = true;
  else if (r.ok) state.authRequired = false;
  if (!r.ok) throw new Error((data && (data.error || data.message)) || ('HTTP ' + r.status));
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

async function viewSettings() {
  const tok = adminToken();

  if (!tok) {
    $('#view').innerHTML = `
      <div class="panel panel-pad" style="max-width:560px">
        <div class="section-title" style="margin-bottom:6px">需要管理令牌</div>
        <p style="font-size:12.5px;color:var(--fg-2);margin:0 0 14px">
          管理功能需要控制端生成的令牌。它位于服务端数据目录（启动参数
          <code style="font-size:11.5px;background:var(--panel-2);padding:1px 5px;border-radius:4px">-data</code>
          指定，默认 <code style="font-size:11.5px;background:var(--panel-2);padding:1px 5px;border-radius:4px">/opt/rollcall/data/admin.token</code>）。
        </p>
        <form id="tokForm" class="form-grid">
          ${field('管理令牌', 'token', '', { type: 'password', placeholder: '粘贴 admin.token 内容', required: true })}
          <div class="form-actions"><button class="btn primary" type="submit">保存令牌</button></div>
        </form>
      </div>`;
    $('#tokForm').onsubmit = (e) => {
      e.preventDefault();
      lsSet(LS_TOKEN, LS_TOKEN_LEGACY, formData(e.target).token);
      toast('令牌已保存');
      viewSettings();
    };
    return;
  }

  let nodes = [], services = [], cfg = null, chans = [], defs = [];
  let loadErr = '';
  try {
    [nodes, services] = await Promise.all([
      adminApi('/api/admin/nodes'),
      adminApi('/api/admin/services'),
    ]);
    const st = await adminApi('/api/admin/settings');
    cfg = st.notify || {};
    chans = st.channels || [];
    defs = st.defs || [];
    state.panelLink = st.panelLink || '';
  } catch (e) {
    loadErr = e.message;
  }

  if (loadErr) {
    $('#view').innerHTML = `
      <div class="panel panel-pad">
        <div style="color:var(--crit);font-weight:600;font-size:13px;margin-bottom:4px">管理接口不可用</div>
        <div style="font-size:12px;color:var(--fg-2)">${esc(loadErr)}</div>
        <div class="form-actions" style="margin-top:14px">
          <button class="btn" id="retryBtn">重新加载</button>
          <button class="btn danger" id="clearTok">清除令牌</button>
        </div>
      </div>`;
    $('#retryBtn').onclick = viewSettings;
    $('#clearTok').onclick = () => { lsDel(LS_TOKEN, LS_TOKEN_LEGACY); viewSettings(); };
    return;
  }

  const t = cfg.telegram || {}, w = cfg.webhook || {}, m = cfg.email || {};

  $('#view').innerHTML = `
    <section class="section">
      <div class="section-head">
        <h2 class="section-title">管理令牌</h2>
        <span class="section-meta">已配置 · ${chans.length ? '通知通道：' + chans.join(' / ') : '未启用通知'}</span>
      </div>
      <div class="panel panel-pad">
        <div style="display:flex;gap:10px;align-items:center;flex-wrap:wrap">
          <span class="badge ok">已认证</span>
          <button class="btn" id="chgTok">更换令牌</button>
        </div>
      </div>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">节点管理</h2>
        <span class="section-meta">${nodes.length} 台</span>
      </div>
      <div class="table-wrap" style="margin-bottom:12px"><table>
        <thead><tr><th>名称</th><th>服务商</th><th>位置</th><th>分组</th><th>到期</th><th class="num">操作</th></tr></thead>
        <tbody>${nodes.map((n) => `<tr>
          <td class="strong">${esc(n.name)}</td>
          <td>${esc(n.provider || '—')}</td>
          <td>${esc(n.region || '—')}</td>
          <td>${esc(n.group || '—')}</td>
          <td class="mono">${esc(n.expireAt || '—')}</td>
          <td class="num"><button class="btn tiny" data-agent-node="${esc(n.id)}">接入</button>
            <button class="btn tiny" data-edit-node="${esc(n.id)}">编辑</button>
            <button class="btn tiny danger" data-del-node="${esc(n.id)}">删除</button></td>
        </tr>`).join('') || `<tr><td colspan="6"><div class="empty">暂无节点</div></td></tr>`}</tbody>
      </table></div>
      <details class="panel panel-pad">
        <summary class="sum">＋ 新增节点</summary>
        <form id="nodeForm" class="form-grid">
          ${field('名称', 'name', '', { required: true, placeholder: '例如 prod-01', hint: '会显示在公开状态页，勿填主机名/IP' })}
          ${field('服务商', 'provider', '', { placeholder: 'racknerd / 腾讯云 …' })}
          ${field('位置', 'region', '', { placeholder: '美国 / 香港 …' })}
          ${field('分组', 'group', '', { placeholder: 'prod / test' })}
          ${field('到期日', 'expireAt', '', { placeholder: '2027-06-01' })}
          ${field('年付价', 'price', '', { type: 'number', placeholder: '11.00' })}
          ${selectField('币种', 'currency', 'USD', [{ v: 'USD', t: 'USD ($)' }, { v: 'CNY', t: 'CNY (¥)' }])}
          ${field('备注', 'note', '', { placeholder: '可选' })}
          ${field('排序权重', 'weight', '', { type: 'number', placeholder: '0' })}
          <div class="form-actions"><button class="btn primary" type="submit">创建并获取令牌</button></div>
        </form>
      </details>
    </section>

    <section class="section">
      <div class="section-head">
        <h2 class="section-title">服务探测</h2>
        <span class="section-meta">${services.length} 个目标</span>
      </div>
      <div class="table-wrap" style="margin-bottom:12px"><table>
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
          ${selectField('类型', 'type', 'https', [{ v: 'https', t: 'HTTPS' }, { v: 'http', t: 'HTTP' }, { v: 'tcp', t: 'TCP' }])}
          ${field('目标', 'target', '', { required: true, placeholder: 'https://example.com 或 1.2.3.4:3306' })}
          ${field('间隔（秒）', 'interval', '60', { type: 'number' })}
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
        <div class="form-grid">
          ${checkbox('启用通知', 'nEnabled', !!cfg.enabled)}
          ${selectField('最低严重度', 'nMinSeverity', cfg.minSeverity || 'warn',
            [{ v: 'info', t: '信息及以上' }, { v: 'warn', t: '警告及以上' }, { v: 'critical', t: '仅严重' }])}
          ${field('重复提醒间隔（秒）', 'nRepeat', cfg.repeatInterval || 0,
            { type: 'number', hint: '0 = 只通知一次，不重复' })}
        </div>
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

  // 事件绑定
  $('#chgTok').onclick = () => { lsDel(LS_TOKEN, LS_TOKEN_LEGACY); viewSettings(); };

  // CSV 导出：带令牌下载为文件
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
        const r = await fetch(url, { headers: { 'X-Admin-Token': adminToken() } });
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
        expireAt: d.expireAt, price: Number(d.price) || 0, currency: d.currency,
        note: d.note, weight: Number(d.weight) || 0,
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
    const body = {
      enabled: $('#nEnabled').checked,
      minSeverity: $('#nMinSeverity').value,
      repeatInterval: Number($('#nRepeat').value) || 0,
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
  const cfg = JSON.stringify({
    server,
    nodeId: node.id,
    token: node.token,
    interval: 30,
  }, null, 2);

  const install = `# 1) 安装二进制与 systemd 单元（在仓库目录执行）
install -m 0755 rollcall-agent /usr/local/bin/rollcall-agent
install -m 0644 deploy/rollcall-agent.service /etc/systemd/system/

# 2) 创建专用低权限用户（只需读 /proc，不需要 root）
useradd --system --no-create-home --shell /usr/sbin/nologin rollcall-agent

# 3) 写入配置，仅 root 与 Agent 用户可读
install -d -m 0750 -o root -g rollcall-agent /etc/rollcall
# 将上面的 JSON 写入 /etc/rollcall/agent.json，然后：
chown root:rollcall-agent /etc/rollcall/agent.json
chmod 0640 /etc/rollcall/agent.json

# 4) 启动并验证一次上报（约 2 秒）
systemctl daemon-reload
systemctl enable --now rollcall-agent
systemctl status rollcall-agent --no-pager
runuser -u rollcall-agent -- /usr/local/bin/rollcall-agent -config /etc/rollcall/agent.json -once`;

  const dlg = document.createElement('div');
  dlg.className = 'modal-mask';
  dlg.innerHTML = `<div class="modal">
    <div class="modal-title">节点接入指引</div>
    <p class="modal-sub">把这台机器的 Agent 指向控制端即可开始上报。</p>
    <pre class="code">${esc(cfg)}</pre>
    ${insecure ? '<p class="modal-sub" style="color:var(--warn)">当前控制端地址是明文 HTTP，非本机 Agent 会拒绝上报。请配置 HTTPS，或确认只在本机回环使用。</p>' : ''}
    <p class="modal-sub">在目标机器上执行：</p>
    <pre class="code">${esc(install)}</pre>
    <p class="modal-sub" style="color:var(--warn)">令牌等同于该节点的上报凭据：轮换后旧令牌立即失效，需同步更新 agent.json 并重启 Agent。</p>
    <div class="form-actions">
      <button class="btn danger" id="rotateTok" type="button">轮换令牌</button>
      <button class="btn primary" id="dlgOk">知道了</button>
    </div>
  </div>`;
  document.body.appendChild(dlg);
  const close = () => dlg.remove();
  dlg.querySelector('#dlgOk').onclick = close;
  dlg.onclick = (e) => { if (e.target === dlg) close(); };
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
      ${field('分组', 'group', n.group, { placeholder: 'prod / test' })}
      ${field('到期日', 'expireAt', n.expireAt, { placeholder: '2027-06-01' })}
      ${field('年付价', 'price', n.price, { type: 'number', placeholder: '11.00' })}
      ${selectField('币种', 'currency', n.currency || 'USD', [{ v: 'USD', t: 'USD ($)' }, { v: 'CNY', t: 'CNY (¥)' }])}
      ${field('备注', 'note', n.note, { placeholder: '可选' })}
      ${field('排序权重', 'weight', n.weight, { type: 'number', placeholder: '0' })}
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
          expireAt: d.expireAt, price: Number(d.price) || 0, currency: d.currency,
          note: d.note, weight: Number(d.weight) || 0,
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
  services: { title: '服务', sub: '网站与端口可用性探测', fn: viewServices },
  alerts: { title: '告警', sub: '规则触发与历史记录', fn: viewAlerts },
  assets: { title: '台账', sub: '资产、到期与成本', fn: viewAssets },
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

function renderTokenGate() {
  state.authRequired = true;
  const had = !!adminToken();
  $('#view').innerHTML = `
    <div class="panel panel-pad" style="max-width:560px;margin:32px auto">
      <div class="section-title" style="margin-bottom:6px">需要管理令牌</div>
      <p style="font-size:12.5px;color:var(--fg-2);margin:0 0 14px">
        仪表盘包含主机名、内核、服务商、价格等资产信息，需要控制端生成的令牌。
        它位于服务端数据目录（默认
        <code style="font-size:11.5px;background:var(--panel-2);padding:1px 5px;border-radius:4px">/opt/rollcall/data/admin.token</code>）。
      </p>
      ${had ? '<p class="field-hint" style="color:var(--warn);margin:0 0 12px">当前保存的令牌无效，请重新输入或清除。</p>' : ''}
      <form id="gateForm" class="form-grid">
        ${field('管理令牌', 'token', '', { type: 'password', placeholder: '粘贴 admin.token 内容', required: true })}
        <div class="form-actions">
          <button class="btn primary" type="submit">进入仪表盘</button>
          ${had ? '<button class="btn" type="button" id="gateClear">清除令牌</button>' : ''}
        </div>
      </form>
      <p class="field-hint" style="margin-top:12px">
        首次部署也可用 <code>#admin=&lt;令牌&gt;</code> 打开；令牌只保存在浏览器本地。
      </p>
    </div>`;
  $('#gateForm').onsubmit = (e) => {
    e.preventDefault();
    lsSet(LS_TOKEN, LS_TOKEN_LEGACY, formData(e.target).token);
    state.authRequired = false;
    state.data = {};
    toast('令牌已保存');
    render();
  };
  if (had) {
    $('#gateClear').onclick = () => {
      lsDel(LS_TOKEN, LS_TOKEN_LEGACY);
      state.authRequired = false;
      state.data = {};
      render();
    };
  }
  $('#liveText').textContent = '需要令牌';
}

async function render() {
  if (state.authRequired) {
    renderTokenGate();
    return;
  }
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
      renderTokenGate();
      return;
    }
    $('#view').innerHTML = `<div class="panel panel-pad" style="text-align:center;padding:48px 20px">
      <div style="font-size:13px;color:var(--crit);font-weight:600;margin-bottom:6px">数据加载失败</div>
      <div style="font-size:12px;color:var(--fg-3)">${esc(e.message)}</div>
      <div style="font-size:11.5px;color:var(--fg-3);margin-top:10px">请确认服务端已启动（默认 :19080）</div>
    </div>`;
    $('#liveText').textContent = '离线';
  }
}

/* ---------------- 主题 ---------------- */

function applyTheme(t) {
  document.documentElement.dataset.theme = t;
  lsSet(LS_THEME, LS_THEME_LEGACY, t);
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

function boot() {
  const params = new URLSearchParams(location.search);

  // 首次访问引导：只接受 URL 片段 #admin=<令牌>。
  // 片段不会发送给服务器，因此不会落入反向代理/网关的访问日志。
  const hashParams = new URLSearchParams(location.hash.slice(1));
  const bootTok = hashParams.get('admin');
  if (bootTok) {
    lsSet(LS_TOKEN, LS_TOKEN_LEGACY, bootTok);
    hashParams.delete('admin');
    const h = hashParams.toString();
    history.replaceState(null, '', location.pathname + location.search + (h ? '#' + h : ''));
  }

  const qTheme = params.get('theme');
  applyTheme(qTheme === 'dark' || qTheme === 'light' ? qTheme
    : (lsGet(LS_THEME, LS_THEME_LEGACY) ||
       (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')));

  $('#themeBtn').onclick = () => {
    applyTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
  };

  $('#refreshBtn').onclick = () => { state.data = {}; render(); };

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
  render();

  // 每 20 秒静默刷新当前视图（节点详情页刷新频率更高以体现实时）
  // 未认证时停止轮询，避免把一次令牌缺失放大成持续的 401 触发限速。
  setInterval(() => {
    if (document.hidden || state.authRequired) return;
    if (state.route === 'nodes' && state.param) render();
  }, 20000);
  setInterval(() => {
    if (document.hidden || state.authRequired) return;
    if (!(state.route === 'nodes' && state.param)) render();
  }, 30000);
}

boot();
