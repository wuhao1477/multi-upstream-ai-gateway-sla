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
// ui-stack.sh 起的真 WebDAV 容器的坐标。没有它时整段 WebDAV 同步验收跳过。
const DAV_BASE = process.env.DAV_BASE || '';
const DAV_USER = process.env.DAV_USER || '';
const DAV_PASS = process.env.DAV_PASS || '';
const DAV_ENC_PASSWORD = process.env.DAV_ENC_PASSWORD || '';

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

  /**
   * 丢掉**本脚本故意触发的**那几条 401。
   *
   * 按「时间窗 + 状态码」双条件，不是全局放行 401：后者会把别处真的鉴权回归
   * 一起放过去，而那正是最后那条「页面无 JavaScript 错误」要抓的东西。
   * 用法：`const at = consoleErrors.length;` … 故意打 401 的那几步 … `drop401(at)`。
   */
  const drop401 = async at => {
    await sleep(300); // 控制台消息是异步到的，早清会漏掉最后一两条
    for (let i = consoleErrors.length - 1; i >= at; i--) {
      if (/401/.test(consoleErrors[i])) consoleErrors.splice(i, 1);
    }
  };

  // ── 布局改成左右分栏后新增的两个助手 ──
  //
  // pane()：表单散在不同分栏里，未激活的分栏是 display:none。
  // puppeteer 往隐藏元素 type 不会报错，但 focus() 是空操作 —— 按键会落到
  // 上一个焦点元素上，症状是"填了却没填进去"，且没有任何报错。故先切分栏。
  //
  // 顶层导航有两项（控制台 / 模型目录），侧栏只属于控制台那一侧 ——
  // 所以从模型目录切回任何一个控制台分栏，都得先点一下「控制台」，
  // 否则那个 .nav-item 根本不在 DOM 上（不是隐藏，是没渲染）。
  const pane = async name => {
    if (name === 'models') {
      await page.click('[data-top-nav="models"]');
    } else {
      if ((await page.$(`.nav-item[data-pane="${name}"]`)) === null) {
        await page.click('[data-top-nav="console"]');
        await page.waitForSelector(`.nav-item[data-pane="${name}"]`, { timeout: 5000 });
      }
      await page.click(`.nav-item[data-pane="${name}"]`);
    }
    await page.waitForFunction(
      n => document.querySelector('#pane-' + n)?.classList.contains('on'),
      { timeout: 5000 }, name);
  };
  const openDetail = async id => {
    await page.goto(`${BASE}/admin/ui/channels/${id}`, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(
      () => document.querySelector('#pane-detail')?.classList.contains('on') === true,
      { timeout: 8000 });
  };
  // fill()：选中渠道时会把渠道 ID 预填进登记表单。
  // 用真实键盘事件更新 v-model，避免直接改 DOM 与 Vue 状态不同步。
  const fill = async (sel, val) => {
    await page.click(sel);
    // 全选再删。原先是三击 + Backspace —— 三击在**长值**输入框里可能只选中一个
    // "词"：实测把 http://127.0.0.1:18192/ 里的 18192 单独选走，于是新值被插进
    // 旧值中间，拼出一个畸形地址（2026-09-13 WebDAV 同步验收红在这上面）。
    // input.select() 选的是整个值，没有歧义；打字仍走真实键盘事件，v-model 照常更新。
    await page.$eval(sel, el => el.select());
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
    // 等菜单**落位**再返回。Chrome 的 toggle 事件比 click 晚约一帧（实测 13ms），
    // 而定位就在 toggle 里做 —— 不等它，量到的是"还没落位"的静态位置，
    // 断言会红在一个早就修好的形态上。菜单收起时 placeMenus 会清掉 style，
    // 所以 style.left 非空确实等于"这一次已经落位"。
    await page.waitForFunction(s => {
      const m = document.querySelector(s)?.closest('details')?.querySelector('.more-menu');
      return m != null && m.style.left !== '';
    }, { timeout: 5000 }, sel);
  };
  // 渠道 / 账号选择器（ScopePicker）。它们不再是 <select>，所以 page.select
  // 用不了 —— 是"触发按钮 + 列表弹窗"：点按钮开弹窗、点行选中，多选还要点确定。
  // 单选点完一行弹窗自己关，多选不关（要让人接着挑）。
  const pick = async (id, ids, multi = false) => {
    await page.click(`#${id}`);
    await page.waitForSelector('.picker', { visible: true, timeout: 5000 });
    for (const v of ids) {
      await page.waitForSelector(`[data-pick-row="${v}"]`, { visible: true, timeout: 5000 });
      await page.click(`[data-pick-row="${v}"]`);
    }
    if (multi) await page.click('[data-pick-ok]');
    await page.waitForFunction(
      () => document.querySelector('.picker') === null, { timeout: 5000 });
  };
  // 选择器按钮上现在显示的是什么（未选时是占位符）。
  const picked = sel => page.$eval(`${sel} .picker-sum`, el => el.textContent.trim());
  // 「更多」菜单到底看不看得见：命中测试而不是量尺寸。
  // 量 getBoundingClientRect 只能证明"它有个矩形"，被 overflow 裁掉的元素
  // 矩形照样在 —— 那正是这个 bug 修复前的样子。elementFromPoint 打的是
  // 真实的合成结果：裁掉了、被盖住了、飘出视口了，都会落空。
  const menuVisible = sel => page.evaluate(s => {
    const menu = document.querySelector(s)?.closest('details')?.querySelector('.more-menu');
    if (menu === null || menu === undefined) return { ok: false, why: '菜单不在 DOM 上' };
    const r = menu.getBoundingClientRect();
    const x = Math.round(r.left + r.width / 2), y = Math.round(r.top + r.height / 2);
    const inView = r.top >= 0 && r.left >= 0
      && r.bottom <= window.innerHeight && r.right <= window.innerWidth;
    const hit = document.elementFromPoint(x, y);
    return {
      ok: inView && menu.contains(hit),
      why: `rect=${JSON.stringify(r.toJSON())} 视口=${window.innerWidth}x${window.innerHeight}`
        + ` 命中=${hit === null ? 'null' : hit.className || hit.tagName}`,
    };
  }, sel);
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
    // 连 URL 一起记：只有状态码的话，想放行一条故意打出来的失败就只能按
    // "429|422" 这种状态码放行，而那会把**真的** 429/422 一并放行。
    if (m.type() === 'error') consoleErrors.push(`${m.text()} @${m.location()?.url ?? ''}`);
  });
  page.on('pageerror', e => consoleErrors.push('pageerror: ' + e.message));
  page.on('dialog', dialog => dialog.accept());

  // ── 1. 打开管理界面 ──
  const resp = await page.goto(`${BASE}/admin/ui`, { waitUntil: 'domcontentloaded' });
  check('打开 /admin/ui 返回 200', resp.status() === 200, `HTTP ${resp.status()}`);

  const title = await page.title();
  check('页面标题正确', title.includes('上游渠道采集与管理'), title);

  // ── 2. 没有令牌时落到登录页，且**带着原本要去的地址** ──
  //
  // 这一条替代了旧的「无令牌时拒绝拉取数据」：那时候没令牌也能进控制台，
  // 于是每一栏各弹一个失败 toast。现在压根进不去。
  //
  // 挑一个**深一点的**地址来验 next：用根路径的话，"带没带 next" 与
  // "没带也正好落在根上" 是同一个结果，这条就等于没验。
  await page.evaluate(() => localStorage.clear());
  await page.goto(`${BASE}/admin/ui/keys`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#pane-login', { timeout: 8000 });
  const loginLanding = await page.evaluate(() => ({
    url: location.pathname + location.search,
    hasToken: document.querySelector('#token') !== null,
    hasSubmit: document.querySelector('#login-submit') !== null,
    // 登录页不该有导航：还没登录，点了也走不到
    hasHeader: document.querySelector('.hdr') !== null,
    hasSidebar: document.querySelector('.sidebar') !== null,
    // 登录页只要一个字段（P1 没有平台用户实体，所以没有用户名）
    inputs: [...document.querySelectorAll('.login-box input')].map(i => i.id),
  }));
  check('没有令牌时自动落到登录页，并记住原本要去的地址',
    loginLanding.url === '/admin/ui/login?next=/keys' &&
    loginLanding.hasToken && loginLanding.hasSubmit &&
    !loginLanding.hasHeader && !loginLanding.hasSidebar &&
    JSON.stringify(loginLanding.inputs) === JSON.stringify(['token']),
    JSON.stringify(loginLanding));

  await page.screenshot({ path: `${SHOT}/00-login.png` });

  // 错令牌当场说清楚，而不是放进去再被弹回来。
  // 这一步会打一个真的 401（就是它该打的），按窗口清掉，见 drop401。
  const at401Login = consoleErrors.length;
  await page.type('#token', 'definitely-not-the-admin-token');
  await page.click('#login-submit');
  await page.waitForSelector('#login-error', { timeout: 8000 });
  const badLogin = await page.evaluate(() => ({
    msg: document.querySelector('#login-error')?.textContent.trim() ?? '',
    url: location.pathname,
    // 错令牌**不能留在本地**：留着的话守卫会认为"有令牌"并放行，
    // 于是每进一页都是先加载、再 401、再弹回来 —— 一个看不见出口的循环。
    stored: localStorage.getItem('adminToken') ?? '',
  }));
  check('令牌不对时留在登录页、说明原因，且不把错令牌存下来',
    /令牌不对/.test(badLogin.msg) && badLogin.url === '/admin/ui/login' &&
    badLogin.stored === '',
    JSON.stringify(badLogin));
  await drop401(at401Login);

  // ── 3. 登录并加载渠道列表 ──
  //
  // ⚠️ 必须容忍**空库**：脚本化验收从干净的库起，此时列表是空状态而非表格。
  // 首版只等 `#channels table`，在空库上必然超时 —— 我的手工验证之所以过，
  // 是因为库里残留着先前手点建的渠道。空库才是"运维第一次打开界面"的真实情形。
  await page.evaluate(() => { document.querySelector('#token').value = ''; });
  await page.click('#token', { clickCount: 3 });
  await page.type('#token', TOKEN);
  await page.click('#login-submit');
  // 登录后回到原本要去的那一页（?next=/keys），不是回根路径
  await page.waitForFunction(
    () => document.querySelector('#pane-keys')?.classList.contains('on') === true,
    { timeout: 10000 });
  const afterLogin = await page.evaluate(() => ({
    url: location.pathname,
    stored: (localStorage.getItem('adminToken') ?? '') !== '',
  }));
  check('令牌正确时进入原本要去的那一页，并持久化到浏览器',
    afterLogin.url === '/admin/ui/keys' && afterLogin.stored,
    JSON.stringify(afterLogin));
  await pane('channels');
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

  // 顶栏（品牌那一块）显示**正在跑的这个二进制**的版本。
  //
  // 三端逐字比对：`-ldflags` 注入的值 → `/admin/version` → DOM。
  // 少任何一端这条都会退化 —— 只比 DOM 与 API 的话，不注入版本时两边都是
  // main.version 的零值 "dev"，此时把界面上的版本号写死成 "dev" 照样绿；
  // 而这条要防的正是"写死"与"编进前端构建期"这两种答非所问的实现
  // （界面要回答的是"这台在跑哪一版"）。CORE_VERSION 由 ui-stack.sh 注入，
  // 每次跑都不同。
  const verWant = process.env.CORE_VERSION || '';
  const verFromAPI = await page.evaluate(async () => {
    const t = localStorage.getItem('adminToken');
    const r = await fetch('/admin/version', { headers: { Authorization: `Bearer ${t}` } });
    return (await r.json()).version ?? '';
  });
  const verInDOM = await page.waitForFunction(() => {
    const el = document.querySelector('[data-app-version]');
    return el && el.textContent.trim() !== '' ? el.textContent.trim() : null;
  }, { timeout: 8000 }).then(h => h.jsonValue(), () => '');
  check('顶栏显示正在运行的 sla-core 版本（取自后端，不是写死）',
    verWant !== '' && verFromAPI === verWant && verInDOM === verWant,
    `注入=${verWant} 后端=${verFromAPI} 界面=${verInDOM}`);

  // 站型下拉必须来自后端注册表（GET /admin/site-families），不是写死的四项。
  //
  // 逐项等值比对而不是数个数：数个数的话，把 v-for 删掉再写死四个 option
  // 照样绿 —— 而"写死"正是这条要防的东西（加站型时那份列表不报错，
  // 新站型只是在界面上不存在）。
  const famFromAPI = await page.evaluate(async () => {
    const t = localStorage.getItem('adminToken');
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
  check('创建时自动探测出站型 NewAPI 系', /NewAPI 系/.test(createToast));
  // 断言"读到了上游此刻真实声明的那个值",不是断言某个固定数字 ——
  // quota_per_unit 逐站不同,写死等于把 mock 的常量搬进真上游验收。
  const qpuHit = new RegExp(`额度换算基数=${UP_QPU}\\b`).test(createToast);
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

  await page.goto(`${BASE}/admin/ui/channels/999999`, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(
    () => document.querySelector('#detail-empty')?.textContent.includes('渠道不存在或已删除'),
    { timeout: 8000 });
  const missingDetail = await page.$eval('#detail-empty', el => el.textContent.trim());
  check('真实令牌下不存在的渠道不显示旧渠道数据', /渠道不存在或已删除/.test(missingDetail), missingDetail);
  await openDetail(newChannelId);

  // 新渠道还没登记凭证与 Key，**必须**报出"缺凭证" ——
  // 首版这条允许"无异常"通过，于是掩盖了一个真缺口：
  // 最常见的"为什么不工作"（没凭证）恰恰是唯一没被 inventory 覆盖的情形。
  //
  // ⚠️ 判据是 **data-anom 属性**，不是可见文案。异常项现在渲染成中文标签
  // （「未登记采集凭证」），原始 kind 不再出现在文本里 —— 拿
  // /credential_missing/ 去 grep textContent 会永远为假，而那看起来像功能坏了。
  // 属性是给机器读的稳定契约，文案是给人读的，两者本就该分开。
  // 先等总览这一轮 XHR 落地。openDetail 只等到分栏切过来，而 inventory 是
  // 切过来之后才发的请求 —— 中间那几十毫秒里 #inv-anomalies 是空的，于是
  // 这条会随机红在 kinds=[] 上，而它下面那条（读同一块的文案）反而是绿的。
  // 那不是功能坏了，是断言比数据快。2026-09-12 补。
  await page.waitForFunction(
    () => (document.querySelector('#inv-anomalies')?.children.length ?? 0) > 0,
    { timeout: 8000 });
  const anomKinds = await page.$$eval('#inv-anomalies [data-anom]',
    els => els.map(e => e.getAttribute('data-anom')));
  const anomText = await page.$eval('#inv-anomalies', el => el.textContent);
  check('新渠道明确报出缺少采集凭证',
    anomKinds.includes('credential_missing'),
    `kinds=${JSON.stringify(anomKinds)}`);
  // 光有属性不够：运维读的是文案。属性对而文案没渲染出来同样是缺陷。
  check('缺凭证异常同时给出可读文案与处置动作',
    /未登记采集凭证/.test(anomText) && /登记凭证/.test(anomText),
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
  // 真正的区别是它现在按"未打上游"归类：带结构化 items 且标"跳过"
  // （旧路径没有 items，只渲染一行光秃秃的"采集失败"）。
  const preCredRows = await page.$$eval('#sync-result tbody tr',
    rs => rs.map(r => [...r.querySelectorAll('td')].map(t => t.textContent.trim())));
  check('前置失败按"未触达上游"归类（items 标跳过）',
    preCredRows.some(r => r.includes('跳过')),
    preCredRows.length ? JSON.stringify(preCredRows[0]) : '无 items（旧 502 路径）');

  // ── 6. 界面登记两个账号与四把 Key，并验证明文不回显（AC-37）──
  //
  // 「账号与 Key」分栏已拆成「账号管理」与「Key 管理」两个分栏，登记表单
  // 下沉到抽屉：渠道与账号都是**选择**而不是手抄 id。
  // 选择器是 ScopePicker（按钮 + 列表弹窗），不是 <select> —— 所以走 pick()。
  await pane('accounts');
  const addAccount = async () => {
    await openDrawer('#btn-new-account', '#acc-channel');
    await pick('acc-channel', [newChannelId]);
    // 必须填**真的**上游用户 ID：SaveAccount 先按 external_user_id 匹配账号行，
    // 匹配不上才退回"该渠道只有一个账号就用它"。填个假 uid 一样能过，
    // 但过的是兜底分支 —— 匹配逻辑本身就没被验到。
    await fill('#acc-uid', UP_UID);
    await page.click('#btn-acc');
    await page.waitForFunction(
      () => /账号已创建/.test(document.querySelector('#toast').textContent),
      { timeout: 8000 });
  };
  // 选择器必须真的列出了刚建的那个渠道 —— 它是"不再手抄 ID"的前提。
  await openDrawer('#btn-new-account', '#acc-channel');
  await page.click('#acc-channel');
  await page.waitForSelector('.picker', { visible: true, timeout: 5000 });
  const chRow = await page.evaluate(
    id => document.querySelector(`[data-pick-row="${id}"]`)?.innerText ?? '',
    newChannelId);
  check('登记账号时渠道来自选择器（不再手抄渠道 ID）', chRow !== '',
    `渠道 #${newChannelId} ${chRow === '' ? '不在选择器里' : '在选择器里'}`);
  // 名字相似的站点只能靠域名区分，所以域名必须**在行里**而不是只在 title 上。
  // 这条是这次改造的全部理由：选错渠道 = 把同步/补齐打到别人家站点上。
  const upHost = new URL(UP_URL).host;
  check('渠道行同时给出名称与域名（名字相似时靠域名分辨）',
    chRow.includes(upHost) && chRow.includes(`#${newChannelId}`),
    `行内容=${chRow.replace(/\s+/g, ' ')} 期望含 ${upHost}`);
  await page.click('[data-pick-cancel]');
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

  // 从账号行点「+ Key」必须**带着这一行的上下文**开抽屉 —— 那是抽屉取代
  // 独立分栏的全部理由（RegisterDrawers 顶部那段）。断言读 data-picked 而不是
  // 按钮文案：文案是 summary 拼出来的，改一次措辞就得改断言。
  //
  // 这条是补的：AccountsView / DetailView 曾把属性写成单数的 `:account-id`，
  // 而 RegisterDrawers 收的是 `accountIds` —— 绑定静默落进 attrs，抽屉打开
  // 但一个字段都不预填，无报错、TS 也不管。当时全套验收是绿的，因为只有
  // Key 页那条路径被验过，而 Key 页恰好传对了。
  await page.click(`[data-account-add-key="${accountIDs[0]}"]`);
  await page.waitForSelector('#key-account', { visible: true, timeout: 5000 });
  const prefill = await page.evaluate(() => ({
    acc: document.querySelector('#key-account')?.getAttribute('data-picked') ?? '',
    ch: document.querySelector('#key-channel')?.getAttribute('data-picked') ?? '',
  }));
  check('从账号行点「+ Key」预填该账号与其渠道（不必再手抄 ID）',
    prefill.acc === String(accountIDs[0]) && prefill.ch === String(newChannelId),
    `账号=${prefill.acc || '空'} 期望 ${accountIDs[0]}；渠道=${prefill.ch || '空'} 期望 ${newChannelId}`);
  await page.click('.drawer-x');

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

  // ── 采集凭证：登记入口与状态都在账号行上 ──
  //
  // 原先这是独立的「采集凭证」分栏。库里 collector_credentials 有
  // UNIQUE(account_id)（023 迁移），一个账号最多一条 —— 凭证是账号的属性，
  // 摆成分栏等于在界面上多编了一个实体；而那个分栏的账号下拉还绑在
  // channels.currentID 上，直接开 /admin/ui/creds 是空的且不说为什么。
  //
  // 登记前先确认这一行**明说了采不了**：缺凭证不是"这一格没数据"。
  const credBefore = await page.$eval(`[data-account-cred="${accountIDs[0]}"]`,
    el => ({ state: el.dataset.cred, text: el.textContent.trim() }));
  check('未登记凭证的账号在行上明确标出（不是留空）',
    credBefore.state === 'missing' && credBefore.text.includes('未登记'),
    `data-cred=${credBefore.state} 文案=${credBefore.text}`);

  // 真实上游令牌挂在第一个账号上。入口是这一行的按钮 —— 点它就该带着这一行。
  await openDrawer(`[data-account-cred-edit="${accountIDs[0]}"]`, '#cred-token');
  const credPicked = await page.$eval('#cred-account', el => el.dataset.picked);
  check('从账号行登记凭证时账号已预填', credPicked === String(accountIDs[0]),
    `账号=${credPicked || '空'} 期望 ${accountIDs[0]}`);
  await fill('#cred-token', UP_TOKEN);
  await page.click('#btn-cred');
  await page.waitForFunction(
    () => /凭证已登记|登记凭证失败/.test(document.querySelector('#toast').textContent),
    { timeout: 10000 });
  const credToast = await page.$eval('#toast', el => el.textContent);
  check('界面登记采集凭证成功', /凭证已登记/.test(credToast),
    credToast.replace(/\n/g, ' | ').slice(0, 80));
  check('凭证类型判定为 NewAPI 系访问令牌', /NewAPI 系访问令牌/.test(credToast));

  // 登记后那一行要翻成"有凭证"，且整页任何位置都不得出现令牌原文
  // （FR-094 同源纪律：凭证内容一律不回显）。
  await page.waitForFunction(
    id => document.querySelector(`[data-account-cred="${id}"]`)?.dataset.cred === 'has',
    { timeout: 8000 }, accountIDs[0]);
  const credCell = await page.$eval(`[data-account-cred="${accountIDs[0]}"]`,
    el => el.closest('td').textContent.replace(/\s+/g, ' ').trim());
  const credDOM = await page.evaluate(() => document.body.innerHTML);
  check('账号行显示凭证状态与类型，且页面不含令牌原文',
    /NewAPI 系访问令牌/.test(credCell) && !credDOM.includes(UP_TOKEN),
    credCell);

  await pane('keys');
  const keySecrets = [
    'sk-ui-account1-key1-never-echoed', 'sk-ui-account1-key2-never-echoed',
    'sk-ui-account2-key1-never-echoed', 'sk-ui-account2-key2-never-echoed',
  ];
  const keyRefs = [UP_KEYREF, `${UP_KEYREF}-2`, `${UP_KEYREF}-3`, `${UP_KEYREF}-4`];
  for (let i = 0; i < keySecrets.length; i++) {
    await openDrawer('#btn-new-key', '#key-account');
    // 账号同样是选择器。选渠道会把账号列表限定到该渠道 —— 跨渠道挂 Key
    // 本来就建不出来，让它在界面上也选不出来。
    await pick('key-channel', [newChannelId]);
    await pick('key-account', [accountIDs[i < 2 ? 0 : 1]]);
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
  //
  // 给它一个短等待而不是当场断言：上面等的是 toast，而 toast 在关抽屉之前
  // 就出现了。当场读必然读到还没被移除的那一帧 —— 那是断言写错，不是产品
  // 没关。给 3 秒：真没关的话照样红，只是不会红在时序上。
  const secretGone = await page.waitForFunction(
    () => document.querySelector('#key-secret') === null,
    { timeout: 3000 }).then(() => true).catch(() => false);
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
  await openDetail(newChannelId);
  await page.click('#btn-sync');
  await page.waitForFunction(
    () => document.querySelector('#sync-result table') !== null, { timeout: 90000 });
  const syncRows = await page.$$eval('#sync-result tbody tr',
    rs => rs.map(r => [...r.querySelectorAll('td')].map(t => t.textContent.trim())));
  check('采集结果表已渲染', syncRows.length >= 5, `${syncRows.length} 项`);

  const byCap = Object.fromEntries(syncRows.map(r => [r[0], r[2]]));
  check('账号采集成功', byCap['账号'] === '成功', byCap['账号']);
  check('分组采集成功', byCap['分组'] === '成功', byCap['分组']);
  check('密钥采集成功', byCap['密钥'] === '成功', byCap['密钥']);
  check('模型目录采集成功', byCap['模型目录'] === '成功', byCap['模型目录']);
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
    const token = localStorage.getItem('adminToken');
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
  // 分组倍率是**常驻列**，不是「列设置」里默认关着的那种。
  //
  // 它直接决定这把 Key 的每次调用被计多少钱（实际计费 = 模型倍率 × 分组倍率，
  // 实测真站点分组倍率 0.12~3.5，十倍以上）。默认关着等于把"这把 Key 贵不贵"
  // 藏进二级菜单。断言列头字面量而不是 data-col 存在：叫「倍率」会被读成模型
  // 倍率，而这个数来自分组。
  const rateHeaderNow = await page.$$eval('#detail-body thead th',
    ts => ts.map(t => t.textContent.trim()));
  const rateCells = await page.$$eval('#detail-body tbody tr[data-key-row] td[data-col="rate"]',
    ts => ts.map(t => t.textContent.trim()));
  // 期望由 API 构造：每把 Key 的倍率必须等于**它所属分组**的 rate_multiplier，
  // 而不是别的分组的、也不是写死的 ×1。
  const rateWant = await page.evaluate(async cid => {
    const t = localStorage.getItem('adminToken');
    const h = { Authorization: `Bearer ${t}` };
    const [ks, gs] = await Promise.all([
      fetch(`/admin/keys?channel_id=${cid}`, { headers: h }).then(r => r.json()),
      fetch(`/admin/channel-groups?channel_id=${cid}`, { headers: h }).then(r => r.json()),
    ]);
    const byID = new Map((gs.items ?? []).map(g => [g.id, g.rate_multiplier]));
    const dynByID = new Map((gs.items ?? []).map(g => [g.id, g.rate_dynamic === true]));
    const candByID = new Map((gs.items ?? []).map(g => [g.id, g.dynamic_candidates ?? []]));
    const rateByRef = new Map((gs.items ?? []).map(g => [g.group_ref, g.rate_multiplier]));
    return (ks.items ?? []).map(k => {
      // auto 是**选组模式**不是可计费分组：实际倍率由运行时命中的候选组决定
      // （NewAPI 源码显式排除 auto 自己）。group_ratio 照样给它一个倍率，
      // 期望里若照抄那个数，就是把"报一个与账单无关的数字"当成正确行为钉死。
      // 动态倍率的组：显示「动态倍率 ×min~max」而不是那个展示占位的固定数。
      // 期望里的范围由**候选分组各自的倍率**现算 —— 照抄 auto 自己那个数就是
      // 把"报一个与账单无关的数字"当成正确行为钉死。
      const gid = k.channel_group_id;
      if (gid !== undefined && dynByID.get(gid) === true) {
        const cands = (candByID.get(gid) ?? [])
          .map(ref => rateByRef.get(ref))
          .filter(r => typeof r === 'number');
        if (cands.length === 0) return '动态倍率';
        const lo = Math.min(...cands), hi = Math.max(...cands);
        return lo === hi ? `动态倍率 ×${lo}` : `动态倍率 ×${lo}~${hi}`;
      }
      const r = k.channel_group_id === undefined ? undefined : byID.get(k.channel_group_id);
      return r === undefined || r === null ? '未知' : `×${r}`;
    });
  }, newChannelId);
  check('Key 表把「分组倍率」作为常驻列，且逐行等于该 Key 所属分组的倍率',
    rateHeaderNow.includes('分组倍率') &&
    JSON.stringify(rateCells) === JSON.stringify(rateWant),
    `列头=${rateHeaderNow.join('|')} 实际=${JSON.stringify(rateCells)} ` +
    `期望=${JSON.stringify(rateWant)}`);

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

  // 不限额的 Key 在配额列**改显所属账号余额**，并带「账号余额」口径注记。
  //
  // 期望由 API 现构造（余额每跑一次都不同，写死必然是错的），逐把比对；
  // 断言里必须带上那条注记 —— 少了它，配额列里就出现一个没有出处的金额，
  // 会被读成 Key 自己的配额，而这两个数不可加也不可比（utils/money 开头）。
  // 空白全剥：注记与金额之间有没有空格取决于 Vue 的 whitespace condense，
  // 不是我们的意图（同上面限流列的理由）。
  const unlimitedWant = await page.evaluate(async cid => {
    const t = localStorage.getItem('adminToken');
    const h = { Authorization: `Bearer ${t}` };
    const [ks, as] = await Promise.all([
      fetch(`/admin/keys?channel_id=${cid}`, { headers: h }).then(r => r.json()),
      fetch(`/admin/accounts?channel_id=${cid}`, { headers: h }).then(r => r.json()),
    ]);
    const bal = new Map((as.items ?? []).map(a => [a.id, a.balance_usd]));
    return (ks.items ?? [])
      .filter(k => k.unlimited_quota === true)
      .map(k => [String(k.id), bal.get(k.account_id) ?? null]);
  }, newChannelId);
  const quotaByID = new Map(
    (await readKeyRows()).map(r => [r.id, r.quota.replace(/\s+/g, '')]));
  // 采到余额的那些才有可比的期望；没采到余额的不限额 Key 退回「不限额度」，
  // 它证明不了借显这件事，所以不计入 —— 一条"集合为空所以全过"的断言
  // 是绿的，但它什么都没验（CLAUDE.md §1：验不了就说验不了）。
  const borrowing = unlimitedWant.filter(([, b]) => typeof b === 'number');
  const borrowGot = borrowing.map(([id]) => quotaByID.get(id) ?? '(缺行)');
  const borrowWant = borrowing.map(([, b]) => `$${b.toFixed(2)}不限额账号余额`);
  check('不限额 Key 的配额列显示所属账号余额并标注口径',
    borrowing.length > 0 &&
    JSON.stringify(borrowGot) === JSON.stringify(borrowWant),
    borrowing.length === 0
      ? `本站 ${unlimitedWant.length} 把不限额 Key 都没采到账号余额，这条无从验`
      : `期望=${JSON.stringify(borrowWant)} 实际=${JSON.stringify(borrowGot)}`);

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
    const t = localStorage.getItem('adminToken');
    const r = await fetch(`/admin/keys?channel_id=${cid}`,
      { headers: { Authorization: `Bearer ${t}` } });
    const d = await r.json();
    return (d.items ?? []).map(k => [k.rpm_limit ?? null, k.concurrency_limit ?? null]);
  }, newChannelId);
  // 由 API 字段**构造**期望串再逐行比对，而不是分「有值/无值」两条断言路径 ——
  // 分路径的那版里，"有值"那支在 newapi 上永不执行（pick-upstream 只选 newapi 系，
  // 而它不报 Key 级限流），等于一半断言从没跑过。构造式只有一条路径，
  // 两种数据都覆盖，且更强：无值时不只验"是 —"，验的是"恰好是 API 说的那个"。
  // 与 KeysView.vue 的模板同构：rpm→"N 次/分钟"、并发→"N 并发"、都有→中间 " / "、
  // 都无→"—"。DOM 侧已折叠连续空白，所以这里也用单空格拼。
  const rlWant = rlFromAPI.map(([r, c]) => {
    const parts = [];
    if (r !== null) parts.push(`${r} 次/分钟`);
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

  // Key 自动化入口只验证界面与默认安全筛选；不点“执行补齐”，避免验收改变真上游。
  const automationButtons = await page.$$eval('#btn-import-keys, #btn-provision-keys', bs => bs.map(b => b.id));
  check('Key 管理页提供同步与批量补齐入口',
    automationButtons.includes('btn-import-keys') && automationButtons.includes('btn-provision-keys'),
    automationButtons.join(', '));

  await page.click('#btn-import-keys');
  await page.waitForFunction(
    () => document.querySelector('.drawer-t')?.textContent.includes('同步已有 Key'),
    { timeout: 5000 });
  check('同步已有 Key 抽屉要求选择渠道', await picked('#key-auto-channel') === '请选择渠道');
  await page.click('.drawer-x');

  await page.click('#btn-provision-keys');
  await page.waitForFunction(
    () => document.querySelector('.drawer-t')?.textContent.includes('批量补齐 Key'),
    { timeout: 5000 });
  const onlyEmpty = await page.$eval('#key-auto-only-without-keys', input => input.checked);
  check('批量补齐默认仅处理无 Key 账号', onlyEmpty);

  // 自动化抽屉里的渠道/账号都是多选：一次挑一批站点比开 N 次抽屉快。
  // 这里只验选择行为，不点「预览/执行补齐」—— 那会真的去动上游。
  await page.click('#key-auto-channel');
  await page.waitForSelector('.picker', { visible: true, timeout: 5000 });
  await page.click('[data-pick-all]');
  const autoPicked = await page.$eval('[data-pick-count]', el => el.textContent.trim());
  await page.click('[data-pick-ok]');
  const autoSummary = await picked('#key-auto-channel');
  check('Key 自动化的渠道范围支持全选（弹窗内批量勾选）',
    /已选 [1-9]/.test(autoPicked) && autoSummary !== '全部渠道',
    `弹窗计数=${autoPicked} 按钮=${autoSummary}`);
  await page.click('.drawer-x');

  // 按渠道筛选后，剩下的必须**都属于**这个渠道 —— 不是"数量对得上"。
  await pick('key-f-channel', [newChannelId], true);
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

  // ── 8quater. 选择器弹窗与「更多」菜单 ──
  //
  // 账号在 P1 **没有名字列**（只有 id 与 external_user_id），渠道名则成批相似。
  // 所以选择器行里必须同时有 #id 与所属渠道域名 —— 这是把「同步/补齐 Key」
  // 打到正确站点上的唯一依据。
  await page.click('#key-f-account');
  await page.waitForSelector('.picker', { visible: true, timeout: 5000 });
  const accRows = await page.$$eval('[data-pick-row]',
    rs => rs.map(r => r.innerText.replace(/\s+/g, ' ')));
  check('账号选择器逐行给出 #id 与所属渠道域名',
    accRows.length >= 2 && accRows.every(t => /#\d+/.test(t) && t.includes(upHost)),
    JSON.stringify(accRows));

  // 弹窗内搜索。65 个渠道 / 几十个账号时，不能搜就只能靠滚。
  await page.type('#key-f-account-q', `#${accountIDs[0]}`, { delay: 2 });
  await sleep(200);
  const searched = await page.$$eval('[data-pick-row]', rs => rs.map(r => r.dataset.pickRow));
  check('选择器弹窗内可搜索并真的收敛到命中行',
    searched.length === 1 && searched[0] === String(accountIDs[0]),
    `搜 #${accountIDs[0]} 命中 ${JSON.stringify(searched)}`);

  await page.click('#key-f-account-q', { clickCount: 3 });
  await page.keyboard.press('Backspace');
  await sleep(200);
  await page.click('[data-pick-all]');
  const allCount = await page.$eval('[data-pick-count]', el => el.textContent.trim());
  await page.click('[data-pick-invert]');
  const invCount = await page.$eval('[data-pick-count]', el => el.textContent.trim());
  check('选择器弹窗支持全选与反选',
    /已选 [1-9]/.test(allCount) && /已选 0 /.test(invCount),
    `全选后=${allCount} 反选后=${invCount}`);
  await page.click('[data-pick-cancel]');

  // 行内「更多」菜单**整块可见**。修复前它是 position:absolute，而表格容器
  // .tw 有 overflow-x:auto（宽表格要横向滚动，于是 overflow-y 也算成 auto）——
  // 菜单从按钮往下展开必然越过表格下边缘，最后一行点开等于什么都没有。
  // 断言用命中测试而不是量矩形：被裁掉的元素矩形照样在（见 menuVisible 注释）。
  await page.click('[data-seg="flat"]');
  await page.waitForFunction(
    () => document.querySelectorAll('#pane-keys tr[data-key-row]').length > 0, { timeout: 5000 });
  const lastMore = await page.$$eval('#pane-keys [data-key-more]',
    bs => bs[bs.length - 1].dataset.keyMore);
  await openMore(`[data-key-more="${lastMore}"]`);
  const lastMenu = await menuVisible(`[data-key-more="${lastMore}"]`);
  check('表格最后一行的「更多」菜单整块可见（不被表格容器裁掉）', lastMenu.ok, lastMenu.why);
  await page.click(`[data-key-more="${lastMore}"]`);

  // 同一段 CSS 也管卡片头里的「列设置」，顺带验一次它没被这次改动带歪。
  await openMore('#btn-cols');
  const colsMenu = await menuVisible('#btn-cols');
  check('卡片头的「列设置」浮窗同样整块可见', colsMenu.ok, colsMenu.why);
  await page.click('#btn-cols');

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
  await openDetail(newChannelId);

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
             navs: document.querySelectorAll('.nav-item').length,
             sidebarHasModels:
               document.querySelector('.sidebar .nav-item[data-pane="models"]') !== null };
  });
  check('侧栏在主区左侧（真左右布局）',
    layout.sx < layout.mx && layout.mx >= layout.sw,
    `侧栏 x=${layout.sx} 宽=${layout.sw}，主区 x=${layout.mx}`);
  check('侧栏与主区顶部对齐（未折成上下堆叠）',
    Math.abs(layout.sTop - layout.mTop) < 2,
    `侧栏 top=${layout.sTop}，主区 top=${layout.mTop}`);
  // 四项：渠道管理 / 账号管理 / Key 管理 / 批量导入。
  // 渠道详情是渠道管理的二级页面，「采集凭证」是账号的属性（并进了账号页），
  // 两者都不占一级菜单位置。
  //
  // 「模型目录」**不在侧栏里**：它与「控制台」同级，在顶层导航上（下一条验）。
  // 那四项都在**管**某种对象，模型目录是**查**，而且它自己就有一整列筛选。
  check('侧栏导航项齐全（模型目录已升到顶层，不占侧栏位）',
    layout.navs === 4 && !layout.sidebarHasModels,
    `${layout.navs} 项，侧栏里有模型目录=${layout.sidebarHasModels}`);

  // ── 11bis. 顶层导航：品牌在左、控制台/模型目录在右，整条居中两边各约 1/4 ──
  //
  // 量**渲染出来的几何**，不是"有没有那个 class"。三件事各自会坏且症状不同：
  // 项数（多出一项就说明有分栏被误升到顶层）、左右次序（品牌与导航调换）、
  // 居中与留白（写成满宽或只居中不留白）。
  const top = await page.evaluate(() => {
    const items = [...document.querySelectorAll('[data-top-nav]')];
    const inner = document.querySelector('.hdr-in').getBoundingClientRect();
    const brand = document.querySelector('.hdr-in .brand').getBoundingClientRect();
    const nav = document.querySelector('.topnav').getBoundingClientRect();
    return {
      keys: items.map(b => b.getAttribute('data-top-nav')),
      labels: items.map(b => b.textContent.trim()),
      active: items.filter(b => b.classList.contains('on'))
        .map(b => b.getAttribute('data-top-nav')),
      brandRight: Math.round(brand.right), navLeft: Math.round(nav.left),
      left: Math.round(inner.left), right: Math.round(inner.right),
      win: window.innerWidth,
    };
  });
  const gapL = top.left;
  const gapR = top.win - top.right;
  check('顶层导航只有「控制台 / 模型目录」两项，品牌在左、导航在右',
    JSON.stringify(top.keys) === JSON.stringify(['console', 'models']) &&
    JSON.stringify(top.labels) === JSON.stringify(['控制台', '模型目录']) &&
    top.brandRight <= top.navLeft,
    `项=${JSON.stringify(top.labels)}，品牌右边界 ${top.brandRight} / 导航左边界 ${top.navLeft}`);
  // 1/4：两边留白各占视口的四分之一（窄屏会退到下限，见 .hdr-in 的注释；
  // 这一轮跑在 1280 宽，走的是严格 1/4 那一支）。
  check('顶层导航居中，两边各留约 1/4',
    Math.abs(gapL - gapR) <= 2 &&
    Math.abs(gapL - top.win / 4) <= 4,
    `左留白 ${gapL} 右留白 ${gapR}，视口 ${top.win}（1/4 = ${Math.round(top.win / 4)}）`);

  // 切到模型目录：控制台那层壳（侧栏 + 分栏顶栏 + 定宽内容区）**整个不渲染**，
  // 它自己铺一页。断言"不在 DOM 上"而不是"看不见"：两套布局别在一起的症状
  // 正是那条几乎全空的横条还在，只是上面没东西了。
  //
  // 令牌框反过来：登录之后**哪一页都不该再有**（它只在登录页填一次，
  // 之后存在浏览器里）。摆在界面上的输入框是一把已经生效的凭证，
  // 却长得像一个还等着你填的表单项。
  await pane('models');
  const onModels = await page.evaluate(() => ({
    shell: document.querySelector('.shell') !== null,
    sidebar: document.querySelector('.sidebar') !== null,
    topbar: document.querySelector('.topbar') !== null,
    content: document.querySelector('.content') !== null,
    ownPage: document.querySelector('#pane-models.mpage') !== null,
    tokens: document.querySelectorAll('#token').length,
    active: document.querySelector('[data-top-nav].on')?.getAttribute('data-top-nav'),
    mside: document.querySelector('.mside') !== null,
  }));
  await pane('channels');
  const backToConsole = await page.evaluate(() => ({
    sidebar: document.querySelector('.sidebar') !== null,
    topbar: document.querySelector('.topbar') !== null,
    tokens: document.querySelectorAll('#token').length,
    active: document.querySelector('[data-top-nav].on')?.getAttribute('data-top-nav'),
  }));
  check('模型目录自己铺一页：控制台那层壳整个不渲染，登录后两边都没有令牌框',
    onModels.shell === false && onModels.sidebar === false &&
    onModels.topbar === false && onModels.content === false &&
    onModels.ownPage && onModels.mside === true && onModels.active === 'models' &&
    onModels.tokens === 0 &&
    backToConsole.sidebar === true && backToConsole.topbar === true &&
    backToConsole.tokens === 0 && backToConsole.active === 'console',
    `模型目录页：shell=${onModels.shell} 侧栏=${onModels.sidebar} ` +
    `分栏顶栏=${onModels.topbar} 定宽内容区=${onModels.content} ` +
    `自己的页容器=${onModels.ownPage} 自带筛选栏=${onModels.mside} ` +
    `令牌框 ${onModels.tokens} 个；` +
    `回控制台：侧栏=${backToConsole.sidebar} 分栏顶栏=${backToConsole.topbar} ` +
    `令牌框 ${backToConsole.tokens} 个`);

  // ── 12. 浅色 / 深色双模式 ──
  //
  // 断言背景亮度而不是 class 名：只看 classList 的话，把 .dark 里的
  // 色值写成白的也照样绿。深/浅两次读数必须真的分处两端。
  // 一个按钮来回切，所以要先确保它此刻不是深色 —— 直接点会在已经是深色时
  // 把它切成浅色，而那时这条断言读到的是"切换坏了"，其实只是起点不同。
  const setTheme = async want => {
    for (let i = 0; i < 2; i++) {
      const now = await page.$eval('#theme-sw', b => b.dataset.theme);
      if (now === want) return;
      await page.click('#theme-sw');
      await sleep(250);
    }
  };
  await setTheme('dark');
  const darkLuma = await bgLuma();
  const darkOn = await page.evaluate(() =>
    document.documentElement.classList.contains('dark'));
  check('深色模式生效且背景确为深色', darkOn && darkLuma < 60,
    `class=dark:${darkOn} 背景亮度=${darkLuma}`);
  await page.screenshot({ path: `${SHOT}/09-theme-dark.png`, fullPage: true });

  await setTheme('light');
  const lightLuma = await bgLuma();
  const lightOff = await page.evaluate(() =>
    !document.documentElement.classList.contains('dark'));
  check('浅色模式生效且背景确为浅色', lightOff && lightLuma > 200,
    `class=dark:${!lightOff} 背景亮度=${lightLuma}`);
  await page.screenshot({ path: `${SHOT}/10-theme-light.png`, fullPage: true });

  // 主题必须跨刷新记住：运维每次开界面都被打回默认色，等于没做
  await setTheme('dark');
  await page.reload({ waitUntil: 'domcontentloaded' });
  await sleep(400);
  const persisted = await page.evaluate(() => ({
    dark: document.documentElement.classList.contains('dark'),
    stored: localStorage.getItem('theme'),
    // 按钮显示的是**当前**主题；刷新后它与 html.dark 必须一致，
    // 否则界面是深色而按钮说浅色（图标读起来就成了假的）
    shown: document.querySelector('#theme-sw')?.dataset.theme,
    // 一个按钮，不是三个
    buttons: document.querySelectorAll('#theme-sw').length,
  }));
  check('主题是一个按钮、跨刷新保留、按钮显示的与实际一致',
    persisted.dark && persisted.stored === 'dark' && persisted.shown === 'dark' &&
    persisted.buttons === 1,
    JSON.stringify(persisted));

  // ── 12bis. 令牌失效（401）自动回登录页 ──
  //
  // 现造一把**不灵的令牌**：直接改 localStorage，不改界面上的任何东西 ——
  // 模拟的是"令牌在后端被换掉了"，而那正是这条要处理的真实情形。
  // 这不违反禁 mock（CLAUDE.md §1）：假的是**输入**（一把过期凭证），
  // 不是依赖 —— 真后端确实会对它回 401，而真后端不肯按需把一把令牌作废给验收用。
  //
  // 清掉那把不灵的令牌是判据的一半：留着的话守卫会认为"有令牌"并放行，
  // 于是每进一页都是先加载、再 401、再弹回来 —— 一个看不见出口的循环。
  const at401Rotated = consoleErrors.length;
  await page.evaluate(() => localStorage.setItem('adminToken', 'token-was-rotated'));
  await page.click('#btn-reload');
  await page.waitForSelector('#pane-login', { timeout: 8000 });
  const bounced = await page.evaluate(() => ({
    url: location.pathname + location.search,
    stored: localStorage.getItem('adminToken') ?? '',
  }));
  check('令牌失效（401）时自动回登录页，带上原地址并清掉那把不灵的令牌',
    /^\/admin\/ui\/login\?next=/.test(bounced.url) && bounced.stored === '',
    JSON.stringify(bounced));
  await drop401(at401Rotated);
  // 登回去，后面的断言还要用
  await page.type('#token', TOKEN);
  await page.click('#login-submit');
  await page.waitForFunction(
    () => document.querySelector('#pane-channels')?.classList.contains('on') === true,
    { timeout: 10000 });

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

  // ── 12ter. all-api-hub 的 WebDAV 定时同步 ──
  //
  // 对手是 ui-stack.sh 起的**真 WebDAV 服务端**（Apache mod_dav），不是静态文件
  // 服务器冒充的：要验的正是"我们发的 GET 能不能从 WebDAV 上把备份取回来"，
  // Basic 鉴权、目录语义、PUT 上去的东西怎么读回来，只有真服务端说了算。
  //
  // 上面有两份：默认路径下是**真的 HUB_FILE**（明文），enc/ 下是仓库里那份
  // **由 all-api-hub 自己的加密实现产出**的信封夹具。
  if (DAV_BASE && HUB_FILE && existsSync(HUB_FILE)) {
    await pane('import');
    const hubAccounts = JSON.parse(readFileSync(HUB_FILE, 'utf8')).accounts?.accounts || [];

    // 保存一次配置。地址给的是**目录**，服务端要自己补出上游的默认文件名
    // all-api-hub-backup/all-api-hub-1-0.json —— 补错了的表现是 404，
    // 而 404 会被报成"还没有备份"，一个看起来正常、于是没人会查的状态。
    const saveHubSync = async (url, encPassword) => {
      await fill('#hs-url', url);
      await fill('#hs-user', DAV_USER);
      await fill('#hs-pass', DAV_PASS);
      await fill('#hs-enc', encPassword);
      await page.click('#btn-hubsync-save');
      await page.waitForFunction(
        () => /同步配置已保存|保存同步配置失败/.test(
          document.querySelector('#toast')?.textContent || ''),
        { timeout: 8000 });
      return page.$eval('#toast', el => el.textContent);
    };
    // ⚠️ 超时必须**小于** puppeteer 的 protocolTimeout（默认 180s）。
    // waitForFunction 是一次长挂起的 Runtime.callFunctionOn：timeout 给到 300s
    // 的话，180s 上限先到，抛的是 ProtocolError 而不是 TimeoutError —— 报错里
    // 只有一句"callFunctionOn timed out"，看不出页面当时到底显示了什么。
    // 真上游那轮实测 ~30s，150s 足够宽。
    const runHubSync = async () => {
      await page.click('#btn-hubsync-run');
      try {
        await page.waitForFunction(
          () => /同步完成|同步失败/.test(document.querySelector('#toast')?.textContent || ''),
          { timeout: 150000 });
      } catch (e) {
        // 超时本身不该让整个脚本崩掉：把页面当时的状态带出来，
        // 让断言红在"toast 是什么"上，而不是红在一句协议错误上。
        const shown = await page.$eval('#toast',
          el => `${el.textContent} [visible=${el.style.display !== 'none'}]`).catch(() => '(读不到)');
        return `等待同步结果超时（${e.name}）；当前 toast=${shown}`;
      }
      return page.$eval('#toast', el => el.textContent);
    };

    const savedToast = await saveHubSync(`${DAV_BASE}/`, DAV_ENC_PASSWORD);
    check('保存 WebDAV 同步配置成功', /同步配置已保存/.test(savedToast),
      savedToast.replace(/\s+/g, ' ').slice(0, 80));

    // 明文备份这一路：目录 → 默认文件名 → 取回 → 解析出全部条目。
    // 条目数与手工上传那条路必须一致 —— 同一份文件，两条入口。
    const chCountNow = async () => {
      await pane('channels');
      await page.click('#btn-reload');
      await sleep(400);
      const txt = await page.$eval('#ch-count', el => el.innerText);
      await pane('import');
      return Number((txt.match(/\d+/) || [NaN])[0]);
    };
    const chBefore = await chCountNow();
    const plainToast = await runHubSync();
    check('从真 WebDAV 取回明文备份并读出全部条目（目录自动补默认文件名）',
      new RegExp(`同步完成（未落库）：${hubAccounts.length} 个条目`).test(plainToast) ||
      plainToast.includes(`${hubAccounts.length} 个条目`),
      plainToast.replace(/\s+/g, ' ').slice(0, 110));
    const chAfter = await chCountNow();
    check('report 模式不落库（渠道数不变）', chAfter === chBefore,
      `同步前 ${chBefore} 个渠道，同步后 ${chAfter} 个`);

    // 加密这一路：enc/ 下那份信封由**上游自己的** encryptWebdavBackupContent 产出，
    // 解得开才说明我方复刻的 PBKDF2-SHA256 + AES-256-GCM 参数是对的。
    await saveHubSync(`${DAV_BASE}/enc/all-api-hub-1-0.json`, DAV_ENC_PASSWORD);
    const encToast = await runHubSync();
    check('从真 WebDAV 取回**加密**备份并解密成功（上游 PBKDF2+AES-GCM 信封）',
      /同步完成/.test(encToast) && /1 个条目/.test(encToast),
      encToast.replace(/\s+/g, ' ').slice(0, 110));

    // 密码错必须红。没有这一条的话，上面那条绿只能说明"取回来了"——
    // 万一解密被跳过、明文判定误把信封当明文放过去，它照样绿。
    await saveHubSync(`${DAV_BASE}/enc/all-api-hub-1-0.json`, 'definitely-not-the-password');
    const badToast = await runHubSync();
    check('解密密码错时同步失败且说清是密码问题',
      /同步失败/.test(badToast) && /密码/.test(badToast),
      badToast.replace(/\s+/g, ' ').slice(0, 110));

    // 两个密码一律不回显（FR-094 同源纪律）：整份 DOM 里不许出现它们。
    await page.click('#btn-hubsync-reload');
    await sleep(400);
    const hubDom = await page.evaluate(() => document.body.innerHTML);
    check('WebDAV 与解密密码不回显到界面',
      !hubDom.includes(DAV_PASS) && !hubDom.includes(DAV_ENC_PASSWORD),
      '已确认 DOM 内无两个密码原文');
    // ── 同步历史与详情弹窗 ──
    //
    // 上面刚跑了三轮（明文成功 / 加密成功 / 密码错失败），历史里就该有三行，
    // 且最新那行是失败的那一轮。**失败也要进历史** —— 它是界面上唯一能看见
    // "为什么一直没同步"的地方，只记成功的话这张表在出事时正好是空的。
    await page.waitForFunction(
      () => document.querySelectorAll('#hubsync-runs tbody tr').length >= 3,
      { timeout: 10000 });
    const runRows = await page.$$eval('#hubsync-runs tbody tr', trs =>
      trs.map(tr => ({
        id: tr.getAttribute('data-hubsync-run'),
        cells: [...tr.querySelectorAll('td')].map(td => td.innerText.replace(/\s+/g, ' ').trim()),
      })));
    check('同步历史列出最近每一轮（成功与失败都进）',
      runRows.length >= 3 && /失败/.test(runRows[0].cells.join(' ')) &&
      runRows.slice(1, 3).every(r => /成功/.test(r.cells.join(' '))),
      runRows.slice(0, 3).map(r => r.cells.slice(0, 4).join('/')).join(' | '));
    // report 模式下"入库 68"一个渠道都没建 —— 两种轮次在数字上看起来一模一样，
    // 所以"是否落库"必须单独成列，不能只靠数字。
    check('历史行区分"落没落库"与触发方式',
      runRows.every(r => /未落库|已落库/.test(r.cells.join(' '))) &&
      runRows.every(r => /手动|定时/.test(r.cells.join(' '))),
      runRows[0].cells.slice(0, 4).join(' / '));

    // 点最老那一行（明文那轮，110 个站点）看详情。
    // 这条同时验着一个容易写错的设计：**列表里的 result 被剥掉了 items**，
    // 点开时必须重新取整条 —— 直接把列表那条塞进弹窗的话明细永远是空的，
    // 而弹窗看起来完全正常。
    const plainRun = runRows[runRows.length - 1];
    await page.click(`[data-hubsync-run="${plainRun.id}"]`);
    await page.waitForSelector('#hubsync-detail', { visible: true, timeout: 8000 });
    await page.waitForFunction(
      () => document.querySelectorAll('#hubsync-detail tbody tr').length > 1,
      { timeout: 8000 });
    const detailRows = await page.$$eval('#hubsync-detail tbody tr', trs => trs.length);
    const detailHead = await page.$eval('#hubsync-detail .drawer-d',
      el => el.innerText.replace(/\s+/g, ' ').trim());
    check('点历史行弹出详情，且带列表里没有的逐站明细',
      detailRows === hubAccounts.length && /未落库|已落库/.test(detailHead),
      `明细 ${detailRows} 行（备份 ${hubAccounts.length} 条）；抬头=${detailHead.slice(0, 60)}`);
    await page.screenshot({ path: `${SHOT}/11d-hubsync-detail.png`, fullPage: true });
    await page.click('#hubsync-detail .drawer-x');
    await page.waitForFunction(
      () => document.querySelector('#hubsync-detail') === null, { timeout: 5000 });

    // 失败那轮的详情要写清错在哪 —— 只标一个红"失败"，人还得去翻服务端日志。
    await page.click(`[data-hubsync-run="${runRows[0].id}"]`);
    await page.waitForSelector('#hubsync-detail-error', { visible: true, timeout: 8000 });
    const detailErr = await page.$eval('#hubsync-detail-error', el => el.innerText);
    check('失败那轮的详情写明失败原因', /密码/.test(detailErr),
      detailErr.replace(/\s+/g, ' ').slice(0, 80));
    await page.click('#hubsync-detail .drawer-x');

    // 勾选框不该被全局的 input{width:100%;height:36px} 拉成一整格方块。
    // 量的是它渲染出来的尺寸，不是"有没有加上那个 class" —— 后者改个类名就失效。
    const chkBox = await page.$eval('#hs-enabled', el => {
      const r = el.getBoundingClientRect();
      return { w: Math.round(r.width), h: Math.round(r.height) };
    });
    check('定时开关渲染成正常大小的勾选框（没被全局 input 规则拉开）',
      chkBox.w > 0 && chkBox.w <= 28 && chkBox.h > 0 && chkBox.h <= 28,
      `${chkBox.w}×${chkBox.h}px`);
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

  // 编辑 / 停用 / 启用都收进了行内「更多」菜单（低频且后果重的操作不与
  // 「详情」并排），所以每次都要先把那个 <details> 打开。
  const renamed = `${uniq}-改名`;
  await openMore(`[data-ch-more="${newChannelId}"]`);
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
  //
  // 停用改走统一确认框（原来是行内展开 + #btn-ch-disable-ok）：确认框里能写下
  // 影响范围，而"确定？"三个字写不下。状态文案也从库里的枚举值改成了中文，
  // 所以下面比的是「启用 / 停用」而不是 enabled / disabled。
  await openMore(`[data-ch-more="${newChannelId}"]`);
  await page.click(`#channels button[data-ch-disable="${newChannelId}"]`);
  await page.waitForSelector('#ch-dis-reason', { timeout: 5000 });
  const chImpact = await page.$eval('.confirm-i', el => el.textContent.trim());
  check('停用渠道的确认框写明影响的账号与 Key 数',
    /账号/.test(chImpact) && /Key/.test(chImpact), chImpact.slice(0, 70));
  await confirmOK();
  await sleep(800);
  const statusAfterEmpty = await page.$eval(`[data-ch-status="${newChannelId}"]`,
    el => el.textContent.trim());
  check('停用不填原因被拦下且状态未变（FR-095）',
    /启用/.test(statusAfterEmpty), `状态=${statusAfterEmpty}`);

  const reason = '验收脚本停用测试-站点余额耗尽';
  await page.type('#ch-dis-reason', reason);
  await confirmOK();
  await page.waitForFunction(
    id => /停用/.test(document.querySelector(`[data-ch-status="${id}"]`)?.textContent || ''),
    { timeout: 8000 }, newChannelId).catch(() => {});
  const statusAfter = await page.$eval(`[data-ch-status="${newChannelId}"]`,
    el => el.textContent.trim());
  check('停用后列表状态变为「停用」', /停用/.test(statusAfter), statusAfter);
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
  await openMore(`[data-ch-more="${newChannelId}"]`);
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
    reasonKept === reason && /停用/.test(stillDisabled),
    `原因=${reasonKept || '（被抹掉了）'} 状态=${stillDisabled}`);

  await openMore(`[data-ch-more="${newChannelId}"]`);
  await page.click(`#channels button[data-ch-enable="${newChannelId}"]`);
  await page.waitForFunction(
    id => /启用/.test(document.querySelector(`[data-ch-status="${id}"]`)?.textContent || ''),
    { timeout: 8000 }, newChannelId).catch(() => {});
  const backOn = await page.$eval(`[data-ch-status="${newChannelId}"]`,
    el => el.textContent.trim());
  // 启用后原因必须一并清掉：留着上次的原因，界面上就是"已启用"却带着停用理由
  const reasonGone = await page.$$eval(`[data-ch-reason="${newChannelId}"]`,
    els => els.length === 0);
  check('启用后状态恢复且停用原因被清空', /启用/.test(backOn) && reasonGone,
    `状态=${backOn}，原因元素${reasonGone ? '已消失' : '仍在'}`);

  await page.screenshot({ path: `${SHOT}/12-channel-edit.png`, fullPage: true });

  // ── 12quater. 模型目录分栏（跨渠道 + 分面筛选）──
  //
  // **要两个不同的真站点才验得了**：只有一个渠道时 channel_count 恒为 1，
  // 聚合的 GROUP BY 写成什么样都绿；渠道分面也只有一项，"筛了渠道之后发行方
  // 收窄"那条同样验不到。库里 base_url 有唯一约束，同一个站建不出两个渠道 ——
  // 那个约束是对的，不该为了凑数据去绕它。第二个站由 ui-stack.sh 用
  // PICK_COUNT=2 现场探活挑出来（CLAUDE.md §1：不造站点）。
  const UP2 = {
    name: process.env.UP2_NAME || '',
    url: process.env.UP2_URL || '',
    token: process.env.UP2_TOKEN || '',
    uid: process.env.UP2_UID || '',
  };
  if (UP2.url === '') {
    check('模型目录：跨渠道聚合（要两个真站点）', false,
      '只挑到一个可用真站点，这几条无从验 —— 调大 PICK_MAX_TRY 或换一份更新的导出。' +
      '不拿单渠道的绿冒充跨渠道通过（CLAUDE.md §1）');
  } else {
    // 建第二个渠道走 API 而不是点界面：建渠道/登记凭证那条路前面已经逐项验过，
    // 这里只需要**第二个真站点的目录数据**当输入，重点在聚合与分面本身。
    const ch2 = await page.evaluate(async up => {
      const t = localStorage.getItem('adminToken');
      const h = { Authorization: `Bearer ${t}`, 'Content-Type': 'application/json' };
      const post = async (p, b) =>
        (await fetch(p, { method: 'POST', headers: h, body: JSON.stringify(b) })).json();
      const c = await post('/admin/channels',
        { name: `目录验收-${up.name}`.slice(0, 40), base_url: up.url, auto_detect: true });
      if (typeof c.id !== 'number') return { error: c.error ?? '建渠道失败' };
      const a = await post('/admin/accounts',
        { channel_id: c.id, external_user_id: up.uid });
      if (typeof a.id !== 'number') return { error: a.error ?? '建账号失败' };
      const cr = await post('/admin/collector/credentials',
        { account_id: a.id, access_token: up.token });
      if (cr.stored !== true) return { error: cr.error ?? '登记凭证失败' };
      const s = await post(`/admin/channels/${c.id}/sync`, {});
      return {
        id: c.id,
        sync: (s.items ?? []).map(i => `${i.capability}=${i.status}`).join(' '),
        error: s.error,
      };
    }, UP2);
    check('第二个真上游建渠道并采集成功（跨渠道目录的输入）',
      ch2.error === undefined && /model_catalog=ok/.test(ch2.sync ?? ''),
      ch2.error ?? `渠道 ${ch2.id}：${ch2.sync}`);

    await pane('models');
    await page.waitForFunction(
      () => document.querySelectorAll('[data-model-row]').length > 0, { timeout: 10000 });

    // 期望从 API 现构造。**逐字比对**而不是"数字大于零"：后者在把两个渠道
    // 合成一行、或把渠道名错位时照样绿。
    const api = async path => page.evaluate(async p => {
      const t = localStorage.getItem('adminToken');
      const r = await fetch(p, { headers: { Authorization: `Bearer ${t}` } });
      return r.json();
    }, path);

    const firstPage = await api('/admin/catalog?limit=50&offset=0');
    const domRows = await page.$$eval('[data-model-row]',
      rs => rs.map(r => r.getAttribute('data-model-row')));
    check('模型目录分栏列出模型，且与 /admin/catalog 逐行同序',
      JSON.stringify(domRows) === JSON.stringify(firstPage.items.map(m => m.model_name)),
      `共 ${firstPage.total} 个模型；界面 ${domRows.length} 行，首行 ${domRows[0]}`);

    // ── 明细一律走抽屉（列表模式已无行内展开）──
    //
    // 两种视图共用同一个抽屉与同一个 ModelChannelTable。断言"点了列表行之后
    // 抽屉里是那个模型"，顺带守住"行内展开真的删干净了" —— 留着一份就意味着
    // 那张七列的表有两套容器，而两套宽度约束迟早会岔开。
    const openDrawerFor = async name => {
      await page.evaluate(n => {
        [...document.querySelectorAll('[data-model-open]')]
          .find(b => b.getAttribute('data-model-open') === n)?.click();
      }, name);
      await page.waitForSelector('.drawer [data-model-channels]', { timeout: 5000 });
    };
    const readDrawer = () => page.evaluate(() => {
      const d = document.querySelector('.drawer');
      const tbl = d.querySelector('[data-model-channels]');
      return {
        wide: d.classList.contains('wide'),
        width: Math.round(d.getBoundingClientRect().width),
        viewport: window.innerWidth,
        title: d.querySelector('.drawer-t')?.textContent.trim(),
        forModel: tbl?.getAttribute('data-model-channels'),
        chNames: [...tbl.querySelectorAll('tbody tr td:first-child')]
          .map(t => t.textContent.trim()),
        // 只取「实际价」那格里的数字本身：后面还跟着"最低 · 分组 X（共 N 个…）"
        eff: [...tbl.querySelectorAll('tbody tr td[data-model-eff]')]
          .map(t => (t.textContent.match(/-?[\d.]+/) ?? ['未采到分组倍率'])[0]),
      };
    });
    /**
     * 遮罩（.drawer-scrim）的形态。全站四处浮层共用这一个类。
     *
     * 判据不是"有没有设那个属性"那么松，也不是去量模糊半径那么假：
     *   · backdrop-filter 里确实有 blur（纯压暗的话这里是 none）
     *   · 遮罩铺满整个视口（只盖住一半的毛玻璃是另一种坏法）
     *   · **浮层自己不糊、且背景不透明** —— 毛玻璃放错地方（放到面板上）的
     *     症状正是"要读的那块反而看不清"，而那时上面两条照样绿。
     */
    const scrimStyle = () => page.evaluate(() => {
      const el = document.querySelector('.drawer-scrim');
      if (el === null) return null;
      const cs = getComputedStyle(el);
      const r = el.getBoundingClientRect();
      const panel = el.firstElementChild;
      const pc = panel === null ? null : getComputedStyle(panel);
      const alpha = c => {
        const m = /^rgba?\(([^)]+)\)/.exec(c ?? '');
        if (m === null) return 1;
        const parts = m[1].split(',').map(x => parseFloat(x));
        return parts.length === 4 ? parts[3] : 1;
      };
      return {
        blur: cs.backdropFilter || cs.webkitBackdropFilter || 'none',
        w: Math.round(r.width), h: Math.round(r.height),
        vw: window.innerWidth, vh: window.innerHeight,
        panelBlur: pc?.backdropFilter || 'none',
        panelAlpha: alpha(pc?.backgroundColor),
      };
    });
    const scrimOK = g =>
      g !== null && /blur\(/.test(g.blur) && g.w >= g.vw && g.h >= g.vh &&
      g.panelBlur === 'none' && g.panelAlpha === 1;

    const closeDrawer = async () => {
      await page.keyboard.press('Escape');
      await page.waitForFunction(
        () => document.querySelector('.drawer') === null, { timeout: 5000 }).catch(() => {});
    };

    const noInlineExpand = await page.$$eval('[data-model-toggle]', els => els.length);
    check('列表模式改用抽屉，行内展开已移除',
      noInlineExpand === 0, `残留的展开按钮 ${noInlineExpand} 个`);

    // 核心那条：两个站都有的模型，渠道数必须是 2，且抽屉里列出的正是那两个站。
    const shared = firstPage.items.filter(m => m.channel_count >= 2);
    if (shared.length === 0) {
      check('跨渠道：两站共有的模型渠道数为 2 且列出两个渠道名', false,
        `两个真站点（${UP_URL} / ${UP2.url}）的目录没有交集 —— 这条无从验`);
    } else {
      const m = shared[0];
      await openDrawerFor(m.model_name);
      const d = await readDrawer();
      const wantNames = m.channels.map(c => c.channel_name);
      const countShown = await page.evaluate(n => {
        const e = [...document.querySelectorAll('[data-model-chcount]')]
          .find(x => x.getAttribute('data-model-chcount') === n);
        return Number(e.textContent.trim());
      }, m.model_name);
      check('跨渠道：两站共有的模型渠道数为 2 且列出两个渠道名',
        countShown === m.channel_count && m.channel_count >= 2 &&
        d.title === m.model_name && d.forModel === m.model_name &&
        JSON.stringify(d.chNames) === JSON.stringify(wantNames),
        `${m.model_name}：渠道数=${countShown} 抽屉=${d.title} ` +
        `渠道=${JSON.stringify(d.chNames)} 期望=${JSON.stringify(wantNames)}`);

      const drawerScrim = await scrimStyle();
      check('抽屉的遮罩是毛玻璃（不是纯压暗），且抽屉自己不跟着糊',
        scrimOK(drawerScrim), JSON.stringify(drawerScrim));

      // 抽屉要够宽：它装的是七列的表，460px 下每列都换行。
      // 量渲染出来的宽度而不是"有没有 wide 这个 class" —— 后者改个类名就失效。
      check('模型抽屉是宽抽屉（约占半屏，装得下七列明细）',
        d.wide && d.width >= Math.min(700, d.viewport * 0.45),
        `宽 ${d.width}px / 视口 ${d.viewport}px，wide=${d.wide}`);

      // 「实际价」= 模型价 × 分组倍率。漏乘的话显示的仍是一个格式正确、量级
      // 正常的数字，只是与账单差十几倍（实测真站点分组倍率 0.12~3.5）。
      const effWant = m.channels.map(c => {
        const rates = (c.groups ?? [])
          .map(g => g.rate_multiplier)
          .filter(r => typeof r === 'number');
        if (c.input_price === undefined || rates.length === 0) return '未采到分组倍率';
        return String(Math.round(c.input_price * Math.min(...rates) * 1e6) / 1e6);
      });
      check('模型目录的「实际价」= 模型价 × 分组倍率（不是裸模型价）',
        JSON.stringify(d.eff) === JSON.stringify(effWant),
        `实际=${JSON.stringify(d.eff)} 期望=${JSON.stringify(effWant)}（` +
        m.channels.map(c => `${c.channel_name} 模型价=${c.input_price} 分组=` +
          JSON.stringify((c.groups ?? []).map(g => `${g.group_ref}×${g.rate_multiplier}`))
        ).join(' / ') + '）');
      await closeDrawer();
    }

    // ── 分面：按渠道 / 发行方 / 端点类型筛 ──
    //
    // 分面计数**在除自己以外的全部筛选之下**统计。这条规则要正着反着各验一次：
    //   正：筛了渠道之后，发行方只剩这些渠道有的（否则点下去是空结果）
    //   反：渠道那一组**仍然列全**（否则选了一个就换不了，只能清空重选）
    // 少哪一半都会得到一个"看起来能用"的分面。
    const facetCounts = () => page.evaluate(() => ({
      // 渠道/Key 在弹窗里，不再有 chip；这里数的是**弹窗里的行**，
      // 同样能回答"换得了渠道吗"这个问题。
      channels: document.querySelectorAll('#model-f-channel').length,
      // ⚠️ 发行方**不能数行**：超过 12 家就折叠，数出来的永远是 12
      // （实测真站点 34 家）。折叠时「展开全部 N 家」那个 N 才是真实分面大小；
      // 展开了就按行数减去「全部发行方」那一行。
      vendors: (() => {
        const m = document.querySelector('#model-vendor-more')?.textContent.match(/(\d+)\s*家/);
        return m === null || m === undefined
          ? document.querySelectorAll('[data-facet-vendor]').length - 1
          : Number(m[1]);
      })(),
      endpoints: document.querySelectorAll('[data-facet-endpoint]').length,
    }));
    const before = await facetCounts();
    const ch2Vendors = Object.keys(
      (await api(`/admin/catalog?channel_id=${ch2.id}&limit=1`)).vendors ?? {}).length;
    const allVendors = Object.keys(firstPage.vendors ?? {}).length;
    // 渠道与 Key 改走 ScopePicker 弹窗（真库 64 个渠道，平铺会把列表挤出首屏）。
    // 用同一个 pick() 助手 —— 它已经会开弹窗、点行、点确定。
    await pick('model-f-channel', [ch2.id], true);
    await sleep(900);
    const after = await facetCounts();
    const urlAfter = await page.evaluate(() => location.search);
    // 逐字比对后端说的家数，不是"变小了就行"：收窄到**另一个**错误的集合
    // （比如少算了一家）同样会变小，而那看起来完全正常。
    check('按渠道筛选：发行方分面收窄到该渠道，而渠道选择器仍能换渠道',
      after.vendors === ch2Vendors && after.vendors < before.vendors &&
      before.vendors === allVendors &&
      after.channels === 1 && new RegExp(`channel=${ch2.id}`).test(urlAfter),
      `发行方 ${before.vendors}→${after.vendors} 家（后端说该渠道有 ${ch2Vendors} 家，` +
      `全库 ${allVendors} 家）URL=${urlAfter}`);

    // 筛出来的每一行都必须真的属于该渠道 —— 期望由 API 现算。
    const chFiltered = await api(`/admin/catalog?channel_id=${ch2.id}&limit=50`);
    const chRows = await page.$$eval('[data-model-row]',
      rs => rs.map(r => r.getAttribute('data-model-row')));
    check('按渠道筛选后每一行都属于该渠道',
      JSON.stringify(chRows) === JSON.stringify(chFiltered.items.map(m => m.model_name)) &&
      chFiltered.items.every(m => m.channels.every(c => c.channel_id === ch2.id)),
      `${chRows.length} 行 / 后端 ${chFiltered.total} 个模型`);

    // 叠一个发行方：两维一起生效，且行里那个发行方徽标就是选中的那个。
    const vendorPick = Object.entries(chFiltered.vendors ?? {})
      .sort((a, b) => b[1] - a[1])[0];
    if (vendorPick === undefined) {
      check('叠加发行方筛选后每一行都是该发行方', false,
        '该渠道的目录里一个发行方都没采到 —— 这条无从验');
    } else {
      await page.evaluate(v => {
        [...document.querySelectorAll('[data-facet-vendor]')]
          .find(b => b.getAttribute('data-facet-vendor') === v)?.click();
      }, vendorPick[0]);
      await sleep(900);
      // 读 .vname 而不是整格 textContent：图标取不到时 VendorIcon 退回的是一个
      // **带字母的**方块，整格会读成 "OOpenAI"。
      const vendorCells = await page.$$eval('td[data-col="vendor"]',
        ts => ts.map(t => [...t.querySelectorAll('.vname')]
          .map(v => v.textContent.trim()).join(',')));
      const vendorURL = await page.evaluate(() => location.search);
      check('叠加发行方筛选后每一行都是该发行方',
        vendorCells.length === Math.min(50, vendorPick[1]) &&
        vendorCells.every(t => t === vendorPick[0]) &&
        /vendor=/.test(vendorURL) && new RegExp(`channel=${ch2.id}`).test(vendorURL),
        `${vendorCells.length} 行（后端说 ${vendorPick[1]} 个）发行方取值=` +
        `${JSON.stringify([...new Set(vendorCells)])} URL=${vendorURL}`);
    }

    // 端点类型：选一个，筛出来的每一行都得带它。
    await page.click('#model-reset');
    await sleep(900);
    const epPick = Object.entries(firstPage.endpoints ?? {})
      .sort((a, b) => a[1] - b[1])[0];
    if (epPick === undefined) {
      check('按端点类型筛选后每一行都支持该端点', false,
        '两个真站点都没声明端点类型 —— 这条无从验');
    } else {
      await page.evaluate(e => {
        [...document.querySelectorAll('[data-facet-endpoint]')]
          .find(b => b.getAttribute('data-facet-endpoint') === e)?.click();
      }, epPick[0]);
      await sleep(900);
      const epResp = await api(`/admin/catalog?endpoint=${encodeURIComponent(epPick[0])}&limit=50`);
      const epRows = await page.$$eval('[data-model-row]',
        rs => rs.map(r => r.getAttribute('data-model-row')));
      check('按端点类型筛选后每一行都支持该端点',
        JSON.stringify(epRows) === JSON.stringify(epResp.items.map(m => m.model_name)) &&
        epResp.items.every(m =>
          m.channels.some(c => (c.endpoint_types ?? []).includes(epPick[0]))),
        `端点 ${epPick[0]}：界面 ${epRows.length} 行 / 后端 ${epResp.total} 个模型`);
      await page.click('#model-reset');
      await sleep(900);
    }

    // ── 布局：左筛选 / 右结果，搜索居中在页头 ──
    //
    // 量**渲染出来的位置**，不是"有没有那个 class"：类名换个写法这条就假绿，
    // 而它要守的恰恰是"发行方在左边、结果在右边"这个视觉事实本身。
    await page.click('#model-reset');
    await sleep(900);
    const layout = await page.evaluate(() => {
      const r = s => document.querySelector(s)?.getBoundingClientRect() ?? null;
      const side = r('.mside'), main = r('.mmain'), q = r('#model-q'), hero = r('.mhero');
      return {
        sideRight: Math.round(side?.right ?? -1),
        mainLeft: Math.round(main?.left ?? -1),
        qCenter: Math.round((q?.left ?? 0) + (q?.width ?? 0) / 2),
        heroCenter: Math.round((hero?.left ?? 0) + (hero?.width ?? 0) / 2),
        // 搜索框必须在页头、不在结果区里：结果区随响应整块重渲染会换掉输入框
        // 节点，焦点与光标随之丢失 —— 症状是"筛选框只认一个字符"。
        qInMain: document.querySelector('.mmain #model-q') !== null,
      };
    });
    check('模型目录是左筛选 / 右结果两栏，搜索框居中在页头（不在结果区里）',
      layout.sideRight > 0 && layout.sideRight <= layout.mainLeft && !layout.qInMain &&
      Math.abs(layout.qCenter - layout.heroCenter) <= 24,
      `左栏右边界 ${layout.sideRight} / 右栏左边界 ${layout.mainLeft}，` +
      `搜索框中心 ${layout.qCenter} / 页头中心 ${layout.heroCenter}，` +
      `搜索框在结果区里=${layout.qInMain}`);

    // ── 发行方图标 ──
    //
    // 判据是 **naturalWidth > 0**：有 <img> 只能证明"写了个 img 标签"，
    // src 指错、文件没打进产物时标签照样在，而页面上是一排裂图。
    // 同时守住离线保证（internal/admin/web.go）：src 必须是本站资源，
    // 一旦有人图省事换成 CDN，这条就红。
    await page.evaluate(() => document.querySelector('#model-vendor-more')?.click());
    await sleep(300);
    await page.waitForFunction(
      () => [...document.querySelectorAll('.mside img.vmark')].every(i => i.complete),
      { timeout: 5000 }).catch(() => {});
    const icons = await page.evaluate(() => {
      const rows = [...document.querySelectorAll('[data-facet-vendor]')]
        .filter(b => b.getAttribute('data-facet-vendor') !== '');
      return rows.map(b => {
        const img = b.querySelector('img.vmark');
        return {
          vendor: b.getAttribute('data-facet-vendor'),
          loaded: img !== null && img.complete && img.naturalWidth > 0,
          broken: img !== null && img.complete && img.naturalWidth === 0,
          src: img?.getAttribute('src') ?? '',
          letter: b.querySelector('span.vmark') !== null,
        };
      });
    });
    // 后端声明了图标名的那些**必须**渲染成真图标。少哪家就把那个 SVG 补进
    // web/src/assets/vendor/（见那里的 README）—— 失败信息直接给出名字。
    const declared = (await api('/admin/catalog?limit=1')).vendor_icons ?? {};
    const shouldHave = icons.filter(i => (declared[i.vendor] ?? '') !== '');
    const missing = shouldHave.filter(i => !i.loaded).map(i => `${i.vendor}(${declared[i.vendor]})`);
    check('发行方图标：上游声明了图标的都渲染成真 SVG，没声明的退回字母块，且无外链',
      shouldHave.length > 0 && missing.length === 0 &&
      icons.every(i => !i.broken) &&
      icons.every(i => i.loaded || i.letter) &&
      icons.every(i => i.src === '' || i.src.startsWith('/admin/ui/')),
      `${icons.length} 家发行方，上游声明图标 ${shouldHave.length} 家，` +
      `渲染出真图标 ${icons.filter(i => i.loaded).length} 家；` +
      `缺图标=${JSON.stringify(missing)}；` +
      `外链=${JSON.stringify(icons.map(i => i.src).filter(u => u !== '' && !u.startsWith('/admin/ui/')))}`);
    await page.evaluate(() => document.querySelector('#model-vendor-more')?.click());
    await sleep(300);

    // ── 排序 ──
    //
    // 排序必须在**服务端**做。客户端只排当前页的话，翻到第二页看到的是
    // "另一段里各自排好的" —— 每一页内部有序，整体并不有序，而那看起来完全正常。
    // 所以判据是与后端同参数的响应**逐行同序**，不是"界面自己排好了"。
    await page.select('#model-sort', 'name');
    await sleep(900);
    const nameRows = await page.$$eval('[data-model-row]',
      rs => rs.map(r => r.getAttribute('data-model-row')));
    const nameResp = await api('/admin/catalog?sort=name&limit=50');
    check('按模型名排序在服务端做，界面与后端逐行同序',
      nameRows.length > 1 &&
      JSON.stringify(nameRows) === JSON.stringify(nameResp.items.map(m => m.model_name)) &&
      nameResp.sort === 'name',
      `界面首行 ${nameRows[0]} 末行 ${nameRows[nameRows.length - 1]}，` +
      `后端首行 ${nameResp.items[0]?.model_name}`);

    // 按价格排序**要先选定价类型**：两种口径的数值区间重叠（实测按次 0.004~7
    // vs 倍率 0.01~175），跨口径排会把 $7/次 的视频模型排在"倍率 175"之前。
    // 所以未选口径时那一项必须是禁用的 —— 藏起来不行，藏了就没人知道有这功能。
    const priceOff = await page.$eval('#model-sort option[value="price"]', o => o.disabled);
    const segUnit = Object.keys(firstPage.units ?? {}).find(u => u !== 'unknown');
    await page.evaluate(u => {
      [...document.querySelectorAll('[data-munit]')]
        .find(b => b.getAttribute('data-munit') === u)?.click();
    }, segUnit);
    await sleep(900);
    const priceOn = await page.$eval('#model-sort option[value="price"]', o => o.disabled);
    await page.select('#model-sort', 'price');
    await sleep(900);
    const priceRows = await page.$$eval('[data-model-row]',
      rs => rs.map(r => r.getAttribute('data-model-row')));
    const priceResp = await api(
      `/admin/catalog?sort=price&unit=${encodeURIComponent(segUnit)}&limit=50`);
    check('按价格排序：选定价类型后才可用，且与后端逐行同序',
      priceOff === true && priceOn === false && priceRows.length > 1 &&
      JSON.stringify(priceRows) === JSON.stringify(priceResp.items.map(m => m.model_name)),
      `未选口径时禁用=${priceOff} 选了 ${segUnit} 之后禁用=${priceOn}；` +
      `界面首行 ${priceRows[0]} / 后端首行 ${priceResp.items[0]?.model_name}`);

    // 切走定价类型时排序要退回默认：后端在没有 unit 时也会把 price 降级，
    // 两边不一致的话界面显示"按价格"、拿到的却是按渠道数排的结果。
    await page.evaluate(() => {
      [...document.querySelectorAll('[data-munit]')]
        .find(b => b.getAttribute('data-munit') === '')?.click();
    });
    await sleep(900);
    const sortAfterUnitCleared = await page.$eval('#model-sort', s => s.value);
    check('清掉定价类型时「按价格」排序退回默认（与后端的降级一致）',
      sortAfterUnitCleared === '',
      `排序框现在是 ${JSON.stringify(sortAfterUnitCleared)}`);

    // ── 每页行数 ──
    await page.select('#model-pagesize', '20');
    await sleep(900);
    const paged = await page.evaluate(() => ({
      rows: document.querySelectorAll('[data-model-row]').length,
      foot: document.querySelector('#model-next')?.parentElement?.innerText
        .replace(/\s+/g, ' ').trim() ?? '',
    }));
    check('每页行数可调，翻页区的计数跟着改',
      paged.rows === 20 && /第 1–20 /.test(paged.foot),
      `${paged.rows} 行；页脚=${JSON.stringify(paged.foot)}`);
    await page.select('#model-pagesize', '50');
    await sleep(900);

    // ── 价格口径 /1M ↔ /1K ──
    //
    // 只改**显示**：同一个数除以 1000。所以判据是两次读到的数正好差一千倍，
    // 不是"点了之后文案变了" —— 后者在把 /1K 实现成"另取一列价格"时照样绿。
    const priceCell = () => page.evaluate(() => {
      const cells = [...document.querySelectorAll('td[data-col="price"]')];
      for (const c of cells) {
        const m = c.textContent.replace(/\s+/g, ' ').match(/\$([\d.]+)[^$]*?(\/1[MK] token|\/次)/);
        if (m !== null) return { n: Number(m[1]), suffix: m[2], row: c.closest('tr')
          ?.getAttribute('data-model-row') };
      }
      return null;
    });
    const per1M = await priceCell();
    await page.click('[data-seg="1K"]');
    await sleep(400);
    const per1K = await priceCell();
    const savedUnit = await page.evaluate(() => localStorage.getItem('sla.models.priceUnit'));
    check('价格可切 /1M ↔ /1K，且两者正好差一千倍（同一个数，不是另一套价）',
      per1M !== null && per1K !== null && per1M.row === per1K.row &&
      per1M.suffix === '/1M token' && per1K.suffix === '/1K token' &&
      Math.abs(per1K.n - per1M.n / 1000) <= 1e-6 && savedUnit === '1K',
      `${per1M?.row}：${per1M?.n}${per1M?.suffix} → ${per1K?.n}${per1K?.suffix}，` +
      `本地偏好=${savedUnit}`);
    await page.click('[data-seg="1M"]');
    await sleep(300);

    // 筛选：收敛到命中行，且写进 URL（把"谁家有 X"的链接发给别人要能复现）。
    await page.click('#model-q');
    await page.type('#model-q', 'claude');
    await sleep(900);
    const filtered = await page.$$eval('[data-model-row]',
      rs => rs.map(r => r.getAttribute('data-model-row')));
    const urlNow = await page.evaluate(() => location.search);
    check('模型目录筛选收敛到命中行并写进 URL',
      filtered.length > 0 && filtered.every(n => n.toLowerCase().includes('claude')) &&
      /q=claude/.test(urlNow),
      `${filtered.length} 行，URL=${urlNow}`);
    await page.screenshot({ path: `${SHOT}/13-models.png`, fullPage: true });

    // ── 卡片模式 + 疏密 + 抽屉 ──
    //
    // 两种视图渲染的是**同一份 items**，所以卡片数必须等于列表行数 ——
    // 不等就说明某一边自己又筛了一道（或者少渲染了），而两边分开看都正常。
    const listRows = filtered.length;
    await page.click('[data-seg="card"]');
    await page.waitForFunction(
      () => document.querySelectorAll('[data-model-card]').length > 0, { timeout: 5000 });
    const cards = await page.$$eval('[data-model-card]',
      cs => cs.map(c => c.getAttribute('data-model-card')));
    const viewURL = await page.evaluate(() => location.search);
    const viewSaved = await page.evaluate(() => localStorage.getItem('sla.models.view'));
    check('模型目录可切卡片模式，卡片与列表同一份数据且视图写进 URL 与本地偏好',
      cards.length === listRows &&
      JSON.stringify(cards) === JSON.stringify(filtered) &&
      /view=card/.test(viewURL) && viewSaved === 'card',
      `卡片 ${cards.length} 张 / 列表 ${listRows} 行，URL=${viewURL} 本地=${viewSaved}`);

    // 疏密：量**渲染出来的列宽**，不是"有没有设上那个 class/属性"。
    // 后者在 CSS 变量名写错、或 grid 用了别的列宽来源时照样绿。
    const widthAt = async d => {
      await page.click(`[data-seg="${d}"]`);
      await sleep(300);
      return page.evaluate(() => ({
        card: Math.round(document.querySelector('.mcard').getBoundingClientRect().width),
        cols: getComputedStyle(document.querySelector('.mcards'))
          .gridTemplateColumns.split(' ').length,
        saved: localStorage.getItem('sla.models.density'),
      }));
    };
    const tight = await widthAt('tight');
    const loose = await widthAt('loose');
    const normal = await widthAt('normal');
    check('卡片疏密三档真的改变了列宽与列数（不是只改了个属性）',
      tight.card < normal.card && normal.card < loose.card &&
      tight.cols > normal.cols && normal.cols > loose.cols &&
      normal.saved === 'normal',
      `紧凑 ${tight.card}px/${tight.cols} 列 · 标准 ${normal.card}px/${normal.cols} 列 · ` +
      `宽松 ${loose.card}px/${loose.cols} 列`);

    // 卡片点开的也是同一个抽屉。
    const cardName = cards[0];
    await page.evaluate(n => {
      [...document.querySelectorAll('[data-model-card]')]
        .find(c => c.getAttribute('data-model-card') === n)?.click();
    }, cardName);
    await page.waitForSelector('.drawer [data-model-channels]', { timeout: 5000 });
    const cardDrawer = await readDrawer();
    const wantDrawer = firstPage.items.find(m => m.model_name === cardName)
      ?? (await api(`/admin/catalog?q=${encodeURIComponent(cardName)}&limit=1`)).items[0];
    check('点卡片打开的是同一个宽抽屉，内容是该模型的逐渠道明细',
      cardDrawer.title === cardName && cardDrawer.forModel === cardName && cardDrawer.wide &&
      JSON.stringify(cardDrawer.chNames) ===
        JSON.stringify((wantDrawer?.channels ?? []).map(c => c.channel_name)),
      `抽屉标题=${cardDrawer.title} 表=${cardDrawer.forModel} ` +
      `渠道=${JSON.stringify(cardDrawer.chNames)}`);

    // Esc 关得掉。抽屉盖着半屏，只能靠那个 ✕ 的话，键盘用户走不出去。
    await closeDrawer();
    check('抽屉可用 Esc 关闭',
      await page.evaluate(() => document.querySelector('.drawer') === null));

    await page.screenshot({ path: `${SHOT}/13b-models-card.png`, fullPage: true });
    // 收尾恢复列表模式：本地偏好会被下一次跑读到，留着 card 会让上面那条
    // "默认列表"的前提在第二次运行时不成立。
    await page.click('[data-seg="list"]');
    await sleep(300);

    // ── 按 Key 筛：「我这把 Key 能调什么、按什么倍率计费」──
    //
    // 这一维与别的分面不同：它筛的不是目录行的字段，而是 Key 所在分组的可用
    // 模型清单（group_models）。所以它有一种别的维度没有的失败态 ——
    // **未归组的 Key**：上游 /api/token 的 group 是空串（站点用 auto_groups 在
    // 调用时自动选组），或它声明的分组不在我方采到的分组里。实测 2026-09-14
    // 两个真站点导入的 6 把 Key 里有 3 把是这样。
    //
    // 对这种 Key，界面必须**不让选**而不是筛出一个空列表：后端按定义匹配不到
    // 任何模型，一个空列表解释不了原因，而"筛坏了"与"确实没有"看起来一样。
    // 现造一把**未归组**的 Key，否则上面那半条断言在本环境里是空转的：
    // 前面那段给每把 Key 都指派了分组，于是"未归组的必须禁用"永远没有样本。
    // 这不是"造假数据" —— 未归组是真站点上的常态（实测两个站导入的 6 把 Key
    // 里有 3 把如此：上游 group 是空串、或分组不在我方采到的清单里），
    // 而真站点不肯按需产出一把这样的 Key 给验收用。
    //
    // ⚠️ account_id 要 **Number()**：accountIDs 来自 getAttribute，是字符串，
    // 而后端那个字段是 int64 —— 传字符串会 400，且错误只体现为"建不出来"。
    const ungroupedKey = await page.evaluate(async acct => {
      const t = localStorage.getItem('adminToken');
      const r = await fetch('/admin/keys', {
        method: 'POST',
        headers: { Authorization: `Bearer ${t}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ account_id: acct, secret: 'sk-ungrouped-for-facet-check-0001' }),
      });
      return { status: r.status, body: await r.json() };
    }, Number(accountIDs[0]));
    const ungroupedKeyID = ungroupedKey.body?.id;
    check('可以登记一把未归组的 Key（给下面两条的禁用态当样本）',
      typeof ungroupedKeyID === 'number',
      `HTTP ${ungroupedKey.status} ${JSON.stringify(ungroupedKey.body).slice(0, 120)}`);

    await page.click('#model-reset');
    await sleep(900);
    // 分面读的是共享 store 里的 Key 列表，刚建的那把要先拉回来才看得见
    await pane('keys');
    await page.click('#btn-keys-reload').catch(() => {});
    await sleep(700);
    await pane('models');
    await sleep(500);
    // Key 也进了弹窗：开一次读出每一行，再关掉。行里带分组与倍率，
    // 解析不出分组的那些标成「筛不了」。
    await page.click('#model-f-key');
    await page.waitForSelector('.picker', { visible: true, timeout: 5000 });
    // 四处浮层共用同一个 .drawer-scrim，所以这里再验一次不是重复 ——
    // 它证明的是"共用"这件事本身：哪天谁给弹窗单写一套遮罩，这条会红。
    const pickerScrim = await scrimStyle();
    check('选择器弹窗的遮罩同样是毛玻璃（四处浮层共用一套遮罩）',
      scrimOK(pickerScrim), JSON.stringify(pickerScrim));
    const keyChips = await page.$$eval('.picker [data-pick-row]', rs => rs.map(r => ({
      id: r.getAttribute('data-pick-row'),
      disabled: /筛不了/.test(r.innerText),
      text: r.innerText.replace(/\s+/g, ' ').trim(),
    })));
    await page.keyboard.press('Escape');
    await page.waitForFunction(
      () => document.querySelector('.picker') === null, { timeout: 5000 }).catch(() => {});
    const allKeys = (await api('/admin/keys')).items ?? [];
    const groupedKeys = allKeys.filter(k => k.group_ref !== undefined && k.group_ref !== '');
    const ungroupedKeys = allKeys.filter(k => k.group_ref === undefined || k.group_ref === '');
    const chipOf = id => keyChips.find(c => c.id === String(id));
    check('「按 Key」分面：解析得出分组的可选、解析不出的禁用并标出来',
      groupedKeys.length > 0 &&
      groupedKeys.every(k => chipOf(k.id)?.disabled === false &&
        chipOf(k.id)?.text.includes(k.group_ref)) &&
      ungroupedKeys.every(k => chipOf(k.id)?.disabled === true &&
        chipOf(k.id)?.text.includes('未归组')),
      `解析得出 ${groupedKeys.length} 把 / 解析不出 ${ungroupedKeys.length} 把；` +
      `chip=${JSON.stringify(keyChips.map(c => `${c.text}${c.disabled ? '(禁用)' : ''}`))}`);

    // auto 是**选组模式**不是可计费分组：实际倍率由运行时命中的候选组决定
    // （NewAPI 源码显式排除 auto 自己）。而 /api/pricing 的 group_ratio 照样
    // 给它一个倍率 —— 照着显示就是报一个与账单无关、却看起来完全正常的数字。
    //
    // 验收环境里恰好有一把 Key 被指派到 auto（那是该站真实存在的分组）。
    const autoKey = allKeys.find(k => k.group_ref === 'auto');
    if (autoKey === undefined) {
      check('分组为 auto 的 Key 显示「自动选组」而不是一个倍率数字', false,
        '这一轮没有落在 auto 分组的 Key —— 这条无从验');
    } else {
      const autoChip = chipOf(autoKey.id)?.text ?? '';
      // 「动态倍率 ×a~b」里也有 × 和数字，所以判据不能是"没有 ×数字"，
      // 而是**那个数不是 auto 自己那个占位倍率**。区间由后端给的候选区间来对。
      const wantRange = autoKey.rate_min === undefined || autoKey.rate_max === undefined
        ? '动态倍率'
        : autoKey.rate_min === autoKey.rate_max
          ? `动态倍率 ×${autoKey.rate_min}`
          : `动态倍率 ×${autoKey.rate_min}~${autoKey.rate_max}`;
      check('分组为 auto 的 Key 显示动态倍率区间，而不是它那个占位的固定倍率',
        autoKey.rate_dynamic === true && autoChip.includes(wantRange) &&
        !autoChip.includes(`×${autoKey.rate_multiplier} `),
        `chip=${JSON.stringify(autoChip)} 期望含 ${JSON.stringify(wantRange)}` +
        `（后端给的占位 rate_multiplier=${autoKey.rate_multiplier}，` +
        `候选区间 ${autoKey.rate_min}~${autoKey.rate_max}）`);
    }

    // 刚建的那把 Key **自己没有分组**，但账号有默认分组（上游 /api/user/self
    // 的 group，实测两个真站点都是 "default"）—— 所以它不是"未归组"，
    // 而是"跟账号走"：调用实际走账号那一档，倍率与可调模型都是确定的。
    //
    // 这条守的是 027 那一路：不采账号分组的话，这种 Key 会被永远显示成
    // 未归组、倍率未知，而它其实有明确的倍率（实测站 A 的 default 是 ×1）。
    const inherited = allKeys.find(k => k.id === ungroupedKeyID);
    const accountGroup = (await api('/admin/accounts')).items
      ?.find(a => a.id === Number(accountIDs[0]))?.account_group;
    if (inherited === undefined || (accountGroup ?? '') === '') {
      check('自己没分组的 Key 回落到账号的默认分组，并标「跟账号」', false,
        `账号默认分组=${JSON.stringify(accountGroup)} —— ` +
        '上游没给账号分组（或它不在该站 group_ratio 里）时这条无从验');
    } else {
      check('自己没分组的 Key 回落到账号的默认分组，并标「跟账号」',
        inherited.group_ref === accountGroup && inherited.group_inherited === true &&
        inherited.channel_group_id === undefined &&
        // ⚠️ 没写分组 **不等于** 动态倍率：它走账号的默认分组，那通常是个
        // 有固定倍率的普通组。把两者混成一档会让一个确定的倍率被说成"动态"。
        inherited.rate_dynamic === false && inherited.rate_multiplier !== undefined &&
        chipOf(inherited.id)?.disabled === false &&
        chipOf(inherited.id)?.text.includes('跟账号'),
        `Key #${inherited.id}：自己的分组=${JSON.stringify(inherited.channel_group_id)} ` +
        `解析出=${inherited.group_ref}（账号默认 ${accountGroup}）` +
        `×${inherited.rate_multiplier} inherited=${inherited.group_inherited} ` +
        `chip=${JSON.stringify(chipOf(inherited.id)?.text)}`);
    }

    if (groupedKeys.length === 0) {
      check('按 Key 筛出的模型 == 该 Key 所在分组能调的模型', false,
        '这一轮没有已归组的 Key —— 这条无从验（上游的 group 为空串时我方不替它猜分组）');
    } else {
      const theKey = groupedKeys[0];
      await pick('model-f-key', [theKey.id], true);
      await sleep(1000);
      const keyResp = await api(`/admin/catalog?key_id=${theKey.id}&limit=50`);
      const keyRows = await page.$$eval('[data-model-row]',
        rs => rs.map(r => r.getAttribute('data-model-row')));
      const keyURL = await page.evaluate(() => location.search);
      check('按 Key 筛出的模型 == 该 Key 所在分组能调的模型',
        keyResp.total > 0 &&
        JSON.stringify(keyRows) === JSON.stringify(keyResp.items.map(m => m.model_name)) &&
        new RegExp(`key=${theKey.id}`).test(keyURL),
        `Key #${theKey.id}（分组 ${theKey.group_ref} ×${theKey.rate_multiplier}）：` +
        `界面 ${keyRows.length} 行 / 后端 ${keyResp.total} 个模型，URL=${keyURL}`);

      // 按 Key 筛时，「实际价」只能用**这把 Key 自己那个分组**的倍率 ——
      // 不是全部分组里最低的那个。留着别的分组，那句"最低 · 分组 X"里的 X
      // 会是一个他根本用不上的分组：一个精确且无关的数字。
      const firstModel = keyResp.items[0];
      await openDrawerFor(firstModel.model_name);
      const keyDrawer = await readDrawer();
      const groupsShown = await page.evaluate(n => {
        const t = [...document.querySelectorAll('[data-model-channels]')]
          .find(x => x.getAttribute('data-model-channels') === n);
        return [...t.querySelectorAll('tbody tr td[data-model-eff]')]
          .map(td => td.textContent.replace(/\s+/g, ' ').trim());
      }, firstModel.model_name);
      const price = firstModel.channels[0]?.input_price;
      const wantEff = price === undefined
        ? '未采到分组倍率'
        : String(Math.round(price * theKey.rate_multiplier * 1e6) / 1e6);
      check('按 Key 筛时「实际价」只按该 Key 的分组折算（不是全部分组的最低价）',
        keyDrawer.eff.length === 1 && keyDrawer.eff[0] === wantEff &&
        groupsShown.every(t => t.includes(theKey.group_ref)) &&
        groupsShown.every(t => !t.includes('共')),
        `${firstModel.model_name} 模型价=${price} × 分组 ${theKey.group_ref} ` +
        `${theKey.rate_multiplier} → 期望 ${wantEff}，实际 ${JSON.stringify(keyDrawer.eff)}；` +
        `整格=${JSON.stringify(groupsShown)}`);
      // 倍率折成**绝对美元价**：`模型倍率 × 分组倍率 × 1e6 / quota_per_unit`。
      //
      // 倍率跨站点不可比 —— 同一个"×1"，在 quota_per_unit=500000 的站上是
      // $2/1M token，在 250000 的站上就是 $4，而界面上两个都写着 ×1。
      // 这正是"有些中转站把倍率设成 ×1 但实际是官方 0.5 或 2 倍"的来源。
      // 期望由后端给的 quota_per_unit 现算，**不写死 ×2**。
      const qpu = firstModel.channels[0]?.quota_per_unit;
      const unit = firstModel.channels[0]?.billing_unit;
      if (qpu === undefined || unit !== 'per_1m_token') {
        check('倍率按站点的 quota_per_unit 折成绝对美元价', false,
          `该渠道 quota_per_unit=${qpu} 口径=${unit} —— 这条无从验`);
      } else {
        const wantUSD = Math.round(price * theKey.rate_multiplier * 1e6 / qpu * 1e6) / 1e6;
        const usdShown = await page.evaluate(n => {
          const t = [...document.querySelectorAll('[data-model-channels]')]
            .find(x => x.getAttribute('data-model-channels') === n);
          return [...t.querySelectorAll('tbody tr td[data-model-eff]')]
            .map(td => (td.textContent.match(/≈\s*\$([\d.]+)/) ?? [])[1] ?? null);
        }, firstModel.model_name);
        check('倍率按站点的 quota_per_unit 折成绝对美元价',
          usdShown.length === 1 && Number(usdShown[0]) === wantUSD,
          `${firstModel.model_name}：倍率 ${price}×${theKey.rate_multiplier} ` +
          `× 1e6/${qpu} → 期望 $${wantUSD}/1M，界面 ${JSON.stringify(usdShown)}`);
      }
      await closeDrawer();

      // ── 「按 Key」跟着上游渠道走 ──
      //
      // 选了渠道之后，别的渠道的 Key 留在候选里是**必然筛出空**的：那把 Key 的
      // 分组属于另一个渠道，与"这几个渠道有什么模型"求交集恒为零。而空结果
      // 看不出原因，人会以为是目录漏了。所以两件事都要 ——
      // 候选收窄，**已选的越界 Key 一并丢掉**。后者不做的话，界面上是
      // "已选 1 把"配一个空列表，而那一把在候选里已经看不见了，取消都取消不掉。
      //
      // ⚠️ 先给**第二个渠道**登记一把 Key，否则这条是空转的：这一轮真库里
      // 4 把 Key 全在渠道 1 上，于是"收窄到渠道 2"的期望是空集，而候选也是空集
      // —— `[] === []` 恒真，那半条断言什么都没验到。造一把假明文的 Key 属于
      // 已认定的例外（同下面那把未归组的 Key：要验的是界面行为，拿真 Key 试
      // 等于把真凭证写进 DOM 快照与 CI 日志）。
      const ch2Account = ((await api('/admin/accounts')).items ?? [])
        .find(a => a.channel_id === ch2.id);
      const ch2Key = await page.evaluate(async acct => {
        const t = localStorage.getItem('adminToken');
        const r = await fetch('/admin/keys', {
          method: 'POST',
          headers: { Authorization: `Bearer ${t}`, 'Content-Type': 'application/json' },
          body: JSON.stringify({ account_id: acct, secret: 'sk-second-channel-scope-check-1' }),
        });
        return { status: r.status, body: await r.json() };
      }, Number(ch2Account?.id ?? 0));
      check('可以在第二个渠道上登记一把 Key（给下面那条收窄断言当样本）',
        typeof ch2Key.body?.id === 'number',
        `账号 #${ch2Account?.id} → HTTP ${ch2Key.status} ` +
        `${JSON.stringify(ch2Key.body).slice(0, 120)}`);
      // 分面读的是共享 store 里的 Key 列表，刚建的那把要先拉回来才看得见
      await pane('keys');
      await page.click('#btn-keys-reload').catch(() => {});
      await sleep(700);
      await pane('models');
      await sleep(500);
      const keysNow = (await api('/admin/keys')).items ?? [];
      const keysOfCh2 = keysNow.filter(k => k.channel_id === ch2.id).map(k => k.id);
      await pick('model-f-channel', [ch2.id], true);
      await sleep(900);
      const scoped = await page.evaluate(() => {
        const btn = document.querySelector('#model-f-key');
        return {
          url: location.search,
          picked: (btn?.dataset.picked ?? '').split(',').filter(x => x !== ''),
          disabled: btn?.disabled === true,
          note: btn?.closest('.mfilter')?.querySelector('.mfilter-n')?.textContent.trim() ?? '',
        };
      });
      // 候选里到底剩哪几把：开一次弹窗读行（范围内一把都没有时它是禁用的，开不了）
      let scopedRows = [];
      if (!scoped.disabled) {
        await page.click('#model-f-key');
        await page.waitForSelector('.picker', { visible: true, timeout: 5000 });
        scopedRows = await page.$$eval('.picker [data-pick-row]',
          rs => rs.map(r => Number(r.getAttribute('data-pick-row'))));
        await page.keyboard.press('Escape');
        await page.waitForFunction(
          () => document.querySelector('.picker') === null, { timeout: 5000 }).catch(() => {});
      }
      const sortNum = a => [...a].sort((x, y) => x - y);
      check('限定上游渠道后，可选的 Key 收窄到该渠道，已选的越界 Key 一并丢掉',
        // 收窄必须是**真子集**，否则这条是空转的（见上面那把现造的 Key）
        keysOfCh2.length > 0 && keysOfCh2.length < keysNow.length &&
        JSON.stringify(sortNum(scopedRows)) === JSON.stringify(sortNum(keysOfCh2)) &&
        scoped.picked.length === 0 && !/key=/.test(scoped.url) &&
        scoped.disabled === (keysOfCh2.length === 0),
        `渠道 ${ch2.id} 下后端有 ${keysOfCh2.length} 把 Key ${JSON.stringify(sortNum(keysOfCh2))}，` +
        `候选列出 ${JSON.stringify(sortNum(scopedRows))}；` +
        `原先选中 Key #${theKey.id}，收窄后选中=${JSON.stringify(scoped.picked)} ` +
        `URL=${scoped.url} 选择器禁用=${scoped.disabled} 提示=${JSON.stringify(scoped.note)}`);
      // 反过来：清掉渠道筛选，候选必须**回到全部**（否则选一次渠道就再也换不回来）
      await page.click('#model-reset');
      await sleep(900);
      await page.click('#model-f-key');
      await page.waitForSelector('.picker', { visible: true, timeout: 5000 });
      const allRows = await page.$$eval('.picker [data-pick-row]',
        rs => rs.map(r => Number(r.getAttribute('data-pick-row'))));
      await page.keyboard.press('Escape');
      await page.waitForFunction(
        () => document.querySelector('.picker') === null, { timeout: 5000 }).catch(() => {});
      check('清掉渠道筛选后，可选的 Key 回到全部（不是留在上次那个范围里）',
        JSON.stringify(sortNum(allRows)) === JSON.stringify(sortNum(keysNow.map(k => k.id))),
        `候选 ${JSON.stringify(sortNum(allRows))} / 全部 ${JSON.stringify(sortNum(keysNow.map(k => k.id)))}`);
      // 后面那条要从 Key 页跳过来，这里先把筛选恢复成"只筛这把 Key"
      await pick('model-f-key', [theKey.id], true);
      await sleep(900);

      // 选中之后按钮上是一串长摘要（前缀 +（渠道名）），正是最容易把左栏顶宽的
      // 那一下。量 scrollWidth 而不是看着像不像：左栏有 overflow:auto，溢出时
      // 它不是换行而是**被裁掉** —— 右边的 chip 只剩半个，看起来像少渲染了一块。
      const sideFit = await page.evaluate(() => {
        const el = document.querySelector('.mside');
        return {
          scrollW: el.scrollWidth, clientW: el.clientWidth,
          summary: document.querySelector('#model-f-key .picker-sum')?.textContent.trim() ?? '',
        };
      });
      check('左栏筛选不横向溢出（长 Key 摘要不把整段顶宽）',
        sideFit.scrollW <= sideFit.clientW + 1 && sideFit.summary !== '',
        `scrollWidth=${sideFit.scrollW} clientWidth=${sideFit.clientW}，` +
        `按钮摘要=${JSON.stringify(sideFit.summary)}`);

      // ── 反方向的入口：Key 管理页 →「能调哪些模型」──
      //
      // 这条同时守住一个刚修的缺陷：ModelsView 的 onMounted 里 `cat.open()` 会
      // 清空 store，而清空触发 watch→syncURL，把刚 push 进来的 ?key= 抹掉 ——
      // 于是"从 Key 页跳过来筛选没生效，直接刷新同一个地址却好使"。
      // 所以这条必须**从 Key 页点过去**，不能只验直接访问带参数的地址。
      await pane('keys');
      await page.waitForFunction(
        () => document.querySelectorAll('[data-key-models]').length > 0, { timeout: 8000 });
      const modelBtns = await page.$$eval('[data-key-models]', bs => bs.map(b => ({
        id: b.getAttribute('data-key-models'), disabled: b.disabled, title: b.title,
      })));
      const btnOf = id => modelBtns.find(b => b.id === String(id));
      check('Key 管理页每行有「能调哪些模型」，未归组的禁用并说明原因',
        groupedKeys.every(k => btnOf(k.id)?.disabled === false) &&
        ungroupedKeys.every(k => btnOf(k.id)?.disabled === true &&
          /未归组/.test(btnOf(k.id)?.title ?? '')),
        `按钮 ${modelBtns.length} 个，禁用 ${modelBtns.filter(b => b.disabled).length} 个` +
        `（未归组 ${ungroupedKeys.length} 把）`);

      await openMore(`[data-key-more="${theKey.id}"]`);
      await page.evaluate(id => {
        [...document.querySelectorAll('[data-key-models]')]
          .find(b => b.getAttribute('data-key-models') === String(id))?.click();
      }, theKey.id);
      await page.waitForFunction(
        () => document.querySelector('#pane-models')?.classList.contains('on') === true,
        { timeout: 8000 });
      await sleep(1200);
      const jumped = await page.evaluate(() => ({
        url: location.pathname + location.search,
        count: document.querySelector('#model-count')?.textContent.trim(),
        on: (document.querySelector('#model-f-key')?.dataset.picked ?? '')
          .split(',').filter(x => x !== ''),
      }));
      check('从 Key 管理页跳到模型目录时筛选真的带过去了（不是被自己抹掉）',
        new RegExp(`key=${theKey.id}`).test(jumped.url) &&
        JSON.stringify(jumped.on) === JSON.stringify([String(theKey.id)]) &&
        jumped.count === `${keyResp.total} / ${keyResp.total}`,
        `URL=${jumped.url} 选中=${JSON.stringify(jumped.on)} 计数=${jumped.count}` +
        `（后端 ${keyResp.total}）`);
      await page.screenshot({ path: `${SHOT}/13c-models-by-key.png`, fullPage: true });
      await page.click('#model-reset');
      await sleep(900);
    }
  }

  // ── 13. 页面无 JS 错误 ──
  // 只看真正的脚本错误：429（限流）与 422（5bis 故意的缺凭证采集）都是
  // 本脚本自己触发的断言，favicon 404 是浏览器自动请求 —— 都不是页面缺陷。
  // /admin/hub-sync/run 同理：12ter 里有两轮**故意**失败的同步（地址不通、
  // 密码错），按**这个 URL** 放行而不是按它的状态码放行 —— 后者会把别处真的
  // 5xx 一起放过去。
  const realErrors = consoleErrors.filter(e =>
    !/429|422|favicon/.test(e) && !/hub-sync\/run/.test(e));
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
