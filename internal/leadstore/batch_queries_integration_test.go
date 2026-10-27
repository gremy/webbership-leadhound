//go:build integration

package leadstore_test

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// formLead adds a site lead on domain whose only route is a contact form (channel form).
func (f outreachFixture) formLead(t *testing.T, domain string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{{Domain: domain}}); err != nil {
		t.Fatal(err)
	}
	probeID, err := f.q.RecordProbe(ctx, siteID(t, domain), f.runID, leadstore.ProbeRow{
		Outcome: leadstore.OutcomeOK, FinalHost: domain, IsDrupal: true, Major: 10, FetchIDs: []int64{f.fid}, DerivedBy: "drupal@1",
	})
	if err != nil {
		t.Fatal(err)
	}
	orgID, _, err := f.q.UpsertSiteOrg(ctx, probeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.InsertRoute(ctx, leadstore.RouteRow{
		OrgID: orgID, Kind: leadstore.RouteForm, Value: "https://" + domain + "/contact", FetchID: f.fid, DerivedBy: "contacts@1",
	}); err != nil {
		t.Fatal(err)
	}
	routes, err := f.q.RoutesFor(ctx, orgID)
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes %v %v", routes, err)
	}
	id, _, err := f.q.UpsertLead(ctx, leadstore.LeadRow{
		OrgID: orgID, Kind: leadstore.LeadD10, ProbeID: &probeID, Channel: leadstore.ChannelForm, RouteID: &routes[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// manualBatch drafts a form text (step 0) and a LinkedIn note (step 1) for lead, approved.
func (f outreachFixture) manualBatch(t *testing.T, shadow bool, lead int64) []leadstore.Message {
	t.Helper()
	ctx := context.Background()
	drafts := []leadstore.DraftRow{
		{LeadID: lead, Step: 0, Channel: "form", Body: "form text", Slots: []byte(`{}`), MailboxID: f.mailbox(t)},
		{LeadID: lead, Step: 1, Channel: "linkedin", Body: "note", Slots: []byte(`{}`)},
	}
	id, sum, err := f.q.CreateBatch(ctx, shadow, drafts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.ApproveBatch(ctx, id, hex.EncodeToString(sum)[:12], nil, nil); err != nil {
		t.Fatal(err)
	}
	ms, err := f.q.BatchMessages(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestOrgsForBuild(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	site := f.siteLead(t, "northwind.localhost", intp(5), 7, "info@northwind.localhost")
	agency := f.agencyLead(t, "wingtip.localhost", "")
	if _, err := f.owner.Exec(ctx, `INSERT INTO org (kind, domain, source) VALUES ('selftest', 'selftest-1', 'selftest')`); err != nil {
		t.Fatal(err)
	}
	orgs, err := f.q.OrgsForBuild(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 2 {
		t.Fatalf("orgs = %+v, want the site owner and the agency (no selftest)", orgs)
	}
	sl, _ := f.q.LeadByID(ctx, site)
	al, _ := f.q.LeadByID(ctx, agency)
	got := map[string]leadstore.BuildOrg{orgs[0].Domain: orgs[0], orgs[1].Domain: orgs[1]}
	s := got["northwind.localhost"]
	if s.ProbeID == nil || *s.ProbeID != *sl.ProbeID || !s.IsDrupal || s.Major != 7 || s.AgencyFindingID != nil || s.Kind != leadstore.OrgSiteOwner {
		t.Errorf("site org = %+v", s)
	}
	if s.LeadID == nil || *s.LeadID != site || s.LeadState != leadstore.LeadNew || s.LeadKind != leadstore.LeadUnsupported || s.LeadChannel != leadstore.ChannelEmail {
		t.Errorf("site org lead = %+v", s)
	}
	a := got["wingtip.localhost"]
	if a.AgencyFindingID == nil || *a.AgencyFindingID != *al.AgencyFindingID || a.ProbeID != nil || a.Kind != leadstore.OrgAgency {
		t.Errorf("agency org = %+v", a)
	}
}

func TestLeadEvidence(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	home := f.fetchRow(t, "https://contoso.localhost/")
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{{Domain: "contoso.localhost"}}); err != nil {
		t.Fatal(err)
	}
	probeID, err := f.q.RecordProbe(ctx, siteID(t, "contoso.localhost"), f.runID, leadstore.ProbeRow{
		Outcome: leadstore.OutcomeOK, FinalHost: "www.contoso.localhost", IsDrupal: true, Major: 10, ExactVersion: "10.6.14",
		Signals:  []leadstore.Signal{{Signal: "x-generator", Value: "10", FetchID: home}, {Signal: "core-asset-v", Value: "10.6.14", FetchID: home}},
		FetchIDs: []int64{home}, DerivedBy: "drupal@1",
	})
	if err != nil {
		t.Fatal(err)
	}
	orgID, _, err := f.q.UpsertSiteOrg(ctx, probeID)
	if err != nil {
		t.Fatal(err)
	}
	site := f.lead(t, leadstore.LeadRow{OrgID: orgID, Kind: leadstore.LeadD10, ProbeID: &probeID}, "")
	agency := f.agencyLead(t, "wingtip.localhost", "")

	ev, err := f.q.LeadEvidence(ctx, []int64{site, agency, 999})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 2 {
		t.Fatalf("evidence for %d leads, want 2", len(ev))
	}
	s := ev[site]
	if s.ProbeID == nil || *s.ProbeID != probeID || s.FinalHost != "www.contoso.localhost" || s.Major != 10 || s.ExactVersion != "10.6.14" || s.ProbedAt.IsZero() {
		t.Errorf("site evidence = %+v", s)
	}
	if len(s.Signals) != 2 || s.Signals[0].Signal.Signal != "x-generator" || s.Signals[1].Value != "10.6.14" {
		t.Errorf("signals = %+v", s.Signals)
	}
	for _, sig := range s.Signals {
		if sig.Fetch.ID != home || sig.Fetch.URL != "https://contoso.localhost/" || sig.Fetch.FetchedAt.IsZero() {
			t.Errorf("signal fetch = %+v", sig.Fetch)
		}
	}
	a := ev[agency]
	if a.AgencyFindingID == nil || a.FindingURL != "https://wingtip.localhost/upgrades" || a.Phrase != "Drupal 7 upgrades" ||
		a.FindingFetch.ID != f.fid || a.FindingFetch.URL != "https://evidence.localhost/" || a.ProbeID != nil || len(a.Signals) != 0 {
		t.Errorf("agency evidence = %+v", a)
	}
	if ev, err := f.q.LeadEvidence(ctx, nil); err != nil || len(ev) != 0 {
		t.Errorf("no leads: %v %v", ev, err)
	}
}

func TestMarkDone(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	live := f.formLead(t, "contoso.localhost")
	shadow := f.formLead(t, "fabrikam.localhost")
	email := f.siteLead(t, "northwind.localhost", nil, 7, "info@northwind.localhost")

	lm := f.manualBatch(t, false, live)
	if f.leadState(t, live) != leadstore.LeadBatched {
		t.Fatalf("live lead %s", f.leadState(t, live))
	}
	d, err := f.q.MarkDone(ctx, lm[0].ID, leadstore.ActorGremy)
	if err != nil {
		t.Fatal(err)
	}
	if d.Shadow || !d.Moved || d.LeadState != leadstore.LeadContacted || d.LeadID != live {
		t.Errorf("live done = %+v", d)
	}
	// The LinkedIn note of the same lead: done, the lead already contacted.
	if d, err = f.q.MarkDone(ctx, lm[1].ID, leadstore.ActorGremy); err != nil || d.Moved || d.LeadState != leadstore.LeadContacted {
		t.Errorf("second item = %+v %v", d, err)
	}
	eqStates(t, "live", f.msgStates(t, lm[0].BatchID, live), "done", "done")
	var actor string
	if err := f.owner.QueryRow(ctx, `SELECT actor FROM lead_event WHERE lead_id = $1 AND state = 'contacted'`, live).Scan(&actor); err != nil || actor != "gremy" {
		t.Errorf("contacted event actor %q %v", actor, err)
	}
	// Done twice is refused.
	if _, err := f.q.MarkDone(ctx, lm[0].ID, leadstore.ActorGremy); !errors.Is(err, leadstore.ErrMessageState) {
		t.Errorf("done twice: %v", err)
	}

	sm := f.manualBatch(t, true, shadow)
	if d, err = f.q.MarkDone(ctx, sm[0].ID, leadstore.ActorGremy); err != nil || !d.Shadow || d.Moved || d.LeadState != leadstore.LeadNew {
		t.Errorf("shadow done = %+v %v", d, err)
	}

	eid, sum, err := f.q.CreateBatch(ctx, false, f.drafts(t, email), nil)
	if err != nil {
		t.Fatal(err)
	}
	em, _ := f.q.BatchMessages(ctx, eid)
	if _, err := f.q.MarkDone(ctx, em[0].ID, leadstore.ActorGremy); !errors.Is(err, leadstore.ErrNotManual) {
		t.Errorf("email item: %v", err)
	}
	if _, err := f.q.ApproveBatch(ctx, eid, hex.EncodeToString(sum)[:12], nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.MarkDone(ctx, 9999, leadstore.ActorGremy); !errors.Is(err, leadstore.ErrMessageNotFound) {
		t.Errorf("missing message: %v", err)
	}
	if _, err := f.q.MarkDone(ctx, sm[1].ID, "someone"); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("bad actor: %v", err)
	}
	// A draft (not approved) form item is refused.
	other := f.formLead(t, "tailspin.localhost")
	did, _, err := f.q.CreateBatch(ctx, false, []leadstore.DraftRow{{LeadID: other, Step: 0, Channel: "form", Body: "x", MailboxID: f.mailbox(t)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dm, _ := f.q.BatchMessages(ctx, did)
	if _, err := f.q.MarkDone(ctx, dm[0].ID, leadstore.ActorGremy); !errors.Is(err, leadstore.ErrMessageState) {
		t.Errorf("draft item: %v", err)
	}
}

func TestSuppressionCounts(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	if c, err := f.q.SuppressionCounts(ctx, 0); err != nil || len(c) != 0 {
		t.Fatalf("empty: %v %v", c, err)
	}
	for _, s := range [][3]string{
		{leadstore.SuppressAddress, "a@one.localhost", leadstore.ReasonOptOut},
		{leadstore.SuppressAddress, "b@one.localhost", leadstore.ReasonOptOut},
		{leadstore.SuppressAddress, "c@one.localhost", leadstore.ReasonBounce},
		{leadstore.SuppressDomain, "two.localhost", leadstore.ReasonManual},
	} {
		if _, err := f.q.Suppress(ctx, 0, s[0], s[1], s[2], "test"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.q.SuppressionCounts(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []leadstore.SuppressionCount{
		{Kind: "address", Reason: "bounce", N: 1}, {Kind: "address", Reason: "opt_out", N: 2}, {Kind: "domain", Reason: "manual", N: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("counts = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("count %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
