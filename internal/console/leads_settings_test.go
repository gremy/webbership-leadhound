package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
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
		TrustGate: []string{"Relevance is an early number: " + text},
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
		"A change applies to new leads at the next build.",
		"Source: file", "Source: changed in the console on 24 October 2026",
		`name="wanted" value="GB" checked`, `name="wanted" value="DE">`, `name="never_contact" value="northwind.example"`,
		`name="on" value="agency_partner">`, `value="down:unsupported"`, `value="up:agency_partner"`,
		`name="skip_rank_over" value="100000"`,
		`name="batches_from" value="starred_then_rest" checked`, "Starred leads first, then the rest of the queue",
		`name="d10_eol" value="2026-12-09" type="date"`,
		`name="rating" value="on" checked`, `name="leads_cap" value="500"`, `max="10000"`, "of 10,000 a day",
		"Relevance is an early number: northwind.example", "A saved change is ignored: northwind.example",
		`name="kind.unsupported" value="20"`, `name="exact_version" value="5"`,
		"Europe/London", "Batches of 20; northwind.example", "one for each of de, en",
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
	for _, bad := range []string{`value="up:unsupported"`, `value="down:agency_partner"`, `name="exclude_countries"`,
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
			msg = "Every setting is back to the file. A change applies to new leads at the next build."
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
	if len(EnvAllowlist) != 9 {
		t.Errorf("EnvAllowlist holds %d names, want the 9 of plan D11", len(EnvAllowlist))
	}
}
