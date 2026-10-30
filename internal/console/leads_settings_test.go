package console

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// settingsSave is one SaveLeadSettings call as the fake backend saw it.
type settingsSave struct {
	Section string
	Values  LeadSettingsValues
	Reset   bool
}

func (f *fakeBackend) LeadSettings(context.Context) (LeadSettingsPage, error) {
	f.count("LeadSettings")
	return f.settings, f.leadsRead()
}

func (f *fakeBackend) SaveLeadSettings(_ context.Context, section string, v LeadSettingsValues, reset bool) error {
	f.count("SaveLeadSettings")
	f.mu.Lock()
	f.saves = append(f.saves, settingsSave{Section: section, Values: v, Reset: reset})
	f.mu.Unlock()
	return f.saveErr
}

func (f *fakeBackend) lastSave() settingsSave {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.saves) == 0 {
		return settingsSave{}
	}
	return f.saves[len(f.saves)-1]
}

// richSettings is a Settings page with something in every section, every text set to text.
// The fallback order and the kinds' order, the batches and the relevance switch come from
// the console (changed on testSent); the rest from the file.
func richSettings(text string) LeadSettingsPage {
	console := SettingSource{Console: true, At: testSent}
	return LeadSettingsPage{
		Values: LeadSettingsValues{
			Countries:    []CountryToggle{{Code: "GB", Label: text, Wanted: true}, {Code: "DE", Label: "Germany", Wanted: false}},
			NeverContact: text,
			Kinds: []KindToggle{{Name: "unsupported", Label: text, On: true}, {Name: "d10", Label: "Drupal 10", On: true},
				{Name: "agency_partner", Label: "Agency partner", On: false}},
			SkipRankOver: "100000",
			BatchesFrom:  BatchesStarredThenRest,
			Dates: []DateSetting{{Name: "d6_eol", Label: "Drupal 6", Value: "2016-02-24"}, {Name: "d7_eol", Label: "Drupal 7", Value: "2025-01-05"},
				{Name: "d10_eol", Label: "Drupal 10", Value: "2026-12-09", Source: console}},
			Rating:   true,
			LeadsCap: "500",
			Order: PriorityForm{Points: []PointField{{Key: "kind.unsupported", Group: "Kind", Label: text, Value: "20"},
				{Key: "major.7", Group: "Drupal", Label: "7", Value: "40"}}, ExactVersion: "5", Countries: "GB 20", RankBands: "10000 15"},
		},
		Sources:   SettingsSources{KindsOrder: console, BatchesFrom: console, Rating: console, Order: console},
		ServerCap: 10000,
		TrustGate: TrustGate{Status: []string{"Relevance is an early number: " + text},
			Rule: "Sorting by relevance starts once Jev agrees with your calls on at least 30 leads.", Exact: "In exact terms: " + text},
		Labelled:  12,
		LabelGoal: 30,
		Notes:     []string{"A saved change is ignored: " + text},
		ReadOnly: ReadOnlySettings{
			Countries:       []CountryPolicy{{Code: "GB", Label: text, Channel: "email", Language: "en", Timezone: "Europe/London", Footer: "uk"}},
			Pace:            []string{"Batches of 20; " + text},
			SenderName:      text,
			IdentityLine:    text,
			SiteURL:         "https://webbership.example",
			OptOutLanguages: []string{"de", "en"},
			Templates:       []string{"Drupal 7 site in English: emails 1 to 3, contact form", "Footer " + text},
			Env:             []EnvSetting{{Name: "LEADHOUND_OUTREACH", Value: "on"}, {Name: "LEADHOUND_TZ", Value: text}},
		},
	}
}

// settingsForms are a valid form for each editable section, and the values the backend
// should be handed for it.
func settingsForms() []struct {
	section string
	form    url.Values
	want    LeadSettingsValues
} {
	return []struct {
		section string
		form    url.Values
		want    LeadSettingsValues
	}{
		{SettingsWho, url.Values{"do": {"save"}, "country": {"GB", "DE", "GB", " "}, "wanted": {"DE"}, "never_contact": {" FR, IT "}},
			LeadSettingsValues{Countries: []CountryToggle{{Code: "GB"}, {Code: "DE", Wanted: true}}, NeverContact: "FR, IT"}},
		{SettingsKinds, url.Values{"do": {"save"}, "kind": {"d10", "unsupported"}, "on": {"unsupported"}},
			LeadSettingsValues{Kinds: []KindToggle{{Name: "d10"}, {Name: "unsupported", On: true}}}},
		{SettingsKinds, url.Values{"do": {"up:d10"}, "kind": {"unsupported", "d10", "agency_partner"}, "on": {"d10"}},
			LeadSettingsValues{Kinds: []KindToggle{{Name: "d10", On: true}, {Name: "unsupported"}, {Name: "agency_partner"}}}},
		{SettingsKinds, url.Values{"do": {"down:unsupported"}, "kind": {"unsupported", "d10"}},
			LeadSettingsValues{Kinds: []KindToggle{{Name: "d10"}, {Name: "unsupported"}}}},
		{SettingsSmallSites, url.Values{"do": {"save"}, "skip_rank_over": {" 250000 "}}, LeadSettingsValues{SkipRankOver: "250000"}},
		{SettingsBatches, url.Values{"do": {"save"}, "batches_from": {"starred"}}, LeadSettingsValues{BatchesFrom: BatchesStarred}},
		{SettingsDates, url.Values{"do": {"save"}, "d7_eol": {"2025-01-05"}, "d10_eol": {"2026-12-09"}, "d11_eol": {"2030-01-01"}},
			LeadSettingsValues{Dates: []DateSetting{{Name: "d6_eol"}, {Name: "d7_eol", Value: "2025-01-05"}, {Name: "d8_eol"}, {Name: "d9_eol"},
				{Name: "d10_eol", Value: "2026-12-09"}}}},
		{SettingsRelevance, url.Values{"do": {"save"}, "rating": {"on"}, "leads_cap": {"800"}}, LeadSettingsValues{Rating: true, LeadsCap: "800"}},
		{SettingsRelevance, url.Values{"do": {"save"}, "leads_cap": {""}}, LeadSettingsValues{}},
		{SettingsOrder, url.Values{"do": {"save"}, "kind.d10": {" 30 "}, "major.7": {""}, "exact_version": {"5"}, "rank_bands": {"10000 15"},
			"countries": {"GB 20"}, "exclude_countries": {"FR"}},
			LeadSettingsValues{Order: PriorityForm{Points: []PointField{{Key: "kind.d10", Value: "30"}, {Key: "major.7"}}, ExactVersion: "5",
				RankBands: "10000 15", Countries: "GB 20"}}},
	}
}

// TestSettingsPage: every section, its form and its reset, where each value comes from, the
// Who note, the read-only part with where to edit it, the sub-nav, and the old priorities
// path sent to the fallback order.
func TestSettingsPage(t *testing.T) {
	h := newHarness(t, richBackend("northwind.example"), false)
	c := h.session(t)
	w := h.do(get("/leads/settings", c))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	wants := []string{
		`<a href="/leads" aria-current="page">Leads</a>`,
		`<a href="/leads/settings" aria-current="page">Settings</a>`,
		"A change applies to the queue straight away and to the next new batch; batches already made keep their leads.",
		"<p class=\"setting-source\">Changed here on 24 October 2026.</p>", "Order changed here on 24 October 2026.",
		`name="wanted" value="GB" checked`, `name="wanted" value="DE">`, `name="never_contact" value="northwind.example"`,
		`name="on" value="agency_partner">`, `value="down:unsupported"`, `value="up:agency_partner"`,
		`name="skip_rank_over" value="100000"`,
		`name="batches_from" value="starred_then_rest" checked`, "Starred leads first, then the rest of the queue",
		`name="d10_eol" value="2026-12-09" type="date"`,
		`name="rating" value="on" checked`, `name="leads_cap" value="500"`, `max="10000"`, "of 10,000 a day",
		"Relevance is an early number: northwind.example", `<p class="note">A saved change is ignored: northwind.example</p>`,
		`name="kind.unsupported" value="20"`, `name="exact_version" value="5"`,
		"Europe/London", "Batches of 20; northwind.example", "<dt>Opt-out lines</dt><dd>de, en</dd>",
		"Edit in leads.yaml.", "Set in .env on the server.", "LEADHOUND_OUTREACH",
		"<h3>Templates</h3>", "Drupal 7 site in English: emails 1 to 3, contact form", "Footer northwind.example",
		`action="/leads/settings/all"`, "Reset everything to the file",
	}
	for _, s := range settingsSections {
		wants = append(wants, `id="`+s.Name+`"`, `action="/leads/settings/`+s.Name+`"`, `href="#`+s.Name+`"`)
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("settings page lacks %q", want)
		}
	}
	if n := strings.Count(body, `value="reset" class="quiet">Reset this section`); n != len(settingsSections) {
		t.Errorf("%d section resets, want %d", n, len(settingsSections))
	}
	// A value from the file says nothing about where it comes from; only a change made here
	// says when. The first kind has no Up and the last no Down: each keeps an empty slot so
	// the buttons line up.
	for _, want := range []string{`<span class="move-up move-none" aria-hidden="true"></span>`,
		`<span class="move-down move-none" aria-hidden="true"></span>`} {
		if n := strings.Count(body, want); n != 1 {
			t.Errorf("settings page holds %q %d times, want once", want, n)
		}
	}
	for _, bad := range []string{`value="up:unsupported"`, `value="down:agency_partner"`, `name="exclude_countries"`,
		"Source:", "Ticks changed here", `<p class="notice">A saved change`,
		"<style", " style=", "onclick", "<script>"} {
		if strings.Contains(body, bad) {
			t.Errorf("settings page holds %q", bad)
		}
	}
	if h.b.n("SaveLeadSettings") != 0 {
		t.Error("reading the page saved")
	}

	w = h.do(get("/leads/priorities", c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/settings#order" {
		t.Errorf("/leads/priorities: %d to %q, want 303 to /leads/settings#order", w.Code, w.Header().Get("Location"))
	}
	if w := h.do(post("/leads/priorities", url.Values{"do": {"save"}}, c)); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /leads/priorities: %d, want 405", w.Code)
	}
	for _, p := range []string{"/leads", "/leads/outreach", "/leads/5"} {
		if !strings.Contains(h.do(get(p, c)).Body.String(), `<a href="/leads/settings">Settings</a>`) {
			t.Errorf("%s: the Settings tab does not link /leads/settings", p)
		}
	}
}

// TestSettingsSaveAndReset: each section's save hands the backend that section's values,
// read and trimmed, and goes back to the section with the outcome; Up and Down move a kind
// and save; a section's reset and the reset of everything ask for a reset; a form the
// console cannot read never reaches the backend.
func TestSettingsSaveAndReset(t *testing.T) {
	for _, tc := range settingsForms() {
		t.Run(tc.section+" "+tc.form.Get("do"), func(t *testing.T) {
			b := richBackend("x")
			h := newHarness(t, b, false)
			c := h.session(t)
			w := h.do(post("/leads/settings/"+tc.section, tc.form, c))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/settings#"+tc.section {
				t.Fatalf("got %d to %q: %s", w.Code, w.Header().Get("Location"), w.Body.String())
			}
			got := b.lastSave()
			if got.Section != tc.section || got.Reset || !reflect.DeepEqual(got.Values, tc.want) {
				t.Errorf("saved %+v, want section %s values %+v", got, tc.section, tc.want)
			}
			flash := cookieNamed(w, "__Host-lh_flash")
			if flash == nil {
				t.Fatal("no outcome cookie")
			}
			page := h.do(get("/leads/settings", c, flash)).Body.String()
			if want := settingsTitle(tc.section) + " saved."; !strings.Contains(page, want) {
				t.Errorf("the page does not say %q", want)
			}
		})
	}

	for _, s := range append(slices.Clone(settingsSections), settingsSection{SettingsAll, ""}) {
		b := richBackend("x")
		h := newHarness(t, b, false)
		c := h.session(t)
		w := h.do(post("/leads/settings/"+s.Name, url.Values{"do": {"reset"}, "skip_rank_over": {"7"}, "kind": {"d10"}}, c))
		next := "/leads/settings#" + s.Name
		if s.Name == SettingsAll {
			next = "/leads/settings"
		}
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != next {
			t.Errorf("reset %s: %d to %q, want %s", s.Name, w.Code, w.Header().Get("Location"), next)
		}
		if got := b.lastSave(); got.Section != s.Name || !got.Reset || !reflect.DeepEqual(got.Values, LeadSettingsValues{}) {
			t.Errorf("reset %s asked %+v", s.Name, got)
		}
		msg := "is back to the file."
		if s.Name == SettingsAll {
			msg = "Every setting is back to the file. A change applies to the queue straight away and to the next new batch; batches already made keep their leads."
		}
		if page := h.do(get("/leads/settings", c, cookieNamed(w, "__Host-lh_flash"))).Body.String(); !strings.Contains(page, msg) {
			t.Errorf("reset %s: the page does not say %q", s.Name, msg)
		}
	}

	refused := []struct {
		path string
		form url.Values
		code int
	}{
		{"/leads/settings/all", url.Values{"do": {"save"}}, http.StatusUnprocessableEntity},
		{"/leads/settings/who", url.Values{"do": {"drop"}}, http.StatusUnprocessableEntity},
		{"/leads/settings/who", url.Values{}, http.StatusUnprocessableEntity},
		{"/leads/settings/who", url.Values{"do": {"up:GB"}, "country": {"GB"}}, http.StatusUnprocessableEntity},
		{"/leads/settings/kinds", url.Values{"do": {"up:unsupported"}, "kind": {"unsupported", "d10"}}, http.StatusUnprocessableEntity},
		{"/leads/settings/kinds", url.Values{"do": {"down:d10"}, "kind": {"unsupported", "d10"}}, http.StatusUnprocessableEntity},
		{"/leads/settings/kinds", url.Values{"do": {"up:selftest"}, "kind": {"unsupported", "d10"}}, http.StatusUnprocessableEntity},
		{"/leads/settings/kinds", url.Values{"do": {"sideways:d10"}, "kind": {"unsupported", "d10"}}, http.StatusUnprocessableEntity},
		{"/leads/settings/sender", url.Values{"do": {"save"}}, http.StatusNotFound},
		{"/leads/settings/operator_addresses", url.Values{"do": {"reset"}}, http.StatusNotFound},
		{"/leads/settings/", url.Values{"do": {"reset"}}, http.StatusNotFound},
	}
	for _, rc := range refused {
		b := richBackend("x")
		h := newHarness(t, b, false)
		w := h.do(post(rc.path, rc.form, h.session(t)))
		if w.Code != rc.code {
			t.Errorf("%s %v: %d, want %d", rc.path, rc.form, w.Code, rc.code)
		}
		if n := b.n("SaveLeadSettings"); n != 0 {
			t.Errorf("%s %v: SaveLeadSettings called %d times", rc.path, rc.form, n)
		}
	}

	// Not set up, and a failure: the one says so, the other is a 500 without its words.
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	b.saveErr = ErrLeadsNotSetUp
	if w := h.do(post("/leads/settings/batches", url.Values{"do": {"save"}, "batches_from": {"starred"}}, c)); w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), leadsNotSetUp) {
		t.Errorf("not set up: %d", w.Code)
	}
	b.saveErr = errors.New("database gone at the primary")
	if w := h.do(post("/leads/settings/batches", url.Values{"do": {"save"}, "batches_from": {"starred"}}, c)); w.Code != http.StatusInternalServerError ||
		strings.Contains(w.Body.String(), "database gone") {
		t.Errorf("failure: %d %s", w.Code, w.Body.String())
	}
	// A cross-site save never reaches the backend.
	r := post("/leads/settings/who", url.Values{"do": {"reset"}}, c)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := h.do(r); w.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d, want 403", w.Code)
	}
	if n := b.n("SaveLeadSettings"); n != 2 {
		t.Errorf("SaveLeadSettings called %d times, want 2", n)
	}
}

// TestSettingsRefusalShowsProblems: a refused save shows the page again (422) with every
// problem under the section and a line at the top that links it, the section as it was
// submitted, and saves nothing; a plain ErrUserFixable shows as one problem.
func TestSettingsRefusalShowsProblems(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	b.saveErr = &SettingsRefused{Problems: []string{"skip_rank_over must be 0 (off) or more, not -5", "<b>second</b> problem"}}
	w := h.do(post("/leads/settings/small-sites", url.Values{"do": {"save"}, "skip_rank_over": {"-5"}}, c))
	body := w.Body.String()
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", w.Code)
	}
	for _, want := range []string{
		`Nothing was saved. <a href="#small-sites">Small sites</a> needs fixes first.`,
		"Fix these before saving:", "<li>skip_rank_over must be 0 (off) or more, not -5</li>", "&lt;b&gt;second&lt;/b&gt; problem",
		`name="skip_rank_over" value="-5"`,
		`name="leads_cap" value="500"`, // another section keeps the value in force
	} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal page lacks %q", want)
		}
	}
	if strings.Contains(body, "<b>second") {
		t.Error("a problem was not escaped")
	}
	if i, j := strings.Index(body, `id="small-sites"`), strings.Index(body, "Fix these before saving:"); i < 0 || j < i || j > strings.Index(body, `id="batches"`) {
		t.Error("the problems are not under the refused section")
	}
	if f := cookieNamed(w, "__Host-lh_flash"); f != nil && f.Value != "" {
		t.Error("a refusal set an outcome")
	}

	// The submitted values come back over the ones in force, in every section.
	cases := []struct {
		section string
		form    url.Values
		wants   []string
	}{
		{SettingsWho, url.Values{"do": {"save"}, "country": {"GB", "DE"}, "wanted": {"DE"}, "never_contact": {"ZZ, 12"}},
			[]string{`name="wanted" value="GB">`, `name="wanted" value="DE" checked`, `name="never_contact" value="ZZ, 12"`}},
		{SettingsKinds, url.Values{"do": {"up:d10"}, "kind": {"unsupported", "d10", "agency_partner"}, "on": {"agency_partner"}},
			[]string{`name="on" value="d10">`, `name="on" value="agency_partner" checked`, `value="down:d10"`, `value="up:unsupported"`}},
		{SettingsBatches, url.Values{"do": {"save"}, "batches_from": {"starred"}}, []string{`name="batches_from" value="starred" checked`}},
		{SettingsDates, url.Values{"do": {"save"}, "d10_eol": {"2026-13-40"}}, []string{`name="d10_eol" value="2026-13-40"`, `name="d6_eol" value=""`}},
		{SettingsRelevance, url.Values{"do": {"save"}, "leads_cap": {"20000"}}, []string{`name="rating" value="on">`, `name="leads_cap" value="20000"`}},
		{SettingsOrder, url.Values{"do": {"save"}, "kind.unsupported": {"200"}, "rank_bands": {"x"}}, []string{`name="kind.unsupported" value="200"`, `name="rank_bands" value="x"`}},
	}
	for _, tc := range cases {
		b.saveErr = fmt.Errorf("%w: The server allows at most 10000 a day; raise it in .env first.", ErrUserFixable)
		w := h.do(post("/leads/settings/"+tc.section, tc.form, c))
		body := w.Body.String()
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "<li>The server allows at most 10000 a day; raise it in .env first.</li>") ||
			!strings.Contains(body, "Fix this before saving:") || !strings.Contains(body, `href="#`+tc.section+`"`) {
			t.Errorf("%s: %d, or the problem is missing", tc.section, w.Code)
		}
		for _, want := range tc.wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s: refusal page lacks %q", tc.section, want)
			}
		}
	}

	// A refused reset shows the values in force.
	b.saveErr = &SettingsRefused{Problems: []string{"no"}}
	body = h.do(post("/leads/settings/small-sites", url.Values{"do": {"reset"}, "skip_rank_over": {"-5"}}, c)).Body.String()
	if !strings.Contains(body, `name="skip_rank_over" value="100000"`) {
		t.Error("a refused reset does not show the values in force")
	}
	body = h.do(post("/leads/settings/all", url.Values{"do": {"reset"}}, c)).Body.String()
	if !strings.Contains(body, `Nothing was saved. <a href="#all">Reset everything</a> needs a fix first.`) {
		t.Errorf("a refused reset of everything is not named: %s", body)
	}
}

// TestSettingsNeverShowsSecrets: a backend that hands the page secret-looking values (an
// operator address, a DSN, the password hash, a key, a mailbox password) as environment
// names off the allowlist never gets them onto the page, read or refused, in any section;
// every allowlisted name is shown; and no field of the page's types is named for a secret.
func TestSettingsNeverShowsSecrets(t *testing.T) {
	secrets := map[string]string{ //nolint:gosec // G101: fake sentinels the page must never show, not credentials
		"LEADHOUND_OPERATOR_ADDRESSES":    "op-sentinel@operator.example",
		"LEADHOUND_DSN":                   "postgres://leadhound@db-sentinel.example/leadhound",
		"LEADHOUND_CONSOLE_PASSWORD_HASH": "$2a$12$hashsentinelhashsentinelhashsentinel",
		"LEADHOUND_MAILBOX_KEY_FILE":      "/etc/leadhound/keyfile-sentinel",
		"LEADHOUND_JEV_API_KEY":           "sk-sentinel-0123456789",
		"LEADHOUND_MAILBOX_PASSWORD":      "mbpass-sentinel",
	}
	b := richBackend("x")
	b.settings.ReadOnly.Env = nil
	for _, name := range EnvAllowlist {
		b.settings.ReadOnly.Env = append(b.settings.ReadOnly.Env, EnvSetting{Name: name, Value: "allowed-" + strings.ToLower(name)})
	}
	for name, v := range secrets {
		b.settings.ReadOnly.Env = append(b.settings.ReadOnly.Env, EnvSetting{Name: name, Value: v})
	}
	h := newHarness(t, b, false)
	c := h.session(t)

	pages := map[string]string{"GET /leads/settings": h.do(get("/leads/settings", c)).Body.String()}
	for _, s := range append(slices.Clone(settingsSections), settingsSection{SettingsAll, ""}) {
		b.saveErr = &SettingsRefused{Problems: []string{"refused"}}
		for _, do := range []string{"save", "reset"} {
			pages["refused "+do+" "+s.Name] = h.do(post("/leads/settings/"+s.Name, url.Values{"do": {do}}, c)).Body.String()
		}
	}
	for name, body := range pages {
		if !strings.Contains(body, `id="file"`) {
			t.Errorf("%s: no read-only section", name)
		}
		for env, v := range secrets {
			if strings.Contains(body, v) || strings.Contains(body, env) {
				t.Errorf("%s shows %s", name, env)
			}
		}
		for _, sentinel := range []string{"sentinel", "postgres://", "$2a$"} {
			if strings.Contains(body, sentinel) {
				t.Errorf("%s holds %q", name, sentinel)
			}
		}
		for _, env := range EnvAllowlist {
			if !strings.Contains(body, "<code>"+env+"</code>") || !strings.Contains(body, "allowed-"+strings.ToLower(env)) {
				t.Errorf("%s does not show %s", name, env)
			}
		}
	}
	if logs := h.logs.String(); strings.Contains(logs, "sentinel") {
		t.Errorf("a secret reached the log: %s", logs)
	}

	// The page's types carry no field a secret would go in.
	for _, typ := range []reflect.Type{reflect.TypeFor[LeadSettingsPage](), reflect.TypeFor[LeadSettingsValues](), reflect.TypeFor[ReadOnlySettings]()} {
		for f := range typ.Fields() {
			name := strings.ToLower(f.Name)
			for _, bad := range []string{"operator", "dsn", "password", "secret", "token", "apikey", "hash", "keyfile", "address"} {
				if strings.Contains(name, bad) {
					t.Errorf("%s.%s looks like a secret's field", typ.Name(), f.Name)
				}
			}
		}
	}
	if len(EnvAllowlist) != 8 {
		t.Errorf("EnvAllowlist holds %d names, want plan D11's 9 less the retired LEADHOUND_LEADS_SEND", len(EnvAllowlist))
	}
}

// helpSections are the Settings page's sections that each open with a plain intro: every
// editable section, the read-only part and the reset of everything.
func helpSections() []string {
	out := make([]string, 0, len(settingsSections)+2)
	for _, s := range settingsSections {
		out = append(out, s.Name)
	}
	return append(out, "file", "reset-all")
}

// parseSettings parses a Settings page and indexes its elements by id.
func parseSettings(t *testing.T, body string) (*html.Node, map[string]*html.Node) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ids := map[string]*html.Node{}
	for n := range doc.Descendants() {
		if id := attr(n, "id"); n.Type == html.ElementNode && id != "" {
			if _, dup := ids[id]; dup {
				t.Errorf("id %q appears twice", id)
			}
			ids[id] = n
		}
	}
	return doc, ids
}

// attr is n's attribute named key, or "".
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// hasClass reports whether n's class attribute holds class.
func hasClass(n *html.Node, class string) bool {
	return slices.Contains(strings.Fields(attr(n, "class")), class)
}

// nodeText is n's text, its spaces folded.
func nodeText(n *html.Node) string {
	var b strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode {
			b.WriteString(d.Data)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// TestSettingsHelp: every section opens with its intro, tied to the section by
// aria-describedby; every field a person can change has a hint tied to it by
// aria-describedby; every editable section and the read-only part carry a "More about
// this"; every allowlisted environment name has its line; and the page holds no inline
// style or script, so it keeps to the CSP. The same holds on a refused save.
func TestSettingsHelp(t *testing.T) {
	b := richBackend("northwind.example")
	b.settings.ReadOnly.Env = nil
	for _, name := range EnvAllowlist {
		b.settings.ReadOnly.Env = append(b.settings.ReadOnly.Env, EnvSetting{Name: name})
	}
	h := newHarness(t, b, false)
	c := h.session(t)
	b.saveErr = &SettingsRefused{Problems: []string{"refused"}}
	pages := map[string]string{
		"GET":     h.do(get("/leads/settings", c)).Body.String(),
		"refused": h.do(post("/leads/settings/order", url.Values{"do": {"save"}, "rank_bands": {"x"}}, c)).Body.String(),
	}
	for name, body := range pages {
		doc, ids := parseSettings(t, body)

		for _, sec := range helpSections() {
			s := ids[sec]
			if s == nil {
				t.Errorf("%s: no section %q", name, sec)
				continue
			}
			intro := ids[sec+"-intro"]
			if intro == nil || !hasClass(intro, "section-intro") || len(nodeText(intro)) < 40 {
				t.Errorf("%s: section %q has no intro", name, sec)
			}
			if got := attr(s, "aria-describedby"); got != sec+"-intro" {
				t.Errorf("%s: section %q is described by %q, want its intro", name, sec, got)
			}
			if intro != nil && !slices.ContainsFunc(slices.Collect(s.Descendants()), func(n *html.Node) bool { return n == intro }) {
				t.Errorf("%s: the intro of %q sits outside it", name, sec)
			}
		}

		fields := 0
		for n := range doc.Descendants() {
			if n.Type != html.ElementNode || !slices.Contains([]string{"input", "select", "textarea"}, n.Data) || attr(n, "type") == "hidden" {
				continue
			}
			fields++
			who := cmp.Or(attr(n, "id"), attr(n, "name")+"="+attr(n, "value"))
			refs := strings.Fields(attr(n, "aria-describedby"))
			if len(refs) == 0 {
				t.Errorf("%s: field %s has no hint", name, who)
			}
			for _, ref := range refs {
				hint := ids[ref]
				if hint == nil || !hasClass(hint, "hint") || nodeText(hint) == "" {
					t.Errorf("%s: field %s points at %q, which is not a hint on the page", name, who, ref)
				}
			}
		}
		// 2 countries, never-contact, 3 kinds, small sites, 2 batch choices, 3 dates,
		// the rating switch and its cap, 2 catch-all choices, 2 points, exact version, rank
		// bands, countries.
		if fields != 21 {
			t.Errorf("%s: %d fields checked, want 21", name, fields)
		}

		more := 0
		for n := range doc.Descendants() {
			if n.Type == html.ElementNode && n.Data == "details" && hasClass(n, "more") {
				more++
				if !strings.HasPrefix(nodeText(n), "More about this ") {
					t.Errorf("%s: a details.more does not open with More about this", name)
				}
			}
		}
		// One for each editable section, and one for the trust gate's exact terms.
		if more != len(settingsSections)+1 {
			t.Errorf("%s: %d More about this, want one for each of the %d editable sections and the trust gate's", name, more, len(settingsSections))
		}

		for n := range doc.Descendants() {
			if n.Type != html.ElementNode {
				continue
			}
			if n.Data == "style" || (n.Data == "script" && attr(n, "src") == "") {
				t.Errorf("%s: an inline <%s>", name, n.Data)
			}
			for _, a := range n.Attr {
				if a.Key == "style" || strings.HasPrefix(a.Key, "on") || strings.HasPrefix(strings.TrimSpace(strings.ToLower(a.Val)), "javascript:") {
					t.Errorf("%s: <%s %s=%q> is inline style or script", name, n.Data, a.Key, a.Val)
				}
			}
		}

		for _, want := range []string{
			"0 uses the server's limit of 10,000 a day; you can set a lower number here, not a higher one.", "making them editable here is planned",
			"Starred leads first, then the rest drafts the 5", "for example 100000",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the help lacks %q", name, want)
			}
		}
	}

	// The leads still to label are one link away, with the count, said once; the trust gate's
	// lines are hints under its heading, its exact terms in a fold; "answer key" is said at
	// most once, and the .env and the old jargon are not in the help at all.
	body := pages["GET"]
	gate := body[strings.Index(body, "<h3>Sorting by relevance</h3>"):]
	gate = gate[:strings.Index(gate, "</div>")]
	for _, want := range []string{
		`<p class="hint">Relevance is an early number: northwind.example</p>`,
		`<p class="hint">Sorting by relevance starts once Jev agrees with your calls on at least 30 leads.</p>`,
		"<summary>More about this</summary>\n        <p>In exact terms: northwind.example</p>",
		`<p class="hint">12 of 30 labelled · <a href="/leads?to_label=1">Leads to label</a></p>`,
	} {
		if !strings.Contains(gate, want) {
			t.Errorf("the trust gate lacks %q:\n%s", want, gate)
		}
	}
	if n := strings.Count(body, "12 of 30 labelled"); n != 1 {
		t.Errorf("the labelled count is said %d times, want once", n)
	}
	if n := strings.Count(body, "answer key"); n > 1 {
		t.Errorf("the page says answer key %d times, want at most once", n)
	}
	for _, bad := range []string{"in .env on the server.</p>\n    </div>", "Spearman) of 0.5", "under To label"} {
		if strings.Contains(body, bad) {
			t.Errorf("the help still holds %q", bad)
		}
	}
	b.settings.TrustGate = TrustGate{}
	noGate := h.do(get("/leads/settings", c)).Body.String()
	if want := `12 of 30 labelled · <a href="/leads?to_label=1">Leads to label</a>`; !strings.Contains(noGate, want) {
		t.Errorf("without the trust gate's lines the page lacks %q", want)
	}
	b.settings.LabelGoal = 0
	if strings.Contains(h.do(get("/leads/settings", c)).Body.String(), "to_label") {
		t.Error("with no label goal the page still links the leads to label")
	}

	for _, env := range EnvAllowlist {
		if envHelp[env] == "" {
			t.Errorf("%s has no help line", env)
		}
	}
	if len(envHelp) != len(EnvAllowlist) {
		t.Errorf("envHelp holds %d lines for %d allowlisted names", len(envHelp), len(EnvAllowlist))
	}
	for env, line := range envHelp {
		if !strings.Contains(body, html.EscapeString(line)) {
			t.Errorf("the page does not show the line for %s", env)
		}
	}
}
