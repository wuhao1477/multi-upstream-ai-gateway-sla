package collector

import (
	"os"
	"regexp"
	"testing"
)

// 本文件是九处 switch 的替代守卫。
//
// 收表之前，"加站型漏改一处"的检出靠人；收表之后靠这四条 —— 它们全是对
// 注册表本身的静态断言，**不新增任何 mock**（CLAUDE.md §1：这里没有被测
// 依赖，被测对象就是注册表这份数据）。

// famConstRE 匹配 collector.go 里的 Family 常量定义。
var famConstRE = regexp.MustCompile(`Family(\w+)\s+Family = "([^"]+)"`)

// TestEveryFamilyConstantIsRegistered 断言每个 Family 常量都有注册。
//
// 为什么扫源码而不是手写一张清单：手写的清单要人去加，而"忘了加"正是本文件
// 要防的那件事 —— 清单漏一项与注册漏一项一样静默。扫源码则是加常量的那一刻
// 自动被纳入。unknown 例外：它是"探测未命中"的哨兵，按 04 §7 明确不许有适配器。
func TestEveryFamilyConstantIsRegistered(t *testing.T) {
	src, err := os.ReadFile("collector.go")
	if err != nil {
		t.Fatalf("读 collector.go: %v", err)
	}
	ms := famConstRE.FindAllStringSubmatch(string(src), -1)
	// 正则失配会让这条断言空过（扫到 0 个常量 → 循环不执行 → 绿），
	// 所以先钉住"扫到的数量"。
	//
	// 下界**由注册表推导**：已注册家族各一个常量，加 unknown 哨兵一个。
	// 原先写死 4，而删掉一族之后它会红在这里 —— 那次红报的是"常量写法变了"，
	// 与真实原因无关，于是唯一的修法是把 4 改成 3，也就是**每次加删家族都要
	// 手改这个数**。手改的数字迟早会被改成"让它变绿的那个值"，那时整条守卫
	// 就静默失效了（本仓库踩过：test-migrate.sh 的 `-ge 45` 在删了 28 张表
	// 之后照样绿，见 016 头部）。推导则不需要人动。
	if want := len(All()) + 1; len(ms) < want {
		t.Fatalf("在 collector.go 里只扫到 %d 个 Family 常量（预期 ≥%d："+
			"%d 个已注册家族 + unknown 哨兵）—— 常量写法变了？"+
			"本断言的正则要跟着改，否则它在空转", len(ms), want, len(All()))
	}
	for _, m := range ms {
		name, val := m[1], Family(m[2])
		if val == FamilyUnknown {
			if _, ok := Lookup(val); ok {
				t.Errorf("Family%s(%q) 竟有注册 —— unknown 是"+
					"「探测未命中」的哨兵，有适配器意味着会去猜家族（04 §7 禁止）", name, val)
			}
			continue
		}
		if _, ok := Lookup(val); !ok {
			t.Errorf("Family%s(%q) 没有注册：加站型要同时加一份 Registration"+
				"（register_*.go）—— 否则采集时报「无对应适配器」，"+
				"而鉴权头与续期阈值会静默走空", name, val)
		}
	}
}

// TestRegistrationsComplete 断言每条注册的必填字段都不空。
//
// 缺 ProbePath/Match → 该站型永远探测不到；缺 New → 采集时才报无适配器；
// 缺 CredType → cred_type 空串进库；缺 DisplayName → 错误信息里出现空家族名。
func TestRegistrationsComplete(t *testing.T) {
	for _, r := range All() {
		if r.Family == "" || r.Family == FamilyUnknown {
			t.Errorf("有一条注册的 Family 为 %q", r.Family)
		}
		if r.DisplayName == "" {
			t.Errorf("%s: DisplayName 为空（凭证校验的错误信息会出现空家族名）", r.Family)
		}
		if r.ProbePath == "" || r.Match == nil {
			t.Errorf("%s: 缺 ProbePath 或 Match —— Detect 永远不会归到这一族", r.Family)
		}
		if r.CredType == "" {
			t.Errorf("%s: CredType 为空 —— 凭证会以空 cred_type 落库", r.Family)
		}
		if r.CredNote == "" {
			t.Errorf("%s: CredNote 为空 —— 运维看到的会是一句没有「为什么」的拒绝", r.Family)
		}
		if r.New == nil {
			t.Errorf("%s: New 为 nil —— 采集时才会报「无对应适配器」", r.Family)
		} else if ad := r.New(nil); ad == nil {
			t.Errorf("%s: New 返回了 nil Adapter", r.Family)
		}
	}
}

// TestAliasesUniqueAcrossFamilies 断言没有两个站型认领同一个别名。
//
// 重复的后果不是报错而是**随机分流**：FamilyOfAlias 按注册顺序返回第一个命中，
// 于是导入侧的"声明与探测不符"判定会时对时错 —— 而那个判定正是 04 铁律
// （不信任导出的 site_type）的落地处。
func TestAliasesUniqueAcrossFamilies(t *testing.T) {
	owner := map[string]Family{}
	for _, r := range All() {
		for _, a := range r.Aliases {
			if prev, dup := owner[a]; dup {
				t.Errorf("别名 %q 被 %s 与 %s 同时认领 —— FamilyOfAlias 只会返回"+
					"注册顺序在前的那个，导入侧的声明比对会静默偏向它", a, prev, r.Family)
				continue
			}
			owner[a] = r.Family
			// 大小写不统一同样会静默失配：FamilyOfAlias 用小写比对。
			if a == "" || a != lower(a) {
				t.Errorf("%s 的别名 %q 不是小写 —— FamilyOfAlias 比对前会小写化，"+
					"这条别名永远匹配不上", r.Family, a)
			}
		}
	}
	if len(owner) == 0 {
		t.Fatal("没有任何别名 —— 导入侧的声明比对整块失效了")
	}
}

// TestRefreshLeadMatchesRefresherImplementation 双向钉住"要不要续期"。
//
// RefreshLead>0 却没有 Refresher：NeedsRefresh 说该刷了，而 Syncer 拿不到
// 刷新器，凭证到期后一路 401。
// RefreshLead==0 却有 Refresher：对 NewAPI 就是不变式 N-1 被破 ——
// 每次采集前重新生成令牌会**立刻作废正在使用的那个**，把自己踢下线。
//
// 写成双向是因为两个方向都出过事的类型：前者是漏改，后者是"顺手实现了个
// Refresh 觉得没坏处"。
func TestRefreshLeadMatchesRefresherImplementation(t *testing.T) {
	for _, r := range All() {
		ad := r.New(nil)
		_, canRefresh := ad.(Refresher)
		switch {
		case r.RefreshLead > 0 && !canRefresh:
			t.Errorf("%s: RefreshLead=%v 但适配器未实现 Refresher —— "+
				"NeedsRefresh 会判定该续期，而 Syncer 没有刷新器可用，到期即 401",
				r.Family, r.RefreshLead)
		case r.RefreshLead == 0 && canRefresh:
			t.Errorf("%s: RefreshLead=0（永不主动续期）但适配器实现了 Refresher —— "+
				"若这是 NewAPI，那是不变式 N-1：重新生成令牌会作废正在使用的那个",
				r.Family)
		}
	}
}

// lower 是不引 strings 的小写化（本文件只需要 ASCII）。
func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
