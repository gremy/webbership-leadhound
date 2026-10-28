//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// orgPhone adds number as a phone fact of the org at domain, read from the latest fetch.
func orgPhone(t *testing.T, domain, number string) {
	t.Helper()
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	var orgID, fetchID int64
	if err := owner.QueryRow(ctx, `SELECT id FROM org WHERE domain = $1`, domain).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `SELECT max(id) FROM http_fetch`).Scan(&fetchID); err != nil {
		t.Fatal(err)
	}
	if _, err := leadstoretest.Q(t).InsertFact(ctx, leadstore.FactRow{OrgID: orgID, Kind: leadstore.FactPhone, Value: number,
		Normalized: number, FetchID: fetchID, DerivedBy: "enrich@1"}); err != nil {
		t.Fatal(err)
	}
}

// TestBatchCallPolicyByCountry: a country whose call policy is off in the campaign's rules
// keeps its call. An IE lead with a phone in a spec with IE call off gets its call row and its
// "tried to reach you" email like a GB lead (call on); every place the call shows says calls
// are off there, so the operator decides: the batch new and batch show summaries, the call
// card, the Today row, the lead page's Sequence step and calls list. The GB call carries no
// such note.
func TestBatchCallPolicyByCountry(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	drawFromAll(t, dir)
	t.Setenv("LEADHOUND_OUTREACH", "")
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC) // a Tuesday
	saved := leadsNow
	leadsNow = func() time.Time { return now }
	t.Cleanup(func() { leadsNow = saved })

	cfgPath := filepath.Join(dir, "leads.yaml")
	raw, err := os.ReadFile(cfgPath) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	ie := strings.Replace(string(raw), "  DE: {channel: form,", "  IE: {channel: email, lang: en, tz: Europe/Dublin, footer: footer-en}\n  DE: {channel: form,", 1)
	if ie == string(raw) {
		t.Fatal("leads.yaml countries not rewritten")
	}
	if err := os.WriteFile(cfgPath, []byte(ie), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(dir, campaign.Dir), os.DirFS("../../examples/campaigns")); err != nil {
		t.Fatal(err)
	}

	batchFixture(t)
	if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE org SET country = 'IE' WHERE domain = 'northwind.example'`); err != nil {
		t.Fatal(err)
	}
	orgPhone(t, "northwind.example", "+353215550100")
	orgPhone(t, "tailspin.example", "+441632960002")

	mustRun(t, "leads", "campaigns", "register")
	mustRun(t, "leads", "build")
	out := mustRun(t, "leads", "batch", "new")
	wantLines(t, "batch new", out, "Calls: 2 call steps", "Calls drafted where the campaign has calls off: 1 (IE): your call.")

	type row struct {
		id                 int64
		channel, condition string
	}
	rows := func(domain string) []row {
		t.Helper()
		rs, err := storetest.OwnerConn(t).Query(ctx,
			`SELECT m.id, m.channel, m.condition FROM outreach_message m JOIN lead l ON l.id = m.lead_id
			 JOIN org o ON o.id = l.org_id WHERE o.domain = $1 ORDER BY m.step`, domain)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		var out []row
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.id, &r.channel, &r.condition); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	callOf := func(domain string) (int64, bool) {
		var call int64
		tried := false
		for _, r := range rows(domain) {
			if r.channel == leadstore.ChannelPhone {
				call = r.id
			}
			if r.condition == campaign.WhenCallNotConnected {
				tried = true
			}
		}
		return call, tried
	}
	ieCall, ieTried := callOf("northwind.example")
	gbCall, gbTried := callOf("tailspin.example")
	if ieCall == 0 || !ieTried || gbCall == 0 || !gbTried {
		t.Fatalf("call rows: IE %d (tried %v), GB %d (tried %v), want both with a call and its follow-up", ieCall, ieTried, gbCall, gbTried)
	}

	show := mustRun(t, "leads", "batch", "show", "1")
	wantLines(t, "batch show", show, "Calls drafted where the campaign has calls off: 1 (IE): your call.",
		"Calls to Ireland are off in this campaign's rules. Your call whether to ring; if you don't, choose Skipped.")
	if n := strings.Count(show, "Calls to "); n != 1 {
		t.Errorf("batch show warns on %d calls, want 1 (the IE one)", n)
	}
	mustRun(t, "leads", "batch", "approve", "1", "--hash", batchHash(t, 1))

	b := testBackend(t)
	b.cfg.DataDir = dir
	b.now = func() time.Time { return now }
	card, err := b.LeadCall(ctx, ieCall)
	if err != nil {
		t.Fatal(err)
	}
	if card.CallsOff != "Calls to Ireland are off in this campaign's rules. Your call whether to ring; if you don't, choose Skipped." ||
		card.NotDue != "Not due yet: this call waits on the email before it." {
		t.Errorf("IE card: calls off %q, not due %q", card.CallsOff, card.NotDue)
	}
	if card, err := b.LeadCall(ctx, gbCall); err != nil || card.CallsOff != "" {
		t.Errorf("GB card: calls off %q, %v", card.CallsOff, err)
	}
	today, err := b.LeadsToday(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var ieLine, gbLine string
	for _, it := range today.CallsWaiting {
		switch it.MessageID {
		case ieCall:
			ieLine = it.Line
		case gbCall:
			gbLine = it.Line
		}
	}
	if !strings.Contains(ieLine, "calls off for IE in the campaign rules") || !strings.Contains(ieLine, "waits on the email before it") ||
		gbLine == "" || strings.Contains(gbLine, "calls off") || today.CallsWaitingTotal != 2 {
		t.Errorf("Today waiting: IE %q, GB %q, total %d", ieLine, gbLine, today.CallsWaitingTotal)
	}
	var ieLead int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT lead_id FROM outreach_message WHERE id = $1`, ieCall).Scan(&ieLead); err != nil {
		t.Fatal(err)
	}
	seq, err := b.LeadSequence(ctx, ieLead)
	if err != nil {
		t.Fatal(err)
	}
	notes := 0
	for _, st := range seq {
		if st.Note == "calls off for IE in the campaign rules" {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("IE sequence carries %d calls-off notes, want 1 (the call): %+v", notes, seq)
	}
	if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE outreach_message SET due_at = $2 WHERE id = $1`, ieCall, now); err != nil {
		t.Fatal(err)
	}
	wantLines(t, "calls list", mustRun(t, "leads", "calls", "list"), "calls off for IE in the campaign rules: your call")
}
