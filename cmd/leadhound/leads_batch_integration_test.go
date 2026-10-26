//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// batchFixture writes the store rows the contact search and agency import leave behind,
// all on reserved names: six probed sites, one agency, their routes.
func batchFixture(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	seedMailbox(t, lq, testUser, withDNSPass())
	core := store.NewQueries(storetest.AppConn(t))
	runID, err := core.StartRun(ctx, "leads-sites-probe", "sites-fixture")
	if err != nil {
		t.Fatal(err)
	}
	page := func(url string) int64 {
		id, err := core.InsertFetch(ctx, runID, "sites-fixture", fetch.Record{Kind: fetch.KindSite, URL: url, Status: 200, Body: []byte("<html></html>"), ContentType: "text/html"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	route := func(orgID int64, kind, value string, fid int64) {
		if _, err := lq.InsertRoute(ctx, leadstore.RouteRow{OrgID: orgID, Kind: kind, Value: value, FetchID: fid, MXOK: true, DerivedBy: "contacts@1"}); err != nil {
			t.Fatal(err)
		}
	}
	type site struct {
		domain, country string
		major           int
		exact           string
		signals         [][2]string
		email, form     string
	}
	for _, s := range []site{
		{"northwind.example", "GB", 7, "", [][2]string{{"Drupal.settings", ""}}, "info@northwind.example", ""},
		{"contoso.example", "DE", 10, "10.6.14", [][2]string{{"x-generator", "10"}, {"core-asset-v", "10.6.14"}}, "", "https://contoso.example/contact"},
		{"fabrikam.example", "GB", 11, "", [][2]string{{"x-generator", "11"}}, "info@fabrikam.example", ""},
		{"tailspin.example", "GB", 9, "", [][2]string{{"x-generator", "9"}}, "info@tailspin.example", ""},
		{"litware.example", "GB", 7, "", [][2]string{{"meta-generator", "7"}}, "office@litware.example", ""},
		{"adatum.example", "NL", 7, "", [][2]string{{"meta-generator", "7"}}, "info@adatum.example", ""},
	} {
		if _, err := lq.ImportSites(ctx, leadstore.OriginManual, []leadstore.SiteImport{{Domain: s.domain, Country: s.country}}); err != nil {
			t.Fatal(err)
		}
		siteID, err := lq.SiteIDByDomain(ctx, s.domain)
		if err != nil {
			t.Fatal(err)
		}
		home := page("https://" + s.domain + "/")
		var sigs []leadstore.Signal
		for _, sg := range s.signals {
			sigs = append(sigs, leadstore.Signal{Signal: sg[0], Value: sg[1], FetchID: home})
		}
		probeID, err := lq.RecordProbe(ctx, siteID, runID, leadstore.ProbeRow{
			Outcome: leadstore.OutcomeOK, FinalHost: s.domain, IsDrupal: true, Major: s.major, ExactVersion: s.exact,
			Signals: sigs, FetchIDs: []int64{home}, DerivedBy: "drupal@1",
		})
		if err != nil {
			t.Fatal(err)
		}
		orgID, _, err := lq.UpsertSiteOrg(ctx, probeID)
		if err != nil {
			t.Fatal(err)
		}
		if s.email != "" {
			route(orgID, leadstore.RouteEmail, s.email, home)
		}
		if s.form != "" {
			route(orgID, leadstore.RouteForm, s.form, page(s.form))
		}
	}
	evidence := page("https://www.wingtip.example/services/drupal-upgrades")
	ag, err := lq.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Wingtip Digital", Website: "https://www.wingtip.example/", Country: "GB"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lq.InsertAgencyFinding(ctx, leadstore.AgencyFindingRow{OrgID: ag.OrgID, URL: "https://www.wingtip.example/services/drupal-upgrades",
		Phrase: "upgrades from Drupal 7 to Drupal 10", FetchID: evidence, DerivedBy: "agencies@1"}); err != nil {
		t.Fatal(err)
	}
	route(ag.OrgID, leadstore.RouteEmail, "hello@wingtip.example", evidence)
}

func batchCount(t *testing.T, sql string) int {
	t.Helper()
	var n int
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := leadsRun(t, args...)
	if code != 0 {
		t.Fatalf("%q exited %d: %s\n%s", args, code, errOut, out)
	}
	return out
}

func TestLeadsBuildBatchApproveFlow(t *testing.T) {
	dir := leadsDB(t)
	t.Setenv("LEADHOUND_OUTREACH", "")
	t.Setenv("LEADHOUND_OUTREACH_USER", "")
	saved := leadsNow
	leadsNow = func() time.Time { return time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC) } // a Tuesday
	t.Cleanup(func() { leadsNow = saved })
	batchFixture(t)

	out := mustRun(t, "leads", "build")
	wantLines(t, "build", out,
		"leads build: 6 leads (6 new · 0 refreshed · 0 past new, kept) from 7 orgs · fixture leads (reserved names, never in a real batch) 6",
		"agency_partner: email 1", "unsupported: email 3 · none 1", "d10: form 1", "no lead: already current 1")
	if out := mustRun(t, "leads", "build"); !strings.Contains(out, "6 leads (0 new · 0 refreshed · 0 past new, kept)") {
		t.Errorf("second build:\n%s", out)
	}

	// Suppression: a domain suppressed now never enters a batch.
	wantLines(t, "suppress add", mustRun(t, "leads", "suppress", "add", "domain", "litware.example"),
		"Suppressed domain litware.example (manual): recorded · 1 lead reached, 1 moved to suppressed, 0 pending messages cancelled.")
	mustRun(t, "leads", "suppress", "add", "address", "jane@northwind.example", "--reason", "opt_out")
	out = mustRun(t, "leads", "suppress", "list")
	wantLines(t, "suppress list", out, "Suppressions: address opt_out 1 · domain manual 1", "address j***@northwind.example", "domain litware.example")
	if strings.Contains(out, "jane@") {
		t.Errorf("list shows an address unmasked:\n%s", out)
	}
	wantLines(t, "check", mustRun(t, "leads", "suppress", "check", "office@litware.example"), "address o***@litware.example: suppressed")
	wantLines(t, "check", mustRun(t, "leads", "suppress", "check", "contoso.example"), "domain contoso.example: not suppressed")

	// A shadow batch (no LEADHOUND_OUTREACH): reserved leads come in, the D9 lead is dropped
	// (no end-of-life fact), the suppressed one never appears.
	out = mustRun(t, "leads", "batch", "new")
	wantLines(t, "batch new", out,
		"Batch 1 (shadow): 3 emails, 1 contact form, 1 LinkedIn note. Read: leadhound leads batch show 1")
	if strings.Contains(out, "dropped") {
		t.Errorf("a lead was dropped:\n%s", out)
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		WHERE o.domain = 'litware.example'`); n != 0 {
		t.Errorf("the suppressed domain has %d drafted messages", n)
	}
	if n := batchCount(t, `SELECT count(*) FROM lead WHERE state <> 'new' AND state <> 'suppressed'`); n != 0 {
		t.Errorf("a shadow batch moved %d leads", n)
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_message WHERE channel = 'email' AND step > 0 AND in_reply_to <> (
		SELECT m0.message_id FROM outreach_message m0 WHERE m0.batch_id = outreach_message.batch_id AND m0.lead_id = outreach_message.lead_id AND m0.step = 0)`); n != 0 {
		t.Errorf("%d follow-ups not threaded to their first email", n)
	}
	if n := batchCount(t, `SELECT count(*) FROM run WHERE command = 'leads-batch-new' AND finished_at IS NOT NULL`); n != 1 {
		t.Errorf("%d finished batch-new runs", n)
	}

	out = mustRun(t, "leads", "batch", "show", "1")
	t.Logf("batch show 1:\n%s", out)
	var sum []byte
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT content_sha256 FROM outreach_batch WHERE id = 1`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	hash := hex.EncodeToString(sum)[:12]
	wantLines(t, "show", out,
		"Batch 1 (shadow) · 4 leads · awaiting your approval",
		"1. northwind.example · unsupported · email\n",
		"To: info@northwind.example",
		"Evidence: Drupal.settings · https://northwind.example/ · fetched",
		"2. contoso.example · d10 · form\n",
		"Form page (the lead's current route): https://contoso.example/contact",
		"Evidence: x-generator = 10 · https://contoso.example/ · fetched",
		"3. tailspin.example · unsupported · email\n",
		"Drupal 9 stopped getting security updates on 1 November 2023",
		"4. wingtip.example · agency_partner · email\n", "To: hello@wingtip.example",
		`Finding: agency page https://www.wingtip.example/services/drupal-upgrades says "upgrades from Drupal 7 to Drupal 10"`,
		"email 3 of 3", "LinkedIn note · message",
		"Approve: leadhound leads batch approve 1 --hash "+hash)
	if !strings.HasSuffix(out, "Approve: leadhound leads batch approve 1 --hash "+hash+"\n") {
		t.Errorf("show does not end with the approve line")
	}
	if one := mustRun(t, "leads", "batch", "show", "1", "--item", "2"); strings.Contains(one, "northwind") || !strings.Contains(one, "2. contoso.example") ||
		strings.Contains(one, "--hash") || !strings.Contains(one, "To approve, read the whole batch: leadhound leads batch show 1") {
		t.Errorf("--item 2:\n%s", one)
	}

	// A wrong hash is refused and changes nothing.
	code, _, errOut := leadsRun(t, "leads", "batch", "approve", "1", "--hash", "0123456789ab")
	if code != 2 || !strings.Contains(errOut, "does not match") {
		t.Errorf("wrong hash exited %d: %s", code, errOut)
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_message WHERE state <> 'draft'`); n != 0 {
		t.Errorf("a refused approval changed %d messages", n)
	}

	// Approve, leaving out item 4 (wingtip).
	out = mustRun(t, "leads", "batch", "approve", "1", "--hash", hash, "--skip", "4")
	wantLines(t, "approve", out,
		"Batch 1 (shadow) approved: 8 messages approved, 3 messages skipped.",
		"First emails are due from Tue 27 Oct 09:30 UTC",
		"Contact forms and LinkedIn notes wait for you")
	if n := batchCount(t, `SELECT count(*) FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		WHERE o.domain = 'wingtip.example' AND m.state = 'skipped'`); n != 3 {
		t.Errorf("--skip 4: %d wingtip messages skipped, want 3", n)
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_message WHERE state = 'approved' AND channel = 'email' AND step = 0
		AND due_at = '2026-10-27 09:30:00+00'`); n != 2 {
		t.Errorf("%d first emails due at 09:30 London", n)
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_message WHERE state = 'approved' AND (step > 0 OR channel <> 'email') AND due_at IS NOT NULL`); n != 0 {
		t.Errorf("%d follow-ups or manual items got a due time", n)
	}
	if n := batchCount(t, `SELECT count(*) FROM lead WHERE state NOT IN ('new', 'suppressed')`); n != 0 {
		t.Errorf("approving a shadow batch moved %d leads", n)
	}

	// The contact form is used by hand: done, and the shadow lead stays new.
	var formID int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT id FROM outreach_message WHERE channel = 'form'`).Scan(&formID); err != nil {
		t.Fatal(err)
	}
	wantLines(t, "done", mustRun(t, "leads", "done", fmt.Sprint(formID)), fmt.Sprintf("Message %d done (shadow batch: lead", formID), "stays new")
	if code, _, _ := leadsRun(t, "leads", "done", fmt.Sprint(formID)); code != 2 {
		t.Errorf("done twice exited %d", code)
	}

	// The outbox file a shadow send writes holds the headers Task 5.1 names.
	setup, err := leadsEnv(mustConfig(t), leadsForProbe)
	if err != nil {
		t.Fatal(err)
	}
	lq := leadstoretest.Q(t)
	msgs, err := lq.BatchMessages(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	box := &mailbox.Outbox{Dir: filepath.Join(dir, "outreach-outbox")}
	for _, m := range msgs {
		if m.Channel != "email" || m.State != leadstore.MsgApproved || m.Step != 1 {
			continue
		}
		raw, err := mailbox.Build(outboxMessage(m, setup.leads.Sender.Name, "outreach@"+"shadow.invalid", leadsNow()))
		if err != nil {
			t.Fatal(err)
		}
		name, err := box.Write(raw)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(box.Dir, name)) // #nosec G304 -- the test's own temp dir
		if err != nil {
			t.Fatal(err)
		}
		msg, err := mail.ReadMessage(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range []string{"From", "To", "Subject", "Date", "Message-Id", "In-Reply-To", "References", "List-Unsubscribe", "Mime-Version", "Content-Type", "Content-Transfer-Encoding"} {
			if msg.Header.Get(h) == "" {
				t.Errorf("outbox file lacks %s", h)
			}
		}
		if msg.Header.Get("In-Reply-To") != "<"+m.InReplyTo+">" || msg.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("outbox headers %v", msg.Header)
		}
	}
	if entries, _ := os.ReadDir(box.Dir); len(entries) != 2 {
		t.Errorf("%d outbox files, want 2 (one follow-up per approved email lead)", len(entries))
	}

	// Live: reserved leads never enter, so there is nothing to batch.
	t.Setenv("LEADHOUND_OUTREACH", "live")
	t.Setenv("LEADHOUND_OUTREACH_USER", "outreach@"+"sender.example")
	wantLines(t, "live batch new", mustRun(t, "leads", "batch", "new"), "No batch: no candidate lead")
	if n := batchCount(t, `SELECT count(*) FROM outreach_batch`); n != 1 {
		t.Errorf("%d batches after a live batch new over fixture leads, want 1", n)
	}
	t.Setenv("LEADHOUND_OUTREACH", "")

	// An email country whose language has no opt-out line cannot be loaded: batch new refuses
	// to start, so no such email is ever drafted.
	cfgPath := filepath.Join(dir, "leads.yaml")
	orig, err := os.ReadFile(cfgPath) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	noOptOut := regexp.MustCompile(`(?m)^  opt_out_lines:.*\n(    .*\n)*`).ReplaceAll(orig, nil)
	if bytes.Equal(noOptOut, orig) {
		t.Fatal("the demo leads.yaml has no opt_out_lines block to remove")
	}
	if err := os.WriteFile(cfgPath, noOptOut, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	if code, _, errOut := leadsRun(t, "leads", "batch", "new"); code != 2 || !strings.Contains(errOut, "sender.opt_out_lines.en is missing") {
		t.Errorf("batch new without opt-out lines exited %d: %s", code, errOut)
	}
	if err := os.WriteFile(cfgPath, orig, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}

	// Contacted by hand: never drafted again.
	wantLines(t, "mark-contacted", mustRun(t, "leads", "mark-contacted", "northwind.example", "--note", "called them"), "is now contacted by hand")
	out = mustRun(t, "leads", "batch", "new")
	wantLines(t, "second shadow batch", out, "Batch 2 (shadow): 2 emails, 1 contact form, 1 LinkedIn note.")
	if n := batchCount(t, `SELECT count(*) FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		WHERE m.batch_id = 2 AND o.domain = 'northwind.example'`); n != 0 {
		t.Errorf("a hand-contacted lead was drafted again")
	}
}

// mustConfig loads the process config the commands use.
func mustConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A row edited after batch new: show prints no approve line and approve refuses it.
func TestLeadsBatchTampered(t *testing.T) {
	leadsDB(t)
	t.Setenv("LEADHOUND_OUTREACH", "")
	t.Setenv("LEADHOUND_OUTREACH_USER", "")
	batchFixture(t)
	mustRun(t, "leads", "build")
	mustRun(t, "leads", "batch", "new")
	out := mustRun(t, "leads", "batch", "show", "1")
	i := strings.Index(out, "--hash ")
	if i < 0 {
		t.Fatalf("no approve line:\n%s", out)
	}
	hash := strings.TrimSpace(out[i+len("--hash "):])
	if _, err := storetest.OwnerConn(t).Exec(context.Background(),
		`UPDATE outreach_message SET to_address = 'other@contoso.example' WHERE id = (SELECT min(id) FROM outreach_message WHERE channel = 'email')`); err != nil {
		t.Fatal(err)
	}
	out = mustRun(t, "leads", "batch", "show", "1")
	if strings.Contains(out, "--hash") || !strings.Contains(out, "Batch changed since it was drafted") || !strings.Contains(out, "To: other@contoso.example") {
		t.Errorf("show of a changed batch:\n%s", out)
	}
	code, _, errOut := leadsRun(t, "leads", "batch", "approve", "1", "--hash", hash)
	if code != 2 || !strings.Contains(errOut, "changed since it was drafted") {
		t.Errorf("approve of a changed batch exited %d: %s", code, errOut)
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_message WHERE state <> 'draft'`); n != 0 {
		t.Errorf("%d messages changed state", n)
	}
}

// A second build refreshes a new lead from its latest probe, skips one whose latest probe
// makes no lead, and leaves a lead past new alone.
func TestLeadsBuildRefreshes(t *testing.T) {
	leadsDB(t)
	batchFixture(t)
	mustRun(t, "leads", "build")
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	core := store.NewQueries(storetest.AppConn(t))
	runID, err := core.StartRun(ctx, "leads-sites-probe", "sites-fixture")
	if err != nil {
		t.Fatal(err)
	}
	reprobe := func(domain string, major int, sig [2]string) int64 {
		t.Helper()
		fid, err := core.InsertFetch(ctx, runID, "sites-fixture", fetch.Record{Kind: fetch.KindSite, URL: "https://" + domain + "/", Status: 200, Body: []byte("<html></html>")})
		if err != nil {
			t.Fatal(err)
		}
		siteID, err := lq.SiteIDByDomain(ctx, domain)
		if err != nil {
			t.Fatal(err)
		}
		id, err := lq.RecordProbe(ctx, siteID, runID, leadstore.ProbeRow{Outcome: leadstore.OutcomeOK, FinalHost: domain, IsDrupal: true, Major: major,
			Signals: []leadstore.Signal{{Signal: sig[0], Value: sig[1], FetchID: fid}}, FetchIDs: []int64{fid}, DerivedBy: "drupal@1"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	newProbe := reprobe("contoso.example", 9, [2]string{"x-generator", "9"}) // D10 → D9: refreshed to unsupported
	reprobe("northwind.example", 11, [2]string{"x-generator", "11"})         // D7 → D11: no longer a lead
	mustRun(t, "leads", "mark-contacted", "tailspin.example")                // past new: kept
	reprobe("tailspin.example", 11, [2]string{"x-generator", "11"})

	out := mustRun(t, "leads", "build")
	wantLines(t, "second build", out, "(0 new · 1 refreshed · 1 past new, kept)", "1 new leads skipped (no longer a lead)", "no lead: already current 2")
	type leadRow struct {
		Kind, State string
		ProbeID     *int64
	}
	get := func(domain string) leadRow {
		var r leadRow
		if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT l.kind, l.state, l.probe_id FROM lead l JOIN org o ON o.id = l.org_id WHERE o.domain = $1`, domain).
			Scan(&r.Kind, &r.State, &r.ProbeID); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := get("contoso.example"); r.Kind != "unsupported" || r.State != "new" || r.ProbeID == nil || *r.ProbeID != newProbe {
		t.Errorf("contoso = %+v, want unsupported on probe %d", r, newProbe)
	}
	if r := get("northwind.example"); r.State != "skipped" {
		t.Errorf("northwind = %+v, want skipped", r)
	}
	if n := batchCount(t, `SELECT count(*) FROM lead_event e JOIN lead l ON l.id = e.lead_id JOIN org o ON o.id = l.org_id
		WHERE o.domain = 'northwind.example' AND e.state = 'skipped' AND e.actor = 'system' AND e.reason = 'build: already current'`); n != 1 {
		t.Errorf("northwind skip event: %d", n)
	}
	if r := get("tailspin.example"); r.State != "contacted" || r.Kind != "unsupported" {
		t.Errorf("tailspin = %+v, want contacted and untouched", r)
	}
}
