package web

// statusPageHTML 是公开状态页的完整 HTML。
//
// 设计约束：
//
//   - 内联 CSS + 同源 status.js，不请求任何第三方资源
//   - 只读，不暴露任何管理入口以外的后台链接
//   - 每 15 秒自动刷新数据，页面隐藏时暂停轮询
//   - CSP 为 script-src 'self'：所有交互必须用 addEventListener 绑定，
//     不能写 onclick="" 这类内联事件属性，也不能内联 <script>
//
// 页面结构由 status.js 按 hash 路由渲染：
// #/ （默认）是分组节点总览，#/n/<hid> 是单个节点详情。
const statusPageHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="robots" content="noindex">
<meta name="color-scheme" content="light dark">
<meta name="theme-color" content="#070b14">
<title>服务状态</title>
<style>
:root{
  --bg:#f3f6fb; --bg-soft:#eef3f9;
  --panel:rgba(255,255,255,.78); --panel-solid:#fff; --panel-2:rgba(246,249,253,.9);
  --line:rgba(15,23,42,.09); --line-2:rgba(15,23,42,.055);
  --fg:#0f172a; --fg2:#526077; --fg3:#8a97ab;
  --accent:#2563eb; --accent-2:#0891b2; --on-accent:#fff;
  --ok:#0f9f6e; --ok-bg:rgba(15,159,110,.10);
  --warn:#b7791f; --warn-bg:rgba(183,121,31,.12);
  --crit:#dc2626; --crit-bg:rgba(220,38,38,.10);
  --info:#2563eb; --info-bg:rgba(37,99,235,.09);
  --violet:#7c3aed; --violet-bg:rgba(124,58,237,.09);
  --glow-a:rgba(37,99,235,.13); --glow-b:rgba(8,145,178,.10);
  --shadow:0 1px 2px rgba(15,23,42,.04),0 14px 38px rgba(15,23,42,.06);
  --shadow-hover:0 2px 4px rgba(15,23,42,.05),0 22px 55px rgba(15,23,42,.10);
  --r:14px; --r-sm:10px;
  --mono:ui-monospace,"SF Mono",Menlo,Consolas,"Liberation Mono",monospace;
  color-scheme:light;
}
html[data-theme="dark"]{
  --bg:#070b14; --bg-soft:#0b1220;
  --panel:rgba(17,24,39,.72); --panel-solid:#111827; --panel-2:rgba(15,23,42,.82);
  --line:rgba(148,163,184,.15); --line-2:rgba(148,163,184,.08);
  --fg:#eef3fb; --fg2:#a6b4c8; --fg3:#6b7a90;
  --accent:#60a5fa; --accent-2:#22d3ee; --on-accent:#07111f;
  --ok:#34d399; --ok-bg:rgba(52,211,153,.12);
  --warn:#fbbf24; --warn-bg:rgba(251,191,36,.13);
  --crit:#f87171; --crit-bg:rgba(248,113,113,.13);
  --info:#60a5fa; --info-bg:rgba(96,165,250,.12);
  --violet:#a78bfa; --violet-bg:rgba(167,139,250,.12);
  --glow-a:rgba(37,99,235,.22); --glow-b:rgba(34,211,238,.12);
  --shadow:0 1px 2px rgba(0,0,0,.30),0 18px 50px rgba(0,0,0,.36);
  --shadow-hover:0 2px 4px rgba(0,0,0,.35),0 26px 70px rgba(0,0,0,.48);
  color-scheme:dark;
}
@media (prefers-color-scheme: dark){
  html:not([data-theme]){
    --bg:#070b14; --bg-soft:#0b1220;
    --panel:rgba(17,24,39,.72); --panel-solid:#111827; --panel-2:rgba(15,23,42,.82);
    --line:rgba(148,163,184,.15); --line-2:rgba(148,163,184,.08);
    --fg:#eef3fb; --fg2:#a6b4c8; --fg3:#6b7a90;
    --accent:#60a5fa; --accent-2:#22d3ee; --on-accent:#07111f;
    --ok:#34d399; --ok-bg:rgba(52,211,153,.12);
    --warn:#fbbf24; --warn-bg:rgba(251,191,36,.13);
    --crit:#f87171; --crit-bg:rgba(248,113,113,.13);
    --info:#60a5fa; --info-bg:rgba(96,165,250,.12);
    --violet:#a78bfa; --violet-bg:rgba(167,139,250,.12);
    --glow-a:rgba(37,99,235,.22); --glow-b:rgba(34,211,238,.12);
    --shadow:0 1px 2px rgba(0,0,0,.30),0 18px 50px rgba(0,0,0,.36);
    --shadow-hover:0 2px 4px rgba(0,0,0,.35),0 26px 70px rgba(0,0,0,.48);
    color-scheme:dark;
  }
}
*{box-sizing:border-box}
[hidden]{display:none!important}
html,body{margin:0;padding:0}
html{scroll-behavior:smooth}
body{
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Hiragino Sans GB","Microsoft YaHei",sans-serif;
  background:
    radial-gradient(980px 520px at 8% -12%,var(--glow-a),transparent 62%),
    radial-gradient(840px 460px at 96% -4%,var(--glow-b),transparent 58%),
    var(--bg);
  color:var(--fg); font-size:14px; line-height:1.5;
  -webkit-font-smoothing:antialiased;
  min-height:100vh;
}
body::before{
  content:""; position:fixed; inset:0; z-index:-1; pointer-events:none;
  background-image:
    linear-gradient(var(--line-2) 1px,transparent 1px),
    linear-gradient(90deg,var(--line-2) 1px,transparent 1px);
  background-size:52px 52px;
  -webkit-mask-image:radial-gradient(ellipse 80% 60% at 50% 0%,#000 20%,transparent 78%);
  mask-image:radial-gradient(ellipse 80% 60% at 50% 0%,#000 20%,transparent 78%);
  opacity:.55;
}
a{color:inherit}
button,input,select{font:inherit;color:inherit}
button{cursor:pointer}
::selection{background:var(--accent);color:var(--on-accent)}
.mono{font-family:var(--mono);font-variant-numeric:tabular-nums}
.wrap{max-width:1280px;margin:0 auto;padding:26px 24px 64px}

/* ---------- 顶部 ---------- */
.topbar{display:flex;align-items:center;justify-content:space-between;gap:18px;
  flex-wrap:wrap;margin-bottom:20px}
.brand{display:flex;align-items:center;gap:13px;min-width:0}
.brand-mark{
  width:42px;height:42px;border-radius:13px;flex:none;position:relative;
  background:linear-gradient(145deg,var(--accent),var(--accent-2));
  box-shadow:0 8px 24px var(--glow-a),inset 0 1px 0 rgba(255,255,255,.35);
  display:flex;align-items:flex-end;justify-content:center;gap:3px;padding:10px;
  overflow:hidden;
}
.brand-mark i{width:4px;border-radius:2px;background:rgba(255,255,255,.92);
  display:block;animation:barPulse 2.4s ease-in-out infinite}
.brand-mark i:nth-child(1){height:10px;animation-delay:0s}
.brand-mark i:nth-child(2){height:18px;animation-delay:.3s}
.brand-mark i:nth-child(3){height:13px;animation-delay:.6s}
.brand-mark::after{content:"";position:absolute;inset:0;
  background:linear-gradient(120deg,transparent 35%,rgba(255,255,255,.22),transparent 65%);
  transform:translateX(-120%);animation:brandShine 5s ease-in-out infinite}
.brand-text{min-width:0}
h1{font-size:21px;line-height:1.2;font-weight:700;letter-spacing:-.02em;margin:0;
  overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.brand-sub{display:flex;align-items:center;gap:7px;flex-wrap:wrap;margin-top:4px;
  font-size:12px;color:var(--fg3)}
.live-dot{width:7px;height:7px;border-radius:50%;background:var(--fg3);flex:none}
.live-dot.on{background:var(--ok);box-shadow:0 0 0 4px var(--ok-bg);animation:livePulse 2s infinite}
.live-dot.bad{background:var(--crit);box-shadow:0 0 0 4px var(--crit-bg)}
.brand-sub .sep{width:1px;height:10px;background:var(--line)}
.top-actions{display:flex;align-items:center;gap:8px;flex:none}
.hdr-btn,.icon-btn{
  display:inline-flex;align-items:center;gap:7px;border:1px solid var(--line);
  background:var(--panel);color:var(--fg2);border-radius:10px;padding:7px 12px;
  font-size:12.5px;text-decoration:none;transition:color .15s,border-color .15s,background .15s;
  backdrop-filter:blur(14px);
}
.hdr-btn:hover,.icon-btn:hover{color:var(--fg);border-color:var(--fg3);background:var(--panel-2)}
.icon-btn svg{width:15px;height:15px;display:block}
.hdr-btn.primary{background:var(--accent);border-color:transparent;color:var(--on-accent);font-weight:600}
.hdr-btn.primary:hover{filter:brightness(1.06);color:var(--on-accent)}

/* ---------- 通用卡片 ---------- */
.card,.node-card{
  background:var(--panel);border:1px solid var(--line);border-radius:var(--r);
  box-shadow:var(--shadow);
  -webkit-backdrop-filter:blur(16px) saturate(1.15);
  backdrop-filter:blur(16px) saturate(1.15);
}
@supports not ((backdrop-filter:blur(1px)) or (-webkit-backdrop-filter:blur(1px))){
  .card,.node-card,.hdr-btn,.icon-btn,.seg,.search{background:var(--panel-solid)}
}
.card{padding:18px 20px;margin-bottom:14px}
.card-h{display:flex;align-items:center;gap:10px;margin:0 0 13px;flex-wrap:wrap}
.card-h h2{font-size:12px;font-weight:700;letter-spacing:.09em;text-transform:uppercase;
  color:var(--fg2);margin:0;display:flex;align-items:center;gap:8px}
.card-h h2::before{content:"";width:3px;height:13px;border-radius:2px;
  background:linear-gradient(var(--accent),var(--accent-2))}
.card-h .hint{font-size:11.5px;color:var(--fg3)}
.card-h .spacer{flex:1}

/* ---------- 公告 / 状态横幅 ---------- */
.banner{display:flex;align-items:flex-start;gap:10px;padding:11px 14px;margin-bottom:12px;
  border-radius:12px;font-size:12.5px;font-weight:550;border:1px solid transparent;
  animation:cardIn .3s ease both}
.banner.ok{background:var(--ok-bg);color:var(--ok);border-color:color-mix(in srgb,var(--ok) 20%,transparent)}
.banner.bad{background:var(--crit-bg);color:var(--crit);border-color:color-mix(in srgb,var(--crit) 20%,transparent)}
.banner.warn{background:var(--warn-bg);color:var(--warn);border-color:color-mix(in srgb,var(--warn) 22%,transparent)}
.banner.info{background:var(--info-bg);color:var(--info);border-color:color-mix(in srgb,var(--info) 20%,transparent)}
.banner .txt{flex:1;min-width:0;white-space:pre-wrap}
.dot{width:8px;height:8px;border-radius:50%;flex:none;display:inline-block;background:var(--fg3)}
.dot.ok{background:var(--ok)} .dot.bad{background:var(--crit)}
.dot.off{background:var(--fg3)} .dot.warn{background:var(--warn)}
.dot.info,.dot.mt{background:var(--info)}

/* ---------- Hero ---------- */
.hero{position:relative;overflow:hidden;padding:22px 24px 20px;margin-bottom:14px}
.hero::before{content:"";position:absolute;width:420px;height:420px;right:-120px;top:-220px;
  border-radius:50%;background:radial-gradient(circle,var(--glow-a),transparent 68%);
  pointer-events:none}
.hero::after{content:"";position:absolute;width:320px;height:320px;left:-160px;bottom:-220px;
  border-radius:50%;background:radial-gradient(circle,var(--glow-b),transparent 68%);
  pointer-events:none}
.hero-main{display:flex;align-items:center;justify-content:space-between;gap:22px;
  flex-wrap:wrap;position:relative;z-index:1}
.hero-status{display:flex;align-items:center;gap:15px;min-width:0}
.status-orb{
  width:46px;height:46px;border-radius:50%;flex:none;display:grid;place-items:center;
  background:var(--ok-bg);color:var(--ok);position:relative;
}
.status-orb::before{content:"";width:16px;height:16px;border-radius:50%;background:currentColor;
  box-shadow:0 0 0 5px color-mix(in srgb,currentColor 16%,transparent)}
.status-orb::after{content:"";position:absolute;inset:0;border-radius:50%;
  border:1px solid currentColor;opacity:.25;animation:orbPulse 2.8s ease-out infinite}
.status-orb.warn{background:var(--warn-bg);color:var(--warn)}
.status-orb.crit{background:var(--crit-bg);color:var(--crit)}
.status-orb.info{background:var(--info-bg);color:var(--info)}
.status-orb.off{background:var(--line-2);color:var(--fg3)}
.hero-title{font-size:22px;font-weight:700;letter-spacing:-.02em;line-height:1.25;margin:0}
.hero-sub{font-size:12.5px;color:var(--fg2);margin-top:4px}
.health{display:flex;align-items:center;gap:14px;flex:none}
.health-ring{width:88px;height:88px;position:relative;flex:none}
.health-ring svg{width:100%;height:100%;transform:rotate(-90deg);display:block}
.health-ring .ring-bg{fill:none;stroke:var(--line-2);stroke-width:7}
.health-ring .ring-fg{fill:none;stroke:var(--ok);stroke-width:7;stroke-linecap:round;
  transition:stroke-dashoffset .8s cubic-bezier(.22,.61,.36,1),stroke .3s}
.health-ring .ring-fg.warn{stroke:var(--warn)}
.health-ring .ring-fg.crit{stroke:var(--crit)}
.health-ring .ring-fg.none{stroke:var(--fg3)}
.health-ring .ring-center{position:absolute;inset:0;display:grid;place-content:center;text-align:center}
.health-ring .ring-v{font-size:18px;font-weight:750;letter-spacing:-.03em;line-height:1}
.health-ring .ring-l{font-size:9.5px;color:var(--fg3);margin-top:4px;letter-spacing:.04em}
.health-copy{font-size:11.5px;color:var(--fg3);line-height:1.55;max-width:120px}
.health-copy b{display:block;color:var(--fg2);font-size:12px;font-weight:600;margin-bottom:2px}

.kpis{display:grid;grid-template-columns:repeat(auto-fit,minmax(132px,1fr));gap:9px;
  margin-top:19px;position:relative;z-index:1}
.kpi{background:var(--panel-2);border:1px solid var(--line-2);border-radius:11px;
  padding:10px 12px;min-width:0;transition:border-color .2s}
.kpi:hover{border-color:var(--line)}
.kpi-l{font-size:10.5px;color:var(--fg3);display:flex;align-items:center;gap:5px;
  white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.kpi-v{font-size:19px;font-weight:700;letter-spacing:-.025em;line-height:1.25;margin-top:3px;
  font-variant-numeric:tabular-nums}
.kpi-v small{font-size:11px;color:var(--fg3);font-weight:500;margin-left:2px;letter-spacing:0}
.kpi-f{font-size:10px;color:var(--fg3);margin-top:2px;white-space:nowrap;overflow:hidden;
  text-overflow:ellipsis}
.kpi-f .ok{color:var(--ok)} .kpi-f .warn{color:var(--warn)} .kpi-f .crit{color:var(--crit)}
.kpi-v.changed{animation:valueFlash .55s ease}

/* ---------- 工具栏 ---------- */
.toolbar{display:flex;align-items:center;gap:9px;flex-wrap:wrap;margin:18px 0 12px}
.search{display:flex;align-items:center;gap:8px;flex:1 1 240px;max-width:380px;
  background:var(--panel);border:1px solid var(--line);border-radius:10px;
  padding:7px 11px;transition:border-color .15s,box-shadow .15s;
  -webkit-backdrop-filter:blur(14px);backdrop-filter:blur(14px)}
.search:focus-within{border-color:var(--accent);box-shadow:0 0 0 3px var(--info-bg)}
.search svg{width:14px;height:14px;color:var(--fg3);flex:none}
.search input{border:0;outline:0;background:transparent;width:100%;min-width:0;font-size:12.5px}
.search input::placeholder{color:var(--fg3)}
.seg{display:inline-flex;align-items:center;gap:2px;background:var(--panel);
  border:1px solid var(--line);border-radius:10px;padding:3px;
  -webkit-backdrop-filter:blur(14px);backdrop-filter:blur(14px)}
.seg button{border:0;background:transparent;color:var(--fg2);border-radius:7px;
  padding:5px 10px;font-size:12px;font-weight:550;transition:background .15s,color .15s;
  white-space:nowrap}
.seg button:hover{color:var(--fg)}
.seg button.on{background:var(--accent);color:var(--on-accent);box-shadow:0 4px 14px var(--glow-a)}
.seg button b{font-weight:650;margin-left:4px;opacity:.75}
.select-wrap{position:relative;display:inline-flex;align-items:center}
.select-wrap select{appearance:none;-webkit-appearance:none;background:var(--panel);
  border:1px solid var(--line);border-radius:10px;padding:8px 30px 8px 11px;
  font-size:12px;color:var(--fg2);outline:0;cursor:pointer;
  -webkit-backdrop-filter:blur(14px);backdrop-filter:blur(14px)}
.select-wrap select:focus{border-color:var(--accent)}
.select-wrap::after{content:"";position:absolute;right:11px;top:50%;width:6px;height:6px;
  border-right:1.5px solid var(--fg3);border-bottom:1.5px solid var(--fg3);
  transform:translateY(-70%) rotate(45deg);pointer-events:none}
.tool-count{margin-left:auto;font-size:11.5px;color:var(--fg3);white-space:nowrap}
.tool-count b{color:var(--fg2);font-weight:650}

/* ---------- 节点卡片 ---------- */
.node-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(338px,1fr));gap:13px}
.node-card{padding:16px 17px;cursor:pointer;position:relative;
  transition:transform .18s ease,border-color .18s,box-shadow .18s;
  animation:cardIn .34s ease both;outline:0}
.node-card:hover{transform:translateY(-2px);border-color:color-mix(in srgb,var(--accent) 34%,var(--line));
  box-shadow:var(--shadow-hover)}
.node-card.no-link{cursor:default}
.node-card.no-link:hover{transform:none;border-color:var(--line);box-shadow:var(--shadow)}
.node-card:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.group-head{display:flex;align-items:center;gap:8px;grid-column:1/-1;margin:4px 2px -3px;
  font-size:10.5px;font-weight:700;color:var(--fg2);letter-spacing:.09em;text-transform:uppercase}
.group-head::after{content:"";flex:1;height:1px;background:var(--line)}
.group-head em{font-style:normal;font-weight:500;color:var(--fg3);font-size:10px;
  letter-spacing:0;text-transform:none}
.node-card.state-offline,.node-card.state-no_data{opacity:.84}
.node-card.state-offline:hover,.node-card.state-no_data:hover{opacity:1}
.nc-head{display:flex;align-items:flex-start;justify-content:space-between;gap:10px}
.nc-id{display:flex;align-items:flex-start;gap:9px;min-width:0}
.nc-dot{width:9px;height:9px;border-radius:50%;background:var(--fg3);flex:none;margin-top:6px;
  box-shadow:0 0 0 4px var(--line-2)}
.nc-dot.ok{background:var(--ok);box-shadow:0 0 0 4px var(--ok-bg)}
.nc-dot.warn{background:var(--warn);box-shadow:0 0 0 4px var(--warn-bg)}
.nc-dot.bad{background:var(--crit);box-shadow:0 0 0 4px var(--crit-bg)}
.nc-dot.info{background:var(--info);box-shadow:0 0 0 4px var(--info-bg)}
.nc-name{font-size:15px;font-weight:680;letter-spacing:-.01em;line-height:1.35;
  overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.nc-tags{display:flex;align-items:center;gap:5px;flex-wrap:wrap;margin-top:4px}
.tag{display:inline-flex;align-items:center;gap:4px;font-size:9.5px;font-weight:600;
  padding:2px 6px;border-radius:5px;background:var(--info-bg);color:var(--info);
  letter-spacing:.01em;line-height:1.4;white-space:nowrap}
.tag.mt{background:var(--info-bg);color:var(--info)}
.tag.soft{background:var(--line-2);color:var(--fg2);font-weight:500}
.nc-side{text-align:right;flex:none;display:flex;flex-direction:column;align-items:flex-end;gap:4px}
.state-badge{display:inline-flex;align-items:center;gap:4px;font-size:10px;font-weight:650;
  padding:2px 7px;border-radius:99px;background:var(--line-2);color:var(--fg2);white-space:nowrap}
.state-badge::before{content:"";width:5px;height:5px;border-radius:50%;background:currentColor}
.state-badge.ok{background:var(--ok-bg);color:var(--ok)}
.state-badge.warn{background:var(--warn-bg);color:var(--warn)}
.state-badge.bad{background:var(--crit-bg);color:var(--crit)}
.state-badge.mt{background:var(--info-bg);color:var(--info)}
.nc-uptime{font-size:11px;color:var(--fg3);white-space:nowrap}
.nc-uptime b{font-size:12.5px;color:var(--fg2);font-weight:650}
.nc-uptime b.ok{color:var(--ok)} .nc-uptime b.warn{color:var(--warn)} .nc-uptime b.crit{color:var(--crit)}

.nc-facts{display:flex;flex-wrap:wrap;gap:5px 12px;font-size:11px;color:var(--fg3);
  margin-top:10px;padding-bottom:9px;border-bottom:1px dashed var(--line)}
.nc-facts span{white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:100%}
.nc-facts b{color:var(--fg2);font-weight:600}

.res-list{display:grid;gap:8px;margin-top:11px}
.res-row{display:grid;grid-template-columns:38px minmax(0,1fr) 42px;gap:8px;align-items:center}
.res-l{font-size:10.5px;color:var(--fg3);letter-spacing:.02em}
.res-bar{height:6px;border-radius:99px;background:var(--line-2);overflow:hidden;position:relative}
.res-bar i{display:block;height:100%;width:0;border-radius:inherit;
  background:linear-gradient(90deg,var(--ok),var(--accent-2));
  transition:width .7s cubic-bezier(.22,.61,.36,1),background .3s}
.res-bar i.warn{background:linear-gradient(90deg,var(--warn),#f59e0b)}
.res-bar i.crit{background:linear-gradient(90deg,var(--crit),#f97316)}
.res-v{font-family:var(--mono);font-size:11.5px;font-weight:650;text-align:right;
  font-variant-numeric:tabular-nums;white-space:nowrap}
.res-v.warn{color:var(--warn)} .res-v.crit{color:var(--crit)}
.nc-net{display:flex;align-items:center;gap:13px;flex-wrap:wrap;margin-top:11px;
  font-size:11.5px;color:var(--fg2)}
.net-item{display:inline-flex;align-items:baseline;gap:4px;white-space:nowrap}
.net-arrow{font-size:10px;color:var(--fg3)}
.net-item b{font-weight:600;color:var(--fg)}
.net-total{margin-left:auto;font-size:10px;color:var(--fg3);white-space:nowrap;
  overflow:hidden;text-overflow:ellipsis;max-width:100%}
.nc-foot{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:12px;align-items:end;
  margin-top:11px;padding-top:10px;border-top:1px solid var(--line-2)}
.spark-wrap{min-width:0}
.spark-head{display:flex;justify-content:space-between;gap:8px;font-size:9.5px;
  color:var(--fg3);margin-bottom:3px}
.spark-box{height:34px}
.spark-box svg{width:100%;height:100%;display:block;overflow:visible}
.avail{text-align:right}
.avail-v{font-size:12.5px;font-weight:700;font-family:var(--mono)}
.avail-v.ok{color:var(--ok)} .avail-v.warn{color:var(--warn)} .avail-v.crit{color:var(--crit)}
.avail-v.none{color:var(--fg3)}
.avail-l{font-size:9.5px;color:var(--fg3);margin-top:1px}
.mini-bars{display:flex;gap:1px;height:13px;width:100px;margin:4px 0 0 auto}
.mini-bars i{flex:1;min-width:1px;background:var(--ok);border-radius:1px;opacity:.9}
.mini-bars i.warn{background:var(--warn)} .mini-bars i.crit{background:var(--crit)}
.mini-bars i.gap{background:var(--line)}
.mini-bars i:hover{outline:1px solid var(--fg3);outline-offset:1px}
.empty{color:var(--fg3);font-size:12.5px;text-align:center;padding:28px 12px}
.empty-card{grid-column:1/-1}

/* ---------- 服务 ---------- */
.svc-list{display:grid;grid-template-columns:repeat(auto-fit,minmax(280px,1fr));gap:9px}
.svc{display:grid;grid-template-columns:auto minmax(0,1fr) auto;gap:4px 10px;
  align-items:center;padding:10px 12px;border:1px solid var(--line-2);border-radius:10px;
  background:var(--panel-2)}
.svc-dot{width:8px;height:8px;border-radius:50%;background:var(--fg3);flex:none}
.svc-dot.ok{background:var(--ok)} .svc-dot.bad{background:var(--crit)}
.svc-dot.off{background:var(--fg3)}
.svc-lat.none{color:var(--fg3)}
.svc-name{font-size:12.5px;font-weight:600;overflow:hidden;text-overflow:ellipsis;
  white-space:nowrap}
.svc-side{display:flex;align-items:baseline;gap:8px;flex:none;font-size:11px;color:var(--fg3)}
.svc-lat{font-family:var(--mono);font-weight:600;color:var(--fg2)}
.svc-lat.ok{color:var(--ok)} .svc-lat.warn{color:var(--warn)} .svc-lat.crit{color:var(--crit)}
.svc-spark{grid-column:1/-1;height:24px;margin-top:2px}
.svc-spark svg{width:100%;height:100%;display:block}
.svc-meta{grid-column:1/-1;display:flex;justify-content:space-between;gap:8px;
  font-size:10px;color:var(--fg3)}
.service-note{font-size:10px;color:var(--fg3);margin-left:auto}

/* ---------- 可用率日柱 ---------- */
.upbar{display:flex;gap:2px;height:44px;align-items:stretch}
.upbar i{flex:1;border-radius:2px;background:var(--ok);min-width:2px;opacity:.9;
  transition:background .2s,opacity .2s}
.upbar i.warn{background:var(--warn)} .upbar i.crit{background:var(--crit)}
.upbar i.gap{background:var(--line-2)}
.upbar i:hover{outline:1px solid var(--fg3);outline-offset:1px;opacity:1}
.upbar-foot{display:flex;justify-content:space-between;gap:12px;font-size:10.5px;
  color:var(--fg3);margin-top:7px;flex-wrap:wrap}
.upbar-foot .mid{font-family:var(--mono);color:var(--fg2)}

/* ---------- 事件时间线 ---------- */
.tl{border-left:2px solid var(--line);margin-left:5px;padding-left:16px}
.tl-day{font-size:10.5px;color:var(--fg3);margin:14px 0 6px;font-weight:650;
  letter-spacing:.03em}
.tl-day:first-child{margin-top:0}
.tl-item{position:relative;padding:6px 0}
.tl-item::before{content:"";position:absolute;left:-23px;top:13px;width:9px;height:9px;
  border-radius:50%;background:var(--fg3);box-shadow:0 0 0 3px var(--bg)}
.tl-item.warn::before{background:var(--warn)}
.tl-item.crit::before{background:var(--crit)}
.tl-item.info::before{background:var(--info)}
.tl-t{font-weight:600;font-size:12.5px;display:flex;align-items:center;gap:6px;flex-wrap:wrap}
.tl-badge{font-size:9.5px;padding:1px 5px;border-radius:4px;background:var(--line-2);
  color:var(--fg2);font-weight:500}
.tl-meta{font-size:10.5px;color:var(--fg3);margin-top:2px;font-family:var(--mono)}
.tl-body{font-size:12px;color:var(--fg2);margin-top:4px;white-space:pre-wrap}

/* ---------- 详情 ---------- */
.back{display:inline-flex;align-items:center;gap:6px;border:1px solid var(--line);
  background:var(--panel);color:var(--fg2);border-radius:9px;padding:6px 11px;
  font-size:12px;margin-bottom:12px;text-decoration:none;transition:color .15s,border-color .15s}
.back:hover{color:var(--fg);border-color:var(--fg3)}
.detail-card{padding:20px 22px}
.dhead{display:flex;align-items:flex-start;gap:14px;flex-wrap:wrap}
.dhead-main{min-width:0;flex:1}
.dhead .kicker{font-size:10.5px;color:var(--fg3);letter-spacing:.1em;
  text-transform:uppercase;font-weight:650;margin-bottom:3px}
.dhead h2{font-size:25px;font-weight:730;letter-spacing:-.03em;margin:0;line-height:1.2}
.dmeta{font-size:11.5px;color:var(--fg2);display:flex;gap:7px 13px;flex-wrap:wrap;margin-top:6px}
.dhead-side{text-align:right;display:flex;flex-direction:column;gap:5px;align-items:flex-end}
.dhead-side .big{font-family:var(--mono);font-size:19px;font-weight:700}
.dhead-side .big.ok{color:var(--ok)} .dhead-side .big.warn{color:var(--warn)}
.dhead-side .big.crit{color:var(--crit)} .dhead-side .big.none{color:var(--fg3)}
.dhead-side .lbl{font-size:10px;color:var(--fg3)}
.rings{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:11px;margin-top:16px}
.ring-card{background:var(--panel-2);border:1px solid var(--line-2);border-radius:12px;
  padding:12px;display:flex;align-items:center;gap:12px}
.ring-wrap{position:relative;width:84px;height:84px;flex:none}
.ring-wrap svg{width:100%;height:100%;transform:rotate(-90deg);display:block}
.ring-wrap .rbg{fill:none;stroke:var(--line-2);stroke-width:7}
.ring-wrap .rfg{fill:none;stroke:var(--accent);stroke-width:7;stroke-linecap:round;
  transition:stroke-dashoffset .7s cubic-bezier(.22,.61,.36,1),stroke .3s}
.ring-center{position:absolute;inset:0;display:grid;place-content:center;text-align:center}
.ring-center b{font-family:var(--mono);font-size:16px;font-weight:700;line-height:1}
.ring-center small{font-size:9px;color:var(--fg3);margin-top:3px}
.ring-copy{min-width:0}
.ring-copy .l{font-size:10.5px;color:var(--fg3);letter-spacing:.04em}
.ring-copy .s{font-size:10px;color:var(--fg3);margin-top:4px;font-family:var(--mono);
  white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.detail-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:13px}
.detail-col{display:flex;flex-direction:column;gap:13px;min-width:0}
.info-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(108px,1fr));gap:8px}
.info-cell{background:var(--panel-2);border:1px solid var(--line-2);border-radius:9px;
  padding:9px 11px;min-width:0}
.info-cell .l{font-size:10px;color:var(--fg3)}
.info-cell .v{font-size:13px;font-weight:650;margin-top:2px;font-family:var(--mono);
  white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.info-cell .s{font-size:10px;color:var(--fg3);margin-top:1px}
.kv{display:grid;grid-template-columns:auto 1fr;gap:7px 18px;font-size:12px;margin:0}
.kv dt{color:var(--fg3);white-space:nowrap}
.kv dd{margin:0;color:var(--fg);font-family:var(--mono);word-break:break-all}
.range-tabs{display:inline-flex;gap:2px;background:var(--panel-2);border:1px solid var(--line-2);
  border-radius:8px;padding:2px;margin-left:auto}
.range-tabs button{border:0;background:transparent;color:var(--fg3);font-size:11px;
  padding:4px 9px;border-radius:6px;font-weight:550;transition:background .15s,color .15s}
.range-tabs button:hover{color:var(--fg)}
.range-tabs button.on{background:var(--accent);color:var(--on-accent)}
.chart{margin-top:11px}
.chart:first-of-type{margin-top:0}
.chart-t{display:flex;justify-content:space-between;align-items:baseline;gap:10px;
  font-size:11px;color:var(--fg3);margin-bottom:5px}
.chart-t .name{font-weight:600;color:var(--fg2)}
.chart-t b{font-family:var(--mono);font-size:12px;color:var(--fg);font-weight:650}
.chart-box{height:150px}
.chart-box svg{width:100%;height:100%;display:block}
.legend{display:flex;align-items:center;gap:12px;font-size:10px;color:var(--fg3);
  margin-top:4px;flex-wrap:wrap}
.legend i{width:7px;height:7px;border-radius:2px;display:inline-block;margin-right:4px}
.detail-svc{display:grid;gap:8px}
.detail-svc .svc{grid-template-columns:auto minmax(0,1fr) auto}
.svc-chart{grid-column:1/-1;height:44px;margin-top:2px}
.svc-chart svg{width:100%;height:100%;display:block}
.svc-stats{grid-column:1/-1;display:flex;gap:12px;font-size:10px;color:var(--fg3);
  flex-wrap:wrap}
.svc-stats b{color:var(--fg2);font-weight:600}

/* ---------- 页脚 / 骨架 ---------- */
footer{margin-top:26px;padding-top:16px;border-top:1px solid var(--line);
  color:var(--fg3);font-size:11px;display:flex;justify-content:space-between;
  gap:10px;flex-wrap:wrap}
footer .mono{color:var(--fg3)}
.skel{background:linear-gradient(90deg,var(--line-2) 25%,var(--line) 37%,var(--line-2) 63%);
  background-size:400% 100%;border-radius:10px;animation:shimmer 1.5s ease infinite}
.skeleton-stack{animation:skelOut 1ms linear 12s forwards}
.skel-hero{height:220px;margin-bottom:14px}
.skel-row{display:grid;grid-template-columns:repeat(3,1fr);gap:13px;margin-top:16px}
.skel-card{height:250px}
#jsfail{display:none;animation:jsfailIn 1ms linear 12s forwards}
@keyframes shimmer{0%{background-position:100% 0}100%{background-position:-100% 0}}
@keyframes skelOut{to{opacity:0;visibility:hidden}}
@keyframes jsfailIn{to{display:block}}
@keyframes cardIn{from{opacity:0;transform:translateY(7px)}to{opacity:1;transform:none}}
@keyframes valueFlash{0%{color:var(--accent)}100%{color:inherit}}
@keyframes livePulse{0%,100%{box-shadow:0 0 0 4px var(--ok-bg)}50%{box-shadow:0 0 0 7px transparent}}
@keyframes orbPulse{0%{transform:scale(1);opacity:.28}70%{transform:scale(1.5);opacity:0}100%{opacity:0}}
@keyframes barPulse{0%,100%{transform:scaleY(.75);opacity:.85}50%{transform:scaleY(1);opacity:1}}
@keyframes brandShine{0%,55%{transform:translateX(-140%)}75%,100%{transform:translateX(140%)}}

/* ---------- 响应式 ---------- */
@media (max-width:980px){
  .detail-grid{grid-template-columns:1fr}
  .rings{grid-template-columns:repeat(3,1fr)}
}
@media (max-width:720px){
  .wrap{padding:18px 13px 48px}
  .topbar{gap:12px;margin-bottom:14px}
  .brand-mark{width:37px;height:37px;border-radius:11px;padding:9px}
  h1{font-size:18px}
  .brand-sub{font-size:11px}
  .hdr-btn,.icon-btn{padding:6px 9px;font-size:11.5px}
  .icon-btn #themeLabel{display:none}
  .hero{padding:17px 16px 15px}
  .hero-main{gap:14px}
  .hero-title{font-size:19px}
  .health{display:none}
  .kpis{grid-template-columns:repeat(2,1fr);gap:7px;margin-top:14px}
  .kpi-v{font-size:17px}
  .node-grid{grid-template-columns:1fr;gap:10px}
  .node-card{padding:14px}
  .toolbar{gap:7px;margin:14px 0 10px}
  .search{flex:1 1 100%;max-width:none;order:1}
  .seg{order:2}
  .select-wrap{order:3}
  .tool-count{order:4;margin-left:0}
  .rings{gap:7px;margin-top:13px}
  .ring-card{flex-direction:column;gap:5px;padding:9px 5px;text-align:center}
  .ring-wrap{width:70px;height:70px}
  .ring-copy .l{font-size:9.5px}
  .ring-copy .s{display:none}
  .detail-card{padding:16px 15px}
  .dhead h2{font-size:21px}
  .dhead-side{text-align:left;align-items:flex-start}
  .detail-grid{gap:10px}
  .chart-box{height:132px}
  .upbar{height:34px}
  .card{padding:15px 14px}
  .svc-list{grid-template-columns:1fr}
  .skel-row{grid-template-columns:1fr}
  footer{font-size:10px}
}
@media (max-width:400px){
  .kpis{grid-template-columns:1fr 1fr}
  .res-row{grid-template-columns:34px minmax(0,1fr) 38px}
  .mini-bars{width:82px}
}
@media (prefers-reduced-motion: reduce){
  *,*::before,*::after{animation-duration:.01ms!important;animation-iteration-count:1!important;
    transition-duration:.01ms!important;scroll-behavior:auto!important}
}
html[data-density="compact"] .wrap{padding-top:18px;padding-bottom:42px}
html[data-density="compact"] .card{padding:14px 16px;margin-bottom:10px}
html[data-density="compact"] .node-card{padding:13px 14px}
html[data-density="compact"] .hero{padding:17px 18px}
html[data-density="compact"] .kpis{margin-top:13px}
</style>
</head>
<body>
<div class="wrap">
  <header class="topbar">
    <div class="brand">
      <span class="brand-mark" aria-hidden="true"><i></i><i></i><i></i></span>
      <div class="brand-text">
        <h1 id="title">服务状态</h1>
        <div class="brand-sub">
          <span class="live-dot" id="liveDot"></span>
          <span id="liveText">正在连接</span>
          <span class="sep"></span>
          <span id="stamp">加载中…</span>
        </div>
      </div>
    </div>
    <div class="top-actions">
      <button class="icon-btn" id="themeBtn" type="button" aria-label="切换外观">
        <span id="themeIcon" aria-hidden="true"></span><span id="themeLabel">外观</span>
      </button>
      <a class="hdr-btn primary" href="/">管理登录</a>
    </div>
  </header>
  <noscript><div class="banner bad"><span class="dot bad"></span><span class="txt">需要启用 JavaScript 才能显示实时状态。</span></div></noscript>
  <div id="jsfail" class="banner bad"><span class="dot bad"></span><span class="txt">状态脚本未能加载（可能被浏览器扩展或 CSP 拦截），请刷新页面重试。</span></div>
  <div id="app">
    <div class="skeleton-stack">
      <div class="skel skel-hero"></div>
      <div class="skel-row"><div class="skel skel-card"></div><div class="skel skel-card"></div><div class="skel skel-card"></div></div>
    </div>
  </div>
  <footer>
    <span id="foot">数据每 15 秒自动刷新 · 页面隐藏时暂停</span>
    <span class="mono" id="foot2"></span>
  </footer>
</div>
<script src="/status.js"></script>
</body>
</html>`
