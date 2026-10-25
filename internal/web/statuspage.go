package web

// statusPageHTML 是公开状态页的 HTML 外壳。
//
// 设计约束：
//
//   - 只加载同源静态资源（Vue、ep-shim.js、status.css/js），不请求第三方 CDN
//   - Element Plus 已由 ep-shim.js 的同名轻量组件替代（v1.28.0 起，
//     页面负载从 ~1.5MB 降到 ~200KB），对外仍暴露 window.ElementPlus 接口
//   - 启用的主题包由 handleStatusPage 注入同源的 /themes/<name>/status.css|js
//   - 只读，不暴露任何管理入口以外的后台链接
//   - SSE 推送 + 10 秒轮询兜底，页面隐藏时暂停刷新
//   - CSP 为 script-src 'self'：不允许内联 <script>，也不允许 eval/new Function，
//     因此 Vue 使用 runtime-only 构建，模板全部由 status.js 的 h() 生成
//
// 页面结构由 status.js 按 hash 路由渲染：
// #/ （默认）是分组节点总览，#/n/<hid> 是单个节点详情，
// #/events、#/activity 是事件与活动的独立分页列表。
//
// theme-boot.js 必须在样式表之前执行：它只根据 localStorage 与系统偏好
// 先给 <html> 打上 dark 类，避免深色模式下出现一帧白屏。服务端主题
// （强调色、圆角、紧凑）由 status.js 拉到 /api/theme 后再补齐。
const statusPageHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="robots" content="noindex">
<meta name="color-scheme" content="light dark">
<meta name="theme-color" content="#f2f4f8">
<title>服务状态</title>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'><rect width='32' height='32' rx='8' fill='%232f5d8a'/><path d='M8 20l5-7 4 5 3-4 4 6' stroke='white' stroke-width='2.2' fill='none' stroke-linecap='round' stroke-linejoin='round'/></svg>">
<script src="/theme-boot.js"></script>
<style>
/* 资源加载失败时的兜底：骨架屏与「脚本未能加载」提示。
 * 正常渲染时 status.js 会给 <html> 加 js-ok 并挂载 Vue，骨架随之被替换。 */
html,body{margin:0;padding:0}
body{background:#f2f4f8;font-family:system-ui,-apple-system,"Segoe UI","PingFang SC","Microsoft YaHei","Noto Sans SC",sans-serif}
.nojs{margin:16px;padding:12px 14px;border-radius:10px;background:#fdecec;color:#c0392b;font-size:13px}
.skel{background:linear-gradient(90deg,#edeff3 25%,#e4e7ed 37%,#edeff3 63%);
  background-size:400% 100%;border-radius:10px;animation:shimmer 1.5s ease infinite}
.skeleton-stack{max-width:1280px;margin:0 auto;padding:22px 24px;
  animation:skelOut 1ms linear 12s forwards}
.skel-hero{height:200px;margin-bottom:14px}
.skel-row{display:grid;grid-template-columns:repeat(3,1fr);gap:13px}
.skel-card{height:250px}
/* 脚本正常执行时会立刻给 <html> 加 js-ok：兜底提示必须随之消失，
 * 否则每次成功加载的页面在 12 秒后也会弹出「脚本未能加载」。
 * 用 visibility/opacity 而不是 display：display 在部分浏览器里不可动画。 */
#jsfail{visibility:hidden;opacity:0;animation:jsfailIn 1ms linear 12s forwards}
html.js-ok #jsfail{display:none}
@keyframes shimmer{0%{background-position:100% 0}100%{background-position:-100% 0}}
@keyframes skelOut{to{opacity:0;visibility:hidden}}
@keyframes jsfailIn{to{visibility:visible;opacity:1}}
@media (max-width:720px){
  .skeleton-stack{padding:15px 13px}
  .skel-row{grid-template-columns:1fr}
}
</style>
<link rel="stylesheet" href="/status.css">
</head>
<body>
<noscript><div class="nojs">需要启用 JavaScript 才能显示实时状态。</div></noscript>
<div id="jsfail" class="nojs">状态脚本未能加载（可能被浏览器扩展或 CSP 拦截），请刷新页面重试。</div>
<div id="app">
  <div class="skeleton-stack">
    <div class="skel skel-hero"></div>
    <div class="skel-row"><div class="skel skel-card"></div><div class="skel skel-card"></div><div class="skel skel-card"></div></div>
  </div>
</div>
<script src="/vendor/vue/vue.global.prod.js"></script>
<script src="/ep-shim.js"></script>
<script src="/status.js"></script>
</body>
</html>`
