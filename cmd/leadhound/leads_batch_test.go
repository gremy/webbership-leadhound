package main

import (
	"bytes"
	"encoding/json"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
)

func demoLeadsConfig(t *testing.T) leadconfig.Config {
	t.Helper()
	cfg, err := leadconfig.Load("../../examples")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func i64(n int64) *int64 { return &n }

var demoFetch = leadstore.EvidenceFetch{ID: 11, URL: "https://northwind.example/", FetchedAt: time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)}

// zeroReader gives Message-IDs of zero bytes, so drafts are reproducible.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestDraftLeadEmailSequence(t *testing.T) {
	cfg := demoLeadsConfig(t)
	c := leadstore.Candidate{LeadID: 3, OrgID: 2, Domain: "northwind.example", Country: "GB", Kind: leadstore.LeadUnsupported,
		Channel: leadstore.ChannelEmail, RouteKind: leadstore.RouteEmail, Route: "info@northwind.example"}
	ev := leadstore.Evidence{LeadID: 3, ProbeID: i64(5), FinalHost: "northwind.example", Major: 7,
		Signals: []leadstore.EvidenceSignal{
			{Signal: leadstore.Signal{Signal: "x-drupal-cache", FetchID: 11}, Fetch: demoFetch},
			{Signal: leadstore.Signal{Signal: "Drupal.settings", FetchID: 11}, Fetch: demoFetch},
		}}
	ds, err := draftLead(c, ev, cfg, "sender.example", zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 3 {
		t.Fatalf("%d drafts, want 3", len(ds))
	}
	first := strings.Repeat("0", 32) + "@sender.example"
	for i, d := range ds {
		if d.Step != i || d.Channel != "email" || d.To != c.Route || d.MessageID != first {
			t.Errorf("draft %d = %+v", i, d)
		}
		if (i == 0) != (d.InReplyTo == "") || (i > 0 && d.InReplyTo != first) {
			t.Errorf("draft %d threads to %q", i, d.InReplyTo)
		}
	}
	if !strings.Contains(ds[0].Body, "the site's settings block is Drupal 7's on https://northwind.example/") ||
		!strings.Contains(ds[0].Body, cfg.Sender.OptOutLines["en"]) || ds[0].Subject != "northwind.example still runs Drupal 7" {
		t.Errorf("first email:\n%s\n%s", ds[0].Subject, ds[0].Body)
	}
	var slots map[string]slotJSON
	if err := json.Unmarshal(ds[0].Slots, &slots); err != nil {
		t.Fatal(err)
	}
	if slots["FindingURL"] != (slotJSON{Value: "https://northwind.example/", Table: "http_fetch", ID: 11}) ||
		slots["Major"] != (slotJSON{Value: "7", Table: "site_probe", ID: 5}) || slots["Domain"].Table != "org" ||
		slots["SenderName"].Table != "config" {
		t.Errorf("slots = %+v", slots)
	}
}

func TestDraftLeadFormAndAgency(t *testing.T) {
	cfg := demoLeadsConfig(t)
	form := leadstore.Candidate{LeadID: 4, OrgID: 3, Domain: "contoso.example", Country: "DE", Kind: leadstore.LeadD10,
		Channel: leadstore.ChannelForm, RouteKind: leadstore.RouteForm, Route: "https://contoso.example/contact"}
	ev := leadstore.Evidence{ProbeID: i64(6), Major: 10, ExactVersion: "10.6.14",
		Signals: []leadstore.EvidenceSignal{{Signal: leadstore.Signal{Signal: "core-asset-v", Value: "10.6.14", FetchID: 12},
			Fetch: leadstore.EvidenceFetch{ID: 12, URL: "https://contoso.example/"}}}}
	ds, err := draftLead(form, ev, cfg, "shadow.invalid", zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds[0].Channel != "form" || ds[1].Channel != "linkedin" || ds[1].Step != 1 || ds[0].To != "" || ds[0].MessageID != "" {
		t.Fatalf("form drafts = %+v", ds)
	}

	agency := leadstore.Candidate{LeadID: 5, OrgID: 4, Domain: "wingtip.example", OrgName: "Wingtip Digital", Country: "GB",
		Kind: leadstore.LeadAgencyPartner, Channel: leadstore.ChannelEmail, RouteKind: leadstore.RouteEmail, Route: "hello@wingtip.example"}
	aev := leadstore.Evidence{AgencyFindingID: i64(9), FindingURL: "https://www.wingtip.example/services/drupal-upgrades"}
	ds, err = draftLead(agency, aev, cfg, "shadow.invalid", zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ds[0].Body, "your page at wingtip.example offers Drupal upgrades (https://www.wingtip.example/services/drupal-upgrades)") {
		t.Errorf("agency email:\n%s", ds[0].Body)
	}
}

func TestDraftLeadDrops(t *testing.T) {
	cfg := demoLeadsConfig(t)
	base := leadstore.Candidate{LeadID: 3, OrgID: 2, Domain: "northwind.example", Country: "GB", Kind: leadstore.LeadUnsupported,
		Channel: leadstore.ChannelEmail, RouteKind: leadstore.RouteEmail, Route: "info@northwind.example"}
	sig := func(name, value string) leadstore.EvidenceSignal {
		return leadstore.EvidenceSignal{Signal: leadstore.Signal{Signal: name, Value: value, FetchID: 11}, Fetch: demoFetch}
	}
	cases := map[string]struct {
		c    leadstore.Candidate
		ev   leadstore.Evidence
		want string
	}{
		"no versioned signal":   {base, leadstore.Evidence{ProbeID: i64(5), Major: 7, Signals: []leadstore.EvidenceSignal{sig("x-drupal-cache", "")}}, "no stored signal"},
		"signal disagrees":      {base, leadstore.Evidence{ProbeID: i64(5), Major: 7, Signals: []leadstore.EvidenceSignal{sig("x-generator", "9")}}, "no stored signal"},
		"no finding":            {base, leadstore.Evidence{}, "no finding"},
		"country without email": {func() leadstore.Candidate { c := base; c.Country = "DE"; return c }(), leadstore.Evidence{ProbeID: i64(5), Major: 7, Signals: []leadstore.EvidenceSignal{sig("Drupal.settings", "")}}, "no email templates"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := draftLead(tc.c, tc.ev, cfg, "shadow.invalid", zeroReader{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// Every major from 6 to 10 has its end-of-life date in config, so a Drupal 9 lead drafts.
func TestDraftLeadDrupal9(t *testing.T) {
	cfg := demoLeadsConfig(t)
	c := leadstore.Candidate{LeadID: 3, OrgID: 2, Domain: "tailspin.example", Country: "GB", Kind: leadstore.LeadUnsupported,
		Channel: leadstore.ChannelEmail, RouteKind: leadstore.RouteEmail, Route: "info@tailspin.example"}
	ev := leadstore.Evidence{ProbeID: i64(5), Major: 9, Signals: []leadstore.EvidenceSignal{
		{Signal: leadstore.Signal{Signal: "x-generator", Value: "9", FetchID: 11}, Fetch: demoFetch}}}
	ds, err := draftLead(c, ev, cfg, "shadow.invalid", zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	eol, _ := cfg.Facts.EOL(9)
	var slots map[string]slotJSON
	if err := json.Unmarshal(ds[0].Slots, &slots); err != nil {
		t.Fatal(err)
	}
	if slots["EOLDate"].Table != "config" || !strings.Contains(ds[0].Body, slots["EOLDate"].Value) || eol.IsZero() {
		t.Errorf("EOLDate slot %+v, body:\n%s", slots["EOLDate"], ds[0].Body)
	}
}

// A language without an opt-out line (config refuses this for email countries; here the
// loaded config is changed by hand) leaves the slot empty, and Check stops the email.
func TestDraftLeadNoOptOutLine(t *testing.T) {
	cfg := demoLeadsConfig(t)
	cfg.Sender.OptOutLines = map[string]string{}
	c := leadstore.Candidate{LeadID: 3, OrgID: 2, Domain: "northwind.example", Country: "GB", Kind: leadstore.LeadUnsupported,
		Channel: leadstore.ChannelEmail, RouteKind: leadstore.RouteEmail, Route: "info@northwind.example"}
	ev := leadstore.Evidence{ProbeID: i64(5), Major: 7, Signals: []leadstore.EvidenceSignal{
		{Signal: leadstore.Signal{Signal: "Drupal.settings", FetchID: 11}, Fetch: demoFetch}}}
	if _, err := draftLead(c, ev, cfg, "shadow.invalid", zeroReader{}); err == nil || !strings.Contains(err.Error(), "OptOutLine") {
		t.Errorf("err = %v, want the missing OptOutLine named", err)
	}
}

func TestNextBusinessSlot(t *testing.T) {
	bh := leadconfig.BusinessHours{Start: "09:30", End: "16:30", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri"}}
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, london)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for now, want := range map[string]string{
		"2026-10-27 08:00": "2026-10-27 09:30", // Tuesday, before the window
		"2026-10-27 11:15": "2026-10-27 11:15", // inside it
		"2026-10-27 17:00": "2026-10-28 09:30", // after it
		"2026-10-30 17:00": "2026-11-02 09:30", // Friday evening → Monday
		"2026-10-31 10:00": "2026-11-02 09:30", // Saturday
	} {
		if got := nextBusinessSlot(at(now), london, bh); !got.Equal(at(want)) {
			t.Errorf("nextBusinessSlot(%s) = %s, want %s", now, got.In(london), want)
		}
	}
	// The zone is the recipient's: 08:00 UTC is 10:00 in Berlin in summer time.
	berlin, _ := time.LoadLocation("Europe/Berlin")
	now := time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)
	if got := nextBusinessSlot(now, berlin, bh); !got.Equal(now) {
		t.Errorf("Berlin: %s", got)
	}
	if !nextBusinessSlot(now, london, leadconfig.BusinessHours{Start: "x"}).IsZero() {
		t.Error("unparsable hours: want zero")
	}
}

func TestParseSkipAndCounts(t *testing.T) {
	if got, err := parseSkip(" 3, 5 "); err != nil || len(got) != 2 || got[0] != 3 || got[1] != 5 {
		t.Errorf("parseSkip = %v %v", got, err)
	}
	for _, bad := range []string{"0", "a", "3,,5", "-1"} {
		if _, err := parseSkip(bad); err == nil {
			t.Errorf("parseSkip(%q): want an error", bad)
		}
	}
	ds := []leadstore.DraftRow{
		{Channel: "email", Step: 0}, {Channel: "email", Step: 1}, {Channel: "email", Step: 2},
		{Channel: "email", Step: 0}, {Channel: "form"}, {Channel: "linkedin", Step: 1},
	}
	if got := batchCounts(ds); got != "2 emails, 1 contact form, 1 LinkedIn note" {
		t.Errorf("batchCounts = %q", got)
	}
}

func TestOutboxMessageBuildsAFollowUp(t *testing.T) {
	box := "outreach" + "@" + "sender.example"
	m := leadstore.Message{Step: 1, Channel: "email", ToAddress: "info@northwind.example", Subject: "Re: x",
		BodyText: "Hello again\n", MessageID: "b@sender.example", InReplyTo: "a@sender.example"}
	raw, err := mailbox.Build(outboxMessage(m, "Demo Sender", box, time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("References") != "<a@sender.example>" || msg.Header.Get("List-Unsubscribe") != "<mailto:"+box+"?subject=unsubscribe>" {
		t.Errorf("headers = %v", msg.Header)
	}
}

func TestOrgNameValue(t *testing.T) {
	long := strings.Repeat("ab ", 40) + "tail"
	for in, want := range map[string]string{
		"Wingtip Digital":              "Wingtip Digital",
		"  Wingtip   Digital ":         "Wingtip Digital",
		"":                             "",
		"wingtip.example":              "",
		"https://www.wingtip.example/": "",
		"www.wingtip.example":          "",
		"Wingtip Digital Ltd.":         "Wingtip Digital Ltd.",
		long:                           strings.TrimSpace(string([]rune(strings.Join(strings.Fields(long), " "))[:80])),
	} {
		if got := orgNameValue(in); got != want {
			t.Errorf("orgNameValue(%q) = %q, want %q", in, got, want)
		}
	}
	if n := len([]rune(orgNameValue(long))); n > 80 {
		t.Errorf("a long name keeps %d runes", n)
	}
}
