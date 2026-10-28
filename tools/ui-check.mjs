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
  await sess.goto('#/nodes', 2000);
  await sess.waitStableView({ selector: '#view .col-picker-btn' });
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

  /* ---------- 1b. 「可升级」必须有可执行入口（v1.37.2） ---------- */
  const up = await sess.eval(`JSON.stringify((()=>{
    const badges = Array.from(document.querySelectorAll('.badge-click[data-node-upgrade]'));
    const btns = Array.from(document.querySelectorAll('button[data-node-upgrade]'));
    const outdated = Array.from(document.querySelectorAll('#view tbody tr'))
      .filter(tr => tr.offsetParent !== null && tr.textContent.includes('可升级')).length;
    return { badges: badges.length, btns: btns.length, outdated,
             clickable: badges.every(b => getComputedStyle(b).cursor === 'pointer') };
  })())`);
  await sess.ok('可升级徽标可点击', up.clickable && (up.outdated === 0 || up.badges >= up.outdated),
    `${up.outdated} 台可升级 / ${up.badges} 个可点徽标`);
  await sess.ok('可升级行有「升级」按钮', up.outdated === 0 || up.btns >= up.outdated,
    `按钮 ${up.btns} 个`);

  /* ---------- 2. 自定义列按钮 ---------- */
  const colBtn = await sess.eval(`JSON.stringify((()=>{
    const b = document.querySelector('.col-picker-btn');
    return b ? { text: b.textContent.trim(), title: b.title } : null;
  })())`);
  await sess.ok('「自定义列」按钮存在', !!colBtn && colBtn.text === '自定义列',
    colBtn ? `文案=${colBtn.text} title=${colBtn.title}` : '未找到按钮');
  await sess.ok('按钮带用途提示', !!colBtn && !!colBtn.title, colBtn ? colBtn.title : '');

  /* ---------- 2b. 告警历史：服务端分页（v1.39.0） ---------- */
  // 这一组断言锁住"历史记录可翻到最早"这件事：
  // 老实现只取最近 200 条再本地分页，存储里的 500 条有 300 条不可达。
  // 页面数必须以服务端 total 计算，而不是当前页条数。
  await sess.goto('/#/alerts', 2000);
  await sess.waitStableView({ selector: '#notifyJobsAllBtn' });
  const alertsPage = await sess.eval(`JSON.stringify((()=>{
    const meta = Array.from(document.querySelectorAll('.section-actions .section-meta'))
      .map(m => m.textContent.trim());
    const kpis = Array.from(document.querySelectorAll('.kpi')).map(k => k.textContent.trim().replace(/\\s+/g,' '));
    return { meta, kpis, items: document.querySelectorAll('.alert-item').length,
             hasAllJobsBtn: !!document.querySelector('#notifyJobsAllBtn') };
  })())`);
  const totalText = (alertsPage.meta.find((m) => /共 \d+ 条|检索命中 \d+ 条/.test(m)) || '');
  const totalNum = Number((totalText.match(/(\d+) 条/) || [])[1] || 0);
  await sess.ok('告警页显示服务端总数', totalNum > 0, totalText || '未找到总数文案');
  await sess.ok('单页不超过 10 条', alertsPage.items <= 10, `本页 ${alertsPage.items} 条（总数 ${totalNum}）`);
  await sess.ok('存在「全部投递记录」入口', alertsPage.hasAllJobsBtn, '');

  // 接口层：v1 信封必须完整（前端与外部脚本都依赖这五个字段）
  const env = await sess.eval(`fetch('/api/v1/alerts?limit=5',{headers:{'X-Requested-With':'XMLHttpRequest'}})
    .then(r=>r.json()).then(d=>JSON.stringify({
      has: ['items','total','limit','offset','hasMore'].filter(k => !(k in d)),
      total: d.total, len: (d.items||[]).length, limit: d.limit, hasMore: d.hasMore }))`);
  await sess.ok('v1 分页信封字段完整', env.has.length === 0, env.has.length ? `缺 ${env.has.join(',')}` : `total=${env.total} limit=${env.limit}`);
  await sess.ok('v1 单页受 limit 约束', env.len <= env.limit,
    `返回 ${env.len} 条 / limit ${env.limit}`);
  await sess.ok('v1 hasMore 与 total 一致', env.hasMore === (env.limit < env.total),
    `hasMore=${env.hasMore} limit=${env.limit} total=${env.total}`);

  /* ---------- 3. 节点详情：系统信息可见性 ---------- */
  let target = nodeID;
  if (!target) {
    target = await sess.eval(
      `fetch('/api/nodes').then(r=>r.json()).then(d=>{const a=Array.isArray(d)?d:(d.nodes||[]);return a[0]?a[0].id:''})`,
    );
  }
  if (target) {
    await sess.goto('#/nodes/' + target, 2000);
    await sess.waitStableView({ selector: '.info-row' });
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

  /* ---------- 2c. CPU 分位带与分位数恒等式（v1.39.1/v1.39.2） ---------- */
  // 数据层不变量：p50 ≤ p95 ≤ max。v1.39.0 曾在生产报出 p95 > max，
  // 那是任何人都能一眼判断为错的值，必须长期守住。
  const bandRes = await sess.eval(`(async()=>{
    const m = (location.hash.match(/#\\/nodes\\/([^?/]+)/) || [])[1];
    const nid = m || (window.__lastNodeId || '');
    if (!nid) return JSON.stringify({ skip: true });
    const d = await fetch('/api/nodes/' + nid + '/series?range=30d').then(r=>r.json());
    const pts = d.points || [];
    const withP95 = pts.filter(p => p.cpuP95 > 0);
    const bad = withP95.filter(p => !(p.cpuP50 <= p.cpuP95 + 1e-9 && p.cpuP95 <= p.cpuMax + 1e-9));
    return JSON.stringify({ skip: false, n: pts.length, withP95: withP95.length, bad: bad.length,
      sample: withP95.slice(0, 1).map(p => [p.cpuP50, p.cpuP95, p.cpuMax]) });
  })()`, 20000);
  if (bandRes && !bandRes.skip) {
    await sess.ok('分位数恒等式 p50≤p95≤max', bandRes.bad === 0,
      `${bandRes.withP95}/${bandRes.n} 个桶带分位，违规 ${bandRes.bad} 个`);
    if (bandRes.withP95 > 0) {
      // 有分位数据时，UI 必须把带画出来（否则用户只能看到统计行的数字）
      await sess.eval(`(()=>{const b=document.querySelector('[data-range="30d"]'); if (b) b.click();})()`);
      // 30 天要取 2000+ 点，再加一次渲染；固定 sleep 会误判成"带没画出来"。
      // 等到图例出现（那是 band 渲染完成的唯一信号）再读结果。
      await sess.waitStableView({ selector: '.chart-legend', timeoutMs: 15000 });
      const band = await sess.eval(`JSON.stringify((()=>{
        const vis = (el) => !!el && el.offsetParent !== null;
        const cards = Array.from(document.querySelectorAll('.chart-card')).filter(c => (c.querySelector('.chart-title')||{}).textContent?.includes('CPU'));
        const card = cards.find(vis) || cards[0];
        const bands = card ? Array.from(card.querySelectorAll('svg path')).filter(p => p.getAttribute('fill-opacity') === '0.13') : [];
        return { cards: cards.length, legend: card ? !!card.querySelector('.chart-legend') : false, bands: bands.length,
                 on: (document.querySelector('.range-tabs button.on')||{}).textContent };
      })())`);
      await sess.ok('30 天区间出现 p50–p95 分位带', band.legend && band.bands > 0,
        `图例=${band.legend} 带区段=${band.bands} 区间=${band.on}`);
    }
  }

  /* ---------- 4. 公开页（无需登录） ---------- */
  await sess.goto('/status', 2000);
  await sess.waitStableView({ selector: '.ep-progress, main' });
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
