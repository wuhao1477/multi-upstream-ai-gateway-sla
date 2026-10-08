// 真 Chrome + 本项目管理 API + 独立真 PG；只验配置输入，不请求/模拟上游站点。
// 必须显式指定可写测试实例，禁止对业务部署运行。
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import puppeteer from 'puppeteer-core';

const BASE = process.env.BASE;
const TOKEN = process.env.ADMIN_TOKEN;
assert(BASE && TOKEN && process.env.SLA_TEST_DSN, '需要 BASE、ADMIN_TOKEN、SLA_TEST_DSN，且只能指向隔离测试实例');
assert(['127.0.0.1', 'localhost'].includes(new URL(BASE).hostname), '只允许本地测试实例');

async function api(path, method = 'GET', data) {
  const response = await fetch(`${BASE}${path}`, {
    method, headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
    body: data === undefined ? undefined : JSON.stringify(data),
  });
  assert(response.ok, `test API ${method} ${path}: ${response.status}`);
  return response.json();
}

const run = randomUUID();
const channel = await api('/admin/channels', 'POST', {
  name: `cookie-form-${run}`, base_url: `https://${run}.example.invalid`, site_family: 'newapi', auto_detect: false,
});
const account = await api('/admin/accounts', 'POST', { channel_id: channel.id, external_user_id: '42' });
const browser = await puppeteer.launch({
  executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  headless: true,
});
try {
  const page = await browser.newPage();
  await page.evaluateOnNewDocument(token => localStorage.setItem('adminToken', token), TOKEN);
  const missingCount = () => page.$$eval('#pane-accounts .stat', stats => Number(
    stats.find(el => el.querySelector('span')?.textContent === '缺采集凭证')?.querySelector('b')?.textContent,
  ));
  for (const width of process.argv.includes('--mobile-only') ? [375] : [1280, 375]) {
    await page.setViewport({ width, height: 900 });
    await page.goto(`${BASE}/admin/ui/accounts`, { waitUntil: 'networkidle0' });
    const edit = `[data-account-cred-edit="${account.id}"]`;
    await page.waitForSelector(edit);
    const missingBefore = await missingCount();
    await page.click(edit);
    await page.waitForSelector('#cookie-header', { timeout: 3000 });
    assert.equal(await page.$('#browser-username'), null, '仍提供上游用户名输入');
    assert.equal(await page.$('#browser-password'), null, '仍提供上游密码输入');
    await page.click('#cookie-enabled');
    await page.type('#cookie-header', 'session=not-an-upstream-cookie==; pref=light');
    const saved = page.waitForResponse(r => r.url().endsWith('/cookie-credentials') && r.request().method() === 'PUT');
    await page.click('#btn-cookie-save');
    assert.equal((await saved).status(), 200, '保存失败');
    await page.waitForFunction(() => document.querySelector('#cookie-header')?.value === '');
    const listed = await api(`/admin/accounts?channel_id=${channel.id}`);
    assert.equal(listed.items[0].cookie_state, 'unverified');
    assert(!JSON.stringify(listed).includes('not-an-upstream-cookie'), '列表泄露 Cookie');
    await page.waitForFunction(() => !document.querySelector('#btn-cookie-validate')?.disabled);
    assert.equal(await missingCount(), missingBefore - 1, 'Cookie 账号仍计入缺凭证汇总');
    assert.equal(await page.$eval(`[data-account-cred="${account.id}"]`, el => el.dataset.cred), 'has',
      'Cookie 账号仍显示未登记');
    const validated = page.waitForResponse(r => r.url().endsWith('/cookie-credentials/validate'));
    await page.click('#btn-cookie-validate');
    // 真实 .invalid 地址不能解析；只验按钮、管理路由与错误展示，不伪造认证成功。
    assert.equal((await validated).status(), 502, '未将真实网络失败报告为失败');
    await page.waitForFunction(() => !document.querySelector('#cookie-header')?.disabled
      && !document.querySelector('#cookie-header')?.closest('fieldset')?.disabled);
    assert.equal((await api(`/admin/accounts?channel_id=${channel.id}`)).items[0].cookie_state, 'unverified');
    const layout = await page.evaluate(() => {
      const input = document.querySelector('#cookie-header');
      const box = input.getBoundingClientRect();
      const button = document.querySelector('#btn-cookie-validate');
      const buttonBox = button.getBoundingClientRect();
      const hit = document.elementFromPoint(buttonBox.x + buttonBox.width / 2, buttonBox.y + buttonBox.height / 2);
      return { left: box.left, right: box.right, font: parseFloat(getComputedStyle(input).fontSize),
        buttonRight: buttonBox.right, buttonHeight: buttonBox.height, hit: hit === button || button.contains(hit) };
    });
    assert(layout.left >= 0 && layout.right <= width && (width > 640 || layout.font >= 16),
      `输入框不适合当前宽度: ${JSON.stringify(layout)}`);
    assert(layout.buttonRight <= width && layout.buttonHeight >= 34 && layout.hit, '验证按钮不适合当前宽度');
    await page.type('#cookie-header', 'session=unsaved-test-input');
    assert.equal(await page.$eval('#btn-cookie-validate', el => el.disabled), true, '不能验证未保存的 Cookie');
    await page.click('.drawer-x');
    // 手机上失败提示会暂时遮住账号行按钮，等待实际隐藏后再点击。
    await page.waitForSelector('#toast', { hidden: true });
    await page.select('#acc-f-cred', 'missing');
    await page.waitForSelector(edit, { hidden: true });
    await page.select('#acc-f-cred', 'has');
    await page.waitForSelector(edit);
    await page.select('#acc-f-cred', 'all');
    await page.click(edit);
    await page.waitForSelector('#cookie-header');
    assert.equal(await page.$eval('#cookie-header', el => el.value), '', '关闭后残留 Cookie');
    const cleared = page.waitForResponse(r => r.url().endsWith('/cookie-credentials') && r.request().method() === 'DELETE');
    await page.click('#btn-cookie-clear');
    assert.equal((await cleared).status(), 200, '清除失败');
    await page.waitForFunction(() => document.querySelector('#cookie-header')?.value === '');
    assert.equal((await api(`/admin/accounts?channel_id=${channel.id}`)).items[0].cookie_configured, false);
    console.log(`PASS ${width}px: 保存、凭证汇总/筛选、验证失败展示、未验证状态、秘密清空、重新打开、清除`);
  }
} finally {
  await browser.close();
}
