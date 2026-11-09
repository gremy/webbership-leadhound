package console

import (
	"html/template"
	"net/http"
	"net/url"
	"regexp"
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
			Summary:      `<b>Cosy</b> corner & "bright" <script>alert(1)</script> · Fictive Town · no website`,
			SummaryShort: `<b>Cosy</b> corner & "bright" <script>alert(1)</script>`,
			Tile:         "F", BusinessKind: "neighbourhood_cafe",
			Finding: "No loyalty app seen", FindingReason: "No loyalty vendor on its pages",
			RelevanceWords: "72, likely match",
			Details: LeadRowDetails{
				Address: "1 Fictive Street",
				About:   `<i>Fresh</i> bakes daily`,
				Kind:    "Neighbourhood café",
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
// select show; in the table the short line is More's own summary, escaped text clamped by its
// class with the whole line in its title, and the cards show the whole line; More opens inside
// the Lead cell and holds About, the kind, Jev's reading, the address, a phone as a tel link,
// the email route as text, web links opening in a new tab, an unsafe or schemeless social
// address as text, and the place type switch; Where shows the town over the country code.
func TestLeadListTriageRow(t *testing.T) {
	body := triageBody(t, "?campaign=cafes")
	for _, want := range []string{
		`<nav class="mt-2" aria-label="Campaigns">`, `>Café loyalty (2,293)</a>`, `>All campaigns (2,300)</a>`,
		`>Coffee shops <span class="chip-count">2,254</span></a>`, `>Roasteries <span class="chip-count">39</span></a>`,
		`>Both <span class="chip-count">2,293</span></a>`,
		`<strong>312</strong> of 2,293 reviewed`,
		`<select class="select" id="filter-town" name="town">`, `<option value="Fictive Town">Fictive Town (12)</option>`,
		`<summary><span class="lead-summary" title="&lt;b&gt;Cosy&lt;/b&gt; corner &amp; &#34;bright&#34; &lt;script&gt;alert(1)&lt;/script&gt; · Fictive Town · no website">&lt;b&gt;Cosy&lt;/b&gt; corner &amp; &#34;bright&#34; &lt;script&gt;alert(1)&lt;/script&gt;</span><span class="lead-more-label">More<span class="visually-hidden"> about Fictive Bean Café</span></span></summary>`,
		`<p class="lead-summary" title="&lt;b&gt;Cosy&lt;/b&gt; corner &amp; &#34;bright&#34; &lt;script&gt;alert(1)&lt;/script&gt; · Fictive Town · no website">`,
		`<summary><span class="lead-more-label">More<span class="visually-hidden"> about Northwind Trading</span></span></summary>`,
		`<span class="lead-tile" aria-hidden="true">F</span>`,
		`<td class="where-cell">Fictive Town<span class="cell-sub">RO</span></td>`, `<td class="where-cell">GB</td>`,
		`<dt>About</dt><dd>&lt;i&gt;Fresh&lt;/i&gt; bakes daily</dd>`, `<dt>Kind</dt><dd>Café</dd>`,
		`<dt>Jev read it as</dt><dd>Neighbourhood café</dd>`,
		`<div class="row-actions lead-triage lead-switch" data-lead="21" data-name="Fictive Bean Café"><form method="post" action="/leads/21/roastery" data-triage="roastery">`,
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
	for _, bad := range []string{"<b>Cosy</b>", "<script>alert(1)", "<i>Fresh", `href="javascript`, `href="//tiktok`, `mailto:`, `href="hello@`,
		`class="lead-extra"`, "colspan", `<th scope="col">Kind</th>`, `<th scope="col">Country</th>`} {
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

// TestLeadListOneRowPerLead renders every combination of the table's optional columns and
// checks that each lead is one table row, so nothing below it needs a span, and that the row
// has exactly as many cells as the header has columns.
func TestLeadListOneRowPerLead(t *testing.T) {
	groupRe := regexp.MustCompile(`(?s)<tbody class="lead-group"[^>]*>(.*?)</tbody>`)
	for mask := range 1 << 7 {
		on := func(bit int) bool { return mask&(1<<bit) != 0 }
		row := LeadListRow{LeadID: 7, Name: "Fictive Row", Country: "RO", Kind: "Café", RType: "cafe",
			StateCode: "contacted", State: "contacted", Campaign: "Café loyalty", Starred: true}
		q := url.Values{}
		if on(0) {
			row.Relevance = "50, maybe a match"
		}
		if on(1) {
			row.Call = "likely"
		}
		if on(2) {
			row.Position = "1"
		}
		if on(3) {
			q.Set("status", "all")
		}
		if on(4) {
			q.Set("sort", "order")
		}
		if on(5) {
			row.StateCode, row.State = "new", "not contacted yet"
		}
		if on(6) {
			q.Set("campaign", "cafes")
		}
		b := richBackend("x")
		b.list = triageList()
		b.list.Rows = []LeadListRow{row, row}
		b.list.Rows[1].LeadID = 8
		h := newHarness(t, b, false)
		w := h.do(get("/leads?"+q.Encode(), h.session(t)))
		if w.Code != http.StatusOK {
			t.Fatalf("mask %07b: %d", mask, w.Code)
		}
		_, table, _ := strings.Cut(w.Body.String(), `<table class="table lead-table">`)
		table, _, _ = strings.Cut(table, "</table>")
		head, _, _ := strings.Cut(table, "</thead>")
		cols := strings.Count(head, "<th ")
		groups := groupRe.FindAllStringSubmatch(table, -1)
		if len(groups) != 2 {
			t.Fatalf("mask %07b: %d lead groups, want 2", mask, len(groups))
		}
		for _, g := range groups {
			if n := strings.Count(g[1], "<tr"); n != 1 {
				t.Errorf("mask %07b: a lead holds %d table rows, want 1", mask, n)
			}
			// No cell holds a table of its own, so the opening tags count the row's cells.
			if n := strings.Count(g[1], "<td"); n != cols {
				t.Errorf("mask %07b: a row has %d cells under %d header columns", mask, n, cols)
			}
		}
		if kind := strings.Contains(head, `<th scope="col">Kind</th>`); kind == on(6) {
			t.Errorf("mask %07b: the Kind column shows %v with one campaign's list %v", mask, kind, on(6))
		}
	}
}

// TestLeadListLongLine: a very long name and line each keep to one clamped line, with the
// whole text, escaped, in the title.
func TestLeadListLongLine(t *testing.T) {
	long := strings.Repeat(`Fictive & "long" <name> `, 12)
	b := richBackend("x")
	b.list = triageList()
	b.list.Rows = []LeadListRow{{LeadID: 9, Name: long, Country: "RO", Kind: "Café", RType: "cafe", StateCode: "new",
		Summary: long + " · Fictive Town", SummaryShort: long}}
	h := newHarness(t, b, false)
	body := h.do(get("/leads?campaign=cafes", h.session(t))).Body.String()
	esc := template.HTMLEscapeString
	for _, want := range []string{
		`<a class="cell-main" href="/leads/9" title="` + esc(long) + `">`,
		`<span class="lead-summary" title="` + esc(long+" · Fictive Town") + `">` + esc(long) + `</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/leads lacks %.120q", want)
		}
	}
	if strings.Contains(body, "<name>") {
		t.Error("the long name is not escaped")
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

// TestSourceWords names who decided a row's place type, Instagram included, and passes an
// unknown source through as itself.
func TestSourceWords(t *testing.T) {
	for src, want := range map[string]string{
		"operator": "your tag", "jev": "Jev", "name": "its name", "instagram": "Instagram",
		"": "", "other": "other",
	} {
		if got := (triageView{Row: LeadListRow{PlaceTypeSource: src}}).SourceWords(); got != want {
			t.Errorf("SourceWords(%q) = %q, want %q", src, got, want)
		}
	}
}
