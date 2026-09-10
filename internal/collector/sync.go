package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// ItemStatus 是 sync 逐项结果（09 §5.0bis 的五态）。
type ItemStatus string

const (
	StatusOK          ItemStatus = "ok"
	StatusPartial     ItemStatus = "partial" // 多账号采集部分成功
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
	SaveAccount(ctx context.Context, channelID, accountID int64, a Account) error
	// SaveGroups 写分组与分组可用模型（单事务）。
	SaveGroups(ctx context.Context, channelID int64, gs []Group) (int, error)
	// SaveKey 写一把 Key 的用量（每把一个事务）。
	// 返回 ErrKeyNotRegistered 表示上游有、库中无 —— 计入异常项而非失败。
	SaveKey(ctx context.Context, channelID, accountID int64, k Key) error
	// SavePricing 写价格快照、目录价格与已登记模型的价格版本。
	SavePricing(ctx context.Context, channelID int64, p Pricing) (PricingWriteResult, error)
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
	return selected.SyncMany(ctx, []Credential{cred})
}

// SyncManySelected 执行同一渠道多个账号的指定能力。
func (s *Syncer) SyncManySelected(
	ctx context.Context, creds []Credential, capabilities ...Capability,
) (*SyncResult, error) {
	selected := *s
	selected.only = capabilities
	return selected.SyncMany(ctx, creds)
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
	return s.SyncMany(ctx, []Credential{cred})
}

type syncAccount struct {
	cred Credential
	sess Session
}

// SyncMany 采集同一渠道下的全部账号，并把渠道级数据合并后各写一次。
func (s *Syncer) SyncMany(ctx context.Context, creds []Credential) (*SyncResult, error) {
	if len(creds) == 0 {
		return nil, fmt.Errorf("采集凭证为空")
	}
	res := &SyncResult{
		ChannelID: creds[0].ChannelID, SiteFamily: creds[0].Family,
		StartedAt: time.Now(),
	}
	defer func() { res.ElapsedMs = time.Since(res.StartedAt).Milliseconds() }()
	for _, cred := range creds[1:] {
		if cred.ChannelID != creds[0].ChannelID || cred.Family != creds[0].Family {
			return res, fmt.Errorf("%w: SyncMany 只接受同一渠道、同一站型的凭证", ErrPrecondition)
		}
	}

	caps := s.Adapter.Capabilities()
	accounts, authErrs := s.authenticate(ctx, creds)
	if len(accounts) == 0 {
		return res, fmt.Errorf("全部账号鉴权失败: %w", errors.Join(authErrs...))
	}

	if s.wants(CapAccount) {
		s.run(ctx, res, caps, CapAccount, func() (int, int, string, error) {
			return s.syncAccounts(ctx, accounts, authErrs)
		})
	}
	if s.wants(CapGroups) {
		s.run(ctx, res, caps, CapGroups, func() (int, int, string, error) {
			return s.syncGroups(ctx, accounts, authErrs)
		})
	}
	if s.wants(CapKeys) {
		s.run(ctx, res, caps, CapKeys, func() (int, int, string, error) {
			return s.syncKeys(ctx, accounts, authErrs)
		})
	}
	if s.wants(CapPricing) {
		s.run(ctx, res, caps, CapPricing, func() (int, int, string, error) {
			return s.syncPricing(ctx, accounts, authErrs)
		})
	}
	if s.wants(CapModelCatalog) {
		s.run(ctx, res, caps, CapModelCatalog, func() (int, int, string, error) {
			return s.syncCatalog(ctx, accounts, authErrs)
		})
	}
	return res, nil
}

func (s *Syncer) authenticate(ctx context.Context, creds []Credential) ([]syncAccount, []error) {
	accounts := make([]syncAccount, 0, len(creds))
	var errs []error
	for _, cred := range creds {
		if s.Auth != nil && s.Refresher != nil {
			fresh, err := s.Auth.EnsureFresh(ctx, cred, s.Refresher, time.Now())
			if err != nil {
				errs = append(errs, accountErr(cred, "续期", err))
				continue
			}
			cred = fresh
		}
		sess, err := s.Adapter.Authenticate(ctx, cred)
		if err != nil {
			errs = append(errs, accountErr(cred, "鉴权", err))
			continue
		}
		accounts = append(accounts, syncAccount{cred: cred, sess: sess})
	}
	return accounts, errs
}

func (s *Syncer) syncAccounts(
	ctx context.Context, accounts []syncAccount, baseErrs []error,
) (int, int, string, error) {
	errs := append([]error{}, baseErrs...)
	var rows int
	var notes []string
	for i := range accounts {
		a, err := s.Adapter.FetchAccount(ctx, accounts[i].sess)
		if err == nil {
			// 账号响应可能补全 external_user_id；后续同一会话的分组/Key
			// 请求必须立即带上它。
			if a.UserID != "" && accounts[i].sess.ExternalUserID == "" {
				accounts[i].sess.ExternalUserID = a.UserID
			}
			err = s.Sink.SaveAccount(ctx, accounts[i].cred.ChannelID, accounts[i].cred.AccountID, a)
		}
		if err != nil {
			errs = append(errs, accountErr(accounts[i].cred, "账号", err))
			continue
		}
		rows++
		notes = append(notes, degradedNote(a.Meta))
	}
	return capabilityResult(rows, errs, joinNotes(notes...))
}

func (s *Syncer) syncGroups(
	ctx context.Context, accounts []syncAccount, baseErrs []error,
) (int, int, string, error) {
	errs := append([]error{}, baseErrs...)
	var all []Group
	for _, account := range accounts {
		groups, err := s.Adapter.FetchGroups(ctx, account.sess)
		if err != nil {
			errs = append(errs, accountErr(account.cred, "分组", err))
			continue
		}
		all = append(all, groups...)
	}
	groups := mergeGroups(all)
	if len(errs) > 0 {
		for i := range groups {
			groups[i].Meta.Partial = true
		}
	}
	n, err := s.Sink.SaveGroups(ctx, accounts[0].cred.ChannelID, groups)
	if err != nil {
		return 0, 0, "", err
	}
	var notes []string
	for _, group := range groups {
		notes = append(notes, degradedNote(group.Meta))
	}
	return capabilityResult(n, errs, joinNotes(notes...))
}

func (s *Syncer) syncKeys(
	ctx context.Context, accounts []syncAccount, baseErrs []error,
) (int, int, string, error) {
	errs := append([]error{}, baseErrs...)
	var rows, unregistered int
	for _, account := range accounts {
		keys, err := s.Adapter.FetchKeys(ctx, account.sess)
		if err != nil {
			errs = append(errs, accountErr(account.cred, "Key", err))
			continue
		}
		for _, key := range keys {
			err := s.Sink.SaveKey(ctx, account.cred.ChannelID, account.cred.AccountID, key)
			switch {
			case err == nil:
				rows++
			case errors.Is(err, ErrKeyNotRegistered):
				rows++
				unregistered++
			default:
				errs = append(errs, accountErr(account.cred, "Key "+key.KeyRef, err))
			}
		}
	}
	note := ""
	if unregistered > 0 {
		note = fmt.Sprintf("%d 把 Key 上游存在但库中未登记，需补登记（计入异常项）", unregistered)
	}
	return capabilityResult(rows, errs, note)
}

func (s *Syncer) syncPricing(
	ctx context.Context, accounts []syncAccount, baseErrs []error,
) (int, int, string, error) {
	errs := append([]error{}, baseErrs...)
	var all []Pricing
	for _, account := range accounts {
		pricing, err := s.Adapter.FetchPricing(ctx, account.sess)
		if err != nil {
			errs = append(errs, accountErr(account.cred, "价格", err))
			continue
		}
		all = append(all, pricing)
	}
	pricing := mergePricing(all)
	if len(errs) > 0 {
		pricing.Meta.Partial = true
	}
	written, err := s.Sink.SavePricing(ctx, accounts[0].cred.ChannelID, pricing)
	if err != nil {
		return 0, 0, "", err
	}
	note := degradedNote(pricing.Meta)
	if written.CatalogUpdates > 0 || written.PriceVersions > 0 || written.UnregisteredModels > 0 {
		note = joinNotes(note, fmt.Sprintf(
			"价格快照=%d，目录价格更新=%d，price_versions=%d，未登记为可路由模型=%d",
			written.Snapshots, written.CatalogUpdates, written.PriceVersions,
			written.UnregisteredModels))
	}
	return capabilityResult(written.Snapshots, errs, note)
}

func (s *Syncer) syncCatalog(
	ctx context.Context, accounts []syncAccount, baseErrs []error,
) (int, int, string, error) {
	errs := append([]error{}, baseErrs...)
	var all []CatalogModel
	for _, account := range accounts {
		catalog, err := s.Adapter.FetchModelCatalog(ctx, account.sess)
		if err != nil {
			errs = append(errs, accountErr(account.cred, "目录", err))
			continue
		}
		all = append(all, catalog...)
	}
	catalog := mergeCatalog(all)
	if len(errs) > 0 {
		// 多账号渠道的目录是各账号结果的并集；任一账号失败时不能把
		// 成功账号的部分结果当成完整轮次，否则失败账号独有的模型会被
		// 连续缺席判定误标为下架。仍保存已采到的模型，但不推进可靠轮次。
		for i := range catalog {
			catalog[i].Meta.Partial = true
		}
	}
	n, err := s.Sink.SaveCatalog(ctx, accounts[0].cred.ChannelID, catalog)
	if err != nil {
		return 0, 0, "", err
	}
	var notes []string
	for _, model := range catalog {
		notes = append(notes, degradedNote(model.Meta))
	}
	return capabilityResult(n, errs, joinNotes(notes...))
}

func capabilityResult(rows int, errs []error, note string) (int, int, string, error) {
	if len(errs) == 0 {
		return rows, 0, note, nil
	}
	if rows == 0 {
		return 0, 0, note, errors.Join(errs...)
	}
	return rows, len(errs), note, errors.Join(errs...)
}

func accountErr(cred Credential, stage string, err error) error {
	return fmt.Errorf("账号 %d %s失败: %w", cred.AccountID, stage, err)
}

func mergeGroups(all []Group) []Group {
	byRef := map[string]Group{}
	for _, group := range all {
		if current, ok := byRef[group.GroupRef]; ok {
			byRef[group.GroupRef] = mergeGroup(current, group)
			continue
		}
		group.AvailableModels = append([]string{}, group.AvailableModels...)
		byRef[group.GroupRef] = group
	}
	out := make([]Group, 0, len(byRef))
	for _, group := range byRef {
		sort.Strings(group.AvailableModels)
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GroupRef < out[j].GroupRef })
	return out
}

func mergeGroup(current, candidate Group) Group {
	merged := current
	if merged.RateMultiplier == 0 {
		merged.RateMultiplier = candidate.RateMultiplier
	}
	merged.AvailableModels = appendUnique(append([]string{}, merged.AvailableModels...), candidate.AvailableModels...)
	merged.Meta = mergeSourceMeta(current.Meta, candidate.Meta)
	return merged
}

func mergePricing(all []Pricing) Pricing {
	out := Pricing{GroupRatios: map[string]float64{}}
	models := map[string]ModelPrice{}
	for _, pricing := range all {
		out.Meta = mergeSourceMeta(out.Meta, pricing.Meta)
		for group, ratio := range pricing.GroupRatios {
			if current, exists := out.GroupRatios[group]; !exists || current == 0 {
				out.GroupRatios[group] = ratio
			}
		}
		for _, model := range pricing.Models {
			if current, exists := models[model.ModelName]; exists {
				models[model.ModelName] = mergeModelPrice(current, model)
			} else {
				models[model.ModelName] = model
			}
		}
	}
	for _, model := range models {
		out.Models = append(out.Models, model)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].ModelName < out.Models[j].ModelName })
	return out
}

func mergeCatalog(all []CatalogModel) []CatalogModel {
	models := map[string]CatalogModel{}
	for _, model := range all {
		if current, exists := models[model.ModelName]; exists {
			models[model.ModelName] = mergeCatalogModel(current, model)
		} else {
			models[model.ModelName] = model
		}
	}
	out := make([]CatalogModel, 0, len(models))
	for _, model := range models {
		out = append(out, model)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelName < out[j].ModelName })
	return out
}

func mergeModelPrice(current, candidate ModelPrice) ModelPrice {
	best, other := current, candidate
	if modelPriceScore(candidate) > modelPriceScore(current) {
		best, other = candidate, current
	}
	if best.InputPrice == 0 {
		best.InputPrice = other.InputPrice
	}
	if best.OutputPrice == 0 {
		best.OutputPrice = other.OutputPrice
	}
	if best.CachePrice == 0 {
		best.CachePrice = other.CachePrice
	}
	if best.BillingUnit == "" {
		best.BillingUnit = other.BillingUnit
	}
	return best
}

func modelPriceScore(m ModelPrice) int {
	score := 0
	if m.InputPrice != 0 {
		score++
	}
	if m.OutputPrice != 0 {
		score++
	}
	if m.CachePrice != 0 {
		score++
	}
	if m.BillingUnit != "" {
		score++
	}
	return score
}

func mergeCatalogModel(current, candidate CatalogModel) CatalogModel {
	best, other := current, candidate
	if catalogModelScore(candidate) > catalogModelScore(current) ||
		(catalogModelScore(candidate) == catalogModelScore(current) &&
			betterMeta(candidate.Meta, current.Meta)) {
		best, other = candidate, current
	}
	if best.InputPrice == 0 {
		best.InputPrice = other.InputPrice
	}
	if best.OutputPrice == 0 {
		best.OutputPrice = other.OutputPrice
	}
	if best.BillingUnit == "" {
		best.BillingUnit = other.BillingUnit
	}
	best.Meta = mergeSourceMeta(current.Meta, candidate.Meta)
	return best
}

func catalogModelScore(m CatalogModel) int {
	return modelPriceScore(ModelPrice{
		InputPrice: m.InputPrice, OutputPrice: m.OutputPrice,
		BillingUnit: m.BillingUnit,
	})
}

func betterMeta(a, b SourceMeta) bool {
	if sourceMetaScore(a) != sourceMetaScore(b) {
		return sourceMetaScore(a) > sourceMetaScore(b)
	}
	return a.FetchedAt.After(b.FetchedAt)
}

func mergeSourceMeta(current, candidate SourceMeta) SourceMeta {
	merged := current
	if sourceMetaEmpty(current) || betterMeta(candidate, current) {
		merged = candidate
	}
	merged.Degraded = current.Degraded || candidate.Degraded
	merged.Partial = current.Partial || candidate.Partial
	merged.Stale = current.Stale || candidate.Stale
	merged.MissingFields = appendUnique(
		append([]string{}, current.MissingFields...), candidate.MissingFields...)
	sort.Strings(merged.MissingFields)
	return merged
}

func sourceMetaEmpty(meta SourceMeta) bool {
	return meta.Source == "" && meta.Endpoint == "" && meta.FetchedAt.IsZero() &&
		meta.ValidUntil.IsZero() && !meta.Stale && !meta.Degraded &&
		len(meta.MissingFields) == 0 && !meta.Partial
}

func sourceMetaScore(meta SourceMeta) int {
	score := 0
	if !meta.Partial {
		score += 4
	}
	if !meta.Degraded {
		score += 2
	}
	score -= len(meta.MissingFields)
	if !meta.FetchedAt.IsZero() {
		score++
	}
	return score
}

func appendUnique(dst []string, values ...string) []string {
	for _, value := range values {
		if value != "" && !slices.Contains(dst, value) {
			dst = append(dst, value)
		}
	}
	return dst
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
		// 多账号采集部分成功：已保存可用账号的结果，同时保留失败信息。
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
