package web

// statusPageHTML 是公开状态页的完整 HTML。
//
// 设计约束：
//
//	· 内联 CSS + 同源 status.js，不请求任何第三方资源
//	· 只读，不暴露任何管理入口与敏感字段
//	· 每 30 秒自动刷新数据
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
  --bg:#f6f7f9; --panel:#fff; --line:#e5e8ee; --fg:#151a21; --fg2:#5b6472; --fg3:#98a1ae;
  --ok:#178a4c; --ok-bg:rgba(23,138,76,.10);
  --warn:#b4700a; --warn-bg:rgba(180,112,10,.12);
  --crit:#c02626; --crit-bg:rgba(192,38,38,.10);
  --accent:#2f5d8a;
  --r:12px;
}
/* 服务端外观模式由 status.js 写入 data-theme；未下发前跟随系统。 */
html[data-theme="dark"]{
  --bg:#0e1216; --panel:#161b22; --line:#242c36; --fg:#e8edf3; --fg2:#9aa6b4; --fg3:#66707e;
  --ok:#3ecf7a; --ok-bg:rgba(62,207,122,.13);
  --warn:#e0a33c; --warn-bg:rgba(224,163,60,.14);
  --crit:#f26d6d; --crit-bg:rgba(242,109,109,.13);
  --accent:#6ea8d8;
}
@media (prefers-color-scheme: dark){
  html:not([data-theme]){
    --bg:#0e1216; --panel:#161b22; --line:#242c36; --fg:#e8edf3; --fg2:#9aa6b4; --fg3:#66707e;
    --ok:#3ecf7a; --ok-bg:rgba(62,207,122,.13);
    --warn:#e0a33c; --warn-bg:rgba(224,163,60,.14);
    --crit:#f26d6d; --crit-bg:rgba(242,109,109,.13);
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
.wrap{max-width:960px;margin:0 auto;padding:34px 20px 70px}
header{display:flex;align-items:baseline;gap:12px;margin-bottom:6px;flex-wrap:wrap}
h1{font-size:21px;font-weight:680;letter-spacing:-.02em;margin:0}
.sub{color:var(--fg2);font-size:12.5px}
.banner{
  margin:14px 0 22px;padding:11px 15px;border-radius:var(--r);
  display:flex;align-items:center;gap:10px;font-size:13px;font-weight:550;
}
.banner.ok{background:var(--ok-bg);color:var(--ok)}
.banner.bad{background:var(--crit-bg);color:var(--crit)}
.dot{width:9px;height:9px;border-radius:50%;flex:none}
.dot.ok{background:var(--ok)} .dot.bad{background:var(--crit)} .dot.off{background:var(--fg3)} .dot.warn{background:var(--warn)}
.group{margin-bottom:26px}
.group-title{
  font-size:11px;font-weight:650;letter-spacing:.08em;text-transform:uppercase;
  color:var(--accent);opacity:.85;margin:0 0 10px;
}
.row{
  background:var(--panel);border:1px solid var(--line);border-radius:var(--r);
  padding:13px 16px;margin-bottom:8px;
  display:grid;grid-template-columns:1fr auto;gap:4px 16px;align-items:center;
}
.row-name{display:flex;align-items:center;gap:9px;font-weight:600;font-size:13.5px}
.row-meta{grid-column:1;display:flex;gap:14px;font-size:12px;color:var(--fg2);flex-wrap:wrap}
.row-meta b{color:var(--fg);font-weight:600}
.bars{grid-column:1/-1;display:flex;gap:2px;margin-top:8px;height:26px}
.bars i{flex:1;border-radius:2px;background:var(--ok);min-width:2px;opacity:.92}
.bars i.bad{background:var(--crit)}
.bars i.gap{background:var(--line)}
.bars-legend{grid-column:1/-1;display:flex;justify-content:space-between;font-size:10.5px;color:var(--fg2);margin-top:4px}
.pct{font-size:12.5px;font-weight:650}
.pct.ok{color:var(--ok)} .pct.warn{color:var(--warn)} .pct.crit{color:var(--crit)}
.svc{display:flex;align-items:center;gap:10px;padding:10px 16px;
  background:var(--panel);border:1px solid var(--line);border-radius:var(--r);margin-bottom:8px;font-size:13px}
.svc .name{font-weight:600}
.svc .lat{margin-left:auto;color:var(--fg2);font-size:12px}
footer{margin-top:34px;padding-top:16px;border-top:1px solid var(--line);
  color:var(--fg2);font-size:11.5px;display:flex;justify-content:space-between;gap:12px;flex-wrap:wrap}
.skel{background:var(--line);border-radius:6px;height:52px;margin-bottom:8px;
  animation:sh 1.3s infinite;background-size:200% 100%}
@keyframes sh{0%{background-position:200% 0}100%{background-position:-200% 0}}
@media (max-width:640px){
  .wrap{padding:22px 14px 60px}
  .row{grid-template-columns:1fr}
  .bars{height:22px}
}
html[data-density="compact"] .wrap{padding-top:22px;padding-bottom:48px}
html[data-density="compact"] .row{padding:10px 13px;margin-bottom:6px}
html[data-density="compact"] .svc{padding:8px 13px;margin-bottom:6px}
html[data-density="compact"] .group{margin-bottom:18px}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1 id="title">服务状态</h1>
    <span class="sub" id="stamp">加载中…</span>
  </header>
  <div id="banner"></div>
  <noscript><div class="banner bad"><span class="dot bad"></span>需要启用 JavaScript 才能显示实时状态。</div></noscript>
  <div id="services"></div>
  <div id="nodes"><div class="skel"></div><div class="skel"></div><div class="skel"></div></div>
  <footer>
    <span>数据每 30 秒自动刷新</span>
    <span class="mono" id="foot"></span>
  </footer>
</div>
<script src="/status.js"></script>
</body>
</html>`
