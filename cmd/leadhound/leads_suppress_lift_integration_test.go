//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The operator's lift of a wrong suppression from the CLI (FO6): `leads suppress lift` and
// `leads suppress list`, and the grid cells the store test cannot reach: the send-time
// SuppressionSet gate and `leads suppress check`, for an active and a lifted suppression.
// Every fixture address is built at run time from reserved names; no output may hold one.

// liftDomain and liftAddr are a fixture lead's domain and its route's address.
func liftDomain(name string) string { return name + ".example" }
func liftAddr(name string) string   { return "info" + "@" + liftDomain(name) }

// suppressRaw writes an active suppression row straight into the table, as a sync would,
// without moving the lead or cancelling its messages, so the send-time gate is what stops
// the send. It returns the row's id.
func suppressRaw(t *testing.T, kind, value, reason string) int64 {
	t.Helper()
	var id int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`INSERT INTO suppression (kind, value, reason, source) VALUES ($1, $2, $3, 'test') RETURNING id`,
		kind, value, reason).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// suppressionID is the id of the one suppression of kind and value.
func suppressionID(t *testing.T, w *sendWorld, kind, value string) int64 {
	t.Helper()
	return int64(w.count(t, `SELECT id::int FROM suppression WHERE kind = $1 AND value = $2`, kind, value))
}

// liftCount is how many lifts are recorded.
func liftCount(t *testing.T, w *sendWorld) int {
	t.Helper()
	return w.count(t, `SELECT count(*)::int FROM suppression_lift`)
}

// dbTime reads one timestamptz.
func dbTime(t *testing.T, sql string, args ...any) time.Time {
	t.Helper()
	var at time.Time
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), sql, args...).Scan(&at); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return at
}

// runSplit runs one command, keeping its output for the sweep, and returns stdout and
// stderr apart.
func (w *sendWorld) runSplit(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	code, out, errOut := leadsRun(t, args...)
	w.outs = append(w.outs, out, errOut)
	return code, out, errOut
}

// TestSuppressLiftSendAndCheck: the send-time gate (leads_send.go's SuppressionSet check)
// and `leads suppress check` over an active suppression (suppressed, never sent) and the
// same suppression lifted (not suppressed, sent).
func TestSuppressLiftSendAndCheck(t *testing.T) {
	for _, lifted := range []bool{false, true} {
		name := map[bool]string{false: "active", true: "lifted"}[lifted]
		t.Run(name, func(t *testing.T) {
			w := newSendWorld(t, true, 20)
			addr := liftAddr("gate")
			w.batch(t, false, map[int64]string{w.lead(t, liftDomain("gate")): liftDomain("gate")})
			id := suppressRaw(t, leadstore.SuppressAddress, addr, leadstore.ReasonOptOut)
			if lifted {
				w.mustRun(t, "leads", "suppress", "lift", fmt.Sprint(id), "--domain", liftDomain("gate"), "--reason", "an auto-reply, not an opt-out")
			}
			check := w.mustRun(t, "leads", "suppress", "check", addr)
			out := w.mustRun(t, "leads", "send")
			if lifted {
				wantLines(t, "check", check, "address i***@gate.example: not suppressed")
				if got := w.smtp.recipients(); len(got) != 1 || got[0] != addr {
					t.Errorf("a lifted suppression's address was not sent to: %d sends\n%s", len(got), out)
				}
				if s := w.msgState(t, liftDomain("gate"), 0); s != leadstore.MsgSent {
					t.Errorf("message %s, want sent", s)
				}
			} else {
				wantLines(t, "check", check, "address i***@gate.example: suppressed: never written to")
				if n := w.smtp.count(); n != 0 {
					t.Errorf("an active suppression's address got %d sends:\n%s", n, out)
				}
				if s := w.msgState(t, liftDomain("gate"), 0); s != leadstore.MsgCancelled {
					t.Errorf("message %s, want cancelled", s)
				}
			}
			w.sweep(t, addr)
		})
	}
}

// TestSuppressLiftHappyPath: a lift prints what it lifted, masked, and that lead states are
// unchanged; it records one row (actor operator, source cli) and leaves the suppression and
// the lead as they were. A lift under an active domain row says the address is still blocked.
func TestSuppressLiftHappyPath(t *testing.T) {
	w := newSendWorld(t, true, 20)
	addr, dom := liftAddr("happy"), liftDomain("happy")
	w.lead(t, dom)
	w.mustRun(t, "leads", "suppress", "add", "address", addr, "--reason", "opt_out")
	id := suppressionID(t, w, leadstore.SuppressAddress, addr)

	code, out, errOut := w.runSplit(t, "leads", "suppress", "lift", fmt.Sprint(id), "--domain", " HAPPY.example ", "--reason", "an auto-reply quoted our opt-out line")
	want := fmt.Sprintf("Lifted suppression %d: address i***@happy.example (opt_out, from cli). It may be written to again.\n"+
		"Lead states are unchanged (1 lead still reads suppressed or opted_out); to block it again: leads suppress add address … --reason manual.\n", id)
	if code != 0 || out != want || errOut != "" {
		t.Errorf("lift exited %d\nstdout %q\nwant   %q\nstderr %q", code, out, want, errOut)
	}
	if got := w.str(t, `SELECT format('%s %s %s', actor, source, reason) FROM suppression_lift WHERE suppression_id = $1`, id); got != "operator cli an auto-reply quoted our opt-out line" {
		t.Errorf("lift row %q", got)
	}
	if n := w.count(t, `SELECT count(*)::int FROM suppression WHERE id = $1 AND reason = 'opt_out'`, id); n != 1 {
		t.Error("the lift changed the suppression row")
	}
	if s := w.leadState(t, dom); s != leadstore.LeadSuppressed {
		t.Errorf("lead %s, want suppressed (a lift never changes lead states)", s)
	}

	// An address row lifted under an active domain row: still blocked, named by id.
	addr2, dom2 := liftAddr("covered"), liftDomain("covered")
	w.mustRun(t, "leads", "suppress", "add", "address", addr2, "--reason", "opt_out")
	w.mustRun(t, "leads", "suppress", "add", "domain", dom2)
	aid, did := suppressionID(t, w, leadstore.SuppressAddress, addr2), suppressionID(t, w, leadstore.SuppressDomain, dom2)
	out = w.mustRun(t, "leads", "suppress", "lift", fmt.Sprint(aid), "--domain", dom2, "--reason", "wrong reading")
	want = fmt.Sprintf("Lifted suppression %d: address i***@covered.example (opt_out, from cli). It is still blocked by #%d (domain covered.example, manual).\n"+
		"Lead states are unchanged.\n", aid, did)
	if out != want {
		t.Errorf("lift under a domain row:\n%q\nwant\n%q", out, want)
	}
	wantLines(t, "check", w.mustRun(t, "leads", "suppress", "check", addr2), "address i***@covered.example: suppressed: never written to")
	w.sweep(t, addr, addr2)
}

// TestSuppressLiftRefusals: each refusal exits 2 with its plain sentence on stderr, nothing
// on stdout, no lift row, and never an address.
func TestSuppressLiftRefusals(t *testing.T) {
	w := newSendWorld(t, true, 20)
	addr, dom := liftAddr("refuse"), liftDomain("refuse")
	w.mustRun(t, "leads", "suppress", "add", "address", addr, "--reason", "opt_out")
	id := suppressionID(t, w, leadstore.SuppressAddress, addr)
	sid := fmt.Sprint(id)
	typedAddr := "someone" + "@" + "elsewhere.example"

	cases := []struct {
		name string
		args []string
		want string // stderr after "leadhound leads: usage: "; "" checks only the usage line
	}{
		{"no domain", []string{sid, "--reason", "a reason"}, ""},
		{"no reason", []string{sid, "--domain", dom}, ""},
		{"blank reason", []string{sid, "--domain", dom, "--reason", "   "}, ""},
		{"no id", []string{"--domain", dom, "--reason", "a reason"}, ""},
		{"not a number", []string{"two", "--domain", dom, "--reason", "a reason"}, ""},
		{"unknown id", []string{"999999", "--domain", dom, "--reason", "a reason"}, "No suppression 999999."},
		{"wrong domain", []string{sid, "--domain", "Other.example", "--reason", "a reason"},
			fmt.Sprintf("Suppression %d is not on other.example: nothing was lifted. Check the id with leads suppress list.", id)},
		{"address as domain", []string{sid, "--domain", typedAddr, "--reason", "a reason"},
			fmt.Sprintf("Suppression %d is not on s***@elsewhere.example: nothing was lifted. Check the id with leads suppress list.", id)},
		{"short reason", []string{sid, "--domain", dom, "--reason", "no"}, "A lift needs a reason, 3 to 300 characters on one line, with no address in it."},
		{"long reason", []string{sid, "--domain", dom, "--reason", strings.Repeat("x", 301)}, "A lift needs a reason, 3 to 300 characters on one line, with no address in it."},
		{"two lines", []string{sid, "--domain", dom, "--reason", "first\nsecond"}, "A lift needs a reason, 3 to 300 characters on one line, with no address in it."},
		{"address in reason", []string{sid, "--domain", dom, "--reason", "they wrote from " + typedAddr}, "A lift needs a reason, 3 to 300 characters on one line, with no address in it."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := w.runSplit(t, append([]string{"leads", "suppress", "lift"}, c.args...)...)
			if code != 2 || out != "" {
				t.Errorf("exited %d, stdout %q; want 2 and nothing", code, out)
			}
			if c.want == "" {
				if !strings.Contains(errOut, leadsSuppressUsage) {
					t.Errorf("stderr %q lacks the usage", errOut)
				}
			} else if errOut != "leadhound leads: usage: "+c.want+"\n" {
				t.Errorf("stderr %q\nwant   %q", errOut, c.want)
			}
			if n := liftCount(t, w); n != 0 {
				t.Errorf("%d lift rows after a refusal", n)
			}
		})
	}

	// Already lifted: the date (operator's zone) and where.
	w.mustRun(t, "leads", "suppress", "lift", sid, "--domain", dom, "--reason", "first lift")
	at := dbTime(t, `SELECT lifted_at FROM suppression_lift WHERE suppression_id = $1`, id)
	code, out, errOut := w.runSplit(t, "leads", "suppress", "lift", sid, "--domain", dom, "--reason", "second lift")
	want := fmt.Sprintf("leadhound leads: usage: Suppression %d was already lifted on %s (cli).\n", id, leadsDay(at))
	if code != 2 || out != "" || errOut != want {
		t.Errorf("a second lift exited %d, stdout %q\nstderr %q\nwant   %q", code, out, errOut, want)
	}
	if n := liftCount(t, w); n != 1 {
		t.Errorf("%d lift rows, want 1", n)
	}
	w.sweep(t, addr, typedAddr)
}

// TestSuppressList: the counts line first (active only), each active suppression with its
// id, then the lifted ones with their lift date, source and reason; with every suppression
// lifted the lifted ones still show.
func TestSuppressList(t *testing.T) {
	w := newSendWorld(t, true, 20)
	a1, a2, d1 := liftAddr("list-one"), liftAddr("list-two"), liftDomain("list-three")
	w.mustRun(t, "leads", "suppress", "add", "address", a1, "--reason", "opt_out")
	w.mustRun(t, "leads", "suppress", "add", "address", a2, "--reason", "opt_out")
	w.mustRun(t, "leads", "suppress", "add", "domain", d1)
	id1, id2 := suppressionID(t, w, leadstore.SuppressAddress, a1), suppressionID(t, w, leadstore.SuppressAddress, a2)
	id3 := suppressionID(t, w, leadstore.SuppressDomain, d1)
	day := func(id int64) string {
		return leadsDay(dbTime(t, `SELECT created_at FROM suppression WHERE id = $1`, id))
	}
	w.mustRun(t, "leads", "suppress", "lift", fmt.Sprint(id1), "--domain", liftDomain("list-one"), "--reason", "an auto-reply")
	lday := leadsDay(dbTime(t, `SELECT lifted_at FROM suppression_lift WHERE suppression_id = $1`, id1))

	out := w.mustRun(t, "leads", "suppress", "list")
	want := "Suppressions: address opt_out 1 · domain manual 1\n" +
		fmt.Sprintf("  #%d domain list-three.example manual cli %s\n", id3, day(id3)) +
		fmt.Sprintf("  #%d address i***@list-two.example opt_out cli %s\n", id2, day(id2)) +
		"Lifted (1):\n" +
		fmt.Sprintf("  #%d address i***@list-one.example opt_out · lifted %s (cli): an auto-reply\n", id1, lday)
	if out != want {
		t.Errorf("list:\n%s\nwant\n%s", out, want)
	}

	// Everything lifted: no active count, the lifted ones still listed.
	w.mustRun(t, "leads", "suppress", "lift", fmt.Sprint(id2), "--domain", liftDomain("list-two"), "--reason", "a quoted reply")
	w.mustRun(t, "leads", "suppress", "lift", fmt.Sprint(id3), "--domain", d1, "--reason", "added by mistake")
	out = w.mustRun(t, "leads", "suppress", "list")
	if !strings.HasPrefix(out, "No suppressions.\nLifted (3):\n") || strings.Count(out, "\n") != 5 {
		t.Errorf("all lifted:\n%s", out)
	}
	wantLines(t, "all lifted", out, fmt.Sprintf("  #%d address i***@list-one.example", id1), fmt.Sprintf("  #%d address i***@list-two.example", id2),
		fmt.Sprintf("  #%d domain list-three.example manual · lifted", id3))
	w.sweep(t, a1, a2)
}

// supID is the id of the one suppression of kind and value, read as the owner.
func supID(t *testing.T, kind, value string) int64 {
	t.Helper()
	var id int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT id FROM suppression WHERE kind = $1 AND value = $2`, kind, value).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// liftAgreeState is what a lift leaves to compare: each suppression_lift row without its
// source and time, the sources apart, and the SuppressionSet the send-time gate reads.
func liftAgreeState(t *testing.T) (rows, sources []string, set string) {
	t.Helper()
	ctx := context.Background()
	r, err := storetest.OwnerConn(t).Query(ctx, `SELECT format('%s %s %s %s', id, suppression_id, actor, reason), source FROM suppression_lift ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var row, src string
		if err := r.Scan(&row, &src); err != nil {
			t.Fatal(err)
		}
		rows, sources = append(rows, row), append(sources, src)
	}
	r.Close()
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	ss, err := leadstoretest.Q(t).SuppressionSet(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows, sources, fmt.Sprintf("%v", ss)
}

// TestLiftCLIAndConsoleAgree: the CLI's `leads suppress lift` and the console's LeadLift run
// the same core on two identical fresh fixtures with the same inputs, so they write the same
// suppression_lift rows (only source differs: cli, console), leave the same SuppressionSet,
// and say the same sentences. Exactly two things may differ in the words, both because the
// console never names a CLI command: the wrong-domain refusal ends "Check the number in the
// list." where the CLI ends "Check the id with leads suppress list.", and a success is one
// line on the console, its last sentence ending "; lead states are unchanged.", where the
// CLI prints that sentence ending "." and a second line about lead states. consoleWords
// turns the CLI's words into the console's by exactly those two rules; anything else that
// differs fails.
func TestLiftCLIAndConsoleAgree(t *testing.T) {
	ctx := context.Background()
	typedAddr := "someone" + "@" + "elsewhere.example"
	type input struct {
		name           string
		sup            [2]string // kind, value of the suppression lifted; "" kind: id 999999
		domain, reason string
		refused        bool
	}
	inputs := []input{
		{"unknown id", [2]string{}, liftDomain("agree"), "a reason", true},
		{"wrong domain", [2]string{leadstore.SuppressAddress, liftAddr("agree")}, "Other.example", "a reason", true},
		{"address as domain", [2]string{leadstore.SuppressAddress, liftAddr("agree")}, typedAddr, "a reason", true},
		{"short reason", [2]string{leadstore.SuppressAddress, liftAddr("agree")}, liftDomain("agree"), "no", true},
		{"two lines", [2]string{leadstore.SuppressAddress, liftAddr("agree")}, liftDomain("agree"), "first\nsecond", true},
		{"address in reason", [2]string{leadstore.SuppressAddress, liftAddr("agree")}, liftDomain("agree"), "they wrote from " + typedAddr, true},
		{"already lifted", [2]string{leadstore.SuppressAddress, liftAddr("before")}, liftDomain("before"), "again", true},
		{"lift", [2]string{leadstore.SuppressAddress, liftAddr("agree")}, " AGREE.example ", "an auto-reply quoted our opt-out line", false},
		{"lift still blocked", [2]string{leadstore.SuppressAddress, liftAddr("covered")}, liftDomain("covered"), "wrong reading", false},
		{"lift a domain", [2]string{leadstore.SuppressDomain, liftDomain("whole")}, liftDomain("whole"), "added by mistake", false},
	}
	// fixture is a fresh database with the same suppressions in the same order, one of them
	// lifted from the CLI beforehand (for the already-lifted refusal).
	fixture := func(t *testing.T) {
		leadsScene(t)
		mustRun(t, "leads", "suppress", "add", "address", liftAddr("agree"), "--reason", "opt_out")
		mustRun(t, "leads", "suppress", "add", "address", liftAddr("covered"), "--reason", "opt_out")
		mustRun(t, "leads", "suppress", "add", "domain", liftDomain("covered"))
		mustRun(t, "leads", "suppress", "add", "domain", liftDomain("whole"), "--reason", "opt_out")
		mustRun(t, "leads", "suppress", "add", "address", liftAddr("before"), "--reason", "opt_out")
		mustRun(t, "leads", "suppress", "lift", fmt.Sprint(supID(t, leadstore.SuppressAddress, liftAddr("before"))),
			"--domain", liftDomain("before"), "--reason", "set up before")
	}
	idOf := func(t *testing.T, in input) int64 {
		if in.sup[0] == "" {
			return 999999
		}
		return supID(t, in.sup[0], in.sup[1])
	}

	// The CLI.
	fixture(t)
	var cliWords []string
	var outs []string
	for _, in := range inputs {
		code, out, errOut := leadsRun(t, "leads", "suppress", "lift", fmt.Sprint(idOf(t, in)), "--domain", in.domain, "--reason", in.reason)
		outs = append(outs, out, errOut)
		if in.refused {
			const pre = "leadhound leads: usage: "
			if code != 2 || out != "" || !strings.HasPrefix(errOut, pre) {
				t.Fatalf("%s: CLI exited %d, stdout %q, stderr %q", in.name, code, out, errOut)
			}
			cliWords = append(cliWords, strings.TrimSuffix(strings.TrimPrefix(errOut, pre), "\n"))
		} else {
			if code != 0 || errOut != "" {
				t.Fatalf("%s: CLI exited %d, stderr %q", in.name, code, errOut)
			}
			cliWords = append(cliWords, strings.TrimSuffix(out, "\n"))
		}
	}
	cliRows, cliSources, cliSet := liftAgreeState(t)

	// The console, on a second fresh fixture.
	fixture(t)
	b := leadsBackend(t)
	var conWords []string
	for _, in := range inputs {
		res, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLift, ID: idOf(t, in), Value: in.domain, Reason: in.reason})
		if in.refused {
			if !errors.Is(err, console.ErrUserFixable) {
				t.Fatalf("%s: console answered %+v, %v; want a refusal", in.name, res, err)
			}
			conWords = append(conWords, strings.TrimPrefix(err.Error(), console.ErrUserFixable.Error()+": "))
		} else {
			if err != nil {
				t.Fatalf("%s: console: %v", in.name, err)
			}
			conWords = append(conWords, res.Message)
		}
		outs = append(outs, conWords[len(conWords)-1])
	}
	conRows, conSources, conSet := liftAgreeState(t)

	// consoleWords is the CLI's sentence as the console says it: the only two differences.
	consoleWords := func(cli string, refused bool) string {
		if refused {
			return strings.Replace(cli, " Check the id with leads suppress list.", " Check the number in the list.", 1)
		}
		first, _, _ := strings.Cut(cli, "\n")
		return strings.TrimSuffix(first, ".") + "; lead states are unchanged."
	}
	for i, in := range inputs {
		if got, want := conWords[i], consoleWords(cliWords[i], in.refused); got != want {
			t.Errorf("%s:\nconsole %q\nCLI     %q\nwant    %q", in.name, got, cliWords[i], want)
		}
		if strings.Contains(conWords[i], "leads suppress") {
			t.Errorf("%s: the console names a CLI command: %q", in.name, conWords[i])
		}
	}
	if strings.Join(cliRows, "\n") != strings.Join(conRows, "\n") || len(cliRows) != 4 {
		t.Errorf("lift rows differ:\nCLI\n%s\nconsole\n%s", strings.Join(cliRows, "\n"), strings.Join(conRows, "\n"))
	}
	if want := []string{"cli", "cli", "cli", "cli"}; strings.Join(cliSources, " ") != strings.Join(want, " ") {
		t.Errorf("CLI sources %v", cliSources)
	}
	if want := "cli console console console"; strings.Join(conSources, " ") != want {
		t.Errorf("console sources %v, want %s", conSources, want)
	}
	if cliSet != conSet {
		t.Errorf("SuppressionSet differs:\nCLI     %s\nconsole %s", cliSet, conSet)
	}
	for _, o := range outs {
		for _, a := range []string{liftAddr("agree"), liftAddr("covered"), liftAddr("before"), typedAddr} {
			if strings.Contains(o, a) {
				t.Errorf("an output holds a full address: %q", o)
			}
		}
	}
}
