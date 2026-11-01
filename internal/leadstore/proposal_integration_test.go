//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// proposalT14Now is the fixed "now" of these tests.
var proposalT14Now = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// proposalT14 returns a good proposal to change.
func proposalT14(lead int64) leadstore.NewProposal {
	return leadstore.NewProposal{LeadID: lead, Title: "Upgrade for Harbour Lane Studio", Actor: leadstore.ActorOperator}
}

// proposalT14Stage asserts the lead's stored stage, effective stage and how many stage events it has.
func proposalT14Stage(t *testing.T, f outreachFixture, lead int64, stored, effective string, events int) {
	t.Helper()
	c, err := f.q.GetLeadCRM(context.Background(), lead)
	if err != nil {
		t.Fatal(err)
	}
	if c.Stage != stored || c.EffectiveStage != effective {
		t.Errorf("stage = %q, effective %q; want %q, %q", c.Stage, c.EffectiveStage, stored, effective)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_stage_event WHERE lead_id = $1`, lead); n != int64(events) {
		t.Errorf("stage events = %d, want %d", n, events)
	}
}

// TestProposalSentMovesStage: a proposal added as sent moves a lead to the proposal stage only
// from none or talking, stored or implied by a reply; a lead already in proposal writes no second
// event, won and lost keep their stage (and the lost reason). The lead's note is kept.
func TestProposalSentMovesStage(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	cases := []struct {
		name   string
		set    func(lead int64)
		before int    // stage events before
		stored string // stored stage after
		events int    // stage events after
	}{
		{"none", func(int64) {}, 0, leadstore.StageProposal, 1},
		{"note only", func(lead int64) {
			if err := f.q.SetLeadNote(ctx, lead, "keep me", "", nil, proposalT14Now); err != nil {
				t.Fatal(err)
			}
		}, 0, leadstore.StageProposal, 1},
		{"talking stored", func(lead int64) {
			if err := f.q.SetLeadStage(ctx, lead, leadstore.StageTalking, "", "", proposalT14Now); err != nil {
				t.Fatal(err)
			}
		}, 1, leadstore.StageProposal, 2},
		{"talking by a reply", func(lead int64) {
			ownerRun(t, f.owner, `UPDATE lead SET state = 'replied' WHERE id = $1`, lead)
		}, 0, leadstore.StageProposal, 1},
		{"already proposal", func(lead int64) {
			if err := f.q.SetLeadStage(ctx, lead, leadstore.StageProposal, "", "", proposalT14Now); err != nil {
				t.Fatal(err)
			}
		}, 1, leadstore.StageProposal, 1},
		{"won", func(lead int64) {
			if err := f.q.SetLeadStage(ctx, lead, leadstore.StageWon, "", "https://harbour-lane.example/", proposalT14Now); err != nil {
				t.Fatal(err)
			}
		}, 1, leadstore.StageWon, 1},
		{"lost", func(lead int64) {
			if err := f.q.SetLeadStage(ctx, lead, leadstore.StageLost, "no budget", "", proposalT14Now); err != nil {
				t.Fatal(err)
			}
		}, 1, leadstore.StageLost, 1},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lead := f.siteLead(t, "stage-"+string(rune('a'+i))+".localhost", nil, 7, "")
			tc.set(lead)
			if _, err := f.q.AddProposal(ctx, proposalT14(lead), proposalT14Now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			proposalT14Stage(t, f, lead, tc.stored, tc.stored, tc.events)
			c, err := f.q.GetLeadCRM(ctx, lead)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "note only" && c.Note != "keep me" {
				t.Errorf("note = %q, want it kept", c.Note)
			}
			if tc.name == "lost" && c.LostReason != "no budget" {
				t.Errorf("lost reason = %q, want it kept", c.LostReason)
			}
			if tc.name == "won" && c.TenantURL != "https://harbour-lane.example/" {
				t.Errorf("tenant = %q, want it kept", c.TenantURL)
			}
			if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_stage_event WHERE lead_id = $1 AND at = $2`, lead,
				proposalT14Now.Add(time.Hour)); tc.events > tc.before && n != 1 {
				t.Errorf("events at the proposal's time = %d, want 1", n)
			}
		})
	}
}

// TestProposalOtherStatusesKeepStage: a proposal added as anything but sent, accepted included,
// moves no stage. SetProposalStatus moves one only on sent, by the same rule: accepted on a lead
// in proposal does not force won, and a withdrawn proposal sent again moves a lead at none.
func TestProposalOtherStatusesKeepStage(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	for i, status := range []string{leadstore.ProposalAccepted, leadstore.ProposalDeclined, leadstore.ProposalWithdrawn} {
		lead := f.siteLead(t, "other-"+string(rune('a'+i))+".localhost", nil, 7, "")
		p := proposalT14(lead)
		p.Status = status
		if _, err := f.q.AddProposal(ctx, p, proposalT14Now); err != nil {
			t.Fatal(err)
		}
		proposalT14Stage(t, f, lead, "", "", 0)
	}

	lead := f.siteLead(t, "again.localhost", nil, 7, "")
	p := proposalT14(lead)
	p.Status = leadstore.ProposalWithdrawn
	id, err := f.q.AddProposal(ctx, p, proposalT14Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{leadstore.ProposalDeclined, leadstore.ProposalAccepted} {
		if err := f.q.SetProposalStatus(ctx, id, s, "", leadstore.ActorOperator, proposalT14Now); err != nil {
			t.Fatal(err)
		}
		proposalT14Stage(t, f, lead, "", "", 0)
	}
	if err := f.q.SetProposalStatus(ctx, id, leadstore.ProposalSent, "", leadstore.ActorOperator, proposalT14Now); err != nil {
		t.Fatal(err)
	}
	proposalT14Stage(t, f, lead, leadstore.StageProposal, leadstore.StageProposal, 1)
	if err := f.q.SetProposalStatus(ctx, id, leadstore.ProposalAccepted, "", leadstore.ActorOperator, proposalT14Now); err != nil {
		t.Fatal(err)
	}
	proposalT14Stage(t, f, lead, leadstore.StageProposal, leadstore.StageProposal, 1)

	// Sent again on a won lead keeps won.
	if err := f.q.SetLeadStage(ctx, lead, leadstore.StageWon, "", "", proposalT14Now); err != nil {
		t.Fatal(err)
	}
	if err := f.q.SetProposalStatus(ctx, id, leadstore.ProposalSent, "", leadstore.ActorOperator, proposalT14Now); err != nil {
		t.Fatal(err)
	}
	proposalT14Stage(t, f, lead, leadstore.StageWon, leadstore.StageWon, 2)
}

// TestProposalAtomic: when the stage move fails, the proposal and its status do not stay; when
// the status fails, neither the proposal nor the stage does. A proposal's status likewise.
func TestProposalAtomic(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "atomic.localhost", nil, 7, "")
	block := func(table, name, check string) func() {
		ownerRun(t, f.owner, `ALTER TABLE `+table+` ADD CONSTRAINT `+name+` CHECK (`+check+`) NOT VALID`)
		drop := func() { ownerRun(t, f.owner, `ALTER TABLE `+table+` DROP CONSTRAINT IF EXISTS `+name) }
		t.Cleanup(drop)
		return drop
	}
	counts := func() (int64, int64) {
		t.Helper()
		return ownerInt(t, f.owner, `SELECT count(*) FROM lead_proposal`), ownerInt(t, f.owner, `SELECT count(*) FROM lead_proposal_status`)
	}

	unblock := block("lead_stage_event", "t14_block_event", `stage <> 'proposal'`)
	if _, err := f.q.AddProposal(ctx, proposalT14(lead), proposalT14Now); err == nil {
		t.Fatal("add with the stage move blocked: no error")
	}
	if p, s := counts(); p != 0 || s != 0 {
		t.Errorf("after a failed stage move: %d proposals, %d statuses", p, s)
	}
	proposalT14Stage(t, f, lead, "", "", 0)
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_crm WHERE lead_id = $1`, lead); n != 0 {
		t.Errorf("a lead_crm row stayed: %d", n)
	}

	withdrawn := proposalT14(lead)
	withdrawn.Status = leadstore.ProposalWithdrawn
	id, err := f.q.AddProposal(ctx, withdrawn, proposalT14Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.q.SetProposalStatus(ctx, id, leadstore.ProposalSent, "", leadstore.ActorOperator, proposalT14Now); err == nil {
		t.Fatal("status with the stage move blocked: no error")
	}
	if _, s := counts(); s != 1 {
		t.Errorf("after a failed status: %d statuses, want 1", s)
	}
	unblock()

	block("lead_proposal_status", "t14_block_status", `note <> 'blocked'`)
	blocked := proposalT14(lead)
	blocked.Note = "blocked"
	if _, err := f.q.AddProposal(ctx, blocked, proposalT14Now); err == nil {
		t.Fatal("add with the status blocked: no error")
	}
	if p, s := counts(); p != 1 || s != 1 {
		t.Errorf("after a failed add: %d proposals, %d statuses; want 1, 1", p, s)
	}
	proposalT14Stage(t, f, lead, "", "", 0)
}

// TestProposalWaitsForStageWriter: an add, or a status of sent, waiting on a transaction that
// makes the lead won reads the stage after that commits, so it leaves the lead won.
func TestProposalWaitsForStageWriter(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q2 := leadstoretest.Q(t)
	run := func(t *testing.T, lead int64, write func() error) {
		t.Helper()
		tx, err := storetest.AppConn(t).Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := leadstore.NewQueries(tx).SetLeadStage(ctx, lead, leadstore.StageWon, "", "", proposalT14Now); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- write() }()
		select {
		case err := <-done:
			t.Fatalf("the write did not wait for the lock: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		proposalT14Stage(t, f, lead, leadstore.StageWon, leadstore.StageWon, 1)
	}
	t.Run("add", func(t *testing.T) {
		lead := f.siteLead(t, "race-add.localhost", nil, 7, "")
		run(t, lead, func() error {
			_, err := q2.AddProposal(ctx, proposalT14(lead), proposalT14Now)
			return err
		})
	})
	t.Run("status", func(t *testing.T) {
		lead := f.siteLead(t, "race-status.localhost", nil, 7, "")
		p := proposalT14(lead)
		p.Status = leadstore.ProposalWithdrawn
		id, err := f.q.AddProposal(ctx, p, proposalT14Now)
		if err != nil {
			t.Fatal(err)
		}
		run(t, lead, func() error {
			return q2.SetProposalStatus(ctx, id, leadstore.ProposalSent, "", leadstore.ActorOperator, proposalT14Now)
		})
	})
}

// TestProposalsForLatestStatus: proposals read newest first with their latest status and only
// their own lead's; a lead with none reads empty, never nil. Defaults and trimming hold.
func TestProposalsForLatestStatus(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "read.localhost", nil, 7, "")
	other := f.siteLead(t, "read-other.localhost", nil, 7, "")
	got, err := f.q.ProposalsFor(ctx, lead)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("cold start: %v %v, want empty and not nil", got, err)
	}
	value := int64(125000)
	full := leadstore.NewProposal{LeadID: lead, Title: "  Full  ", URL: "  https://harbour-lane.example/p  ", ValueCents: &value,
		Currency: " eur ", Note: "  first  ", Actor: leadstore.ActorOperator}
	first, err := f.q.AddProposal(ctx, full, proposalT14Now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.q.AddProposal(ctx, proposalT14(lead), proposalT14Now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.AddProposal(ctx, proposalT14(other), proposalT14Now); err != nil {
		t.Fatal(err)
	}
	later := proposalT14Now.Add(2 * time.Hour)
	if err := f.q.SetProposalStatus(ctx, first, leadstore.ProposalDeclined, "  too dear  ", leadstore.ActorOperator, later); err != nil {
		t.Fatal(err)
	}
	if err := f.q.SetProposalStatus(ctx, first, leadstore.ProposalAccepted, "after all", leadstore.ActorSystem, later); err != nil {
		t.Fatal(err)
	}
	got, err = f.q.ProposalsFor(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	if !slices.Equal(ids, []int64{second, first}) {
		t.Fatalf("order: %v, want %v", ids, []int64{second, first})
	}
	p := got[1]
	if p.Title != "Full" || p.URL != "https://harbour-lane.example/p" || p.ValueCents == nil || *p.ValueCents != value ||
		p.Currency != "EUR" || p.Status != leadstore.ProposalAccepted || p.StatusNote != "after all" ||
		!p.StatusAt.Equal(later) || !p.At.Equal(proposalT14Now) || p.Actor != leadstore.ActorOperator || p.LeadID != lead {
		t.Errorf("full: %+v", p)
	}
	d := got[0]
	if d.URL != "" || d.ValueCents != nil || d.Currency != leadstore.DefaultCurrency || d.Status != leadstore.ProposalSent || d.StatusNote != "" {
		t.Errorf("defaults: %+v", d)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_proposal_status WHERE proposal_id = $1 AND note = 'too dear'`, first); n != 1 {
		t.Errorf("trimmed note rows = %d, want 1", n)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_proposal WHERE url IS NULL`); n != 2 {
		t.Errorf("proposals with a NULL link = %d, want 2", n)
	}
}

// TestProposalRefusals: bad input and missing rows write nothing and move no stage; a limit
// counts characters.
func TestProposalRefusals(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "refused.localhost", nil, 7, "")
	negative := int64(-1)
	cases := []struct {
		what string
		mut  func(*leadstore.NewProposal)
		want error
	}{
		{"blank title", func(p *leadstore.NewProposal) { p.Title = " " }, leadstore.ErrBadCRM},
		{"long title", func(p *leadstore.NewProposal) { p.Title = strings.Repeat("t", leadstore.MaxProposalTitle+1) }, leadstore.ErrBadCRM},
		{"two-line title", func(p *leadstore.NewProposal) { p.Title = "a\nb" }, leadstore.ErrBadCRM},
		{"http link", func(p *leadstore.NewProposal) { p.URL = "http://harbour-lane.example/" }, leadstore.ErrBadCRM},
		{"negative value", func(p *leadstore.NewProposal) { p.ValueCents = &negative }, leadstore.ErrBadCRM},
		{"unknown currency", func(p *leadstore.NewProposal) { p.Currency = "CHF" }, leadstore.ErrBadCRM},
		{"unknown status", func(p *leadstore.NewProposal) { p.Status = "maybe" }, leadstore.ErrBadCRM},
		{"long note", func(p *leadstore.NewProposal) { p.Note = strings.Repeat("n", leadstore.MaxProposalNote+1) }, leadstore.ErrBadCRM},
		{"unknown actor", func(p *leadstore.NewProposal) { p.Actor = leadstore.ActorGremy }, leadstore.ErrUnknownActor},
		{"missing lead", func(p *leadstore.NewProposal) { p.LeadID = 999999 }, leadstore.ErrLeadNotFound},
	}
	for _, tc := range cases {
		p := proposalT14(lead)
		tc.mut(&p)
		if _, err := f.q.AddProposal(ctx, p, proposalT14Now); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.what, err, tc.want)
		}
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_proposal`); n != 0 {
		t.Errorf("%d proposals written by refused input", n)
	}
	proposalT14Stage(t, f, lead, "", "", 0)

	if err := f.q.SetProposalStatus(ctx, 999999, leadstore.ProposalSent, "", leadstore.ActorOperator, proposalT14Now); !errors.Is(err, leadstore.ErrProposalNotFound) {
		t.Errorf("missing proposal: %v", err)
	}
	p := proposalT14(lead)
	p.Status = leadstore.ProposalDeclined
	p.Title = strings.Repeat("é", leadstore.MaxProposalTitle)
	p.Note = strings.Repeat("é", leadstore.MaxProposalNote)
	id, err := f.q.AddProposal(ctx, p, proposalT14Now)
	if err != nil {
		t.Fatalf("at every limit: %v", err)
	}
	if err := f.q.SetProposalStatus(ctx, id, "maybe", "", leadstore.ActorOperator, proposalT14Now); !errors.Is(err, leadstore.ErrBadCRM) {
		t.Errorf("unknown status: %v", err)
	}
	if err := f.q.SetProposalStatus(ctx, id, leadstore.ProposalSent, strings.Repeat("n", leadstore.MaxProposalNote+1), leadstore.ActorOperator, proposalT14Now); !errors.Is(err, leadstore.ErrBadCRM) {
		t.Errorf("long note: %v", err)
	}
	if err := f.q.SetProposalStatus(ctx, id, leadstore.ProposalSent, "", "gremy", proposalT14Now); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("unknown actor: %v", err)
	}
	proposalT14Stage(t, f, lead, "", "", 0)
}
