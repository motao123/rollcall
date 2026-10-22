/* 在样式表生效前同步确定深浅色，避免首屏闪白。
 * 服务端主题模式需要请求 /api/theme 才知道，这里只处理本机/系统偏好；
 * 服务端强制模式会在脚本加载后接管。 */
(function () {
  var mode = null;
  try { mode = localStorage.getItem('rollcall-status-theme'); } catch (e) { /* 隐私模式 */ }
  if (mode !== 'light' && mode !== 'dark') mode = null;
  var dark = mode === 'dark' ||
    (mode === null && window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
  var root = document.documentElement;
  root.setAttribute('data-theme', dark ? 'dark' : 'light');
  if (dark) root.classList.add('dark');
})();
