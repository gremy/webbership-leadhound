package leadconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/config"
)

func ptr[T any](v T) *T { return &v }

func demoConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := Load(demoDir(t))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestOverridesApply: each knob alone replaces the file's value, the others stay, and the
// input config is left exactly as it was.
func TestOverridesApply(t *testing.T) {
	cases := []struct {
		knob  string
		o     Overrides
		check func(t *testing.T, c Config)
	}{
		{"kinds_off", Overrides{KindsOff: &[]string{KindD10}}, func(t *testing.T, c Config) {
			if !slices.Equal(c.KindsOff, []string{KindD10}) || !c.KindOff(KindD10) || c.KindOff(KindUnsupported) {
				t.Errorf("kinds_off = %v", c.KindsOff)
			}
		}},
		{"kinds_off emptied", Overrides{KindsOff: &[]string{}}, func(t *testing.T, c Config) {
			if len(c.KindsOff) != 0 {
				t.Errorf("kinds_off = %v", c.KindsOff)
			}
		}},
		{"kinds_priority", Overrides{KindsPriority: &[]string{KindD10, KindUnsupported, KindAgencyPartner}}, func(t *testing.T, c Config) {
			if !slices.Equal(c.KindsPriority, []string{KindD10, KindUnsupported, KindAgencyPartner}) {
				t.Errorf("kinds_priority = %v", c.KindsPriority)
			}
		}},
		{"skip_rank_over", Overrides{SkipRankOver: ptr(250000)}, func(t *testing.T, c Config) {
			if c.SkipRankOver != 250000 {
				t.Errorf("skip_rank_over = %d", c.SkipRankOver)
			}
		}},
		{"batches_from", Overrides{BatchesFrom: ptr(BatchesStarredThenRest)}, func(t *testing.T, c Config) {
			if c.BatchesFrom() != BatchesStarredThenRest {
				t.Errorf("batches_from = %q", c.BatchesFrom())
			}
		}},
		{"facts.d6_eol", Overrides{Facts: &FactsOverrides{D6EOL: ptr(ParseDate("2016-03-01"))}}, func(t *testing.T, c Config) {
			wantDate(t, c.Facts.D6EOL, "2016-03-01")
		}},
		{"facts.d7_eol", Overrides{Facts: &FactsOverrides{D7EOL: ptr(ParseDate("2025-02-01"))}}, func(t *testing.T, c Config) {
			wantDate(t, c.Facts.D7EOL, "2025-02-01")
		}},
		{"facts.d8_eol", Overrides{Facts: &FactsOverrides{D8EOL: ptr(ParseDate("2021-12-01"))}}, func(t *testing.T, c Config) {
			wantDate(t, c.Facts.D8EOL, "2021-12-01")
		}},
		{"facts.d9_eol", Overrides{Facts: &FactsOverrides{D9EOL: ptr(ParseDate("2023-12-01"))}}, func(t *testing.T, c Config) {
			wantDate(t, c.Facts.D9EOL, "2023-12-01")
		}},
		{"facts.d10_eol", Overrides{Facts: &FactsOverrides{D10EOL: ptr(ParseDate("2027-01-31"))}}, func(t *testing.T, c Config) {
			wantDate(t, c.Facts.D10EOL, "2027-01-31")
			wantDate(t, c.Facts.D9EOL, "2023-11-01") // the dates not set keep the file's
		}},
		{"relevance.rating", Overrides{Relevance: &RelevanceOverrides{Rating: ptr(false)}}, func(t *testing.T, c Config) {
			if c.Relevance.RatingOn() {
				t.Error("rating still on")
			}
		}},
		{"relevance.leads_daily_calls", Overrides{Relevance: &RelevanceOverrides{LeadsDailyCalls: ptr(200)}}, func(t *testing.T, c Config) {
			if c.Relevance.LeadsDailyCalls != 200 || c.Relevance.DailyCalls(10000) != 200 || !c.Relevance.RatingOn() {
				t.Errorf("relevance = %+v", c.Relevance)
			}
		}},
	}
	// before is a second, independent load of the same file, so it shares nothing with base.
	dir := demoDir(t)
	base, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.knob, func(t *testing.T) {
			got := tc.o.Apply(base)
			tc.check(t, got)
			if p := tc.o.Validate(base); len(p) != 0 {
				t.Errorf("Validate = %v, want none", p)
			}
			eff, p := tc.o.Effective(base)
			if len(p) != 0 {
				t.Fatalf("Effective problems = %v", p)
			}
			tc.check(t, eff)
			// Writing into the result never reaches the input.
			got.KindsOff = append(got.KindsOff[:0:0], "x")
			if len(got.KindsPriority) > 0 {
				got.KindsPriority[0] = "x"
			}
			got.Countries["GB"] = Country{Channel: ChannelNone}
			got.SiteLists.TLDs[0] = "x"
			got.Sender.OptOutLines["en"] = "x"
			if got.Relevance.Rating != nil {
				*got.Relevance.Rating = !*got.Relevance.Rating
			}
			if !reflect.DeepEqual(base, before) {
				t.Error("Apply changed its input")
			}
		})
	}
	if got := (Overrides{}).Apply(base); !reflect.DeepEqual(got, base) {
		t.Error("no overrides changed the config")
	}
	// The override's own slice is not shared either.
	off := []string{KindD10}
	got := Overrides{KindsOff: &off}.Apply(base)
	off[0] = KindUnsupported
	if !got.KindOff(KindD10) {
		t.Error("Apply shares the override's slice")
	}
}

func wantDate(t *testing.T, d Date, want string) {
	t.Helper()
	if got := d.Format("2006-01-02"); got != want {
		t.Errorf("date = %s, want %s", got, want)
	}
}

// TestOverridesExactlyTheEditableKnobs: the JSON keys Overrides accepts are D11's editable
// lead_setting knobs (and the email check's verify.catch_all), no more and no fewer.
func TestOverridesExactlyTheEditableKnobs(t *testing.T) {
	var keys []string
	var walk func(prefix string, typ reflect.Type)
	walk = func(prefix string, typ reflect.Type) {
		for f := range typ.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && ft != reflect.TypeFor[Date]() {
				walk(prefix+name+".", ft)
				continue
			}
			keys = append(keys, prefix+name)
		}
	}
	walk("", reflect.TypeFor[Overrides]())
	slices.Sort(keys)
	want := []string{
		"batches_from", "facts.d10_eol", "facts.d6_eol", "facts.d7_eol", "facts.d8_eol", "facts.d9_eol",
		"kinds_off", "kinds_priority", "relevance.leads_daily_calls", "relevance.rating", "skip_rank_over", "verify.catch_all",
	}
	if !slices.Equal(keys, want) {
		t.Errorf("override keys = %v\nwant %v", keys, want)
	}
}

// forbiddenField matches a field name that must never be a settings override: a secret, or a
// knob that stays read-only in leads.yaml.
var forbiddenField = regexp.MustCompile(`(?i)operator|password|key|secret|dsn|opt_out|caps|ramp|sender|channel|template`)

// unsafeFields lists every field reachable from typ whose type is config.Secret or whose Go or
// JSON name matches forbiddenField.
func unsafeFields(typ reflect.Type) []string {
	var bad []string
	seen := map[reflect.Type]bool{}
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		if typ == reflect.TypeFor[config.Secret]() {
			bad = append(bad, path+" is a config.Secret")
			return
		}
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(path, typ.Elem())
		case reflect.Map:
			walk(path+"[key]", typ.Key())
			walk(path, typ.Elem())
		case reflect.Struct:
			if seen[typ] {
				return
			}
			seen[typ] = true
			for f := range typ.Fields() {
				jsonName, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				if forbiddenField.MatchString(f.Name) || forbiddenField.MatchString(jsonName) {
					bad = append(bad, path+"."+f.Name+" has a read-only or secret name")
				}
				walk(path+"."+f.Name, f.Type)
			}
		}
	}
	walk(typ.Name(), typ)
	return bad
}

func TestOverridesNoSecretOrReadOnlyFields(t *testing.T) {
	if bad := unsafeFields(reflect.TypeFor[Overrides]()); len(bad) > 0 {
		t.Errorf("Overrides carries fields it must not: %v", bad)
	}
	// The sweep itself catches a planted secret, a read-only name and a nested one.
	type planted struct {
		Inner *struct {
			Addresses []config.Secret `json:"addresses"`
		} `json:"inner"`
		Lines map[string]string `json:"opt_out_lines"`
		Ramp  []Ramp            `json:"steps"`
	}
	if bad := unsafeFields(reflect.TypeFor[planted]()); len(bad) != 3 {
		t.Errorf("the sweep found %v in the planted struct, want 3 findings", bad)
	}
}

func TestUnknownOverrideKeyRefused(t *testing.T) {
	cases := []struct {
		name, in string
		want     string
	}{
		{"read-only key", `{"sender":{"name":"x"}}`, `"sender" is not a setting the console can change`},
		{"nested unknown key", `{"relevance":{"daily":5}}`, `"daily" is not a setting the console can change`},
		{"secret key", `{"operator_addresses":["x"]}`, `"operator_addresses"`},
		{"wrong type", `{"skip_rank_over":"big"}`, "skip_rank_over must be a whole number, not string"},
		{"nested wrong type", `{"relevance":{"rating":"yes"}}`, "relevance.rating must be true or false, not string"},
		{"list wrong type", `{"kinds_off":"d10"}`, "kinds_off must be a list, not string"},
		{"two values", `{} {}`, "more than one JSON value"},
		{"not json", `{kinds_off}`, "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := DecodeOverrides([]byte(tc.in))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrInvalid naming %q", err, tc.want)
			}
			if !reflect.DeepEqual(o, Overrides{}) {
				t.Errorf("a refused decode returned %+v", o)
			}
		})
	}
	for _, in := range []string{"", "  ", "null", "{}"} {
		if o, err := DecodeOverrides([]byte(in)); err != nil || !reflect.DeepEqual(o, Overrides{}) {
			t.Errorf("DecodeOverrides(%q) = %+v, %v; want no overrides", in, o, err)
		}
	}
}

// TestOverridesRoundTrip: every knob saved as JSON reads back the same, dates as written.
func TestOverridesRoundTrip(t *testing.T) {
	o := Overrides{
		KindsOff:      &[]string{},
		KindsPriority: &[]string{KindUnsupported, KindD10, KindAgencyPartner},
		SkipRankOver:  ptr(0),
		BatchesFrom:   ptr(BatchesStarred),
		Facts: &FactsOverrides{
			D6EOL: ptr(ParseDate("2016-02-24")), D7EOL: ptr(ParseDate("2025-01-05")), D8EOL: ptr(ParseDate("2021-11-17")),
			D9EOL: ptr(ParseDate("2023-11-01")), D10EOL: ptr(ParseDate("soon")),
		},
		Relevance: &RelevanceOverrides{Rating: ptr(false), LeadsDailyCalls: ptr(0)},
	}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"d6_eol":"2016-02-24"`) || !strings.Contains(string(b), `"kinds_off":[]`) {
		t.Errorf("saved = %s", b)
	}
	back, err := DecodeOverrides(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, o) {
		t.Errorf("read back %+v\nwant %+v", back, o)
	}
}

// TestOverridesValidateMatchesLoad: a bad override is refused with the very words Load uses
// for the same value written in the file.
func TestOverridesValidateMatchesLoad(t *testing.T) {
	cases := []struct {
		name     string
		o        Overrides
		old, new string // the same value written into the demo file
		want     string
	}{
		{"unknown kind off", Overrides{KindsOff: &[]string{"hand"}}, "kinds_off: []", `kinds_off: [hand]`, `kinds_off: "hand" is not one of`},
		{"kind off twice", Overrides{KindsOff: &[]string{KindD10, KindD10}}, "kinds_off: []", `kinds_off: [d10, d10]`, `kinds_off: "d10" appears twice`},
		{"no kinds order", Overrides{KindsPriority: &[]string{}}, "kinds_priority: [agency_partner, unsupported, d10]", "kinds_priority: []", "kinds_priority is empty"},
		{"unknown kind in order", Overrides{KindsPriority: &[]string{"hand"}}, "kinds_priority: [agency_partner, unsupported, d10]", "kinds_priority: [hand]", `kinds_priority: "hand" is not one of`},
		{"negative rank", Overrides{SkipRankOver: ptr(-1)}, "skip_rank_over: 0", "skip_rank_over: -1", "skip_rank_over must be 0 (off) or more, not -1"},
		{"unknown batch source", Overrides{BatchesFrom: ptr("everything")}, "batches_from: starred", "batches_from: everything", `batches_from: "everything" is not one of starred, starred_then_rest`},
		{"bad date", Overrides{Facts: &FactsOverrides{D10EOL: ptr(ParseDate("soon"))}}, "d10_eol: 2026-12-09", "d10_eol: soon", `facts.d10_eol: "soon" is not a date`},
		{"blank date", Overrides{Facts: &FactsOverrides{D7EOL: ptr(ParseDate(""))}}, "d7_eol: 2025-01-05", `d7_eol: ""`, "facts.d7_eol is missing"},
		{"negative calls", Overrides{Relevance: &RelevanceOverrides{LeadsDailyCalls: ptr(-5)}}, "leads_daily_calls: 0", "leads_daily_calls: -5", "relevance.leads_daily_calls must be 0 (the server's cap) or more, not -5"},
		{"unknown catch-all rule", Overrides{Verify: &VerifyOverrides{CatchAll: ptr("maybe")}}, "catch_all: send", "catch_all: maybe", `verify.catch_all: "maybe" is not one of send, block`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := demoConfig(t)
			p := tc.o.Validate(base)
			if len(p) != 1 || !strings.Contains(p[0].Message, tc.want) {
				t.Fatalf("Validate = %v, want one problem naming %q", p, tc.want)
			}
			if eff, _ := tc.o.Effective(base); !reflect.DeepEqual(eff, Config{}) {
				t.Error("Effective returned a config with problems")
			}
			dir := demoDir(t)
			replaceIn(t, dir, FileName, tc.old, tc.new)
			_, err := Load(dir)
			wantProblem(t, err, p[0].Message)
		})
	}
}

// TestOverridesValidateReadsTemplates: an override that puts a kind back in the order needs
// that kind's templates, exactly as Load would; Effective carries the templates it read.
func TestOverridesValidateReadsTemplates(t *testing.T) {
	dir := demoDir(t)
	replaceIn(t, dir, FileName, "kinds_priority: [agency_partner, unsupported, d10]", "kinds_priority: [agency_partner, unsupported]")
	base, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := base.Templates.Set(KindD10, "en"); ok {
		t.Fatal("the file without d10 loaded its templates")
	}
	o := Overrides{KindsPriority: &[]string{KindD10, KindUnsupported, KindAgencyPartner}}
	eff, p := o.Effective(base)
	if len(p) != 0 {
		t.Fatalf("Effective = %v", p)
	}
	if set, ok := eff.Templates.Set(KindD10, "en"); !ok || len(set.Steps) != Steps {
		t.Errorf("effective d10 templates = %+v, %v", set, ok)
	}
	if err := os.Remove(filepath.Join(dir, "leads-templates/d10/en/0.txt")); err != nil {
		t.Fatal(err)
	}
	p = o.Validate(base)
	if len(p) != 1 || !strings.Contains(p[0].Message, "template d10/en/0.txt is missing") {
		t.Errorf("Validate = %v, want the missing d10 template", p)
	}
}

func TestLeadsCapNotOverEnv(t *testing.T) {
	cases := []struct {
		calls, envCap int
		want          string
		inForce       int
	}{
		{0, 10000, "", 10000},
		{500, 10000, "", 500},
		{500, 500, "", 500},
		{501, 500, "The server allows at most 500 a day; raise it in .env first.", 500},
	}
	for _, tc := range cases {
		cfg := Overrides{Relevance: &RelevanceOverrides{LeadsDailyCalls: ptr(tc.calls)}}.Apply(Config{})
		p := cfg.LeadsCapProblems(tc.envCap)
		switch {
		case tc.want == "" && len(p) != 0:
			t.Errorf("%d calls under a %d cap: %v, want none", tc.calls, tc.envCap, p)
		case tc.want != "" && (len(p) != 1 || p[0].Message != tc.want):
			t.Errorf("%d calls under a %d cap: %v, want %q", tc.calls, tc.envCap, p, tc.want)
		}
		if got := cfg.Relevance.DailyCalls(tc.envCap); got != tc.inForce {
			t.Errorf("%d calls under a %d cap: %d in force, want %d", tc.calls, tc.envCap, got, tc.inForce)
		}
	}
}

// TestNewFileKeys: the queue and relevance keys default when left out, read when set, and
// LoadForProbe's strict decode accepts them.
func TestNewFileKeys(t *testing.T) {
	dir := demoDir(t)
	for _, key := range []string{"kinds_off:", "skip_rank_over:", "batches_from:", "relevance:", "  rating:", "  leads_daily_calls:"} {
		removeLine(t, dir, key)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.KindsOff) != 0 || cfg.SkipRankOver != 0 || cfg.BatchesFrom() != BatchesStarred ||
		!cfg.Relevance.RatingOn() || cfg.Relevance.DailyCalls(10000) != 10000 {
		t.Errorf("defaults: kinds_off %v, skip %d, batches %q, rating %v, calls %d",
			cfg.KindsOff, cfg.SkipRankOver, cfg.BatchesFrom(), cfg.Relevance.RatingOn(), cfg.Relevance.DailyCalls(10000))
	}

	dir = demoDir(t)
	replaceIn(t, dir, FileName, "kinds_off: []", "kinds_off: [d10, agency_partner]")
	replaceIn(t, dir, FileName, "skip_rank_over: 0", "skip_rank_over: 500000")
	replaceIn(t, dir, FileName, "batches_from: starred", "batches_from: starred_then_rest")
	replaceIn(t, dir, FileName, "rating: true", "rating: false")
	replaceIn(t, dir, FileName, "leads_daily_calls: 0", "leads_daily_calls: 300")
	for name, load := range map[string]func(string) (Config, error){"Load": Load, "LoadForProbe": LoadForProbe} {
		cfg, err := load(dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(cfg.KindsOff, []string{KindD10, KindAgencyPartner}) || cfg.SkipRankOver != 500000 ||
			cfg.BatchesFrom() != BatchesStarredThenRest || cfg.Relevance.RatingOn() || cfg.Relevance.DailyCalls(10000) != 300 {
			t.Errorf("%s: kinds_off %v, skip %d, batches %q, rating %v, calls %d", name,
				cfg.KindsOff, cfg.SkipRankOver, cfg.BatchesFrom(), cfg.Relevance.RatingOn(), cfg.Relevance.DailyCalls(10000))
		}
	}

	// A bad value is a problem for Load only: the probe commands do not read these keys.
	dir = demoDir(t)
	replaceIn(t, dir, FileName, "batches_from: starred", "batches_from: all")
	replaceIn(t, dir, FileName, "skip_rank_over: 0", "skip_rank_over: -3")
	_, err = Load(dir)
	wantProblem(t, err, `batches_from: "all"`, "skip_rank_over must be 0 (off) or more, not -3")
	if _, err := LoadForProbe(dir); err != nil {
		t.Errorf("LoadForProbe checked a queue key: %v", err)
	}
	dir = demoDir(t)
	replaceIn(t, dir, FileName, "  rating: true", "  rating: maybe")
	_, err = LoadForProbe(dir)
	wantProblem(t, err, "maybe")
}

// removeLine drops the first line of the demo file that starts with prefix.
func removeLine(t *testing.T, dir, prefix string) {
	t.Helper()
	path := filepath.Join(dir, FileName)
	b, err := os.ReadFile(path) // #nosec G304 -- test data
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(b), "\n")
	i := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, prefix) })
	if i < 0 {
		t.Fatalf("%s has no line starting %q", FileName, prefix)
	}
	if err := os.WriteFile(path, []byte(strings.Join(slices.Delete(lines, i, i+1), "")), 0o600); err != nil { // #nosec G703 -- test data in a temp dir
		t.Fatal(err)
	}
}

// TestVerifyCatchAll: verify.catch_all is send when left out, block when saved so, and an
// override replaces the file's value.
func TestVerifyCatchAll(t *testing.T) {
	base := demoConfig(t)
	if got := base.Verify.CatchAllRule(); got != CatchAllSend {
		t.Errorf("file: %q, want send", got)
	}
	if got := (Verify{}).CatchAllRule(); got != CatchAllSend {
		t.Errorf("left out: %q, want send", got)
	}
	eff, p := Overrides{Verify: &VerifyOverrides{CatchAll: ptr(CatchAllBlock)}}.Effective(base)
	if len(p) != 0 || eff.Verify.CatchAllRule() != CatchAllBlock || base.Verify.CatchAllRule() != CatchAllSend {
		t.Errorf("override: %q %v (file now %q)", eff.Verify.CatchAllRule(), p, base.Verify.CatchAllRule())
	}
}
