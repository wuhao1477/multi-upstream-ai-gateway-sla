package main

import (
	"context"
	"fmt"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// adapterFor 按站型选适配器（04 总原则：**必须按站型分流**）。
//
// unknown 家族显式拒绝而不是随便挑一个：猜错会让全部字段映射都错，
// 而错误的余额/额度数据比没有数据更危险（04 §7 要求走未知家族接入流程）。
func adapterFor(fam collector.Family, hc *collector.Client) (collector.Adapter, collector.Refresher, error) {
	switch fam {
	case collector.FamilyNewAPI:
		// NewAPI 不提供 Refresher —— 不变式 N-1：运行时重新生成令牌会作废
		// 正在使用的那个（04 §5.1）。故此处**故意传 nil**。
		return collector.NewNewAPIAdapter(hc), nil, nil
	case collector.FamilySub2API:
		ad := collector.NewSub2APIAdapter(hc)
		return ad, ad, nil
	case collector.FamilyASXS:
		ad := collector.NewASXSAdapter(hc)
		return ad, ad, nil
	default:
		return nil, nil, fmt.Errorf(
			"渠道站型为 %q，无对应适配器：请先探测站型，"+
				"未知家族需按 04 §7 新建专属适配器（不猜，猜错会让字段映射全错）", fam)
	}
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
	ad, refresher, err := adapterFor(collector.Family(ch.SiteFamily), hc)
	if err != nil {
		return nil, err
	}

	conn, release, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	credStore := store.NewCredentialStore(pool)
	cred, err := credStore.Load(ctx, conn, ch)
	if err != nil {
		release()
		return nil, err
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
