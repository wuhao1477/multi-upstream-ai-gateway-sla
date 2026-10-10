package collector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"strings"
)

// HTTP clients wrap network failures in url.Error, whose text includes secrets
// in the URL. Preserve internal errors (including the shared limiter) unchanged.
func transportError(err error) error {
	var requestErr *url.Error
	if errors.As(err, &requestErr) {
		return networkError(requestErr.Err)
	}
	return err
}

// Only retain safe categories, never remote addresses, URLs or reader messages.
func networkError(err error) error {
	var timeout net.Error
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var unknownCA x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var operation *net.OpError
	switch {
	case errors.Is(err, ErrOutboundBlocked):
		return ErrOutboundBlocked
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return context.DeadlineExceeded
	case errors.As(err, &dns):
		return errors.New("DNS 解析失败")
	case errors.As(err, &cert), errors.As(err, &record), errors.As(err, &unknownCA), errors.As(err, &hostname):
		return errors.New("TLS 校验或握手失败")
	case errors.As(err, &operation) && operation.Op == "dial":
		return errors.New("连接失败")
	default:
		return errors.New("网络读取或请求失败")
	}
}

// Paths are internal API routes; queries and fragments are never diagnostics.
func diagnosticPath(path string) string {
	path, _, _ = strings.Cut(path, "?")
	path, _, _ = strings.Cut(path, "#")
	return path
}
