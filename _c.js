
'use strict';

/* ============================================================
   一、常量与状态
   ============================================================ */

const STORE_KEY = 'rollcall-panel-prefs-v1';

/** 全局状态。只放"界面偏好 + 最近一次快照"，不放派生数据。 */
const state = {
  view: 'bars',          // bars | rings | list
  sort: { key: 'cpu', dir: -1 },
  range: '24h',          // 详情页时间范围
  source: 'mock',        // mock | http
  baseUrl: '',           // http 模式的面板地址
  interval: 30000,       // 自动刷新间隔（0 = 关闭）
  filter: { q: '', status: 'all', group: 'all' },
  theme: 'auto',         // auto | light | dark
  streamOK: false,       // 实时推送是否已连接
  snap: null,            // 最近一次快照
  prev: null,            // 上一次快照（用于差值高亮）
  loading: true,
  lastError: '',
  failures: 0,
  timer: null,
  inFlight: false,
};

/**
 * 时间范围：只保留 24h / 7d / 30d 三档。
 *
 * 不是偷懒 —— 公开详情接口（/api/status/node/{hid}?range=）只认这三档，
 * 其它值会静默回落成 24h。若面板上还摆着 1 小时/6 小时，用户选了却拿到
 * 24 小时的数据，图看起来"没变"，是比少一个档位严重得多的缺陷。
 */
const RANGES = {
  '24h': { label: '24 小时', points: 96, stepSec: 900 },
  '7d':  { label: '7 天',    points: 84, stepSec: 7200 },
  '30d': { label: '30 天',   points: 90, stepSec: 28800 },
};

/* ------------------------------------------------------------
   节点元数据（模拟数据源的"资产台账"）
   真实数据源下这些字段来自 /api/status 的 nodes[]，
   缺失的（如内核、CPU 型号）显示为"未上报"而不是编造。
   ------------------------------------------------------------ */
const NODES = [
  { id: 'n_control', name: 'Hostdzire-控制端', group: '生产', provider: 'Hostdzire', region: 'IN', flag: '🇮🇳',
    os: 'Debian 12', arch: 'x86_64', kernel: '6.1.0-18-amd64', cpuModel: 'AMD EPYC 7K62', cores: 4,
    memTotal: 8 * 2 ** 30, diskTotal: 80 * 2 ** 30,
    base: { cpu: 11, mem: 38, disk: 31, load: 0.55, rx: 1.9e6, tx: 1.2e6 }, latency: 57 },
  { id: 'n_bestvm', name: 'bestvm-de', group: '生产', provider: 'BestVM', region: 'DE', flag: '🇩🇪',
    os: 'Ubuntu 24.04', arch: 'x86_64', kernel: '6.8.0-45-generic', cpuModel: 'Intel Xeon Gold 6230', cores: 6,
    memTotal: 4 * 2 ** 30, diskTotal: 100 * 2 ** 30,
    base: { cpu: 24, mem: 63, disk: 66, load: 1.2, rx: 4.4e6, tx: 3.1e6 }, latency: 78 },
  { id: 'n_dmit', name: 'dmit-49.9', group: '生产', provider: 'DMIT', region: 'US', flag: '🇺🇸',
    os: 'Debian 11', arch: 'x86_64', kernel: '5.10.0-32-amd64', cpuModel: 'AMD EPYC 7402P', cores: 8,
    memTotal: 8 * 2 ** 30, diskTotal: 200 * 2 ** 30,
    base: { cpu: 31, mem: 49, disk: 22, load: 0.9, rx: 6.2e6, tx: 5.8e6 }, latency: 152 },
  { id: 'n_hostyun', name: 'hostyun-香港', group: '生产', provider: 'HostYun', region: 'HK', flag: '🇭🇰',
    os: 'Ubuntu 22.04', arch: 'x86_64', kernel: '5.15.0-118-generic', cpuModel: 'Intel Xeon E5-2680 v4', cores: 4,
    memTotal: 4 * 2 ** 30, diskTotal: 60 * 2 ** 30,
    base: { cpu: 44, mem: 71, disk: 58, load: 1.8, rx: 8.1e6, tx: 7.3e6 }, latency: 34 },
  { id: 'n_txy_sh', name: '腾讯云-上海', group: '边缘', provider: '腾讯云', region: 'CN', flag: '🇨🇳',
    os: 'Ubuntu 22.04', arch: 'x86_64', kernel: '5.15.0-113-generic', cpuModel: 'Intel Xeon Platinum 8255C', cores: 4,
    memTotal: 4 * 2 ** 30, diskTotal: 60 * 2 ** 30,
    base: { cpu: 18, mem: 52, disk: 47, load: 0.7, rx: 2.6e6, tx: 2.2e6 }, latency: 12 },
  { id: 'n_ali_cd', name: '阿里云-成都', group: '边缘', provider: '阿里云', region: 'CN', flag: '🇨🇳',
    os: 'Alibaba Cloud Linux 3', arch: 'x86_64', kernel: '5.10.134-16.al8', cpuModel: 'Intel Xeon Platinum 8369B', cores: 2,
    memTotal: 4 * 2 ** 30, diskTotal: 40 * 2 ** 30,
    base: { cpu: 27, mem: 44, disk: 39, load: 0.5, rx: 1.4e6, tx: 1.1e6 }, latency: 21 },
  { id: 'n_lvyun', name: '绿云-jp', group: '边缘', provider: '绿云', region: 'JP', flag: '🇯🇵',
    os: 'Debian 12', arch: 'x86_64', kernel: '6.1.0-26-amd64', cpuModel: 'Intel Xeon E5-2696 v4', cores: 2,
    memTotal: 2 * 2 ** 30, diskTotal: 40 * 2 ** 30,
    base: { cpu: 62, mem: 88, disk: 73, load: 2.4, rx: 3.3e6, tx: 2.9e6 }, latency: 96 },
  { id: 'n_wu', name: 'cera-洛杉矶', group: '边缘', provider: 'Cera', region: 'US', flag: '🇺🇸',
    os: 'Debian 12', arch: 'x86_64', kernel: '6.1.0-21-amd64', cpuModel: 'AMD EPYC 7B13', cores: 2,
    memTotal: 2 * 2 ** 30, diskTotal: 40 * 2 ** 30,
    base: { cpu: 9, mem: 33, disk: 18, load: 0.3, rx: 1.1e6, tx: 0.9e6 }, latency: 168 },
  { id: 'n_rn', name: 'racknerd-西雅图', group: '边缘', provider: 'RackNerd', region: 'US', flag: '🇺🇸',
    os: 'CentOS 7', arch: 'x86_64', kernel: '3.10.0-1160.el7', cpuModel: 'Intel Xeon E5-2690 v4', cores: 2,
    memTotal: 2 * 2 ** 30, diskTotal: 30 * 2 ** 30,
    base: { cpu: 7, mem: 61, disk: 88, load: 0.2, rx: 0.7e6, tx: 0.5e6 }, latency: 189 },
  { id: 'n_vultr', name: 'vultr-法兰克福', group: '边缘', provider: 'Vultr', region: 'DE', flag: '🇩🇪',
    os: 'Ubuntu 24.04', arch: 'x86_64', kernel: '6.8.0-41-generic', cpuModel: 'AMD EPYC 7R13', cores: 2,
    memTotal: 4 * 2 ** 30, diskTotal: 80 * 2 ** 30,
    base: { cpu: 15, mem: 29, disk: 26, load: 0.4, rx: 1.6e6, tx: 1.3e6 }, latency: 142 },
  { id: 'n_oracle', name: 'oracle-大阪', group: '实验', provider: 'Oracle', region: 'JP', flag: '🇯🇵',
    os: 'Ubuntu 22.04', arch: 'aarch64', kernel: '5.15.0-119-generic', cpuModel: 'Ampere Altra Q80-30', cores: 4,
    memTotal: 24 * 2 ** 30, diskTotal: 200 * 2 ** 30,
    base: { cpu: 5, mem: 17, disk: 9, load: 0.12, rx: 0.4e6, tx: 0.3e6 }, latency: 88 },
  { id: 'n_home', name: '家宽-西安', group: '实验', provider: '自建', region: 'CN', flag: '🇨🇳',
    os: 'Debian 12', arch: 'x86_64', kernel: '6.1.0-25-amd64', cpuModel: 'Intel N100', cores: 4,
    memTotal: 16 * 2 ** 30, diskTotal: 500 * 2 ** 30,
    base: { cpu: 21, mem: 34, disk: 41, load: 0.8, rx: 5.1e6, tx: 4.2e6 }, latency: 8 },
  { id: 'n_test', name: 'test-成都', group: '实验', provider: '阿里云', region: 'CN', flag: '🇨🇳',
    os: 'Debian 12', arch: 'x86_64', kernel: '6.1.0-18-cloud-amd64', cpuModel: 'Intel Xeon Platinum 8269CY', cores: 2,
    memTotal: 2 * 2 ** 30, diskTotal: 40 * 2 ** 30,
    base: { cpu: 3, mem: 22, disk: 15, load: 0.08, rx: 0.2e6, tx: 0.15e6 }, latency: 19 },
];

/* ============================================================
   二、工具函数
   ============================================================ */

/** 稳定哈希：把字符串映射到 32 位整数，用作各节点随机流的种子。 */
function hash32(str) {
  let h = 2166136261;
  for (let i = 0; i < str.length; i++) {
    h ^= str.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

/** mulberry32：小而快的确定性 PRNG。同一 seed 永远给同一序列。 */
function makeRng(seed) {
  let a = seed >>> 0;
  return function rng() {
    a = (a + 0x6D2B79F5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

/** 字节数 → 人类可读。监控面板里 GB/TB 混用时保留 1 位小数。 */
function fmtBytes(bytes, digits = 1) {
  const n = Number(bytes) || 0;
  if (n < 1024) return n.toFixed(0) + ' B';
  const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let i = -1, v = n;
  do { v /= 1024; i++; } while (v >= 1024 && i < units.length - 1);
  return v.toFixed(v >= 100 ? 0 : digits) + ' ' + units[i];
}

/** 速率（字节/秒）。小于 1 KB/s 时用整数 B/s，避免显示 "0.0 KB/s"。 */
function fmtRate(bps) {
  const n = Number(bps) || 0;
  if (Math.abs(n) < 1024) return n.toFixed(0) + ' B/s';
  return fmtBytes(n) + '/s';
}

/**
 * 容量对（已用/总量）：单位相同时只写一次，如 "24/80 GB"。
 * 卡片里三列并排，写两遍单位会撑换行导致卡片高度不齐。
 */
function fmtPair(used, total) {
  if (used == null || total == null) return '未上报';
  const split = (v) => {
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0, x = Number(v) || 0;
    while (x >= 1024 && i < units.length - 1) { x /= 1024; i++; }
    return { x, u: units[i] };
  };
  const a = split(used), b = split(total);
  if (a.u === b.u && b.x > 0) {
    const f = (n) => n.toFixed(n >= 100 ? 0 : 1);
    return `${f(a.x)}/${b.x.toFixed(0)} ${b.u}`;
  }
  return `${fmtBytes(used)} / ${fmtBytes(total)}`;
}

/** 秒 → "227天 10:10" 这类紧凑写法。 */
function fmtDuration(sec) {
  const s = Math.max(0, Math.floor(Number(sec) || 0));
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d} 天 ${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}`;
  if (h > 0) return `${h} 小时 ${String(m).padStart(2, '0')} 分`;
  return `${m} 分 ${String(s % 60).padStart(2, '0')} 秒`;
}

/** 时间戳 → "8 秒前"。超过一天直接显示日期，避免"37 天前"这类难换算的表述。 */
function fmtAgo(ts, now = Date.now()) {
  const diff = Math.max(0, Math.floor((now - ts) / 1000));
  if (diff < 5) return '刚刚';
  if (diff < 60) return diff + ' 秒前';
  if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
  if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
  const d = new Date(ts);
  return `${d.getMonth() + 1}/${d.getDate()}`;
}

/** 时间戳 → HH:MM 或 MM/DD（跨度超过一天时用日期更易读）。 */
function fmtClock(ts, spanSec) {
  const d = new Date(ts);
  const pad = (n) => String(n).padStart(2, '0');
  if (spanSec > 86400 * 1.5) return `${pad(d.getMonth() + 1)}/${pad(d.getDate())}`;
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

const pct1 = (v) => (Number(v) || 0).toFixed(1) + '%';

/** 水位配色：<60 主色、60–80 琥珀、≥80 红。只有越线才改色，避免视觉噪音。 */
function levelColor(pct) {
  const v = Number(pct) || 0;
  if (v >= 80) return 'var(--crit)';
  if (v >= 60) return 'var(--warn)';
  return 'var(--accent)';
}

/** HTML 转义。所有来自数据源的字符串都必须过这一层。 */
function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

/* ============================================================
   三、数据层
   ------------------------------------------------------------
   两种数据源统一产出同一个快照结构，视图层不关心来源：

   snapshot = {
     at: 毫秒时间戳,
     source: 'mock' | 'http',
     partial: bool,          // http 模式下是否缺字段
     nodes: [ node ]
   }
   node = {
     id, name, group, provider, region, flag, os, arch, kernel, cpuModel, cores,
     online, state: 'online'|'stale'|'offline',
     cpu, mem, disk, load1, load5, load15,
     memUsed, memTotal, diskUsed, diskTotal,
     rxRate, txRate, totalRx, totalTx,
     uptimeSec, lastSeen, latency
   }
   ============================================================ */

/**
 * 模拟数据：每个节点一条"随机游走 + 回归基线"的状态链。
 *
 * 为什么不是每次随机生成？——那样每次刷新数字都会跳变，看不出趋势，
 * 也无法验证"数据在动"之外的任何东西。随机游走能同时给出：
 *   · 连续变化（相邻两次刷新差异小）
 *   · 偶发尖峰（真实监控里最常见的那种）
 *   · 稳定性（不会漂到 0 或 100）
 */
const mockState = new Map();

function mockNodeState(node, now) {
  const key = node.id;
  let st = mockState.get(key);
  if (!st) {
    const rng = makeRng(hash32(key));
    st = {
      rng,
      cpu: node.base.cpu,
      mem: node.base.mem,
      disk: node.base.disk,
      load: node.base.load,
      rx: node.base.rx,
      tx: node.base.tx,
      spike: 0,
      bootAt: now - (90 + rng() * 260) * 86400 * 1000,
    };
    mockState.set(key, st);
  }

  const r = st.rng;
  // 日周期：白天负载高、凌晨低。幅度随节点基线缩放，低负载节点不放大。
  const hour = new Date(now).getHours() + new Date(now).getMinutes() / 60;
  const diurnal = Math.sin(((hour - 4) / 24) * Math.PI * 2) * (0.12 + node.base.cpu / 260);

  // 回归基线：离基线越远，回拉越强（等价于 Ornstein-Uhlenbeck 的离散形式）
  const pull = (cur, base, k = 0.16) => cur + (base - cur) * k;

  st.cpu = pull(st.cpu, node.base.cpu * (1 + diurnal)) + (r() - 0.5) * 3.2;
  st.mem = pull(st.mem, node.base.mem, 0.06) + (r() - 0.5) * 1.1;
  st.disk = pull(st.disk, node.base.disk, 0.02) + (r() - 0.5) * 0.18;
  st.load = Math.max(0, pull(st.load, node.base.load * (1 + diurnal), 0.2) + (r() - 0.5) * 0.16);

  // 尖峰：约 4% 的刷新会触发一次 20~45 个百分点的短时抬升，随后衰减
  if (st.spike > 0.4) st.spike *= 0.82;
  else if (r() < 0.04) st.spike = 20 + r() * 25;

  // 网络：突发型（大部分时间平稳，偶发大流量）
  const burst = r() < 0.08 ? 2.6 + r() * 3.4 : 1;
  st.rx = Math.max(0, pull(st.rx, node.base.rx * burst, 0.35) + (r() - 0.5) * node.base.rx * 0.22);
  st.tx = Math.max(0, pull(st.tx, node.base.tx * burst, 0.35) + (r() - 0.5) * node.base.tx * 0.22);

  const cpu = clamp(st.cpu + st.spike, 0.4, 99.6);
  const mem = clamp(st.mem + st.spike * 0.35, 1, 99.4);
  const disk = clamp(st.disk, 0.5, 99.5);
  const load1 = clamp(st.load + st.spike / 40, 0, node.cores * 4);

  // 状态：约 3% 概率进入"陈旧"（模拟采集延迟），offline 由固定节点演示
  const offline = node.id === 'n_test';
  const stale = !offline && r() < 0.03;

  return {
    id: node.id, name: node.name, group: node.group, provider: node.provider,
    region: node.region, flag: node.flag, os: node.os, arch: node.arch,
    kernel: node.kernel, cpuModel: node.cpuModel, cores: node.cores,
    online: !offline,
    state: offline ? 'offline' : stale ? 'stale' : 'online',
    cpu, mem, disk,
    load1, load5: Math.max(0, load1 * (0.82 + r() * 0.2)), load15: Math.max(0, load1 * (0.66 + r() * 0.24)),
    memUsed: (mem / 100) * node.memTotal, memTotal: node.memTotal,
    diskUsed: (disk / 100) * node.diskTotal, diskTotal: node.diskTotal,
    rxRate: offline ? 0 : st.rx, txRate: offline ? 0 : st.tx,
    totalRx: (18 + (node.base.rx / 1e6) * 40) * 2 ** 30,
    totalTx: (12 + (node.base.tx / 1e6) * 32) * 2 ** 30,
    uptimeSec: offline ? 0 : Math.floor((now - st.bootAt) / 1000),
    lastSeen: offline ? now - 3.5 * 3600 * 1000 : now - (stale ? 210 + r() * 200 : r() * 22) * 1000,
    latency: offline ? 0 : Math.round(node.latency * (0.9 + r() * 0.28)),
  };
}

function mockSnapshot() {
  const now = Date.now();
  const nodes = NODES.map((n) => mockNodeState(n, now));
  // 事件流：模拟数据源也给几条，保证两种数据源下界面结构一致。
  // 内容取自当前节点状态，时间倒推，读起来像真实的"刚刚发生了什么"。
  const busiest = nodes.slice().sort((a, b) => b.cpu - a.cpu)[0];
  const worstDisk = nodes.slice().sort((a, b) => b.disk - a.disk)[0];
  const off = nodes.find((n) => n.state === 'offline');
  const activity = [
    busiest && { kind: 'alert_opened', level: 'warn', ts: Math.floor((now - 4 * 60000) / 1000),
      text: `${busiest.name} CPU 使用率 ${busiest.cpu.toFixed(1)}%，超过阈值 80%` },
    { kind: 'alert_resolved', level: 'info', ts: Math.floor((now - 26 * 60000) / 1000),
      text: `${busiest.name} 的「CPU 使用率过高」已恢复` },
    worstDisk && { kind: 'alert_opened', level: 'warn', ts: Math.floor((now - 92 * 60000) / 1000),
      text: `${worstDisk.name} 磁盘使用率 ${worstDisk.disk.toFixed(1)}%` },
    off && { kind: 'node_offline', level: 'error', ts: Math.floor((now - 3.4 * 3600000) / 1000),
      text: `${off.name} 已离线（最后一次上报 3 小时前）` },
    { kind: 'node_online', level: 'info', ts: Math.floor((now - 7.2 * 3600000) / 1000),
      text: '所有节点已完成 Agent v1.40.0 升级' },
  ].filter(Boolean);

  return { at: now, source: 'mock', partial: false, nodes, activity };
}

/**
 * 实时接口：GET {base}/api/status。
 *
 * 字段映射按 Rollcall 的公开状态接口；缺字段一律留空并置 partial=true，
 * 由视图层显示"未上报"，不静默补零（补零会被误读成"使用率为 0"）。
 */
async function httpSnapshot(base) {
  const root = String(base || '').replace(/\/+$/, '');
  const url = root + '/api/status';
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 12000);
  let res;
  try {
    res = await fetch(url, { signal: ctrl.signal, headers: { Accept: 'application/json' }, credentials: 'omit' });
  } finally {
    clearTimeout(timer);
  }
  if (!res.ok) throw new Error(`接口返回 HTTP ${res.status}`);
  let d;
  try {
    d = await res.json();
  } catch {
    throw new Error('接口返回的不是 JSON（公开页可能未开启）');
  }

  const raw = Array.isArray(d.nodes) ? d.nodes : [];
  if (!raw.length) throw new Error('接口未返回任何节点');

  const now = Date.now();
  let partial = false;
  const nodes = raw.map((n, i) => {
    // 用公开句柄作路由键：公开接口不下发内部 ID，面板也不该依赖它
    const id = n.hid || n.id || 'n' + i;
    const need = (v) => { if (v == null) { partial = true; return null; } return v; };
    const lastSeen = Number(n.lastSeen || 0) * 1000 || now;
    const online = n.online === true || n.state === 'online';
    return {
      // 硬件与地区：公开接口不提供，一律留空由视图隐藏。
      // 不用本地资产台账"补"—— 台账只属于模拟数据；部署到别人的面板上，
      // 同名节点会被贴上错误的 OS/地区，那比空着危险得多。
      id, name: n.name || id, group: n.group || '',
      provider: '', region: '', flag: '',
      os: '', arch: '', kernel: '', cpuModel: '', cores: 0,
      online,
      state: n.state || (online ? 'online' : 'offline'),
      cpu: need(n.cpu), mem: need(n.mem), disk: need(n.disk),
      load1: need(n.load1), load5: n.load5 ?? null, load15: n.load15 ?? null,
      memUsed: n.memUsed ?? null, memTotal: n.memTotal ?? null,
      diskUsed: n.diskUsed ?? null, diskTotal: n.diskTotal ?? null,
      rxRate: n.netRx ?? null, txRate: n.netTx ?? null,
      totalRx: n.totalRx ?? null, totalTx: n.totalTx ?? null,
      uptimeSec: n.uptime ?? null, lastSeen,
      latency: n.latency ?? null,
    };
  });

  return {
    at: now,
    source: 'http',
    partial,
    nodes,
    // 原始响应留给"最近事件"用（activity 是公开接口的一部分，
    // 与节点列表一起返回，不需要额外请求）。
    activity: Array.isArray(d.activity) ? d.activity : [],
    summary: d.summary || {},
  };
}

/* 数据源统一入口：http 失败不回退静默，而是抛出由调度层决定如何提示。 */
async function loadSnapshot() {
  if (state.source === 'http' && state.baseUrl) {
    return httpSnapshot(state.baseUrl);
  }
  // 模拟数据也走一次微小延迟，让骨架屏与"刷新中"反馈在演示时可见
  await new Promise((r) => setTimeout(r, 180));
  return mockSnapshot();
}

/* ------------------------------------------------------------
   四、历史序列
   ------------------------------------------------------------
   模拟模式：按 range 的粒度生成确定性序列（同节点同区间形态稳定）。
   http 模式：尝试 /api/nodes/{id}/series?range=，失败则返回 null，
   视图层显示"该数据源未提供历史"——不伪造曲线。
   ------------------------------------------------------------ */
/**
 * 读取历史序列，返回**列式**结构：
 *   { cpu:[{t,v}], mem:[...], disk:[...], rx:[...], tx:[...], load1:[...] }
 * 某一列为 null 表示该数据源没提供 —— 视图层据此显示「未提供」，
 * 而不是画一条假曲线。
 *
 * 两种数据源：
 *   · 实时接口 → GET /api/status/node/{hid}?range=，返回 series.{cpu,mem,disk,netRx,netTx}
 *     这是公开页自己的详情接口（按可见性裁剪、无需鉴权），面板作为公开页的
 *     另一种视图，复用它而不是去要管理接口的权限。
 *   · 模拟数据 → 按区间粒度确定性生成，形态与真实一致。
 */
async function loadSeries(nodeId, rangeKey) {
  const range = RANGES[rangeKey] || RANGES['24h'];

  if (state.source === 'http' && state.baseUrl) {
    const root = state.baseUrl.replace(/\/+$/, '');
    try {
      const res = await fetch(
        `${root}/api/status/node/${encodeURIComponent(nodeId)}?range=${encodeURIComponent(rangeKey)}`,
        { headers: { Accept: 'application/json' }, credentials: 'omit' });
      if (!res.ok) return null;
      const d = await res.json();
      const s = d.series || {};
      const col = (arr) => (Array.isArray(arr) && arr.length
        ? arr.map((p) => ({ t: Number(p.t) * 1000, v: Number(p.v) }))
            .filter((p) => isFinite(p.t) && isFinite(p.v))
        : null);
      const out = {
        cpu: col(s.cpu), mem: col(s.mem), disk: col(s.disk),
        rx: col(s.netRx), tx: col(s.netTx),
        // 公开接口只给负载的当前值，不给历史 —— 如实留空，让视图显示"未提供"
        load1: null,
      };
      return (out.cpu || out.mem) ? out : null;
    } catch {
      return null;
    }
  }

  // 模拟：确定性生成，最后展开成与真实接口相同的列式结构
  const meta = NODES.find((n) => n.id === nodeId);
  const base = meta ? meta.base : { cpu: 20, mem: 45, disk: 40, load: 0.8, rx: 2e6, tx: 1.5e6 };
  const rng = makeRng(hash32(nodeId + '|' + rangeKey));
  const now = Date.now();
  const rows = [];
  let cpu = base.cpu, mem = base.mem, disk = base.disk, load = base.load;
  let rx = base.rx, tx = base.tx;
  let spike = 0;
  for (let i = range.points - 1; i >= 0; i--) {
    const t = now - i * range.stepSec * 1000;
    const hour = new Date(t).getHours() + new Date(t).getMinutes() / 60;
    const diurnal = Math.sin(((hour - 4) / 24) * Math.PI * 2) * (0.14 + base.cpu / 240);
    cpu += (base.cpu * (1 + diurnal) - cpu) * 0.18 + (rng() - 0.5) * 7;
    mem += (base.mem - mem) * 0.07 + (rng() - 0.5) * 2.2;
    disk += (base.disk - disk) * 0.02 + (rng() - 0.5) * 0.3;
    load += (base.load - load) * 0.2 + (rng() - 0.5) * 0.3;
    if (spike > 0.5) spike *= 0.78; else if (rng() < 0.05) spike = 18 + rng() * 30;
    const burst = rng() < 0.07 ? 2.4 + rng() * 3 : 1;
    rx += (base.rx * burst - rx) * 0.3 + (rng() - 0.5) * base.rx * 0.3;
    tx += (base.tx * burst - tx) * 0.3 + (rng() - 0.5) * base.tx * 0.3;
    rows.push({
      t,
      cpu: clamp(cpu + spike, 0.5, 99.5),
      mem: clamp(mem + spike * 0.3, 1, 99),
      disk: clamp(disk, 0.5, 99.5),
      rx: Math.max(0, rx),
      tx: Math.max(0, tx),
      load1: Math.max(0, load + spike / 45),
    });
  }
  const col = (k) => rows.map((r) => ({ t: r.t, v: r[k] }));
  return {
    cpu: col('cpu'), mem: col('mem'), disk: col('disk'),
    rx: col('rx'), tx: col('tx'), load1: col('load1'),
  };
}

/* ============================================================
   五、图表原语（手绘 SVG，无第三方库）
   ============================================================ */

/**
 * 3/4 环形图。
 * 缺口朝正下方（旋转 135° 起画 270°），进度从缺口左侧顺时针增长 ——
 * 与参考项目一致，也比整圆更容易看清"起点/终点"。
 */
function donutSVG(pct, opts) {
  const o = opts || {};
  const size = o.size || 96;
  const stroke = o.stroke || 9;
  const v = clamp(Number(pct) || 0, 0, 100);
  const color = o.color || levelColor(v);
  const r = (size - stroke) / 2 - 1.5;
  const cx = size / 2;
  const circ = 2 * Math.PI * r;
  const trackLen = circ * 0.75;
  const valLen = trackLen * (v / 100);
  const dim = o.dim ? ' opacity=".42"' : '';
  return `<svg viewBox="0 0 ${size} ${size}" role="img" aria-label="${esc(o.label || '')} ${v.toFixed(1)}%">
    <g transform="rotate(135 ${cx} ${cx})"${dim}>
      <circle class="donut-track" cx="${cx}" cy="${cx}" r="${r}" fill="none" stroke-width="${stroke}"
              stroke-linecap="round" stroke-dasharray="${trackLen.toFixed(2)} ${circ.toFixed(2)}"/>
      <circle class="donut-val" cx="${cx}" cy="${cx}" r="${r}" fill="none" stroke="${color}"
              stroke-width="${stroke}" stroke-linecap="round"
              stroke-dasharray="${valLen.toFixed(2)} ${circ.toFixed(2)}"/>
    </g>
    <text class="donut-num" x="${cx}" y="${cx + 5}" text-anchor="middle">${v.toFixed(0)}<tspan class="donut-pct" dy="-2">%</tspan></text>
  </svg>`;
}

/* 图表注册表：hover 需要拿到生成时的几何信息，避免把数据塞进 DOM 属性。 */
const CHART_REG = new Map();

/** 把点列按 null 切成连续段 —— 缺数据不能补 0，否则会画出从 0 起算的假曲线。 */
function splitSegments(points) {
  const segs = [];
  let cur = [];
  for (const p of points) {
    const ok = p.v != null && isFinite(p.v);
    if (!ok) {
      if (cur.length) segs.push(cur);
      cur = [];
    } else {
      cur.push(p);
    }
  }
  if (cur.length) segs.push(cur);
  return segs;
}

/**
 * 折线 / 面积图。
 * cfg = {
 *   id, height, series:[{key,label,color,points:[{t,v}],area:bool}],
 *   yMax, yMin, yFmt, spanSec, unit
 * }
 */
function lineChart(cfg) {
  const W = 640;
  const H = cfg.height || 172;
  const padL = cfg.padL || 42;
  const padR = 10;
  const padT = 10;
  const padB = 20;
  const iw = W - padL - padR;
  const ih = H - padT - padB;

  const series = (cfg.series || []).filter((s) => s.points && s.points.length);
  if (!series.length) return '';

  const allT = series.flatMap((s) => s.points.map((p) => p.t));
  const tMin = Math.min(...allT);
  const tMax = Math.max(...allT);
  const vals = series.flatMap((s) => s.points.map((p) => p.v)).filter((v) => v != null && isFinite(v));
  if (!vals.length) return '';

  const yMax = cfg.yMax != null ? cfg.yMax : Math.max(...vals) * 1.15;
  const yMin = cfg.yMin != null ? cfg.yMin : 0;
  const ySpan = Math.max(1e-9, yMax - yMin);

  const xOf = (t) => padL + (tMax === tMin ? iw / 2 : ((t - tMin) / (tMax - tMin)) * iw);
  const yOf = (v) => padT + ih - ((clamp(v, yMin, yMax) - yMin) / ySpan) * ih;

  // 水平网格 4 条 + Y 轴标签
  let grid = '';
  const ticks = 4;
  for (let i = 0; i <= ticks; i++) {
    const val = yMin + (ySpan * i) / ticks;
    const y = yOf(val).toFixed(1);
    grid += `<line class="grid-line" x1="${padL}" y1="${y}" x2="${padL + iw}" y2="${y}"/>`;
    grid += `<text class="axis-text" x="${padL - 6}" y="${(+y + 3).toFixed(1)}" text-anchor="end">${cfg.yFmt ? cfg.yFmt(val) : Math.round(val)}</text>`;
  }

  // X 轴：4 个时间标签（首/中/尾，避免拥挤）
  let axis = '';
  const spanSec = cfg.spanSec || (tMax - tMin) / 1000;
  for (let i = 0; i < 4; i++) {
    const t = tMin + ((tMax - tMin) * i) / 3;
    const x = xOf(t);
    axis += `<text class="axis-text" x="${x.toFixed(1)}" y="${padT + ih + 14}" text-anchor="${i === 0 ? 'start' : i === 3 ? 'end' : 'middle'}">${fmtClock(t, spanSec)}</text>`;
  }

  // 曲线与面积
  let body = '';
  series.forEach((s, si) => {
    const pts = s.points.map((p) => ({ t: p.t, v: p.v }));
    const segs = splitSegments(pts);
    segs.forEach((seg) => {
      const line = seg.map((p, i) => `${i === 0 ? 'M' : 'L'}${xOf(p.t).toFixed(1)},${yOf(p.v).toFixed(1)}`).join(' ');
      if (s.area !== false && seg.length > 1 && si === 0) {
        const area = `${line} L${xOf(seg[seg.length - 1].t).toFixed(1)},${(padT + ih).toFixed(1)} L${xOf(seg[0].t).toFixed(1)},${(padT + ih).toFixed(1)} Z`;
        body += `<path d="${area}" fill="${s.color}" fill-opacity="0.10"/>`;
      }
      body += `<path d="${line}" fill="none" stroke="${s.color}" stroke-width="1.7"
                stroke-linejoin="round" stroke-linecap="round"/>`;
      // 单点（只有 1 个样本）时画个圆点，否则看不见
      if (seg.length === 1) {
        body += `<circle cx="${xOf(seg[0].t).toFixed(1)}" cy="${yOf(seg[0].v).toFixed(1)}" r="2.4" fill="${s.color}"/>`;
      }
    });
  });

  // 十字线（hover 时显示）
  const cid = 'c' + Math.random().toString(36).slice(2, 8);
  CHART_REG.set(cid, {
    series, xOf, yOf, padL, padR, padT, ih, W, tMin, tMax,
    yFmt: cfg.yFmt, spanSec,
    minT: tMin, maxT: tMax,
  });

  return `<div class="chart-wrap" data-chart="${cid}">
    <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" style="height:${H}px" role="img"
         aria-label="${esc(cfg.aria || '趋势图')}">
      ${grid}
      <line class="grid-line" x1="${padL}" y1="${padT + ih}" x2="${padL + iw}" y2="${padT + ih}"/>
      ${body}
      ${axis}
      <line class="cross" x1="0" y1="${padT}" x2="0" y2="${padT + ih}"
            stroke="var(--line-3)" stroke-width="1" stroke-dasharray="3 3" opacity="0"/>
    </svg>
    <div class="tip"></div>
  </div>`;
}

/** 折线图的悬停读数：按 X 反查最近的时间点，显示该时刻所有序列的值。 */
function bindChartHover(root) {
  root.querySelectorAll('[data-chart]').forEach((wrap) => {
    if (wrap.dataset.bound) return;
    wrap.dataset.bound = '1';
    const reg = CHART_REG.get(wrap.dataset.chart);
    const tip = wrap.querySelector('.tip');
    const cross = wrap.querySelector('.cross');
    if (!reg || !tip) return;

    const move = (ev) => {
      const rect = wrap.getBoundingClientRect();
      const svgX = ((ev.clientX - rect.left) / rect.width) * reg.W;
      // 反推时间，再找最近样本
      const ratio = clamp((svgX - reg.padL) / Math.max(1, reg.W - reg.padL - reg.padR), 0, 1);
      const t = reg.tMin + (reg.tMax - reg.tMin) * ratio;
      const ref = reg.series[0].points;
      let best = null, bestDiff = Infinity;
      for (const p of ref) {
        const d = Math.abs(p.t - t);
        if (d < bestDiff) { bestDiff = d; best = p; }
      }
      if (!best) return;
      const x = reg.xOf(best.t);
      if (cross) {
        cross.setAttribute('x1', x.toFixed(1));
        cross.setAttribute('x2', x.toFixed(1));
        cross.setAttribute('opacity', '1');
      }
      const rows = reg.series.map((s) => {
        const pt = s.points.find((p) => p.t === best.t);
        const v = pt ? pt.v : null;
        const txt = v == null ? '—' : (reg.yFmt ? reg.yFmt(v) : v.toFixed(1));
        return `<div class="t-row"><span class="t-dot" style="background:${s.color}"></span>${esc(s.label)} <b>${esc(txt)}</b></div>`;
      }).join('');
      tip.innerHTML = `<div class="t-time">${esc(fmtClock(best.t, reg.spanSec))}</div>${rows}`;
      tip.classList.add('on');
      // 提示框跟随指针，但夹在容器内，避免溢出卡片
      const px = (x / reg.W) * rect.width;
      tip.style.left = clamp(px, 46, rect.width - 46) + 'px';
      tip.style.top = (ev.clientY - rect.top - 6) + 'px';
    };

    wrap.addEventListener('pointermove', move);
    wrap.addEventListener('pointerleave', () => {
      tip.classList.remove('on');
      if (cross) cross.setAttribute('opacity', '0');
    });
  });
}

/** 把数值上限收成"好读"的刻度（1/2/5 × 10^n），让 Y 轴落在整数上。 */
function niceMax(v) {
  const x = Number(v);
  if (!(x > 0)) return 1;
  const exp = Math.floor(Math.log10(x));
  const base = Math.pow(10, exp);
  const n = x / base;
  const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
  return step * base;
}

/** 分位数（线性插值）。详情页用它显示"较差的 5% 样本"，而不是只看均值。 */
function percentile(arr, q) {
  const a = arr.filter((v) => v != null && isFinite(v)).slice().sort((x, y) => x - y);
  if (!a.length) return null;
  const pos = q * (a.length - 1);
  const lo = Math.floor(pos), hi = Math.ceil(pos);
  return a[lo] + (a[hi] - a[lo]) * (pos - lo);
}

/* ============================================================
   六、视图渲染
   ============================================================ */

/** 顶部四段统计条。数据来自当前快照的聚合，不在前端做二次持久化。 */
function renderStats(snap) {
  const nodes = snap.nodes;
  const online = nodes.filter((n) => n.state === 'online').length;
  const stale = nodes.filter((n) => n.state === 'stale').length;
  const offline = nodes.length - online - stale;

  const sum = (f) => nodes.reduce((a, n) => a + (Number(f(n)) || 0), 0);
  const totalRx = sum((n) => n.totalRx);
  const totalTx = sum((n) => n.totalTx);
  const rateRx = sum((n) => n.rxRate);
  const rateTx = sum((n) => n.txRate);
  const avgCpu = sum((n) => n.cpu) / Math.max(1, nodes.length);
  const avgMem = sum((n) => n.mem) / Math.max(1, nodes.length);
  const memUsed = sum((n) => n.memUsed);
  const memTotal = sum((n) => n.memTotal);

  const upPct = (online / Math.max(1, nodes.length)) * 100;

  document.getElementById('stats').innerHTML = `
    <div class="stat">
      <div class="stat-k">服务器</div>
      <div class="stat-v"><span style="color:var(--ok)">${online}</span><span style="color:var(--fg-3);font-weight:400"> / ${nodes.length}</span>
        <span class="unit">在线率 ${upPct.toFixed(0)}%</span></div>
      <div class="stat-sub">
        ${stale ? `<span style="color:var(--warn)">陈旧 ${stale}</span><span class="stat-sep">·</span>` : ''}
        <span style="color:${offline ? 'var(--crit)' : 'var(--fg-3)'}">离线 ${offline}</span>
      </div>
    </div>
    <div class="stat">
      <div class="stat-k">累计流量</div>
      <div class="stat-v">${esc(fmtBytes(totalRx + totalTx))}</div>
      <div class="stat-sub"><span class="down">${esc(fmtBytes(totalRx))}</span><span class="up">${esc(fmtBytes(totalTx))}</span></div>
    </div>
    <div class="stat">
      <div class="stat-k">实时速率</div>
      <div class="stat-v">${esc(fmtRate(rateRx + rateTx))}</div>
      <div class="stat-sub"><span class="down">${esc(fmtRate(rateRx))}</span><span class="up">${esc(fmtRate(rateTx))}</span></div>
    </div>
    <div class="stat">
      <div class="stat-k">平均水位</div>
      <div class="stat-v">${pct1(avgCpu)}<span class="unit">CPU</span></div>
      <div class="stat-sub">
        <span>内存 ${pct1(avgMem)}</span>
        <span class="stat-sep">·</span>
        <span>${esc(fmtBytes(memUsed))} / ${esc(fmtBytes(memTotal))}</span>
      </div>
    </div>`;
}

/** 筛选：状态 / 分组 / 关键词。三种视图共用同一份结果，规则只写一遍。 */
function applyFilter(nodes) {
  const { q, status, group } = state.filter;
  const kw = (q || '').trim().toLowerCase();
  return nodes.filter((n) => {
    if (status === 'online' && n.state !== 'online') return false;
    if (status === 'abnormal' && n.state === 'online') return false;
    if (group !== 'all' && (n.group || '未分组') !== group) return false;
    if (kw) {
      const hay = `${n.name} ${n.region} ${n.os} ${n.provider} ${n.group}`.toLowerCase();
      if (!hay.includes(kw)) return false;
    }
    return true;
  });
}

/** 筛选条：搜索 + 状态 + 分组；右侧明确"看见几台 / 共几台"。 */
function renderFilterBar(snap) {
  const groups = [...new Set(snap.nodes.map((n) => n.group || '未分组'))];
  const f = state.filter;
  const shown = applyFilter(snap.nodes).length;
  const active = shown !== snap.nodes.length;
  return `<div class="filters">
    <input class="input" id="qInput" type="search" placeholder="搜索主机名 / 地区 / 系统"
           value="${esc(f.q)}" aria-label="搜索节点" autocomplete="off">
    <select class="select" id="statusSel" aria-label="按状态筛选">
      <option value="all"${f.status === 'all' ? ' selected' : ''}>全部状态</option>
      <option value="online"${f.status === 'online' ? ' selected' : ''}>仅在线</option>
      <option value="abnormal"${f.status === 'abnormal' ? ' selected' : ''}>仅异常</option>
    </select>
    <select class="select" id="groupSel" aria-label="按分组筛选">
      <option value="all">全部分组</option>
      ${groups.map((g) => `<option value="${esc(g)}"${f.group === g ? ' selected' : ''}>${esc(g)}</option>`).join('')}
    </select>
    ${active ? '<button class="btn" id="clearFilter">清空筛选</button>' : ''}
    <span class="filter-count">${active ? `显示 <b>${shown}</b> / ${snap.nodes.length} 台` : `共 <b>${snap.nodes.length}</b> 台`}</span>
  </div>`;
}

/** 最近事件：直接来自公开状态接口的 activity，不额外发请求。 */
function renderActivity(snap) {
  const acts = Array.isArray(snap.activity) ? snap.activity : [];
  if (!acts.length) return '';
  const dot = (lv) => (lv === 'error' ? 'var(--crit)' : lv === 'warn' ? 'var(--warn)' : 'var(--ok)');
  const rows = acts.slice(0, 6).map((a) => `<div class="activity-row">
      <span class="activity-dot" style="background:${dot(a.level)}"></span>
      <span class="activity-text" title="${esc(a.text)}">${esc(a.text)}</span>
      <span class="activity-ts">${esc(fmtAgo(Number(a.ts || 0) * 1000))}</span>
    </div>`).join('');
  return `<div class="group-head" style="margin-top:22px">
      <h2><span class="hash">#</span> 最近事件</h2>
      <span class="meta">显示最近 ${Math.min(acts.length, 6)} 条 / 共 ${acts.length} 条</span>
    </div>
    <div class="activity">${rows}</div>`;
}

/* ---------- 主题 ---------- */
const ICON_SUN = '<circle cx="12" cy="12" r="4"/><path d="M12 3v2M12 19v2M3 12h2M19 12h2M5.6 5.6l1.4 1.4M17 17l1.4 1.4M18.4 5.6L17 7M7 17l-1.4 1.4" stroke-linecap="round"/>';
const ICON_MOON = '<path d="M20.5 14.3A8.5 8.5 0 0 1 9.7 3.5a8.5 8.5 0 1 0 10.8 10.8z" stroke-linecap="round" stroke-linejoin="round"/>';

/** 应用主题。auto 跟随系统，light/dark 为手动覆盖。 */
function applyTheme(mode, persist) {
  const order = ['auto', 'light', 'dark'];
  const t = order.includes(mode) ? mode : 'auto';
  state.theme = t;
  const sysDark = !!(window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
  const dark = t === 'dark' || (t === 'auto' && sysDark);
  document.documentElement.dataset.theme = dark ? 'dark' : 'light';
  const icon = document.getElementById('themeIcon');
  if (icon) icon.innerHTML = dark ? ICON_SUN : ICON_MOON;
  const btn = document.getElementById('themeBtn');
  if (btn) {
    const label = t === 'auto' ? '跟随系统' : t === 'dark' ? '深色' : '浅色';
    btn.title = `主题：${label}（点击切换）`;
    btn.setAttribute('aria-label', `主题：${label}，点击切换`);
  }
  if (persist) savePrefs();
}

/**
 * 主题按钮：在当前"可见外观"上取反。
 *
 * 不用 auto → light → dark 的三态循环：系统是浅色时 auto 与 light 视觉相同，
 * 用户点一次看不出任何变化，会以为按钮坏了。
 * 现在 auto 只作为「从未手动设置」的初始状态，点一次即切到相反外观并记住。
 */
function cycleTheme() {
  const dark = document.documentElement.dataset.theme === 'dark';
  applyTheme(dark ? 'light' : 'dark', true);
}

/* ---------- 实时推送 ----------
   /api/status/stream 的事件只带版本号（{rev}），因此收到 change 就重新取一次
   快照。保留轮询作为兜底：推送断了（跨网代理、CSP、服务端满载）仍能拿到数据。 */
let esConn = null;
let lastStreamAt = 0;

function connectStream() {
  if (esConn) {
    try { esConn.close(); } catch { /* 忽略 */ }
    esConn = null;
  }
  state.streamOK = false;
  if (state.source !== 'http' || !state.baseUrl) return;
  if (typeof EventSource === 'undefined') return;
  try {
    esConn = new EventSource(state.baseUrl.replace(/\/+$/, '') + '/api/status/stream');
  } catch {
    return;
  }
  esConn.addEventListener('hello', () => { state.streamOK = true; updateHeartbeat(); });
  esConn.addEventListener('change', () => {
    // 1.2 秒内合并：一批节点同时上报时服务端会连续推事件，
    // 每个事件都触发一次全量取数没有意义。
    const now = Date.now();
    if (now - lastStreamAt < 1200) return;
    lastStreamAt = now;
    runRefresh(false);
  });
  esConn.onerror = () => { state.streamOK = false; updateHeartbeat(); };
}

/** 重渲染但保住输入框焦点与光标位置 —— 否则每敲一个字搜索框就失焦。 */
function rerenderKeepFocus() {
  const el = document.activeElement;
  const id = el && el.id;
  const pos = el && typeof el.selectionStart === 'number' ? el.selectionStart : null;
  render();
  if (!id) return;
  const again = document.getElementById(id);
  if (!again) return;
  again.focus({ preventScroll: true });
  if (pos != null && typeof again.setSelectionRange === 'function') {
    try { again.setSelectionRange(pos, pos); } catch { /* 非文本输入忽略 */ }
  }
}

/** 按分组切分节点，保持 NODES 里的顺序（分组顺序即资产台账顺序）。 */
function groupNodes(nodes) {
  const order = [];
  const map = new Map();
  for (const n of nodes) {
    const g = n.group || '未分组';
    if (!map.has(g)) { map.set(g, []); order.push(g); }
    map.get(g).push(n);
  }
  return order.map((g) => ({ name: g, nodes: map.get(g) }));
}

/** 状态徽章：三种状态用不同颜色，且文案本身就是结论（在线/陈旧/离线）。 */
function stateBadge(n) {
  if (n.state === 'offline') return '<span class="card-badge badge-offline">离线</span>';
  if (n.state === 'stale') return '<span class="card-badge badge-stale">数据陈旧</span>';
  return '<span class="card-badge badge-online">在线</span>';
}

/** 与上一次快照对比，给变化的数值加一次脉冲高亮。metric 为空表示不参与比较。 */
function flashClass(nodeId, metric, cur) {
  if (!metric) return '';
  const prev = state.prev && state.prev.nodes.find((n) => n.id === nodeId);
  if (!prev || prev[metric] == null || cur == null) return '';
  const d = cur - prev[metric];
  if (Math.abs(d) < 0.5) return '';
  return d > 0 ? 'flash-up' : 'flash-down';
}

/* ---------- 视图 1：条形图 ---------- */
function renderBars(nodes) {
  return `<div class="grid">${nodes.map((n) => {
    const rows = [
      { k: 'CPU', metric: 'cpu', v: n.cpu },
      { k: '内存', metric: 'mem', v: n.mem },
      { k: '磁盘', metric: 'disk', v: n.disk },
    ];
    // 负载：条宽是 load1 ÷ 核数 归一化后的百分比，右侧数字给原始 load1。
    // 两者单位不同，所以标题里写明换算关系，避免"条很短但数字不为 0"的困惑。
    const loadPct = n.cores ? clamp((n.load1 / n.cores) * 100, 0, 100) : null;
    if (loadPct != null) {
      rows.push({
        k: '负载', metric: '', v: loadPct,
        raw: n.load1.toFixed(2),
        hint: `负载 ${n.load1.toFixed(2)} ÷ ${n.cores} 核 = ${loadPct.toFixed(0)}%`,
      });
    }

    return `<article class="card ${n.state === 'offline' ? 'is-offline' : ''}" data-node="${esc(n.id)}" tabindex="0" role="button"
              aria-label="${esc(n.name)} 详情">
      <div class="card-head">
        ${n.region ? `<span class="card-flag" title="${esc(n.region)} 地区">${esc(n.region)}</span>` : ''}
        <span class="card-title">${esc(n.name)}</span>
        ${stateBadge(n)}
      </div>
      <div class="bar-rows">
        ${rows.map((r) => `<div class="bar-row"${r.hint ? ` title="${esc(r.hint)}"` : ''}>
          <span class="bar-label">${r.k}</span>
          <span class="bar-track"><i class="bar-fill" style="width:${(r.v == null ? 0 : clamp(r.v, 0, 100)).toFixed(1)}%;background:${levelColor(r.v)}"></i></span>
          <span class="bar-val num ${flashClass(n.id, r.metric, r.v)}">${r.raw != null ? r.raw : (r.v == null ? '—' : pct1(r.v))}</span>
        </div>`).join('')}
      </div>
      <div class="card-foot">
        <span class="kv"><span class="down">${esc(fmtRate(n.rxRate))}</span></span>
        <span class="kv"><span class="up">${esc(fmtRate(n.txRate))}</span></span>
        <span class="dot-sep">·</span>
        <span class="kv"><b>${esc(fmtDuration(n.uptimeSec))}</b></span>
        <span class="dot-sep">·</span>
        <span>${esc(fmtAgo(n.lastSeen))}</span>
      </div>
    </article>`;
  }).join('')}</div>`;
}

/* ---------- 视图 2：环形图 ---------- */
function renderRings(nodes) {
  return `<div class="grid">${nodes.map((n) => {
    const rings = [
      { k: 'CPU', v: n.cpu, spec: n.cores ? n.cores + ' 核' : '未上报' },
      { k: '内存', v: n.mem, spec: fmtPair(n.memUsed, n.memTotal) },
      { k: '磁盘', v: n.disk, spec: fmtPair(n.diskUsed, n.diskTotal) },
    ];
    return `<article class="card ${n.state === 'offline' ? 'is-offline' : ''}" data-node="${esc(n.id)}" tabindex="0" role="button"
              aria-label="${esc(n.name)} 详情">
      <div class="card-head">
        ${n.region ? `<span class="card-flag" title="${esc(n.region)} 地区">${esc(n.region)}</span>` : ''}
        <span class="card-title">${esc(n.name)}</span>
        ${stateBadge(n)}
      </div>
      <div class="rings">
        ${rings.map((r) => `<div class="ring-cell">
          ${donutSVG(r.v, { label: r.k, dim: n.state === 'offline' })}
          <div class="ring-k">${r.k}</div>
          <div class="ring-spec num">${esc(r.spec)}</div>
        </div>`).join('')}
      </div>
      <div class="card-foot">
        <span class="kv">负载 <b class="num">${n.load1 == null ? '—' : n.load1.toFixed(2)}</b></span>
        <span class="kv"><span class="down">${esc(fmtRate(n.rxRate))}</span></span>
        <span class="kv"><span class="up">${esc(fmtRate(n.txRate))}</span></span>
        <span style="margin-left:auto">${esc(fmtAgo(n.lastSeen))}</span>
      </div>
    </article>`;
  }).join('')}</div>`;
}

/* ---------- 视图 3：列表 ---------- */
const LIST_COLS = [
  { key: 'name', label: '主机名', cls: 'cell-host', sortable: true },
  { key: 'region', label: '地区', sortable: true, hideSm: true },
  { key: 'os', label: '系统 / 架构', sortable: true, hideSm: true },
  { key: 'cpu', label: 'CPU', n: true, sortable: true },
  { key: 'mem', label: '内存', n: true, sortable: true },
  { key: 'disk', label: '磁盘', n: true, sortable: true },
  { key: 'load1', label: '负载 1/5/15', n: true, sortable: true },
  { key: 'rxRate', label: '入站', n: true, sortable: true },
  { key: 'txRate', label: '出站', n: true, sortable: true },
  { key: 'uptimeSec', label: '运行时长', n: true, sortable: true, hideSm: true },
  { key: 'lastSeen', label: '最后上报', n: true, sortable: true },
];

function sortNodes(nodes) {
  const { key, dir } = state.sort;
  const val = (n) => {
    if (key === 'lastSeen') return n.lastSeen;
    if (key === 'name') return n.name;
    if (key === 'os') return n.os;
    if (key === 'region') return n.region;
    return n[key];
  };
  return nodes.slice().sort((a, b) => {
    const va = val(a), vb = val(b);
    if (typeof va === 'string' || typeof vb === 'string') {
      return String(va).localeCompare(String(vb), 'zh-CN') * dir;
    }
    const na = va == null ? -Infinity : va;
    const nb = vb == null ? -Infinity : vb;
    return (na - nb) * dir;
  });
}

function miniBar(pct) {
  if (pct == null) return '<span style="color:var(--fg-3)">—</span>';
  const w = clamp(pct, 0, 100);
  return `<span class="mini-bar"><i style="width:${w.toFixed(1)}%;background:${levelColor(pct)}"></i></span>`;
}

function renderList(nodes) {
  const rows = sortNodes(nodes);
  return `<div class="table-wrap">
    <table>
      <thead><tr>
        ${LIST_COLS.map((c) => {
          const active = state.sort.key === c.key;
          const aria = active ? ` aria-sort="${state.sort.dir > 0 ? 'ascending' : 'descending'}"` : '';
          // 箭头必须体现方向：恒用 ↕ 时用户看不出当前是升序还是降序
          const arrow = active ? (state.sort.dir > 0 ? '↑' : '↓') : '↕';
          const cls = [c.n ? 'n' : '', c.sortable ? 'sortable' : '', c.hideSm ? 'hide-sm' : ''].filter(Boolean).join(' ');
          return `<th class="${cls}"${c.sortable ? ` data-sort="${c.key}"` : ''}${aria}
            ${c.sortable ? 'tabindex="0" role="button"' : ''}>${c.label}${c.sortable ? `<span class="arrow">${arrow}</span>` : ''}</th>`;
        }).join('')}
      </tr></thead>
      <tbody>
        ${rows.map((n) => `<tr data-node="${esc(n.id)}" aria-label="${esc(n.name)} 详情">
          <td class="cell-host">
            <span class="status-dot ${n.state === 'offline' ? 'off' : n.state === 'stale' ? 'stale' : ''}"></span>
            <span class="host-name">${esc(n.name)}</span>
            <span class="host-group">${esc(n.group)}</span>
          </td>
          <td class="hide-sm">${n.region ? `<span class="card-flag" style="display:inline-block">${esc(n.region)}</span>` : '<span style="color:var(--fg-3)">—</span>'}
          <td class="hide-sm">${esc(n.os)} <span style="color:var(--fg-3)">/ ${esc(n.arch)}</span></td>
          <td class="n">${miniBar(n.cpu)}<span class="num">${n.cpu == null ? '—' : pct1(n.cpu)}</span></td>
          <td class="n">${miniBar(n.mem)}<span class="num">${n.mem == null ? '—' : pct1(n.mem)}</span></td>
          <td class="n">${miniBar(n.disk)}<span class="num">${n.disk == null ? '—' : pct1(n.disk)}</span></td>
          <td class="n num" style="font-size:11.5px;color:var(--fg-2)">${n.load1 == null ? '—' : `${n.load1.toFixed(2)} ${n.load5 == null ? '' : n.load5.toFixed(2)} ${n.load15 == null ? '' : n.load15.toFixed(2)}`}</td>
          <td class="n num">${esc(fmtRate(n.rxRate))}</td>
          <td class="n num">${esc(fmtRate(n.txRate))}</td>
          <td class="n num hide-sm">${esc(fmtDuration(n.uptimeSec))}</td>
          <td class="n" style="color:var(--fg-3)">${esc(fmtAgo(n.lastSeen))}</td>
        </tr>`).join('')}
      </tbody>
    </table>
  </div>`;
}

/* ============================================================
   七、详情页
   ============================================================ */

let detailToken = 0; // 防止慢请求覆盖新页面（切节点/切区间时常见）

function infoCell(k, v, sub) {
  return `<div class="info-cell"><div class="info-k">${k}</div>
    <div class="info-v" title="${esc(v)}">${esc(v)}${sub ? ` <span class="sub">${sub}</span>` : ''}</div></div>`;
}

function renderDetail(node, rangeKey) {
  const n = node;
  const range = RANGES[rangeKey] || RANGES['24h'];
  const online = n.state === 'online';

  document.getElementById('stats').style.display = 'none';
  const view = document.getElementById('view');

  view.innerHTML = `
    <div class="back-row">
      <button class="btn" data-back>
        <svg class="ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
          <path d="M15 18l-6-6 6-6" stroke-linecap="round" stroke-linejoin="round"/>
        </svg>
        返回
      </button>
      <div class="detail-title">
        ${n.region ? `<span class="card-flag">${esc(n.region)}</span>` : ''}${esc(n.name)}
        ${stateBadge(n)}
      </div>
      <div style="margin-left:auto;font-size:12px;color:var(--fg-3)">
        ${esc(n.group)} · ${esc(n.provider)}
      </div>
    </div>

    <div class="info-grid">
      ${[
        infoCell('运行时长', n.uptimeSec == null ? '未公开' : fmtDuration(n.uptimeSec)),
        (n.os || n.arch) ? infoCell('操作系统 / 架构', [n.os, n.arch].filter(Boolean).join(' / ')) : '',
        n.kernel ? infoCell('内核版本', n.kernel) : '',
        n.cpuModel ? infoCell('CPU 型号', n.cpuModel, n.cores ? `× ${n.cores}` : '') : '',
        infoCell('内存', `${fmtBytes(n.memUsed)} / ${fmtBytes(n.memTotal)}`, n.mem == null ? '' : pct1(n.mem)),
        infoCell('磁盘', `${fmtBytes(n.diskUsed)} / ${fmtBytes(n.diskTotal)}`, n.disk == null ? '' : pct1(n.disk)),
        infoCell('负载 1 / 5 / 15', n.load1 == null ? '未公开'
          : `${n.load1.toFixed(2)} / ${(n.load5 ?? 0).toFixed(2)} / ${(n.load15 ?? 0).toFixed(2)}`),
        infoCell('累计流量', `${fmtBytes(n.totalRx)} / ${fmtBytes(n.totalTx)}`, '↓ / ↑'),
        infoCell('实时速率', `${fmtRate(n.rxRate)} / ${fmtRate(n.txRate)}`, '↓ / ↑'),
        n.region ? infoCell('地区', n.region) : '',
        infoCell('最后上报', fmtAgo(n.lastSeen), online ? '' : '（非在线）'),
        infoCell('状态', online ? '在线' : n.state === 'stale' ? '数据陈旧' : '离线'),
      ].filter(Boolean).join('')}
    </div>

    <div class="range-row">
      <span class="range-label">时间范围</span>
      <div class="range-tabs" role="group" aria-label="时间范围">
        ${Object.entries(RANGES).map(([k, r]) =>
          `<button data-range="${k}" aria-pressed="${k === rangeKey}">${r.label}</button>`).join('')}
      </div>
      <span class="range-label nowrap" id="rangeMeta">${range.points} 个采样点 · 间隔 ${fmtStep(range.stepSec)}</span>
    </div>

    <div class="charts" id="charts">
      ${[1, 2, 3, 4].map(() => '<div class="chart-card"><div class="skel" style="height:180px"></div></div>').join('')}
    </div>`;

  mountDetailCharts(n, rangeKey);
}

/** 采样间隔：真实数据的间隔往往不是整分钟（6.017 分钟），必须收敛到 1 位小数。 */
function fmtStep(sec) {
  const v = Number(sec);
  if (!isFinite(v) || v <= 0) return '—';
  const trim = (x) => x.toFixed(1).replace(/\.0$/, '');
  if (v >= 3600) return trim(v / 3600) + ' 小时';
  if (v >= 60) return trim(v / 60) + ' 分钟';
  return Math.round(v) + ' 秒';
}

async function mountDetailCharts(node, rangeKey) {
  const token = ++detailToken;
  const box = document.getElementById('charts');
  if (!box) return;

  const cols = await loadSeries(node.id, rangeKey);
  if (token !== detailToken) return; // 已被更晚的操作取代
  if (!document.getElementById('charts')) return;

  const filled = (c) => Array.isArray(c) && c.length > 1;
  if (!cols || ![cols.cpu, cols.mem, cols.disk, cols.rx, cols.tx].some(filled)) {
    box.innerHTML = `<div class="empty" style="grid-column:1/-1">
      <strong>该数据源未提供历史序列</strong>
      实时接口读 <code>/api/status/node/{hid}?range=</code>；若公开页关闭了「节点详情」区块，
      该接口会返回 404，面板不会用当前值伪造曲线。可切回「数据源：模拟」查看图表形态。
    </div>`;
    return;
  }

  // 时间轴：取所有列的并集首尾，避免某列起点不同导致横轴错位
  const allT = [cols.cpu, cols.mem, cols.disk, cols.rx, cols.tx]
    .filter(filled)
    .flatMap((c) => [c[0].t, c[c.length - 1].t]);
  const tMin = Math.min(...allT);
  const tMax = Math.max(...allT);
  const spanSec = (tMax - tMin) / 1000;

  // 「N 个采样点 · 间隔」按实际返回的数据算，不照抄请求参数 ——
  // 服务端会按分辨率降采样，写死的数字经常和真实点数对不上。
  const metaEl = document.getElementById('rangeMeta');
  if (metaEl) {
    const cnt = (cols.cpu || cols.mem || cols.disk || []).length;
    const step = cnt > 1 ? Math.round(spanSec / (cnt - 1)) : 0;
    metaEl.textContent = `${cnt} 个采样点 · 实际间隔 ${fmtStep(step)}`;
  }

  const vals = (c) => (filled(c) ? c.map((p) => p.v).filter((v) => v != null && isFinite(v)) : []);
  const avg = (a) => (a.length ? a.reduce((x, y) => x + y, 0) / a.length : null);
  const maxOf = (a) => (a.length ? Math.max(...a) : null);
  const cpuVals = vals(cols.cpu);
  const memVals = vals(cols.mem);
  const diskVals = vals(cols.disk);
  const rxVals = vals(cols.rx);
  const txVals = vals(cols.tx);

  // 某列缺失时给明确原因，而不是画一张空图
  const missing = (title, why) => `<div class="chart-card">
      <div class="chart-head"><span class="chart-title">${title}</span></div>
      <div class="empty" style="padding:30px 14px;font-size:12.5px;border:0;background:transparent">${why}</div>
    </div>`;

  const netMax = Math.max(maxOf(rxVals) || 0, maxOf(txVals) || 0, 1);

  box.innerHTML = `
    ${filled(cols.cpu) ? `<div class="chart-card">
      <div class="chart-head">
        <span class="chart-title">CPU 使用率</span>
        <span class="chart-now mono">${node.cpu == null ? '—' : pct1(node.cpu)}</span>
      </div>
      <div class="chart-legend"><span><i style="background:var(--accent)"></i>CPU</span></div>
      ${lineChart({ series: [{ key: 'cpu', label: 'CPU', color: 'var(--accent)', points: cols.cpu }],
        yMax: 100, yFmt: (v) => v.toFixed(0) + '%', spanSec, aria: 'CPU 使用率趋势' })}
      <div class="chart-foot">
        均值 <b>${avg(cpuVals) == null ? '—' : pct1(avg(cpuVals))}</b>
        峰值 <b>${maxOf(cpuVals) == null ? '—' : pct1(maxOf(cpuVals))}</b>
        p95 <b>${percentile(cpuVals, 0.95) == null ? '—' : pct1(percentile(cpuVals, 0.95))}</b>
      </div>
    </div>` : missing('CPU 使用率', '该数据源未返回 CPU 序列')}

    ${filled(cols.mem) ? `<div class="chart-card">
      <div class="chart-head">
        <span class="chart-title">内存使用率</span>
        <span class="chart-now mono">${node.mem == null ? '—' : pct1(node.mem)}</span>
      </div>
      <div class="chart-legend"><span><i style="background:#7c3aed"></i>内存 <span class="mono">${esc(fmtBytes(node.memUsed))} / ${esc(fmtBytes(node.memTotal))}</span></span></div>
      ${lineChart({ series: [{ key: 'mem', label: '内存', color: '#7c3aed', points: cols.mem }],
        yMax: 100, yFmt: (v) => v.toFixed(0) + '%', spanSec, aria: '内存使用率趋势' })}
      <div class="chart-foot">
        均值 <b>${avg(memVals) == null ? '—' : pct1(avg(memVals))}</b>
        峰值 <b>${maxOf(memVals) == null ? '—' : pct1(maxOf(memVals))}</b>
      </div>
    </div>` : missing('内存使用率', '该数据源未返回内存序列')}

    ${filled(cols.disk) ? `<div class="chart-card">
      <div class="chart-head">
        <span class="chart-title">磁盘使用率</span>
        <span class="chart-now mono">${node.disk == null ? '—' : pct1(node.disk)}</span>
      </div>
      <div class="chart-legend"><span><i style="background:#0891b2"></i>磁盘 <span class="mono">${esc(fmtBytes(node.diskUsed))} / ${esc(fmtBytes(node.diskTotal))}</span></span></div>
      ${lineChart({ series: [{ key: 'disk', label: '磁盘', color: '#0891b2', points: cols.disk }],
        yMax: 100, yFmt: (v) => v.toFixed(0) + '%', spanSec, aria: '磁盘使用率趋势' })}
      <div class="chart-foot">磁盘变化缓慢，短区间内近乎平线属正常</div>
    </div>` : missing('磁盘使用率', '该数据源未返回磁盘序列')}

    ${(filled(cols.rx) || filled(cols.tx)) ? `<div class="chart-card">
      <div class="chart-head">
        <span class="chart-title">网络吞吐</span>
        <span class="chart-now mono" style="font-size:13px">${esc(fmtRate(node.rxRate))} / ${esc(fmtRate(node.txRate))}</span>
      </div>
      <div class="chart-legend">
        <span><i style="background:#0891b2"></i>入站 ↓</span>
        <span><i style="background:#c026d3"></i>出站 ↑</span>
      </div>
      ${lineChart({ series: [
          { key: 'rx', label: '入站 ↓', color: '#0891b2', area: false, points: cols.rx || [] },
          { key: 'tx', label: '出站 ↑', color: '#c026d3', area: false, points: cols.tx || [] },
        ], yMax: niceMax(netMax * 1.12), yFmt: (v) => fmtRate(v), spanSec, aria: '网络吞吐趋势' })}
      <div class="chart-foot">
        入站峰值 <b>${esc(fmtRate(maxOf(rxVals) || 0))}</b>
        出站峰值 <b>${esc(fmtRate(maxOf(txVals) || 0))}</b>
      </div>
    </div>` : missing('网络吞吐', '该数据源未返回网络序列')}

    ${filled(cols.load1) ? `<div class="chart-card" style="grid-column:1/-1">
      <div class="chart-head">
        <span class="chart-title">负载（1 分钟）</span>
        <span class="chart-now mono" style="font-size:13px">
          ${node.load1 == null ? '—' : `${node.load1.toFixed(2)} / ${(node.load5 ?? 0).toFixed(2)} / ${(node.load15 ?? 0).toFixed(2)}`}
          <span style="font-size:11px;color:var(--fg-3);font-weight:400">1/5/15 分钟</span>
        </span>
      </div>
      ${lineChart({ series: [{ key: 'load', label: '负载 1 分钟', color: '#b45309', points: cols.load1 }],
        yFmt: (v) => v.toFixed(1), spanSec, height: 150, aria: '系统负载趋势' })}
      <div class="chart-foot">
        核数 <b>${node.cores || '未上报'}</b>
        峰值 <b>${maxOf(vals(cols.load1)) == null ? '—' : maxOf(vals(cols.load1)).toFixed(2)}</b>
      </div>
    </div>` : `<div class="chart-card" style="grid-column:1/-1">
      <div class="chart-head"><span class="chart-title">负载</span>
        <span class="chart-now mono" style="font-size:13px">
          ${node.load1 == null ? '—' : `${node.load1.toFixed(2)} / ${(node.load5 ?? 0).toFixed(2)} / ${(node.load15 ?? 0).toFixed(2)}`}
          <span style="font-size:11px;color:var(--fg-3);font-weight:400">当前 1/5/15 分钟</span>
        </span>
      </div>
      <div class="empty" style="padding:20px 14px;font-size:12.5px;border:0;background:transparent">
        公开接口只提供负载的<b>当前值</b>，没有历史序列 —— 因此这里只显示数值，不给曲线
      </div>
    </div>`}`;

  bindChartHover(box);
}

/* ============================================================
   八、首页与路由
   ============================================================ */

function renderHome() {
  const snap = state.snap;
  document.getElementById('stats').style.display = '';
  const view = document.getElementById('view');
  if (!snap) return;

  renderStats(snap);
  const sub = document.getElementById('brandSub');
  if (sub) sub.textContent = `${snap.nodes.length} 台 · 只读聚合视图`;

  // 筛选在渲染前生效：三种视图共用同一份过滤结果，
  // 避免"卡片过滤了但列表没过滤"这类不一致。
  const nodes = applyFilter(snap.nodes);
  const groups = groupNodes(nodes);
  const body = !nodes.length
    ? `<div class="empty"><strong>没有匹配的节点</strong>当前筛选条件下没有节点，调整条件或点「清空筛选」。</div>`
    : state.view === 'list'
      ? renderList(nodes)
      : groups.map((g) => `
        <div class="group-head">
          <h2><span class="hash">#</span> ${esc(g.name)}</h2>
          <span class="meta">[${g.nodes.length}]</span>
          ${g.nodes.some((n) => n.state !== 'online')
            ? `<span class="meta offline-note">${g.nodes.filter((n) => n.state !== 'online').length} 台非在线</span>` : ''}
        </div>
        ${state.view === 'rings' ? renderRings(g.nodes) : renderBars(g.nodes)}`).join('');

  view.innerHTML = `
    <div class="toolbar">
      <div class="toolbar-title">
        节点总览 <span class="count">${groups.length} 个分组</span>
      </div>
      <div class="toolbar-right">
        <div class="seg" role="group" aria-label="视图切换">
          <button data-view="bars" aria-pressed="${state.view === 'bars'}" title="条形图：每台一张卡片，CPU/内存/磁盘 用进度条表示">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M4 6h16M4 12h11M4 18h7" stroke-linecap="round"/></svg>
            条形图
          </button>
          <button data-view="rings" aria-pressed="${state.view === 'rings'}" title="环形图：CPU/内存/磁盘 各一个 3/4 环">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><circle cx="12" cy="12" r="8"/><path d="M12 12V4" stroke-linecap="round"/></svg>
            环形图
          </button>
          <button data-view="list" aria-pressed="${state.view === 'list'}" title="列表：表格形式，可按列排序">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M4 6h16M4 10h16M4 14h16M4 18h16" stroke-linecap="round"/></svg>
            列表
          </button>
        </div>
      </div>
    </div>
    ${renderFilterBar(snap)}
    ${body}
    ${renderActivity(snap)}
    ${snap.partial ? `<div class="empty" style="margin-top:12px;text-align:left;padding:12px 14px">
      <strong style="display:inline;font-size:12.5px">部分字段未上报</strong>
      实时接口缺少部分指标（显示为「未上报」而非 0），这类缺失通常是 Agent 版本或采集能力不同导致的。
    </div>` : ''}`;
}

function parseRoute() {
  const raw = (location.hash || '').replace(/^#/, '');
  const [path, query] = raw.split('?');
  const q = new URLSearchParams(query || '');
  const m = /^\/server\/(.+)$/.exec(path || '');
  if (m) return { name: 'detail', id: decodeURIComponent(m[1]), range: q.get('range') || state.range };
  return { name: 'home' };
}

/** 当前详情页的结构签名（节点 + 区间）。用于抑制自动刷新引起的重建。 */
let detailSig = '';

/**
 * 渲染入口。
 *
 * forceDetail=true 时才允许重建详情页结构。自动刷新（轮询 / SSE）走 false ——
 * 否则每 30 秒详情页都会重建骨架、重新取一次 24h 序列、图表闪一下再画回来，
 * 而且多次渲染会互相抢 detailToken，最后一次可能因为 DOM 已被换掉而落不了地
 * （表现为"停在骨架屏"）。指标数值本身由心跳与卡片展示，详情页看的是趋势，
 * 不值得为了几秒的新鲜度反复重画。
 */
function render(forceDetail) {
  if (state.loading && !state.snap) return;
  const route = parseRoute();
  if (route.name === 'detail' && state.snap) {
    const node = state.snap.nodes.find((n) => n.id === route.id);
    if (!node) {
      document.getElementById('stats').style.display = '';
      document.getElementById('view').innerHTML =
        `<div class="empty" style="margin-top:18px"><strong>节点不存在</strong>该节点可能已被删除，或数据源已切换。</div>`;
      detailSig = '';
      return;
    }
    state.range = route.range;
    const sig = route.id + '|' + route.range;
    if (!forceDetail && sig === detailSig && document.getElementById('charts')) return;
    detailSig = sig;
    renderDetail(node, route.range);
  } else {
    detailSig = '';
    renderHome();
  }
}

/* ============================================================
   九、刷新调度与反馈
   ============================================================ */

const els = {
  hb: document.getElementById('heartbeat'),
  hbText: document.getElementById('hbText'),
  refreshBtn: document.getElementById('refreshBtn'),
  view: document.getElementById('view'),
  stats: document.getElementById('stats'),
  toastHost: document.getElementById('toastHost'),
};

let hbTimer = null;
let nextAt = 0;

function toast(msg, kind) {
  const el = document.createElement('div');
  el.className = 'toast' + (kind === 'err' ? ' err' : '');
  el.setAttribute('role', 'status');
  const ico = kind === 'err'
    ? '<svg class="t-ico" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="9"/><path d="M12 8v5M12 16.5v.01" stroke-linecap="round"/></svg>'
    : '<svg class="t-ico" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M20 6L9 17l-5-5" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  el.innerHTML = `${ico}<span>${esc(msg)}</span>`;
  els.toastHost.appendChild(el);
  setTimeout(() => {
    el.style.transition = 'opacity .2s, transform .2s';
    el.style.opacity = '0';
    el.style.transform = 'translateY(4px)';
    setTimeout(() => el.remove(), 220);
  }, 3600);
}

function updateHeartbeat() {
  if (!els.hbText) return;
  const snap = state.snap;
  els.hb.className = 'heartbeat';
  if (state.lastError) {
    els.hb.classList.add('error');
    const wait = state.interval ? Math.max(0, Math.round((nextAt - Date.now()) / 1000)) : null;
    els.hbText.innerHTML = `<strong>刷新失败</strong> 第 ${state.failures} 次`
      + (wait != null ? ` · ${wait}s 后重试` : ' · 自动刷新已关闭');
    return;
  }
  if (!snap) { els.hbText.textContent = '初始化…'; return; }
  const age = Math.round((Date.now() - snap.at) / 1000);
  // 超过 2 倍刷新间隔就提示"陈旧"，让"没在动"这件事可见
  if (state.interval && age * 1000 > state.interval * 2.2) els.hb.classList.add('stale');
  const next = state.interval ? Math.max(0, Math.round((nextAt - Date.now()) / 1000)) : null;
  const src = snap.source === 'http' ? '实时接口' : '模拟数据';
  // 推送连接状态只在实时接口下有意义；模拟数据没有流可连。
  const push = snap.source === 'http' && state.streamOK ? ' · 推送已连接' : '';
  els.hbText.innerHTML = `<strong>${fmtAgo(snap.at)}</strong> · ${esc(src)}${push}`
    + (next != null ? ` · ${next}s 后刷新` : ' · 自动刷新已关闭');
}

function setBusy(on) {
  if (!els.refreshBtn) return;
  els.refreshBtn.classList.toggle('spinning', on);
  els.refreshBtn.disabled = on;
  els.refreshBtn.setAttribute('aria-busy', on ? 'true' : 'false');
}

/** 失败退避：间隔 × 2^(n-1)，上限 5 分钟。避免面板挂掉时把浏览器打满。 */
function backoffDelay() {
  if (!state.failures) return state.interval;
  return Math.min(state.interval * 2 ** (state.failures - 1), 300000);
}

function scheduleNext() {
  if (state.timer) { clearTimeout(state.timer); state.timer = null; }
  if (!state.interval) { nextAt = 0; updateHeartbeat(); return; }
  const delay = backoffDelay();
  nextAt = Date.now() + delay;
  state.timer = setTimeout(() => runRefresh(false), delay);
  updateHeartbeat();
}

async function runRefresh(manual) {
  if (state.inFlight) return;
  state.inFlight = true;
  setBusy(true);
  try {
    const snap = await loadSnapshot();
    state.prev = state.snap;
    state.snap = snap;
    state.failures = 0;
    state.lastError = '';
    state.loading = false;
    // 手动刷新强制重建详情页；自动刷新只更新首页（详情页保持当前图表）
    render(!!manual);
    if (manual) toast(`已刷新 · ${snap.nodes.length} 台节点`);
  } catch (err) {
    state.failures += 1;
    state.lastError = err.message || String(err);
    state.loading = false;
    if (!state.snap) {
      // 首次就失败：给一个可操作的错误页，而不是空白
      document.getElementById('view').innerHTML = `<div class="empty" style="margin-top:18px">
        <strong>取数失败</strong>
        ${esc(state.lastError)}<br><br>
        实时接口需要与面板同源部署（浏览器 CORS 限制），或用 <code>?api=https://你的面板地址</code> 指定。
        也可以把数据源切回「模拟」查看界面效果。
      </div>`;
    }
    if (manual || state.failures === 1) {
      const wait = state.interval ? Math.round(backoffDelay() / 1000) + ' 秒后自动重试' : '自动刷新已关闭';
      toast(`刷新失败：${state.lastError}（${wait}）`, 'err');
    }
  } finally {
    state.inFlight = false;
    setBusy(false);
    scheduleNext();
  }
}

/* 心跳时钟：每秒只更新心跳文本，不重绘视图（避免整页闪烁） */
function startHeartbeatClock() {
  if (hbTimer) clearInterval(hbTimer);
  hbTimer = setInterval(updateHeartbeat, 1000);
}

/* ============================================================
   十、说明区（数据契约写在界面上）
   ============================================================ */

function renderNotes() {
  const body = document.getElementById('notesBody');
  if (!body) return;
  const isHttp = state.source === 'http';
  body.innerHTML = `
    <h4>三种视图各展示什么</h4>
    <ul>
      <li><b>条形图</b>：每台服务器一张卡片，CPU / 内存 / 磁盘 各一条进度条（宽度 = 使用率，颜色在 ≥60% 转琥珀、≥80% 转红），
        第四条「负载」是 <code>load1 ÷ 核数</code> 归一化后的百分比，便于跨核数比较；卡片底部是实时速率、运行时长与最后上报时间。</li>
      <li><b>环形图</b>：同一组指标画成 3/4 圆环，中心是百分比；环下方标注容量规格（核数 / 已用÷总量）。</li>
      <li><b>列表</b>：表格形式，一屏可比 13 台的绝对值；水位列内嵌迷你条，表头可点击排序（再点切换升降序）。</li>
      <li><b>内页详情</b>：点任意卡片/行进入 <code>#/server/&lt;id&gt;</code>，含 12 项资产信息、时间范围切换（24 小时 / 7 天 / 30 天）
        与 5 张趋势图（CPU / 内存 / 磁盘 / 网络 / 负载），图上悬停可读取该时刻的全部序列值。</li>
      <li><b>筛选与最近事件</b>：搜索（主机名/地区/系统）、状态、分组三种筛选对<b>所有视图共用</b>，
        切换视图不丢条件；页面底部「最近事件」直接取自公开状态接口的 <code>activity</code> 数组，
        按 warn / error / info 着色，不额外发请求。</li>
    </ul>

    <h4>数据来源</h4>
    <table>
      <tr><th>字段</th><th>含义</th><th>${isHttp ? '实时接口取值' : '模拟数据取值'}</th></tr>
      <tr><td><code>cpu</code> / <code>mem</code> / <code>disk</code></td><td>使用率（%）</td><td>${isHttp ? '<code>nodes[].cpu</code> / <code>mem</code> / <code>disk</code>' : '随机游走 + 日周期 + 4% 概率尖峰'}</td></tr>
      <tr><td><code>load1/5/15</code></td><td>系统负载均值</td><td>${isHttp ? '<code>nodes[].load1</code> 等' : '由基线负载派生'}</td></tr>
      <tr><td><code>memUsed/Total</code>、<code>diskUsed/Total</code></td><td>容量（字节）</td><td>${isHttp ? '<code>nodes[].memUsed/memTotal</code> 等' : '节点台账 × 使用率'}</td></tr>
      <tr><td><code>rxRate/txRate</code></td><td>实时速率（字节/秒）</td><td>${isHttp ? '<code>nodes[].netRx/netTx</code>，按 bytes/s 解释' : '突发型随机游走'}</td></tr>
      <tr><td><code>totalRx/totalTx</code></td><td>累计流量（字节）</td><td>${isHttp ? '<code>nodes[].totalRx/totalTx</code>' : '按基线速率推算'}</td></tr>
      <tr><td><code>uptimeSec</code>、<code>lastSeen</code></td><td>运行时长、最后上报</td><td>${isHttp ? '<code>nodes[].uptime</code>、<code>lastSeen</code>（秒）' : '启动时刻到当前时间'}</td></tr>
      <tr><td>历史曲线</td><td>详情页趋势图</td><td>${isHttp ? '<code>GET /api/status/node/{hid}?range=</code>（公开详情接口，支持 24h/7d/30d）；不可用时显示「未提供历史」，不伪造' : '按区间粒度确定性生成'}</td></tr>
    </table>
    <p style="margin:8px 0 0">
      当前数据源：<b>${isHttp ? '实时接口（' + esc(state.baseUrl || '未配置') + '）' : '内置模拟数据'}</b>。
      实时接口读取 <code>GET /api/status</code>（Rollcall 公开状态接口，未鉴权）。
      浏览器跨域限制要求面板与该接口<b>同源部署</b>，或用 <code>?api=</code> 参数指定地址；
      若返回缺字段，面板显示「未上报」而<b>不补零</b> —— 0% 和"没有数据"是两件事。
    </p>

    <h4>列表字段</h4>
    <table>
      <tr><th>列</th><th>说明</th></tr>
      <tr><td>状态点 + 主机名 + 分组</td><td>绿=在线、琥珀=数据陈旧、红=离线；分组来自资产台账</td></tr>
      <tr><td>地区</td><td>国旗 + 地区代码</td></tr>
      <tr><td>系统 / 架构</td><td>发行版与 CPU 架构（异构机群里 <code>aarch64</code> 与 <code>x86_64</code> 要分开看）</td></tr>
      <tr><td>CPU / 内存 / 磁盘</td><td>迷你条 + 百分比，颜色按水位分级</td></tr>
      <tr><td>负载 1/5/15</td><td>三个时间窗的均值，单位是"可运行进程数"，需结合核数判断</td></tr>
      <tr><td>入站 / 出站</td><td>实时速率（不是累计），用于看当下是否在跑流量</td></tr>
      <tr><td>运行时长</td><td>自上次启动，用于发现意外重启</td></tr>
      <tr><td>最后上报</td><td>距今多久，<b>比"在线"徽章更可信</b>：徽章是状态，这个是事实</td></tr>
    </table>

    <h4>数据更新的触发方式</h4>
    <ul>
      <li><b>实时推送</b>：连上 <code>/api/status/stream</code>（SSE）后，服务端每次变更只推一个版本号，
        客户端收到就重取一次快照（1.2 秒内合并，避免一批节点同时上报导致连环重绘）。
        心跳区显示「推送已连接」。</li>
      <li><b>自动刷新</b>：间隔可选 10 / 30 / 60 秒或关闭，默认 30 秒；只重绘数据区，不重载页面。
        推送不可用时它就是唯一的数据来源 —— 两者并存而不是二选一。</li>
      <li><b>手动刷新</b>：点右上角「刷新」或按 <kbd>R</kbd>；刷新期间按钮图标旋转并禁用，防止重复请求。</li>
      <li><b>页面可见性</b>：切到后台标签页时暂停定时器，回到前台立即刷新一次 —— 避免后台空转，也避免回来后看到过期数据。</li>
      <li><b>失败退避</b>：连续失败时间隔翻倍（30s → 60s → 120s …，上限 5 分钟），成功一次即复位。</li>
      <li><b>切换数据源</b>：立即重新取数、重连推送并刷新说明区。</li>
    </ul>

    <h4>交互反馈</h4>
    <ul>
      <li><b>首屏</b>：卡片骨架屏，尺寸与真实卡片一致，避免布局跳动。</li>
      <li><b>刷新中</b>：刷新按钮旋转 + 禁用；心跳区显示"X 秒后刷新"的倒计时。</li>
      <li><b>刷新成功</b>：心跳时间戳更新（等宽数字，列宽不抖）；变动的数值有一次淡黄/淡绿脉冲（升/降）。</li>
      <li><b>刷新失败</b>：心跳圆点转红并显示失败次数与重试倒计时，同时弹出一条错误提示（同类错误只在第 1 次与手动触发时提示，不刷屏）。</li>
      <li><b>数据陈旧</b>：超过 2 倍刷新间隔未更新，心跳转为琥珀色；单台节点超过阈值则卡片打「数据陈旧」徽章。</li>
      <li><b>筛选</b>：搜索框 180ms 防抖，重绘后自动恢复焦点与光标位置；状态/分组下拉即时生效；右侧显示「显示 N / 共 M 台」。</li>
      <li><b>视图与排序</b>：选择记忆在 localStorage，下次打开保持；表头箭头随升/降序变化；图表悬停显示该时刻各序列读数。</li>
      <li><b>主题</b>：右上角按钮在「跟随系统 → 浅色 → 深色」间循环，选择同样记忆；<code>auto</code> 时系统切换会实时跟随。</li>
      <li><b>键盘</b>：<kbd>R</kbd> 刷新、<kbd>Esc</kbd> 从详情返回、表头可 Tab 聚焦后回车排序。</li>
    </ul>`;

  // kbd 样式内联补齐（说明区专用，避免污染主样式表）
  body.querySelectorAll('kbd').forEach((k) => {
    k.style.cssText = 'font-family:var(--mono);font-size:11px;background:#f4f6f9;border:1px solid var(--line-3);border-bottom-width:2px;border-radius:4px;padding:0 4px;';
  });
}

/* ============================================================
   十一、事件绑定与初始化
   ============================================================ */

function savePrefs() {
  try {
    localStorage.setItem(STORE_KEY, JSON.stringify({
      view: state.view, source: state.source, interval: state.interval,
      range: state.range, theme: state.theme,
    }));
  } catch { /* 隐私模式下 localStorage 可能不可写，忽略 */ }
}

function loadPrefs() {
  // 默认数据源：同源优先。部署在 Rollcall 上时 /api/status 就在同源，
  // 直接用真实数据（也绕开浏览器跨域限制）；用 file:// 打开时没有同源接口，
  // 自动退回内置模拟数据，保证界面仍可查看。
  if (/^https?:$/.test(location.protocol)) {
    state.source = 'http';
    state.baseUrl = location.origin;
  }

  try {
    const raw = localStorage.getItem(STORE_KEY);
    if (raw) {
      const p = JSON.parse(raw);
      if (p.view && ['bars', 'rings', 'list'].includes(p.view)) state.view = p.view;
      if (p.source === 'http' || p.source === 'mock') state.source = p.source;
      if ([0, 10000, 30000, 60000].includes(p.interval)) state.interval = p.interval;
      if (p.range && RANGES[p.range]) state.range = p.range;
      if (p.theme && ['auto', 'light', 'dark'].includes(p.theme)) state.theme = p.theme;
    }
  } catch { /* 坏数据或隐私模式不可读，忽略 */ }

  // 数据源地址：URL 参数 ?api= 优先级最高（便于临时指向另一个面板实例）
  const q = new URLSearchParams((location.hash.split('?')[1] || '') + '&' + (location.search.slice(1) || ''));
  const api = q.get('api');
  if (api) {
    state.baseUrl = api;
    state.source = 'http';
  }
}

function setView(v) {
  if (state.view === v) return;
  state.view = v;
  savePrefs();
  render();
  // 切换后把焦点留在当前按钮上，键盘用户不会丢失位置
  const btn = document.querySelector(`[data-view="${v}"]`);
  if (btn) btn.focus({ preventScroll: true });
}

function bindEvents() {
  let qTimer = null;

  // 视图切换 + 通用点击委托
  els.view.addEventListener('click', (ev) => {
    const viewBtn = ev.target.closest('[data-view]');
    if (viewBtn) { setView(viewBtn.dataset.view); return; }

    if (ev.target.closest('#clearFilter')) {
      state.filter = { q: '', status: 'all', group: 'all' };
      render();
      return;
    }

    const back = ev.target.closest('[data-back]');
    if (back) { location.hash = '#/'; return; }

    const rangeBtn = ev.target.closest('[data-range]');
    if (rangeBtn) {
      const route = parseRoute();
      if (route.name === 'detail') {
        location.hash = `#/server/${encodeURIComponent(route.id)}?range=${rangeBtn.dataset.range}`;
      }
      return;
    }

    const sortTh = ev.target.closest('[data-sort]');
    if (sortTh) {
      const key = sortTh.dataset.sort;
      if (state.sort.key === key) state.sort.dir *= -1;
      else state.sort = { key, dir: key === 'name' || key === 'os' || key === 'region' ? 1 : -1 };
      render();
      return;
    }

    const nodeEl = ev.target.closest('[data-node]');
    if (nodeEl) {
      location.hash = `#/server/${encodeURIComponent(nodeEl.dataset.node)}`;
    }
  });

  // 卡片与行的键盘可达性
  els.view.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Enter' && ev.key !== ' ') return;
    const sortTh = ev.target.closest('[data-sort]');
    if (sortTh) { ev.preventDefault(); sortTh.click(); return; }
    const nodeEl = ev.target.closest('[data-node]');
    if (nodeEl) { ev.preventDefault(); nodeEl.click(); }
  });

  // 筛选：输入防抖，下拉即时生效
  els.view.addEventListener('input', (ev) => {
    if (ev.target.id !== 'qInput') return;
    const v = ev.target.value;
    clearTimeout(qTimer);
    qTimer = setTimeout(() => {
      state.filter.q = v;
      rerenderKeepFocus();
    }, 180);
  });
  els.view.addEventListener('change', (ev) => {
    if (ev.target.id === 'statusSel') { state.filter.status = ev.target.value; render(); }
    if (ev.target.id === 'groupSel') { state.filter.group = ev.target.value; render(); }
  });

  document.getElementById('refreshBtn').addEventListener('click', () => runRefresh(true));

  document.getElementById('themeBtn').addEventListener('click', cycleTheme);

  // 系统主题变化时，auto 模式要跟着变（用户没手动指定就别锁死）
  if (window.matchMedia) {
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const onChange = () => { if (state.theme === 'auto') applyTheme('auto', false); };
    if (mq.addEventListener) mq.addEventListener('change', onChange);
    else if (mq.addListener) mq.addListener(onChange);
  }

  document.getElementById('sourceSel').addEventListener('change', (ev) => {
    state.source = ev.target.value;
    if (state.source === 'http' && !state.baseUrl) {
      state.baseUrl = location.protocol.startsWith('http') ? location.origin : '';
    }
    savePrefs();
    renderNotes();
    state.failures = 0;
    state.lastError = '';
    toast(state.source === 'http'
      ? `已切到实时接口：${state.baseUrl || '未配置地址'}`
      : '已切到内置模拟数据');
    connectStream();
    runRefresh(true);
  });

  document.getElementById('intervalSel').addEventListener('change', (ev) => {
    state.interval = Number(ev.target.value) || 0;
    savePrefs();
    state.failures = 0;
    state.lastError = '';
    scheduleNext();
    if (!state.interval) toast('已关闭自动刷新');
  });

  window.addEventListener('hashchange', () => {
    const route = parseRoute();
    if (route.name === 'detail') state.range = route.range;
    render();
    window.scrollTo({ top: 0, behavior: 'instant' in window ? 'instant' : 'auto' });
  });

  window.addEventListener('keydown', (ev) => {
    // 输入控件里的按键不拦截
    const tag = (ev.target.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'select' || tag === 'textarea') return;
    if (ev.key === 'r' || ev.key === 'R') { ev.preventDefault(); runRefresh(true); }
    if (ev.key === 'Escape' && parseRoute().name === 'detail') { location.hash = '#/'; }
  });

  // 可见性变化：后台暂停、回来立即刷一次（避免"回到页面看到 10 分钟前的数据"）
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) {
      if (state.timer) { clearTimeout(state.timer); state.timer = null; }
    } else {
      runRefresh(false);
    }
  });
}

function init() {
  loadPrefs();
  applyTheme(state.theme, false);

  // 控件初值同步（localStorage 可能改过默认值）
  const srcSel = document.getElementById('sourceSel');
  const itvSel = document.getElementById('intervalSel');
  srcSel.value = state.source;
  itvSel.value = String(state.interval);

  renderNotes();
  bindEvents();
  startHeartbeatClock();
  connectStream();

  // 首屏骨架：与真实卡片同高，避免加载完成时的布局跳动
  els.view.innerHTML = `<div class="grid">${Array.from({ length: 6 })
    .map(() => '<div class="skel skel-card"></div>').join('')}</div>`;
  runRefresh(false);
}

init();
