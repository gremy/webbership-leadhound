//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestResearchCallFirstPhone is D5's research aim through the store, the CLI and the console,
// under a registered campaign with a call step: a Romanian lead (email needs a yes on a call)
// that has an email but no phone looks for a business phone, and the star note, the star's
// request, the lead page's "Look again" and the CLI all say so; the same lead with a phone to
// dial gets nothing; a GB lead with an email gets nothing; a German contact-form lead looks
// for a phone because this campaign calls Germany with the written reason the law asks for.
func TestResearchCallFirstPhone(t *testing.T) {
	callFirstTable(t)
	ctx := context.Background()
	dir := leadsDB(t)
	t.Setenv("LEADHOUND_OUTREACH", "")
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	saved := leadsNow
	leadsNow = func() time.Time { return now }
	t.Cleanup(func() { leadsNow = saved })

	rewrite := func(path string, pairs ...string) {
		t.Helper()
		raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temp copy
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		for i := 0; i < len(pairs); i += 2 {
			if !strings.Contains(s, pairs[i]) {
				t.Fatalf("%s has no %q", filepath.Base(path), pairs[i])
			}
			s = strings.Replace(s, pairs[i], pairs[i+1], 1)
		}
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil { // #nosec G703 -- a test temp dir
			t.Fatal(err)
		}
	}
	rewrite(filepath.Join(dir, "leads.yaml"),
		"  DE: {channel: form,", "  RO: {channel: email, lang: en, tz: Europe/Bucharest, footer: footer-en}\n  DE: {channel: form,")
	cdir := filepath.Join(dir, campaign.Dir)
	if err := os.CopyFS(cdir, os.DirFS("../../examples/campaigns")); err != nil {
		t.Fatal(err)
	}
	rewrite(filepath.Join(cdir, "drupal-eol", campaign.FileName),
		"  DE: {email: off, call: off}", "  RO: {email: on, call: on, lang: en}\n  DE: {email: off, call: on, lang: en, acknowledge: \"their sites run the version we support\"}",
		"templates_dir: templates", `sequences:
  call_first:
    - {step: 0, type: call}
    - {step: 1, type: call, after_business_days: 2, when: call_not_connected}
    - {step: 2, type: email, when: email_consent}
    - {step: 3, type: email, after_business_days: 4, when: email_consent}
templates_dir: templates`)
	batchFixture(t)
	owner := storetest.OwnerConn(t)
	if _, err := owner.Exec(ctx, `UPDATE org SET country = 'RO' WHERE domain IN ('northwind.example', 'litware.example')`); err != nil {
		t.Fatal(err)
	}
	orgPhone(t, "litware.example", "+40215550101")
	mustRun(t, "leads", "campaigns", "register")
	mustRun(t, "leads", "build")
	ro, roPhone, gb, de := leadIDOf(t, "northwind.example"), leadIDOf(t, "litware.example"), leadIDOf(t, "tailspin.example"), leadIDOf(t, "contoso.example")
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM lead WHERE id IN (%d, %d, %d) AND channel = 'email'`, ro, roPhone, gb)); n != 3 {
		t.Fatalf("%d of the RO and GB leads are email leads, want 3", n)
	}

	// The console: the star note and "Look again" read the decision the star's request applies.
	b := leadsBackend(t)
	for id, want := range map[int64]struct {
		note string
		ask  bool
	}{ro: {console.StarNotePhone, true}, roPhone: {"", false}, gb: {"", false}, de: {starNoteFindPhone, true}} {
		d, err := b.LeadDetail(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if d.StarNote != want.note || d.Research.CanAsk != want.ask {
			t.Errorf("lead %d: star note %q, Look again %v; want %q, %v", id, d.StarNote, d.Research.CanAsk, want.note, want.ask)
		}
	}
	page, err := b.LeadList(ctx, console.LeadListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range page.Rows {
		switch r.LeadID {
		case ro:
			seen++
			if r.StarNote != console.StarNotePhone {
				t.Errorf("the list's star note for the RO lead is %q", r.StarNote)
			}
		case roPhone:
			seen++
			if r.StarNote != "" {
				t.Errorf("the list's star note for the RO lead with a phone is %q", r.StarNote)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("the list shows %d of the two RO leads", seen)
	}
	out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadStar, ID: ro})
	if err != nil || !strings.HasSuffix(out.Message, "Its country takes calls first, so a search for a business phone is queued for the research job.") {
		t.Errorf("star the RO lead: %q %v", out.Message, err)
	}
	if out, err = b.LeadAct(ctx, console.LeadAction{Do: console.LeadStar, ID: roPhone}); err != nil || strings.Contains(out.Message, "search") {
		t.Errorf("star the RO lead with a phone: %q %v", out.Message, err)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_research WHERE lead_id = %d AND trigger = 'star' AND want = '{phone}'`, ro)); n != 1 {
		t.Errorf("%d star phone searches for the RO lead, want 1", n)
	}

	// The CLI.
	for _, c := range []struct {
		id   int64
		want string
	}{
		{roPhone, "no request: litware.example: it already has a phone to call"},
		{gb, "no request: tailspin.example: it already has an email to write to"},
		{de, "queued for contoso.example: it looks for a business phone"},
	} {
		if out := mustRun(t, "leads", "research", "request", strconv.FormatInt(c.id, 10)); !strings.Contains(out, c.want) {
			t.Errorf("lead %d:\n%s\nwant %q", c.id, out, c.want)
		}
	}
	if n := dbCount(t, `SELECT count(*) FROM contact_research`); n != 2 {
		t.Errorf("%d attempts, want the RO star and the DE request", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM contact_research WHERE want <> '{phone}'`); n != 0 {
		t.Errorf("%d attempts look for something other than a phone", n)
	}
}
