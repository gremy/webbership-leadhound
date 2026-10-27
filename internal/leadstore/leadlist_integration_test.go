//go:build integration

package leadstore_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// The Leads list and the lead page (Task 2.1).

// listIDs returns the leads f matches under c, one page of up to 100, as ids in order, and the total.
func listIDs(t *testing.T, q leadstore.Queries, f leadstore.ListFilter, c leadstore.ListContext) ([]int64, int) {
	t.Helper()
	rows, total := listRows(t, q, f, c)
	ids := []int64{}
	for _, r := range rows {
		ids = append(ids, r.LeadID)
	}
	return ids, total
}

func listRows(t *testing.T, q leadstore.Queries, f leadstore.ListFilter, c leadstore.ListContext) ([]leadstore.ListRow, int) {
	t.Helper()
	if f.PerPage == 0 {
		f.PerPage = leadstore.ListPerPageMax
	}
	rows, total, err := q.LeadList(context.Background(), f, c)
	if err != nil {
		t.Fatalf("lead list %+v: %v", f, err)
	}
	return rows, total
}

func profileJSON(t *testing.T, p leadconfig.Profile) string {
	t.Helper()
	s, err := leadstore.ListProfileJSON(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// rate appends one relevance rating of lead.
func (f outreachFixture) rate(t *testing.T, lead int64, score int, band string) {
	t.Helper()
	sum := sha256.Sum256(fmt.Appendf(nil, "%d-%d", lead, score))
	if _, err := f.q.InsertRelevance(context.Background(), leadstore.RelevanceRow{
		LeadID: lead, Question: "relevance@1", Decider: "fake", InputSHA256: sum[:], Band: band, Score: score,
		Probabilities: map[string]float64{band: 1}, InputTokens: 10, RunID: f.runID,
	}); err != nil {
		t.Fatalf("rate lead %d: %v", lead, err)
	}
}

// fact adds a fact of lead's org, read from page.
func (f outreachFixture) fact(t *testing.T, lead, page int64, kind, value, normalized string) {
	t.Helper()
	if _, err := f.q.InsertFact(context.Background(), leadstore.FactRow{
		OrgID: f.leadOf(t, lead).OrgID, Kind: kind, Value: value, Normalized: normalized, FetchID: page, DerivedBy: "enrich@1",
	}); err != nil {
		t.Fatalf("fact %s: %v", kind, err)
	}
}

func (f outreachFixture) star(t *testing.T, lead int64) {
	t.Helper()
	if _, err := f.q.Star(context.Background(), lead, leadstore.ActorOperator); err != nil {
		t.Fatalf("star %d: %v", lead, err)
	}
}

func sortedIDs(ids ...int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	if out == nil {
		out = []int64{}
	}
	return out
}

func boolp(b bool) *bool { return &b }

func TestLeadListFilters(t *testing.T) {
	f := newOutreachFixture(t)
	page := f.pageFetch(t, "https://a.localhost/contact", "", "<html>contact</html>")
	a := f.siteLead(t, "a.localhost", intp(5000), 7, "info@a.localhost")
	f.setOrgCountry(t, a, "GB")
	f.fact(t, a, page, leadstore.FactPhone, "+44 20 7946 0123", "+442079460123")
	f.star(t, a)
	f.rate(t, a, 80, leadstore.BandStrong)
	b := f.formLead(t, "b.localhost")
	f.setOrgCountry(t, b, "NL")
	c := f.agencyLead(t, "c.localhost", "hello@c.localhost")
	f.setOrgCountry(t, c, "FR")
	f.rate(t, c, 30, leadstore.BandUnlikely)
	d := f.siteLead(t, "d.localhost", nil, 8, "")
	// A selftest lead is never listed.
	if _, err := f.q.CreateSelftest(context.Background(), leadstore.SelftestDraft{To: "operator" + "@" + "sender.localhost",
		Subject: "selftest", Body: "selftest", MessageID: "st@sender.localhost", Due: dueBase, MailboxID: f.mailbox(t)}); err != nil {
		t.Fatal(err)
	}
	ctx := leadstore.ListContext{}
	for name, tc := range map[string]struct {
		f    leadstore.ListFilter
		want []int64
	}{
		"none":                {leadstore.ListFilter{}, []int64{a, b, c, d}},
		"kind":                {leadstore.ListFilter{Kinds: []string{leadstore.LeadD10}}, []int64{b}},
		"kinds":               {leadstore.ListFilter{Kinds: []string{leadstore.LeadD10, leadstore.LeadAgencyPartner}}, []int64{b, c}},
		"country":             {leadstore.ListFilter{Countries: []string{"GB"}}, []int64{a}},
		"countries":           {leadstore.ListFilter{Countries: []string{"GB", "FR", "DE"}}, []int64{a, c}},
		"email":               {leadstore.ListFilter{Channels: []string{leadstore.ListChannelEmail}}, []int64{a, c}},
		"form":                {leadstore.ListFilter{Channels: []string{leadstore.ListChannelForm}}, []int64{b}},
		"phone channel":       {leadstore.ListFilter{Channels: []string{leadstore.ListChannelPhone}}, []int64{a}},
		"form or phone":       {leadstore.ListFilter{Channels: []string{leadstore.ListChannelForm, leadstore.ListChannelPhone}}, []int64{a, b}},
		"starred":             {leadstore.ListFilter{Starred: boolp(true)}, []int64{a}},
		"not starred":         {leadstore.ListFilter{Starred: boolp(false)}, []int64{b, c, d}},
		"has phone":           {leadstore.ListFilter{HasPhone: true}, []int64{a}},
		"min relevance 50":    {leadstore.ListFilter{MinRelevance: 50}, []int64{a}},
		"min relevance 1":     {leadstore.ListFilter{MinRelevance: 1}, []int64{a, c}},
		"min relevance 30":    {leadstore.ListFilter{MinRelevance: 30}, []int64{a, c}},
		"min relevance 31":    {leadstore.ListFilter{MinRelevance: 31}, []int64{a}},
		"min relevance > 100": {leadstore.ListFilter{MinRelevance: 500}, []int64{}},
		"combined": {leadstore.ListFilter{Kinds: []string{leadstore.LeadUnsupported, leadstore.LeadAgencyPartner},
			Channels: []string{leadstore.ListChannelEmail}, MinRelevance: 20}, []int64{a, c}},
		"combined, not starred": {leadstore.ListFilter{Kinds: []string{leadstore.LeadUnsupported, leadstore.LeadAgencyPartner},
			Channels: []string{leadstore.ListChannelEmail}, MinRelevance: 20, Starred: boolp(false)}, []int64{c}},
		"combined, nothing": {leadstore.ListFilter{Countries: []string{"GB"}, HasPhone: true, Kinds: []string{leadstore.LeadD10}}, []int64{}},
		"order sort":        {leadstore.ListFilter{Sort: leadstore.SortOrder}, []int64{a}},
	} {
		t.Run(name, func(t *testing.T) {
			ids, total := listIDs(t, f.q, tc.f, ctx)
			if got := sortedIDs(ids...); !reflect.DeepEqual(got, sortedIDs(tc.want...)) {
				t.Errorf("ids %v, want %v", got, sortedIDs(tc.want...))
			}
			if total != len(tc.want) {
				t.Errorf("total %d, want %d", total, len(tc.want))
			}
		})
	}
	// Paging: the total is every match, a page past the last is empty with the true total.
	for _, p := range []struct{ page, per, rows, total int }{
		{1, 2, 2, 4}, {2, 2, 2, 4}, {3, 2, 0, 4}, {0, 3, 3, 4}, {-1, 3, 3, 4}, {2, 3, 1, 4},
	} {
		rows, total, err := f.q.LeadList(context.Background(), leadstore.ListFilter{Page: p.page, PerPage: p.per}, ctx)
		if err != nil || len(rows) != p.rows || total != p.total {
			t.Errorf("page %d per %d: %d rows, total %d, %v; want %d, %d", p.page, p.per, len(rows), total, err, p.rows, p.total)
		}
	}
	if _, total, err := f.q.LeadList(context.Background(), leadstore.ListFilter{Kinds: []string{"none"}, Page: 2}, ctx); err != nil || total != 0 {
		t.Errorf("no match, page 2: total %d, %v", total, err)
	}
	for name, bad := range map[string]leadstore.ListFilter{
		"channel": {Channels: []string{"fax"}}, "status": {Status: "closed"}, "sort": {Sort: "name"},
	} {
		if _, _, err := f.q.LeadList(context.Background(), bad, ctx); !errors.Is(err, leadstore.ErrBadListFilter) {
			t.Errorf("unknown %s: %v, want ErrBadListFilter", name, err)
		}
	}
}

// sortFixture is five leads whose points under testProfile and relevance give distinct
// orders: points x1 60, x2 45, x4 38 (rank 50000), x5 38 (rank 99999), x3 25; relevance x3
// 90, x4 70.
func sortFixture(t *testing.T, f outreachFixture) (x1, x2, x3, x4, x5 int64) {
	t.Helper()
	x1 = f.siteLead(t, "x1.localhost", intp(500000), 7, "info@x1.localhost")
	x2 = f.siteLead(t, "x2.localhost", intp(5), 10, "info@x2.localhost")
	x3 = f.agencyLead(t, "x3.localhost", "hello@x3.localhost")
	x4 = f.siteLead(t, "x4.localhost", intp(50000), 10, "info@x4.localhost")
	x5 = f.siteLead(t, "x5.localhost", intp(99999), 10, "info@x5.localhost")
	f.rate(t, x3, 90, leadstore.BandStrong)
	f.rate(t, x4, 70, leadstore.BandLikely)
	return
}

func TestLeadListSorts(t *testing.T) {
	f := newOutreachFixture(t)
	x1, x2, x3, x4, x5 := sortFixture(t, f)
	pf := profileJSON(t, testProfile)
	for name, tc := range map[string]struct {
		f    leadstore.ListFilter
		c    leadstore.ListContext
		want []int64
	}{
		"relevance, trusted":   {leadstore.ListFilter{}, leadstore.ListContext{ProfileJSON: pf, SortByRelevance: true}, []int64{x3, x4, x1, x2, x5}},
		"relevance, untrusted": {leadstore.ListFilter{Sort: leadstore.SortRelevance}, leadstore.ListContext{ProfileJSON: pf}, []int64{x1, x2, x4, x5, x3}},
		"newest":               {leadstore.ListFilter{Sort: leadstore.SortNewest}, leadstore.ListContext{ProfileJSON: pf, SortByRelevance: true}, []int64{x5, x4, x3, x2, x1}},
		"no profile, trusted":  {leadstore.ListFilter{}, leadstore.ListContext{SortByRelevance: true}, []int64{x3, x4, x2, x5, x1}},
	} {
		t.Run(name, func(t *testing.T) {
			if got, _ := listIDs(t, f.q, tc.f, tc.c); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("order %v, want %v", got, tc.want)
			}
		})
	}
	rows, _ := listRows(t, f.q, leadstore.ListFilter{}, leadstore.ListContext{ProfileJSON: pf})
	points := map[int64]int{}
	for _, r := range rows {
		points[r.LeadID] = r.Points
	}
	if want := map[int64]int{x1: 60, x2: 45, x3: 25, x4: 38, x5: 38}; !reflect.DeepEqual(points, want) {
		t.Errorf("points %v, want %v", points, want)
	}
	// Untrusted, the number is still there.
	if rows[len(rows)-1].LeadID != x3 || rows[len(rows)-1].Relevance == nil || *rows[len(rows)-1].Relevance != 90 {
		t.Errorf("untrusted last row %+v: want x3 with relevance 90", rows[len(rows)-1])
	}

	// order: the starred only, by position.
	if got, _ := listIDs(t, f.q, leadstore.ListFilter{Sort: leadstore.SortOrder}, leadstore.ListContext{}); len(got) != 0 {
		t.Errorf("order with no stars: %v", got)
	}
	f.star(t, x2)
	f.star(t, x5)
	f.star(t, x4)
	if _, err := f.q.MoveTop(context.Background(), x4, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.Unstar(context.Background(), x5, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	rows, total := listRows(t, f.q, leadstore.ListFilter{Sort: leadstore.SortOrder}, leadstore.ListContext{ProfileJSON: pf})
	var got []int64
	for _, r := range rows {
		got = append(got, r.LeadID)
		if !r.Starred || r.Position == nil {
			t.Errorf("lead %d in my order: starred %v position %v", r.LeadID, r.Starred, r.Position)
		}
	}
	if !reflect.DeepEqual(got, []int64{x4, x2}) || total != 2 {
		t.Errorf("my order %v (total %d), want %v", got, total, []int64{x4, x2})
	}
	if *rows[0].Position >= *rows[1].Position {
		t.Errorf("positions %d, %d not increasing", *rows[0].Position, *rows[1].Position)
	}

	// Small sites: site leads whose rank is unknown or over the bound; never an agency.
	unranked := f.siteLead(t, "x6.localhost", nil, 7, "info@x6.localhost")
	rows, _ = listRows(t, f.q, leadstore.ListFilter{}, leadstore.ListContext{SkipRankOver: 50000})
	small := map[int64]bool{}
	for _, r := range rows {
		small[r.LeadID] = r.SmallSite
	}
	if want := map[int64]bool{x1: true, x2: false, x3: false, x4: false, x5: true, unranked: true}; !reflect.DeepEqual(small, want) {
		t.Errorf("small sites %v, want %v", small, want)
	}
	rows, _ = listRows(t, f.q, leadstore.ListFilter{}, leadstore.ListContext{})
	for _, r := range rows {
		if r.SmallSite {
			t.Errorf("lead %d small with the switch off", r.LeadID)
		}
	}
}

func TestLeadListStatusGrid(t *testing.T) {
	f := newOutreachFixture(t)
	ids := map[string]int64{}
	for _, state := range []string{leadstore.LeadNew, leadstore.LeadBatched, leadstore.LeadContacted, leadstore.LeadReplied,
		leadstore.LeadBounced, leadstore.LeadOptedOut, leadstore.LeadSuppressed, leadstore.LeadSkipped, leadstore.LeadClosed} {
		dom := fmt.Sprintf("state%d.localhost", len(ids))
		id := f.siteLead(t, dom, nil, 7, "info@"+dom)
		if state != leadstore.LeadNew {
			f.setState(t, id, state)
		}
		ids[state] = id
	}
	pick := func(states ...string) []int64 {
		var out []int64
		for _, s := range states {
			out = append(out, ids[s])
		}
		return sortedIDs(out...)
	}
	all := pick(leadstore.LeadNew, leadstore.LeadBatched, leadstore.LeadContacted, leadstore.LeadReplied, leadstore.LeadBounced,
		leadstore.LeadOptedOut, leadstore.LeadSuppressed, leadstore.LeadSkipped, leadstore.LeadClosed)
	for status, want := range map[string][]int64{
		"":                      pick(leadstore.LeadNew),
		leadstore.ListOpen:      pick(leadstore.LeadNew),
		leadstore.ListContacted: pick(leadstore.LeadBatched, leadstore.LeadContacted, leadstore.LeadReplied, leadstore.LeadBounced, leadstore.LeadOptedOut),
		leadstore.ListAll:       all,
	} {
		got, total := listIDs(t, f.q, leadstore.ListFilter{Status: status}, leadstore.ListContext{})
		if got = sortedIDs(got...); !reflect.DeepEqual(got, want) || total != len(want) {
			t.Errorf("status %q: %v (total %d), want %v", status, got, total, want)
		}
	}
	rows, _ := listRows(t, f.q, leadstore.ListFilter{Status: leadstore.ListAll}, leadstore.ListContext{})
	for _, r := range rows {
		if ids[r.State] != r.LeadID {
			t.Errorf("lead %d state %q", r.LeadID, r.State)
		}
	}
}

// signalLead is a site lead on domain whose probe saw two signals on page and a site name.
func (f outreachFixture) signalLead(t *testing.T, domain string, page int64) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{{Domain: domain, Rank: intp(1234)}}); err != nil {
		t.Fatal(err)
	}
	probeID, err := f.q.RecordProbe(ctx, siteID(t, domain), f.runID, leadstore.ProbeRow{
		Outcome: leadstore.OutcomeOK, FinalHost: domain, IsDrupal: true, Major: 10, ExactVersion: "10.2.3",
		Signals: []leadstore.Signal{
			{Signal: "generator", Value: "Drupal 10", FetchID: page},
			{Signal: "x-generator", Value: "Drupal 10 (https://www.drupal.org)", FetchID: page},
		},
		FetchIDs: []int64{page}, SiteName: "Signal Bakery", DerivedBy: "drupal@1",
	})
	if err != nil {
		t.Fatal(err)
	}
	orgID, _, err := f.q.UpsertSiteOrg(ctx, probeID)
	if err != nil {
		t.Fatal(err)
	}
	return f.lead(t, leadstore.LeadRow{OrgID: orgID, Kind: leadstore.LeadD10, ProbeID: &probeID}, "info@"+domain)
}

func TestLeadDetailEverything(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	home := f.pageFetch(t, "https://signal.localhost/", "https://www.signal.localhost/", "<html>home</html>")
	contact := f.pageFetch(t, "https://signal.localhost/contact", "", "<html>contact</html>")
	lead := f.signalLead(t, "signal.localhost", home)
	f.setOrgCountry(t, lead, "GB")
	f.fact(t, lead, contact, leadstore.FactPhone, "+44 20 7946 0123", "+442079460123")
	f.fact(t, lead, home, leadstore.FactDescription, "A bakery.", "A bakery.")
	f.fact(t, lead, home, leadstore.FactSocial, "https://social.example/signal", "https://social.example/signal")
	for i := range 6 {
		f.rate(t, lead, 10*(i+1), leadstore.BandMaybe)
	}
	f.star(t, lead)
	if err := f.q.DeferLead(ctx, lead, priorityDay); err != nil {
		t.Fatal(err)
	}
	batchID, _ := f.batch(t, true, lead)

	d, err := f.q.LeadDetail(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	if d.LeadID != lead || d.Domain != "signal.localhost" || d.Name != "Signal Bakery" || d.Country != "GB" ||
		d.Kind != leadstore.LeadD10 || d.State != leadstore.LeadNew {
		t.Errorf("row %+v", d.ListRow)
	}
	if !d.HasEmail || d.HasForm || !d.HasPhone || !d.Starred || d.Position == nil {
		t.Errorf("flags: email %v form %v phone %v starred %v position %v", d.HasEmail, d.HasForm, d.HasPhone, d.Starred, d.Position)
	}
	if d.Relevance == nil || *d.Relevance != 60 || d.Band != leadstore.BandMaybe || d.RatedAt == nil {
		t.Errorf("relevance %v %q %v, want the latest, 60", d.Relevance, d.Band, d.RatedAt)
	}
	if want := (leadstore.FindingSubject{Kind: leadstore.LeadD10, Detector: "drupal@1", Major: 10, ExactVersion: "10.2.3"}); d.Finding != want {
		t.Errorf("finding %+v, want %+v", d.Finding, want)
	}
	if len(d.Signals) != 2 || d.Signals[0].Name != "generator" || d.Signals[0].Value != "Drupal 10" ||
		d.Signals[0].FetchID == nil || *d.Signals[0].FetchID != home || d.Signals[0].URL != "https://signal.localhost/" ||
		d.Signals[0].FinalURL != "https://www.signal.localhost/" || d.Signals[0].Status != 200 || d.Signals[0].FetchedAt == nil ||
		d.Signals[1].Name != "x-generator" {
		t.Errorf("signals %+v", d.Signals)
	}
	if len(d.Routes) != 1 || d.Routes[0].Kind != leadstore.RouteEmail || d.Routes[0].Value != "info@signal.localhost" ||
		!d.Routes[0].MXOK || d.Routes[0].FetchID != f.fid || d.Routes[0].FinalURL != "https://evidence.localhost/" {
		t.Errorf("routes %+v", d.Routes)
	}
	var facts []string
	for _, x := range d.Facts {
		facts = append(facts, fmt.Sprintf("%s=%s@%d %s", x.Kind, x.Value, x.FetchID, x.FinalURL))
	}
	if want := []string{
		fmt.Sprintf("description=A bakery.@%d https://www.signal.localhost/", home),
		fmt.Sprintf("phone=+44 20 7946 0123@%d https://signal.localhost/contact", contact),
		fmt.Sprintf("social=https://social.example/signal@%d https://www.signal.localhost/", home),
	}; !reflect.DeepEqual(facts, want) {
		t.Errorf("facts %q, want %q", facts, want)
	}
	if d.Agency != nil {
		t.Errorf("a site lead has an agency %+v", d.Agency)
	}
	if d.ListRank == nil || *d.ListRank != 1234 || d.Origin != leadstore.OriginTranco {
		t.Errorf("list rank %v origin %q", d.ListRank, d.Origin)
	}
	var events []string
	for _, e := range d.Events {
		events = append(events, e.State)
	}
	if want := []string{leadstore.LeadNew}; !reflect.DeepEqual(events, want) || d.Events[0].Actor == "" || d.Events[0].At.IsZero() {
		t.Errorf("events %+v, want states %v", d.Events, want)
	}
	if d.Priority == nil || d.Priority.Action != leadstore.PriorityDefer || d.Priority.Until == nil ||
		d.Priority.Until.Format(time.DateOnly) != priorityDay.Format(time.DateOnly) || d.Priority.Actor != leadstore.ActorGremy {
		t.Errorf("priority %+v", d.Priority)
	}
	if len(d.Messages) != 2 || d.Messages[0].BatchID != batchID || !d.Messages[0].Shadow || d.Messages[0].Step != 0 ||
		d.Messages[0].Channel != "email" || d.Messages[0].State == "" || d.Messages[0].Subject != "Drupal upgrade 0" ||
		d.Messages[0].Body != fmt.Sprintf("Body 0 of lead %d", lead) || d.Messages[1].Step != 1 {
		t.Errorf("messages %+v", d.Messages)
	}
	var scores []int
	for _, r := range d.RelevanceHistory {
		scores = append(scores, r.Score)
		if r.Question != "relevance@1" || r.Decider != "fake" || r.Band != leadstore.BandMaybe || r.CreatedAt.IsZero() {
			t.Errorf("rating %+v", r)
		}
	}
	if want := []int{60, 50, 40, 30, 20}; !reflect.DeepEqual(scores, want) {
		t.Errorf("relevance history %v, want %v", scores, want)
	}

	// Before Weigh no profile applies; after it, the reasons and points the list gives.
	if d.Points != 0 || d.Reasons != nil || d.SmallSite {
		t.Errorf("unweighed: points %d reasons %v small %v", d.Points, d.Reasons, d.SmallSite)
	}
	d.Weigh(testProfile, 1000)
	rows, _ := listRows(t, f.q, leadstore.ListFilter{}, leadstore.ListContext{ProfileJSON: profileJSON(t, testProfile), SkipRankOver: 1000})
	if len(rows) != 1 || rows[0].Points != d.Points || !rows[0].SmallSite || !d.SmallSite {
		t.Errorf("weighed points %d small %v; list %+v", d.Points, d.SmallSite, rows)
	}
	// d10 10 + major 10 10 + exact 5 + GB 20 + email 10 + rank under 10000 15.
	if d.Points != 70 || len(d.Reasons) != 6 {
		t.Errorf("weighed %d %+v, want 70 over 6 reasons", d.Points, d.Reasons)
	}

	// An agency lead: its finding page and words, its one piece of evidence.
	ag := f.agencyLead(t, "agency.localhost", "hello@agency.localhost")
	d, err = f.q.LeadDetail(ctx, ag)
	if err != nil {
		t.Fatal(err)
	}
	if d.Agency == nil || d.Agency.URL != "https://agency.localhost/upgrades" || d.Agency.Phrase != "Drupal 7 upgrades" || d.Agency.FetchID != f.fid {
		t.Errorf("agency %+v", d.Agency)
	}
	if d.Finding.URL != "https://agency.localhost/upgrades" || d.Finding.Phrase != "Drupal 7 upgrades" || d.Finding.Major != 0 {
		t.Errorf("agency finding %+v", d.Finding)
	}
	if d.Name != "Agency agency.localhost" || len(d.Signals) != 1 || d.Signals[0].Name != "phrase" || d.Signals[0].Value != "Drupal 7 upgrades" {
		t.Errorf("agency name %q signals %+v", d.Name, d.Signals)
	}
	if len(d.Messages) != 0 {
		t.Errorf("another lead's messages: %+v", d.Messages)
	}

	if _, err := f.q.LeadDetail(ctx, ag+1000); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("missing lead: %v, want ErrLeadNotFound", err)
	}
}

// TestLeadListColdStart: leads with no facts, stars, relevance, priority or messages list
// and open with empty values, and a hand lead with no finding has an empty one.
func TestLeadListColdStart(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	site := f.siteLead(t, "cold.localhost", nil, 7, "")
	hand, err := f.q.MarkContacted(ctx, 0, "hand.localhost", "")
	if err != nil {
		t.Fatal(err)
	}
	rows, total := listRows(t, f.q, leadstore.ListFilter{Status: leadstore.ListAll}, leadstore.ListContext{SortByRelevance: true})
	if total != 2 || len(rows) != 2 {
		t.Fatalf("%d rows, total %d", len(rows), total)
	}
	for _, r := range rows {
		if r.Relevance != nil || r.Band != "" || r.RatedAt != nil || r.Starred || r.Position != nil ||
			r.HasEmail || r.HasForm || r.HasPhone || r.Points != 0 || r.SmallSite || r.CreatedAt.IsZero() {
			t.Errorf("cold row %+v", r)
		}
		if r.Name != r.Domain {
			t.Errorf("name %q, want the domain %q", r.Name, r.Domain)
		}
	}
	for _, id := range []int64{site, hand.LeadID} {
		d, err := f.q.LeadDetail(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Routes) != 0 || len(d.Facts) != 0 || d.Agency != nil || d.Priority != nil || len(d.Messages) != 0 ||
			len(d.RelevanceHistory) != 0 || d.Reasons != nil {
			t.Errorf("lead %d cold detail %+v", id, d)
		}
		d.Weigh(leadconfig.Profile{}, 0)
		if d.Points != 0 || d.Reasons != nil {
			t.Errorf("lead %d, empty profile: %d %v", id, d.Points, d.Reasons)
		}
	}
	d, err := f.q.LeadDetail(ctx, hand.LeadID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != leadstore.LeadHand || d.State != leadstore.LeadContacted || d.Finding != (leadstore.FindingSubject{}) ||
		len(d.Signals) != 0 || d.ListRank != nil || len(d.Events) != 2 {
		t.Errorf("hand lead %+v", d)
	}
	d, err = f.q.LeadDetail(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Signals) != 0 || d.Finding.Major != 7 || d.Finding.Detector != "drupal@1" {
		t.Errorf("site lead with no signals: %+v %+v", d.Signals, d.Finding)
	}
}

// TestLeadListHugePage: a page past ListPageMax reads ListPageMax, so the offset never
// overflows into a negative OFFSET that Postgres refuses: no rows, the true total, no error.
func TestLeadListHugePage(t *testing.T) {
	f := newOutreachFixture(t)
	f.siteLead(t, "page.localhost", nil, 7, "")
	for _, page := range []int{math.MaxInt, math.MaxInt/leadstore.ListPerPageMax + 2, leadstore.ListPageMax + 1} {
		for _, per := range []int{leadstore.ListPerPageDefault, leadstore.ListPerPageMax} {
			rows, total, err := f.q.LeadList(context.Background(), leadstore.ListFilter{Status: leadstore.ListAll, Page: page, PerPage: per}, leadstore.ListContext{})
			if err != nil || len(rows) != 0 || total != 1 {
				t.Errorf("page %d of %d: %d rows, total %d, %v; want none, total 1, no error", page, per, len(rows), total, err)
			}
		}
	}
}

// TestLeadListPointsDrift: the list's points equal Score on the same leads, for several
// profiles, over the varied fixture the queue's drift test uses; and the trusted and
// untrusted orders follow those points.
func TestLeadListPointsDrift(t *testing.T) {
	f := newOutreachFixture(t)
	ids := variedLeads(t, f)
	for i, id := range ids {
		if i%3 == 0 {
			f.rate(t, id, (i*7)%101, leadstore.BandMaybe)
		}
	}
	negative := testProfile.Clone()
	negative.Kinds[leadstore.LeadD10] = -100
	negative.Majors[11] = 100
	negative.Channels[leadstore.ChannelForm] = 40
	negative.RankBands = []leadconfig.RankBand{{Under: 2, Points: -50}, {Under: 100000, Points: 100}}
	negative.ExcludeCountries = nil
	for name, p := range map[string]leadconfig.Profile{
		"suggested": testProfile, "negative": negative,
		"kinds only": {Kinds: leadconfig.KindPoints([]string{leadstore.LeadD10})},
	} {
		t.Run(name, func(t *testing.T) {
			p.ExcludeCountries = nil // the list shows every country; the queue's candidates must too
			cands := map[int64]leadstore.Candidate{}
			for _, c := range candidates(t, f.q, leadstore.CandidateOptions{Profile: p}) {
				cands[c.LeadID] = c
			}
			if len(cands) != len(ids) {
				t.Fatalf("%d candidates, want %d", len(cands), len(ids))
			}
			for _, trusted := range []bool{false, true} {
				rows, total := listRows(t, f.q, leadstore.ListFilter{}, leadstore.ListContext{ProfileJSON: profileJSON(t, p), SortByRelevance: trusted})
				if total != len(ids) {
					t.Errorf("trusted %v: total %d, want %d", trusted, total, len(ids))
				}
				for i, r := range rows {
					c := cands[r.LeadID]
					if score, _ := leadstore.Score(p, c); r.Points != score {
						t.Errorf("lead %d (%s %s major %d %q rank %s %s): list points %d, Score %d",
							r.LeadID, c.Kind, c.Country, c.Major, c.ExactVersion, rankOf(c.ListRank), c.Channel, r.Points, score)
					}
					if i > 0 && !listBefore(rows[i-1], r, cands, trusted) {
						t.Errorf("trusted %v: lead %d before lead %d", trusted, rows[i-1].LeadID, r.LeadID)
					}
				}
				for _, id := range ids[:3] {
					d, err := f.q.LeadDetail(context.Background(), id)
					if err != nil {
						t.Fatal(err)
					}
					d.Weigh(p, 0)
					if score, _ := leadstore.Score(p, cands[id]); d.Points != score {
						t.Errorf("lead %d: weighed %d, Score %d", id, d.Points, score)
					}
				}
			}
		})
	}
}

// listBefore reports whether a may come before b in the relevance sort.
func listBefore(a, b leadstore.ListRow, cands map[int64]leadstore.Candidate, trusted bool) bool {
	if trusted && (a.Relevance == nil) != (b.Relevance == nil) {
		return b.Relevance == nil
	}
	if trusted && a.Relevance != nil && *a.Relevance != *b.Relevance {
		return *a.Relevance > *b.Relevance
	}
	ca, cb := cands[a.LeadID], cands[b.LeadID]
	ca.Score, cb.Score = a.Points, b.Points
	return before(ca, cb)
}
