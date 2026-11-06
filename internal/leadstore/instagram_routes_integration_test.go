//go:build integration

package leadstore_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestInstagramRouteRanks: an Instagram profile is a café campaign's channel and nothing more.
// The export ranks it after a LinkedIn page, a hand lead (never a café's) never takes it, the
// route search still searches an org that holds only one, and no reader of routes ever sees a
// loyalty vendor's own account (signals.yaml) as an org's route. Every name here is invented.
func TestInstagramRouteRanks(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	agency := func(domain string) int64 {
		t.Helper()
		r, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Agency " + domain, Domain: domain}, nil)
		if err != nil {
			t.Fatalf("agency %s: %v", domain, err)
		}
		return r.OrgID
	}
	route := func(org int64, kind, value string) {
		t.Helper()
		if _, err := f.q.InsertRoute(ctx, leadstore.RouteRow{OrgID: org, Kind: kind, Value: value, FetchID: f.fid, DerivedBy: "enrich@1"}); err != nil {
			t.Fatalf("route %s: %v", value, err)
		}
	}
	// Both orgs hold an Instagram route found first; the first also a LinkedIn page found after.
	both, igOnly := agency("example.org"), agency("igonly.localhost")
	route(both, leadstore.RouteInstagram, "both_agency.example")
	route(both, leadstore.RouteLinkedIn, "https://www.linkedin.com/company/both-agency-example")
	route(igOnly, leadstore.RouteInstagram, "igonly_agency.example")

	rows, err := f.q.ExportRoutes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(rows, func(r leadstore.ExportRow) bool { return r.Domain == "example.org" })
	if i < 0 || rows[i].RouteKind != leadstore.RouteLinkedIn {
		t.Errorf("export of the org with LinkedIn and Instagram: %+v, want its LinkedIn page", rows)
	}

	for domain, want := range map[string]string{"example.org": leadstore.RouteLinkedIn, "igonly.localhost": leadstore.ChannelNone} {
		hand, err := f.q.MarkContacted(ctx, 0, domain, "")
		if err != nil {
			t.Fatalf("hand lead %s: %v", domain, err)
		}
		if l := f.leadOf(t, hand.LeadID); l.Channel != want || (want == leadstore.ChannelNone) != (l.RouteID == nil) {
			t.Errorf("hand lead of %s: channel %q route %v, want %q", domain, l.Channel, l.RouteID, want)
		}
	}

	need, err := f.q.OrgsNeedingRoutes(ctx, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(need, func(o leadstore.Org) bool { return o.ID == igOnly }) {
		t.Errorf("an org holding only an Instagram route is not searched for routes: %+v", need)
	}

	// A café's site links its loyalty vendor's account; the 00031 backfill read such links as
	// routes. It is never the café's route.
	cafe := f.cafeLead(t, "vendorlink.example", nil, "")
	org := f.leadOf(t, cafe).OrgID
	route(org, leadstore.RouteInstagram, "stampino.app")
	route(org, leadstore.RouteInstagram, "vendorlink_cafe.example")
	routes, err := f.q.RoutesFor(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for _, r := range routes {
		if r.Kind == leadstore.RouteInstagram {
			values = append(values, r.Value)
		}
	}
	if !slices.Equal(values, []string{"https://www.instagram.com/vendorlink_cafe.example/"}) {
		t.Errorf("instagram routes %v, want only the café's own", values)
	}
	if !leadstore.IsVendorIGHandle("https://www.instagram.com/stampino.app/") || leadstore.IsVendorIGHandle("vendorlink_cafe.example") {
		t.Error("IsVendorIGHandle does not tell the vendor's account from the café's")
	}
}
