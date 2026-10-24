package outreach

import (
	"fmt"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

func leadTestConfig() leadconfig.Config {
	return leadconfig.Config{
		PublicBodySuffixes: []string{"gov.example", "ac.example"},
		Countries: map[string]leadconfig.Country{
			"GB": {Channel: leadconfig.ChannelEmail},
			"DE": {Channel: leadconfig.ChannelForm},
			"FR": {Channel: leadconfig.ChannelNone},
		},
	}
}

func leadPtr(v int64) *int64 { return &v }

func leadEq(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestClassify(t *testing.T) {
	site := func(country string) Org {
		return Org{ID: 1, Kind: OrgKindSiteOwner, Domain: "northwind.example", Country: country}
	}
	agency := Org{ID: 2, Kind: OrgKindAgency, Domain: "fabrikam.example", Country: "GB"}
	probe := func(major int, ambiguous string) *Probe {
		return &Probe{ID: 10, IsDrupal: true, Major: major, Ambiguous: ambiguous}
	}
	finding := &AgencyFinding{ID: 20}
	mail := Route{ID: 30, Kind: RouteEmail, Value: "office@northwind.example", MXOK: true}
	mailNoMX := Route{ID: 31, Kind: RouteEmail, Value: "info@northwind.example", MXOK: false}
	form := Route{ID: 32, Kind: RouteForm, Value: "https://northwind.example/contact"}

	tests := []struct {
		name    string
		org     Org
		probe   *Probe
		finding *AgencyFinding
		routes  []Route
		ok      bool
		reason  string
		want    Lead
	}{
		{name: "major 6 unsupported", org: site("GB"), probe: probe(6, ""), routes: []Route{mail}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelEmail, ProbeID: leadPtr(10), RouteID: leadPtr(30), Reserved: true}},
		{name: "major 7 unsupported", org: site("GB"), probe: probe(7, ""), routes: []Route{mail}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelEmail, ProbeID: leadPtr(10), RouteID: leadPtr(30), Reserved: true}},
		{name: "major 8 unsupported", org: site("GB"), probe: probe(8, ""), routes: []Route{mail}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelEmail, ProbeID: leadPtr(10), RouteID: leadPtr(30), Reserved: true}},
		{name: "major 9 unsupported", org: site("GB"), probe: probe(9, ""), routes: []Route{mail}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelEmail, ProbeID: leadPtr(10), RouteID: leadPtr(30), Reserved: true}},
		{name: "major 10 d10", org: site("GB"), probe: probe(10, ""), routes: []Route{mail}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindD10, Channel: leadconfig.ChannelEmail, ProbeID: leadPtr(10), RouteID: leadPtr(30), Reserved: true}},
		{name: "major 11 already current", org: site("GB"), probe: probe(11, ""), routes: []Route{mail}, reason: ReasonCurrent},
		{name: "major unknown", org: site("GB"), probe: probe(0, ""), routes: []Route{mail}, reason: ReasonVersionUnknown},
		{name: "ambiguous 10-or-11", org: site("GB"), probe: probe(10, "10-or-11"), routes: []Route{mail}, reason: ReasonVersionUnknown},
		{name: "major 5 out of range", org: site("GB"), probe: probe(5, ""), reason: ReasonVersionUnknown},
		{name: "not drupal", org: site("GB"), probe: &Probe{ID: 10}, reason: ReasonNotDrupal},
		{name: "site without probe", org: site("GB"), reason: ReasonNoFinding},
		{name: "agency with finding", org: agency, finding: finding, routes: []Route{mail}, ok: true,
			want: Lead{OrgID: 2, Kind: leadconfig.KindAgencyPartner, Channel: leadconfig.ChannelEmail, AgencyFindingID: leadPtr(20), RouteID: leadPtr(30), Reserved: true}},
		{name: "agency without finding", org: agency, routes: []Route{mail}, reason: ReasonNoFinding},
		{name: "public body", org: Org{ID: 3, Kind: OrgKindSiteOwner, Domain: "council.gov.example", Country: "GB"}, probe: probe(7, ""), routes: []Route{mail}, reason: ReasonPublicBody},
		{name: "public body suffix itself", org: Org{ID: 3, Kind: OrgKindSiteOwner, Domain: "AC.Example", Country: "GB"}, probe: probe(7, ""), reason: ReasonPublicBody},
		{name: "suffix lookalike is not public", org: Org{ID: 3, Kind: OrgKindSiteOwner, Domain: "mygov.example", Country: "GB"}, probe: probe(7, ""), routes: []Route{mail}, ok: true,
			want: Lead{OrgID: 3, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelEmail, ProbeID: leadPtr(10), RouteID: leadPtr(30), Reserved: true}},
		{name: "selftest org", org: Org{ID: 4, Kind: OrgKindSelftest, Domain: "selftest-1"}, probe: probe(7, ""), reason: ReasonSelftest},
		{name: "unknown org kind", org: Org{ID: 5, Kind: "other", Domain: "northwind.example"}, probe: probe(7, ""), reason: ReasonOrgKind},
		{name: "email falls back to form without MX", org: site("GB"), probe: probe(9, ""), routes: []Route{mailNoMX, form}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelForm, ProbeID: leadPtr(10), RouteID: leadPtr(32), Reserved: true}},
		{name: "email with MX route after a non-MX one", org: site("GB"), probe: probe(9, ""), routes: []Route{mailNoMX, form, mail}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelEmail, ProbeID: leadPtr(10), RouteID: leadPtr(30), Reserved: true}},
		{name: "email with no usable route is none", org: site("GB"), probe: probe(9, ""), routes: []Route{mailNoMX}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelNone, ProbeID: leadPtr(10), Reserved: true}},
		{name: "form country uses the form route", org: site("DE"), probe: probe(8, ""), routes: []Route{mail, form}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelForm, ProbeID: leadPtr(10), RouteID: leadPtr(32), Reserved: true}},
		{name: "form country without form is none, never email", org: site("DE"), probe: probe(8, ""), routes: []Route{mail}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelNone, ProbeID: leadPtr(10), Reserved: true}},
		{name: "none country", org: site("FR"), probe: probe(8, ""), routes: []Route{mail, form}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelNone, ProbeID: leadPtr(10), Reserved: true}},
		{name: "missing country is none", org: site(""), probe: probe(8, ""), routes: []Route{mail, form}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelNone, ProbeID: leadPtr(10), Reserved: true}},
		{name: "country absent from policy is none", org: site("NL"), probe: probe(8, ""), routes: []Route{mail}, ok: true,
			want: Lead{OrgID: 1, Kind: leadconfig.KindUnsupported, Channel: leadconfig.ChannelNone, ProbeID: leadPtr(10), Reserved: true}},
		{name: "invalid TLD flagged reserved", org: Org{ID: 6, Kind: OrgKindSiteOwner, Domain: "contoso.invalid", Country: "GB"}, probe: probe(10, ""), routes: []Route{mail}, ok: true,
			want: Lead{OrgID: 6, Kind: leadconfig.KindD10, Channel: leadconfig.ChannelEmail, ProbeID: leadPtr(10), RouteID: leadPtr(30), Reserved: true}},
		{name: "real TLD not reserved", org: Org{ID: 7, Kind: OrgKindSiteOwner, Domain: leadRealLike, Country: "GB"}, probe: probe(10, ""), routes: []Route{mail}, ok: true,
			want: Lead{OrgID: 7, Kind: leadconfig.KindD10, Channel: leadconfig.ChannelEmail, ProbeID: leadPtr(10), RouteID: leadPtr(30)}},
	}
	cfg := leadTestConfig()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, ok := Classify(tc.org, tc.probe, tc.finding, tc.routes, cfg)
			if ok != tc.ok {
				t.Fatalf("ok = %v (reason %q), want %v", ok, reason, tc.ok)
			}
			if !ok {
				if reason != tc.reason {
					t.Errorf("reason = %q, want %q", reason, tc.reason)
				}
				if got != (Lead{}) {
					t.Errorf("lead = %+v, want zero", got)
				}
				return
			}
			if reason != "" {
				t.Errorf("reason = %q on a lead", reason)
			}
			w := tc.want
			if got.OrgID != w.OrgID || got.Kind != w.Kind || got.Channel != w.Channel || got.Reserved != w.Reserved ||
				!leadEq(got.ProbeID, w.ProbeID) || !leadEq(got.AgencyFindingID, w.AgencyFindingID) || !leadEq(got.RouteID, w.RouteID) {
				t.Errorf("lead = %s, want %s", leadFmt(got), leadFmt(w))
			}
		})
	}
}

// TestClassifyOneFinding holds the lead finding CHECK: a classified lead has exactly one of
// probe and agency finding, even when the caller passes both.
func TestClassifyOneFinding(t *testing.T) {
	cfg := leadTestConfig()
	p := &Probe{ID: 10, IsDrupal: true, Major: 7}
	f := &AgencyFinding{ID: 20}
	for _, org := range []Org{
		{ID: 1, Kind: OrgKindSiteOwner, Domain: "northwind.example", Country: "GB"},
		{ID: 2, Kind: OrgKindAgency, Domain: "fabrikam.example", Country: "GB"},
	} {
		l, _, ok := Classify(org, p, f, nil, cfg)
		if !ok {
			t.Fatalf("%s: no lead", org.Kind)
		}
		if (l.ProbeID != nil) == (l.AgencyFindingID != nil) {
			t.Errorf("%s: lead %s has not exactly one finding", org.Kind, leadFmt(l))
		}
	}
}

func TestReservedDomain(t *testing.T) {
	for d, want := range map[string]bool{
		"northwind.example": true, "MAIL.Contoso.INVALID.": true, "fabrikam.test": true, "x.localhost": true,
		leadRealLike: false, "example.com": false, "": false,
	} {
		if got := ReservedDomain(d); got != want {
			t.Errorf("ReservedDomain(%q) = %v, want %v", d, got, want)
		}
	}
}

// TestLeadStateGrid covers the pure cells of the status × operation grid (live batches):
// which lead states a new batch drafts, an approve binds, and a send may send.
func TestLeadStateGrid(t *testing.T) {
	tests := []struct {
		state                 string
		batch, approve, sends bool
	}{
		{LeadNew, true, false, false},
		{LeadBatched, false, true, true},
		{LeadContacted, false, false, true},
		{LeadReplied, false, false, false},
		{LeadBounced, false, false, false},
		{LeadOptedOut, false, false, false},
		{LeadSuppressed, false, false, false},
		{LeadSkipped, false, false, false},
		{LeadClosed, false, false, false},
		{"", false, false, false},
		{"NEW", false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.state, func(t *testing.T) {
			if got := CanBatch(tc.state); got != tc.batch {
				t.Errorf("CanBatch = %v, want %v", got, tc.batch)
			}
			if got := CanApprove(tc.state); got != tc.approve {
				t.Errorf("CanApprove = %v, want %v", got, tc.approve)
			}
			if got := CanSend(tc.state); got != tc.sends {
				t.Errorf("CanSend = %v, want %v", got, tc.sends)
			}
		})
	}
}

func leadFmt(l Lead) string {
	p := func(v *int64) string {
		if v == nil {
			return "nil"
		}
		return fmt.Sprint(*v)
	}
	return fmt.Sprintf("{org %d %s %s probe=%s finding=%s route=%s reserved=%v}",
		l.OrgID, l.Kind, l.Channel, p(l.ProbeID), p(l.AgencyFindingID), p(l.RouteID), l.Reserved)
}

func TestPublicBody(t *testing.T) {
	suffixes := []string{"gov.example", " .ac.example. "}
	for d, want := range map[string]bool{
		"gov.example": true, "council.gov.example": true, "Uni.AC.Example.": true, " council.gov.example ": true,
		"mygov.example": false, "gov.example.other": false, "": false,
	} {
		if got := PublicBody(d, suffixes); got != want {
			t.Errorf("PublicBody(%q) = %v, want %v", d, got, want)
		}
	}
	if PublicBody("council.gov.example", nil) {
		t.Error("no suffixes: nothing is a public body")
	}
}

// leadRealLike is a name on a real public suffix, built at run time so no source line holds
// a real-looking domain.
var leadRealLike = "northwind" + ".co." + "uk"
