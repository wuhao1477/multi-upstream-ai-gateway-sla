package collector

import (
	"strings"
	"testing"
)

// 结构来源：all-api-hub 949bd8e 的 SiteAccount / AccountStorageConfig。
// 被测的是导入格式，不是上游站点认证协议；Cookie 值为测试输入。
func TestHubCookieCredentials(t *testing.T) {
	for _, id := range []string{`42`, `"42"`} {
		b, err := ParseHubBackup(strings.NewReader(`{"accounts":{"accounts":[{
			"id":"extension-entry-not-user-id","site_url":"https://example.invalid",
			"authType":"cookie","cookieAuth":{"sessionCookie":"session=test-only==; pref=light"},
			"account_info":{"id":` + id + `,"access_token":""}}]}}`))
		if err != nil {
			t.Fatal(err)
		}
		a := b.Accounts.Accounts[0]
		if !a.HasCredential() || a.UserID() != "42" {
			t.Fatal("cookie account or upstream identity was lost")
		}
	}
	for _, auth := range []string{"cookie", "none"} {
		b, err := ParseHubBackup(strings.NewReader(`{"accounts":{"accounts":[{
			"authType":"` + auth + `","account_info":{"access_token":"residual-token"}}]}}`))
		if err != nil {
			t.Fatal(err)
		}
		if b.Accounts.Accounts[0].HasCredential() {
			t.Fatal("inactive residual token was accepted")
		}
	}
}
