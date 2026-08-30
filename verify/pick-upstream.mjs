// 从 all-api-hub 导出里挑一个**当前真的能用**的 NewAPI 系上游,供验收使用。
//
// 存在的理由见 CLAUDE.md §1:验收不许构造假上游。于是"哪个真站点能用"这件事
// 必须在每次验收开始时现场探,不能写死在脚本里 —— 站点会挂、会限流、会换证书,
// 写死一个 URL 等于把假设又搬回来了一次。
//
// 探活是**只读**的,每个候选站最多两个请求:
//   GET /api/status      免鉴权,拿 quota_per_unit,同时确认是 NewAPI 系
//   GET /api/user/self   带凭证,确认 token 还活着,并试出用户 ID 头名
//   GET /api/token       取一个真 token id,给 external_ref 用
// 第一个两项都过的站就返回,不继续探 —— 别给别人的站白发请求。
//
// 输出:一行 JSON 到 stdout(给 shell 用 node -e 取字段),日志走 stderr。
// 找不到可用站就非零退出并说明每个候选是怎么失败的 —— 那是真实信息。
import { readFileSync } from 'node:fs';

const HUB = process.env.HUB_FILE;
if (!HUB) {
  console.error('需要 HUB_FILE 指向 all-api-hub 导出文件（CLAUDE.md §1：验收不用假上游）');
  process.exit(2);
}

// 导出结构:顶层 accounts 是个**对象**,真正的数组在 accounts.accounts 里。
// 直接对顶层 accounts 做数组操作会静默得到空结果。
const raw = JSON.parse(readFileSync(HUB, 'utf8'));
const list = raw?.accounts?.accounts;
if (!Array.isArray(list)) {
  console.error(`导出结构不符预期：accounts.accounts 不是数组（拿到 ${typeof list}）`);
  process.exit(2);
}

// 用户 ID 头名要试探:二开站点会改名(04 §3.1 的 fan-out 约束)。
// 顺序按实测命中率排,New-API-User 是上游主线的名字。
const UID_HEADERS = ['New-API-User', 'Veloera-User', 'Rix-Api-User', 'Api-User'];
// **刻意只认 NewAPI 系的自称**，不是站型注册表的第二份拷贝（04 §7bis）。
// 理由：本脚本下面的每一个探测都是 NewAPI 专属的（/api/status 取
// quota_per_unit、/api/user/self 试头名、/api/token 取 token id）。放开成
// 全部已注册家族，只会挑中一个 sub2api 站然后在 /api/status 上 404 —— 那不是
// "支持得更全"，是把"没有可用的 newapi 站"这个真信息换成一个假失败。
// 也不能改读 /admin/site-families：本脚本在 sla-core 起来**之前**跑
// （它的输出是建渠道用的入参），那时没有端点可读。
const FAMILIES = new Set(['new-api', 'newapi', 'rix-api']);

const obj = (v) => (v && typeof v === 'object' && !Array.isArray(v) ? v : {});

const candidates = list
  .filter((a) => obj(a).site_url && FAMILIES.has(String(a.site_type ?? '').toLowerCase()))
  .filter((a) => obj(a.account_info).access_token && obj(a.account_info).id)
  // 导出侧自称健康的先试,但不排除其余的 —— 那个字段是备份时的快照,不是现况
  .sort((a, b) => (obj(b.health).status === 'healthy') - (obj(a.health).status === 'healthy'));

if (candidates.length === 0) {
  console.error('导出里没有 NewAPI 系且带凭证的站点');
  process.exit(2);
}

const timeout = Number(process.env.PICK_TIMEOUT_MS ?? 10000);
const maxTry = Number(process.env.PICK_MAX_TRY ?? 12);

// 默认 **不**跟跳转:/api/status 上的 302 是登录墙,是有效的拒绝信号,跟过去
// 只会拿到一页 HTML。但 /api/token 会 301 到 /api/token/(补斜杠),Go 的
// http.Client 同源跳转会跟且保留 Authorization —— 采集器因此没事,探活脚本
// 若不跟就会把好站判死。故按同源放行一跳,跨源仍然当拒绝。
async function get(url, headers, { followSameOrigin = false } = {}) {
  const ac = new AbortController();
  const t = setTimeout(() => ac.abort(), timeout);
  try {
    let r = await fetch(url, {
      headers: { 'User-Agent': 'Mozilla/5.0 (sla-verify)', ...headers },
      signal: ac.signal,
      redirect: 'manual',
    });
    if (followSameOrigin && r.status >= 300 && r.status < 400) {
      const loc = r.headers.get('location');
      const next = loc ? new URL(loc, url) : null;
      if (next && next.origin === new URL(url).origin) {
        r = await fetch(next, {
          headers: { 'User-Agent': 'Mozilla/5.0 (sla-verify)', ...headers },
          signal: ac.signal,
          redirect: 'manual',
        });
      }
    }
    const text = await r.text();
    return { status: r.status, text };
  } finally {
    clearTimeout(t);
  }
}

const fails = [];

for (const a of candidates.slice(0, maxTry)) {
  const base = String(a.site_url).replace(/\/+$/, '');
  const name = String(a.site_name ?? '').slice(0, 20);
  const { access_token: token, id: uid } = obj(a.account_info);

  let qpu;
  try {
    const s = await get(`${base}/api/status`);
    if (s.status !== 200) throw new Error(`/api/status HTTP ${s.status}`);
    const j = JSON.parse(s.text);
    qpu = j?.data?.quota_per_unit;
    if (typeof qpu !== 'number' || qpu <= 0) {
      throw new Error(`quota_per_unit 不是正数：${JSON.stringify(qpu)}`);
    }
    // 开了人机验证的站,服务端自动采集本就不可行(04 §6),别选它
    if (j?.data?.turnstile_check === true) throw new Error('该站开了 turnstile');
  } catch (e) {
    fails.push(`${name} ${base} → ${e.message ?? e}`);
    continue;
  }

  // 目录必须**同时**有倍率(quota_type 0)与按次(1)两种口径,否则
  // "按次标 /次""按口径分段排序""跨段不可比告警"这三条根本无从验证。
  // 实测 14 个真站点里只有 5 个两种都有 —— 这正是 mock 掩盖掉的现实:
  // 我的 mock 故意两种混排,真站点大多只有一种。
  let qt0 = 0, qt1 = 0, nModels = 0;
  try {
    const p = await get(`${base}/api/pricing`);
    if (p.status !== 200) throw new Error(`/api/pricing HTTP ${p.status}`);
    const arr = JSON.parse(p.text)?.data;
    if (!Array.isArray(arr)) throw new Error('pricing 的 data 不是数组');
    nModels = arr.length;
    for (const m of arr) {
      if (m?.quota_type === 0) qt0++;
      else if (m?.quota_type === 1) qt1++;
    }
    if (qt0 === 0 || qt1 === 0) {
      throw new Error(`只有单一计价口径（倍率 ${qt0} / 按次 ${qt1}），分段类断言无从验证`);
    }
  } catch (e) {
    fails.push(`${name} ${base} → ${e.message ?? e}`);
    continue;
  }

  // 逐个试用户 ID 头名。全 401 说明 token 过期,换下一个站。
  let hit = null;
  for (const h of UID_HEADERS) {
    try {
      const r = await get(`${base}/api/user/self`, {
        Authorization: `Bearer ${token}`,
        [h]: String(uid),
      });
      if (r.status === 200 && typeof JSON.parse(r.text)?.data?.quota === 'number') {
        hit = h;
        break;
      }
    } catch {
      // 单个头名失败不算站点失败,继续试下一个
    }
  }
  if (hit === null) {
    fails.push(`${name} ${base} → 凭证不可用（所有用户 ID 头名均未通过）`);
    continue;
  }

  // 取一个**真的** token id。SaveKey 只 UPDATE 不 INSERT,靠 external_ref 与
  // 上游 /api/token 的 id 对齐;对不上的话那把 Key 计入"未登记"异常项,
  // keys 项照样报 ok,但额度一列永远空 —— 界面上"额度已归一为美元"那条
  // 就成了空断言。所以这里把"站上至少有一把 token"提成选站前提。
  let keyRef = null;
  try {
    const r = await get(`${base}/api/token`, {
      Authorization: `Bearer ${token}`,
      [hit]: String(uid),
    }, { followSameOrigin: true });
    if (r.status !== 200) throw new Error(`HTTP ${r.status}`);
    // 取列表的方式必须与采集器的 unwrapDataList 一致:data 可能直接是数组,
    // 也可能是分页对象(实测redacted-channel-03给 data.items,另有二开给 data.records)。
    // 这里若只认一种,选出来的站采集器却读不到,等于把 bug 挪进探活。
    const list = JSON.parse(r.text)?.data;
    const items = Array.isArray(list) ? list : (list?.items ?? list?.records);
    const first = Array.isArray(items) ? items.find(t => t?.id != null) : null;
    if (!first) throw new Error('账号下没有任何 token');
    keyRef = String(first.id);
  } catch (e) {
    fails.push(`${name} ${base} → 取不到 token id：${e.message ?? e}`);
    continue;
  }

  console.error(
    `✅ 选中真上游：${name} ${base}\n` +
      `   uid 头名 ${hit} / quota_per_unit=${qpu} / token id=${keyRef} / ` +
      `目录 ${nModels} 个模型（倍率 ${qt0} + 按次 ${qt1}）`,
  );
  process.stdout.write(
    JSON.stringify({
      name, url: base, uid: String(uid), token,
      uidHeader: hit, quotaPerUnit: qpu, keyRef,
      models: nModels, ratioModels: qt0, perCallModels: qt1,
    }),
  );
  process.exit(0);
}

console.error(`探活了 ${Math.min(candidates.length, maxTry)} 个候选，没有一个可用：`);
for (const f of fails) console.error(`  ✗ ${f}`);
console.error('\n这不是脚本坏了 —— 真站点会挂、会限流、会换证书。');
console.error('换一份更新的导出，或调大 PICK_MAX_TRY 试更多候选。');
process.exit(1);
