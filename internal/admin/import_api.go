package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// ImportRoutes 注册数据导入端点。
func (s *Server) ImportRoutes(mux *http.ServeMux) {
	mux.Handle("POST /admin/import/all-api-hub",
		s.requireToken(http.HandlerFunc(s.importAllAPIHub)))
}

// importConcurrency 限制探测并发。
//
// 不设太高：探测虽是公开端点，但一次导入会打上百个陌生站点，
// 并发过大既容易被当成扫描，也会让本机文件描述符吃紧。
// 实测 16 并发探 106 站约 44 秒，可接受。
const importConcurrency = 12

// importAllAPIHub 导入 all-api-hub 的导出数据。
//
// 关键设计（collector/allapihub.go 的铁律）：**以我方 Detect 为准，
// 不信任导出里的 site_type**。实测 106 站中有 2 站声明错，而站型决定全部
// 字段映射 —— 信错一次就把余额、额度、模型全解析错，且错得静默。
func (s *Server) importAllAPIHub(w http.ResponseWriter, r *http.Request) {
	dryRun := r.URL.Query().Get("dry_run") == "true"

	backup, err := collector.ParseHubBackup(r.Body)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.Detect == nil {
		s.fail(w, http.StatusServiceUnavailable, "站型探测未配置，无法导入")
		return
	}

	accounts := backup.Accounts.Accounts
	res := &collector.HubImportResult{
		Total: len(accounts),
		Items: make([]collector.HubImportItem, len(accounts)),
	}

	// 探测阶段：并发跑，只打公开端点。
	// 整体给一个宽超时 —— 上百个站点里总有慢的，但不能无限等。
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Minute)
	defer cancel()

	stash := &detectStash{m: map[int]collector.DetectResult{}}
	sem := make(chan struct{}, importConcurrency)
	var wg sync.WaitGroup
	for i := range accounts {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			a := accounts[idx]
			item := collector.HubImportItem{
				SiteName:       a.SiteName,
				SiteURL:        a.SiteURL,
				DeclaredFamily: a.SiteType,
			}
			if strings.TrimSpace(a.SiteURL) == "" {
				item.Status, item.Reason = "skipped", "导出条目没有 site_url"
				res.Items[idx] = item
				return
			}

			// 每个站点单独给短超时：一个慢站不该拖累整批
			pctx, pcancel := context.WithTimeout(ctx, 12*time.Second)
			d, derr := s.Detect(pctx, a.SiteURL)
			pcancel()

			if derr != nil || d.Family == collector.FamilyUnknown {
				item.Status = "skipped"
				item.Reason = "站型探测失败或未识别"
				if derr != nil {
					item.Reason += "：" + shortErr(derr)
				}
				// 未识别就不建渠道：没有适配器，建了也采不了，
				// 只会在资产台账里留一堆永远报异常的空壳（04 §7）
				res.Items[idx] = item
				return
			}
			item.DetectedFamily = d.Family
			// 对照导出声明 —— 不一致时以探测为准，但要报出来。
			// 别名表在注册表里（每个站型自己声明认领哪些自称）。
			if declared := collector.FamilyOfAlias(a.SiteType); declared != collector.FamilyUnknown &&
				declared != d.Family {
				item.Mismatch = true
			}
			if !d.NoShield {
				item.Warning = "该站开启 turnstile 人机验证，服务端自动采集不可行，" +
					"需转人工录入（04 §6）"
			}
			if !a.HasCredential() {
				if item.Warning != "" {
					item.Warning += "；"
				}
				item.Warning += "导出里没有凭证，需另行登记后才能采集"
			}
			item.Status = "detected"
			// 探测结果暂存在 item 里，落库在下面串行做（避免并发写库争用）
			res.Items[idx] = item
			stash.store(idx, d)
		}(i)
	}
	wg.Wait()

	// 落库阶段：串行。
	// 不并发是刻意的 —— 建渠道/账号/凭证是三张表的关联写入，
	// 并发只会带来锁争用与更难排查的失败，而导入是低频操作。
	if !dryRun {
		s.withConn(w, r, func(conn *pgx.Conn) {
			for i := range res.Items {
				it := &res.Items[i]
				if it.Status != "detected" {
					continue
				}
				d, ok := stash.load(i)
				if !ok {
					it.Status, it.Reason = "failed", "探测结果丢失（内部错误）"
					continue
				}
				if err := s.importOne(r.Context(), conn, accounts[i], d, it); err != nil {
					it.Status = "failed"
					it.Reason = shortErr(err)
				}
			}
			finishImport(res)
			s.Logger.Info("all-api-hub 导入完成",
				"total", res.Total, "imported", res.Imported,
				"skipped", res.Skipped, "failed", res.Failed,
				"mismatches", res.Mismatches)
			s.ok(w, res)
		})
		return
	}

	// dry_run：只报探测结果，不落库。
	// 上百个站点的导入是不可逆操作（会建出上百个渠道），先看一眼再决定。
	for i := range res.Items {
		if res.Items[i].Status == "detected" {
			res.Items[i].Status = "would_import"
		}
	}
	finishImport(res)
	s.ok(w, res)
}

// importOne 建渠道 + 账号 + 凭证。
func (s *Server) importOne(
	ctx context.Context, conn *pgx.Conn,
	a collector.HubAccount, d collector.DetectResult, it *collector.HubImportItem,
) error {
	// 同 base_url 已存在则跳过 —— 重复导入是常态（导出文件会更新后再导一次），
	// 每次都新建会让台账里出现几十个重复渠道
	existing, err := store.ListChannels(ctx, conn)
	if err != nil {
		return err
	}
	want := strings.TrimRight(a.SiteURL, "/")
	for _, c := range existing {
		if strings.TrimRight(c.BaseURL, "/") == want {
			it.Status = "skipped"
			it.Reason = fmt.Sprintf("已存在同地址的渠道 #%d", c.ID)
			it.ChannelID = c.ID
			return nil
		}
	}

	name := strings.TrimSpace(a.SiteName)
	if name == "" {
		name = want
	}
	chID, err := store.CreateChannel(ctx, conn, store.Channel{
		Name: name, BaseURL: want, SiteFamily: string(d.Family),
	})
	if err != nil {
		return fmt.Errorf("建渠道: %w", err)
	}
	it.ChannelID = chID

	// 探测结果落库：quota_per_unit 是额度归一的必需输入，
	// 而它**逐站不同**（实测 500000 与 1000000 两种）
	if s.SaveDetected != nil {
		if err := s.SaveDetected(ctx, chID, d); err != nil {
			return fmt.Errorf("存探测结果: %w", err)
		}
	}

	accID, err := store.CreateAccount(ctx, conn, store.Account{
		ChannelID: chID, ExternalUserID: a.UserID(),
	})
	if err != nil {
		return fmt.Errorf("建账号: %w", err)
	}
	_ = accID

	// 有凭证就一并登记 —— 否则运维还要逐站手填上百次。
	// cred_type 与必需字段都走注册表的同一处判定（与 saveCredential 同一函数），
	// 原先这里是第二份按家族分流的 credType 映射：它与凭证登记那份各写一遍，
	// 少一个家族就静默写空串。
	if a.HasCredential() && s.SaveCredential != nil {
		reg, ok := collector.Lookup(d.Family)
		if !ok {
			return fmt.Errorf("站型 %q 无注册信息，无法确定凭证形态", d.Family)
		}
		// 导入侧只可能带 token（导出里没有密码），不传账密一路
		credType, err := reg.CredTypeFor(true, a.UserID() != "", false)
		if err != nil {
			// 报出来而不是存一份必然 401 的凭证：那种凭证要等到某次采集
			// 才暴露，而那时已经分不清是站点挂了还是导入时就缺字段。
			return fmt.Errorf("导出里的凭证字段不足: %w", err)
		}
		if err := s.SaveCredential(ctx, collector.Credential{
			ChannelID: chID, Family: d.Family, CredType: credType,
			BaseURL:        want,
			AccessToken:    a.AccountInfo.AccessToken,
			ExternalUserID: a.UserID(),
		}); err != nil {
			return fmt.Errorf("存凭证: %w", err)
		}
	}

	it.Status = "imported"
	return nil
}

func finishImport(res *collector.HubImportResult) {
	for _, it := range res.Items {
		switch it.Status {
		case "imported", "would_import":
			res.Imported++
		case "skipped":
			res.Skipped++
		case "failed":
			res.Failed++
		}
		if it.Mismatch {
			res.Mismatches++
		}
		if strings.Contains(it.Warning, "turnstile") {
			res.Shielded++
		}
		if strings.Contains(it.Warning, "没有凭证") {
			res.NoCredential++
		}
	}
}

// shortErr 截短错误信息。
// 上游可能返回整页 HTML，全塞进报告会让 JSON 膨胀到不可读。
func shortErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// detectStash 在探测（并发）与落库（串行）两阶段之间传递结果。
//
// ⚠️ **每次请求新建一个实例**，不用包级变量：两次并发导入会互相覆盖
// 彼此的探测结果，进而把 A 站的 quota_per_unit 写到 B 站上 —— 那是
// 静默的数据污染，比报错难查得多。
//
// 用独立结构而非塞进 item：DetectResult 含内部字段，
// 不该出现在返回给调用方的报告里（报告是给人看的，不是状态转储）。
type detectStash struct {
	mu sync.Mutex
	m  map[int]collector.DetectResult
}

func (d *detectStash) store(i int, r collector.DetectResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.m[i] = r
}

func (d *detectStash) load(i int) (collector.DetectResult, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.m[i]
	return r, ok
}
