package collector

import (
	"context"
	"errors"
	"testing"
)

// fakeSink 记录落库调用顺序，用于验证编排顺序（09 §5.0bis 冻结的依赖）。
type fakeSink struct {
	order      []string
	keyErrs    map[string]error // KeyRef → 返回的错误
	groupsErr  error
	pricingErr error
}

func (f *fakeSink) SaveAccount(context.Context, int64, Account) error {
	f.order = append(f.order, "account")
	return nil
}
func (f *fakeSink) SaveGroups(_ context.Context, _ int64, gs []Group) (int, error) {
	f.order = append(f.order, "groups")
	if f.groupsErr != nil {
		return 0, f.groupsErr
	}
	return len(gs), nil
}
func (f *fakeSink) SaveKey(_ context.Context, _ int64, k Key) error {
	f.order = append(f.order, "key:"+k.KeyRef)
	if f.keyErrs != nil {
		return f.keyErrs[k.KeyRef]
	}
	return nil
}
func (f *fakeSink) SavePricing(_ context.Context, _ int64, p Pricing) (int, error) {
	f.order = append(f.order, "pricing")
	if f.pricingErr != nil {
		return 0, f.pricingErr
	}
	return len(p.Models), nil
}
func (f *fakeSink) SaveCatalog(_ context.Context, _ int64, cs []CatalogModel) (int, error) {
	f.order = append(f.order, "catalog")
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
func (s *stubAdapter) FetchSubscriptionQuotas(context.Context, Session) ([]SubscriptionQuota, error) {
	return nil, ErrUnsupported
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
		CapSubscriptionQuotas: Unsupported,
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
		keys: []Key{{KeyRef: "k1"}, {KeyRef: "k2"}},
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
	if item.Note == "" {
		t.Error("应在 note 里提示需补登记（计入 inventory 异常项）")
	}
}

// AC-38：不支持的项**必须出现在 items 里**，不能静默省略，
// 且须与 Capabilities() 声明一致。
func TestSyncReportsUnsupportedExplicitly(t *testing.T) {
	sink := &fakeSink{}
	// ASXS 形态：keys/groups 不支持
	ad := &stubAdapter{caps: CapabilityMap{
		CapAccount: Supported, CapKeys: Unsupported, CapGroups: Unsupported,
		CapPricing: Degraded, CapModelCatalog: Degraded,
		CapSubscriptionQuotas: Unsupported,
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
	for _, cap := range []Capability{CapKeys, CapGroups, CapSubscriptionQuotas} {
		it, ok := byCap[cap]
		if !ok {
			t.Errorf("%s 声明 unsupported，但 items 里没有它 —— "+
				"AC-38 要求明确返回不支持而非静默留空", cap)
			continue
		}
		if it.Status != StatusUnsupported {
			t.Errorf("%s 应标 unsupported，实际 %s", cap, it.Status)
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
