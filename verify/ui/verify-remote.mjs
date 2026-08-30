// 真实数据下的管理界面验收（只读）。
//
// 与 verify-ui.mjs 的分工：那一份对**探活选出的单个真上游**跑写路径（建渠道、
// 登记凭证、采集、限流），库是一次性的；这一份连内网真库（数十个真实渠道 /
// 数千条目录行，具体规模每轮由 pickTargets() 现量），只做读与试运行，
// 不往真库写测试行。
//
// 为什么这一份不能省：那一份只覆盖 1 个渠道，而真库有 65 个、口径混着
// per_1m_token 与 per_call，且渠道间的目录规模差两个数量级。表格布局把侧栏顶出
// 视口、口径跨段比较这类问题只在这个量级上才会露出来。
import puppeteer from 'puppeteer-core';
import { writeFileSync } from 'node:fs';

const CHROME = process.env.CHROME ||
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const BASE = process.env.BASE || 'http://127.0.0.1:18390';
const TOKEN = process.env.ADMIN_TOKEN || 'remote-verify-token';
const SHOT = process.env.SHOTS || '/tmp/sla-remote-shots';
// 口径混合 / 口径为 NULL 的渠道号**从库里查，不写死**。
//
// ⚠️ 写死过一次，2026-08-30 那轮采集就把它作废了：原注释说"channel 56 有 294 行
// 全无 billing_unit"，采集补齐后那 294 行已全部有口径（2 种），于是这条断言会去
// 一个口径已知的渠道上要求界面显示"口径未知" —— 它会红，而红的原因是靶子搬走了，
// 不是被测对象坏了。**采集会改变真库，所以靶子必须每轮重新找。**
//
// 环境变量仍可覆盖（调试单渠道时用），但默认值来自 pickTargets() 的实测查询。
let MIXED_CH = Number(process.env.MIXED_CH || 0);
let NULL_CH = Number(process.env.NULL_CH || 0);
// 靶子的**规模**同样每轮重新量：分段按钮上的行数原先写死 208/1161，那是
// channel 3 在 2026-08-29 的形态。它和渠道号是同一个问题的两半 —— 号搬走了
// 断言会红，号没搬但行数变了断言也会红，而两种红都不是被测对象坏了。
let MIXED_UNITS = {};   // { per_1m_token: 1161, per_call: 208 }
let MIXED_GROUPS = 0;   // 该渠道的分组数
let NULL_ROWS = 0;      // 空口径渠道的目录行数
let CAT_Q = '';         // 名称筛选用的关键词，取自 MIXED_CH 的非首段
let CH_TOTAL = 0;       // 渠道总数（含 1 行夹具残留）
let CRED_TOTAL = 0;     // 采集凭证条数
// 真库 2 把 Key 都挂在 channel 1 的账号下（upstream_keys 走 account_id 关联，
// 没有 channel_id 列）。FR-094「只显前缀」必须在有 Key 的渠道上验。
//
// ⚠️ 但 channel 1 是**夹具残留**，不是真渠道：名字 `UI验收-397444`（verify-ui.mjs
// :148 的 `'UI验收-' + Date.now()`），base_url 指向 127.0.0.1:18099 —— 已删除的
// mock_newapi.py 的端口，2026-08-28 那轮 verify-ui 打真库时留下的。它名下两把 Key
// 的明文是 verify-ui.mjs 的 SECRET 夹具（`sk-ui-secret…`），不是采来的。
// 实测真渠道采到的 Key 数为 0（`upstream_keys` join 到 channel<>1：0 行）。
//
// 所以下面三条 Key 断言（有行 / 不回显明文 / 前缀截断）**跑在夹具上**。它们仍然
// 有意义 —— 验的是渲染层不吐明文，与 Key 从哪来无关；但别把它们当作"真采集的 Key
// 也不回显"的证据。真正在真上游上验这条的是 verify-ui.mjs（那边 SECRET 是刻意的
// 假 Key，理由见 CLAUDE.md §1 例外表第一行）。
// 夹具行的处置未决（清掉它会让这三条无靶子），见 docs/acceptance/P1-evidence.md §3.4。
const KEY_CH = Number(process.env.KEY_CH || 1);

const results = [];
function check(name, ok, detail = '') {
  results.push({ name, ok, detail });
  console.log(`${ok ? '✅' : '❌'} ${name}${detail ? ' — ' + detail : ''}`);
  if (!ok) process.exitCode = 1;
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

// api 走管理 API 而不是 psql：这个脚本只依赖 puppeteer-core，不带 pg 客户端，
// 而靶子要的事实（每渠道目录规模、各口径行数、分组数）管理 API 全都给。
// 顺带的好处是靶子与被测界面读的是同一条链路 —— 若 API 本身坏了，
// 这里会先炸，而不是让下游断言报出一个误导性的"界面没渲染"。
async function api(path) {
  const r = await fetch(`${BASE}${path}`, {
    headers: { Authorization: `Bearer ${TOKEN}` },
  });
  if (!r.ok) throw new Error(`GET ${path} → ${r.status} ${await r.text()}`);
  return r.json();
}

// pickTargets 每轮从真库现状里挑靶子。见文件头 MIXED_CH/NULL_CH 处的 ⚠️。
//
// 挑法与断言想验的东西对齐，不是"随便找一个"：
//   MIXED_CH —— 口径 ≥2 种、行数最多的渠道。行数最多才撑得起"翻 24 页才够得着
//               按次段"这个原始缺陷的复现条件；口径只有 1 种的渠道上，
//               "跨段不可比"的告警和分段按钮都没有意义。
//   NULL_CH  —— 整表口径全 NULL 的渠道。**不能**用"含 NULL 行"的渠道：
//               ListCatalog 按 billing_unit 排序，NULL 排在最后，首页看到的
//               全是有口径的行，于是"未误标为倍率"必然红。
async function pickTargets() {
  const { items: chans } = await api('/admin/channels?limit=1000');
  const seen = [];
  for (const c of chans) {
    // limit=1 只是为了别把上千行搬回来：units 是**分段筛选前**统计的，
    // 与分页无关（upstream_api.go:filterCatalog 的注释写了这个顺序）。
    const cat = await api(`/admin/channels/${c.id}/catalog?limit=1`);
    const units = cat.units || {};
    const kinds = Object.keys(units).filter(u => u !== 'unknown');
    seen.push({ id: c.id, total: cat.total, units, kinds });
  }
  // 要的是 per_1m_token + per_call 这两种口径都有的渠道，不是"任意 ≥2 种"：
  // 下游 3 条断言分别找 `×倍率`、`/次`、以及"按次段内不混倍率"，
  // 换成别的口径组合它们会在一个语义上无关的段上红。
  const mixed = seen.filter(s =>
    s.kinds.includes('per_1m_token') && s.kinds.includes('per_call'))
    .sort((a, b) => b.total - a.total)[0];
  const nulls = seen.filter(s => s.total > 0 &&
    Object.keys(s.units).length === 1 && s.units.unknown === s.total)
    .sort((a, b) => b.total - a.total)[0];

  if (!mixed) {
    throw new Error('真库里没有同时含 per_1m_token 与 per_call 的渠道，' +
      '分段相关的 5 条断言无靶子可验');
  }
  // 空口径渠道消失是**好事**（015 迁移的存量被采集回填完了），但不能静默跳过：
  // 那 2 条断言会因为 NULL_CH=0 去点一个不存在的按钮，报成 TypeError。
  // 所以显式炸，并说清该怎么办 —— 结清 P1-evidence §3.3 那个缺口。
  if (!nulls) {
    throw new Error('真库已无整表口径全 NULL 的渠道：' +
      '「口径未知」那 2 条断言失去靶子。请确认 P1-evidence §3.3 的存量缺口是否已结清，' +
      '结清了就把这 2 条连同 NULL_CH 一起删掉，别留个空跑的壳');
  }

  CH_TOTAL = chans.length;
  CRED_TOTAL = (await api('/admin/collector/credentials?limit=1000')).items.length;
  MIXED_CH = MIXED_CH || mixed.id;
  NULL_CH = NULL_CH || nulls.id;
  MIXED_UNITS = seen.find(s => s.id === MIXED_CH)?.units || {};
  NULL_ROWS = seen.find(s => s.id === NULL_CH)?.total || 0;

  const { count } = await api(`/admin/channel-groups?channel_id=${MIXED_CH}`);
  MIXED_GROUPS = count;

  // 名称筛选的关键词取自**非首段**（字母序在后的那个口径）的模型名。
  // 原先写死 'flux' 靠的正是这个性质：它落在按次段，首页 50 行里没有，
  // 所以能守住"筛选走后端 q 参数"。关键词换渠道就得跟着换，故一并推导。
  const lastUnit = Object.keys(MIXED_UNITS).sort().pop();
  const seg = await api(
    `/admin/channels/${MIXED_CH}/catalog?unit=${encodeURIComponent(lastUnit)}&limit=1`);
  CAT_Q = seg.items?.[0]?.model_name || '';
  if (!CAT_Q) throw new Error(`渠道 ${MIXED_CH} 的 ${lastUnit} 段取不到模型名`);

  console.log(`靶子（本轮实测）: MIXED_CH=${MIXED_CH} ` +
    `${JSON.stringify(MIXED_UNITS)} 分组 ${MIXED_GROUPS} 个; ` +
    `NULL_CH=${NULL_CH} ${NULL_ROWS} 行全无口径; CAT_Q=${CAT_Q}`);
}
await pickTargets();

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
  // 行数与 API 的 total 对，不写死：CH_TOTAL 含 1 行夹具残留（channel 1，
  // 见上面 KEY_CH 处）。写死过 65，而导入/新建渠道都会改这个数。
  check(`真库 ${CH_TOTAL} 个渠道全部渲染`, rows.length === CH_TOTAL,
    `${rows.length} 行`);

  // 站型从**第 3 个单元格**取，且不预设候选名单。
  //
  // 上一版拿一张写死的名单（含两个库里并不存在的族）去 innerText.includes()，
  // 有两处失真：名单外的族数不出来（多一族 → 静默漏掉），而 includes 会把
  // 渠道名里出现的族名也算进去（"某站-newapi-备用"这种名字会重复计数）。
  // 现在数的是全部实际取值，断言比对整张分布表 —— 少一族、多一族、名字撞车
  // 都会红，且红的信息就是库里真实的分布。
  const families = await page.$$eval('#channels tbody tr[data-ch-row]', trs => {
    const c = {};
    trs.forEach(tr => {
      const fam = tr.querySelectorAll('td')[2].innerText.trim();
      c[fam] = (c[fam] || 0) + 1;
    });
    return c;
  });
  // 序列化前排键：对象的键序是插入序（= 表格行序），不排的话换个排序方式
  // 就会红，而那与"分布对不对"无关。
  const canon = o => JSON.stringify(Object.fromEntries(
    Object.entries(o).sort(([a], [b]) => a.localeCompare(b))));
  const wantFamilies = { newapi: 52, sub2api: 13 };
  check('站型分布与库一致（52 newapi + 13 sub2api）',
    canon(families) === canon(wantFamilies),
    `实际 ${canon(families)}，期望 ${canon(wantFamilies)}`);

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
  check(`清空筛选后恢复全部 ${CH_TOTAL} 行`, restored === CH_TOTAL, `${restored} 行`);

  // ── 3. 真实渠道详情：口径混合的那一个 ──
  await page.evaluate(id => {
    document.querySelector(`#channels button[data-ch="${id}"]`).click();
  }, MIXED_CH);
  await page.waitForFunction(
    () => document.querySelector('#pane-detail')?.classList.contains('on'),
    { timeout: 5000 });
  // 等条件而非睡固定时长（同下方 FR-124 那条的教训）：总览是 fetch 回来才渲染的
  await page.waitForFunction(
    () => /账号|Key|分组|目录/.test(document.querySelector('#inv-stats')?.innerText ?? ''),
    { timeout: 20000 },
  ).catch(() => {});

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

  // 分段规模必须直接写在按钮上：不显示行数的话，运维无从知道自己看的是全表还是一段。
  // 期望值来自 pickTargets() 的实测 units（本轮 MIXED_UNITS），不写死。
  const segs = await page.$$eval('#detail-body button[data-unit]',
    bs => bs.map(b => ({ u: b.dataset.unit, t: b.innerText.replace(/\s+/g, ' ') })));
  const segMiss = Object.entries(MIXED_UNITS).filter(([u, n]) =>
    !segs.some(s => s.u === u && new RegExp(`\\b${n}\\b`).test(s.t)));
  check('口径分段按钮已渲染且各段行数与库一致',
    segMiss.length === 0 && segs.length >= Object.keys(MIXED_UNITS).length,
    segMiss.length ? `缺/错: ${segMiss.map(([u, n]) => `${u}=${n}`).join(',')}；` +
      `实际: ${segs.map(s => s.t).join(' | ') || '未见分段按钮'}`
      : segs.map(s => s.t).join(' | '));

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
  //
  // 等条件而不是等固定时长：真库这条「全部」查询实测 0.64～1.6 秒（1369 行、
  // 冷热差一倍），原先固定 sleep(700) 正好卡在这个区间上 —— 慢的那次读到的
  // 还是切换前的按次行，于是报一个并不存在的"分段不可逆"。
  await page.click('#detail-body button[data-unit=""]');
  await page.waitForFunction(
    () => document.querySelector('#detail-body tbody tr')?.innerText.includes('×倍率'),
    { timeout: 10000 }).catch(() => {});
  const backCells = await cells();
  check('切回「全部」后回到倍率段首页（分段切换可逆）',
    backCells.includes('×倍率'), backCells.slice(0, 60).replace(/\s+/g, ' '));

  await page.screenshot({ path: `${SHOT}/03-real-catalog.png`, fullPage: true });

  // ── 4ter. 名称筛选：1369 行里定位一个模型 ──
  // 真库单渠道 1369 行，靠翻页找模型不现实。这条同时守住"筛选走后端 q 参数"
  // ——若只在当前页 50 行里过滤，搜 flux 会一条都搜不到（它们在按次段）。
  // 先确认关键词**确实**不在首页 50 行里 —— 这是这条断言的前提。
  // 前提自己也要验：若关键词恰好落在首页，"筛选走后端"就退化成
  // "前端在当前页里过滤也能绿"，断言等于没验。
  check(`筛选关键词 ${CAT_Q} 不在首页行内（跨页前提成立）`,
    !first.includes(CAT_Q) && !backCells.includes(CAT_Q),
    first.includes(CAT_Q) ? '首页就有，这条筛选断言退化' : '首页无此行');
  await page.type('#cat-q', CAT_Q);
  await page.waitForFunction(
    q => (document.querySelector('#detail-body tbody')?.innerText || '').includes(q),
    { timeout: 10000 }, CAT_Q).catch(() => {});
  const qCells = await cells();
  const qRows = await page.$$eval('#detail-body tbody tr', trs => trs.length);
  check('按模型名筛选命中跨页的行（后端 q 参数生效）',
    qRows > 0 && qCells.includes(CAT_Q), `${qRows} 行`);
  // 焦点必须留在输入框里：重渲染换掉 input 而不搬回焦点的话，
  // 第二个字符就打到 body 上了，看着像"筛选只认一个字"
  const stillFocused = await page.evaluate(() => document.activeElement?.id);
  check('筛选框重渲染后焦点保留（可连续输入）',
    stillFocused === 'cat-q', `activeElement=${stillFocused}`);
  await page.screenshot({ path: `${SHOT}/03d-catalog-search.png`, fullPage: true });

  // ── 4bis. 口径全缺的渠道：必须显式标「口径未知」，不能替上游假定默认口径 ──
  //
  // 真库仍有 billing_unit 为 NULL 的存量行（015 迁移前的，等各渠道下次采集
  // 回填；2026-08-30 那轮采集把 1413 行降到了 50 行）。pickTargets() 挑一个
  // 整表全 NULL 的渠道来验：若界面悄悄按 per_1m_token 渲染，按次计价的
  // 绝对美元价会被当成倍率读，量级差一百万倍。
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
  // 等表体真出行，别睡（同 FR-124 那条）
  await page.waitForFunction(
    () => document.querySelectorAll('#detail-body tbody tr').length >= 1,
    { timeout: 20000 },
  ).catch(() => {});
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
  // 必须回到 MIXED_CH：真库里多数分组的可用模型数为 0（上游没给或没采到），
  // channel 1 的 default/vip/svip 恰好全是 0。在那种渠道上验"可用模型可查"，
  // 看到的 0 个模型 是真实的，但这条断言就什么都没验到。
  // MIXED_CH 是目录最大的渠道，它的分组是有内容的那种（实测 1354/1333/445/…）。
  await pane('channels');
  await page.evaluate(id => {
    document.querySelector(`#channels button[data-ch="${id}"]`).click();
  }, MIXED_CH);
  await page.waitForFunction(
    () => document.querySelector('#pane-detail')?.classList.contains('on'),
    { timeout: 5000 });
  await sleep(1200);
  await page.click('#btn-groups');
  // 等分组行真出来（数量来自 pickTargets 的实测查询），别睡固定时长
  await page.waitForFunction(
    n => document.querySelectorAll('#detail-body tbody tr').length >= n,
    { timeout: 20000 }, MIXED_GROUPS,
  ).catch(() => {});
  const grpRows = await page.$$eval('#detail-body tbody tr',
    trs => trs.map(tr => tr.innerText.replace(/\s+/g, ' ').trim()));
  // 断等号而不是 >=：少一行是"分组没渲染全"，多一行是"串了别的渠道的分组"，
  // 两种都是缺陷，>= 只拦得住前一种。
  check(`渠道 #${MIXED_CH} 分组列表已渲染（${MIXED_GROUPS} 个）`,
    grpRows.length === MIXED_GROUPS, `${grpRows.length} 个分组`);
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
    // ⚠️ 这里**必须等条件，不能睡固定时长**。真库这个分组有 1354 个模型，
    // 请求 236ms 就回来了，但把上千个模型名拼进一个文本节点再排版，实测耗时
    // 在 500ms~3000ms+ 之间飘（取决于当时机器负载）。原先睡 1500ms 就读，
    // 于是这条断言按机器快慢随机红 —— 实测三轮里红了两轮，而功能是好的。
    // 这类"睡一会儿再断言"的写法在慢的那一端必然假红，在快的那一端又掩盖
    // 真慢；等条件才两头都对。
    await page.waitForFunction(
      () => (document.querySelector('#gm')?.innerText ?? '').length > 20,
      { timeout: 20000 },
    ).catch(() => {});
    const gmText = await page.$eval('#gm', el => el.innerText).catch(() => '');
    check('分组可用模型可展开且标出上游分组名（FR-124）',
      gmText.length > 20 && /分组/.test(gmText),
      gmText.slice(0, 110).replace(/\n/g, ' '));
    await page.screenshot({ path: `${SHOT}/04b-group-models.png`, fullPage: true });
  } else {
    check('分组可用模型可展开且标出上游分组名（FR-124）', false,
      '未找到可点开的非零分组按钮');
  }

  // ── 7. 采集凭证：真库现有若干条，一条都不能回显内容 ──
  await pane('creds');
  await page.click('#btn-cred-reload');
  await page.waitForFunction(
    () => document.querySelectorAll('#cred-list tbody tr').length > 1,
    { timeout: 20000 });
  const creds = await page.$$eval('#cred-list tbody tr', trs =>
    trs.map(tr => [...tr.querySelectorAll('td')].map(td => td.innerText.trim())));
  check(`真库 ${CRED_TOTAL} 条采集凭证全部渲染`, creds.length === CRED_TOTAL,
    `${creds.length} 条`);
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
