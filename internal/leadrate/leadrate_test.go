package leadrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/eval"
	"github.com/gremy/webbership-leadhound/internal/scoring"
)

const demoGolden = "../../examples/leads-relevance.demo.jsonl"

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func sampleInput() Input {
	return Input{Kind: "unsupported", Major: 7, Version: "7.98", EOL: date(2025, time.January, 5), Country: "GB",
		ListRank: 48210, SiteName: "Northwind Traders", Description: "Wholesale fine foods.",
		Channels: []string{"form", "email"}, HasPhone: true, SocialLinks: 3}
}

func TestStateNeverCarriesContacts(t *testing.T) {
	in := sampleInput()
	in.SiteName = "Northwind hello@northwind.example"
	in.Description = "Call +44 20 7946 0123 or (020) 7946 0456 or 020-7946-0789,\n" +
		"write to sales@northwind.example, <info@northwind.example>, orders [at] northwind [dot] example " +
		"or accounts at northwind dot example. Two numbers back to back: 020 7946 0111 020 7946 0222."
	in.Version = "7.98 tel +44 20 7946 0999"
	st := State(in)
	for _, bad := range []string{"@", "7946", "northwind.example", "[at]", "sales", "orders", "accounts"} {
		if strings.Contains(st, bad) {
			t.Errorf("state carries %q:\n%s", bad, st)
		}
	}
	// No line holds six or more digits with only phone separators between them.
	digits := regexp.MustCompile(`\d(?:[ ().\-/+]*\d){5,}`)
	for _, l := range strings.Split(st, "\n") {
		if m := digits.FindString(l); m != "" {
			t.Errorf("state line %q carries a digit run %q", l, m)
		}
	}
	// The Input type itself has no field for an address or a number, only whether a phone exists.
	ty := reflect.TypeFor[Input]()
	for i := range ty.NumField() {
		f := ty.Field(i)
		n := strings.ToLower(f.Name)
		if strings.Contains(n, "email") || strings.Contains(n, "mail") ||
			(strings.Contains(n, "phone") && f.Type.Kind() != reflect.Bool) {
			t.Errorf("Input has a contact field %s %s", f.Name, f.Type)
		}
	}
}

func TestStateHasNoRelativeDates(t *testing.T) {
	relative := regexp.MustCompile(`(?i)\b(ends|ended|ending|ago|until|left|since|remaining|today|tomorrow|yesterday|soon|passed|expired|expires|overdue|days?|weeks?|months?|years?)\b`)
	for _, eol := range []time.Time{date(2025, time.January, 5), date(2026, time.December, 9), date(2031, time.June, 30)} {
		in := sampleInput()
		in.EOL = eol
		st := State(in)
		if m := relative.FindString(st); m != "" {
			t.Errorf("state for %s says %q:\n%s", eol.Format(DateLayout), m, st)
		}
		want := "End-of-life date of that major version: " + eol.Format("2 January 2006")
		if !strings.Contains(st, want) {
			t.Errorf("state lacks %q:\n%s", want, st)
		}
	}
	in := sampleInput()
	in.EOL = time.Time{}
	if st := State(in); !strings.Contains(st, "End-of-life date of that major version: unknown") {
		t.Errorf("unknown date not written as unknown:\n%s", st)
	}
}

func TestStateLayoutIsFixed(t *testing.T) {
	a := sampleInput()
	b := sampleInput()
	b.Channels = []string{"email", "form", "email", "none"}
	if State(a) != State(b) {
		t.Errorf("channel order or repeats change the state:\n%s\n---\n%s", State(a), State(b))
	}
	if got := strings.Count(State(Input{Kind: "d10"}), "\n") + 1; got != 11 {
		t.Errorf("empty input gives %d lines, want 11", got)
	}
	in := sampleInput()
	in.Description = strings.Repeat("word ", 1000)
	for _, l := range strings.Split(State(in), "\n") {
		if strings.HasPrefix(l, "Description: ") && len([]rune(l)) > len("Description: ")+MaxDescription+1 {
			t.Errorf("description not cut: %d runes", len([]rune(l)))
		}
	}
}

func TestScoreFromProbabilities(t *testing.T) {
	cases := []struct {
		name  string
		a     ai.Answer
		score int
		band  string
	}{
		{"all strong", ai.Answer{Choice: "strong", Probabilities: map[string]float64{"strong": 1}}, 100, "strong"},
		{"all no", ai.Answer{Choice: "no", Probabilities: map[string]float64{"no": 1}}, 0, "no"},
		{"split", ai.Answer{Choice: "likely", Probabilities: map[string]float64{"strong": 0.2, "likely": 0.5, "maybe": 0.3}}, 73, "likely"},
		{"half rounds up", ai.Answer{Choice: "strong", Probabilities: map[string]float64{"strong": 0.7, "likely": 0.3}}, 93, "strong"},
		{"tie goes to choice", ai.Answer{Choice: "unlikely", Probabilities: map[string]float64{"maybe": 0.5, "unlikely": 0.5}}, 38, "unlikely"},
		{"tie without choice goes to better band", ai.Answer{Choice: "no", Probabilities: map[string]float64{"maybe": 0.5, "unlikely": 0.5}}, 38, "maybe"},
		{"not summing to one", ai.Answer{Choice: "maybe", Probabilities: map[string]float64{"strong": 0.5, "no": 0.5, "other": 3}}, 50, "strong"},
		{"no probabilities", ai.Answer{Choice: "likely"}, 75, "likely"},
		{"nothing usable", ai.Answer{Choice: "other"}, 0, ""},
		{"bad numbers ignored", ai.Answer{Choice: "likely", Probabilities: map[string]float64{"strong": math.NaN(), "likely": 1, "no": -2}}, 75, "likely"},
	}
	for _, c := range cases {
		s, b := Score(c.a)
		if s != c.score || b != c.band {
			t.Errorf("%s: Score = (%d, %q), want (%d, %q)", c.name, s, b, c.score, c.band)
		}
	}
}

func TestHashChangesWithWording(t *testing.T) {
	in := sampleInput()
	base := InputHash("jev-1.13.0", in)
	if !bytes.Equal(base, InputHash("jev-1.13.0", sampleInput())) {
		t.Fatal("same input, different hash")
	}
	if bytes.Equal(base, InputHash("jev-1.14.0", in)) {
		t.Error("a new decider keeps the hash")
	}
	changed := sampleInput()
	changed.SocialLinks++
	if bytes.Equal(base, InputHash("jev-1.13.0", changed)) {
		t.Error("a changed lead keeps the hash")
	}

	saved := bands[2].description
	bands[2].description += " Reworded."
	reworded := InputHash("jev-1.13.0", in)
	bands[2].description = saved
	if bytes.Equal(base, reworded) {
		t.Error("an option's new wording keeps the hash")
	}
	if !bytes.Equal(base, InputHash("jev-1.13.0", in)) {
		t.Fatal("restoring the wording did not restore the hash")
	}
}

func TestHashMatchesScoringEncoding(t *testing.T) {
	in := sampleInput()
	h := sha256.New()
	field := func(s string) { _, _ = fmt.Fprintf(h, "%d:%s", len(s), s) }
	q := AIQuestion()
	field("jev-1.13.0")
	field(q.Name)
	field(q.Instructions)
	for _, o := range q.Options {
		field(o.Name)
		field(o.Description)
	}
	field(State(in))
	field("0")        // the threshold slot
	field("1")        // one keyword list
	field("question") // its name
	field("1")        // one entry
	field(Question)
	want := h.Sum(nil)
	if got := InputHash("jev-1.13.0", in); !bytes.Equal(got, want) {
		t.Errorf("InputHash = %x, want the length-prefixed encoding %x", got, want)
	}
	if got := scoring.InputHash("jev-1.13.0", []ai.Question{q}, State(in), 0, map[string][]string{"question": {Question}}); !bytes.Equal(got, want) {
		t.Errorf("scoring.InputHash drifted from the encoding this test writes out")
	}
}

func TestSpearmanTies(t *testing.T) {
	if got, want := ranks([]float64{1, 2, 2, 3}), []float64{1, 2.5, 2.5, 4}; !reflect.DeepEqual(got, want) {
		t.Errorf("ranks = %v, want %v", got, want)
	}
	if got, want := ranks([]float64{50, 0, 100, 50, 50}), []float64{3, 1, 5, 3, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("ranks = %v, want %v", got, want)
	}
	// Pearson over average ranks: 4.5/sqrt(4.5*5). The no-ties shortcut 1-6Σd²/(n(n²-1))
	// would give 0.95, so this pins the tie handling.
	if got, want := Spearman([]float64{1, 2, 2, 3}, []float64{1, 2, 3, 4}), 4.5/math.Sqrt(22.5); math.Abs(got-want) > 1e-12 {
		t.Errorf("Spearman = %v, want %v", got, want)
	}
	if got := Spearman([]float64{1, 2, 3}, []float64{30, 20, 10}); math.Abs(got+1) > 1e-12 {
		t.Errorf("reversed order = %v, want -1", got)
	}
	for _, c := range [][2][]float64{{{1, 1, 1}, {1, 2, 3}}, {{1}, {2}}, {{1, 2}, {1}}} {
		if got := Spearman(c[0], c[1]); got != 0 {
			t.Errorf("undefined Spearman(%v, %v) = %v, want 0", c[0], c[1], got)
		}
	}
}

// demoAnswers scripts the fake decider for the demo golden set by lead id.
var demoAnswers = map[string]map[string]float64{
	"northwind.example":       {"strong": 0.8, "likely": 0.2},  // 95 strong · exact
	"contoso-museum.example":  {"likely": 0.6, "maybe": 0.4},   // 65 likely · exact
	"tailspin.example":        {"strong": 0.7, "likely": 0.3},  // 93 strong · one off
	"fabrikam.example":        {"maybe": 1},                    // 50 maybe · exact
	"wingtip-club.example":    {"maybe": 0.5, "unlikely": 0.5}, // 38 unlikely (the choice) · exact
	"placeholder.example":     {"no": 0.9, "unlikely": 0.1},    // 3 no · exact
	"adventure-works.example": {"likely": 0.6, "strong": 0.4},  // 85 likely · one off
}

func demoFake(t *testing.T, golden []Golden) *ai.Fake {
	t.Helper()
	f := &ai.Fake{ByState: map[string]map[string]ai.Answer{}, Usage: ai.Usage{InputTokens: 1900}}
	for _, g := range golden {
		p, ok := demoAnswers[g.ID]
		if !ok {
			continue // unlabelled: Evaluate must never ask, and the fake would refuse
		}
		choice := ""
		for _, b := range Bands() {
			if p[b] > p[choice] || (p[b] == p[choice] && g.Label != nil && b == *g.Label) {
				choice = b
			}
		}
		f.ByState[State(g.Input)] = map[string]ai.Answer{AIQuestion().Name: {Choice: choice, Confidence: p[choice], Probabilities: p}}
	}
	return f
}

func TestDemoGoldenEval(t *testing.T) {
	golden, err := LoadGolden(demoGolden)
	if err != nil {
		t.Fatal(err)
	}
	if len(golden) != 8 {
		t.Fatalf("demo golden has %d lines, want 8", len(golden))
	}
	for _, g := range golden {
		if !strings.HasSuffix(g.ID, ".example") {
			t.Errorf("demo lead %s is not on a .example domain", g.ID)
		}
	}
	f := demoFake(t, golden)
	rep, err := Evaluate(context.Background(), f, golden, 3)
	if err != nil {
		t.Fatal(err)
	}
	if rep.N != 7 || rep.Unlabelled != 1 || f.Calls() != 21 {
		t.Fatalf("N %d unlabelled %d calls %d, want 7, 1 and 21", rep.N, rep.Unlabelled, f.Calls())
	}
	near := func(name string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-4 {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	near("exact", rep.Exact, 5.0/7)
	near("within one", rep.WithinOne, 1)
	near("MAE", rep.MAE, 64.0/7) // 5+10+18+0+13+3+15
	// label ranks 6.5 4.5 4.5 3 2 1 6.5, score ranks 7 4 6 3 2 1 5: 25/sqrt(27*28)
	near("Spearman", rep.Spearman, 25/math.Sqrt(27*28))
	if len(rep.SpearmanRuns) != 3 {
		t.Errorf("Spearman per run: %v", rep.SpearmanRuns)
	}
	if rep.Tokens != 21*1900 {
		t.Errorf("tokens %d, want %d", rep.Tokens, 21*1900)
	}
	near("cost", rep.Cost, 21*1900*eval.CostPerMillionInput/1e6)
	if len(rep.Disagreements) != 0 {
		t.Errorf("a scripted fake disagreed with itself: %v", rep.Disagreements)
	}
	if rep.Trusted() {
		t.Error("7 labelled leads passed the trust gate, which needs 30")
	}
	var out strings.Builder
	if err := rep.Write(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"7 labelled lead(s), 1 unlabelled skipped", "within one band: 100%", "need 30", "disagreements between runs: none", "input tokens: 39900"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	for _, g := range golden {
		if g.Input.SiteName != "" && strings.Contains(out.String(), g.Input.SiteName) {
			t.Errorf("report names a site (%s); it must name leads by id only", g.Input.SiteName)
		}
	}
}

// flipDecider answers one state differently on every second ask.
type flipDecider struct {
	*ai.Fake
	state string
	n     int
}

func (d *flipDecider) Decide(ctx context.Context, state string, qs []ai.Question) (map[string]ai.Answer, ai.Usage, error) {
	if state == d.state {
		d.n++
		if d.n%2 == 0 {
			return map[string]ai.Answer{AIQuestion().Name: {Choice: "no", Probabilities: map[string]float64{"no": 1}}}, ai.Usage{InputTokens: 10}, nil
		}
	}
	return d.Fake.Decide(ctx, state, qs)
}

func TestEvaluateDisagreementsAndErrors(t *testing.T) {
	golden, err := LoadGolden(demoGolden)
	if err != nil {
		t.Fatal(err)
	}
	f := demoFake(t, golden)
	d := &flipDecider{Fake: f, state: State(golden[0].Input)}
	rep, err := Evaluate(context.Background(), d, golden, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{golden[0].ID + ": strong / no / strong"}; !reflect.DeepEqual(rep.Disagreements, want) {
		t.Errorf("disagreements %v, want %v", rep.Disagreements, want)
	}

	if _, err := Evaluate(context.Background(), f, golden, 0); err == nil {
		t.Error("0 runs accepted")
	}
	if _, err := Evaluate(context.Background(), f, golden, eval.MaxRuns+1); err == nil {
		t.Error("too many runs accepted")
	}
	none := []Golden{{ID: "a.example", Input: Input{Kind: "d10"}}}
	if _, err := Evaluate(context.Background(), f, none, 1); !errors.Is(err, ErrNoLabels) {
		t.Errorf("all unlabelled: err %v, want ErrNoLabels", err)
	}
	failing := &ai.Fake{Err: ai.ErrSpendCap}
	if _, err := Evaluate(context.Background(), failing, golden, 1); !errors.Is(err, ai.ErrSpendCap) {
		t.Errorf("decider error: err %v, want ErrSpendCap", err)
	}
}

func TestTrusted(t *testing.T) {
	pass := Report{N: 30, WithinOne: 0.8, Spearman: 0.5}
	if !pass.Trusted() {
		t.Error("the gate's own edge did not pass")
	}
	for _, r := range []Report{{N: 29, WithinOne: 1, Spearman: 1}, {N: 100, WithinOne: 0.79, Spearman: 1}, {N: 100, WithinOne: 1, Spearman: 0.49}} {
		if r.Trusted() {
			t.Errorf("%+v passed the gate", r)
		}
	}
}

func TestLoadGoldenStrict(t *testing.T) {
	dir := t.TempDir()
	ok := `{"id":"a.example","input":{"kind":"d10","major":10,"eol":"2026-12-09"},"label":"maybe","note":""}`
	cases := map[string]string{
		"unknown top field":   `{"id":"a.example","input":{"kind":"d10"},"label":null,"note":"","extra":1}`,
		"unknown input field": `{"id":"a.example","input":{"kind":"d10","email":"x"},"label":null,"note":""}`,
		"bad label":           `{"id":"a.example","input":{"kind":"d10"},"label":"great","note":""}`,
		"no kind":             `{"id":"a.example","input":{},"label":null,"note":""}`,
		"bad date":            `{"id":"a.example","input":{"kind":"d10","eol":"9 December 2026"},"label":null,"note":""}`,
		"negative count":      `{"id":"a.example","input":{"kind":"d10","social_links":-1},"label":null,"note":""}`,
		"spaced id":           `{"id":"a example","input":{"kind":"d10"},"label":null,"note":""}`,
		"trailing data":       ok + ` {}`,
		"repeated id":         ok + "\n" + ok,
		"empty":               "\n\n",
	}
	for name, body := range cases {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".jsonl")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadGolden(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p := filepath.Join(dir, "ok.jsonl")
	if err := os.WriteFile(p, []byte(ok+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := LoadGolden(p)
	if err != nil {
		t.Fatal(err)
	}
	if !g[0].Input.EOL.Equal(date(2026, time.December, 9)) || g[0].Label == nil || *g[0].Label != "maybe" {
		t.Errorf("loaded %+v", g[0])
	}
}

func TestInputJSONRoundTrip(t *testing.T) {
	in := sampleInput()
	b, err := in.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"eol":"2025-01-05"`) {
		t.Errorf("eol not written as a date: %s", b)
	}
	var back Input
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, in) {
		t.Errorf("round trip: %+v, want %+v", back, in)
	}
}
