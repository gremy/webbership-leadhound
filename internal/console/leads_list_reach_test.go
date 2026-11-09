package console

import (
	"html"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// tabHrefs reads the List's tabs from a page: each tab's words and its link, unescaped.
func tabHrefs(t *testing.T, body string) (map[string]string, []string) {
	t.Helper()
	_, nav, ok := strings.Cut(body, `aria-label="Next action"`)
	if !ok {
		t.Fatal("the page has no tabs")
	}
	nav, _, _ = strings.Cut(nav, "</nav>")
	hrefs, current := map[string]string{}, []string(nil)
	for _, m := range regexp.MustCompile(`<a href="([^"]*)"( aria-current="true")?>([^<]*)</a>`).FindAllStringSubmatch(nav, -1) {
		label, _, _ := strings.Cut(m[3], " (")
		hrefs[label] = html.UnescapeString(m[1])
		if m[2] != "" {
			current = append(current, label)
		}
	}
	return hrefs, current
}

// TestListReachTabCounts: every tab shows its number from ReachCounts; All shows none.
func TestListReachTabCounts(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	body := h.do(get("/leads", h.session(t))).Body.String()
	for _, want := range []string{">Email-ready (12)</a>", ">Manual (7)</a>", ">Dead end (31)</a>", ">Contacted (4)</a>", `aria-current="true">All</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the tabs lack %q", want)
		}
	}
}

// TestListReachTabsKeepFilters: a tab keeps the other filters and goes back to the first
// page; only the tab in force is marked.
func TestListReachTabsKeepFilters(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	body := h.do(get("/leads?kind=d10&country=GB&starred=yes&sort=newest&campaign=drupal-eol&page=2&reach=manual", h.session(t))).Body.String()
	if b.listQ.Reach != ReachManual {
		t.Errorf("asked next action %q", b.listQ.Reach)
	}
	hrefs, current := tabHrefs(t, body)
	if !slices.Equal(current, []string{"Manual"}) {
		t.Errorf("the marked tabs are %v", current)
	}
	for label, reach := range map[string]string{"Email-ready": ReachEmail, "Manual": ReachManual, "Dead end": ReachDead, "Contacted": ReachContacted, "All": ""} {
		u, err := url.Parse(hrefs[label])
		if err != nil || u.Path != "/leads" {
			t.Fatalf("tab %s links to %q", label, hrefs[label])
		}
		q := u.Query()
		if q.Get("reach") != reach {
			t.Errorf("tab %s has reach %q, want %q", label, q.Get("reach"), reach)
		}
		for k, v := range map[string]string{"kind": "d10", "country": "GB", "starred": "yes", "sort": "newest", "campaign": "drupal-eol"} {
			if q.Get(k) != v {
				t.Errorf("tab %s dropped %s: %q", label, k, hrefs[label])
			}
		}
		if q.Has("page") {
			t.Errorf("tab %s keeps the page: %q", label, hrefs[label])
		}
	}
}

// TestListReachTabsStatus: a next action's tab never takes the default status, which would hide
// its leads in other states, and an explicit other status stays; All is the list as it was.
func TestListReachTabsStatus(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)

	hrefs, _ := tabHrefs(t, h.do(get("/leads", c)).Body.String())
	if hrefs["All"] != "/leads" || hrefs["Contacted"] != "/leads?reach=contacted" {
		t.Errorf("the plain list's tabs are %v", hrefs)
	}
	if b.listQ.Status != "" || b.listQ.Reach != "" {
		t.Errorf("the plain list asked %+v", b.listQ)
	}

	// The backend echoes the default it took as open.
	hrefs, _ = tabHrefs(t, h.do(get("/leads?status=open", c)).Body.String())
	if hrefs["All"] != "/leads?status=open" || hrefs["Contacted"] != "/leads?reach=contacted" {
		t.Errorf("under the default status the tabs are %v", hrefs)
	}

	hrefs, _ = tabHrefs(t, h.do(get("/leads?status=all", c)).Body.String())
	if hrefs["All"] != "/leads?status=all" || hrefs["Dead end"] != "/leads?reach=dead&status=all" {
		t.Errorf("under status all the tabs are %v", hrefs)
	}

	body := h.do(get("/leads?reach=contacted", c)).Body.String()
	if b.listQ.Status != "" || b.listQ.Reach != ReachContacted {
		t.Errorf("the tab asked %+v, want a blank status", b.listQ)
	}
	for _, want := range []string{`<input type="hidden" name="reach" value="contacted">`, `<th scope="col">State</th>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the tab lacks %q", want)
		}
	}
	if strings.Contains(body, `name="status"`) {
		t.Error("the filter on a tab would submit a status")
	}
	if strings.Contains(body, "Filters · ") {
		t.Error("a tab counts as a filter")
	}

	body = h.do(get("/leads?reach=contacted&status=all", c)).Body.String()
	if !strings.Contains(body, `<input type="hidden" name="status" value="all">`) {
		t.Error("the filter on a tab loses an explicit status")
	}

	body = h.do(get("/leads", c)).Body.String()
	for _, want := range []string{`name="status"><option value="open"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the All tab lacks %q", want)
		}
	}
	if strings.Contains(body, `name="reach"`) {
		t.Error("the All tab's filter carries a next action")
	}
}

// TestListReachPagerAndBack: the pager and a row's way back stay on the tab, with the echoed
// default status left out so an action does not come back to an empty list.
func TestListReachPagerAndBack(t *testing.T) {
	for _, tc := range []struct{ f LeadListQuery }{{LeadListQuery{Reach: ReachDead, Status: "open"}}, {LeadListQuery{Reach: ReachDead}}} {
		if got := leadListURL(tc.f, 2); got != "/leads?page=2&reach=dead" {
			t.Errorf("%+v at page 2 is %q", tc.f, got)
		}
	}
	h := newHarness(t, richBackend("x"), false)
	body := h.do(get("/leads?reach=manual&page=2", h.session(t))).Body.String()
	for _, want := range []string{`name="back" value="/leads?page=2&amp;reach=manual#lead-w-5"`, `href="/leads?reach=manual" rel="prev"`, `href="/leads?page=3&amp;reach=manual" rel="next"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the tab's page lacks %q", want)
		}
	}
}

// TestListReachRows: each row shows its next action; a dead end also says why, in both layouts,
// and offers the search unless a press could only be refused.
func TestListReachRows(t *testing.T) {
	const look = `<form method="post" action="/leads/5/look-again">`
	for _, tc := range []struct {
		reason, words string
		button        bool
	}{
		{ReachNotSearched, "Not searched yet", true},
		{ReachNothing, "Nothing found", true},
		{ReachLooking, "Looking for a contact", false},
		{ReachFound, "A contact was found; the next build picks it up", false},
		{ReachCountry, "This country is off in your settings", false},
	} {
		b := richBackend("x")
		b.list.Rows[0].Reach, b.list.Rows[0].ReachReason = ReachDead, tc.reason
		h := newHarness(t, b, false)
		body := h.do(get("/leads?reach=dead", h.session(t))).Body.String()
		if n := strings.Count(body, `<span class="item-meta">`+tc.words+`</span>`); n != 2 {
			t.Errorf("%s: the words show %d times, want 2", tc.reason, n)
		}
		if n := strings.Count(body, `<span class="badge reach-badge badge-bad">Dead end</span>`); n != 2 {
			t.Errorf("%s: the badge shows %d times, want 2", tc.reason, n)
		}
		want := 0
		if tc.button {
			want = 2
		}
		if n := strings.Count(body, look); n != want {
			t.Errorf("%s: the button shows %d times, want %d", tc.reason, n, want)
		}
		if tc.button {
			for _, part := range []string{`name="back" value="/leads?page=2&amp;reach=dead#lead-n-5"`, `name="back" value="/leads?page=2&amp;reach=dead#lead-w-5"`, ">Look for a contact<"} {
				if !strings.Contains(body, part) {
					t.Errorf("%s: the page lacks %q", tc.reason, part)
				}
			}
		}
	}

	// In the table a dead end keeps to two lines: the badge and the reason as one line, whose
	// title holds the reason, the channels and the star note, with the button under it. The
	// channels and the star note are still read out to a screen reader.
	b := richBackend("x")
	r := &b.list.Rows[0]
	r.Reach, r.ReachReason, r.Starred, r.StarNote = ReachDead, ReachNotSearched, false, StarNoteEmail
	r.HasEmail, r.HasForm, r.HasLinkedIn, r.HasInstagram, r.HasPhone = false, false, false, false, false
	h := newHarness(t, b, false)
	body := h.do(get("/leads?reach=dead", h.session(t))).Body.String()
	line := `<span class="reach-dead-line" title="Not searched yet · no way to reach yet · ` + StarNoteEmail + `">` +
		`<span class="badge reach-badge badge-bad">Dead end</span> <span class="item-meta">Not searched yet</span></span>` +
		`<span class="visually-hidden"> no way to reach yet. ` + StarNoteEmail + `.</span>`
	if !strings.Contains(body, line) {
		t.Errorf("the table's dead end lacks its one line %q", line)
	}
	if strings.Contains(body, `<span class="item-meta star-note" title="`+StarNoteEmail+`">`) {
		t.Error("the table's dead end still shows the star note as a line of its own")
	}

	for bucket, words := range map[string]string{ReachEmail: "Email-ready", ReachManual: "Manual", ReachContacted: "Contacted"} {
		b := richBackend("x")
		b.list.Rows[0].Reach, b.list.Rows[0].ReachReason = bucket, ""
		h := newHarness(t, b, false)
		body := h.do(get("/leads", h.session(t))).Body.String()
		if n := strings.Count(body, `<span class="`+ReachBadge(bucket)+`">`+words+`</span>`); n != 2 {
			t.Errorf("%s: the badge shows %d times, want 2", bucket, n)
		}
		if strings.Contains(body, "/look-again") {
			t.Errorf("%s: a row that is no dead end offers the search", bucket)
		}
	}
}

// TestListReachLookAgain: the row's button is the existing action and comes back to the tab.
func TestListReachLookAgain(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	back := "/leads?reach=dead#lead-w-5"
	w := h.do(post("/leads/5/look-again", url.Values{"back": {back}}, h.session(t)))
	if w.Code != 303 || w.Header().Get("Location") != back {
		t.Errorf("got %d to %q", w.Code, w.Header().Get("Location"))
	}
	if !sameAction(b.lastLead, LeadAction{Do: LeadLookAgain, ID: 5}) {
		t.Errorf("asked %+v", b.lastLead)
	}
}

// TestListNoLeadsNoTabs: with no leads at all there is nothing to split into tabs.
func TestListNoLeadsNoTabs(t *testing.T) {
	b := richBackend("x")
	b.list = LeadListPage{}
	h := newHarness(t, b, false)
	if strings.Contains(h.do(get("/leads", h.session(t))).Body.String(), `aria-label="Next action"`) {
		t.Error("an empty list shows tabs")
	}
}
