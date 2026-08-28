// P1 管理界面的真实浏览器验收。
//
// 用真 Chrome（非 headless shell 的 DOM dump）：点击、填表、等 XHR、截图，
// 验证的是"运维真能在 web 端加渠道商并采集"，而不是"HTML 里有那些字符串"。
import puppeteer from 'puppeteer-core';
import { writeFileSync } from 'node:fs';

// 路径由 test-ui.sh 按平台探测后注入（macOS 在 .app 里、CI 在 PATH 上）
const CHROME = process.env.CHROME ||
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const BASE = process.env.BASE || 'http://127.0.0.1:18090';
const TOKEN = process.env.ADMIN_TOKEN || 'local-verify-token';
const MOCK = process.env.MOCK || 'http://127.0.0.1:18099';
const SHOT = process.env.SHOTS || '/tmp/sla-ui-shots';

const results = [];
function check(name, ok, detail = '') {
  results.push({ name, ok, detail });
  console.log(`${ok ? '✅' : '❌'} ${name}${detail ? ' — ' + detail : ''}`);
  if (!ok) process.exitCode = 1;
}

const sleep = ms => new Promise(r => setTimeout(r, ms));

const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: 'shell',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
});

try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 1400 });

  // 收集控制台错误 —— 页面报 JS 错等于功能不可用，即便 DOM 看着对
  const consoleErrors = [];
  page.on('console', m => {
    if (m.type() === 'error') consoleErrors.push(m.text());
  });
  page.on('pageerror', e => consoleErrors.push('pageerror: ' + e.message));

  // ── 1. 打开管理界面 ──
  const resp = await page.goto(`${BASE}/admin/ui`, { waitUntil: 'domcontentloaded' });
  check('打开 /admin/ui 返回 200', resp.status() === 200, `HTTP ${resp.status()}`);

  const title = await page.title();
  check('页面标题正确', title.includes('上游渠道采集与管理'), title);

  // ── 2. 未填令牌时不该能拉数据（鉴权在服务端）──
  await page.evaluate(() => localStorage.clear());
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.click('#btn-reload');
  await sleep(600);
  const toastText = await page.$eval('#toast', el => el.textContent);
  check('无令牌时拒绝拉取数据', /ADMIN_TOKEN|no token/.test(toastText),
    toastText.slice(0, 60));

  // ── 3. 填入令牌并加载渠道列表 ──
  //
  // ⚠️ 必须容忍**空库**：脚本化验收从干净的库起，此时列表是空状态而非表格。
  // 首版只等 `#channels table`，在空库上必然超时 —— 我的手工验证之所以过，
  // 是因为库里残留着先前手点建的渠道。空库才是"运维第一次打开界面"的真实情形。
  await page.type('#token', TOKEN);
  await page.evaluate(() => document.querySelector('#token')
    .dispatchEvent(new Event('change')));
  await page.click('#btn-reload');
  await page.waitForFunction(
    () => {
      const el = document.querySelector('#channels');
      return el && (el.querySelector('table') || /还没有渠道/.test(el.textContent));
    },
    { timeout: 8000 });
  const chCount = await page.$$eval('#channels tbody tr', rs => rs.length);
  const emptyState = await page.$eval('#channels',
    el => /还没有渠道/.test(el.textContent));
  check('渠道列表可加载（空库显示空状态）', chCount > 0 || emptyState,
    emptyState ? '空库空状态' : `${chCount} 行`);

  await page.screenshot({ path: `${SHOT}/01-list.png` });

  // ── 4. 在界面上真实创建一个渠道商 ──
  const uniq = 'UI验收-' + Date.now().toString().slice(-6);
  await page.type('#ch-name', uniq);
  await page.type('#ch-url', MOCK);
  // 站型留空 = 自动探测
  await page.click('#btn-create');
  await page.waitForFunction(
    () => /渠道已创建/.test(document.querySelector('#toast').textContent),
    { timeout: 15000 });
  const createToast = await page.$eval('#toast', el => el.textContent);
  check('界面创建渠道成功', /渠道已创建/.test(createToast),
    createToast.replace(/\n/g, ' | ').slice(0, 100));
  check('创建时自动探测出站型 newapi', /newapi/.test(createToast));
  check('探测读到 quota_per_unit（未写死）',
    /quota_per_unit=500000/.test(createToast));

  await page.screenshot({ path: `${SHOT}/02-created.png` });

  // 列表里能看到刚建的渠道
  await page.click('#btn-reload');
  await sleep(800);
  const names = await page.$$eval('#channels tbody tr td:nth-child(2)',
    ts => ts.map(t => t.textContent.trim()));
  check('新渠道出现在列表中', names.includes(uniq), names.slice(-3).join(', '));

  check('创建后列表至少有 1 行', names.length >= 1, `${names.length} 行`);
  const newRowIdx = names.indexOf(uniq);
  const newChannelId = await page.$$eval('#channels tbody tr td:first-child',
    (ts, i) => ts[i].textContent.trim(), newRowIdx);

  // ── 5. 点开详情，看资产总览与异常项 ──
  await page.evaluate(i => {
    document.querySelectorAll('#channels button[data-ch]')[i].click();
  }, newRowIdx);
  await page.waitForFunction(
    () => document.querySelector('#inv-stats .stat') !== null, { timeout: 8000 });
  const stats = await page.$$eval('#inv-stats .stat',
    ss => ss.map(s => s.querySelector('span').textContent + '=' +
                      s.querySelector('b').textContent));
  check('资产总览已渲染', stats.length >= 5, stats.join(' '));

  const anomText = await page.$eval('#inv-anomalies', el => el.textContent);
  // 新渠道还没登记凭证与 Key，**必须**报出"缺凭证" ——
  // 首版这条允许"无异常"通过，于是掩盖了一个真缺口：
  // 最常见的"为什么不工作"（没凭证）恰恰是唯一没被 inventory 覆盖的情形。
  check('新渠道明确报出缺少采集凭证',
    /credential_missing/.test(anomText),
    anomText.replace(/\s+/g, ' ').slice(0, 110));

  await page.screenshot({ path: `${SHOT}/03-detail.png` });

  // ── 6. 界面登记凭证 ──
  await page.type('#cr-channel', String(newChannelId));
  await page.type('#cr-token', 'sk-ui-collector-token');
  await page.type('#cr-uid', '42');
  await page.click('#btn-cred');
  await page.waitForFunction(
    () => /凭证已登记|登记凭证失败/.test(document.querySelector('#toast').textContent),
    { timeout: 10000 });
  const credToast = await page.$eval('#toast', el => el.textContent);
  check('界面登记采集凭证成功', /凭证已登记/.test(credToast),
    credToast.replace(/\n/g, ' | ').slice(0, 80));
  check('凭证类型判定为 newapi_access_token',
    /newapi_access_token/.test(credToast));

  // ── 7. 界面登记账号与 Key，并验证明文不回显 ──
  await page.type('#acc-channel', String(newChannelId));
  await page.type('#acc-uid', '42');
  await page.click('#btn-acc');
  await page.waitForFunction(
    () => /账号已创建/.test(document.querySelector('#toast').textContent),
    { timeout: 8000 });

  const SECRET = 'sk-ui-secret-should-never-be-echoed-9f3a';
  await page.type('#key-secret', SECRET);
  await page.type('#key-ref', '7');
  await page.click('#btn-key');
  await page.waitForFunction(
    () => /Key 已登记/.test(document.querySelector('#toast').textContent),
    { timeout: 8000 });
  check('界面登记 Key 成功', true);

  // 关键安全断言：整个页面 DOM 里不得出现完整明文（FR-094）
  const dom = await page.content();
  check('页面任何位置都不回显 Key 明文（FR-094）',
    !dom.includes(SECRET),
    dom.includes(SECRET) ? '⚠️ DOM 中发现明文' : 'DOM 已确认无明文');

  await page.screenshot({ path: `${SHOT}/04-key-registered.png` });

  // ── 8. 界面触发采集，验证逐项结果表 ──
  await page.click('#btn-sync');
  await page.waitForFunction(
    () => document.querySelector('#sync-result table') !== null, { timeout: 90000 });
  const syncRows = await page.$$eval('#sync-result tbody tr',
    rs => rs.map(r => [...r.querySelectorAll('td')].map(t => t.textContent.trim())));
  check('采集结果表已渲染', syncRows.length >= 6, `${syncRows.length} 项`);

  const byCap = Object.fromEntries(syncRows.map(r => [r[0], r[1]]));
  check('account 采集成功', byCap['account'] === 'ok', byCap['account']);
  check('groups 采集成功', byCap['groups'] === 'ok', byCap['groups']);
  check('keys 采集成功', byCap['keys'] === 'ok', byCap['keys']);
  check('model_catalog 采集成功', byCap['model_catalog'] === 'ok',
    byCap['model_catalog']);
  // AC-38：不支持的项必须显式出现，不能静默省略
  check('subscription_quotas 显式标 unsupported（AC-38）',
    byCap['subscription_quotas'] === 'unsupported', byCap['subscription_quotas']);

  await page.screenshot({ path: `${SHOT}/05-sync.png`, fullPage: true });

  // ── 9. 查看分组、目录、Key ──
  await page.click('#btn-groups');
  await page.waitForFunction(
    () => /可用模型/.test(document.querySelector('#detail-body')?.textContent || ''),
    { timeout: 8000 });
  const groupRows = await page.$$eval('#detail-body tbody tr', rs => rs.length);
  check('分组列表已渲染', groupRows === 3, `${groupRows} 个分组（期望 3）`);

  // 点开某分组的可用模型（FR-124："这把 Key 能用哪些模型"）
  await page.click('#detail-body button[data-g]');
  await page.waitForFunction(
    () => /可用模型/.test(document.querySelector('#gm')?.textContent || ''),
    { timeout: 8000 });
  const gmText = await page.$eval('#gm', el => el.textContent);
  check('分组可用模型可查（FR-124）', /gpt-5\.5/.test(gmText),
    gmText.replace(/\s+/g, ' ').slice(0, 80));

  await page.screenshot({ path: `${SHOT}/06-groups.png` });

  await page.click('#btn-catalog');
  await page.waitForFunction(
    () => /输入价/.test(document.querySelector('#detail-body')?.textContent || ''),
    { timeout: 8000 });
  const catRows = await page.$$eval('#detail-body tbody tr', rs => rs.length);
  check('模型目录已渲染', catRows === 8, `${catRows} 个模型（期望 8）`);
  await page.screenshot({ path: `${SHOT}/07-catalog.png` });

  await page.click('#btn-keys');
  // ⚠️ 等"表头出现 Key 专有列"而非"存在 table"：切视图时上一个视图的表格
  // 仍在 DOM 里，只等 table 会读到目录数据（本脚本首版就这样误报了两项）
  await page.waitForFunction(
    () => /前缀/.test(document.querySelector('#detail-body')?.textContent || ''),
    { timeout: 8000 });
  const keyCells = await page.$$eval('#detail-body tbody tr',
    rs => rs.map(r => [...r.querySelectorAll('td')].map(t => t.textContent.trim())));
  check('Key 列表已渲染', keyCells.length >= 1, `${keyCells.length} 把`);
  // 列表只显示前缀
  const prefixOK = keyCells.every(c => c[1].includes('…'));
  check('Key 列表只显示前缀', prefixOK, keyCells.map(c => c[1]).join(' '));
  // 采集后应有剩余额度（归一为美元）
  const hasQuota = keyCells.some(c => c[3].startsWith('$'));
  check('Key 剩余额度已归一为美元显示', hasQuota,
    keyCells.map(c => c[3]).join(' '));

  await page.screenshot({ path: `${SHOT}/08-keys.png`, fullPage: true });

  // ── 10. 限流：立刻再点一次采集应被拒（429，且不打上游）──
  await page.click('#btn-sync');
  await sleep(2500);
  const rateText = await page.$eval('#toast', el => el.textContent);
  check('60s 内重复采集被限流拒绝（09 §5.0bis）',
    /间隔未到|已有 sync/.test(rateText), rateText.slice(0, 70));

  // ── 11. 页面无 JS 错误 ──
  // 只看真正的脚本错误：429 是本脚本自己触发的限流断言，
  // favicon 404 是浏览器自动请求 —— 两者都不是页面缺陷
  const realErrors = consoleErrors.filter(e =>
    !/429|favicon/.test(e));
  check('页面无 JavaScript 错误', realErrors.length === 0,
    realErrors.slice(0, 2).join(' | ') || '无（已排除预期的 429 与 favicon）');

  writeFileSync((process.env.SHOTS || '/tmp/sla-ui-shots') + '/results.json',
    JSON.stringify(results, null, 2));
} finally {
  await browser.close();
}

const failed = results.filter(r => !r.ok);
console.log(`\n${'='.repeat(58)}`);
console.log(`浏览器验收：${results.length - failed.length}/${results.length} 通过`);
if (failed.length) {
  console.log('失败项：');
  failed.forEach(f => console.log(`  ❌ ${f.name} — ${f.detail}`));
}
