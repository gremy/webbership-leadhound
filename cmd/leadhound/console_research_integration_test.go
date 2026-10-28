//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
)

// researchRequestFile is the console's research request file under the test's data dir.
func researchRequestFile(t *testing.T, b *backend) string {
	t.Helper()
	setup, err := b.leadsSetup(context.Background(), leadsForProbe)
	if err != nil {
		t.Fatal(err)
	}
	if setup.dataDir == "" {
		t.Fatal("the test has no data dir")
	}
	return filepath.Join(setup.dataDir, runRequestDir, researchAction)
}

// finishResearch claims the queued attempt and finishes it with status.
func finishResearch(t *testing.T, status string) {
	t.Helper()
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	rows, err := lq.ClaimResearch(ctx, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim: %v, %d rows", err, len(rows))
	}
	if err := lq.FinishResearch(ctx, rows[0].ID, status, "fake", nil, 0, 0); err != nil {
		t.Fatal(err)
	}
}

// TestResearchConsoleStar: starring a new lead with no usable route queues one research
// attempt (an email in an email country) and writes the request file the .path unit
// watches, mode 0600; starring it again queues nothing more; a lead with a route gets no
// attempt; a lead in a contact-form country of a campaign that makes no calls (the legacy
// campaign) gets none either; the star control's note says so up front; a request file
// that cannot be written never undoes the star. (The phone search a call-first lead gets
// is TestResearchCallFirstPhone's.)
func TestResearchConsoleStar(t *testing.T) {
	researchScene(t)
	addFormCountry(t, "RO", "Europe/Bucharest")
	researchSite(t, "northwind.example", "GB", "", false)
	researchSite(t, "contoso.example", "RO", "", false)
	researchSite(t, "fabrikam.example", "GB", "contacts@1", true)
	researchSite(t, "lucerne.example", "GB", "", false)
	researchSite(t, "blocked.example", "DE", "", false)
	mustRun(t, "leads", "build")
	ctx := context.Background()
	b := leadsBackend(t)
	gb, de, routed := leadIDOf(t, "northwind.example"), leadIDOf(t, "contoso.example"), leadIDOf(t, "fabrikam.example")
	blocked := leadIDOf(t, "blocked.example")

	// D5: a contact-form country (RO made one here, DE) of a campaign that makes no calls
	// offers no phone search, and the star asks for none.
	for id, want := range map[int64]string{gb: console.StarNoteEmail, de: starNoteNoCalls, routed: "", blocked: starNoteNoCalls} {
		d, err := b.LeadDetail(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if d.StarNote != want {
			t.Errorf("lead %d star note %q, want %q", id, d.StarNote, want)
		}
	}
	page, err := b.LeadList(ctx, console.LeadListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Rows {
		if r.LeadID == gb && r.StarNote != console.StarNoteEmail {
			t.Errorf("the list's star note for %d is %q", gb, r.StarNote)
		}
	}

	file := researchRequestFile(t, b)
	out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadStar, ID: gb})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "starred") || !strings.HasSuffix(out.Message, ": it is last in your order. It has no email yet, so a search for one is queued for the research job.") {
		t.Errorf("star: %q", out.Message)
	}
	st, err := os.Stat(file)
	if err != nil {
		t.Fatalf("no request file: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("request file mode %v, want 0600", st.Mode().Perm())
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_research WHERE lead_id = %d AND status = 'queued' AND trigger = 'star' AND want = '{email}'`, gb)); n != 1 {
		t.Errorf("%d queued star attempts for the GB lead, want 1", n)
	}
	if out, err = b.LeadAct(ctx, console.LeadAction{Do: console.LeadStar, ID: gb}); err != nil || strings.Contains(out.Message, "search") {
		t.Errorf("star again: %q %v", out.Message, err)
	}
	if out, err = b.LeadAct(ctx, console.LeadAction{Do: console.LeadStar, ID: de}); err != nil || strings.Contains(out.Message, "search") {
		t.Errorf("star a form-country RO lead of a campaign with no calls: %q %v", out.Message, err)
	}
	if out, err = b.LeadAct(ctx, console.LeadAction{Do: console.LeadStar, ID: blocked}); err != nil || strings.Contains(out.Message, "search") {
		t.Errorf("star a DE lead of a campaign with no calls: %q %v", out.Message, err)
	}
	if out, err = b.LeadAct(ctx, console.LeadAction{Do: console.LeadStar, ID: routed}); err != nil || strings.Contains(out.Message, "search") {
		t.Errorf("star a lead with a route: %q %v", out.Message, err)
	}
	if n := dbCount(t, `SELECT count(*) FROM contact_research`); n != 1 {
		t.Errorf("%d attempts, want the GB one", n)
	}
	d, err := b.LeadDetail(ctx, gb)
	if err != nil {
		t.Fatal(err)
	}
	if r := d.Research; r.Status != console.ResearchQueued || !r.Open() || !r.CanAsk || r.At == nil || r.NextAt != nil {
		t.Errorf("the GB lead's research: %+v", r)
	}

	// A request file that cannot be written: the star and its attempt stand, and it says the
	// job picks it up on its next run.
	run := filepath.Dir(file)
	if err := os.RemoveAll(run); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(run, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(run) })
	lucerne := leadIDOf(t, "lucerne.example")
	out, err = b.LeadAct(ctx, console.LeadAction{Do: console.LeadStar, ID: lucerne})
	if err != nil || !strings.Contains(out.Message, "It runs on the research job's next run.") {
		t.Errorf("star with no request file: %q %v", out.Message, err)
	}
	if d, err := b.LeadDetail(ctx, lucerne); err != nil || !d.Starred || d.Research.Status != console.ResearchQueued {
		t.Errorf("the star or its attempt did not stand: %+v %v", d.Research, err)
	}
}

// TestResearchConsoleLookAgain: "Look again" after an attempt that found nothing is refused
// with the date it may run, and the page says that date; after a capped attempt (which
// called nothing) it queues one and writes the request file; a lead that is not there is
// refused; driven through the real handler, the refusal shows on the lead's page.
func TestResearchConsoleLookAgain(t *testing.T) {
	researchScene(t, "northwind.example", "contoso.example")
	ctx := context.Background()
	b := leadsBackend(t)
	nothing, capped := leadIDOf(t, "northwind.example"), leadIDOf(t, "contoso.example")

	if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadStar, ID: nothing}); err != nil {
		t.Fatal(err)
	}
	finishResearch(t, leadstore.ResearchNothing)
	_, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLookAgain, ID: nothing})
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "Not looked again: research last ran on") ||
		!strings.Contains(err.Error(), "it may run again from") {
		t.Errorf("look again inside 90 days: %v", err)
	}
	d, err := b.LeadDetail(ctx, nothing)
	if err != nil {
		t.Fatal(err)
	}
	if r := d.Research; r.Status != console.ResearchNothing || r.NextAt == nil || r.At == nil || r.Open() {
		t.Errorf("after nothing: %+v", r)
	}

	c := newConsoleClient(t, b, nil)
	path := fmt.Sprintf("/leads/%d", nothing)
	w := c.post(path+"/look-again", url.Values{"back": {path + "#contact-research"}})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "it may run again from") ||
		!strings.Contains(w.Body.String(), "It can look again from") {
		t.Errorf("look again through the console: %d\n%s", w.Code, w.Body.String())
	}

	if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadStar, ID: capped}); err != nil {
		t.Fatal(err)
	}
	finishResearch(t, leadstore.ResearchCapped)
	file := researchRequestFile(t, b)
	_ = os.Remove(file)
	out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLookAgain, ID: capped})
	if err != nil || !strings.Contains(out.Message, "Looking again") {
		t.Fatalf("look again after capped: %q %v", out.Message, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("no request file after look again: %v", err)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_research WHERE lead_id = %d AND status = 'queued' AND trigger = 'button'`, capped)); n != 1 {
		t.Errorf("%d queued button attempts, want 1", n)
	}
	if out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLookAgain, ID: capped}); err != nil || !strings.Contains(out.Message, "already waiting or running") {
		t.Errorf("look again while queued: %q %v", out.Message, err)
	}
	if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLookAgain, ID: 999999}); !errors.Is(err, console.ErrUserFixable) ||
		!strings.Contains(err.Error(), "There is no lead 999999.") {
		t.Errorf("unknown lead: %v", err)
	}
}

// TestReportConsole: a probed Drupal site lead's page offers its report, and the Report link
// serves it as a file named after its domain; a lead that is not there is not found.
func TestReportConsole(t *testing.T) {
	researchScene(t, "northwind.example")
	ctx := context.Background()
	b := leadsBackend(t)
	id := leadIDOf(t, "northwind.example")
	d, err := b.LeadDetail(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Report.Ready {
		t.Fatalf("the report is not offered: %q", d.Report.Why)
	}
	rep, err := b.LeadReport(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rep.Filename, "upgrade-risk-northwind.example-") || !strings.HasSuffix(rep.Filename, ".html") ||
		!strings.Contains(string(rep.HTML), "Drupal 7") {
		t.Errorf("report %q: %d bytes", rep.Filename, len(rep.HTML))
	}
	if _, err := b.LeadReport(ctx, 999999); !errors.Is(err, console.ErrNotFound) {
		t.Errorf("unknown lead: %v", err)
	}
	c := newConsoleClient(t, b, nil)
	w := c.get(fmt.Sprintf("/leads/reports/%d", id), false)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
		w.Header().Get("Content-Disposition") != "attachment; filename="+rep.Filename {
		t.Errorf("report route: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Header().Get("Content-Disposition"))
	}
	if n := dbCount(t, `SELECT count(*) FROM contact_research`); n != 0 {
		t.Errorf("reading the report wrote %d research rows", n)
	}
}
