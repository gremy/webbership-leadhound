package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// directLeadsReaders are the files allowed to read leads.yaml past the settings door (a
// leadsEnv call outside leadsOpen and backend.leadsSetup, or leadconfig.Load or LoadForProbe
// outside leadsEnv), each with its reason: none reads a knob Settings may change (D11).
var directLeadsReaders = map[string]string{
	"leads_send.go":        "reads only knobs Settings shows read-only: caps, the ramp, the schedule, the sender, channels and templates (D11; plan B routes sending through settings)",
	"leads_sites.go":       "probe-only knobs: site_lists, probe and the probe rate",
	"leads_contacts.go":    "probe-only knobs: role_local_parts, public_body_suffixes and the probe rate",
	"leads_agencies.go":    "probe-only knobs: agency_finding_pattern and the probe rate",
	"leads_enrich.go":      "probe-only knobs: the probe rate and concurrency",
	"leads_checkconfig.go": "check-config checks leads.yaml itself, as the file stands",
}

// leadsDoors are the functions that read leads.yaml through leadsEnv and must apply the
// saved settings before anyone reads the result.
var leadsDoors = []string{"leadsOpen", "backend.leadsSetup"}

// funcName is fn's name, with its receiver's type for a method: "backend.leadsSetup".
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// leadsRead is one direct read of leads.yaml in the package source.
type leadsRead struct {
	file, caller, call string
	line               int
}

// directLeadsReads lists every leadsEnv call and every leadconfig.Load or LoadForProbe call
// in the package's non-test source, with the function it sits in, and which functions call
// applyLeadSettings.
func directLeadsReads(t *testing.T) (reads []leadsRead, applies map[string]bool) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	applies = map[string]bool{}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			caller := "(package level)"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				caller = funcName(fn)
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					name = fun.Name
				case *ast.SelectorExpr:
					if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "leadconfig" {
						name = "leadconfig." + fun.Sel.Name
					}
				}
				switch name {
				case "applyLeadSettings":
					applies[caller] = true
				case "leadsEnv", "leadconfig.Load", "leadconfig.LoadForProbe":
					reads = append(reads, leadsRead{file: path, caller: caller, call: name, line: fset.Position(call.Pos()).Line})
				}
				return true
			})
		}
	}
	return reads, applies
}

// TestSettingsReachEveryDoor (Task 5.1, D10 and D11): every reader of leads.yaml sees the
// effective config. A leadsEnv call sits in leadsOpen or backend.leadsSetup, which apply the
// saved settings, or in a file directLeadsReaders names with its reason; leadconfig.Load
// and LoadForProbe sit in leadsEnv or such a file. The allowlist holds no file that no
// longer reads directly.
func TestSettingsReachEveryDoor(t *testing.T) {
	reads, applies := directLeadsReads(t)
	if len(reads) == 0 {
		t.Fatal("no leadsEnv caller found: the sweep reads nothing")
	}
	used := map[string]bool{}
	for _, r := range reads {
		door := r.call == "leadsEnv" && slices.Contains(leadsDoors, r.caller) ||
			r.call != "leadsEnv" && r.caller == "leadsEnv"
		if door {
			continue
		}
		if _, ok := directLeadsReaders[r.file]; ok {
			used[r.file] = true
			continue
		}
		t.Errorf("%s:%d: %s calls %s and so reads leads.yaml without the console's settings; go through leadsOpen or backend.leadsSetup, or add %s to directLeadsReaders with the reason it reads no knob Settings may change",
			r.file, r.line, r.caller, r.call, r.file)
	}
	for _, d := range leadsDoors {
		if !applies[d] {
			t.Errorf("%s does not call applyLeadSettings: its readers would see the file alone", d)
		}
	}
	for file, why := range directLeadsReaders {
		if why == "" {
			t.Errorf("directLeadsReaders: %s has no reason", file)
		}
		if !used[file] {
			t.Errorf("directLeadsReaders: %s no longer reads leads.yaml directly; take it off the list", file)
		}
	}
}

// probeOnlyDir is a copy of the examples whose leads.yaml passes LoadForProbe but not Load:
// batches_from names no source, a part only Load checks.
func probeOnlyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../examples")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, leadconfig.FileName)
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	out := regexp.MustCompile(`(?m)^batches_from: \S+`).ReplaceAll(raw, []byte("batches_from: nowhere"))
	if string(out) == string(raw) {
		t.Fatal("leads.yaml batches_from not rewritten")
	}
	if err := os.WriteFile(path, out, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	return dir
}

// TestStaleOverrideCountsOnlyItsOwnProblems: under a probe load, a part of leads.yaml only
// Load checks may already fail on its own. Saved settings are stale only for the problems
// they bring: a sound document still applies, and a broken one is ignored for its own
// problem alone.
func TestStaleOverrideCountsOnlyItsOwnProblems(t *testing.T) {
	dir := probeOnlyDir(t)
	if _, err := leadconfig.Load(dir); err == nil {
		t.Fatal("the probe-only file passes Load")
	}
	file, err := leadconfig.LoadForProbe(dir)
	if err != nil {
		t.Fatal(err)
	}
	eff, problems := effectiveLeads([]byte(`{"kinds_off":["d10"],"skip_rank_over":500}`), file)
	if len(problems) > 0 {
		t.Fatalf("sound settings refused for the file's own problem: %v", problems)
	}
	if !slices.Equal(eff.KindsOff, []string{"d10"}) || eff.SkipRankOver != 500 {
		t.Errorf("effective kinds_off %v, skip_rank_over %d", eff.KindsOff, eff.SkipRankOver)
	}
	if file.SkipRankOver == 500 || slices.Contains(file.KindsOff, "d10") {
		t.Error("applying the settings changed the file's config")
	}
	_, problems = effectiveLeads([]byte(`{"skip_rank_over":-3}`), file)
	if len(problems) != 1 || !strings.Contains(problems[0].Message, "skip_rank_over") {
		t.Errorf("broken settings: %v, want the skip_rank_over problem alone", problems)
	}
}

// TestJevLeadsDailyCap (D9): the leads cap in force is the setting when above 0, never above
// LEADHOUND_JEV_LEADS_DAILY_CALL_CAP; 0 is the server's cap.
func TestJevLeadsDailyCap(t *testing.T) {
	for _, c := range []struct{ setting, env, want int }{
		{0, 10000, 10000},
		{50, 10000, 50},
		{20000, 10000, 10000},
		{10000, 10000, 10000},
		{50, 0, 0},
	} {
		var s leadsSetup
		s.leads.Relevance.LeadsDailyCalls, s.jevLeadsEnvCap = c.setting, c.env
		if got := s.jevLeadsDailyCap(); got != c.want {
			t.Errorf("setting %d, env %d: cap %d, want %d", c.setting, c.env, got, c.want)
		}
	}
}
