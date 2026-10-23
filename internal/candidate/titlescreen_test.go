package candidate

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func rulesOf(include, exclude []string) Config {
	return Config{TitleScreenRules: TitleRules{Include: include, Exclude: exclude}}
}

func screenOf(include, exclude []string) func(string) bool {
	return rulesOf(include, exclude).TitleScreen()
}

// TestTitleCheckMatching: whole-word matching, exclude winning, and TitleScreen agreeing
// with verdict != TitleDrop on every case.
func TestTitleCheckMatching(t *testing.T) {
	cases := []struct {
		name             string
		include, exclude []string
		title            string
		want             TitleVerdict
	}{
		{"go whole word", []string{"go"}, nil, "Senior Go Engineer", TitleKeep},
		{"go not in google", []string{"go"}, nil, "Google Ads Specialist", TitleUnsure},
		{"go not in cargo", []string{"go"}, nil, "Cargo Coordinator", TitleUnsure},
		{"go not in golang", []string{"go"}, nil, "Golang Developer", TitleUnsure},
		{"golang whole word", []string{"golang"}, nil, "Golang Developer", TitleKeep},
		{"go after slash", []string{"go"}, nil, "Backend Engineer (Python/Go)", TitleKeep},
		{"go before comma", []string{"go"}, nil, "Engineer - Go, Rust", TitleKeep},
		{"ai not in maintain", []string{"ai"}, nil, "Maintainer, Build Systems", TitleUnsure},
		{"ai not in maintenance", []string{"ai"}, nil, "Maintenance Technician", TitleUnsure},
		{"ai engineer", []string{"ai"}, nil, "AI Engineer", TitleKeep},
		{"ai phrase", []string{"ai engineer"}, nil, "Senior AI Engineer | Remote", TitleKeep},
		{"ai in parens", []string{"ai"}, nil, "Software Engineer (AI)", TitleKeep},
		{"hyphen title, space pattern", []string{"full stack"}, nil, "Full-Stack Developer", TitleKeep},
		{"space title, hyphen pattern", []string{"full-stack"}, nil, "Full Stack Developer", TitleKeep},
		{"phrase needs both words in order", []string{"full stack"}, nil, "Stack Full Developer", TitleUnsure},
		{"phrase not across a partial word", []string{"full stack"}, nil, "Full Stacks", TitleUnsure},
		{"no include match", []string{"engineer"}, nil, "Account Executive", TitleUnsure},
		{"exclude wins", []string{"engineer"}, []string{"sales"}, "Sales Engineer", TitleDrop},
		{"exclude whole word only", []string{"engineer"}, []string{"sales"}, "Salesforce Engineer", TitleKeep},
		{"exclude phrase", []string{"success"}, []string{"customer success"}, "Customer Success Manager", TitleDrop},
		{"mixed case", []string{"DevOps"}, nil, "dEVoPS lead", TitleKeep},
		{"pattern mixed case", []string{"Tech Lead"}, nil, "TECH LEAD, Payments", TitleKeep},
		{"fullwidth letters", []string{"go"}, nil, "Ｇｏ Ｅｎｇｉｎｅｅｒ", TitleKeep},
		{"fullwidth not partial", []string{"go"}, nil, "Ｇｏｏｇｌｅ", TitleUnsure},
		{"empty title", []string{"go"}, nil, "", TitleUnsure},
		{"punctuation only title", []string{"go"}, nil, " / | ", TitleUnsure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := rulesOf(tc.include, tc.exclude)
			if got := cfg.TitleCheck()(tc.title); got != tc.want {
				t.Errorf("check(%q) = %v, want %v", tc.title, got, tc.want)
			}
			if got, want := cfg.TitleScreen()(tc.title), tc.want != TitleDrop; got != want {
				t.Errorf("screen(%q) = %v, want %v", tc.title, got, want)
			}
		})
	}
}

func TestNormaliseTitle(t *testing.T) {
	cases := map[string]string{
		"":                      " ",
		"Go":                    " go ",
		"  Full-Stack / (Go)  ": " full stack go ",
		"Ｇｏ｜ＡＩ":                 " go ai ",
		"Ingénieur Logiciel":    " ingénieur logiciel ",
		"C# .NET":               " c net ",
	}
	for in, want := range cases {
		if got := normaliseTitle(in); got != want {
			t.Errorf("normaliseTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNoScreenKeepsAll: no title_screen, or an empty include list, keeps every title.
func TestNoScreenKeepsAll(t *testing.T) {
	for name, cfg := range map[string]Config{
		"zero config":   {},
		"empty include": rulesOf([]string{}, nil),
		"exclude only":  rulesOf(nil, []string{"sales"}),
	} {
		screen, check := cfg.TitleScreen(), cfg.TitleCheck()
		if screen == nil || check == nil {
			t.Fatalf("%s: TitleScreen() or TitleCheck() returned nil", name)
		}
		for _, title := range []string{"", "Sales Manager", "Go Engineer"} {
			if !screen(title) || check(title) != TitleKeep {
				t.Errorf("%s: %q not kept (check = %v)", name, title, check(title))
			}
		}
	}
}

func TestTitleVerdictString(t *testing.T) {
	for v, want := range map[TitleVerdict]string{TitleKeep: "keep", TitleDrop: "drop", TitleUnsure: "unsure", 7: "TitleVerdict(7)"} {
		if got := v.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(v), got, want)
		}
	}
}

// TestTitleTypos: a one-word include of 6+ letters matches a title word of 6+ letters one
// Damerau-Levenshtein edit away. One-letter plurals and inflections are a Keep through this
// rule by design ("engineers", "ingeniera"). Short words, phrases and excludes are exact.
func TestTitleTypos(t *testing.T) {
	include := []string{"engineer", "developer", "go", "ai", "golang", "full stack", "devops"}
	exclude := []string{"recruiter", "marketing"}
	cases := []struct {
		title string
		want  TitleVerdict
	}{
		{"Senior Enginer", TitleKeep},            //nolint:misspell // deliberate typo: deletion
		{"Engneer, Payments", TitleKeep},         // deletion
		{"Software Engineeer", TitleKeep},        //nolint:misspell // deliberate typo: insertion
		{"Devloper", TitleKeep},                  // deletion
		{"Developre", TitleKeep},                 // adjacent swap
		{"Enigneer", TitleKeep},                  //nolint:misspell // deliberate typo: adjacent swap
		{"Developar", TitleKeep},                 // substitution
		{"Engineers Wanted", TitleKeep},          // plural: one insertion, kept by design
		{"Developers", TitleKeep},                // plural
		{"Golnag Developer", TitleKeep},          // swap on a 6-letter include
		{"Engieer", TitleKeep},                   // 7 letters, one deletion
		{"Enginr", TitleUnsure},                  // two edits
		{"Developmental Biologist", TitleUnsure}, // far off
		{"Engineering Manager", TitleUnsure},     // three edits from engineer
		{"Ga Engineer", TitleKeep},               // kept by engineer, not by go
		{"Ga Specialist", TitleUnsure},           // go never fuzzes
		{"Og Lead", TitleUnsure},                 // go never fuzzes (swap)
		{"Gos Lead", TitleUnsure},                // go never fuzzes (insertion)
		{"Al Lead", TitleUnsure},                 // ai never fuzzes
		{"Devop Lead", TitleUnsure},              // title word under 6 letters never fuzzes
		{"Fulll Stack", TitleUnsure},             // phrases are exact
		{"Recruitr, Engineer", TitleKeep},        // exclude is exact: the typo does not drop
		{"Engineer Recruiter", TitleDrop},        // exact exclude wins over include
		{"Marketting Engineer", TitleKeep},       //nolint:misspell // deliberate typo: exclude typo does not drop
		{"Ｅｎｇｉｎｅｒ", TitleKeep},                   // fuzzy after NFKC
	}
	check := rulesOf(include, exclude).TitleCheck()
	for _, tc := range cases {
		if got := check(tc.title); got != tc.want {
			t.Errorf("check(%q) = %v, want %v", tc.title, got, tc.want)
		}
	}
}

func TestWithinOneEdit(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"abcdef", "abcdef", true},
		{"abcdef", "abcdeg", true},
		{"abcdef", "bacdef", true},
		{"abcdef", "abcdfe", true},
		{"abcdef", "abcde", true},
		{"abcdef", "bcdef", true},
		{"abcdef", "abcdefg", true},
		{"abcdef", "xabcdef", true},
		{"abcdef", "badcef", false},
		{"abcdef", "abcd", false},
		{"abcdef", "acbdfe", false},
		{"abcdef", "abcfed", false},
		{"développeur", "developpeur", true},
	}
	for _, tc := range cases {
		if got := withinOneEdit([]rune(tc.a), []rune(tc.b)); got != tc.want {
			t.Errorf("withinOneEdit(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := withinOneEdit([]rune(tc.b), []rune(tc.a)); got != tc.want {
			t.Errorf("withinOneEdit(%q, %q) = %v, want %v (reversed)", tc.b, tc.a, got, tc.want)
		}
	}
}

// broadList is a generic, broad engineering screen with role words in five languages, as a
// real profile would carry.
var broadList = TitleRules{
	Include: []string{"engineer", "engineering", "developer", "architect", "backend", "programmer", "software",
		"full stack", "fullstack", "platform", "sre", "devops", "tech lead", "cto", "ai", "llm", "go", "golang", "php",
		"ingénieur", "ingenieur", "développeur", "developpeur", "entwickler", "softwareentwickler", "programmierer",
		"desarrollador", "ingeniero", "sviluppatore", "ingegnere", "programmatore", "inginer", "dezvoltator", "programator"},
	Exclude: []string{"sales", "account executive", "recruiter", "talent acquisition", "marketing", "customer success",
		"legal", "nurse", "driver", "warehouse", "ai trainer", "annotator"},
}

func TestBroadListVerdicts(t *testing.T) {
	check := Config{TitleScreenRules: broadList}.TitleCheck()
	for title, want := range map[string]TitleVerdict{
		"Member of Technical Staff":           TitleUnsure,
		"Craftsperson, Payments":              TitleUnsure,
		"Solutions Consultant":                TitleUnsure,
		"Product Owner":                       TitleUnsure,
		"Data Scientist":                      TitleUnsure,
		"Software Craftsman":                  TitleKeep, // "software" is an include
		"Ingénieur logiciel":                  TitleKeep,
		"Ingenieur Logiciel Senior":           TitleKeep,
		"Ingénieure DevOps":                   TitleKeep, // feminine: one insertion
		"Développeuse Back-end":               TitleUnsure,
		"Développeur Full-Stack (H/F)":        TitleKeep,
		"Softwareentwickler (m/w/d)":          TitleKeep,
		"Entwickler:in Backend":               TitleKeep,
		"Senior PHP-Entwickler":               TitleKeep,
		"Programmierer C++":                   TitleKeep,
		"Desarrollador/a Backend":             TitleKeep,
		"Desarrolladora Senior":               TitleKeep, // one insertion
		"Ingeniera de Software":               TitleKeep,
		"Sviluppatore Senior":                 TitleKeep,
		"Ingegnere del Software":              TitleKeep,
		"Inginer Software":                    TitleKeep,
		"Dezvoltator Backend":                 TitleKeep,
		"Sales Engineer":                      TitleDrop,
		"Technical Recruiter, Engineering":    TitleDrop,
		"AI Trainer (Freelance)":              TitleDrop,
		"Customer Success Manager":            TitleDrop,
		"Delivery Driver":                     TitleDrop,
		"Google Ads Specialist":               TitleUnsure,
		"Senior Backend Engineer (Go/Python)": TitleKeep,
	} {
		if got := check(title); got != want {
			t.Errorf("check(%q) = %v, want %v", title, got, want)
		}
	}
}

func TestDemoTitleScreen(t *testing.T) {
	cfg, err := LoadFile(demoPath)
	if err != nil {
		t.Fatal(err)
	}
	check, screen := cfg.TitleCheck(), cfg.TitleScreen()
	for title, want := range map[string]TitleVerdict{
		"Senior Rust Engineer":         TitleKeep,
		"Backend Developer (Rust/Go)":  TitleKeep,
		"Site Reliability Engineer":    TitleKeep,
		"SRE":                          TitleKeep,
		"Platform Engineer - Internal": TitleKeep,
		"Rust-Entwickler (m/w/d)":      TitleKeep,
		"Développeur Rust":             TitleKeep,
		"Ingeniero de Sistemas":        TitleKeep,
		"Member of Technical Staff":    TitleUnsure,
		"Sales Engineer":               TitleDrop,
		"Account Executive, DACH":      TitleDrop,
		"Technical Recruiter":          TitleDrop,
		"Customer Success Manager":     TitleDrop,
		"Warehouse Associate":          TitleDrop,
		"Head of Marketing":            TitleDrop,
	} {
		if got := check(title); got != want {
			t.Errorf("demo check(%q) = %v, want %v", title, got, want)
		}
		if got := screen(title); got != (want != TitleDrop) {
			t.Errorf("demo screen(%q) = %v, want %v", title, got, want != TitleDrop)
		}
	}
}

// TestTitleScreenConcurrent: one compiled screen is shared by many goroutines (run with
// -race).
func TestTitleScreenConcurrent(t *testing.T) {
	screen := screenOf([]string{"engineer", "go"}, []string{"sales"})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 200 {
				if !screen("Go Engineer") || screen("Sales Engineer") {
					t.Errorf("goroutine %d: wrong verdict", i)
					return
				}
			}
		})
	}
	wg.Wait()
}

// TestTitleScreenInvalid: the loader refuses unknown keys, empty, non-plain and duplicate
// patterns.
func TestTitleScreenInvalid(t *testing.T) {
	demo := demoBytes(t)
	const old = "title_screen:\n"
	cases := []struct {
		name, block, want string
	}{
		{"unknown key", "title_screen:\n  include: [go]\n  regex: [x]\n", "regex"},
		{"empty pattern", "title_screen:\n  include: [go, \"\"]\n", "title_screen.include has an empty pattern"},
		{"blank pattern", "title_screen:\n  include: [go]\n  exclude: [\"  \"]\n", "title_screen.exclude has an empty pattern"},
		{"regex pattern", "title_screen:\n  include: [\"go.*\"]\n", "not a plain word"},
		{"alternation", "title_screen:\n  include: [\"go|rust\"]\n", "not a plain word"},
		{"word boundary escape", "title_screen:\n  include: [\"\\\\bgo\\\\b\"]\n", "not a plain word"},
		{"hyphen only", "title_screen:\n  include: [\"-\"]\n", "not a plain word"},
		{"duplicate", "title_screen:\n  include: [go, Go]\n", "listed twice"},
		{"duplicate after normalising", "title_screen:\n  include: [full stack, Full-Stack]\n", "listed twice"},
		{"in both lists", "title_screen:\n  include: [go, sales]\n  exclude: [sales]\n", "also in title_screen.include"},
		{"exclude without include", "title_screen:\n  exclude: [sales]\n", "needs an include list"},
		{"not a mapping", "title_screen: [go]\n", "TitleRules"},
	}
	idx := strings.Index(demo, old)
	if idx < 0 {
		t.Fatal("demo has no title_screen")
	}
	base := demo[:idx]
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(base + tc.block))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrInvalid naming %q", err, tc.want)
			}
		})
	}
	// Valid variations load.
	for name, block := range map[string]string{
		"absent":        "",
		"empty mapping": "title_screen: {}\n",
		"include only":  "title_screen:\n  include: [go, full-stack, ai engineer]\n",
	} {
		if _, err := Parse([]byte(base + block)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// BenchmarkTitleCheck: one title through a realistic screen (about 45 patterns, five
// languages), mixing exact keeps, drops, typo keeps and unsure titles (the slowest path:
// every exact and fuzzy pattern is tried).
func BenchmarkTitleCheck(b *testing.B) {
	check := Config{TitleScreenRules: broadList}.TitleCheck()
	titles := []string{
		"Senior Backend Engineer (Go/PostgreSQL) - Remote, EU",
		"Account Executive, Mid-Market DACH",
		"Staff Sofware Enginer, AI Platform | Hybrid", //nolint:misspell // deliberate typos
		"Member of Technical Staff, Applied Research Group",
	}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		check(titles[i%len(titles)])
	}
}
