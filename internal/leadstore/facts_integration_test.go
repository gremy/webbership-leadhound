//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migration 00012: facts read from an org's own pages, and the enrichment attempts.

// pageFetch writes one site fetch of the fixture's run with body (empty: kept as size and
// hash only) and returns its id.
func (f outreachFixture) pageFetch(t *testing.T, url, finalURL, body string) int64 {
	t.Helper()
	return ownerInt(t, f.owner, `INSERT INTO http_fetch (run_id, source, kind, url, final_url, status, body)
		VALUES ($1, 'leads', 'site', $2, $3, 200, convert_to($4, 'UTF8')) RETURNING id`, f.runID, url, finalURL, body)
}

func TestFactsAppendOnly(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "facts.localhost", nil, 7, "")
	org := f.leadOf(t, lead).OrgID
	page := f.pageFetch(t, "https://facts.localhost/contact", "", "<html>contact</html>")
	phone := leadstore.FactRow{OrgID: org, Kind: leadstore.FactPhone, Value: "+44 20 7946 0123", Normalized: "+442079460123",
		FetchID: page, DerivedBy: "enrich@1"}
	if ins, err := f.q.InsertFact(ctx, phone); err != nil || !ins {
		t.Fatalf("insert phone: %v %v", ins, err)
	}
	again := phone
	again.Value = "020 7946 0123"
	if ins, err := f.q.InsertFact(ctx, again); err != nil || ins {
		t.Errorf("the same phone again: inserted %v, %v; want kept as it was", ins, err)
	}
	for _, r := range []leadstore.FactRow{
		{OrgID: org, Kind: leadstore.FactDescription, Value: "A bakery.", Normalized: "A bakery.", FetchID: page, DerivedBy: "enrich@1"},
		{OrgID: org, Kind: leadstore.FactSocial, Value: "https://social.example/facts", Normalized: "https://social.example/facts", FetchID: page, DerivedBy: "enrich@1"},
	} {
		if ins, err := f.q.InsertFact(ctx, r); err != nil || !ins {
			t.Fatalf("insert %s: %v %v", r.Kind, ins, err)
		}
	}
	facts, err := f.q.OrgFacts(ctx, []int64{org, org + 1000})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, x := range facts {
		kinds = append(kinds, x.Kind+"="+x.Value)
	}
	if want := []string{"description=A bakery.", "phone=+44 20 7946 0123", "social=https://social.example/facts"}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("org facts = %v, want %v", kinds, want)
	}
	if got, err := f.q.OrgFacts(ctx, nil); err != nil || got != nil {
		t.Errorf("no orgs: %v %v", got, err)
	}
	// Refusals: in Go, and the checks behind them.
	for name, c := range map[string]struct {
		row  leadstore.FactRow
		want error
	}{
		"kind":  {leadstore.FactRow{OrgID: org, Kind: "email", Value: "x", Normalized: "x", FetchID: page}, leadstore.ErrUnknownFactKind},
		"blank": {leadstore.FactRow{OrgID: org, Kind: leadstore.FactSocial, Value: " ", Normalized: "x", FetchID: page}, leadstore.ErrEmptyFact},
		"long":  {leadstore.FactRow{OrgID: org, Kind: leadstore.FactDescription, Value: strings.Repeat("é", 301), Normalized: "x", FetchID: page}, leadstore.ErrFactTooLong},
	} {
		if _, err := f.q.InsertFact(ctx, c.row); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if _, err := f.q.InsertFact(ctx, leadstore.FactRow{OrgID: org, Kind: leadstore.FactPhone, Value: "123", Normalized: "123", FetchID: page, DerivedBy: "enrich@1"}); sqlState(err) != "23514" {
		t.Errorf("a short phone: %v, want 23514", err)
	}
	app := storetest.AppConn(t)
	for _, q := range []string{"UPDATE org_fact SET value = 'x'", "DELETE FROM org_fact", "UPDATE enrich_attempt SET facts_found = 0", "DELETE FROM enrich_attempt"} {
		if _, err := app.Exec(ctx, q); sqlState(err) != "42501" {
			t.Errorf("%s as app: %v, want 42501", q, err)
		}
	}
}

func TestOrgsNeedingEnrichSkipsRecent(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	// Cold start: no leads, nothing to enrich.
	if got, err := f.q.OrgsNeedingEnrich(ctx, 0, time.Now()); err != nil || len(got) != 0 {
		t.Fatalf("cold start: %v %v", got, err)
	}
	older := f.siteLead(t, "older.localhost", nil, 7, "")
	newer := f.siteLead(t, "newer.localhost", nil, 7, "")
	recent := f.siteLead(t, "recent.localhost", nil, 7, "")
	long := f.siteLead(t, "long.localhost", nil, 7, "")
	ownerInt(t, f.owner, `INSERT INTO org (kind, domain, source) VALUES ('site_owner', 'nolead.localhost', 'probe') RETURNING id`)
	if _, err := f.q.CreateSelftest(ctx, leadstore.SelftestDraft{To: "me@sender.localhost", Subject: "s", Body: "b",
		MessageID: "<s@sender.localhost>", Due: time.Now(), MailboxID: f.mailbox(t)}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := f.q.RecordEnrichAttempt(ctx, f.leadOf(t, recent).OrgID, f.runID, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.q.RecordEnrichAttempt(ctx, f.leadOf(t, long).OrgID, f.runID, 2); err != nil {
		t.Fatal(err)
	}
	ownerRun(t, f.owner, `UPDATE enrich_attempt SET attempted_at = $1 WHERE org_id = $2`,
		now.Add(-leadstore.EnrichRetryAfter-time.Hour), f.leadOf(t, long).OrgID)
	orgs, err := f.q.OrgsNeedingEnrich(ctx, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range orgs {
		got = append(got, o.Domain)
	}
	if want := []string{"long.localhost", "newer.localhost", "older.localhost"}; !reflect.DeepEqual(got, want) {
		t.Errorf("orgs needing enrich = %v, want %v (newest lead first; recent, no-lead and selftest left out)", got, want)
	}
	if orgs, err := f.q.OrgsNeedingEnrich(ctx, 1, now); err != nil || len(orgs) != 1 {
		t.Errorf("limit 1: %d orgs, %v", len(orgs), err)
	}
	_, _ = older, newer
}

func TestEnrichBodiesOnlyKept(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{{Domain: "kept.localhost"}}); err != nil {
		t.Fatal(err)
	}
	home := f.pageFetch(t, "https://kept.localhost/", "https://www.kept.localhost/", "<html>home</html>")
	robots := ownerInt(t, f.owner, `INSERT INTO http_fetch (run_id, source, kind, url, status, body)
		VALUES ($1, 'leads', 'robots', 'https://kept.localhost/robots.txt', 200, convert_to('User-agent: *', 'UTF8')) RETURNING id`, f.runID)
	probeID, err := f.q.RecordProbe(ctx, siteID(t, "kept.localhost"), f.runID, leadstore.ProbeRow{
		Outcome: leadstore.OutcomeOK, IsDrupal: true, Major: 7, FetchIDs: []int64{home, robots}, DerivedBy: "drupal@1",
	})
	if err != nil {
		t.Fatal(err)
	}
	org, _, err := f.q.UpsertSiteOrg(ctx, probeID)
	if err != nil {
		t.Fatal(err)
	}
	if bodies, err := f.q.EnrichBodies(ctx, org); err != nil || len(bodies) != 1 || bodies[0].FetchID != home || bodies[0].Contact {
		t.Fatalf("before any route: %+v %v, want the homepage alone", bodies, err)
	}
	f.lead(t, leadstore.LeadRow{OrgID: org, Kind: leadstore.LeadUnsupported, ProbeID: &probeID}, "")
	contact := f.pageFetch(t, "https://kept.localhost/contact", "", "<html>contact</html>")
	sizeOnly := f.pageFetch(t, "https://kept.localhost/impressum", "", "")
	for _, r := range []leadstore.RouteRow{
		{OrgID: org, Kind: leadstore.RouteEmail, Value: "info@kept.localhost", FetchID: contact, DerivedBy: "contacts@1"},
		{OrgID: org, Kind: leadstore.RouteForm, Value: "https://kept.localhost/contact", FetchID: contact, DerivedBy: "contacts@1"},
		{OrgID: org, Kind: leadstore.RouteEmail, Value: "office@kept.localhost", FetchID: sizeOnly, DerivedBy: "contacts@1"},
	} {
		if _, err := f.q.InsertRoute(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	bodies, err := f.q.EnrichBodies(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	type page struct {
		ID      int64
		URL     string
		Body    string
		Contact bool
	}
	var got []page
	for _, b := range bodies {
		got = append(got, page{b.FetchID, b.URL, string(b.Body), b.Contact})
	}
	want := []page{
		{home, "https://www.kept.localhost/", "<html>home</html>", false},
		{contact, "https://kept.localhost/contact", "<html>contact</html>", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("enrich bodies = %+v, want %+v (each once, no robots, no size-only page)", got, want)
	}
	if _, err := f.q.EnrichBodies(ctx, org+1000); !errors.Is(err, leadstore.ErrOrgNotFound) {
		t.Errorf("missing org: %v, want ErrOrgNotFound", err)
	}
}
