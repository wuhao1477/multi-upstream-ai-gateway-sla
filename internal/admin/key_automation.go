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

type keyAutomationRequest struct {
	ChannelID      int64  `json:"channel_id"`
	AccountID      int64  `json:"account_id"`
	All            bool   `json:"all"`
	Model          string `json:"model"`
	OnlyWithoutKey bool   `json:"only_without_keys"`
}

const maxKeyAutomationAccounts = 20

// Key 自动化与渠道 sync 共用同一进程内 guard，但两个操作不能互相消耗
// 采集间隔：补齐预览是只读上游查询，不应被导入窗口挡住，反之亦然。
const (
	keyImportGuardSlot    int64 = -1
	keyProvisionGuardSlot int64 = -2
)

func decodeKeyAutomationRequest(r *http.Request) (keyAutomationRequest, error) {
	var request keyAutomationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return request, err
	}
	request.Model = strings.TrimSpace(request.Model)
	return request, nil
}

func validateKeyAutomationRequest(request keyAutomationRequest) error {
	if request.ChannelID < 0 || request.AccountID < 0 {
		return fmt.Errorf("channel_id 与 account_id 不可为负")
	}
	if request.ChannelID == 0 && request.AccountID == 0 && !request.All {
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

func keyAutomationTargets(
	r *http.Request, conn *pgx.Conn, request keyAutomationRequest,
) ([]store.Account, error) {
	accounts, err := store.ListAccounts(r.Context(), conn, request.ChannelID)
	if err != nil {
		return nil, err
	}
	if request.AccountID == 0 {
		return accounts, nil
	}
	for _, account := range accounts {
		if account.ID == request.AccountID {
			return []store.Account{account}, nil
		}
	}
	return nil, fmt.Errorf("account_id %d 不属于指定 channel_id", request.AccountID)
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
	if request.All {
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
