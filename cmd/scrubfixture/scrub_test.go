package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// Test values are reserved or fictional. A profile path is built at runtime so this file
// never holds one whole.
var profilePath = "https://www.linkedin.com" + profileMarker + "jane-example"

func searchCard(id, slug, company string) string {
	return `<li><div class="base-card base-search-card" data-entity-urn="urn:li:jobPosting:` + id + `" data-tracking-id="abc123==" data-reference-id="def456==">` +
		`<a class="base-card__full-link" href="https://ro.linkedin.com/jobs/view/backend-dev-at-` + slug + `-` + id + `?position=1&amp;pageNum=0&amp;refId=xyz&amp;trackingId=abc&amp;trk=public_jobs" data-tracking-control-name="public_jobs_jserp-result_search-card"><span class="sr-only">Backend Developer</span></a>` +
		`<img class="artdeco-entity-image" data-delayed-url="https://media.licdn.com/logo.png" alt="` + company + `">` +
		`<div class="base-search-card__info"><h3 class="base-search-card__title">Backend Developer</h3>` +
		`<h4 class="base-search-card__subtitle"><a class="hidden-nested-link" href="https://ro.linkedin.com/company/` + slug + `?trk=public_jobs">` + company + `</a></h4>` +
		`<div class="base-search-card__metadata"><span class="job-search-card__location">Example City, Example County</span>` +
		`<div class="job-posting-benefits"><span class="job-posting-benefits__text">Actively Hiring</span></div>` +
		`<time class="job-search-card__listdate--new" datetime="2026-10-20">3 days ago</time></div></div></div></li>`
}

func detailPage(id, slug, company, description string) string {
	return `<section class="top-card-layout"><div class="top-card-layout__card">` +
		`<a href="https://ro.linkedin.com/company/` + slug + `?trk=public_jobs_topcard_logo"><img class="artdeco-entity-image" alt="` + company + `"></a>` +
		`<a href="https://ro.linkedin.com/jobs/view/backend-dev-` + id + `" class="topcard__link"><h2 class="top-card-layout__title topcard__title">Backend Developer</h2></a>` +
		`<h4 class="top-card-layout__second-subline"><div class="topcard__flavor-row"><span class="topcard__flavor">` +
		`<a class="topcard__org-name-link" href="https://ro.linkedin.com/company/` + slug + `?trk=x">` + company + `</a></span>` +
		`<span class="topcard__flavor topcard__flavor--bullet">Example City, Example County</span></div>` +
		`<div class="topcard__flavor-row"><span class="posted-time-ago__text">6 days ago</span>` +
		`<figure class="num-applicants__figure topcard__flavor--bullet"><figcaption class="num-applicants__caption">Over 200 applicants</figcaption></figure></div></h4>` +
		`<button class="apply-button" data-tracking-control-name="public_jobs_apply-link-onsite" data-tracking-id="tok==">Apply</button>` +
		`<button class="sign-up-modal__outlet" aria-label="Apply at ` + company + `">Apply<icon data-svg-class-name="apply-button__offsite-apply-icon-svg" data-delayed-url="https://static.licdn.com/sc/h/icon"></icon></button>` +
		`<form action="https://www.linkedin.com/uas/login-submit"><input name="loginCsrfParam" value="11111111-2222-3333-4444-555555555555" type="hidden"></form>` +
		`<code id="decoratedJobPostingId" style="display: none"><!--"` + id + `"--></code>` +
		`<script>var person = "Jane Example";</script>` +
		`<div class="ellipsis-menu"><a data-semaphore-content-urn="urn:li:jobPosting:` + id + `" href="https://www.linkedin.com/uas/login?session_redirect=%2Fjobs%2Fview%2F` + id + `">Report this job</a></div>` +
		`</div></section>` +
		`<div class="decorated-job-posting__details"><section class="core-section-container description"><div class="core-section-container__content">` +
		`<div class="` + recruiterClass + `"><p>Meet the hiring team</p><div class="base-card base-main-card">` +
		`<a class="base-card__full-link" href="` + profilePath + `"><span class="sr-only">Jane Example</span></a>` +
		`<h3 class="base-main-card__title">Jane Example</h3><h4 class="base-main-card__subtitle">Talent Partner at ` + company + `</h4>` +
		`<a class="` + recruiterClass + `__cta" aria-label="Message Jane Example" href="https://www.linkedin.com/login">Message</a></div></div>` +
		`<div class="description__text"><section class="show-more-less-html"><div class="show-more-less-html__markup">` + description +
		`</div><button class="show-more-less-html__button" aria-label="Show more">Show more</button></section></div>` +
		`<ul class="description__job-criteria-list">` +
		`<li class="description__job-criteria-item"><h3 class="description__job-criteria-subheader">Seniority level</h3><span class="description__job-criteria-text">Mid-Senior level</span></li>` +
		`<li class="description__job-criteria-item"><h3 class="description__job-criteria-subheader">Employment type</h3><span class="description__job-criteria-text">Full-time</span></li>` +
		`<li class="description__job-criteria-item"><h3 class="description__job-criteria-subheader">Industries</h3><span class="description__job-criteria-text">Widgets made by ` + company + `</span></li>` +
		`</ul></div></section></div>`
}

const englishSource = `<p><strong>Who we are</strong></p><p>Our small team builds payment software for shops across the region and we are hiring.</p>` +
	`<ul><li>Build and run services that handle many requests every day.</li><li>Write clear documentation for the rest of the company.</li></ul>`

var sources = map[Lang]string{
	LangEnglish: englishSource,
	LangFrench: `<p><strong>Qui sommes-nous</strong></p><p>Notre petite équipe développe un logiciel de paiement pour les commerçants de la région et nous recrutons un développeur.</p>` +
		`<ul><li>Construire et faire tourner des services qui traitent beaucoup de requêtes chaque jour.</li></ul>`,
	LangSpanish: `<p><strong>Quiénes somos</strong></p><p>Nuestro pequeño equipo desarrolla un programa de pagos para las tiendas de la región y estamos contratando.</p>` +
		`<ul><li>Construir y mantener servicios que atienden muchas peticiones cada día.</li></ul>`,
	LangDutch: `<p><strong>Wie wij zijn</strong></p><p>Ons kleine team bouwt betaalsoftware voor winkels in de regio en wij zoeken een nieuwe collega.</p>` +
		`<ul><li>Je bouwt en beheert diensten die elke dag veel verzoeken verwerken.</li></ul>`,
}

func scrub(t *testing.T, s *Scrubber, page string, opt Options) string {
	t.Helper()
	out, err := s.Scrub([]byte(page), opt)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	return string(out)
}

func TestDetailRemovesRecruiterImagesScriptsCodeAndTokens(t *testing.T) {
	page := detailPage("4000000123", "acme-example", "Acme Example", englishSource)
	out := scrub(t, NewScrubber(detectPhraseLang), page, Options{Kind: KindDetail})
	for rule, bad := range map[string]string{
		"recruiter card":   recruiterClass,
		"profile link":     profileMarker,
		"person name":      "Jane Example",
		"image":            "<img",
		"script":           "<script",
		"code blob":        "<code",
		"csrf token":       "11111111-2222",
		"tracking token":   "tok==",
		"tracking params":  "trk=",
		"redirect param":   "session_redirect",
		"original job id":  "4000000123",
		"original company": "Acme Example",
		"original slug":    "acme-example",
		"logo url":         "media.licdn.com",
	} {
		if strings.Contains(out, bad) {
			t.Errorf("rule %s: output still contains it", rule)
		}
	}
}

func TestDetailKeepsParserMarkup(t *testing.T) {
	out := scrub(t, NewScrubber(detectPhraseLang), detailPage("4000000123", "acme-example", "Acme Example", englishSource), Options{Kind: KindDetail})
	for _, want := range []string{
		`class="top-card-layout__title topcard__title"`,
		`class="topcard__org-name-link"`,
		`class="topcard__flavor topcard__flavor--bullet">Exampleville, Country<`,
		`class="posted-time-ago__text">6 days ago<`,
		`class="num-applicants__caption">Over 200 applicants<`,
		`data-tracking-control-name="public_jobs_apply-link-onsite"`,
		`data-svg-class-name="apply-button__offsite-apply-icon-svg"`,
		`<h3 class="description__job-criteria-subheader">Seniority level</h3><span class="description__job-criteria-text">Mid-Senior level</span>`,
		`<h3 class="description__job-criteria-subheader">Employment type</h3><span class="description__job-criteria-text">Full-time</span>`,
		`<h3 class="description__job-criteria-subheader">Industries</h3><span class="description__job-criteria-text">Software Development</span>`,
		`class="show-more-less-html__markup"`,
		`<strong>About the role</strong>`,
		`<ul><li>`,
		`Senior Platform Engineer 1`,
		`href="https://www.linkedin.com/jobs/view/senior-platform-engineer-1000000001"`,
		`href="https://www.linkedin.com/company/northwind-labs"`,
		`>Northwind Labs<`,
		`data-semaphore-content-urn="urn:li:jobPosting:1000000001"`,
		`Fully remote within the EU.`,
		`Experience with Go and PostgreSQL.`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

func TestSearchCardsRewriteIDsURLsAndCompaniesConsistently(t *testing.T) {
	s := NewScrubber(detectPhraseLang)
	search := searchCard("4000000111", "acme-example", "Acme Example") +
		searchCard("4000000222", "globex-example", "Globex Example") +
		searchCard("4000000333", "acme-example", "Acme Example")
	out := scrub(t, s, search, Options{Kind: KindSearch})
	for _, want := range []string{
		`data-entity-urn="urn:li:jobPosting:1000000001"`,
		`data-entity-urn="urn:li:jobPosting:1000000002"`,
		`data-entity-urn="urn:li:jobPosting:1000000003"`,
		`href="https://www.linkedin.com/jobs/view/senior-platform-engineer-1000000002"`,
		`<h3 class="base-search-card__title">Senior Platform Engineer 2</h3>`,
		`<span class="sr-only">Senior Platform Engineer 3</span>`,
		`data-tracking-id="AAAAAAAAAAAAAAAAAAAAAA=="`,
		`class="job-search-card__location">Exampleville, Country<`,
		`class="job-posting-benefits__text">Actively Hiring<`,
		`datetime="2026-10-17">3 days ago<`, // shifted by dateShift
	} {
		if !strings.Contains(out, want) {
			t.Errorf("search output lacks %q", want)
		}
	}
	if n := strings.Count(out, `>Northwind Labs<`); n != 2 {
		t.Errorf("first company named Northwind Labs %d times, want 2", n)
	}
	if n := strings.Count(out, `>Contoso Systems<`); n != 1 {
		t.Errorf("second company named Contoso Systems %d times, want 1", n)
	}
	// A detail page scrubbed later in the same run keeps the card's IDs and names.
	detail := scrub(t, s, detailPage("4000000222", "globex-example", "Globex Example", englishSource), Options{Kind: KindDetail})
	for _, want := range []string{"senior-platform-engineer-1000000002", "Senior Platform Engineer 2", ">Contoso Systems<", "/company/contoso-systems"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail output lacks %q", want)
		}
	}
	if strings.Contains(out+detail, "trackingId") || strings.Contains(out+detail, "refId") {
		t.Error("rule tracking-param: a tracking parameter survived")
	}
}

func TestDescriptionKeepsLanguageAndStructure(t *testing.T) {
	for lang, desc := range sources {
		t.Run(string(lang), func(t *testing.T) {
			out := scrub(t, NewScrubber(detectPhraseLang), detailPage("4000000123", "acme-example", "Acme Example", desc), Options{Kind: KindDetail})
			_, nodes, _, err := parse([]byte(out))
			if err != nil {
				t.Fatal(err)
			}
			text := descriptionText(nodes)
			if got := Lang(detectLanguage(text)); got != lang {
				t.Errorf("output description detected as %q, want %q", got, lang)
			}
			if strings.Contains(strings.ToLower(text), "lorem") {
				t.Error("lorem ipsum in output")
			}
			if !strings.Contains(out, "<p><strong>"+phrases[lang].headings[0]+"</strong></p>") {
				t.Error("heading structure lost")
			}
			if c := strings.Count(out, "<li>"); c < 1 {
				t.Error("list structure lost")
			}
			hasMarkers := strings.Contains(text, englishMarkers[0]) && strings.Contains(text, englishMarkers[1])
			if hasMarkers != (lang == LangEnglish) {
				t.Errorf("English marker lines present=%v for %s", hasMarkers, lang)
			}
		})
	}
}

func TestScrubIsDeterministic(t *testing.T) {
	run := func() string {
		s := NewScrubber(detectPhraseLang)
		a := scrub(t, s, searchCard("4000000111", "acme-example", "Acme Example")+searchCard("4000000222", "globex-example", "Globex Example"), Options{Kind: KindSearch})
		b := scrub(t, s, detailPage("4000000222", "globex-example", "Globex Example", sources[LangFrench]), Options{Kind: KindDetail})
		return a + b
	}
	first := run()
	for range 3 {
		if run() != first {
			t.Fatal("two scrubs of the same input differ")
		}
	}
}

func TestClosedAndBrokenVariants(t *testing.T) {
	s := NewScrubber(detectPhraseLang)
	closed := scrub(t, s, detailPage("4000000123", "acme-example", "Acme Example", englishSource), Options{Kind: KindDetail, Closed: true})
	if !strings.Contains(closed, closedBanner) {
		t.Error("closed banner missing")
	}
	if !strings.Contains(closed, "upstream-documented markup, not observed") {
		t.Error("closed banner comment missing")
	}
	top := closed[:strings.Index(closed, "decorated-job-posting__details")]
	if !strings.Contains(top, "No longer accepting applications") {
		t.Error("banner not inside the top card")
	}
	broken := scrub(t, s, searchCard("4000000111", "acme-example", "Acme Example"), Options{Kind: KindSearch, RenameTitleClass: true})
	if strings.Contains(broken, `"base-search-card__title"`) || !strings.Contains(broken, "base-search-card__heading") {
		t.Error("title class not renamed")
	}
}

func TestProfileLinksEmailsAndAddressesDropped(t *testing.T) {
	// A blocked address and a public IPv4 are built at runtime, never written whole.
	mail := "jane" + "@" + "acme-example.com"
	ip := "10.1.2" + ".3"
	page := `<div class="x"><a href="` + profilePath + `" data-person="` + profilePath + `">Someone</a>` +
		`<a href="https://careers.acme-example.com/jobs/1?utm=1">Careers</a>` +
		`<span data-contact="` + mail + `" data-host="` + ip + `">text</span></div>`
	out := scrub(t, NewScrubber(nil), page, Options{Kind: KindDetail})
	if strings.Contains(out, profileMarker) {
		t.Error("rule profile-link: survived")
	}
	if strings.Contains(out, "acme-example.com") {
		t.Error("rule external-host: survived")
	}
	if !strings.Contains(out, `href="`+companyWebsite+`"`) {
		t.Error("external link not rewritten to the fictional website")
	}
	if strings.Contains(out, ip) || !strings.Contains(out, "someone@example.invalid") {
		t.Error("rule email/ipv4: not replaced")
	}
}

func TestCompanyModeDropsPeopleAndRewritesWebsite(t *testing.T) {
	page := `<html><head><title>Acme Example | LinkedIn</title><meta property="og:url" content="https://www.linkedin.com/company/acme-example"><script>{"name":"Acme Example"}</script></head><body>` +
		`<section class="top-card-layout"><img class="logo" data-delayed-url="https://media.licdn.com/logo.png"><h1 class="top-card-layout__title">Acme Example</h1>` +
		`<h4 class="top-card-layout__second-subline">Software Development · Example City</h4></section>` +
		`<section class="about-us"><dl><div data-test-id="about-us__website"><dt>Website</dt><dd>` +
		`<a data-tracking-control-name="about_website" href="https://www.linkedin.com/redir/redirect?url=https%3A%2F%2Facme-example.com">acme-example.com</a></dd></div></dl></section>` +
		`<section class="core-section-container"><h2>Employees at Acme Example</h2><ul><li><a class="base-card" href="` + profilePath + `"><h3>Jane Example</h3></a></li></ul></section>` +
		`<section class="core-section-container"><h2>Similar pages</h2><ul>` +
		`<li><a class="base-card base-main-card" href="` + profilePath + `"><img src="x"><h3>John Example</h3></a></li>` +
		`<li><a class="base-card" href="https://ro.linkedin.com/company/globex-example?trk=similar"><h3>Globex Example</h3></a></li></ul></section>` +
		`</body></html>`
	out := scrub(t, NewScrubber(nil), page, Options{Kind: KindCompany})
	for rule, bad := range map[string]string{
		"profile-link": profileMarker, "person": "Jane Example", "person2": "John Example",
		"employees-section": "Employees at", "image": "<img", "script": "<script", "meta": "<meta",
		"company": "Acme Example", "other-company": "Globex", "website": "acme-example.com",
	} {
		if strings.Contains(out, bad) {
			t.Errorf("rule %s: survived", rule)
		}
	}
	for _, want := range []string{
		`href="https://www.northwind.example"`,
		`<h1 class="top-card-layout__title">Northwind Labs</h1>`,
		`href="https://www.linkedin.com/company/contoso-systems"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("company output lacks %q", want)
		}
	}
}

func TestLeaksNamesRulesWithoutValues(t *testing.T) {
	orig := Originals{JobIDs: []string{"4000000123"}, Slugs: []string{"ing"}, Companies: []string{"Acme Example"}}
	clean := []byte(`<p>Engineering at Northwind Labs, job 1000000001</p>`)
	if got := Leaks(clean, orig); len(got) != 0 {
		t.Errorf("clean page flagged: %v", got)
	}
	dirty := []byte(`<p class="` + recruiterClass + `">acme example 4000000123 ing</p>`)
	got := Leaks(dirty, orig)
	for _, rule := range []string{"recruiter-class", "original-job-id", "original-company-slug", "original-company-name"} {
		if !slices.Contains(got, rule) {
			t.Errorf("rule %s not reported (got %v)", rule, got)
		}
	}
	for _, r := range got {
		if bytes.Contains([]byte(r), []byte("4000000123")) {
			t.Error("a rule name carries the matched value")
		}
	}
}

func TestDatesShiftAndCountsMove(t *testing.T) {
	for in, want := range map[string]string{
		"2026-10-20":           "2026-10-17",
		"2026-03-01":           "2026-02-26",
		"2026-10-20T08:00:00Z": "2026-10-17T08:00:00Z",
		"soon":                 "2026-01-01",
	} {
		if got := shiftDate(in); got != want {
			t.Errorf("shiftDate(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"116 applicants":                   "123 applicants",
		"195 applicants":                   "188 applicants",
		"1 applicant":                      "8 applicants",
		"Over 200 applicants":              "Over 200 applicants",
		"Be among the first 25 applicants": "Be among the first 25 applicants",
	} {
		if got := perturbCount(in); got != want {
			t.Errorf("perturbCount(%q) = %q, want %q", in, got, want)
		}
	}
}
