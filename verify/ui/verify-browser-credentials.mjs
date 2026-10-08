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
  name: `browser-form-${run}`, base_url: `https://${run}.example.invalid`, site_family: 'newapi', auto_detect: false,
});
const account = await api('/admin/accounts', 'POST', { channel_id: channel.id, external_user_id: '42' });
const browser = await puppeteer.launch({
  executablePath: process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  headless: true,
});
try {
  const page = await browser.newPage();
  await page.evaluateOnNewDocument(token => localStorage.setItem('adminToken', token), TOKEN);
  for (const width of [1280, 375]) {
    await page.setViewport({ width, height: 900 });
    await page.goto(`${BASE}/admin/ui/accounts`, { waitUntil: 'networkidle0' });
    const edit = `[data-account-cred-edit="${account.id}"]`;
    await page.waitForSelector(edit);
    await page.click(edit);
    await page.waitForSelector('#browser-username', { timeout: 3000 });
    await page.click('#browser-enabled');
    await page.type('#browser-username', 'ui-test-user');
    await page.type('#browser-password', 'not-an-upstream-password');
    const saved = page.waitForResponse(r => r.url().endsWith('/browser-credentials') && r.request().method() === 'PUT');
    await page.click('#btn-browser-save');
    assert.equal((await saved).status(), 200, '保存失败');
    await page.waitForFunction(() => document.querySelector('#browser-password')?.value === '');
    const listed = await api(`/admin/accounts?channel_id=${channel.id}`);
    assert.equal(listed.items[0].browser_state, 'unverified');
    assert(!JSON.stringify(listed).includes('not-an-upstream-password'), '列表泄露密码');
    const layout = await page.evaluate(() => {
      const input = document.querySelector('#browser-password');
      const box = input.getBoundingClientRect();
      return { left: box.left, right: box.right, font: parseFloat(getComputedStyle(input).fontSize) };
    });
    assert(layout.left >= 0 && layout.right <= width && (width > 640 || layout.font >= 16),
      `输入框不适合当前宽度: ${JSON.stringify(layout)}`);
    await page.type('#browser-password', 'unsaved-test-input');
    await page.click('.drawer-x');
    await page.click(edit);
    assert.equal(await page.$eval('#browser-password', el => el.value), '', '关闭后残留密码');
    assert.equal(await page.$eval('#browser-username', el => el.value), 'ui-test-user', '已存用户名未加载');
    const cleared = page.waitForResponse(r => r.url().endsWith('/browser-credentials') && r.request().method() === 'DELETE');
    await page.click('#btn-browser-clear');
    assert.equal((await cleared).status(), 200, '清除失败');
    await page.waitForFunction(() => document.querySelector('#browser-username')?.value === '');
    assert.equal((await api(`/admin/accounts?channel_id=${channel.id}`)).items[0].browser_configured, false);
    console.log(`PASS ${width}px: 保存、未验证状态、秘密清空、重新打开、清除`);
  }
} finally {
  await browser.close();
}
