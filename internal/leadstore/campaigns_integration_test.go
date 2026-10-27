//go:build integration

package leadstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

func specSHA(spec string) string {
	sum := sha256.Sum256([]byte(spec))
	return hex.EncodeToString(sum[:])
}

// register registers campaign slug of account with spec and returns its id.
func register(t *testing.T, q leadstore.Queries, account int64, slug, spec string) int64 {
	t.Helper()
	id, _, err := q.RegisterCampaign(context.Background(), account, slug, "Campaign "+slug, []byte(spec), specSHA(spec),
		leadstore.CampaignFromFile, leadstore.ActorOperator)
	if err != nil {
		t.Fatalf("register %s: %v", slug, err)
	}
	return id
}

// secondAccount makes account 2 (the app role cannot, by design of the tests only: INSERT
// is granted, so the owner is used to keep the fixture plain).
func (f outreachFixture) secondAccount(t *testing.T) int64 {
	t.Helper()
	return ownerInt(t, f.owner, `INSERT INTO account (slug, name) VALUES ('second', 'Second') RETURNING id`)
}

// leadIn upserts org's lead in campaign, on the org's first route (channel email) when it has
// one, else channel none.
func (f outreachFixture) leadIn(t *testing.T, campaign, orgID int64, kind string, probeID *int64) int64 {
	t.Helper()
	ctx := context.Background()
	row := leadstore.LeadRow{CampaignID: campaign, OrgID: orgID, Kind: kind, ProbeID: probeID, Channel: leadstore.ChannelNone}
	routes, err := f.q.RoutesFor(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) > 0 {
		row.RouteID, row.Channel = &routes[0].ID, leadstore.ChannelEmail
	}
	id, created, err := f.q.UpsertLead(ctx, row)
	if err != nil || !created {
		t.Fatalf("lead in campaign %d: %d %v %v", campaign, id, created, err)
	}
	return id
}

func (f outreachFixture) lead1(t *testing.T, id int64) leadstore.Lead {
	t.Helper()
	l, err := f.q.LeadByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRegisterCampaignVersions(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	q := leadstoretest.Q(t)

	const v1, v2 = `{"slug": "cafes-loyalty", "v": 1}`, `{"slug": "cafes-loyalty", "v": 2}`
	reg := func(spec string) (int64, bool) {
		t.Helper()
		id, wrote, err := q.RegisterCampaign(ctx, 0, "cafes-loyalty", "Cafés", []byte(spec), specSHA(spec),
			leadstore.CampaignFromFile, leadstore.ActorOperator)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		return id, wrote
	}
	id, wrote := reg(v1)
	if id != 2 || !wrote {
		t.Fatalf("first register = %d %v, want campaign 2 (1 is the seeded drupal-eol) and a version", id, wrote)
	}
	if again, wrote := reg(v1); again != id || wrote {
		t.Fatalf("same spec again = %d %v, want %d and no version", again, wrote, id)
	}
	if again, wrote := reg(v2); again != id || !wrote {
		t.Fatalf("changed spec = %d %v, want %d and a version", again, wrote, id)
	}
	if again, wrote := reg(v1); again != id || !wrote {
		t.Fatalf("back to the first spec = %d %v, want a version (the latest differs)", again, wrote)
	}
	var versions int
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT count(*) FROM campaign_version WHERE campaign_id = $1`, id).Scan(&versions); err != nil || versions != 3 {
		t.Fatalf("versions = %d (%v), want 3", versions, err)
	}

	cs, err := q.Campaigns(ctx, 0)
	if err != nil || len(cs) != 2 {
		t.Fatalf("campaigns = %+v (%v), want drupal-eol and cafes-loyalty", cs, err)
	}
	if cs[0].Slug != leadstore.DefaultCampaignSlug || cs[0].ID != leadstore.DefaultCampaignID || cs[0].VersionAt != nil || cs[0].Spec != nil || cs[0].SpecSHA256 != "" {
		t.Errorf("campaign 1 = %+v, want drupal-eol with no version yet", cs[0])
	}
	if c := cs[1]; c.Slug != "cafes-loyalty" || c.Name != "Cafés" || c.AccountID != 1 || c.SpecSHA256 != specSHA(v1) || c.VersionAt == nil || c.RetiredAt != nil {
		t.Errorf("campaign 2 = %+v, want cafes-loyalty at the v1 sha", c)
	}
	c, err := q.CampaignBySlug(ctx, 0, "cafes-loyalty")
	if err != nil || c.ID != id || string(c.Spec) == "" {
		t.Fatalf("by slug = %+v %v", c, err)
	}
	if _, err := q.CampaignBySlug(ctx, 0, "nope"); !errors.Is(err, leadstore.ErrCampaignNotFound) {
		t.Errorf("missing slug: %v, want ErrCampaignNotFound", err)
	}
	// Another account's campaigns are its own.
	if cs, err := q.Campaigns(ctx, 7); err != nil || len(cs) != 0 {
		t.Errorf("account 7's campaigns = %+v %v, want none", cs, err)
	}

	for name, c := range map[string]struct {
		slug, spec, sha, source, actor string
		want                           error
	}{
		"bad slug":   {"Cafés!", v1, specSHA(v1), "file", "operator", leadstore.ErrBadCampaign},
		"array spec": {"ok-slug", `[1]`, specSHA(`[1]`), "file", "operator", leadstore.ErrBadCampaign},
		"bad sha":    {"ok-slug", v1, "abc", "file", "operator", leadstore.ErrBadCampaign},
		"bad source": {"ok-slug", v1, specSHA(v1), "web", "operator", leadstore.ErrBadCampaign},
		"bad actor":  {"ok-slug", v1, specSHA(v1), "file", "gremy", leadstore.ErrUnknownActor},
	} {
		if _, _, err := q.RegisterCampaign(ctx, 0, c.slug, "Name", []byte(c.spec), c.sha, c.source, c.actor); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}

	// The app role retires a campaign (retired_at is its one pointer).
	if _, err := storetest.AppConn(t).Exec(ctx, `UPDATE campaign SET retired_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("retire as app: %v", err)
	}
	if c, _ := q.CampaignBySlug(ctx, 0, "cafes-loyalty"); c.RetiredAt == nil {
		t.Error("retired campaign has no retired_at")
	}
}

// TestSameOrgTwoCampaigns: one business is a lead in two campaigns, each lead its own; the
// per-campaign readers and writers touch only theirs.
func TestSameOrgTwoCampaigns(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	c2 := register(t, f.q, 0, "second-campaign", `{"slug": "second-campaign"}`)

	a := f.siteLead(t, "acme.localhost", intp(10), 7, "info@acme.localhost")
	la := f.lead1(t, a)
	if la.CampaignID != leadstore.DefaultCampaignID {
		t.Fatalf("a lead with no campaign is in campaign %d, want 1", la.CampaignID)
	}
	b := f.leadIn(t, c2, la.OrgID, la.Kind, la.ProbeID)
	if b == a || f.lead1(t, b).CampaignID != c2 {
		t.Fatalf("campaign 2's lead = %d (campaign %d), want a new lead in %d", b, f.lead1(t, b).CampaignID, c2)
	}
	// Upserting again in either campaign finds that campaign's lead.
	for camp, want := range map[int64]int64{0: a, 1: a, c2: b} {
		id, created, err := f.q.UpsertLead(ctx, leadstore.LeadRow{CampaignID: camp, OrgID: la.OrgID, Kind: la.Kind, ProbeID: la.ProbeID, Channel: la.Channel, RouteID: la.RouteID})
		if err != nil || created || id != want {
			t.Errorf("upsert again in campaign %d = %d %v %v, want %d, not created", camp, id, created, err, want)
		}
	}

	// The build reads each campaign's own lead of the org.
	for camp, want := range map[int64]int64{0: a, c2: b} {
		orgs, err := f.q.OrgsForBuild(ctx, camp)
		if err != nil || len(orgs) != 1 || orgs[0].LeadID == nil || *orgs[0].LeadID != want {
			t.Errorf("orgs for build of campaign %d = %+v %v, want lead %d", camp, orgs, err, want)
		}
	}
	// Enrichment reads the org once.
	if orgs, err := f.q.OrgsNeedingEnrich(ctx, 0, dueBase); err != nil || len(orgs) != 1 {
		t.Errorf("orgs needing enrich = %d %v, want the one org once", len(orgs), err)
	}

	// Candidates: per campaign, or every campaign of the account.
	cands := func(camp int64) []int64 {
		t.Helper()
		cs, err := f.q.CandidateLeads(ctx, leadstore.CandidateOptions{CampaignID: camp, IncludeReserved: true})
		if err != nil {
			t.Fatal(err)
		}
		ids := []int64{}
		for _, c := range cs {
			ids = append(ids, c.LeadID)
		}
		slices.Sort(ids)
		return ids
	}
	if got := cands(c2); !slices.Equal(got, []int64{b}) {
		t.Errorf("campaign 2 candidates = %v, want [%d]", got, b)
	}
	if got := cands(1); !slices.Equal(got, []int64{a}) {
		t.Errorf("campaign 1 candidates = %v, want [%d]", got, a)
	}
	if got := cands(0); !slices.Equal(got, []int64{a, b}) {
		t.Errorf("all candidates = %v, want [%d %d]", got, a, b)
	}

	// The list filters by campaign and names each lead's.
	rows, total, err := f.q.LeadList(ctx, leadstore.ListFilter{CampaignID: c2}, leadstore.ListContext{})
	if err != nil || total != 1 || rows[0].LeadID != b || rows[0].CampaignSlug != "second-campaign" || rows[0].CampaignID != c2 {
		t.Errorf("list of campaign 2 = %+v %d %v", rows, total, err)
	}
	if _, total, _ := f.q.LeadList(ctx, leadstore.ListFilter{}, leadstore.ListContext{}); total != 2 {
		t.Errorf("list of every campaign = %d leads, want 2", total)
	}

	// Refresh moves only the campaign named.
	changed, err := f.q.RefreshLead(ctx, leadstore.LeadRow{CampaignID: c2, OrgID: la.OrgID, Kind: la.Kind, ProbeID: la.ProbeID, Channel: leadstore.ChannelNone})
	if err != nil || !changed {
		t.Fatalf("refresh campaign 2: %v %v", changed, err)
	}
	if f.lead1(t, b).Channel != leadstore.ChannelNone || f.lead1(t, a).Channel != leadstore.ChannelEmail {
		t.Error("refresh of campaign 2 touched campaign 1's lead, or missed its own")
	}

	// A hand mark is per campaign: campaign 2's lead moves, campaign 1's stays new.
	res, err := f.q.MarkContacted(ctx, c2, "acme.localhost", "met at an event")
	if err != nil || res.LeadID != b || !res.Moved {
		t.Fatalf("mark contacted in campaign 2 = %+v %v, want lead %d moved", res, err, b)
	}
	if s := f.leadState(t, a); s != leadstore.LeadNew {
		t.Errorf("campaign 1's lead after campaign 2's hand mark = %s, want new", s)
	}
	// The reached-domain rule is account-wide: campaign 2's contact blocks campaign 1's lead.
	if got := cands(1); len(got) != 0 {
		t.Errorf("campaign 1 candidates after campaign 2 reached the domain = %v, want none", got)
	}
}

// TestOptOutAccountWide: a suppression recorded for account 1 blocks every campaign of that
// account, and no other account.
func TestOptOutAccountWide(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	c2 := register(t, f.q, 0, "second-campaign", `{"slug": "second-campaign"}`)
	acct2 := f.secondAccount(t)
	c3 := register(t, f.q, acct2, "other-account", `{"slug": "other-account"}`)

	a := f.siteLead(t, "acme.localhost", intp(10), 7, "info@acme.localhost") // campaign 1
	la := f.lead1(t, a)
	b := f.leadIn(t, c2, la.OrgID, la.Kind, la.ProbeID) // campaign 2, same org and address
	c := f.leadIn(t, c3, la.OrgID, la.Kind, la.ProbeID) // account 2

	// The opt-out came from campaign 1's lead: account 1's suppression.
	res, err := f.q.Suppress(ctx, leadstore.DefaultAccountID, leadstore.SuppressAddress, "info@acme.localhost", leadstore.ReasonOptOut, "mailbox:1:INBOX:7")
	if err != nil || !res.Recorded || res.Leads != 2 || res.Moved != 2 {
		t.Fatalf("suppress = %+v %v, want both account 1 leads reached and moved", res, err)
	}
	if f.leadState(t, a) != leadstore.LeadSuppressed || f.leadState(t, b) != leadstore.LeadSuppressed {
		t.Errorf("account 1 leads = %s %s, want suppressed", f.leadState(t, a), f.leadState(t, b))
	}
	if s := f.leadState(t, c); s != leadstore.LeadNew {
		t.Errorf("account 2's lead = %s, want new: another account's opt-out does not reach it", s)
	}
	if set, _ := f.q.SuppressionSet(ctx, 1); !slices.Equal(set.Addresses, []string{"info@acme.localhost"}) {
		t.Errorf("account 1 suppressions = %+v", set)
	}
	if set, _ := f.q.SuppressionSet(ctx, acct2); len(set.Addresses)+len(set.Domains) != 0 {
		t.Errorf("account 2 suppressions = %+v, want none", set)
	}
	if n, _ := f.q.SuppressionCounts(ctx, acct2); len(n) != 0 {
		t.Errorf("account 2 counts = %+v, want none", n)
	}
	cs, err := f.q.CandidateLeads(ctx, leadstore.CandidateOptions{AccountID: acct2, IncludeReserved: true})
	if err != nil || len(cs) != 1 || cs[0].LeadID != c {
		t.Errorf("account 2 candidates = %+v %v, want lead %d", cs, err, c)
	}
	// The same opt-out again for account 1 records nothing; for account 2 it is its own row.
	if res, _ := f.q.Suppress(ctx, 1, leadstore.SuppressAddress, "info@acme.localhost", leadstore.ReasonOptOut, "cli"); res.Recorded {
		t.Error("the same suppression recorded twice for account 1")
	}

	// A domain suppression of account 1 on a domain only campaign 2 has a lead on: the
	// draft-time check (leadCheck) refuses that lead and lets account 2's through. The rows
	// are written as the owner, so no lead is moved and the check itself is what refuses.
	g := f.siteLead(t, "gamma.localhost", intp(20), 7, "hi@gamma.localhost")
	lg := f.lead1(t, g)
	g2 := f.leadIn(t, c2, lg.OrgID, lg.Kind, lg.ProbeID)
	g3 := f.leadIn(t, c3, lg.OrgID, lg.Kind, lg.ProbeID)
	ownerRun(t, f.owner, `INSERT INTO suppression (kind, value, reason, source, account_id) VALUES ('domain', 'gamma.localhost', 'manual', 'cli', 1)`)
	if _, _, err := f.q.CreateBatch(ctx, true, f.drafts(t, g2), []byte("t")); !errors.Is(err, leadstore.ErrNotDraftable) {
		t.Errorf("draft campaign 2's lead on a domain account 1 suppressed: %v, want ErrNotDraftable", err)
	}
	if _, _, err := f.q.CreateBatch(ctx, true, f.drafts(t, g3), []byte("t")); err != nil {
		t.Errorf("draft account 2's lead on that domain: %v, want drafted", err)
	}

	// A lift counts and re-checks within the suppression's own account.
	rows, _, err := f.q.SuppressionRows(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var addrID int64
	for _, r := range rows {
		if r.Kind == leadstore.SuppressAddress {
			addrID = r.ID
		}
	}
	lifted, err := f.q.LiftSuppression(ctx, addrID, "acme.localhost", "recorded by mistake", leadstore.LiftFromCLI)
	if err != nil || lifted.LeadsSuppressed != 2 {
		t.Errorf("lift = %+v %v, want account 1's 2 suppressed leads counted", lifted, err)
	}
}

// TestOpenElsewhere: one business, one conversation, over every state that counts.
func TestOpenElsewhere(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	c2 := register(t, f.q, 0, "second-campaign", `{"slug": "second-campaign"}`)
	acct2 := f.secondAccount(t)
	c3 := register(t, f.q, acct2, "other-account", `{"slug": "other-account"}`)

	org := func(lead int64) int64 { return f.lead1(t, lead).OrgID }
	setMsgs := func(lead int64, state string) {
		ownerRun(t, f.owner, `UPDATE outreach_message SET state = $2 WHERE lead_id = $1`, lead, state)
	}
	// Every scenario's lead is in campaign 1 unless named.
	approved := f.siteLead(t, "approved.localhost", nil, 7, "a@approved.localhost")
	id, h := f.batch(t, false, approved)
	f.approve(t, id, h)
	sending := f.siteLead(t, "sending.localhost", nil, 7, "a@sending.localhost")
	id, h = f.batch(t, false, sending)
	f.approve(t, id, h)
	setMsgs(sending, "sending")
	maybe := f.siteLead(t, "maybe.localhost", nil, 7, "a@maybe.localhost")
	id, h = f.batch(t, false, maybe)
	f.approve(t, id, h)
	setMsgs(maybe, "maybe_sent")
	sent := f.siteLead(t, "sent.localhost", nil, 7, "a@sent.localhost")
	id, h = f.batch(t, false, sent)
	f.approve(t, id, h)
	setMsgs(sent, "sent")
	shadow := f.siteLead(t, "shadow.localhost", nil, 7, "a@shadow.localhost")
	id, h = f.batch(t, true, shadow)
	f.approve(t, id, h)
	replied := f.siteLead(t, "replied.localhost", nil, 7, "a@replied.localhost")
	f.setState(t, replied, leadstore.LeadReplied)
	fresh := f.siteLead(t, "fresh.localhost", nil, 7, "a@fresh.localhost")
	// Account 2's open sequence never blocks account 1.
	other := f.siteLead(t, "other.localhost", nil, 7, "a@other.localhost")
	ownerRun(t, f.owner, `UPDATE lead SET campaign_id = $2 WHERE id = $1`, other, c3)
	id, h = f.batch(t, false, other)
	f.approve(t, id, h)

	orgs := []int64{org(approved), org(sending), org(maybe), org(sent), org(shadow), org(replied), org(fresh), org(other)}
	got, err := f.q.OpenElsewhere(ctx, 0, c2, orgs)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{
		org(approved): "open sequence in campaign drupal-eol",
		org(sending):  "open sequence in campaign drupal-eol",
		org(maybe):    "open sequence in campaign drupal-eol",
		org(replied):  "replied in campaign drupal-eol",
	}
	if len(got) != len(want) {
		t.Errorf("open elsewhere = %v, want %v", got, want)
	}
	for o, w := range want {
		if got[o] != w {
			t.Errorf("org %d: %q, want %q", o, got[o], w)
		}
	}
	// From campaign 1 itself nothing is elsewhere; from account 2, account 1 never counts.
	if got, _ := f.q.OpenElsewhere(ctx, 0, 1, orgs); len(got) != 0 {
		t.Errorf("from campaign 1 = %v, want none", got)
	}
	if got, _ := f.q.OpenElsewhere(ctx, acct2, 0, orgs); len(got) != 1 || got[org(other)] != "open sequence in campaign other-account" {
		t.Errorf("account 2 seen from outside its campaigns = %v, want only other.localhost", got)
	}
	if got, err := f.q.OpenElsewhere(ctx, 0, c2, nil); err != nil || len(got) != 0 {
		t.Errorf("no orgs = %v %v", got, err)
	}
}

// TestMigrateCampaignsKeepsLeads: a database at 00015 with leads and a suppression gets every
// lead in campaign 1 and the suppression in account 1 at 00016; Down narrows back while each
// org has one lead, and refuses (rolled back, nothing changed) once one has two.
func TestMigrateCampaignsKeepsLeads(t *testing.T) {
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	p := migrateLeadsTo(t, owner, 15)
	var leads []int64
	for _, d := range []string{"one.localhost", "two.localhost"} {
		org := ownerInt(t, owner, `INSERT INTO org (kind, domain, source) VALUES ('site_owner', $1, 'probe') RETURNING id`, d)
		leads = append(leads, ownerInt(t, owner, `INSERT INTO lead (org_id, kind, channel) VALUES ($1, 'hand', 'none') RETURNING id`, org))
	}
	ownerRun(t, owner, `INSERT INTO suppression (kind, value, reason) VALUES ('domain', 'one.localhost', 'manual')`)
	if _, err := p.UpTo(ctx, 16); err != nil {
		t.Fatalf("up to 16: %v", err)
	}
	var n int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM lead WHERE campaign_id = 1 AND id = ANY($1)`, leads).Scan(&n); err != nil || n != 2 {
		t.Fatalf("leads in campaign 1 after 00016 = %d (%v), want 2", n, err)
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM suppression_active WHERE account_id = 1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("suppression_active rows of account 1 = %d (%v), want 1 (the view carries account_id)", n, err)
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname IN ('lead_org_id_key', 'lead_campaign_org_key')
		AND conrelid = 'lead'::regclass AND conname = 'lead_campaign_org_key'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("lead_campaign_org_key = %d (%v)", n, err)
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = 'lead_org_id_key'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("lead_org_id_key still there (%d, %v)", n, err)
	}

	// Down while every org has one lead: back to 00015, the leads kept.
	if _, err := p.DownTo(ctx, 15); err != nil {
		t.Fatalf("down to 15: %v", err)
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM lead`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("leads after down = %d (%v)", n, err)
	}
	if _, err := p.UpTo(ctx, 16); err != nil {
		t.Fatalf("up to 16 again: %v", err)
	}
	// One org in two campaigns cannot narrow back to one lead per org.
	camp := ownerInt(t, owner, `INSERT INTO campaign (account_id, slug, name) VALUES (1, 'second', 'Second') RETURNING id`)
	ownerRun(t, owner, `INSERT INTO lead (org_id, kind, channel, campaign_id) SELECT org_id, kind, channel, $2 FROM lead WHERE id = $1`, leads[0], camp)
	if _, err := p.DownTo(ctx, 15); err == nil {
		t.Fatal("down to 15 with an org in two campaigns succeeded, want refused")
	}
	if v, err := p.GetDBVersion(ctx); err != nil || v != 16 {
		t.Fatalf("version after a refused down = %d (%v), want 16 (rolled back)", v, err)
	}
}
