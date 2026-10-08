package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// Key 自动化的范围请求。
//
// 复数字段（channel_ids / account_ids）是界面现在发的形状 —— 渠道与账号在
// 界面上都是多选。单数字段保留：它们是已发布的契约（docs/dev/09 §5bis），
// 而且"选一个渠道"是这两个端点最常见的调用方式。decode 时把单数**归一进**
// 复数切片，下游只看复数 —— 两条路径并存才会出"到底哪个说了算"的分叉。
type keyAutomationRequest struct {
	ChannelID      int64   `json:"channel_id"`
	AccountID      int64   `json:"account_id"`
	ChannelIDs     []int64 `json:"channel_ids"`
	AccountIDs     []int64 `json:"account_ids"`
	All            bool    `json:"all"`
	Model          string  `json:"model"`
	OnlyWithoutKey bool    `json:"only_without_keys"`
}

// maxKeyAutomationAccounts 是**批量补齐**的单次上限。
//
// 保持 20 不变：补齐会在别人家站点上**真的创建 Key**，这是不可逆的写操作，
// 一次点错波及 20 个账号已经够疼了。它与下面那个上限不是一个数，也不该是 ——
// 两个操作的后果差着一整个数量级。
const maxKeyAutomationAccounts = 20

// maxKeyImportAccounts 是**同步已有 Key**的单次上限。
//
// 从 20 抬到 200（2026-09-16）。原来那个 20 不是上游的要求，是被同步响应的
// 时长逼出来的：一个 HTTP 请求里跑不完更多。改成后台批次之后这个理由没了，
// 而 20 反过来成了这个功能最大的痛点 —— 真库 65 个渠道，要分四次点。
//
// 仍然保留一个上限：范围选错（比如把 all 当成"全选"）时，一个手滑不该变成
// 几百个站点的后台扫描。200 覆盖得住"把整份台账同步一遍"，又拦得住数量级错误。
const maxKeyImportAccounts = 200

// Key 自动化与渠道 sync 共用同一进程内 guard，但两个操作不能互相消耗
// 采集间隔：补齐预览是只读上游查询，不应被导入窗口挡住，反之亦然。
//
// 同步已有 Key 不再走这个 guard：它现在是后台批次，"同时只跑一个"由队列自己
// 保证（keyImportQueue.begin），而那比"两次点击之间隔 60 秒"更贴切 —— 真正
// 要防的是两批并发去花同一份限流预算，不是点得太快。
const (
	keyProvisionGuardSlot int64 = -2
)

// mergeScopeIDs 把单数 id 归一进复数切片并去重，顺带保持稳定顺序。
// 0 视为"未指定"（旧契约就是这么用的），负数留给校验去拒绝。
func mergeScopeIDs(single int64, many []int64) []int64 {
	out := make([]int64, 0, len(many)+1)
	seen := make(map[int64]bool, len(many)+1)
	for _, id := range append([]int64{single}, many...) {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func decodeKeyAutomationRequest(r *http.Request) (keyAutomationRequest, error) {
	var request keyAutomationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return request, err
	}
	request.Model = strings.TrimSpace(request.Model)
	request.ChannelIDs = mergeScopeIDs(request.ChannelID, request.ChannelIDs)
	request.AccountIDs = mergeScopeIDs(request.AccountID, request.AccountIDs)
	return request, nil
}

func validateKeyAutomationRequest(request keyAutomationRequest) error {
	for _, id := range append(append([]int64{}, request.ChannelIDs...), request.AccountIDs...) {
		if id < 0 {
			return fmt.Errorf("channel_id 与 account_id 不可为负")
		}
	}
	if len(request.ChannelIDs) == 0 && len(request.AccountIDs) == 0 && !request.All {
		return fmt.Errorf("必须指定 channel_id、account_id，或明确传 all=true")
	}
	return nil
}

func (s *Server) keyAutomationRequest(w http.ResponseWriter, r *http.Request) (keyAutomationRequest, bool) {
	request, err := decodeKeyAutomationRequest(r)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return request, false
	}
	if err := validateKeyAutomationRequest(request); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return request, false
	}
	return request, true
}

// keyAutomationTargets 把范围请求展开成具体账号。
//
// 一次查全量再在内存里过滤，而不是按渠道逐个查库：多选渠道时后者是 N 次
// 往返，而账号表是"数百行"这个量级（store/channels.go 的 ListAccounts
// 本身就支持 channel_id <= 0 取全部）。
func keyAutomationTargets(
	r *http.Request, conn *pgx.Conn, request keyAutomationRequest,
) ([]store.Account, error) {
	channelScope := int64(0)
	if len(request.ChannelIDs) == 1 {
		channelScope = request.ChannelIDs[0]
	}
	accounts, err := store.ListAccounts(r.Context(), conn, channelScope)
	if err != nil {
		return nil, err
	}
	if channelScope == 0 && len(request.ChannelIDs) > 0 {
		accounts = filterAccountsByChannels(accounts, request.ChannelIDs)
	}
	if len(request.AccountIDs) == 0 {
		return accounts, nil
	}
	// 指名了账号：逐个核对它确实落在渠道范围内。跨渠道的账号不是"筛掉就好"，
	// 它意味着调用方对归属的理解是错的 —— 静默忽略会让人以为已经处理过了。
	byID := make(map[int64]store.Account, len(accounts))
	for _, account := range accounts {
		byID[account.ID] = account
	}
	targets := make([]store.Account, 0, len(request.AccountIDs))
	for _, id := range request.AccountIDs {
		account, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("account_id %d 不属于指定 channel_id", id)
		}
		targets = append(targets, account)
	}
	return targets, nil
}

func filterAccountsByChannels(accounts []store.Account, channelIDs []int64) []store.Account {
	wanted := make(map[int64]bool, len(channelIDs))
	for _, id := range channelIDs {
		wanted[id] = true
	}
	out := make([]store.Account, 0, len(accounts))
	for _, account := range accounts {
		if wanted[account.ChannelID] {
			out = append(out, account)
		}
	}
	return out
}

func validateKeyAutomationTargetLimit(targets []store.Account) error {
	if len(targets) > maxKeyAutomationAccounts {
		return fmt.Errorf("一次最多处理 %d 个账号，请缩小到渠道或账号范围", maxKeyAutomationAccounts)
	}
	return nil
}

type keyImportItem struct {
	ChannelID int64 `json:"channel_id"`
	AccountID int64 `json:"account_id"`
	// Status：ok / partial / deferred / skipped / failed。
	// `deferred` 是新的一档：这个账号没出错，只是本渠道的明文读取预算用完了 ——
	// 它要人做的事（过一会儿再来）与 failed（去修凭证）完全不同，
	// 合成一档会让"等一会儿就好"和"这个站废了"在界面上长得一样。
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// Attempts 是这条结果是第几次尝试得出的。退避重试之后，
	// 不写出来的话界面上只看得到最后那次的结果，看不出它重来过。
	Attempts int `json:"attempts,omitempty"`
	collector.KeyImportResult
}

type keyImportBatchResult struct {
	Count int `json:"count"`
	Found int `json:"found"`
	// Imported 是**新登记**的 Key 数，Skipped 是已经有了、这次只刷新了额度的。
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
	Deferred int `json:"deferred"`

	// ── 账号级计数 ──
	//
	// ⚠️ 上面那四个数的单位是**把**（Key），下面三个的单位是**个**（账号），
	// 两组不可混着读。2026-09-16 实测撞到过：10 个渠道里 5 个站整个连不上，
	// 而汇总里 `failed` 是 2 —— 那个 2 来自唯一一个连得上的站里失败的两把 Key，
	// 5 个连不上的站一把 Key 都没轮到，对 `failed` 的贡献是 0。于是界面上
	// "失败 2" 读起来像"只有两处小问题"，而事实是半数站点根本没采到。
	// 账号级计数必须单独给，不能让人从 items 里自己数。

	// FailedAccounts 是整个账号都没成的个数（凭证失效、站型不认……要人去修）。
	FailedAccounts int `json:"failed_accounts"`
	// DeferredAccounts 是"稍后再来"的个数：本渠道明文读取预算用尽，
	// 或批次被进程退出打断时还没轮到的那些。它与 FailedAccounts 要人做的事不同。
	DeferredAccounts int `json:"deferred_accounts"`
	// SkippedAccounts 是被跳过的个数（账号已停用）。
	SkippedAccounts int             `json:"skipped_accounts"`
	Items           []keyImportItem `json:"items"`
}

func mergeKeyProvisionBatchResult(dst *keyProvisionBatchResult, src collector.KeyProvisionResult) {
	dst.Found += src.Found
	dst.Imported += src.Imported
	dst.MatchedGroups += src.MatchedGroups
	dst.ExistingGroups += src.ExistingGroups
	dst.WouldCreate += src.WouldCreate
	dst.Created += src.Created
	dst.Failed += src.Failed
	dst.Deferred += src.Deferred
}

// importKeys 排一个后台批次，**立刻返回**。
//
// 从「同步等结果」改成「排队 + 轮询」（2026-09-16）。不是为了让接口好看：
// 这个操作的时长由上游的限流窗口决定（实测每站 20 次 / 20 分钟），跨几十个
// 站点的一批天然是小时级的，塞不进一个 HTTP 请求。旧实现只好反过来迁就请求
// 时长 —— 全批共用 20 次明文预算、一个账号出错就 break 整批、单次最多 20 个
// 账号。那三条限制都不是上游要求的，是被同步响应逼出来的。
//
// 进度与结果走 GET /admin/keys/import/jobs[/{id}]（同一份 keyImportBatchResult，
// 只是晚一点到）。
func (s *Server) importKeys(w http.ResponseWriter, r *http.Request) {
	request, ok := s.keyAutomationRequest(w, r)
	if !ok {
		return
	}
	if s.ReadOnly {
		s.fail(w, http.StatusServiceUnavailable, "只读实例不支持 Key 自动同步")
		return
	}
	// 看范围而不是看 all 这一位：多选之后"有没有明确范围"才是真正的判据，
	// 而 all=true 只是"调用方承认自己没给范围"的一种写法。
	if len(request.ChannelIDs) == 0 && len(request.AccountIDs) == 0 {
		s.fail(w, http.StatusBadRequest, "同步已有 Key 必须选择具体渠道")
		return
	}
	if s.ImportKeys == nil {
		s.fail(w, http.StatusNotImplemented, "Key 自动导入未配置")
		return
	}
	// 展开范围要查库，而这一步必须在**请求的** ctx 里做：它是同步的，
	// 拿不到目标就没有批次可排，该让调用方当场看到 400。
	var targets []store.Account
	failed := false
	s.withConn(w, r, func(conn *pgx.Conn) {
		got, err := keyAutomationTargets(r, conn, request)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			failed = true
			return
		}
		targets = got
	})
	if failed {
		return
	}
	if len(targets) == 0 {
		s.fail(w, http.StatusBadRequest, "所选范围内没有账号，先在这些渠道下登记账号")
		return
	}
	if len(targets) > maxKeyImportAccounts {
		s.fail(w, http.StatusBadRequest,
			fmt.Sprintf("一次最多排 %d 个账号，请缩小到渠道或账号范围", maxKeyImportAccounts))
		return
	}

	job, started := s.keyQueue().begin(len(targets))
	if !started {
		// 409 带上正在跑的那个批次 id：界面据此直接切到它的进度，
		// 而不是丢一句"正忙"让人自己去找是哪一批。
		s.failWith(w, http.StatusConflict, "已有一批 Key 同步在后台执行",
			map[string]any{"running_job_id": job.ID})
		return
	}
	go func() {
		ctx := s.backgroundCtx()
		unlock, locked, err := s.keyImportAcquireLock(ctx)
		if err != nil {
			s.queue.finish(job, "done", "取得 Key 自动化锁失败: "+shortErr(err))
			return
		}
		if !locked {
			s.queue.finish(job, "done", "另一实例正在执行 Key 自动化")
			return
		}
		defer unlock()
		s.Logger.Info("后台 Key 同步开始", "job_id", job.ID, "accounts", len(targets))
		s.runKeyImportJob(ctx, job, targets)
		snapshot, _ := s.queue.snapshot(job.ID)
		s.Logger.Info("后台 Key 同步结束", "job_id", job.ID, "status", snapshot.Status,
			"found", snapshot.Found, "imported", snapshot.Imported,
			"skipped", snapshot.Skipped, "failed", snapshot.Failed,
			"deferred", snapshot.Deferred)
	}()
	snapshot, _ := s.keyQueue().snapshot(job.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(snapshot)
}

// keyQueue 懒建队列。NewServer 之外还有别的构造路径（单测直接取结构体字面量），
// 在那里漏掉队列的表现是一个 nil map panic，而不是"这个功能没配"。
func (s *Server) keyQueue() *keyImportQueue {
	s.queueOnce.Do(func() {
		if s.queue == nil {
			s.queue = newKeyImportQueue()
		}
	})
	return s.queue
}

// listKeyImportJobs 回最近的批次（新的在前）。
//
// 只给汇总不给逐账号明细：列表要回答的是"有没有在跑、上一批什么结果"，
// 而一批可能有两百条 Items —— 全塞进列表会让这个每两秒轮询一次的接口
// 每次回几百 KB。明细在 /{id} 那条。
func (s *Server) listKeyImportJobs(w http.ResponseWriter, r *http.Request) {
	jobs := s.keyQueue().list()
	out := make([]keyImportJob, 0, len(jobs))
	for _, job := range jobs {
		job.Items = nil
		out = append(out, job)
	}
	s.ok(w, map[string]any{"count": len(out), "items": out})
}

func (s *Server) getKeyImportJob(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	job, found := s.keyQueue().snapshot(id)
	if !found {
		// 404 的原因**必须说出来**：队列在内存里，重启与超出保留条数都会让
		// 一个真实存在过的批次消失。只回"不存在"会被读成"这个 id 是假的"。
		s.fail(w, http.StatusNotFound,
			fmt.Sprintf("批次 %d 不在内存里：进程重启过，或它已被更近的 %d 批挤出历史",
				id, keyImportJobHistory))
		return
	}
	s.ok(w, job)
}

type keyProvisionItem struct {
	ChannelID int64  `json:"channel_id"`
	AccountID int64  `json:"account_id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	collector.KeyProvisionResult
}

type keyProvisionBatchResult struct {
	Count           int                `json:"count"`
	SkippedAccounts int                `json:"skipped_accounts"`
	Found           int                `json:"found"`
	Imported        int                `json:"imported"`
	MatchedGroups   int                `json:"matched_groups"`
	ExistingGroups  int                `json:"existing_groups"`
	WouldCreate     int                `json:"would_create"`
	Created         int                `json:"created"`
	Failed          int                `json:"failed"`
	Deferred        int                `json:"deferred"`
	Items           []keyProvisionItem `json:"items"`
}

func (s *Server) provisionKeys(w http.ResponseWriter, r *http.Request) {
	request, ok := s.keyAutomationRequest(w, r)
	if !ok {
		return
	}
	if s.ReadOnly {
		s.fail(w, http.StatusServiceUnavailable, "只读实例不支持批量创建 Key")
		return
	}
	if request.All && !request.OnlyWithoutKey {
		s.fail(w, http.StatusBadRequest, "全局补齐只能处理仅无 Key 的账号")
		return
	}
	dryRunValue := r.URL.Query().Get("dry_run")
	if dryRunValue != "true" && dryRunValue != "false" {
		s.fail(w, http.StatusBadRequest, "dry_run 必须明确为 true 或 false")
		return
	}
	if s.ProvisionKeys == nil {
		s.fail(w, http.StatusNotImplemented, "Key 批量创建未配置")
		return
	}
	dryRun := dryRunValue == "true"
	if err := s.guard.acquire(keyProvisionGuardSlot, s.syncMinInterval()); err != nil {
		code := http.StatusTooManyRequests
		if errors.Is(err, errSyncRunning) {
			code = http.StatusConflict
		}
		s.fail(w, code, "批量创建 Key 正在执行或间隔未到: "+err.Error())
		return
	}
	reachedUpstream := false
	defer func() { s.guard.release(keyProvisionGuardSlot, reachedUpstream && !dryRun) }()
	s.withConn(w, r, func(conn *pgx.Conn) {
		locked, err := store.TryKeyAutomationLock(r.Context(), conn)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "取得 Key 补齐锁失败: "+err.Error())
			return
		}
		if !locked {
			s.fail(w, http.StatusConflict, "另一实例正在批量创建 Key")
			return
		}
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := store.UnlockKeyAutomation(unlockCtx, conn); err != nil {
				s.Logger.Error("释放 Key 补齐锁失败", "err", err)
			}
		}()
		targets, err := keyAutomationTargets(r, conn, request)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateKeyAutomationTargetLimit(targets); err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		result := keyProvisionBatchResult{Items: []keyProvisionItem{}}
		remainingCreates := collector.DefaultKeyProvisionCreateLimit
		remainingSecrets := collector.DefaultKeySecretResolveLimit
		for _, account := range targets {
			if !dryRun && (remainingCreates == 0 || remainingSecrets == 0) {
				break
			}
			item := keyProvisionItem{ChannelID: account.ChannelID, AccountID: account.ID}
			if account.Status != "active" {
				item.Status, item.Error = "skipped", "账号已停用"
				result.SkippedAccounts++
				result.Items = append(result.Items, item)
				continue
			}
			reachedUpstream = true
			got, err := s.ProvisionKeys(r.Context(), conn, account.ChannelID, account.ID,
				collector.KeyProvisionRequest{
					Model: request.Model, OnlyWithoutKeys: request.OnlyWithoutKey, DryRun: dryRun,
					MaxCreates: remainingCreates, MaxSecretResolves: remainingSecrets,
				})
			item.KeyProvisionResult = got
			switch {
			case err != nil:
				item.Status, item.Error = "failed", shortErr(err)
			case got.SkippedReason != "":
				item.Status = "skipped"
				result.SkippedAccounts++
			case got.Failed > 0:
				item.Status = "partial"
			default:
				item.Status = "ok"
			}
			mergeKeyProvisionBatchResult(&result, got)
			result.Items = append(result.Items, item)
			if !dryRun {
				remainingCreates -= got.Created
				remainingSecrets -= got.SecretResolves
				if err != nil || got.Failed > 0 || got.Deferred > 0 {
					break
				}
			}
		}
		result.Count = len(result.Items)
		s.ok(w, result)
	})
}
