//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Every handle, name and link here is invented (example/exemplu handles, .example hosts).

// intentLineRE is one lead line of leads intent report.
var intentLineRE = regexp.MustCompile(`^  lead \d+ · [^·]+ · [^·]+ · (@\S+|no Instagram) · overlap \d+ · [^·]+ · (\d{1,2} [A-Z][a-z]{2} \d{4}|no date)$`)

// intentOf is lead id's effective intent now.
func intentOf(t *testing.T, id int64) leadstore.IntentRow {
	t.Helper()
	m, err := leadstoretest.Q(t).LatestIntents(context.Background(), []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	return m[id]
}

// intentBlocks is the report's block headers in the order printed, as class names.
func intentBlocks(out string) []string {
	var got []string
	for _, l := range strings.Split(out, "\n") {
		for _, c := range leadstore.IntentClasses {
			if strings.HasPrefix(l, c+" (") {
				got = append(got, c)
			}
		}
	}
	return got
}

// TestLeadsIntentImportSyncsAndReport: an import ends with the intent line after the place
// types; a match does too; the report counts every class, prints its blocks in rank order and
// each line in the one shape, naming the lead's handle.
func TestLeadsIntentImportSyncsAndReport(t *testing.T) {
	s := newIGScene(t)
	igSceneFake(t)
	out := igImport(t, igSceneLines(""))
	place := strings.Index(out, "  place types from Instagram: ")
	in := strings.Index(out, "  intent: shopping ")
	if place < 0 || in < place || !strings.Contains(out[in:], "(changed ") {
		t.Fatalf("the import's intent line is missing or before the place types:\n%s", out)
	}
	if n := dbCount(t, `SELECT count(*) FROM lead_intent`); n == 0 {
		t.Errorf("the import wrote no lead_intent row:\n%s", out)
	}
	if strings.Contains(out, "run leads intent sync") {
		t.Errorf("a whole import printed the catch-up line:\n%s", out)
	}

	m := mustRun(t, "leads", "ig", "match", "cafeneaua.exemplu", fmt.Sprint(s.C))
	wantLines(t, "match", m, "leads ig match: @cafeneaua.exemplu is lead", "  intent: shopping ")

	rep := mustRun(t, "leads", "intent", "report")
	wantLines(t, "report", rep, "leads intent report: campaign cafes-loyalty, 4 café leads")
	total := 0
	for _, c := range leadstore.IntentClasses {
		var n int
		if _, err := fmt.Sscanf(rep[strings.Index(rep, "  "+c+": "):], "  "+c+": %d", &n); err != nil {
			t.Fatalf("no count for %s: %v\n%s", c, err, rep)
		}
		total += n
	}
	if total != 4 {
		t.Errorf("the counts add up to %d, want 4:\n%s", total, rep)
	}
	blocks := intentBlocks(rep)
	if len(blocks) == 0 || !slices.IsSortedFunc(blocks, func(a, b string) int {
		return slices.Index(leadstore.IntentClasses, a) - slices.Index(leadstore.IntentClasses, b)
	}) {
		t.Errorf("blocks %v are not in rank order:\n%s", blocks, rep)
	}
	lines := 0
	for _, l := range strings.Split(rep, "\n") {
		if strings.HasPrefix(l, "  lead ") {
			lines++
			if !intentLineRE.MatchString(l) {
				t.Errorf("report line %q is not lead · name · town · @handle · overlap · reason · date", l)
			}
		}
	}
	if lines != 4 {
		t.Errorf("%d lead lines, want 4:\n%s", lines, rep)
	}
	if !strings.Contains(rep, fmt.Sprintf("  lead %d · Harbour Lane Coffee · ", s.A)) || !strings.Contains(rep, "· @harbour_lane.example · ") {
		t.Errorf("lead A's line does not name it and its handle:\n%s", rep)
	}
}

// TestLeadsIntentSyncVerb: --dry-run says what it would write and writes nothing; the sync
// writes it; a rerun changes nothing.
func TestLeadsIntentSyncVerb(t *testing.T) {
	newIGScene(t)
	igSceneFake(t)
	igImport(t, igSceneLines(""))
	written := dbCount(t, `SELECT count(*) FROM lead_intent`)
	if _, err := storetest.OwnerConn(t).Exec(context.Background(), `DELETE FROM lead_intent`); err != nil {
		t.Fatal(err)
	}
	dry := mustRun(t, "leads", "intent", "sync", "--dry-run")
	wantLines(t, "dry sync", dry, "leads intent sync: campaign cafes-loyalty, 4 café leads; no model called",
		fmt.Sprintf("(would change %d; nothing written)", written))
	if n := dbCount(t, `SELECT count(*) FROM lead_intent`); n != 0 {
		t.Errorf("a dry sync wrote %d rows", n)
	}
	wantLines(t, "sync", mustRun(t, "leads", "intent", "sync", "--campaign", "cafes-loyalty"), fmt.Sprintf("(changed %d)", written))
	wantLines(t, "rerun", mustRun(t, "leads", "intent", "sync"), "(changed 0)")
}

// TestLeadsIntentSetClear: set out is the operator's call, the report shows it as his; clear
// brings the derived class back. Set refuses a non-café lead, an unknown class and an unknown
// lead, writing nothing.
func TestLeadsIntentSetClear(t *testing.T) {
	s := newIGScene(t)
	igSceneFake(t)
	igImport(t, igSceneLines(""))
	before := intentOf(t, s.A)

	out := mustRun(t, "leads", "intent", "set", fmt.Sprint(s.A), "out", "--note", "chose a rival example")
	wantLines(t, "set", out, fmt.Sprintf("leads intent set: lead %d is out now (your call)", s.A), "  intent: shopping ")
	rep := mustRun(t, "leads", "intent", "report")
	outBlock := rep[strings.Index(rep, "\nout ("):]
	if !strings.Contains(outBlock, fmt.Sprintf("  lead %d · Harbour Lane Coffee · ", s.A)) || !strings.Contains(outBlock, " · your call: chose a rival example · ") {
		t.Errorf("the out block does not show lead A as your call:\n%s", rep)
	}

	out = mustRun(t, "leads", "intent", "clear", fmt.Sprint(s.A))
	wantLines(t, "clear", out, fmt.Sprintf("leads intent clear: lead %d is %s now (derived)", s.A, before.Class))
	if got := intentOf(t, s.A); got.Yours || got.Class != before.Class {
		t.Errorf("after clear: %+v, want the derived %s", got, before.Class)
	}

	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	var org, drupal int64
	if err := owner.QueryRow(ctx, `INSERT INTO org (kind, domain, source) VALUES ('site_owner', 'drupal-intent.example', 'probe') RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `INSERT INTO lead (org_id, kind, channel, state, campaign_id) VALUES ($1, 'hand', 'none', 'new', 1) RETURNING id`, org).Scan(&drupal); err != nil {
		t.Fatal(err)
	}
	calls := dbCount(t, `SELECT count(*) FROM lead_intent_call`)
	for _, c := range []struct {
		what string
		args []string
		want string
	}{
		{"a Drupal lead", []string{"set", fmt.Sprint(drupal), "out"}, fmt.Sprintf("lead %d is not a café lead", drupal)},
		{"a Drupal lead's clear", []string{"clear", fmt.Sprint(drupal)}, fmt.Sprintf("lead %d is not a café lead", drupal)},
		{"an unknown class", []string{"set", fmt.Sprint(s.A), "warm"}, `"warm" is not an intent class; say one of shopping, paper`},
		{"an unknown lead", []string{"set", "999999", "out"}, "there is no lead 999999"},
		{"an unknown lead's clear", []string{"clear", "999999"}, "there is no lead 999999"},
	} {
		code, _, errOut := leadsRun(t, append([]string{"leads", "intent"}, c.args...)...)
		if code == 0 || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: exit %d, %q; want a refusal saying %q", c.what, code, errOut, c.want)
		}
	}
	if n := dbCount(t, `SELECT count(*) FROM lead_intent_call`); n != calls {
		t.Errorf("the refusals wrote %d calls", n-calls)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM lead_intent WHERE lead_id = %d`, drupal)); n != 0 {
		t.Errorf("the Drupal lead has %d intent rows", n)
	}
}

// cancelDecider is the scene's Jev that interrupts the import on its first call, as Ctrl-C
// would mid-run.
type cancelDecider struct {
	*ai.Fake
	cancel context.CancelFunc
}

func (d cancelDecider) Decide(ctx context.Context, state string, qs []ai.Question) (map[string]ai.Answer, ai.Usage, error) {
	d.cancel()
	return d.Fake.Decide(ctx, state, qs)
}

// TestLeadsIntentImportInterrupted: an interrupted import syncs neither the place types nor
// the intent, and prints the intent's catch-up line beside the place types'.
func TestLeadsIntentImportInterrupted(t *testing.T) {
	newIGScene(t)
	f := igSceneFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	saved := igFake
	igFake = func() ai.Decider { return cancelDecider{Fake: f, cancel: cancel} }
	t.Cleanup(func() { igFake = saved })

	var out, errOut bytes.Buffer
	savedOut := stdout
	stdout = &out
	code := run(ctx, []string{"leads", "ig", "import", igFile(t, igSceneLines("")...), "--campaign", "cafes-loyalty"}, &errOut)
	stdout = savedOut
	if code == 0 {
		t.Errorf("an interrupted import exited 0:\n%s", out.String())
	}
	wantLines(t, "interrupted import", out.String(), "run leads ig place-types", "  loyalty intent: not synced; run leads intent sync")
	if n := dbCount(t, `SELECT count(*) FROM lead_intent`); n != 0 {
		t.Errorf("an interrupted import wrote %d intent rows", n)
	}
}
