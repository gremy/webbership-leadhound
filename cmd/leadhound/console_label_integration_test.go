//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
)

// latestCall is lead id's latest lead_label row: its band ("" when cleared) and input, as
// the owner reads them; ok is false with no row.
func latestCall(t *testing.T, id int64) (band string, input leadrate.Input, ok bool) {
	t.Helper()
	ls, err := leadstoretest.Q(t).LatestLabels(context.Background(), []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	l, ok := ls[id]
	if !ok || l.Band == nil {
		return "", leadrate.Input{}, ok
	}
	if err := json.Unmarshal(l.Input, &input); err != nil {
		t.Fatalf("call on %d: input %q: %v", id, l.Input, err)
	}
	return *l.Band, input, true
}

// rateInputOf is lead id's leadrate.Input exactly as leads rate builds it now: its
// LeadsToRate row through rateInput with the effective settings.
func rateInputOf(t *testing.T, b *backend, id int64) leadrate.Input {
	t.Helper()
	ctx := context.Background()
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		t.Fatal(err)
	}
	all, err := b.lq().LeadsToRate(ctx, leadstore.ListContext{})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(all, func(l leadstore.RateLead) bool { return l.LeadID == id })
	if i < 0 {
		t.Fatalf("lead %d is not open", id)
	}
	return rateInput(all[i], setup.leads)
}

// TestConsoleLabel: a call set on a lead's page through the console is stored with the
// lead's facts exactly as leads rate reads them, and the page shows it; a changed call wins
// over the earlier one, and a later change of the settings is in the next call's facts, not
// the earlier one's; Clear clears it, and a second Clear changes nothing; a band that is not
// one and a lead that is not there are refused with nothing written.
func TestConsoleLabel(t *testing.T) {
	orderScene(t)
	ctx := context.Background()
	b := leadsBackend(t)
	c := newConsoleClient(t, b, nil)
	id := leadIDOf(t, "lucerne.example")
	path := fmt.Sprintf("/leads/%d", id)

	w := c.post(path+"/label", url.Values{"band": {"likely"}, "back": {path}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != path+"#your-call" {
		t.Fatalf("label: %d to %q\n%s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	page := c.pages[len(c.pages)-1]
	// The outcome names the lead by its domain and shows inside the call block, which says the
	// count itself.
	if i, j := strings.Index(page, `id="your-call"`), strings.Index(page, `<p class="notice call-notice" role="status">Saved your call on lucerne.example: likely.</p>`); i < 0 || j < i {
		t.Errorf("the call's outcome is not in the call block: %d %d", i, j)
	}
	for _, want := range []string{
		"You said <strong>likely</strong>", `value="likely" title=`, `class="btn btn-secondary is-current" aria-current="true">Likely</button>`,
		`value="clear" class="btn btn-ghost call-clear">Clear</button>`, "1 of 30 labelled"} {
		if !strings.Contains(page, want) {
			t.Errorf("the lead's page after the call lacks %q", want)
		}
	}
	band, in, ok := latestCall(t, id)
	if !ok || band != "likely" {
		t.Fatalf("stored call %q %v, want likely", band, ok)
	}
	first := rateInputOf(t, b, id)
	if !reflect.DeepEqual(in, first) || in.Kind == "" || in.EOL.IsZero() || len(in.Channels) == 0 {
		t.Errorf("the call's input is %+v, want what leads rate reads, %+v", in, first)
	}

	saveRateSettings(t, `{"facts":{"d7_eol":"2025-02-01"}}`)
	if w := c.post(path+"/label", url.Values{"band": {"no"}, "back": {path}}); w.Code != http.StatusSeeOther {
		t.Fatalf("second call: %d", w.Code)
	}
	band, in, _ = latestCall(t, id)
	if second := rateInputOf(t, b, id); band != "no" || !reflect.DeepEqual(in, second) || in.EOL.Equal(first.EOL) {
		t.Errorf("after a change: %q %+v, want no on the new facts %+v", band, in, second)
	}
	ls, err := b.lq().LabelledLeads(ctx)
	if err != nil || len(ls) != 1 || *ls[0].Band != "no" {
		t.Errorf("labelled set %+v %v, want lucerne as no", ls, err)
	}

	if w := c.post(path+"/label", url.Values{"band": {"clear"}, "back": {path}}); w.Code != http.StatusSeeOther {
		t.Fatalf("clear: %d", w.Code)
	}
	page = c.pages[len(c.pages)-1]
	if !strings.Contains(page, "Your call on lucerne.example is cleared.") || strings.Contains(page, "leads labelled.") || !strings.Contains(page, "No call yet.") {
		t.Errorf("after Clear the page reads:\n%s", page)
	}
	if band, _, ok := latestCall(t, id); !ok || band != "" {
		t.Errorf("after Clear: %q %v, want a cleared call", band, ok)
	}
	rows := func() int { return dbCount(t, `SELECT count(*) FROM lead_label`) }
	if n := rows(); n != 3 {
		t.Errorf("%d rows, want 3", n)
	}
	out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLabel, ID: id, Value: console.LabelClear})
	if err != nil || out.Message != "lucerne.example has no call to clear; nothing changed." || rows() != 3 {
		t.Errorf("second clear: %q %v, %d rows", out.Message, err, rows())
	}

	for _, a := range []console.LeadAction{
		{Do: console.LeadLabel, ID: id, Value: "great"},
		{Do: console.LeadLabel, ID: id, Value: ""},
		{Do: console.LeadLabel, ID: 999999, Value: "likely"},
	} {
		if _, err := b.LeadAct(ctx, a); !errors.Is(err, console.ErrUserFixable) {
			t.Errorf("%+v: %v, want a refusal", a, err)
		}
	}
	if w := c.post(path+"/label", url.Values{"band": {"great"}, "back": {path}}); w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), "Choose strong, likely, maybe, unlikely or no, or Clear; your call was not changed.") {
		t.Errorf("bad band through the console: %d", w.Code)
	}
	if n := rows(); n != 3 {
		t.Errorf("%d rows after the refusals, want still 3", n)
	}
}

// TestRelevanceEvalFromCalls: with no --golden the eval reads the operator's calls from the
// database: the latest call per lead that names a band, on the facts it was made on. With no
// call it refuses in plain words; a fake eval prints its report on the labelled leads and
// records nothing.
func TestRelevanceEvalFromCalls(t *testing.T) {
	orderScene(t)
	b := leadsBackend(t)
	ctx := context.Background()
	if code, _, errOut := leadsRun(t, "leads", "relevance", "eval", "--runs", "1"); code != 2 || !strings.Contains(errOut, noCallsWords) {
		t.Fatalf("eval with no call: exit %d, %q", code, errOut)
	}
	call := func(domain, band string) {
		t.Helper()
		if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLabel, ID: leadIDOf(t, domain), Value: band}); err != nil {
			t.Fatal(err)
		}
	}
	call("lucerne.example", "maybe")
	call("margie.example", "likely")
	call("woodgrove.example", "strong")
	call("woodgrove.example", console.LabelClear) // cleared: not in the eval
	call("cpandl.example", "no")
	call("cpandl.example", "maybe") // changed: counts once, as maybe

	out := mustRun(t, "leads", "relevance", "eval", "--runs", "1")
	wantLines(t, "eval from the calls", out,
		"relevance eval: 3 labelled lead(s), 0 unlabelled skipped · question relevance@1 · decider fake · 1 run(s)",
		"exact band: 67% · within one band: 100%", "not recorded: only an eval with --decider jev measures the number the list shows")
	if n := dbCount(t, `SELECT count(*) FROM lead_relevance_eval`); n != 0 {
		t.Errorf("%d evals recorded from a fake eval", n)
	}
	// The golden file still works, for the demo set.
	out = mustRun(t, "leads", "relevance", "eval", "--golden", filepath.Join(examplesDir, "leads-relevance.demo.jsonl"), "--runs", "1")
	wantLines(t, "eval of the demo set", out, "relevance eval: 7 labelled lead(s), 1 unlabelled skipped")
}

// labelSites are forty reachable leads over four Drupal majors, for the To label filter.
func labelSites() []struct {
	domain string
	major  int
} {
	var out []struct {
		domain string
		major  int
	}
	for i := range 40 {
		out = append(out, struct {
			domain string
			major  int
		}{fmt.Sprintf("label%02d.example", i), []int{7, 8, 9, 10}[i%4]})
	}
	return out
}

// TestConsoleToLabel: /leads?to_label=1 lists the thirty open leads a golden draft would pick,
// spread over the Drupal majors, plus every lead with a call outside them; a lead whose call
// was cleared leaves the list unless it was picked; the page counts the calls.
func TestConsoleToLabel(t *testing.T) {
	saved := orderSites
	orderSites = labelSites()
	t.Cleanup(func() { orderSites = saved })
	orderScene(t)
	ctx := context.Background()
	b := leadsBackend(t)
	c := newConsoleClient(t, b, nil)
	list := func() []string {
		t.Helper()
		w := c.get("/leads?to_label=1", false)
		if w.Code != http.StatusOK {
			t.Fatalf("/leads?to_label=1: %d", w.Code)
		}
		return listedLeads(w.Body.String())
	}

	open, err := b.lq().LeadsToRate(ctx, leadstore.ListContext{})
	if err != nil || len(open) != 40 {
		t.Fatalf("%d open leads, %v; want 40", len(open), err)
	}
	var sample []string
	majors := map[int]bool{}
	for _, l := range spreadSample(open, 30) {
		sample = append(sample, fmt.Sprint(l.LeadID))
		majors[l.Major] = true
	}
	got := list()
	slices.Sort(got)
	slices.Sort(sample)
	if !slices.Equal(got, sample) || len(got) != 30 || len(majors) != 4 {
		t.Fatalf("to_label lists %v, want the 30 spread leads %v over 4 majors (%d)", got, sample, len(majors))
	}

	var outside []int64
	for _, l := range open {
		if !slices.Contains(sample, fmt.Sprint(l.LeadID)) {
			outside = append(outside, l.LeadID)
		}
	}
	inside := open[slices.IndexFunc(open, func(l leadstore.RateLead) bool { return slices.Contains(sample, fmt.Sprint(l.LeadID)) })].LeadID
	for _, id := range []int64{outside[0], outside[1], inside} {
		if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLabel, ID: id, Value: "maybe"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := list(); len(got) != 32 || !slices.Contains(got, fmt.Sprint(outside[0])) || !slices.Contains(got, fmt.Sprint(outside[1])) {
		t.Errorf("with two calls outside the sample, to_label lists %d leads %v, want 32", len(got), got)
	}
	page := c.pages[len(c.pages)-1]
	for _, want := range []string{"<strong>3 of 30 labelled.</strong>", `<th scope="col">Your call</th>`, `<span class="badge badge-accent">maybe</span>`} {
		if !strings.Contains(page, want) {
			t.Errorf("to_label page lacks %q", want)
		}
	}
	for _, id := range []int64{outside[0], inside} {
		if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLabel, ID: id, Value: console.LabelClear}); err != nil {
			t.Fatal(err)
		}
	}
	if got := list(); len(got) != 31 || slices.Contains(got, fmt.Sprint(outside[0])) || !slices.Contains(got, fmt.Sprint(inside)) {
		t.Errorf("after clearing one call outside and one inside, to_label lists %d leads, want 31 without %d", len(got), outside[0])
	}
	if n := dbCount(t, `SELECT count(*) FROM lead_label WHERE actor = 'operator'`); n != 5 {
		t.Errorf("%d calls stored, want 5", n)
	}
}
