package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ItemStatus 是 sync 逐项结果（09 §5.0bis 的五态）。
type ItemStatus string

const (
	StatusOK          ItemStatus = "ok"
	StatusPartial     ItemStatus = "partial" // 仅 keys 可能：部分 Key 失败
	StatusFailed      ItemStatus = "failed"
	StatusUnsupported ItemStatus = "unsupported"
	StatusSkipped     ItemStatus = "skipped" // 被限流跳过
)

// SyncItem 是一项采集的结果。
type SyncItem struct {
	Capability Capability   `json:"capability"`
	Support    SupportLevel `json:"support"`
	Status     ItemStatus   `json:"status"`
	ElapsedMs  int64        `json:"elapsed_ms"`
	Rows       int          `json:"rows,omitempty"`
	Failed     int          `json:"failed,omitempty"`
	Error      string       `json:"error,omitempty"`
	Note       string       `json:"note,omitempty"`
}

// SyncResult 是一次 sync 的完整结果（09 §5.0bis 的响应结构）。
type SyncResult struct {
	ChannelID  int64      `json:"channel_id"`
	SiteFamily Family     `json:"site_family"`
	StartedAt  time.Time  `json:"started_at"`
	ElapsedMs  int64      `json:"elapsed_ms"`
	Items      []SyncItem `json:"items"`
}

// Sink 是采集结果的落库出口。
//
// 抽成接口让 sync 编排可脱库单测 —— 编排逻辑（顺序、事务边界、部分失败语义）
// 是本模块最容易出错的部分，不该只能靠集成测试验证。
type Sink interface {
	// SaveAccount 写余额信号与账号快照。
	SaveAccount(ctx context.Context, channelID int64, a Account) error
	// SaveGroups 写分组与分组可用模型（单事务）。
	SaveGroups(ctx context.Context, channelID int64, gs []Group) (int, error)
	// SaveKey 写一把 Key 的用量（每把一个事务）。
	// 返回 ErrKeyNotRegistered 表示上游有、库中无 —— 计入异常项而非失败。
	SaveKey(ctx context.Context, channelID int64, k Key) error
	// SavePricing 写价格版本。
	SavePricing(ctx context.Context, channelID int64, p Pricing) (int, error)
	// SaveCatalog 写模型目录（upsert，first_seen_at 不覆盖）。
	SaveCatalog(ctx context.Context, channelID int64, cs []CatalogModel) (int, error)
}

// ErrKeyNotRegistered：采到一把库中未登记的 Key。
//
// **不是失败**（02 §1.3bis）：采集不自动创建 Key（secret 是明文凭证，
// 上游只回掩码，凭空插一行会让它永远不可用），故计入 inventory 异常项
// 提示运维补登记。
var ErrKeyNotRegistered = errors.New("collector: 上游存在但库中未登记的 Key")

// Syncer 执行一次渠道的全量采集编排。
type Syncer struct {
	Adapter Adapter
	Sink    Sink
	// Auth 用于在采集前确保凭证新鲜（不变式 S-1 在此生效）。
	Auth *Authenticator
	// Refresher 是站型特定的续期实现（NewAPI 传 nil：不变式 N-1 禁止刷新）。
	Refresher Refresher
	only      []Capability
}

// SyncSelected 执行指定能力，供不同周期的后台任务复用同一套采集编排。
func (s *Syncer) SyncSelected(
	ctx context.Context, cred Credential, capabilities ...Capability,
) (*SyncResult, error) {
	selected := *s
	selected.only = capabilities
	return selected.Sync(ctx, cred)
}

func (s *Syncer) wants(capability Capability) bool {
	return len(s.only) == 0 || slices.Contains(s.only, capability)
}

// Sync 按 09 §5.0bis 的**冻结顺序**执行，逐项独立提交。
//
// 顺序（有依赖，不可交换）：
//
//	① Authenticate      失败即整体中止，不写任何表
//	② FetchAccount      产出 external_user_id，NewAPI 后续请求头部必需
//	③ FetchGroups       **必须先于 ④**（Key 的外键要分组行先存在）
//	④ FetchKeys
//	⑤ FetchPricing
//	⑥ FetchModelCatalog
//
// 事务边界：**逐项独立提交，不做跨项大事务**。理由（09 §5.0bis）——
// 一次 sync 可能写数百行，单事务会长时间持锁；且"价格采到了但目录超时"
// 没有理由把价格也丢掉：采集是幂等补齐动作，不是要么全有要么全无的账务操作。
func (s *Syncer) Sync(ctx context.Context, cred Credential) (*SyncResult, error) {
	res := &SyncResult{
		ChannelID: cred.ChannelID, SiteFamily: cred.Family,
		StartedAt: time.Now(),
	}
	defer func() { res.ElapsedMs = time.Since(res.StartedAt).Milliseconds() }()

	caps := s.Adapter.Capabilities()

	// ── ① 凭证新鲜 + 鉴权。失败即整体中止：连不上或认不过，谈不上采集 ──
	if s.Auth != nil && s.Refresher != nil {
		fresh, err := s.Auth.EnsureFresh(ctx, cred, s.Refresher, time.Now())
		if err != nil {
			return res, fmt.Errorf("凭证续期失败: %w", err)
		}
		cred = fresh
	}
	sess, err := s.Adapter.Authenticate(ctx, cred)
	if err != nil {
		return res, fmt.Errorf("鉴权失败: %w", err)
	}

	// ── ② 账号 ──
	if s.wants(CapAccount) {
		s.run(ctx, res, caps, CapAccount, func() (int, int, string, error) {
			a, err := s.Adapter.FetchAccount(ctx, sess)
			if err != nil {
				return 0, 0, "", err
			}
			// ⚠️ 账号级的 external_user_id 要回填进会话：NewAPI 系后续请求
			// 必须带用户 ID 头（04 §3.1），Detect 阶段拿不到它。
			if a.UserID != "" && sess.ExternalUserID == "" {
				sess.ExternalUserID = a.UserID
			}
			if err := s.Sink.SaveAccount(ctx, cred.ChannelID, a); err != nil {
				return 0, 0, "", err
			}
			return 1, 0, degradedNote(a.Meta), nil
		})
	}

	// ── ③ 分组（必须先于 ④）──
	if s.wants(CapGroups) {
		s.run(ctx, res, caps, CapGroups, func() (int, int, string, error) {
			gs, err := s.Adapter.FetchGroups(ctx, sess)
			if err != nil {
				return 0, 0, "", err
			}
			n, err := s.Sink.SaveGroups(ctx, cred.ChannelID, gs)
			if err != nil {
				return 0, 0, "", err
			}
			notes := make([]string, 0, len(gs))
			for _, g := range gs {
				if note := degradedNote(g.Meta); note != "" {
					notes = append(notes, note)
				}
			}
			return n, 0, joinNotes(notes...), nil
		})
	}

	// ── ④ Key：**每把一个事务**，单把失败不影响其它（09 §5.0bis）──
	if s.wants(CapKeys) {
		s.run(ctx, res, caps, CapKeys, func() (int, int, string, error) {
			ks, err := s.Adapter.FetchKeys(ctx, sess)
			if err != nil {
				return 0, 0, "", err
			}
			var ok, failed, unregistered int
			var firstErr error
			for _, k := range ks {
				err := s.Sink.SaveKey(ctx, cred.ChannelID, k)
				switch {
				case err == nil:
					ok++
				case errors.Is(err, ErrKeyNotRegistered):
					// 上游有、库中无 → 异常项而非失败（02 §1.3bis）
					unregistered++
				default:
					failed++
					if firstErr == nil {
						firstErr = err
					}
				}
			}
			note := ""
			if unregistered > 0 {
				note = fmt.Sprintf("%d 把 Key 上游存在但库中未登记，需补登记（计入异常项）",
					unregistered)
			}
			processed := ok + unregistered
			if failed > 0 {
				return processed, failed, note, fmt.Errorf("部分 Key 写入失败: %w", firstErr)
			}
			return processed, 0, note, nil
		})
	}

	// ── ⑤ 价格 ──
	if s.wants(CapPricing) {
		s.run(ctx, res, caps, CapPricing, func() (int, int, string, error) {
			p, err := s.Adapter.FetchPricing(ctx, sess)
			if err != nil {
				return 0, 0, "", err
			}
			n, err := s.Sink.SavePricing(ctx, cred.ChannelID, p)
			if err != nil {
				return 0, 0, "", err
			}
			// ⚠️ 采到 ≠ 写入：价格版本按 (channel, model) 作用域，只为**已登记进
			// models 的可路由模型**写行（02 §2bis：models 要求 token 上界必填，
			// 目录阶段拿不到，故不自动建行）。P1 不登记任何可路由模型（AC-39），
			// 于是这里**必然 n=0**。
			//
			// 不说明就会退化成"ok + rows=0 + 无备注"——运维无法区分"写成功了"
			// 和"一行都没写"，而后者在 P1 是**预期行为**、在 P3 则是**故障**。
			// 同 ④ 未登记 Key 的处理：如实记 note，而不是让 ok 替它兜着。
			if skipped := len(p.Models) - n; skipped > 0 {
				extra := fmt.Sprintf(
					"采到 %d 个模型价格，其中 %d 个未登记为可路由模型（models 表无对应行）→ "+
						"不写 price_versions，价格已存入模型目录备查",
					len(p.Models), skipped)
				rows := n
				if rows == 0 {
					rows = len(p.Models)
				}
				return rows, 0, joinNotes(degradedNote(p.Meta), extra), nil
			}
			return n, 0, degradedNote(p.Meta), nil
		})
	}

	// ── ⑥ 目录 ──
	if s.wants(CapModelCatalog) {
		s.run(ctx, res, caps, CapModelCatalog, func() (int, int, string, error) {
			cs, err := s.Adapter.FetchModelCatalog(ctx, sess)
			if err != nil {
				return 0, 0, "", err
			}
			n, err := s.Sink.SaveCatalog(ctx, cred.ChannelID, cs)
			if err != nil {
				return 0, 0, "", err
			}
			notes := make([]string, 0, len(cs))
			for _, model := range cs {
				if note := degradedNote(model.Meta); note != "" {
					notes = append(notes, note)
				}
			}
			return n, 0, joinNotes(notes...), nil
		})
	}

	// ── 订阅：P4，但**必须出现在 items 里** ──
	// AC-38 要求"不支持的项返回明确的不支持而非静默留空"，
	// 且须与 Capabilities() 声明一致。
	if s.wants(CapSubscriptionQuotas) {
		res.Items = append(res.Items, SyncItem{
			Capability: CapSubscriptionQuotas,
			Support:    Unsupported,
			Status:     StatusUnsupported,
			Note:       "订阅制属交付阶段 P4（ISSUE-005 §2）",
		})
	}

	return res, nil
}

// run 执行一项采集并记录结果。
//
// 声明 unsupported 的项**不执行**，直接记 unsupported —— 既省一次无谓请求，
// 也保证 items 里必然出现该项（AC-38）。
func (s *Syncer) run(
	_ context.Context, res *SyncResult, caps CapabilityMap, cap Capability,
	fn func() (rows int, failed int, note string, err error),
) {
	if caps[cap] == Unsupported {
		res.Items = append(res.Items, SyncItem{
			Capability: cap, Support: Unsupported, Status: StatusUnsupported,
			Note: "该站型不支持此能力（04 §3.4）",
		})
		return
	}

	start := time.Now()
	rows, failed, note, err := fn()
	item := SyncItem{
		Capability: cap,
		Support:    caps[cap],
		ElapsedMs:  time.Since(start).Milliseconds(),
		Rows:       rows, Failed: failed, Note: note,
	}
	switch {
	case err != nil && failed > 0:
		// 部分成功（只有 keys 会到这里）
		item.Status = StatusPartial
		item.Error = err.Error()
	case err != nil:
		item.Status = StatusFailed
		item.Error = err.Error()
		// ⚠️ degraded 能力返回 ErrUnsupported 是**实现 bug**，不是正常状态
		// （04 §3.4bis）。显式点出来，避免它被当成"这个站不支持"而忽略。
		if errors.Is(err, ErrUnsupported) && caps[cap] == Degraded {
			item.Note = "⚠️ 声明 degraded 却返回 ErrUnsupported —— " +
				"违反 04 §3.4bis，属实现缺陷"
		}
	case caps[cap] == Supported && rows == 0:
		item.Status = StatusFailed
		item.Error = "该能力声明 supported，但上游未返回任何数据"
		item.Note = joinNotes(note,
			"supported 能力必须产生数据；请检查上游端点或修正 Capabilities() 声明")
	default:
		item.Status = StatusOK
		if caps[cap] == Degraded && item.Note == "" {
			if rows == 0 {
				item.Note = "该能力声明 degraded；上游未返回可用数据"
			} else {
				item.Note = "该站型仅部分支持此能力；结果可能缺少字段"
			}
		}
	}
	res.Items = append(res.Items, item)
}

// degradedNote 把 Degraded/MissingFields 转成人可读的说明。
func degradedNote(m SourceMeta) string {
	if !m.Degraded {
		return ""
	}
	if len(m.MissingFields) == 0 {
		return "该站型此项为 degraded（部分字段缺失）"
	}
	return fmt.Sprintf("degraded：缺 %v，需人工补录（FR-011）", m.MissingFields)
}

// joinNotes 拼接多条备注，跳过空串。
//
// degraded 与"未登记模型"是两件独立的事，可能同时成立
// （如 Sub2API 既缺单价、又没有可路由模型），任一条被另一条挤掉都是信息丢失。
func joinNotes(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "；")
}

// HasFailure 报告是否有任何一项失败，供调用方决定 HTTP 状态码。
func (r *SyncResult) HasFailure() bool {
	for _, it := range r.Items {
		if it.Status == StatusFailed || it.Status == StatusPartial {
			return true
		}
	}
	return false
}
