#!/usr/bin/env node
/**
 * ui-check.mjs — 管理面板 UI 关键不变量（可复跑，失败自动截图）
 *
 * 这些断言对应历史上真实出过的问题，每次发版后跑一遍即可：
 *   - 分页器是否生效（v1.35.3 统一每页 10 条）
 *   - 「自定义列」按钮是否存在（v1.35.6 定名）
 *   - 节点详情「系统信息」的值是否真的可见（v1.35.4 修复：值被挤出屏幕）
 *   - 公开页 Vue 是否挂载、有无横向溢出
 *
 * 用法：RCP_PASSWORD=... node tools/ui-check.mjs [baseURL] [nodeID]
 */
import { openSession } from './ui-session.mjs';

const base = process.argv[2] || 'https://status.endgo.top';
const nodeID = process.argv[3] || '';

const sess = await openSession({ base, admin: true });

try {
  /* ---------- 1. 全部节点：分页 ---------- */
  await sess.goto('#/nodes', 6000);
  const nodesPage = await sess.eval(`JSON.stringify((()=>{
    const pager = document.querySelector('.pager-info');
    // ★ 分页靠 display:none 隐藏行，DOM 里仍是全量——必须数「可见行」，
    //   否则断言拿到 13 行会误报（首版就踩过这个口径问题）。
    const all = Array.from(document.querySelectorAll('#view tbody tr'));
    const visible = all.filter(tr => tr.offsetParent !== null && tr.getBoundingClientRect().height > 0);
    return { pager: pager ? pager.textContent.trim().replace(/\\s+/g,' ') : '',
             rows: visible.length, domRows: all.length,
             docW: document.documentElement.scrollWidth, vw: innerWidth };
  })())`);
  await sess.ok('节点页有分页器', /第 \d+ \/ \d+ 页/.test(nodesPage.pager), nodesPage.pager);
  await sess.ok('节点页可见行 ≤10', nodesPage.rows <= 10,
    `可见 ${nodesPage.rows} 行 / DOM ${nodesPage.domRows} 行`);
  await sess.ok('节点页无横向溢出', nodesPage.docW <= nodesPage.vw + 2,
    `docW=${nodesPage.docW} vw=${nodesPage.vw}`);

  /* ---------- 2. 自定义列按钮 ---------- */
  const colBtn = await sess.eval(`JSON.stringify((()=>{
    const b = document.querySelector('.col-picker-btn');
    return b ? { text: b.textContent.trim(), title: b.title } : null;
  })())`);
  await sess.ok('「自定义列」按钮存在', !!colBtn && colBtn.text === '自定义列',
    colBtn ? `文案=${colBtn.text} title=${colBtn.title}` : '未找到按钮');
  await sess.ok('按钮带用途提示', !!colBtn && !!colBtn.title, colBtn ? colBtn.title : '');

  /* ---------- 3. 节点详情：系统信息可见性 ---------- */
  let target = nodeID;
  if (!target) {
    target = await sess.eval(
      `fetch('/api/nodes').then(r=>r.json()).then(d=>{const a=Array.isArray(d)?d:(d.nodes||[]);return a[0]?a[0].id:''})`,
    );
  }
  if (target) {
    await sess.goto('#/nodes/' + target, 8000);
    const detail = await sess.eval(`JSON.stringify((()=>{
      const rows = Array.from(document.querySelectorAll('.info-row'));
      const vals = rows.slice(0, 12).map(r=>{
        const v = r.querySelector('.info-v');
        if (!v) return { txt:'', inView:false, rowW:0 };
        const rr = r.getBoundingClientRect(), vr = v.getBoundingClientRect();
        return { txt: v.textContent.trim().slice(0,20), inView: vr.right <= innerWidth + 1, rowW: Math.round(rr.width) };
      });
      const grid = document.querySelector('.detail-grid');
      return { count: rows.length, vals, gridW: grid ? Math.round(grid.getBoundingClientRect().width) : 0,
               docW: document.documentElement.scrollWidth, vw: innerWidth };
    })())`);
    await sess.ok('详情页存在系统信息行', detail.count > 0, `${detail.count} 行`);
    const withText = detail.vals.filter((v) => v.txt && v.txt !== '—').length;
    await sess.ok('系统信息有实际值', withText >= 3, `${withText} 行有值`);
    await sess.ok('系统信息值都在视口内', detail.vals.every((v) => v.inView),
      detail.vals.filter((v) => !v.inView).map((v) => v.txt).join(',') || '全部可见');
    await sess.ok('详情页无横向溢出', detail.docW <= detail.vw + 2,
      `docW=${detail.docW} vw=${detail.vw}`);
  } else {
    await sess.ok('取到节点 ID', false, '节点列表为空');
  }

  /* ---------- 4. 公开页（无需登录） ---------- */
  await sess.goto('/status', 8000);
  const pub = await sess.eval(`JSON.stringify((()=>{
    return { mounted: !!document.querySelector('.ep-progress, .status-ring, .uptime-bar, main'),
             docW: document.documentElement.scrollWidth, vw: innerWidth,
             bars: document.querySelectorAll('.ep-progress').length };
  })())`);
  // 公开页不需要登录态，但同一会话的 cookie 无碍
  await sess.ok('公开页已渲染', pub.mounted, `进度条 ${pub.bars} 个`);
  await sess.ok('公开页无横向溢出', pub.docW <= pub.vw + 2, `docW=${pub.docW} vw=${pub.vw}`);

  await sess.shot('ui-check-final');
} finally {
  await sess.close();
}
