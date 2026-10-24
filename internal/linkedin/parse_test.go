package linkedin

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // Europe/Bucharest without relying on the host's zoneinfo
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden.json from the parsers")

// goldenFetchedAt is the fetch time every detail golden is computed against.
var goldenFetchedAt = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) // #nosec G304 -- fixed test fixture names
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func marshalGolden(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	return buf.Bytes()
}

// checkGolden compares v with testdata/<name>.golden.json, rewriting it under -update.
func checkGolden(t *testing.T, name string, v any) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden.json")
	got := marshalGolden(t, v)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path) // #nosec G304 -- derived from fixed fixture names
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from golden; run `go test ./internal/linkedin/ -update` and review the diff\ngot:\n%s", path, got)
	}
}

// TestFixturesGolden runs every fixture page through its parser and compares the typed
// result with its golden file. search_broken and search_empty have their own tests.
func TestFixturesGolden(t *testing.T) {
	pages, err := filepath.Glob(filepath.Join("testdata", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) < 10 {
		t.Fatalf("expected the scrubbed fixture set, found %d pages", len(pages))
	}
	for _, p := range pages {
		name := strings.TrimSuffix(filepath.Base(p), ".html")
		t.Run(name, func(t *testing.T) {
			body := readFixture(t, name+".html")
			switch {
			case name == "search_broken" || name == "search_empty":
				t.Skip("covered by its own test")
			case strings.HasPrefix(name, "search"):
				cards, err := ParseSearch(body)
				if err != nil {
					t.Fatalf("ParseSearch: %v", err)
				}
				checkGolden(t, name, cards)
			case strings.HasPrefix(name, "detail"):
				d, err := ParseDetail(body, goldenFetchedAt)
				if err != nil {
					t.Fatalf("ParseDetail: %v", err)
				}
				checkGolden(t, name, d)
			default:
				t.Fatalf("fixture %s has no parser", name)
			}
		})
	}
}

func TestSearchBrokenIsParserBroke(t *testing.T) {
	cards, err := ParseSearch(readFixture(t, "search_broken.html"))
	if !errors.Is(err, ErrParserBroke) {
		t.Fatalf("err = %v, want ErrParserBroke", err)
	}
	if cards != nil {
		t.Fatalf("broken page returned %d cards; want none so nothing is written", len(cards))
	}
}

func TestSearchEmptyIsZeroCardsNoError(t *testing.T) {
	cards, err := ParseSearch(readFixture(t, "search_empty.html"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(cards) != 0 {
		t.Fatalf("got %d cards, want 0", len(cards))
	}
}

// TestSearchCardSelectorDriftIsParserBroke: the job URNs are still on the page, but the
// card container's class was renamed, so the card selector finds fewer cards (here none).
// That is a broken parser, never an empty page.
func TestSearchCardSelectorDriftIsParserBroke(t *testing.T) {
	card := func(id, class string) string {
		return `<li><div class="` + class + `" data-entity-urn="urn:li:jobPosting:` + id + `">
	<h3 class="base-search-card__title">Engineer</h3>
	<h4 class="base-search-card__subtitle"><a href="https://www.linkedin.com/company/northwind-labs">Northwind Labs</a></h4></div></li>`
	}
	cases := map[string]string{
		"every card renamed": card("1000000101", "job-search-card-v2") + card("1000000102", "job-search-card-v2"),
		"one card renamed":   card("1000000101", "base-search-card") + card("1000000102", "job-search-card-v2"),
	}
	for name, page := range cases {
		t.Run(name, func(t *testing.T) {
			cards, err := ParseSearch([]byte(page))
			if !errors.Is(err, ErrParserBroke) || cards != nil {
				t.Fatalf("ParseSearch = %d cards, %v; want none and ErrParserBroke", len(cards), err)
			}
		})
	}
	// The same cards with the expected class parse cleanly.
	cards, err := ParseSearch([]byte(card("1000000101", "base-search-card") + card("1000000102", "base-search-card")))
	if err != nil || len(cards) != 2 {
		t.Fatalf("ParseSearch = %d cards, %v; want 2, nil", len(cards), err)
	}
}

// TestSearchLinkCardVariant: LinkedIn sometimes renders a card as the link itself, an
// <a class="base-search-card ..."> carrying the job URN and the job href, with no inner
// full-card link and a company with no link. search_link_card.html is a scrubbed live page
// with nine div cards and one such link card; every card on it must parse, and the link
// card takes its URL from its own href.
func TestSearchLinkCardVariant(t *testing.T) {
	cards, err := ParseSearch(readFixture(t, "search_link_card.html"))
	if err != nil {
		t.Fatalf("ParseSearch: %v", err)
	}
	if len(cards) != pageSize {
		t.Fatalf("got %d cards, want %d", len(cards), pageSize)
	}
	var link *Card
	for i := range cards {
		if cards[i].ID == "1000000009" {
			link = &cards[i]
		}
	}
	if link == nil {
		t.Fatal("the link card (job 1000000009) was not parsed")
	}
	if link.URL != "https://www.linkedin.com/jobs/view/senior-platform-engineer-1000000009" {
		t.Errorf("link card URL = %q, want its own href, canonical", link.URL)
	}
	if link.Title == "" || link.Company == "" || link.CompanyURL != "" || link.CompanySlug != "" {
		t.Errorf("link card = %+v, want a title, a plain company name and no company link", *link)
	}

	// A minimal link card on its own parses too, and a link card that lost its title is
	// still a broken parser.
	linkCard := func(title string) string {
		return `<li><a class="base-card base-search-card base-search-card--link job-search-card"
	data-entity-urn="urn:li:jobPosting:1000000201" href="https://ro.linkedin.com/jobs/view/engineer-1000000201?position=9&amp;refId=x">
	<h3 class="base-search-card__title">` + title + `</h3>
	<h4 class="base-search-card__subtitle">Northwind Labs</h4></a></li>`
	}
	got, err := ParseSearch([]byte(linkCard("Engineer")))
	if err != nil || len(got) != 1 || got[0].URL != "https://www.linkedin.com/jobs/view/engineer-1000000201" {
		t.Fatalf("ParseSearch(link card) = %+v, %v; want one card with its canonical href", got, err)
	}
	if _, err := ParseSearch([]byte(linkCard(""))); !errors.Is(err, ErrParserBroke) {
		t.Fatalf("link card without a title: err = %v, want ErrParserBroke", err)
	}
}

// TestSearchEndOfResultsIsEmpty: past the last result LinkedIn answers 200 with a near
// empty document (a doctype and a comment). That is the end of the results: no cards and
// no error, never a broken parser.
func TestSearchEndOfResultsIsEmpty(t *testing.T) {
	for name, page := range map[string]string{
		"doctype and comment": "<!DOCTYPE html>\n\n<!---->  ",
		"empty body":          "",
		"whitespace":          "  \n",
	} {
		t.Run(name, func(t *testing.T) {
			cards, err := ParseSearch([]byte(page))
			if err != nil || len(cards) != 0 {
				t.Fatalf("ParseSearch = %d cards, %v; want 0, nil", len(cards), err)
			}
		})
	}
}

func TestSearchCardMissingCompanyIsParserBroke(t *testing.T) {
	page := `<li><div class="base-search-card" data-entity-urn="urn:li:jobPosting:1000000099">
	<h3 class="base-search-card__title">Engineer</h3>
	<h4 class="base-search-card__subtitle"></h4></div></li>`
	if _, err := ParseSearch([]byte(page)); !errors.Is(err, ErrParserBroke) {
		t.Fatalf("err = %v, want ErrParserBroke", err)
	}
}

func TestSearchDecodesEntities(t *testing.T) {
	page := `<li><div class="base-search-card" data-entity-urn="urn:li:jobPosting:1000000099">
	<a class="base-card__full-link" href="https://fr.linkedin.com/jobs/view/r-d-1000000099?position=1&amp;pageNum=0"></a>
	<h3 class="base-search-card__title">R&amp;D Engineer &#8211; Go</h3>
	<h4 class="base-search-card__subtitle"><a href="https://www.linkedin.com/company/smith-and-co?trk=x">Smith &amp; Co</a></h4>
	<span class="job-search-card__location">Exampleville</span>
	<time class="job-search-card__listdate--new" datetime="2026-10-23">now</time>
	</div></li>`
	cards, err := ParseSearch([]byte(page))
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("got %d cards", len(cards))
	}
	c := cards[0]
	if c.Title != "R&D Engineer – Go" || c.Company != "Smith & Co" {
		t.Errorf("title %q company %q: entities not decoded", c.Title, c.Company)
	}
	if c.CompanySlug != "smith-and-co" || c.CompanyURL != "https://www.linkedin.com/company/smith-and-co" {
		t.Errorf("company url %q slug %q", c.CompanyURL, c.CompanySlug)
	}
	if c.URL != "https://www.linkedin.com/jobs/view/r-d-1000000099" {
		t.Errorf("url %q not canonical", c.URL)
	}
	if want := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC); !c.ListDate.Equal(want) {
		t.Errorf("listdate %v, want %v (the --new variant)", c.ListDate, want)
	}
}

func TestClosedOnlyOnClosedFixture(t *testing.T) {
	pages, _ := filepath.Glob(filepath.Join("testdata", "detail_*.html"))
	for _, p := range pages {
		name := filepath.Base(p)
		d, err := ParseDetail(readFixture(t, name), goldenFetchedAt)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := name == "detail_closed.html"; d.Closed != want {
			t.Errorf("%s: Closed = %v, want %v", name, d.Closed, want)
		}
	}
}

func TestClosedTextInDescriptionIsNotClosed(t *testing.T) {
	page := `<section class="top-card-layout"><h2 class="top-card-layout__title">Engineer</h2>
	<a class="topcard__org-name-link" href="https://www.linkedin.com/company/acme">Acme</a></section>
	<div class="description__text"><div class="show-more-less-html__markup">
	<p class="closed-job">No longer accepting applications after Friday.</p></div></div>`
	d, err := ParseDetail([]byte(page), goldenFetchedAt)
	if err != nil {
		t.Fatal(err)
	}
	if d.Closed {
		t.Fatal("closed detected from the description; only the top card counts")
	}
}

func TestClosedTextInTopCard(t *testing.T) {
	page := `<section class="top-card-layout"><h1 class="top-card-layout__title">Engineer</h1>
	<a class="topcard__org-name-link" href="https://www.linkedin.com/company/acme">Acme</a>
	<span>No longer accepting applications</span></section>`
	d, err := ParseDetail([]byte(page), goldenFetchedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Closed {
		t.Fatal("banner text in the top card not detected")
	}
}

func TestDetailWithoutTitleIsParserBroke(t *testing.T) {
	page := `<section class="top-card-layout"><a class="topcard__org-name-link" href="https://www.linkedin.com/company/acme">Acme</a></section>`
	if _, err := ParseDetail([]byte(page), goldenFetchedAt); !errors.Is(err, ErrParserBroke) {
		t.Fatalf("err = %v, want ErrParserBroke", err)
	}
}

func TestApplyType(t *testing.T) {
	cases := []struct {
		name, markup, want string
	}{
		{"onsite", `<button data-tracking-control-name="public_jobs_apply-link-onsite">Apply</button>`, ApplyOnsite},
		{"simple onsite", `<button data-tracking-control-name="public_jobs_apply-link-simple_onsite">Apply</button>`, ApplySimple},
		{"simple plain", `<button data-tracking-control-name="public_jobs_apply-link-simple">Apply</button>`, ApplySimple},
		{"offsite icon", `<button>Apply<icon data-svg-class-name="apply-button__offsite-apply-icon-svg"></icon></button>`, ApplyOffsite},
		{"offsite prefix", `<a data-tracking-control-name="public_jobs_apply-link-offsite_sign-up-modal">Apply</a>`, ApplyOffsite},
		{"none", `<button>Save</button>`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := `<section class="top-card-layout"><h2 class="top-card-layout__title">Engineer</h2>` + tc.markup + `</section>`
			d, err := ParseDetail([]byte(page), goldenFetchedAt)
			if err != nil {
				t.Fatal(err)
			}
			if d.ApplyType != tc.want {
				t.Errorf("ApplyType = %q, want %q", d.ApplyType, tc.want)
			}
		})
	}
}

func TestParseApplicants(t *testing.T) {
	cases := []struct {
		text string
		want int
		ok   bool
	}{
		{"142 applicants", 142, true},
		{"  1 applicant ", 1, true},
		{"Over 200 applicants", 200, true},
		{"Be among the first 25 applicants", 25, true},
		{"1,234 applicants", 1234, true},
		{"", 0, false},
		{"Applicants unknown", 0, false},
	}
	for _, tc := range cases {
		got := ParseApplicants(tc.text)
		if !tc.ok {
			if got != nil {
				t.Errorf("%q: got %d, want nil", tc.text, *got)
			}
			continue
		}
		if got == nil || *got != tc.want {
			t.Errorf("%q: got %v, want %d", tc.text, got, tc.want)
		}
	}
}

func TestParsePosted(t *testing.T) {
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		text string
		want time.Time
		ok   bool
	}{
		{"6 days ago", at.AddDate(0, 0, -6), true},
		{"1 day ago", at.AddDate(0, 0, -1), true},
		{"2 hours ago", at.Add(-2 * time.Hour), true},
		{"1 hour ago", at.Add(-time.Hour), true},
		{"30 minutes ago", at.Add(-30 * time.Minute), true},
		{"1 week ago", at.AddDate(0, 0, -7), true},
		{"3 weeks ago", at.AddDate(0, 0, -21), true},
		{"1 month ago", at.AddDate(0, 0, -30), true},
		{"2 months ago", at.AddDate(0, 0, -60), true},
		{"1 year ago", at.AddDate(0, 0, -365), true},
		{"Just now", at, true},
		{"Reposted 2 days ago", at.AddDate(0, 0, -2), true},
		{"  Reposted  5 hours ago ", at.Add(-5 * time.Hour), true},
		{"", time.Time{}, false},
		{"yesterday-ish", time.Time{}, false},
	}
	for _, tc := range cases {
		got := ParsePosted(tc.text, at)
		if !tc.ok {
			if got != nil {
				t.Errorf("%q: got %v, want nil", tc.text, *got)
			}
			continue
		}
		if got == nil || !got.Equal(tc.want) {
			t.Errorf("%q: got %v, want %v", tc.text, got, tc.want)
		}
	}
}

// TestPostedAroundMidnight: "2 hours ago" fetched at 01:00 in Bucharest is an instant on
// the previous local day; bucketing into a day is DayIn's job, in the caller's zone.
func TestPostedAroundMidnight(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	fetched := time.Date(2026, 10, 23, 1, 0, 0, 0, loc)
	got := ParsePosted("2 hours ago", fetched)
	if got == nil {
		t.Fatal("not parsed")
	}
	if want := fetched.Add(-2 * time.Hour); !got.Equal(want) {
		t.Fatalf("instant %v, want %v", got, want)
	}
	if day := DayIn(*got, loc); !day.Equal(time.Date(2026, 10, 22, 0, 0, 0, 0, loc)) {
		t.Errorf("DayIn = %v, want 2026-10-22 in Bucharest", day)
	}
	// The same instant is still the 22nd in UTC, and the fetch itself is the 23rd locally
	// but the 22nd in UTC: the zone decides.
	if day := DayIn(fetched, loc); day.Day() != 23 {
		t.Errorf("fetch day in Bucharest = %v, want the 23rd", day)
	}
	if day := DayIn(fetched, time.UTC); day.Day() != 22 {
		t.Errorf("fetch day in UTC = %v, want the 22nd", day)
	}
}

// upstreamCardFields is the card field list transcribed from upstream helpers.ts
// (parseJobCards: id, title, company, location, date as the listdate), mapped to
// Card's fields. The TS selectors were verified against raw pages on 2026-10-23;
// if LinkedIn's markup drifts, the scrubbed fixtures and this list move together.
var upstreamCardFields = map[string]string{
	"title":    "Title",
	"company":  "Company",
	"location": "Location",
	"id":       "ID",
	"listdate": "ListDate",
}

// upstreamCardClasses are the class names helpers.ts keys each field on.
var upstreamCardClasses = []string{
	"base-search-card__title",
	"base-search-card__subtitle",
	"job-search-card__location",
	"job-search-card__listdate",
}

func TestContractDriftAgainstUpstream(t *testing.T) {
	ct := reflect.TypeFor[Card]()
	for up, field := range upstreamCardFields {
		if _, ok := ct.FieldByName(field); !ok {
			t.Errorf("upstream field %q has no Card.%s", up, field)
		}
	}
	table := strings.Join([]string{sel.card, sel.cardTitle, sel.cardCompany, sel.cardLocation, sel.cardListDate}, " ")
	for _, class := range upstreamCardClasses {
		if !strings.Contains(table, class) {
			t.Errorf("selector table lost upstream class %q", class)
		}
	}
	cards, err := ParseSearch(readFixture(t, "search.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) == 0 {
		t.Fatal("search fixture parsed to zero cards")
	}
	for _, c := range cards {
		v := reflect.ValueOf(c)
		for up, field := range upstreamCardFields {
			if v.FieldByName(field).IsZero() {
				t.Errorf("card %s: upstream field %q (Card.%s) is empty", c.ID, up, field)
			}
		}
	}
}
