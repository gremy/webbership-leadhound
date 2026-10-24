package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/probe"
)

func TestLeadsSitesIsRegistered(t *testing.T) {
	if _, ok := leadsCommands["sites"]; !ok {
		t.Fatal(`leadsCommands["sites"] is not registered`)
	}
}

func TestLeadsSitesUsage(t *testing.T) {
	tests := [][]string{
		{},
		{"nope"},
		{"import"},
		{"import", "majestic"},
		{"import", "majestic", "a.csv", "b.csv"},
		{"import", "alexa", "a.csv"},
		{"probe", "extra"},
		{"probe", "--max", "0"},
		{"probe", "--dry-run"},
		{"probe", "--domain", "192.0.2.10"},
		{"eval"},
		{"eval", "--labels", ""},
	}
	for _, args := range tests {
		err := runLeadsSites(context.Background(), args)
		if !errors.Is(err, errUsage) {
			t.Errorf("sites %q: err = %v, want a usage error", args, err)
		}
	}
}

const labelsCSV = "domain,final_url,major,inferred,exact_version,has_contact,title,evidence\n" +
	"northwind.example,https://northwind.example/,7,,,,,\n" +
	"contoso.example,https://contoso.example/,10,,10.6.14,,,\n" +
	"fabrikam.example,https://fabrikam.example/,10,,,,,\n" +
	"tailspin.example,https://tailspin.example/,,10.1+,,,,\n" +
	"wingtip.example,https://wingtip.example/,11,,,,,\n" +
	"adatum.example,,10,,,,,\n" +
	"litware.example,https://litware.example/,none,,,,,\n" +
	"proseware.example,https://proseware.example/,9,,,,,\n"

func TestReadLabels(t *testing.T) {
	got, err := readLabels(strings.NewReader("\ufeff" + labelsCSV))
	if err != nil {
		t.Fatal(err)
	}
	want := []siteLabel{
		{"northwind.example", true, 7, true},
		{"contoso.example", true, 10, true},
		{"fabrikam.example", true, 10, true},
		{"tailspin.example", true, 0, true},
		{"wingtip.example", true, 11, true},
		{"adatum.example", true, 10, false},
		{"litware.example", false, 0, true},
		{"proseware.example", true, 9, true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d labels, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("label %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"site,major\nx.example,7\n", "domain,major\nx.example,seven\n"} {
		if _, err := readLabels(strings.NewReader(bad)); err == nil {
			t.Errorf("readLabels(%q): no error", bad)
		}
	}
}

// evalRows is the latest probe of each fixture site, as EvalRows returns them.
var evalRows = []leadstore.EvalRow{
	{Domain: "adatum.example", Outcome: leadstore.OutcomeRobotsDisallowed},
	{Domain: "contoso.example", Outcome: leadstore.OutcomeOK, IsDrupal: true, Major: 10, ExactVersion: "10.6.14"},
	{Domain: "fabrikam.example", Outcome: leadstore.OutcomeOK, IsDrupal: true, Major: 11,
		Signals: []leadstore.Signal{{Signal: "meta-generator", Value: "11", FetchID: 5}, {Signal: "drupalSettings", FetchID: 5}}},
	{Domain: "litware.example", Outcome: leadstore.OutcomeOK, IsDrupal: true, Ambiguous: "10-or-11",
		Signals: []leadstore.Signal{{Signal: "agg-include(>=10.1)", FetchID: 9}}},
	{Domain: "northwind.example", Outcome: leadstore.OutcomeOK, IsDrupal: true, Major: 7},
	{Domain: "tailspin.example", Outcome: leadstore.OutcomeOK, IsDrupal: true, Ambiguous: "10-or-11"},
	{Domain: "wingtip.example", Outcome: leadstore.OutcomeOK},
	{Domain: "proseware.example", Outcome: leadstore.OutcomeOK, IsDrupal: true, Ambiguous: "8+",
		Signals: []leadstore.Signal{{Signal: "drupalSettings", FetchID: 12}}},
}

func TestEvalTable(t *testing.T) {
	labels, err := readLabels(strings.NewReader(labelsCSV))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printEval(&out, evaluate(labels, evalRows))
	want := "leads sites eval: 8 labelled · 7 reachable in both runs · agreement 3/7 (42.9%)\n" +
		"  missed Drupal: 1 · false Drupal: 1 · wrong major: 1 · major unknown: 1\n" +
		"  unreachable in either run: 1 · not probed: 0\n" +
		"  disagreements (domain · label · found · signals):\n" +
		"    fabrikam.example · D10 · D11 · meta-generator:11|drupalSettings\n" +
		"    wingtip.example · D11 · not Drupal · none\n" +
		"    litware.example · not Drupal · Drupal 10-or-11 · agg-include(>=10.1)\n" +
		"    proseware.example · D9 · Drupal 8+ · drupalSettings\n"
	if out.String() != want {
		t.Errorf("eval table:\n%s\nwant:\n%s", out.String(), want)
	}
}

// outsideReserved is a fictional label on a ccTLD, joined at runtime (as sitelist's tests
// do) so no whole real-looking domain is written in source: a site a dry run must skip.
var outsideReserved = "leadhound-fixture-" + "zq" + "." + "ro"

func TestPickSites(t *testing.T) {
	due := []leadstore.Site{{ID: 1, Domain: "a.example"}, {ID: 2, Domain: outsideReserved}, {ID: 3, Domain: "b.example"}, {ID: 4, Domain: "c.invalid"}}
	ids := func(ss []leadstore.Site) []int64 {
		var out []int64
		for _, s := range ss {
			out = append(out, s.ID)
		}
		return out
	}
	tests := []struct {
		name       string
		want       map[string]bool
		dryRun     bool
		max        int
		wantIDs    string
		wantNotDue int
	}{
		{"all, capped", nil, false, 3, "[1 2 3]", 0},
		{"dry run keeps reserved TLDs", nil, true, 10, "[1 3 4]", 0},
		{"named", map[string]bool{outsideReserved: true, "b.example": true, "gone.example": true}, false, 10, "[2 3]", 1},
		{"named dry run", map[string]bool{outsideReserved: true, "b.example": true}, true, 10, "[3]", 0},
	}
	for _, tt := range tests {
		got, notDue := pickSites(due, tt.want, tt.dryRun, tt.max)
		if s := fmt.Sprint(ids(got)); s != tt.wantIDs || notDue != tt.wantNotDue {
			t.Errorf("%s: got %s notDue %d, want %s %d", tt.name, s, notDue, tt.wantIDs, tt.wantNotDue)
		}
	}
}

func TestPrintProbeSummary(t *testing.T) {
	sum := probe.Summary{
		Sites: 9, Drupal: 4,
		ByMajor:   map[int]int{7: 1, 10: 1, 11: 1, 0: 1},
		Ambiguous: map[string]int{"10-or-11": 1},
		Exact:     map[string]int{"10.6.14": 1},
		Outcomes: map[string]int{
			leadstore.OutcomeOK: 5, leadstore.OutcomeBotCheck: 1, leadstore.OutcomeRobotsDisallowed: 1,
			leadstore.OutcomeTransportError: 2,
		},
		NotPublic: 1, Requests: 17, Elapsed: 90 * time.Second, Interrupted: 2,
	}
	var out bytes.Buffer
	printProbeSummary(&out, 12, true, sum, 1)
	want := "leads sites probe: run 12 (dry run, fixtures) · 9 sites probed\n" +
		"  Drupal found: 4 (D7 1 · D10 1 · D11 1 · major unknown 1: 10-or-11 1)\n" +
		"  exact versions seen: 10.6.14 1\n" +
		"  bot checks: 1 · robots-disallowed: 1 · not public: 1\n" +
		"  errors: 1 (unreachable 1 · HTTP errors 0 · not HTML 0)\n" +
		"  named but unknown or not due: 1\n" +
		"  cut short (still due): 2\n" +
		"  1.5 minutes · 17 requests · stop none\n"
	if out.String() != want {
		t.Errorf("summary:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	printProbeSummary(&out, 3, false, probe.Summary{StopReason: probe.StopFailureRate}, 0)
	if !strings.Contains(out.String(), "Drupal found: 0\n") || !strings.Contains(out.String(), "stop failure_rate (network down") ||
		!strings.Contains(out.String(), "exact versions seen: none") {
		t.Errorf("stopped summary:\n%s", out.String())
	}
}

func TestManualEntries(t *testing.T) {
	got, tally := manualEntries([]string{"www.Northwind.example", "contoso.example.", "192.0.2.10", "bad host.example", "example"})
	if len(got) != 2 || got[0].Domain != "northwind.example" || got[1].Domain != "contoso.example" || tally.Bad != 3 {
		t.Errorf("got %+v, tally %+v", got, tally)
	}
}

func TestIsReserved(t *testing.T) {
	for d, want := range map[string]bool{"a.example": true, "b.invalid": true, "c.test": true, outsideReserved: false, "example.invalid.zz": false} {
		if got := isReserved(d); got != want {
			t.Errorf("isReserved(%q) = %v", d, got)
		}
	}
}
