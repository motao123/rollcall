package web

// statusPageHTML 是公开状态页的完整 HTML。
//
// 设计约束：
//
//   - 内联 CSS + 同源 status.js，不请求任何第三方资源
//   - 只读，不暴露任何管理入口与敏感字段
//   - 每 30 秒自动刷新数据
//   - CSP 为 script-src 'self'：所有交互必须用 addEventListener 绑定，
//     不能写 onclick="" 这类内联事件属性，也不能内联 <script>
//
// 页面结构由 status.js 按 hash 路由渲染：
// #/ （默认）是分组节点总览，#/n/<id> 是单个节点详情。
const statusPageHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="robots" content="noindex">
<meta name="color-scheme" content="light dark">
<title>服务状态</title>
<style>
:root{
  --bg:#f5f6f8; --panel:#fff; --panel-2:#fafbfc; --line:#e4e8ee; --line-2:#eff2f6;
  --fg:#141920; --fg2:#5a6472; --fg3:#939dab;
  --ok:#168a4b; --ok-bg:rgba(22,138,75,.09);
  --warn:#a9680a; --warn-bg:rgba(169,104,10,.11);
  --crit:#bf2424; --crit-bg:rgba(191,36,36,.09);
  --info:#2f5d8a; --info-bg:rgba(47,93,138,.09);
  --accent:#2f5d8a;
  --r:12px;
}
html[data-theme="dark"]{
  --bg:#0d1116; --panel:#151a21; --panel-2:#1a2028; --line:#232b35; --line-2:#1d242d;
  --fg:#e7ecf2; --fg2:#98a3b1; --fg3:#656f7d;
  --ok:#3ecf7a; --ok-bg:rgba(62,207,122,.13);
  --warn:#e0a33c; --warn-bg:rgba(224,163,60,.14);
  --crit:#f26d6d; --crit-bg:rgba(242,109,109,.13);
  --info:#6ea8d8; --info-bg:rgba(110,168,216,.13);
  --accent:#6ea8d8;
}
@media (prefers-color-scheme: dark){
  html:not([data-theme]){
    --bg:#0d1116; --panel:#151a21; --panel-2:#1a2028; --line:#232b35; --line-2:#1d242d;
    --fg:#e7ecf2; --fg2:#98a3b1; --fg3:#656f7d;
    --ok:#3ecf7a; --ok-bg:rgba(62,207,122,.13);
    --warn:#e0a33c; --warn-bg:rgba(224,163,60,.14);
    --crit:#f26d6d; --crit-bg:rgba(242,109,109,.13);
    --info:#6ea8d8; --info-bg:rgba(110,168,216,.13);
    --accent:#6ea8d8;
  }
}
*{box-sizing:border-box}
html,body{margin:0;padding:0}
body{
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Hiragino Sans GB","Microsoft YaHei",sans-serif;
  background:var(--bg); color:var(--fg); font-size:14px; line-height:1.5;
  -webkit-font-smoothing:antialiased;
}
.mono{font-family:ui-monospace,"SF Mono",Menlo,Consolas,monospace;font-variant-numeric:tabular-nums}
.wrap{max-width:1000px;margin:0 auto;padding:30px 20px 72px}

/* ---------- 页头 ---------- */
header{display:flex;align-items:baseline;gap:12px;margin-bottom:4px;flex-wrap:wrap}
h1{font-size:21px;font-weight:680;letter-spacing:-.02em;margin:0}
.sub{color:var(--fg2);font-size:12.5px}
.spacer{flex:1}
.hdr-btn{
  border:1px solid var(--line);background:var(--panel);color:var(--fg2);
  border-radius:8px;padding:4px 10px;font-size:12px;cursor:pointer;
}
.hdr-btn:hover{color:var(--fg);border-color:var(--fg3)}

/* ---------- 横幅 ---------- */
.banner{
  margin:14px 0 0;padding:11px 15px;border-radius:var(--r);
  display:flex;align-items:center;gap:10px;font-size:13px;font-weight:550;
}
.banner.ok{background:var(--ok-bg);color:var(--ok)}
.banner.bad{background:var(--crit-bg);color:var(--crit)}
.banner.warn{background:var(--warn-bg);color:var(--warn)}
.banner.info{background:var(--info-bg);color:var(--info)}
.banner .txt{flex:1;min-width:0}
.dot{width:9px;height:9px;border-radius:50%;flex:none;display:inline-block}
.dot.ok{background:var(--ok)} .dot.bad{background:var(--crit)}
.dot.off{background:var(--fg3)} .dot.warn{background:var(--warn)}
.dot.info{background:var(--info)} .dot.mt{background:var(--info)}

/* ---------- 卡片 ---------- */
.card{
  background:var(--panel);border:1px solid var(--line);border-radius:var(--r);
  padding:14px 16px;margin-top:14px;
}
.card-h{
  display:flex;align-items:baseline;gap:10px;margin:0 0 10px;
}
.card-h h2{font-size:11px;font-weight:650;letter-spacing:.08em;text-transform:uppercase;
  color:var(--accent);opacity:.85;margin:0}
.card-h .hint{font-size:11.5px;color:var(--fg3)}

/* ---------- KPI ---------- */
.kpis{display:grid;grid-template-columns:repeat(auto-fit,minmax(112px,1fr));gap:10px}
.kpi{
  background:var(--panel-2);border:1px solid var(--line-2);border-radius:9px;padding:9px 11px;
}
.kpi-l{font-size:11px;color:var(--fg3);margin-bottom:3px}
.kpi-v{font-size:18px;font-weight:640;letter-spacing:-.01em;line-height:1.2}
.kpi-u{font-size:11px;color:var(--fg3);font-weight:500;margin-left:2px}
.kpi-f{font-size:10.5px;color:var(--fg3);margin-top:2px}

/* ---------- N 天可用率条 ---------- */
.upbar{display:flex;gap:2px;height:34px;align-items:stretch}
.upbar i{flex:1;border-radius:2px;background:var(--ok);min-width:2px}
.upbar i.warn{background:var(--warn)}
.upbar i.crit{background:var(--crit)}
.upbar i.gap{background:var(--line-2)}
.upbar i:hover{outline:1px solid var(--fg3);outline-offset:1px}
.upbar-foot{display:flex;justify-content:space-between;font-size:10.5px;color:var(--fg3);margin-top:5px}

/* ---------- 分组与节点行 ---------- */
.group{margin-top:24px}
.group-title{
  font-size:11px;font-weight:650;letter-spacing:.08em;text-transform:uppercase;
  color:var(--accent);opacity:.85;margin:0 0 10px;display:flex;align-items:center;gap:8px;
}
.group-title .cnt{font-size:10.5px;color:var(--fg3);letter-spacing:0;text-transform:none;font-weight:500}
.row{
  background:var(--panel);border:1px solid var(--line);border-radius:var(--r);
  padding:12px 15px;margin-bottom:8px;cursor:pointer;
  transition:border-color .12s,background .12s;
}
.row:hover{border-color:var(--fg3);background:var(--panel-2)}
.row:focus-visible{outline:2px solid var(--accent);outline-offset:1px}
.row-top{display:flex;align-items:center;gap:10px}
.row-name{display:flex;align-items:center;gap:8px;font-weight:600;font-size:13.5px;min-width:0}
.row-name .nm{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.tag{
  font-size:10px;padding:1px 6px;border-radius:4px;font-weight:550;
  background:var(--info-bg);color:var(--info);flex:none;
}
.tag.mt{background:var(--info-bg);color:var(--info)}
.row-spark{width:88px;height:26px;flex:none;margin-left:auto}
.row-spark svg{width:100%;height:100%;display:block}
.row-pct{font-size:12.5px;font-weight:650;flex:none;min-width:52px;text-align:right}
.pct.ok{color:var(--ok)} .pct.warn{color:var(--warn)} .pct.crit{color:var(--crit)}
.pct.none{color:var(--fg3)}
.row-meta{display:flex;gap:13px;font-size:12px;color:var(--fg2);flex-wrap:wrap;margin-top:5px}
.row-meta b{color:var(--fg);font-weight:600}
.bars{display:flex;gap:2px;margin-top:8px;height:22px}
.bars i{flex:1;border-radius:2px;background:var(--ok);min-width:2px;opacity:.92}
.bars i.bad{background:var(--crit)}
.bars i.gap{background:var(--line)}
.bars i.warnv{background:var(--warn)}
.bars-legend{display:flex;justify-content:space-between;font-size:10.5px;color:var(--fg3);margin-top:4px}

/* ---------- 服务 ---------- */
.svc{display:flex;align-items:center;gap:10px;padding:9px 0;border-bottom:1px solid var(--line-2);font-size:13px}
.svc:last-child{border-bottom:0}
.svc .name{font-weight:600}
.svc .lat{margin-left:auto;color:var(--fg2);font-size:12px}

/* ---------- 事件时间线 ---------- */
.tl{border-left:2px solid var(--line);margin-left:5px;padding-left:16px}
.tl-day{font-size:11px;color:var(--fg3);margin:12px 0 6px;font-weight:600}
.tl-day:first-child{margin-top:0}
.tl-item{position:relative;padding:7px 0 7px 0}
.tl-item::before{
  content:"";position:absolute;left:-23px;top:13px;width:9px;height:9px;border-radius:50%;
  background:var(--fg3);
}
.tl-item.warn::before{background:var(--warn)}
.tl-item.crit::before{background:var(--crit)}
.tl-item.info::before{background:var(--info)}
.tl-t{font-weight:600;font-size:13px;display:flex;align-items:center;gap:7px;flex-wrap:wrap}
.tl-badge{font-size:10px;padding:1px 5px;border-radius:4px;background:var(--line-2);color:var(--fg2);font-weight:500}
.tl-meta{font-size:11.5px;color:var(--fg3);margin-top:2px}
.tl-body{font-size:12.5px;color:var(--fg2);margin-top:4px;white-space:pre-wrap}

/* ---------- 详情页 ---------- */
.back{
  display:inline-flex;align-items:center;gap:6px;border:1px solid var(--line);
  background:var(--panel);color:var(--fg2);border-radius:8px;padding:5px 11px;
  font-size:12.5px;cursor:pointer;margin-bottom:12px;
}
.back:hover{color:var(--fg);border-color:var(--fg3)}
.dhead{display:flex;align-items:center;gap:11px;flex-wrap:wrap;margin-bottom:4px}
.dhead h2{font-size:19px;font-weight:660;letter-spacing:-.02em;margin:0}
.dmeta{font-size:12px;color:var(--fg2);display:flex;gap:12px;flex-wrap:wrap;margin-top:3px}
.mgrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(128px,1fr));gap:10px;margin-top:12px}
.mcell{background:var(--panel-2);border:1px solid var(--line-2);border-radius:9px;padding:9px 11px}
.mcell .l{font-size:11px;color:var(--fg3)}
.mcell .v{font-size:16px;font-weight:640;margin-top:2px}
.mcell .v small{font-size:11px;color:var(--fg3);font-weight:500;margin-left:3px}
.chart{margin-top:10px}
.chart-t{font-size:11.5px;color:var(--fg2);margin-bottom:4px;display:flex;justify-content:space-between}
.chart-t b{color:var(--fg);font-weight:620}
.kv{display:grid;grid-template-columns:auto 1fr;gap:6px 16px;font-size:12.5px}
.kv dt{color:var(--fg3)}
.kv dd{margin:0;color:var(--fg)}
.empty{color:var(--fg3);font-size:12.5px;text-align:center;padding:22px 0}

footer{margin-top:32px;padding-top:16px;border-top:1px solid var(--line);
  color:var(--fg2);font-size:11.5px;display:flex;justify-content:space-between;gap:12px;flex-wrap:wrap}

/* ---------- 骨架屏 ---------- */
.skel{background:var(--line);border-radius:8px;height:56px;margin-bottom:8px;
  animation:sh 1.3s infinite,skelOut 1ms linear 12s forwards;background-size:200% 100%}
@keyframes sh{0%{background-position:200% 0}100%{background-position:-200% 0}}
/* 脚本被拦截或 404 时骨架屏会永远脉动下去。CSP 是 script-src 'self'，
   不能内联脚本做超时检测，这里用纯 CSS 在 12 秒后退场并揭示提示；
   脚本正常执行时 #app 会被整体替换，这两个动画自然失效。 */
@keyframes skelOut{to{opacity:0;visibility:hidden}}
#jsfail{display:none;animation:jsfailIn 1ms linear 12s forwards}
@keyframes jsfailIn{to{display:block}}

@media (max-width:640px){
  .wrap{padding:20px 13px 56px}
  .row-spark{width:64px}
  .kpis{grid-template-columns:repeat(2,1fr)}
  .upbar{height:28px}
}
html[data-density="compact"] .wrap{padding-top:20px;padding-bottom:44px}
html[data-density="compact"] .row{padding:9px 12px;margin-bottom:6px}
html[data-density="compact"] .card{padding:11px 13px;margin-top:10px}
html[data-density="compact"] .group{margin-top:18px}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1 id="title">服务状态</h1>
    <span class="sub" id="stamp">加载中…</span>
    <span class="spacer"></span>
    <button class="hdr-btn" id="themeBtn" type="button">切换外观</button>
  </header>
  <noscript><div class="banner bad"><span class="dot bad"></span>需要启用 JavaScript 才能显示实时状态。</div></noscript>
  <div id="jsfail" class="banner bad"><span class="dot bad"></span>状态脚本未能加载（可能被浏览器扩展或 CSP 拦截），请刷新页面重试。</div>
  <div id="app">
    <div class="skel"></div><div class="skel"></div><div class="skel"></div>
  </div>
  <footer>
    <span id="foot">数据每 30 秒自动刷新</span>
    <span class="mono" id="foot2"></span>
  </footer>
</div>
<script src="/status.js"></script>
</body>
</html>`
