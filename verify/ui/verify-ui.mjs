// P1 管理界面的真实浏览器验收。
//
// 用真 Chrome（非 headless shell 的 DOM dump）：点击、填表、等 XHR、截图，
// 验证的是"运维真能在 web 端加渠道商并采集"，而不是"HTML 里有那些字符串"。
import puppeteer from 'puppeteer-core';
import { writeFileSync, existsSync, readFileSync } from 'node:fs';

// 路径由 test-ui.sh 按平台探测后注入（macOS 在 .app 里、CI 在 PATH 上）
const CHROME = process.env.CHROME ||
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const BASE = process.env.BASE || 'http://127.0.0.1:18090';
const TOKEN = process.env.ADMIN_TOKEN || 'local-verify-token';
const SHOT = process.env.SHOTS || '/tmp/sla-ui-shots';
// all-api-hub 备份文件。批量导入试运行用它,同时它也是上游凭证的来源。
const HUB_FILE = process.env.HUB_FILE || '';

// 真上游。由 test-ui.sh 跑 verify/pick-upstream.mjs 现场探活后注入 ——
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
  console.error('缺 UP_URL / UP_TOKEN / UP_UID / UP_KEYREF —— 请通过 verify/test-ui.sh 运行');
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
  // fill()：选中渠道时会把渠道 ID 预填进登记表单（省手抄），
  // 此时 type() 是**追加**而不是覆盖 —— 会把 "12" 填成 "1212"。先清空。
  const fill = async (sel, val) => {
    await page.$eval(sel, el => { el.value = ''; });
    await page.type(sel, val);
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

  // ── 6. 界面登记凭证 ──
  await pane('creds');
  await fill('#cr-channel', String(newChannelId));
  // 真凭证:后面第 8 步的四能力采集要靠它去真上游取数据。
  await fill('#cr-token', UP_TOKEN);
  await fill('#cr-uid', UP_UID);
  await page.click('#btn-cred');
  await page.waitForFunction(
    () => /凭证已登记|登记凭证失败/.test(document.querySelector('#toast').textContent),
    { timeout: 10000 });
  const credToast = await page.$eval('#toast', el => el.textContent);
  check('界面登记采集凭证成功', /凭证已登记/.test(credToast),
    credToast.replace(/\n/g, ' | ').slice(0, 80));
  check('凭证类型判定为 newapi_access_token',
    /newapi_access_token/.test(credToast));

  // 凭证列表：只报"已存什么"，绝不回显内容
  const credRow = await page.$$eval('#cred-list tbody tr',
    rs => rs.map(r => [...r.querySelectorAll('td')].map(t => t.textContent.trim())));
  // 用真令牌验这条比用假令牌更有意义:泄露了就是泄露了真东西。
  check('凭证列表已渲染且不含令牌内容',
    credRow.length >= 1 && !JSON.stringify(credRow).includes(UP_TOKEN),
    credRow.length ? credRow[0].join(' / ') : '空');

  // ── 7. 界面登记账号与 Key，并验证明文不回显 ──
  await pane('register');
  await fill('#acc-channel', String(newChannelId));
  // 必须填**真的**上游用户 ID：SaveAccount 先按 external_user_id 匹配账号行，
  // 匹配不上才退回"该渠道只有一个账号就用它"。填个假 uid 一样能过，
  // 但过的是兜底分支 —— 匹配逻辑本身就没被验到。
  await fill('#acc-uid', UP_UID);
  await page.click('#btn-acc');
  await page.waitForFunction(
    () => /账号已创建/.test(document.querySelector('#toast').textContent),
    { timeout: 8000 });

  // 这把 Key 是**假的**,而且必须是假的 —— CLAUDE.md §1 允许的唯一例外:
  // 被造的东西本身就是测试输入。这里要验的是"明文不回显",拿真 Key 试等于
  // 把真凭证写进 DOM 快照和 CI 日志,失败时反而漏得更彻底。
  //
  // 由 test-ui.sh 用 UI_KEY_SECRET 传进来:P1 退出标准③ 要求明文在"响应/
  // 日志/抓包"里一处都不出现,而日志那一端只有 shell 侧看得到
  // (/tmp/sla-ui-core.log)。两边各写一份字面量必然哪天漂掉,故只留一处来源。
  const SECRET = process.env.UI_KEY_SECRET || 'sk-ui-secret-should-never-be-echoed-9f3a';
  await fill('#key-secret', SECRET);
  // external_ref 必须是上游 /api/token 里真实存在的 id：SaveKey 只 UPDATE
  // 不 INSERT,对不上就只计"未登记"异常项 —— keys 项仍报 ok,额度列却永远空。
  await fill('#key-ref', UP_KEYREF);
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
  //
  // 注意这是本次会话的**第二次**点采集（5bis 缺凭证失败过一次）。
  // 它能成功本身就是断言：前置失败没有起算最小间隔窗口。
  await pane('detail');
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

  await page.click('#btn-catalog');
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
  check('侧栏导航项齐全', layout.navs === 5, `${layout.navs} 项`);

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
