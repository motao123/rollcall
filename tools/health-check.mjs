#!/usr/bin/env node
// 线上健康巡检：不依赖凭据，只读公开接口。
//
// 与 tools/audit-headers.mjs 的分工：
//   audit-headers  —— 安全基线（CSP/HSTS/证书），回答"配置还对不对"
//   health-check   —— 运行状态（数据是否在动），回答"机器还活着吗、数据还新吗"
//
// 为什么需要它：面板能打开 ≠ 系统正常。以下三种故障在浏览器里看起来完全正常：
//   1) 服务端时钟漂移 → 上报签名（±90s 窗口）全部 401，节点静默掉线，页面只是变空
//   2) 全部 Agent 停止上报 → 概览还显示最后一次的数据，看起来"在线"
//   3) 历史层写坏 → 首页曲线还在（来自内存 raw），但 7d/30d 已经没数
// 这几种都只能靠"数据的新鲜度"发现，不能靠 HTTP 状态码。
//
// 用法：
//   node tools/health-check.mjs                      # 默认检查 https://status.endgo.top
//   node tools/health-check.mjs http://127.0.0.1:19080
//   node tools/health-check.mjs --json               # 机器可读输出（便于接监控）
//
// 退出码：0 全部通过 / 1 存在失败项 / 2 参数或连接错误。

const DEFAULT_BASE = 'https://status.endgo.top';
// 上报间隔默认 30s。判定"数据陈旧"用 6 倍间隔（180s），避开秒级抖动与一次丢包；
// 判定"全局停摆"更严格：所有节点都超过 3 倍间隔就说明服务端侧没在收。
const REPORT_INTERVAL_SEC = 30;
const STALE_SEC = REPORT_INTERVAL_SEC * 6;
const STALLED_SEC = REPORT_INTERVAL_SEC * 3;
// 服务端时钟与本地偏差超过这个值就危险了：上报签名窗口是 ±90s，
// 超过 120s 的对时误差会让所有上报陆续被拒。
const CLOCK_SKEW_FAIL_SEC = 120;
const CLOCK_SKEW_WARN_SEC = 45;

const args = process.argv.slice(2);
const jsonOut = args.includes('--json');
const baseArg = args.find((a) => !a.startsWith('--'));
const base = (baseArg || DEFAULT_BASE).replace(/\/+$/, '');

const results = [];
function record(level, name, detail) {
  results.push({ level, name, detail });
  if (!jsonOut) {
    const mark = level === 'fail' ? '✗' : level === 'warn' ? '!' : '✓';
    const line = `  ${mark} ${name}${detail ? ' — ' + detail : ''}`;
    console.log(line);
  }
}
const pass = (n, d) => record('pass', n, d);
const warn = (n, d) => record('warn', n, d);
const fail = (n, d) => record('fail', n, d);

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

/** 带超时的 fetch，返回 { status, text, ms, headers } */
async function get(path, { timeoutMs = 15000, wantText = false } = {}) {
  const url = base + path;
  const t0 = Date.now();
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), timeoutMs);
  try {
    const res = await fetch(url, { signal: ctrl.signal, redirect: 'manual' });
    const body = wantText ? await res.text() : await res.arrayBuffer();
    return { status: res.status, body, ms: Date.now() - t0, headers: res.headers };
  } finally {
    clearTimeout(timer);
  }
}

(async () => {
  if (!jsonOut) {
    console.log(`\n健康巡检 ${base}  ${new Date().toISOString()}\n`);
    console.log('[1] 端点可达性');
  }

  // ---- 1. 端点可达性 ----
  let readyz = null;
  try {
    readyz = await get('/readyz', { timeoutMs: 8000 });
    if (readyz.status === 200) pass('GET /readyz', `200 · ${readyz.ms}ms`);
    else fail('GET /readyz', `HTTP ${readyz.status}（服务未就绪）`);
  } catch (err) {
    fail('GET /readyz', `连接失败: ${err.message}`);
  }

  let page = null;
  try {
    page = await get('/status', { timeoutMs: 15000, wantText: true });
    if (page.status === 200) pass('GET /status', `200 · ${(page.body.length / 1024).toFixed(1)}KB · ${page.ms}ms`);
    else fail('GET /status', `HTTP ${page.status}`);
  } catch (err) {
    fail('GET /status', `连接失败: ${err.message}`);
  }

  // 端点全挂就没必要继续了，直接汇总
  if (results.every((r) => r.level === 'fail')) {
    return finish();
  }

  // ---- 2. 状态数据（这是本工具的核心） ----
  if (!jsonOut) console.log('\n[2] 数据新鲜度');
  let st = null;
  try {
    const r = await get('/api/status', { timeoutMs: 20000, wantText: true });
    if (r.status !== 200) {
      fail('GET /api/status', `HTTP ${r.status}`);
    } else {
      try {
        st = JSON.parse(r.body);
        pass('GET /api/status', `200 · ${(r.body.length / 1024).toFixed(1)}KB · ${r.ms}ms`);
      } catch (err) {
        // 公开页关闭时返回的是 404 页面而不是 JSON，这里要区分开
        fail('GET /api/status', `返回非 JSON（公开页可能已关闭）: ${err.message}`);
      }
    }
  } catch (err) {
    fail('GET /api/status', `连接失败: ${err.message}`);
  }

  if (st) {
    const now = Math.floor(Date.now() / 1000);
    const total = Number(st.total ?? (st.nodes || []).length);
    const online = Number(st.online ?? 0);
    const stale = Number(st.stale ?? 0);
    const offline = Number(st.offline ?? 0);
    const noData = Number(st.noData ?? 0);

    // 2.1 服务端时钟：签名窗口 ±90s，漂移会让上报整批 401
    const gen = Number(st.generatedAt || 0);
    if (gen > 0) {
      const skew = Math.abs(now - gen);
      if (skew > CLOCK_SKEW_FAIL_SEC) {
        fail('服务端时钟', `与本地差 ${skew}s（>${CLOCK_SKEW_FAIL_SEC}s，上报签名 ±90s 窗口会被拒）`);
      } else if (skew > CLOCK_SKEW_WARN_SEC) {
        warn('服务端时钟', `与本地差 ${skew}s（建议核对 NTP）`);
      } else {
        pass('服务端时钟', `与本地差 ${skew}s`);
      }
    } else {
      warn('服务端时钟', '响应缺少 generatedAt，无法判断');
    }

    // 2.2 节点规模
    if (total === 0) fail('节点数量', '状态里没有任何节点');
    else pass('节点数量', `共 ${total} 台`);

    // 2.3 在线构成：离线是运维事实（不算故障），noData/stale 才是可疑信号
    if (total > 0) {
      const parts = [`在线 ${online}`, `离线 ${offline}`, `陈旧 ${stale}`, `无数据 ${noData}`];
      const line = parts.join(' · ');
      if (offline >= total) fail('在线构成', `${line}（全部离线，优先查服务端与网络）`);
      else if (offline > 0) warn('在线构成', `${line}（有节点离线，属事实，非故障）`);
      else if (stale > 0 || noData > 0) warn('在线构成', `${line}（陈旧/无数据需关注）`);
      else pass('在线构成', line);
    }

    // 2.4 上报新鲜度：单台超时是节点问题，全台超时是服务端问题
    const nodes = Array.isArray(st.nodes) ? st.nodes : [];
    const ages = nodes
      .map((n) => ({ name: n.name || n.hid || '?', age: now - Number(n.lastSeen || 0) }))
      .filter((x) => Number.isFinite(x.age) && x.age >= 0);
    if (ages.length) {
      const maxAge = Math.max(...ages.map((x) => x.age));
      const minAge = Math.min(...ages.map((x) => x.age));
      const worst = ages.filter((x) => x.age === maxAge)[0];
      if (minAge > STALLED_SEC) {
        fail('上报新鲜度', `最新一条也已经 ${minAge}s 前（全机群停摆，查服务端接收）`);
      } else if (maxAge > STALE_SEC) {
        warn('上报新鲜度', `最旧 ${worst.name} ${maxAge}s 前（>${STALE_SEC}s）`);
      } else {
        pass('上报新鲜度', `最新 ${minAge}s / 最旧 ${maxAge}s 前`);
      }
      // 单台超时点名，便于直接定位
      const staleNodes = ages.filter((x) => x.age > STALE_SEC);
      if (staleNodes.length && staleNodes.length < ages.length) {
        warn('陈旧节点', staleNodes.map((x) => `${x.name}(${x.age}s)`).join(', '));
      }
    } else {
      warn('上报新鲜度', '节点列表里没有 lastSeen 字段');
    }

    // 2.5 业务水位：可用率异常与积压告警
    const s = st.summary || {};
    if (typeof s.avgUptime24 === 'number') {
      if (s.avgUptime24 < 99) warn('24h 平均可用率', `${s.avgUptime24}%`);
      else pass('24h 平均可用率', `${s.avgUptime24}%`);
    }
    if (typeof s.activeAlert === 'number') {
      if (s.activeAlert > 0) warn('活跃告警', `${s.activeAlert} 条`);
      else pass('活跃告警', '0');
    }
    if (typeof s.svcTotal === 'number' && s.svcTotal > 0) {
      if (s.svcDown > 0) warn('服务探测', `${s.svcDown}/${s.svcTotal} 不可用`);
      else pass('服务探测', `${s.svcTotal} 个全部正常`);
    }
    if (typeof s.avgDisk === 'number' && s.avgDisk > 85) {
      warn('平均磁盘使用率', `${s.avgDisk}%`);
    }

    // 2.6 历史层：首页曲线来自内存 raw，可能掩盖历史层写坏
    if (typeof st.historyDays === 'number') {
      if (st.historyDays <= 0) fail('历史天数', `${st.historyDays} 天（历史层疑似为空）`);
      else pass('历史天数', `${st.historyDays} 天`);
    }
  }

  finish();
})().catch((err) => {
  if (jsonOut) console.log(JSON.stringify({ ok: false, error: err.message }, null, 1));
  else console.error('\n巡检异常终止:', err.message);
  process.exit(2);
});

function finish() {
  const fails = results.filter((r) => r.level === 'fail').length;
  const warns = results.filter((r) => r.level === 'warn').length;
  const passes = results.filter((r) => r.level === 'pass').length;

  if (jsonOut) {
    console.log(JSON.stringify({ base, ok: fails === 0, pass: passes, warn: warns, fail: fails, results }, null, 1));
  } else {
    console.log(`\n结果：通过 ${passes} · 警告 ${warns} · 失败 ${fails}`);
    if (fails) console.log('判定：存在失败项，需要处理');
    else if (warns) console.log('判定：可用，但有需关注的项');
    else console.log('判定：全部正常');
    console.log('');
  }
  process.exit(fails ? 1 : 0);
}
