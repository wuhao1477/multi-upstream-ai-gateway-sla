package store

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wuhao1477/multi-upstream-ai-gateway-sla/internal/collector"
)

// The controlled rejection tests persistence behavior, not a real site's protocol:
// a real upstream cannot expire one selected account between two reads on demand.
func TestSub2APIBusinessRejectionPreservesAccountSnapshots(t *testing.T) {
	dsn := os.Getenv("SLA_TEST_DSN")
	if dsn == "" {
		t.Skip("需要 SLA_TEST_DSN 指向可写的测试库")
	}
	ctx := context.Background()
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, release, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var badReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer test-rejected" && badReads.Add(1) > 1 {
			_, _ = io.WriteString(w, `{"code":401,"message":"test-secret-marker","data":null}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"id":1}}`)
	}))
	defer server.Close()
	defer wipeInventoryTest(ctx, t, conn, server.URL)
	channelID, err := CreateChannel(ctx, conn, Channel{Name: "business-rejection", SiteFamily: "sub2api", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	var creds []collector.Credential
	for _, token := range []string{"test-rejected", "test-accepted"} {
		accountID, err := CreateAccount(ctx, conn, Account{ChannelID: channelID})
		if err != nil {
			t.Fatal(err)
		}
		creds = append(creds, collector.Credential{
			ChannelID: channelID, AccountID: accountID, BaseURL: server.URL,
			Family: collector.FamilySub2API, AccessToken: token,
		})
	}
	sink := NewCollectorSink(pool)
	if err := sink.SaveAccount(ctx, channelID, creds[0].AccountID, collector.Account{
		UserID: "previous", GroupRef: "keep",
		Meta: collector.SourceMeta{Degraded: true, FetchedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	syncer := collector.Syncer{Adapter: collector.NewSub2APIAdapter(collector.NewClient(0)), Sink: sink}
	result, err := syncer.SyncManySelected(ctx, creds, collector.CapAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Status != collector.StatusPartial || result.Items[0].Rows != 1 {
		t.Fatal("rejected account was not reported as partial while preserving the successful account")
	}
	if strings.Contains(result.Items[0].Error, "test-secret-marker") {
		t.Fatal("sync result exposed the rejected response")
	}
	for _, cred := range creds {
		var count int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM collector_snapshots
WHERE channel_id=$1 AND scope_type='account' AND scope_id=$2`, channelID, fmt.Sprint(cred.AccountID)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("account %d has %d snapshots; want one retained or successful snapshot", cred.AccountID, count)
		}
	}
	var group string
	if err := conn.QueryRow(ctx, `SELECT account_group FROM upstream_accounts WHERE id=$1`, creds[0].AccountID).Scan(&group); err != nil {
		t.Fatal(err)
	}
	if group != "keep" {
		t.Fatal("rejected account overwrote its previous group")
	}
}
