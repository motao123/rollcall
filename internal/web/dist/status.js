/* 公开状态页：Vue 3 + Element Plus（runtime-only）
 *
 * CSP 是 script-src 'self'，Vue 只能使用不带模板编译器的运行时版本，
 * 因此所有界面都用 h() 渲染函数描述，不出现 template / innerHTML 模板。
 * 页面刷新策略：SSE 通知 + 10 秒轮询兜底；数据到达后由响应式状态局部更新。
 */
(function () {
  'use strict';

  var mountEl = document.getElementById('app');
  var Vue = window.Vue;
  var EP = window.ElementPlus;
  if (!mountEl || !Vue || !EP) return; // 脚本被拦截时保留 CSS 兜底提示

  var h = Vue.h;
  var reactive = Vue.reactive;
  var computed = Vue.computed;
  var watchEffect = Vue.watchEffect;
  var locale = window.ElementPlusLocaleZhCn;

  document.documentElement.classList.add('js-ok');

  var LS_THEME = 'rollcall-status-theme';
  var SEC_KEYS = ['nodes', 'services', 'map', 'history', 'incidents', 'activity'];
  var SEC_LABEL = {
    nodes: '节点列表', services: '服务探测', map: '地区分布',
    history: '可用率', incidents: '事件', activity: '最近活动'
  };
  var INCIDENT_PREVIEW = 3;
  var ACTIVITY_PREVIEW = 5;
  var FEED_PAGE_SIZE = 10;
  var KPI_MIN_W = 132;

  /* ---------------- 格式化 ---------------- */

  function fixed(v, n) {
    v = Number(v);
    if (!isFinite(v)) return '—';
    return v.toFixed(n == null ? 1 : n);
  }

  function clampPct(v) {
    v = Number(v);
    if (!isFinite(v)) return 0;
    return Math.max(0, Math.min(100, v));
  }

  function dur(sec) {
    sec = Number(sec) || 0;
    if (sec <= 0) return '—';
    var d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
    if (d > 0) return d + ' 天' + (h > 0 ? ' ' + h + ' 小时' : '');
    if (h > 0) return h + ' 小时' + (m > 0 ? ' ' + m + ' 分' : '');
    return m + ' 分钟';
  }

  var UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  function bytes(n) {
    n = Number(n) || 0;
    var i = 0;
    while (n >= 1024 && i < UNITS.length - 1) { n /= 1024; i++; }
    return (i === 0 ? Math.round(n) : n.toFixed(n < 10 ? 1 : 0)) + ' ' + UNITS[i];
  }

  function rate(bps) {
    bps = Number(bps) || 0;
    if (bps < 1) return '0 B/s';
    return bytes(bps) + '/s';
  }

  function axisBytes(v) {
    v = Number(v) || 0;
    if (v >= 1024 * 1024) return (v / 1024 / 1024).toFixed(1) + 'M';
    if (v >= 1024) return (v / 1024).toFixed(0) + 'K';
    return Math.round(v) + 'B';
  }

  function p2(n) { return (n < 10 ? '0' : '') + n; }

  function clock(ts) {
    var d = new Date(ts * 1000);
    return p2(d.getHours()) + ':' + p2(d.getMinutes()) + ':' + p2(d.getSeconds());
  }

  function clockHM(ts) {
    var d = new Date(ts * 1000);
    return p2(d.getHours()) + ':' + p2(d.getMinutes());
  }

  function dayMD(ts) {
    var d = new Date(ts * 1000);
    return (d.getMonth() + 1) + '月' + d.getDate() + '日';
  }

  function fullTime(ts) {
    if (!ts) return '—';
    var d = new Date(ts * 1000);
    return d.getFullYear() + '-' + p2(d.getMonth() + 1) + '-' + p2(d.getDate()) + ' ' +
      p2(d.getHours()) + ':' + p2(d.getMinutes()) + ':' + p2(d.getSeconds());
  }

  function timeAgo(ts) {
    if (!ts) return '—';
    var diff = Math.max(0, Math.floor(Date.now() / 1000) - Number(ts));
    if (diff < 60) return '刚刚';
    if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
    if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
    return Math.floor(diff / 86400) + ' 天前';
  }

  function upCls(v) { return v >= 99.5 ? 'ok' : v >= 95 ? 'warn' : 'crit'; }
  function useCls(v) { return v >= 90 ? 'crit' : v >= 75 ? 'warn' : 'ok'; }
  function latCls(v) { return v > 300 ? 'crit' : v > 100 ? 'warn' : 'ok'; }

  function usageColor(v) {
    var c = useCls(v);
    return c === 'crit' ? 'var(--el-color-danger)' : c === 'warn' ? 'var(--el-color-warning)' : 'var(--el-color-success)';
  }

  function sparkColor(cpu) {
    var c = useCls(cpu);
    return c === 'crit' ? 'var(--el-color-danger)' : c === 'warn' ? 'var(--el-color-warning)' : 'var(--app-accent)';
  }

  var STATE = {
    online: { label: '在线', dot: 'ok', tag: 'success' },
    stale: { label: '数据延迟', dot: 'warn', tag: 'warning' },
    offline: { label: '离线', dot: 'bad', tag: 'danger' },
    no_data: { label: '待接入', dot: 'info', tag: 'info' }
  };

  function stateInfo(n) {
    var state = n.state || (n.online ? 'online' : 'offline');
    var m = STATE[state] || { label: '未知', dot: 'info', tag: 'info' };
    return { state: state, label: m.label, dot: m.dot, tag: m.tag };
  }

  function levelInfo(level) {
    if (level === 'critical') return { name: '严重', tag: 'danger' };
    if (level === 'warn') return { name: '警告', tag: 'warning' };
    return { name: '信息', tag: 'primary' };
  }

  var ACT = {
    node_online: { label: '上线', cls: 'ok' },
    node_offline: { label: '离线', cls: 'crit' },
    alert_opened: { label: '告警', cls: 'crit' },
    alert_resolved: { label: '恢复', cls: 'ok' }
  };

  var COUNTRY_NAMES = {
    CN: '中国', HK: '中国香港', TW: '中国台湾', MO: '中国澳门',
    US: '美国', CA: '加拿大', MX: '墨西哥', BR: '巴西', AR: '阿根廷', CL: '智利',
    JP: '日本', KR: '韩国', SG: '新加坡', MY: '马来西亚', TH: '泰国', VN: '越南',
    ID: '印度尼西亚', PH: '菲律宾', IN: '印度', PK: '巴基斯坦', BD: '孟加拉国',
    GB: '英国', IE: '爱尔兰', FR: '法国', DE: '德国', NL: '荷兰', BE: '比利时',
    LU: '卢森堡', CH: '瑞士', AT: '奥地利', IT: '意大利', ES: '西班牙', PT: '葡萄牙',
    SE: '瑞典', NO: '挪威', DK: '丹麦', FI: '芬兰', IS: '冰岛', PL: '波兰',
    CZ: '捷克', SK: '斯洛伐克', HU: '匈牙利', RO: '罗马尼亚', BG: '保加利亚',
    GR: '希腊', UA: '乌克兰', RU: '俄罗斯', TR: '土耳其', IL: '以色列',
    AE: '阿联酋', SA: '沙特阿拉伯', QA: '卡塔尔', EG: '埃及', ZA: '南非',
    NG: '尼日利亚', KE: '肯尼亚', AU: '澳大利亚', NZ: '新西兰'
  };

  function countryName(code) {
    var c = String(code || '').toUpperCase();
    return COUNTRY_NAMES[c] || c || '未知';
  }

  function countryFlag(code) {
    var c = String(code || '').toUpperCase();
    if (!/^[A-Z]{2}$/.test(c)) return '🌐';
    return String.fromCodePoint(0x1F1E6 + c.charCodeAt(0) - 65, 0x1F1E6 + c.charCodeAt(1) - 65);
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  /* ---------------- 主题 ---------------- */

  var mq = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;

  function readLocalMode() {
    var v = null;
    try { v = localStorage.getItem(LS_THEME); } catch (e) { /* 隐私模式 */ }
    return v === 'light' || v === 'dark' ? v : null;
  }

  var S = reactive({
    data: null,
    loading: true,
    loadError: '',
    admin: false,
    route: parseHash(),
    q: '',
    state: 'all',
    group: '',
    range: '24h',
    detail: null,
    detailError: '',
    detailLoading: false,
    connection: 'idle',
    width: window.innerWidth || 1280,
    theme: { mode: 'auto', accent: '#2563eb', accentDark: '#60a5fa', radius: 10, compact: false },
    localMode: readLocalMode(),
    sysDark: !!(mq && mq.matches),
    order: []
  });

  var resolvedMode = computed(function () {
    if (S.localMode === 'light' || S.localMode === 'dark') return S.localMode;
    if (S.theme.mode === 'light' || S.theme.mode === 'dark') return S.theme.mode;
    return S.sysDark ? 'dark' : 'light';
  });

  var isDark = computed(function () { return resolvedMode.value === 'dark'; });

  function hexToRgb(hex) {
    var m = /^#?([0-9a-f]{6})$/i.exec(String(hex || ''));
    if (!m) return [37, 99, 235];
    var n = parseInt(m[1], 16);
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
  }

  function mixHex(hex, target, weight) {
    var a = hexToRgb(hex), b = hexToRgb(target);
    var out = '#';
    for (var i = 0; i < 3; i++) {
      var v = Math.round(a[i] * (1 - weight) + b[i] * weight);
      out += (v < 16 ? '0' : '') + v.toString(16);
    }
    return out;
  }

  function applyTheme() {
    var dark = isDark.value;
    var accent = dark ? S.theme.accentDark : S.theme.accent;
    if (!/^#[0-9a-f]{6}$/i.test(accent)) accent = dark ? '#60a5fa' : '#2563eb';
    var root = document.documentElement;
    root.setAttribute('data-theme', dark ? 'dark' : 'light');
    root.classList.toggle('dark', dark);
    root.style.colorScheme = dark ? 'dark' : 'light';
    root.style.setProperty('--el-color-primary', accent);
    var base = dark ? '#000000' : '#ffffff';
    var opposite = dark ? '#ffffff' : '#000000';
    root.style.setProperty('--el-color-primary-light-3', mixHex(accent, base, 0.3));
    root.style.setProperty('--el-color-primary-light-5', mixHex(accent, base, 0.5));
    root.style.setProperty('--el-color-primary-light-7', mixHex(accent, base, 0.7));
    root.style.setProperty('--el-color-primary-light-8', mixHex(accent, base, 0.8));
    root.style.setProperty('--el-color-primary-light-9', mixHex(accent, base, 0.9));
    root.style.setProperty('--el-color-primary-dark-2', mixHex(accent, opposite, 0.2));
    root.style.setProperty('--app-accent', accent);
    root.style.setProperty('--app-accent-2', dark ? '#22d3ee' : '#0891b2');
    root.style.setProperty('--app-radius', S.theme.radius + 'px');
    root.style.setProperty('--el-border-radius-base', Math.min(S.theme.radius, 12) + 'px');
    if (S.theme.compact) root.setAttribute('data-density', 'compact');
    else root.removeAttribute('data-density');
    var meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.setAttribute('content', dark ? '#0a0a0a' : '#f2f4f8');
  }

  function toggleTheme() {
    S.localMode = isDark.value ? 'light' : 'dark';
    try { localStorage.setItem(LS_THEME, S.localMode); } catch (e) { /* 隐私模式 */ }
  }

  function loadTheme() {
    fetch('/api/theme', { headers: { Accept: 'application/json' }, cache: 'no-store' })
      .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
      .then(function (t) {
        if (!t || typeof t !== 'object') return;
        if (t.mode === 'auto' || t.mode === 'light' || t.mode === 'dark') S.theme.mode = t.mode;
        if (/^#[0-9a-f]{6}$/i.test(t.accent || '')) S.theme.accent = t.accent;
        if (/^#[0-9a-f]{6}$/i.test(t.accentDark || '')) S.theme.accentDark = t.accentDark;
        if (typeof t.radius === 'number' && isFinite(t.radius)) {
          S.theme.radius = Math.max(0, Math.min(24, t.radius));
        }
        S.theme.compact = !!t.compact;
      })
      .catch(function () { /* 保留默认外观 */ });
  }

  watchEffect(applyTheme);

  if (mq && mq.addEventListener) {
    mq.addEventListener('change', function (e) { S.sysDark = !!e.matches; });
  }

  /* ---------------- 路由 ---------------- */

  function parseHash() {
    var raw = String(location.hash || '');
    var m = raw.match(/^#\/n\/(.+)$/);
    if (m) {
      var id = m[1];
      try { id = decodeURIComponent(m[1]); } catch (e) { /* 保留原值 */ }
      return { view: 'node', id: id, page: 1 };
    }
    m = raw.match(/^#\/(events|activity)(?:\/(\d+))?$/);
    if (m) {
      return { view: m[1], id: null, page: m[2] ? Math.max(1, parseInt(m[2], 10)) : 1 };
    }
    return { view: 'list', id: null, page: 1 };
  }

  function goHome() {
    if (location.hash === '#/' || location.hash === '') {
      window.scrollTo(0, 0);
      return;
    }
    location.hash = '#/';
  }

  function goNode(hid) {
    if (!hid) return;
    location.hash = '#/n/' + encodeURIComponent(hid);
  }

  /* ---------------- 数据访问 ---------------- */

  function block(name) {
    if (!S.data || !S.data.blocks) return true;
    return !!S.data.blocks[name];
  }

  function vis(name) {
    if (!S.data || !S.data.visibility) return true;
    return !!S.data.visibility[name];
  }

  var allNodes = computed(function () { return (S.data && S.data.nodes) || []; });

  var filteredNodes = computed(function () {
    var q = S.q.trim().toLowerCase();
    return allNodes.value.filter(function (n) {
      if (S.state === 'online' && n.state !== 'online') return false;
      if (S.state === 'attention' && n.state === 'online') return false;
      if (S.group && (n.group || '未分组') !== S.group) return false;
      if (!q) return true;
      var hay = [n.name, n.group, n.region, n.provider, n.os, n.arch, n.country, (n.tags || []).join(' ')].join(' ').toLowerCase();
      return hay.indexOf(q) >= 0;
    });
  });

  var groups = computed(function () {
    var out = [], seen = {};
    allNodes.value.forEach(function (n) {
      var g = n.group || '未分组';
      if (!seen[g]) { seen[g] = true; out.push(g); }
    });
    return out;
  });

  var nodeCounts = computed(function () {
    var online = 0;
    allNodes.value.forEach(function (n) { if (n.state === 'online') online++; });
    return { all: allNodes.value.length, online: online, attention: allNodes.value.length - online };
  });

  function groupsOf(nodes) {
    var out = [], seen = {};
    (nodes || []).forEach(function (n) {
      var g = n.group || '未分组';
      if (!seen[g]) { seen[g] = true; out.push(g); }
    });
    return out;
  }

  function normalizeOrder(raw) {
    var out = [];
    (raw || []).forEach(function (k) {
      if (SEC_KEYS.indexOf(k) >= 0 && out.indexOf(k) < 0) out.push(k);
    });
    SEC_KEYS.forEach(function (k) { if (out.indexOf(k) < 0) out.push(k); });
    return out;
  }

  var sectionOrder = computed(function () {
    var raw = S.order && S.order.length ? S.order : ((S.data && S.data.sections) || []);
    return normalizeOrder(raw);
  });

  var siteName = computed(function () { return (S.data && S.data.site) || '服务状态'; });

  var pageTitle = computed(function () {
    var site = siteName.value;
    if (S.route.view === 'node') {
      var n = S.detail && S.detail.node;
      return (n && n.name ? n.name + ' · ' : '') + site;
    }
    if (S.route.view === 'events') return site === '服务状态' ? '事件' : site + ' · 事件';
    if (S.route.view === 'activity') return site === '服务状态' ? '最近活动' : site + ' · 最近活动';
    return site === '服务状态' ? '服务状态' : site + ' · 服务状态';
  });

  watchEffect(function () { document.title = pageTitle.value; });

  var overall = computed(function () {
    var d = S.data || {};
    var total = d.total || 0;
    if (!total) {
      return { cls: 'info', label: '等待第一个节点接入', sub: '在管理后台创建节点并启动 Agent 后，这里会显示实时状态。' };
    }
    var issues = [];
    if (d.offline > 0) issues.push(d.offline + ' 个节点离线');
    if (d.stale > 0) issues.push(d.stale + ' 个节点数据延迟');
    if (d.noData > 0) issues.push(d.noData + ' 个节点待接入');
    if (d.svcDown > 0) issues.push(d.svcDown + ' 个服务异常');
    if (!issues.length) {
      return { cls: 'ok', label: '全部系统运行正常', sub: '所有节点与服务均在预期状态。' };
    }
    return {
      cls: (d.offline > 0 || d.svcDown > 0) ? 'crit' : 'warn',
      label: '部分系统需要关注',
      sub: issues.join(' · ') + '。'
    };
  });

  var healthPct = computed(function () {
    var d = S.data || {};
    var total = d.total || 0;
    return total ? clampPct((d.online || 0) / total * 100) : 0;
  });

  var healthColor = computed(function () {
    if (!(S.data && S.data.total)) return 'var(--el-color-info)';
    var p = healthPct.value;
    return p >= 99.9 ? 'var(--el-color-success)' : p >= 80 ? 'var(--el-color-warning)' : 'var(--el-color-danger)';
  });

  function healthNote(d) {
    var notes = [];
    if (d.stale) notes.push(d.stale + ' 台数据延迟');
    if (d.noData) notes.push(d.noData + ' 台待接入');
    if (d.svcDown) notes.push(d.svcDown + ' 个服务异常');
    return notes.length ? notes.join('，') : '状态良好';
  }

  var kpiList = computed(function () {
    var d = S.data || {};
    var s = d.summary || {};
    var out = [];
    var extra = [];
    if (d.stale > 0) extra.push(d.stale + ' 延迟');
    if (d.noData > 0) extra.push(d.noData + ' 待接入');
    out.push({
      key: 'online', label: '在线节点',
      value: (d.online || 0) + ' / ' + (d.total || 0),
      foot: extra.length ? extra.join(' · ') : '全部节点'
    });
    var dataNodes = Math.max(0, (s.total || d.total || 0) - (s.noData || d.noData || 0));
    out.push({
      key: 'uptime', label: '24 小时可用率',
      value: dataNodes ? fixed(s.avgUptime24, 1) : '—',
      unit: dataNodes ? '%' : '',
      foot: dataNodes ? '有数据节点平均' : '暂无采样数据'
    });
    if (vis('metrics')) {
      out.push({ key: 'cpu', label: '平均 CPU', value: fixed(s.avgCpu, 0), unit: '%' });
      out.push({ key: 'mem', label: '平均内存', value: fixed(s.avgMem, 0), unit: '%' });
      out.push({ key: 'disk', label: '平均磁盘', value: fixed(s.avgDisk, 0), unit: '%' });
    }
    if (vis('traffic')) {
      out.push({
        key: 'net', label: '实时下行', value: rate(s.trafficIn),
        foot: '↑ ' + rate(s.trafficOut)
      });
    }
    if (s.svcTotal) {
      out.push({
        key: 'services', label: '服务健康',
        value: (s.svcTotal - s.svcDown) + ' / ' + s.svcTotal,
        foot: s.svcDown > 0
          ? h('span', { class: 'crit' }, s.svcDown + ' 个异常')
          : h('span', { class: 'ok' }, '全部正常')
      });
    }
    if (s.activeAlert) {
      out.push({
        key: 'alerts', label: '活跃告警', value: String(s.activeAlert),
        foot: h('span', { class: 'warn' }, '需关注')
      });
    }
    return out;
  });

  var kpiStyle = computed(function () {
    var n = kpiList.value.length;
    if (S.width <= 720 || n < 2) return null;
    var avail = Math.min(1280, S.width) - 96;
    var maxCols = Math.max(1, Math.floor((avail + 9) / (KPI_MIN_W + 9)));
    if (n <= maxCols) return null;
    var rows = Math.ceil(n / maxCols);
    var cols = Math.ceil(n / rows);
    return { gridTemplateColumns: 'repeat(' + cols + ', minmax(0, 1fr))' };
  });

  /* ---------------- 列表数据 ---------------- */

  var listInflight = false;
  var listSeq = 0;

  function humanError(e) {
    if (!e) return '未知错误';
    if (e.name === 'AbortError') return '请求超时，请检查网络后重试';
    return e.message || String(e);
  }

  function fetchList() {
    if (listInflight) return;
    listInflight = true;
    var seq = ++listSeq;
    var ctl = window.AbortController ? new AbortController() : null;
    var timer = ctl ? setTimeout(function () { ctl.abort(); }, 12000) : null;
    fetch('/api/status', {
      headers: { Accept: 'application/json' },
      cache: 'no-store',
      signal: ctl ? ctl.signal : undefined
    })
      .then(function (r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
      })
      .then(function (d) {
        if (timer) clearTimeout(timer);
        listInflight = false;
        if (seq !== listSeq) return;
        S.data = d;
        S.loading = false;
        S.loadError = '';
        S.connection = 'on';
        savedOrder = normalizeOrder(d.sections);
      })
      .catch(function (e) {
        if (timer) clearTimeout(timer);
        listInflight = false;
        if (seq !== listSeq) return;
        S.loading = false;
        S.connection = 'bad';
        if (!S.data) S.loadError = humanError(e);
      });
  }

  /* ---------------- 节点详情 ---------------- */

  var detailCache = {};
  var detailSeq = 0;

  function fetchDetail(hid, opts) {
    if (!hid) return;
    opts = opts || {};
    var keep = !!opts.keep;
    var background = !!opts.background;
    var key = hid + '|' + S.range;
    var seq = ++detailSeq;
    if (!keep) {
      S.detail = null;
      S.detailError = '';
    }
    if (!background) S.detailLoading = true;
    fetch('/api/status/node/' + encodeURIComponent(hid) + '?range=' + encodeURIComponent(S.range), {
      headers: { Accept: 'application/json' },
      cache: 'no-store'
    })
      .then(function (r) {
        if (r.status === 404) throw new Error('节点不存在或详情页未开放');
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
      })
      .then(function (dt) {
        detailCache[key] = dt;
        if (seq !== detailSeq) return;
        if (S.route.view !== 'node' || S.route.id !== hid || key !== hid + '|' + S.range) return;
        S.detail = dt;
        S.detailError = '';
        S.detailLoading = false;
      })
      .catch(function (e) {
        if (seq !== detailSeq) return;
        if (S.route.view !== 'node' || S.route.id !== hid || key !== hid + '|' + S.range) return;
        S.detailLoading = false;
        if (!keep || !S.detail) S.detailError = humanError(e);
      });
  }

  function setRange(r) {
    if (r === S.range) return;
    S.range = r;
    S.detailError = '';
    if (!S.route.id) return;
    var key = S.route.id + '|' + r;
    if (detailCache[key]) {
      S.detail = detailCache[key];
      S.detailLoading = false;
      return;
    }
    fetchDetail(S.route.id, { keep: true });
  }

  function onHashChange() {
    var next = parseHash();
    var prev = S.route;
    S.route = next;
    window.scrollTo(0, 0);
    if (next.view === 'node') {
      if (prev.view !== 'node' || prev.id !== next.id) {
        var cached = detailCache[next.id + '|' + S.range];
        if (cached) {
          S.detail = cached;
          S.detailLoading = false;
          S.detailError = '';
        } else {
          fetchDetail(next.id, {});
        }
      }
    } else if (next.view === 'list') {
      S.range = '24h';
      S.detailError = '';
    }
  }

  /* ---------------- 刷新与推送 ---------------- */

  var stream = null;
  var streamTimer = null;

  function connectStream() {
    if (!window.EventSource) return;
    try {
      stream = new EventSource('/api/status/stream');
    } catch (e) {
      return;
    }
    stream.addEventListener('hello', function () { S.connection = 'on'; });
    stream.addEventListener('change', function () {
      if (streamTimer) return;
      streamTimer = setTimeout(function () {
        streamTimer = null;
        tick();
      }, 300);
    });
    stream.onerror = function () { S.connection = 'bad'; };
  }

  function tick() {
    if (document.hidden) return;
    fetchList();
    if (S.route.view === 'node' && S.route.id) {
      fetchDetail(S.route.id, { keep: true, background: true });
    }
  }

  function checkAdmin() {
    fetch('/api/auth/session', { credentials: 'same-origin', cache: 'no-store' })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) { if (d && d.authenticated) S.admin = true; })
      .catch(function () { /* 匿名访客保持只读 */ });
  }

  /* ---------------- 区块排序 ---------------- */

  var savedOrder = [];
  var drag = { key: '', moved: false };

  function toast(msg, type) {
    if (EP.ElMessage) EP.ElMessage({ message: msg, type: type || 'success', duration: 2400 });
  }

  function saveOrder(order) {
    if (!S.admin) {
      S.order = savedOrder.slice();
      return;
    }
    fetch('/api/admin/status-page', { credentials: 'same-origin', cache: 'no-store' })
      .then(function (r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
      })
      .then(function (d) {
        var cfg = (d && d.config) || {};
        cfg.sections = order;
        return fetch('/api/admin/status-page', {
          method: 'PUT',
          credentials: 'same-origin',
          headers: {
            'Content-Type': 'application/json',
            'X-Requested-With': 'XMLHttpRequest'
          },
          body: JSON.stringify(cfg)
        });
      })
      .then(function (r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
      })
      .then(function (d) {
        var saved = normalizeOrder((d && d.config && d.config.sections) || order);
        savedOrder = saved;
        S.order = saved.slice();
        toast('排序已保存，对所有访客生效');
      })
      .catch(function (e) {
        S.order = savedOrder.slice();
        toast('保存排序失败：' + humanError(e), 'error');
      });
  }

  function startDrag(e, key) {
    if (!S.admin || e.button !== 0) return;
    e.preventDefault();
    drag.key = key;
    drag.moved = false;
    if (!S.order.length) S.order = sectionOrder.value.slice();
    try { e.currentTarget.setPointerCapture(e.pointerId); } catch (err) { /* 老浏览器 */ }
  }

  function onDragMove(e) {
    if (!drag.key) return;
    var order = sectionOrder.value.slice();
    var before = null;
    for (var i = 0; i < order.length; i++) {
      var k = order[i];
      if (k === drag.key) continue;
      var el = document.querySelector('[data-sec="' + k + '"]');
      if (!el) continue;
      var r = el.getBoundingClientRect();
      if (e.clientY < r.top + r.height / 2) { before = k; break; }
    }
    var next = order.filter(function (k) { return k !== drag.key; });
    var idx = before ? next.indexOf(before) : next.length;
    if (idx < 0) idx = next.length;
    next.splice(idx, 0, drag.key);
    if (next.join('|') !== order.join('|')) {
      S.order = next;
      drag.moved = true;
    }
  }

  function endDrag() {
    if (!drag.key) return;
    var changed = drag.moved;
    drag.key = '';
    drag.moved = false;
    if (changed) saveOrder(sectionOrder.value.slice());
  }

  function cancelDrag() {
    if (!drag.key) return;
    drag.key = '';
    drag.moved = false;
    if (savedOrder.length) S.order = savedOrder.slice();
  }

  function onHandleKey(e, key) {
    if (!e.altKey || (e.key !== 'ArrowUp' && e.key !== 'ArrowDown')) return;
    e.preventDefault();
    var order = sectionOrder.value.slice();
    var i = order.indexOf(key);
    if (i < 0) return;
    var j = e.key === 'ArrowUp' ? i - 1 : i + 1;
    if (j < 0 || j >= order.length) return;
    order.splice(i, 1);
    order.splice(j, 0, key);
    S.order = order;
    saveOrder(order);
  }

  /* ---------------- 图表 ---------------- */

  var chartSeq = 0;
  function nextChartId(prefix) {
    chartSeq++;
    return prefix + chartSeq;
  }

  function validValues(points) {
    return (points || []).map(function (p) {
      if (p && typeof p === 'object') return Number(p.v);
      return Number(p);
    }).filter(function (v) { return isFinite(v); });
  }

  function curvePath(values, w, h, min, max, step) {
    var span = max - min;
    if (span < 0.0001) span = 1;
    var y = function (v) { return h - 3 - ((v - min) / span) * (h - 6); };
    var pts = values.map(function (v, i) { return { x: i * step, y: y(v) }; });
    if (pts.length < 2) return { d: '', pts: pts };
    var d = 'M' + pts[0].x.toFixed(1) + ',' + pts[0].y.toFixed(1);
    for (var i = 1; i < pts.length; i++) {
      var prev = pts[i - 1], cur = pts[i];
      var mx = (prev.x + cur.x) / 2;
      d += ' C' + mx.toFixed(1) + ',' + prev.y.toFixed(1) + ' ' + mx.toFixed(1) + ',' +
        cur.y.toFixed(1) + ' ' + cur.x.toFixed(1) + ',' + cur.y.toFixed(1);
    }
    return { d: d, pts: pts };
  }

  function sparkSvg(points, color, height) {
    var hgt = height || 34, w = 100;
    var vals = validValues(points);
    if (vals.length < 2) return '';
    var min = Math.min.apply(null, vals), max = Math.max.apply(null, vals);
    var pad = (max - min) * 0.12 || 1;
    min -= pad;
    max += pad;
    if (min < 0) min = 0;
    var path = curvePath(vals, w, hgt - 4, min, max, w / (vals.length - 1));
    var gid = nextChartId('sg');
    return '<svg viewBox="0 0 ' + w + ' ' + hgt + '" preserveAspectRatio="none">' +
      '<defs><linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1">' +
      '<stop offset="0%" stop-color="' + color + '" stop-opacity=".24"/>' +
      '<stop offset="100%" stop-color="' + color + '" stop-opacity="0"/></linearGradient></defs>' +
      '<path d="' + path.d + ' L' + w + ',' + hgt + ' L0,' + hgt + ' Z" fill="url(#' + gid + ')"/>' +
      '<path d="' + path.d + '" fill="none" stroke="' + color + '" stroke-width="1.6" ' +
      'stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/></svg>';
  }

  function chartPlot(axis, svg) {
    return '<div class="chart-plot"><div class="chart-axis">' + axis + '</div>' +
      '<div class="chart-area">' + svg + '</div></div>';
  }

  function areaChart(points, color, fmt) {
    var w = 640, hgt = 150, padR = 8, padT = 10, padB = 20;
    var vals = validValues(points);
    if (vals.length < 2) return '';
    var iw = w - padR, ih = hgt - padT - padB;
    var min = Math.min.apply(null, vals), max = Math.max.apply(null, vals);
    var span = max - min;
    if (span < 0.0001) span = 1;
    min = Math.max(0, min - span * 0.14);
    max = max + span * 0.14;
    if (min === max) max = min + 1;
    var y = function (v) { return padT + ih - ((v - min) / (max - min)) * ih; };
    var step = iw / (vals.length - 1);
    var pts = vals.map(function (v, i) { return { x: i * step, y: y(v) }; });
    var d = 'M' + pts[0].x.toFixed(1) + ',' + pts[0].y.toFixed(1);
    for (var i = 1; i < pts.length; i++) {
      var prev = pts[i - 1], cur = pts[i];
      var mx = (prev.x + cur.x) / 2;
      d += ' C' + mx.toFixed(1) + ',' + prev.y.toFixed(1) + ' ' + mx.toFixed(1) + ',' +
        cur.y.toFixed(1) + ' ' + cur.x.toFixed(1) + ',' + cur.y.toFixed(1);
    }
    var area = d + ' L' + iw.toFixed(1) + ',' + (padT + ih) + ' L0,' + (padT + ih) + ' Z';
    var gid = nextChartId('ag');
    var grid = '', axis = '';
    for (var g = 0; g <= 3; g++) {
      var gv = min + ((max - min) / 3) * g;
      var gy = y(gv);
      grid += '<line x1="0" y1="' + gy.toFixed(1) + '" x2="' + (w - padR) +
        '" y2="' + gy.toFixed(1) + '" stroke="var(--app-line)" stroke-width="1"/>';
      axis += '<span style="top:' + (gy / hgt * 100).toFixed(2) + '%">' +
        esc(fmt ? fmt(gv) : gv.toFixed(0)) + '</span>';
    }
    var svg = '<svg viewBox="0 0 ' + w + ' ' + hgt + '" preserveAspectRatio="none">' +
      '<defs><linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1">' +
      '<stop offset="0%" stop-color="' + color + '" stop-opacity=".22"/>' +
      '<stop offset="100%" stop-color="' + color + '" stop-opacity="0"/></linearGradient></defs>' +
      grid +
      '<path d="' + area + '" fill="url(#' + gid + ')"/>' +
      '<path d="' + d + '" fill="none" stroke="' + color + '" stroke-width="1.8" ' +
      'stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/></svg>';
    return chartPlot(axis, svg);
  }

  function multiAreaChart(seriesList, colors, fmt) {
    var w = 640, hgt = 150, padR = 8, padT = 10, padB = 20;
    var all = [];
    var prepared = (seriesList || []).map(function (points) {
      var vals = validValues(points);
      all = all.concat(vals);
      return vals;
    });
    if (all.length < 2) return '';
    var max = Math.max.apply(null, all);
    if (max <= 0) max = 1;
    max = max * 1.15;
    var iw = w - padR, ih = hgt - padT - padB;
    var y = function (v) { return padT + ih - (v / max) * ih; };
    var grid = '', axis = '';
    for (var g = 0; g <= 3; g++) {
      var gv = (max / 3) * g;
      var gy = y(gv);
      grid += '<line x1="0" y1="' + gy.toFixed(1) + '" x2="' + (w - padR) +
        '" y2="' + gy.toFixed(1) + '" stroke="var(--app-line)" stroke-width="1"/>';
      axis += '<span style="top:' + (gy / hgt * 100).toFixed(2) + '%">' +
        esc(fmt ? fmt(gv) : gv.toFixed(0)) + '</span>';
    }
    var defs = '<defs>';
    var paths = '';
    prepared.forEach(function (vals, idx) {
      if (vals.length < 2) return;
      var step = iw / (vals.length - 1);
      var pts = vals.map(function (v, i) { return { x: i * step, y: y(v) }; });
      var d = 'M' + pts[0].x.toFixed(1) + ',' + pts[0].y.toFixed(1);
      for (var i = 1; i < pts.length; i++) {
        var prev = pts[i - 1], cur = pts[i];
        var mx = (prev.x + cur.x) / 2;
        d += ' C' + mx.toFixed(1) + ',' + prev.y.toFixed(1) + ' ' + mx.toFixed(1) + ',' +
          cur.y.toFixed(1) + ' ' + cur.x.toFixed(1) + ',' + cur.y.toFixed(1);
      }
      var gid = nextChartId('mg');
      defs += '<linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1">' +
        '<stop offset="0%" stop-color="' + colors[idx] + '" stop-opacity=".18"/>' +
        '<stop offset="100%" stop-color="' + colors[idx] + '" stop-opacity="0"/></linearGradient>';
      paths += '<path d="' + d + ' L' + iw.toFixed(1) + ',' + (padT + ih) + ' L0,' +
        (padT + ih) + ' Z" fill="url(#' + gid + ')"/>' +
        '<path d="' + d + '" fill="none" stroke="' + colors[idx] + '" stroke-width="1.8" ' +
        'stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/>';
    });
    defs += '</defs>';
    var svg = '<svg viewBox="0 0 ' + w + ' ' + hgt + '" preserveAspectRatio="none">' +
      defs + grid + paths + '</svg>';
    return chartPlot(axis, svg);
  }

  function pctAxisFmt(points) {
    var vals = validValues(points);
    var span = vals.length ? Math.max.apply(null, vals) - Math.min.apply(null, vals) : 0;
    var digits = span < 0.3 ? 2 : span < 3 ? 1 : 0;
    return function (x) { return fixed(x, digits) + '%'; };
  }

  /* ---------------- 通用组件片段 ---------------- */

  function svgIcon(children, width) {
    return h('svg', {
      viewBox: '0 0 24 24',
      width: width || 16,
      height: width || 16,
      fill: 'none',
      stroke: 'currentColor',
      'stroke-width': 1.8,
      'stroke-linecap': 'round',
      'stroke-linejoin': 'round',
      'aria-hidden': 'true',
      style: { display: 'block' }
    }, children);
  }

  function searchIcon() {
    return svgIcon([
      h('circle', { cx: 11, cy: 11, r: 7 }),
      h('path', { d: 'm20 20-3.2-3.2' })
    ], 15);
  }

  function sunIcon() {
    return svgIcon([
      h('circle', { cx: 12, cy: 12, r: 4 }),
      h('path', { d: 'M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M19.1 4.9l-1.4 1.4M6.3 17.7l-1.4 1.4' })
    ]);
  }

  function moonIcon() {
    return svgIcon([
      h('path', { d: 'M20.5 14.1A8.5 8.5 0 0 1 9.9 3.5 8.5 8.5 0 1 0 20.5 14.1z' })
    ]);
  }

  function tag(text, type, opts) {
    var props = { size: 'small', type: type || 'info', effect: 'light', round: true };
    if (opts) {
      if (opts.effect) props.effect = opts.effect;
      if (opts.round != null) props.round = opts.round;
      if (opts.size) props.size = opts.size;
    }
    return h(EP.ElTag, props, { default: function () { return String(text == null ? '' : text); } });
  }

  function moreLink(href) {
    return h('a', { class: 'more-link', href: href }, ['查看更多 ', h('span', { 'aria-hidden': 'true' }, '→')]);
  }

  function backButton() {
    return h(EP.ElButton, {
      link: true, type: 'primary', onClick: goHome
    }, { default: function () { return '← 返回总览'; } });
  }

  function panelHead(title, sub, extra) {
    return h('div', { class: 'panel-head' }, [
      h('h2', { class: 'panel-title' }, title),
      sub ? h('span', { class: 'panel-sub' }, sub) : null,
      extra ? h('div', { class: 'panel-extra' }, extra) : null
    ]);
  }

  function panelCard(opts) {
    return h(EP.ElCard, {
      class: 'panel' + (opts.class ? ' ' + opts.class : ''),
      shadow: 'never'
    }, {
      header: function () { return panelHead(opts.title, opts.sub, opts.extra); },
      default: function () { return opts.body; }
    });
  }

  function descItem(label, value, sub) {
    return h(EP.ElDescriptionsItem, { label: label }, {
      default: function () {
        if (!sub) return value;
        return h('span', null, [value, h('span', { class: 'info-sub' }, sub)]);
      }
    });
  }

  /* ---------------- 顶部 / 页脚 / 横幅 ---------------- */

  function renderTopbar() {
    var conn = S.connection;
    var live = conn === 'on' ? '实时连接' : conn === 'bad' ? '连接中断' : '正在连接';
    var stamp = S.data ? '更新于 ' + clock(S.data.generatedAt) : '加载中…';
    if (conn === 'bad' && S.data) stamp = '连接中断 · 上次更新 ' + clock(S.data.generatedAt);
    return h('header', { class: 'topbar' }, [
      h('div', { class: 'topbar-inner' }, [
        h('div', { class: 'brand' }, [
          h('span', { class: 'brand-mark', 'aria-hidden': 'true' }, [h('i'), h('i'), h('i')]),
          h('div', { class: 'brand-text' }, [
            h('h1', null, siteName.value),
            h('div', { class: 'brand-sub' }, [
              h('span', { class: 'live-dot' + (conn === 'on' ? ' on' : conn === 'bad' ? ' bad' : '') }),
              h('span', null, live),
              h('span', { class: 'sep' }),
              h('span', null, stamp)
            ])
          ])
        ]),
        h('div', { class: 'top-actions' }, [
          h(EP.ElButton, {
            circle: true,
            plain: true,
            title: isDark.value ? '切换到浅色' : '切换到深色',
            'aria-label': isDark.value ? '切换到浅色' : '切换到深色',
            onClick: toggleTheme
          }, { default: function () { return isDark.value ? sunIcon() : moonIcon(); } }),
          h(EP.ElButton, {
            type: 'primary',
            onClick: function () { location.href = '/'; }
          }, { default: function () { return '管理登录'; } })
        ])
      ])
    ]);
  }

  function renderFooter() {
    var d = S.data;
    var parts = [];
    if (d) {
      parts.push((d.online || 0) + '/' + (d.total || 0) + ' 在线');
      if (d.stale) parts.push(d.stale + ' 台数据延迟');
      if (d.noData) parts.push(d.noData + ' 台待接入');
    }
    return h('footer', { class: 'page-foot' }, [
      h('span', null, '数据实时更新 · 页面隐藏时暂停'),
      h('span', null, parts.join(' · '))
    ]);
  }

  function renderBanners() {
    var out = [];
    var d = S.data;
    if (d && d.announcement && d.announcement.enabled && d.announcement.text) {
      var type = d.announcement.level === 'critical' ? 'error' : d.announcement.level === 'warn' ? 'warning' : 'info';
      out.push(h(EP.ElAlert, {
        class: 'page-alert',
        type: type,
        title: d.announcement.text,
        'show-icon': true,
        closable: false
      }));
    }
    var m = d && d.maintenance;
    if (m && (m.global || m.count > 0)) {
      var detail = m.text || '';
      if (!m.global && m.count > 0) detail += '（影响 ' + m.count + ' 个节点）';
      out.push(h(EP.ElAlert, {
        class: 'page-alert',
        type: 'info',
        title: m.title || '计划维护',
        description: detail,
        'show-icon': true,
        closable: false
      }));
    }
    return out;
  }

  /* ---------------- 总览 ---------------- */

  function kpiCard(k) {
    return h('div', { class: 'kpi' }, [
      h('div', { class: 'kpi-l' }, k.label),
      h('div', { class: 'kpi-v' }, [String(k.value), k.unit ? h('small', null, k.unit) : null]),
      k.foot ? h('div', { class: 'kpi-f' }, k.foot) : null
    ]);
  }

  function renderHero() {
    var d = S.data;
    var o = overall.value;
    var total = d.total || 0;
    var pct = healthPct.value;
    var ring = h(EP.ElProgress, {
      type: 'circle',
      percentage: Math.round(pct),
      width: 88,
      strokeWidth: 7,
      color: healthColor.value,
      'show-text': true,
      format: function () { return total ? Math.round(pct) + '%' : '—'; }
    });
    return h(EP.ElCard, { class: 'hero-card', shadow: 'never' }, {
      default: function () {
        return h('div', { class: 'hero-inner' }, [
          h('div', { class: 'hero-main' }, [
            h('div', { class: 'hero-status' }, [
              h('span', { class: 'status-orb ' + o.cls }),
              h('div', null, [
                h('h2', { class: 'hero-title' }, o.label),
                h('div', { class: 'hero-sub' }, o.sub)
              ])
            ]),
            h('div', { class: 'hero-health' }, [
              h('div', { class: 'health-ring' }, ring),
              h('div', { class: 'health-copy' }, [
                h('b', null, (d.online || 0) + '/' + total + ' 节点在线'),
                healthNote(d)
              ])
            ])
          ]),
          block('summary') ? h('div', { class: 'kpis', style: kpiStyle.value || undefined },
            kpiList.value.map(kpiCard)) : null
        ]);
      }
    });
  }

  function renderToolbar() {
    var counts = nodeCounts.value;
    var children = [
      h(EP.ElInput, {
        class: 'search-input',
        modelValue: S.q,
        'onUpdate:modelValue': function (v) { S.q = v || ''; },
        placeholder: '搜索节点、分组、地区…',
        clearable: true,
        'aria-label': '搜索节点'
      }, { prefix: function () { return searchIcon(); } }),
      h(EP.ElRadioGroup, {
        class: 'state-filter',
        modelValue: S.state,
        'onUpdate:modelValue': function (v) { S.state = v || 'all'; }
      }, {
        default: function () {
          return [
            h(EP.ElRadioButton, { value: 'all' }, { default: function () { return '全部 ' + counts.all; } }),
            h(EP.ElRadioButton, { value: 'online' }, { default: function () { return '在线 ' + counts.online; } }),
            h(EP.ElRadioButton, { value: 'attention' }, { default: function () { return '非在线 ' + counts.attention; } })
          ];
        }
      })
    ];
    if (groups.value.length > 1) {
      children.push(h(EP.ElSelect, {
        class: 'group-select',
        modelValue: S.group,
        'onUpdate:modelValue': function (v) { S.group = v || ''; },
        placeholder: '全部分组',
        clearable: true,
        teleported: true
      }, {
        default: function () {
          return [h(EP.ElOption, { label: '全部分组', value: '' })].concat(
            groups.value.map(function (g) {
              return h(EP.ElOption, { key: g, label: g, value: g });
            })
          );
        }
      }));
    }
    children.push(h('div', { class: 'tool-count' }, [
      '显示 ', h('b', null, filteredNodes.value.length), ' / ', allNodes.value.length, ' 台'
    ]));
    return h('div', { class: 'toolbar' }, children);
  }

  function factsFor(n) {
    var facts = [];
    if (vis('os') && n.os) {
      var osText = n.os + (vis('arch') && n.arch ? ' · ' + n.arch : '');
      facts.push(h('span', null, osText));
    }
    if (vis('cores') && n.cores) facts.push(h('span', null, [h('b', null, n.cores + ' 核')]));
    if (vis('uptime') && n.uptime) facts.push(h('span', null, ['运行 ', h('b', null, dur(n.uptime))]));
    if (vis('load') && n.load1 != null) facts.push(h('span', null, ['负载 ', h('b', null, fixed(n.load1, 2))]));
    if (n.state === 'offline' || n.state === 'stale') {
      facts.push(h('span', null, ['最后上报 ', h('b', null, timeAgo(n.lastSeen))]));
    }
    if (n.state === 'no_data') facts.push(h('span', null, '等待 Agent 首次上报'));
    if (vis('quota') && n.quotaGb > 0) {
      var used = n.quotaUsed || 0;
      var quotaBytes = n.quotaGb * 1024 * 1024 * 1024;
      var pctUsed = quotaBytes > 0 ? (used / quotaBytes) * 100 : 0;
      facts.push(h('span', { title: '本周期已用 ' + bytes(used) + ' / ' + n.quotaGb + ' GB' }, [
        '流量 ', h('b', {
          style: pctUsed >= 90 ? 'color:var(--el-color-danger)' : pctUsed >= 80 ? 'color:var(--el-color-warning)' : undefined
        }, fixed(pctUsed, 1) + '%')
      ]));
    }
    if (!facts.length) return null;
    return h('div', { class: 'nc-facts' }, facts);
  }

  function tagsFor(n) {
    var tags = [];
    if (n.group) tags.push(tag(n.group, 'info', { effect: 'plain' }));
    if (vis('region') && n.region) tags.push(tag(n.region, 'primary', { effect: 'plain' }));
    if (vis('provider') && n.provider) tags.push(tag(n.provider, 'info', { effect: 'plain' }));
    if (vis('tags')) {
      (n.tags || []).forEach(function (t) { tags.push(tag(t, 'success', { effect: 'plain' })); });
    }
    if (vis('billing') && n.price) {
      tags.push(tag((n.currency === 'CNY' ? '¥' : '$') + n.price + ' / 年', 'info', { effect: 'plain' }));
    }
    if (vis('expire') && n.expireAt) {
      tags.push(tag('到期 ' + n.expireAt, 'warning', { effect: 'plain' }));
    }
    if (n.maintenanceTitle) tags.push(tag('维护中', 'warning', { effect: 'plain' }));
    if (!tags.length) return null;
    return h('div', { class: 'nc-tags' }, tags);
  }

  function resLine(label, pct, sub) {
    pct = clampPct(pct);
    var cls = useCls(pct);
    return h('div', { class: 'res-line', title: sub || undefined }, [
      h('span', { class: 'res-label' }, label),
      h(EP.ElProgress, {
        percentage: Number(pct.toFixed(1)),
        strokeWidth: 6,
        'show-text': false,
        color: usageColor(pct)
      }),
      h('span', { class: 'res-value ' + (cls === 'ok' ? '' : cls) }, fixed(pct, 1) + '%')
    ]);
  }

  function metricsFor(n) {
    if (!vis('metrics')) return null;
    var rows = [
      resLine('CPU', n.cpu, ''),
      resLine('内存', n.mem, n.memTotal ? bytes(n.memUsed) + ' / ' + bytes(n.memTotal) : ''),
      resLine('磁盘', n.disk, n.diskTotal ? bytes(n.diskUsed) + ' / ' + bytes(n.diskTotal) : '')
    ];
    return h('div', { class: 'res-rows' }, rows);
  }

  function netFor(n) {
    if (!vis('traffic')) return null;
    var children = [
      h('span', { class: 'net-item' }, [
        h('span', { class: 'arrow' }, '↓'), h('b', null, rate(n.netRx))
      ]),
      h('span', { class: 'net-item' }, [
        h('span', { class: 'arrow' }, '↑'), h('b', null, rate(n.netTx))
      ])
    ];
    if (n.totalRx || n.totalTx) {
      children.push(h('span', { class: 'net-total' },
        '累计 ↓' + bytes(n.totalRx) + ' · ↑' + bytes(n.totalTx)));
    }
    return h('div', { class: 'net-row' }, children);
  }

  function barsFor(bars) {
    var now = Math.floor(Date.now() / 1000);
    var map = {};
    (bars || []).forEach(function (x) {
      if (x && x.t) map[Math.round((now - x.t) / 1800)] = x.ok;
    });
    var out = [];
    for (var i = 47; i >= 0; i--) {
      var v = map[i];
      var cls = v === undefined ? 'gap' : v ? '' : 'crit';
      var tip = v === undefined ? '无数据' : v ? '在线' : '离线';
      out.push(h('i', { key: i, class: cls, title: tip }));
    }
    return out;
  }

  function nodeCard(n) {
    var info = stateInfo(n);
    var link = block('nodeDetail');
    var up = n.uptime24;
    var upText = up != null ? fixed(up, 1) + '%' : '—';
    var upClass = up != null ? upCls(up) : 'none';
    var spark = validValues(n.spark);
    var peak = spark.length ? Math.max.apply(null, spark) : null;
    var sparkHtml = spark.length > 1 ? sparkSvg(n.spark, sparkColor(n.cpu), 34) : '';
    var children = [
      h('div', { class: 'nc-head' }, [
        h('div', { class: 'nc-id' }, [
          h('span', { class: 'state-dot ' + info.dot }),
          h('div', { class: 'nc-main' }, [
            h('div', { class: 'nc-name', title: n.name }, n.name),
            tagsFor(n)
          ])
        ]),
        h('div', { class: 'nc-side' }, [
          tag(n.maintenanceTitle ? '维护中' : info.label, n.maintenanceTitle ? 'warning' : info.tag),
          h('span', { class: 'nc-uptime' }, [
            '24 小时 ', h('b', { class: upClass }, upText)
          ])
        ])
      ]),
      factsFor(n),
      metricsFor(n),
      netFor(n),
      h('div', { class: 'nc-foot' }, [
        h('div', { class: 'spark-wrap' }, vis('metrics') ? [
          h('div', { class: 'spark-head' }, [
            h('span', null, 'CPU 趋势'),
            h('span', null, peak != null ? '峰值 ' + fixed(peak, 0) + '%' : '')
          ]),
          h('div', { class: 'spark-box', innerHTML: sparkHtml })
        ] : []),
        h('div', { class: 'avail' }, [
          h('div', { class: 'avail-v ' + upClass }, upText),
          h('div', { class: 'avail-l' }, '24 小时可用率'),
          h('div', { class: 'mini-bars' }, barsFor(n.bars))
        ])
      ])
    ];
    return h(EP.ElCard, {
      key: n.hid || n.name,
      class: 'node-card' + (link ? ' is-link' : '') + ' state-' + info.state,
      shadow: 'never',
      tabindex: link ? 0 : undefined,
      role: link ? 'link' : undefined,
      'aria-label': n.name + '，' + info.label + '，24 小时可用率 ' + upText,
      onClick: link ? function () { goNode(n.hid); } : undefined,
      onKeydown: link ? function (e) {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          goNode(n.hid);
        }
      } : undefined
    }, { default: function () { return children; } });
  }

  function renderNodesSection() {
    if (!allNodes.value.length) {
      return [h(EP.ElCard, { class: 'panel', shadow: 'never' }, {
        default: function () {
          return h(EP.ElEmpty, { description: '还没有节点接入', imageSize: 80 });
        }
      })];
    }
    var nodes = filteredNodes.value;
    if (!nodes.length) {
      return [renderToolbar(), h(EP.ElCard, { class: 'panel', shadow: 'never' }, {
        default: function () {
          return h(EP.ElEmpty, { description: '没有匹配的节点', imageSize: 80 });
        }
      })];
    }
    var groupsInView = groupsOf(nodes);
    var grid = [];
    groupsInView.forEach(function (g) {
      var list = nodes.filter(function (n) { return (n.group || '未分组') === g; });
      if (groupsInView.length > 1) {
        grid.push(h('div', { class: 'group-head', key: 'g-' + g }, [
          h('span', null, g),
          h('em', null, list.length + ' 台')
        ]));
      }
      list.forEach(function (n) { grid.push(nodeCard(n)); });
    });
    return [renderToolbar(), h('div', { class: 'node-grid' }, grid)];
  }

  /* ---------------- 各区块 ---------------- */

  function serviceItem(s, detail) {
    var has = s.has !== false;
    var dot = has ? (s.ok ? 'ok' : 'bad') : 'info';
    var lat = has && s.latencyMs != null ? s.latencyMs : null;
    var latText = lat != null ? (lat < 1 ? '<1' : String(lat)) + 'ms' : '—';
    var latClass = lat != null ? latCls(lat) : 'none';
    var uptime = s.uptime != null ? fixed(s.uptime, 1) + '%' : '—';
    var meta = [uptime + ' 可用'];
    if (s.failRate) meta.push('失败 ' + fixed(s.failRate, 2) + '%');
    if (s.lossPct) meta.push('丢包 ' + fixed(s.lossPct, 1) + '%');
    if (s.jitterMs) meta.push('抖动 ' + fixed(s.jitterMs, 1) + 'ms');
    var pts = detail ? s.latency : s.latencySpark;
    var spark = pts && pts.length > 1
      ? h('div', { class: 'svc-spark', innerHTML: sparkSvg(pts, 'var(--app-accent-2)', detail ? 42 : 24) })
      : null;
    return h('div', { class: 'svc', key: s.name }, [
      h('span', { class: 'state-dot ' + dot }),
      h('div', { class: 'svc-main' }, [
        h('div', { class: 'svc-name', title: s.name }, s.name),
        h('div', { class: 'svc-meta' }, meta.join(' · '))
      ]),
      h('span', { class: 'svc-side' }, [
        h('span', { class: 'svc-lat ' + latClass }, latText),
        h('span', null, uptime)
      ]),
      spark
    ]);
  }

  function servicesSection() {
    if (!block('services')) return null;
    var list = (S.data && S.data.services) || [];
    if (!list.length) return null;
    return panelCard({
      title: '服务探测',
      sub: 'HTTP / TCP / DNS 可用率、延迟与网络质量',
      body: h('div', { class: 'svc-grid' }, list.map(function (s) { return serviceItem(s, false); }))
    });
  }

  function mapSection() {
    if (!block('map')) return null;
    var counts = (S.data && S.data.map) || {};
    var keys = Object.keys(counts).filter(function (k) { return counts[k] > 0; });
    if (!keys.length) return null;
    keys.sort(function (a, b) { return counts[b] - counts[a] || a.localeCompare(b); });
    var total = 0, max = 0;
    keys.forEach(function (k) {
      total += counts[k];
      if (counts[k] > max) max = counts[k];
    });
    var rows = keys.map(function (k) {
      var n = counts[k];
      var width = max ? Math.max(4, n / max * 100) : 0;
      var pct = total ? n / total * 100 : 0;
      return h('div', { class: 'map-row', key: k, title: countryName(k) + '：' + n + ' 台' }, [
        h('span', { class: 'map-flag' }, countryFlag(k)),
        h('span', { class: 'map-name' }, countryName(k)),
        h('span', { class: 'map-bar' }, h('i', { style: { width: width.toFixed(1) + '%' } })),
        h('span', { class: 'map-count' }, [
          h('b', null, String(n)), ' 台 · ' + fixed(pct, 0) + '%'
        ])
      ]);
    });
    return panelCard({
      title: '地区分布',
      sub: '按国家/地区统计节点',
      body: h('div', { class: 'map-list' }, rows)
    });
  }

  function upBar(days) {
    return (days || []).map(function (d, i) {
      var cls = !d.has ? 'gap' : d.up >= 99.5 ? '' : d.up >= 95 ? 'warn' : 'crit';
      var tip = dayMD(d.ts) + '　' + (d.has ? d.up.toFixed(1) + '%' : '无数据');
      return h('i', { key: i, class: cls, title: tip });
    });
  }

  function upBarSummary(days) {
    var sum = 0, n = 0;
    (days || []).forEach(function (x) {
      if (x.has) { sum += x.up; n++; }
    });
    return n ? (sum / n).toFixed(1) : '—';
  }

  function upBarFoot(days, avg) {
    return h('div', { class: 'upbar-foot' }, [
      h('span', null, dayMD(days[0].ts)),
      h('span', { class: 'mid' }, avg + (avg === '—' ? '' : '%')),
      h('span', null, dayMD(days[days.length - 1].ts))
    ]);
  }

  function historySection() {
    if (!block('history')) return null;
    var days = (S.data && S.data.history) || [];
    if (!days.length) return null;
    var avg = upBarSummary(days);
    return panelCard({
      title: '可用率',
      sub: '过去 ' + ((S.data && S.data.historyDays) || days.length) + ' 天 · 每日一格',
      extra: h('span', null, ['平均 ', h('b', { class: 'mono' }, avg + (avg === '—' ? '' : '%'))]),
      body: [
        h('div', { class: 'upbar' }, upBar(days)),
        upBarFoot(days, avg)
      ]
    });
  }

  function incidentNodeText(it) {
    if (!it.nodeNames || !it.nodeNames.length) return '';
    return ' · ' + it.nodeNames.join('、');
  }

  var INC_UPDATE_META = {
    investigating: ['调查中', 'warning'],
    identified: ['已定位', 'primary'],
    monitoring: ['观察中', 'primary'],
    resolved: ['已恢复', 'success']
  };

  function incidentUpdates(it) {
    var ups = it.updates || [];
    if (!ups.length) return null;
    return h('div', { class: 'incident-updates' }, ups.map(function (u, i) {
      var meta = INC_UPDATE_META[u.status] || ['更新', 'info'];
      return h('div', { class: 'inc-upd', key: i }, [
        h('span', { class: 'inc-upd-time mono' }, clockHM(u.ts)),
        h('span', { class: 'inc-upd-tag tl-tag ' + meta[1] }, meta[0]),
        h('span', { class: 'inc-upd-text' }, u.text)
      ]);
    }));
  }

  function incidentItem(it) {
    var lv = levelInfo(it.level);
    var time = clockHM(it.startAt);
    if (it.endAt) time += ' → ' + clockHM(it.endAt);
    else time += ' 起';
    time += '，持续 ' + dur(it.duration);
    time += incidentNodeText(it);
    return h(EP.ElTimelineItem, {
      timestamp: clockHM(it.startAt),
      type: lv.tag,
      hollow: !it.endAt
    }, {
      default: function () {
        return [
          h('div', { class: 'incident-title' }, [
            h('span', null, it.title),
            h('span', { class: 'tl-tag ' + lv.tag }, lv.name),
            !it.endAt ? h('span', { class: 'tl-tag primary' }, '进行中') : null,
            it.auto ? null : h('span', { class: 'tl-tag' }, '人工')
          ]),
          h('div', { class: 'incident-meta' }, time),
          it.body ? h('div', { class: 'incident-body' }, it.body) : null,
          incidentUpdates(it)
        ];
      }
    });
  }

  function incidentTimeline(items) {
    var byDay = {}, order = [];
    (items || []).forEach(function (it) {
      var d = new Date(it.startAt * 1000);
      var key = d.getFullYear() + '-' + (d.getMonth() + 1) + '-' + d.getDate();
      if (!byDay[key]) { byDay[key] = []; order.push(key); }
      byDay[key].push(it);
    });
    return h('div', { class: 'feed-timeline' }, order.map(function (k) {
      return h('div', { class: 'day-group', key: k }, [
        h('div', { class: 'day-label' }, dayMD(byDay[k][0].startAt)),
        h(EP.ElTimeline, null, {
          default: function () { return byDay[k].map(incidentItem); }
        })
      ]);
    }));
  }

  function upcomingMaintenance() {
    var list = (S.data && S.data.maintenanceUpcoming) || [];
    if (!list.length) return null;
    return h('div', { class: 'upcoming-mt' }, [
      h('div', { class: 'upcoming-mt-title' }, '计划维护'),
      h('div', { class: 'upcoming-list' }, list.map(function (m, i) {
        var scope = m.nodeNames && m.nodeNames.length ? m.nodeNames.join('、') : '全部节点';
        return h('div', { class: 'upcoming-item', key: i }, [
          h('span', { class: 'upcoming-when mono' }, fullTime(m.startAt) + ' 起'),
          h('span', { class: 'upcoming-name' }, m.title),
          h('span', { class: 'upcoming-scope' }, scope),
          m.reason ? h('span', { class: 'upcoming-reason' }, m.reason) : null
        ]);
      }))
    ]);
  }

  function incidentsSection() {
    if (!block('incidents')) return null;
    var items = (S.data && S.data.incidents) || [];
    var shown = items.slice(0, INCIDENT_PREVIEW);
    var body = [
      upcomingMaintenance(),
      shown.length
        ? incidentTimeline(shown)
        : h(EP.ElEmpty, { description: '过去一段时间没有记录到事件', imageSize: 70 })
    ];
    return panelCard({
      title: '事件',
      sub: '告警自动生成 + 人工登记',
      extra: items.length > shown.length ? moreLink('#/events') : null,
      body: body
    });
  }

  function activityList(items) {
    if (!items || !items.length) {
      return h(EP.ElEmpty, { description: '暂无活动记录', imageSize: 70 });
    }
    return h('div', { class: 'act-list' }, items.map(function (ev, i) {
      var meta = ACT[ev.kind] || { label: '动态', cls: 'info' };
      return h('div', { class: 'act-item', key: (ev.ts || 0) + '-' + i }, [
        h('span', { class: 'act-dot ' + meta.cls }),
        h('div', { class: 'act-main' }, [
          h('div', { class: 'act-text' }, ev.text),
          h('div', { class: 'act-meta' }, [
            h('span', { class: 'act-badge ' + meta.cls }, meta.label),
            h('span', null, timeAgo(ev.ts)),
            h('span', { class: 'act-time' }, fullTime(ev.ts))
          ])
        ])
      ]);
    }));
  }

  function activitySection() {
    if (!block('activity')) return null;
    var items = (S.data && S.data.activity) || [];
    var shown = items.slice(0, ACTIVITY_PREVIEW);
    return panelCard({
      title: '最近活动',
      sub: '节点上下线与告警动态',
      extra: items.length > shown.length ? moreLink('#/activity') : null,
      body: activityList(shown)
    });
  }

  function sectionWrap(key, content) {
    if (!content) return null;
    var body = Array.isArray(content) ? content : [content];
    if (S.admin) {
      body = [h('button', {
        class: 'sec-handle',
        type: 'button',
        title: '拖动调整位置，或按 Alt+上下方向键移动',
        'aria-label': '调整「' + (SEC_LABEL[key] || key) + '」模块位置',
        onPointerdown: function (e) { startDrag(e, key); },
        onPointermove: onDragMove,
        onPointerup: endDrag,
        onPointercancel: cancelDrag,
        onKeydown: function (e) { onHandleKey(e, key); }
      }, '⠿')].concat(body);
    }
    return h('section', { class: 'page-section', 'data-sec': key, key: key }, body);
  }

  function renderListPage() {
    var out = renderBanners();
    out.push(renderHero());
    var map = {
      nodes: renderNodesSection(),
      services: servicesSection(),
      map: mapSection(),
      history: historySection(),
      incidents: incidentsSection(),
      activity: activitySection()
    };
    sectionOrder.value.forEach(function (k) {
      var wrap = sectionWrap(k, map[k]);
      if (wrap) out.push(wrap);
    });
    return out;
  }

  /* ---------------- 独立列表页 ---------------- */

  function feedItems(kind) {
    if (kind === 'events') return (S.data && S.data.incidents) || [];
    return (S.data && S.data.activity) || [];
  }

  function feedPageCount(items) {
    return Math.max(1, Math.ceil(items.length / FEED_PAGE_SIZE));
  }

  function renderFeedPage(kind) {
    var items = feedItems(kind);
    var pages = feedPageCount(items);
    var page = Math.min(Math.max(1, S.route.page || 1), pages);
    var slice = items.slice((page - 1) * FEED_PAGE_SIZE, page * FEED_PAGE_SIZE);
    var isEvents = kind === 'events';
    var title = isEvents ? '事件' : '最近活动';
    var sub = isEvents ? '告警自动生成 + 人工登记' : '节点上下线与告警动态';
    var body = isEvents
      ? (slice.length
        ? incidentTimeline(slice)
        : h(EP.ElEmpty, { description: '暂无事件记录', imageSize: 80 }))
      : activityList(slice);
    var pager = pages > 1 ? h('div', { class: 'pager-row' }, h(EP.ElPagination, {
      currentPage: page,
      pageSize: FEED_PAGE_SIZE,
      total: items.length,
      layout: 'prev, pager, next',
      pagerCount: 5,
      small: true,
      background: true,
      onCurrentChange: function (p) { location.hash = '#/' + kind + '/' + p; }
    })) : null;
    return [
      h('div', { class: 'feed-top' }, backButton()),
      panelCard({
        title: title,
        sub: sub,
        extra: h('span', null, '共 ' + items.length + ' 条 · 每页 ' + FEED_PAGE_SIZE + ' 条'),
        body: [body, pager]
      })
    ];
  }

  /* ---------------- 详情页 ---------------- */

  function ringCard(label, pct, sub) {
    pct = clampPct(pct);
    return h('div', { class: 'ring-card' }, [
      h(EP.ElProgress, {
        type: 'circle',
        percentage: Number(pct.toFixed(1)),
        width: 78,
        strokeWidth: 7,
        color: usageColor(pct),
        'show-text': true,
        format: function (p) { return fixed(p, 0) + '%'; }
      }),
      h('div', { class: 'ring-label' }, label),
      h('div', { class: 'ring-sub' }, sub || '当前使用率')
    ]);
  }

  function detailRings(dt) {
    var v = dt.visibility || {};
    if (!v.metrics) return null;
    var l = dt.latest || {};
    return h('div', { class: 'detail-rings' }, [
      ringCard('CPU', l.cpu, ''),
      ringCard('内存', l.memPct, l.memTotal ? bytes(l.memUsed) + ' / ' + bytes(l.memTotal) : ''),
      ringCard('磁盘', l.diskPct, l.diskTotal ? bytes(l.diskUsed) + ' / ' + bytes(l.diskTotal) : '')
    ]);
  }

  function renderDetailHead(dt) {
    var n = dt.node || {};
    var info = stateInfo({ state: dt.state, online: dt.online });
    var up = dt.uptime24;
    var upText = up != null ? fixed(up, 1) + '%' : '—';
    var upClass = up != null ? upCls(up) : 'none';
    var tags = [];
    if (n.group) tags.push(tag(n.group, 'info', { effect: 'plain' }));
    if (vis('region') && n.region) tags.push(tag(n.region, 'primary', { effect: 'plain' }));
    if (vis('provider') && n.provider) tags.push(tag(n.provider, 'info', { effect: 'plain' }));
    if (vis('tags')) {
      (n.tags || []).forEach(function (t) { tags.push(tag(t, 'success', { effect: 'plain' })); });
    }
    if (vis('billing') && n.price) {
      tags.push(tag((n.currency === 'CNY' ? '¥' : '$') + n.price + ' / 年', 'info', { effect: 'plain' }));
    }
    if (vis('expire') && n.expireAt) {
      tags.push(tag('到期 ' + n.expireAt, 'warning', { effect: 'plain' }));
    }
    tags.push(tag(dt.maintenance ? '维护中' : info.label, dt.maintenance ? 'warning' : info.tag));
    return h(EP.ElCard, { class: 'detail-head panel', shadow: 'never' }, {
      default: function () {
        return [
          h('div', { class: 'detail-title-row' }, [
            h('div', { class: 'detail-title' }, [
              h('div', { class: 'kicker' }, '节点详情'),
              h('h2', { class: 'detail-name' }, n.name || '节点'),
              h('div', { class: 'detail-tags' }, tags)
            ]),
            h('div', { class: 'detail-side' }, [
              h('span', { class: 'detail-big ' + upClass }, upText),
              h('span', { class: 'detail-side-label' }, '24 小时可用率')
            ])
          ]),
          detailRings(dt)
        ];
      }
    });
  }

  function rangeTabs() {
    return h(EP.ElRadioGroup, {
      class: 'range-tabs',
      size: 'small',
      modelValue: S.range,
      'onUpdate:modelValue': function (v) { setRange(v); }
    }, {
      default: function () {
        return [
          h(EP.ElRadioButton, { value: '24h' }, { default: function () { return '24 小时'; } }),
          h(EP.ElRadioButton, { value: '7d' }, { default: function () { return '7 天'; } }),
          h(EP.ElRadioButton, { value: '30d' }, { default: function () { return '30 天'; } })
        ];
      }
    });
  }

  function chartCard(title, points, color, fmt) {
    var last = points && points.length ? points[points.length - 1].v : null;
    return h('div', { class: 'chart-card' }, [
      h('div', { class: 'chart-title' }, [
        h('span', { class: 'name' }, title),
        h('b', null, last != null ? fmt(last) : '—')
      ]),
      h('div', { class: 'chart-box', innerHTML: areaChart(points, color, pctAxisFmt(points)) })
    ]);
  }

  function networkChart(dt) {
    var s = dt.series || {};
    var rx = s.netRx || [];
    var tx = s.netTx || [];
    var lastRx = rx.length ? rx[rx.length - 1].v : 0;
    var lastTx = tx.length ? tx[tx.length - 1].v : 0;
    return h('div', { class: 'chart-card wide' }, [
      h('div', { class: 'chart-title' }, [
        h('span', { class: 'name' }, '网络吞吐'),
        h('b', null, '↓ ' + rate(lastRx) + '　↑ ' + rate(lastTx))
      ]),
      h('div', {
        class: 'chart-box',
        innerHTML: multiAreaChart([rx, tx], ['var(--app-accent-2)', 'var(--el-color-primary)'], axisBytes)
      }),
      h('div', { class: 'chart-legend' }, [
        h('span', null, [h('i', { style: { background: 'var(--app-accent-2)' } }), '下行']),
        h('span', null, [h('i', { style: { background: 'var(--el-color-primary)' } }), '上行'])
      ])
    ]);
  }

  function renderTrends(dt) {
    var s = dt.series || {};
    var v = dt.visibility || {};
    var charts = [];
    if (v.metrics) {
      if (s.cpu && s.cpu.length > 1) {
        charts.push(chartCard('CPU 使用率', s.cpu, 'var(--el-color-primary)',
          function (x) { return fixed(x, 1) + '%'; }));
      }
      if (s.mem && s.mem.length > 1) {
        charts.push(chartCard('内存使用率', s.mem, 'var(--el-color-success)',
          function (x) { return fixed(x, 1) + '%'; }));
      }
      if (s.disk && s.disk.length > 1) {
        charts.push(chartCard('磁盘使用率', s.disk, 'var(--app-accent-2)',
          function (x) { return fixed(x, 1) + '%'; }));
      }
    }
    if (v.traffic && s.netRx && s.netRx.length > 1) {
      charts.push(networkChart(dt));
    }
    var extra = [
      S.detailLoading ? h('span', { class: 'panel-sub' }, '加载中…') : null,
      rangeTabs()
    ];
    if (!charts.length) {
      return panelCard({
        title: '性能趋势',
        extra: extra,
        body: h(EP.ElEmpty, { description: '当前展示配置下没有可公开的趋势数据', imageSize: 70 })
      });
    }
    return panelCard({
      title: '性能趋势',
      extra: extra,
      body: h('div', { class: 'chart-grid' }, charts)
    });
  }

  function availabilityCard(dt) {
    if (!block('history') || !dt.daily || !dt.daily.length) return null;
    var avg = upBarSummary(dt.daily);
    return panelCard({
      title: '可用率',
      sub: '过去 ' + dt.daily.length + ' 天',
      extra: h('span', null, ['平均 ', h('b', { class: 'mono' }, avg + (avg === '—' ? '' : '%'))]),
      body: [
        h('div', { class: 'upbar' }, upBar(dt.daily)),
        upBarFoot(dt.daily, avg)
      ]
    });
  }

  function detailServicesCard(dt) {
    if (!block('services') || !dt.services || !dt.services.length) return null;
    return panelCard({
      title: '服务延迟',
      sub: '延迟曲线只含成功探测',
      body: h('div', { class: 'service-detail' },
        dt.services.map(function (s) { return serviceItem(s, true); }))
    });
  }

  function runInfoCard(dt) {
    var l = dt.latest || {};
    var v = dt.visibility || {};
    var items = [];
    if (v.load) {
      items.push(descItem('负载',
        l.load1 != null ? fixed(l.load1, 2) : '—',
        '5 分钟 ' + fixed(l.load5, 2) + ' · 15 分钟 ' + fixed(l.load15, 2)));
    }
    if (v.uptime) items.push(descItem('运行时间', dur(l.uptime)));
    if (v.procs) items.push(descItem('进程数', l.procs != null ? String(l.procs) : '—'));
    if (v.tcpConn) items.push(descItem('TCP 连接', l.tcpConn != null ? String(l.tcpConn) : '—'));
    if (v.swap && l.swapTotal) {
      items.push(descItem('Swap', bytes(l.swapUsed), '/ ' + bytes(l.swapTotal)));
    }
    if (v.cores && l.cores) items.push(descItem('CPU 核心', String(l.cores)));
    if (v.traffic) {
      items.push(descItem('实时网络', '↓ ' + rate(l.netRx), '↑ ' + rate(l.netTx)));
    }
    if (dt.lastSeen) {
      items.push(descItem('最后上报', timeAgo(dt.lastSeen), fullTime(dt.lastSeen)));
    }
    if (!items.length) return null;
    return h(EP.ElCard, { class: 'panel detail-card', shadow: 'never' }, {
      header: function () { return panelHead('运行状态', '最近一次上报的数据'); },
      default: function () {
        return h(EP.ElDescriptions, {
          class: 'info-desc',
          column: S.width <= 720 ? 1 : 2,
          border: true,
          size: 'small'
        }, { default: function () { return items; } });
      }
    });
  }

  function hostCard(dt) {
    var host = dt.host || {};
    var pairs = [
      ['hostname', '主机名'], ['os', '操作系统'], ['kernel', '内核'], ['arch', '架构'],
      ['cpuModel', 'CPU 型号'], ['cores', '核心数'], ['virt', '虚拟化']
    ];
    var items = [];
    pairs.forEach(function (p) {
      var val = host[p[0]];
      if (val != null && val !== '') items.push(descItem(p[1], String(val)));
    });
    if (!items.length) return null;
    return h(EP.ElCard, { class: 'panel detail-card', shadow: 'never' }, {
      header: function () { return panelHead('主机信息', ''); },
      default: function () {
        return h(EP.ElDescriptions, {
          class: 'info-desc',
          column: S.width <= 720 ? 1 : 2,
          border: true,
          size: 'small'
        }, { default: function () { return items; } });
      }
    });
  }

  function skeletonBlock(height, marginBottom) {
    return h('div', {
      class: 'skel',
      style: { height: height + 'px', marginBottom: (marginBottom || 0) + 'px' }
    });
  }

  function skeletonDetail() {
    return h('div', { class: 'loading-page' }, [
      skeletonBlock(140, 14),
      skeletonBlock(220, 14),
      skeletonBlock(180, 0)
    ]);
  }

  function resultError(msg, retry) {
    return h('div', { class: 'result-page' }, h(EP.ElResult, {
      icon: 'error',
      title: '无法加载状态数据',
      subTitle: msg
    }, {
      extra: function () {
        return h(EP.ElButton, { type: 'primary', onClick: retry }, {
          default: function () { return '重新加载'; }
        });
      }
    }));
  }

  function renderDetailPage() {
    if (S.detailLoading && !S.detail) {
      return [h('div', { class: 'detail-top' }, backButton()), skeletonDetail()];
    }
    if (S.detailError && !S.detail) {
      return [
        h('div', { class: 'detail-top' }, backButton()),
        resultError(S.detailError, function () { fetchDetail(S.route.id, {}); })
      ];
    }
    var dt = S.detail;
    if (!dt) {
      return [h('div', { class: 'detail-top' }, backButton()), skeletonDetail()];
    }
    var out = [h('div', { class: 'detail-top' }, backButton())];
    if (S.detailError) {
      out.push(h(EP.ElAlert, {
        class: 'page-alert',
        type: 'warning',
        title: '刷新失败，以下为上次成功加载的数据',
        'show-icon': true,
        closable: false
      }));
    }
    if (dt.maintenance) {
      out.push(h(EP.ElAlert, {
        class: 'page-alert',
        type: 'info',
        title: '维护中：' + dt.maintenance,
        'show-icon': true,
        closable: false
      }));
    }
    if (!dt.lastSeen) {
      out.push(h(EP.ElAlert, {
        class: 'page-alert',
        type: 'info',
        title: '该节点尚未上报数据',
        description: '启动 Agent 后，这里会显示实时指标与趋势。',
        'show-icon': true,
        closable: false
      }));
    }
    out.push(renderDetailHead(dt));
    out.push(renderTrends(dt));
    var cards = [];
    var availability = availabilityCard(dt);
    if (availability) cards.push(availability);
    var services = detailServicesCard(dt);
    if (services) cards.push(services);
    var run = runInfoCard(dt);
    if (run) cards.push(run);
    var host = hostCard(dt);
    if (host) cards.push(host);
    if (cards.length) out.push(h('div', { class: 'detail-body' }, cards));
    return out;
  }

  /* ---------------- 根渲染 ---------------- */

  function renderMain() {
    if (S.route.view === 'node') return renderDetailPage();
    if (S.route.view === 'events' || S.route.view === 'activity') {
      if (!S.data) {
        if (S.loadError) return resultError(S.loadError, fetchList);
        return h('div', { class: 'loading-page' }, [skeletonBlock(120, 14), skeletonBlock(320, 0)]);
      }
      return renderFeedPage(S.route.view);
    }
    if (!S.data) {
      if (S.loadError) return resultError(S.loadError, fetchList);
      return h('div', { class: 'loading-page' }, [
        skeletonBlock(220, 14),
        h('div', { class: 'skel-row' }, [
          skeletonBlock(250, 0), skeletonBlock(250, 0), skeletonBlock(250, 0)
        ])
      ]);
    }
    return renderListPage();
  }

  function renderRoot() {
    return h(EP.ElConfigProvider, { locale: locale }, {
      default: function () {
        return [
          renderTopbar(),
          h('main', { class: 'page' }, renderMain()),
          renderFooter()
        ];
      }
    });
  }

  /* ---------------- 启动 ---------------- */

  var app = Vue.createApp({ render: renderRoot });
  if (locale) app.use(EP, { locale: locale });
  else app.use(EP);
  app.config.errorHandler = function (err) {
    if (window.console && console.error) console.error(err);
  };
  app.mount('#app');
  document.documentElement.classList.add('js-ok');

  window.addEventListener('hashchange', onHashChange);
  window.addEventListener('resize', function () { S.width = window.innerWidth || 1280; });
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) tick();
  });

  loadTheme();
  checkAdmin();
  fetchList();
  if (S.route.view === 'node' && S.route.id) fetchDetail(S.route.id, {});
  connectStream();
  setInterval(tick, 10000);
})();
