package collector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeSink 记录落库调用顺序，用于验证编排顺序（09 §5.0bis 冻结的依赖）。
type fakeSink struct {
	order        []string
	keyErrs      map[string]error // KeyRef → 返回的错误
	groupsErr    error
	pricingErr   error
	pricingRows  *int
	pricingStats *PricingWriteResult
	groups       []Group
	catalogs     [][]CatalogModel
}

func (f *fakeSink) SaveAccount(context.Context, int64, int64, Account) error {
	f.order = append(f.order, "account")
	return nil
}
func (f *fakeSink) SaveGroups(_ context.Context, _ int64, gs []Group) (int, error) {
	f.order = append(f.order, "groups")
	f.groups = append([]Group(nil), gs...)
	if f.groupsErr != nil {
		return 0, f.groupsErr
	}
	return len(gs), nil
}
func (f *fakeSink) SaveKey(_ context.Context, _, _ int64, k Key) error {
	f.order = append(f.order, "key:"+k.KeyRef)
	if f.keyErrs != nil {
		return f.keyErrs[k.KeyRef]
	}
	return nil
}
func (f *fakeSink) SavePricing(_ context.Context, _ int64, p Pricing) (PricingWriteResult, error) {
	f.order = append(f.order, "pricing")
	if f.pricingErr != nil {
		return PricingWriteResult{}, f.pricingErr
	}
	if f.pricingStats != nil {
		return *f.pricingStats, nil
	}
	if f.pricingRows != nil {
		return PricingWriteResult{Snapshots: *f.pricingRows}, nil
	}
	return PricingWriteResult{Snapshots: len(p.Models)}, nil
}
func (f *fakeSink) SaveCatalog(_ context.Context, _ int64, cs []CatalogModel) (int, error) {
	f.order = append(f.order, "catalog")
	f.catalogs = append(f.catalogs, cs)
	return len(cs), nil
}

// stubAdapter 是可控的适配器，用于编排测试。
type stubAdapter struct {
	caps     CapabilityMap
	authErr  error
	account  Account
	groups   []Group
	keys     []Key
	pricing  Pricing
	catalog  []CatalogModel
	groupErr error
}

type partialCatalogAdapter struct {
	stubAdapter
	catalogByToken    map[string][]CatalogModel
	catalogErrByToken map[string]error
}

func (s *partialCatalogAdapter) Authenticate(_ context.Context, cred Credential) (Session, error) {
	return Session{Token: cred.AccessToken}, nil
}

func (s *partialCatalogAdapter) FetchModelCatalog(_ context.Context, sess Session) ([]CatalogModel, error) {
	return s.catalogByToken[sess.Token], s.catalogErrByToken[sess.Token]
}

type partialGroupsAdapter struct {
	stubAdapter
	groupsByToken    map[string][]Group
	groupsErrByToken map[string]error
}

func (s *partialGroupsAdapter) Authenticate(_ context.Context, cred Credential) (Session, error) {
	return Session{Token: cred.AccessToken}, nil
}

func (s *partialGroupsAdapter) FetchGroups(_ context.Context, sess Session) ([]Group, error) {
	return s.groupsByToken[sess.Token], s.groupsErrByToken[sess.Token]
}

func (s *stubAdapter) Capabilities() CapabilityMap { return s.caps }
func (s *stubAdapter) Detect(context.Context, string) (DetectResult, error) {
	return DetectResult{}, nil
}
func (s *stubAdapter) Authenticate(context.Context, Credential) (Session, error) {
	return Session{}, s.authErr
}
func (s *stubAdapter) FetchAccount(context.Context, Session) (Account, error) {
	return s.account, nil
}
func (s *stubAdapter) FetchKeys(context.Context, Session) ([]Key, error) { return s.keys, nil }
func (s *stubAdapter) FetchGroups(context.Context, Session) ([]Group, error) {
	return s.groups, s.groupErr
}
func (s *stubAdapter) FetchPricing(context.Context, Session) (Pricing, error) {
	return s.pricing, nil
}
func (s *stubAdapter) FetchModelCatalog(context.Context, Session) ([]CatalogModel, error) {
	return s.catalog, nil
}

func fullCaps() CapabilityMap {
	return CapabilityMap{
		CapAccount: Supported, CapKeys: Supported, CapGroups: Supported,
		CapPricing: Supported, CapModelCatalog: Supported,
	}
}

// P1 只返回当前阶段真正采集的五类数据。P4 订阅不应以 unsupported
// 占位项混进 P1 响应，否则调用方仍被迫维护一个没有实现的未来能力。
func TestSyncReturnsOnlyP1Capabilities(t *testing.T) {
	ad := &stubAdapter{
		caps:    fullCaps(),
		account: Account{UserID: "u1"},
		groups:  []Group{{GroupRef: "default"}},
		keys:    []Key{{KeyRef: "k1"}},
		pricing: Pricing{Models: []ModelPrice{{ModelName: "m1"}}},
		catalog: []CatalogModel{{ModelName: "m1"}},
	}
	res, err := (&Syncer{Adapter: ad, Sink: &fakeSink{}}).Sync(
		context.Background(), Credential{ChannelID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 5 {
		t.Fatalf("P1 应只返回 5 个采集项，实际 %d：%+v", len(res.Items), res.Items)
	}
}

// **分组必须先于 Key**（09 §5.0bis）：upstream_keys.channel_group_id
// 是外键，反序会拿不到 id。
func TestSyncOrderGroupsBeforeKeys(t *testing.T) {
	sink := &fakeSink{}
	ad := &stubAdapter{
		caps:    fullCaps(),
		groups:  []Group{{GroupRef: "g1"}},
		keys:    []Key{{KeyRef: "k1"}},
		pricing: Pricing{Models: []ModelPrice{{ModelName: "m"}}},
		catalog: []CatalogModel{{ModelName: "m"}},
	}
	s := &Syncer{Adapter: ad, Sink: sink}
	if _, err := s.Sync(context.Background(), Credential{ChannelID: 1}); err != nil {
		t.Fatal(err)
	}

	gi, ki := -1, -1
	for i, o := range sink.order {
		if o == "groups" {
			gi = i
		}
		if o == "key:k1" {
			ki = i
		}
	}
	if gi < 0 || ki < 0 {
		t.Fatalf("落库顺序不完整: %v", sink.order)
	}
	if gi > ki {
		t.Fatalf("分组(%d) 晚于 Key(%d)——upstream_keys.channel_group_id 是外键，"+
			"反序会拿不到分组 id（09 §5.0bis）。实际顺序: %v", gi, ki, sink.order)
	}
	// 账号也必须最先（NewAPI 后续请求要它给的 external_user_id）
	if sink.order[0] != "account" {
		t.Errorf("首项应是 account（产出 external_user_id），实际 %v", sink.order)
	}
}

// 鉴权失败必须**整体中止且不写任何表**（09 §5.0bis ①）。
func TestSyncAbortsOnAuthFailureWithoutWriting(t *testing.T) {
	sink := &fakeSink{}
	ad := &stubAdapter{caps: fullCaps(), authErr: errors.New("401")}
	s := &Syncer{Adapter: ad, Sink: sink}

	_, err := s.Sync(context.Background(), Credential{ChannelID: 1})
	if err == nil {
		t.Fatal("鉴权失败应返回错误")
	}
	if len(sink.order) != 0 {
		t.Fatalf("鉴权失败后不得写任何表，实际写了 %v", sink.order)
	}
}

// **逐项独立提交**：一项失败不影响后续项（09 §5.0bis 的核心取舍）。
// "价格采到了但目录超时"没有理由把价格也丢掉。
func TestSyncItemsAreIndependent(t *testing.T) {
	sink := &fakeSink{groupsErr: errors.New("分组写库失败")}
	ad := &stubAdapter{
		caps:    fullCaps(),
		groups:  []Group{{GroupRef: "g1"}},
		keys:    []Key{{KeyRef: "k1"}},
		pricing: Pricing{Models: []ModelPrice{{ModelName: "m"}}},
		catalog: []CatalogModel{{ModelName: "m"}},
	}
	s := &Syncer{Adapter: ad, Sink: sink}
	res, err := s.Sync(context.Background(), Credential{ChannelID: 1})
	if err != nil {
		t.Fatalf("单项失败不该让整体返回 error: %v", err)
	}

	byCap := map[Capability]SyncItem{}
	for _, it := range res.Items {
		byCap[it.Capability] = it
	}
	if byCap[CapGroups].Status != StatusFailed {
		t.Errorf("分组应标 failed，实际 %s", byCap[CapGroups].Status)
	}
	// 后续项仍须执行
	if byCap[CapPricing].Status != StatusOK {
		t.Errorf("分组失败后价格仍应执行，实际 %s", byCap[CapPricing].Status)
	}
	if byCap[CapModelCatalog].Status != StatusOK {
		t.Errorf("分组失败后目录仍应执行，实际 %s", byCap[CapModelCatalog].Status)
	}
}

// 部分 Key 失败 → partial，且成功的那些已落库。
func TestSyncPartialOnSomeKeysFailing(t *testing.T) {
	sink := &fakeSink{keyErrs: map[string]error{"k2": errors.New("写库失败")}}
	ad := &stubAdapter{
		caps: fullCaps(),
		keys: []Key{{KeyRef: "k1"}, {KeyRef: "k2"}, {KeyRef: "k3"}},
	}
	s := &Syncer{Adapter: ad, Sink: sink}
	res, _ := s.Sync(context.Background(), Credential{ChannelID: 1})

	var item SyncItem
	for _, it := range res.Items {
		if it.Capability == CapKeys {
			item = it
		}
	}
	if item.Status != StatusPartial {
		t.Fatalf("部分 Key 失败应标 partial，实际 %s", item.Status)
	}
	if item.Rows != 2 || item.Failed != 1 {
		t.Errorf("应 2 成功 1 失败，实际 rows=%d failed=%d", item.Rows, item.Failed)
	}
}

// 未登记的 Key **不算失败**（02 §1.3bis）：采集不自动创建 Key，
// 计入异常项提示补登记。
func TestSyncUnregisteredKeyIsNotFailure(t *testing.T) {
	sink := &fakeSink{keyErrs: map[string]error{"k2": ErrKeyNotRegistered}}
	ad := &stubAdapter{
		caps: fullCaps(),
		keys: []Key{{KeyRef: "k2"}},
	}
	s := &Syncer{Adapter: ad, Sink: sink}
	res, _ := s.Sync(context.Background(), Credential{ChannelID: 1})

	var item SyncItem
	for _, it := range res.Items {
		if it.Capability == CapKeys {
			item = it
		}
	}
	if item.Status != StatusOK {
		t.Fatalf("未登记的 Key 不该让该项失败（采集不自动建 Key，02 §1.3bis），"+
			"实际 %s", item.Status)
	}
	if item.Rows != 1 {
		t.Fatalf("未登记 Key 已记录异常快照，应计为 1 条采集数据，实际 rows=%d", item.Rows)
	}
	if item.Note == "" {
		t.Error("应在 note 里提示需补登记（计入 inventory 异常项）")
	}
}

// AC-38：不支持的项**必须出现在 items 里**，不能静默省略，
// 且须与 Capabilities() 声明一致。
func TestSyncReportsUnsupportedExplicitly(t *testing.T) {
	sink := &fakeSink{}
	// 一种能力不全的站型形态：keys/groups 不支持（自研站常是这样，
	// 自建面板往往只有余额页，没有令牌/分组管理的 API）
	ad := &stubAdapter{caps: CapabilityMap{
		CapAccount: Supported, CapKeys: Unsupported, CapGroups: Unsupported,
		CapPricing: Degraded, CapModelCatalog: Degraded,
	}}
	s := &Syncer{Adapter: ad, Sink: sink}
	res, err := s.Sync(context.Background(), Credential{ChannelID: 1})
	if err != nil {
		t.Fatal(err)
	}

	byCap := map[Capability]SyncItem{}
	for _, it := range res.Items {
		byCap[it.Capability] = it
	}
	for _, cap := range []Capability{CapKeys, CapGroups} {
		it, ok := byCap[cap]
		if !ok {
			t.Errorf("%s 声明 unsupported，但 items 里没有它 —— "+
				"AC-38 要求明确返回不支持而非静默留空", cap)
			continue
		}
		if it.Status != StatusUnsupported {
			t.Errorf("%s 应标 unsupported，实际 %s", cap, it.Status)
		}
		if it.Support != Unsupported {
			t.Errorf("%s support = %q，期望 unsupported", cap, it.Support)
		}
		if it.Note == "" {
			t.Errorf("%s 必须解释为何 unsupported", cap)
		}
	}
	// unsupported 的项不该真去请求上游（省一次无谓请求）
	for _, o := range sink.order {
		if o == "groups" {
			t.Error("声明 unsupported 的分组不该执行落库")
		}
	}
}

// degraded 项的结果应带 note 说明缺什么（供 inventory 的待补录计数）。
func TestSyncDegradedCarriesNote(t *testing.T) {
	meta := SourceMeta{Degraded: true, MissingFields: []string{"input_price"}}
	sink := &fakeSink{}
	ad := &stubAdapter{
		caps:    fullCaps(),
		pricing: Pricing{Meta: meta, Models: []ModelPrice{{ModelName: "m"}}},
	}
	s := &Syncer{Adapter: ad, Sink: sink}
	res, _ := s.Sync(context.Background(), Credential{ChannelID: 1})

	for _, it := range res.Items {
		if it.Capability == CapPricing {
			if it.Status != StatusOK {
				t.Errorf("degraded 仍应 ok（有数据），实际 %s", it.Status)
			}
			if it.Note == "" {
				t.Error("degraded 项应在 note 里说明缺哪些字段")
			}
			return
		}
	}
	t.Error("未找到 pricing 项")
}

func TestMergePricingPreservesFirstDegradedFetchTime(t *testing.T) {
	at := time.Unix(123, 0)
	got := mergePricing([]Pricing{{Meta: SourceMeta{
		Source: "api", Endpoint: "/pricing", FetchedAt: at,
		Degraded: true, MissingFields: []string{"input_price"},
	}}})
	if !got.Meta.FetchedAt.Equal(at) || got.Meta.Source != "api" {
		t.Fatalf("首个 degraded 结果的来源时间不能丢失，得到 %+v", got.Meta)
	}
}

func TestMergeGroupsPreservesDegradedMetadataAcrossAccounts(t *testing.T) {
	merged := mergeGroups([]Group{
		{GroupRef: "shared", AvailableModels: []string{"m1"}, Meta: SourceMeta{
			FetchedAt: time.Unix(2, 0), Degraded: true, MissingFields: []string{"available_models"},
		}},
		{GroupRef: "shared", AvailableModels: []string{"m2"}, Meta: SourceMeta{
			FetchedAt: time.Unix(3, 0),
		}},
	})
	if len(merged) != 1 {
		t.Fatalf("重复分组应合并为一行，得到 %d", len(merged))
	}
	got := merged[0].Meta
	if !got.Degraded || !containsString(got.MissingFields, "available_models") {
		t.Fatalf("合并后必须保留任一账号的 degraded/缺失字段，得到 %+v", got)
	}
}

// 采到 0 行必须说出来 —— "ok + rows=0 + 无 note" 读起来与"采集成功且有内容"
// 一模一样，那正是 AC-38 点名的静默留空。
//
// 靶子取真形态：2026-09-01 对真 Sub2API 站（upstream-b.invalid 0.1.183）跑 AC-38，
// model_catalog 返回的就是 ok / 无 rows / 无 note —— 因为该族目录派生自
// /api/v1/groups/available 的 available_models，而真站点一个都不返回。
// 内网真库佐证：13 个 sub2api 渠道合计 0 行目录，同库 newapi 是 2905 行。
func TestSyncSupportedEmptyResultFails(t *testing.T) {
	sink := &fakeSink{}
	// 全部声明 supported，但适配器什么都没采到（上游没给）
	ad := &stubAdapter{caps: fullCaps()}
	s := &Syncer{Adapter: ad, Sink: sink}
	res, err := s.Sync(context.Background(), Credential{ChannelID: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		if it.Rows > 0 {
			continue
		}
		if it.Support != Supported {
			t.Errorf("%s support = %q，期望 supported", it.Capability, it.Support)
		}
		if it.Status != StatusFailed || it.Error == "" {
			t.Errorf("%s 声明 supported 却落 0 行，应 failed 并解释；实际 status=%s error=%q",
				it.Capability, it.Status, it.Error)
		}
	}
}

func TestSyncSupportedZeroRowsFailsEvenWithNote(t *testing.T) {
	ad := &stubAdapter{
		caps: fullCaps(),
		pricing: Pricing{Meta: SourceMeta{
			Degraded: true, MissingFields: []string{"model_price"},
		}},
	}
	res, err := (&Syncer{Adapter: ad, Sink: &fakeSink{}}).Sync(
		context.Background(), Credential{ChannelID: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		if it.Capability == CapPricing {
			if it.Status != StatusFailed {
				t.Fatalf("supported 零行即使有 note 也必须 failed，得到 %+v", it)
			}
			return
		}
	}
	t.Fatal("未找到 pricing 项")
}

func TestSyncPricingRowsCountPersistedSnapshotsWhenVersionsSkipped(t *testing.T) {
	ad := &stubAdapter{
		caps:    fullCaps(),
		pricing: Pricing{Models: []ModelPrice{{ModelName: "m"}}},
	}
	res, err := (&Syncer{Adapter: ad, Sink: &fakeSink{pricingStats: &PricingWriteResult{
		UnregisteredModels: 1,
	}}}).Sync(
		context.Background(), Credential{ChannelID: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		if it.Capability == CapPricing {
			if it.Status != StatusFailed || it.Rows != 0 {
				t.Fatalf("未写价格快照时 rows 应为 0 且 supported 应失败，得到 %+v", it)
			}
			if !strings.Contains(it.Note, "未登记为可路由模型") {
				t.Fatalf("未登记模型应在 note 中说明，得到 %q", it.Note)
			}
			return
		}
	}
	t.Fatal("未找到 pricing 项")
}

func TestSyncPricingNoteUsesPricingWriteStats(t *testing.T) {
	ad := &stubAdapter{
		caps:    fullCaps(),
		pricing: Pricing{Models: []ModelPrice{{ModelName: "m"}}},
	}
	sink := &fakeSink{pricingStats: &PricingWriteResult{
		Snapshots: 1, CatalogUpdates: 0, PriceVersions: 0, UnregisteredModels: 1,
	}}
	res, err := (&Syncer{Adapter: ad, Sink: sink}).Sync(
		context.Background(), Credential{ChannelID: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		if it.Capability != CapPricing {
			continue
		}
		if it.Rows != 1 || !strings.Contains(it.Note, "未登记为可路由模型=1") ||
			!strings.Contains(it.Note, "目录价格更新=0") {
			t.Fatalf("价格说明必须使用结构化写入统计，得到 %+v", it)
		}
		return
	}
	t.Fatal("未找到 pricing 项")
}

func TestSyncDegradedEmptyResultIsExplicit(t *testing.T) {
	ad := &stubAdapter{caps: CapabilityMap{CapGroups: Degraded}}
	res, err := (&Syncer{Adapter: ad, Sink: &fakeSink{}}).Sync(
		context.Background(), Credential{ChannelID: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		if it.Capability != CapGroups {
			continue
		}
		if it.Support != Degraded || it.Note == "" {
			t.Fatalf("degraded 空结果必须返回声明与说明，得到 %+v", it)
		}
		return
	}
	t.Fatal("未找到 groups 项")
}

func TestSyncPartialCatalogDoesNotAdvanceReliableRound(t *testing.T) {
	sink := &fakeSink{}
	ad := &partialCatalogAdapter{
		stubAdapter: stubAdapter{caps: CapabilityMap{CapModelCatalog: Supported}},
		catalogByToken: map[string][]CatalogModel{
			"account-a": {{ModelName: "model-a"}},
		},
		catalogErrByToken: map[string]error{
			"account-b": errors.New("账号 b 目录请求失败"),
		},
	}
	res, err := (&Syncer{Adapter: ad, Sink: sink}).SyncManySelected(
		context.Background(), []Credential{
			{ChannelID: 1, AccessToken: "account-a"},
			{ChannelID: 1, AccessToken: "account-b"},
		}, CapModelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.catalogs) != 1 || len(sink.catalogs[0]) != 1 {
		t.Fatalf("成功账号的目录仍应保存一次，得到 %+v", sink.catalogs)
	}
	if !sink.catalogs[0][0].Meta.Partial {
		t.Fatal("账号级目录部分失败时，合并结果必须标 partial，禁止推进可靠采集轮次")
	}
	if len(res.Items) != 1 || res.Items[0].Status != StatusPartial {
		t.Fatalf("目录部分失败应报告 partial，得到 %+v", res.Items)
	}
}

func TestSyncPartialGroupsMarksMergedRowsPartial(t *testing.T) {
	sink := &fakeSink{}
	ad := &partialGroupsAdapter{
		stubAdapter: stubAdapter{caps: CapabilityMap{CapGroups: Supported}},
		groupsByToken: map[string][]Group{
			"account-a": {{GroupRef: "vip", AvailableModels: []string{"model-a"}}},
		},
		groupsErrByToken: map[string]error{
			"account-b": errors.New("账号 b 分组请求失败"),
		},
	}
	res, err := (&Syncer{Adapter: ad, Sink: sink}).SyncManySelected(
		context.Background(), []Credential{
			{ChannelID: 1, AccessToken: "account-a"},
			{ChannelID: 1, AccessToken: "account-b"},
		}, CapGroups)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.groups) != 1 || !sink.groups[0].Meta.Partial {
		t.Fatalf("分组部分失败时合并结果必须标 partial，得到 %+v", sink.groups)
	}
	if len(res.Items) != 1 || res.Items[0].Status != StatusPartial {
		t.Fatalf("分组部分失败应报告 partial，得到 %+v", res.Items)
	}
}

func TestMergeGroupsPrefersCompleteMetadataAndUnionsModels(t *testing.T) {
	first := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)
	got := mergeGroups([]Group{
		{GroupRef: "vip", AvailableModels: []string{"model-a"}, Meta: SourceMeta{
			FetchedAt: first, Degraded: true, MissingFields: []string{"available_models"},
		}},
		{GroupRef: "vip", RateMultiplier: 0.5, AvailableModels: []string{"model-b"}, Meta: SourceMeta{
			FetchedAt: second,
		}},
	})
	if len(got) != 1 {
		t.Fatalf("重复分组应合并为一行，得到 %+v", got)
	}
	group := got[0]
	if group.RateMultiplier != 0.5 || !group.Meta.Degraded ||
		!containsString(group.Meta.MissingFields, "available_models") || len(group.AvailableModels) != 2 {
		t.Fatalf("应保留完整倍率并传播任一账号的降级元数据，得到 %+v", group)
	}
	if !strings.EqualFold(strings.Join(group.AvailableModels, ","), "model-a,model-b") {
		t.Fatalf("模型并集顺序不稳定，得到 %v", group.AvailableModels)
	}
}

func TestMergePricingAndCatalogPreferRicherDuplicateData(t *testing.T) {
	first := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)
	pricing := mergePricing([]Pricing{
		{Meta: SourceMeta{FetchedAt: first, Degraded: true, MissingFields: []string{"input_price"}},
			Models: []ModelPrice{{ModelName: "m", InputPrice: 0}}},
		{Meta: SourceMeta{FetchedAt: second}, Models: []ModelPrice{{
			ModelName: "m", InputPrice: 0.5, OutputPrice: 1, BillingUnit: "per_1m_token",
		}}},
	})
	if len(pricing.Models) != 1 || pricing.Models[0].InputPrice != 0.5 ||
		pricing.Models[0].OutputPrice != 1 || pricing.Models[0].BillingUnit != "per_1m_token" ||
		!pricing.Meta.Degraded || !containsString(pricing.Meta.MissingFields, "input_price") {
		t.Fatalf("价格重复项应保留完整数据并传播降级元数据，得到 %+v", pricing)
	}

	catalog := mergeCatalog([]CatalogModel{
		{ModelName: "m", Meta: SourceMeta{FetchedAt: first, Degraded: true}, BillingUnit: ""},
		{ModelName: "m", InputPrice: 0.5, OutputPrice: 1, BillingUnit: "per_call",
			Meta: SourceMeta{FetchedAt: second}},
	})
	if len(catalog) != 1 || catalog[0].InputPrice != 0.5 || catalog[0].OutputPrice != 1 ||
		catalog[0].BillingUnit != "per_call" || !catalog[0].Meta.Degraded {
		t.Fatalf("目录重复项应保留完整数据并传播降级元数据，得到 %+v", catalog)
	}
}

func TestSyncManyRejectsMixedChannelOrFamily(t *testing.T) {
	tests := []struct {
		name  string
		creds []Credential
	}{
		{name: "channel", creds: []Credential{{ChannelID: 1, Family: FamilyNewAPI}, {ChannelID: 2, Family: FamilyNewAPI}}},
		{name: "family", creds: []Credential{{ChannelID: 1, Family: FamilyNewAPI}, {ChannelID: 1, Family: FamilySub2API}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			ad := &stubAdapter{caps: fullCaps()}
			res, err := (&Syncer{Adapter: ad, Sink: sink}).SyncMany(context.Background(), tc.creds)
			if err == nil || !errors.Is(err, ErrPrecondition) {
				t.Fatalf("混合渠道/站型必须在触达上游前返回 ErrPrecondition，res=%+v err=%v", res, err)
			}
			if len(sink.order) != 0 {
				t.Fatalf("前置校验失败不得写入，实际 %v", sink.order)
			}
		})
	}
}

func TestSyncFindsDegradationOutsideFirstGroup(t *testing.T) {
	ad := &stubAdapter{
		caps: CapabilityMap{CapGroups: Degraded},
		groups: []Group{
			{GroupRef: "complete"},
			{GroupRef: "partial", Meta: SourceMeta{
				Degraded: true, MissingFields: []string{"available_models"},
			}},
		},
	}
	res, _ := (&Syncer{Adapter: ad, Sink: &fakeSink{}}).Sync(
		context.Background(), Credential{ChannelID: 1})
	for _, it := range res.Items {
		if it.Capability == CapGroups {
			if !strings.Contains(it.Note, "available_models") {
				t.Fatalf("必须扫描所有分组的降级元数据，得到 note=%q", it.Note)
			}
			return
		}
	}
	t.Fatal("未找到 groups 项")
}

// 声明 degraded 却返回 ErrUnsupported 是实现 bug，必须被显式点出
// 而不是当成"这个站不支持"（04 §3.4bis）。
func TestSyncFlagsDegradedReturningUnsupported(t *testing.T) {
	sink := &fakeSink{}
	ad := &stubAdapter{
		caps:     CapabilityMap{CapGroups: Degraded},
		groupErr: ErrUnsupported,
	}
	s := &Syncer{Adapter: ad, Sink: sink}
	res, _ := s.Sync(context.Background(), Credential{ChannelID: 1})

	for _, it := range res.Items {
		if it.Capability == CapGroups {
			if it.Status != StatusFailed {
				t.Errorf("应标 failed，实际 %s", it.Status)
			}
			if it.Note == "" {
				t.Error("应显式指出这违反 04 §3.4bis 属实现缺陷，" +
					"否则会被当成'该站不支持'而忽略")
			}
			return
		}
	}
	t.Error("未找到 groups 项")
}

func TestSyncResultHasFailure(t *testing.T) {
	r := &SyncResult{Items: []SyncItem{{Status: StatusOK}, {Status: StatusUnsupported}}}
	if r.HasFailure() {
		t.Error("ok + unsupported 不该算失败")
	}
	r.Items = append(r.Items, SyncItem{Status: StatusPartial})
	if !r.HasFailure() {
		t.Error("partial 应算失败")
	}
}

func TestSyncSelectedRunsOnlyRequestedCapabilities(t *testing.T) {
	sink := &fakeSink{}
	ad := &stubAdapter{
		caps:    fullCaps(),
		groups:  []Group{{GroupRef: "g1"}},
		catalog: []CatalogModel{{ModelName: "m1"}},
	}
	res, err := (&Syncer{Adapter: ad, Sink: sink}).SyncSelected(
		context.Background(), Credential{ChannelID: 1},
		CapGroups, CapModelCatalog,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sink.order, ","); got != "groups,catalog" {
		t.Fatalf("只应执行选中的能力，实际顺序 %q", got)
	}
	if len(res.Items) != 2 {
		t.Fatalf("逐项结果数 = %d，期望 2", len(res.Items))
	}
	if res.Items[0].Capability != CapGroups || res.Items[1].Capability != CapModelCatalog {
		t.Fatalf("逐项结果 = %+v", res.Items)
	}
}
