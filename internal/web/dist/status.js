/* 公开状态页脚本（外部文件，受 CSP script-src 'self' 保护）
 *
 * 页面只有两种视图：
 *   #/        节点总览（默认）
 *   #/n/<hid> 单个节点详情
 *
 * 数据刷新分两层：
 *   - 首屏/路由切换整页渲染；
 *   - 轮询刷新原地更新数值、进度条和曲线，避免每 15 秒整页闪一次。
 * 所有交互都用事件委托绑定在不变的容器上，符合 CSP script-src 'self'。
 */
(function () {
  'use strict';

  // 脚本能执行到这一行，就说明没有被 CSP/扩展拦截：
  // 立刻关掉 CSS 里的「脚本未能加载」兜底提示（见 statuspage.go 的 html.js-ok）。
  document.documentElement.classList.add('js-ok');

  /* ---------------- 转义与格式化 ---------------- */

  var esc = function (s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  };

  var fixed = function (v, n) {
    v = Number(v);
    if (!isFinite(v)) return '—';
    return v.toFixed(n == null ? 1 : n);
  };

  var dur = function (sec) {
    sec = Number(sec) || 0;
    if (sec <= 0) return '—';
    var d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
    if (d > 0) return d + ' 天 ' + h + ' 小时';
    if (h > 0) return h + ' 小时 ' + m + ' 分';
    return m + ' 分钟';
  };

  var UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  var bytes = function (n) {
    n = Number(n) || 0;
    var i = 0;
    while (n >= 1024 && i < UNITS.length - 1) { n /= 1024; i++; }
    return (i === 0 ? Math.round(n) : n.toFixed(n < 10 ? 1 : 0)) + ' ' + UNITS[i];
  };
  var rate = function (bps) {
    bps = Number(bps) || 0;
    if (bps < 1) return '0 B/s';
    return bytes(bps) + '/s';
  };

  var p2 = function (n) { return (n < 10 ? '0' : '') + n; };
  var clock = function (ts) {
    var d = new Date(ts * 1000);
    return p2(d.getHours()) + ':' + p2(d.getMinutes()) + ':' + p2(d.getSeconds());
  };
  var clockHM = function (ts) {
    var d = new Date(ts * 1000);
    return p2(d.getHours()) + ':' + p2(d.getMinutes());
  };
  var dayMD = function (ts) {
    var d = new Date(ts * 1000);
    return (d.getMonth() + 1) + ' 月 ' + d.getDate() + ' 日';
  };
  var fullTime = function (ts) {
    if (!ts) return '—';
    var d = new Date(ts * 1000);
    return d.getFullYear() + '-' + p2(d.getMonth() + 1) + '-' + p2(d.getDate()) + ' ' +
      p2(d.getHours()) + ':' + p2(d.getMinutes()) + ':' + p2(d.getSeconds());
  };
  var timeAgo = function (ts) {
    if (!ts) return '—';
    var diff = Math.max(0, Math.floor(Date.now() / 1000) - Number(ts));
    if (diff < 60) return '刚刚';
    if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
    if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
    return Math.floor(diff / 86400) + ' 天前';
  };

  // 可用率配色：越低越红。
  var upCls = function (v) { return v >= 99.5 ? 'ok' : v >= 95 ? 'warn' : 'crit'; };
  // 使用率配色：越高越红。
  var useCls = function (v) { return v >= 90 ? 'crit' : v >= 75 ? 'warn' : 'ok'; };
  // 延迟配色：超过 100ms 提醒，超过 300ms 视为异常。
  var latCls = function (v) { return v > 300 ? 'crit' : v > 100 ? 'warn' : 'ok'; };

  var LEVEL_CLS = { critical: 'crit', warn: 'warn', info: 'info' };
  var LEVEL_NAME = { critical: '严重', warn: '警告', info: '信息' };

  // 国家/地区分布：常用 ISO 3166-1 alpha-2 代码的中文名。
  // 未收录的代码直接显示原代码，不阻断渲染。
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
  var STATE_NAME = { online: '在线', stale: '数据延迟', offline: '离线', no_data: '待接入' };
  var STATE_DOT = { online: 'ok', stale: 'warn', offline: 'bad', no_data: 'off' };
  var STATE_BADGE = { online: 'ok', stale: 'warn', offline: 'bad', no_data: '' };

  function stateInfo(n) {
    var state = n.state || (n.online ? 'online' : 'offline');
    return {
      state: state,
      dot: STATE_DOT[state] || 'off',
      badge: STATE_BADGE[state] || '',
      label: STATE_NAME[state] || '未知',
    };
  }

  /* ---------------- 外观 ---------------- */

  var mq = window.matchMedia('(prefers-color-scheme: dark)');
  var serverTheme = null;
  var LS_THEME = 'rollcall-status-theme';
  var localMode = null;
  try { localMode = localStorage.getItem(LS_THEME); } catch (e) { /* 隐私模式下不可用 */ }
  if (localMode !== 'light' && localMode !== 'dark' && localMode !== 'auto') localMode = null;

  var THEME_ICONS = {
    auto: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><circle cx="12" cy="12" r="8"/><path d="M12 4a8 8 0 0 1 0 16z" fill="currentColor" stroke="none"/></svg>',
    light: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M19.1 4.9l-1.4 1.4M6.3 17.7l-1.4 1.4"/></svg>',
    dark: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M20.5 14.1A8.5 8.5 0 0 1 9.9 3.5 8.5 8.5 0 1 0 20.5 14.1z"/></svg>'
  };

  function themeMode() {
    var t = serverTheme || {};
    var mode = localMode;
    if (mode !== 'light' && mode !== 'dark') {
      mode = (t.mode === 'light' || t.mode === 'dark') ? t.mode : (mq.matches ? 'dark' : 'light');
    }
    return mode;
  }

  function paintTheme() {
    var t = serverTheme || {};
    var mode = themeMode();
    var root = document.documentElement;
    root.setAttribute('data-theme', mode);
    root.style.setProperty('--accent', mode === 'dark' ? (t.accentDark || '#60a5fa') : (t.accent || '#2563eb'));
    root.style.setProperty('--r', ((t.radius == null ? 14 : Number(t.radius))) + 'px');
    if (t.compact) root.setAttribute('data-density', 'compact');
    else root.removeAttribute('data-density');
    updateThemeButton();
  }

  function updateThemeButton() {
    var icon = document.getElementById('themeIcon');
    var label = document.getElementById('themeLabel');
    var btn = document.getElementById('themeBtn');
    if (!icon || !label || !btn) return;
    var effective = localMode === null ? 'auto' : localMode;
    icon.innerHTML = THEME_ICONS[effective] || THEME_ICONS.auto;
    label.textContent = effective === 'auto' ? '跟随系统' : effective === 'light' ? '浅色' : '深色';
    btn.setAttribute('aria-label', '当前外观：' + label.textContent + '，点击切换');
    btn.setAttribute('title', '外观：' + label.textContent);
  }

  function loadTheme() {
    fetch('/api/theme', { headers: { Accept: 'application/json' }, cache: 'no-store' })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function (t) {
        serverTheme = t;
        paintTheme();
        if (mq.addEventListener) {
          mq.addEventListener('change', function () { if (localMode === null) paintTheme(); });
        }
      })
      .catch(paintTheme);
  }

  /* ---------------- 图表 ---------------- */

  function validValues(points) {
    return (points || []).map(function (p) {
      if (p && typeof p === 'object') return Number(p.v);
      return Number(p);
    }).filter(function (v) { return isFinite(v); });
  }

  function pathOf(values, w, h, min, max, step) {
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

  // 卡片里的迷你曲线。
  function sparkSVG(values, color, opt) {
    opt = opt || {};
    var h = opt.height || 34, w = 100;
    var vals = validValues(values);
    if (vals.length < 2) {
      return '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none">' +
        '<line x1="0" y1="' + (h / 2) + '" x2="' + w + '" y2="' + (h / 2) +
        '" stroke="var(--line)" stroke-width="1" stroke-dasharray="2 4"/></svg>';
    }
    var min = Math.min.apply(null, vals), max = Math.max.apply(null, vals);
    var pad = (max - min) * 0.12 || 1;
    min -= pad; max += pad;
    if (min < 0 && opt.clampZero !== false) min = 0;
    var path = pathOf(vals, w, h - 4, min, max, w / (vals.length - 1));
    var gid = 'sg' + Math.random().toString(36).slice(2, 9);
    return '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none">' +
      '<defs><linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1">' +
      '<stop offset="0%" stop-color="' + color + '" stop-opacity=".26"/>' +
      '<stop offset="100%" stop-color="' + color + '" stop-opacity="0"/></linearGradient></defs>' +
      '<path d="' + path.d + ' L' + w + ',' + h + ' L0,' + h + ' Z" fill="url(#' + gid + ')"/>' +
      '<path d="' + path.d + '" fill="none" stroke="' + color + '" stroke-width="1.6" ' +
      'stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/></svg>';
  }

  // 纵轴刻度用 HTML 渲染：SVG 会随容器横向拉伸，文字放在 SVG 里会跟着变形。
  function plotHTML(axis, wide, svg) {
    return '<div class="plot' + (wide ? ' wide' : '') + '">' +
      '<div class="y-axis">' + axis + '</div>' +
      '<div class="plot-area">' + svg + '</div></div>';
  }

  // 详情页面积图，带网格和纵轴刻度。
  function areaSVG(points, color, opt) {
    opt = opt || {};
    var w = 640, h = 150, padR = 8, padT = 10, padB = 20;
    var vals = validValues(points);
    if (vals.length < 2) return '<div class="empty" style="padding:16px 0">暂无数据</div>';
    var iw = w - padR, ih = h - padT - padB;
    var min = Math.min.apply(null, vals), max = Math.max.apply(null, vals);
    var span = max - min;
    if (span < 0.0001) span = 1;
    min = Math.max(opt.clampZero === false ? -Infinity : 0, min - span * 0.14);
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
    var gid = 'ag' + Math.random().toString(36).slice(2, 9);
    var grid = '', axis = '';
    for (var g = 0; g <= 3; g++) {
      var gv = min + ((max - min) / 3) * g;
      var gy = y(gv);
      grid += '<line x1="0" y1="' + gy.toFixed(1) + '" x2="' + (w - padR) +
        '" y2="' + gy.toFixed(1) + '" stroke="var(--line)" stroke-width="1"/>';
      axis += '<span style="top:' + (gy / h * 100).toFixed(2) + '%">' +
        esc(opt.fmt ? opt.fmt(gv) : gv.toFixed(0)) + '</span>';
    }
    var svg = '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none">' +
      '<defs><linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1">' +
      '<stop offset="0%" stop-color="' + color + '" stop-opacity=".22"/>' +
      '<stop offset="100%" stop-color="' + color + '" stop-opacity="0"/></linearGradient></defs>' +
      grid +
      '<path d="' + area + '" fill="url(#' + gid + ')"/>' +
      '<path d="' + d + '" fill="none" stroke="' + color + '" stroke-width="1.8" ' +
      'stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/></svg>';
    return plotHTML(axis, false, svg);
  }

  // 双曲线图：网络下行/上行共用一个 Y 轴。
  function multiAreaSVG(seriesList, colors, opt) {
    opt = opt || {};
    var w = 640, h = 150, padR = 8, padT = 10, padB = 20;
    var all = [];
    var prepared = (seriesList || []).map(function (points) {
      var vals = validValues(points);
      all = all.concat(vals);
      return vals;
    });
    if (all.length < 2) return '<div class="empty" style="padding:16px 0">暂无数据</div>';
    var max = Math.max.apply(null, all);
    var min = 0;
    if (max <= 0) max = 1;
    max = max * 1.15;
    var iw = w - padR, ih = h - padT - padB;
    var y = function (v) { return padT + ih - ((v - min) / (max - min)) * ih; };
    var grid = '', axis = '';
    for (var g = 0; g <= 3; g++) {
      var gv = min + ((max - min) / 3) * g;
      var gy = y(gv);
      grid += '<line x1="0" y1="' + gy.toFixed(1) + '" x2="' + (w - padR) +
        '" y2="' + gy.toFixed(1) + '" stroke="var(--line)" stroke-width="1"/>';
      axis += '<span style="top:' + (gy / h * 100).toFixed(2) + '%">' +
        esc(opt.fmt ? opt.fmt(gv) : gv.toFixed(0)) + '</span>';
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
      var gid = 'mg' + idx + Math.random().toString(36).slice(2, 8);
      defs += '<linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1">' +
        '<stop offset="0%" stop-color="' + colors[idx] + '" stop-opacity=".18"/>' +
        '<stop offset="100%" stop-color="' + colors[idx] + '" stop-opacity="0"/></linearGradient>';
      paths += '<path d="' + d + ' L' + iw.toFixed(1) + ',' + (padT + ih) + ' L0,' +
        (padT + ih) + ' Z" fill="url(#' + gid + ')"/>' +
        '<path d="' + d + '" fill="none" stroke="' + colors[idx] + '" stroke-width="1.8" ' +
        'stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/>';
    });
    defs += '</defs>';
    var svg = '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none">' +
      defs + grid + paths + '</svg>';
    return plotHTML(axis, true, svg);
  }

  function ringSVG(pct, color) {
    pct = Math.max(0, Math.min(100, Number(pct) || 0));
    var r = 34, c = 2 * Math.PI * r, off = c * (1 - pct / 100);
    return '<svg viewBox="0 0 84 84" aria-hidden="true">' +
      '<circle class="rbg" cx="42" cy="42" r="' + r + '"/>' +
      '<circle class="rfg" cx="42" cy="42" r="' + r + '" stroke="' + color +
      '" stroke-dasharray="' + c.toFixed(1) + '" stroke-dashoffset="' + off.toFixed(1) + '"/></svg>' +
      '<div class="ring-center"><b>' + fixed(pct, pct > 0 && pct < 10 ? 1 : 0) + '%</b><small>使用率</small></div>';
  }

  function healthRingHTML(pct, cls) {
    pct = Math.max(0, Math.min(100, Number(pct) || 0));
    var r = 42, c = 2 * Math.PI * r, off = c * (1 - pct / 100);
    return '<svg viewBox="0 0 100 100" aria-hidden="true">' +
      '<circle class="ring-bg" cx="50" cy="50" r="' + r + '"/>' +
      '<circle id="healthFg" class="ring-fg ' + cls + '" cx="50" cy="50" r="' + r +
      '" stroke-dasharray="' + c.toFixed(1) + '" stroke-dashoffset="' + off.toFixed(1) + '"/></svg>' +
      '<div class="ring-center"><div class="ring-v" id="healthPct">' + fixed(pct, 0) + '%</div>' +
      '<div class="ring-l">在线率</div></div>';
  }

  // 48 段 24h 可用性色块。TS 是段结束时刻，与 uptime24 的映射一致。
  function barsHTML(list) {
    var now = Math.floor(Date.now() / 1000);
    var map = {};
    (list || []).forEach(function (x) { map[Math.round((now - x.t) / 1800)] = x.ok; });
    var html = '';
    for (var i = 47; i >= 0; i--) {
      var v = map[i];
      var cls = v === undefined ? 'gap' : (v ? '' : 'crit');
      var title = v === undefined ? '无数据' : (v ? '在线' : '离线');
      html += '<i class="' + cls + '" title="' + esc(title) + '"></i>';
    }
    return html;
  }

  function upBar(days) {
    if (!days || !days.length) return '';
    var html = '<div class="upbar">';
    days.forEach(function (d) {
      var cls = !d.has ? 'gap' : d.up >= 99.5 ? '' : d.up >= 95 ? 'warn' : 'crit';
      var tip = dayMD(d.ts) + '　' + (d.has ? d.up.toFixed(1) + '%' : '无数据');
      html += '<i class="' + cls + '" title="' + esc(tip) + '"></i>';
    });
    return html + '</div>';
  }

  function upBarSummary(days) {
    var sum = 0, n = 0;
    (days || []).forEach(function (x) { if (x.has) { sum += x.up; n++; } });
    return n ? (sum / n).toFixed(1) : '—';
  }

  /* ---------------- 状态 ---------------- */

  var app = document.getElementById('app');
  var DATA = null;
  var DETAILS = {};
  var DETAIL_ERR = {};
  var route = { view: 'list', id: null };
  var filters = { q: '', state: 'all', group: '' };
  var range = '24h';
  var rendered = false;
  var listLoading = false;
  var detailSeq = 0;
  var pollTimer = null;
  var stream = null;
  var streamTimer = null;
  var lastGroupsKey = '';
  var lastKpiSig = '';
  var searchTimer = null;
  var reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)');

  // 区块顺序：服务端下发（管理员配置），缺失时回退默认顺序。
  // 这里必须自己再补一遍缺失项：老服务端不认识 sections 时页面仍要完整。
  var DEFAULT_SEC_ORDER = ['nodes', 'services', 'map', 'history', 'incidents', 'activity'];
  var SEC_LABEL = {
    nodes: '节点列表', services: '服务探测', map: '地区分布',
    history: '可用率', incidents: '事件', activity: '最近活动',
  };
  // 列表页只保留少量预览，完整内容走独立页面，避免时间线把页面拉得过长。
  var INCIDENT_PREVIEW = 3;
  var ACTIVITY_PREVIEW = 5;
  // 独立列表页统一每页 10 条，避免把几十条时间线一次铺开。
  var FEED_PAGE_SIZE = 10;
  // ADMIN 为 true 时才渲染拖拽手柄：公开页是匿名可访问的，
  // 排序属于"对所有访客生效"的配置，不能由任意访客改动。
  var ADMIN = false;
  var SEC_HINTED = false;
  var secToastEl = null;
  var secToastTimer = null;

  function block(name) {
    if (!DATA || !DATA.blocks) return true;
    return !!DATA.blocks[name];
  }

  function vis(name, fallback) {
    if (!DATA || !DATA.visibility) return fallback !== false;
    return !!DATA.visibility[name];
  }

  function routeOf() {
    var h = String(location.hash || '').replace(/^#\/?/, '');
    var m = h.match(/^n\/(.+)$/);
    if (m) {
      try { return { view: 'node', id: decodeURIComponent(m[1]) }; }
      catch (e) { return { view: 'node', id: m[1] }; }
    }
    m = h.match(/^(events|activity)(?:\/(\d+))?$/);
    if (m) {
      return { view: m[1], id: null, page: m[2] ? Math.max(1, parseInt(m[2], 10)) : 1 };
    }
    return { view: 'list', id: null };
  }

  function showFatal(msg) {
    if (!rendered) {
      app.innerHTML = '<div class="banner bad"><span class="dot bad"></span>' +
        '<span class="txt">页面渲染出错：' + esc(msg) + '</span></div>';
    }
  }

  window.addEventListener('error', function (ev) {
    showFatal(ev.message || '未知错误');
  });
  window.addEventListener('unhandledrejection', function (ev) {
    showFatal(String(ev.reason && ev.reason.message || ev.reason || '未知错误'));
  });

  /* ---------------- 顶部与连接状态 ---------------- */

  function setConnection(state, text) {
    var dot = document.getElementById('liveDot');
    var label = document.getElementById('liveText');
    if (!dot || !label) return;
    dot.className = 'live-dot' + (state === 'on' ? ' on' : state === 'bad' ? ' bad' : '');
    label.textContent = text;
  }

  function updateHeader(d) {
    var site = (d && d.site) || '服务状态';
    document.getElementById('title').textContent = site;
    if (route.view === 'list') document.title = listTitle();
    else if (route.view === 'events') document.title = pageTitle('事件');
    else if (route.view === 'activity') document.title = pageTitle('最近活动');
    if (d) {
      document.getElementById('stamp').textContent = '更新于 ' + clock(d.generatedAt);
      var parts = [d.online + ' / ' + d.total + ' 在线'];
      if (d.stale > 0) parts.push(d.stale + ' 数据延迟');
      if (d.noData > 0) parts.push(d.noData + ' 待接入');
      document.getElementById('foot2').textContent = parts.join(' · ');
    }
  }

  function markOffline() {
    setConnection('bad', '连接中断');
    if (DATA) document.getElementById('stamp').textContent = '连接中断 · 上次更新 ' + clock(DATA.generatedAt);
  }

  /* ---------------- 数据加载 ---------------- */

  function fetchList() {
    if (listLoading) return;
    listLoading = true;
    // 没有超时的请求会让 listLoading 永远为 true，
    // 之后所有轮询都被 if (listLoading) return 挡掉 —— 页面看起来"不再刷新"。
    var ctl = new AbortController();
    var timer = setTimeout(function () { ctl.abort(); }, 12000);
    fetch('/api/status', { headers: { Accept: 'application/json' }, cache: 'no-store', signal: ctl.signal })
      .then(function (r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
      })
      .then(function (d) {
        clearTimeout(timer);
        listLoading = false;
        DATA = d;
        setConnection('on', '实时连接');
        updateHeader(d);
        if (!rendered) {
          route = routeOf();
          render();
        } else if (route.view === 'list') {
          patchList();
        } else if (route.view === 'node') {
          patchDetailHeader();
        } else if (route.view === 'events' || route.view === 'activity') {
          patchFeedPage();
        }
      })
      .catch(function (e) {
        clearTimeout(timer);
        listLoading = false;
        markOffline();
        if (!DATA) {
          app.innerHTML = '<div class="card"><div class="empty">数据加载失败：' + esc(e.message) +
            '<br><button class="hdr-btn" type="button" data-act="retry" style="margin-top:12px">重新加载</button></div></div>';
          rendered = true;
        }
      });
  }

  // 详情请求允许并发：快速切换节点或区间时，只采用最后一次发出的响应。
  // 用布尔锁会让第二次点击被直接丢弃，看起来像“点了没反应”。
  function fetchDetail(id, silent) {
    var useRange = range;
    var key = id + '|' + useRange;
    var seq = ++detailSeq;
    if (!silent) {
      renderDetailShell(id);
    }
    fetch('/api/status/node/' + encodeURIComponent(id) + '?range=' + encodeURIComponent(useRange), {
      headers: { Accept: 'application/json' }, cache: 'no-store'
    })
      .then(function (r) {
        if (!r.ok) throw new Error(r.status === 404 ? '节点不存在或详情页未开放' : 'HTTP ' + r.status);
        return r.json();
      })
      .then(function (dt) {
        DETAILS[key] = dt;
        delete DETAIL_ERR[key];
        if (seq !== detailSeq) return;
        if (route.view === 'node' && route.id === id && range === useRange) {
          renderDetail(dt, false);
        }
      })
      .catch(function (e) {
        DETAIL_ERR[key] = e.message;
        if (seq !== detailSeq) return;
        if (route.view === 'node' && route.id === id && range === useRange) {
          renderDetailError(id, e.message);
        }
      });
  }

  /* ---------------- 总览 ---------------- */

  function filteredNodes() {
    var nodes = (DATA && DATA.nodes) || [];
    var q = filters.q.trim().toLowerCase();
    return nodes.filter(function (n) {
      if (filters.state === 'online' && n.state !== 'online') return false;
      if (filters.state === 'attention' && n.state === 'online') return false;
      if (filters.group && (n.group || '未分组') !== filters.group) return false;
      if (!q) return true;
      var hay = [n.name, n.group, n.region, n.provider, n.os, n.arch].join(' ').toLowerCase();
      return hay.indexOf(q) >= 0;
    });
  }

  function groupsOf(nodes) {
    var out = [], seen = {};
    (nodes || []).forEach(function (n) {
      var g = n.group || '未分组';
      if (!seen[g]) { seen[g] = true; out.push(g); }
    });
    return out;
  }

  function overallInfo(d) {
    var total = d.total || 0;
    var offline = d.offline || 0, stale = d.stale || 0, noData = d.noData || 0;
    var svcDown = d.svcDown || 0;
    if (!total) {
      return { cls: 'info', label: '等待第一个节点接入', sub: '在管理后台创建节点并启动 Agent 后，这里会显示实时状态。' };
    }
    var issues = [];
    if (offline > 0) issues.push(offline + ' 个节点离线');
    if (stale > 0) issues.push(stale + ' 个节点数据延迟');
    if (noData > 0) issues.push(noData + ' 个节点待接入');
    if (svcDown > 0) issues.push(svcDown + ' 个服务异常');
    if (!issues.length) {
      return { cls: 'ok', label: '全部系统运行正常', sub: '所有节点与服务均在预期状态。' };
    }
    return {
      cls: (offline > 0 || svcDown > 0) ? 'crit' : 'warn',
      label: '部分系统需要关注',
      sub: issues.join(' · ') + '。'
    };
  }

  function kpi(label, value, unit, foot, key) {
    return '<div class="kpi"><div class="kpi-l">' + esc(label) + '</div>' +
      '<div class="kpi-v" data-kpi="' + esc(key) + '">' + value +
      (unit ? '<small>' + esc(unit) + '</small>' : '') + '</div>' +
      (foot ? '<div class="kpi-f" data-kf="' + esc(key) + '">' + foot + '</div>' : '') + '</div>';
  }

  function kpiValues(d) {
    var s = d.summary || {};
    var foot = [];
    if (d.stale > 0) foot.push(d.stale + ' 延迟');
    if (d.noData > 0) foot.push(d.noData + ' 待接入');
    var dataNodes = Math.max(0, (s.total || d.total || 0) - (s.noData || d.noData || 0));
    var html = kpi('在线节点', d.online + ' <small>/ ' + d.total + '</small>', '', foot.join(' · ') || '全部节点', 'online') +
      kpi('24h 可用率', dataNodes ? fixed(s.avgUptime24, 1) : '—', '%', '有数据节点平均', 'uptime');
    if (vis('metrics')) {
      html += kpi('平均 CPU', fixed(s.avgCpu, 0), '%', '', 'cpu') +
        kpi('平均内存', fixed(s.avgMem, 0), '%', '', 'mem') +
        kpi('平均磁盘', fixed(s.avgDisk, 0), '%', '', 'disk');
    }
    if (vis('traffic')) {
      html += kpi('实时下行', rate(s.trafficIn), '', '↑ ' + rate(s.trafficOut), 'net');
    }
    if (s.svcTotal) {
      html += kpi('服务健康', (s.svcTotal - s.svcDown) + ' <small>/ ' + s.svcTotal + '</small>', '',
        s.svcDown > 0 ? '<span class="crit">' + s.svcDown + ' 个异常</span>' : '<span class="ok">全部正常</span>', 'services');
    }
    if (s.activeAlert) {
      html += kpi('活跃告警', String(s.activeAlert), '', '<span class="warn">需关注</span>', 'alerts');
    }
    return html;
  }

  // KPI 的「数量」会随告警/服务出现或消失而变化；数量变了就重绘整条，
  // 数量不变时只改数值，保留数值变化的高亮动画。
  function kpiSignature(d) {
    var s = d.summary || {};
    return [vis('metrics'), vis('traffic'), s.svcTotal ? 1 : 0, s.activeAlert ? 1 : 0].join('|');
  }

  // KPI 卡片数量会随告警/服务出现而变化。auto-fit 在“卡片数比列数多 1”时
  // 会把最后一张单独甩到下一行（7+1），这里按实际宽度算出最多能放几列，
  // 放不下就按等分的行数重排：8 张在窄屏排 4+4，宽屏才排成一行。
  var KPI_MIN_W = 116; // 必须与 statuspage.go 中 .kpis 的 minmax 下限一致

  function balanceKpis() {
    var box = document.getElementById('heroKpis');
    if (!box) return;
    if (window.innerWidth <= 720) {
      // 窄屏交给 CSS 的 2 列规则；内联样式会盖住媒体查询，必须清掉。
      if (box.style.gridTemplateColumns) box.style.gridTemplateColumns = '';
      return;
    }
    var n = box.children.length;
    var w = box.clientWidth;
    if (!n || !w) return;
    var gap = 9;
    var maxCols = Math.max(1, Math.floor((w + gap) / (KPI_MIN_W + gap)));
    var next = '';
    if (n > maxCols) {
      var rows = Math.ceil(n / maxCols);
      var cols = Math.ceil(n / rows);
      next = 'repeat(' + cols + ', minmax(0, 1fr))';
    }
    if (box.style.gridTemplateColumns !== next) box.style.gridTemplateColumns = next;
  }

  function bannerHTML(d) {
    var html = '';
    if (d.announcement && d.announcement.enabled) {
      var lv = LEVEL_CLS[d.announcement.level] || 'info';
      html += '<div class="banner ' + lv + '"><span class="dot ' + lv + '"></span>' +
        '<span class="txt">' + esc(d.announcement.text) + '</span></div>';
    }
    if (d.maintenance) {
      html += '<div class="banner info"><span class="dot mt"></span><span class="txt">' +
        esc(d.maintenance.title || '计划维护') + '：' + esc(d.maintenance.text || '') + '</span></div>';
    }
    return html;
  }

  function healthNote(d) {
    var notes = [];
    if (d.stale) notes.push(d.stale + ' 台数据延迟');
    if (d.noData) notes.push(d.noData + ' 台待接入');
    return notes.length ? notes.join('，') : '状态良好';
  }

  function heroHTML(d) {
    var o = overallInfo(d);
    var total = d.total || 0;
    var pct = total ? (d.online || 0) / total * 100 : 0;
    var ringCls = !total ? 'none' : pct >= 99.9 ? 'ok' : pct >= 80 ? 'warn' : 'crit';
    return '<section class="card hero" id="hero">' +
      '<div class="hero-main">' +
        '<div class="hero-status">' +
          '<span class="status-orb ' + o.cls + '" id="statusOrb"></span>' +
          '<div><h2 class="hero-title" id="overallLabel">' + esc(o.label) + '</h2>' +
          '<div class="hero-sub" id="overallSub">' + esc(o.sub) + '</div></div>' +
        '</div>' +
        '<div class="health">' +
          '<div class="health-ring" id="healthRing" role="img" aria-label="节点在线率 ' + fixed(pct, 0) + '%">' +
            healthRingHTML(pct, ringCls) +
          '</div>' +
          '<div class="health-copy" id="healthCopy"><b>' + (d.online || 0) + ' / ' + total + '</b>节点在线<br>' +
            esc(healthNote(d)) + '</div>' +
        '</div>' +
      '</div>' +
      (block('summary') ? '<div class="kpis" id="heroKpis">' + kpiValues(d) + '</div>' : '') +
    '</section>';
  }

  function toolbarHTML() {
    var nodes = (DATA && DATA.nodes) || [];
    var groups = groupsOf(nodes);
    var online = 0, attention = 0;
    nodes.forEach(function (n) {
      if (n.state === 'online') online++; else attention++;
    });
    var opts = '<option value="">全部分组</option>' + groups.map(function (g) {
      return '<option value="' + esc(g) + '"' + (filters.group === g ? ' selected' : '') + '>' + esc(g) + '</option>';
    }).join('');
    return '<div class="toolbar" id="toolbar">' +
      '<label class="search"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="11" cy="11" r="7"/><path d="m20 20-3.2-3.2"/></svg>' +
        '<input id="searchInput" type="search" placeholder="搜索节点、分组、地区…" aria-label="搜索节点" autocomplete="off" value="' + esc(filters.q) + '"></label>' +
      '<div class="seg" id="stateFilter" role="group" aria-label="按状态筛选">' +
        '<button type="button" data-filter="all" class="' + (filters.state === 'all' ? 'on' : '') + '">全部 <b data-count="all">' + nodes.length + '</b></button>' +
        '<button type="button" data-filter="online" class="' + (filters.state === 'online' ? 'on' : '') + '">在线 <b data-count="online">' + online + '</b></button>' +
        '<button type="button" data-filter="attention" class="' + (filters.state === 'attention' ? 'on' : '') + '">非在线 <b data-count="attention">' + attention + '</b></button>' +
      '</div>' +
      (groups.length ? '<label class="select-wrap"><select id="groupFilter" aria-label="按分组筛选">' + opts + '</select></label>' : '') +
      '<div class="tool-count" id="toolCount">显示 <b>' + filteredNodes().length + '</b> / ' + nodes.length + ' 台</div>' +
    '</div>';
  }

  function tagsInner(n) {
    var tags = [];
    if (n.group) tags.push('<span class="tag soft">' + esc(n.group) + '</span>');
    if (vis('region') && n.region) tags.push('<span class="tag">' + esc(n.region) + '</span>');
    if (vis('provider') && n.provider) tags.push('<span class="tag soft">' + esc(n.provider) + '</span>');
    if (n.maintenanceTitle) tags.push('<span class="tag mt">维护中</span>');
    return tags.join('');
  }

  function tagsHTML(n) {
    var inner = tagsInner(n);
    return '<div class="nc-tags" data-f="tags"' + (inner ? '' : ' hidden') + '>' + inner + '</div>';
  }

  function factsHTML(n) {
    var facts = [];
    if (vis('os') && n.os) facts.push('<span>' + esc(n.os) + (vis('arch') && n.arch ? ' · ' + esc(n.arch) : '') + '</span>');
    if (vis('cores') && n.cores) facts.push('<span><b>' + n.cores + '</b> 核</span>');
    if (vis('uptime') && n.uptime) facts.push('<span>运行 <b>' + esc(dur(n.uptime)) + '</b></span>');
    if (vis('load') && n.load1 != null) facts.push('<span>负载 <b>' + fixed(n.load1, 2) + '</b></span>');
    if (n.state === 'offline' || n.state === 'stale') {
      facts.push('<span>最后上报 <b>' + esc(timeAgo(n.lastSeen)) + '</b></span>');
    }
    if (n.state === 'no_data') facts.push('<span>等待 Agent 首次上报</span>');
    return facts.length ? '<div class="nc-facts" data-f="facts">' + facts.join('') + '</div>' : '<div class="nc-facts" data-f="facts" hidden></div>';
  }

  function resRow(key, label, pct, sub) {
    var cls = useCls(pct);
    return '<div class="res-row" data-res="' + key + '"' + (sub ? ' title="' + esc(sub) + '"' : '') + '>' +
      '<span class="res-l">' + esc(label) + '</span>' +
      '<span class="res-bar"><i class="' + (cls === 'ok' ? '' : cls) + '" style="width:' + Math.max(0, Math.min(100, pct)) + '%"></i></span>' +
      '<span class="res-v ' + (cls === 'ok' ? '' : cls) + '">' + fixed(pct, 1) + '%</span></div>';
  }

  function resHTML(n) {
    if (!vis('metrics')) return '';
    var rows = [
      resRow('cpu', 'CPU', Number(n.cpu) || 0),
      resRow('mem', '内存', Number(n.mem) || 0, n.memTotal ? bytes(n.memUsed) + ' / ' + bytes(n.memTotal) : ''),
      resRow('disk', '磁盘', Number(n.disk) || 0, n.diskTotal ? bytes(n.diskUsed) + ' / ' + bytes(n.diskTotal) : '')
    ];
    return '<div class="res-list" data-f="res">' + rows.join('') + '</div>';
  }

  function netHTML(n) {
    if (!vis('traffic')) return '';
    var html = '<div class="nc-net" data-f="net">' +
      '<span class="net-item"><span class="net-arrow">↓</span><b>' + rate(n.netRx) + '</b></span>' +
      '<span class="net-item"><span class="net-arrow">↑</span><b>' + rate(n.netTx) + '</b></span>';
    if (n.totalRx || n.totalTx) {
      html += '<span class="net-total">累计 ↓' + bytes(n.totalRx) + ' · ↑' + bytes(n.totalTx) + '</span>';
    }
    return html + '</div>';
  }

  function nodeCardHTML(n, clickable) {
    var info = stateInfo(n);
    var up = n.uptime24;
    var upv = up != null ? fixed(up, 1) + '%' : '—';
    var upcls = up != null ? upCls(up) : 'none';
    var spark = (n.spark && n.spark.length > 1) ? sparkSVG(n.spark, useCls(n.cpu) === 'crit' ? 'var(--crit)' : useCls(n.cpu) === 'warn' ? 'var(--warn)' : 'var(--accent)') : '';
    var peak = (n.spark && n.spark.length) ? Math.max.apply(null, validValues(n.spark)) : null;
    var aria = n.name + '，' + info.label + '，24 小时可用率 ' + upv;
    var dataNode = clickable ? ' data-node="' + esc(n.hid || '') + '"' : '';
    return '<article class="node-card state-' + info.state + (clickable ? '' : ' no-link') + '"' +
      ' data-card="' + esc(n.hid || '') + '"' + dataNode +
      (clickable ? ' tabindex="0" role="link"' : '') +
      ' aria-label="' + esc(aria) + '">' +
      '<div class="nc-head">' +
        '<div class="nc-id"><span class="nc-dot ' + info.dot + '" data-f="dot"></span>' +
          '<div><div class="nc-name">' + esc(n.name) + '</div>' + tagsHTML(n) + '</div></div>' +
        '<div class="nc-side"><span class="state-badge ' + info.badge + '" data-f="badge">' + esc(info.label) + '</span>' +
          '<span class="nc-uptime">24h <b class="' + upcls + '" data-f="uptime24">' + upv + '</b></span></div>' +
      '</div>' +
      factsHTML(n) +
      resHTML(n) +
      netHTML(n) +
      '<div class="nc-foot">' +
        (vis('metrics') ? '<div class="spark-wrap"><div class="spark-head"><span>CPU 趋势</span><span data-f="peak">' +
          (peak != null ? '峰值 ' + fixed(peak, 0) + '%' : '') + '</span></div>' +
          '<div class="spark-box" data-f="spark">' + spark + '</div></div>' : '<div class="spark-wrap"></div>') +
        '<div class="avail"><div class="avail-v ' + upcls + '" data-f="avail">' + upv + '</div>' +
          '<div class="avail-l">24h 可用率</div>' +
          '<div class="mini-bars" data-f="bars">' + barsHTML(n.bars) + '</div></div>' +
      '</div>' +
    '</article>';
  }

  function gridHTML(nodes) {
    if (!nodes.length) {
      return '<div class="card empty-card"><div class="empty">没有匹配的节点<br>' +
        '<span style="font-size:11px">试试清除搜索或状态筛选</span></div></div>';
    }
    var groups = groupsOf(nodes);
    var html = '';
    groups.forEach(function (g) {
      var list = nodes.filter(function (n) { return (n.group || '未分组') === g; });
      if (groups.length > 1) {
        html += '<div class="group-head" style="grid-column:1/-1"><span>' + esc(g) + '</span><em>' + list.length + ' 台</em></div>';
      }
      list.forEach(function (n) { html += nodeCardHTML(n, block('nodeDetail')); });
    });
    return html;
  }

  // 区块外壳始终存在，数据到达/消失时只切换 hidden 与内容，
  // 避免「首次为空、后来有数据」时找不到容器而整段丢失。
  function servicesSection() {
    var list = block('services') && DATA.services ? DATA.services : [];
    var show = list.length > 0;
    return '<section class="card" id="servicesWrap"' + (show ? '' : ' hidden') + '>' +
      '<div class="card-h"><h2>服务探测</h2>' +
      '<span class="hint">HTTP / TCP / DNS 可用率、延迟与网络质量</span></div>' +
      '<div class="svc-list" id="svcList">' + serviceListHTML(list) + '</div></section>';
  }

  function serviceListHTML(list) {
    return (list || []).map(function (s, i) {
      return serviceHTML(s, i);
    }).join('');
  }

  // 地区分布：按国家代码聚合的横向条形图。
  // 不加载第三方地图瓦片，公开页保持零外部请求。
  function mapHTML(counts) {
    var keys = Object.keys(counts || {}).filter(function (k) { return counts[k] > 0; });
    if (!keys.length) return '';
    keys.sort(function (a, b) { return counts[b] - counts[a] || a.localeCompare(b); });
    var total = 0, max = 0;
    keys.forEach(function (k) { total += counts[k]; if (counts[k] > max) max = counts[k]; });
    return keys.map(function (k) {
      var n = counts[k];
      var pct = total ? n / total * 100 : 0;
      var w = max ? Math.max(4, n / max * 100) : 0;
      return '<div class="map-row" title="' + esc(countryName(k)) + '：' + n + ' 台">' +
        '<span class="map-flag">' + countryFlag(k) + '</span>' +
        '<span class="map-name">' + esc(countryName(k)) + '</span>' +
        '<span class="map-bar"><i style="width:' + w.toFixed(1) + '%"></i></span>' +
        '<span class="map-count"><b>' + n + '</b> 台 · ' + fixed(pct, 0) + '%</span></div>';
    }).join('');
  }

  function mapSection() {
    var counts = block('map') && DATA.map ? DATA.map : {};
    var html = mapHTML(counts);
    return '<section class="card" id="mapWrap"' + (html ? '' : ' hidden') + '>' +
      '<div class="card-h"><h2>地区分布</h2><span class="hint">按国家/地区统计节点</span></div>' +
      '<div class="map-list" id="mapList">' + html + '</div></section>';
  }

  function serviceHTML(s, i) {
    var has = s.has !== false;
    var dot = has ? (s.ok ? 'ok' : 'bad') : 'off';
    var lat = has && s.latencyMs != null ? s.latencyMs : null;
    var latTxt = lat != null ? (lat < 1 ? '&lt;1' : lat) + 'ms' : '—';
    var cls = lat != null ? latCls(lat) : 'none';
    var latSpark = s.latencySpark && s.latencySpark.length > 1 ?
      sparkSVG(s.latencySpark, 'var(--accent-2)', { height: 24 }) : '';
    var foot = [];
    foot.push(s.uptime != null ? fixed(s.uptime, 1) + '% 可用' : '暂无数据');
    if (s.failRate) foot.push('失败 ' + fixed(s.failRate, 2) + '%');
    if (s.lossPct) foot.push('丢包 ' + fixed(s.lossPct, 1) + '%');
    if (s.jitterMs) foot.push('抖动 ' + fixed(s.jitterMs, 1) + 'ms');
    return '<div class="svc" data-svc="' + i + '">' +
      '<span class="svc-dot ' + dot + '" data-f="svc-dot"></span>' +
      '<span class="svc-name">' + esc(s.name) + '</span>' +
      '<span class="svc-side"><span class="svc-lat ' + cls + '" data-f="svc-lat">' + latTxt + '</span>' +
        '<span data-f="svc-uptime">' + (s.uptime != null ? fixed(s.uptime, 1) + '%' : '—') + '</span></span>' +
      '<span class="svc-spark" data-f="svc-spark">' + latSpark + '</span>' +
      '<span class="svc-meta"><span data-f="svc-foot">' + esc(foot.join(' · ')) + '</span>' +
        '<span class="service-note">' + (has && !s.ok ? '探测失败' : '近 24 小时') + '</span></span>' +
    '</div>';
  }

  function historyBody(days) {
    if (!days.length) return '';
    var avg = upBarSummary(days);
    return '<div class="card-h"><h2>可用率</h2>' +
      '<span class="hint">过去 ' + (DATA.historyDays || days.length) + ' 天 · 每日一格</span>' +
      '<span class="hint" style="margin-left:auto">平均 <b class="mono">' + avg + '%</b></span></div>' +
      upBar(days) +
      '<div class="upbar-foot"><span>' + esc(dayMD(days[0].ts)) + '</span>' +
      '<span class="mid">' + avg + '%</span>' +
      '<span>' + esc(dayMD(days[days.length - 1].ts)) + '</span></div>';
  }

  function historySection() {
    var days = block('history') && DATA.history ? DATA.history : [];
    return '<section class="card" id="historyWrap"' + (days.length ? '' : ' hidden') + '>' +
      historyBody(days) + '</section>';
  }

  // moreLink 只在预览被截断时出现；具体条数留给列表页显示。
  function moreLink(href) {
    return '<a class="more-link" href="' + href + '">查看更多' +
      ' <span aria-hidden="true">→</span></a>';
  }

  function incidentsBody(items, preview) {
    var shown = preview ? items.slice(0, INCIDENT_PREVIEW) : items;
    var more = preview && items.length > shown.length ? moreLink('#/events') : '';
    return '<div class="card-h"><h2>事件</h2>' +
      '<span class="hint">告警自动生成 + 人工登记</span>' + more + '</div>' + timelineHTML(shown);
  }

  function incidentsSection() {
    var show = block('incidents');
    var items = show ? (DATA.incidents || []) : [];
    return '<section class="card" id="incidentsWrap"' + (show ? '' : ' hidden') + '>' +
      incidentsBody(items, true) + '</section>';
  }

  var ACTIVITY_KIND = {
    node_online: { label: '上线', cls: 'ok' },
    node_offline: { label: '离线', cls: 'crit' },
    alert_opened: { label: '告警', cls: 'crit' },
    alert_resolved: { label: '恢复', cls: 'ok' }
  };

  function activityHTML(items) {
    if (!items || !items.length) return '<div class="empty">暂无活动记录</div>';
    return '<div class="act-list">' + items.map(function (ev) {
      var meta = ACTIVITY_KIND[ev.kind] || { label: '动态', cls: 'info' };
      return '<div class="act-item">' +
        '<span class="act-dot ' + meta.cls + '"></span>' +
        '<div class="act-main"><div class="act-text">' + esc(ev.text) + '</div>' +
          '<div class="act-meta"><span class="act-badge ' + meta.cls + '">' + esc(meta.label) + '</span>' +
            '<span>' + esc(timeAgo(ev.ts)) + '</span>' +
            '<span class="act-time">' + esc(fullTime(ev.ts)) + '</span></div></div></div>';
    }).join('') + '</div>';
  }

  function activityBody(items, preview) {
    var shown = preview ? items.slice(0, ACTIVITY_PREVIEW) : items;
    var more = preview && items.length > shown.length ? moreLink('#/activity') : '';
    return '<div class="card-h"><h2>最近活动</h2>' +
      '<span class="hint">节点上下线与告警动态</span>' + more + '</div>' +
      activityHTML(shown);
  }

  function activitySection() {
    var show = block('activity');
    return '<section class="card" id="activityWrap"' + (show ? '' : ' hidden') + '>' +
      (show ? activityBody(DATA.activity || [], true) : '') + '</section>';
  }

  // 事件与活动的完整列表：列表页只放预览，这里按页展示服务端下发的全部条目。
  function feedItems(kind) {
    if (!DATA) return [];
    return kind === 'events' ? (DATA.incidents || []) : (DATA.activity || []);
  }

  function feedPageCount(total) {
    return Math.max(1, Math.ceil(total / FEED_PAGE_SIZE));
  }

  function feedPagerHTML(kind, page, pages) {
    if (pages <= 1) return '';
    var prev = page > 1
      ? '<a class="pager-btn" href="#/' + kind + '/' + (page - 1) + '" rel="prev">← 上一页</a>'
      : '<span class="pager-btn disabled" aria-disabled="true">← 上一页</span>';
    var next = page < pages
      ? '<a class="pager-btn" href="#/' + kind + '/' + (page + 1) + '" rel="next">下一页 →</a>'
      : '<span class="pager-btn disabled" aria-disabled="true">下一页 →</span>';
    return '<nav class="pager" aria-label="分页">' + prev +
      '<span class="pager-info">第 ' + page + ' / ' + pages + ' 页</span>' + next + '</nav>';
  }

  function feedCardHTML(kind, page) {
    var isEvents = kind === 'events';
    var items = feedItems(kind);
    var pages = feedPageCount(items.length);
    var cur = Math.min(Math.max(1, page || 1), pages);
    var body = items.slice((cur - 1) * FEED_PAGE_SIZE, cur * FEED_PAGE_SIZE);
    return '<div class="card-h"><h2>' + (isEvents ? '事件' : '最近活动') + '</h2>' +
      '<span class="hint">' + (isEvents ? '告警自动生成 + 人工登记' : '节点上下线与告警动态') + '</span>' +
      '<span class="hint" style="margin-left:auto">共 ' + items.length + ' 条 · 每页 ' +
        FEED_PAGE_SIZE + ' 条</span></div>' +
      (isEvents ? timelineHTML(body) : activityHTML(body)) +
      feedPagerHTML(kind, cur, pages);
  }

  function feedPageHTML(kind, page) {
    return '<a class="back" href="#/">← 返回总览</a>' +
      '<section class="card" id="feedPage">' + feedCardHTML(kind, page) + '</section>';
  }

  function feedSignature(kind) {
    return JSON.stringify(feedItems(kind));
  }

  var lastFeedSig = '';

  // 数据变少时当前页可能越界，把地址栏收敛到最后一页。
  function clampFeedPage(kind) {
    var pages = feedPageCount(feedItems(kind).length);
    var page = Math.min(Math.max(1, route.page || 1), pages);
    if (page !== route.page) {
      route.page = page;
      try { history.replaceState(null, '', '#/' + kind + '/' + page); } catch (e) { /* 忽略 */ }
    }
    return page;
  }

  function renderFeedPage(kind) {
    var page = clampFeedPage(kind);
    app.innerHTML = feedPageHTML(kind, page);
    app.setAttribute('data-view', kind);
    app.removeAttribute('data-detail-id');
    app.removeAttribute('aria-busy');
    document.body.classList.remove('sec-admin');
    lastFeedSig = feedSignature(kind) + '|' + page;
    document.title = pageTitle(kind === 'events' ? '事件' : '最近活动');
  }

  // 轮询时只替换卡片内容：整页重绘会把滚动位置一起丢掉。
  function patchFeedPage() {
    var kind = route.view;
    var page = clampFeedPage(kind);
    var sig = feedSignature(kind) + '|' + page;
    if (sig === lastFeedSig) return;
    var card = document.getElementById('feedPage');
    if (!card) { renderFeedPage(kind); return; }
    lastFeedSig = sig;
    card.innerHTML = feedCardHTML(kind, page);
  }

  function timelineHTML(items) {
    if (!items.length) return '<div class="empty">过去一段时间没有记录到事件</div>';
    var byDay = {}, order = [];
    items.forEach(function (it) {
      var d = new Date(it.startAt * 1000);
      var key = d.getFullYear() + '-' + (d.getMonth() + 1) + '-' + d.getDate();
      if (!byDay[key]) { byDay[key] = []; order.push(key); }
      byDay[key].push(it);
    });
    var html = '<div class="tl">';
    order.forEach(function (k) {
      html += '<div class="tl-day">' + esc(dayMD(byDay[k][0].startAt)) + '</div>';
      byDay[k].forEach(function (it) {
        var cls = LEVEL_CLS[it.level] || 'info';
        var ongoing = !it.endAt;
        html += '<div class="tl-item ' + cls + '"><div class="tl-t">' + esc(it.title) +
          '<span class="tl-badge">' + esc(LEVEL_NAME[it.level] || '信息') + '</span>' +
          (ongoing ? '<span class="tl-badge">进行中</span>' : '') +
          (it.auto ? '' : '<span class="tl-badge">人工</span>') + '</div>' +
          '<div class="tl-meta">' + clockHM(it.startAt) +
          (ongoing ? ' 起，持续 ' + dur(it.duration) : ' → ' + clockHM(it.endAt) + '，持续 ' + dur(it.duration)) +
          (it.nodeNames && it.nodeNames.length ? '　' + esc(it.nodeNames.join('、')) : '') + '</div>' +
          (it.body ? '<div class="tl-body">' + esc(it.body) + '</div>' : '') +
        '</div>';
      });
    });
    return html + '</div>';
  }

  function sectionOrder() {
    var raw = (DATA && DATA.sections) || [];
    var out = [];
    raw.forEach(function (k) {
      if (DEFAULT_SEC_ORDER.indexOf(k) >= 0 && out.indexOf(k) < 0) out.push(k);
    });
    DEFAULT_SEC_ORDER.forEach(function (k) {
      if (out.indexOf(k) < 0) out.push(k);
    });
    return out;
  }

  function secWrap(key, inner) {
    var label = SEC_LABEL[key] || key;
    var handle = ADMIN
      ? '<button class="sec-handle" type="button" data-handle="' + key +
        '" aria-label="拖动调整「' + label + '」模块位置，或按 Alt+上下方向键移动" title="拖动调整位置">⠿</button>'
      : '';
    return '<div class="sec" data-sec="' + key + '">' + handle + inner + '</div>';
  }

  function secToast(msg, bad) {
    if (!secToastEl) {
      secToastEl = document.createElement('div');
      secToastEl.className = 'sec-toast';
      document.body.appendChild(secToastEl);
    }
    secToastEl.textContent = msg;
    secToastEl.className = 'sec-toast' + (bad ? ' bad' : '');
    clearTimeout(secToastTimer);
    secToastTimer = setTimeout(function () { secToastEl.className = 'sec-toast hidden'; }, 2400);
  }

  function currentOrder() {
    return Array.prototype.map.call(app.querySelectorAll('.sec'), function (el) {
      return el.getAttribute('data-sec');
    });
  }

  // saveSectionOrder 把顺序写回服务端配置：对所有访客生效，刷新/换设备都保持。
  // 只存 localStorage 会让"换台电脑就变回去"，看起来像没保存。
  function saveSectionOrder(order) {
    secToast('正在保存排序…');
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
          headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'XMLHttpRequest' },
          body: JSON.stringify(cfg),
        });
      })
      .then(function (r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
      })
      .then(function (d) {
        var saved = (d && d.config && d.config.sections) || order;
        DATA.sections = saved;
        secToast('排序已保存，对所有访客生效');
      })
      .catch(function (e) {
        secToast('保存失败：' + e.message, true);
        // 回滚：重新按服务端顺序渲染，避免界面显示的和实际保存的不一致
        render();
      });
  }

  // bindSectionDrag 用 Pointer Events 实现拖动（鼠标/触屏通用），
  // 不做 placeholder：直接移动元素本身，松手后按 DOM 顺序落库。
  function bindSectionDrag() {
    var dragging = null;
    var moved = false;

    function finish() {
      if (!dragging) return null;
      var el = dragging;
      dragging = null;
      el.classList.remove('sec-dragging');
      document.body.classList.remove('sec-sorting');
      return el;
    }

    app.querySelectorAll('.sec-handle').forEach(function (h) {
      h.addEventListener('pointerdown', function (e) {
        if (e.button !== 0) return;
        e.preventDefault();
        dragging = h.closest('.sec');
        moved = false;
        try { h.setPointerCapture(e.pointerId); } catch (err) { /* 老浏览器忽略 */ }
        dragging.classList.add('sec-dragging');
        document.body.classList.add('sec-sorting');
      });

      h.addEventListener('pointermove', function (e) {
        if (!dragging) return;
        moved = true;
        var after = null;
        var secs = Array.prototype.slice.call(app.querySelectorAll('.sec'));
        for (var i = 0; i < secs.length; i++) {
          var s = secs[i];
          if (s === dragging) continue;
          var r = s.getBoundingClientRect();
          if (e.clientY < r.top + r.height / 2) { after = s; break; }
        }
        if (after) app.insertBefore(dragging, after);
        else app.appendChild(dragging);
      });

      h.addEventListener('pointerup', function () {
        if (!finish()) return;
        if (moved) saveSectionOrder(currentOrder());
      });

      h.addEventListener('pointercancel', function () {
        finish();
      });

      // 键盘可达：聚焦手柄后 Alt+↑/↓ 移动一格
      h.addEventListener('keydown', function (e) {
        if (!e.altKey || (e.key !== 'ArrowUp' && e.key !== 'ArrowDown')) return;
        e.preventDefault();
        var sec = h.closest('.sec');
        if (e.key === 'ArrowUp' && sec.previousElementSibling) {
          app.insertBefore(sec, sec.previousElementSibling);
        } else if (e.key === 'ArrowDown' && sec.nextElementSibling) {
          app.insertBefore(sec.nextElementSibling, sec);
        } else {
          return;
        }
        sec.focus();
        saveSectionOrder(currentOrder());
      });
    });
  }

  function listHTML() {
    var nodes = filteredNodes();
    // 工具栏跟随节点列表一起移动：它只作用于节点网格，
    // 若固定在顶部而网格被拖到下面，筛选器会指向看不见的内容。
    var blocks = {
      nodes: toolbarHTML() +
        '<section class="node-grid" id="nodeGrid" aria-live="polite">' + gridHTML(nodes) + '</section>',
      services: servicesSection(),
      map: mapSection(),
      history: historySection(),
      incidents: incidentsSection(),
      activity: activitySection(),
    };
    return '<div id="banners">' + bannerHTML(DATA) + '</div>' + heroHTML(DATA) +
      sectionOrder().map(function (k) { return secWrap(k, blocks[k] || ''); }).join('');
  }

  function renderList() {
    app.innerHTML = listHTML();
    app.setAttribute('data-view', 'list');
    app.removeAttribute('data-detail-id');
    app.removeAttribute('aria-busy');
    document.body.classList.toggle('sec-admin', ADMIN);
    if (ADMIN) {
      bindSectionDrag();
      // 只在本次会话第一次渲染时提示，避免每次轮询都弹
      if (!SEC_HINTED) {
        SEC_HINTED = true;
        setTimeout(function () { secToast('可拖动区块右上角的手柄调整模块顺序'); }, 900);
      }
    }
    var grid = document.getElementById('nodeGrid');
    if (grid) grid.setAttribute('data-sig', gridSignature(filteredNodes()));
    lastGroupsKey = groupsOf((DATA && DATA.nodes) || []).join('|');
    lastKpiSig = kpiSignature(DATA);
    bindToolbar();
    balanceKpis();
  }

  function render() {
    if (!DATA) return;
    try {
      route = routeOf();
      if (route.view === 'node') {
        document.querySelectorAll('.node-card').forEach(function (el) { el.blur(); });
        renderDetailShell(route.id);
        var dt = DETAILS[route.id + '|' + range];
        if (dt) renderDetail(dt, false);
        else if (DETAIL_ERR[route.id + '|' + range]) renderDetailError(route.id, DETAIL_ERR[route.id + '|' + range]);
        else fetchDetail(route.id, true);
      } else if (route.view === 'events' || route.view === 'activity') {
        renderFeedPage(route.view);
      } else {
        renderList();
      }
      rendered = true;
      updateHeader(DATA);
    } catch (err) {
      showFatal(String(err && err.message || err));
    }
  }

  /* ---------------- 增量刷新 ---------------- */

  function gridSignature(nodes) {
    var v = DATA && DATA.visibility ? DATA.visibility : {};
    // 名称也进签名：patchCard 不重写 .nc-name，改名后只有整格重绘才会显示新名字。
    return nodes.map(function (n) { return (n.hid || '') + ':' + (n.name || ''); }).join(',') + '|' +
      groupsOf(nodes).join(',') + '|' + [v.metrics, v.load, v.uptime, v.traffic, v.os, v.arch,
      v.cores, v.region, v.provider, block('nodeDetail')].join(',');
  }

  function renderGridOnly() {
    var nodes = filteredNodes();
    var grid = document.getElementById('nodeGrid');
    if (!grid) { renderList(); return; }
    grid.innerHTML = gridHTML(nodes);
    var sig = gridSignature(nodes);
    grid.setAttribute('data-sig', sig);
    updateToolbarCounts();
  }

  function updateToolbarCounts() {
    var nodes = (DATA && DATA.nodes) || [];
    var online = 0;
    nodes.forEach(function (n) { if (n.state === 'online') online++; });
    var counts = { all: nodes.length, online: online, attention: nodes.length - online };
    document.querySelectorAll('[data-count]').forEach(function (el) {
      var k = el.getAttribute('data-count');
      if (counts[k] != null) el.textContent = counts[k];
    });
    var tc = document.getElementById('toolCount');
    if (tc) tc.innerHTML = '显示 <b>' + filteredNodes().length + '</b> / ' + nodes.length + ' 台';
    document.querySelectorAll('#stateFilter button').forEach(function (el) {
      var on = el.getAttribute('data-filter') === filters.state;
      el.classList.toggle('on', on);
      el.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
  }

  function setKPI(key, html) {
    var el = document.querySelector('[data-kpi="' + key + '"]');
    if (!el) return;
    if (el.innerHTML === html) return;
    el.innerHTML = html;
    el.classList.remove('changed');
    void el.offsetWidth;
    el.classList.add('changed');
  }

  function patchHero() {
    var o = overallInfo(DATA);
    var orb = document.getElementById('statusOrb');
    if (orb) orb.className = 'status-orb ' + o.cls;
    var lbl = document.getElementById('overallLabel');
    var sub = document.getElementById('overallSub');
    if (lbl) lbl.textContent = o.label;
    if (sub) sub.textContent = o.sub;
    var total = DATA.total || 0;
    var pct = total ? (DATA.online || 0) / total * 100 : 0;
    var cls = !total ? 'none' : pct >= 99.9 ? 'ok' : pct >= 80 ? 'warn' : 'crit';
    var fg = document.getElementById('healthFg');
    if (fg) {
      var c = 2 * Math.PI * 42;
      fg.setAttribute('stroke-dashoffset', (c * (1 - pct / 100)).toFixed(1));
      fg.setAttribute('class', 'ring-fg ' + cls);
    }
    var hp = document.getElementById('healthPct');
    if (hp) hp.textContent = fixed(pct, 0) + '%';
    var ring = document.getElementById('healthRing');
    if (ring) ring.setAttribute('aria-label', '节点在线率 ' + fixed(pct, 0) + '%');
    var hc = document.getElementById('healthCopy');
    if (hc) {
      hc.innerHTML = '<b>' + (DATA.online || 0) + ' / ' + total + '</b>节点在线<br>' +
        esc(healthNote(DATA));
    }
    // 页脚统计
    var parts = [DATA.online + ' / ' + DATA.total + ' 在线'];
    if (DATA.stale > 0) parts.push(DATA.stale + ' 数据延迟');
    if (DATA.noData > 0) parts.push(DATA.noData + ' 待接入');
    var foot = document.getElementById('foot2');
    if (foot) foot.textContent = parts.join(' · ');
    if (!block('summary')) return;
    var sig = kpiSignature(DATA);
    if (sig !== lastKpiSig) {
      var box = document.getElementById('heroKpis');
      if (box) { box.innerHTML = kpiValues(DATA); balanceKpis(); }
      lastKpiSig = sig;
      return;
    }
    var s = DATA.summary || {};
    setKPI('online', DATA.online + ' <small>/ ' + DATA.total + '</small>');
    var kfOnline = document.querySelector('[data-kf="online"]');
    if (kfOnline) {
      var note = [];
      if (DATA.stale > 0) note.push(DATA.stale + ' 延迟');
      if (DATA.noData > 0) note.push(DATA.noData + ' 待接入');
      kfOnline.textContent = note.join(' · ') || '全部节点';
    }
    if (document.querySelector('[data-kpi="uptime"]')) {
      var dataNodes = Math.max(0, (s.total || DATA.total || 0) - (s.noData || DATA.noData || 0));
      setKPI('uptime', (dataNodes ? fixed(s.avgUptime24, 1) : '—') + '<small>%</small>');
    }
    if (vis('metrics')) {
      setKPI('cpu', fixed(s.avgCpu, 0) + '<small>%</small>');
      setKPI('mem', fixed(s.avgMem, 0) + '<small>%</small>');
      setKPI('disk', fixed(s.avgDisk, 0) + '<small>%</small>');
    }
    if (vis('traffic')) {
      setKPI('net', rate(s.trafficIn));
      var kfNet = document.querySelector('[data-kf="net"]');
      if (kfNet) kfNet.textContent = '↑ ' + rate(s.trafficOut);
    }
    if (s.svcTotal) {
      setKPI('services', (s.svcTotal - s.svcDown) + ' <small>/ ' + s.svcTotal + '</small>');
      var kfSvc = document.querySelector('[data-kf="services"]');
      if (kfSvc) {
        kfSvc.innerHTML = s.svcDown > 0 ?
          '<span class="crit">' + s.svcDown + ' 个异常</span>' : '<span class="ok">全部正常</span>';
      }
    }
    if (s.activeAlert) setKPI('alerts', String(s.activeAlert));
  }

  function patchCard(card, n) {
    var info = stateInfo(n);
    card.className = 'node-card state-' + info.state + (block('nodeDetail') ? '' : ' no-link');
    if (block('nodeDetail')) {
      card.setAttribute('data-node', n.hid || '');
      card.setAttribute('tabindex', '0');
      card.setAttribute('role', 'link');
    } else {
      card.removeAttribute('data-node');
      card.removeAttribute('tabindex');
      card.removeAttribute('role');
    }
    card.setAttribute('aria-label', n.name + '，' + info.label + '，24 小时可用率 ' +
      (n.uptime24 != null ? fixed(n.uptime24, 1) + '%' : '—'));
    var dot = card.querySelector('[data-f="dot"]');
    if (dot) dot.className = 'nc-dot ' + info.dot;
    var badge = card.querySelector('[data-f="badge"]');
    if (badge) { badge.className = 'state-badge ' + info.badge; badge.textContent = info.label; }
    var tags = card.querySelector('[data-f="tags"]');
    if (tags) {
      var ti = tagsInner(n);
      tags.innerHTML = ti;
      tags.hidden = !ti;
    }
    var up = n.uptime24;
    var upTxt = up != null ? fixed(up, 1) + '%' : '—';
    var upcls = up != null ? upCls(up) : 'none';
    var upEl = card.querySelector('[data-f="uptime24"]');
    if (upEl) { upEl.textContent = upTxt; upEl.className = upcls; }
    var facts = card.querySelector('[data-f="facts"]');
    if (facts) {
      var fh = factsHTML(n);
      var inner = fh.replace(/^<div[^>]*>|<\/div>$/g, '');
      facts.innerHTML = inner;
      facts.hidden = !inner;
    }
    if (vis('metrics')) {
      ['cpu', 'mem', 'disk'].forEach(function (k) {
        setCardBar(card, k, Number(n[k]) || 0, n);
      });
    }
    var net = card.querySelector('[data-f="net"]');
    if (net) {
      var vals = net.querySelectorAll('.net-item b');
      if (vals.length === 2) {
        vals[0].textContent = rate(n.netRx);
        vals[1].textContent = rate(n.netTx);
      }
      var totTxt = (n.totalRx || n.totalTx) ? '累计 ↓' + bytes(n.totalRx) + ' · ↑' + bytes(n.totalTx) : '';
      var tot = net.querySelector('.net-total');
      if (!tot && totTxt) {
        tot = document.createElement('span');
        tot.className = 'net-total';
        net.appendChild(tot);
      }
      if (tot) {
        tot.textContent = totTxt;
        tot.hidden = !totTxt;
      }
    }
    var sparkBox = card.querySelector('[data-f="spark"]');
    if (sparkBox) {
      sparkBox.innerHTML = (n.spark && n.spark.length > 1) ?
        sparkSVG(n.spark, useCls(n.cpu) === 'crit' ? 'var(--crit)' : useCls(n.cpu) === 'warn' ? 'var(--warn)' : 'var(--accent)') : '';
      var peak = (n.spark && n.spark.length) ? Math.max.apply(null, validValues(n.spark)) : null;
      var peakEl = card.querySelector('[data-f="peak"]');
      if (peakEl) peakEl.textContent = peak != null ? '峰值 ' + fixed(peak, 0) + '%' : '';
    }
    var avail = card.querySelector('[data-f="avail"]');
    if (avail) { avail.textContent = upTxt; avail.className = 'avail-v ' + upcls; }
    var bars = card.querySelector('[data-f="bars"]');
    if (bars) {
      var sig = (n.bars || []).map(function (b) { return b.ok ? 1 : 0; }).join('');
      if (bars.getAttribute('data-sig') !== sig) {
        bars.innerHTML = barsHTML(n.bars);
        bars.setAttribute('data-sig', sig);
      }
    }
  }

  function setCardBar(card, key, pct, n) {
    var row = card.querySelector('[data-res="' + key + '"]');
    if (!row) return;
    var cls = useCls(pct);
    var fill = row.querySelector('.res-bar i');
    var val = row.querySelector('.res-v');
    if (fill) {
      fill.style.width = Math.max(0, Math.min(100, pct)) + '%';
      fill.className = cls === 'ok' ? '' : cls;
    }
    if (val) {
      val.textContent = fixed(pct, 1) + '%';
      val.className = 'res-v ' + (cls === 'ok' ? '' : cls);
    }
    var sub = '';
    if (key === 'mem' && n.memTotal) sub = bytes(n.memUsed) + ' / ' + bytes(n.memTotal);
    if (key === 'disk' && n.diskTotal) sub = bytes(n.diskUsed) + ' / ' + bytes(n.diskTotal);
    row.title = sub;
  }

  function patchList() {
    if (!DATA) return;
    var nodes = filteredNodes();
    var sig = gridSignature(nodes);
    var grid = document.getElementById('nodeGrid');
    if (!grid || grid.getAttribute('data-sig') !== sig) {
      // 结构变化（节点增减、可见性开关变化、筛选结果变化）才重绘网格。
      renderGridOnly();
      if (grid) grid.setAttribute('data-sig', sig);
    } else {
      var byHid = {};
      nodes.forEach(function (n) { byHid[n.hid] = n; });
      grid.querySelectorAll('[data-card]').forEach(function (card) {
        var n = byHid[card.getAttribute('data-card')];
        if (n) patchCard(card, n);
      });
    }
    patchHero();
    var banners = document.getElementById('banners');
    if (banners) banners.innerHTML = bannerHTML(DATA);
    // 分组下拉在节点/分组变化时同步；保留当前选择。
    var groups = groupsOf((DATA && DATA.nodes) || []);
    var gk = groups.join('|');
    if (gk !== lastGroupsKey) {
      lastGroupsKey = gk;
      if (filters.group && groups.indexOf(filters.group) < 0) {
        filters.group = '';
        renderGridOnly();
      }
      syncGroupSelect(groups);
    }
    updateServices();
    updateMap();
    updateHistory();
    updateIncidents();
    updateActivity();
    updateToolbarCounts();
  }

  function syncGroupSelect(groups) {
    var sel = document.getElementById('groupFilter');
    if (!sel) return;
    var html = '<option value="">全部分组</option>' + groups.map(function (g) {
      return '<option value="' + esc(g) + '"' + (filters.group === g ? ' selected' : '') + '>' + esc(g) + '</option>';
    }).join('');
    if (sel.innerHTML !== html) sel.innerHTML = html;
  }

  function updateServices() {
    var wrap = document.getElementById('servicesWrap');
    if (!wrap) return;
    var list = block('services') && DATA.services ? DATA.services : [];
    if (!list.length) {
      wrap.hidden = true;
      return;
    }
    wrap.hidden = false;
    var box = document.getElementById('svcList');
    if (!box) return;
    var items = box.querySelectorAll('[data-svc]');
    if (items.length !== list.length) {
      box.innerHTML = serviceListHTML(list);
      return;
    }
    list.forEach(function (s, i) {
      var el = items[i];
      var has = s.has !== false;
      var dot = el.querySelector('[data-f="svc-dot"]');
      if (dot) dot.className = 'svc-dot ' + (has ? (s.ok ? 'ok' : 'bad') : 'off');
      var lat = has && s.latencyMs != null ? s.latencyMs : null;
      var latEl = el.querySelector('[data-f="svc-lat"]');
      if (latEl) {
        latEl.textContent = lat != null ? (lat < 1 ? '<1' : lat) + 'ms' : '—';
        latEl.className = 'svc-lat ' + (lat != null ? latCls(lat) : 'none');
      }
      var upEl = el.querySelector('[data-f="svc-uptime"]');
      if (upEl) upEl.textContent = s.uptime != null ? fixed(s.uptime, 1) + '%' : '—';
      var foot = [];
      foot.push(s.uptime != null ? fixed(s.uptime, 1) + '% 可用' : '暂无数据');
      if (s.failRate) foot.push('失败 ' + fixed(s.failRate, 2) + '%');
      if (s.lossPct) foot.push('丢包 ' + fixed(s.lossPct, 1) + '%');
      if (s.jitterMs) foot.push('抖动 ' + fixed(s.jitterMs, 1) + 'ms');
      var footEl = el.querySelector('[data-f="svc-foot"]');
      if (footEl) footEl.textContent = foot.join(' · ');
      var sp = el.querySelector('[data-f="svc-spark"]');
      if (sp) {
        sp.innerHTML = s.latencySpark && s.latencySpark.length > 1 ?
          sparkSVG(s.latencySpark, 'var(--accent-2)', { height: 24 }) : '';
      }
    });
  }

  function updateHistory() {
    var wrap = document.getElementById('historyWrap');
    if (!wrap) return;
    var days = block('history') && DATA.history ? DATA.history : [];
    if (!days.length) {
      wrap.hidden = true;
      return;
    }
    wrap.hidden = false;
    var bars = wrap.querySelectorAll('.upbar i');
    if (bars.length !== days.length) {
      wrap.innerHTML = historyBody(days);
      return;
    }
    days.forEach(function (d, i) {
      var el = bars[i];
      var cls = !d.has ? 'gap' : d.up >= 99.5 ? '' : d.up >= 95 ? 'warn' : 'crit';
      el.className = cls;
      el.title = dayMD(d.ts) + '　' + (d.has ? d.up.toFixed(1) + '%' : '无数据');
    });
    var avg = upBarSummary(days) + '%';
    var avgEl = wrap.querySelector('.upbar-foot .mid');
    if (avgEl) avgEl.textContent = avg;
    var labels = wrap.querySelectorAll('.upbar-foot span');
    if (labels.length === 3) {
      labels[0].textContent = dayMD(days[0].ts);
      labels[2].textContent = dayMD(days[days.length - 1].ts);
    }
  }

  function updateIncidents() {
    var wrap = document.getElementById('incidentsWrap');
    if (!wrap) return;
    if (!block('incidents')) {
      wrap.hidden = true;
      return;
    }
    wrap.hidden = false;
    var items = DATA.incidents || [];
    var sig = JSON.stringify(items);
    if (wrap.getAttribute('data-sig') === sig) return;
    wrap.setAttribute('data-sig', sig);
    wrap.innerHTML = incidentsBody(items, true);
  }

  function updateMap() {
    var wrap = document.getElementById('mapWrap');
    if (!wrap) return;
    var counts = block('map') && DATA.map ? DATA.map : {};
    var html = mapHTML(counts);
    wrap.hidden = !html;
    if (!html) return;
    var sig = JSON.stringify(counts);
    if (wrap.getAttribute('data-sig') === sig) return;
    wrap.setAttribute('data-sig', sig);
    var list = document.getElementById('mapList');
    if (list) list.innerHTML = html;
  }

  function updateActivity() {
    var wrap = document.getElementById('activityWrap');
    if (!wrap) return;
    if (!block('activity')) {
      wrap.hidden = true;
      return;
    }
    wrap.hidden = false;
    var items = DATA.activity || [];
    var sig = JSON.stringify(items);
    if (wrap.getAttribute('data-sig') === sig) return;
    wrap.setAttribute('data-sig', sig);
    wrap.innerHTML = activityBody(items, true);
  }

  /* ---------------- 详情 ---------------- */

  function listTitle() {
    var site = DATA && DATA.site ? DATA.site : '';
    return !site || site === '服务状态' ? '服务状态' : site + ' · 服务状态';
  }

  function pageTitle(label) {
    var site = DATA && DATA.site ? DATA.site : '';
    return !site || site === '服务状态' ? label : site + ' · ' + label;
  }

  function detailTitle(n) {
    if (n && n.name) document.title = n.name + ' · ' + ((DATA && DATA.site) || '服务状态');
  }

  function detailHeaderHTML(dt) {
    var n = dt.node || {};
    var info = stateInfo({ state: dt.state, online: dt.online });
    var up = dt.uptime24;
    var upcls = up != null ? upCls(up) : 'none';
    var stateLabel = dt.maintenance ? '维护中' : info.label;
    var badge = dt.maintenance ? 'mt' : info.badge;
    var meta = [];
    if (n.group) meta.push('分组 ' + esc(n.group));
    if (n.region) meta.push('地区 ' + esc(n.region));
    if (n.provider) meta.push('服务商 ' + esc(n.provider));
    var rings = ringsHTML(dt);
    var infoHTML = infoCells(dt);
    var body = '';
    if (rings || infoHTML) {
      body = '<div class="dhead-body' + (rings && infoHTML ? '' : ' solo') + '">' +
        rings +
        (infoHTML ? '<div class="dhead-info"><div class="dhead-info-h">当前状态' +
          '<span>最近一次上报</span></div>' + infoHTML + '</div>' : '') +
      '</div>';
    }
    return '<section class="card detail-card">' +
      '<div class="dhead">' +
        '<div class="dhead-main">' +
          '<div class="kicker">节点详情</div>' +
          '<div class="dhead-title">' +
            '<span class="nc-dot ' + (info.dot === 'ok' ? 'ok' : info.dot) + '"></span>' +
            '<h2>' + esc(n.name || '节点') + '</h2>' +
            '<span class="state-badge ' + badge + '">' + esc(stateLabel) + '</span>' +
          '</div>' +
          (meta.length ? '<div class="dmeta">' + meta.join('　') + '</div>' : '') +
        '</div>' +
        '<div class="dhead-side">' +
          '<span class="big ' + upcls + '">' + (up != null ? fixed(up, 1) + '%' : '—') + '</span>' +
          '<span class="lbl">24 小时可用率</span>' +
        '</div>' +
      '</div>' +
      body +
    '</section>';
  }

  function ringsHTML(dt) {
    var l = dt.latest || {};
    var v = dt.visibility || {};
    if (!v.metrics) return '';
    var cards = [];
    cards.push(ringCard('CPU', l.cpu || 0, 'var(--accent)', ''));
    cards.push(ringCard('内存', l.memPct || 0, 'var(--violet)',
      l.memTotal ? bytes(l.memUsed) + ' / ' + bytes(l.memTotal) : ''));
    cards.push(ringCard('磁盘', l.diskPct || 0, 'var(--accent-2)',
      l.diskTotal ? bytes(l.diskUsed) + ' / ' + bytes(l.diskTotal) : ''));
    return '<div class="rings">' + cards.join('') + '</div>';
  }

  function ringCard(label, pct, color, sub) {
    pct = Number(pct) || 0;
    return '<div class="ring-card"><div class="ring-wrap">' + ringSVG(pct, color) + '</div>' +
      '<div class="ring-copy"><div class="l">' + esc(label) + '</div>' +
      '<div class="s">' + esc(sub || '当前使用率') + '</div></div></div>';
  }

  function infoCells(dt) {
    var l = dt.latest || {};
    var v = dt.visibility || {};
    var cells = [];
    if (v.load) {
      cells.push(infoCell('负载', l.load1 != null ? fixed(l.load1, 2) : '—',
        '5m ' + fixed(l.load5, 2) + ' · 15m ' + fixed(l.load15, 2)));
    }
    if (v.uptime) cells.push(infoCell('运行时间', dur(l.uptime), ''));
    if (v.procs) cells.push(infoCell('进程数', l.procs != null ? String(l.procs) : '—', ''));
    if (v.tcpConn) cells.push(infoCell('TCP 连接', l.tcpConn != null ? String(l.tcpConn) : '—', ''));
    if (v.swap && l.swapTotal) cells.push(infoCell('Swap', bytes(l.swapUsed), '/ ' + bytes(l.swapTotal)));
    if (v.cores && l.cores) cells.push(infoCell('CPU 核心', String(l.cores), ''));
    if (v.traffic) cells.push(infoCell('实时下行', rate(l.netRx), '上行 ' + rate(l.netTx)));
    if (dt.lastSeen) cells.push(infoCell('最后上报', timeAgo(dt.lastSeen), fullTime(dt.lastSeen)));
    return cells.length ? '<div class="info-grid">' + cells.join('') + '</div>' : '';
  }

  function infoCell(label, value, sub) {
    return '<div class="info-cell"><div class="l">' + esc(label) + '</div>' +
      '<div class="v">' + value + '</div>' +
      (sub ? '<div class="s">' + esc(sub) + '</div>' : '') + '</div>';
  }

  function hostHTML(dt) {
    var host = dt.host || {};
    var pairs = [['hostname', '主机名'], ['os', '操作系统'], ['kernel', '内核'], ['arch', '架构'],
      ['cpuModel', 'CPU 型号'], ['cores', '核心数'], ['virt', '虚拟化']];
    var rows = [];
    pairs.forEach(function (p) {
      if (host[p[0]] != null && host[p[0]] !== '') {
        rows.push('<dt>' + esc(p[1]) + '</dt><dd>' + esc(host[p[0]]) + '</dd>');
      }
    });
    if (!rows.length) return '';
    return '<section class="card"><div class="card-h"><h2>主机信息</h2></div>' +
      '<dl class="kv">' + rows.join('') + '</dl></section>';
  }

  // 轴的量程可能只有零点几个百分点（空闲 CPU、稳定磁盘占用），
  // 固定 0 位小数会把 23.7/23.8/23.9 全画成「24%」。量程窄时保留一位小数。
  function pctAxisFmt(points) {
    var vals = validValues(points);
    var span = vals.length ? Math.max.apply(null, vals) - Math.min.apply(null, vals) : 0;
    var d = span < 0.3 ? 2 : span < 3 ? 1 : 0;
    return function (x) { return fixed(x, d) + '%'; };
  }

  function chartBlock(title, points, color, fmt, axisFmt) {
    var last = points && points.length ? points[points.length - 1].v : null;
    var lastTxt = last != null ? fmt(last) : '—';
    return '<div class="chart"><div class="chart-t"><span class="name">' + esc(title) + '</span>' +
      '<b>' + esc(lastTxt) + '</b></div>' +
      '<div class="chart-box">' + areaSVG(points, color, { fmt: axisFmt || fmt }) + '</div></div>';
  }

  function trendsHTML(dt) {
    var s = dt.series || {};
    var v = dt.visibility || {};
    var charts = [];
    if (v.metrics) {
      if (s.cpu && s.cpu.length > 1) charts.push(chartBlock('CPU 使用率', s.cpu, 'var(--accent)', function (x) { return fixed(x, 1) + '%'; }, pctAxisFmt(s.cpu)));
      if (s.mem && s.mem.length > 1) charts.push(chartBlock('内存使用率', s.mem, 'var(--violet)', function (x) { return fixed(x, 1) + '%'; }, pctAxisFmt(s.mem)));
      if (s.disk && s.disk.length > 1) charts.push(chartBlock('磁盘使用率', s.disk, 'var(--accent-2)', function (x) { return fixed(x, 1) + '%'; }, pctAxisFmt(s.disk)));
    }
    if (v.traffic && s.netRx && s.netRx.length > 1) {
      var lastRx = s.netRx[s.netRx.length - 1].v || 0;
      var lastTx = s.netTx && s.netTx.length ? s.netTx[s.netTx.length - 1].v || 0 : 0;
      charts.push('<div class="chart"><div class="chart-t"><span class="name">网络吞吐</span>' +
        '<b>↓ ' + esc(rate(lastRx)) + '　↑ ' + esc(rate(lastTx)) + '</b></div>' +
        '<div class="chart-box">' + multiAreaSVG([s.netRx, s.netTx || []],
          ['var(--accent-2)', 'var(--violet)'], { fmt: function (x) { return rate(x); } }) + '</div>' +
        '<div class="legend"><span><i style="background:var(--accent-2)"></i>下行</span>' +
        '<span><i style="background:var(--violet)"></i>上行</span></div></div>');
    }
    var tabs = '<div class="range-tabs" role="group" aria-label="趋势区间">' +
      rangeBtn('24h', '24 小时') + rangeBtn('7d', '7 天') + rangeBtn('30d', '30 天') +
    '</div>';
    if (!charts.length) {
      return '<section class="card"><div class="card-h"><h2>性能趋势</h2>' + tabs + '</div>' +
        '<div class="empty">当前展示配置下没有可公开的趋势数据</div></section>';
    }
    return '<section class="card"><div class="card-h"><h2>性能趋势</h2>' + tabs + '</div>' +
      '<div class="chart-grid">' + charts.join('') + '</div></section>';
  }

  function rangeBtn(key, label) {
    return '<button type="button" data-range="' + key + '" class="' + (range === key ? 'on' : '') + '">' +
      esc(label) + '</button>';
  }

  function paintRangeTabs() {
    document.querySelectorAll('.range-tabs button[data-range]').forEach(function (b) {
      b.classList.toggle('on', b.getAttribute('data-range') === range);
    });
  }

  function servicesHTML(dt) {
    var list = dt.services || [];
    if (!block('services') || !list.length) return '';
    var rows = list.map(function (s) {
      var has = s.has !== false;
      var lat = has && s.latencyMs != null ? s.latencyMs : null;
      var cls = lat != null ? latCls(lat) : 'none';
      var stats = [];
      if (s.uptime != null) stats.push('<span>24h 可用 <b>' + fixed(s.uptime, 1) + '%</b></span>');
      if (s.failRate) stats.push('<span>失败率 <b>' + fixed(s.failRate, 2) + '%</b></span>');
      if (s.avgLatency != null) stats.push('<span>平均延迟 <b>' + fixed(s.avgLatency, 0) + 'ms</b></span>');
      if (s.samples) stats.push('<span>成功样本 <b>' + s.samples + '</b></span>');
      var sp = s.latency && s.latency.length > 1 ?
        sparkSVG(s.latency, 'var(--accent-2)', { height: 44, clampZero: true }) : '<div class="empty" style="padding:6px 0">暂无延迟样本</div>';
      return '<div class="svc"><span class="svc-dot ' + (has ? (s.ok ? 'ok' : 'bad') : 'off') + '"></span>' +
        '<span class="svc-name">' + esc(s.name) + '</span>' +
        '<span class="svc-side"><span class="svc-lat ' + cls + '">' +
          (lat != null ? (lat < 1 ? '&lt;1' : lat) + 'ms' : '—') + '</span></span>' +
        '<span class="svc-chart">' + sp + '</span>' +
        '<span class="svc-stats">' + stats.join('') + '</span></div>';
    }).join('');
    return '<section class="card"><div class="card-h"><h2>服务延迟</h2>' +
      '<span class="hint">延迟曲线只含成功探测</span></div><div class="detail-svc">' + rows + '</div></section>';
  }

  function availabilityHTML(dt) {
    if (!block('history') || !dt.daily || !dt.daily.length) return '';
    var avg = upBarSummary(dt.daily);
    return '<section class="card"><div class="card-h"><h2>可用率</h2>' +
      '<span class="hint">过去 ' + dt.daily.length + ' 天</span>' +
      '<span class="hint" style="margin-left:auto">平均 <b class="mono">' + avg + '%</b></span></div>' +
      upBar(dt.daily) +
      '<div class="upbar-foot"><span>' + esc(dayMD(dt.daily[0].ts)) + '</span>' +
      '<span class="mid">' + avg + '%</span>' +
      '<span>' + esc(dayMD(dt.daily[dt.daily.length - 1].ts)) + '</span></div></section>';
  }

  function detailHTML(dt) {
    var bottom = availabilityHTML(dt) + servicesHTML(dt) + hostHTML(dt);
    return '<a class="back" href="#/">← 返回总览</a>' +
      detailHeaderHTML(dt) +
      trendsHTML(dt) +
      (bottom ? '<div class="detail-bottom">' + bottom + '</div>' : '');
  }

  // 只在从列表进入详情页时显示加载骨架；切换 7d/30d 时保留旧图，
  // 避免每点一次区间整页闪一下。
  function renderDetailShell(id) {
    app.setAttribute('data-view', 'detail');
    if (!DATA) return;
    if (app.getAttribute('data-detail-id') === id) {
      app.setAttribute('aria-busy', 'true');
      return;
    }
    app.innerHTML = '<a class="back" href="#/">← 返回总览</a>' +
      '<div class="card"><div class="empty">加载节点详情…</div></div>';
    setConnection('on', '实时连接');
  }

  function renderDetail(dt, animate) {
    route = routeOf();
    if (route.view !== 'node') return;
    detailTitle(dt.node || {});
    var html = detailHTML(dt);
    if (animate && !reduceMotion.matches && document.startViewTransition) {
      document.startViewTransition(function () { app.innerHTML = html; });
    } else {
      app.innerHTML = html;
    }
    app.setAttribute('data-view', 'detail');
    app.setAttribute('data-detail-id', route.id);
    app.removeAttribute('aria-busy');
  }

  function renderDetailError(id, msg) {
    app.innerHTML = '<a class="back" href="#/">← 返回总览</a>' +
      '<div class="card"><div class="empty">无法加载该节点：' + esc(msg) +
      '<br><button class="hdr-btn" type="button" data-act="retry-detail" data-id="' + esc(id) +
      '" style="margin-top:12px">重试</button></div></div>';
    app.setAttribute('data-view', 'detail');
    app.setAttribute('data-detail-id', id);
    app.removeAttribute('aria-busy');
  }

  function patchDetailHeader() {
    if (!DATA) return;
    if (route.view === 'node') {
      // 详情页周期性刷新只更新页脚统计，详情本身由 fetchDetail 拉取。
      var parts = [DATA.online + ' / ' + DATA.total + ' 在线'];
      document.getElementById('foot2').textContent = parts.join(' · ');
    }
  }

  /* ---------------- 筛选交互 ---------------- */

  function bindToolbar() {
    var q = document.getElementById('searchInput');
    if (q) {
      q.addEventListener('input', function () {
        filters.q = q.value;
        clearTimeout(searchTimer);
        searchTimer = setTimeout(function () { renderGridOnly(); }, 90);
      });
    }
    var seg = document.getElementById('stateFilter');
    if (seg) {
      seg.addEventListener('click', function (e) {
        var btn = e.target.closest ? e.target.closest('button[data-filter]') : null;
        if (!btn) return;
        filters.state = btn.getAttribute('data-filter');
        renderGridOnly();
      });
    }
    var g = document.getElementById('groupFilter');
    if (g) {
      g.addEventListener('change', function () {
        filters.group = g.value;
        renderGridOnly();
      });
    }
  }

  function go(hash) {
    if (location.hash === hash) return;
    location.hash = hash;
  }

  app.addEventListener('click', function (e) {
    var rangeBtnEl = e.target.closest ? e.target.closest('[data-range]') : null;
    if (rangeBtnEl) {
      var nr = rangeBtnEl.getAttribute('data-range');
      if (nr !== range) {
        range = nr;
        paintRangeTabs();
        var cached = DETAILS[route.id + '|' + range];
        if (cached) renderDetail(cached, false);
        else fetchDetail(route.id, false);
      }
      return;
    }
    var retry = e.target.closest ? e.target.closest('[data-act="retry"]') : null;
    if (retry) { rendered = false; fetchList(); return; }
    var retryDetail = e.target.closest ? e.target.closest('[data-act="retry-detail"]') : null;
    if (retryDetail) {
      var rid = retryDetail.getAttribute('data-id');
      fetchDetail(rid, false);
      return;
    }
    var node = e.target.closest ? e.target.closest('[data-node]') : null;
    if (node && block('nodeDetail')) {
      go('#/n/' + encodeURIComponent(node.getAttribute('data-node')));
    }
  });

  app.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    var node = e.target.closest ? e.target.closest('[data-node]') : null;
    if (node) {
      e.preventDefault();
      go('#/n/' + encodeURIComponent(node.getAttribute('data-node')));
    }
  });

  document.getElementById('themeBtn').addEventListener('click', function () {
    // auto → light → dark → auto
    localMode = localMode === null || localMode === 'auto' ? 'light' : localMode === 'light' ? 'dark' : null;
    try {
      if (localMode === null) localStorage.removeItem(LS_THEME);
      else localStorage.setItem(LS_THEME, localMode);
    } catch (e) { /* 隐私模式忽略 */ }
    paintTheme();
  });

  window.addEventListener('hashchange', function () {
    route = routeOf();
    if (route.view === 'node') {
      var dt = DETAILS[route.id + '|' + range];
      if (dt) renderDetail(dt, true);
      else {
        renderDetailShell(route.id);
        fetchDetail(route.id, false);
      }
    } else if (route.view === 'events' || route.view === 'activity') {
      if (DATA) renderFeedPage(route.view);
    } else {
      range = '24h';
      if (DATA) { document.title = listTitle(); renderList(); }
    }
    window.scrollTo(0, 0);
  });

  /* ---------------- 实时推送与轮询 ---------------- */

  // 公开状态页同样接入 SSE：事件只带版本号，收到后合并 300ms 内的多次变更
  // 再拉一次快照。10 秒轮询保留，作为代理不支持流式响应时的兜底。
  function connectStream() {
    if (!window.EventSource) return;
    try {
      stream = new EventSource('/api/status/stream');
    } catch (e) {
      return; // 退化为轮询
    }
    stream.addEventListener('hello', function () {
      setConnection('on', '实时连接');
    });
    stream.addEventListener('change', function () {
      if (streamTimer) return;
      streamTimer = setTimeout(function () {
        streamTimer = null;
        tick();
      }, 300);
    });
  }

  function tick() {
    if (document.hidden) return;
    fetchList();
    if (route.view === 'node' && route.id) fetchDetail(route.id, true);
  }

  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) tick();
  });
  window.addEventListener('resize', balanceKpis);

  // 只有已登录管理员才拿到拖拽手柄：公开页匿名可访问，
  // 排序会写进服务端配置并对所有访客生效，不能由访客改动。
  // 用同源会话探测（不发送任何凭证，浏览器自动带 Cookie）。
  fetch('/api/auth/session', { credentials: 'same-origin', cache: 'no-store' })
    .then(function (r) { return r.ok ? r.json() : null; })
    .then(function (d) {
      if (d && d.authenticated) {
        ADMIN = true;
        if (rendered) render();   // 已渲染则重绘一次，让手柄出现
      }
    })
    .catch(function () { /* 匿名访客：保持只读 */ });

  loadTheme();
  fetchList();
  connectStream();
  pollTimer = setInterval(tick, 10000);
})();
