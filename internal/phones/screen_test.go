package phones

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/compliance"
)

var (
	now           = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	campaignStart = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
)

func reg(t *testing.T, code string) compliance.Register {
	t.Helper()
	r, ok := compliance.Default().Registers[code]
	if !ok {
		t.Fatalf("register %q not in the country table", code)
	}
	return r
}

func TestCallableGrid(t *testing.T) {
	const n = "+442079460000"
	gb := compliance.RegistersFor("GB") // tps + ctps, 28 days
	it := []compliance.Register{reg(t, "rpo")}
	day := 24 * time.Hour
	both := func(res string, age time.Duration) []Screening {
		return []Screening{
			{Number: n, Register: "tps", Result: res, CheckedAt: now.Add(-age)},
			{Number: n, Register: "ctps", Result: Clean, CheckedAt: now.Add(-age)},
		}
	}
	cases := []struct {
		name     string
		s        []Screening
		required []compliance.Register
		want     bool
		reason   string
	}{
		{"clean on both GB registers", both(Clean, 2*day), gb, true, ""},
		{"listed on tps", both(Listed, 2*day), gb, false, "listed on"},
		{"clean 29 days ago for a 28-day register", both(Clean, 29*day), gb, false, "older than 28 days"},
		{"clean exactly 28 days ago", both(Clean, 28*day), gb, true, ""},
		{"missing register (ctps never screened)", both(Clean, 2*day)[:1], gb, false, "not screened against"},
		{"never screened", nil, gb, false, "not screened against"},
		{"clean then listed later: latest wins", append(both(Clean, 5*day), Screening{Number: n, Register: "tps", Result: Listed, CheckedAt: now.Add(-day)}), gb, false, "listed on"},
		{"another number's screening does not count", []Screening{{Number: "+442079460001", Register: "rpo", Result: Clean, CheckedAt: now}}, it, false, "not screened"},
		{"0-day register screened before the campaign started", []Screening{{Number: n, Register: "rpo", Result: Clean, CheckedAt: campaignStart.Add(-time.Hour)}}, it, false, "before this campaign"},
		{"0-day register screened after the campaign started", []Screening{{Number: n, Register: "rpo", Result: Clean, CheckedAt: campaignStart.Add(time.Hour)}}, it, true, ""},
		{"a future-dated screening is ignored", []Screening{{Number: n, Register: "rpo", Result: Clean, CheckedAt: now.Add(day)}}, it, false, "not screened"},
		{"no register required (RO)", nil, compliance.RegistersFor("RO"), true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := Callable(n, c.s, c.required, campaignStart, now)
			if ok != c.want || !strings.Contains(reason, c.reason) {
				t.Fatalf("Callable = %v %q, want %v containing %q", ok, reason, c.want, c.reason)
			}
		})
	}
	if ok, _ := Callable(n, []Screening{{Number: n, Register: "rpo", Result: Clean, CheckedAt: now}}, it, time.Time{}, now); ok {
		t.Fatal("a 0-day register with no campaign start must not be callable")
	}
}

func TestParseResultsSynonymsAndForms(t *testing.T) {
	tps := reg(t, "tps")
	file := strings.Join([]string{
		"Phone Number;TPS_Status",
		"+44 (0)20 7946 0001;Not Registered",
		"0044 20 7946 0002;registered",
		"020 7946 0003; on-list ",
		"'+442079460004;CLEAN",
		"+44 20 7946 0005;not on the list",
		"+44 20 7946 0006;do_not_call",
		"",
		"+44 20 7946 0007;maybe",          // unknown status
		"hello;clean",                     // not a number
		"442079460008;clean",              // country code without + or 00: not guessed
		"+44 20 7946 0009",                // no status column
		"+44 20 7946 0010;unlisted;extra", // extra cell is fine
		"+44 20 7946 0010;listed",         // duplicate: listed wins
		"+44 20 7946 0011;no match",       // clean
		"+44 20 7946 0012;opted out",      // listed
	}, "\n")
	checked := now.Add(-time.Hour)
	sum, err := ParseResults(strings.NewReader(file), tps, checked, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"+442079460001": Clean, "+442079460002": Listed, "+442079460003": Listed,
		"+442079460004": Clean, "+442079460005": Clean, "+442079460006": Listed,
		"+442079460010": Listed, "+442079460011": Clean, "+442079460012": Listed,
	}
	got := map[string]string{}
	for _, s := range sum.Screenings {
		if s.Register != "tps" || !s.CheckedAt.Equal(checked) {
			t.Fatalf("screening %+v: wrong register or date", s)
		}
		got[s.Number] = s.Result
	}
	if len(got) != len(want) {
		t.Fatalf("got %d screenings %v, want %d %v", len(got), got, len(want), want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if sum.Refused != 4 {
		t.Fatalf("Refused = %d (lines %v), want 4", sum.Refused, sum.Lines)
	}
	if want := []int{9, 10, 11, 12}; len(sum.Lines) != 4 || sum.Lines[0] != want[0] || sum.Lines[3] != want[3] {
		t.Fatalf("refused lines %v, want %v", sum.Lines, want)
	}
}

func TestParseResultsNoHeaderYesNoAndDates(t *testing.T) {
	ie := reg(t, "ndd")
	// No header: column 1 number, column 2 status words; national IE numbers take +353.
	sum, err := ParseResults(strings.NewReader("01 234 5678,clean\n00353 1 234 5679,listed\n01 234 5670,yes\n"), ie, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Screenings) != 2 || sum.Screenings[0].Number != "+35312345678" || sum.Screenings[1].Result != Listed || sum.Refused != 1 {
		t.Fatalf("no header: %+v", sum)
	}

	// A "listed?" column takes yes/no; a date column gives each row its date.
	tps := reg(t, "tps")
	file := "number\ttps\tchecked\n" +
		"+442079460001\tno\t2026-11-09\n" +
		"+442079460002\tYes\t2026-11-08T10:00:00Z\n" +
		"+442079460003\tno\t09/11/2026\n" + // not an ISO date: refused
		"+442079460004\tno\t2027-01-01\n" // after now: refused
	sum, err = ParseResults(strings.NewReader(file), tps, time.Time{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Screenings) != 2 || sum.Refused != 2 {
		t.Fatalf("tab file: %+v", sum)
	}
	if s := sum.Screenings[0]; s.Result != Clean || !s.CheckedAt.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("first row %+v", s)
	}
	if sum.Screenings[1].Result != Listed {
		t.Fatalf("Yes in a tps column must read as listed: %+v", sum.Screenings[1])
	}

	// In a plain status column, yes/no is ambiguous and refused.
	sum, err = ParseResults(strings.NewReader("number,status\n+442079460001,yes\n"), tps, now, now)
	if err != nil || sum.Refused != 1 || len(sum.Screenings) != 0 {
		t.Fatalf("yes in a status column: %+v %v", sum, err)
	}
}

type fakeStore struct {
	missing  []string
	gotReg   string
	gotCtry  string
	gotSince time.Time
	recorded []Screening
}

func (f *fakeStore) RecordScreening(_ context.Context, s Screening, _, _ string) error {
	f.recorded = append(f.recorded, s)
	return nil
}

func (f *fakeStore) LatestScreening(context.Context, string, string) (Screening, bool, error) {
	return Screening{}, false, nil
}

func (f *fakeStore) ScreeningsMissingOrOlder(_ context.Context, register, country string, before time.Time) ([]string, error) {
	f.gotReg, f.gotCtry, f.gotSince = register, country, before
	return f.missing, nil
}

func TestExportListsOnlyMissingOrStale(t *testing.T) {
	st := &fakeStore{missing: []string{"+442079460002", "02079460001", "+442079460002", "12345"}}
	var buf bytes.Buffer
	sum, err := Export(context.Background(), st, reg(t, "tps"), campaignStart, now, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if st.gotReg != "tps" || st.gotCtry != "GB" || !st.gotSince.Equal(now.AddDate(0, 0, -28)) {
		t.Fatalf("store asked for %s %s since %v", st.gotReg, st.gotCtry, st.gotSince)
	}
	if want := "number\n+442079460001\n+442079460002\n"; buf.String() != want {
		t.Fatalf("export = %q, want %q", buf.String(), want)
	}
	if sum.Written != 2 || sum.Undialable != 1 {
		t.Fatalf("summary %+v", sum)
	}

	// A 0-day register asks for screenings since the campaign's start, and needs one.
	st = &fakeStore{}
	if _, err := Export(context.Background(), st, reg(t, "rpo"), campaignStart, now, &buf); err != nil || !st.gotSince.Equal(campaignStart) {
		t.Fatalf("rpo export since %v, err %v", st.gotSince, err)
	}
	if _, err := Export(context.Background(), st, reg(t, "rpo"), time.Time{}, now, &buf); err == nil {
		t.Fatal("a 0-day register export with no campaign start must be refused")
	}
}

func TestImportRecordsEach(t *testing.T) {
	st := &fakeStore{}
	sum := ImportSummary{Screenings: []Screening{{Number: "+442079460001", Register: "tps", Result: Clean, CheckedAt: now}}}
	n, err := Import(context.Background(), st, sum, "tps-results.csv", "operator")
	if err != nil || n != 1 || len(st.recorded) != 1 {
		t.Fatalf("Import = %d %v, recorded %v", n, err, st.recorded)
	}
}
