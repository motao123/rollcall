#!/usr/bin/env node
/**
 * ui-session.mjs — 可复用的管理面板 UI 校验会话（原生 CDP，零依赖）
 *
 * 解决四个问题：
 *   1. 每次校验都重新登录 → cookie 持久化到 .auth/cookies.json，登录态跨脚本复用
 *   2. 面板密码散落在脚本里 → 只从环境变量 RCP_PASSWORD 或 .auth/password 读取
 *   3. 失败看不到现场 → 断言失败自动截图到 .auth/shots/
 *   4. 每个脚本各写一遍 CDP 样板 → 统一 openSession()/goto()/eval()/shot()/ok()
 *
 * 用法：
 *   RCP_PASSWORD=... node tools/ui-check.mjs           # 首次登录并缓存
 *   node tools/ui-check.mjs                            # 之后走 cookie 复用
 *
 * 注意：.auth/ 已在 .gitignore 中排除（含会话 cookie 与密码）。
 */
import { execFile } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
export const AUTH_DIR = path.join(HERE, '..', '.auth');
const COOKIE_FILE = path.join(AUTH_DIR, 'cookies.json');
const PWD_FILE = path.join(AUTH_DIR, 'password');
const SHOT_DIR = path.join(AUTH_DIR, 'shots');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function chromePath() {
  if (process.env.RCP_CHROME) return process.env.RCP_CHROME;
  const candidates = [
    process.platform === 'win32'
      ? path.join(os.homedir(), 'AppData/Local/ms-playwright/chromium-1228/chrome-win64/chrome.exe')
      : '',
    process.platform === 'win32' ? 'C:/Program Files/Google/Chrome/Application/chrome.exe' : '',
    process.platform === 'darwin' ? '/Applications/Chromium.app/Contents/MacOS/Chromium' : '',
    '/usr/bin/chromium',
    '/usr/bin/chromium-browser',
    '/usr/bin/google-chrome',
  ].filter(Boolean);
  for (const c of candidates) if (fs.existsSync(c)) return c;
  throw new Error('找不到 Chromium，请用 RCP_CHROME 指定可执行文件路径');
}

function readPassword() {
  if (process.env.RCP_PASSWORD) return process.env.RCP_PASSWORD.trim();
  if (fs.existsSync(PWD_FILE)) return fs.readFileSync(PWD_FILE, 'utf8').trim();
  return '';
}

/**
 * 打开一个会话。
 * @param {{base?:string, width?:number, height?:number, port?:number, admin?:boolean}} opts
 *   admin=true 时会确保处于已登录态（必要时用 RCP_PASSWORD 登录）。
 */
export async function openSession(opts = {}) {
  const base = (opts.base || 'https://status.endgo.top').replace(/\/$/, '');
  const width = opts.width || 1366;
  const height = opts.height || 900;
  const port = opts.port || 9333 + Math.floor(Math.random() * 200);
  const wantAdmin = opts.admin !== false;

  fs.mkdirSync(SHOT_DIR, { recursive: true });

  const chrome = execFile(chromePath(), [
    '--headless=new', '--no-sandbox', '--no-proxy-server', '--no-first-run',
    '--enable-unsafe-swiftshader', '--use-gl=angle', '--use-angle=swiftshader',
    `--remote-debugging-port=${port}`, `--window-size=${width},${height}`, 'about:blank',
  ]);

  let wsUrl = '';
  for (let i = 0; i < 30 && !wsUrl; i++) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/json/list`);
      const page = (await res.json()).find((t) => t.type === 'page');
      if (page) wsUrl = page.webSocketDebuggerUrl;
    } catch { /* 未就绪 */ }
    if (!wsUrl) await sleep(300);
  }
  if (!wsUrl) {
    chrome.kill();
    throw new Error('CDP 未就绪（端口 ' + port + '）');
  }

  const ws = new WebSocket(wsUrl);
  await new Promise((res, rej) => {
    ws.onopen = res;
    ws.onerror = () => rej(new Error('WebSocket 连接失败'));
  });

  let msgId = 0;
  const pending = new Map();
  ws.onmessage = (m) => {
    const d = JSON.parse(m.data);
    if (d.id && pending.has(d.id)) {
      pending.get(d.id)(d);
      pending.delete(d.id);
    }
  };
  const send = (method, params = {}) =>
    new Promise((res) => {
      const id = ++msgId;
      pending.set(id, res);
      ws.send(JSON.stringify({ id, method, params }));
    });

  await send('Page.enable');
  await send('Network.enable');

  const results = [];

  const sess = {
    base,
    port,

    /** 直接执行表达式，返回解析后的 JSON（表达式应产出 JSON 字符串或可序列化值） */
    async eval(expr) {
      const r = await send('Runtime.evaluate', {
        expression: expr,
        awaitPromise: true,
        returnByValue: true,
      });
      if (r.result?.exceptionDetails) {
        throw new Error('页面执行异常: ' + (r.result.exceptionDetails.text || '') +
          ' ' + (r.result.exceptionDetails.exception?.description || ''));
      }
      const v = r.result?.result?.value;
      if (typeof v === 'string') {
        try { return JSON.parse(v); } catch { return v; }
      }
      return v;
    },

    /** 打开路由（path 以 / 开头，可为 #/xxx 形式的 hash 路由）并等待渲染 */
    async goto(route, waitMs = 5000) {
      // 同文档的 hash 变化不会触发整页加载，先切空白页确保重新渲染
      await send('Page.navigate', { url: 'about:blank' });
      await sleep(200);
      await send('Page.navigate', { url: base + '/' + route.replace(/^\/+/, '') });
      await sleep(waitMs);
    },

    /**
     * 等到页面稳定：`#view` 已挂载、可选的选择器已出现，且连续两次采样的
     * DOM 规模一致。
     *
     * 为什么需要：面板走 SSE，机群状态变化会触发整页重渲染。固定 sleep 之后
     * 断言，可能正好落在「旧 DOM 已拆、新 DOM 未挂」的中间态 —— 表现为
     * 「按钮/分页器找不到」这类假失败（实测同一版本连跑两次，一次 19/19、
     * 一次 4 条失败，失败项还每次都不同）。
     *
     * 为什么还要 selector：详情页会先挂骨架再异步填数据，只看"DOM 规模稳定"
     * 会在骨架阶段就判稳定。传一个"数据出现后才有的选择器"（如 `.info-row`）
     * 才能真正确认渲染完成。
     *
     * 判据 = (选择器命中或未传) && 两次采样规模相同。
     */
    async waitStableView({ selector = '', timeoutMs = 20000, quietMs = 400 } = {}) {
      const deadline = Date.now() + timeoutMs;
      let prev = null;
      while (Date.now() < deadline) {
        const sig = await sess.eval(`JSON.stringify({
          ready: (document.querySelector('#view')?.children.length || 0) > 0 &&
                 ${selector ? `!!document.querySelector(${JSON.stringify(selector)})` : 'true'},
          shape: (document.querySelector('#view')?.children.length || 0) * 10000 +
                 document.querySelectorAll('*').length
        })`).catch(() => null);
        if (sig && sig.ready && prev === sig.shape) {
          await sleep(quietMs);
          return true;
        }
        prev = sig ? sig.shape : null;
        await sleep(150);
      }
      return false;
    },

    /** 截图；name 用于文件名，失败断言也复用它 */
    async shot(name, clip) {
      const params = { format: 'png' };
      if (clip) params.clip = { ...clip, scale: clip.scale || 1 };
      const r = await send('Page.captureScreenshot', params);
      const file = path.join(SHOT_DIR, `${name}-${Date.now()}.png`);
      fs.writeFileSync(file, Buffer.from(r.result.data, 'base64'));
      return file;
    },

    /** 断言：记录结果，失败自动截图（同一用例只截一次） */
    async ok(name, cond, detail = '') {
      const pass = !!cond;
      results.push({ name, pass, detail });
      if (!pass) {
        const f = await sess.shot('fail-' + name.replace(/[^\w\u4e00-\u9fa5-]+/g, '_')).catch(() => '');
        if (f) results[results.length - 1].shot = f;
      }
      console.log(`${pass ? '✓' : '✗'} ${name}${detail ? ' — ' + detail : ''}`);
      return pass;
    },

    /** 保存当前 cookie，供下次复用 */
    async saveCookies() {
      // ★ 必须用 Network.getCookies(urls)：Storage.getCookies 是 browser 级命令，
      //   经 page session 调用时实测返回空数组（cookie 缓存成了 []，复用永远失效）。
      const r = await send('Network.getCookies', { urls: [base + '/'] });
      const cookies = r.result?.cookies || [];
      fs.mkdirSync(AUTH_DIR, { recursive: true });
      fs.writeFileSync(COOKIE_FILE, JSON.stringify(cookies, null, 1));
      try { fs.chmodSync(COOKIE_FILE, 0o600); } catch { /* Windows 忽略 */ }
      return cookies.length;
    },

    /** 关闭：保存 cookie、打印汇总、设置退出码 */
    async close() {
      try { await sess.saveCookies(); } catch { /* 尽力而为 */ }
      try { ws.close(); } catch { /* ignore */ }
      chrome.kill();
      const failed = results.filter((r) => !r.pass);
      console.log(`\n汇总：${results.length - failed.length}/${results.length} 通过` +
        (failed.length ? `，失败 ${failed.length}` : ''));
      for (const f of failed) {
        console.log(`  ✗ ${f.name}${f.detail ? ' — ' + f.detail : ''}${f.shot ? '  截图: ' + f.shot : ''}`);
      }
      process.exitCode = failed.length ? 1 : 0;
      return failed.length === 0;
    },
  };

  /* ---------- 登录态准备 ---------- */
  if (wantAdmin) {
    // 1) 先注入已缓存的 cookie
    if (fs.existsSync(COOKIE_FILE)) {
      try {
        const cookies = JSON.parse(fs.readFileSync(COOKIE_FILE, 'utf8'));
        // 逐个 Network.setCookie：同属 page session，且用 url 让浏览器自己推导 domain，
        // 避免手写 domain 与 host-only cookie 的差别。
        for (const c of cookies) {
          await send('Network.setCookie', {
            name: c.name,
            value: c.value,
            url: base + '/',
            path: c.path || '/',
            secure: c.secure,
            httpOnly: c.httpOnly,
            ...(c.sameSite ? { sameSite: c.sameSite } : {}),
            ...(c.expires && c.expires > 0 ? { expires: c.expires } : {}),
          });
        }
        if (cookies.length) console.log(`· 注入 ${cookies.length} 条缓存 cookie`);
      } catch { /* cookie 损坏则重新登录 */ }
    }

    await send('Page.navigate', { url: base + '/' });
    await sleep(2500);

    // 2) 用管理接口探测登录态（未登录返回 401）
    const status = await sess.eval(
      `fetch('/api/admin/settings',{headers:{'X-Requested-With':'XMLHttpRequest'}}).then(r=>r.status)`,
    ).catch(() => 0);

    if (status !== 200) {
      const pwd = readPassword();
      if (!pwd) {
        chrome.kill();
        throw new Error(
          '未处于登录态，且没有可用密码。请设置环境变量 RCP_PASSWORD，' +
          `或把密码写入 ${PWD_FILE}（该目录已 gitignore）。`);
      }
      const code = await sess.eval(
        `fetch('/api/auth/login',{method:'POST',headers:{'Content-Type':'application/json'},` +
        `body:JSON.stringify({identifier:'admin',password:${JSON.stringify(pwd)}})}).then(r=>r.status)`,
      );
      if (code !== 200) {
        chrome.kill();
        throw new Error('登录失败，HTTP ' + code + '（密码错误或触发登录限速）');
      }
      console.log('· 已用密码登录（cookie 将缓存到 .auth/cookies.json）');
    } else {
      console.log('· 复用已缓存的登录态');
    }
  }

  return sess;
}

/** 便捷入口：跑完自动 close */
export async function withSession(fn, opts = {}) {
  const sess = await openSession(opts);
  try {
    return await fn(sess);
  } finally {
    await sess.close();
  }
}
