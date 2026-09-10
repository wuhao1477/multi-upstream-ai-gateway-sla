// P1 管理界面的真实浏览器验收。
//
// 用真 Chrome（非 headless shell 的 DOM dump）：点击、填表、等 XHR、截图，
// 验证的是"运维真能在 web 端加渠道商并采集"，而不是"HTML 里有那些字符串"。
import puppeteer from 'puppeteer-core';
import { writeFileSync, existsSync, readFileSync } from 'node:fs';

// 路径由 ui-stack.sh 按平台探测后注入（macOS 在 .app 里、CI 在 PATH 上）
const CHROME = process.env.CHROME ||
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const BASE = process.env.BASE || 'http://127.0.0.1:18090';
const TOKEN = process.env.ADMIN_TOKEN || 'local-verify-token';
const SHOT = process.env.SHOTS || '/tmp/sla-ui-shots';
// all-api-hub 备份文件。批量导入试运行用它,同时它也是上游凭证的来源。
const HUB_FILE = process.env.HUB_FILE || '';

// 真上游。由 ui-stack.sh 跑 verify/pick-upstream.mjs 现场探活后注入 ——
// 验收不构造假上游(CLAUDE.md §1),所以这些值每次都可能不同,
// 断言必须写成"形态与关系"而不是具体数字。
const UP_URL = process.env.UP_URL || '';
const UP_TOKEN = process.env.UP_TOKEN || '';
const UP_UID = process.env.UP_UID || '';
const UP_QPU = Number(process.env.UP_QPU || 0);
const UP_MODELS = Number(process.env.UP_MODELS || 0);
const UP_PER_CALL = Number(process.env.UP_PER_CALL || 0);
const UP_KEYREF = process.env.UP_KEYREF || '';
if (!UP_URL || !UP_TOKEN || !UP_UID || !UP_KEYREF) {
  console.error('缺 UP_URL / UP_TOKEN / UP_UID / UP_KEYREF —— 请通过 verify/ui-stack.sh 运行');
  console.error('（它会先跑 pick-upstream.mjs 从 HUB_FILE 里挑一个真上游）');
  process.exit(2);
}

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
  timeout: 90_000,   // 理由见 verify-spa.mjs 同处
});

try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1280, height: 1400 });

  // ── 布局改成左右分栏后新增的两个助手 ──
  //
  // pane()：表单散在不同分栏里，未激活的分栏是 display:none。
  // puppeteer 往隐藏元素 type 不会报错，但 focus() 是空操作 —— 按键会落到
  // 上一个焦点元素上，症状是"填了却没填进去"，且没有任何报错。故先切分栏。
  const pane = async name => {
    await page.click(`.nav-item[data-pane="${name}"]`);
    await page.waitForFunction(
      n => document.querySelector('#pane-' + n)?.classList.contains('on'),
      { timeout: 5000 }, name);
  };
  // fill()：选中渠道时会把渠道 ID 预填进登记表单。
  // 用真实键盘事件更新 v-model，避免直接改 DOM 与 Vue 状态不同步。
  const fill = async (sel, val) => {
    await page.click(sel, { clickCount: 3 });
    await page.keyboard.press('Backspace');
    await page.type(sel, val, { delay: 2 });
  };
  // ── 抽屉 / 菜单 / 确认框改造后新增的三个助手 ──
  //
  // 登记表单搬进了右侧抽屉，低频操作（停用/删除）收进了行内的「更多」菜单，
  // 危险操作走统一确认框而不再是 window.confirm。三者都要先"打开"才能点到
  // 里面的控件，而 puppeteer 往未渲染的元素 click 会直接抛错 —— 不是静默
  // 失败，所以漏了这一步会红在正确的地方。
  const openDrawer = async (btn, field) => {
    await page.click(btn);
    await page.waitForSelector(field, { visible: true, timeout: 5000 });
  };
  // 行内「更多」菜单：原生 <details>，点 summary 展开。
  // 已经展开时再点会收起，故先判状态。
  const openMore = async (sel) => {
    const el = await page.$(sel);
    if (el === null) throw new Error(`找不到更多菜单：${sel}`);
    const open = await page.evaluate(s => document.querySelector(s)
      ?.closest('details')?.open === true, sel);
    if (!open) await page.click(sel);
  };
  // 统一确认框。同一时刻只会开一个，故按 [data-confirm-ok] 取即可。
  const confirmOK = async () => {
    await page.waitForSelector('[data-confirm-ok]', { visible: true, timeout: 5000 });
    await page.click('[data-confirm-ok]');
  };
  // 取 body 背景的真实 sRGB 亮度：断言"深色模式真的是深的"，
  // 而不是只断言 class 名变了（那样把 .dark 里的色值写成白色也照样绿）。
  const bgLuma = () => page.evaluate(() => {
    const c = document.createElement('canvas').getContext('2d');
    c.fillStyle = getComputedStyle(document.body).backgroundColor;
    c.fillRect(0, 0, 1, 1);
    const [r, g, b] = c.getImageData(0, 0, 1, 1).data;
    return Math.round(0.2126 * r + 0.7152 * g + 0.0722 * b);
  });

  // 收集控制台错误 —— 页面报 JS 错等于功能不可用，即便 DOM 看着对
  const consoleErrors = [];
  page.on('console', m => {
    if (m.type() === 'error') consoleErrors.push(m.text());
  });
  page.on('pageerror', e => consoleErrors.push('pageerror: ' + e.message));
  page.on('dialog', dialog => dialog.accept());

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
  // 用 tr[data-ch-row] 而不是 tbody tr：编辑/停用展开时 tbody 里会多一个
  // sub-row（那是表单，不是渠道），不区分的话渠道数会多算。
  const chCount = await page.$$eval('#channels tr[data-ch-row]', rs => rs.length);
  const emptyState = await page.$eval('#channels',
    el => /还没有渠道/.test(el.textContent));
  check('渠道列表可加载（空库显示空状态）', chCount > 0 || emptyState,
    emptyState ? '空库空状态' : `${chCount} 行`);

  // 站型下拉必须来自后端注册表（GET /admin/site-families），不是写死的四项。
  //
  // 逐项等值比对而不是数个数：数个数的话，把 v-for 删掉再写死四个 option
  // 照样绿 —— 而"写死"正是这条要防的东西（加站型时那份列表不报错，
  // 新站型只是在界面上不存在）。
  const famFromAPI = await page.evaluate(async () => {
    const t = document.querySelector('#token').value;
    const r = await fetch('/admin/site-families',
      { headers: { Authorization: `Bearer ${t}` } });
    const d = await r.json();
    return (d.items ?? []).map(f => [f.family, f.display_name]);
  });
  // 新建渠道搬进了抽屉（建渠道一周一次，不该常年占着首屏），
  // 所以站型下拉要先把抽屉打开才存在。
  await openDrawer('#btn-open-create', '#ch-family');
  // 首项是"自动探测"（value=''），它不属注册表 —— 按 value 非空筛掉。
  const famInDOM = await page.$$eval('#ch-family option',
    os => os.filter(o => o.value !== '').map(o => [o.value, o.textContent.trim()]));
  check('站型下拉逐项等于注册表返回的家族与显示名（不是写死）',
    famFromAPI.length > 0 &&
    JSON.stringify(famInDOM) === JSON.stringify(famFromAPI),
    `注册表=${JSON.stringify(famFromAPI)} 下拉=${JSON.stringify(famInDOM)}`);

  await page.screenshot({ path: `${SHOT}/01-list.png` });

  // ── 4. 在界面上真实创建一个渠道商 ──
  const uniq = 'UI验收-' + Date.now().toString().slice(-6);
  await page.type('#ch-name', uniq);
  await page.type('#ch-url', UP_URL);
  // 站型留空 = 自动探测
  await page.click('#btn-create');
  await page.waitForFunction(
    () => /渠道已创建/.test(document.querySelector('#toast').textContent),
    { timeout: 30000 });
  const createToast = await page.$eval('#toast', el => el.textContent);
  check('界面创建渠道成功', /渠道已创建/.test(createToast),
    createToast.replace(/\n/g, ' | ').slice(0, 100));
  check('创建时自动探测出站型 newapi', /newapi/.test(createToast));
  // 断言"读到了上游此刻真实声明的那个值",不是断言某个固定数字 ——
  // quota_per_unit 逐站不同,写死等于把 mock 的常量搬进真上游验收。
  const qpuHit = new RegExp(`quota_per_unit=${UP_QPU}\\b`).test(createToast);
  check('探测读到上游真实声明的 quota_per_unit（未写死）',
    qpuHit && UP_QPU > 0, `上游声明 ${UP_QPU}，界面回显${qpuHit ? '一致' : '不一致'}`);

  await page.screenshot({ path: `${SHOT}/02-created.png` });

  // 列表里能看到刚建的渠道。
  //
  // ⚠️ 名称与 id **都按 data-* 属性取，不按列下标**。原先取的是
  // `td:nth-child(2)`（名称）与 `td:first-child`（id）—— 列一改就全错位，
  // 而错位后断言仍然绿，只是从此验的是别的列。现在名称格里还多了一行
  // base_url，按下标取到的会是 "UI验收-123456https://…"，`includes(uniq)`
  // 恰好还能过，然后在下一行 indexOf 上静默错位。
  await page.click('#btn-reload');
  await sleep(800);
  const names = await page.$$eval('#channels [data-ch-name]',
    ts => ts.map(t => t.textContent.trim()));
  check('新渠道出现在列表中', names.includes(uniq), names.slice(-3).join(', '));

  check('创建后列表至少有 1 行', names.length >= 1, `${names.length} 行`);
  const newRowIdx = names.indexOf(uniq);
  const newChannelId = await page.$$eval('#channels [data-ch-name]',
    (ts, i) => ts[i].getAttribute('data-ch-name'), newRowIdx);

  // ── 4bis. 渠道行二级展开：直接看到该渠道下的账号与 Key ──
  //
  // 此刻这个渠道**刚建好、什么都没有**，所以正确的展开区不是空白，
  // 而是明确说"没有账号"。这一条断的正是那个说法：旧版一行只有名称与
  // 状态，"它是空的"和"它采失败了"在界面上一模一样。
  await page.click(`[data-ch-toggle="${newChannelId}"]`);
  await page.waitForFunction(
    id => document.querySelector(`tr[data-ch-row="${id}"]`)?.classList.contains('open'),
    { timeout: 5000 }, newChannelId);
  const emptyExpand = await page.evaluate(() =>
    document.querySelector('.expand')?.textContent ?? '');
  check('新渠道展开后明确说明"还没有账号"（不是空白）',
    /还没有账号/.test(emptyExpand),
    emptyExpand.replace(/\s+/g, ' ').slice(0, 80));
  await page.click(`[data-ch-toggle="${newChannelId}"]`);

  // ── 5. 点开详情，看资产总览与异常项 ──
  await page.click(`[data-ch="${newChannelId}"]`);
  await page.waitForFunction(
    () => document.querySelector('#inv-stats .stat') !== null, { timeout: 8000 });
  const stats = await page.$$eval('#inv-stats .stat',
    ss => ss.map(s => s.querySelector('span').textContent + '=' +
                      s.querySelector('b').textContent));
  check('资产总览已渲染', stats.length >= 5, stats.join(' '));

  // 口径断言。这一格取的是 SUM(upstream_keys.remain_quota_usd) —— 那是 Key
  // 的**使用约束**，不是这个渠道的钱。它原先叫「额度合计」，摆在「账号」
  // 旁边会被读成渠道余额，正是 FR-022 禁止的那种混淆（多把 Key 的配额相加
  // 不等于账号余额）。名字必须写全，且必须与「账号余额合计」并存、可区分。
  const statLabels = stats.map(s => s.split('=')[0]);
  check('资金口径在总览里分得开（Key 配额 ≠ 账号余额）',
    statLabels.includes('Key 剩余配额合计')
      && statLabels.includes('关联账号余额合计')
      && !statLabels.includes('额度合计'),
    statLabels.join(' | '));

  const anomText = await page.$eval('#inv-anomalies', el => el.textContent);
  // 新渠道还没登记凭证与 Key，**必须**报出"缺凭证" ——
  // 首版这条允许"无异常"通过，于是掩盖了一个真缺口：
  // 最常见的"为什么不工作"（没凭证）恰恰是唯一没被 inventory 覆盖的情形。
  check('新渠道明确报出缺少采集凭证',
    /credential_missing/.test(anomText),
    anomText.replace(/\s+/g, ' ').slice(0, 110));

  await page.screenshot({ path: `${SHOT}/03-detail.png` });

  // ── 5bis. 缺凭证就点采集：必须报缺凭证，且**不占限流窗口** ──
  //
  // 这一步顺序是刻意的。本脚本原先直接按"先登记凭证再采集"的正确顺序走，
  // 于是 33 项全绿却漏掉了运维第一次用界面的真实路线：先点采集才知道缺什么。
  // 那次失败在本地就返回、没发出一个上游字节，却照样起算了 60 秒窗口，
  // 把紧随其后的第一次真采集挡死（P1-evidence §4 第 15 项）。
  //
  // 断言分两处：这里认"报的是缺凭证"，第 8 步认"登记完能立刻采成"——
  // 若窗口重新被本地失败起算，第 8 步会因 429 而红。
  // 422 带 items，所以表格照渲染，原因文案落在 toast 上（index.html 的 catch 分支）。
  await page.click('#btn-sync');
  await page.waitForFunction(
    () => /采集未成功/.test(document.querySelector('#toast')?.textContent || ''),
    { timeout: 30000 });
  const preCredMsg = await page.$eval('#toast', el => el.textContent);
  check('缺凭证时采集报出缺凭证而非上游故障（§4 缺陷 15）',
    /未登记采集凭证|前置条件不满足/.test(preCredMsg),
    preCredMsg.replace(/\s+/g, ' ').slice(0, 90));
  // 单看文案不足以判定修好了 —— 修复前的 502 也带同样的"未登记采集凭证"。
  // 真正的区别是它现在按"未打上游"归类：带结构化 items 且标 skipped
  // （旧路径没有 items，只渲染一行光秃秃的"采集失败"）。
  const preCredRows = await page.$$eval('#sync-result tbody tr',
    rs => rs.map(r => [...r.querySelectorAll('td')].map(t => t.textContent.trim())));
  check('前置失败按"未触达上游"归类（items 标 skipped）',
    preCredRows.some(r => r.includes('skipped')),
    preCredRows.length ? JSON.stringify(preCredRows[0]) : '无 items（旧 502 路径）');

  // ── 6. 界面登记两个账号与四把 Key，并验证明文不回显（AC-37）──
  //
  // 「账号与 Key」分栏已拆成「账号管理」与「Key 管理」两个分栏，登记表单
  // 下沉到抽屉：渠道与账号都是**下拉选择**，不再要求把 id 从列表抄进输入框。
  // 所以这里用 page.select 而不是 fill —— 往 <select> 里 type 不会报错，
  // 但也什么都不会发生（症状是"填了却没填进去"）。
  await pane('accounts');
  const addAccount = async () => {
    await openDrawer('#btn-new-account', '#acc-channel');
    await page.select('#acc-channel', String(newChannelId));
    // 必须填**真的**上游用户 ID：SaveAccount 先按 external_user_id 匹配账号行，
    // 匹配不上才退回"该渠道只有一个账号就用它"。填个假 uid 一样能过，
    // 但过的是兜底分支 —— 匹配逻辑本身就没被验到。
    await fill('#acc-uid', UP_UID);
    await page.click('#btn-acc');
    await page.waitForFunction(
      () => /账号已创建/.test(document.querySelector('#toast').textContent),
      { timeout: 8000 });
  };
  // 渠道下拉必须真的列出了刚建的那个渠道 —— 它是"不再手抄 ID"的前提。
  await openDrawer('#btn-new-account', '#acc-channel');
  const chOptionExists = await page.evaluate(
    id => document.querySelector(`#acc-channel option[value="${id}"]`) !== null,
    newChannelId);
  check('登记账号时渠道来自下拉选择（不再手抄渠道 ID）', chOptionExists,
    `渠道 #${newChannelId} ${chOptionExists ? '在下拉里' : '不在下拉里'}`);
  await page.click('.drawer-x');

  await addAccount();
  await addAccount();
  await page.waitForFunction(
    () => document.querySelectorAll('#pane-accounts tr[data-account-row]').length >= 2,
    { timeout: 8000 });
  // id 取自 data-account-row 属性而不是第一格文本：第一格前面还多了一个
  // 展开箭头格，按下标取会取到那个箭头。
  const accountIDs = await page.$$eval('#pane-accounts tr[data-account-row]',
    rs => rs.map(r => r.getAttribute('data-account-row')));
  check('界面登记两个账号（AC-37）', accountIDs.length >= 2, accountIDs.join(', '));

  // 账号页必须给出**该账号下的 Key 数**：停用确认框的影响面文案靠它，
  // 而"这个账号挂了几把 Key"是决定要不要停用的依据。
  const keyCountCells = await page.$$eval('#pane-accounts td[data-col="keys"]',
    ts => ts.map(t => t.textContent.replace(/\s+/g, ' ').trim()));
  check('账号列表显示各账号的 Key 数（可用 / 总数）',
    keyCountCells.length >= 2 && keyCountCells.every(t => /\d+\s*\/\s*\d+/.test(t)),
    keyCountCells.join(' | '));

  // 账号编辑与停用/启用必须在同一条真实管理路径中可用。
  const secondAccount = accountIDs[1];
  await page.click(`[data-account-edit="${secondAccount}"]`);
  await page.waitForSelector(`#account-edit-uid-${secondAccount}`, { timeout: 5000 });
  await fill(`#account-edit-uid-${secondAccount}`, `${UP_UID}-edited`);
  await page.click(`[data-account-save="${secondAccount}"]`);
  await page.waitForFunction(
    (id, uid) => document.querySelector(`[data-account-row="${id}"]`)?.textContent.includes(uid),
    { timeout: 8000 }, secondAccount, `${UP_UID}-edited`);

  // 停用走统一确认框（原来是 window.confirm，写不下影响面）。
  // 确认框必须**说清影响多少把 Key** —— 那是决定要不要点的依据，
  // 而"确定？"三个字给不了这个信息。
  await page.click(`[data-account-disable="${secondAccount}"]`);
  await page.waitForSelector(`#account-disable-reason-${secondAccount}`, { timeout: 5000 });
  const impactText = await page.$eval('.confirm-i', el => el.textContent.trim());
  check('停用确认框写明影响范围（不是一句"确定？"）',
    /Key/.test(impactText) && impactText.length > 8,
    impactText.slice(0, 70));
  // 原因必填（FR-095）：先不填就点确认，必须被拦住、账号仍是启用。
  await confirmOK();
  await sleep(400);
  const stillOpen = await page.evaluate(() =>
    document.querySelector('[data-confirm-ok]') !== null);
  const reasonToast = await page.$eval('#toast', el => el.textContent);
  check('停用原因未填时被拦下（FR-095）',
    stillOpen && /原因/.test(reasonToast),
    `${stillOpen ? '确认框仍开着' : '确认框已关闭'}；toast=${reasonToast.slice(0, 40)}`);

  await fill(`#account-disable-reason-${secondAccount}`, '验收脚本停用测试');
  await confirmOK();
  // 状态文案从 active/disabled 改成了启用/停用：英文枚举值直接抛给运维，
  // 而 disabled 与 revoked 在界面上要靠猜。
  await page.waitForFunction(
    id => document.querySelector(`[data-account-row="${id}"] [data-col="status"]`)
      ?.textContent.includes('停用'),
    { timeout: 8000 }, secondAccount);
  await page.click(`[data-account-enable="${secondAccount}"]`);
  await page.waitForFunction(
    id => document.querySelector(`[data-account-row="${id}"] [data-col="status"]`)
      ?.textContent.includes('启用'),
    { timeout: 8000 }, secondAccount);
  check('账号编辑、停用与启用可用（AC-37）', true);

  // 凭证现在按账号登记：真实上游令牌挂在第一个账号上。
  await pane('creds');
  await page.waitForFunction(
    id => document.querySelector(`#cr-account option[value="${id}"]`) !== null,
    { timeout: 8000 }, accountIDs[0]);
  await page.select('#cr-account', accountIDs[0]);
  await fill('#cr-token', UP_TOKEN);
  await page.click('#btn-cred');
  await page.waitForFunction(
    () => /凭证已登记|登记凭证失败/.test(document.querySelector('#toast').textContent),
    { timeout: 10000 });
  const credToast = await page.$eval('#toast', el => el.textContent);
  check('界面登记采集凭证成功', /凭证已登记/.test(credToast),
    credToast.replace(/\n/g, ' | ').slice(0, 80));
  check('凭证类型判定为 newapi_access_token', /newapi_access_token/.test(credToast));
  const credRow = await page.$$eval('#cred-list tbody tr',
    rs => rs.map(r => [...r.querySelectorAll('td')].map(t => t.textContent.trim())));
  check('凭证列表已渲染且不含令牌内容',
    credRow.length >= 1 && !JSON.stringify(credRow).includes(UP_TOKEN),
    credRow.length ? credRow[0].join(' / ') : '空');

  await pane('keys');
  const keySecrets = [
    'sk-ui-account1-key1-never-echoed', 'sk-ui-account1-key2-never-echoed',
    'sk-ui-account2-key1-never-echoed', 'sk-ui-account2-key2-never-echoed',
  ];
  const keyRefs = [UP_KEYREF, `${UP_KEYREF}-2`, `${UP_KEYREF}-3`, `${UP_KEYREF}-4`];
  for (let i = 0; i < keySecrets.length; i++) {
    await openDrawer('#btn-new-key', '#key-account');
    // 账号同样是下拉。选渠道会把账号下拉限定到该渠道 —— 跨渠道挂 Key
    // 本来就建不出来，让它在界面上也选不出来。
    await page.select('#key-channel', String(newChannelId));
    await page.select('#key-account', accountIDs[i < 2 ? 0 : 1]);
    await fill('#key-secret', keySecrets[i]);
    await fill('#key-ref', keyRefs[i]);
    await page.click('#btn-key');
    await page.waitForFunction(
      () => /Key 已登记/.test(document.querySelector('#toast').textContent),
      { timeout: 8000 });
  }
  check('界面登记四把 Key（AC-37）', true);

  // 登记抽屉关闭后，明文输入框必须**从 DOM 上消失**而不只是隐藏。
  // 留在 DOM 里等于把它留在页面上 —— 与 FR-094 是同一条理由。
  const secretGone = await page.evaluate(() =>
    document.querySelector('#key-secret') === null);
  check('登记抽屉关闭后明文输入框离开 DOM（FR-094）', secretGone,
    secretGone ? '已移除' : '⚠️ #key-secret 仍在 DOM 中');

  // 这些 Key 是**假的**,而且必须是假的 —— CLAUDE.md §1 允许的唯一例外:
  // 被造的东西本身就是测试输入。这里要验的是"明文不回显",拿真 Key 试等于
  // 把真凭证写进 DOM 快照和 CI 日志,失败时反而漏得更彻底。
  //
  // 由 ui-stack.sh 用 UI_KEY_SECRET 传进来:P1 退出标准③ 要求明文在"响应/
  // 日志/抓包"里一处都不出现,而日志那一端只有 shell 侧看得到
  // (/tmp/sla-ui-core.log)。两边各写一份字面量必然哪天漂掉,故只留一处来源。
  const SECRET = process.env.UI_KEY_SECRET || keySecrets[0];
  keySecrets[0] = SECRET;

  // 关键安全断言：整个页面 DOM 里不得出现完整明文（FR-094）
  const dom = await page.content();
  check('页面任何位置都不回显 Key 明文（FR-094）',
    !keySecrets.some(secret => dom.includes(secret)),
    keySecrets.some(secret => dom.includes(secret)) ? '⚠️ DOM 中发现明文' : 'DOM 已确认无明文');

  await page.screenshot({ path: `${SHOT}/04-key-registered.png` });

  // ── 7. 界面触发采集，验证逐项结果表 ──
  //
  // 注意这是本次会话的**第二次**点采集（5bis 缺凭证失败过一次）。
  // 它能成功本身就是断言：前置失败没有起算最小间隔窗口。
  await pane('detail');
  await page.click('#btn-sync');
  await page.waitForFunction(
    () => document.querySelector('#sync-result table') !== null, { timeout: 90000 });
  const syncRows = await page.$$eval('#sync-result tbody tr',
    rs => rs.map(r => [...r.querySelectorAll('td')].map(t => t.textContent.trim())));
  check('采集结果表已渲染', syncRows.length >= 5, `${syncRows.length} 项`);

  const byCap = Object.fromEntries(syncRows.map(r => [r[0], r[2]]));
  check('account 采集成功', byCap['account'] === 'ok', byCap['account']);
  check('groups 采集成功', byCap['groups'] === 'ok', byCap['groups']);
  check('keys 采集成功', byCap['keys'] === 'ok', byCap['keys']);
  check('model_catalog 采集成功', byCap['model_catalog'] === 'ok',
    byCap['model_catalog']);
  await page.screenshot({ path: `${SHOT}/05-sync.png`, fullPage: true });

  // ── 8. 查看分组、目录、Key ──
  //
  // 三个 Tab 按钮换成了分段控件（渠道详情现在有四个页签：账号 / Key /
  // 分组 / 模型目录）。按 data-seg 取，不按 id。
  await page.click('[data-seg="groups"]');
  await page.waitForFunction(
    () => /可用模型/.test(document.querySelector('#detail-body')?.textContent || ''),
    { timeout: 8000 });
  const groupRows = await page.$$eval('#detail-body tbody tr', rs => rs.length);
  // 真上游的分组数由人家怎么配决定,不写死。至少一个,否则采集没拿到东西。
  check('分组列表已渲染', groupRows >= 1, `${groupRows} 个分组`);

  // 点开某分组的可用模型（FR-124："这把 Key 能用哪些模型"）。
  //
  // ⚠️ **必须挑非空分组，不能点第一个**。真站点常有空分组（实测redacted-channel-03的
  // `auto` 就是 0 个模型，那是上游的真实配置而非采集失败），而分组顺序由上游
  // 返回决定、逐轮会变。原先直接 click 第一个 button[data-g]，于是这条断言
  // 按上游那一轮的排序随机红 —— 2026-08-29 实测同一天两轮：一轮点到「测试」
  // (163 个) 绿，一轮点到 `auto` (0 个) 红，而功能两轮都是好的。
  //
  // 但"全部分组都空"仍须红：那才是采集真没拿到东西。故先数一遍。
  const groupCounts = await page.$$eval('#detail-body button[data-g]',
    bs => bs.map(b => ({
      name: b.dataset.name,
      n: Number(/^(\d+)/.exec(b.innerText.trim())?.[1] ?? 0),
    })));
  const nonEmpty = groupCounts.filter(g => g.n > 0);
  check('至少一个分组解析出了可用模型（否则是采集没拿到）',
    nonEmpty.length >= 1,
    `${nonEmpty.length}/${groupCounts.length} 个分组非空` +
    (nonEmpty[0] ? `，最大 ${Math.max(...nonEmpty.map(g => g.n))} 个模型` : ''));

  await page.evaluate(() => {
    const b = [...document.querySelectorAll('#detail-body button[data-g]')]
      .find(x => Number(/^(\d+)/.exec(x.innerText.trim())?.[1] ?? 0) > 0)
      ?? document.querySelector('#detail-body button[data-g]');
    b.click();
  });
  await page.waitForFunction(
    () => /可用模型/.test(document.querySelector('#gm')?.textContent || ''),
    { timeout: 8000 });
  const gmText = await page.$eval('#gm', el => el.textContent);
  // 不找某个具体模型名 —— 真上游的目录随人家上下架而变。验"可用模型(n)"
  // 里的 n 为正,即这个分组真的解析出了模型清单。
  const gmCount = Number(/可用模型\s*[（(](\d+)[）)]/.exec(gmText)?.[1] ?? 0);
  check('分组可用模型可查（FR-124）', gmCount > 0,
    gmText.replace(/\s+/g, ' ').slice(0, 80));
  // 标题必须给**上游分组名**：内部 id 逐次采集会变，"分组 2"对不上上游的 vip。
  // 组名由上游定,只断言"分组"后面跟了个非空且不是纯数字的名字。
  check('分组可用模型标出上游分组名',
    /分组\s+\S+/.test(gmText) && !/分组\s+\d+\s/.test(gmText),
    gmText.replace(/\s+/g, ' ').slice(0, 40));

  await page.screenshot({ path: `${SHOT}/06-groups.png` });

  await page.click('[data-seg="catalog"]');
  await page.waitForFunction(
    () => /输入价/.test(document.querySelector('#detail-body')?.textContent || ''),
    { timeout: 8000 });
  const catCells = await page.$$eval('#detail-body tbody tr',
    rs => rs.map(r => [...r.querySelectorAll('td')].map(t => t.textContent.trim())));
  // 首页最多 50 行(CAT_PAGE),上游有 1000+ 模型时不可能等于总数 ——
  // 断言"渲染了一整页,且不超过上游实际模型数",不写死某个数字。
  check('模型目录已渲染',
    catCells.length > 0 && catCells.length <= Math.min(50, UP_MODELS),
    `${catCells.length} 行 / 上游共 ${UP_MODELS} 个模型`);

  // 价格必须与口径同格显示：按次模型的绝对美元价与倍率的数值区间**重叠**,
  // 光看数字分不出 "$3.5/次" 和 "倍率 3.5"。
  // 按模型名找具体某个模型是行不通的 —— 真上游的目录随人家上下架而变。
  // 改成按**口径标记**找:上游声明了按次模型,界面就必须有行标着 /次。
  const ratioRows = catCells.filter(c => c[1].includes('×倍率'));
  check('倍率模型标出 ×倍率',
    ratioRows.length > 0, `${ratioRows.length} 行标了 ×倍率`);

  // 排序必须**先分段再比价**。模型多的站首页 50 行往往全是倍率段(实测redacted-channel-03
  // 1369 个模型里 208 个按次),所以改用分段按钮切到按次段验 ——
  // 与真库那份(verify-remote.mjs)同路。
  await page.click('#detail-body button[data-unit="per_call"]');
  await page.waitForFunction(
    () => document.querySelector('#detail-body tbody tr')?.innerText.includes('/次'),
    { timeout: 15000 }).catch(() => {});
  const callSeg = await page.$$eval('#detail-body tbody tr',
    rs => rs.map(r => [...r.querySelectorAll('td')].map(t => t.textContent.trim())));
  check('按次计价模型标出 /次（FR-124）',
    callSeg.length > 0 && callSeg.every(c => c[1].includes('/次')),
    `按次段 ${callSeg.length} 行（上游声明 ${UP_PER_CALL} 个按次模型）`);
  check('按次分段内不混入倍率行',
    !callSeg.some(c => c[1].includes('×倍率')),
    callSeg.some(c => c[1].includes('×倍率')) ? '混入了 ×倍率' : '该段只有按次');
  // 切回全部,后面的断言依赖默认视图
  await page.click('#detail-body button[data-unit=""]');
  await page.waitForFunction(
    () => document.querySelector('#detail-body tbody tr')?.innerText.includes('×倍率'),
    { timeout: 15000 }).catch(() => {});
  await page.screenshot({ path: `${SHOT}/07-catalog.png` });

  await page.click('[data-seg="keys"]');
  // ⚠️ 等"表头出现 Key 专有列"而非"存在 table"：切视图时上一个视图的表格
  // 仍在 DOM 里，只等 table 会读到目录数据（本脚本首版就这样误报了两项）
  await page.waitForFunction(
    () => /Key 前缀/.test(document.querySelector('#detail-body')?.textContent || ''),
    { timeout: 8000 });
  // 单元格按 **data-col 语义名** 取，不按下标。
  // 原先用的是 c[2]（前缀）、slice(4,6)（分组/倍率）、c[6]（额度）——
  // 列顺序一改就全错位，而错位后断言照样绿，只是从此验的是别的列。
  // 这次改版正好证明了这一点：加一个"所属渠道"列就会让三处同时静默失准。
  const readKeyRows = () => page.$$eval('#detail-body tbody tr[data-key-row]',
    rs => rs.map(r => {
      const o = { id: r.getAttribute('data-key-row') };
      for (const td of r.querySelectorAll('td[data-col]')) {
        o[td.dataset.col] = td.textContent.replace(/\s+/g, ' ').trim();
      }
      return o;
    }));
  const keyCells = await readKeyRows();
  check('Key 列表已渲染', keyCells.length >= 4, `${keyCells.length} 把`);
  // 列表只显示前缀
  const prefixOK = keyCells.every(c => c.prefix.includes('…'));
  check('Key 列表只显示前缀', prefixOK, keyCells.map(c => c.prefix).join(' '));

  const groupData = await page.evaluate(async cid => {
    const token = document.querySelector('#token').value;
    const resp = await fetch(`/admin/channel-groups?channel_id=${cid}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    return (await resp.json()).items ?? [];
  }, newChannelId);
  check('Key 可选择至少两个上游分组（AC-37）', groupData.length >= 2,
    `${groupData.length} 个分组`);
  for (let i = 0; i < keyCells.length && groupData.length >= 2; i++) {
    const keyID = keyCells[i].id;
    const group = groupData[i % 2];
    await page.click(`[data-key-edit="${keyID}"]`);
    await page.waitForSelector(`#key-edit-group-${keyID}`, { timeout: 5000 });
    await page.select(`#key-edit-group-${keyID}`, String(group.id));
    await page.click(`[data-key-save="${keyID}"]`);
    await page.waitForFunction(
      (id, ref) => document.querySelector(`[data-key-row="${id}"] td[data-col="group"]`)
        ?.textContent.includes(ref),
      { timeout: 8000 }, keyID, group.group_ref);
  }
  const groupedKeys = await readKeyRows();
  check('四把 Key 均显示分组与倍率（AC-37）',
    groupedKeys.length >= 4
      && groupedKeys.every(c => c.group !== '—' && c.rate !== '未知'),
    JSON.stringify(groupedKeys.map(c => [c.group, c.rate])));

  // 采集后应有剩余配额（归一为美元）。
  //
  // ⚠️ 判据是 quotaView 的四态之一，不是"以 $ 开头"。无限额度的 NewAPI Key
  // 上游回的是 `remain_quota: 0`，旧版把它渲染成 $0.0000 —— 与"额度耗尽"
  // 完全无法区分，而两者对"能不能承接请求"是相反的结论（FR-025）。
  // 现在无限额度渲染成「不限额度」，所以这条改成"每一行都落在四态里，
  // 且至少有一行给出了真实金额或明确的不限额度"。
  const quotaCells = keyCells.map(c => c.quota);
  const quotaTyped = quotaCells.every(t =>
    /\$/.test(t) || /不限额度/.test(t) || /配额耗尽/.test(t) || /未采集/.test(t));
  const quotaInformative = quotaCells.some(t => /\$/.test(t) || /不限额度/.test(t));
  check('Key 剩余配额已归一为美元显示，且四态可区分',
    quotaTyped && quotaInformative, quotaCells.join(' | '));

  // 配额与账号余额必须是**两个列**，不能共用一个"额度"字样。
  const keyHeaders = await page.$$eval('#detail-body thead th',
    ts => ts.map(t => t.textContent.trim()));
  check('Key 表列名写全为「Key 剩余配额」（不是笼统的"额度"）',
    keyHeaders.includes('Key 剩余配额'), keyHeaders.join(' | '));
  // FR-127「登记与展示」的展示端。**不能断言"有数字"**—— 实测 newapi 的
  // /api/token 不给 Key 级 RPM/并发，真站点就是「—」。断言有数字就只能靠 mock
  // 才绿，那正是 CLAUDE.md §1 禁的。
  // 比对时**剥掉全部空白**：模板里 " / " 分隔符独占一行，Vue 的 whitespace
  // condense 会把它周围的换行缩掉，渲染出来到底是 "60 rpm / 3 并发" 还是
  // "60 rpm/3 并发" 取决于编译器行为而非我们的意图。而 newapi 不报 Key 级限流，
  // 「两个都有」这一支在真站点上跑不到 —— 断言若绑死空格，就会在某个家族
  // 真的两个都报时才炸，而那时没人记得是空格的事。数字、单位、分隔符、
  // 有无值这些**真内容**剥空白后一个不少。
  const squash = s => s.replace(/\s+/g, '');
  const rlCells = await page.$$eval('#detail-body tbody tr[data-key-row] td[data-rl]',
    ts => ts.map(t => t.textContent.trim()));
  check('Key 上游限流列已渲染（FR-127 展示端）',
    rlCells.length === keyCells.length && rlCells.every(t => t.length > 0),
    `${rlCells.length}/${keyCells.length} 行：${rlCells.join(' | ')}`);
  const rlFromAPI = await page.evaluate(async cid => {
    const t = document.querySelector('#token').value;
    const r = await fetch(`/admin/keys?channel_id=${cid}`,
      { headers: { Authorization: `Bearer ${t}` } });
    const d = await r.json();
    return (d.items ?? []).map(k => [k.rpm_limit ?? null, k.concurrency_limit ?? null]);
  }, newChannelId);
  // 由 API 字段**构造**期望串再逐行比对，而不是分「有值/无值」两条断言路径 ——
  // 分路径的那版里，"有值"那支在 newapi 上永不执行（pick-upstream 只选 newapi 系，
  // 而它不报 Key 级限流），等于一半断言从没跑过。构造式只有一条路径，
  // 两种数据都覆盖，且更强：无值时不只验"是 —"，验的是"恰好是 API 说的那个"。
  // 与 KeysView.vue 的模板同构：rpm→"N rpm"、并发→"N 并发"、都有→中间 " / "、
  // 都无→"—"。DOM 侧已折叠连续空白，所以这里也用单空格拼。
  const rlWant = rlFromAPI.map(([r, c]) => {
    const parts = [];
    if (r !== null) parts.push(`${r} rpm`);
    if (c !== null) parts.push(`${c} 并发`);
    return parts.length === 0 ? '—' : parts.join(' / ');
  });
  check('限流列逐行等于 API 字段构造出的串（不是写死、也不是错位）',
    rlCells.length === rlWant.length
      && rlCells.every((t, i) => squash(t) === squash(rlWant[i])),
    `期望=${JSON.stringify(rlWant)} 实际=${JSON.stringify(rlCells)}`);

  // 删除收进了行内「更多」菜单，并走统一确认框 ——
  // 原来它与「编辑」并排且是 window.confirm，两个按钮看起来一样轻，
  // 而后果差着一个数量级。
  const deletedKeyID = keyCells[keyCells.length - 1].id;
  await openMore(`[data-key-more="${deletedKeyID}"]`);
  await page.click(`[data-key-delete="${deletedKeyID}"]`);
  const delImpact = await page.$eval('.confirm-i', el => el.textContent.trim());
  check('删除 Key 的确认框写明不可恢复与归属',
    /不可恢复|永久删除/.test(delImpact), delImpact.slice(0, 70));
  await confirmOK();
  await page.waitForFunction(
    id => !document.querySelector(`[data-key-row="${id}"]`),
    { timeout: 8000 }, deletedKeyID);
  check('Key 编辑与删除可用（AC-37）', true);

  await page.screenshot({ path: `${SHOT}/08-keys.png`, fullPage: true });

  // ── 8bis. Key 管理页：平铺 / 分组双视图 + 筛选互通 ──
  //
  // 两种视图解决两类任务（跨渠道排查 vs 理解归属），所以都要有。
  // 关键断言是**切视图不丢筛选** —— 各自实现筛选的话，用户切一下就发现
  // "少了几把"，而那时分不清是筛选变了还是数据变了。
  await pane('keys');
  await page.waitForFunction(
    () => document.querySelectorAll('#pane-keys tr[data-key-row]').length > 0,
    { timeout: 8000 });
  const allKeysFlat = await page.$$eval('#pane-keys tr[data-key-row]', rs => rs.length);
  check('Key 管理页默认平铺列出全部渠道的 Key', allKeysFlat >= 3, `${allKeysFlat} 把`);

  // 按渠道筛选后，剩下的必须**都属于**这个渠道 —— 不是"数量对得上"。
  await page.select('#key-f-channel', String(newChannelId));
  await sleep(400);
  const filteredRows = await page.$$eval('#pane-keys tr[data-key-row]',
    rs => rs.map(r => r.querySelector('td[data-col="channel"]')?.textContent.trim()));
  check('按渠道筛选后每一行都属于该渠道',
    filteredRows.length > 0 && new Set(filteredRows).size === 1,
    `${filteredRows.length} 行，渠道取值 ${JSON.stringify([...new Set(filteredRows)])}`);

  // 筛选条件必须落进 URL：刷新 / 回退 / 分享链接看到的是同一批。
  const urlHasFilter = await page.evaluate(() =>
    new URLSearchParams(location.search).get('channel'));
  check('筛选条件写入 URL（刷新与分享可复现）',
    urlHasFilter === String(newChannelId),
    `?channel=${urlHasFilter}`);

  // 切到分组视图：筛选保留，且分组头写「匹配 N / 全部 M」。
  await page.click('[data-seg="by-channel"]');
  await page.waitForFunction(
    () => document.querySelector('#pane-keys .group-h') !== null, { timeout: 5000 });
  const groupedCount = await page.$$eval('#pane-keys tr[data-key-row]', rs => rs.length);
  check('切换视图不丢筛选（平铺与分组行数一致）',
    groupedCount === filteredRows.length,
    `平铺 ${filteredRows.length} 行 / 分组 ${groupedCount} 行`);
  const bucketLabel = await page.$eval(`[data-bucket-count="${newChannelId}"]`,
    el => el.textContent.replace(/\s+/g, ' ').trim());
  check('分组头写明「匹配 N / 全部 M」（不让人以为 Key 丢了）',
    /匹配\s*\d+\s*\/\s*全部\s*\d+/.test(bucketLabel), bucketLabel);

  await page.click('#btn-key-reset');
  await sleep(300);
  const afterReset = await page.$$eval('#pane-keys tr[data-key-row]', rs => rs.length);
  check('重置筛选恢复全量', afterReset === groupedCount || afterReset > groupedCount,
    `重置后 ${afterReset} 行（筛选时 ${groupedCount} 行）`);
  await page.screenshot({ path: `${SHOT}/09-keys-grouped.png`, fullPage: true });

  // ── 8ter. 账号页：余额三件事同格（金额 / 状态 / 确认时刻）──
  //
  // 采集刚跑过一轮，所以这个渠道的账号**应该**已经有余额了。
  // 但断言不写死金额 —— 上游余额是人家的钱，随时在变。断的是形态：
  // 要么给出金额与确认时刻，要么明说"未采集"，不能是空白也不能是裸 0。
  await pane('accounts');
  await page.waitForFunction(
    () => document.querySelectorAll('#pane-accounts tr[data-account-row]').length > 0,
    { timeout: 8000 });
  const balCells = await page.$$eval('#pane-accounts td[data-col="balance"]',
    ts => ts.map(t => t.textContent.replace(/\s+/g, ' ').trim()));
  check('账号余额列非空且区分「未采集」与金额',
    balCells.length > 0
      && balCells.every(t => t.length > 0)
      && balCells.every(t => /未采集/.test(t) || /\$\d/.test(t)),
    balCells.join(' | '));
  // 采到金额的行必须**同时**给出确认时刻 —— 金额离开时刻就没有意义
  // （三天前的 $0.42 和五分钟前的 $0.42 是不同的结论，FR-020/026）。
  const withAmount = balCells.filter(t => /\$\d/.test(t));
  check('有金额的账号同时显示确认时刻（FR-020）',
    withAmount.length === 0 || withAmount.every(t => /确认/.test(t)),
    withAmount.length === 0 ? '本轮无账号采到余额（站型可能不提供）' : withAmount.join(' | '));
  await page.screenshot({ path: `${SHOT}/10-accounts.png`, fullPage: true });
  await pane('detail');

  // ── 10. 限流：立刻再点一次采集应被拒（429，且不打上游）──
  await page.click('#btn-sync');
  await sleep(2500);
  const rateText = await page.$eval('#toast', el => el.textContent);
  check('60s 内重复采集被限流拒绝（09 §5.0bis）',
    /间隔未到|已有 sync/.test(rateText), rateText.slice(0, 70));

  // ── 11. 左右布局：侧栏与主区必须真的并排，不是上下堆叠 ──
  const layout = await page.evaluate(() => {
    const s = document.querySelector('.sidebar').getBoundingClientRect();
    const m = document.querySelector('.main').getBoundingClientRect();
    return { sx: s.x, sw: s.width, mx: m.x, sTop: s.y, mTop: m.y,
             navs: document.querySelectorAll('.nav-item').length };
  });
  check('侧栏在主区左侧（真左右布局）',
    layout.sx < layout.mx && layout.mx >= layout.sw,
    `侧栏 x=${layout.sx} 宽=${layout.sw}，主区 x=${layout.mx}`);
  check('侧栏与主区顶部对齐（未折成上下堆叠）',
    Math.abs(layout.sTop - layout.mTop) < 2,
    `侧栏 top=${layout.sTop}，主区 top=${layout.mTop}`);
  // 六项：渠道管理 / 渠道详情 / 账号管理 / Key 管理 / 采集凭证 / 批量导入。
  // 原来是五项 ——「账号与 Key」一个分栏管两个对象，拆成两个之后
  // 「在哪登记、在哪管理」不再是两个地方。
  check('侧栏导航项齐全', layout.navs === 6, `${layout.navs} 项`);

  // ── 12. 浅色 / 深色双模式 ──
  //
  // 断言背景亮度而不是 class 名：只看 classList 的话，把 .dark 里的
  // 色值写成白的也照样绿。深/浅两次读数必须真的分处两端。
  await page.click('#theme-sw button[data-theme="dark"]');
  await sleep(250);
  const darkLuma = await bgLuma();
  const darkOn = await page.evaluate(() =>
    document.documentElement.classList.contains('dark'));
  check('深色模式生效且背景确为深色', darkOn && darkLuma < 60,
    `class=dark:${darkOn} 背景亮度=${darkLuma}`);
  await page.screenshot({ path: `${SHOT}/09-theme-dark.png`, fullPage: true });

  await page.click('#theme-sw button[data-theme="light"]');
  await sleep(250);
  const lightLuma = await bgLuma();
  const lightOff = await page.evaluate(() =>
    !document.documentElement.classList.contains('dark'));
  check('浅色模式生效且背景确为浅色', lightOff && lightLuma > 200,
    `class=dark:${!lightOff} 背景亮度=${lightLuma}`);
  await page.screenshot({ path: `${SHOT}/10-theme-light.png`, fullPage: true });

  // 主题必须跨刷新记住：运维每次开界面都被打回默认色，等于没做
  await page.click('#theme-sw button[data-theme="dark"]');
  await sleep(150);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await sleep(400);
  const persisted = await page.evaluate(() => ({
    dark: document.documentElement.classList.contains('dark'),
    stored: localStorage.getItem('theme'),
    pressed: document.querySelector('#theme-sw button[aria-pressed="true"]')
      ?.dataset.theme,
  }));
  check('主题选择跨刷新保留且按钮态一致',
    persisted.dark && persisted.stored === 'dark' && persisted.pressed === 'dark',
    JSON.stringify(persisted));

  // 批量导入分栏可达（端点已实现，界面上要能找到入口）
  await pane('import');
  const impHas = await page.evaluate(() =>
    !!document.querySelector('#imp-file') && !!document.querySelector('#btn-imp-dry'));
  check('批量导入分栏可打开且有试运行入口', impHas);
  await page.screenshot({ path: `${SHOT}/11-import.png` });

  // ── 12bis. 真备份文件走一遍试运行 ──
  //
  // 默认跳过：这一段会**真的去探测备份里的上百个陌生站点**（实测 106 站 24 秒）。
  // 放进默认流程等于每跑一次验收就对第三方站点打一轮请求，既慢又不礼貌。
  // 要跑就显式给 HUB_FILE。
  //
  // 只跑 dry_run：真导入会往库里写上百个渠道，那是运维的决定，不是验收脚本的。
  if (HUB_FILE) {
    if (!existsSync(HUB_FILE)) {
      check('批量导入试运行（真备份文件）', false, `文件不存在：${HUB_FILE}`);
    } else {
      // 落库与否从界面自己的计数读，不另开一路 fetch：
      // 验的是"运维在界面上看到的数没变"，绕过界面去问后端就验不到这一点。
      const chCount = async () => {
        await pane('channels');
        await page.click('#btn-reload');
        await sleep(400);
        const t = await page.$eval('#ch-count', el => el.innerText);
        return Number((t.match(/\d+/) || [NaN])[0]);
      };
      const before = await chCount();
      await pane('import');
      const input = await page.$('#imp-file');
      await input.uploadFile(HUB_FILE);
      await page.click('#btn-imp-dry');
      // 探测上百个站点，给足超时；文案在 renderImport 里写死为"试运行结果（未落库）"
      await page.waitForFunction(
        () => /试运行结果/.test(document.querySelector('#imp-result')?.innerText || ''),
        { timeout: 300000 });

      const sum = await page.$eval('#imp-result .stats', el =>
        [...el.querySelectorAll('.stat')].map(s => ({
          n: Number(s.querySelector('b').innerText),
          k: s.querySelector('span').innerText,
        })));
      const val = k => (sum.find(s => s.k === k) || {}).n;
      // 条目数从文件本身数出来，不写死 106：写死的话换一份备份就会因为
      // "不是那 106 条"而失败，而这跟被验的行为毫无关系。
      // 是 accounts.accounts 而非顶层 accounts —— 外层那个还装着 bookmarks
      // 等别的东西（见 collector/allapihub.go 的 HubBackup）
      const accounts = JSON.parse(readFileSync(HUB_FILE, 'utf8'))
        .accounts?.accounts || [];
      check('试运行读出备份全部条目', val('备份条目') === accounts.length,
        `文件里 ${accounts.length} 条，界面显示 ${val('备份条目')} 条`);
      check('试运行区分可入库与跳过',
        val('可入库') > 0 && val('跳过') > 0 &&
        val('可入库') + val('跳过') + val('失败') === val('备份条目'),
        `可入库=${val('可入库')} 跳过=${val('跳过')} 失败=${val('失败')}`);

      // 站型声明不符必须**报出来而不是静默采信**：站型决定全部字段映射，
      // 信错一次余额/额度/模型全解析错，且错得没有任何报错。
      //
      // 断言"计数与表内标注一一对应"而不是"恰好 2 条"：不符的站数取决于备份内容
      // （这份实测 2 站：sub2api→newapi、new-api→sub2api），
      // 而"数出来几条就得在表里标出几条"对任何备份都必须成立。
      const mism = val('站型声明不符');
      const mismRows = await page.$$eval('#imp-result tbody tr', trs =>
        trs.filter(t => /声明 /.test(t.innerText))
           .map(t => t.innerText.replace(/\s+/g, ' ')));
      check('站型声明与探测不符的站点逐行标出（不静默采信导出声明）',
        mismRows.length === mism,
        `计数=${mism} 表内标出=${mismRows.length}：${mismRows.join(' / ').slice(0, 120)}`);
      // 计数为 0 时上面那条是空过 —— 说清楚，别让它冒充"验过了"
      if (!mism) console.log('   ⚠️ 这份备份里没有站型声明不符的站点，该性质本轮未被真正触发');

      // 有人机验证的站不能被当成"可自动采集"混进去。
      // 断言要落在**表格行**上：分栏末尾那段说明文字里本来就有"人机验证"四个字，
      // 对整个面板做正则等于自己给自己放行，把行渲染删掉照样绿。
      const shieldRows = await page.$$eval('#imp-result tbody tr', trs =>
        trs.filter(t => /人机验证/.test(t.innerText)).length);
      check('有人机验证的站点逐行标出需人工录入（04 §6）',
        shieldRows === val('有人机验证'),
        `计数=${val('有人机验证')} 表内标出=${shieldRows}`);
      if (!val('有人机验证')) console.log('   ⚠️ 这份备份里没有开人机验证的站点，该性质本轮未被真正触发');

      // 试运行的本分：一行都不许落库
      const after = await chCount();
      check('试运行不落库（渠道数不变）',
        Number.isFinite(before) && after === before,
        `试运行前 ${before} 个渠道，试运行后 ${after} 个`);
      await pane('import');
      await page.screenshot({ path: `${SHOT}/11b-import-dryrun.png`, fullPage: true });
    }
  }

  // ── 12bis. 渠道编辑与停用（PATCH /admin/channels/{id} 的界面入口）──
  //
  // 放在最后：编辑/停用会往 tbody 里插展开行，而前面若干断言按行数与行序取值。
  // 这里每步做完都收起展开行，最后一步把渠道恢复成 enabled。
  //
  // 每条都从**列表回显**读，不从 toast 读 —— toast 是我们自己写的文案，
  // 读它只能证明前端说了句话；列表是 patch 后重新 GET 回来的，读它才证明库里真变了。
  await pane('channels');
  await page.click('#btn-reload');
  await sleep(600);

  const renamed = `${uniq}-改名`;
  await page.click(`#channels button[data-ch-edit="${newChannelId}"]`);
  await page.waitForSelector('#ch-edit-name', { timeout: 5000 });
  await page.evaluate(() => { document.querySelector('#ch-edit-name').value = ''; });
  await page.type('#ch-edit-name', renamed);
  await page.click('#btn-ch-save');
  await page.waitForFunction(
    (id, want) => document.querySelector(`[data-ch-name="${id}"]`)?.textContent.trim() === want,
    { timeout: 8000 }, newChannelId, renamed).catch(() => {});
  const nameAfter = await page.$eval(`[data-ch-name="${newChannelId}"]`,
    el => el.textContent.trim());
  check('改渠道名后列表回显新名字（PATCH 落库）', nameAfter === renamed,
    `期望 ${renamed}，实际 ${nameAfter}`);

  // 停用：原因必填。先试空原因 —— 应被拦住且状态不变，否则"必填"是句空话。
  await page.click(`#channels button[data-ch-disable="${newChannelId}"]`);
  await page.waitForSelector('#ch-dis-reason', { timeout: 5000 });
  await page.click('#btn-ch-disable-ok');
  await sleep(800);
  const statusAfterEmpty = await page.$eval(`[data-ch-status="${newChannelId}"]`,
    el => el.textContent.trim());
  check('停用不填原因被拦下且状态未变（FR-095）',
    /enabled/.test(statusAfterEmpty), `状态=${statusAfterEmpty}`);

  const reason = '验收脚本停用测试-站点余额耗尽';
  await page.type('#ch-dis-reason', reason);
  await page.click('#btn-ch-disable-ok');
  await page.waitForFunction(
    id => /disabled/.test(document.querySelector(`[data-ch-status="${id}"]`)?.textContent || ''),
    { timeout: 8000 }, newChannelId).catch(() => {});
  const statusAfter = await page.$eval(`[data-ch-status="${newChannelId}"]`,
    el => el.textContent.trim());
  check('停用后列表状态变为 disabled', /disabled/.test(statusAfter), statusAfter);
  // 原因必须显示出来 —— 停用是要人来解除的，看不到原因就无从判断能不能解
  const reasonShown = await page.$eval(`[data-ch-reason="${newChannelId}"]`,
    el => el.textContent.trim()).catch(() => '');
  check('停用原因在列表里可见（不是只写进了库）', reasonShown === reason,
    `期望 ${reason}，实际 ${reasonShown || '（没有这个元素）'}`);

  // **停用态下改名，原因不能被抹掉。** 这是 store.UpdateChannel 的三分支里最容易
  // 写错的一支：原先 disabled_reason 是无条件赋值，于是只带 name 的 PATCH 会把
  // 原因清成 NULL —— 留下一个"已停用但没人知道为什么"的渠道，而库里没有 CHECK
  // 拦这个状态。改成随 status 变更才动之后，这条断言守着它。
  const renamed2 = `${uniq}-停用中改名`;
  await page.click(`#channels button[data-ch-edit="${newChannelId}"]`);
  await page.waitForSelector('#ch-edit-name', { timeout: 5000 });
  await page.evaluate(() => { document.querySelector('#ch-edit-name').value = ''; });
  await page.type('#ch-edit-name', renamed2);
  await page.click('#btn-ch-save');
  await page.waitForFunction(
    (id, want) => document.querySelector(`[data-ch-name="${id}"]`)?.textContent.trim() === want,
    { timeout: 8000 }, newChannelId, renamed2).catch(() => {});
  const reasonKept = await page.$eval(`[data-ch-reason="${newChannelId}"]`,
    el => el.textContent.trim()).catch(() => '');
  const stillDisabled = await page.$eval(`[data-ch-status="${newChannelId}"]`,
    el => el.textContent.trim());
  check('停用态下只改名，停用原因与状态都不受影响',
    reasonKept === reason && /disabled/.test(stillDisabled),
    `原因=${reasonKept || '（被抹掉了）'} 状态=${stillDisabled}`);

  await page.click(`#channels button[data-ch-enable="${newChannelId}"]`);
  await page.waitForFunction(
    id => /enabled/.test(document.querySelector(`[data-ch-status="${id}"]`)?.textContent || ''),
    { timeout: 8000 }, newChannelId).catch(() => {});
  const backOn = await page.$eval(`[data-ch-status="${newChannelId}"]`,
    el => el.textContent.trim());
  // 启用后原因必须一并清掉：留着上次的原因，界面上就是"已启用"却带着停用理由
  const reasonGone = await page.$$eval(`[data-ch-reason="${newChannelId}"]`,
    els => els.length === 0);
  check('启用后状态恢复且停用原因被清空', /enabled/.test(backOn) && reasonGone,
    `状态=${backOn}，原因元素${reasonGone ? '已消失' : '仍在'}`);

  await page.screenshot({ path: `${SHOT}/12-channel-edit.png`, fullPage: true });

  // ── 13. 页面无 JS 错误 ──
  // 只看真正的脚本错误：429（限流）与 422（5bis 故意的缺凭证采集）都是
  // 本脚本自己触发的断言，favicon 404 是浏览器自动请求 —— 都不是页面缺陷
  const realErrors = consoleErrors.filter(e =>
    !/429|422|favicon/.test(e));
  check('页面无 JavaScript 错误', realErrors.length === 0,
    realErrors.slice(0, 2).join(' | ') || '无（已排除预期的 429/422 与 favicon）');

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
