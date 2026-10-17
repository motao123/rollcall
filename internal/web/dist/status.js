/* 公开状态页脚本（外部文件，受 CSP script-src 'self' 保护） */
(function () {
  'use strict';

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

  // 可用率配色：与"使用率"方向相反 —— 可用率越低越红。
  // 切勿复用使用率的配色函数，否则会出现"83% 可用率显示绿色"的语义颠倒。
  var upCls = function (v) { return v >= 99.5 ? 'ok' : v >= 95 ? 'warn' : 'crit'; };

  function bars(node) {
    var b = node.bars || [];
    if (!b.length) return '';
    var now = Math.floor(Date.now() / 1000);
    var map = {};
    b.forEach(function (x) { map[Math.round((now - x.t) / 1800)] = x.ok; });
    var html = '<div class="bars" title="过去 24 小时可用性">';
    for (var i = 47; i >= 0; i--) {
      var v = map[i];
      html += '<i class="' + (v === undefined ? 'gap' : (v ? '' : 'bad')) + '"></i>';
    }
    html += '</div><div class="bars-legend"><span>24 小时前</span><span>现在</span></div>';
    return html;
  }

  function render(d) {
    document.getElementById('title').textContent = d.site || '服务状态';
    var st = new Date(d.generatedAt * 1000);
    var p = function (n) { return (n < 10 ? '0' : '') + n; };
    document.getElementById('stamp').textContent =
      '更新于 ' + p(st.getMonth() + 1) + '-' + p(st.getDate()) + ' ' + p(st.getHours()) + ':' + p(st.getMinutes());

    var down = (d.total - d.online);
    var b = document.getElementById('banner');
    if (down === 0 && (d.svcDown || 0) === 0) {
      b.innerHTML = '<div class="banner ok"><span class="dot ok"></span>全部系统运行正常</div>';
    } else {
      var parts = [];
      if (down > 0) parts.push(down + ' 个节点离线');
      if (d.svcDown > 0) parts.push(d.svcDown + ' 个服务异常');
      b.innerHTML = '<div class="banner bad"><span class="dot bad"></span>' + esc(parts.join('，')) + '</div>';
    }

    var sv = (d.services || []);
    var sh = '';
    if (sv.length) {
      sh = '<div class="group"><h2 class="group-title">服务</h2>' + sv.map(function (s) {
        return '<div class="svc"><span class="dot ' + (s.ok ? 'ok' : 'bad') + '"></span>' +
          '<span class="name">' + esc(s.name) + '</span>' +
          '<span class="pct ' + upCls(s.uptime) + '" style="font-size:12px">' + s.uptime.toFixed(1) + '%</span>' +
          '<span class="lat mono">' + (s.latencyMs ? s.latencyMs + 'ms' : '—') + '</span></div>';
      }).join('') + '</div>';
    }
    document.getElementById('services').innerHTML = sh;

    var nodes = d.nodes || [];
    var groups = {};
    nodes.forEach(function (n) {
      var g = n.group || '节点';
      (groups[g] = groups[g] || []).push(n);
    });
    var html = '';
    Object.keys(groups).forEach(function (g) {
      html += '<div class="group"><h2 class="group-title">' + esc(g) + '</h2>';
      html += groups[g].map(function (n) {
        var up = n.uptime24;
        var cls = upCls(up || 0);
        return '<div class="row">' +
          '<div class="row-name"><span class="dot ' + (n.online ? 'ok' : 'off') + '"></span>' + esc(n.name) + '</div>' +
          '<div style="text-align:right"><span class="pct ' + cls + '">' + (up ? up.toFixed(1) + '%' : '—') + '</span>' +
          '<div style="font-size:10.5px;color:var(--fg3)">24h 可用率</div></div>' +
          '<div class="row-meta">' +
          '<span>CPU <b class="mono">' + n.cpu.toFixed(0) + '%</b></span>' +
          '<span>内存 <b class="mono">' + n.mem.toFixed(0) + '%</b></span>' +
          '<span>磁盘 <b class="mono">' + n.disk.toFixed(0) + '%</b></span>' +
          '<span>运行 <b class="mono">' + dur(n.uptime) + '</b></span>' +
          '</div>' +
          bars(n) +
          '</div>';
      }).join('');
      html += '</div>';
    });
    document.getElementById('nodes').innerHTML = html || '<div class="sub">暂无节点</div>';
    document.getElementById('foot').textContent = d.online + ' / ' + d.total + ' 在线';
  }

  function load() {
    fetch('/api/status', { headers: { Accept: 'application/json' } })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(render)
      .catch(function (e) {
        document.getElementById('nodes').innerHTML =
          '<div class="banner bad"><span class="dot bad"></span>数据加载失败：' + esc(e.message) + '</div>';
      });
  }

  load();
  setInterval(load, 30000);
})();
