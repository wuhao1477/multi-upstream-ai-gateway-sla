package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla-public/internal/store"
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

	// 探测结果按下标存进一个预分配切片，与下面 `res.Items[idx] = item` 同一个道理：
	// 每个 goroutine 只写自己那一格，各格是各自的内存，不需要锁（`-race` 下全绿）。
	// ⚠️ 这里原先是一个带 mutex 的 map（`detectStash`）—— 它与紧邻的 `res.Items[idx]`
	// 做的是同一件事，而后者从来没上锁。多出来的那把锁不保护任何东西。
	detects := make([]collector.DetectResult, len(accounts))
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
			// ⚠️ 校验必须在**发出请求之前**（2026-09-01 修）。
			//
			// 原先这里直接把导出文件里的 SiteURL 交给 Detect，而 validateBaseURL
			// 只在后面的落库阶段（importOne）才跑 —— 于是"拒绝落库"拦不住**已经
			// 发出的探测请求**，而 SSRF 关心的正是那个出站请求。`dry_run=true`
			// 根本走不到落库阶段，等于完全不校验。实测：喂一个
			// `http://127.0.0.1:9/probe`，报告里回来的是 "connection refused" ——
			// 请求真的发出去了。
			//
			// 这一条同时证伪了 §3.7 与那次提交说明里的一句话：「校验收成一处、
			// 两个入口共用」—— 共用是真的，但导入侧共用得太晚。本轮自审与
			// Codex 二次评审各自独立查到（后者判 [high]）。
			probeURL, err := validateBaseURL(a.SiteURL)
			if err != nil {
				item.Status = "skipped"
				item.Reason = "site_url 不能用于采集：" + err.Error()
				res.Items[idx] = item
				return
			}

			// 每个站点单独给短超时：一个慢站不该拖累整批
			pctx, pcancel := context.WithTimeout(ctx, 12*time.Second)
			d, derr := s.Detect(pctx, probeURL)
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
			// 探测结果按下标暂存，落库在下面串行做（避免并发写库争用）
			res.Items[idx] = item
			detects[idx] = d
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
				if err := s.importOne(r.Context(), conn, accounts[i], detects[i], it); err != nil {
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

// importOne 建渠道 + 探测快照 + 账号 + 凭证，**四处同生共死**。
//
// ⚠️ 全程在一个事务里（2026-08-29 二次评审后改）。原先是四次独立写入，
// 任一步失败只把条目标成 failed，已建的渠道/快照/账号**留在库里**；
// 而下面那个"同 base_url 就跳过"的判断会让重导认定它已存在 ——
// 于是半成品永不自愈，且台账里多一个采不到数据的渠道（每轮 sync 都报
// ErrPrecondition：凭证读不到）。库里没有 DELETE 渠道的入口，只能手工补。
//
// 事务不是为了并发（导入是低频串行操作），是为了**失败原子性**。
func (s *Server) importOne(
	ctx context.Context, conn *pgx.Conn,
	a collector.HubAccount, d collector.DetectResult, it *collector.HubImportItem,
) error {
	// 校验 + 规范化同一处（与 createChannel / patchChannel 共用）。
	// 这里再跑一次不是重复：importOne 也被测试直接调用，且它收的是**原始**
	// HubAccount —— 函数边界上的输入校验不该依赖"调用方已经查过了"。
	// ⚠️ 但两处必须得出**同一个字符串**：原先这里是 `TrimRight(a.SiteURL,"/")`
	// 而探测那头多了个 `TrimSpace`，于是首尾带空白的 site_url 在探测那头放行
	// （请求真的发出去了），到这里判 400 —— 同一个输入两种判定，条目变成 failed
	// 而理由是"须以 http:// 开头"。
	want, err := validateBaseURL(a.SiteURL)
	if err != nil {
		return err
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开事务: %w", err)
	}
	// Rollback 在已 Commit 后返回 ErrTxClosed，忽略即可；未提交时它才是真正的回滚。
	defer func() { _ = tx.Rollback(ctx) }()

	// 同 base_url 已存在则跳过 —— 重复导入是常态（导出文件会更新后再导一次），
	// 每次都新建会让台账里出现几十个重复渠道。
	//
	// ⚠️ 只跳过**完整**的渠道。不完整的（缺凭证/缺账号）要能补齐，否则一次失败
	// 就把那个站永久钉死在半成品状态 —— 这正是上面那条事务要防的另一半。
	existing, err := store.ListChannels(ctx, tx)
	if err != nil {
		return err
	}
	for _, c := range existing {
		if strings.TrimRight(c.BaseURL, "/") != want {
			continue
		}
		missing, err := s.incompleteParts(ctx, tx, c.ID, c.SiteFamily, d.Family, a)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			it.Status = "skipped"
			it.Reason = fmt.Sprintf("已存在同地址的渠道 #%d", c.ID)
			it.ChannelID = c.ID
			return nil
		}
		// 补齐后提交 —— 同一个事务，补不全就整体回滚。
		if err := s.repairChannel(ctx, tx, c.ID, a, want, d, missing); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("提交补齐: %w", err)
		}
		it.Status = "imported"
		it.ChannelID = c.ID
		// ⚠️ **追加**而不是覆盖（2026-09-01 修）。原先这里是 `it.Warning = ...`，
		// 于是探测阶段写下的「该站开启 turnstile…需转人工录入」或「导出里没有凭证」
		// 被整句抹掉 —— 而 finishImport 靠 strings.Contains(it.Warning, …) 统计
		// Shielded / NoCredential，覆盖之后这两个计数直接少计，界面上那行提示也没了。
		// 实测：一个不带凭证的条目走补齐分支，warning 变成"补齐了…"，
		// no_credential 从 1 掉成 0。
		repaired := fmt.Sprintf("补齐了已有渠道 #%d 的%s", c.ID, strings.Join(missing, "与"))
		if it.Warning == "" {
			it.Warning = repaired
		} else {
			it.Warning += "；" + repaired
		}
		return nil
	}

	name := strings.TrimSpace(a.SiteName)
	if name == "" {
		name = want
	}
	chID, err := store.CreateChannel(ctx, tx, store.Channel{
		Name: name, BaseURL: want, SiteFamily: string(d.Family),
	})
	if err != nil {
		return fmt.Errorf("建渠道: %w", err)
	}
	it.ChannelID = chID

	// 探测结果落库：quota_per_unit 是额度归一的必需输入，
	// 而它**逐站不同**（实测 500000 与 1000000 两种）
	if s.SaveDetected != nil {
		if err := s.SaveDetected(ctx, tx, chID, d); err != nil {
			return fmt.Errorf("存探测结果: %w", err)
		}
	}

	accountID, err := store.CreateAccount(ctx, tx, store.Account{
		ChannelID: chID, ExternalUserID: a.UserID(),
	})
	if err != nil {
		return fmt.Errorf("建账号: %w", err)
	}

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
		credType, err := reg.CredTypeFor(true, a.UserID() != "")
		if err != nil {
			// 报出来而不是存一份必然 401 的凭证：那种凭证要等到某次采集
			// 才暴露，而那时已经分不清是站点挂了还是导入时就缺字段。
			return fmt.Errorf("导出里的凭证字段不足: %w", err)
		}
		if err := s.SaveCredential(ctx, tx, collector.Credential{
			AccountID: accountID, ChannelID: chID, Family: d.Family, CredType: credType,
			BaseURL:        want,
			AccessToken:    a.AccountInfo.AccessToken,
			ExternalUserID: a.UserID(),
		}); err != nil {
			return fmt.Errorf("存凭证: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交导入: %w", err)
	}
	it.Status = "imported"
	return nil
}

// incompleteParts 返回该渠道缺哪几件东西（空 = 完整）。
//
// 判据是"采集需要什么"：缺其中任一件，每轮 sync 都会在装配阶段失败，
// 是一行采不到数据的台账。导出里没带 token 的站不算缺凭证 —— 那是数据源本来
// 就没有，补不出来，硬算成缺会让它每次导入都报一次"补齐失败"。
//
// ⚠️ **探测快照也是采集前置**（2026-09-01 补）。原先只查账号与凭证两张表，
// 于是"有账号有凭证但没有 __detect__ 快照"的渠道被判为完整 → skipped，
// 而 QuotaPerUnit() 取不到会返 0、FetchAccount 直接报错（04 §2：不猜，
// 猜错差 50 万倍）。实测：手工建一个只有渠道行的半成品再导入，报 imported
// 而快照仍是 0 行 —— 报告说成功，那个渠道其实一次都采不了。
// 可达路径是 createChannel 的 auto_detect=false，以及本轮之前 SaveDetected
// 落库失败只 Warn 不回滚的那一支（同轮已改成事务）。
func (s *Server) incompleteParts(
	ctx context.Context, db store.DBTX, chID int64, existingFamily string,
	detectedFamily collector.Family, a collector.HubAccount,
) ([]string, error) {
	var accounts, creds, snaps int
	if err := db.QueryRow(ctx, `
SELECT (SELECT count(*) FROM upstream_accounts
         WHERE channel_id=$1 AND ($2='' OR external_user_id=$2)),
       (SELECT count(*) FROM collector_credentials AS c
          JOIN upstream_accounts AS a ON a.id=c.account_id
         WHERE a.channel_id=$1 AND ($2='' OR a.external_user_id=$2)),
       (SELECT count(*) FROM collector_snapshots
         WHERE channel_id=$1 AND scope_type='pricing' AND scope_id='__detect__')`,
		chID, a.UserID()).Scan(&accounts, &creds, &snaps); err != nil {
		return nil, fmt.Errorf("查渠道 %d 完整性: %w", chID, err)
	}
	var missing []string
	if accounts == 0 {
		missing = append(missing, "账号")
	}
	if creds == 0 && a.HasCredential() {
		missing = append(missing, "凭证")
	}
	if snaps == 0 {
		missing = append(missing, "探测快照")
	}
	if needsFamilyRepair(existingFamily, detectedFamily) {
		missing = append(missing, "站型")
	}
	return missing, nil
}

func needsFamilyRepair(existing string, detected collector.Family) bool {
	unknown := existing == "" || existing == string(collector.FamilyUnknown)
	known := detected != "" && detected != collector.FamilyUnknown
	return unknown && known
}

// repairChannel 给已存在但不完整的渠道补上缺的那几件。
//
// 只补 incompleteParts 报缺的，不动已有行 —— 补齐不该覆盖运维手工改过的凭证。
//
// `base` 由调用方传入而不在这里再算一遍：那是 `a.SiteURL` 规范化的**第三份**
// 拷贝，而它原先漏了 `TrimSpace`（另两份的规范化各不相同，见 validateBaseURL 头部）。
func (s *Server) repairChannel(
	ctx context.Context, db store.DBTX, chID int64,
	a collector.HubAccount, base string, d collector.DetectResult, missing []string,
) error {
	var accountID int64
	for _, m := range missing {
		switch m {
		case "账号":
			var err error
			accountID, err = store.CreateAccount(ctx, db, store.Account{
				ChannelID: chID, ExternalUserID: a.UserID(),
			})
			if err != nil {
				return fmt.Errorf("补账号: %w", err)
			}
		case "探测快照":
			if s.SaveDetected == nil {
				return fmt.Errorf("补探测快照：未注入 SaveDetected")
			}
			if err := s.SaveDetected(ctx, db, chID, d); err != nil {
				return fmt.Errorf("补探测快照: %w", err)
			}
		case "凭证":
			if s.SaveCredential == nil {
				return fmt.Errorf("补凭证：未注入 SaveCredential")
			}
			reg, ok := collector.Lookup(d.Family)
			if !ok {
				return fmt.Errorf("站型 %q 无注册信息，无法确定凭证形态", d.Family)
			}
			credType, err := reg.CredTypeFor(true, a.UserID() != "")
			if err != nil {
				return fmt.Errorf("导出里的凭证字段不足: %w", err)
			}
			if accountID == 0 {
				accountID, err = importAccountID(ctx, db, chID, a.UserID())
				if err != nil {
					return fmt.Errorf("补凭证: %w", err)
				}
			}
			if err := s.SaveCredential(ctx, db, collector.Credential{
				AccountID: accountID, ChannelID: chID, Family: d.Family, CredType: credType,
				BaseURL:        base,
				AccessToken:    a.AccountInfo.AccessToken,
				ExternalUserID: a.UserID(),
			}); err != nil {
				return fmt.Errorf("补凭证: %w", err)
			}
		case "站型":
			if err := store.UpdateChannel(ctx, db, store.Channel{
				ID: chID, SiteFamily: string(d.Family),
			}); err != nil {
				return fmt.Errorf("补站型: %w", err)
			}
			if _, err := db.Exec(ctx, `
UPDATE collector_credentials SET site_family=$2, updated_at=now()
 WHERE channel_id=$1`, chID, string(d.Family)); err != nil {
				return fmt.Errorf("补凭证站型: %w", err)
			}
		}
	}
	return nil
}

func importAccountID(
	ctx context.Context, db store.DBTX, channelID int64, externalUserID string,
) (int64, error) {
	var accountID int64
	err := db.QueryRow(ctx, `
SELECT id FROM upstream_accounts
 WHERE channel_id=$1 AND ($2='' OR external_user_id=$2)
 ORDER BY id LIMIT 1`, channelID, externalUserID).Scan(&accountID)
	if err != nil {
		return 0, fmt.Errorf("找不到渠道 %d 的账号 %q: %w", channelID, externalUserID, err)
	}
	return accountID, nil
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
