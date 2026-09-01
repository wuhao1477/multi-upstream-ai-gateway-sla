package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

// ── 账号 ──

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	chID, _ := strconv.ParseInt(r.URL.Query().Get("channel_id"), 10, 64)
	s.withConn(w, r, func(conn *pgx.Conn) {
		as, err := store.ListAccounts(r.Context(), conn, chID)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if as == nil {
			as = []store.Account{}
		}
		s.ok(w, map[string]any{"count": len(as), "items": as})
	})
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChannelID       int64  `json:"channel_id"`
		ExternalUserID  string `json:"external_user_id"`
		BalanceGroupKey string `json:"balance_group_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.ChannelID <= 0 {
		s.fail(w, http.StatusBadRequest, "channel_id 必填")
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		id, err := store.CreateAccount(r.Context(), conn, store.Account{
			ChannelID: in.ChannelID, ExternalUserID: in.ExternalUserID,
			BalanceGroupKey: in.BalanceGroupKey,
		})
		if err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	})
}

func (s *Server) patchAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		ExternalUserID  string     `json:"external_user_id"`
		BalanceGroupKey string     `json:"balance_group_key"`
		Status          string     `json:"status"`
		DisabledReason  string     `json:"disabled_reason"`
		DisabledUntil   *time.Time `json:"disabled_until"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Status != "" && in.Status != "active" && in.Status != "disabled" {
		s.fail(w, http.StatusBadRequest,
			"status 只能是 active 或 disabled，收到："+in.Status)
		return
	}
	if in.Status == "disabled" && strings.TrimSpace(in.DisabledReason) == "" {
		s.fail(w, http.StatusBadRequest, "停用账号必须填 disabled_reason（FR-095）")
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		err := store.UpdateAccount(r.Context(), conn, store.Account{
			ID: id, ExternalUserID: in.ExternalUserID,
			BalanceGroupKey: in.BalanceGroupKey, Status: in.Status,
			DisabledReason: in.DisabledReason, DisabledUntil: in.DisabledUntil,
		})
		if err != nil {
			s.mapNotFound(w, err)
			return
		}
		s.ok(w, map[string]any{"id": id, "updated": true})
	})
}

// ── Key（FR-122，脱敏是硬要求）──

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	chID, _ := strconv.ParseInt(r.URL.Query().Get("channel_id"), 10, 64)
	s.withConn(w, r, func(conn *pgx.Conn) {
		ks, err := store.ListKeys(r.Context(), conn, chID)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ks == nil {
			ks = []store.Key{}
		}
		s.ok(w, map[string]any{"count": len(ks), "items": ks})
	})
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID   int64  `json:"account_id"`
		Secret      string `json:"secret"`
		ExternalRef string `json:"external_ref"`
		GroupRef    string `json:"group_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.AccountID <= 0 || in.Secret == "" {
		s.fail(w, http.StatusBadRequest, "account_id 与 secret 必填")
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		var gid *int64
		if in.GroupRef != "" {
			// 需要 channel_id 才能定位分组，先查账号
			as, err := store.ListAccounts(r.Context(), conn, 0)
			if err != nil {
				s.fail(w, http.StatusInternalServerError, err.Error())
				return
			}
			var channelID int64
			for _, a := range as {
				if a.ID == in.AccountID {
					channelID = a.ChannelID
					break
				}
			}
			if channelID == 0 {
				s.fail(w, http.StatusBadRequest, "account_id 不存在")
				return
			}
			id, found, err := store.GroupIDByRef(r.Context(), conn, channelID, in.GroupRef)
			if err != nil {
				s.fail(w, http.StatusInternalServerError, err.Error())
				return
			}
			if !found {
				s.fail(w, http.StatusBadRequest, "group_ref 不存在："+in.GroupRef)
				return
			}
			gid = &id
		}
		id, err := store.CreateKey(r.Context(), conn, in.AccountID,
			in.Secret, in.ExternalRef, gid)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		s.Logger.Info("Key 已登记", "id", id, "account_id", in.AccountID)
		w.WriteHeader(http.StatusCreated)
		// **不回显明文**（FR-094）：只确认已收到
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "secret_stored": true,
			"note": "明文不回显；列表只显示前缀（FR-094）",
		})
	})
}

func (s *Server) patchKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Secret           *string    `json:"secret"`
		ExternalRef      *string    `json:"external_ref"`
		ChannelGroupID   *int64     `json:"channel_group_id"`
		GroupRef         string     `json:"group_ref"`
		RemainQuotaUSD   *float64   `json:"remain_quota_usd"`
		UsedQuotaUSD     *float64   `json:"used_quota_usd"`
		RPMLimit         *int       `json:"rpm_limit"`
		ConcurrencyLimit *int       `json:"concurrency_limit"`
		Status           *string    `json:"status"`
		ExpiredTime      *time.Time `json:"expired_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Secret != nil && *in.Secret == "" {
		s.fail(w, http.StatusBadRequest, "secret 不可为空")
		return
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "revoked" &&
		*in.Status != "expired" && *in.Status != "insufficient_perm" {
		s.fail(w, http.StatusBadRequest,
			"status 只能是 active、revoked、expired 或 insufficient_perm")
		return
	}
	for name, p := range map[string]*int{
		"rpm_limit": in.RPMLimit, "concurrency_limit": in.ConcurrencyLimit,
	} {
		if p != nil && *p < 0 {
			s.fail(w, http.StatusBadRequest, name+" 不可为负")
			return
		}
	}
	for name, p := range map[string]*float64{
		"remain_quota_usd": in.RemainQuotaUSD, "used_quota_usd": in.UsedQuotaUSD,
	} {
		if p != nil && *p < 0 {
			s.fail(w, http.StatusBadRequest, name+" 不可为负")
			return
		}
	}

	s.withConn(w, r, func(conn *pgx.Conn) {
		groupID := in.ChannelGroupID
		if in.GroupRef != "" || groupID != nil {
			var ok bool
			groupID, ok = s.resolveKeyGroupPatch(w, r, conn, id, groupID, in.GroupRef)
			if !ok {
				return
			}
		}
		err := store.UpdateKey(r.Context(), conn, store.KeyPatch{
			ID: id, Secret: in.Secret, ExternalRef: in.ExternalRef,
			ChannelGroupID: groupID,
			RemainQuotaUSD: in.RemainQuotaUSD, UsedQuotaUSD: in.UsedQuotaUSD,
			RPMLimit: in.RPMLimit, ConcurrencyLimit: in.ConcurrencyLimit,
			Status: in.Status, ExpiredTime: in.ExpiredTime,
		})
		if err != nil {
			if errors.Is(err, store.ErrDuplicate) {
				s.fail(w, http.StatusConflict, err.Error())
				return
			}
			s.mapNotFound(w, err)
			return
		}
		s.ok(w, map[string]any{"id": id, "updated": true})
	})
}

func (s *Server) resolveKeyGroupPatch(
	w http.ResponseWriter, r *http.Request, conn *pgx.Conn,
	keyID int64, groupID *int64, groupRef string,
) (*int64, bool) {
	k, err := store.GetKey(r.Context(), conn, keyID)
	if err != nil {
		s.mapNotFound(w, err)
		return nil, false
	}
	if groupID != nil {
		gs, err := store.ListChannelGroups(r.Context(), conn, k.ChannelID)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return nil, false
		}
		found := false
		for _, g := range gs {
			if g.ID == *groupID {
				found = true
				break
			}
		}
		if !found {
			s.fail(w, http.StatusBadRequest, "channel_group_id 不属于该 Key 的渠道")
			return nil, false
		}
	}
	if groupRef == "" {
		return groupID, true
	}
	gid, found, err := store.GroupIDByRef(r.Context(), conn, k.ChannelID, groupRef)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if !found {
		s.fail(w, http.StatusBadRequest, "group_ref 不存在："+groupRef)
		return nil, false
	}
	if groupID != nil && *groupID != gid {
		s.fail(w, http.StatusBadRequest, "channel_group_id 与 group_ref 不匹配")
		return nil, false
	}
	return &gid, true
}

func deleteKeyTokenKey(id int64) string {
	return fmt.Sprintf("delete:key:%d", id)
}

func (s *Server) previewDeleteKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		k, err := store.GetKey(r.Context(), conn, id)
		if err != nil {
			s.mapNotFound(w, err)
			return
		}
		version, err := store.KeyVersion(r.Context(), conn, id)
		if err != nil {
			s.mapNotFound(w, err)
			return
		}
		tok, err := s.tokens.issue(deleteKeyTokenKey(id), version, 0)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.ok(w, map[string]any{
			"key_id": id, "account_id": k.AccountID,
			"secret_prefix":             k.SecretPrefix,
			"confirm_token":             tok,
			"confirm_token_ttl_seconds": int(confirmTTL.Seconds()),
		})
	})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		ConfirmToken string `json:"confirm_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		s.fail(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if in.ConfirmToken == "" {
		s.fail(w, http.StatusBadRequest,
			"删除 Key 必须先 POST /admin/keys/{id}/delete-preview 取 confirm_token")
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		version, err := store.KeyVersion(r.Context(), conn, id)
		if err != nil {
			if !s.tokens.consume(in.ConfirmToken, deleteKeyTokenKey(id), "", 0) {
				s.fail(w, http.StatusBadRequest,
					"confirm_token 无效、已过期或与本次删除不符，请重新预览")
				return
			}
			s.mapNotFound(w, err)
			return
		}
		if !s.tokens.consume(in.ConfirmToken, deleteKeyTokenKey(id), version, 0) {
			s.fail(w, http.StatusBadRequest,
				"confirm_token 无效、已过期或与本次删除不符，请重新预览")
			return
		}
		if err := store.DeleteKey(r.Context(), conn, id, version); err != nil {
			if errors.Is(err, store.ErrVersionConflict) {
				s.fail(w, http.StatusConflict, err.Error())
				return
			}
			s.mapNotFound(w, err)
			return
		}
		s.ok(w, map[string]any{"id": id, "deleted": true})
	})
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Secret string `json:"secret"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)

	newSecret := in.Secret
	generated := false
	if newSecret == "" {
		// 未提供则生成一个 —— 便于"我就想换一个"的场景
		buf := make([]byte, 24)
		if _, err := rand.Read(buf); err != nil {
			s.fail(w, http.StatusInternalServerError, "生成随机 secret 失败")
			return
		}
		newSecret = "sk-" + hex.EncodeToString(buf)
		generated = true
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		if err := store.RotateKey(r.Context(), conn, id, newSecret); err != nil {
			s.mapNotFound(w, err)
			return
		}
		s.Logger.Info("Key 已轮换", "id", id, "generated", generated)
		resp := map[string]any{"id": id, "rotated": true}
		if generated {
			// **明文只在此处返回一次**（09 §5.0 / FR-094）
			resp["secret"] = newSecret
			resp["note"] = "明文只返回这一次，请立即保存"
		}
		s.ok(w, resp)
	})
}

func (s *Server) disableKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		if err := store.DisableKey(r.Context(), conn, id); err != nil {
			s.mapNotFound(w, err)
			return
		}
		s.ok(w, map[string]any{"id": id, "status": "revoked"})
	})
}

func (s *Server) keyUsage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	from := time.Now().Add(-30 * 24 * time.Hour)
	to := time.Now()
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		}
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		// 需要 channel_id 定位快照
		ks, err := store.ListKeys(r.Context(), conn, 0)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		var chID int64
		for _, k := range ks {
			if k.ID == id {
				chID = k.ChannelID
				break
			}
		}
		if chID == 0 {
			s.fail(w, http.StatusNotFound, fmt.Sprintf("Key %d 不存在", id))
			return
		}
		pts, err := store.KeyUsageHistory(r.Context(), conn, chID, id, from, to)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if pts == nil {
			pts = []store.KeyUsagePoint{}
		}
		s.ok(w, map[string]any{"key_id": id, "count": len(pts), "points": pts})
	})
}
