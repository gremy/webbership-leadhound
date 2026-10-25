//go:build integration

package leadstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The suggested defaults, a weighed and a negative country, and an exclusion.
var testProfile = leadconfig.Profile{
	Kinds:        map[string]int{leadstore.LeadUnsupported: 20, leadstore.LeadD10: 10, leadstore.LeadAgencyPartner: 15},
	Majors:       map[int]int{6: 5, 7: 30, 8: 20, 9: 15, 10: 10},
	Countries:    map[string]int{"GB": 20, "NL": -10},
	Channels:     map[string]int{leadstore.ChannelEmail: 10, leadstore.ChannelForm: 0},
	RankBands:    []leadconfig.RankBand{{Under: 10000, Points: 15}, {Under: 100000, Points: 8}},
	ExactVersion: 5, ExcludeCountries: []string{"DE"},
}

var priorityDay = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

// setOrgCountry, setExact and setForm shape a fixture lead's facts as the owner.
func (f outreachFixture) setOrgCountry(t *testing.T, lead int64, country string) {
	t.Helper()
	f.ownerExec(t, `UPDATE org SET country = $2 FROM lead WHERE lead.org_id = org.id AND lead.id = $1`, lead, country)
}

func (f outreachFixture) setExact(t *testing.T, lead int64, exact string) {
	t.Helper()
	f.ownerExec(t, `UPDATE site_probe SET exact_version = $2 FROM lead WHERE lead.probe_id = site_probe.id AND lead.id = $1`, lead, exact)
}

func (f outreachFixture) setForm(t *testing.T, lead int64) {
	t.Helper()
	f.ownerExec(t, `UPDATE lead SET channel = 'form' WHERE id = $1`, lead)
}

func (f outreachFixture) ownerExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if tag, err := f.owner.Exec(context.Background(), sql, args...); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("%s %v: %v (%d rows)", sql, args, err, tag.RowsAffected())
	}
}

// variedLeads makes 24 leads over every kind, major, band edge, country, channel and exact
// version the profile weighs, and returns their ids.
func variedLeads(t *testing.T, f outreachFixture) []int64 {
	t.Helper()
	ranks := []*int{intp(1), intp(9999), intp(10000), intp(99999), intp(100000), nil}
	countries := []string{"GB", "NL", "FR", ""}
	var ids []int64
	for i := range 20 {
		major := 6 + i%6 // 6 … 11
		dom := fmt.Sprintf("site%02d.localhost", i)
		id := f.siteLead(t, dom, ranks[i%len(ranks)], major, "info@"+dom)
		f.setOrgCountry(t, id, countries[i%len(countries)])
		if i%3 == 0 {
			f.setExact(t, id, fmt.Sprintf("%d.%d", major, i))
		}
		if i%4 == 1 {
			f.setForm(t, id)
		}
		ids = append(ids, id)
	}
	for i := range 4 {
		dom := fmt.Sprintf("agency%02d.localhost", i)
		id := f.agencyLead(t, dom, "hello@"+dom)
		f.setOrgCountry(t, id, countries[i])
		if i%2 == 1 {
			f.setForm(t, id)
		}
		ids = append(ids, id)
	}
	return ids
}

func candidates(t *testing.T, q leadstore.Queries, o leadstore.CandidateOptions) []leadstore.Candidate {
	t.Helper()
	o.IncludeReserved = true
	if o.Today.IsZero() {
		o.Today = priorityDay
	}
	cs, err := q.CandidateLeads(context.Background(), o)
	if err != nil {
		t.Fatalf("candidate leads: %v", err)
	}
	return cs
}

func leadIDs(cs []leadstore.Candidate) []int64 {
	ids := []int64{}
	for _, c := range cs {
		ids = append(ids, c.LeadID)
	}
	return ids
}

// TestPriorityDrift: the score SQL computes from the profile's jsonb equals the Go oracle
// on every candidate of a varied fixture, for several profiles, and the order is score
// descending, then list rank, then id.
func TestPriorityDrift(t *testing.T) {
	f := newOutreachFixture(t)
	ids := variedLeads(t, f)
	negative := testProfile.Clone()
	negative.Kinds[leadstore.LeadD10] = -100
	negative.Majors[11] = 100
	negative.Channels[leadstore.ChannelForm] = 40
	negative.RankBands = []leadconfig.RankBand{{Under: 2, Points: -50}, {Under: 100000, Points: 100}}
	negative.ExcludeCountries = nil
	profiles := map[string]leadconfig.Profile{
		"suggested": testProfile, "negative": negative,
		"kinds only": {Kinds: leadconfig.KindPoints([]string{leadstore.LeadD10})},
	}
	for name, p := range profiles {
		t.Run(name, func(t *testing.T) {
			cs := candidates(t, f.q, leadstore.CandidateOptions{Profile: p})
			if len(cs) != len(ids) {
				t.Errorf("%d candidates, want %d", len(cs), len(ids))
			}
			for i, c := range cs {
				score, why := leadstore.Score(p, c)
				if c.Score != score {
					t.Errorf("lead %d (%s %s major %d %q rank %s %s): SQL score %d, Go %d",
						c.LeadID, c.Kind, c.Country, c.Major, c.ExactVersion, rankOf(c.ListRank), c.Channel, c.Score, score)
				}
				if !slices.Equal(c.Why, why) {
					t.Errorf("lead %d: why %q, want %q", c.LeadID, c.Why, why)
				}
				if !leadstore.Offered(p, c) {
					t.Errorf("lead %d in %s offered though excluded", c.LeadID, c.Country)
				}
				if i > 0 && !before(cs[i-1], c) {
					t.Errorf("order: lead %d (%d) before lead %d (%d)", cs[i-1].LeadID, cs[i-1].Score, c.LeadID, c.Score)
				}
			}
		})
	}
}

func rankOf(r *int) string {
	if r == nil {
		return "none"
	}
	return fmt.Sprint(*r)
}

func before(a, b leadstore.Candidate) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	switch {
	case a.ListRank != nil && b.ListRank != nil && *a.ListRank != *b.ListRank:
		return *a.ListRank < *b.ListRank
	case (a.ListRank == nil) != (b.ListRank == nil):
		return b.ListRank == nil
	}
	return a.LeadID < b.LeadID
}

// TestPriorityOrderFollowsProfile: the file's profile orders the leads; a saved version
// replaces it; a reset goes back to the file's; with no profile at all, kinds_priority.
func TestPriorityOrderFollowsProfile(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	d7 := f.siteLead(t, "seven.localhost", intp(500000), 7, "info@seven.localhost")
	d10 := f.siteLead(t, "ten.localhost", intp(5), 10, "info@ten.localhost")
	agency := f.agencyLead(t, "agency.localhost", "hello@agency.localhost")
	f.setOrgCountry(t, agency, "GB")

	file := testProfile
	if got := leadIDs(candidates(t, f.q, leadstore.CandidateOptions{Profile: file})); !reflect.DeepEqual(got, []int64{d7, d10, agency}) {
		// d7: 20+30+10 = 60; d10: 10+10+10+15 = 45; agency: 15+20+10 = 45, unranked last
		t.Errorf("file profile order = %v, want %v", got, []int64{d7, d10, agency})
	}
	saved := leadconfig.Profile{Kinds: map[string]int{leadstore.LeadAgencyPartner: 50}, Countries: map[string]int{"GB": 1}}
	if _, err := f.q.SaveProfile(ctx, saved); err != nil {
		t.Fatal(err)
	}
	cs := candidates(t, f.q, leadstore.CandidateOptions{Profile: file})
	if got := leadIDs(cs); !reflect.DeepEqual(got, []int64{agency, d10, d7}) {
		t.Errorf("saved profile order = %v, want %v", got, []int64{agency, d10, d7})
	}
	if cs[0].Score != 51 || !slices.Equal(cs[0].Why, []string{"agency partner +50", "GB +1"}) {
		t.Errorf("saved profile: %d %q", cs[0].Score, cs[0].Why)
	}
	eff, fromDB, err := f.q.EffectiveProfile(ctx, file)
	if err != nil || !fromDB || eff.Kinds[leadstore.LeadAgencyPartner] != 50 {
		t.Errorf("effective = %+v %v %v", eff, fromDB, err)
	}
	// A saved version without kinds keeps the file's kind points.
	if _, err := f.q.SaveProfile(ctx, leadconfig.Profile{Majors: map[int]int{10: 100}}); err != nil {
		t.Fatal(err)
	}
	if eff, _, _ := f.q.EffectiveProfile(ctx, file); eff.Kinds[leadstore.LeadUnsupported] != 20 || eff.Majors[10] != 100 {
		t.Errorf("saved without kinds: %+v", eff)
	}
	if got := leadIDs(candidates(t, f.q, leadstore.CandidateOptions{Profile: file})); got[0] != d10 {
		t.Errorf("majors-only version: order %v, want d10 first", got)
	}
	// An invalid profile is refused and writes nothing.
	if _, err := f.q.SaveProfile(ctx, leadconfig.Profile{Majors: map[int]int{7: 500}}); !errors.Is(err, leadconfig.ErrInvalid) {
		t.Errorf("invalid profile: err = %v", err)
	}
	if _, err := f.q.ResetProfile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := leadIDs(candidates(t, f.q, leadstore.CandidateOptions{Profile: file})); !reflect.DeepEqual(got, []int64{d7, d10, agency}) {
		t.Errorf("after reset = %v, want the file's order", got)
	}
	if _, fromDB, err := f.q.EffectiveProfile(ctx, file); fromDB || err != nil {
		t.Errorf("after reset: from db %v, %v", fromDB, err)
	}
	// Cold start: no profile at all → kinds_priority, then rank, then id (as before priorities).
	got := leadIDs(candidates(t, f.q, leadstore.CandidateOptions{KindsPriority: []string{leadstore.LeadD10, leadstore.LeadAgencyPartner}}))
	if !reflect.DeepEqual(got, []int64{d10, agency, d7}) {
		t.Errorf("kinds_priority order = %v, want %v", got, []int64{d10, agency, d7})
	}
	var versions int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM priority_profile WHERE actor = 'gremy'`).Scan(&versions); err != nil || versions != 3 {
		t.Errorf("versions = %d (%v), want 3 (two saved, one reset)", versions, err)
	}
}

// TestPriorityPinDeferSkip: a pin puts a lead first (newest pin first), clear undoes it, a
// defer hides the lead through its date and brings it back the day after, and a skip moves
// only a new lead to skipped, for good.
func TestPriorityPinDeferSkip(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "a.localhost", intp(1), 7, "info@a.localhost")
	b := f.siteLead(t, "b.localhost", intp(2), 7, "info@b.localhost")
	c := f.siteLead(t, "c.localhost", intp(3), 7, "info@c.localhost")
	order := func(today time.Time) []int64 {
		t.Helper()
		return leadIDs(candidates(t, f.q, leadstore.CandidateOptions{Profile: testProfile, Today: today}))
	}
	if got := order(priorityDay); !reflect.DeepEqual(got, []int64{a, b, c}) {
		t.Fatalf("start = %v", got)
	}
	for _, id := range []int64{b, c} {
		if err := f.q.PinLead(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	cs := candidates(t, f.q, leadstore.CandidateOptions{Profile: testProfile})
	if got := leadIDs(cs); !reflect.DeepEqual(got, []int64{c, b, a}) {
		t.Errorf("pinned b then c = %v, want c, b, a", got)
	}
	if !cs[0].Pinned || cs[0].Why[0] != "pinned" || cs[2].Pinned {
		t.Errorf("pinned flags: %+v %q / %+v", cs[0].Pinned, cs[0].Why, cs[2].Pinned)
	}
	if err := f.q.ClearLeadPriority(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got := order(priorityDay); !reflect.DeepEqual(got, []int64{b, a, c}) {
		t.Errorf("c cleared = %v, want b, a, c", got)
	}
	// Defer a until two days on: hidden through that day, back the day after.
	until := priorityDay.AddDate(0, 0, 2)
	if err := f.q.DeferLead(ctx, a, until); err != nil {
		t.Fatal(err)
	}
	for _, day := range []time.Time{priorityDay, until} {
		if got := order(day); slices.Contains(got, a) {
			t.Errorf("on %s a deferred until %s is offered: %v", day.Format(time.DateOnly), until.Format(time.DateOnly), got)
		}
	}
	if got := order(until.AddDate(0, 0, 1)); !reflect.DeepEqual(got, []int64{b, a, c}) {
		t.Errorf("the day after = %v, want b, a, c", got)
	}
	// A pin after a defer wins: the latest row per lead decides.
	if err := f.q.PinLead(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got := order(priorityDay); got[0] != a {
		t.Errorf("pin after defer = %v, want a first", got)
	}

	// Skip: new → skipped, never offered again; a batched lead does not move.
	moved, err := f.q.SkipLead(ctx, c)
	if err != nil || !moved || f.leadState(t, c) != leadstore.LeadSkipped {
		t.Errorf("skip c: %v %v, state %s", moved, err, f.leadState(t, c))
	}
	if got := order(until.AddDate(0, 1, 0)); slices.Contains(got, c) {
		t.Errorf("skipped lead offered: %v", got)
	}
	f.batch(t, false, b)
	if moved, err := f.q.SkipLead(ctx, b); err != nil || moved || f.leadState(t, b) != leadstore.LeadBatched {
		t.Errorf("skip a batched lead: %v %v, state %s", moved, err, f.leadState(t, b))
	}
	var ev string
	if err := f.owner.QueryRow(ctx, `SELECT reason || ' ' || actor FROM lead_event WHERE lead_id = $1 AND state = 'skipped'`, c).Scan(&ev); err != nil || ev != "skipped from the queue gremy" {
		t.Errorf("skip event = %q, %v", ev, err)
	}
	for name, err := range map[string]error{
		"pin":   f.q.PinLead(ctx, 999),
		"defer": f.q.DeferLead(ctx, 999, until),
		"clear": f.q.ClearLeadPriority(ctx, 999),
	} {
		if !errors.Is(err, leadstore.ErrLeadNotFound) {
			t.Errorf("%s an unknown lead: err = %v", name, err)
		}
	}
	if _, err := f.q.SkipLead(ctx, 999); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("skip an unknown lead: err = %v", err)
	}
}

// TestPriorityExcludeCountries: an excluded country is never offered, pinned or not.
func TestPriorityExcludeCountries(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	de := f.siteLead(t, "de.localhost", intp(1), 7, "info@de.localhost")
	gb := f.siteLead(t, "gb.localhost", intp(2), 7, "info@gb.localhost")
	f.setOrgCountry(t, de, "DE")
	f.setOrgCountry(t, gb, "GB")
	if err := f.q.PinLead(ctx, de); err != nil {
		t.Fatal(err)
	}
	if got := leadIDs(candidates(t, f.q, leadstore.CandidateOptions{Profile: testProfile})); !reflect.DeepEqual(got, []int64{gb}) {
		t.Errorf("candidates = %v, want only %d", got, gb)
	}
	if got := leadIDs(candidates(t, f.q, leadstore.CandidateOptions{})); !reflect.DeepEqual(got, []int64{de, gb}) {
		t.Errorf("no exclusion: %v", got)
	}
}

// TestNextUp: the console queue is CandidateLeads' order, 50 at a time by default, and
// never a reserved-TLD lead unless asked.
func TestNextUp(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	for i := range 52 {
		dom := fmt.Sprintf("q%02d.localhost", i)
		f.siteLead(t, dom, intp(i+1), 7, "info@"+dom)
	}
	o := leadstore.CandidateOptions{Profile: testProfile, Today: priorityDay, IncludeReserved: true}
	all, err := f.q.CandidateLeads(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	next, err := f.q.NextUp(ctx, o, 0)
	if err != nil || len(next) != leadstore.NextUpPage || !reflect.DeepEqual(leadIDs(next), leadIDs(all[:leadstore.NextUpPage])) {
		t.Errorf("NextUp = %d leads (%v), want the first %d of CandidateLeads", len(next), err, leadstore.NextUpPage)
	}
	if next, err := f.q.NextUp(ctx, o, 3); err != nil || len(next) != 3 || next[0].Why == nil {
		t.Errorf("NextUp(3) = %+v, %v", next, err)
	}
	o.IncludeReserved = false
	if next, err := f.q.NextUp(ctx, o, 0); err != nil || len(next) != 0 {
		t.Errorf("NextUp without reserved = %d, %v", len(next), err)
	}
}

// TestPriorityTablesAppendOnly: the app role inserts and reads the priority tables and never
// rewrites or deletes a row.
func TestPriorityTablesAppendOnly(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "a.localhost", intp(1), 7, "info@a.localhost")
	if _, err := f.q.SaveProfile(ctx, testProfile); err != nil {
		t.Fatal(err)
	}
	if err := f.q.PinLead(ctx, lead); err != nil {
		t.Fatal(err)
	}
	app := storetest.AppConn(t)
	for _, q := range []string{
		"UPDATE priority_profile SET actor = 'system'", "DELETE FROM priority_profile", "TRUNCATE priority_profile",
		"UPDATE lead_priority SET action = 'clear'", "DELETE FROM lead_priority", "TRUNCATE lead_priority",
	} {
		if _, err := app.Exec(ctx, q); sqlState(err) != "42501" {
			t.Errorf("%s as app: err = %v, want 42501", q, err)
		}
	}
	// A defer needs its date and nothing else carries one.
	for _, q := range []string{
		fmt.Sprintf("INSERT INTO lead_priority (lead_id, action, actor) VALUES (%d, 'defer', 'gremy')", lead),
		fmt.Sprintf("INSERT INTO lead_priority (lead_id, action, until, actor) VALUES (%d, 'pin', '2026-12-01', 'gremy')", lead),
		fmt.Sprintf("INSERT INTO lead_priority (lead_id, action, actor) VALUES (%d, 'boost', 'gremy')", lead),
		"INSERT INTO priority_profile (profile, actor) VALUES ('[]', 'gremy')",
	} {
		if _, err := app.Exec(ctx, q); sqlState(err) != "23514" {
			t.Errorf("%s: err = %v, want 23514", q, err)
		}
	}
}

// oracleCandidatesSQL is CandidateLeads as it was before the set-based rewrite: the
// suppression and reached-domain checks as correlated anti-joins per candidate. It is
// frozen here as the oracle of TestCandidateLeadsSetBasedOracle; never edit it to make the
// new query pass. $1 limit, $2 profile jsonb, $3 reserved-domain regexp, $4 include
// reserved, $5 today.
const oracleCandidatesSQL = `SELECT l.id AS lead_id, l.org_id, o.domain, o.name AS org_name, o.country, l.kind, l.channel,
	   l.route_id, coalesce(r.kind, '') AS route_kind, coalesce(r.value, '') AS route,
	   l.probe_id, l.agency_finding_id, s.list_rank,
	   coalesce(p.major, 0) AS major, coalesce(p.exact_version, '') AS exact_version,
	   coalesce(p.site_name, '') AS site_name,
	   coalesce(af.phrase, '') AS phrase, coalesce(af.url, '') AS finding_url,
	   sc.score, coalesce(pr.action = 'pin', false) AS pinned
	 FROM lead l
	 CROSS JOIN (SELECT $2::jsonb AS pf) pf
	 JOIN org o ON o.id = l.org_id
	 LEFT JOIN contact_route r ON r.id = l.route_id
	 LEFT JOIN site_probe p ON p.id = l.probe_id
	 LEFT JOIN site s ON s.id = coalesce(p.site_id, o.site_id)
	 LEFT JOIN agency_finding af ON af.id = l.agency_finding_id
	 LEFT JOIN LATERAL (
	   SELECT lp.id, lp.action, lp.until FROM lead_priority lp
	   WHERE lp.lead_id = l.id ORDER BY lp.id DESC LIMIT 1
	 ) pr ON true
	 CROSS JOIN LATERAL (
	   SELECT CASE WHEN r.kind = 'email' THEN r.value ELSE '' END AS addr
	 ) a
	 CROSS JOIN LATERAL (
	   SELECT nullif(lower(split_part(a.addr, '@', 2)), '') AS adom
	 ) ad
	 CROSS JOIN LATERAL (SELECT coalesce((pf.pf->'kinds'->>l.kind)::int, 0)
	   + coalesce((pf.pf->'majors'->>p.major::text)::int, 0)
	   + CASE WHEN coalesce(p.exact_version, '') <> '' THEN coalesce((pf.pf->>'exact_version')::int, 0) ELSE 0 END
	   + coalesce((pf.pf->'countries'->>o.country)::int, 0)
	   + coalesce((pf.pf->'channels'->>l.channel)::int, 0)
	   + coalesce((SELECT (b->>'points')::int FROM jsonb_array_elements(pf.pf->'rank_bands') b
	       WHERE s.list_rank < (b->>'under')::int ORDER BY (b->>'under')::int LIMIT 1), 0) AS score) sc
	 WHERE l.state = 'new' AND l.channel <> 'none'
	   AND (l.probe_id IS NULL OR p.id = s.last_probe_id)
	   AND l.kind NOT IN ('selftest', 'hand') AND o.kind <> 'selftest'
	   AND ($4 OR (o.domain !~ $3 AND coalesce(ad.adom, '') !~ $3))
	   AND NOT coalesce(pr.action = 'defer' AND pr.until >= coalesce($5::date, current_date), false)
	   AND NOT pf.pf->'exclude_countries' @> to_jsonb(o.country)
	   AND NOT EXISTS (SELECT 1 FROM suppression x WHERE
	     (x.kind = 'address' AND x.value = lower(a.addr))
	     OR (x.kind = 'domain' AND (
	       x.value = lower(o.domain) OR right(lower(o.domain), length(x.value) + 1) = '.' || x.value
	       OR x.value = lower(split_part(a.addr, '@', 2))
	       OR right(lower(split_part(a.addr, '@', 2)), length(x.value) + 1) = '.' || x.value)))
	   AND NOT EXISTS (
	     SELECT 1 FROM lead l2
	     JOIN org o2 ON o2.id = l2.org_id
	     LEFT JOIN contact_route r2 ON r2.id = l2.route_id AND r2.kind = 'email'
	     WHERE l2.id <> l.id
	       AND l2.state IN ('batched', 'contacted', 'replied', 'bounced', 'opted_out')
	       AND (o2.domain IN (o.domain, ad.adom)
	         OR nullif(lower(split_part(coalesce(r2.value, ''), '@', 2)), '') IN (o.domain, ad.adom)))
	 ORDER BY coalesce(pr.action = 'pin', false) DESC, CASE WHEN pr.action = 'pin' THEN pr.id END DESC NULLS LAST,
	   sc.score DESC, s.list_rank NULLS LAST, l.id
	 LIMIT $1`

// TestCandidateLeadsSetBasedOracle: the set-based CandidateLeads blocks exactly the leads
// the old correlated query blocked, and returns the same rows in the same order: suffix
// suppression on the org's domain and on the address's domain (a parent at a label
// boundary only), address suppression in any case, a domain or an address domain another
// lead already reached, reserved names, pins and defers.
func TestCandidateLeadsSetBasedOracle(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	kept := map[string]int64{}
	add := func(dom, email string) {
		t.Helper()
		kept[dom] = f.siteLead(t, dom, intp(len(kept)+1), 6+len(kept)%5, email)
	}
	add("plain.localhost", "info@plain.localhost")
	add("x.test", "info@xmail.localhost")                        // org domain under a suppressed parent (a TLD here)
	add("deepaddr.localhost", "web@a.b.hosted.localhost")        // address domain's grandparent suppressed
	add("notbydom.localhost", "info@notbydom.localhost")         // no label boundary: kept
	add("bydom.localhost", "info@mailer-bd.localhost")           // the domain itself
	add("addr-eq.localhost", "info@Hosted.localhost")            // address domain itself suppressed
	add("addrdom.localhost", "web@Mail.Hosted.localhost")        // address domain's parent suppressed
	add("byaddr.localhost", "INFO@byaddr.localhost")             // address suppressed, any case
	add("otheraddr.localhost", "sales@byaddr.localhost")         // same domain, other address: kept
	add("reachdom.localhost", "info@reachmail.localhost")        // org domain reached by another lead
	add("routes-to-reached.localhost", "web@agency-r.localhost") // address domain reached
	add("routed.localhost", "info@shared.localhost")             // address domain a reached lead's address shares
	add("fixture.example", "info@fixture.example")               // reserved
	add("addr-reserved.localhost", "info@mail.example")          // address on a reserved TLD
	add("pinned.localhost", "info@pinned.localhost")
	add("deferred.localhost", "info@deferred.localhost")
	add("noroute.localhost", "") // channel none

	// Reached leads (org domains are unique, so a domain is reached through an address):
	// one at an address on reachdom's own domain (in another case), an agency whose domain a
	// candidate's address is at, and one whose address shares routed's address domain.
	reachedAt := f.agencyLead(t, "faraway1.localhost", "someone@Reachdom.localhost")
	f.setState(t, reachedAt, leadstore.LeadContacted)
	reachedAgency := f.agencyLead(t, "agency-r.localhost", "hello@agency-r.localhost")
	f.setState(t, reachedAgency, leadstore.LeadBounced)
	reachedShared := f.agencyLead(t, "faraway2.localhost", "boss@shared.localhost")
	f.setState(t, reachedShared, leadstore.LeadBatched)
	// Leads in other states reach nothing.
	notReached := f.agencyLead(t, "faraway3.localhost", "x@plain.localhost")
	f.setState(t, notReached, leadstore.LeadSkipped)

	// The suppressions are written by the owner: Suppress also moves the leads it matches to
	// suppressed, which would leave the query nothing to decide. 'test' is a bare TLD, which
	// Suppress refuses, so the suffix rule on an org domain is pinned too.
	for _, sup := range [][2]string{
		{"domain", "bydom.localhost"}, {"domain", "hosted.localhost"}, {"domain", "test"},
		{"address", "info@byaddr.localhost"},
	} {
		f.ownerExec(t, `INSERT INTO suppression (kind, value, reason) VALUES ($1, $2, 'manual')`, sup[0], sup[1])
	}
	if err := f.q.PinLead(ctx, kept["pinned.localhost"]); err != nil {
		t.Fatal(err)
	}
	if err := f.q.DeferLead(ctx, kept["deferred.localhost"], priorityDay.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}

	pf, err := json.Marshal(testProfile)
	if err != nil {
		t.Fatal(err)
	}
	reserved := `\.(` + strings.Join(leadstore.ReservedTLDs, "|") + `)$`
	app := storetest.AppConn(t)
	for _, tc := range []struct {
		name string
		o    leadstore.CandidateOptions
	}{
		{"reserved included", leadstore.CandidateOptions{Profile: testProfile, Today: priorityDay, IncludeReserved: true}},
		{"reserved left out", leadstore.CandidateOptions{Profile: testProfile, Today: priorityDay}},
		{"defer over", leadstore.CandidateOptions{Profile: testProfile, Today: priorityDay.AddDate(0, 0, 2), IncludeReserved: true}},
		{"limit", leadstore.CandidateOptions{Profile: testProfile, Today: priorityDay, IncludeReserved: true, Limit: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.q.CandidateLeads(ctx, tc.o)
			if err != nil {
				t.Fatal(err)
			}
			var lim *int
			if tc.o.Limit > 0 {
				lim = &tc.o.Limit
			}
			rows, err := app.Query(ctx, oracleCandidatesSQL, lim, string(pf), reserved, tc.o.IncludeReserved, tc.o.Today.Format(time.DateOnly))
			if err != nil {
				t.Fatal(err)
			}
			want, err := pgx.CollectRows(rows, pgx.RowToStructByName[leadstore.Candidate])
			if err != nil {
				t.Fatal(err)
			}
			for i := range got {
				got[i].Why = nil
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("set-based query differs from the oracle:\n got %v\nwant %v", leadIDs(got), leadIDs(want))
			}
		})
	}

	// The semantics, spelled out: exactly these leads are offered (reserved included, before
	// the defer ends).
	got := leadIDs(candidates(t, f.q, leadstore.CandidateOptions{Profile: testProfile, IncludeReserved: true}))
	var names []string
	for dom, id := range kept {
		if slices.Contains(got, id) {
			names = append(names, dom)
		}
	}
	slices.Sort(names)
	want := []string{"addr-reserved.localhost", "fixture.example", "notbydom.localhost", "otheraddr.localhost", "pinned.localhost", "plain.localhost"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("offered = %q, want %q", names, want)
	}
}
