package console

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// leadPageBody renders /leads/5 with d as the lead.
func leadPageBody(t *testing.T, d LeadDetailPage) string {
	t.Helper()
	b := richBackend("x")
	b.detail = d
	h := newHarness(t, b, false)
	w := h.do(get("/leads/5", h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("/leads/5: %d", w.Code)
	}
	return w.Body.String()
}

// TestRenderLeadIntentLine: a café lead with no Instagram match shows its intent line in the
// place card with its reason and date, and its vendor overlap when above 0; the operator's own
// call adds "your call"; a zero overlap shows no overlap line.
func TestRenderLeadIntentLine(t *testing.T) {
	d := cafeLeadDetail()
	d.PlaceType = "cafe"
	d.Intent = &LeadIntentView{Class: "shopping", Label: "Shopping", Reason: "follows Stampino and Pick Coupons", Date: "6 Nov", Overlap: 2}
	body := leadPageBody(t, d)
	if strings.Contains(body, `id="instagram"`) {
		t.Fatal("fixture has an Instagram card; the test needs none")
	}
	for _, want := range []string{
		`<p class="lead-intent">Loyalty intent: Shopping · follows Stampino and Pick Coupons · 6 Nov</p>`,
		`Vendor overlap: 2</p>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lead page lacks %q", want)
		}
	}
	if strings.Contains(body, "your call</p>") {
		t.Error("a derived class says your call")
	}
	place := body[strings.Index(body, "<h2>The place</h2>"):]
	if !strings.Contains(place[:strings.Index(place, "</section>")], "Loyalty intent:") {
		t.Error("the intent line is not in the place card")
	}

	d.Intent = &LeadIntentView{Class: "out", Label: "Out", Reason: "no loyalty signal yet", Date: "5 Nov", Yours: true}
	body = leadPageBody(t, d)
	if !strings.Contains(body, `Loyalty intent: Out · no loyalty signal yet · 5 Nov · your call</p>`) {
		t.Error("the operator's call does not say your call")
	}
	if strings.Contains(body, "Vendor overlap") {
		t.Error("a zero overlap shows a line")
	}
}

// TestRenderLeadIntentNotOnDrupal: a lead that is not a café shows no intent line, even with
// one set.
func TestRenderLeadIntentNotOnDrupal(t *testing.T) {
	d := richLeadDetail("x")
	d.Intent = &LeadIntentView{Class: "shopping", Label: "Shopping"}
	if body := leadPageBody(t, d); strings.Contains(body, "Loyalty intent") {
		t.Error("a Drupal lead shows an intent line")
	}
}

// TestRenderLeadListIntentChip: the chip shows for shopping, paper, just chose, switch and out,
// in both layouts and in light and dark, from the existing badge tokens only; curious, cold and
// a lead with no class show none.
func TestRenderLeadListIntentChip(t *testing.T) {
	want := map[string]string{
		"shopping":   `<span class="badge badge-good intent-badge">Shopping</span>`,
		"paper":      `<span class="badge badge-accent intent-badge">Paper card</span>`,
		"switch":     `<span class="badge badge-accent intent-badge">Switch</span>`,
		"just_chose": `<span class="badge intent-badge">Just chose</span>`,
		"out":        `<span class="badge intent-badge">Out</span>`,
		"curious":    "",
		"cold":       "",
		"":           "",
	}
	for class, chip := range want {
		b := richBackend("x")
		b.list = triageList()
		b.list.Rows[0].IntentClass = class
		h := newHarness(t, b, false)
		c := h.session(t)
		for _, theme := range []string{"light", "dark"} {
			r := get("/leads?campaign=cafes", c)
			r.AddCookie(themeCookie(false, theme))
			w := h.do(r)
			if w.Code != http.StatusOK {
				t.Fatalf("%s %s: %d", class, theme, w.Code)
			}
			body := w.Body.String()
			n := strings.Count(body, "intent-badge")
			switch {
			case chip == "" && n != 0:
				t.Errorf("%q (%s) shows %d chips, want none", class, theme, n)
			case chip != "" && strings.Count(body, chip) != 2:
				t.Errorf("%q (%s): %q shows %d times, want 2 (both layouts)", class, theme, chip, strings.Count(body, chip))
			}
		}
	}
	css, err := os.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(css), ".intent-badge") {
		t.Error("the chip has its own styles; it must use the existing badge tokens")
	}
}
