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

const maxKeyAutomationAccounts = 20

// Key 自动化与渠道 sync 共用同一进程内 guard，但两个操作不能互相消耗
// 采集间隔：补齐预览是只读上游查询，不应被导入窗口挡住，反之亦然。
const (
	keyImportGuardSlot    int64 = -1
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
	ChannelID int64  `json:"channel_id"`
	AccountID int64  `json:"account_id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	collector.KeyImportResult
}

type keyImportBatchResult struct {
	Count            int             `json:"count"`
	Found            int             `json:"found"`
	Imported         int             `json:"imported"`
	Skipped          int             `json:"skipped"`
	Failed           int             `json:"failed"`
	Deferred         int             `json:"deferred"`
	DeferredAccounts int             `json:"deferred_accounts"`
	Items            []keyImportItem `json:"items"`
}

func deferredImportAccounts(total, processed int) int {
	if processed >= total {
		return 0
	}
	return total - processed
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
	if err := s.guard.acquire(keyImportGuardSlot, s.syncMinInterval()); err != nil {
		code := http.StatusTooManyRequests
		if errors.Is(err, errSyncRunning) {
			code = http.StatusConflict
		}
		s.fail(w, code, "Key 自动化正在执行或间隔未到: "+err.Error())
		return
	}
	reachedUpstream := false
	defer func() { s.guard.release(keyImportGuardSlot, reachedUpstream) }()
	s.withConn(w, r, func(conn *pgx.Conn) {
		locked, err := store.TryKeyAutomationLock(r.Context(), conn)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "取得 Key 自动化锁失败: "+err.Error())
			return
		}
		if !locked {
			s.fail(w, http.StatusConflict, "另一实例正在执行 Key 自动化")
			return
		}
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := store.UnlockKeyAutomation(unlockCtx, conn); err != nil {
				s.Logger.Error("释放 Key 自动化锁失败", "err", err)
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
		result := keyImportBatchResult{Items: []keyImportItem{}}
		remainingSecrets := collector.DefaultKeySecretResolveLimit
		for index, account := range targets {
			if remainingSecrets == 0 {
				result.DeferredAccounts += deferredImportAccounts(len(targets), index)
				break
			}
			item := keyImportItem{ChannelID: account.ChannelID, AccountID: account.ID}
			if account.Status != "active" {
				item.Status, item.Error = "skipped", "账号已停用"
				result.Items = append(result.Items, item)
				continue
			}
			reachedUpstream = true
			got, err := s.ImportKeys(r.Context(), conn, account.ChannelID, account.ID,
				collector.KeyImportRequest{MaxSecretResolves: remainingSecrets})
			item.KeyImportResult = got
			if err != nil {
				item.Status, item.Error = "failed", shortErr(err)
			} else if got.Failed > 0 || got.Deferred > 0 {
				item.Status = "partial"
			} else {
				item.Status = "ok"
			}
			result.Found += got.Found
			result.Imported += got.Imported
			result.Skipped += got.Skipped
			result.Failed += got.Failed
			result.Deferred += got.Deferred
			result.Items = append(result.Items, item)
			remainingSecrets -= got.SecretResolves
			if err != nil || got.Failed > 0 || got.Deferred > 0 {
				break
			}
		}
		result.Count = len(result.Items)
		s.ok(w, result)
	})
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
