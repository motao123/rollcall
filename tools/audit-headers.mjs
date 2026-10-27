#!/usr/bin/env node
/**
 * audit-headers.mjs — 线上安全头与 TLS 证书回归
 *
 * 为什么需要：CSP/HSTS/证书这些都在代码或反代里配对了，但没有线上回归。
 * 每次反代变迁（如 2026-10-26 从内置 TLS 切到 Caddy+ACME）、证书续期、
 * 或有人调整 Caddyfile，都可能悄悄丢掉某个头，而页面看起来一切正常。
 *
 * 特性：零依赖（node:fetch + node:tls），退出码可挂 CI/cron。
 *
 * 用法：
 *   node tools/audit-headers.mjs                       # 默认生产地址
 *   node tools/audit-headers.mjs https://example.com   # 指定目标
 *   RCP_MIN_CERT_DAYS=21 node tools/audit-headers.mjs  # 调整证书剩余天数阈值
 */
import tls from 'node:tls';

const target = (process.argv[2] || 'https://status.endgo.top').replace(/\/$/, '');
const MIN_CERT_DAYS = Number(process.env.RCP_MIN_CERT_DAYS || 14);
const u = new URL(target);

const results = [];
const pass = (name, detail = '') => { results.push({ ok: true, name, detail }); console.log(`✓ ${name}${detail ? ' — ' + detail : ''}`); };
const fail = (name, detail = '') => { results.push({ ok: false, name, detail }); console.log(`✗ ${name}${detail ? ' — ' + detail : ''}`); };
const warn = (name, detail = '') => { results.push({ ok: true, warn: true, name, detail }); console.log(`! ${name}${detail ? ' — ' + detail : ''}`); };

/* ---------------- HTTP 安全头 ---------------- */

async function checkHeaders() {
  const res = await fetch(target + '/status', { redirect: 'manual' });
  const h = res.headers;
  const get = (k) => (h.get(k) || '').trim();

  console.log(`\n目标 ${target}/status → HTTP ${res.status}\n`);

  // CSP：脚本必须只来自本站，且脚本部分不得含 unsafe-inline / unsafe-eval
  const csp = get('content-security-policy');
  if (!csp) {
    fail('CSP 存在', '缺失 Content-Security-Policy');
  } else {
    pass('CSP 存在', csp.slice(0, 90) + (csp.length > 90 ? '…' : ''));
    const scriptSrc = (csp.match(/script-src([^;]*)/i) || [])[1] || '';
    scriptSrc.includes("'self'")
      ? pass('CSP script-src 含 self')
      : fail('CSP script-src 含 self', 'script-src =' + scriptSrc.trim());
    /unsafe-inline|unsafe-eval/i.test(scriptSrc)
      ? fail('CSP 脚本未放行 inline/eval', 'script-src =' + scriptSrc.trim())
      : pass('CSP 脚本未放行 inline/eval');
    /frame-ancestors\s+'none'/i.test(csp)
      ? pass("CSP frame-ancestors 'none'")
      : warn("CSP frame-ancestors 'none'", '未设置，点击劫持防护依赖 X-Frame-Options');
  }

  // HSTS：max-age 至少一年，否则浏览器仍会明文重试
  const hsts = get('strict-transport-security');
  if (!hsts) {
    fail('HSTS 存在', '缺失 Strict-Transport-Security（HTTP 可达时可能被降级）');
  } else {
    const maxAge = Number((hsts.match(/max-age=(\d+)/i) || [])[1] || 0);
    maxAge >= 31536000
      ? pass('HSTS max-age ≥ 1 年', hsts)
      : fail('HSTS max-age ≥ 1 年', hsts + '（建议 31536000）');
  }

  get('x-content-type-options').toLowerCase() === 'nosniff'
    ? pass('X-Content-Type-Options: nosniff')
    : fail('X-Content-Type-Options: nosniff', get('x-content-type-options') || '缺失');

  get('referrer-policy')
    ? pass('Referrer-Policy 存在', get('referrer-policy'))
    : warn('Referrer-Policy 存在', '缺失（默认可能泄露完整 URL）');

  (get('x-frame-options') || /frame-ancestors/i.test(csp))
    ? pass('点击劫持防护', get('x-frame-options') || 'CSP frame-ancestors')
    : fail('点击劫持防护', '既无 X-Frame-Options 也无 frame-ancestors');

  // Server 头不必存在，但若存在且带版本号属于信息泄露
  const server = get('server');
  if (server && /\d+\.\d+/.test(server)) warn('Server 头未泄露版本', server);
  else if (server) pass('Server 头未泄露版本', server);

  // 明文访问应被重定向（而不是返回内容）
  try {
    const plain = new URL(target);
    plain.protocol = 'http:';
    const r2 = await fetch(plain.toString(), { redirect: 'manual' });
    const loc = r2.headers.get('location') || '';
    [301, 302, 308].includes(r2.status) && loc.startsWith('https://')
      ? pass('HTTP 明文跳转 HTTPS', `${r2.status} → ${loc}`)
      : (r2.status === 200
        ? fail('HTTP 明文跳转 HTTPS', '明文返回 200，内容可被中间人读取/篡改')
        : warn('HTTP 明文跳转 HTTPS', `HTTP ${r2.status}`));
  } catch (e) {
    warn('HTTP 明文探测', '无法完成：' + e.message);
  }
}

/* ---------------- TLS 证书 ---------------- */

function checkCert() {
  return new Promise((resolve) => {
    const host = u.hostname;
    const port = Number(u.port || 443);
    const sock = tls.connect({ host, port, servername: host, rejectUnauthorized: false }, () => {
      const cert = sock.getPeerCertificate();
      sock.end();
      if (!cert || !cert.valid_to) {
        fail('TLS 证书可读取', '未取到证书');
        return resolve();
      }
      const validTo = new Date(cert.valid_to);
      const days = Math.floor((validTo.getTime() - Date.now()) / 86400000);
      const issuer = (cert.issuer?.O || cert.issuer?.CN || '未知').toString();
      const cn = (cert.subject?.CN || '').toString();

      days >= MIN_CERT_DAYS
        ? pass('证书剩余天数', `${days} 天（≥${MIN_CERT_DAYS}）· ${cn} · 签发者 ${issuer}`)
        : fail('证书剩余天数', `${days} 天 < ${MIN_CERT_DAYS} 天 · 到期 ${validTo.toISOString()}`);

      // 证书链是否可信：用默认 CA 再连一次
      const verify = tls.connect({ host, port, servername: host }, () => {
        verify.end();
        pass('证书链可信（系统 CA）', '握手成功');
        resolve();
      });
      verify.on('error', (e) => {
        fail('证书链可信（系统 CA）', e.message);
        resolve();
      });
    });
    sock.on('error', (e) => {
      fail('TLS 握手', e.message);
      resolve();
    });
  });
}

try {
  await checkHeaders();
  console.log('');
  await checkCert();
} catch (e) {
  fail('审计执行', e.message);
}

const failed = results.filter((r) => !r.ok);
const warned = results.filter((r) => r.warn);
console.log(`\n汇总：${results.length - failed.length}/${results.length} 通过` +
  (warned.length ? `，${warned.length} 项提示` : '') +
  (failed.length ? `，${failed.length} 项失败` : ''));
for (const f of failed) console.log(`  ✗ ${f.name}${f.detail ? ' — ' + f.detail : ''}`);
process.exit(failed.length ? 1 : 0);
