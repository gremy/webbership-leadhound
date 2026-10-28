//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestCallFirstBatchSequence: a lead whose country's email needs consent (RO) gets the
// campaign's call_first sequence, stored per lead: a call first, a second call if it did not
// connect, then the emails only on their yes, and never the default sequence's cold email;
// a GB lead in the same batch keeps the default one. Connected with their yes, the first
// email falls due; an opt-out then cancels the emails still to go.
func TestCallFirstBatchSequence(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	drawFromAll(t, dir)
	t.Setenv("LEADHOUND_OUTREACH", "")
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	saved := leadsNow
	leadsNow = func() time.Time { return now }
	t.Cleanup(func() { leadsNow = saved })

	cfgPath := filepath.Join(dir, "leads.yaml")
	raw, err := os.ReadFile(cfgPath) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	ro := strings.Replace(string(raw), "  DE: {channel: form,", "  RO: {channel: email, lang: en, tz: Europe/Bucharest, footer: footer-en}\n  DE: {channel: form,", 1)
	if err := os.WriteFile(cfgPath, []byte(ro), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	cdir := filepath.Join(dir, campaign.Dir)
	if err := os.CopyFS(cdir, os.DirFS("../../examples/campaigns")); err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(cdir, "drupal-eol", campaign.FileName)
	spec, err := os.ReadFile(specPath) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(spec), "  DE: {email: off, call: off}", "  RO: {email: on, call: on, lang: en}\n  DE: {email: off, call: off}", 1)
	s = strings.Replace(s, "templates_dir: templates", `sequences:
  call_first:
    - {step: 0, type: call}
    - {step: 1, type: call, after_business_days: 2, when: call_not_connected}
    - {step: 2, type: email, when: email_consent}
    - {step: 3, type: email, after_business_days: 4, when: email_consent}
templates_dir: templates`, 1)
	if s == string(spec) {
		t.Fatal("spec not rewritten")
	}
	if err := os.WriteFile(specPath, []byte(s), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	batchFixture(t)
	owner := storetest.OwnerConn(t)
	if _, err := owner.Exec(ctx, `UPDATE org SET country = 'RO' WHERE domain = 'northwind.example'`); err != nil {
		t.Fatal(err)
	}
	orgPhone(t, "northwind.example", "+40215550100")
	orgPhone(t, "tailspin.example", "+441632960002")
	mustRun(t, "leads", "campaigns", "register")
	mustRun(t, "leads", "build")
	// A call-first lead with a number and no email address at all (an RO café from a map
	// listing, say): batchable, its emails waiting on the address it gives on the call.
	if _, err := owner.Exec(ctx, `UPDATE org SET country = 'RO' WHERE domain = 'litware.example'`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE lead SET channel = 'none', route_id = NULL
		WHERE org_id = (SELECT id FROM org WHERE domain = 'litware.example')`); err != nil {
		t.Fatal(err)
	}
	orgPhone(t, "litware.example", "+40215550101")
	out := mustRun(t, "leads", "batch", "new")

	type row struct {
		id                       int64
		step                     int
		channel, condition, name string
	}
	rows := func(domain string) []row {
		t.Helper()
		rs, err := owner.Query(ctx,
			`SELECT m.id, m.step, m.channel, m.condition, coalesce(m.sequence_name, '') FROM outreach_message m
			 JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id WHERE o.domain = $1 ORDER BY m.step`, domain)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		var got []row
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.id, &r.step, &r.channel, &r.condition, &r.name); err != nil {
				t.Fatal(err)
			}
			got = append(got, r)
		}
		return got
	}
	roRows := rows("northwind.example")
	shape := ""
	for _, r := range roRows {
		shape += fmt.Sprintf("%d:%s:%s:%s ", r.step, r.channel, r.condition, r.name)
	}
	want := "0:phone:always:call_first 1:phone:call_not_connected:call_first 2:email:email_consent:call_first 3:email:email_consent:call_first "
	if shape != want {
		t.Fatalf("RO lead rows = %q\nwant %q\nbatch new said:\n%s", shape, want, out)
	}
	phoneOnly := ""
	for _, r := range rows("litware.example") {
		phoneOnly += fmt.Sprintf("%d:%s:%s:%s ", r.step, r.channel, r.condition, r.name)
	}
	if phoneOnly != want {
		t.Errorf("phone-only RO lead rows = %q\nwant %q\nbatch new said:\n%s", phoneOnly, want, out)
	}
	// Every email_consent step is stored without a recipient: it goes to the address given
	// on the call, which the approval binds as "the address they agree to".
	var stored int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM outreach_message WHERE condition = 'email_consent' AND to_address <> ''`).Scan(&stored); err != nil || stored != 0 {
		t.Errorf("email_consent rows with a stored recipient = %d, %v; want 0", stored, err)
	}
	if strings.Contains(out, "dropped contoso.example") {
		t.Errorf("a form lead in a call-first country was dropped:\n%s", out)
	}
	for _, r := range rows("tailspin.example") {
		if r.name != "" {
			t.Errorf("GB lead step %d on sequence %q, want the default", r.step, r.name)
		}
	}

	mustRun(t, "leads", "batch", "approve", "1", "--hash", batchHash(t, 1))
	if _, err := owner.Exec(ctx, `UPDATE outreach_batch SET approved_at = $1`, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE outreach_message SET due_at = $2 WHERE id = $1`, roRows[0].id, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	out = mustRun(t, "leads", "calls", "outcome", fmt.Sprint(roRows[0].id), "connected", "--consent-address", "office@northwind.example")
	wantLines(t, "connected with consent", out, "Their agreement to get an email is recorded.")
	state := func(id int64) (st string, due bool) {
		t.Helper()
		var d *time.Time
		if err := owner.QueryRow(ctx, `SELECT state, due_at FROM outreach_message WHERE id = $1`, id).Scan(&st, &d); err != nil {
			t.Fatal(err)
		}
		return st, d != nil
	}
	if st, due := state(roRows[2].id); st != leadstore.MsgApproved || !due {
		t.Errorf("the consented email = %s (due %v), want approved and due", st, due)
	}
	if st, _ := state(roRows[1].id); st != leadstore.MsgCancelled {
		t.Errorf("the second call = %s, want cancelled (connected)", st)
	}
	mustRun(t, "leads", "suppress", "add", "domain", "northwind.example", "--reason", "opt_out")
	for _, r := range roRows[2:] {
		if st, _ := state(r.id); st != leadstore.MsgCancelled {
			t.Errorf("after the opt-out step %d = %s, want cancelled", r.step, st)
		}
	}
}
