/* 公开状态页脚本（外部文件，受 CSP script-src 'self' 保护）
 *
 * 页面只有两种视图：
 *   #/        节点总览（默认）
 *   #/n/<id>  单个节点详情
 *
 * 所有交互都用事件委托绑定在容器上：行是每 30 秒整体重绘的，
 * 逐行 addEventListener 会随刷新不断泄漏监听器，
 * 而且 CSP 禁止 onclick 这类内联属性。
 */
(function () {
  'use strict';

  /* ---------------- 格式化 ---------------- */

  var esc = function (s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  };

  var dur = function (sec) {
    sec = Number(sec) || 0;
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
    return (i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)) + ' ' + UNITS[i];
  };
  var rate = function (bps) {
    return (bytes(bps) || '0 B') + '/s';
  };

  var p2 = function (n) { return (n < 10 ? '0' : '') + n; };

  var clockHM = function (ts) {
    var d = new Date(ts * 1000);
    return p2(d.getHours()) + ':' + p2(d.getMinutes());
  };
  var dayMD = function (ts) {
    var d = new Date(ts * 1000);
    return (d.getMonth() + 1) + ' 月 ' + d.getDate() + ' 日';
  };
  var fullTime = function (ts) {
    var d = new Date(ts * 1000);
    return d.getFullYear() + '-' + p2(d.getMonth() + 1) + '-' + p2(d.getDate()) + ' ' + p2(d.getHours()) + ':' + p2(d.getMinutes());
  };

  // 可用率配色：与"使用率"方向相反 —— 可用率越低越红。
  // 切勿复用使用率的配色函数，否则会出现"83% 可用率显示绿色"的语义颠倒。
  var upCls = function (v) { return v >= 99.5 ? 'ok' : v >= 95 ? 'warn' : 'crit'; };
  // 使用率：越高越红。
  var useCls = function (v) { return v >= 90 ? 'crit' : v >= 75 ? 'warn' : 'ok'; };

  var LEVEL_CLS = { critical: 'crit', warn: 'warn', info: 'info' };
  var LEVEL_NAME = { critical: '严重', warn: '警告', info: '信息' };

  /* ---------------- 外观 ---------------- */

  // 只应用服务端下发的固定字段；不接收、不拼接任何 CSS 文本。
  var mq = window.matchMedia('(prefers-color-scheme: dark)');
  var serverTheme = null;
  // 访客可在本页临时覆盖深浅色，存在 localStorage 里。
  // 键名与后台面板的 rollcall-theme 分开：这里是访客侧偏好，
  // 不该把面板管理员的选择覆盖掉，也不该被面板的选择冲掉。
  var LS_THEME = 'rollcall-status-theme';
  var localMode = null;

  try { localMode = localStorage.getItem(LS_THEME); } catch (e) { /* 隐私模式下不可用 */ }
  if (localMode !== 'light' && localMode !== 'dark' && localMode !== 'auto') localMode = null;

  function paintTheme() {
    var t = serverTheme || {};
    var mode = localMode;
    if (mode !== 'light' && mode !== 'dark') {
      mode = (t.mode === 'light' || t.mode === 'dark') ? t.mode : (mq.matches ? 'dark' : 'light');
    }
    var root = document.documentElement;
    root.setAttribute('data-theme', mode);
    root.style.setProperty('--accent', mode === 'dark' ? (t.accentDark || '#6ea8d8') : (t.accent || '#2f5d8a'));
    root.style.setProperty('--r', ((t.radius == null ? 12 : Number(t.radius))) + 'px');
    if (t.compact) root.setAttribute('data-density', 'compact');
    else root.removeAttribute('data-density');
  }

  function loadTheme() {
    fetch('/api/theme', { headers: { Accept: 'application/json' } })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function (t) {
        serverTheme = t;
        paintTheme();
        if (t.mode !== 'light' && t.mode !== 'dark' && mq.addEventListener) {
          mq.addEventListener('change', paintTheme);
        }
      })
      .catch(paintTheme);
  }

  /* ---------------- 手绘图表 ---------------- */

  // 迷你曲线（列表行内）
  function sparkSVG(values, color) {
    var h = 26, w = 100;
    // ★ 必须 var 声明：本文件整体是 'use strict'，
    // 给未声明的变量赋值会直接抛 ReferenceError，整个脚本静默失效。
    var vals = (values || []).filter(function (v) { return typeof v === 'number' && isFinite(v); });
    if (vals.length < 2) {
      return '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none">' +
        '<line x1="0" y1="13" x2="' + w + '" y2="13" stroke="var(--line)" stroke-width="1" stroke-dasharray="2 3"/></svg>';
    }
    var min = Math.min.apply(null, vals), max = Math.max.apply(null, vals);
    if (max - min < 0.001) { max = min + 1; }
    var y = function (v) { return h - 3 - ((v - min) / (max - min)) * (h - 6); };
    var step = w / (vals.length - 1);
    var d = vals.map(function (v, i) {
      return (i ? 'L' : 'M') + (i * step).toFixed(1) + ',' + y(v).toFixed(1);
    }).join(' ');
    var gid = 'sg' + Math.random().toString(36).slice(2, 8);
    return '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none">' +
      '<defs><linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1">' +
      '<stop offset="0%" stop-color="' + color + '" stop-opacity="0.22"/>' +
      '<stop offset="100%" stop-color="' + color + '" stop-opacity="0"/></linearGradient></defs>' +
      '<path d="' + d + ' L' + w + ',' + h + ' L0,' + h + ' Z" fill="url(#' + gid + ')"/>' +
      '<path d="' + d + '" fill="none" stroke="' + color + '" stroke-width="1.5" ' +
      'stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/></svg>';
  }

  // 大图（详情页）：带网格与纵轴刻度
  function areaSVG(points, color, opt) {
    opt = opt || {};
    var h = opt.height || 118, w = 640, padL = 40, padR = 6, padT = 8, padB = 16;
    var vals = (points || []).map(function (p) { return Number(p && p.v); })
      .filter(function (v) { return isFinite(v); });
    if (vals.length < 2) return '<div class="empty">暂无数据</div>';
    var iw = w - padL - padR, ih = h - padT - padB;
    var min = Math.min.apply(null, vals), max = Math.max.apply(null, vals);
    var span = max - min;
    if (span < 0.001) span = 1;
    min = Math.max(0, min - span * 0.12);
    max = max + span * 0.12;
    var y = function (v) { return padT + ih - ((v - min) / (max - min)) * ih; };
    var step = iw / (vals.length - 1);
    var pts = vals.map(function (v, i) { return (padL + i * step).toFixed(1) + ',' + y(v).toFixed(1); });
    var line = 'M' + pts.join(' L');
    var area = line + ' L' + (padL + iw).toFixed(1) + ',' + (padT + ih) + ' L' + padL + ',' + (padT + ih) + ' Z';
    var gid = 'ag' + Math.random().toString(36).slice(2, 8);

    var grid = '';
    for (var i = 0; i <= 3; i++) {
      var gv = min + ((max - min) / 3) * i;
      var gy = y(gv);
      grid += '<line x1="' + padL + '" y1="' + gy.toFixed(1) + '" x2="' + (w - padR) + '" y2="' + gy.toFixed(1) +
        '" stroke="var(--line)" stroke-width="1"/>' +
        '<text x="' + (padL - 6) + '" y="' + (gy + 3).toFixed(1) + '" text-anchor="end" font-size="9" ' +
        'fill="var(--fg3)" font-family="ui-monospace,monospace">' + esc(opt.fmt ? opt.fmt(gv) : gv.toFixed(0)) + '</text>';
    }
    return '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none" style="width:100%;height:' + h + 'px;display:block">' +
      '<defs><linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1">' +
      '<stop offset="0%" stop-color="' + color + '" stop-opacity="0.20"/>' +
      '<stop offset="100%" stop-color="' + color + '" stop-opacity="0"/></linearGradient></defs>' +
      grid +
      '<path d="' + area + '" fill="url(#' + gid + ')"/>' +
      '<path d="' + line + '" fill="none" stroke="' + color + '" stroke-width="1.8" ' +
      'stroke-linejoin="round" stroke-linecap="round"/></svg>';
  }

  // 24h 分段色块：48 段 × 30 分钟
  function bars48(list) {
    var b = list || [];
    if (!b.length) return '';
    var now = Math.floor(Date.now() / 1000);
    var map = {};
    b.forEach(function (x) { map[Math.round((now - x.t) / 1800)] = x.ok; });
    var html = '<div class="bars" title="过去 24 小时可用性">';
    for (var i = 47; i >= 0; i--) {
      var v = map[i];
      html += '<i class="' + (v === undefined ? 'gap' : (v ? '' : 'bad')) + '"></i>';
    }
    return html + '</div><div class="bars-legend"><span>24 小时前</span><span>现在</span></div>';
  }

  // N 天可用率日柱
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

  /* ---------------- 状态 ---------------- */

  var app = document.getElementById('app');
  var rendered = false;

  function showFatal(msg) {
    if (!rendered) {
      app.innerHTML = '<div class="banner bad"><span class="dot bad"></span>' +
        '<span class="txt">页面渲染出错：' + esc(msg) + '</span></div>';
    }
  }

  // 脚本异常兜底：render 在 Promise 里跑，未捕获的异常只会把页面留成
  // 空白（骨架屏 12 秒后退场），访客和运维都看不到任何线索。
  window.addEventListener('error', function (ev) {
    showFatal(ev.message || '未知错误');
  });
  window.addEventListener('unhandledrejection', function (ev) {
    showFatal(String(ev.reason && ev.reason.message || ev.reason || '未知错误'));
  });
  var DATA = null;
  var DETAIL = {};      // nodeId -> 详情（带 ts，避免每次切视图都重取）
  var DETAIL_ERR = {};  // nodeId -> 错误信息
  var pending = null;

  function routeOf() {
    var h = String(location.hash || '').replace(/^#\/?/, '');
    var m = h.match(/^n\/(.+)$/);
    if (m) return { view: 'node', id: decodeURIComponent(m[1]) };
    return { view: 'list' };
  }

  /* ---------------- 总览 ---------------- */

  function renderList(d) {
    var html = '';
    html += renderBanner(d);

    if (d.summary) html += renderSummary(d);
    if (d.history && d.history.length) html += renderHistory(d);

    if (d.services && d.services.length) html += renderServices(d.services);
    html += renderNodes(d);
    if (d.incidents) html += renderTimeline(d.incidents);
    return html;
  }

  function renderBanner(d) {
    var html = '';
    if (d.announcement && d.announcement.enabled) {
      html += '<div class="banner ' + (LEVEL_CLS[d.announcement.level] || 'info') + '" style="margin-top:14px">' +
        '<span class="dot ' + (LEVEL_CLS[d.announcement.level] || 'info') + '"></span>' +
        '<span class="txt">' + esc(d.announcement.text) + '</span></div>';
    }
    if (d.maintenance) {
      html += '<div class="banner info"><span class="dot mt"></span><span class="txt">' +
        esc(d.maintenance.title) + '：' + esc(d.maintenance.text) + '</span></div>';
    }
    var offline = d.offline != null ? d.offline : 0;
    var stale = d.stale || 0, noData = d.noData || 0, svcDown = d.svcDown || 0;
    if (offline === 0 && stale === 0 && noData === 0 && svcDown === 0) {
      html += '<div class="banner ok"><span class="dot ok"></span><span class="txt">全部系统运行正常</span></div>';
    } else {
      var parts = [];
      if (offline > 0) parts.push(offline + ' 个节点离线');
      if (stale > 0) parts.push(stale + ' 个节点数据延迟');
      if (noData > 0) parts.push(noData + ' 个节点待接入');
      if (svcDown > 0) parts.push(svcDown + ' 个服务异常');
      html += '<div class="banner bad"><span class="dot bad"></span><span class="txt">' + esc(parts.join('，')) + '</span></div>';
    }
    return html;
  }

  function renderSummary(d) {
    var s = d.summary;
    var cells = [];
    cells.push(kpi('节点在线', s.online + ' / ' + s.total, '', (s.stale ? s.stale + ' 个数据延迟　' : '') + (s.noData ? s.noData + ' 个待接入' : '')));
    cells.push(kpi('24h 可用率', s.avgUptime24.toFixed(1), '%', '全部节点平均'));
    if (d.nodes && d.nodes.length && d.nodes[0].cpu != null) {
      // 注意键名是后端 JSON tag：avgCpu（小写 p），不是 avgCPU
      cells.push(kpi('平均 CPU', s.avgCpu.toFixed(0), '%', ''));
      cells.push(kpi('平均内存', s.avgMem.toFixed(0), '%', ''));
    }
    if (s.trafficIn || s.trafficOut) {
      cells.push(kpi('实时下行', rate(s.trafficIn), '', '上行 ' + rate(s.trafficOut)));
    }
    if (s.svcTotal) cells.push(kpi('服务', (s.svcTotal - s.svcDown) + ' / ' + s.svcTotal, '', '正常 / 总数'));
    if (s.activeAlert) cells.push(kpi('进行中告警', String(s.activeAlert), '', ''));
    return '<div class="card"><div class="card-h"><h2>总览</h2>' +
      '<span class="hint">点击任一节点查看详细指标</span></div>' +
      '<div class="kpis">' + cells.join('') + '</div></div>';
  }

  function kpi(label, value, unit, foot) {
    return '<div class="kpi"><div class="kpi-l">' + esc(label) + '</div>' +
      '<div class="kpi-v mono">' + value + (unit ? '<span class="kpi-u">' + unit + '</span>' : '') + '</div>' +
      (foot ? '<div class="kpi-f">' + esc(foot) + '</div>' : '') + '</div>';
  }

  function renderHistory(d) {
    var days = d.history;
    // 只统计有数据的天：把"尚未接入"的日子算进均值会把历史可用率拉低
    var sum = 0, n = 0;
    days.forEach(function (x) { if (x.has) { sum += x.up; n++; } });
    var avg = n ? sum / n : 0;
    var span = d.historyDays || days.length;
    return '<div class="card"><div class="card-h"><h2>可用率</h2>' +
      '<span class="hint">过去 ' + span + ' 天，每日一格</span></div>' +
      upBar(days) +
      '<div class="upbar-foot"><span>' + (days.length ? dayMD(days[0].ts) : '') + '</span>' +
      '<span class="mono">平均 ' + (n ? avg.toFixed(1) : '—') + '%</span>' +
      '<span>' + (days.length ? dayMD(days[days.length - 1].ts) : '') + '</span></div></div>';
  }

  function renderServices(list) {
    var rows = list.map(function (s) {
      var has = s.has !== false;
      var dot = has ? (s.ok ? 'ok' : 'bad') : 'off';
      var pctTxt = has ? s.uptime.toFixed(1) + '%' : '—';
      return '<div class="svc"><span class="dot ' + dot + '"></span>' +
        '<span class="name">' + esc(s.name) + '</span>' +
        '<span class="pct ' + (has ? upCls(s.uptime) : 'none') + '" style="font-size:12px">' + pctTxt + '</span>' +
        '<span class="lat mono">' + (has && s.latencyMs ? s.latencyMs + 'ms' : '—') + '</span></div>';
    }).join('');
    return '<div class="card"><div class="card-h"><h2>服务</h2></div>' + rows + '</div>';
  }

  function renderNodes(d) {
    var nodes = d.nodes || [];
    if (!nodes.length) return '<div class="card"><div class="empty">暂无节点</div></div>';
    var groups = {}, order = [];
    nodes.forEach(function (n) {
      var g = n.group || '节点';
      if (!groups[g]) { groups[g] = []; order.push(g); }
      groups[g].push(n);
    });
    var html = '';
    order.forEach(function (g) {
      var list = groups[g];
      html += '<div class="group"><h2 class="group-title">' + esc(g) +
        '<span class="cnt">' + list.length + ' 台</span></h2>';
      html += list.map(nodeRow).join('');
      html += '</div>';
    });
    return html;
  }

  function nodeRow(n) {
    var state = n.state || (n.online ? 'online' : 'offline');
    var dot = state === 'online' ? 'ok' : state === 'stale' ? 'warn' : state === 'no_data' ? 'off' : 'bad';
    var label = state === 'online' ? '在线' : state === 'stale' ? '数据延迟' : state === 'no_data' ? '待接入' : '离线';
    var hasData = state !== 'no_data';
    var up = n.uptime24;

    var html = '<div class="row" tabindex="0" role="link" data-node="' + esc(n.id) + '" ' +
      'aria-label="' + esc(n.name + '　' + label + '　24 小时可用率 ' + (up != null ? up.toFixed(1) : '—') + '%') + '">';
    html += '<div class="row-top">';
    html += '<span class="row-name"><span class="dot ' + dot + '" title="' + label + '"></span>' +
      '<span class="nm">' + esc(n.name) + '</span>';
    if (n.maintenanceTitle) html += '<span class="tag mt">维护中</span>';
    if (n.region) html += '<span class="tag">' + esc(n.region) + '</span>';
    html += '</span>';
    if (n.spark && n.spark.length > 1) {
      var color = n.cpu >= 90 ? 'var(--crit)' : n.cpu >= 75 ? 'var(--warn)' : 'var(--accent)';
      html += '<span class="row-spark" title="24 小时 CPU 趋势">' + sparkSVG(n.spark.map(function (p) { return p.v; }), color) + '</span>';
    } else {
      html += '<span class="row-spark"></span>';
    }
    html += '<span class="row-pct pct ' + (up != null ? upCls(up) : 'none') + '">' +
      (up != null ? up.toFixed(1) + '%' : '—') + '</span>';
    html += '</div>';

    html += '<div class="row-meta">';
    if (n.cpu != null) html += '<span>CPU <b class="mono">' + n.cpu.toFixed(0) + '%</b></span>';
    if (n.mem != null) html += '<span>内存 <b class="mono">' + n.mem.toFixed(0) + '%</b></span>';
    if (n.disk != null) html += '<span>磁盘 <b class="mono">' + n.disk.toFixed(0) + '%</b></span>';
    if (n.netRx != null || n.netTx != null) {
      html += '<span>↓<b class="mono">' + rate(n.netRx || 0) + '</b> ↑<b class="mono">' + rate(n.netTx || 0) + '</b></span>';
    }
    if (n.load1 != null) html += '<span>负载 <b class="mono">' + n.load1.toFixed(2) + '</b></span>';
    if (n.uptime) html += '<span>运行 <b class="mono">' + dur(n.uptime) + '</b></span>';
    html += '</div>';
    html += bars48(n.bars);
    html += '</div>';
    return html;
  }

  function renderTimeline(items) {
    if (!items || !items.length) {
      return '<div class="card"><div class="card-h"><h2>事件</h2></div>' +
        '<div class="empty">过去一段时间没有记录到事件</div></div>';
    }
    // 按本地日分组：服务端按 UTC 切日，这里用本地日期更符合访客直觉
    var byDay = {}, order = [];
    items.forEach(function (it) {
      var d = new Date(it.startAt * 1000);
      var key = d.getFullYear() + '-' + (d.getMonth() + 1) + '-' + d.getDate();
      if (!byDay[key]) { byDay[key] = []; order.push(key); }
      byDay[key].push(it);
    });
    var html = '<div class="card"><div class="card-h"><h2>事件</h2>' +
      '<span class="hint">告警自动生成 + 人工登记</span></div><div class="tl">';
    order.forEach(function (k) {
      html += '<div class="tl-day">' + esc(dayMD(byDay[k][0].startAt)) + '</div>';
      byDay[k].forEach(function (it) {
        var cls = LEVEL_CLS[it.level] || 'info';
        var ongoing = !it.endAt;
        html += '<div class="tl-item ' + cls + '">';
        html += '<div class="tl-t">' + esc(it.title) +
          '<span class="tl-badge">' + esc(LEVEL_NAME[it.level] || '信息') + '</span>' +
          (ongoing ? '<span class="tl-badge">进行中</span>' : '') +
          (it.auto ? '' : '<span class="tl-badge">人工</span>') + '</div>';
        html += '<div class="tl-meta mono">' + clockHM(it.startAt) +
          (ongoing ? ' 起，持续 ' + dur(it.duration) : ' → ' + clockHM(it.endAt) + '，持续 ' + dur(it.duration)) +
          (it.nodeNames && it.nodeNames.length ? '　' + esc(it.nodeNames.join('、')) : '') + '</div>';
        if (it.body) html += '<div class="tl-body">' + esc(it.body) + '</div>';
        html += '</div>';
      });
    });
    return html + '</div></div>';
  }

  /* ---------------- 节点详情 ---------------- */

  function renderDetail(d, dt) {
    var n = dt.node || {};
    var host = dt.host || {};
    var l = dt.latest || {};
    var state = dt.state || (dt.online ? 'online' : 'offline');
    var dot = state === 'online' ? 'ok' : state === 'stale' ? 'warn' : state === 'no_data' ? 'off' : 'bad';
    var label = state === 'online' ? '在线' : state === 'stale' ? '数据延迟' : state === 'no_data' ? '待接入' : '离线';

    var html = '<button class="back" type="button" data-act="back">← 返回总览</button>';

    html += '<div class="card">';
    html += '<div class="dhead"><span class="dot ' + dot + '"></span><h2>' + esc(n.name || '节点') + '</h2>' +
      '<span class="sub">' + esc(label) + '</span>' +
      (dt.maintenance ? '<span class="tag mt">维护中：' + esc(dt.maintenance) + '</span>' : '') +
      '<span class="spacer"></span>' +
      '<span class="row-pct pct ' + (dt.uptime24 != null ? upCls(dt.uptime24) : 'none') + '">' +
      (dt.uptime24 != null ? dt.uptime24.toFixed(1) + '%' : '—') + '</span></div>';
    var meta = [];
    if (n.group) meta.push('分组 ' + esc(n.group));
    if (n.region) meta.push('地区 ' + esc(n.region));
    if (n.provider) meta.push('服务商 ' + esc(n.provider));
    if (dt.lastSeen) meta.push('最后上报 ' + esc(fullTime(dt.lastSeen)));
    if (meta.length) html += '<div class="dmeta">' + meta.join('　') + '</div>';
    html += '</div>';

    // 指标卡片
    var cells = [];
    if (l.cpu != null) cells.push(mcell('CPU', l.cpu.toFixed(1), '%'));
    if (l.memPct != null) cells.push(mcell('内存', l.memPct.toFixed(1), '%',
      l.memTotal ? bytes(l.memUsed) + ' / ' + bytes(l.memTotal) : ''));
    if (l.diskPct != null) cells.push(mcell('磁盘', l.diskPct.toFixed(1), '%',
      l.diskTotal ? bytes(l.diskUsed) + ' / ' + bytes(l.diskTotal) : ''));
    if (l.load1 != null) cells.push(mcell('负载', l.load1.toFixed(2), '',
      '5m ' + l.load5.toFixed(2) + '　15m ' + l.load15.toFixed(2)));
    if (l.uptime) cells.push(mcell('运行时间', dur(l.uptime), ''));
    if (l.procs != null) cells.push(mcell('进程数', String(l.procs), ''));
    if (l.tcpConn != null) cells.push(mcell('TCP 连接', String(l.tcpConn), ''));
    if (l.swapTotal) cells.push(mcell('Swap', bytes(l.swapUsed), '', '/ ' + bytes(l.swapTotal)));
    if (l.netRx != null) cells.push(mcell('下行速率', rate(l.netRx), '', '上行 ' + rate(l.netTx || 0)));
    if (l.totRx != null) cells.push(mcell('累计流量', bytes(l.totRx), '', '上行 ' + bytes(l.totTx || 0)));
    if (l.cores) cells.push(mcell('CPU 核心', String(l.cores), ''));
    if (cells.length) {
      html += '<div class="card"><div class="card-h"><h2>当前指标</h2></div>' +
        '<div class="mgrid">' + cells.join('') + '</div></div>';
    }

    // 曲线
    var s = dt.series || {};
    var charts = [];
    if (s.cpu && s.cpu.length > 1) {
      charts.push(chart('CPU 使用率 · 24 小时', s.cpu, 'var(--accent)', function (v) { return v.toFixed(0) + '%'; }));
    }
    if (s.mem && s.mem.length > 1) {
      charts.push(chart('内存使用率 · 24 小时', s.mem, '#8b5cf6', function (v) { return v.toFixed(0) + '%'; }));
    }
    if (s.disk && s.disk.length > 1) {
      charts.push(chart('磁盘使用率 · 24 小时', s.disk, '#d97706', function (v) { return v.toFixed(0) + '%'; }));
    }
    if (s.netRx && s.netRx.length > 1) {
      charts.push(chart('网络速率 · 24 小时', s.netRx, 'var(--ok)', function (v) { return bytes(v) + '/s'; }, 132));
      charts.push(chart('网络速率（上行）· 24 小时', s.netTx, 'var(--info)', function (v) { return bytes(v) + '/s'; }, 132));
    }
    if (charts.length) {
      html += '<div class="card"><div class="card-h"><h2>趋势</h2></div>' + charts.join('') + '</div>';
    }

    // N 天可用率
    if (dt.daily && dt.daily.length) {
      var sum = 0, cnt = 0;
      dt.daily.forEach(function (x) { if (x.has) { sum += x.up; cnt++; } });
      html += '<div class="card"><div class="card-h"><h2>可用率</h2>' +
        '<span class="hint">过去 ' + dt.daily.length + ' 天</span></div>' + upBar(dt.daily) +
        '<div class="upbar-foot"><span>' + esc(dayMD(dt.daily[0].ts)) + '</span>' +
        '<span class="mono">平均 ' + (cnt ? (sum / cnt).toFixed(1) : '—') + '%</span>' +
        '<span>' + esc(dayMD(dt.daily[dt.daily.length - 1].ts)) + '</span></div></div>';
    }

    // 主机信息
    var kv = [];
    [['hostname', '主机名'], ['os', '操作系统'], ['kernel', '内核'], ['arch', '架构'],
     ['cpuModel', 'CPU 型号'], ['cores', '核心数'], ['virt', '虚拟化']].forEach(function (pair) {
      if (host[pair[0]] != null && host[pair[0]] !== '') kv.push([pair[1], host[pair[0]]]);
    });
    if (kv.length) {
      html += '<div class="card"><div class="card-h"><h2>主机</h2></div><dl class="kv">' +
        kv.map(function (p) { return '<dt>' + esc(p[0]) + '</dt><dd class="mono">' + esc(p[1]) + '</dd>'; }).join('') +
        '</dl></div>';
    }

    // 该节点的服务
    if (dt.services && dt.services.length) {
      html += '<div class="card"><div class="card-h"><h2>服务探测</h2></div>' +
        dt.services.map(function (sv) {
          var has = sv.has !== false;
          var d2 = has ? (sv.ok ? 'ok' : 'bad') : 'off';
          return '<div class="svc"><span class="dot ' + d2 + '"></span><span class="name">' + esc(sv.name) + '</span>' +
            '<span class="pct ' + (has ? upCls(sv.uptime) : 'none') + '" style="font-size:12px">' +
            (has ? sv.uptime.toFixed(1) + '%' : '—') + '</span>' +
            '<span class="lat mono">' + (has && sv.latencyMs ? sv.latencyMs + 'ms' : '—') + '</span></div>';
        }).join('') + '</div>';
    }

    if (!cells.length && !charts.length && !kv.length) {
      html += '<div class="card"><div class="empty">该节点暂无可公开的指标</div></div>';
    }
    return html;
  }

  function mcell(label, value, unit, foot) {
    return '<div class="mcell"><div class="l">' + esc(label) + '</div>' +
      '<div class="v mono">' + esc(value) + (unit ? '<small>' + unit + '</small>' : '') + '</div>' +
      (foot ? '<div class="l" style="margin-top:2px">' + esc(foot) + '</div>' : '') + '</div>';
  }

  function chart(title, points, color, fmt, h) {
    var last = points && points.length ? points[points.length - 1].v : null;
    return '<div class="chart"><div class="chart-t"><span>' + esc(title) + '</span>' +
      '<b class="mono">' + (last != null ? fmt(last) : '—') + '</b></div>' +
      areaSVG(points, color, { fmt: fmt, height: h || 118 }) + '</div>';
  }

  /* ---------------- 渲染入口 ---------------- */

  function render() {
    if (!DATA) return;
    try {
      var r = routeOf();
      var html;
      if (r.view === 'node') {
        var dt = DETAIL[r.id];
        if (dt) html = renderDetail(DATA, dt);
        else if (DETAIL_ERR[r.id]) {
          html = '<button class="back" type="button" data-act="back">← 返回总览</button>' +
            '<div class="card"><div class="empty">无法加载该节点：' + esc(DETAIL_ERR[r.id]) + '</div></div>';
        } else {
          html = '<div class="card"><div class="empty">加载节点详情…</div></div>';
          fetchDetail(r.id);
        }
      } else {
        html = renderList(DATA);
      }
      app.innerHTML = html;
      rendered = true;
    } catch (err) {
      showFatal(String(err && err.message || err) + (err && err.stack ? ' @ ' + String(err.stack).split('\n')[1] : ''));
    }
  }

  function stamp() {
    if (!DATA) return;
    var st = new Date(DATA.generatedAt * 1000);
    document.getElementById('stamp').textContent =
      '更新于 ' + p2(st.getHours()) + ':' + p2(st.getMinutes()) + ':' + p2(st.getSeconds());
    var parts = [DATA.online + ' / ' + DATA.total + ' 在线'];
    if (DATA.stale > 0) parts.push(DATA.stale + ' 数据延迟');
    if (DATA.noData > 0) parts.push(DATA.noData + ' 待接入');
    document.getElementById('foot2').textContent = parts.join(' · ');
  }

  /* ---------------- 数据加载 ---------------- */

  function load() {
    return fetch('/api/status', { headers: { Accept: 'application/json' } })
      .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
      .then(function (d) {
        DATA = d;
        document.getElementById('title').textContent = d.site || '服务状态';
        // 同步浏览器标签标题：否则改服务端标题后标签页永远停在硬编码值
        document.title = (d.site ? d.site + ' · ' : '') + '服务状态';
        stamp();
        render();
      })
      .catch(function (e) {
        if (!DATA) {
          app.innerHTML = '<div class="banner bad"><span class="dot bad"></span>' +
            '<span class="txt">数据加载失败：' + esc(e.message) + '</span></div>';
        }
      });
  }

  function fetchDetail(id) {
    if (pending === id) return;
    pending = id;
    fetch('/api/status/node/' + encodeURIComponent(id), { headers: { Accept: 'application/json' } })
      .then(function (r) {
        if (!r.ok) throw new Error(r.status === 404 ? '节点不存在或详情页未开放' : 'HTTP ' + r.status);
        return r.json();
      })
      .then(function (dt) {
        DETAIL[id] = dt;
        delete DETAIL_ERR[id];
        pending = null;
        render();
      })
      .catch(function (e) {
        DETAIL_ERR[id] = e.message;
        pending = null;
        render();
      });
  }

  /* ---------------- 交互（事件委托） ---------------- */

  // 行/按钮都是每 30 秒整体重绘的，监听器必须挂在不变的容器上，
  // 否则旧节点的监听器会随 DOM 一起被丢弃但闭包仍引用着它们。
  app.addEventListener('click', function (e) {
    var back = e.target.closest ? e.target.closest('[data-act="back"]') : null;
    if (back) { location.hash = '#/'; return; }
    var row = e.target.closest ? e.target.closest('[data-node]') : null;
    if (row) { location.hash = '#/n/' + encodeURIComponent(row.getAttribute('data-node')); }
  });

  app.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    var row = e.target.closest ? e.target.closest('[data-node]') : null;
    if (row) {
      e.preventDefault();
      location.hash = '#/n/' + encodeURIComponent(row.getAttribute('data-node'));
    }
  });

  document.getElementById('themeBtn').addEventListener('click', function () {
    // auto → light → dark → auto
    localMode = localMode === null || localMode === 'auto' ? 'light' : localMode === 'light' ? 'dark' : null;
    try {
      if (localMode === null) localStorage.removeItem(LS_THEME);
      else localStorage.setItem(LS_THEME, localMode);
    } catch (err) { /* 忽略：隐私模式下写不进去不影响本次切换 */ }
    paintTheme();
  });

  window.addEventListener('hashchange', function () {
    render();
    window.scrollTo(0, 0);
  });

  loadTheme();
  load();
  setInterval(load, 30000);
  // 外观配置变化不频繁，每 5 分钟同步一次即可。
  setInterval(loadTheme, 300000);
})();
