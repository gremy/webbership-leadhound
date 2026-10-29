package contacts

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/robots"
)

var testRoles = []string{"info", "office", "contact", "hello", "sales"}

// personLike are the person-like addresses the fixtures hold; no output may ever equal one.
var personLike = []string{"john.smith@northwind.example", "j.smith@northwind.example"}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: tests read their own fixtures by name
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// cfEncode builds a Cloudflare email-protection token for addr (key byte, then each byte
// XORed with it), so the fixture holds no plain address and the test can prove Extract
// never decodes it.
func cfEncode(addr string) string {
	const key = 0x5a
	b := []byte{key}
	for i := 0; i < len(addr); i++ {
		b = append(b, addr[i]^key)
	}
	return hex.EncodeToString(b)
}

// protectedAddr is a role address that only appears encoded, built at runtime.
var protectedAddr = "contact" + "@" + "northwind.example"

func contactPage(t *testing.T) []byte {
	t.Helper()
	return []byte(strings.ReplaceAll(string(readFixture(t, "contact.html")), "CFEMAIL", cfEncode(protectedAddr)))
}

func TestExtract(t *testing.T) {
	foreignMail := "info" + "@" + "gmail" + ".com" // a free-mail address, built at runtime
	tests := []struct {
		name       string
		body       []byte
		wantEmails []string
		person     int
		foreign    int
		protected  int
		forms      int
	}{
		{
			name: "contact page: roles kept, persons dropped, foreign and protected counted, form found",
			body: contactPage(t),
			wantEmails: []string{
				"office@northwind.example", "info@northwind.example",
				"office.london@northwind.example", "sales@shop.northwind.example",
			},
			person: 2, foreign: 1, protected: 1, forms: 1,
		},
		{
			name:    "a free-mail role-looking address is foreign",
			body:    []byte(`<p>Mail us: ` + foreignMail + `</p><a href="mailto:` + foreignMail + `">x</a>`),
			foreign: 1,
		},
		{
			name:       "mailto with query and several addresses, percent-encoded",
			body:       []byte(`<a href="MAILTO:info%40northwind.example,hello@northwind.example?cc=john.smith@northwind.example">x</a>`),
			wantEmails: []string{"info@northwind.example", "hello@northwind.example"},
		},
		{
			name:   "a role prefix needs a separator: officer is not office",
			body:   []byte(`<p>officer@northwind.example infox@northwind.example office-@northwind.example</p>`),
			person: 3,
		},
		{
			name:  "form needs a textarea and an email field",
			body:  []byte(`<form><input name="your-email"><textarea></textarea></form>`),
			forms: 1,
		},
		{
			name: "a search form is not a contact form",
			body: []byte(`<form><input type="search" name="q"></form><form><textarea></textarea></form>`),
		},
		{
			name: "script and style text is not read",
			body: []byte(`<script>a="info@northwind.example"</script><style>b{}</style>`),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Extract("https://northwind.example/contact", tc.body, "northwind.example", testRoles)
			if !reflect.DeepEqual(r.Emails, tc.wantEmails) && (len(r.Emails) != 0 || len(tc.wantEmails) != 0) {
				t.Errorf("Emails = %q, want %q", r.Emails, tc.wantEmails)
			}
			if r.PersonLike != tc.person || r.Foreign != tc.foreign || r.Protected != tc.protected {
				t.Errorf("counts person/foreign/protected = %d/%d/%d, want %d/%d/%d",
					r.PersonLike, r.Foreign, r.Protected, tc.person, tc.foreign, tc.protected)
			}
			if len(r.Forms) != tc.forms {
				t.Errorf("Forms = %q, want %d", r.Forms, tc.forms)
			}
			for _, f := range r.Forms {
				if f != "https://northwind.example/contact" {
					t.Errorf("form %q is not the page URL", f)
				}
			}
			assertNoPerson(t, r.Emails, r.Forms)
		})
	}
}

func TestExtractNeverDecodesProtected(t *testing.T) {
	r := Extract("https://northwind.example/contact", contactPage(t), "northwind.example", testRoles)
	for _, e := range r.Emails {
		if e == protectedAddr {
			t.Fatalf("protected address %q was decoded", e)
		}
	}
	if r.Protected != 1 {
		t.Errorf("Protected = %d, want 1 (link and span are one address)", r.Protected)
	}
}

func TestContactLinks(t *testing.T) {
	tests := []struct {
		name string
		base string
		file string
		want []string
	}{
		{
			name: "Romanian: contact before despre, foreign host skipped",
			base: "https://www.northwind.example/",
			file: "home_ro.html",
			want: []string{"https://www.northwind.example/contact", "https://www.northwind.example/despre-noi"},
		},
		{
			name: "German Impressum: fragment dropped, same-domain host kept, repeat deduped",
			base: "https://kontor.example/",
			file: "home_de.html",
			want: []string{"https://www.kontor.example/impressum", "https://kontor.example/impressum"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base, _ := url.Parse(tc.base)
			got := ContactLinks(base, readFixture(t, tc.file), base.Hostname())
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ContactLinks = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestContactLinksRules(t *testing.T) {
	base, _ := url.Parse("https://northwind.example/")
	body := []byte(`
		<a href="/about">About</a>
		<a href="javascript:void(0)">Contact</a>
		<a href="ftp://northwind.example/contact">Contact</a>
		<a href="/">Contact home</a>
		<a href="/kontakt">Kontakt</a>
		<a href="/contactati-ne">Scrie</a>
		<a href="/x">Contact us</a>`)
	got := ContactLinks(base, body, "northwind.example")
	want := []string{"https://northwind.example/contactati-ne", "https://northwind.example/x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ContactLinks = %q, want %q", got, want)
	}
}

// fakeFetcher serves pages by URL and counts every Get.
type fakeFetcher struct {
	pages map[string]string
	errs  map[string]error
	moves map[string]string // URL → the final URL it redirects to
	gets  []string
}

func (f *fakeFetcher) Get(_ context.Context, kind fetch.Kind, rawURL string) (fetch.Response, int64, error) {
	if kind != fetch.KindSite {
		return fetch.Response{}, 0, errors.New("unexpected kind " + string(kind))
	}
	f.gets = append(f.gets, rawURL)
	id := int64(100 + len(f.gets))
	if err := f.errs[rawURL]; err != nil {
		return fetch.Response{}, 0, err
	}
	final := rawURL
	if m, ok := f.moves[rawURL]; ok {
		final = m
	}
	body, ok := f.pages[final]
	if !ok {
		return fetch.Response{Status: 404, FinalURL: final}, id, nil
	}
	return fetch.Response{Status: 200, FinalURL: final, Body: []byte(body)}, id, nil
}

func robotsFrom(txt string) RobotsFor {
	rules := robots.Parse([]byte(txt))
	return func(context.Context, string, string) (robots.Verdict, *robots.Rules) {
		return robots.VerdictRules, rules
	}
}

type fakeResolver struct {
	mx  []*net.MX
	err error
}

func (r fakeResolver) LookupMX(context.Context, string) ([]*net.MX, error) { return r.mx, r.err }

const homeURL = "https://northwind.example/"

func homeWithLinks(links ...string) Page {
	var b strings.Builder
	for _, l := range links {
		b.WriteString(`<a href="` + l + `">Contact</a>`)
	}
	b.WriteString(`<p>info@northwind.example</p>`)
	return Page{FetchID: 1, URL: homeURL, Body: []byte(b.String())}
}

func TestFind(t *testing.T) {
	contact := string(contactPage(t))
	org := Org{Domain: "northwind.example", Roles: testRoles, UserAgent: "LeadHound/1",
		Resolver: fakeResolver{mx: []*net.MX{{Host: "mx.northwind.example.", Pref: 10}}}}

	t.Run("at most 3 pages, routes cite their page, evidence only for yielding pages", func(t *testing.T) {
		f := &fakeFetcher{pages: map[string]string{
			homeURL + "contact": contact,
			homeURL + "kontakt": `<p>nothing here</p>`,
			homeURL + "about":   `<p>hello@northwind.example</p>`,
		}}
		home := homeWithLinks("/contact", "/kontakt", "/about")
		got, ev := Find(context.Background(), f, robotsFrom(""), org, home)
		if len(f.gets) != 2 || got.Pages != MaxPages {
			t.Fatalf("gets = %q, pages = %d; want 2 gets and %d pages", f.gets, got.Pages, MaxPages)
		}
		wantRoutes := []Route{
			{Kind: KindEmail, Value: "info@northwind.example", FetchID: 1, URL: homeURL, MXOK: true},
			{Kind: KindEmail, Value: "office@northwind.example", FetchID: 101, URL: homeURL + "contact", MXOK: true},
			{Kind: KindEmail, Value: "office.london@northwind.example", FetchID: 101, URL: homeURL + "contact", MXOK: true},
			{Kind: KindEmail, Value: "sales@shop.northwind.example", FetchID: 101, URL: homeURL + "contact", MXOK: true},
			{Kind: KindForm, Value: homeURL + "contact", FetchID: 101, URL: homeURL + "contact"},
		}
		if !reflect.DeepEqual(got.Routes, wantRoutes) {
			t.Errorf("Routes =\n%+v\nwant\n%+v", got.Routes, wantRoutes)
		}
		if got.PersonLike != 2 || got.Foreign != 1 || got.Protected != 1 {
			t.Errorf("counts = %d/%d/%d, want 2/1/1", got.PersonLike, got.Foreign, got.Protected)
		}
		var ids []int64
		for _, e := range ev {
			ids = append(ids, e.FetchID)
		}
		if !reflect.DeepEqual(ids, []int64{1, 101}) {
			t.Errorf("evidence ids = %v, want [1 101] (the empty kontakt page yields nothing)", ids)
		}
		assertNoPersonRoutes(t, got.Routes)
	})

	t.Run("robots disallowing /contact: not fetched", func(t *testing.T) {
		f := &fakeFetcher{pages: map[string]string{homeURL + "contact": contact}}
		got, _ := Find(context.Background(), f, robotsFrom("User-agent: *\nDisallow: /contact\n"), org, homeWithLinks("/contact"))
		if len(f.gets) != 0 {
			t.Fatalf("fetched %q despite robots", f.gets)
		}
		if got.RobotsSkipped != 1 || got.Pages != 1 {
			t.Errorf("RobotsSkipped = %d, Pages = %d; want 1, 1", got.RobotsSkipped, got.Pages)
		}
	})

	t.Run("robots skip verdict: nothing fetched", func(t *testing.T) {
		f := &fakeFetcher{}
		skip := func(context.Context, string, string) (robots.Verdict, *robots.Rules) { return robots.VerdictSkip, nil }
		Find(context.Background(), f, skip, org, homeWithLinks("/contact"))
		if len(f.gets) != 0 {
			t.Fatalf("fetched %q on a skip verdict", f.gets)
		}
	})

	t.Run("not public: skipped quietly", func(t *testing.T) {
		f := &fakeFetcher{errs: map[string]error{homeURL + "contact": fetch.ErrNotPublic}}
		got, _ := Find(context.Background(), f, robotsFrom(""), org, homeWithLinks("/contact"))
		if got.Stopped != nil || got.FetchFailed != 1 || len(got.Routes) != 1 {
			t.Errorf("Stopped = %v, FetchFailed = %d, routes = %d; want nil, 1, 1", got.Stopped, got.FetchFailed, len(got.Routes))
		}
	})

	t.Run("breaker open: stops and reports", func(t *testing.T) {
		open := &fetch.OpenError{Reason: fetch.StopBudget}
		f := &fakeFetcher{errs: map[string]error{homeURL + "contact": open}}
		got, _ := Find(context.Background(), f, robotsFrom(""), org, homeWithLinks("/contact", "/kontakt"))
		if got.Stopped == nil || len(f.gets) != 1 {
			t.Errorf("Stopped = %v, gets = %q; want the open error after one get", got.Stopped, f.gets)
		}
	})

	t.Run("MX false: route kept with MXOK false", func(t *testing.T) {
		noMX := org
		noMX.Resolver = fakeResolver{mx: []*net.MX{{Host: "."}}}
		got, _ := Find(context.Background(), &fakeFetcher{}, robotsFrom(""), noMX, homeWithLinks())
		if len(got.Routes) != 1 || got.Routes[0].MXOK {
			t.Errorf("Routes = %+v, want one route with MXOK false", got.Routes)
		}
	})
}

func TestMX(t *testing.T) {
	tests := []struct {
		name string
		r    Resolver
		want bool
	}{
		{"a host", fakeResolver{mx: []*net.MX{{Host: "mx.northwind.example.", Pref: 10}}}, true},
		{"null MX", fakeResolver{mx: []*net.MX{{Host: ".", Pref: 0}}}, false},
		{"no records", fakeResolver{}, false},
		{"lookup error", fakeResolver{err: errors.New("nxdomain")}, false},
		{"nil resolver", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MX(context.Background(), tc.r, "northwind.example"); got != tc.want {
				t.Errorf("MX = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestInvariantNoPersonOutput runs every fixture through every entry point and checks no
// output string equals a person-like address.
func TestInvariantNoPersonOutput(t *testing.T) {
	bodies := [][]byte{contactPage(t), readFixture(t, "home_ro.html"), readFixture(t, "home_de.html")}
	for _, b := range bodies {
		r := Extract(homeURL, b, "northwind.example", testRoles)
		assertNoPerson(t, r.Emails, r.Forms)
		base, _ := url.Parse(homeURL)
		assertNoPerson(t, ContactLinks(base, b, "northwind.example"))
	}
	f := &fakeFetcher{pages: map[string]string{homeURL + "contact": string(contactPage(t))}}
	org := Org{Domain: "northwind.example", Roles: testRoles}
	got, ev := Find(context.Background(), f, robotsFrom(""), org, homeWithLinks("/contact"))
	assertNoPersonRoutes(t, got.Routes)
	for _, e := range ev {
		assertNoPerson(t, []string{e.URL})
	}
}

func assertNoPersonRoutes(t *testing.T, rs []Route) {
	t.Helper()
	for _, r := range rs {
		assertNoPerson(t, []string{r.Value, r.URL})
	}
}

func assertNoPerson(t *testing.T, lists ...[]string) {
	t.Helper()
	for _, l := range lists {
		for _, s := range l {
			for _, p := range personLike {
				if strings.EqualFold(s, p) {
					t.Errorf("output holds person-like address %q", s)
				}
			}
		}
	}
}

func TestContactLinksOrgDomain(t *testing.T) {
	// The homepage ended on another domain; only links on the org's own domain count.
	base, _ := url.Parse("https://cdn.hosting.example/")
	body := []byte(`<a href="/contact">Contact</a><a href="https://www.northwind.example/contact">Contact</a>`)
	got := ContactLinks(base, body, "northwind.example")
	want := []string{"https://www.northwind.example/contact"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ContactLinks = %q, want %q", got, want)
	}
}

func TestContactLinksDropAddressLinks(t *testing.T) {
	person := "john.smith" + "@" + "northwind.example"
	base, _ := url.Parse(homeURL)
	body := []byte(`
		<a href="/contact?to=` + person + `">Contact</a>
		<a href="/kontakt/john.smith%40northwind.example">Kontakt</a>
		<a href="/cdn-cgi/l/email-protection#abcd">Contact</a>
		<a href="/contact-us?ref=nav#form">Contact us</a>`)
	got := ContactLinks(base, body, "northwind.example")
	want := []string{"https://northwind.example/contact-us"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ContactLinks = %q, want %q", got, want)
	}

	f := &fakeFetcher{pages: map[string]string{homeURL + "contact-us": `<p>info@northwind.example</p>`}}
	org := Org{Domain: "northwind.example", Roles: testRoles}
	res, ev := Find(context.Background(), f, robotsFrom(""), org, Page{FetchID: 1, URL: homeURL, Body: body})
	all := append([]string(nil), f.gets...)
	for _, r := range res.Routes {
		all = append(all, r.Value, r.URL)
	}
	for _, e := range ev {
		all = append(all, e.URL)
	}
	for _, s := range all {
		if strings.Contains(strings.ToLower(s), "smith") {
			t.Errorf("output or fetch holds the person's address: %q", s)
		}
	}
	if !reflect.DeepEqual(f.gets, []string{homeURL + "contact-us"}) {
		t.Errorf("gets = %q, want only the contact-us page", f.gets)
	}
}

func TestFindOffDomainRedirect(t *testing.T) {
	f := &fakeFetcher{
		moves: map[string]string{homeURL + "contact": "https://elsewhere.example/contact"},
		pages: map[string]string{"https://elsewhere.example/contact": `<p>info@northwind.example</p><form><input type="email"><textarea></textarea></form>`},
	}
	org := Org{Domain: "northwind.example", Roles: testRoles}
	home := Page{FetchID: 1, URL: homeURL, Body: []byte(`<a href="/contact">Contact</a>`)}
	got, ev := Find(context.Background(), f, robotsFrom(""), org, home)
	if got.OffDomain != 1 || len(got.Routes) != 0 || len(ev) != 0 || got.Pages != 1 {
		t.Errorf("OffDomain = %d, routes = %+v, evidence = %d, pages = %d; want 1, none, 0, 1",
			got.OffDomain, got.Routes, len(ev), got.Pages)
	}
	if !got.Complete {
		t.Error("an off-domain redirect is an answer: the search is complete")
	}
}

func TestFindComplete(t *testing.T) {
	org := Org{Domain: "northwind.example", Roles: testRoles}
	contactURL := homeURL + "contact"
	skip := func(context.Context, string, string) (robots.Verdict, *robots.Rules) { return robots.VerdictSkip, nil }
	tests := []struct {
		name   string
		robots RobotsFor
		errs   map[string]error
		want   bool
	}{
		{"every page fetched", robotsFrom(""), nil, true},
		{"refused by robots", robotsFrom("User-agent: *\nDisallow: /contact\n"), nil, true},
		{"robots skip verdict", skip, nil, true},
		{"not public", robotsFrom(""), map[string]error{contactURL: fetch.ErrNotPublic}, true},
		{"transport error", robotsFrom(""), map[string]error{contactURL: errors.New("connection reset")}, false},
		{"breaker open", robotsFrom(""), map[string]error{contactURL: &fetch.OpenError{Reason: fetch.StopBudget}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeFetcher{errs: tc.errs, pages: map[string]string{contactURL: "<p>x</p>"}}
			got, _ := Find(context.Background(), f, tc.robots, org, homeWithLinks("/contact"))
			if got.Complete != tc.want {
				t.Errorf("Complete = %v, want %v", got.Complete, tc.want)
			}
		})
	}

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		f := &fakeFetcher{errs: map[string]error{contactURL: context.Canceled}}
		got, _ := Find(ctx, f, robotsFrom(""), org, homeWithLinks("/contact"))
		if got.Complete || got.Stopped == nil {
			t.Errorf("Complete = %v, Stopped = %v; want false and the cancel", got.Complete, got.Stopped)
		}
	})
}

func TestMailboxProvider(t *testing.T) {
	for d, want := range map[string]bool{
		"gmail.com": true, "GoogleMail.com": true, "mail.yahoo.com": true, "yahoo.ro": true, "hotmail.co.uk": true,
		"outlook.com": true, "live.com": true, "icloud.com": true, "me.com": true, "aol.com": true, "gmx.de": true,
		"mail.ru": true, "yandex.ru": true, "proton.me": true, "protonmail.com": true,
		"cafe-luna.example": false, "gmail.example.com": false, "mail.cafe.example": false, "": false,
	} {
		if got := MailboxProvider(d); got != want {
			t.Errorf("MailboxProvider(%q) = %v, want %v", d, got, want)
		}
	}
}
