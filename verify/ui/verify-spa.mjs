// Vue 重写引入的三条新性质，两份既有验收都没有覆盖：
//
// 1) history 路由刷新。旧版是单页面 + class 切换，任何路径刷新都还是那一个页面；
//    现在 /admin/ui/channels/1 是真实 URL，后端必须回 index.html 由前端路由接管。
//    这条不验的话，运维刷新一次就是 404，而验收脚本从不刷新非根路径。
// 2) 窄屏断点。两份验收的主体都在 1280/1440 宽下跑，移动端那几支 CSS 从未被
//    执行过 —— 而 sticky + height:100vh 忘了改成 static 的话，折行后侧栏会占满
//    整屏，主区被挤到首屏之外。2026-09-16 起这一段跑在 375px（手机那一档），
//    覆盖骨架、导航、浮层与控件尺寸；**表格摊成卡片那几条验不了**（空库没有
//    行可摊），它们在 verify-ui.mjs 里对真数据跑。
// 3) 点号开头的路径必须拒绝。webdist/.gitignore 是给 go:embed 用的占位文件
//    （模式匹配不到文件就是编译错误），它不该能被当静态资源取走。
import puppeteer from 'puppeteer-core';

const CHROME = process.env.CHROME ||
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const BASE = process.env.BASE || 'http://127.0.0.1:18390';
// 本地起的那个 core 自己的 ADMIN_TOKEN（ui-stack.sh 生成并注入）。
// 这不是第三方凭证 —— 那些永远不进 CI（CLAUDE.md §1 的"后果"一节）。
const TOKEN = process.env.ADMIN_TOKEN || 'dev-ui-token';

const results = [];
function check(name, ok, detail = '') {
  results.push({ name, ok, detail });
  console.log(`${ok ? '✅' : '❌'} ${name}${detail ? ' — ' + detail : ''}`);
  if (!ok) process.exitCode = 1;
}

const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: 'shell',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
  // puppeteer 默认只等 30s 拿 WS 端点。GitHub runner 上实测不够：
  // 2026-09-01 gate run #33438992013 红在 "Timed out after 30000 ms while
  // waiting for the WS endpoint URL"，而同一个 runner 镜像五分钟前那轮
  // （#33438576085）同一步是绿的 —— 即慢，不是不兼容。90s 只在真慢时才花掉。
  timeout: 90_000,
});

try {
  const page = await browser.newPage();
  const consoleErrors = [];
  page.on('console', m => { if (m.type() === 'error') consoleErrors.push(m.text()); });
  page.on('pageerror', e => consoleErrors.push('pageerror: ' + e.message));

  // ── 0. 没令牌先去登录页，登录后才有下面这些分栏 ──
  //
  // 这一条在 CI 里也跑（免密：用的是本地 core 自己的令牌），所以登录这条路
  // 每次提交都有人走一遍 —— 它是现在**唯一**的入口，坏了整个界面就进不去。
  await page.setViewport({ width: 1280, height: 1000 });
  await page.goto(`${BASE}/admin/ui/import`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#pane-login', { timeout: 8000 });
  const gate = await page.evaluate(() => ({
    url: location.pathname + location.search,
    fields: [...document.querySelectorAll('.login-box input')].map(i => i.id),
  }));
  check('没令牌时任何分栏都先落到登录页（带上原地址）',
    gate.url === '/admin/ui/login?next=/import' &&
    JSON.stringify(gate.fields) === JSON.stringify(['token']),
    JSON.stringify(gate));
  await page.type('#token', TOKEN);
  await page.click('#login-submit');
  await page.waitForFunction(
    () => document.querySelector('#pane-import')?.classList.contains('on') === true,
    { timeout: 10000 });

  // ── 1. 带尾斜杠与不带尾斜杠都能开到同一个分栏 ──
  for (const path of ['/admin/ui', '/admin/ui/']) {
    const r = await page.goto(BASE + path, { waitUntil: 'domcontentloaded' });
    const on = await page.evaluate(() =>
      document.querySelector('#pane-channels')?.classList.contains('on') === true);
    check(`${path} 打开即落在渠道商分栏`, r.status() === 200 && on,
      `HTTP ${r.status()} pane-channels.on=${on}`);
  }

  // ── 2. 直接访问 / 刷新子路径：后端回 index.html，前端路由接管 ──
  //
  // 这里原先用 /admin/ui/creds。采集凭证并进账号页之后它成了重定向，
  // 重定向验不了"刷新后还在原路径" —— 换一个仍然存在的子路径来验这条性质，
  // 旧入口本身在下一条单独验。
  const r2 = await page.goto(`${BASE}/admin/ui/keys`, { waitUntil: 'domcontentloaded' });
  const keysOn = await page.evaluate(() =>
    document.querySelector('#pane-keys')?.classList.contains('on') === true);
  check('直接访问 /admin/ui/keys 命中 Key 分栏（history 刷新可用）',
    r2.status() === 200 && keysOn, `HTTP ${r2.status()} pane-keys.on=${keysOn}`);

  await page.reload({ waitUntil: 'domcontentloaded' });
  const stillKeys = await page.evaluate(() => ({
    on: document.querySelector('#pane-keys')?.classList.contains('on') === true,
    url: location.pathname,
  }));
  check('在子路径上刷新不丢分栏', stillKeys.on && stillKeys.url === '/admin/ui/keys',
    JSON.stringify(stillKeys));

  // ── 2bis. 旧的 /creds 收藏夹链接不许变成 404 或空白页 ──
  // 凭证是账号的属性（UNIQUE(account_id)），并进账号页；那一页顶上就有
  // 「缺采集凭证 N」与凭证筛选，所以落到账号页就是落到了对的地方。
  const rCreds = await page.goto(`${BASE}/admin/ui/creds`, { waitUntil: 'domcontentloaded' });
  const credsLanding = await page.evaluate(() => ({
    on: document.querySelector('#pane-accounts')?.classList.contains('on') === true,
    url: location.pathname,
    credFilter: document.querySelector('#acc-f-cred') !== null,
  }));
  check('旧入口 /admin/ui/creds 落到账号页（凭证已并入账号）',
    rCreds.status() === 200 && credsLanding.on &&
    credsLanding.url === '/admin/ui/accounts' && credsLanding.credFilter,
    `HTTP ${rCreds.status()} ${JSON.stringify(credsLanding)}`);

  // ── 3. 渠道详情是渠道管理的二级路由，不是侧栏一级项 ──
  const detailResp = await page.goto(`${BASE}/admin/ui/channels/1`, { waitUntil: 'domcontentloaded' });
  const detailState = await page.evaluate(() => ({
    on: document.querySelector('#pane-detail')?.classList.contains('on') === true,
    detailNav: document.querySelector('.nav-item[data-pane="detail"]') !== null,
    channelsActive: document.querySelector('.nav-item[data-pane="channels"]')?.classList.contains('active') === true,
  }));
  check('直接访问渠道详情二级路由', detailResp.status() === 200 && detailState.on,
    `HTTP ${detailResp.status()} pane-detail.on=${detailState.on}`);
  check('渠道详情不出现在侧栏一级菜单', !detailState.detailNav,
    `detailNav=${detailState.detailNav}`);
  check('渠道详情页面高亮渠道管理', detailState.channelsActive,
    `channelsActive=${detailState.channelsActive}`);

  // ── 3bis. 模型目录与控制台同级：顶层导航的另一半，不带侧栏 ──
  //
  // 走**直接访问地址**而不是点过去：这条要守的是"冷启动就该是这个形态"。
  // 点过去只能证明切换逻辑对，而收藏夹里的链接、别人发来的链接走的是这条路。
  const modelsResp = await page.goto(`${BASE}/admin/ui/models`, { waitUntil: 'domcontentloaded' });
  const modelsShell = await page.evaluate(() => ({
    on: document.querySelector('#pane-models')?.classList.contains('on') === true,
    sidebar: document.querySelector('.sidebar') !== null,
    shell: document.querySelector('.shell') !== null,
    topbar: document.querySelector('.topbar') !== null,
    topNav: [...document.querySelectorAll('[data-top-nav]')]
      .map(b => b.getAttribute('data-top-nav')),
    active: document.querySelector('[data-top-nav].on')?.getAttribute('data-top-nav'),
  }));
  check('直接访问模型目录：顶层导航高亮它，且控制台那层壳整个不渲染',
    modelsResp.status() === 200 && modelsShell.on &&
    !modelsShell.sidebar && !modelsShell.shell && !modelsShell.topbar &&
    JSON.stringify(modelsShell.topNav) === JSON.stringify(['console', 'models']) &&
    modelsShell.active === 'models',
    `HTTP ${modelsResp.status()} ${JSON.stringify(modelsShell)}`);
  // 回控制台那一侧，下面几条要用侧栏
  await page.goto(`${BASE}/admin/ui/channels/1`, { waitUntil: 'domcontentloaded' });

  // ── 4. 点导航会改地址栏（分栏可分享链接）──
  await page.click('.nav-item[data-pane="import"]');
  await page.waitForFunction(() =>
    document.querySelector('#pane-import')?.classList.contains('on') === true,
    { timeout: 5000 });
  const navURL = await page.evaluate(() => location.pathname);
  check('点侧栏导航同步更新地址栏', navURL === '/admin/ui/import', navURL);

  // ── 4bis. Key 自动化入口：免密 SPA 验收也必须覆盖新增按钮与安全默认值 ──
  const keysResp = await page.goto(`${BASE}/admin/ui/keys`, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() =>
    document.querySelector('#pane-keys')?.classList.contains('on') === true,
    { timeout: 5000 });
  const keyButtons = await page.$$eval('#btn-import-keys, #btn-provision-keys', bs => bs.map(b => b.id));
  check('Key 管理页显示同步与批量补齐入口',
    keysResp.status() === 200
      && keyButtons.includes('btn-import-keys')
      && keyButtons.includes('btn-provision-keys'),
    `HTTP ${keysResp.status()} ${keyButtons.join(', ')}`);

  await page.click('#btn-import-keys');
  await page.waitForFunction(() =>
    document.querySelector('.drawer-t')?.textContent.includes('同步已有 Key'),
    { timeout: 5000 });
  // 渠道/账号改成了选择器（ScopePicker）：触发按钮 + 列表弹窗，不再是 <select>。
  // 未选中时按钮上显示占位符，「同步已有 Key」的占位符就是那句要求。
  const importScope = await page.$eval('#key-auto-channel .picker-sum', el => el.textContent.trim());
  check('同步已有 Key 要求选择渠道', importScope === '请选择渠道', importScope);
  // 点开必须真的弹出列表。免密路径下库里没有渠道，所以断言停在"弹窗开了且
  // 明说没有可选项"——**不要**在这里断言行内容：SPA 这套跑的是空库，
  // 行的形态（域名、#id）只能由带真上游的那套验（CLAUDE.md §1）。
  await page.click('#key-auto-channel');
  await page.waitForSelector('.picker', { visible: true, timeout: 5000 });
  const pickerText = await page.$eval('.picker', el => el.innerText.replace(/\s+/g, ' '));
  check('渠道选择器点开即弹出列表弹窗',
    pickerText.includes('选择渠道') && pickerText.includes('按域名核对'), pickerText.slice(0, 80));
  await page.click('[data-pick-cancel]');
  await page.click('.drawer-x');

  await page.click('#btn-provision-keys');
  await page.waitForFunction(() =>
    document.querySelector('.drawer-t')?.textContent.includes('批量补齐 Key'),
    { timeout: 5000 });
  const onlyEmpty = await page.$eval('#key-auto-only-without-keys', input => input.checked);
  check('批量补齐默认仅无 Key 账号', onlyEmpty);
  await page.click('.drawer-x');

  // ── 4ter. 后台同步批次的三条契约（免密可验的那半）──
  //
  // 同步已有 Key 从「同步等结果」改成了「排队 + 轮询」。真正跑一批要真上游凭证，
  // 那部分在 verify-ui.mjs；这里守的是**与数据无关**的三条：端点在不在、空范围
  // 怎么拒、批次不在内存里时说不说得清。
  //
  // 第三条尤其要守：队列只在内存里，重启就没了。那时轮询拿到的 404 必须带原因 ——
  // 只回"不存在"会被读成"这个 id 是假的"，而事实是"它曾经在、被重启抹掉了"。
  const jobsAPI = async (path, init) => {
    const r = await fetch(`${BASE}${path}`, {
      ...init,
      headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json' },
    });
    return { status: r.status, body: await r.json().catch(() => ({})) };
  };
  const emptyJobs = await jobsAPI('/admin/keys/import/jobs');
  check('后台同步批次列表端点可用（空库时是空列表，不是 404）',
    emptyJobs.status === 200 && emptyJobs.body.count === 0,
    `HTTP ${emptyJobs.status} ${JSON.stringify(emptyJobs.body).slice(0, 60)}`);

  // 空库里 1 号渠道不存在，于是范围展开出 0 个账号。**在排队之前就拒**：
  // 排一个注定什么都不做的批次，界面上会显示一条 0/0 的进度，读起来像跑完了。
  const emptyScope = await jobsAPI('/admin/keys/import', {
    method: 'POST',
    body: JSON.stringify({ channel_ids: [1], account_ids: [], all: false }),
  });
  check('范围内没有账号时当场 400，不排一个空批次',
    emptyScope.status === 400 && /没有账号/.test(emptyScope.body.error ?? ''),
    `HTTP ${emptyScope.status} ${emptyScope.body.error ?? ''}`);

  const missingJob = await jobsAPI('/admin/keys/import/jobs/999');
  check('批次不在内存里时 404 要说清原因（重启过 / 被挤出历史）',
    missingJob.status === 404 && /重启|历史/.test(missingJob.body.error ?? ''),
    `HTTP ${missingJob.status} ${missingJob.body.error ?? ''}`);

  // ── 5. 未知子路径回渠道列表，而不是白屏 ──
  await page.goto(`${BASE}/admin/ui/no-such-pane`, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() =>
    document.querySelector('#pane-channels')?.classList.contains('on') === true,
    { timeout: 5000 });
  const fallbackURL = await page.evaluate(() => location.pathname);
  check('未知子路径回落到渠道列表', fallbackURL === '/admin/ui/', fallbackURL);

  // ── 6. 手机宽度（375px）：骨架、导航与输入控件 ──
  //
  // 375 而不是 390/414：iPhone SE 与 13 mini 是现役最窄的那一档，横向溢出先在
  // 这里出现，宽一点的机型是它的超集。原先这一段跑在 480px —— 那只碰得到
  // 900 那个断点，手机那一档（≤640，表格摊开、触摸目标放大、输入框 16px）
  // 从来没被执行过。
  //
  // ⚠️ 这一档**真正的主角（表格摊成卡片）验不了**：免密路径下库是空的，
  //    一行数据都没有，摊开与否无从谈起。那几条在 verify-ui.mjs 里，
  //    对真上游、真库跑，只在本地（CLAUDE.md §1 的 CI 表）。这里守的是
  //    与数据无关的那半：骨架、导航、浮层、控件尺寸。
  await page.setViewport({ width: 375, height: 812 });
  await page.goto(`${BASE}/admin/ui`, { waitUntil: 'domcontentloaded' });
  const narrow = await page.evaluate(() => {
    const box = s => document.querySelector(s).getBoundingClientRect();
    const sb = document.querySelector('.sidebar');
    const s = sb.getBoundingClientRect();
    return {
      sBottom: Math.round(s.bottom), sHeight: Math.round(s.height),
      mTop: Math.round(box('.main').top),
      pos: getComputedStyle(sb).position,
      sScroll: sb.scrollWidth, sClient: sb.clientWidth,
      hdrH: Math.round(box('.hdr').height),
      brandTop: Math.round(box('.hdr .brand').top),
      themeTop: Math.round(box('#theme-sw').top),
      topbarPos: getComputedStyle(document.querySelector('.topbar')).position,
      font: Math.round(parseFloat(getComputedStyle(
        document.querySelector('#ch-filter')).fontSize)),
      btnSm: Math.round(box('#btn-reload').height),
      docW: document.documentElement.scrollWidth,
      winW: window.innerWidth,
    };
  });
  check('手机上侧栏折到主区上方（不再左右并排）',
    narrow.mTop >= narrow.sBottom - 2,
    `侧栏 bottom=${narrow.sBottom}，主区 top=${narrow.mTop}`);
  // 这一条是 sticky/height:100vh 忘了改的直接症状：侧栏高度吃掉整个视口
  check('手机上侧栏不占满整屏（position 已改为 static）',
    narrow.pos === 'static' && narrow.sHeight < 400,
    `position=${narrow.pos} 高=${narrow.sHeight}`);
  // 四项折成两行要吃掉约 140px 首屏，而首屏是手机上最贵的东西。
  // 排成一条横着滚的带子：**高度是一行**，且确实滚得动（滚不动说明它又折行了）。
  check('手机上侧栏是一条横滚的导航带，不是折成两行',
    narrow.sHeight < 80 && narrow.sScroll > narrow.sClient,
    `高=${narrow.sHeight} 内容宽=${narrow.sScroll} 可视宽=${narrow.sClient}`);
  // 顶栏排不下就会折行，而它是吸顶的 —— 折一次首屏少 45px。
  // 判据不用高度一个数：品牌与主题开关的 top 相同才说明它们真在同一行上。
  check('手机上顶栏仍是一行（品牌与主题开关同排）',
    narrow.hdrH <= 72 && Math.abs(narrow.brandTop - narrow.themeTop) < 12,
    `顶栏高=${narrow.hdrH} 品牌 top=${narrow.brandTop} 主题 top=${narrow.themeTop}`);
  // 分栏抬头的 sticky top 是写死的 --hdr-h(60px)，而窄屏顶栏不一定是那个高度 ——
  // 吸错位置的症状是滚起来被顶栏盖掉半条，所以这一档直接不吸。
  check('手机上分栏抬头不吸顶（否则会吸在顶栏底下被盖住）',
    narrow.topbarPos === 'static', `position=${narrow.topbarPos}`);
  // iOS Safari 聚焦字号 <16px 的输入框时会整页放大，且放大后缩不回来。
  check('手机上输入框字号 ≥16px（小于它 iOS 聚焦时会整页放大）',
    narrow.font >= 16, `${narrow.font}px`);
  check('手机上行内小按钮的触摸目标 ≥34px', narrow.btnSm >= 34, `${narrow.btnSm}px`);
  check('手机上无横向溢出', narrow.docW <= narrow.winW + 1,
    `scrollWidth=${narrow.docW} innerWidth=${narrow.winW}`);

  // ── 6bis. 手机上每个分栏都不横向溢出 ──
  //
  // 逐个分栏走一遍而不是只看首页：溢出多半是某一页独有的那个控件撑出来的
  // （定宽的搜索框、写死 380px 的卡片网格、比屏幕还宽的提示条），
  // 只验首页等于只验了渠道列表那一页。
  for (const p of ['accounts', 'keys', 'import', 'models']) {
    await page.goto(`${BASE}/admin/ui/${p}`, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(
      n => document.querySelector('#pane-' + n)?.classList.contains('on') === true,
      { timeout: 8000 }, p);
    const w = await page.evaluate(() => ({
      docW: document.documentElement.scrollWidth, winW: window.innerWidth,
    }));
    check(`手机上 ${p} 分栏无横向溢出`, w.docW <= w.winW + 1,
      `scrollWidth=${w.docW} innerWidth=${w.winW}`);
  }

  // ── 6ter. 手机上抽屉铺满屏宽 ──
  //
  // 抽屉在宽屏是 460px 的右侧面板。这一条盯的是它有没有把那个定宽带进手机 ——
  // 带进来的表现不是"窄了一点"，是 460px 的面板从 375px 的屏幕右边探出去，
  // 整页跟着能横向滚。
  await page.goto(`${BASE}/admin/ui/keys`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#btn-import-keys', { timeout: 8000 });
  await page.click('#btn-import-keys');
  await page.waitForSelector('.drawer', { visible: true, timeout: 5000 });
  const drawer = await page.evaluate(() => ({
    w: Math.round(document.querySelector('.drawer').getBoundingClientRect().width),
    winW: window.innerWidth,
    docW: document.documentElement.scrollWidth,
  }));
  check('手机上抽屉铺满屏宽且不把页面撑宽',
    drawer.w === drawer.winW && drawer.docW <= drawer.winW + 1,
    JSON.stringify(drawer));
  await page.click('.drawer-x');

  // 控制台断言放在**探针之前**：下面那两条要故意打一个 404，
  // 若用页面内的 fetch 去打，浏览器会把它记进控制台，于是这条断言
  // 只能靠"把 404 也过滤掉"来通过 —— 那就等于把真的资源缺失也一起放行了。
  // 所以探针改走 Node 侧 fetch，完全不经过页面。
  const real = consoleErrors.filter(e => !/favicon/.test(e));
  check('无 JavaScript 错误', real.length === 0, real.slice(0, 2).join(' | ') || '无');

  // ── 6. 占位文件不可取，但真资源可取 ──
  const dot = await fetch(`${BASE}/admin/ui/.gitignore`);
  check('go:embed 占位文件不可通过 HTTP 取走', dot.status === 404, `HTTP ${dot.status}`);
  await dot.body?.cancel();
  const html = await (await fetch(`${BASE}/admin/ui/`)).text();
  const asset = html.match(/\/admin\/ui\/assets\/[^"]+\.js/)?.[0];
  const assetResp = asset ? await fetch(BASE + asset) : null;
  check('带 hash 的资源可正常取回', assetResp?.status === 200,
    `${asset} → HTTP ${assetResp?.status}`);
  // 长缓存是安全的**前提**是文件名带内容 hash：名字不变而内容变了的话，
  // 浏览器会拿着一年不过期的旧文件，发新版等于没发。
  check('带 hash 的资源发不可变长缓存',
    assetResp?.headers.get('cache-control')?.includes('immutable') === true,
    assetResp?.headers.get('cache-control') ?? '无');
  await assetResp?.body?.cancel();
  const idxResp = await fetch(`${BASE}/admin/ui`);
  check('index.html 不缓存（否则会引用已不存在的旧资源名）',
    idxResp.headers.get('cache-control') === 'no-store',
    idxResp.headers.get('cache-control') ?? '无');
  await idxResp.body?.cancel();
} finally {
  await browser.close();
}

const failed = results.filter(r => !r.ok);
console.log(`\n${'='.repeat(58)}`);
console.log(`SPA 路由与断点验收：${results.length - failed.length}/${results.length} 通过`);
failed.forEach(f => console.log(`  ❌ ${f.name} — ${f.detail}`));
