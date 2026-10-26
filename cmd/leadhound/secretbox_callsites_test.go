package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// MB-R4: the console process never holds the private key, so the code that opens a sealed
// password or loads the key lives only in the files the workers run. Tests are skipped: the
// leak sweep opens seals in tests.

const secretboxImport = "github.com/gremy/webbership-leadhound/internal/secretbox" //nolint:gosec // G101: an import path, not a credential

// secretboxAllowed names, per secretbox function, the only non-test files of this package
// that may refer to it.
var secretboxAllowed = map[string][]string{
	"Open":           {"leads_senders.go", "mailboxcore.go"},
	"LoadPrivateKey": {"leads_send.go", "leads_sync.go", "leads_selftest.go", "mailboxcore.go"},
	"ReadPrivateKey": {"leads_mailboxes.go"},
}

// secretboxSites lists every reference to a guarded secretbox function in the non-test Go
// files of dir, as "file:line: secretbox.Name", by function. A dot import of secretbox is
// listed as "file:line: dot import", since its calls cannot be told apart.
func secretboxSites(t *testing.T, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sites := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		// Every name the file imports secretbox under: a file may import it twice, once
		// renamed (serve.go itself imports it to seal).
		locals := map[string]bool{}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p != secretboxImport {
				continue
			}
			local := "secretbox"
			if imp.Name != nil {
				local = imp.Name.Name
			}
			switch local {
			case ".":
				sites["dot import"] = append(sites["dot import"], fmt.Sprintf("%s:%d: dot import", name, fset.Position(imp.Pos()).Line))
			case "_":
			default:
				locals[local] = true
			}
		}
		if len(locals) == 0 {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && locals[id.Name] {
				if _, guarded := secretboxAllowed[sel.Sel.Name]; guarded {
					sites[sel.Sel.Name] = append(sites[sel.Sel.Name],
						fmt.Sprintf("%s:%d: secretbox.%s", name, fset.Position(sel.Pos()).Line, sel.Sel.Name))
				}
			}
			return true
		})
	}
	return sites
}

// secretboxViolations is every site outside its function's allowlist, sorted.
func secretboxViolations(sites map[string][]string) []string {
	var out []string
	for fn, list := range sites {
		for _, s := range list {
			file, _, _ := strings.Cut(s, ":")
			if !slices.Contains(secretboxAllowed[fn], file) {
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestSecretboxCallSites: secretbox.Open, LoadPrivateKey and ReadPrivateKey appear only in
// their allowlisted files, and the scan does find the calls that are there.
func TestSecretboxCallSites(t *testing.T) {
	sites := secretboxSites(t, ".")
	if v := secretboxViolations(sites); len(v) > 0 {
		t.Errorf("secretbox used outside the worker files (MB-R4):\n%s", strings.Join(v, "\n"))
	}
	// The scan is not blind: the known calls are found.
	for fn, file := range map[string]string{"Open": "leads_senders.go", "LoadPrivateKey": "mailboxcore.go", "ReadPrivateKey": "leads_mailboxes.go"} {
		found := false
		for _, s := range sites[fn] {
			found = found || strings.HasPrefix(s, file+":")
		}
		if !found {
			t.Errorf("the scan did not find secretbox.%s in %s", fn, file)
		}
	}
}

// TestSecretboxCallSitesTurnRed: the same scan over a copy of the package with a call added
// to serve.go (the console), or with secretbox imported under another name or as a dot
// import, fails naming the file.
func TestSecretboxCallSitesTurnRed(t *testing.T) {
	copyPkg := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		entries, err := os.ReadDir(".")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(e.Name()) // #nosec G304 -- this package's own files
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil { // #nosec G703 -- the test's own temp copy
				t.Fatal(err)
			}
		}
		return dir
	}
	addToServe := func(t *testing.T, dir, importLine, body string) {
		t.Helper()
		p := filepath.Join(dir, "serve.go")
		b, err := os.ReadFile(p) // #nosec G304 -- the test's own temp copy
		if err != nil {
			t.Fatal(err)
		}
		src := strings.Replace(string(b), "import (", "import (\n\t"+importLine, 1)
		if src == string(b) {
			t.Fatal("serve.go has no import block")
		}
		if err := os.WriteFile(p, []byte(src+"\n"+body+"\n"), 0o600); err != nil { // #nosec G703 -- the test's own temp copy
			t.Fatal(err)
		}
	}
	quoted := strconv.Quote(secretboxImport)
	for _, c := range []struct{ name, imp, body, want string }{
		{"load the key", quoted, "func leakKey() { _, _ = secretbox.LoadPrivateKey(os.Getenv) }", "serve.go:"},
		{"open a seal", quoted, "func leakOpen() { _, _ = secretbox.Open(secretbox.PrivateKey{}, nil) }", "serve.go:"},
		{"read a key file", quoted, "var leakRead = secretbox.ReadPrivateKey", "serve.go:"},
		{"renamed import", "sb " + quoted, "func leakSB() { _, _ = sb.Open(sb.PrivateKey{}, nil) }", "serve.go:"},
		{"dot import", ". " + quoted, "func leakDot() {}", "dot import"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := copyPkg(t)
			if v := secretboxViolations(secretboxSites(t, dir)); len(v) != 0 {
				t.Fatalf("the unchanged copy is red: %v", v)
			}
			addToServe(t, dir, c.imp, c.body)
			v := secretboxViolations(secretboxSites(t, dir))
			if len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), c.want) {
				t.Errorf("violations = %q, want one naming %q", v, c.want)
			}
		})
	}
}
