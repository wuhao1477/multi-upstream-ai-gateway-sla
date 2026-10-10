import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';

const ERROR_INPUT = '<img src=x onerror="window.__securityExecuted=1">';
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
  "img-src 'self' data:; font-src 'self' data:; connect-src 'self'; object-src 'none'; " +
  "base-uri 'none'; frame-ancestors 'none'; form-action 'self'";

function testSQL(sql) {
  assert(process.env.SLA_TEST_DSN, 'SPA 安全验收必须显式使用隔离测试库');
  assert(['127.0.0.1', 'localhost'].includes(new URL(process.env.SLA_TEST_DSN).hostname));
  const args = ['-XAtq', '-v', 'ON_ERROR_STOP=1', '-v', `security_error=${ERROR_INPUT}`];
  if (process.env.SLA_TEST_PG_CONTAINER) {
    return execFileSync('docker', ['exec', '-i', process.env.SLA_TEST_PG_CONTAINER,
      'psql', '-U', 'postgres', '-d', 'sla', ...args], { input: sql, encoding: 'utf8' }).trim();
  }
  assert(process.env.SLA_TEST_PSQL, '本地 PG 路径需要 SLA_TEST_PSQL');
  return execFileSync(process.env.SLA_TEST_PSQL, [process.env.SLA_TEST_DSN, ...args],
    { input: sql, encoding: 'utf8' }).trim();
}

async function checkFirstPaint(page, base, token, scenario, check) {
  const { width, theme, dark } = scenario;
  await page.setViewport({ width, height: 900 });
  await page.emulateMediaFeatures([{ name: 'prefers-color-scheme', value: 'dark' }]);
  await page.evaluateOnNewDocument(preference => {
    localStorage.removeItem('adminToken');
    localStorage.setItem('theme', preference);
  }, theme);
  let entry;
  let holdEntry = true;
  let themeLoaded = false;
  await page.setRequestInterception(true);
  page.on('request', request => {
    // 只暂缓真实入口模块，不替换任何响应；观察主题是否在应用执行前就生效。
    if (holdEntry && /\/assets\/index-[^/]+\.js$/.test(new URL(request.url()).pathname)) {
      entry = request;
      return;
    }
    void request.continue();
  });
  page.on('response', response => {
    if (new URL(response.url()).pathname === '/admin/ui/theme-init.js') {
      themeLoaded = response.status() === 200
        && /javascript/.test(response.headers()['content-type'] ?? '')
        && response.headers()['x-content-type-options'] === 'nosniff';
    }
  });
  const navigation = page.goto(`${base}/admin/ui/import`, { waitUntil: 'domcontentloaded' });
  try {
    await page.waitForFunction(() => document.querySelector('#app') !== null);
    const first = await page.evaluate(() => ({
      dark: document.documentElement.classList.contains('dark'),
      appEmpty: document.querySelector('#app').childElementCount === 0,
    }));
    check(`${width}px ${theme}：同源主题脚本在应用执行前生效`,
      themeLoaded && first.dark === dark && first.appEmpty, JSON.stringify(first));
  } finally {
    holdEntry = false;
    if (entry) await entry.continue();
  }
  const response = await navigation;
  check(`${width}px ${theme}：history 入口执行 CSP 与 nosniff`,
    response.headers()['content-security-policy'] === CSP
      && response.headers()['x-content-type-options'] === 'nosniff');
  await page.waitForSelector('#pane-login');
  await page.type('#token', token);
  await page.click('#login-submit');
  await page.waitForSelector('#pane-import');
  check(`${width}px ${theme}：登录后返回目标分栏`, new URL(page.url()).pathname === '/admin/ui/import');
  // 后续刷新不得清掉刚通过真实登录取得的令牌。
  await page.evaluateOnNewDocument(auth => localStorage.setItem('adminToken', auth), token);
  await page.reload({ waitUntil: 'networkidle0' });
  check(`${width}px ${theme}：history 刷新与主题保持`,
    await page.evaluate(expected => document.querySelector('#pane-import') !== null
      && document.documentElement.classList.contains('dark') === expected, dark));
}

async function checkErrorText(page, id, width, check) {
  await page.waitForSelector(`[data-hubsync-run="${id}"]`);
  check(`${width}px：最近同步错误以文字显示`,
    await page.$eval('#hubsync-card .note .bad', (el, input) =>
      el.textContent.includes(input) && el.querySelector('img') === null, ERROR_INPUT));
  await page.click(`[data-hubsync-run="${id}"]`);
  await page.waitForSelector('#hubsync-detail-error');
  const safe = await page.$eval('#hubsync-detail-error', (el, input) => ({
    text: el.textContent.includes(input),
    noElement: el.querySelector('img') === null,
    noExecution: window.__securityExecuted === undefined,
  }), ERROR_INPUT);
  check(`${width}px：同步错误详情不解释为 HTML、不执行脚本`,
    safe.text && safe.noElement && safe.noExecution, JSON.stringify(safe));
}

// WebDAV 地址整体加密存储（#34），界面只拿到去掉认证信息的展示值。
const WEBDAV_SECRET = 'test-webdav-url-marker';
const WEBDAV_URL = `https://dav.example.invalid/dav/?token=${WEBDAV_SECRET}`;

async function hubSyncAPI(base, token, method, body) {
  const res = await fetch(`${base}/admin/hub-sync`, {
    method,
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: body && JSON.stringify(body),
  });
  const text = await res.text();
  assert(res.ok, `${method} /admin/hub-sync → ${res.status}: ${text}`);
  return JSON.parse(text);
}

async function checkWebDAVRedaction(page, width, check) {
  await page.waitForSelector('#hs-url-redacted');
  const view = await page.evaluate(secret => {
    const note = document.querySelector('#hs-url-redacted').getBoundingClientRect();
    const width = document.documentElement.clientWidth;
    return {
      value: document.querySelector('#hs-url').value,
      hidden: !document.documentElement.outerHTML.includes(secret),
      // 只量本次新增的提示：它必须完整落在视口内（375px 不能把页面顶宽）。
      fits: note.left >= 0 && note.right <= width && note.width > 0,
      wider: [...document.querySelectorAll('body *')]
        .filter(el => el.getBoundingClientRect().right > width + 1)
        .slice(0, 3).map(el => el.id || el.className || el.tagName),
    };
  }, WEBDAV_SECRET);
  check(`${width}px：WebDAV 地址只显示去掉认证参数的展示值并提示已隐藏`,
    view.value === 'https://dav.example.invalid/dav/' && view.hidden && view.fits, JSON.stringify(view));
}

export async function verifySPASecurity(browser, { base, token, check }) {
  assert(['127.0.0.1', 'localhost'].includes(new URL(base).hostname), '仅允许本地验收实例');
  // 危险字符串是被测输入，不是假上游协议。真 PG 写入后经真实历史 API 与 Vue 渲染；
  // 不伪造成功站点或备份，结束后只删除本用例插入的这一条记录。
  const id = testSQL(`INSERT INTO hub_sync_runs
    (started_at, finished_at, trigger, applied, error)
    VALUES (now(), now(), 'manual', false, :'security_error') RETURNING id;`);
  assert(/^\d+$/.test(id), '测试记录必须返回唯一数字 ID');
  const hubSync = await hubSyncAPI(base, token, 'GET');
  assert(!hubSync.webdav_url_redacted, '隔离测试库不应已有带认证信息的 WebDAV 地址');
  const saved = await hubSyncAPI(base, token, 'PUT', {
    webdav_url: WEBDAV_URL, webdav_username: hubSync.webdav_username, enabled: false,
    interval_minutes: hubSync.interval_minutes, apply_mode: hubSync.apply_mode,
  });
  check('WebDAV 地址的管理接口只回展示值', saved.webdav_url_redacted === true &&
    !JSON.stringify(saved).includes(WEBDAV_SECRET), JSON.stringify(saved));
  try {
    for (const scenario of [
      { width: 1280, theme: 'dark', dark: true },
      { width: 375, theme: 'light', dark: false },
      { width: 375, theme: 'system', dark: true },
    ]) {
      const context = await browser.createBrowserContext();
      try {
        const page = await context.newPage();
        const violations = [];
        const errors = [];
        page.on('pageerror', e => errors.push(e.message));
        page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
        await page.exposeFunction('reportCSPViolation', directive => violations.push(directive));
        await page.evaluateOnNewDocument(() => document.addEventListener('securitypolicyviolation', e => {
          void window.reportCSPViolation(e.violatedDirective);
        }));
        await checkFirstPaint(page, base, token, scenario, check);
        await checkErrorText(page, id, scenario.width, check);
        await checkWebDAVRedaction(page, scenario.width, check);
        check(`${scenario.width}px ${scenario.theme}：无 CSP 违例或页面错误`,
          violations.length === 0 && errors.length === 0,
          [...violations, ...errors].slice(0, 2).join(' | '));
      } finally {
        await context.close();
      }
    }
  } finally {
    testSQL(`DELETE FROM hub_sync_runs WHERE id = ${id};`);
    await hubSyncAPI(base, token, 'PUT', {
      webdav_url: hubSync.webdav_url, webdav_username: hubSync.webdav_username, enabled: hubSync.enabled,
      interval_minutes: hubSync.interval_minutes, apply_mode: hubSync.apply_mode,
    });
  }
}
