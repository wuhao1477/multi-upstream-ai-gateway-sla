// Vue 重写引入的三条新性质，两份既有验收都没有覆盖：
//
// 1) history 路由刷新。旧版是单页面 + class 切换，任何路径刷新都还是那一个页面；
//    现在 /admin/ui/channels/1 是真实 URL，后端必须回 index.html 由前端路由接管。
//    这条不验的话，运维刷新一次就是 404，而验收脚本从不刷新非根路径。
// 2) 900px 断点。两份验收都在 1280/1440 宽下跑，侧栏折成顶部横排这条分支
//    从未被执行过 —— 而 sticky + height:100vh 忘了改成 static 的话，折行后
//    侧栏会占满整屏，主区被挤到首屏之外。
// 3) 点号开头的路径必须拒绝。webdist/.gitignore 是给 go:embed 用的占位文件
//    （模式匹配不到文件就是编译错误），它不该能被当静态资源取走。
import puppeteer from 'puppeteer-core';

const CHROME = process.env.CHROME ||
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const BASE = process.env.BASE || 'http://127.0.0.1:18390';

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

  // ── 1. 带尾斜杠与不带尾斜杠都能开到同一个分栏 ──
  await page.setViewport({ width: 1280, height: 1000 });
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

  // ── 5. 未知子路径回渠道列表，而不是白屏 ──
  await page.goto(`${BASE}/admin/ui/no-such-pane`, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() =>
    document.querySelector('#pane-channels')?.classList.contains('on') === true,
    { timeout: 5000 });
  const fallbackURL = await page.evaluate(() => location.pathname);
  check('未知子路径回落到渠道列表', fallbackURL === '/admin/ui/', fallbackURL);

  // ── 6. 900px 断点：侧栏折成顶部横排，且不占满整屏 ──
  await page.setViewport({ width: 480, height: 900 });
  await page.goto(`${BASE}/admin/ui`, { waitUntil: 'domcontentloaded' });
  const narrow = await page.evaluate(() => {
    const s = document.querySelector('.sidebar').getBoundingClientRect();
    const m = document.querySelector('.main').getBoundingClientRect();
    return {
      sBottom: Math.round(s.bottom), sHeight: Math.round(s.height),
      mTop: Math.round(m.top),
      pos: getComputedStyle(document.querySelector('.sidebar')).position,
      docW: document.documentElement.scrollWidth,
      winW: window.innerWidth,
    };
  });
  check('窄屏下侧栏折到主区上方（不再左右并排）',
    narrow.mTop >= narrow.sBottom - 2,
    `侧栏 bottom=${narrow.sBottom}，主区 top=${narrow.mTop}`);
  // 这一条是 sticky/height:100vh 忘了改的直接症状：侧栏高度吃掉整个视口
  check('窄屏下侧栏不占满整屏（position 已改为 static）',
    narrow.pos === 'static' && narrow.sHeight < 400,
    `position=${narrow.pos} 高=${narrow.sHeight}`);
  check('窄屏下无横向溢出', narrow.docW <= narrow.winW + 1,
    `scrollWidth=${narrow.docW} innerWidth=${narrow.winW}`);

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
