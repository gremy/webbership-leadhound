package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// TestStatusUnitsMatchDeploy: the Status page lists every leadhound timer deploy/ installs,
// and every unit it asks systemd about exists in deploy/: the request-driven units (Send now,
// the mailbox check, research) and the Reoon balance unit included.
func TestStatusUnitsMatchDeploy(t *testing.T) {
	timers, err := filepath.Glob("../../deploy/leadhound-*.timer")
	if err != nil || len(timers) == 0 {
		t.Fatalf("no timers in deploy/: %v", err)
	}
	var asked []string
	for _, u := range statusUnits {
		asked = append(asked, u.unit)
	}
	for _, path := range timers {
		if name := filepath.Base(path); !slices.Contains(asked, name) {
			t.Errorf("%s is not on the Status page (statusUnits)", name)
		}
	}
	for _, unit := range asked {
		if _, err := os.Stat(filepath.Join("../../deploy", unit)); err != nil {
			t.Errorf("statusUnits asks for %s, which deploy/ does not install", unit)
		}
	}
	for _, name := range []string{"leads-send-now", "mailbox-check", "leads-research", "leads-verify", "leads-verify-balance"} {
		if !slices.ContainsFunc(statusUnits, func(u statusUnit) bool { return u.name == name }) {
			t.Errorf("%s is not on the Status page", name)
		}
	}
	seen := map[string]bool{}
	for _, u := range statusUnits {
		if seen[u.name] {
			t.Errorf("%s listed twice", u.name)
		}
		seen[u.name] = true
	}
}

// TestStatusCommandsCoverUnits: every run command a unit reads is in statusCommands, so its
// latest run is read.
func TestStatusCommandsCoverUnits(t *testing.T) {
	for _, u := range statusUnits {
		if u.runs == nil {
			continue
		}
		found := false
		for _, c := range statusCommands {
			found = found || u.runs(store.Run{Command: c, Source: "greenhouse"}) || u.runs(store.Run{Command: c, Source: "linkedin"}) ||
				u.runs(store.Run{Command: c, Note: "2 sent · notes " + noteSendNow})
		}
		if !found {
			t.Errorf("%s reads no command in statusCommands", u.name)
		}
	}
}

func TestReadLoad(t *testing.T) {
	if v := readLoad("0.42 0.30 0.25 1/123 4567\n"); v == nil || *v != 0.42 {
		t.Errorf("load %v", v)
	}
	for _, s := range []string{"", "x 1 2", "-1 0 0"} {
		if v := readLoad(s); v != nil {
			t.Errorf("readLoad(%q) = %v; want nil", s, *v)
		}
	}
}

func TestReadMemoryPct(t *testing.T) {
	v := readMemoryPct("MemTotal:        4000000 kB\nMemFree:  100 kB\nMemAvailable:    1000000 kB\n")
	if v == nil || *v != 75 {
		t.Fatalf("memory %v", v)
	}
	for _, s := range []string{"", "MemTotal: 100 kB\n", "MemAvailable: 1 kB\n", "MemTotal: 0 kB\nMemAvailable: 0 kB\n"} {
		if v := readMemoryPct(s); v != nil {
			t.Errorf("readMemoryPct(%q) = %v; want nil", s, *v)
		}
	}
}

// TestReoonReadingOf: the three keys the balance unit writes are read, other fields ignored;
// a missing key or one of another type is no reading.
func TestReoonReadingOf(t *testing.T) {
	api, daily, instant, ok := reoonReadingOf([]byte(`{"api_status":"active","daily":150,"instant":20,"extra":true}`))
	if !ok || api != "active" || daily != 150 || instant != 20 {
		t.Errorf("got %q %d %d %v", api, daily, instant, ok)
	}
	for _, s := range []string{
		``, `[]`, `{}`, `{"daily":1,"instant":2}`, `{"api_status":"active","daily":"1","instant":2}`,
		`{"api_status":"active","daily":1.5,"instant":2}`, `{"api_status":1,"daily":1,"instant":2}`,
		`{"api_status":"active","daily":1}`,
	} {
		if _, _, _, ok := reoonReadingOf([]byte(s)); ok {
			t.Errorf("reoonReadingOf(%s) ok; want no reading", s)
		}
	}
}

func TestWaitingRefresh(t *testing.T) {
	at := func(m int) *time.Time { v := time.Date(2026, 10, 20, 12, m, 0, 0, time.UTC); return &v }
	cases := []struct {
		name  string
		asked *time.Time
		row   *leadstore.ServiceStatusRow
		want  bool
	}{
		{"nothing asked", nil, nil, false},
		{"asked, never read", at(5), nil, true},
		{"asked after the reading", at(5), &leadstore.ServiceStatusRow{FetchedAt: at(1)}, true},
		{"answered by a reading", at(5), &leadstore.ServiceStatusRow{FetchedAt: at(6)}, false},
		{"answered by a failure", at(5), &leadstore.ServiceStatusRow{FetchedAt: at(1), ErrorAt: at(6)}, false},
	}
	for _, c := range cases {
		if got := waitingRefresh(c.asked, c.row); got != c.want {
			t.Errorf("%s: %v; want %v", c.name, got, c.want)
		}
	}
}

func TestRunResult(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	cases := []struct {
		name         string
		run          store.Run
		result, note string
	}{
		{"finished", store.Run{StartedAt: now.Add(-time.Hour), FinishedAt: ago(time.Hour - 41*time.Second)}, console.UnitOK, "41 s"},
		{"running", store.Run{StartedAt: now.Add(-time.Minute)}, console.UnitRunning, ""},
		{"stuck", store.Run{StartedAt: now.Add(-runStuckAfter - time.Minute)}, console.UnitFailed, "Did not finish"},
		{"stopped", store.Run{StartedAt: now.Add(-time.Hour), FinishedAt: ago(time.Minute), StopReason: "blocked"}, console.UnitFailed, "Stopped: it was blocked"},
		{"failures", store.Run{StartedAt: now.Add(-time.Hour), FinishedAt: ago(time.Minute), Failures: 2}, console.UnitFailed, "Finished with 2 failures"},
		{"switched off", store.Run{StartedAt: now.Add(-time.Hour), FinishedAt: ago(time.Hour), Note: runNoteSwitchedOff}, console.UnitOK, "0 s · Switched off"},
	}
	for _, c := range cases {
		if r, n := runResult(c.run, now); r != c.result || n != c.note {
			t.Errorf("%s: %q %q; want %q %q", c.name, r, n, c.result, c.note)
		}
	}
}

func TestDurationWords(t *testing.T) {
	for d, want := range map[time.Duration]string{41 * time.Second: "41 s", 3 * time.Minute: "3 min", 2*time.Hour + 5*time.Minute: "2 h 5 min", -time.Second: "0 s"} {
		if got := durationWords(d); got != want {
			t.Errorf("%v: %q; want %q", d, got, want)
		}
	}
}

// TestMailboxChecks: the parts in order, a part never run left out, the record check used for
// SPF, DKIM and DMARC when it is newer, and nothing for a check only requested.
func TestMailboxChecks(t *testing.T) {
	yes, no := true, false
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	r := leadstore.MailboxHealthRow{LastCheck: &leadstore.MailboxCheck{CheckedAt: &at, SMTPOK: &yes, IMAPOK: &yes, SPF: &yes, DKIM: &no, DMARC: &yes}}
	got, parts := mailboxChecks(r)
	if got == nil || !got.Equal(at) {
		t.Fatalf("at %v", got)
	}
	if fmt.Sprint(parts) != "[{SMTP true} {IMAP true} {SPF true} {DKIM false} {DMARC true}]" {
		t.Errorf("parts %v", parts)
	}
	r.SenderCheck = &leadstore.SenderCheckRow{SPF: true, DKIM: true, DMARC: true, CheckedAt: at.Add(time.Hour)}
	if _, parts := mailboxChecks(r); fmt.Sprint(parts) != "[{SMTP true} {IMAP true} {SPF true} {DKIM true} {DMARC true}]" {
		t.Errorf("newer record check: %v", parts)
	}
	r.SenderCheck.CheckedAt = at.Add(-time.Hour)
	if _, parts := mailboxChecks(r); fmt.Sprint(parts) != "[{SMTP true} {IMAP true} {SPF true} {DKIM false} {DMARC true}]" {
		t.Errorf("older record check: %v", parts)
	}
	r.LastCheck.CheckedAt = nil
	if got, parts := mailboxChecks(r); got != nil || parts != nil {
		t.Errorf("requested only: %v %v", got, parts)
	}
	if got, parts := mailboxChecks(leadstore.MailboxHealthRow{}); got != nil || parts != nil {
		t.Errorf("never checked: %v %v", got, parts)
	}
}

func TestNeedsMeWords(t *testing.T) {
	if needsMeWords(leadstore.NeedsReplied) != "Replied · nothing scheduled" || needsMeWords(leadstore.NeedsConnected) != "Connected on a call · no next step" {
		t.Error("words")
	}
}

// The console holds no key: the Status page's and the dashboard's code never loads one,
// never names a credential and never reads the environment beyond the Reoon daily cap
// (controlsGetenv). keyFiles are the files swept whole; keyFuncs the functions of other files
// swept alone.
var (
	keyFiles = []string{"consolecore_status.go"}
	keyFuncs = map[string][]string{"serve.go": {"Dashboard"}, "consolecore.go": {"leadsSummary", "leadsToday"}}
	// keyImports are the packages that load or open a key.
	keyImports = []string{"/internal/secretbox", "/internal/verify", "/internal/mailbox"}
	// keySelectors are the names that load a key or name a credential, in any package.
	keySelectors = []string{"LoadKey", "LoadPrivateKey", "ReadPrivateKey", "Open", "KeyFileEnv", "CredentialName", "Getenv", "LookupEnv", "Environ", "ReadFile"}
	// keyIdents are this package's own key loaders.
	keyIdents = []string{"newReoonClient", "consolePublicKey"}
)

// keySites is every reference in src (file name) to a key loader, a credential's name or the
// environment, in the whole file or, when funcs is not empty, in those functions only. The
// /proc reader (procFile) is the one ReadFile allowed: it reads fixed /proc paths.
func keySites(t *testing.T, name string, src []byte, funcs []string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var out []string
	site := func(n ast.Node, what string) {
		out = append(out, fmt.Sprintf("%s:%d: %s", name, fset.Position(n.Pos()).Line, what))
	}
	if len(funcs) == 0 {
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, k := range keyImports {
				if strings.HasSuffix(p, k) {
					site(imp, "imports "+p)
				}
			}
		}
	}
	inspect := func(root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Name.Name == "procFile" || n.Name.Name == "init" {
					return false
				}
			case *ast.ValueSpec:
				if slices.ContainsFunc(n.Names, func(id *ast.Ident) bool { return id.Name == "procFile" }) {
					return false
				}
			case *ast.SelectorExpr:
				if slices.Contains(keySelectors, n.Sel.Name) {
					site(n, "uses ."+n.Sel.Name)
				}
			case *ast.Ident:
				if slices.Contains(keyIdents, n.Name) {
					site(n, "uses "+n.Name)
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					v, _ := strconv.Unquote(n.Value)
					u := strings.ToUpper(v)
					if strings.Contains(u, "CREDENTIALS_DIRECTORY") || strings.Contains(u, "API_KEY") || strings.Contains(u, "KEY_FILE") ||
						strings.HasSuffix(v, ".key") || strings.Contains(v, "anthropic-key") {
						site(n, "names "+v)
					}
				}
			}
			return true
		})
	}
	if len(funcs) == 0 {
		inspect(f)
		return out
	}
	found := map[string]bool{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && slices.Contains(funcs, fd.Name.Name) {
			found[fd.Name.Name] = true
			inspect(fd)
		}
	}
	for _, fn := range funcs {
		if !found[fn] {
			t.Errorf("%s: no function %s to sweep", name, fn)
		}
	}
	return out
}

// statusKeySites sweeps the Status and dashboard code read from read.
func statusKeySites(t *testing.T, read func(string) ([]byte, error)) []string {
	t.Helper()
	var out []string
	for _, name := range keyFiles {
		src, err := read(name)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, keySites(t, name, src, nil)...)
	}
	for name, funcs := range keyFuncs {
		src, err := read(name)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, keySites(t, name, src, funcs)...)
	}
	return out
}

// TestStatusHoldsNoKey: Status, RefreshStatus, AddClaudeTopup and the dashboard's figures load
// no key, name no credential and read no environment variable but the Reoon daily cap.
func TestStatusHoldsNoKey(t *testing.T) {
	if sites := statusKeySites(t, os.ReadFile); len(sites) > 0 {
		t.Errorf("the console's Status and dashboard code reaches for a key:\n%s", strings.Join(sites, "\n"))
	}
}

// TestStatusHoldsNoKeyTurnsRed: the same sweep over copies with a key load, an environment
// read or a credential's name added fails naming each.
func TestStatusHoldsNoKeyTurnsRed(t *testing.T) {
	for _, c := range []struct{ file, add, want string }{
		{"consolecore_status.go", "\nfunc init() {}\nfunc leak() { _, _ = verify.LoadKey(os.Getenv) }\n", ".LoadKey"},
		{"consolecore_status.go", "\nfunc leak() { _ = os.Getenv(\"LEADHOUND_REOON_KEY_FILE\") }\n", ".Getenv"},
		{"consolecore_status.go", "\nfunc leak() { _, _ = newReoonClient(true) }\n", "uses newReoonClient"},
		{"consolecore_status.go", "\nconst leak = \"/run/credentials/x/reoon.key\"\n", "names /run/credentials/x/reoon.key"},
		{"consolecore_status.go", "\nimport _ \"github.com/gremy/webbership-leadhound/internal/secretbox\"\n", "imports"},
	} {
		read := func(name string) ([]byte, error) {
			b, err := os.ReadFile(name) // #nosec G304 -- this package's own files
			if err != nil || name != c.file {
				return b, err
			}
			if strings.HasPrefix(c.add, "\nimport") {
				return []byte(strings.Replace(string(b), "\nimport (", c.add+"\nimport (", 1)), nil
			}
			return append(b, c.add...), nil
		}
		sites := statusKeySites(t, read)
		if !slices.ContainsFunc(sites, func(s string) bool { return strings.Contains(s, c.want) }) {
			t.Errorf("adding %q: sweep found %v; want %q", c.add, sites, c.want)
		}
	}
	// A key load added inside Dashboard is found too.
	read := func(name string) ([]byte, error) {
		b, err := os.ReadFile(name) // #nosec G304 -- this package's own files
		if err != nil || name != "serve.go" {
			return b, err
		}
		return []byte(strings.Replace(string(b), "\tnow := b.now()\n\tv := b.current(ctx, now)",
			"\tnow := b.now()\n\t_, _ = claude.LoadKey(os.Getenv)\n\tv := b.current(ctx, now)", 1)), nil
	}
	if sites := statusKeySites(t, read); !slices.ContainsFunc(sites, func(s string) bool { return strings.Contains(s, "serve.go") && strings.Contains(s, ".LoadKey") }) {
		t.Errorf("a key load in Dashboard: sweep found %v", sites)
	}
}
