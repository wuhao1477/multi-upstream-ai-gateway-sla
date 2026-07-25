# 09 管理 API 与设置面（FR-115 落地 · v0.1 草案）

| 项目 | 内容 |
| --- | --- |
| 状态 | 草案，待评审（M0 出骨架，M4 出完整设置页） |
| 日期 | 2026-07-23 |
| 定位 | 补齐设计审查发现的空缺：[02 `config_params`](./02-data-model.md) 有表有 `confirmed_twice` 列，但**无操作面**。本篇定义读写这些配置的**管理 API**，以及 FR-115 要求的"设置页明示含义/用法/影响 + 关键项二次确认" |
| 输入 | [FR-115/116](../PRD.md)、[DECISIONS 总原则](../DECISIONS.md)（策略参数"建议默认 + 设置页可改，关键项二次确认"）、[02 数据模型](./02-data-model.md)、[05 调度与经营策略](./05-scheduling-and-operations.md)（策略语义来源） |
| 范围 | **管理平面 API + 二次确认流程 + 策略元数据**。设置页 UI 本身一期可用最简形态（甚至 API + 文档即可），本篇只定契约 |

> **务实边界**：个人/内部使用，不追求华丽后台。一期目标是"**有一条安全、可审计、带二次确认的配置通道**"，而非完整运营控制台。UI 可后置，API/契约必须先定——否则 `config_params` 只能靠手改 SQL，二次确认（FR-115 硬要求）无从落地。

---

## 1. 管理平面与数据平面分离

| 平面 | 端点前缀 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| 数据平面 | `/v1/*`（chat_completions、responses） | 上游业务 API Key | 承载真实请求（[03](./03-upstream-layer.md)） |
| 健康 | `/healthz` | 无 | LB 探针（[06](./06-deployment-and-operations.md)） |
| **管理平面** | **`/admin/*`** | **独立管理令牌**（非业务 Key） | 本篇；配置读写、策略、审计查询 |

- 管理平面挂在 sla-core 上，是**我方核心对运维暴露的面**；与上游站点自身的管理接口（采集器访问，[04](./04-collector-adapter.md)）互不相干。
- 一期鉴权：单个**管理令牌**（环境变量注入，明文一期可接受，随 FR-113 一起在对外前升级）；管理平面**只在内网/本机可达**，不经公网。

---

## 2. 配置读写 API（对应 [02 `config_params`](./02-data-model.md)）

| 方法 | 端点 | 作用 |
| --- | --- | --- |
| `GET` | `/admin/config` | 列出所有配置项（含 scope、当前值、是否关键、版本、元数据） |
| `GET` | `/admin/config/{scope_type}/{scope_id}/{param_key}` | 取单项（含默认值、含义、影响、历史版本） |
| `POST` | `/admin/config/preview` | **预演**：提交拟改值，返回校验结果 + 影响评估 + 是否需二次确认，**不落库** |
| `POST` | `/admin/config/apply` | 应用变更；关键项须带二次确认令牌（§3） |
| `GET` | `/admin/config/history?param_key=` | 变更历史（FR-099：前后值/责任人/原因/时间） |

- 所有写操作**只 INSERT 新版本**（`config_params` 的 `version+1`），不 UPDATE 旧行——与价格版本同源的不可覆盖原则，保证可回溯、可回滚（FR-104）。
- `apply` 成功后触发**内存快照重建**（[01 §5](./01-architecture.md)），使新配置对决策路径生效；决策路径本身仍只读快照、不查库。

---

## 3. 二次确认流程（FR-115 的核心，`is_critical=true` 强制）

关键策略（测活预算、禁测活规则、订阅倾斜、旁路开关、保留窗口缩短、SLA 指标等，`config_params.is_critical=true`）**必须两步提交**：

```
1 POST /admin/config/preview {param_key, new_value, changed_by, change_reason}
    → 200 {
        valid: true,
        is_critical: true,
        current_value, new_value,
        impact: "该改动会把全局测活预算从 2% 降到 1%，预计每日探索次数 -50%…",  ← 明示影响(FR-115)
        confirm_token: "<一次性令牌，绑定本次 diff + 5min TTL>"
      }
2 POST /admin/config/apply {param_key, new_value, changed_by, change_reason, confirm_token}
    → 校验 confirm_token 与提交 diff 一致且未过期 → INSERT 新版本、confirmed_twice=true → 200
```

- `confirm_token` 绑定**具体 diff**：改了值再 apply 会因 token 不匹配被拒——防"确认了 A 却提交了 B"。
- 非关键项（`is_critical=false`）可跳过 preview 直接 apply（仍记审计）。
- **对应 [02 列](./02-data-model.md)**：`confirmed_twice` 唯有走完二次确认才为 `true`；决策路径加载配置时可断言"关键项 `confirmed_twice=true` 才生效"，未确认的关键改动不进快照。

---

## 4. 策略元数据（FR-115"明示含义/用法/影响"）

每个策略参数在代码里带静态元数据（随二进制发布，非用户可改），供 `GET /admin/config` 与设置页渲染：

```go
type ParamMeta struct {
    Key         string   // "probe.budget.global_pct"
    Group       string   // "测活" / "SLA" / "订阅" / "容量" / "告警" …
    Title       string   // "全局测活预算"
    Description string   // 一句话含义
    Unit        string   // "%" / "秒" / "次"
    Default     any      // 建议默认值（DECISIONS 参数值）
    Critical    bool     // 是否关键 → 二次确认
    ImpactHint  string   // "调低会减少探索、可能延缓渠道评估更新"
    SourceRef   string   // "DECISIONS 参数3 / FR-063 / 05 §2.1"  ← 每项可回溯
}
```

- 元数据**逐项回溯到 DECISIONS 参数号或 FR**（`SourceRef`），杜绝"设置页有个开关但没人知道它对应哪条需求"。
- 一期设置页可以只是：`GET /admin/config` 的 JSON + 一个极简表单页（甚至 curl 脚本），把 `Description/ImpactHint/二次确认`落到位即可满足 FR-115；美化后置 M4。

---

## 5. 其余管理端点（M1~M4 逐步）

| 端点 | 作用 | 里程碑 |
| --- | --- | --- |
| `GET /admin/bindings`、`POST /admin/bindings` | 渠道/绑定登记（落 `channels`/`upstream_keys`，供上游对接层读取，[03](./03-upstream-layer.md)） | M1 |
| `GET /admin/aliases` | 模型别名 ↔ 策略映射（[02 §2](./02-data-model.md)、FR-062） | M1 |
| `GET /admin/ledger/requests?…` | 账本查询（逐 Attempt、对账状态；FR-097/098） | M1 |
| `GET /admin/subscriptions` | 订阅台账与双倍率、到期浪费预测（[02 §5](./02-data-model.md)） | M3 |
| `GET /admin/alerts` | 告警事件流（[02 §8](./02-data-model.md)、参数16） | M3 |
| `GET /admin/health` | 各 binding 健康/冷却/样本（[02 §6](./02-data-model.md)） | M2 |
| `POST /admin/collector/credentials` | 采集凭证登记（[04](./04-collector-adapter.md)、明文一期） | M3 |

---

## 6. FR / AC 覆盖

| 契约要素 | FR / AC |
| --- | --- |
| 策略可配置 + 建议默认 | FR-115、DECISIONS 总原则 |
| 关键项二次确认 | FR-115、[02 `confirmed_twice`](./02-data-model.md) |
| 明示含义/用法/影响 | FR-115（`ParamMeta.Description/ImpactHint`） |
| 配置版本化、前后值、责任人、原因 | FR-099/104、[02 `config_params`](./02-data-model.md) |
| 数据时效可配置 | FR-116 |
| 别名策略载体管理 | FR-062/117 |
| 账本可查询 | FR-097/098 |

---

## 7. 开放点

| # | 开放点 | 结论 |
| --- | --- | --- |
| 1 | 设置页 UI 一期做到什么程度 | ✅ **已定**：一期只保证 API + 二次确认 + 元数据（可 curl/极简表单）；完整 UI 后置 M4 |
| 2 | 管理令牌 vs 多用户 RBAC | ✅ **已定**：一期单管理令牌（个人/内部）；多用户 RBAC 对外前再评估 |
| 3 | 管理平面是否也要多实例一致 | ✅ **已定**：无状态，写走同一 PG（`config_params` 版本化），任一 core 实例可服务；与数据平面同源（[06](./06-deployment-and-operations.md)） |

---

_本篇补齐 FR-115 的操作面：`config_params` 表 + 本篇 API + 二次确认流程三者合起来才让"策略可配置且关键项二次确认"可落地。UI 可后置，契约先定。_
