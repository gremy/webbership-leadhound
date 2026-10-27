//go:build integration

package main

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// ownerScan reads one value as the owner.
func ownerScan(t *testing.T, dest any, sql string) error {
	t.Helper()
	return storetest.OwnerConn(t).QueryRow(context.Background(), sql).Scan(dest)
}

// TestConsoleSuppressLift: the Leads page's lift form end to end, through the real backend
// against the test database (FO6). The page lists each active suppression by its number with
// its address masked; a wrong domain is refused in the console's words and writes nothing; a
// good lift records one row with source console, flashes what it lifted, and the page then
// lists it under Lifted with its reason. No page, flash or log line holds a full address.
func TestConsoleSuppressLift(t *testing.T) {
	leadsScene(t)
	logs, logger := captureLogs(t)
	addr, dom := liftAddr("console-lift"), liftDomain("console-lift")
	mustRun(t, "leads", "suppress", "add", "address", addr, "--reason", "opt_out")
	id := supID(t, leadstore.SuppressAddress, addr)
	c := newConsoleClient(t, leadsBackend(t), logger)

	page := c.get("/leads/outreach", false).Body.String()
	listed := fmt.Sprintf("#%d address i***@console-lift.example · opt_out · cli · ", id)
	if !strings.Contains(page, listed) || !strings.Contains(page, `action="/leads/suppressions/lift"`) || strings.Contains(page, "Lifted (") {
		t.Fatalf("leads page before the lift lacks %q or the form, or shows a Lifted section:\n%s", listed, page)
	}

	lifts := func() int {
		var n int
		if err := ownerScan(t, &n, `SELECT count(*)::int FROM suppression_lift`); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A wrong domain: refused with the console's words (no CLI command), nothing written.
	w := c.post("/leads/suppressions/lift", url.Values{"id": {fmt.Sprint(id)}, "domain": {"other.example"}, "reason": {"an auto-reply"}})
	refusal := fmt.Sprintf("Suppression %d is not on other.example: nothing was lifted. Check the number in the list.", id)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), template.HTMLEscapeString(refusal)) {
		t.Errorf("wrong domain %d, want 422 with %q:\n%s", w.Code, refusal, w.Body.String())
	}
	if n := lifts(); n != 0 {
		t.Errorf("%d lift rows after a refusal", n)
	}

	// The lift: one row from the console, the flash on the page it lands on, and the
	// suppression now under Lifted.
	w = c.post("/leads/suppressions/lift", url.Values{"id": {fmt.Sprint(id)}, "domain": {" " + dom + " "}, "reason": {" an auto-reply quoted our opt-out line "}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/outreach" {
		t.Fatalf("lift: %d to %q:\n%s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	var row string
	if err := ownerScan(t, &row, `SELECT format('%s %s %s %s', suppression_id, actor, source, reason) FROM suppression_lift`); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%d operator console an auto-reply quoted our opt-out line", id); row != want {
		t.Errorf("lift row %q, want %q", row, want)
	}
	flash := fmt.Sprintf("Lifted suppression %d: address i***@console-lift.example (opt_out, from cli). It may be written to again; lead states are unchanged.", id)
	landed := c.pages[len(c.pages)-1]
	if !strings.Contains(landed, template.HTMLEscapeString(flash)) {
		t.Errorf("the page after the lift lacks the flash %q:\n%s", flash, landed)
	}
	lifted := fmt.Sprintf("#%d address i***@console-lift.example · lifted ", id)
	for _, want := range []string{"<summary>Lifted (1)</summary>", lifted, "(console) · an auto-reply quoted our opt-out line"} {
		if !strings.Contains(landed, want) {
			t.Errorf("the page after the lift lacks %q:\n%s", want, landed)
		}
	}
	if strings.Contains(landed, listed) {
		t.Error("the lifted suppression is still listed as active")
	}

	// A second lift of the same number: already lifted, from the console.
	w = c.post("/leads/suppressions/lift", url.Values{"id": {fmt.Sprint(id)}, "domain": {dom}, "reason": {"again"}})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "was already lifted on ") ||
		!strings.Contains(w.Body.String(), "(console).") {
		t.Errorf("second lift %d:\n%s", w.Code, w.Body.String())
	}
	if n := lifts(); n != 1 {
		t.Errorf("%d lift rows, want 1", n)
	}

	if strings.Contains(strings.Join(c.pages, "\n"), addr) || strings.Contains(logs.String(), addr) {
		t.Error("a full address reached a page, a flash cookie or a log line")
	}
	if strings.Contains(logs.String(), "quoted our") {
		t.Error("the lift's reason reached a log line")
	}
}
