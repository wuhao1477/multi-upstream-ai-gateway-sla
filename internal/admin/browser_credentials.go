package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/store"
)

func (s *Server) saveBrowserCredential(w http.ResponseWriter, r *http.Request) {
	if s.ReadOnly {
		s.fail(w, http.StatusConflict, "只读模式不能修改凭据")
		return
	}
	if s.BrowserCredentials == nil {
		s.fail(w, http.StatusServiceUnavailable, "未配置 SLA_BROWSER_SECRET_KEY")
		return
	}
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var in store.SaveBrowserCredentialInput
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		s.fail(w, http.StatusBadRequest, "浏览器凭据请求格式无效")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		s.fail(w, http.StatusBadRequest, "浏览器凭据请求格式无效")
		return
	}
	s.withConn(w, r, func(conn *pgx.Conn) {
		tx, err := conn.Begin(r.Context())
		if err != nil {
			s.browserCredentialError(w, err)
			return
		}
		defer func() { _ = tx.Rollback(r.Context()) }()
		if err := s.BrowserCredentials.Save(r.Context(), tx, id, in); err != nil {
			s.browserCredentialError(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.browserCredentialError(w, err)
			return
		}
		s.ok(w, map[string]any{"stored": true, "state": "unverified"})
	})
}

func (s *Server) deleteBrowserCredential(w http.ResponseWriter, r *http.Request) {
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
			s.browserCredentialError(w, err)
			return
		}
		defer func() { _ = tx.Rollback(r.Context()) }()
		if err := store.DeleteBrowserCredential(r.Context(), tx, id); err != nil {
			s.browserCredentialError(w, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.browserCredentialError(w, err)
			return
		}
		s.ok(w, map[string]bool{"cleared": true})
	})
}

func (s *Server) browserCredentialError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, http.StatusNotFound, "账号或浏览器凭据不存在")
	case errors.Is(err, store.ErrBrowserCredentialInput):
		s.fail(w, http.StatusBadRequest, store.ErrBrowserCredentialInput.Error())
	case errors.Is(err, store.ErrBrowserSecret):
		s.fail(w, http.StatusServiceUnavailable, store.ErrBrowserSecret.Error())
	case errors.Is(err, store.ErrBrowserSite):
		s.fail(w, http.StatusBadRequest, store.ErrBrowserSite.Error())
	default:
		// 数据库错误可能含 DETAIL，禁止把请求值或密文带入日志/响应。
		var pgErr *pgconn.PgError
		code := ""
		if errors.As(err, &pgErr) {
			code = pgErr.Code
		}
		s.Logger.Error("浏览器凭据操作失败", "error_type", fmt.Sprintf("%T", err), "sql_state", code)
		s.fail(w, http.StatusInternalServerError, "浏览器凭据操作失败，请检查站型、HTTPS 地址和数据库")
	}
}
