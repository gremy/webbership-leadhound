//go:build integration

package leadstore_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Fixtures live on .localhost (reserved, allowed by privacycheck). CandidateLeads leaves
// every reserved TLD out by default (RA25), so the tests ask with IncludeReserved.

// outreachFixture is a reset database with one run and one evidence fetch.
type outreachFixture struct {
	siteFixture
	owner *pgx.Conn
	fid   int64
}

func newOutreachFixture(t *testing.T) outreachFixture {
	t.Helper()
	f := newSiteFixture(t)
	return outreachFixture{siteFixture: f, owner: storetest.OwnerConn(t), fid: f.fetchRow(t, "https://evidence.localhost/")}
}

// siteLead adds site domain (rank, Drupal major), its org, an email route when email is
// not blank, and its lead. It returns the lead id.
func (f outreachFixture) siteLead(t *testing.T, domain string, rank *int, major int, email string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{{Domain: domain, Rank: rank}}); err != nil {
		t.Fatalf("import %s: %v", domain, err)
	}
	probeID, err := f.q.RecordProbe(ctx, siteID(t, domain), f.runID, leadstore.ProbeRow{
		Outcome: leadstore.OutcomeOK, FinalHost: domain, IsDrupal: true, Major: major,
		FetchIDs: []int64{f.fid}, DerivedBy: "drupal@1",
	})
	if err != nil {
		t.Fatalf("probe %s: %v", domain, err)
	}
	orgID, _, err := f.q.UpsertSiteOrg(ctx, probeID)
	if err != nil {
		t.Fatalf("org %s: %v", domain, err)
	}
	kind := leadstore.LeadUnsupported
	if major == 10 {
		kind = leadstore.LeadD10
	}
	return f.lead(t, leadstore.LeadRow{OrgID: orgID, Kind: kind, ProbeID: &probeID}, email)
}

// agencyLead adds an agency org with one finding, an email route and its lead.
func (f outreachFixture) agencyLead(t *testing.T, domain, email string) int64 {
	t.Helper()
	ctx := context.Background()
	ag, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Agency " + domain, Website: "https://" + domain + "/"}, &f.fid)
	if err != nil {
		t.Fatalf("agency %s: %v", domain, err)
	}
	findingID, err := f.q.InsertAgencyFinding(ctx, leadstore.AgencyFindingRow{
		OrgID: ag.OrgID, URL: "https://" + domain + "/upgrades", Phrase: "Drupal 7 upgrades", FetchID: f.fid, DerivedBy: "agencies@1",
	})
	if err != nil {
		t.Fatalf("finding %s: %v", domain, err)
	}
	return f.lead(t, leadstore.LeadRow{OrgID: ag.OrgID, Kind: leadstore.LeadAgencyPartner, AgencyFindingID: &findingID}, email)
}

// lead adds email as l's route (channel email), or no route (channel none), and upserts l.
func (f outreachFixture) lead(t *testing.T, l leadstore.LeadRow, email string) int64 {
	t.Helper()
	ctx := context.Background()
	l.Channel = leadstore.ChannelNone
	if email != "" {
		if _, err := f.q.InsertRoute(ctx, leadstore.RouteRow{
			OrgID: l.OrgID, Kind: leadstore.RouteEmail, Value: email, FetchID: f.fid, MXOK: true, DerivedBy: "contacts@1",
		}); err != nil {
			t.Fatalf("route %s: %v", email, err)
		}
		routes, err := f.q.RoutesFor(ctx, l.OrgID)
		if err != nil || len(routes) == 0 {
			t.Fatalf("routes: %v", err)
		}
		l.RouteID, l.Channel = &routes[0].ID, leadstore.ChannelEmail
	}
	id, created, err := f.q.UpsertLead(ctx, l)
	if err != nil || !created {
		t.Fatalf("upsert lead: %d %v %v", id, created, err)
	}
	return id
}

// fixtureMailbox is the sending mailbox every fixture draft goes from (mailbox_id is
// required, 00008).
const fixtureMailbox = "outreach@fixture.localhost"

// mailbox returns the id of fixtureMailbox, seeding it on first use.
func (f outreachFixture) mailbox(t *testing.T) int64 {
	t.Helper()
	var id int64
	err := f.owner.QueryRow(context.Background(), `SELECT id FROM sender_mailbox WHERE address = $1`, fixtureMailbox).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return leadstoretest.SeedMailbox(t, f.q, fixtureMailbox).ID
	}
	if err != nil {
		t.Fatalf("fixture mailbox: %v", err)
	}
	return id
}

// drafts returns steps 0 and 1 for every lead, to the lead's route address, from the
// fixture's mailbox.
func (f outreachFixture) drafts(t *testing.T, leadIDs ...int64) []leadstore.DraftRow {
	t.Helper()
	var out []leadstore.DraftRow
	mb := f.mailbox(t)
	for _, id := range leadIDs {
		var to string
		if err := f.owner.QueryRow(context.Background(),
			`SELECT coalesce(r.value, '') FROM lead l LEFT JOIN contact_route r ON r.id = l.route_id WHERE l.id = $1`, id).Scan(&to); err != nil {
			t.Fatalf("lead %d route: %v", id, err)
		}
		for step := range 2 {
			out = append(out, leadstore.DraftRow{
				LeadID: id, Step: step, Channel: "email", To: to,
				Subject: fmt.Sprintf("Drupal upgrade %d", step), Body: fmt.Sprintf("Body %d of lead %d", step, id),
				MessageID: fmt.Sprintf("<%d.%d@shadow.invalid>", id, step),
				Slots:     []byte(`{"major": {"value": "7", "source": "site_probe"}, "domain": {"value": "x"}}`),
				MailboxID: mb,
			})
		}
	}
	return out
}

func (f outreachFixture) batch(t *testing.T, shadow bool, leadIDs ...int64) (int64, string) {
	t.Helper()
	id, sum, err := f.q.CreateBatch(context.Background(), shadow, f.drafts(t, leadIDs...), []byte("templates"))
	if err != nil {
		t.Fatalf("create batch (shadow %v): %v", shadow, err)
	}
	return id, hex.EncodeToString(sum)
}

func (f outreachFixture) approve(t *testing.T, batchID int64, hash string) leadstore.Approval {
	t.Helper()
	a, err := f.q.ApproveBatch(context.Background(), batchID, hash[:12], nil, dueAt)
	if err != nil {
		t.Fatalf("approve batch %d: %v", batchID, err)
	}
	return a
}

var dueBase = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// dueAt puts step n n days after dueBase.
func dueAt(m leadstore.Message) time.Time { return dueBase.AddDate(0, 0, 3*m.Step) }

func (f outreachFixture) leadState(t *testing.T, id int64) string {
	t.Helper()
	var s string
	if err := f.owner.QueryRow(context.Background(), "SELECT state FROM lead WHERE id = $1", id).Scan(&s); err != nil {
		t.Fatalf("lead %d state: %v", id, err)
	}
	return s
}

// msgStates returns lead's message states in batch, by step.
func (f outreachFixture) msgStates(t *testing.T, batchID, leadID int64) []string {
	t.Helper()
	rows, err := f.owner.Query(context.Background(),
		"SELECT state FROM outreach_message WHERE batch_id = $1 AND lead_id = $2 ORDER BY step", batchID, leadID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	s, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	return s
}

func (f outreachFixture) setState(t *testing.T, id int64, state string) {
	t.Helper()
	if err := f.q.SetLeadState(context.Background(), id, state, "test", leadstore.ActorSystem); err != nil {
		t.Fatalf("set lead %d %s: %v", id, state, err)
	}
}

func candidateIDs(t *testing.T, q leadstore.Queries, kinds ...string) []int64 {
	t.Helper()
	cs, err := q.CandidateLeads(context.Background(), leadstore.CandidateOptions{KindsPriority: kinds, IncludeReserved: true})
	if err != nil {
		t.Fatalf("candidate leads: %v", err)
	}
	ids := []int64{}
	for _, c := range cs {
		ids = append(ids, c.LeadID)
	}
	return ids
}

func eqStates(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: messages = %v, want %v", what, got, want)
	}
}

func TestUpsertLeadOnceAndSetLeadState(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	id := f.siteLead(t, "northwind.localhost", intp(5), 7, "info@northwind.localhost")
	var orgID int64
	if err := f.owner.QueryRow(ctx, "SELECT org_id FROM lead WHERE id = $1", id).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	again, created, err := f.q.UpsertLead(ctx, leadstore.LeadRow{OrgID: orgID, Kind: leadstore.LeadHand, Channel: leadstore.ChannelNone})
	if err != nil || created || again != id {
		t.Fatalf("second upsert = %d %v %v, want %d not created", again, created, err, id)
	}
	if err := f.q.SetLeadState(ctx, id, leadstore.LeadClosed, "done", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	if err := f.q.SetLeadState(ctx, 999, leadstore.LeadClosed, "", leadstore.ActorGremy); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("unknown lead: err = %v, want ErrLeadNotFound", err)
	}
	if err := f.q.SetLeadState(ctx, id, "gone", "", leadstore.ActorGremy); !errors.Is(err, leadstore.ErrUnknownLeadState) {
		t.Errorf("unknown state: err = %v", err)
	}
	rows, _ := f.owner.Query(ctx, "SELECT state || ' ' || reason || ' ' || actor FROM lead_event WHERE lead_id = $1 ORDER BY id", id)
	events, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"new created system", "closed done gremy"}; !reflect.DeepEqual(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}
	// A real lead needs exactly one finding (lead_finding_check, BA1 keeps hand).
	_, _, err = f.q.UpsertLead(ctx, leadstore.LeadRow{OrgID: orgID + 100, Kind: leadstore.LeadD10, Channel: leadstore.ChannelNone})
	if sqlState(err) != "23514" && sqlState(err) != "23503" {
		t.Errorf("finding-less d10 lead: err = %v, want a check or FK violation", err)
	}
}

func TestLeadFindingCheck(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	ag, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "A", Website: "https://a.localhost/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.q.UpsertLead(ctx, leadstore.LeadRow{OrgID: ag.OrgID, Kind: leadstore.LeadAgencyPartner, Channel: leadstore.ChannelNone})
	if sqlState(err) != "23514" {
		t.Errorf("agency lead without a finding: err = %v, want 23514", err)
	}
	if _, _, err := f.q.UpsertLead(ctx, leadstore.LeadRow{OrgID: ag.OrgID, Kind: leadstore.LeadHand, Channel: leadstore.ChannelNone}); err != nil {
		t.Errorf("hand lead without a finding: %v", err)
	}
}

func TestCandidateLeadsFilterAndOrder(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	d10 := f.siteLead(t, "tenth.localhost", intp(1), 10, "info@tenth.localhost")
	late := f.siteLead(t, "late.localhost", nil, 7, "info@late.localhost")
	early := f.siteLead(t, "early.localhost", intp(3), 7, "info@early.localhost")
	agency := f.agencyLead(t, "agency.localhost", "hello@agency.localhost")
	f.siteLead(t, "noroute.localhost", intp(2), 7, "")                                      // channel none
	fixture := f.siteLead(t, "fixture.example", intp(2), 7, "info@fixture.example")         // reserved, let through by IncludeReserved
	byAddr := f.siteLead(t, "byaddr.localhost", intp(2), 7, "info@byaddr.localhost")        // address suppressed
	byDom := f.siteLead(t, "shop.bydom.localhost", intp(2), 7, "info@shop.bydom.localhost") // parent domain suppressed
	// A site whose route is at a reached agency's domain.
	client := f.siteLead(t, "client.localhost", intp(2), 7, "web@reached.localhost")
	reached := f.agencyLead(t, "reached.localhost", "hello@reached.localhost")
	f.setState(t, reached, leadstore.LeadContacted)
	// A contacted lead itself is no candidate.
	done := f.siteLead(t, "done.localhost", intp(2), 7, "info@done.localhost")
	f.setState(t, done, leadstore.LeadContacted)
	// selftest orgs never come up.
	if _, err := f.owner.Exec(ctx, "INSERT INTO org (kind, domain, source) VALUES ('selftest', 'selftest-1', 'selftest')"); err != nil {
		t.Fatal(err)
	}
	var stOrg int64
	if err := f.owner.QueryRow(ctx, "SELECT id FROM org WHERE domain = 'selftest-1'").Scan(&stOrg); err != nil {
		t.Fatal(err)
	}
	f.lead(t, leadstore.LeadRow{OrgID: stOrg, Kind: leadstore.LeadSelftest}, "me@self.localhost")

	for _, s := range [][3]string{
		{leadstore.SuppressAddress, "INFO@byaddr.localhost", leadstore.ReasonOptOut},
		{leadstore.SuppressDomain, "bydom.localhost", leadstore.ReasonManual},
	} {
		if _, err := f.q.Suppress(ctx, s[0], s[1], s[2], "cli"); err != nil {
			t.Fatal(err)
		}
	}
	_ = byAddr
	_ = byDom
	_ = client

	got := candidateIDs(t, f.q, leadstore.LeadUnsupported, leadstore.LeadAgencyPartner)
	if want := []int64{fixture, early, late, agency, d10}; !reflect.DeepEqual(got, want) {
		t.Errorf("candidates = %v, want %v (unsupported by rank, unranked last; then agency; then unlisted kinds)", got, want)
	}
	if got := candidateIDs(t, f.q); !reflect.DeepEqual(got, []int64{d10, fixture, early, late, agency}) {
		t.Errorf("candidates with no priority = %v, want by rank", got)
	}
	cs, err := f.q.CandidateLeads(ctx, leadstore.CandidateOptions{Limit: 1, KindsPriority: []string{leadstore.LeadAgencyPartner}, IncludeReserved: true})
	if err != nil || len(cs) != 1 || cs[0].LeadID != agency || cs[0].Phrase != "Drupal 7 upgrades" || cs[0].Route != "hello@agency.localhost" {
		t.Errorf("limit 1 = %+v, %v", cs, err)
	}
}

func TestBatchNewGrid(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		t.Run(fmt.Sprintf("shadow=%v", shadow), func(t *testing.T) {
			f := newOutreachFixture(t)
			ctx := context.Background()
			lead := f.siteLead(t, "northwind.localhost", intp(1), 7, "info@northwind.localhost")
			// new → drafted.
			id, sum := f.batch(t, shadow, lead)
			eqStates(t, "new lead", f.msgStates(t, id, lead), "draft", "draft")
			b, err := f.q.BatchByID(ctx, id)
			if err != nil || b.Shadow != shadow || hex.EncodeToString(b.ContentSHA256) != sum {
				t.Fatalf("batch = %+v, %v", b, err)
			}
			want := leadstore.LeadBatched
			if shadow {
				want = leadstore.LeadNew // RA1: a shadow batch never consumes a lead
			}
			if got := f.leadState(t, lead); got != want {
				t.Fatalf("lead after batch new = %s, want %s", got, want)
			}
			cands := candidateIDs(t, f.q)
			if shadow != (len(cands) == 1) {
				t.Errorf("candidates after batch = %v (shadow %v)", cands, shadow)
			}
			// Every state but new is never (re-)drafted.
			for _, st := range []string{leadstore.LeadBatched, leadstore.LeadContacted, leadstore.LeadReplied, leadstore.LeadBounced,
				leadstore.LeadOptedOut, leadstore.LeadSuppressed, leadstore.LeadSkipped} {
				f.setState(t, lead, st)
				if _, _, err := f.q.CreateBatch(ctx, shadow, f.drafts(t, lead), nil); !errors.Is(err, leadstore.ErrNotDraftable) {
					t.Errorf("batch new on a %s lead: err = %v, want ErrNotDraftable", st, err)
				}
			}
			// A suppression is checked at draft time even when the lead is still new.
			other := f.siteLead(t, "contoso.localhost", intp(2), 7, "info@contoso.localhost")
			drafts := f.drafts(t, other)
			if _, err := f.owner.Exec(ctx, "INSERT INTO suppression (kind, value, reason) VALUES ('address', 'info@contoso.localhost', 'bounce')"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := f.q.CreateBatch(ctx, shadow, drafts, nil); !errors.Is(err, leadstore.ErrNotDraftable) {
				t.Errorf("batch new on a suppressed address: err = %v, want ErrNotDraftable", err)
			}
			var batches int
			if err := f.owner.QueryRow(ctx, "SELECT count(*) FROM outreach_batch").Scan(&batches); err != nil || batches != 1 {
				t.Errorf("batches = %d, %v; refused batches must write nothing", batches, err)
			}
			if _, _, err := f.q.CreateBatch(ctx, shadow, nil, nil); !errors.Is(err, leadstore.ErrEmptyBatch) {
				t.Errorf("empty batch: err = %v", err)
			}
		})
	}
}

func TestContentHashCanonical(t *testing.T) {
	a := []leadstore.DraftRow{
		{LeadID: 2, Step: 0, Body: "b", Slots: []byte(`{"b":1,"a":2}`)},
		{LeadID: 1, Step: 1, Body: "a"},
	}
	b := []leadstore.DraftRow{
		{LeadID: 1, Step: 1, Body: "a", Slots: []byte(`{}`)},
		{LeadID: 2, Step: 0, Body: "b", Slots: []byte(` {"a": 2, "b": 1}`)},
	}
	ha, err := leadstore.ContentSHA256(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := leadstore.ContentSHA256(b)
	if !bytes.Equal(ha, hb) {
		t.Error("the same drafts in another order hash differently")
	}
	b[0].Body = "changed"
	if hc, _ := leadstore.ContentSHA256(b); bytes.Equal(ha, hc) {
		t.Error("a changed body kept the hash")
	}
	if _, err := leadstore.ContentSHA256(append(a, a[0])); !errors.Is(err, leadstore.ErrDuplicateDraft) {
		t.Errorf("duplicate draft: err = %v", err)
	}
}

func TestApproveGrid(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		t.Run(fmt.Sprintf("shadow=%v", shadow), func(t *testing.T) {
			f := newOutreachFixture(t)
			ctx := context.Background()
			ok := f.siteLead(t, "ok.localhost", intp(1), 7, "info@ok.localhost")
			skipMe := f.siteLead(t, "skipme.localhost", intp(2), 7, "info@skipme.localhost")
			replied := f.siteLead(t, "replied.localhost", intp(3), 7, "info@replied.localhost")
			bounced := f.siteLead(t, "bounced.localhost", intp(4), 7, "info@bounced.localhost")
			optedOut := f.siteLead(t, "optedout.localhost", intp(5), 7, "info@optedout.localhost")
			suppressed := f.siteLead(t, "suppressed.localhost", intp(6), 7, "info@suppressed.localhost")
			id, sum := f.batch(t, shadow, ok, skipMe, replied, bounced, optedOut, suppressed)

			// A wrong or short --hash is refused and changes nothing (RA2).
			wrong := "000000000000"
			if sum[:12] == wrong {
				wrong = "111111111111"
			}
			if _, err := f.q.ApproveBatch(ctx, id, wrong, nil, dueAt); !errors.Is(err, leadstore.ErrHashMismatch) {
				t.Fatalf("wrong hash: err = %v, want ErrHashMismatch", err)
			}
			for _, h := range []string{sum[:11], "zzzzzzzzzzzz", ""} {
				if _, err := f.q.ApproveBatch(ctx, id, h, nil, dueAt); !errors.Is(err, leadstore.ErrHashTooShort) {
					t.Errorf("hash %q: err = %v, want ErrHashTooShort", h, err)
				}
			}
			if _, err := f.q.ApproveBatch(ctx, id, sum[:12], []int{7}, dueAt); !errors.Is(err, leadstore.ErrBadSkip) {
				t.Errorf("skip 7 of 6: err = %v, want ErrBadSkip", err)
			}
			if b, _ := f.q.BatchByID(ctx, id); b.ApprovedAt != nil {
				t.Fatal("a refused approve approved the batch")
			}

			// The lead moves on between batch new and approve.
			f.setState(t, replied, leadstore.LeadReplied)
			f.setState(t, bounced, leadstore.LeadBounced)
			f.setState(t, optedOut, leadstore.LeadOptedOut)
			// A suppression written straight into the table (no Suppress effects) is still
			// caught by approve's re-check.
			if _, err := f.owner.Exec(ctx, "INSERT INTO suppression (kind, value, reason) VALUES ('domain', 'suppressed.localhost', 'manual')"); err != nil {
				t.Fatal(err)
			}

			a, err := f.q.ApproveBatch(ctx, id, sum[:16], []int{2}, dueAt) // item 2 = skipMe
			if err != nil {
				t.Fatalf("approve: %v", err)
			}
			if a.Approved != 2 || a.Skipped != 10 || a.Already {
				t.Errorf("approval = %+v, want 2 approved, 10 skipped", a)
			}
			eqStates(t, "ok", f.msgStates(t, id, ok), "approved", "approved")
			for _, l := range []int64{skipMe, replied, bounced, optedOut, suppressed} {
				eqStates(t, fmt.Sprint("lead ", l), f.msgStates(t, id, l), "skipped", "skipped")
			}
			b, _ := f.q.BatchByID(ctx, id)
			if b.ApprovedAt == nil || !bytes.Equal(b.ApprovedHash, b.ContentSHA256) {
				t.Errorf("batch after approve = %+v, want approved_hash = content_sha256", b)
			}
			ms, _ := f.q.BatchMessages(ctx, id)
			for _, m := range ms {
				if m.LeadID != ok {
					continue
				}
				if !bytes.Equal(m.ApprovedSHA256, leadstore.MessageSHA256(m.ToAddress, m.Subject, m.BodyText, m.InReplyTo)) {
					t.Errorf("message %d approved_sha256 does not bind its text", m.ID)
				}
				if m.DueAt == nil || !m.DueAt.Equal(dueAt(m)) {
					t.Errorf("message %d due_at = %v, want %v", m.ID, m.DueAt, dueAt(m))
				}
			}
			wantSkip, wantOK := leadstore.LeadSkipped, leadstore.LeadBatched
			if shadow {
				wantSkip, wantOK = leadstore.LeadNew, leadstore.LeadNew
			}
			if got := f.leadState(t, skipMe); got != wantSkip {
				t.Errorf("skipped lead = %s, want %s", got, wantSkip)
			}
			if got := f.leadState(t, ok); got != wantOK {
				t.Errorf("approved lead = %s, want %s", got, wantOK)
			}
			if got := f.leadState(t, replied); got != leadstore.LeadReplied {
				t.Errorf("replied lead = %s, want replied", got)
			}

			// Approving again is a no-op, and a wrong hash is still refused.
			if a2 := f.approve(t, id, sum); !a2.Already {
				t.Errorf("second approve = %+v, want Already", a2)
			}
			if _, err := f.q.ApproveBatch(ctx, id, wrong, nil, dueAt); !errors.Is(err, leadstore.ErrHashMismatch) {
				t.Errorf("wrong hash on an approved batch: err = %v", err)
			}
		})
	}
}

func TestSuppressGrid(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		t.Run(fmt.Sprintf("shadow=%v", shadow), func(t *testing.T) {
			f := newOutreachFixture(t)
			ctx := context.Background()
			newLead := f.siteLead(t, "new.localhost", intp(1), 7, "info@new.localhost")
			batched := f.siteLead(t, "batched.localhost", intp(2), 7, "info@batched.localhost")
			contacted := f.siteLead(t, "contacted.localhost", intp(3), 7, "info@contacted.localhost")
			replied := f.siteLead(t, "replied.localhost", intp(4), 7, "info@replied.localhost")
			bounced := f.siteLead(t, "bounced.localhost", intp(5), 7, "info@bounced.localhost")
			skipped := f.siteLead(t, "skipped.localhost", intp(6), 7, "info@skipped.localhost")
			id, sum := f.batch(t, shadow, batched, contacted)
			f.approve(t, id, sum)
			f.setState(t, contacted, leadstore.LeadContacted)
			f.setState(t, replied, leadstore.LeadReplied)
			f.setState(t, bounced, leadstore.LeadBounced)
			f.setState(t, skipped, leadstore.LeadSkipped)

			type cell struct {
				lead                int64
				kind, value, reason string
				wantState           string
				wantMoved           int
				wantCancelled       int64
			}
			batchedState := leadstore.LeadSuppressed
			wantBatchedMoved := 1
			for _, c := range []cell{
				{newLead, leadstore.SuppressAddress, "info@new.localhost", leadstore.ReasonOptOut, leadstore.LeadSuppressed, 1, 0},
				{batched, leadstore.SuppressDomain, "batched.localhost", leadstore.ReasonManual, batchedState, wantBatchedMoved, 2},
				{contacted, leadstore.SuppressAddress, "info@contacted.localhost", leadstore.ReasonOptOut, leadstore.LeadContacted, 0, 2},
				{replied, leadstore.SuppressDomain, "replied.localhost", leadstore.ReasonRepliedNo, leadstore.LeadReplied, 0, 0},
				{bounced, leadstore.SuppressAddress, "info@bounced.localhost", leadstore.ReasonBounce, leadstore.LeadBounced, 0, 0},
				{skipped, leadstore.SuppressDomain, "skipped.localhost", leadstore.ReasonManual, leadstore.LeadSkipped, 0, 0},
			} {
				res, err := f.q.Suppress(ctx, c.kind, c.value, c.reason, "cli")
				if err != nil {
					t.Fatalf("suppress %s: %v", c.value, err)
				}
				if !res.Recorded || res.Leads != 1 || res.Moved != c.wantMoved || res.Cancelled != c.wantCancelled {
					t.Errorf("suppress %s = %+v, want recorded, 1 lead, %d moved, %d cancelled", c.value, res, c.wantMoved, c.wantCancelled)
				}
				if got := f.leadState(t, c.lead); got != c.wantState {
					t.Errorf("lead %s after suppress = %s, want %s", c.value, got, c.wantState)
				}
				// The same suppression again is idempotent: no new row, nothing moves.
				res, err = f.q.Suppress(ctx, c.kind, c.value, c.reason, "cli")
				if err != nil || res.Recorded || res.Moved != 0 || res.Cancelled != 0 {
					t.Errorf("suppress %s again = %+v, %v; want a no-op", c.value, res, err)
				}
			}
			eqStates(t, "batched", f.msgStates(t, id, batched), "cancelled", "cancelled")
			eqStates(t, "contacted", f.msgStates(t, id, contacted), "cancelled", "cancelled")
			// A suppression is only ever added: another reason is a second row.
			if res, _ := f.q.Suppress(ctx, leadstore.SuppressDomain, "replied.localhost", leadstore.ReasonManual, "cli"); !res.Recorded {
				t.Error("a second reason for one value was not recorded")
			}
			var n int
			if err := f.owner.QueryRow(ctx, "SELECT count(*) FROM suppression").Scan(&n); err != nil || n != 7 {
				t.Errorf("suppression rows = %d, %v; want 7", n, err)
			}
			set, err := f.q.SuppressionSet(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := leadstore.Suppressions{
				Addresses: []string{"info@bounced.localhost", "info@contacted.localhost", "info@new.localhost"},
				Domains:   []string{"batched.localhost", "replied.localhost", "skipped.localhost"},
			}
			if !reflect.DeepEqual(set, want) {
				t.Errorf("suppression set = %+v, want %+v", set, want)
			}
			for _, bad := range [][3]string{
				{"address", "no-at.localhost", "manual"}, {"domain", "a@b.localhost", "manual"},
				{"address", " ", "manual"}, {"phone", "x", "manual"}, {"domain", "x.localhost", "angry"},
			} {
				if _, err := f.q.Suppress(ctx, bad[0], bad[1], bad[2], "cli"); err == nil {
					t.Errorf("suppress %v: want an error", bad)
				}
			}
		})
	}
}

func TestSuppressionBetweenApproveAndSendCancels(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "northwind.localhost", intp(1), 7, "info@northwind.localhost")
	id, sum := f.batch(t, false, lead)
	f.approve(t, id, sum)
	if due, _ := f.q.DueMessages(ctx, dueBase, false); len(due) != 1 {
		t.Fatalf("due before suppress = %d, want 1", len(due))
	}
	if _, err := f.q.Suppress(ctx, leadstore.SuppressAddress, "info@northwind.localhost", leadstore.ReasonOptOut, "mailbox:INBOX:7"); err != nil {
		t.Fatal(err)
	}
	eqStates(t, "after suppress", f.msgStates(t, id, lead), "cancelled", "cancelled")
	if due, _ := f.q.DueMessages(ctx, dueBase.AddDate(1, 0, 0), false); len(due) != 0 {
		t.Errorf("due after suppress = %d, want 0", len(due))
	}
	ms, _ := f.q.BatchMessages(ctx, id)
	if changed, err := f.q.MarkSending(ctx, ms[0].ID); err != nil || changed {
		t.Errorf("mark sending a cancelled message = %v, %v; want false", changed, err)
	}
	if ms[0].CancelReason != "suppressed: opt_out" {
		t.Errorf("cancel reason = %q", ms[0].CancelReason)
	}
}

func TestSendStoreCells(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	live := f.siteLead(t, "live.localhost", intp(1), 7, "info@live.localhost")
	shad := f.siteLead(t, "shadow.localhost", intp(2), 7, "info@shadow.localhost")
	liveID, liveSum := f.batch(t, false, live)
	shadowID, shadowSum := f.batch(t, true, shad)
	f.approve(t, liveID, liveSum)
	f.approve(t, shadowID, shadowSum)

	liveDue, err := f.q.DueMessages(ctx, dueBase, false)
	if err != nil || len(liveDue) != 1 || liveDue[0].LeadID != live || liveDue[0].Step != 0 || liveDue[0].Shadow ||
		liveDue[0].LeadState != leadstore.LeadBatched || liveDue[0].OrgDomain != "live.localhost" {
		t.Fatalf("live due = %+v, %v", liveDue, err)
	}
	shadowDue, err := f.q.DueMessages(ctx, dueBase, true)
	if err != nil || len(shadowDue) != 1 || shadowDue[0].LeadID != shad || !shadowDue[0].Shadow {
		t.Fatalf("shadow due = %+v, %v", shadowDue, err)
	}
	if early, _ := f.q.DueMessages(ctx, dueBase.Add(-time.Minute), false); len(early) != 0 {
		t.Errorf("due before due_at = %d, want 0", len(early))
	}

	// Two senders race for one message: exactly one claims it (RA3).
	m := liveDue[0].ID
	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	for i := range 2 {
		q := leadstore.NewQueries(storetest.AppConn(t))
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = q.MarkSending(ctx, m)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || results[0] == results[1] {
		t.Fatalf("concurrent mark sending = %v, %v; want exactly one true", results, errs)
	}
	if again, _ := f.q.MarkSending(ctx, m); again {
		t.Error("mark sending a sending message changed it")
	}
	// A live mark refuses a shadow message, and the other way round (RA1).
	if err := f.q.MarkShadowSent(ctx, m, dueBase); !errors.Is(err, leadstore.ErrMessageState) {
		t.Errorf("shadow-sent on a live message: err = %v", err)
	}
	if err := f.q.MarkSent(ctx, m, "250 ok", dueBase); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if got := f.leadState(t, live); got != leadstore.LeadContacted {
		t.Errorf("lead after send = %s, want contacted", got)
	}
	if err := f.q.MarkSent(ctx, m, "250 ok", dueBase); !errors.Is(err, leadstore.ErrMessageState) {
		t.Errorf("mark sent twice: err = %v", err)
	}
	sm := shadowDue[0].ID
	if changed, _ := f.q.MarkSending(ctx, sm); !changed {
		t.Fatal("mark sending a shadow message")
	}
	if err := f.q.MarkSent(ctx, sm, "250", dueBase); !errors.Is(err, leadstore.ErrMessageState) {
		t.Errorf("live mark sent on a shadow message: err = %v", err)
	}
	if err := f.q.MarkShadowSent(ctx, sm, dueBase); err != nil {
		t.Fatalf("mark shadow sent: %v", err)
	}
	if got := f.leadState(t, shad); got != leadstore.LeadNew {
		t.Errorf("shadow lead after shadow send = %s, want new (RA1)", got)
	}

	// Contacted: the follow-up comes due; maybe_sent is never re-queued; refused is final.
	follow, _ := f.q.DueMessages(ctx, dueBase.AddDate(0, 0, 3), false)
	if len(follow) != 1 || follow[0].Step != 1 || follow[0].LeadState != leadstore.LeadContacted {
		t.Fatalf("follow-up due = %+v", follow)
	}
	if err := f.q.MarkMaybeSent(ctx, follow[0].ID, "timeout after DATA"); !errors.Is(err, leadstore.ErrMessageState) {
		t.Errorf("maybe sent from approved: err = %v, want ErrMessageState", err)
	}
	if changed, _ := f.q.MarkSending(ctx, follow[0].ID); !changed {
		t.Fatal("claim follow-up")
	}
	if err := f.q.MarkMaybeSent(ctx, follow[0].ID, "timeout after DATA"); err != nil {
		t.Fatal(err)
	}
	if again, _ := f.q.DueMessages(ctx, dueBase.AddDate(1, 0, 0), false); len(again) != 0 {
		t.Errorf("due after maybe_sent = %+v, want none", again)
	}
	if err := f.q.MarkRefused(ctx, follow[0].ID, "x"); !errors.Is(err, leadstore.ErrMessageState) {
		t.Errorf("refuse a maybe_sent message: err = %v", err)
	}
	ss := f.msgStates(t, shadowID, shad)
	eqStates(t, "shadow", ss, "shadow_sent", "approved")
	sms, _ := f.q.BatchMessages(ctx, shadowID)
	if err := f.q.MarkRefused(ctx, sms[1].ID, "550 no such user"); err != nil {
		t.Fatal(err)
	}
	eqStates(t, "shadow after refuse", f.msgStates(t, shadowID, shad), "shadow_sent", "refused")
	eqStates(t, "live", f.msgStates(t, liveID, live), "sent", "maybe_sent")
}

func TestCancelPendingByDomain(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "a.localhost", intp(1), 7, "info@a.localhost")
	b := f.siteLead(t, "client.localhost", intp(2), 7, "web@shop.a.localhost") // recipient on a subdomain
	c := f.siteLead(t, "c.localhost", intp(3), 7, "info@c.localhost")
	id, _ := f.batch(t, false, a, b, c)
	n, err := f.q.CancelPending(ctx, leadstore.CancelTarget{Domain: "A.localhost"}, "replied")
	if err != nil || n != 4 {
		t.Fatalf("cancel by domain = %d, %v; want 4", n, err)
	}
	eqStates(t, "c", f.msgStates(t, id, c), "draft", "draft")
	if n, _ := f.q.CancelPending(ctx, leadstore.CancelTarget{LeadID: c}, "x"); n != 2 {
		t.Errorf("cancel by lead = %d, want 2", n)
	}
	if _, err := f.q.CancelPending(ctx, leadstore.CancelTarget{}, "x"); err == nil {
		t.Error("cancel with no target: want an error")
	}
}

func TestMarkContacted(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()

	// A fresh domain: no site, no org. An org and a hand lead appear, contacted by gremy.
	res, err := f.q.MarkContacted(ctx, "www.Fresh.localhost", "wrote by hand")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OrgCreated || res.LeadKind != leadstore.LeadHand || res.State != leadstore.LeadContacted || !res.Moved {
		t.Errorf("fresh domain = %+v", res)
	}
	o := orgByDomain(t, f.q, "fresh.localhost")
	if o.Kind != leadstore.OrgSiteOwner || o.Source != leadstore.OrgSourceProbe || o.SiteID != nil {
		t.Errorf("org = %+v", o)
	}
	var ev string
	if err := f.owner.QueryRow(ctx, "SELECT state || ' ' || reason || ' ' || actor FROM lead_event WHERE lead_id = $1 ORDER BY id DESC LIMIT 1",
		res.LeadID).Scan(&ev); err != nil || ev != "contacted wrote by hand gremy" {
		t.Errorf("last event = %q, %v", ev, err)
	}
	// Again: a no-op.
	if again, err := f.q.MarkContacted(ctx, "fresh.localhost", ""); err != nil || again.Moved || again.OrgCreated || again.LeadID != res.LeadID {
		t.Errorf("again = %+v, %v", again, err)
	}

	// A site with a Drupal probe but no org yet: the org links its site, the lead its finding.
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{{Domain: "probed.localhost"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.RecordProbe(ctx, siteID(t, "probed.localhost"), f.runID, leadstore.ProbeRow{
		Outcome: leadstore.OutcomeOK, FinalHost: "probed.localhost", IsDrupal: true, Major: 10, FetchIDs: []int64{f.fid}, DerivedBy: "drupal@1",
	}); err != nil {
		t.Fatal(err)
	}
	res, err = f.q.MarkContacted(ctx, "probed.localhost", "")
	if err != nil || res.LeadKind != leadstore.LeadD10 || res.State != leadstore.LeadContacted {
		t.Errorf("probed site = %+v, %v", res, err)
	}
	if o := orgByDomain(t, f.q, "probed.localhost"); o.SiteID == nil || *o.SiteID != siteID(t, "probed.localhost") {
		t.Errorf("org site = %v", o.SiteID)
	}

	// A batched lead: its pending messages are cancelled, it is contacted, never drafted again.
	lead := f.siteLead(t, "batched.localhost", intp(1), 7, "info@batched.localhost")
	id, sum := f.batch(t, false, lead)
	f.approve(t, id, sum)
	res, err = f.q.MarkContacted(ctx, "batched.localhost", "")
	if err != nil || res.LeadID != lead || res.Cancelled != 2 || res.State != leadstore.LeadContacted || res.OrgCreated {
		t.Errorf("batched lead = %+v, %v", res, err)
	}
	ms, _ := f.q.BatchMessages(ctx, id)
	for _, m := range ms {
		if m.State != leadstore.MsgCancelled || m.CancelReason != leadstore.CancelHandContacted {
			t.Errorf("message %d = %s %q, want cancelled hand-contacted", m.ID, m.State, m.CancelReason)
		}
	}
	if got := candidateIDs(t, f.q); len(got) != 0 {
		t.Errorf("candidates = %v, want none", got)
	}
	if _, err := f.q.MarkContacted(ctx, "not a domain", ""); !errors.Is(err, leadstore.ErrNoDomain) {
		t.Errorf("bad domain: err = %v", err)
	}
}

func TestAppRoleCannotRewriteOutreach(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "northwind.localhost", intp(1), 7, "info@northwind.localhost")
	f.batch(t, false, lead)
	if _, err := f.q.Suppress(ctx, leadstore.SuppressAddress, "x@northwind.localhost", leadstore.ReasonManual, "cli"); err != nil {
		t.Fatal(err)
	}
	app := storetest.AppConn(t)
	for _, q := range []string{
		"UPDATE outreach_message SET body_text = 'changed'",
		"UPDATE outreach_message SET to_address = 'other@x.localhost'",
		"UPDATE outreach_message SET subject = ''",
		"UPDATE outreach_message SET in_reply_to = ''",
		"UPDATE outreach_batch SET content_sha256 = ''",
		"UPDATE lead SET org_id = 1",
		"UPDATE lead_event SET reason = ''",
		"UPDATE suppression SET value = 'y@x.localhost'",
		"DELETE FROM suppression",
		"DELETE FROM outreach_message",
	} {
		t.Run(q, func(t *testing.T) {
			_, err := app.Exec(ctx, q)
			if got := sqlState(err); got != "42501" {
				t.Fatalf("%s as app: err = %v (SQLSTATE %q), want 42501", q, err, got)
			}
		})
	}
}

// outreachSnapshot is what one outreach pass leaves behind.
type outreachSnapshot struct {
	Batch     int64
	Hash      string
	Approval  leadstore.Approval
	Messages  []string
	Leads     []string
	Candidate []int64
}

func outreachPass(t *testing.T) outreachSnapshot {
	t.Helper()
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "northwind.localhost", intp(2), 7, "info@northwind.localhost")
	f.siteLead(t, "contoso.localhost", intp(1), 10, "info@contoso.localhost")
	f.agencyLead(t, "agency.localhost", "hello@agency.localhost")
	cands := candidateIDs(t, f.q, leadstore.LeadUnsupported)
	var s outreachSnapshot
	s.Candidate = cands
	tx, err := storetest.AppConn(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q := leadstore.NewQueries(tx)
	id, sum, err := q.CreateBatch(ctx, false, f.drafts(t, cands...), []byte("t"))
	if err != nil {
		t.Fatal(err)
	}
	s.Batch, s.Hash = id, hex.EncodeToString(sum)
	if s.Approval, err = q.ApproveBatch(ctx, id, s.Hash[:12], []int{3}, dueAt); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	_ = a
	rows, _ := f.owner.Query(ctx, "SELECT id || ' ' || lead_id || ' ' || step || ' ' || state || ' ' || coalesce(due_at::text, '-') FROM outreach_message ORDER BY id")
	s.Messages, _ = pgx.CollectRows(rows, pgx.RowTo[string])
	rows, _ = f.owner.Query(ctx, "SELECT id || ' ' || kind || ' ' || state FROM lead ORDER BY id")
	s.Leads, _ = pgx.CollectRows(rows, pgx.RowTo[string])
	return s
}

// TestOutreachColdStartTwin: the first batch ever, on an empty database, twice; both end
// the same, ids and hashes included.
func TestOutreachColdStartTwin(t *testing.T) {
	first := outreachPass(t)
	if first.Batch != 1 || len(first.Messages) != 6 || first.Approval.Approved != 4 || first.Approval.Skipped != 2 {
		t.Fatalf("first pass = %+v", first)
	}
	if twin := outreachPass(t); !reflect.DeepEqual(twin, first) {
		t.Errorf("fresh database differs:\n%+v\nwant\n%+v", twin, first)
	}
}

// TestCandidateLeadsLeaveOutReserved: by default no lead on a reserved TLD is a candidate,
// .localhost included (RA25); IncludeReserved lets them through for shadow and dry runs.
func TestCandidateLeadsLeaveOutReserved(t *testing.T) {
	f := newOutreachFixture(t)
	var ids []int64
	for _, tld := range leadstore.ReservedTLDs {
		ids = append(ids, f.siteLead(t, "northwind."+tld, intp(len(ids)+1), 7, "info@northwind."+tld))
	}
	cs, err := f.q.CandidateLeads(context.Background(), leadstore.CandidateOptions{})
	if err != nil || len(cs) != 0 {
		t.Errorf("default candidates = %+v, %v; want none on reserved TLDs", cs, err)
	}
	if got := candidateIDs(t, f.q); !reflect.DeepEqual(got, ids) {
		t.Errorf("IncludeReserved candidates = %v, want %v", got, ids)
	}
}

// reprobe records a new Drupal probe of domain's site with major and returns its id.
func (f outreachFixture) reprobe(t *testing.T, domain string, major int) int64 {
	t.Helper()
	id, err := f.q.RecordProbe(context.Background(), siteID(t, domain), f.runID, leadstore.ProbeRow{
		Outcome: leadstore.OutcomeOK, FinalHost: domain, IsDrupal: true, Major: major,
		FetchIDs: []int64{f.fid}, DerivedBy: "drupal@1",
	})
	if err != nil {
		t.Fatalf("reprobe %s: %v", domain, err)
	}
	return id
}

func (f outreachFixture) leadOf(t *testing.T, id int64) leadstore.Lead {
	t.Helper()
	l, err := f.q.LeadByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestStaleFindingWaitsForRefresh (P17): a new lead follows the latest facts; a lead past
// new never changes.
func TestStaleFindingWaitsForRefresh(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	d9 := f.siteLead(t, "nine.localhost", intp(1), 9, "info@nine.localhost")
	if got := candidateIDs(t, f.q); !reflect.DeepEqual(got, []int64{d9}) {
		t.Fatalf("candidates = %v", got)
	}
	// Re-probed as Drupal 11: the stored finding is stale, so the lead is not drafted.
	p11 := f.reprobe(t, "nine.localhost", 11)
	if got := candidateIDs(t, f.q); len(got) != 0 {
		t.Errorf("candidates after re-probe = %v, want none until refreshed", got)
	}
	if _, _, err := f.q.CreateBatch(ctx, false, f.drafts(t, d9), nil); !errors.Is(err, leadstore.ErrNotDraftable) {
		t.Errorf("batch on a stale lead: err = %v, want ErrNotDraftable", err)
	}
	// The caller classifies D11 as no lead: it moves the lead to skipped.
	f.setState(t, d9, leadstore.LeadSkipped)
	_ = p11

	// Re-probed as Drupal 10: refresh moves the lead onto the new finding and kind.
	d7 := f.siteLead(t, "seven.localhost", intp(2), 7, "info@seven.localhost")
	p10 := f.reprobe(t, "seven.localhost", 10)
	l := f.leadOf(t, d7)
	changed, err := f.q.RefreshLead(ctx, leadstore.LeadRow{OrgID: l.OrgID, Kind: leadstore.LeadD10, ProbeID: &p10, Channel: l.Channel, RouteID: l.RouteID})
	if err != nil || !changed {
		t.Fatalf("refresh = %v, %v", changed, err)
	}
	if got := f.leadOf(t, d7); got.Kind != leadstore.LeadD10 || *got.ProbeID != p10 {
		t.Errorf("refreshed lead = %+v", got)
	}
	if got := candidateIDs(t, f.q); !reflect.DeepEqual(got, []int64{d7}) {
		t.Errorf("candidates after refresh = %v, want [%d]", got, d7)
	}
	if again, _ := f.q.RefreshLead(ctx, leadstore.LeadRow{OrgID: l.OrgID, Kind: leadstore.LeadD10, ProbeID: &p10, Channel: l.Channel, RouteID: l.RouteID}); again {
		t.Error("an identical refresh reported a change")
	}
	// lead_finding_check still holds.
	if _, err := f.q.RefreshLead(ctx, leadstore.LeadRow{OrgID: l.OrgID, Kind: leadstore.LeadD10, Channel: leadstore.ChannelNone}); sqlState(err) != "23514" {
		t.Errorf("refresh to no finding: err = %v, want 23514", err)
	}

	// A form-channel lead takes the email route that appears later.
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{{Domain: "form.localhost", Rank: intp(3)}}); err != nil {
		t.Fatal(err)
	}
	pf := f.reprobe(t, "form.localhost", 7)
	orgID, _, err := f.q.UpsertSiteOrg(ctx, pf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.InsertRoute(ctx, leadstore.RouteRow{OrgID: orgID, Kind: leadstore.RouteForm, Value: "https://form.localhost/contact", FetchID: f.fid, DerivedBy: "contacts@1"}); err != nil {
		t.Fatal(err)
	}
	routes, _ := f.q.RoutesFor(ctx, orgID)
	formLead, _, err := f.q.UpsertLead(ctx, leadstore.LeadRow{OrgID: orgID, Kind: leadstore.LeadUnsupported, ProbeID: &pf, Channel: leadstore.ChannelForm, RouteID: &routes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.InsertRoute(ctx, leadstore.RouteRow{OrgID: orgID, Kind: leadstore.RouteEmail, Value: "info@form.localhost", FetchID: f.fid, MXOK: true, DerivedBy: "contacts@1"}); err != nil {
		t.Fatal(err)
	}
	routes, _ = f.q.RoutesFor(ctx, orgID) // emails first
	if changed, err := f.q.RefreshLead(ctx, leadstore.LeadRow{OrgID: orgID, Kind: leadstore.LeadUnsupported, ProbeID: &pf, Channel: leadstore.ChannelEmail, RouteID: &routes[0].ID}); err != nil || !changed {
		t.Fatalf("refresh form → email = %v, %v", changed, err)
	}
	if got := f.leadOf(t, formLead); got.Channel != leadstore.ChannelEmail || *got.RouteID != routes[0].ID {
		t.Errorf("form lead after refresh = %+v", got)
	}

	// A batched lead is never refreshed.
	id, _ := f.batch(t, false, d7)
	_ = id
	before := f.leadOf(t, d7)
	p11b := f.reprobe(t, "seven.localhost", 11)
	if changed, err := f.q.RefreshLead(ctx, leadstore.LeadRow{OrgID: l.OrgID, Kind: leadstore.LeadUnsupported, ProbeID: &p11b, Channel: leadstore.ChannelNone}); err != nil || changed {
		t.Errorf("refresh of a batched lead = %v, %v; want no change", changed, err)
	}
	if got := f.leadOf(t, d7); !reflect.DeepEqual(got, before) {
		t.Errorf("batched lead changed: %+v, was %+v", got, before)
	}
}

// TestSuppressDomainIsRegistrable: a domain suppression covers the whole registrable domain.
func TestSuppressDomainIsRegistrable(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	acme := f.siteLead(t, "acme.localhost", intp(1), 7, "info@acme.localhost")
	other := f.siteLead(t, "other.localhost", intp(2), 7, "info@other.localhost")
	id, sum := f.batch(t, true, acme, other) // shadow: leads stay new
	drafts := f.drafts(t, acme)

	if _, err := f.q.Suppress(ctx, leadstore.SuppressDomain, "localhost", leadstore.ReasonManual, "cli"); !errors.Is(err, leadstore.ErrSuppressNotRegistrable) {
		t.Errorf("suppress a bare TLD: err = %v, want ErrSuppressNotRegistrable", err)
	}
	if _, err := f.q.Suppress(ctx, leadstore.SuppressDomain, "Mail.Acme.localhost", leadstore.ReasonManual, "cli"); err != nil {
		t.Fatal(err)
	}
	set, _ := f.q.SuppressionSet(ctx)
	if !reflect.DeepEqual(set.Domains, []string{"acme.localhost"}) {
		t.Errorf("suppressed domains = %v, want [acme.localhost]", set.Domains)
	}
	if got := candidateIDs(t, f.q); !reflect.DeepEqual(got, []int64{other}) {
		t.Errorf("candidates = %v, want [%d]", got, other)
	}
	if _, _, err := f.q.CreateBatch(ctx, true, drafts, nil); !errors.Is(err, leadstore.ErrNotDraftable) {
		t.Errorf("batch new for acme: err = %v, want ErrNotDraftable", err)
	}
	f.approve(t, id, sum)
	eqStates(t, "acme", f.msgStates(t, id, acme), "cancelled", "cancelled")
	eqStates(t, "other", f.msgStates(t, id, other), "approved", "approved")
}

// TestApproveRefusesEditedRows: the batch's rows must still hash to content_sha256.
func TestApproveRefusesEditedRows(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "northwind.localhost", intp(1), 7, "info@northwind.localhost")
	id, sum := f.batch(t, false, lead)
	got, err := f.q.BatchContentSHA256(ctx, id)
	if err != nil || hex.EncodeToString(got) != sum {
		t.Fatalf("recomputed hash = %x, %v; want %s", got, err, sum)
	}
	if _, err := f.owner.Exec(ctx, "UPDATE outreach_message SET body_text = body_text || ' edited' WHERE batch_id = $1 AND step = 1", id); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.q.BatchContentSHA256(ctx, id); hex.EncodeToString(got) == sum {
		t.Error("recomputed hash ignored an edit")
	}
	if _, err := f.q.ApproveBatch(ctx, id, sum[:12], nil, dueAt); !errors.Is(err, leadstore.ErrBatchTampered) {
		t.Errorf("approve an edited batch: err = %v, want ErrBatchTampered", err)
	}
	eqStates(t, "edited", f.msgStates(t, id, lead), "draft", "draft")
}
