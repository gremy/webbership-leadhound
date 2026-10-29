//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/phones"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// cp5Digits is the GB fixture number's national digits: any copy of the number, in any
// format, holds them.
const cp5Digits = "1632960002"

// tablesHolding names every public table one of whose rows (as JSON) holds digits, and
// counts the http_fetch bodies holding them as text.
func tablesHolding(t *testing.T, digits string) (tables []string, bodies int) {
	t.Helper()
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	rows, err := owner.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' AND table_name NOT LIKE 'goose%' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		var hit bool
		// row_to_json writes a bytea as hex, so a body is searched as text below.
		if err := owner.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %q x WHERE row_to_json(x)::text LIKE '%%' || $1 || '%%')`, n), digits).Scan(&hit); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if hit {
			tables = append(tables, n)
		}
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM http_fetch WHERE position(convert_to($1, 'UTF8') in body) > 0`, digits).Scan(&bodies); err != nil {
		t.Fatal(err)
	}
	return tables, bodies
}

// TestForgetOrgLeavesNoTrace: `leads forget org` leaves none of the org's number anywhere:
// no table row and no kept page body holds its digits afterwards (its phone fact, its call
// note, its screenings, its research row and the page it was read from all held them
// before); the suppression holds a hash, by design. A second run counts nothing. Research on
// the org is refused afterwards: its domain is suppressed and its lead with it.
func TestForgetOrgLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	w := newCP5World(t)
	t.Setenv("LEADHOUND_MIGRATE_DATABASE_URL", storetest.MigrateDSN())
	owner := storetest.OwnerConn(t)
	w.screen(t, w.gbNumber, "tps", "clean", w.now.Add(-time.Hour))
	w.screen(t, w.gbNumber, "ctps", "clean", w.now.Add(-time.Hour))
	mustRun(t, "leads", "calls", "outcome", fmt.Sprint(w.gbCall), "no_answer", "--note", "rang 01632960002 twice")
	if _, err := leadstoretest.Q(t).RecordPhoneResearch(ctx, leadstore.PhoneResearchRow{OrgID: w.gbOrg, FactID: &w.gbFact,
		LineKind: leadstore.LineDirect, Role: "Office manager", Model: "claude-haiku-5-5"}); err != nil {
		t.Fatal(err)
	}
	var runID int64
	if err := owner.QueryRow(ctx, `SELECT max(id) FROM run`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	var fetchID int64
	if err := owner.QueryRow(ctx,
		`INSERT INTO http_fetch (run_id, source, kind, url, status, body) VALUES ($1, 'research', 'site', 'https://www.tailspin.example/contact', 200, $2) RETURNING id`,
		runID, []byte("<p>Call us: +441632960002</p>")).Scan(&fetchID); err != nil {
		t.Fatal(err)
	}
	if _, err := leadstoretest.Q(t).InsertFact(ctx, leadstore.FactRow{OrgID: w.gbOrg, Kind: leadstore.FactDescription,
		Value: "Tailspin, call 01632960002", Normalized: "Tailspin, call 01632960002", FetchID: fetchID, DerivedBy: "enrich@1"}); err != nil {
		t.Fatal(err)
	}
	if tables, bodies := tablesHolding(t, cp5Digits); len(tables) < 3 || bodies == 0 {
		t.Fatalf("the fixture does not spread the number: tables %v, bodies %d", tables, bodies)
	}

	out := mustRun(t, "leads", "forget", "org", fmt.Sprint(w.gbOrg), "--reason", "asked to be forgotten")
	wantLines(t, "forget", out, fmt.Sprintf("Org %d forgotten:", w.gbOrg), "1 phone fact", "1 call note blanked")
	if strings.Contains(out, cp5Digits) {
		t.Error("forget printed the number")
	}
	if tables, bodies := tablesHolding(t, cp5Digits); len(tables) > 0 || bodies > 0 {
		t.Errorf("after forget the number is still in tables %v and %d page bodies", tables, bodies)
	}
	if n := countRows(t, fmt.Sprintf(`SELECT count(*) FROM suppression WHERE kind = 'phone' AND value = '%s'`,
		leadstore.PhoneSuppressionValue(w.gbNumber))); n != 1 {
		t.Errorf("hashed phone suppressions = %d, want 1", n)
	}
	again := mustRun(t, "leads", "forget", "org", fmt.Sprint(w.gbOrg), "--reason", "re-applied after a restore")
	wantLines(t, "forget again", again, "nothing left to forget")

	_, _, err := leadstoretest.Q(t).RequestResearch(ctx, w.gbLead, leadstore.ResearchCLI, []string{leadstore.WantPhone}, leadstore.ActorOperator)
	if !errors.Is(err, leadstore.ErrResearchBlocked) {
		t.Errorf("research after forget: %v, want ErrResearchBlocked", err)
	}
	leads, err := callLeadSource{db: owner, lq: leadstoretest.Q(t), table: compliance.Default(), now: w.now}.CallLeadsAhead(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range leads {
		if l.OrgID == w.gbOrg {
			t.Error("phone research would still pick the forgotten org")
		}
	}
	code, _, _ := leadsRun(t, "leads", "forget", "org", fmt.Sprint(w.gbOrg), "--reason", "mail me at a@b.example")
	if code == 0 {
		t.Error("a reason with an address was taken")
	}
}

// countRows runs a count query on the owner's connection.
func countRows(t *testing.T, sql string) int {
	t.Helper()
	var n int
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// TestSuppressPhoneCommand: `leads suppress add phone` stores the number hashed (both its
// E.164 form and a stored national-form fact that dials as it), never in clear, refuses a
// number without its country code and a reason that is not a phone one, and the call it
// reaches is not callable after.
func TestSuppressPhoneCommand(t *testing.T) {
	w := newCP5World(t)
	w.screen(t, w.gbNumber, "tps", "clean", w.now.Add(-time.Hour))
	w.screen(t, w.gbNumber, "ctps", "clean", w.now.Add(-time.Hour))
	if s := w.surfaces(t); !s.card {
		t.Fatalf("baseline not callable: %+v", s)
	}
	for _, args := range [][]string{
		{"01632 960002", "--reason", "do_not_call"},
		{"+44 1632 960002", "--reason", "opt_out"},
		{"+44 1632 960002"},
	} {
		if code, out, _ := leadsRun(t, append([]string{"leads", "suppress", "add", "phone"}, args...)...); code == 0 {
			t.Errorf("%q taken: %s", args, out)
		}
	}
	out := mustRun(t, "leads", "suppress", "add", "phone", "+44 (0)1632 960002", "--reason", "do_not_call")
	wantLines(t, "suppress phone", out, "stored hashed")
	if strings.Contains(out, cp5Digits) {
		t.Error("the command printed the number")
	}
	if n := countRows(t, `SELECT count(*) FROM suppression WHERE kind = 'phone' AND value LIKE '%`+cp5Digits+`%'`); n != 0 {
		t.Error("a phone suppression holds the number in clear")
	}
	if n := countRows(t, fmt.Sprintf(`SELECT count(*) FROM suppression WHERE kind = 'phone' AND reason = 'do_not_call' AND value = '%s'`,
		leadstore.PhoneSuppressionValue(w.gbNumber))); n != 1 {
		t.Errorf("hashed suppression rows = %d, want 1", n)
	}
	s := w.surfaces(t)
	if s.card || s.today || s.list {
		t.Errorf("a suppressed number is callable: %+v", s)
	}
}

// TestPhonesScreeningGrid: the screening flow end to end through the commands. The export
// lists the GB number once, in E.164, to a new 0600 file outside the repository (never over
// one, never inside the checkout); importing clean makes the call callable and the next
// export empty; clean then listed later: listed wins and the number is suppressed; listed
// then clean: still suppressed (only the operator lifts a suppression).
func TestPhonesScreeningGrid(t *testing.T) {
	w := newCP5World(t)
	tmp := t.TempDir()
	exportTo := func(name string) string {
		t.Helper()
		p := filepath.Join(tmp, name)
		mustRun(t, "leads", "phones", "screen", "export", "--register", "tps", "--out", p)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", name, fi.Mode().Perm())
		}
		b, err := os.ReadFile(p) // #nosec G304 -- the test's own temp file
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := exportTo("tps-1.csv"); got != "number\n"+w.gbNumber+"\n" {
		t.Errorf("first export = %q", got)
	}
	if code, _, _ := leadsRun(t, "leads", "phones", "screen", "export", "--register", "tps", "--out", filepath.Join(tmp, "tps-1.csv")); code == 0 {
		t.Error("the export overwrote a file")
	}
	if code, _, _ := leadsRun(t, "leads", "phones", "screen", "export", "--register", "tps", "--out", "inside-the-repo.csv"); code == 0 {
		_ = os.Remove("inside-the-repo.csv")
		t.Error("the export wrote inside the repository")
	}
	importFile := func(register, name, body string) string {
		t.Helper()
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return mustRun(t, "leads", "phones", "screen", "import", "--register", register, p)
	}
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	out := importFile("tps", "tps-clean.csv", "number,status,checked\n01632 960002,not listed,"+day+"\nnot-a-number,clean,"+day+"\n")
	wantLines(t, "import clean", out, "recorded 1 screening (1 clean, 0 listed)", "1 line refused (line 3)")
	importFile("ctps", "ctps-clean.csv", "number,status,checked\n+441632960002,clean,"+day+"\n")
	if s := w.surfaces(t); !s.card || !s.today || !s.list {
		t.Errorf("screened clean on both registers, not callable: %+v", s)
	}
	if got := exportTo("tps-2.csv"); got != "number\n" {
		t.Errorf("export after a fresh clean = %q, want no numbers", got)
	}
	later := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	out = importFile("tps", "tps-listed.csv", "number,status,checked\n+441632960002,listed,"+later+"\n")
	wantLines(t, "import listed", out, "1 listed", "1 new phone suppression")
	if s := w.surfaces(t); s.card || s.today || s.list {
		t.Errorf("listed later, still callable: %+v", s)
	}
	importFile("tps", "tps-clean-again.csv", "number,status\n+441632960002,clean\n")
	if s := w.surfaces(t); s.card || s.today || s.list || !strings.Contains(s.reason, "do-not-call") {
		t.Errorf("listed then clean: %+v, want still suppressed", s)
	}
	if n := countRows(t, `SELECT count(*) FROM suppression WHERE kind = 'phone' AND reason = 'tps'`); n != 1 {
		t.Errorf("tps suppressions = %d, want 1", n)
	}
}

// TestPhonesResearchSelection: phone research picks the campaign's leads with a call step
// ahead whose org has no direct line (a number of unknown kind counts as a switchboard),
// never a lead in a country whose calls the law table has off, and not again within 90 days
// of a phone research of the org (found or not). With live calls off the command does
// nothing and writes no research row.
func TestPhonesResearchSelection(t *testing.T) {
	callFirstTable(t)
	ctx := context.Background()
	w := newCP5World(t)
	owner := storetest.OwnerConn(t)
	src := callLeadSource{db: owner, lq: leadstoretest.Q(t), table: compliance.Default(), now: w.now}
	orgs := func() map[int64]bool {
		t.Helper()
		leads, err := src.CallLeadsAhead(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]bool{}
		for _, l := range phones.Select(leads, 0) {
			out[l.OrgID] = true
		}
		return out
	}
	if !orgs()[w.gbOrg] {
		t.Fatal("the GB lead with only an unknown-kind number is not picked")
	}
	out := mustRun(t, "leads", "phones", "research", "--campaign", "drupal-eol", "--max", "1")
	wantLines(t, "live off", out, phonesLiveOffWords)
	if n := countRows(t, `SELECT count(*) FROM phone_research`); n != 0 {
		t.Errorf("live off wrote %d research rows", n)
	}
	if _, err := owner.Exec(ctx, `UPDATE org SET country = 'AT' WHERE id = $1`, w.gbOrg); err != nil {
		t.Fatal(err)
	}
	if orgs()[w.gbOrg] {
		t.Error("a lead in a country with calls off by law is picked")
	}
	if _, err := owner.Exec(ctx, `UPDATE org SET country = 'GB' WHERE id = $1`, w.gbOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := leadstoretest.Q(t).RecordPhoneResearch(ctx, leadstore.PhoneResearchRow{OrgID: w.gbOrg, Model: "claude-haiku-5-5"}); err != nil {
		t.Fatal(err)
	}
	if orgs()[w.gbOrg] {
		t.Error("an org researched today is picked again")
	}
}
