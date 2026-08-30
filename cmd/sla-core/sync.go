package main

import (
	"context"
	"fmt"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// adapterFor 按站型选适配器（04 总原则：**必须按站型分流**）。
//
// 分流表在 collector 的注册表里，这里只查表 —— 原先这是一个 switch，
// 是"加站型要改九处"里的第 3 处。
//
// Refresher 由**类型断言**推导而非逐家族声明：NewAPI 的适配器根本没有
// Refresh 方法（不变式 N-1：运行时重新生成令牌会作废正在使用的那个，
// 04 §5.1），那个"没有"本身就是声明，断言不成立时 r 为 nil 接口 ——
// 正是 Syncer 期望的"该站型不续期"。写成 switch 就得手工保证两者一致，
// 而写反了不报错：给 NewAPI 传个非 nil Refresher 会让它每次采集前先把
// 自己的令牌作废掉。
func adapterFor(fam collector.Family, hc *collector.Client) (collector.Adapter, collector.Refresher, error) {
	reg, ok := collector.Lookup(fam)
	if !ok {
		return nil, nil, fmt.Errorf(
			"渠道站型为 %q，无对应适配器：请先探测站型，"+
				"未知家族需按 04 §7 新建专属适配器（不猜，猜错会让字段映射全错）", fam)
	}
	ad := reg.New(hc)
	r, _ := ad.(collector.Refresher)
	return ad, r, nil
}

// runChannelSync 执行一次渠道采集。
//
// 编排本身在 collector.Syncer（09 §5.0bis 的顺序与事务边界）；
// 这里只负责装配：选适配器、读凭证、补齐额度换算基数。
func runChannelSync(
	ctx context.Context,
	pool *store.Pool,
	hc *collector.Client,
	sink collector.Sink,
	auth *collector.Authenticator,
	ch store.Channel,
) (*collector.SyncResult, error) {
	// 装配阶段的失败一律裹 ErrPrecondition：到这里为止一个上游字节都没发出，
	// 调用方据此决定不消耗最小间隔窗口（见 collector.ErrPrecondition）。
	ad, refresher, err := adapterFor(collector.Family(ch.SiteFamily), hc)
	if err != nil {
		return nil, fmt.Errorf("%w：%w", collector.ErrPrecondition, err)
	}

	conn, release, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w：%w", collector.ErrPrecondition, err)
	}
	credStore := store.NewCredentialStore(pool)
	cred, err := credStore.Load(ctx, conn, ch)
	if err != nil {
		release()
		return nil, fmt.Errorf("%w：%w", collector.ErrPrecondition, err)
	}
	// 额度换算基数逐站读取（04 §2：不写死）。
	// 取不到时留 0 —— FetchAccount 会因此报错而不是猜（差 50 万倍）。
	cred.QuotaPerUnit = store.QuotaPerUnit(ctx, conn, ch.ID)
	release()

	s := &collector.Syncer{
		Adapter: ad, Sink: sink, Auth: auth, Refresher: refresher,
	}
	return s.Sync(ctx, cred)
}
