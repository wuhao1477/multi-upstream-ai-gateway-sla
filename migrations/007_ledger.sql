-- 请求与逐 Attempt 账本（02 §4，分区母表；P2 才写入）
-- 由 verify/split_migrations.py 从 docs/dev/02-data-model.md 生成初始基线。
-- ⚠️ 此后本文件**不可修改**（已应用的迁移不可变）；schema 变更请新增迁移文件。

CREATE TABLE requests (
  id            UUID NOT NULL,                -- UUIDv7（时间有序，多实例无冲突，FR-110）
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  gateway_client_id BIGINT NOT NULL REFERENCES gateway_clients(id), -- 入站调用方（FR-120）；配额归集与审计必需
  alias_id      BIGINT REFERENCES model_aliases(id), -- 调用方选的别名（承载策略，FR-062/117）
  policy_id     BIGINT REFERENCES routing_policies(id),
  sla_level     TEXT,                         -- 冗余快照，便于按等级统计（FR-090/091）
  tenant_id     TEXT,                         -- 个人/内部默认单租户可空（FR-093 默认关闭）
  session_id    TEXT,                         -- 连续会话键（FR-050/051）；无正文，仅标识
  request_type  TEXT NOT NULL,               -- 'chat_completions' / 'responses'（FR-111/AC-26）
  is_streaming  BOOLEAN NOT NULL DEFAULT false,
  probe_kind    TEXT NOT NULL DEFAULT 'normal' CHECK (probe_kind IN ('normal','probe')), -- 业务测活标记（FR-092）
  -- 决策快照（FR-097）：候选/排除原因/选择——元数据 JSON，不含正文
  decision_snapshot JSONB,                    -- {candidates:[...], excluded:[{binding,reason}], chosen, cache_pred, balance, capacity}
  -- 请求级结果（FR-098）：最终对外结果
  -- 请求推进到哪一阶段（第 18 轮 [critical]）：C′ 提前插入 requests 后，
  -- 崩溃可能发生在 selector **之前** —— 此时零 attempt、无预留、无 decision_snapshot，
  -- 与「全候选不可用」「预留失败 429」外观完全相同，恢复扫描无从区分该判 unavailable 还是 failed。
  stage         TEXT NOT NULL DEFAULT 'authenticated'
                  CHECK (stage IN ('authenticated',      -- C′ 已落库，尚未进 selector
                                   'planned',            -- selector 已产出非空 RoutePlan
                                   'no_candidates',      -- selector 输出空候选集
                                   'reservation_rejected',-- D 阶段原子预留返回 0 行（429）
                                   'dispatched')),       -- 已发起上游调用（至少一条 attempt）
  final_status  TEXT NOT NULL DEFAULT 'pending'
                  CHECK (final_status IN ('pending','completed','failed','canceled','unavailable','interrupted')),
                  -- ⚠️ 'pending' 是**唯一非终态**；恢复扫描必须把它推到某个终态并同步终结
                  --    对应的 client_reservations 行（§2bis 统一终结事务），否则配额永久泄漏
  effective_ttft_ms INTEGER,                  -- 对外有效首字（取被提交 attempt 的自算 TTFT，AC-31）
  had_takeover  BOOLEAN NOT NULL DEFAULT false, -- 是否发生首字前接管（FR-076/092）
  PRIMARY KEY (id, created_at)                -- 分区键须入主键
) PARTITION BY RANGE (created_at);

CREATE TABLE attempts (
  id                UUID NOT NULL,            -- UUIDv7
  request_id        UUID NOT NULL,            -- 关联同一外部请求（归集主键）
  request_created_at TIMESTAMPTZ NOT NULL,    -- 冗余分区键，与 requests 对齐
  attempt_no        SMALLINT NOT NULL,        -- 该请求内第几跳（1=主，2=接管…）
  binding_id        BIGINT NOT NULL REFERENCES bindings(id), -- 渠道+key+url（路由资源）
  -- 该跳的费用上界（第 37 轮补）：恢复结算要按「已终结但无用量 → 该跳单跳保守估算」汇总，
  -- 而 RoutePlanEntry.SingleHopUSD 只在**内存**里——崩溃后恢复任务读不到它。必须落库。
  single_hop_est_usd nonneg_usd,
  price_version_id  UUID REFERENCES price_versions(id),      -- 决策时的**基础价**版本（FR-013/AC-02）
  multiplier_version_id UUID REFERENCES multiplier_versions(id), -- 决策时的**倍率**版本（binding 级）
  -- 两者合起来才是该 attempt 的完整计价输入，缺一不可复算
  role              TEXT NOT NULL CHECK (role IN ('primary','takeover','retry','canary','probe')), -- FR-076/079
                    -- canary：一期受控验证——**本来就要发的真实业务请求**被分给待验证 binding（不额外产生费用）
                    -- probe：主动测活（**一期第二轨**）——为探测而额外发起的请求（[05 §2.1~2.3](./05-scheduling-and-operations.md)）

  -- ── 状态：上游原始 vs 我方归并（AC-30 核心）──
  -- ⚠️ 第 44 轮删除了 `gateway_status`（原 beta5 `execution.status` 存证列）：
  --    C6 只改了它的注释、没删列，而**全库没有任何规则读写它**（收窄后的悬空列检查抓出）。
  --    转向自研后（[11](./11-decision-full-selfbuilt.md)）数据面没有外部网关，上游是中转站、
  --    只回 HTTP 状态与错误体，不存在"网关自身状态枚举"可存；且原注释自己写着
  --    "永不作为判定依据""多为 NULL"。归并所需的输入只有 error_message + 我方观测事实。
  error_message     TEXT,                     -- 上游错误原文（元数据，非正文）；**归并的唯一外部输入**
  -- 归并后的取消/失败口径：外部实现常把 canceled 只用于客户端取消，上游断开/内部超时归 failed
  -- → 我方按 error_message + 观测事实自行归并（AC-30、ISSUE-001 假设2 语义澄清）
  cancel_reason     TEXT CHECK (cancel_reason IN
                      ('none','client_disconnect','sla_takeover','upstream_disconnect','internal_timeout','upstream_error')),
  attempt_status    TEXT NOT NULL DEFAULT 'pending'
                      CHECK (attempt_status IN (
                        -- 非终态（两者都须被 §4.2bis 租约扫描覆盖）
                        'pending',            -- 已落意图，未提交
                        'committed',          -- 已提交输出给下游，流未结束
                        -- 终态
                        'completed','failed','canceled_by_sla','canceled_by_client',
                        'unknown_billing','interrupted')),
                      -- ⚠️ canceled_by_client：**客户端主动断开**（Codex 用户按 Ctrl-C 是高频操作）。
                      --    此前枚举里没有它，三个可选项全错：记 canceled_by_sla 是语义错误
                      --    （不是我们取消的）、记 failed 会污染渠道成功率（渠道没问题）、
                      --    记 completed 更错。故单列一态：
                      --      · **不计入**渠道成功率（不是渠道的锅，与 unknown_billing/interrupted 同档）
                      --      · **不计入**用户 SLA 失败（PRD 术语：用户主动取消不计入）
                      --      · 成本**计入**（上游已经生成的 token 照样收费）
                      -- ⚠️ unknown_billing：上游**已发出**但进程在写入首条 outbox 事件前崩溃 →
                      --    可能已计费、结果完全不明。
                      -- ⚠️ interrupted：已提交输出（见过首字）但**未见终帧**即崩溃 →
                      --    **确定已计费**、用量未知。与 unknown_billing 的区别是计费确定性，
                      --    两者都按保守估算计入成本；**渠道健康**统计不计入（崩溃不是渠道的锅），
                      --    但**用户侧 SLA 必须计入失败**（FR-071/AC-12）。两套口径见 §4.2bis 不变式 3。
                      -- ⚠️ `pending` 与 `committed` 都是**非终态**：只覆盖 pending 的扫描
                      --    会让首字后崩溃的 attempt 永久悬挂（对抗性审查第 7 轮 critical）。见 §4.2bis

  -- ── 三个**互相独立**的持久化事实（对抗性审查第 8 轮 [high]）──
  -- ⚠️ 不可用 attempt_status='committed' 反推"见过首字"：[03 §3.2](./03-upstream-layer.md) 的
  --    ShouldCommit 在**空终态/错误终态**上也为 true 而 HasTTFTOutput 为 false。
  --    若恢复扫描按"committed ⇒ 见过首字"判定，会把"空响应已提交、关单前崩溃"
  --    误写成 interrupted + 人工核对，而它的真实终态是 completed/failed。
  response_committed_at TIMESTAMPTZ,          -- ShouldCommit 首次为真、**且已落库**的时刻
  commit_trigger    TEXT CHECK (commit_trigger IN ('actionable','buffer_limit')),
                                              -- 'actionable' = 真见到可执行输出；
                                              -- 'buffer_limit' = T2 缓冲上限强制提交（非真首字，
                                              --   has_ttft_output=false、ttft 为 NULL、不计入 TTFT 统计）
  -- ── 上游事件到达时刻（**同步写之前**）：与下游交付时刻配对，度量我们自己的开销 ──
  upstream_first_actionable_at TIMESTAMPTZ,   -- 上游首个 ShouldCommit 事件**到达**时刻（早于 response_committed_at）
  upstream_terminal_at         TIMESTAMPTZ,   -- 上游终帧**到达**时刻（早于 finalize_upstream 提交）

  -- ── 本进程下游写入完成（[03 §3.0](./03-upstream-layer.md)）：与上游事实**独立**，不可互相推导 ──
  -- ⚠️ 只证明「我们写出去了」，**不证明客户端收到了**（对端是 Caddy）。端到端确认需客户端回执，
  --    而主力客户端 Codex CLI 不可要求配合 → 该残余不确定性已知并接受，不写进任何保证。
  -- DB 提交与 socket 写出无法原子化，反方向窗口不可消除 → 必须分开记，冲突时按"未确认=未交付"取保守解释。
  -- 不参与计费（成本只看上游事实），故可异步经 outbox 落库。
  downstream_first_byte_written_at TIMESTAMPTZ,       -- 首字节已写出下游 socket
  downstream_write_completed_at  TIMESTAMPTZ,       -- 终帧已写出下游 socket / 流正常关闭
  has_ttft_output   BOOLEAN NOT NULL DEFAULT false, -- HasTTFTOutput 是否曾为真（决定 TTFT 是否有效）
  terminal_event    TEXT CHECK (terminal_event IN
                      ('completed','empty_completed','error','incomplete')),
                                              -- 观察到的上游终帧类型；NULL = **未见终帧**
                                              -- 恢复扫描据此区分"流真的断了"与"已收完只是没关单"

  -- ── 逐尝试 metrics（自算，AC-31）──
  content_aware_ttft_ms INTEGER,              -- 自算内容感知首字（排除 role-only/空SSE/心跳，AC-31/假设3/6）
                                              -- has_ttft_output=false 时恒为 NULL
  gateway_reported_ttft_ms INTEGER,           -- 上游/中间层若回传首字类字段 → **仅存证、永不采信**（AC-31）
                                              -- 命名保留"gateway_"前缀只为兼容既有引用；语义是"非我方自算的那个值"
  full_latency_ms   INTEGER,                  -- 总延迟（请求进入→完整结束）
  upstream_latency_ms INTEGER,                -- 上游耗时（发往上游→完整结束）
  -- 决策/网关自身开销 = full_latency_ms − upstream_latency_ms，用于 FR-110「P99≤50ms」自监控（测法见 06 §6）
  -- ⚠️ 该差值**测不到同步写**（首字同步写落在 upstream_latency_ms 内被抵消，第 11 轮 [high]）。
  --    同步写代价用下面两个**派生指标**度量（无需新增列，由上面四个时刻算出，[14 判定口径](./14-acceptance-matrix.md)）：
  --      downstream_ttft_delay_ms   = downstream_first_byte_written_at − upstream_first_actionable_at
  --      downstream_finish_delay_ms = downstream_write_completed_at  − upstream_terminal_at
  output_tokens_per_s NUMERIC(12,3),          -- 输出速度（FR-040）；**分母 = full_latency_ms − content_aware_ttft_ms**
                                              -- 只算生成阶段；用总延迟会把慢首字渠道误判为「生成慢」
  stream_broken     BOOLEAN NOT NULL DEFAULT false, -- 已输出首字后中断=完整失败，不拼接（FR-078/AC-12）

  -- ── SLA 取消 / 继续计费 / 重复费用（FR-080、AC-32）──
  canceled_by_sla   BOOLEAN NOT NULL DEFAULT false, -- 是否被 SLA 主动取消（首字前接管/期限到达）
  cancel_propagated BOOLEAN,                  -- 取消是否已传播到上游止损（AC-32；自研层 Close() 拆上游连接，[03 §6](./03-upstream-layer.md)）
  continue_billing  BOOLEAN NOT NULL DEFAULT false, -- 取消后是否仍继续计费（FR-080 记录继续计费情况）
  is_duplicate_cost BOOLEAN NOT NULL DEFAULT false, -- 未取消重复请求费用（FR-058/8.4）

  -- ── 隐藏重试补算（FR-119、假设6）──
  -- 外部调用恒为 1 次（=本 attempt）；上游实际调用可能 >1（ccLoad Codex 400 body-rewrite → 2 次 POST，仅留一条审计）
  upstream_call_count SMALLINT NOT NULL DEFAULT 1, -- 实际上游调用数（补算后）
  hidden_retry_detected BOOLEAN NOT NULL DEFAULT false,
  hidden_retry_kind  TEXT,                    -- 如 'codex_400_strip_thinking'（假设6补验）
  -- ⚠️ 上面三列（upstream_call_count / hidden_retry_detected / hidden_retry_kind）**一期恒为默认值**：
  --    §11 开放点 5 已裁定一期不做隐藏重试推断（我们自己不发，上游中转站内部若有也不可观测）。
  --    保留建表只为二期接入会隐藏重试的通道时零改表。**不是漏实现**（第 40 轮明示）。

  -- ── 上游关联键（自研直连，旁路观察提取）──
  upstream_response_id    TEXT,               -- 上游响应 id（如 resp_.../chatcmpl-...），用于排障关联

  started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  ended_at          TIMESTAMPTZ,

  -- ── attempt 租约（§4.2bis 悬挂检测）──
  external_call_started_at TIMESTAMPTZ,       -- 上游请求实际发出的时刻；NULL=尚未发出（崩溃则未计费）
  lease_heartbeat_at TIMESTAMPTZ,             -- 持有实例的心跳；**插入 attempt 时即写入非 NULL**（防漏扫）
  lease_owner       TEXT,                     -- 持有该 attempt 的实例标识

  PRIMARY KEY (id, request_created_at),
  -- ⚠️ 恢复扫描用 max(attempt_no) 判定「最后一跳」；无此约束时重复 attempt_no 会让最后一跳不唯一，
  --    同一 request 可能被处理多次（第 18 轮 [high]；第 19 轮修正：此约束曾被误加到 session_prefix_ledger）
  UNIQUE (request_id, request_created_at, attempt_no)
) PARTITION BY RANGE (request_created_at);

CREATE INDEX idx_requests_open ON requests(created_at) WHERE final_status = 'pending';

CREATE INDEX idx_attempts_lease ON attempts(request_id, attempt_no, lease_heartbeat_at NULLS FIRST);

CREATE TABLE attempt_usage (
  id                UUID NOT NULL,            -- UUIDv7（分区表主键须含分区键，见表尾复合 PK）
  attempt_id        UUID NOT NULL,
  request_created_at TIMESTAMPTZ NOT NULL,    -- 冗余分区键
  upstream_seq      SMALLINT NOT NULL DEFAULT 1, -- 一期恒为 1；>1 保留给二期（FR-119）
  prompt_tokens     INTEGER,                  -- 上游终帧 usage.prompt_tokens（旁路观察提取，[03 §7](./03-upstream-layer.md)）
  completion_tokens INTEGER,                  -- usage.completion_tokens
  total_tokens      INTEGER,                  -- usage.total_tokens
  prompt_cached_tokens INTEGER,               -- usage.prompt_tokens_details.cached_tokens（缓存命中部分，FR-054）
  -- FR-056 预测需要**分母**：命中率 = prompt_cached_tokens / cacheable_prompt_tokens。
  -- 只有 prompt_tokens 不够——系统提示、工具定义等固定前缀才可缓存，用户新增的那一轮不可缓存，
  -- 拿 prompt_tokens 当分母会系统性低估命中率（开发视角审查第 27 轮 [P1]）。
  cacheable_prompt_tokens INTEGER,            -- 本次请求中**理论可缓存**的输入 token 数
  total_cost        nonneg_usd,               -- 该跳实际费用：上游若回传费用则用它，否则按价格版本自算（见 cost_source）
  cost_items        JSONB,                    -- 费用明细（元数据；输入/输出/缓存分项，便于排障与对账）
  cost_source       TEXT NOT NULL DEFAULT 'upstream' CHECK (cost_source IN ('upstream','estimated')),
  -- 预估 vs 实际扣费差异（FR-016/019）：超容差标计费异常
  estimated_cost    nonneg_usd,
  cost_variance     usd_amount,               -- 实际-预估；**可为负**（实际低于预估），故用可正负的 usd_amount（FR-019/AC-23）
  PRIMARY KEY (id, request_created_at),       -- 分区表：主键必须包含分区键
  -- 指向 attempts 的复合外键（分区表间引用须带分区键）
  FOREIGN KEY (attempt_id, request_created_at)
      REFERENCES attempts (id, request_created_at),
  -- 支撑 finalize_upstream 的 ON CONFLICT 幂等目标（第 19 轮 [critical]）；须含分区键
  UNIQUE (attempt_id, request_created_at, upstream_seq)
) PARTITION BY RANGE (request_created_at);

CREATE TABLE session_prefix_ledger (
  session_id        TEXT NOT NULL,
  turn_no           INTEGER NOT NULL,
  request_id        UUID NOT NULL,
  turn_first_token_ms INTEGER,               -- 本轮实际首字（自算，AC-31）
  cumulative_first_token_ms BIGINT,          -- 累计首字
  prefix_avg_ms     INTEGER,                 -- 前缀平均（=累计/轮数）
  target_ms         INTEGER,                 -- 本轮目标（默认前缀均 ≤10s，§11）
  met_target        BOOLEAN,                 -- 每轮结束达标判定（FR-051）
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, turn_no)
);

CREATE INDEX idx_attempts_request   ON attempts(request_id, request_created_at); -- 归集同一请求的多跳

CREATE INDEX idx_attempts_binding   ON attempts(binding_id, started_at);         -- 喂健康统计（§7）

CREATE INDEX idx_attempts_upresp    ON attempts(upstream_response_id);           -- 排障关联上游响应

CREATE INDEX idx_attempts_cancel    ON attempts(cancel_reason)
                                       WHERE cancel_reason<>'none';               -- 取消口径统计（AC-30）

CREATE INDEX idx_usage_attempt      ON attempt_usage(attempt_id, request_created_at);

CREATE INDEX idx_req_session        ON requests(session_id, created_at) WHERE session_id IS NOT NULL;

CREATE INDEX idx_req_tenant_level   ON requests(tenant_id, sla_level, created_at); -- 指标维度（FR-091）
