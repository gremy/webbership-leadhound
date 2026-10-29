package compliance

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"
)

func at(t *testing.T, zone string, m time.Month, d, hh, mm int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(2026, m, d, hh, mm, 0, 0, loc)
}

// at is a 2026 time in zone. 28 Oct 2026 is a Wednesday; 31 Oct a Saturday; 1 Nov a Sunday.
var company = OrgFacts{EntityType: EntityCompany}

func allOn(codes ...string) CampaignOverrides {
	m := map[string]CampaignCountry{}
	for _, c := range codes {
		m[c] = CampaignCountry{Email: true, Call: true}
	}
	return CampaignOverrides{Countries: m}
}

type vcase struct {
	name     string
	country  string
	ch       Channel
	org      OrgFacts
	camp     CampaignOverrides
	now      time.Time
	ok, hard bool
	rule     string
	inReason string
}

func runCases(t *testing.T, tab *Table, cases []vcase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, hard, rule, reason := tab.Verdict(c.country, c.ch, c.org, c.camp, c.now)
			if ok != c.ok || hard != c.hard || rule != c.rule {
				t.Fatalf("Verdict = (%v, %v, %q, %q), want (%v, %v, %q)", ok, hard, rule, reason, c.ok, c.hard, c.rule)
			}
			if reason == "" {
				t.Fatal("empty reason")
			}
			if c.inReason != "" && !strings.Contains(reason, c.inReason) {
				t.Fatalf("reason %q lacks %q", reason, c.inReason)
			}
		})
	}
}

func TestDefaultTableLoads(t *testing.T) {
	tab := Default()
	if got := tab.ResearchDate.Format(time.DateOnly); got != "2026-10-27" {
		t.Fatalf("research date %s", got)
	}
	want := map[string][]string{
		"both_open":       {"AU", "BE", "CA", "CY", "EE", "FI", "FR", "GB", "IE", "NZ", "SE", "SK", "US"},
		"operator_opened": {"AT", "BG", "CH", "CZ", "DE", "DK", "ES", "GR", "HR", "HU", "IT", "LT", "LU", "LV", "MT", "NL", "PL", "PT", "RO", "SI"},
	}
	got := map[string][]string{}
	for _, code := range tab.Codes() {
		c, _ := tab.Lookup(code)
		got[c.Group] = append(got[c.Group], code)
		if c.Confidence == ConfidenceOperatorDecision && c.Acknowledge == "" {
			t.Errorf("%s: operator decision without acknowledge", code)
		}
		if c.Email.Verdict == VerdictHold || c.Call.Verdict == VerdictHold {
			t.Errorf("%s: the starting table has no hold country left", code)
		}
		// The operator, 29 Oct 2026: every country is email first; none starts with a call, none is off.
		if tab.CallFirst(code) || c.Email.Verdict == VerdictConsent || c.Email.Verdict == VerdictOff || c.Call.Verdict == VerdictOff {
			t.Errorf("%s: email %s, call %s; every country is email first with both channels on", code, c.Email.Verdict, c.Call.Verdict)
		}
	}
	for g, codes := range want {
		if !slices.Equal(got[g], codes) {
			t.Errorf("group %s = %v, want %v", g, got[g], codes)
		}
	}
	for _, code := range []string{"PT", "SI", "LT", "HU", "LU", "MT", "HR", "GR", "CH", "NL", "LV", "RO", "IT", "ES", "DE", "DK", "AT", "PL", "CZ", "BG"} {
		if c, _ := tab.Lookup(code); c.Confidence != ConfidenceOperatorDecision {
			t.Errorf("%s confidence %s, want operator_decision", code, c.Confidence)
		}
	}
	for _, code := range []string{"SK", "CY"} {
		if c, _ := tab.Lookup(code); !c.LowConfidence {
			t.Errorf("%s should be flagged low confidence", code)
		}
	}
	if !bytes.Contains(countriesYAML[:200], []byte("NOT LEGAL ADVICE")) {
		t.Error("countries.yaml must say at its top it is not legal advice")
	}
}

func TestParseRejectsBadVocabulary(t *testing.T) {
	cases := map[string][2]string{
		"bad verdict":     {"verdict: open\n      rules: [\"corporate bodies may be emailed", "verdict: maybe\n      rules: [\"corporate bodies may be emailed"},
		"bad confidence":  {"confidence: verified\n    sources:\n      - {title: \"ICO", "confidence: certain\n    sources:\n      - {title: \"ICO"},
		"bad individuals": {"individuals: {email: consent, call: off}\n    legal_forms: [ApS", "individuals: {email: consent, call: never}\n    legal_forms: [ApS"},
		"unknown field":   {"low_confidence: true\n    email: {verdict: open", "low_confidence: true\n    mood: calm\n    email: {verdict: open"},
		"bad register":    {"registers: [rpo]", "registers: [nope]"},
		"bad zone":        {"tz: Europe/Rome", "tz: Europe/Atlantis"},
		"bad window":      {"start: \"10:00\", end: \"13:00\"", "start: \"13:00\", end: \"10:00\""},
		"invented url":    {"url: \"https://www.gesetze-im-internet.de/uwg_2004/__7.html\"", "url: \"ftp://example.org/x\""},
	}
	for name, sw := range cases {
		t.Run(name, func(t *testing.T) {
			if !bytes.Contains(countriesYAML, []byte(sw[0])) {
				t.Fatalf("fixture text %q not in countries.yaml", sw[0])
			}
			bad := bytes.Replace(countriesYAML, []byte(sw[0]), []byte(sw[1]), 1)
			if _, err := Parse(bad); err == nil {
				t.Fatal("Parse accepted a bad file")
			}
		})
	}
}

func TestVerdictBothOpen(t *testing.T) {
	wed := at(t, "Europe/London", 10, 28, 11, 0)
	runCases(t, Default(), []vcase{
		{"GB company email", "GB", ChannelEmail, company, CampaignOverrides{}, wed, true, false, RuleOpen, ""},
		{"GB sole trader email refused", "GB", ChannelEmail, OrgFacts{EntityType: EntitySoleTrader}, CampaignOverrides{}, wed, false, true, RuleIndividual, "individual"},
		{"GB unknown entity email refused", "GB", ChannelEmail, OrgFacts{}, CampaignOverrides{}, wed, false, true, RuleIndividual, ""},
		{"GB company call in hours", "GB", ChannelCall, company, CampaignOverrides{}, wed, true, false, RuleOpen, "TPS"},
		{"GB call after hours", "GB", ChannelCall, company, CampaignOverrides{}, at(t, "Europe/London", 10, 28, 18, 0), false, true, RuleWindow, ""},
		{"US lead in FL not callable", "US", ChannelCall, OrgFacts{EntityType: EntityCompany, Region: "FL"}, CampaignOverrides{}, at(t, "America/New_York", 10, 28, 11, 0), false, true, RuleRegionBlocked, "Florida"},
		{"US lead in US-TX not callable", "US", ChannelCall, OrgFacts{EntityType: EntityCompany, Region: "us-tx"}, CampaignOverrides{}, at(t, "America/Chicago", 10, 28, 11, 0), false, true, RuleRegionBlocked, ""},
		{"US region unknown", "US", ChannelCall, company, CampaignOverrides{}, at(t, "America/New_York", 10, 28, 11, 0), false, true, RuleRegionUnknown, "region unknown: not callable"},
		{"US NY callable Saturday 20:30", "US", ChannelCall, OrgFacts{EntityType: EntityCompany, Region: "NY"}, CampaignOverrides{}, at(t, "America/New_York", 10, 31, 20, 30), true, false, RuleOpen, ""},
		{"US NY not at 21:00", "US", ChannelCall, OrgFacts{EntityType: EntityCompany, Region: "NY"}, CampaignOverrides{}, at(t, "America/New_York", 10, 28, 21, 0), false, true, RuleWindow, ""},
		{"US KY in hours in both zones", "US", ChannelCall, OrgFacts{EntityType: EntityCompany, Region: "KY"}, CampaignOverrides{}, at(t, "America/New_York", 10, 28, 8, 30), false, true, RuleWindow, ""},
		{"US email sole trader same", "US", ChannelEmail, OrgFacts{EntityType: EntitySoleTrader}, CampaignOverrides{}, wed, true, false, RuleOpen, "CAN-SPAM"},
		{"AU call on a Sunday not callable", "AU", ChannelCall, OrgFacts{EntityType: EntityCompany, Region: "NSW"}, CampaignOverrides{}, at(t, "Australia/Sydney", 11, 1, 11, 0), false, true, RuleWindow, ""},
		{"AU call Saturday 16:00", "AU", ChannelCall, OrgFacts{EntityType: EntityCompany, Region: "VIC"}, CampaignOverrides{}, at(t, "Australia/Melbourne", 10, 31, 16, 0), true, false, RuleOpen, ""},
		{"AU call Saturday 17:30", "AU", ChannelCall, OrgFacts{EntityType: EntityCompany, Region: "VIC"}, CampaignOverrides{}, at(t, "Australia/Melbourne", 10, 31, 17, 30), false, true, RuleWindow, ""},
		{"AU region unknown", "AU", ChannelCall, company, CampaignOverrides{}, at(t, "Australia/Sydney", 10, 28, 11, 0), false, true, RuleRegionUnknown, ""},
		{"AU email published address", "AU", ChannelEmail, company, CampaignOverrides{}, wed, true, false, RulePublishedAddress, ""},
		{"CA weekend 17:00 callable", "CA", ChannelCall, OrgFacts{EntityType: EntityCompany, Region: "ON"}, CampaignOverrides{}, at(t, "America/Toronto", 10, 31, 17, 0), true, false, RuleOpen, ""},
		{"CA weekend 19:00 not callable", "CA", ChannelCall, OrgFacts{EntityType: EntityCompany, Region: "ON"}, CampaignOverrides{}, at(t, "America/Toronto", 10, 31, 19, 0), false, true, RuleWindow, ""},
		{"CA weekday 21:00 callable", "CA", ChannelCall, OrgFacts{EntityType: EntityCompany, Region: "BC"}, CampaignOverrides{}, at(t, "America/Vancouver", 10, 28, 21, 0), true, false, RuleOpen, ""},
		{"CA weekend 09:30 not callable", "CA", ChannelCall, OrgFacts{EntityType: EntityCompany, Region: "ON"}, CampaignOverrides{}, at(t, "America/Toronto", 11, 1, 9, 30), false, true, RuleWindow, ""},
		{"IE mobile refused", "IE", ChannelCall, OrgFacts{EntityType: EntityCompany, PhoneType: PhoneType("+353 87 123 4567")}, CampaignOverrides{}, at(t, "Europe/Dublin", 10, 28, 11, 0), false, true, RuleMobile, "never mobiles"},
		{"IE unknown line refused", "IE", ChannelCall, company, CampaignOverrides{}, at(t, "Europe/Dublin", 10, 28, 11, 0), false, true, RuleMobile, ""},
		{"IE landline callable", "IE", ChannelCall, OrgFacts{EntityType: EntityCompany, PhoneType: PhoneType("+35312345678")}, CampaignOverrides{}, at(t, "Europe/Dublin", 10, 28, 11, 0), true, false, RuleOpen, ""},
		{"FR lunch break", "FR", ChannelCall, company, CampaignOverrides{}, at(t, "Europe/Paris", 10, 28, 13, 30), false, true, RuleWindow, ""},
		{"FR evening 19:00", "FR", ChannelCall, company, CampaignOverrides{}, at(t, "Europe/Paris", 10, 28, 19, 0), true, false, RuleOpen, ""},
		{"FR email role address", "FR", ChannelEmail, company, CampaignOverrides{}, wed, true, false, RuleRoleAddressOnly, ""},
		{"unlisted NO", "NO", ChannelEmail, company, CampaignOverrides{}, wed, false, true, RuleUnlisted, "not in the country rules"},
	})
}

// consentOffYAML keeps the call-first and off mechanics under test now that no default country uses them.
const consentOffYAML = `# test
research_date: 2026-10-27
default_call_windows:
  - {days: [Mon, Tue, Wed, Thu, Fri], start: "09:30", end: "17:00"}
registers: {}
countries:
  XC:
    name: Callfirstland
    group: call_first
    tz: Europe/Bucharest
    email: {verdict: consent, rules: ["unlocked by a 'yes, email me' on the call"]}
    call: {verdict: open}
    individuals: {email: consent, call: same}
    confidence: unverified
  XO:
    name: Offland
    group: off
    tz: Europe/Vienna
    email: {verdict: off}
    call: {verdict: off}
    individuals: {email: off, call: off}
    confidence: unverified
`

func TestVerdictCallFirst(t *testing.T) {
	tab, err := Parse([]byte(consentOffYAML))
	if err != nil {
		t.Fatal(err)
	}
	wed := at(t, "Europe/Bucharest", 10, 28, 11, 0)
	runCases(t, tab, []vcase{
		{"email consent refused", "XC", ChannelEmail, company, CampaignOverrides{}, wed, false, true, RuleConsent, "call first"},
		{"email after consent", "XC", ChannelEmail, OrgFacts{EntityType: EntityCompany, EmailConsent: true}, CampaignOverrides{}, wed, true, false, RuleConsent, ""},
		{"call open", "XC", ChannelCall, company, CampaignOverrides{}, wed, true, false, RuleOpen, ""},
		{"sole trader call open", "XC", ChannelCall, OrgFacts{EntityType: EntitySoleTrader}, CampaignOverrides{}, wed, true, false, RuleOpen, ""},
	})
	for code, want := range map[string]bool{"XC": true, "XO": false, "NO": false} {
		if got := tab.CallFirst(code); got != want {
			t.Errorf("CallFirst(%s) = %v, want %v", code, got, want)
		}
	}
}

func TestVerdictOffCountries(t *testing.T) {
	tab, err := Parse([]byte(consentOffYAML))
	if err != nil {
		t.Fatal(err)
	}
	now := at(t, "Europe/Vienna", 10, 28, 11, 0)
	runCases(t, tab, []vcase{
		{"email off", "XO", ChannelEmail, company, CampaignOverrides{}, now, false, true, RuleOff, ""},
		{"call off", "XO", ChannelCall, company, CampaignOverrides{}, now, false, true, RuleOff, ""},
		{"campaign cannot enable", "XO", ChannelCall, company, CampaignOverrides{Countries: map[string]CampaignCountry{"XO": {Email: true, Call: true, Acknowledge: "please"}}}, now, false, true, RuleOff, "no campaign can turn it on"},
	})
	if got := tab.CheckCampaign(CampaignOverrides{Countries: map[string]CampaignCountry{"XO": {Call: true}}}); len(got) != 1 || !strings.HasPrefix(got[0], "countries.XO: call is off by law") {
		t.Fatalf("CheckCampaign = %q", got)
	}
}

// The operator opened these on 29 Oct 2026: email first, on the 27 Oct terms (companies, role addresses).
func TestVerdictEmailFirstEverywhere(t *testing.T) {
	wed := at(t, "Europe/Bucharest", 10, 28, 11, 0)
	runCases(t, Default(), []vcase{
		{"RO company email", "RO", ChannelEmail, company, CampaignOverrides{}, wed, true, false, RuleRoleAddressOnly, "29 Oct 2026"},
		{"RO sole trader email needs consent", "RO", ChannelEmail, OrgFacts{EntityType: EntitySoleTrader}, CampaignOverrides{}, wed, false, true, RuleIndividual, ""},
		{"RO call open", "RO", ChannelCall, company, CampaignOverrides{}, wed, true, false, RuleOpen, "Calls to Romania are allowed"},
		{"RO sole trader call open", "RO", ChannelCall, OrgFacts{EntityType: EntitySoleTrader}, CampaignOverrides{}, wed, true, false, RuleOpen, ""},
		{"DE call needs acknowledge", "DE", ChannelCall, company, allOn("DE"), at(t, "Europe/Berlin", 10, 28, 11, 0), false, true, RuleAcknowledge, "relevant"},
		{"DE call with acknowledge", "DE", ChannelCall, company, CampaignOverrides{Countries: map[string]CampaignCountry{"DE": {Call: true, Acknowledge: "D7 sites past end of life"}}}, at(t, "Europe/Berlin", 10, 28, 11, 0), true, false, RuleOpen, ""},
		{"DK call sole trader refused", "DK", ChannelCall, OrgFacts{EntityType: EntitySoleTrader}, CampaignOverrides{}, wed, false, true, RuleIndividual, "companies only"},
		{"NL call company", "NL", ChannelCall, company, CampaignOverrides{}, at(t, "Europe/Amsterdam", 10, 28, 11, 0), true, false, RuleOpen, ""},
		{"NL call unknown entity refused", "NL", ChannelCall, OrgFacts{}, CampaignOverrides{}, at(t, "Europe/Amsterdam", 10, 28, 11, 0), false, true, RuleIndividual, ""},
		{"LV company email", "LV", ChannelEmail, company, CampaignOverrides{}, wed, true, false, RuleRoleAddressOnly, "29 Oct 2026"},
	})
	if _, _, _, reason := Default().Verdict("RO", ChannelEmail, company, CampaignOverrides{}, wed); strings.Contains(reason, "..") {
		t.Errorf("reason %q ends in a double full stop", reason)
	}
	now := at(t, "Europe/Vienna", 10, 28, 11, 0)
	for _, code := range []string{"IT", "ES", "DE", "DK", "NL", "LV", "AT", "PL", "CZ", "BG"} {
		runCases(t, Default(), []vcase{
			{code + " company email", code, ChannelEmail, company, CampaignOverrides{}, now, true, false, RuleRoleAddressOnly, "29 Oct 2026"},
			{code + " unknown email", code, ChannelEmail, OrgFacts{}, CampaignOverrides{}, now, false, true, RuleIndividual, ""},
		})
	}
	for _, code := range []string{"AT", "PL", "CZ", "BG"} {
		runCases(t, Default(), []vcase{
			{code + " company call", code, ChannelCall, company, CampaignOverrides{}, now, true, false, RuleOpen, ""},
			{code + " sole trader call", code, ChannelCall, OrgFacts{EntityType: EntitySoleTrader}, CampaignOverrides{}, now, false, true, RuleIndividual, ""},
		})
	}
}

func TestVerdictOperatorOpened(t *testing.T) {
	now := at(t, "Europe/Lisbon", 10, 28, 11, 0)
	for _, code := range []string{"PT", "SI", "LT", "HU", "LU", "MT", "HR", "GR"} {
		runCases(t, Default(), []vcase{
			{code + " company email", code, ChannelEmail, company, CampaignOverrides{}, now, true, false, RuleRoleAddressOnly, "Operator, 27 Oct 2026"},
			{code + " sole trader email", code, ChannelEmail, OrgFacts{EntityType: EntitySoleTrader}, CampaignOverrides{}, now, false, true, RuleIndividual, ""},
			{code + " unknown email", code, ChannelEmail, OrgFacts{}, CampaignOverrides{}, now, false, true, RuleIndividual, ""},
			{code + " company call", code, ChannelCall, company, CampaignOverrides{}, now, true, false, RuleOpen, ""},
		})
	}
}

// CH was opened by the operator on 28 Oct 2026, on the same terms as the 27 Oct group.
func TestVerdictSwitzerland(t *testing.T) {
	wed := at(t, "Europe/Zurich", 10, 28, 11, 0)
	runCases(t, Default(), []vcase{
		{"CH company email", "CH", ChannelEmail, company, CampaignOverrides{}, wed, true, false, RuleRoleAddressOnly, "28 Oct 2026"},
		{"CH sole trader email refused", "CH", ChannelEmail, OrgFacts{EntityType: EntitySoleTrader}, CampaignOverrides{}, wed, false, true, RuleIndividual, ""},
		{"CH unknown entity email refused", "CH", ChannelEmail, OrgFacts{}, CampaignOverrides{}, wed, false, true, RuleIndividual, ""},
		{"CH company call in hours", "CH", ChannelCall, company, CampaignOverrides{}, wed, true, false, RuleOpen, "Sterne"},
		{"CH sole trader call refused", "CH", ChannelCall, OrgFacts{EntityType: EntitySoleTrader}, CampaignOverrides{}, wed, false, true, RuleIndividual, ""},
		{"CH call after hours", "CH", ChannelCall, company, CampaignOverrides{}, at(t, "Europe/Zurich", 10, 28, 17, 30), false, true, RuleWindow, ""},
	})
	if c, _ := Default().Lookup("CH"); c.TZ != "Europe/Zurich" {
		t.Errorf("CH tz %q", c.TZ)
	}
}

// A DK sole trader is off by law: hard. IE calls are legal, the campaign has them off: soft.
func TestHardLegalVersusSoftCampaign(t *testing.T) {
	camp := CampaignOverrides{Countries: map[string]CampaignCountry{
		"IE": {Email: true, Call: false},
		"DK": {Email: false, Call: true},
	}}
	ie := OrgFacts{EntityType: EntityCompany, PhoneType: "landline"}
	runCases(t, Default(), []vcase{
		{"DK sole trader call blocked hard", "DK", ChannelCall, OrgFacts{EntityType: EntitySoleTrader}, camp, at(t, "Europe/Copenhagen", 10, 28, 11, 0), false, true, RuleIndividual, ""},
		{"IE campaign call off soft", "IE", ChannelCall, ie, camp, at(t, "Europe/Dublin", 10, 28, 11, 0), false, false, RuleCampaignOff, "your call"},
		{"IE campaign email on", "IE", ChannelEmail, company, camp, at(t, "Europe/Dublin", 10, 28, 11, 0), true, false, RuleOpen, ""},
		{"GB not in campaign soft", "GB", ChannelCall, company, camp, at(t, "Europe/London", 10, 28, 11, 0), false, false, RuleCampaignOff, ""},
		{"IE campaign off but hard rule wins", "IE", ChannelCall, OrgFacts{EntityType: EntityCompany, PhoneType: "mobile"}, camp, at(t, "Europe/Dublin", 10, 28, 11, 0), false, true, RuleMobile, ""},
		{"IE outside hours hard before soft", "IE", ChannelCall, ie, camp, at(t, "Europe/Dublin", 10, 28, 20, 0), false, true, RuleWindow, ""},
	})
}

const holdYAML = `# test
research_date: 2026-10-27
default_call_windows:
  - {days: [Mon, Tue, Wed, Thu, Fri], start: "09:30", end: "17:00"}
registers: {}
countries:
  XA:
    name: Holdland
    group: both_open
    tz: Europe/London
    email: {verdict: hold}
    call: {verdict: hold}
    individuals: {email: consent, call: same}
    confidence: unverified
`

func TestVerdictHoldNeedsAcknowledge(t *testing.T) {
	tab, err := Parse([]byte(holdYAML))
	if err != nil {
		t.Fatal(err)
	}
	now := at(t, "Europe/London", 10, 28, 11, 0)
	runCases(t, tab, []vcase{
		{"no campaign", "XA", ChannelEmail, company, CampaignOverrides{}, now, false, true, RuleHold, "acknowledge"},
		{"on without reason", "XA", ChannelEmail, company, allOn("XA"), now, false, true, RuleHold, ""},
		{"on with reason", "XA", ChannelEmail, company, CampaignOverrides{Countries: map[string]CampaignCountry{"XA": {Email: true, Acknowledge: "checked with counsel"}}}, now, true, false, RuleHoldAcknowledged, ""},
		{"reason but channel off", "XA", ChannelCall, company, CampaignOverrides{Countries: map[string]CampaignCountry{"XA": {Email: true, Acknowledge: "checked"}}}, now, false, true, RuleHold, ""},
	})
	if got := tab.CheckCampaign(allOn("XA")); len(got) != 2 {
		t.Fatalf("CheckCampaign = %q, want both channels flagged", got)
	}
}

func TestCheckCampaign(t *testing.T) {
	got := Default().CheckCampaign(CampaignOverrides{Countries: map[string]CampaignCountry{
		"AT": {Email: true, Call: true},
		"GB": {Email: true, Call: true},
		"NO": {Email: true},
		"DE": {Call: true},
		"IE": {Email: true, Call: false},
	}})
	want := []string{"countries.DE: call in Germany needs a written acknowledge", "countries.NO: email is on"}
	if len(got) != len(want) {
		t.Fatalf("CheckCampaign = %q", got)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("fault %d = %q, want prefix %q", i, got[i], want[i])
		}
	}
}

func TestFreshness(t *testing.T) {
	d := Default().ResearchDate
	if w := Freshness(d.AddDate(0, 0, 180)); w != "" {
		t.Fatalf("180 days warned: %q", w)
	}
	w := Freshness(d.AddDate(0, 0, 181))
	if !strings.Contains(w, "181 days") || !strings.Contains(w, "2026-10-27") {
		t.Fatalf("181 days: %q", w)
	}
}

func TestRegistersFor(t *testing.T) {
	cases := map[string]map[string]int{
		"GB": {"tps": 28, "ctps": 28}, "BE": {"dncm": 3}, "IE": {"ndd": 0}, "AU": {"dncr": 0},
		"IT": {"rpo": 0}, "ES": {"robinson": 0}, "GR": {"gr_art11": 0}, "RO": {}, "CH": {},
	}
	for code, want := range cases {
		got := map[string]int{}
		for _, r := range RegistersFor(code) {
			got[r.Code] = r.MaxAgeDays
			if r.Country != code {
				t.Errorf("%s register %s belongs to %s", code, r.Code, r.Country)
			}
		}
		if len(got) != len(want) {
			t.Errorf("RegistersFor(%s) = %v, want %v", code, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("RegistersFor(%s)[%s] = %d, want %d", code, k, got[k], v)
			}
		}
	}
}

func TestPhoneType(t *testing.T) {
	for in, want := range map[string]string{
		"+353871234567": "mobile", "+353 83 000 0000": "mobile", "+35312345678": "landline",
		"+3531800123456": "landline", "+353818123456": "landline", "+442078031100": "",
	} {
		if got := PhoneType(in); got != want {
			t.Errorf("PhoneType(%s) = %q, want %q", in, got, want)
		}
	}
}

// TestVerdictIndividualsWording: the companies-only refusal agrees with its channel ("Calls
// in Denmark go", never "goes").
func TestVerdictIndividualsWording(t *testing.T) {
	ok, hard, rule, reason := Verdict("DK", ChannelCall, OrgFacts{EntityType: EntitySoleTrader}, CampaignOverrides{}, time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC))
	if ok || !hard || rule != RuleIndividual || !strings.HasPrefix(reason, "Calls in Denmark go to companies only;") {
		t.Fatalf("ok %v hard %v rule %s reason %q", ok, hard, rule, reason)
	}
}

// A call channel whose verdict is consent is refused in the call's own words, never as email.
func TestVerdictCallConsentWording(t *testing.T) {
	old := []byte("    email: *opened_email\n    call:\n      verdict: open\n      rules: [\"live human calls are outside art. 12")
	if !bytes.Contains(countriesYAML, old) {
		t.Fatal("RO email and call block not found in countries.yaml")
	}
	yml := bytes.Replace(countriesYAML, old,
		[]byte("    email: {verdict: consent}\n    call:\n      verdict: consent\n      rules: [\"live human calls are outside art. 12"), 1)
	tab, err := Parse(yml)
	if err != nil {
		t.Fatal(err)
	}
	wed := at(t, "Europe/Bucharest", 10, 28, 11, 0)
	runCases(t, tab, []vcase{
		{"RO call consent refused", "RO", ChannelCall, company, CampaignOverrides{}, wed, false, true, RuleConsent, "Calls to Romania need their recorded yes first"},
		{"RO email consent still email", "RO", ChannelEmail, company, CampaignOverrides{}, wed, false, true, RuleConsent, "Email to Romania needs their recorded yes first"},
	})
	_, _, _, reason := tab.Verdict("RO", ChannelCall, company, CampaignOverrides{}, wed)
	if strings.Contains(reason, "Email") || strings.Contains(reason, "call first") {
		t.Errorf("call refusal %q speaks of email", reason)
	}
}
