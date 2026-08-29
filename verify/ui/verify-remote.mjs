// 真实数据下的管理界面验收（只读）。
//
// 与 verify-ui.mjs 的分工：那一份用 mock 上游跑**写路径**（建渠道、登记凭证、
// 采集、限流），库是一次性的；这一份连内网真库（65 个真实渠道 / 2782 条目录行），
// 只做读与试运行，不往真库写测试行。
//
// 为什么这一份不能省：mock 只有 1 个渠道 9 个模型，而真库里单渠道就有 1369 行
// 目录、口径混着 per_1m_token 与 per_call。表格布局把侧栏顶出视口、口径跨段
// 比较这类问题只在这个量级上才会露出来。
import puppeteer from 'puppeteer-core';
import { writeFileSync } from 'node:fs';

const CHROME = process.env.CHROME ||
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const BASE = process.env.BASE || 'http://127.0.0.1:18390';
const TOKEN = process.env.ADMIN_TOKEN || 'remote-verify-token';
const SHOT = process.env.SHOTS || '/tmp/sla-remote-shots';
// 口径混合的渠道：真库里 channel 3 有 1161 条 per_1m_token + 208 条 per_call，
// 正好能验"跨段比较告警"这条只在真数据上出现的分支。
const MIXED_CH = Number(process.env.MIXED_CH || 3);
// 口径整表为 NULL 的渠道：真库 channel 56 有 294 行全无 billing_unit
// （015 迁移前存量），用来验界面不替上游假定默认口径。
const NULL_CH = Number(process.env.NULL_CH || 56);
// 真库 2 把 Key 都挂在 channel 1 的账号下（upstream_keys 走 account_id 关联，
// 没有 channel_id 列）。FR-094「只显前缀」必须在有 Key 的渠道上验。
const KEY_CH = Number(process.env.KEY_CH || 1);

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
  await page.setViewport({ width: 1440, height: 1600 });

  const pane = async name => {
    await page.click(`.nav-item[data-pane="${name}"]`);
    await page.waitForFunction(
      n => document.querySelector('#pane-' + n)?.classList.contains('on'),
      { timeout: 5000 }, name);
  };
  const bgLuma = () => page.evaluate(() => {
    const c = document.createElement('canvas').getContext('2d');
    c.fillStyle = getComputedStyle(document.body).backgroundColor;
    c.fillRect(0, 0, 1, 1);
    const [r, g, b] = c.getImageData(0, 0, 1, 1).data;
    return Math.round(0.2126 * r + 0.7152 * g + 0.0722 * b);
  });

  const consoleErrors = [];
  page.on('console', m => { if (m.type() === 'error') consoleErrors.push(m.text()); });
  page.on('pageerror', e => consoleErrors.push('pageerror: ' + e.message));

  // ── 1. 打开界面并加载真库渠道 ──
  const resp = await page.goto(`${BASE}/admin/ui`, { waitUntil: 'domcontentloaded' });
  check('打开 /admin/ui 返回 200', resp.status() === 200, `HTTP ${resp.status()}`);

  await page.type('#token', TOKEN);
  await page.click('#btn-reload');
  await page.waitForFunction(
    () => document.querySelectorAll('#channels tbody tr').length > 1,
    { timeout: 20000 });

  const rows = await page.$$eval('#channels tbody tr', trs =>
    trs.map(tr => [...tr.querySelectorAll('td')].map(td => td.innerText.trim())));
  check('真库 65 个渠道全部渲染', rows.length === 65, `${rows.length} 行`);

  const families = await page.$$eval('#channels tbody tr', trs => {
    const c = {};
    trs.forEach(tr => {
      const t = tr.innerText;
      ['newapi', 'sub2api', 'asxs', 'veloera'].forEach(f => {
        if (t.includes(f)) c[f] = (c[f] || 0) + 1;
      });
    });
    return c;
  });
  check('站型分布与库一致（52 newapi + 13 sub2api）',
    families.newapi === 52 && families.sub2api === 13,
    JSON.stringify(families));

  // 65 行表格最容易触发的布局事故：宽表把网格列顶宽、侧栏被挤出视口
  const layout = await page.evaluate(() => {
    const s = document.querySelector('.sidebar').getBoundingClientRect();
    const m = document.querySelector('.main').getBoundingClientRect();
    return { sx: s.x, sw: s.width, mx: m.x, mw: m.width,
             docW: document.documentElement.scrollWidth,
             winW: window.innerWidth };
  });
  check('65 行长表下侧栏仍在视口内且未被顶走',
    layout.sx === 0 && layout.mx === layout.sw,
    `侧栏 x=${layout.sx} 宽=${layout.sw}，主区 x=${layout.mx}`);
  check('页面无横向溢出（表格自己滚，不顶整页）',
    layout.docW <= layout.winW + 1,
    `scrollWidth=${layout.docW} innerWidth=${layout.winW}`);
  await page.screenshot({ path: `${SHOT}/01-real-65.png`, fullPage: true });

  // ── 2. 渠道过滤（65 行靠肉眼找不现实，这是新增的） ──
  // 过滤是**重渲染**而不是隐藏行（renderChannels 按 q 过滤 allChannels），
  // 所以直接数行数即可，不要去看 offsetParent。
  await page.type('#ch-filter', 'sub2api');
  await sleep(300);
  const filtered = await page.$$eval('#channels tbody tr', trs => trs.length);
  check('按站型过滤生效', filtered === 13, `过滤后 ${filtered} 行（期望 13）`);

  await page.type('#ch-filter', 'zzz-不存在-zzz');
  await sleep(300);
  const emptyHint = await page.$eval('#channels', el => el.innerText.trim());
  check('无匹配时给空状态而不是空白表', emptyHint.includes('没有匹配'),
    emptyHint.slice(0, 60));

  await page.$eval('#ch-filter', el => {
    el.value = ''; el.dispatchEvent(new Event('input'));
  });
  await sleep(300);
  const restored = await page.$$eval('#channels tbody tr', trs => trs.length);
  check('清空筛选后恢复全部 65 行', restored === 65, `${restored} 行`);

  // ── 3. 真实渠道详情：口径混合的那一个 ──
  await page.evaluate(id => {
    document.querySelector(`#channels button[data-ch="${id}"]`).click();
  }, MIXED_CH);
  await page.waitForFunction(
    () => document.querySelector('#pane-detail')?.classList.contains('on'),
    { timeout: 5000 });
  await sleep(1500);

  const stats = await page.$eval('#inv-stats', el => el.innerText.replace(/\s+/g, ' '));
  check('真实渠道资产总览已渲染', /账号|Key|分组|目录/.test(stats), stats.slice(0, 160));
  await page.screenshot({ path: `${SHOT}/02-real-detail.png`, fullPage: true });

  // ── 4. 真实模型目录：口径标注与跨段告警 ──
  await page.click('#btn-catalog');
  await page.waitForFunction(
    () => document.querySelector('#detail-body')?.innerText.includes('模型'),
    { timeout: 20000 });
  await sleep(800);

  const cat = await page.$eval('#detail-body', el => el.innerText);
  // 口径标注一律只在表体里找：脚注写着「`/次`是…、`×倍率`是…」，
  // 拿全文匹配的话价格列一个口径都没渲染出来也照样绿。
  const cells = () => page.$$eval('#detail-body tbody tr',
    trs => trs.map(tr => tr.innerText).join('\n'));
  const first = await cells();
  check('真实目录已渲染（上千行量级）',
    /共 \d{3,}/.test(cat), (cat.match(/共 [\d,]+/) || ['未见总数'])[0]);
  check('倍率计价在价格列标 ×倍率', first.includes('×倍率'));

  // 分段规模必须直接写在按钮上。真库 channel 3 是 1161 倍率 + 208 按次，
  // 不显示行数的话运维无从知道自己看的是全表还是一段。
  const segs = await page.$$eval('#detail-body button[data-unit]',
    bs => bs.map(b => ({ u: b.dataset.unit, t: b.innerText.replace(/\s+/g, ' ') })));
  check('口径分段按钮已渲染且带各段行数',
    segs.some(s => s.u === 'per_call' && /208/.test(s.t)) &&
    segs.some(s => s.u === 'per_1m_token' && /1161/.test(s.t)),
    segs.map(s => s.t).join(' | ') || '未见分段按钮');

  // ★ 这条是本次修复的核心：按次那 208 行原先**在界面上不可达**。
  // ListCatalog 按 billing_unit 分段排序，per_1m_token 字母序在前，
  // 一页 50 行 —— 按次的第一条落在第 1162 位，要翻 24 页才见得到，
  // 而脚注偏偏在提醒"跨段不可直接比大小"。告警对着一个永远只有一段的
  // 表格说话，等于没说。分段按钮把两段都拉到一次点击之内。
  await page.click('#detail-body button[data-unit="per_call"]');
  await page.waitForFunction(
    () => document.querySelector('#detail-body tbody tr')?.innerText.includes('/次'),
    { timeout: 10000 }).catch(() => {});
  const callCells = await cells();
  check('切到按次分段后，价格列确实出现 /次（原先翻 24 页才够得着）',
    callCells.includes('/次'), callCells.includes('/次') ? '已标注' : '未见 /次');
  // 分段要**纯净**：混进倍率行说明筛选没生效，跨段比较的风险照旧
  check('按次分段内不混入倍率行',
    !callCells.includes('×倍率'),
    callCells.includes('×倍率') ? '混入了 ×倍率' : '该段只有按次');
  const callHead = await page.$eval('#detail-body', el => el.innerText);
  check('跨段不可比的告警文案在位',
    callHead.includes('跨段不可直接比大小'));
  await page.screenshot({ path: `${SHOT}/03c-per-call-seg.png`, fullPage: true });

  // 切回全部：分段是视图状态，切换必须可逆，否则运维以为目录只剩 208 行
  await page.click('#detail-body button[data-unit=""]');
  await sleep(700);
  const backCells = await cells();
  check('切回「全部」后回到倍率段首页（分段切换可逆）',
    backCells.includes('×倍率'), backCells.slice(0, 60).replace(/\s+/g, ' '));

  await page.screenshot({ path: `${SHOT}/03-real-catalog.png`, fullPage: true });

  // ── 4ter. 名称筛选：1369 行里定位一个模型 ──
  // 真库单渠道 1369 行，靠翻页找模型不现实。这条同时守住"筛选走后端 q 参数"
  // ——若只在当前页 50 行里过滤，搜 flux 会一条都搜不到（它们在按次段）。
  await page.type('#cat-q', 'flux');
  await page.waitForFunction(
    () => /flux/i.test(document.querySelector('#detail-body tbody')?.innerText || ''),
    { timeout: 10000 }).catch(() => {});
  const qCells = await cells();
  const qRows = await page.$$eval('#detail-body tbody tr', trs => trs.length);
  check('按模型名筛选命中跨页的行（后端 q 参数生效）',
    qRows > 0 && /flux/i.test(qCells), `${qRows} 行`);
  // 焦点必须留在输入框里：重渲染换掉 input 而不搬回焦点的话，
  // 第二个字符就打到 body 上了，看着像"筛选只认一个字"
  const stillFocused = await page.evaluate(() => document.activeElement?.id);
  check('筛选框重渲染后焦点保留（可连续输入）',
    stillFocused === 'cat-q', `activeElement=${stillFocused}`);
  await page.screenshot({ path: `${SHOT}/03d-catalog-search.png`, fullPage: true });

  // ── 4bis. 口径全缺的渠道：必须显式标「口径未知」，不能替上游假定默认口径 ──
  //
  // 真库有 1413 行 billing_unit 为 NULL（015 迁移前的存量，等各渠道下次采集
  // 回填）。挑一个整表全 NULL 的渠道来验：若界面悄悄按 per_1m_token 渲染，
  // 那 208 条按次计价的绝对美元价会被当成倍率读，量级差一百万倍。
  await pane('channels');
  await page.evaluate(id => {
    document.querySelector(`#channels button[data-ch="${id}"]`).click();
  }, NULL_CH);
  await page.waitForFunction(
    () => document.querySelector('#pane-detail')?.classList.contains('on'),
    { timeout: 5000 });
  await sleep(1200);
  await page.click('#btn-catalog');
  await page.waitForFunction(
    () => document.querySelector('#detail-body')?.innerText.includes('模型'),
    { timeout: 20000 });
  await sleep(600);
  // 只看表体单元格，不看整个 #detail-body：常驻脚注里就写着
  // 「`/次`是绝对美元价、`×倍率`是倍数」，拿 innerText 全文匹配会命中脚注，
  // 于是"未误标"这条断言永远失败（第一版就是这样红的）。
  const nullCells = await page.$$eval('#detail-body tbody tr',
    trs => trs.map(tr => tr.innerText).join('\n'));
  check(`口径缺失的渠道 #${NULL_CH} 显式标「口径未知」而非假定默认值`,
    nullCells.includes('口径未知'),
    nullCells.includes('口径未知') ? '已显式标注' : '未见「口径未知」');
  check(`渠道 #${NULL_CH} 的价格单元格未被误标为倍率或按次`,
    !nullCells.includes('×倍率') && !nullCells.includes('/次'),
    nullCells.includes('×倍率') ? '误标为倍率' :
      nullCells.includes('/次') ? '误标为按次' : '未误标');
  await page.screenshot({ path: `${SHOT}/03b-null-unit.png`, fullPage: true });

  // ── 5. 真实 Key 列表仍然只显示前缀（FR-094 在真库上复核） ──
  //
  // 必须切到真的有 Key 的渠道：真库 2 把 Key 都挂在 channel 1 的账号下。
  // 在没有 Key 的渠道上点「Key」只会看到空状态，那样这条断言等于没验。
  await pane('channels');
  await page.evaluate(id => {
    document.querySelector(`#channels button[data-ch="${id}"]`).click();
  }, KEY_CH);
  await page.waitForFunction(
    () => document.querySelector('#pane-detail')?.classList.contains('on'),
    { timeout: 5000 });
  await sleep(1200);
  await page.click('#btn-keys');
  await sleep(1500);
  const keyText = await page.$eval('#detail-body', el => el.innerText);
  const keyRows = await page.$$eval('#detail-body tbody tr', trs => trs.length)
    .catch(() => 0);
  check(`渠道 #${KEY_CH} 的 Key 列表确有行（不是空状态）`, keyRows >= 1,
    `${keyRows} 行`);
  const leaked = await page.evaluate(() =>
    /sk-[A-Za-z0-9]{20,}/.test(document.body.innerText));
  check('真库 Key 列表不回显完整密钥（FR-094）', !leaked,
    leaked ? '发现疑似完整密钥' : 'DOM 已确认无长密钥串');
  check('Key 前缀以省略号截断显示', /…|\.\.\./.test(keyText),
    keyText.split('\n').filter(l => /…|\.\.\./.test(l))[0] || '未见截断标记');

  // ── 6. 分组与分组可用模型（FR-124）──
  //
  // 必须回到 channel 3：真库里 64 个分组的可用模型数为 0（上游没给或没采到），
  // channel 1 的 default/vip/svip 恰好全是 0。在那种渠道上验"可用模型可查"，
  // 看到的 0 个模型 是真实的，但这条断言就什么都没验到。
  // channel 3 的分组有 1354/1333/445/163/37 个模型，是有内容的那种。
  await pane('channels');
  await page.evaluate(id => {
    document.querySelector(`#channels button[data-ch="${id}"]`).click();
  }, MIXED_CH);
  await page.waitForFunction(
    () => document.querySelector('#pane-detail')?.classList.contains('on'),
    { timeout: 5000 });
  await sleep(1200);
  await page.click('#btn-groups');
  await sleep(1800);
  const grpRows = await page.$$eval('#detail-body tbody tr',
    trs => trs.map(tr => tr.innerText.replace(/\s+/g, ' ').trim()));
  check(`渠道 #${MIXED_CH} 分组列表已渲染`, grpRows.length >= 5,
    `${grpRows.length} 个分组`);
  const nonZero = grpRows.filter(r => /[1-9]\d* 个模型/.test(r));
  check('分组可用模型数非零（FR-124 真数据）', nonZero.length >= 1,
    nonZero[0] || '全部为 0 个模型');
  await page.screenshot({ path: `${SHOT}/04-real-groups.png`, fullPage: true });

  // 点开一个有模型的分组，验"上游分组名 + 模型清单"真能查
  const opened = await page.evaluate(() => {
    // 只认 button[data-g]（分组行上的"N 个模型"按钮），别用泛的 button 选择器
    const b = [...document.querySelectorAll('#detail-body button[data-g]')]
      .find(x => !/^0 个模型/.test(x.innerText.trim()));
    if (!b) return null;
    b.click();
    return b.dataset.name;
  });
  if (opened) {
    await sleep(1500);
    const gmText = await page.$eval('#gm', el => el.innerText).catch(() => '');
    check('分组可用模型可展开且标出上游分组名（FR-124）',
      gmText.length > 20 && /分组/.test(gmText),
      gmText.slice(0, 110).replace(/\n/g, ' '));
    await page.screenshot({ path: `${SHOT}/04b-group-models.png`, fullPage: true });
  } else {
    check('分组可用模型可展开且标出上游分组名（FR-124）', false,
      '未找到可点开的非零分组按钮');
  }

  // ── 7. 采集凭证：真库 65 条，一条都不能回显内容 ──
  await pane('creds');
  await page.click('#btn-cred-reload');
  await page.waitForFunction(
    () => document.querySelectorAll('#cred-list tbody tr').length > 1,
    { timeout: 20000 });
  const creds = await page.$$eval('#cred-list tbody tr', trs =>
    trs.map(tr => [...tr.querySelectorAll('td')].map(td => td.innerText.trim())));
  check('真库 65 条采集凭证全部渲染', creds.length === 65, `${creds.length} 条`);
  const credDump = JSON.stringify(creds);
  check('凭证列表只报"有/无"，不含令牌内容',
    !/sk-[A-Za-z0-9]{20,}/.test(credDump) && !/eyJ[A-Za-z0-9_-]{10,}/.test(credDump),
    '已确认无令牌串与 JWT');
  await page.screenshot({ path: `${SHOT}/05-real-creds.png`, fullPage: true });

  // ── 8. 深浅两色在真数据页面上都成立 ──
  await pane('channels');
  await page.click('#theme-sw button[data-theme="dark"]');
  await sleep(300);
  const dl = await bgLuma();
  check('真数据页面深色模式确为深色', dl < 60, `亮度=${dl}`);
  await page.screenshot({ path: `${SHOT}/06-real-dark.png`, fullPage: true });

  await page.click('#theme-sw button[data-theme="light"]');
  await sleep(300);
  const ll = await bgLuma();
  check('真数据页面浅色模式确为浅色', ll > 200, `亮度=${ll}`);
  await page.screenshot({ path: `${SHOT}/07-real-light.png`, fullPage: true });

  // ── 9. 无 JS 错误 ──
  const real = consoleErrors.filter(e =>
    !e.includes('favicon') && !/40[0-9]|42[0-9]|50[0-9]/.test(e));
  check('页面无 JavaScript 错误', real.length === 0,
    real.length ? real.slice(0, 3).join(' | ') : '无');
} finally {
  await browser.close();
  writeFileSync(`${SHOT}/results.json`, JSON.stringify(results, null, 2));
  const pass = results.filter(r => r.ok).length;
  console.log('\n' + '='.repeat(58));
  console.log(`真库浏览器验收：${pass}/${results.length} 通过`);
}
