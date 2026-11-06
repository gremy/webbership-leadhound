//go:build integration

package leadstore_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestForgetOrgInstagram is Decision 9: erasing an org moves its leads off its Instagram
// routes, empties its Instagram messages' target, deletes its Instagram routes and every
// ig_* row of its handles, and leaves another org's alone. A sweep over every table finds none
// of its handles afterwards, a later import cannot re-attach anything (IGLeadKeys leaves the
// org out), and a second run counts zero. Every handle here is invented.
func TestForgetOrgInstagram(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	gone := f.igChannelLead(t, "erased.example", "gone_cafe.example")
	keep := f.igChannelLead(t, "kept.example", "keep_cafe.example")
	goneOrg, keepOrg := f.leadOf(t, gone).OrgID, f.leadOf(t, keep).OrgID
	// The erased org also holds a route found only by a page (no match yet) and its loyalty
	// vendor's account, which is not its own.
	for _, v := range []string{"gone_route.example", "stampino.app"} {
		if _, err := f.q.InsertRoute(ctx, leadstore.RouteRow{OrgID: goneOrg, Kind: leadstore.RouteInstagram, Value: v, FetchID: f.fid, DerivedBy: "enrich@1"}); err != nil {
			t.Fatal(err)
		}
	}
	f.ownerRun(t, `INSERT INTO org_fact (org_id, kind, value, normalized, fetch_id, derived_by) VALUES ($1, 'social', $2, $2, $3, 'enrich@1')`,
		goneOrg, igURL("gone_cafe.example"), f.fid)

	// One DM each, written directly: the erasure blanks only the erased org's target.
	batch := ownerInt(t, f.owner, `INSERT INTO outreach_batch (shadow, content_sha256, templates_sha256, manual) VALUES (false, '\x00', '\x00', true) RETURNING id`)
	const addMessage = `INSERT INTO outreach_message (batch_id, lead_id, step, channel, to_address, body_text, slots, mailbox_id)
		VALUES ($1, $2, 0, 'instagram', $3, 'dm text', '{}', $4) RETURNING id`
	goneMsg := ownerInt(t, f.owner, addMessage, batch, gone, "https://www.instagram.com/gone_cafe.example/", f.mailbox(t))
	keepMsg := ownerInt(t, f.owner, addMessage, batch, keep, "https://www.instagram.com/keep_cafe.example/", f.mailbox(t))

	// Captures. friend is read from the erased café's own following list; hinted names the
	// erased lead only as its capture's hint (it was read for that lead, so it goes too), and
	// hinted_other names it as a hint too, but its latest match is the kept lead's: it stays,
	// its hint cleared.
	at := time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC)
	profiles := map[string]int64{}
	for i, h := range []string{"gone_cafe.example", "gone_route.example", "gone_tie.example", "gone_name.example",
		"gone_old.example", "keep_cafe.example", "stampino.app", "gone_friend.example", "hinted.example", "hinted_other.example"} {
		p := igProfile(h, byte(i+1))
		switch h {
		case "gone_friend.example":
			p.Source, p.SourceOf = leadstore.IGSourceFollowing, "gone_cafe.example"
		case "hinted.example", "hinted_other.example":
			p.SourceLeadID = gone
		}
		id, _, err := f.q.InsertIGProfile(ctx, p)
		if err != nil {
			t.Fatalf("profile %s: %v", h, err)
		}
		profiles[h] = id
	}
	for _, h := range []string{"gone_cafe.example", "gone_friend.example", "keep_cafe.example"} {
		if _, err := f.q.AddIGJudgment(ctx, leadstore.IGJudgment{ProfileID: profiles[h], Question: leadstore.IGPlace,
			Choice: "unclear", Source: leadstore.IGFromRule, Detail: "no text"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{"gone_cafe.example", "keep_cafe.example"} {
		if _, err := f.q.AddIGHint(ctx, leadstore.IGHint{Handle: h, Kind: leadstore.IGHintFollowedByVendor, Value: "stampino", CapturedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.q.InsertIGFollows(ctx, "stampino.app", leadstore.IGFollowing, at, igSHA(0xf1), []leadstore.IGFollowRow{
		{Handle: "gone_cafe.example", FullName: "Cafe Gone Exemplu"}, {Handle: "keep_cafe.example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.InsertIGFollows(ctx, "gone_cafe.example", leadstore.IGFollowing, at, igSHA(0xf2), []leadstore.IGFollowRow{{Handle: "gone_friend.example"}}); err != nil {
		t.Fatal(err)
	}
	// 00033: the erased café on the vendor's followers list; its comment on the vendor's post;
	// a touch where it is the vendor (its own post tagging another account); the kept café's
	// comment, which stays; a class and a call of yours on each lead.
	if _, err := f.q.InsertIGFollows(ctx, "stampino.app", leadstore.IGFollowers, at, igSHA(0xf3), []leadstore.IGFollowRow{
		{Handle: "gone_cafe.example"}, {Handle: "keep_cafe.example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.InsertIGTouches(ctx, "stampino.app", at, igSHA(0xf4), []leadstore.IGTouch{
		{Handle: "gone_cafe.example", Kind: leadstore.IGTouchComment, Ref: "c100", PostCode: "PostGone", At: at, Text: "Cat costa?"},
		{Handle: "keep_cafe.example", Kind: leadstore.IGTouchComment, Ref: "c101", PostCode: "PostGone", At: at}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.InsertIGTouches(ctx, "gone_cafe.example", at, igSHA(0xf5), []leadstore.IGTouch{
		{Handle: "gone_tagfan.example", Kind: leadstore.IGTouchUsertag, Ref: "OwnGone", PostCode: "OwnGone", At: at}}); err != nil {
		t.Fatal(err)
	}
	for _, l := range []int64{gone, keep} {
		f.ownerRun(t, `INSERT INTO lead_intent (lead_id, class, overlap, score, reason) VALUES ($1, 'curious', 1, 1, 'commented on a vendor post')`, l)
		f.ownerRun(t, `INSERT INTO lead_intent_call (lead_id, class, actor) VALUES ($1, 'paper', 'gremy')`, l)
	}

	match := func(m leadstore.IGMatch) {
		t.Helper()
		if m.Actor == "" {
			m.Actor = leadstore.ActorSystem
		}
		if _, err := f.q.SetIGMatch(ctx, m); err != nil {
			t.Fatalf("match %+v: %v", m, err)
		}
	}
	match(leadstore.IGMatch{Handle: "gone_cafe.example", LeadID: gone, Tier: leadstore.IGTierHandle})
	match(leadstore.IGMatch{Handle: "gone_tie.example", Tier: leadstore.IGTierName, Candidates: []int64{gone, keep}})
	match(leadstore.IGMatch{Handle: "gone_name.example", LeadID: gone, Tier: leadstore.IGTierName})
	// Once weakly the erased lead's, now the operator's call for the kept one: only the old row goes.
	match(leadstore.IGMatch{Handle: "gone_old.example", LeadID: gone, Tier: leadstore.IGTierName})
	match(leadstore.IGMatch{Handle: "gone_old.example", LeadID: keep, Tier: leadstore.IGTierOperator, Actor: leadstore.ActorGremy})
	match(leadstore.IGMatch{Handle: "keep_cafe.example", LeadID: keep, Tier: leadstore.IGTierHandle})
	match(leadstore.IGMatch{Handle: "hinted_other.example", LeadID: keep, Tier: leadstore.IGTierHandle})
	for _, h := range []string{"gone_tie.example", "gone_name.example", "gone_old.example"} {
		if _, err := f.q.QueueIGReview(ctx, h); err != nil {
			t.Fatal(err)
		}
	}

	erased := []string{"gone_cafe.example", "gone_route.example", "gone_tie.example", "gone_name.example", "gone_friend.example",
		"hinted.example", "gone_tagfan.example"}
	before := dumpLeadTables(t, f.owner)
	for _, h := range erased {
		if !strings.Contains(before, h) {
			t.Fatalf("fixture: %s is not stored before the erasure, so the sweep proves nothing", h)
		}
	}

	owner := leadstore.NewQueries(f.owner)
	got, err := owner.ForgetOrg(ctx, goneOrg, "erasure request by email")
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := owner.ForgetOrgPages(ctx, goneOrg); err != nil { // `leads forget` runs both
		t.Fatal(err)
	}
	ig := [10]int64{got.IGLeadRoutes, got.IGTargets, got.IGRoutes, got.IGJudgments, got.IGHints,
		got.IGMatches, got.IGReviews, got.IGProfiles, got.IGFollows, got.IGLeadHints}
	// Routes: gone_cafe, gone_route and the vendor's. Matches: gone_cafe, the tie, gone_name and
	// gone_old's old row. Profiles: the four erased handles', friend's and hinted's. Follows: the
	// vendor's following row and its followers row of gone_cafe, and gone_cafe's own list. Lead
	// hints: hinted_other's.
	if want := [10]int64{1, 1, 3, 2, 1, 4, 2, 6, 3, 1}; ig != want {
		t.Errorf("instagram counts = %v, want %v (%+v)", ig, want, got)
	}
	// Touches: gone_cafe's comment, and its own post's tag of gone_tagfan. One class, one call.
	if in := [3]int64{got.IGTouches, got.Intents, got.IntentCalls}; in != [3]int64{2, 1, 1} {
		t.Errorf("touch and intent counts = %v, want [2 1 1]", in)
	}
	// The sweep over the four 00033 tables: none names the erased org's handles or leads.
	for _, c := range []struct {
		what string
		sql  string
	}{
		{"ig_follow rows (either direction)", `SELECT count(*) FROM ig_follow WHERE handle = ANY($1) OR list_owner = ANY($1)`},
		{"ig_vendor_touch rows", `SELECT count(*) FROM ig_vendor_touch WHERE handle = ANY($1) OR vendor_handle = ANY($1)`},
	} {
		if n := ownerInt(t, f.owner, c.sql, erased); n != 0 {
			t.Errorf("%s naming the erased handles: %d, want 0", c.what, n)
		}
	}
	for _, tb := range []string{"lead_intent", "lead_intent_call"} {
		if n := ownerInt(t, f.owner, `SELECT count(*) FROM `+tb+` WHERE lead_id IN (SELECT id FROM lead WHERE org_id = $1)`, goneOrg); n != 0 {
			t.Errorf("%s rows of the erased org's leads: %d, want 0", tb, n)
		}
		if n := ownerInt(t, f.owner, `SELECT count(*) FROM `+tb+` WHERE lead_id = $1`, keep); n != 1 {
			t.Errorf("%s rows of the kept lead: %d, want 1", tb, n)
		}
	}

	after := dumpLeadTables(t, f.owner)
	for _, h := range erased {
		for _, l := range strings.Split(after, "\n") {
			if strings.Contains(l, h) {
				t.Errorf("%s left after the erasure: %s", h, l)
			}
		}
	}
	for _, c := range []struct {
		what string
		sql  string
		want int64
	}{
		{"matches naming the erased lead", `SELECT count(*) FROM ig_match WHERE lead_id = $1 OR $1 = ANY(candidate_ids)`, 0},
		{"lead hints naming it", `SELECT count(*) FROM ig_profile WHERE source_lead_id = $1`, 0},
		{"its lead on a route", `SELECT count(*) FROM lead WHERE id = $1 AND (route_id IS NOT NULL OR channel <> 'none')`, 0},
		{"its routes", `SELECT count(*) FROM contact_route WHERE org_id = (SELECT org_id FROM lead WHERE id = $1)`, 0},
	} {
		if n := ownerInt(t, f.owner, c.sql, gone); n != c.want {
			t.Errorf("%s: %d, want %d", c.what, n, c.want)
		}
	}
	// The kept org, the vendor and the other accounts are untouched.
	for _, c := range []struct {
		what string
		sql  string
		want int64
	}{
		{"kept route", `SELECT count(*) FROM contact_route WHERE org_id = $1 AND kind = 'instagram'`, 1},
		{"kept lead on it", `SELECT count(*) FROM lead WHERE org_id = $1 AND channel = 'instagram' AND route_id IS NOT NULL`, 1},
	} {
		if n := ownerInt(t, f.owner, c.sql, keepOrg); n != c.want {
			t.Errorf("%s: %d, want %d", c.what, n, c.want)
		}
	}
	for _, c := range []struct {
		what string
		sql  string
		want int64
	}{
		{"kept and vendor profiles", `SELECT count(*) FROM ig_profile WHERE handle IN ('keep_cafe.example', 'stampino.app', 'hinted_other.example', 'gone_old.example')`, 4},
		{"kept matches", `SELECT count(*) FROM ig_match WHERE handle IN ('keep_cafe.example', 'gone_old.example', 'hinted_other.example')`, 3},
		{"kept follows", `SELECT count(*) FROM ig_follow WHERE list_owner = 'stampino.app' AND handle = 'keep_cafe.example'`, 2},
		{"kept touch", `SELECT count(*) FROM ig_vendor_touch WHERE vendor_handle = 'stampino.app' AND handle = 'keep_cafe.example'`, 1},
		{"kept judgment", `SELECT count(*) FROM ig_judgment j JOIN ig_profile p ON p.id = j.profile_id WHERE p.handle = 'keep_cafe.example'`, 1},
		{"kept hint", `SELECT count(*) FROM ig_signal_hint WHERE handle = 'keep_cafe.example'`, 1},
		{"kept review", `SELECT count(*) FROM ig_review_status WHERE handle = 'gone_old.example'`, 1},
	} {
		if n := ownerInt(t, f.owner, c.sql); n != c.want {
			t.Errorf("%s: %d, want %d", c.what, n, c.want)
		}
	}
	if to := ownerString(t, f.owner, `SELECT to_address FROM outreach_message WHERE id = $1`, goneMsg); to != "" {
		t.Errorf("erased DM target %q", to)
	}
	if to := ownerString(t, f.owner, `SELECT to_address FROM outreach_message WHERE id = $1`, keepMsg); to != "https://www.instagram.com/keep_cafe.example/" {
		t.Errorf("kept DM target %q", to)
	}

	// A later import matches only against IGLeadKeys: the erased org is never among them.
	keys, err := f.q.IGLeadKeys(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, k := range keys {
		ids = append(ids, k.LeadID)
		if k.OrgID == goneOrg {
			t.Errorf("erased org's lead %d is a lead key: %+v", k.LeadID, k)
		}
	}
	if !slices.Equal(ids, []int64{keep}) {
		t.Errorf("lead keys %v, want only the kept lead %d", ids, keep)
	}

	again, err := owner.ForgetOrg(ctx, goneOrg, "erasure request by email")
	if err != nil {
		t.Fatal(err)
	}
	if ig := [10]int64{again.IGLeadRoutes, again.IGTargets, again.IGRoutes, again.IGJudgments, again.IGHints,
		again.IGMatches, again.IGReviews, again.IGProfiles, again.IGFollows, again.IGLeadHints}; ig != [10]int64{} {
		t.Errorf("a second run counts %v, want zero", ig)
	}
	if in := [3]int64{again.IGTouches, again.Intents, again.IntentCalls}; in != [3]int64{} {
		t.Errorf("a second run counts touches and intents %v, want zero", in)
	}
}
