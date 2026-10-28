package phones

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/research"
)

type fakeSource struct{ leads []CallLead }

func (f fakeSource) CallLeadsAhead(context.Context, int64) ([]CallLead, error) { return f.leads, nil }

func lead(id int64, starred bool, phones ...LeadPhone) CallLead {
	return CallLead{LeadID: id, OrgID: id * 10, OrgName: fmt.Sprintf("Org %d", id), Domain: fmt.Sprintf("org%d.example", id), Country: "GB", Starred: starred, Phones: phones}
}

func TestSelectStarredFirstSkipsDirectLines(t *testing.T) {
	leads := []CallLead{
		lead(1, false),
		lead(2, false, LeadPhone{Normalized: "+442079460002", LineKind: research.LineDirect}),
		lead(3, true, LeadPhone{Normalized: "+442079460003", LineKind: research.LineSwitchboard}),
		lead(4, false, LeadPhone{Normalized: "+442079460004"}), // unknown kind counts as switchboard
		lead(5, true, LeadPhone{Normalized: "+442079460005", LineKind: research.LineDecisionMaker}),
		lead(6, true),
		{LeadID: 7, OrgID: 70, Starred: true}, // no domain: cannot be researched
	}
	dup := lead(8, false)
	dup.OrgID = 10 // same org as lead 1
	leads = append(leads, dup)

	got := Select(leads, 0)
	var ids []int64
	for _, l := range got {
		ids = append(ids, l.LeadID)
	}
	want := []int64{3, 6, 1, 4}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("Select = %v, want %v", ids, want)
	}
	if got := Select(leads, 2); len(got) != 2 || got[0].LeadID != 3 || got[1].LeadID != 6 {
		t.Fatalf("Select max 2 = %+v", got)
	}
}

func TestRunPhoneOnlyAndRecords(t *testing.T) {
	src := fakeSource{leads: []CallLead{lead(1, false), lead(2, true)}}
	var asked []research.Request
	rs := func(_ context.Context, r research.Request) (research.Result, error) {
		asked = append(asked, r)
		return research.Result{Model: "claude-haiku-5-5", Phones: []research.Phone{{
			Fact:     leadstore.FactRow{OrgID: r.Org.ID, Kind: leadstore.FactPhone, Normalized: "+442079460000", DerivedBy: research.DerivedBy},
			LineKind: research.LineDirect, Role: "Head of IT",
		}}}, nil
	}
	out, err := Run(context.Background(), src, rs, 1, 0)
	if err != nil || len(out) != 2 {
		t.Fatalf("Run = %d outcomes, %v", len(out), err)
	}
	if asked[0].LeadID != 2 || asked[0].Want.Email || !asked[0].Want.Phone || asked[0].Org.Domain != "org2.example" {
		t.Fatalf("first request %+v: want lead 2 (starred), phone only", asked[0])
	}
	rec := out[0].Records
	if len(rec) != 1 || rec[0].LineKind != research.LineDirect || rec[0].Role != "Head of IT" || rec[0].Model != "claude-haiku-5-5" || rec[0].Fact.OrgID != 20 {
		t.Fatalf("records %+v", rec)
	}
}

func TestRunStopsOnSpendCapKeepsOtherErrors(t *testing.T) {
	src := fakeSource{leads: []CallLead{lead(1, false), lead(2, false), lead(3, false)}}
	calls := 0
	rs := func(_ context.Context, r research.Request) (research.Result, error) {
		calls++
		switch r.LeadID {
		case 1:
			return research.Result{}, errors.New("fetch failed")
		case 2:
			return research.Result{}, fmt.Errorf("capped: %w", ai.ErrSpendCap)
		}
		return research.Result{}, nil
	}
	out, err := Run(context.Background(), src, rs, 1, 0)
	if !errors.Is(err, ai.ErrSpendCap) || calls != 2 || len(out) != 2 || out[0].Err == nil {
		t.Fatalf("Run = %d outcomes, %d calls, %v", len(out), calls, err)
	}
}
