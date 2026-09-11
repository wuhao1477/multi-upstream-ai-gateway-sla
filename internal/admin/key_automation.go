package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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

type keyImportItem struct {
	ChannelID int64  `json:"channel_id"`
	AccountID int64  `json:"account_id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	collector.KeyImportResult
}

type keyImportBatchResult struct {
	Count    int             `json:"count"`
	Found    int             `json:"found"`
	Imported int             `json:"imported"`
	Skipped  int             `json:"skipped"`
	Failed   int             `json:"failed"`
	Deferred int             `json:"deferred"`
	Items    []keyImportItem `json:"items"`
}

func (s *Server) importKeys(w http.ResponseWriter, r *http.Request) {
	request, ok := s.keyAutomationRequest(w, r)
	if !ok {
		return
	}
	if s.ImportKeys == nil {
		s.fail(w, http.StatusNotImplemented, "Key 自动导入未配置")
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		targets, err := keyAutomationTargets(r, conn, request)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		result := keyImportBatchResult{Items: []keyImportItem{}}
		for _, account := range targets {
			item := keyImportItem{ChannelID: account.ChannelID, AccountID: account.ID}
			if account.Status != "active" {
				item.Status, item.Error = "skipped", "账号已停用"
				result.Items = append(result.Items, item)
				continue
			}
			got, err := s.ImportKeys(r.Context(), conn, account.ChannelID, account.ID)
			item.KeyImportResult = got
			if err != nil {
				item.Status, item.Error = "failed", shortErr(err)
				result.Failed++
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
	Items           []keyProvisionItem `json:"items"`
}

func (s *Server) provisionKeys(w http.ResponseWriter, r *http.Request) {
	request, ok := s.keyAutomationRequest(w, r)
	if !ok {
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
	s.withConn(w, r, func(conn *pgx.Conn) {
		targets, err := keyAutomationTargets(r, conn, request)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		result := keyProvisionBatchResult{Items: []keyProvisionItem{}}
		for _, account := range targets {
			item := keyProvisionItem{ChannelID: account.ChannelID, AccountID: account.ID}
			if account.Status != "active" {
				item.Status, item.Error = "skipped", "账号已停用"
				result.SkippedAccounts++
				result.Items = append(result.Items, item)
				continue
			}
			got, err := s.ProvisionKeys(r.Context(), conn, account.ChannelID, account.ID,
				collector.KeyProvisionRequest{
					Model: request.Model, OnlyWithoutKeys: request.OnlyWithoutKey, DryRun: dryRun,
				})
			item.KeyProvisionResult = got
			switch {
			case err != nil:
				item.Status, item.Error = "failed", shortErr(err)
				result.Failed++
			case got.SkippedReason != "":
				item.Status = "skipped"
				result.SkippedAccounts++
			case got.Failed > 0:
				item.Status = "partial"
			default:
				item.Status = "ok"
			}
			result.Found += got.Found
			result.Imported += got.Imported
			result.MatchedGroups += got.MatchedGroups
			result.ExistingGroups += got.ExistingGroups
			result.WouldCreate += got.WouldCreate
			result.Created += got.Created
			result.Failed += got.Failed
			result.Items = append(result.Items, item)
		}
		result.Count = len(result.Items)
		s.ok(w, result)
	})
}
