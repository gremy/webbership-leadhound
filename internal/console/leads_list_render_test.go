package console

import (
	"net/http"
	"strings"
	"testing"
)

// triageList is a café campaign's page holding one of each row the review column words: an
// open café lead with every detail, an open Drupal lead, a lead the system skipped, a lead the
// operator skipped, and an open café lead tagged a roastery. Every name is fictive.
func triageList() LeadListPage {
	p := richLeadList("x")
	p.Rows = []LeadListRow{
		{LeadID: 21, Name: "Fictive Bean Café", Country: "RO", Kind: "Café", RType: "cafe", StateCode: "new", State: "not contacted yet",
			Town: "Fictive Town", PlaceType: "cafe", PlaceTypeSource: "name",
			Summary: `<b>Cosy</b> corner & "bright" <script>alert(1)</script>`,
			Finding: "No loyalty app seen", FindingReason: "No loyalty vendor on its pages",
			RelevanceWords: "72, likely match",
			Details: LeadRowDetails{
				Address: "1 Fictive Street",
				Phones:  []string{"+40 256 000 111"},
				Routes:  []RowLink{{Label: "hello@bean.example"}, {Label: "Contact form", URL: "https://bean.example/contact"}},
				Socials: []RowLink{
					{Label: "Facebook", URL: "https://facebook.example/fictivebean"},
					{Label: "Instagram", URL: "javascript:alert(1)"},
					{Label: "TikTok", URL: "//tiktok.example/fictivebean"},
				},
			}},
		{LeadID: 22, Name: "Northwind Trading", Domain: "northwind.example", Country: "GB", Kind: "Drupal 10", RType: "drupal", StateCode: "new", State: "not contacted yet"},
		{LeadID: 23, Name: "Halden Copy", Country: "RO", Kind: "Café", RType: "cafe", StateCode: "skipped", State: "skipped", SkipReason: "Same place as lead 21"},
		{LeadID: 24, Name: "Willow Lane", Country: "RO", Kind: "Café", RType: "cafe", StateCode: "skipped", State: "skipped", CanUndo: true},
		{LeadID: 25, Name: "Saltmarsh Roasters", Country: "RO", Kind: "Café", RType: "cafe", StateCode: "new", State: "not contacted yet",
			PlaceType: "roastery", PlaceTypeSource: "operator"},
	}
	p.CampaignTabs = []CampaignTab{{Slug: "cafes", Name: "Café loyalty", Open: 2293, Cafe: true}, {Slug: "drupal-eol", Name: "Drupal", Open: 7}}
	p.PlaceTypeCounts = &PlaceTypeCounts{Cafe: 2254, Roastery: 39}
	p.Progress = &CampaignProgress{Reviewed: 312, Total: 2293}
	p.Towns = []FilterOption{{Value: "Fictive Town", Label: "Fictive Town (12)"}}
	return p
}

// triageBody renders the list of triageList under query.
func triageBody(t *testing.T, query string) string {
	t.Helper()
	b := richBackend("x")
	b.list = triageList()
	h := newHarness(t, b, false)
	w := h.do(get("/leads"+query, h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("/leads%s: %d", query, w.Code)
	}
	return w.Body.String()
}

// TestLeadListTriageActions: every open lead offers Skip and a café lead Good and its place type
// switch, in both layouts; a Drupal lead has neither Good nor a switch; the operator's own skip
// offers Undo coming back to its row, and the system's skip shows its reason with no Undo. Each
// moving action's back names the next row's anchor (the row before, for the last row), so the
// page without its script lands on the next row.
func TestLeadListTriageActions(t *testing.T) {
	body := triageBody(t, "?campaign=cafes")
	count := func(s string) int { return strings.Count(body, s) }
	for _, c := range []struct {
		part string
		want int
	}{
		{`action="/leads/21/good" data-triage="good"`, 2},
		{`action="/leads/21/skip" data-triage="skip"`, 2},
		{`action="/leads/21/roastery" data-triage="roastery"`, 2},
		{`action="/leads/22/skip" data-triage="skip"`, 2},
		{`action="/leads/22/good"`, 0},
		{`action="/leads/22/roastery"`, 0},
		{`action="/leads/25/cafe" data-triage="cafe"`, 2},
		{`action="/leads/25/roastery"`, 0},
		{`action="/leads/23/skip"`, 0},
		{`action="/leads/23/unskip"`, 0},
		{`Skipped by LeadHound: Same place as lead 21`, 2},
		{`action="/leads/24/unskip" data-triage="unskip"><input type="hidden" name="back" value="/leads?campaign=cafes&amp;page=2#lead-w-24">`, 1},
		{`action="/leads/24/unskip" data-triage="unskip"><input type="hidden" name="back" value="/leads?campaign=cafes&amp;page=2#lead-n-24">`, 1},
		{`action="/leads/24/skip"`, 0},
		{`action="/leads/21/skip" data-triage="skip"><input type="hidden" name="back" value="/leads?campaign=cafes&amp;page=2#lead-w-22">`, 1},
		{`action="/leads/21/skip" data-triage="skip"><input type="hidden" name="back" value="/leads?campaign=cafes&amp;page=2#lead-n-22">`, 1},
		{`action="/leads/25/skip" data-triage="skip"><input type="hidden" name="back" value="/leads?campaign=cafes&amp;page=2#lead-w-24">`, 1},
		{`>Good<span class="visually-hidden"> Fictive Bean Café</span>`, 2},
		{`>Skip<span class="visually-hidden"> Northwind Trading</span>`, 2},
		{`>It&#39;s a coffee shop<span class="visually-hidden">: Saltmarsh Roasters</span>`, 2},
		{`<span class="badge badge-warn roastery-badge">roastery</span>`, 2},
		{`<th scope="col">Review</th>`, 1},
		{`data-key-hint hidden`, 1},
	} {
		if n := count(c.part); n != c.want {
			t.Errorf("%q shows %d times, want %d", c.part, n, c.want)
		}
	}
}

// TestLeadListTriageRow: the campaign tabs, the chips with Both, the progress line and the town
// select show; the summary is escaped text, clamped by its class; More spans every column and
// holds the address, a phone as a tel link, the email route as text, web links opening in a new
// tab, and an unsafe or schemeless social address as text.
func TestLeadListTriageRow(t *testing.T) {
	body := triageBody(t, "?campaign=cafes")
	for _, want := range []string{
		`<nav class="mt-2" aria-label="Campaigns">`, `>Café loyalty (2,293)</a>`, `>All campaigns (2,300)</a>`,
		`>Coffee shops <span class="chip-count">2,254</span></a>`, `>Roasteries <span class="chip-count">39</span></a>`,
		`>Both <span class="chip-count">2,293</span></a>`,
		`<strong>312</strong> of 2,293 reviewed`,
		`<select class="select" id="filter-town" name="town">`, `<option value="Fictive Town">Fictive Town (12)</option>`,
		`&lt;b&gt;Cosy&lt;/b&gt; corner &amp; &#34;bright&#34; &lt;script&gt;alert(1)&lt;/script&gt;`,
		`<p class="lead-summary"`,
		`<tr class="lead-extra"><td colspan="8">`,
		`<summary>More<span class="visually-hidden"> about Fictive Bean Café</span></summary>`,
		`<dt>Address</dt><dd>1 Fictive Street</dd>`,
		`<a href="tel:&#43;40256000111">&#43;40 256 000 111</a>`,
		`<dd>hello@bean.example · <a href="https://bean.example/contact" target="_blank" rel="noopener noreferrer nofollow">Contact form</a></dd>`,
		`<a href="https://facebook.example/fictivebean" target="_blank" rel="noopener noreferrer nofollow">Facebook</a>`,
		`Instagram (javascript:alert(1))`, `TikTok (//tiktok.example/fictivebean)`,
		`<a href="https://northwind.example" target="_blank" rel="noopener noreferrer nofollow">northwind.example</a>`,
		`<dt>Jev</dt><dd>72, likely match</dd>`, `<dt>Place type</dt><dd>Coffee shop, from its name</dd>`,
		`<dd>Roastery or online store, from your tag</dd>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/leads lacks %q", want)
		}
	}
	for _, bad := range []string{"<b>Cosy</b>", "<script>alert(1)", `href="javascript`, `href="//tiktok`, `mailto:`, `href="hello@`} {
		if strings.Contains(body, bad) {
			t.Errorf("/leads holds %q", bad)
		}
	}
	// Every link the page opens in a new tab keeps the opener away.
	for _, part := range strings.Split(body, `target="_blank"`)[1:] {
		if end := strings.IndexByte(part, '>'); end < 0 || !strings.Contains(part[:end], "noopener") {
			t.Errorf("a new-tab link lacks noopener: %.80s", part)
		}
	}
}

// TestLeadListTriageCols: the table's column count follows its optional columns, so More always
// spans the whole row.
func TestLeadListTriageCols(t *testing.T) {
	v := newLeadListView(LeadListPage{Rows: []LeadListRow{{LeadID: 1, StateCode: "contacted"}}})
	if v.Cols() != leadListBaseCols || v.ShowTriage {
		t.Errorf("a contacted lead alone: %d columns, review %v", v.Cols(), v.ShowTriage)
	}
	v = newLeadListView(LeadListPage{Filter: LeadListQuery{Sort: "order", Status: "all"},
		Rows: []LeadListRow{{LeadID: 1, StateCode: "new", State: "not contacted yet", Relevance: "50", Call: "likely", Position: "1"}}})
	if want := leadListBaseCols + 6; v.Cols() != want {
		t.Errorf("every optional column on: %d columns, want %d", v.Cols(), want)
	}
}

// TestLeadListTriagePlaceFilter: a café campaign's list on one place type marks itself, so the
// script takes a switched row away; on Both, or off a café campaign, it does not.
func TestLeadListTriagePlaceFilter(t *testing.T) {
	if body := triageBody(t, "?campaign=cafes"); !strings.Contains(body, `data-triage-list data-place-filtered>`) {
		t.Error("the coffee shops list is not marked as one place type")
	}
	if body := triageBody(t, "?campaign=cafes&place_type=all"); strings.Contains(body, "data-place-filtered") {
		t.Error("Both is marked as one place type")
	}
}

// TestLeadListInstagramChannel: a row reached on Instagram says so beside its other channels, a
// row with none says what it said before, and an Instagram filter option is offered by its label.
func TestLeadListInstagramChannel(t *testing.T) {
	b := richBackend("x")
	b.list = triageList()
	b.list.Rows[1].HasEmail, b.list.Rows[1].HasInstagram = true, true
	b.list.Rows[2].HasLinkedIn = true
	b.list.Options.Channels = append(b.list.Options.Channels, FilterOption{"instagram", "Instagram account"})
	h := newHarness(t, b, false)
	body := h.do(get("/leads?channel=instagram", h.session(t))).Body.String()
	for _, want := range []string{"email · Instagram", ">LinkedIn<", "no way to reach yet",
		`name="channel" value="instagram" checked> Instagram account</label>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the list lacks %q", want)
		}
	}
}
