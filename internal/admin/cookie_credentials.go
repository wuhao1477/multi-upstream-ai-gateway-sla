package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collection"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

func (s *Server) validateCookieCredential(w http.ResponseWriter, r *http.Request) {
	if s.ReadOnly {
		s.fail(w, http.StatusConflict, "只读模式不能验证 Cookie")
		return
	}
	if s.CookieCredentials == nil || s.ValidateCookie == nil {
		s.fail(w, http.StatusServiceUnavailable, "未配置 Cookie 读取，请检查 SLA_COOKIE_SECRET_KEY")
		return
	}
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil || strings.TrimSpace(string(body)) != "" {
		s.fail(w, http.StatusBadRequest, "验证仅使用已存 Cookie，不接收请求字段")
		return
	}
	if err := s.ValidateCookie(r.Context(), id); err != nil {
		status, _, _ := collector.HTTPFailure(err)
		s.Logger.Warn("Cookie 验证失败", "account_id", id, "error_type", fmt.Sprintf("%T", err), "http_status", status)
		s.cookieCredentialError(w, err)
		return
	}
	s.ok(w, map[string]string{"state": "ready"})
}

func (s *Server) saveCookieCredential(w http.ResponseWriter, r *http.Request) {
	if s.ReadOnly {
		s.fail(w, http.StatusConflict, "只读模式不能修改凭据")
		return
	}
	if s.CookieCredentials == nil {
		s.fail(w, http.StatusServiceUnavailable, "未配置 SLA_COOKIE_SECRET_KEY")
		return
	}
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var in store.SaveCookieCredentialInput
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		s.fail(w, http.StatusBadRequest, "Cookie 请求格式无效")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		s.fail(w, http.StatusBadRequest, "Cookie 请求格式无效")
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		tx, err := conn.Begin(r.Context())
		if err != nil {
			s.cookieCredentialError(w, err)
			return
		}
		defer func() { _ = tx.Rollback(r.Context()) }()
		changed, err := s.CookieCredentials.Save(r.Context(), tx, id, in)
		if err != nil {
			s.cookieCredentialError(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.cookieCredentialError(w, err)
			return
		}
		s.ok(w, map[string]bool{"stored": true, "changed": changed})
	})
}

func (s *Server) deleteCookieCredential(w http.ResponseWriter, r *http.Request) {
	if s.ReadOnly {
		s.fail(w, http.StatusConflict, "只读模式不能修改凭据")
		return
	}
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	// 清除不需要密钥，部署密钥丢失后仍可删除旧配置。
	s.withConn(w, r, func(conn *pgx.Conn) {
		tx, err := conn.Begin(r.Context())
		if err != nil {
			s.cookieCredentialError(w, err)
			return
		}
		defer func() { _ = tx.Rollback(r.Context()) }()
		if err := store.DeleteCookieCredential(r.Context(), tx, id); err != nil {
			s.cookieCredentialError(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.cookieCredentialError(w, err)
			return
		}
		s.ok(w, map[string]bool{"cleared": true})
	})
}

func (s *Server) cookieCredentialError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, http.StatusNotFound, "账号或 Cookie 配置不存在")
	case errors.Is(err, store.ErrCookieCredentialInput):
		s.fail(w, http.StatusBadRequest, store.ErrCookieCredentialInput.Error())
	case errors.Is(err, store.ErrCookieSecret):
		s.fail(w, http.StatusServiceUnavailable, store.ErrCookieSecret.Error())
	case errors.Is(err, store.ErrCookieSite):
		s.fail(w, http.StatusBadRequest, store.ErrCookieSite.Error())
	case errors.Is(err, store.ErrCookieCredentialChanged):
		s.fail(w, http.StatusConflict, store.ErrCookieCredentialChanged.Error())
	case errors.Is(err, collection.ErrChannelReadBusy):
		s.fail(w, http.StatusConflict, collection.ErrChannelReadBusy.Error())
	case errors.Is(err, collector.ErrCookieExpired):
		s.fail(w, http.StatusUnprocessableEntity, collector.ErrCookieExpired.Error())
	case errors.Is(err, collector.ErrCookieNeedsAction):
		s.fail(w, http.StatusUnprocessableEntity, "Cookie 需人工处理，请检查原站会话和登记的上游用户 ID")
	default:
		// 数据库错误可能含 DETAIL，禁止把请求值或密文带入日志/响应。
		var pgErr *pgconn.PgError
		code := ""
		if errors.As(err, &pgErr) {
			code = pgErr.Code
		}
		s.Logger.Error("Cookie 操作失败", "error_type", fmt.Sprintf("%T", err), "sql_state", code)
		s.fail(w, http.StatusBadGateway, "Cookie 操作失败，请检查站点、网络和数据库")
	}
}
