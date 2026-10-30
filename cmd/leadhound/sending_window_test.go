package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// windowReadersAllowed are the only functions that may read schedule.business_hours straight
// off a config (X.Schedule.BusinessHours): the one reader of the window in force and the
// config's own plumbing (its clone, its checks, the console's settings applied over it).
var windowReadersAllowed = []string{
	"internal/leadconfig/leadconfig.go:Config.SendingWindow",
	"internal/leadconfig/leadconfig.go:Config.Clone",
	"internal/leadconfig/leadconfig.go:Config.checkRest",
	"internal/leadconfig/overrides.go:Overrides.Apply",
}

// windowRead is one X.Schedule.BusinessHours in the module's source.
type windowRead struct {
	at   string // file:function, as windowReadersAllowed names it
	line int
}

// windowReads lists every X.Schedule.BusinessHours selector in the non-test Go source under
// root, with the file (relative to root) and function it sits in.
func windowReads(t *testing.T, root string) []windowRead {
	t.Helper()
	var out []windowRead
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			caller := "(package level)"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				caller = funcName(fn)
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "BusinessHours" {
					return true
				}
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Schedule" {
					out = append(out, windowRead{at: rel + ":" + caller, line: fset.Position(sel.Pos()).Line})
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSendingWindowOneReader (Task 4.1): every reader of the sending window takes the one in
// force through leadconfig.Config.SendingWindow (saved on Controls, else leads.yaml's), so the
// send, the batch sizing, the sentence and Controls can never disagree. Any other
// X.Schedule.BusinessHours in the module's source fails here.
func TestSendingWindowOneReader(t *testing.T) {
	reads := windowReads(t, "../..")
	found := false
	for _, r := range reads {
		if r.at == windowReadersAllowed[0] {
			found = true
		}
		if !slices.Contains(windowReadersAllowed, r.at) {
			t.Errorf("%s (line %d) reads Schedule.BusinessHours directly; take the window in force with cfg.SendingWindow()", r.at, r.line)
		}
	}
	if !found {
		t.Fatal("the sweep found no read in Config.SendingWindow: it reads nothing")
	}
}

// TestSendingWindowSweepCatchesARead: the sweep sees a direct read in a file of its own.
func TestSendingWindowSweepCatchesARead(t *testing.T) {
	dir := t.TempDir()
	src := "package x\n\nfunc f(cfg struct{ Schedule struct{ BusinessHours int } }) int { return cfg.Schedule.BusinessHours }\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	reads := windowReads(t, dir)
	if len(reads) != 1 || reads[0].at != "x.go:f" {
		t.Errorf("reads %v, want one in x.go:f", reads)
	}
}

// TestSettingsWindowSection: the window saved from the Controls form is the one in force
// (Apply), a reset of its section drops it, and the form's mistakes are refused in plain
// words before anything is merged.
func TestSettingsWindowSection(t *testing.T) {
	var o leadconfig.Overrides
	v := console.LeadSettingsValues{WindowStart: "08:30", WindowEnd: "18:00", WindowDays: []string{"Fri", "Mon"}}
	if p := setSection(&o, console.SettingsWindow, v); len(p) != 0 {
		t.Fatalf("a sound window refused: %v", p)
	}
	file := leadconfig.Config{}
	file.Schedule.BusinessHours = leadconfig.BusinessHours{Start: "09:00", End: "16:30", Days: []string{"Mon", "Tue"}}
	got := o.Apply(file).SendingWindow()
	if got.Start != "08:30" || got.End != "18:00" || !slices.Equal(got.Days, []string{"Mon", "Fri"}) {
		t.Errorf("window in force %+v, want 08:30-18:00 Mon, Fri", got)
	}
	if !overrideKeys(o)["schedule.business_hours"] {
		t.Error("the saved window is not counted as the console's")
	}
	if fw := file.SendingWindow(); fw.Start != "09:00" {
		t.Errorf("the file's window changed: %+v", fw)
	}
	clearSection(&o, console.SettingsWindow)
	if o != (leadconfig.Overrides{}) || o.Apply(file).SendingWindow().Start != "09:00" {
		t.Error("a reset left the window saved")
	}
	for _, c := range []struct {
		name string
		v    console.LeadSettingsValues
		want string
	}{
		{"not a time", console.LeadSettingsValues{WindowStart: "8.30", WindowEnd: "18:00", WindowDays: []string{"Mon"}}, "a time like 08:30"},
		{"end before start", console.LeadSettingsValues{WindowStart: "18:00", WindowEnd: "08:30", WindowDays: []string{"Mon"}}, "end must come after the start"},
		{"same time", console.LeadSettingsValues{WindowStart: "09:00", WindowEnd: "09:00", WindowDays: []string{"Mon"}}, "end must come after the start"},
		{"no day", console.LeadSettingsValues{WindowStart: "08:30", WindowEnd: "18:00"}, "at least one day"},
		{"not a day", console.LeadSettingsValues{WindowStart: "08:30", WindowEnd: "18:00", WindowDays: []string{"Mon", "Funday"}}, "not a day of the week"},
	} {
		var o leadconfig.Overrides
		p := setSection(&o, console.SettingsWindow, c.v)
		if len(p) == 0 || !strings.Contains(strings.Join(p, " "), c.want) {
			t.Errorf("%s: problems %q, want %q", c.name, p, c.want)
		}
		if o.Schedule != nil {
			t.Errorf("%s: a refused window was set", c.name)
		}
	}
}
