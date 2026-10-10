package store

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestCredentialCipherBindsRecordAndRejectsTampering(t *testing.T) {
	const plain = "test-credential-marker"
	a, err := sealCredential("upstream_keys", 7, "secret", plain)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := sealCredential("upstream_keys", 7, "secret", plain)
	if bytes.Equal(a, b) || bytes.Contains(a, []byte(plain)) {
		t.Fatal("same secret must encrypt differently each time and never contain plaintext")
	}
	if got, err := openCredential("upstream_keys", 7, "secret", a); err != nil || got != plain {
		t.Fatalf("round trip = %q, %v", got, err)
	}

	tampered := append([]byte(nil), a...)
	tampered[len(tampered)-1] ^= 1
	unknownVersion := append([]byte{9}, a[1:]...)
	for name, open := range map[string]func() (string, error){
		"other_record": func() (string, error) { return openCredential("upstream_keys", 8, "secret", a) },
		"other_field":  func() (string, error) { return openCredential("upstream_keys", 7, "refresh_token", a) },
		"other_table":  func() (string, error) { return openCredential("collector_credentials", 7, "secret", a) },
		"tampered":     func() (string, error) { return openCredential("upstream_keys", 7, "secret", tampered) },
		"version":      func() (string, error) { return openCredential("upstream_keys", 7, "secret", unknownVersion) },
		"truncated":    func() (string, error) { return openCredential("upstream_keys", 7, "secret", a[:5]) },
	} {
		if got, err := open(); !errors.Is(err, ErrCredentialDecrypt) || got != "" {
			t.Errorf("%s: got %q, %v; want ErrCredentialDecrypt", name, got, err)
		}
	}

	useCredentialKey(t, base64.StdEncoding.EncodeToString([]byte("another-test-key-of-32-bytes-ok!")))
	if _, err := openCredential("upstream_keys", 7, "secret", a); !errors.Is(err, ErrCredentialDecrypt) {
		t.Errorf("wrong key: %v", err)
	}
}

func TestCredentialCipherWithoutKeyFailsInsteadOfFallingBack(t *testing.T) {
	sealed, err := sealCredential("upstream_keys", 1, "secret", "test-credential-marker")
	if err != nil {
		t.Fatal(err)
	}
	useCredentialKey(t, "")
	if _, err := sealCredential("upstream_keys", 1, "secret", "x"); !errors.Is(err, ErrCredentialKeyMissing) {
		t.Errorf("seal without key: %v", err)
	}
	if _, err := openCredential("upstream_keys", 1, "secret", sealed); !errors.Is(err, ErrCredentialKeyMissing) {
		t.Errorf("open without key: %v", err)
	}
	// 空秘密即 NULL：库里没有秘密时不需要密钥。
	if got, err := sealCredential("upstream_keys", 1, "secret", ""); got != nil || err != nil {
		t.Errorf("empty secret = %v, %v", got, err)
	}
	if got, err := openCredential("upstream_keys", 1, "secret", nil); got != "" || err != nil {
		t.Errorf("NULL secret = %q, %v", got, err)
	}
	for _, bad := range []string{"not-base64!", base64.StdEncoding.EncodeToString([]byte("too-short"))} {
		if err := ConfigureCredentialKey(bad); err == nil || strings.Contains(err.Error(), bad) {
			t.Errorf("bad key %q: %v", bad, err)
		}
	}
}

func TestWebDAVDisplayURLHidesCredentials(t *testing.T) {
	for raw, want := range map[string]string{
		"https://user:pass-marker@dav.example.invalid/dav/a.json?token=marker#frag": "https://dav.example.invalid/dav/a.json",
		"https://dav.example.invalid/dav/?":                                         "https://dav.example.invalid/dav/",
		"HTTPS://dav.example.invalid/dav/":                                          "HTTPS://dav.example.invalid/dav/",
		"":                                                                          "",
	} {
		if got := webdavDisplayURL(raw); got != want {
			t.Errorf("webdavDisplayURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
