# P1 验收证据（AC-37~40 + 退出标准）

| 项目 | 内容 |
| --- | --- |
| 阶段 | **P1：多上游渠道采集与管理**（[PRD §2.1.0](../PRD.md)、[ISSUE-005](../issues/ISSUE-005-phase1-upstream-inventory.md)） |
| 日期 | 2026-08-28 |
| 判定依据 | [14 §2 P1 段](../dev/14-acceptance-matrix.md) 的四条 AC + [00 §3](../dev/00-overview-and-milestones.md) P1 退出标准 |
| 验证环境 | ① **真库**：SLA_DB @ <internal-db-host>（PostgreSQL **17.5**，设计基线是 16 —— 顺带验证向上兼容）② mock NewAPI 上游（`verify/mock_newapi.py`，字段形态照 04 §3.1 实测构造）③ **真 Chrome 152**（点击/填表/等 XHR/截图，非 DOM dump） |
| 可复现 | `make test-ui` 一键起 PG + mock + sla-core + Chrome；CI 第 12 步同一脚本，在 GitHub Linux runner 上独立跑通 |
| 结论 | **AC-37/38/39/40 全部通过；浏览器验收 29/29** |

---

## 1. 四条 AC 的判定结果

### AC-37 渠道挂多账号多 Key、分属不同分组、明文不回显

| 断言 | 实测 |
| --- | --- |
| 渠道下 2 账号、2 Key | ✅ `Key 数 = 2`，`acct=1`/`acct=2` |
| 各 Key 分属不同分组 | ✅ `id=1 group=2`（vip）、`id=2 group=1`（default） |
| **明文一律不回显** | ✅ 列表只见 `sk-ui-se…`/`sk-secon…`；**并断言整个页面 DOM 里搜不到完整明文**（登记时用可识别 secret 后 grep 整份 HTML） |
| Key 生命周期 | ✅ `disable` 后 `status=revoked`；`rotate` 返回新明文且**只返回一次** |

> **FR-094 的验证方式**是刻意选的：只看"列表显示前缀"不足以证明不泄露 —— 明文可能出现在别处（错误信息、隐藏字段、JS 变量）。故断言整份 DOM。
> 脱敏用 SQL 表达式 `left(secret,8)||'…'` 而非 Go 侧截断，**完整 secret 根本不出库**，少一条可能被日志带出去的路径。

### AC-38 三家族手动刷新，逐项结果与 unsupported 显式上报

真库实测一次完整 sync（NewAPI 系）：

```
account              ok    rows=1
groups               ok    rows=3
keys                 ok    rows=2
pricing              ok
model_catalog        ok    rows=8
subscription_quotas  unsupported  「订阅制属交付阶段 P4」
```

| 断言 | 实测 |
| --- | --- |
| 逐项结果与耗时 | ✅ 6 项各有 status/rows/elapsed_ms |
| 四类数据均更新 | ✅ `channel_groups` 3 行、`group_models` 24 行、`channel_model_catalog` 8 行、`upstream_keys.quota_synced_at` 前进 |
| **不支持项显式上报** | ✅ `subscription_quotas` 出现在 items 里且标 `unsupported`，**不静默省略** |
| 声明与实现一致 | ✅ 包级测试 `TestUnsupportedDeclarationsReturnErrUnsupported` 逐家族断言"声明 unsupported ⟺ 返回 `ErrUnsupported`"，且"声明 degraded 的**不得**返回 `ErrUnsupported`" |
| 限流生效 | ✅ 60s 内重复调用返回 **429** 且 `items` 全 `skipped`，**不打上游** |

> ⚠️ **三家族只有 NewAPI 走了真实端到端**：Sub2API 与 ASXS 的适配器有完整包级测试（字段映射、凭证状态机、degraded 语义），但**尚未对真实站点跑过**。那需要真实凭证（[15 §1bis](../dev/15-scope-and-preflight.md) 的基线站点），属 §3 的遗留项。

### AC-39 目录容纳大量模型且不产生可路由模型行

| 断言 | 实测 |
| --- | --- |
| 全量入目录、**无需 token 上界** | ✅ 8 个模型全部入库，`channel_model_catalog` 无 token 上界列 |
| **`models` 表不产生任何行** | ✅ `models = 0`、`channel_models = 0` —— 这是本条的核心：目录（上游有什么）与可路由模型（我们决定用什么）是两层，否则 20 渠道 × 200~300 模型要手填几千行 token 上界 |
| 可分页与排序 | ✅ `?limit/offset/q` 生效，按价格排序（NULL 末位） |

### AC-40 模型下架识别

| 断言 | 实测 |
| --- | --- |
| 连续 N 轮未出现即识别 | ✅ 把 `deepseek-v4` 的 `last_seen_at` 回拨 40 小时（超 `catalog_missing_rounds=3` × `collector_catalog_interval_h=12` = 36h 阈值）→ `?stale=true` 精确筛出它 |
| inventory 报异常 | ✅ `delisted_model × 1 → ['deepseek-v4']` |
| **不断言"自动停用"** | ✅ 按 14 的判定：P1 无路由，没有可停用的对象；`channel_models.enabled` 联动属 P2 |

> **用轮数而非时长**是刻意的（02 §1.3bis）：采集周期本身可配，轮数对周期变化免疫。

---

## 2. 退出标准

| 标准 | 状态 |
| --- | --- |
| AC-37~40 全绿 | ✅ 见 §1 |
| #1~#11 全部关闭 | ✅ 12 个 issue 全关（#13 EPIC 收尾） |
| 证据留档 | ✅ 本文件 + 截图 `/tmp/sla-ui-shots/`（9 张，含 fullPage） |
| 门禁全绿 | ✅ CI 12 步：文档 12 类 + DDL 真跑 + 迁移一致 + 迁移集成 + 管理 API + compose 冒烟 + **浏览器验收** |
| 全渠道 sync 覆盖率报告 | ❌ **未完成** —— 见 §3 |

---

## 3. 未完成项（诚实记录，不算通过）

### 3.1 全渠道覆盖率报告（P1 退出标准之一）

[00 §3](../dev/00-overview-and-milestones.md) 要求"对**全部真实渠道**跑一次全量 sync 并留存覆盖率报告"，它同时提前完成 [15 T6](../dev/15-scope-and-preflight.md) 的验证点（~20 个渠道的价格数据是否都采得到）。

**当前只对 1 个 mock 站点验过**。缺的是真实渠道的凭证 —— 这不是代码问题，是输入问题：需要运营提供约 20 个渠道的站点地址与采集凭证。

**这一项未完成，故 P1 严格意义上尚未退出**。已具备的是：跑这份报告所需的全部能力（`Detect` 批量分桶、逐渠道 sync、inventory 异常分类），执行只需凭证到位。

### 3.2 Sub2API / ASXS 未对真实站点验证

见 AC-38 的备注。两个适配器的包级测试覆盖了字段映射与凭证状态机，但真实站点的响应形态可能与实测记录有出入（04 的记录来自 2026-07 的四站实测，站点会升级）。

### 3.3 `billing_unit` 缩放守卫未接入 CI

[#11](https://github.com/wuhao1477/multi-upstream-ai-gateway-sla/issues/11) 把这道断言标为"随 #7 落地"。实际情况：价格采集已完成，但**成本计算的消费方在 P3**（P1 无成本排序），故 CI 里暂无可断言的计算路径。已在 workflow 注释保留待办。

⚠️ 这条不可遗忘：漏缩放会把每笔成本**放大 100 万倍**（`per_1m_token` 是默认单位），进而让预留、配额、错误预算、告警阈值全部失真（02 §3）。

---

## 4. 实测中发现并修复的缺陷（8 个）

只有真跑才会暴露的那些，值得单独记：

| # | 缺陷 | 暴露条件 |
| --- | --- | --- |
| 1 | 迁移选主用 `pg_try_advisory_lock`，落败者 return nil 后**无人轮询**就去灌种子 → 表还没建好 | 双实例 compose |
| 2 | 种子 `WHERE NOT EXISTS` 挡不住并发 → 撞唯一约束，实例启动失败 | 双实例 compose |
| 3 | Caddy 站点写裸 `:443` 无法签证书，而**对 IP 的 HTTPS 不带 SNI** → 握手失败。症状极误导：Caddy 日志全正常、后端 host is up，只有 curl 连不上 | 真实 TLS |
| 4 | compose 用 `command` 覆盖但 ENTRYPOINT 是 `/sla-core` → **采集器根本没跑** | 真实镜像 |
| 5 | `token_expires_at` 用 `time.Time` 扫 NULL → **NewAPI 渠道一次都采不成**（其长期令牌本就无到期时间） | 真库 + 真采集 |
| 6 | 探测出 `quota_per_unit` 却**只回显不落库** → sync 时因缺它而失败 | 端到端 |
| 7 | inventory 对**全新渠道报"✓ 无异常项"** —— 而它没凭证、根本采不了 | 浏览器验收 |
| 8 | 凭证刷新的锁内 double-check 读了**调用方自己的旧副本** → 10 并发刷 10 次，真实环境下后 9 次互相作废 `refresh_token` | 并发单测 |

> 第 6 项值得注意：它恰好由"宁可失败也不猜"的守卫暴露。若当初给 `quota_per_unit` 写了默认值 500000，它会**静默算错余额**（差 50 万倍）而无人察觉。
>
> 第 8 项的教训：串行化的目的不是排队，是**让后到者看见先到者的结果**。

另修 4 处**测试脚本自身**的缺陷（我的断言错，不是应用错），其中最能说明问题的一处：脚本化跑第一次就失败 —— 它等 `#channels table`，而干净库是空状态；我的手工验证之所以过，是因为库里残留着先前手点建的渠道。**空库才是"运维第一次打开界面"的真实情形。**

---

_验收人：开发。判定依据 [14](../dev/14-acceptance-matrix.md) §3 规则 4：出现"基本正常""大致达标"视为未通过 —— 本文件的每一项都给了具体数值或 diff。_
