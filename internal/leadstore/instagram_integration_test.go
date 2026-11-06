//go:build integration

package leadstore_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// Migration 00031: the Instagram store. Every handle here is invented (privacycheck's
// Instagram allow list: a handle naming example or exemplu).

// igSHA is a 32-byte hash made from one byte, so each capture in a test is told apart.
func igSHA(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// igProfile is a profile capture of handle with hash byte b, reached from a lead.
func igProfile(handle string, b byte) leadstore.IGProfile {
	return leadstore.IGProfile{Handle: handle, Source: leadstore.IGSourceLead, CaptureSHA: igSHA(b),
		CapturedAt: time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC), Bio: "Cafea de specialitate", Captions: []string{"espresso"}}
}

// TestIGProfileStoredOnce: a capture is stored once, whatever the form of its handle; a new
// capture of the same handle is a new row; a bad row is ErrBadIG and stores nothing.
func TestIGProfileStoredOnce(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	first, isNew, err := f.q.InsertIGProfile(ctx, igProfile("@Cafe.Exemplu", 1))
	if err != nil || !isNew {
		t.Fatalf("first capture: %d %v %v", first, isNew, err)
	}
	again, isNew, err := f.q.InsertIGProfile(ctx, igProfile("cafe.exemplu", 1))
	if err != nil || isNew || again != first {
		t.Fatalf("the same capture again: %d %v %v, want %d known", again, isNew, err, first)
	}
	followers := 1200
	p := igProfile("https://www.instagram.com/cafe.exemplu/", 2)
	p.Followers, p.FullName, p.Category = &followers, "Cafe Exemplu", "Coffee shop"
	second, isNew, err := f.q.InsertIGProfile(ctx, p)
	if err != nil || !isNew || second == first {
		t.Fatalf("a new capture: %d %v %v", second, isNew, err)
	}
	missing := leadstore.IGProfile{Handle: "gone.example", Outcome: leadstore.IGOutcomeMissing, Source: leadstore.IGSourceLead,
		CaptureSHA: igSHA(3), CapturedAt: time.Now()}
	if _, _, err := f.q.InsertIGProfile(ctx, missing); err != nil {
		t.Fatalf("a missing handle: %v", err)
	}
	for name, bad := range map[string]leadstore.IGProfile{
		"not a handle":       {Handle: "cafe exemplu", Source: leadstore.IGSourceLead, CaptureSHA: igSHA(9), CapturedAt: time.Now()},
		"a post":             {Handle: "https://www.instagram.com/p/AbC/", Source: leadstore.IGSourceLead, CaptureSHA: igSHA(9), CapturedAt: time.Now()},
		"unknown source":     {Handle: "a.example", Source: "ad", CaptureSHA: igSHA(9), CapturedAt: time.Now()},
		"list with no owner": {Handle: "a.example", Source: leadstore.IGSourceFollowing, CaptureSHA: igSHA(9), CapturedAt: time.Now()},
		"owner with no list": {Handle: "a.example", Source: leadstore.IGSourceLead, SourceOf: "vendor.example", CaptureSHA: igSHA(9), CapturedAt: time.Now()},
		"short hash":         {Handle: "a.example", Source: leadstore.IGSourceLead, CaptureSHA: []byte{1}, CapturedAt: time.Now()},
		"no capture time":    {Handle: "a.example", Source: leadstore.IGSourceLead, CaptureSHA: igSHA(9)},
		"thirteen captions":  {Handle: "a.example", Source: leadstore.IGSourceLead, CaptureSHA: igSHA(9), CapturedAt: time.Now(), Captions: make([]string, 13)},
		"unknown outcome":    {Handle: "a.example", Outcome: "private", Source: leadstore.IGSourceLead, CaptureSHA: igSHA(9), CapturedAt: time.Now()},
		"owner not a handle": {Handle: "a.example", Source: leadstore.IGSourceFollowing, SourceOf: "not one", CaptureSHA: igSHA(9), CapturedAt: time.Now()},
	} {
		if _, _, err := f.q.InsertIGProfile(ctx, bad); !errors.Is(err, leadstore.ErrBadIG) {
			t.Errorf("%s: err = %v, want ErrBadIG", name, err)
		}
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM ig_profile`); n != 3 {
		t.Errorf("profiles = %d, want 3", n)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM ig_profile WHERE handle = 'cafe.exemplu' AND followers = 1200 AND full_name = 'Cafe Exemplu'`); n != 1 {
		t.Error("the second capture's counts and name were not stored")
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM ig_profile WHERE bio IS NULL AND outcome = 'missing'`); n != 1 {
		t.Error("a missing handle: want its row with no text")
	}
}

// TestIGFollowsStoredOnce: a following list is stored once per capture; one bad handle stores
// nothing of the call.
func TestIGFollowsStoredOnce(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	at := time.Now()
	rows := []leadstore.IGFollowRow{{Handle: "cafe.exemplu", FullName: "Cafe Exemplu"}, {Handle: "@Bean.Example"}, {Handle: "bean.example"}}
	n, err := f.q.InsertIGFollows(ctx, "Vendor.Example", leadstore.IGFollowing, at, igSHA(1), rows)
	if err != nil || n != 2 {
		t.Fatalf("first list: %d %v, want 2 (a handle twice is one row)", n, err)
	}
	if n, err := f.q.InsertIGFollows(ctx, "vendor.example", leadstore.IGFollowing, at, igSHA(1), rows); err != nil || n != 0 {
		t.Fatalf("the same list again: %d %v, want 0", n, err)
	}
	if _, err := f.q.InsertIGFollows(ctx, "vendor.example", leadstore.IGFollowing, at, igSHA(2), []leadstore.IGFollowRow{{Handle: "ok.example"}, {Handle: "not one"}}); !errors.Is(err, leadstore.ErrBadIG) {
		t.Fatalf("a bad handle: %v, want ErrBadIG", err)
	}
	if got := ownerInt(t, f.owner, `SELECT count(*) FROM ig_follow`); got != 2 {
		t.Errorf("follow rows = %d, want 2", got)
	}
	if got := ownerInt(t, f.owner, `SELECT count(*) FROM ig_follow WHERE full_name IS NULL AND handle = 'bean.example'`); got != 1 {
		t.Error("a row with no name: want full_name NULL")
	}
}

// TestIGMatchLatestWins: a match is written only when it changes; candidates are a set; the
// operator's tier is his alone; the latest row of a handle is its match.
func TestIGMatchLatestWins(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.cafeLead(t, "cafe-a.example", nil, "")
	b := f.cafeLead(t, "cafe-b.example", nil, "")
	const h = "cafe.exemplu"
	for i, c := range []struct {
		m    leadstore.IGMatch
		want bool
	}{
		{leadstore.IGMatch{Handle: h, LeadID: a, Tier: leadstore.IGTierName, Actor: leadstore.ActorSystem}, true},
		{leadstore.IGMatch{Handle: "@Cafe.Exemplu", LeadID: a, Tier: leadstore.IGTierName, Detail: "other words", Actor: leadstore.ActorSystem}, false},
		{leadstore.IGMatch{Handle: h, Tier: leadstore.IGTierName, Candidates: []int64{b, a, b}, Actor: leadstore.ActorSystem}, true},
		{leadstore.IGMatch{Handle: h, Tier: leadstore.IGTierName, Candidates: []int64{a, b}, Actor: leadstore.ActorSystem}, false},
		{leadstore.IGMatch{Handle: h, LeadID: b, Tier: leadstore.IGTierDomain, Actor: leadstore.ActorSystem}, true},
		{leadstore.IGMatch{Handle: h, Tier: leadstore.IGTierOperator, Actor: leadstore.ActorGremy}, true}, // an unmatch
		{leadstore.IGMatch{Handle: h, Tier: leadstore.IGTierOperator, Actor: leadstore.ActorGremy}, false},
	} {
		got, err := f.q.SetIGMatch(ctx, c.m)
		if err != nil || got != c.want {
			t.Fatalf("match %d: changed %v, %v; want %v", i, got, err, c.want)
		}
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM ig_match WHERE candidate_ids = ARRAY[$1, $2]::bigint[]`, min(a, b), max(a, b)); n != 1 {
		t.Error("the tie: want its candidates stored sorted, once each")
	}
	for name, bad := range map[string]leadstore.IGMatch{
		"operator by the system": {Handle: h, LeadID: a, Tier: leadstore.IGTierOperator, Actor: leadstore.ActorSystem},
		"none with a lead":       {Handle: h, LeadID: a, Tier: leadstore.IGTierNone, Actor: leadstore.ActorSystem},
		"one candidate":          {Handle: h, Tier: leadstore.IGTierName, Candidates: []int64{a}, Actor: leadstore.ActorSystem},
		"a lead and a tie":       {Handle: h, LeadID: a, Tier: leadstore.IGTierName, Candidates: []int64{a, b}, Actor: leadstore.ActorSystem},
		"strong with no lead":    {Handle: h, Tier: leadstore.IGTierHandle, Actor: leadstore.ActorSystem},
		"unknown tier":           {Handle: h, LeadID: a, Tier: "guess", Actor: leadstore.ActorSystem},
		"not a handle":           {Handle: "not one", Tier: leadstore.IGTierNone, Actor: leadstore.ActorSystem},
	} {
		if _, err := f.q.SetIGMatch(ctx, bad); !errors.Is(err, leadstore.ErrBadIG) {
			t.Errorf("%s: err = %v, want ErrBadIG", name, err)
		}
	}
	if _, err := f.q.SetIGMatch(ctx, leadstore.IGMatch{Handle: h, Tier: leadstore.IGTierNone, Actor: "operator"}); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("unknown actor: %v", err)
	}
	if _, err := f.q.SetIGMatch(ctx, leadstore.IGMatch{Handle: "bean.example", LeadID: b, Tier: leadstore.IGTierHandle, Actor: leadstore.ActorSystem}); err != nil {
		t.Fatal(err)
	}
	got, err := f.q.IGMatches(ctx, []string{"Cafe.Exemplu", "bean.example", "never.example", "not one"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[h].Tier != leadstore.IGTierOperator || got[h].LeadID != 0 || got[h].Strong() ||
		got["bean.example"].LeadID != b || !got["bean.example"].Strong() {
		t.Errorf("matches = %+v", got)
	}
}

// TestIGForLead: only a strong or operator match shows a lead its Instagram; the standing
// answers come from the newest capture that has one, a rule's before Jev's; a recapture Jev
// was not asked about keeps the older answers and their hash; an unmatch hides it all.
func TestIGForLead(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.cafeLead(t, "cafe-a.example", nil, "")
	const h = "cafe.exemplu"
	p1, _, err := f.q.InsertIGProfile(ctx, igProfile(h, 1))
	if err != nil {
		t.Fatal(err)
	}
	conf := 0.9
	jev := func(profile int64, q, choice string, sha byte) leadstore.IGJudgment {
		return leadstore.IGJudgment{ProfileID: profile, Question: q, Choice: choice, RawChoice: choice, Confidence: &conf,
			Source: leadstore.IGFromJev, Model: "fake", InputSHA: igSHA(sha)}
	}
	for _, j := range []leadstore.IGJudgment{
		jev(p1, leadstore.IGPlace, "cafe_seating", 0xa1),
		jev(p1, leadstore.IGVendor, "paper_card", 0xa1),
		{ProfileID: p1, Question: leadstore.IGVendor, Choice: "stampino", Source: leadstore.IGFromRule, Detail: "tag"},
		jev(p1, leadstore.IGLoyalty, "runs_scheme", 0xa1),
	} {
		if _, err := f.q.AddIGJudgment(ctx, j); err != nil {
			t.Fatalf("judgment %+v: %v", j, err)
		}
	}
	if _, err := f.q.AddIGHint(ctx, leadstore.IGHint{Handle: h, Kind: leadstore.IGHintFollowedByVendor, Value: "stampino", CapturedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// A weak match shows nothing.
	if _, err := f.q.SetIGMatch(ctx, leadstore.IGMatch{Handle: h, LeadID: lead, Tier: leadstore.IGTierName, Actor: leadstore.ActorSystem}); err != nil {
		t.Fatal(err)
	}
	if got, err := f.q.IGForLead(ctx, lead); err != nil || got != nil {
		t.Fatalf("weak match: %+v %v, want nothing", got, err)
	}
	if _, err := f.q.SetIGMatch(ctx, leadstore.IGMatch{Handle: h, LeadID: lead, Tier: leadstore.IGTierOperator, Actor: leadstore.ActorGremy}); err != nil {
		t.Fatal(err)
	}
	got, err := f.q.IGForLead(ctx, lead)
	if err != nil || got == nil {
		t.Fatalf("operator match: %+v %v", got, err)
	}
	choices := func(g *leadstore.IGLead) map[string]string {
		out := map[string]string{}
		for q, j := range g.Judgments {
			out[q] = j.Choice + "/" + j.Source
		}
		return out
	}
	want := map[string]string{leadstore.IGPlace: "cafe_seating/jev", leadstore.IGVendor: "stampino/rule", leadstore.IGLoyalty: "runs_scheme/jev"}
	if got.Match.Tier != leadstore.IGTierOperator || got.Profile == nil || got.Profile.ID != p1 || !reflect.DeepEqual(choices(got), want) ||
		len(got.Hints) != 1 || got.Hints[0].Value != "stampino" || got.Judgments[leadstore.IGPlace].Confidence == nil {
		t.Fatalf("for lead: %+v, choices %v", got, choices(got))
	}

	// A recapture with the same text: Jev is not asked, so the answers and the hash stand.
	p2, _, err := f.q.InsertIGProfile(ctx, igProfile(h, 2))
	if err != nil {
		t.Fatal(err)
	}
	if sha, err := f.q.IGJudgmentHash(ctx, "@"+h, leadstore.IGPlace, true); err != nil || !bytes.Equal(sha, igSHA(0xa1)) {
		t.Fatalf("hash by handle after a recapture: %x %v", sha, err)
	}
	if got, _ = f.q.IGForLead(ctx, lead); got.Profile.ID != p2 || !reflect.DeepEqual(choices(got), want) {
		t.Fatalf("after a recapture: profile %d, choices %v", got.Profile.ID, choices(got))
	}
	// The new capture's answers replace the old ones question by question.
	if _, err := f.q.AddIGJudgment(ctx, jev(p2, leadstore.IGPlace, "roastery_or_online", 0xa2)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.AddIGJudgment(ctx, jev(p2, leadstore.IGVendor, "none", 0xa2)); err != nil {
		t.Fatal(err)
	}
	want[leadstore.IGPlace], want[leadstore.IGVendor] = "roastery_or_online/jev", "none/jev"
	if got, _ = f.q.IGForLead(ctx, lead); !reflect.DeepEqual(choices(got), want) {
		t.Fatalf("after new answers: %v, want %v", choices(got), want)
	}
	if sha, err := f.q.IGJudgmentHash(ctx, h, leadstore.IGPlace, true); err != nil || !bytes.Equal(sha, igSHA(0xa2)) {
		t.Fatalf("latest hash: %x %v", sha, err)
	}
	if sha, err := f.q.IGJudgmentHash(ctx, "never.example", leadstore.IGPlace, true); err != nil || sha != nil {
		t.Fatalf("never asked: %x %v", sha, err)
	}

	// Real Jev's answer, on the older capture: it stands over the fake's newer ones, and each
	// kind of decider's hash is read from its own rows only.
	if sha, err := f.q.IGJudgmentHash(ctx, h, leadstore.IGPlace, false); err != nil || sha != nil {
		t.Fatalf("real hash before any real answer: %x %v", sha, err)
	}
	if ok, err := f.q.IGRealJudged(ctx, h); err != nil || ok {
		t.Fatalf("real judged before any real answer: %v %v", ok, err)
	}
	for q, c := range map[string]string{leadstore.IGPlace: "cafe_seating", leadstore.IGLoyalty: "none_seen"} {
		real := jev(p1, q, c, 0xb1)
		real.Model = "jev-test-1"
		if _, err := f.q.AddIGJudgment(ctx, real); err != nil {
			t.Fatal(err)
		}
	}
	want[leadstore.IGPlace], want[leadstore.IGLoyalty], want[leadstore.IGVendor] = "cafe_seating/jev", "none_seen/jev", "stampino/rule"
	if got, _ = f.q.IGForLead(ctx, lead); !reflect.DeepEqual(choices(got), want) || got.Judgments[leadstore.IGPlace].Model != "jev-test-1" {
		t.Fatalf("after a real answer: %v, want %v (real Jev's over the fake's)", choices(got), want)
	}
	if sha, err := f.q.IGJudgmentHash(ctx, h, leadstore.IGPlace, false); err != nil || !bytes.Equal(sha, igSHA(0xb1)) {
		t.Fatalf("real hash: %x %v", sha, err)
	}
	if sha, err := f.q.IGJudgmentHash(ctx, h, leadstore.IGPlace, true); err != nil || !bytes.Equal(sha, igSHA(0xa2)) {
		t.Fatalf("fake hash after a real answer: %x %v", sha, err)
	}
	if ok, err := f.q.IGRealJudged(ctx, h); err != nil || !ok {
		t.Fatalf("real judged: %v %v", ok, err)
	}

	// An operator's unmatch hides it; another lead never sees it.
	if _, err := f.q.SetIGMatch(ctx, leadstore.IGMatch{Handle: h, Tier: leadstore.IGTierOperator, Actor: leadstore.ActorGremy}); err != nil {
		t.Fatal(err)
	}
	if got, err := f.q.IGForLead(ctx, lead); err != nil || got != nil {
		t.Fatalf("after unmatch: %+v %v", got, err)
	}

	for name, bad := range map[string]leadstore.IGJudgment{
		"jev without a hash":   {ProfileID: p2, Question: leadstore.IGPlace, Choice: "unclear", RawChoice: "unclear", Source: leadstore.IGFromJev},
		"jev without raw":      {ProfileID: p2, Question: leadstore.IGPlace, Choice: "unclear", Source: leadstore.IGFromJev, InputSHA: igSHA(1)},
		"rule with a hash":     {ProfileID: p2, Question: leadstore.IGPlace, Choice: "unclear", Source: leadstore.IGFromRule, InputSHA: igSHA(1)},
		"rule with confidence": {ProfileID: p2, Question: leadstore.IGPlace, Choice: "unclear", Source: leadstore.IGFromRule, Confidence: &conf},
		"unknown question":     {ProfileID: p2, Question: "ig_mood", Choice: "unclear", Source: leadstore.IGFromRule},
		"unknown source":       {ProfileID: p2, Question: leadstore.IGPlace, Choice: "unclear", Source: "operator"},
	} {
		if _, err := f.q.AddIGJudgment(ctx, bad); !errors.Is(err, leadstore.ErrBadIG) {
			t.Errorf("%s: %v, want ErrBadIG", name, err)
		}
	}
	if ins, err := f.q.AddIGHint(ctx, leadstore.IGHint{Handle: h, Kind: leadstore.IGHintFollowedByVendor, Value: "stampino", CapturedAt: time.Now()}); err != nil || ins {
		t.Errorf("the same hint again: %v %v, want not inserted", ins, err)
	}
	if _, err := f.q.AddIGHint(ctx, leadstore.IGHint{Handle: h, Kind: "runs_scheme", Value: "stampino", CapturedAt: time.Now()}); !errors.Is(err, leadstore.ErrBadIG) {
		t.Errorf("unknown hint kind: %v", err)
	}
}

// TestIGReviewStatusGrid: a handle is queued once, never over the operator's call; a status is
// written only when it changes; the list reads by latest status with what is known of each.
func TestIGReviewStatusGrid(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.cafeLead(t, "cafe-a.example", nil, "")
	b := f.cafeLead(t, "cafe-b.example", nil, "")
	for _, h := range []string{"new.example", "dismissed.example", "matched.example", "wanted.example"} {
		if q, err := f.q.QueueIGReview(ctx, h); err != nil || !q {
			t.Fatalf("queue %s: %v %v", h, q, err)
		}
		if q, err := f.q.QueueIGReview(ctx, h); err != nil || q {
			t.Fatalf("queue %s twice: %v %v", h, q, err)
		}
	}
	for h, st := range map[string]string{"dismissed.example": leadstore.IGReviewDismissed,
		"matched.example": leadstore.IGReviewMatched, "wanted.example": leadstore.IGReviewWanted} {
		if c, err := f.q.SetIGReviewStatus(ctx, h, st, "  checked  ", leadstore.ActorGremy); err != nil || !c {
			t.Fatalf("set %s %s: %v %v", h, st, c, err)
		}
		if c, err := f.q.SetIGReviewStatus(ctx, h, st, "again", leadstore.ActorGremy); err != nil || c {
			t.Fatalf("set %s %s again: %v %v", h, st, c, err)
		}
		// A later import never queues over the operator's call.
		if q, err := f.q.QueueIGReview(ctx, h); err != nil || q {
			t.Fatalf("re-queue %s: %v %v", h, q, err)
		}
	}
	if _, err := f.q.SetIGReviewStatus(ctx, "new.example", "maybe", "", leadstore.ActorGremy); !errors.Is(err, leadstore.ErrBadIG) {
		t.Errorf("unknown status: %v", err)
	}
	if _, err := f.q.SetIGReviewStatus(ctx, "new.example", leadstore.IGReviewNew, "", "operator"); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("unknown actor: %v", err)
	}

	// What the list knows: a read profile with its place call and a tie; a following-list
	// account never read, with its kept name; a weak single candidate.
	p, _, err := f.q.InsertIGProfile(ctx, igProfile("new.example", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.AddIGJudgment(ctx, leadstore.IGJudgment{ProfileID: p, Question: leadstore.IGPlace, Choice: "cafe_seating",
		RawChoice: "cafe_seating", Source: leadstore.IGFromJev, InputSHA: igSHA(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.SetIGMatch(ctx, leadstore.IGMatch{Handle: "new.example", Tier: leadstore.IGTierName, Candidates: []int64{a, b}, Actor: leadstore.ActorSystem}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.InsertIGFollows(ctx, "vendor.example", leadstore.IGFollowing, time.Now(), igSHA(2), []leadstore.IGFollowRow{{Handle: "wanted.example", FullName: "Wanted Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.SetIGMatch(ctx, leadstore.IGMatch{Handle: "wanted.example", LeadID: a, Tier: leadstore.IGTierName, Actor: leadstore.ActorSystem}); err != nil {
		t.Fatal(err)
	}

	all, err := f.q.IGReview(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var handles []string
	for _, it := range all {
		handles = append(handles, it.Handle+":"+it.Status)
	}
	if want := []string{"dismissed.example:dismissed", "matched.example:matched", "new.example:new", "wanted.example:wanted"}; !slices.Equal(handles, want) {
		t.Fatalf("all = %v, want %v", handles, want)
	}
	news, err := f.q.IGReview(ctx, leadstore.IGReviewNew)
	if err != nil || len(news) != 1 {
		t.Fatalf("new: %+v %v", news, err)
	}
	n := news[0]
	if n.Profile == nil || n.Profile.ID != p || n.Place != "cafe_seating" || n.Match == nil || !slices.Equal(n.Candidates, []int64{min(a, b), max(a, b)}) {
		t.Errorf("new item = %+v", n)
	}
	wanted, err := f.q.IGReview(ctx, leadstore.IGReviewWanted)
	if err != nil || len(wanted) != 1 {
		t.Fatalf("wanted: %+v %v", wanted, err)
	}
	w := wanted[0]
	if w.Profile != nil || w.FollowName != "Wanted Example" || w.Place != "" || !slices.Equal(w.Candidates, []int64{a}) || w.Note != "checked" {
		t.Errorf("wanted item = %+v", w)
	}
}

// TestIGLeadKeys: every handle a café lead holds, from each of the four places one lives,
// normalised and once each; a retired route or fact, a vendor's own account and another
// network are not among them; a Drupal lead and an erased (suppressed) org are not read.
func TestIGLeadKeys(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.cafeLead(t, "cafe-a.example", nil, "")
	bare := f.cafeLead(t, "cafe-b.example", nil, "")
	gone := f.cafeLead(t, "cafe-gone.example", nil, "")
	drupal := f.siteLead(t, "drupal.example", nil, 7, "")
	org := f.leadOf(t, lead).OrgID
	goneOrg := f.leadOf(t, gone).OrgID

	// Routes: the import's and a page's live, one retired.
	for _, r := range []leadstore.RouteRow{
		{OrgID: org, Kind: leadstore.RouteInstagram, Value: "from_route.example", DerivedBy: leadstore.DerivedByInstagram},
		{OrgID: org, Kind: leadstore.RouteInstagram, Value: "https://instagram.com/retired.example", FetchID: f.fid, DerivedBy: "enrich@1"},
		{OrgID: goneOrg, Kind: leadstore.RouteInstagram, Value: "gone.example", DerivedBy: leadstore.DerivedByInstagram},
	} {
		if _, err := f.q.InsertRoute(ctx, r); err != nil {
			t.Fatalf("route %+v: %v", r, err)
		}
	}
	retired := ownerInt(t, f.owner, `SELECT id FROM contact_route WHERE value = 'https://www.instagram.com/retired.example/'`)
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: retired, Mark: leadstore.MarkRetire}); err != nil {
		t.Fatal(err)
	}
	// Social facts: an Instagram profile, a retired one, another network.
	for _, u := range []string{igURL("from_fact.example"), igURL("retired_fact.example"), "https://facebook.com/cafe-a"} {
		f.ownerRun(t, `INSERT INTO org_fact (org_id, kind, value, normalized, fetch_id, derived_by) VALUES ($1, 'social', $2, $2, $3, 'enrich@1')`, org, u, f.fid)
	}
	fact := ownerInt(t, f.owner, `SELECT id FROM org_fact WHERE normalized = $1`, igURL("retired_fact.example"))
	// MarkContact retires phones only; a retire mark on any fact is what the readers check.
	f.ownerRun(t, `INSERT INTO contact_mark (fact_id, mark, actor) VALUES ($1, 'retire', 'operator')`, fact)
	// The lead's probe: its own Instagram link, and a link to a vendor's account, which the
	// detector writes both as loyalty and as Instagram.
	f.ownerRun(t, `UPDATE site_probe SET signals = $2::jsonb WHERE id = (SELECT probe_id FROM lead WHERE id = $1)`, lead,
		`[{"signal": "instagram", "value": "instagram:instagram:link:https://www.instagram.com/From_Probe.Example/?hl=ro", "fetch_id": 1},
		  {"signal": "instagram", "value": "instagram:instagram:link:https://www.instagram.com/stampino.app/", "fetch_id": 1},
		  {"signal": "loyalty", "value": "loyalty:stampino:link:https://www.instagram.com/stampino.app/", "fetch_id": 1},
		  {"signal": "ordering", "value": "ordering:glovo:link:https://glovoapp.com/ro/cafe", "fetch_id": 1}]`)
	// The bare lead holds only Instagram's own host, never a handle: the probe's inline-script
	// needle (the bare host, as on dev), a social fact and a map listing's social with no path.
	bareOrg := f.leadOf(t, bare).OrgID
	if n := ownerInt(t, f.owner, `WITH u AS (UPDATE site_probe SET signals = $2::jsonb WHERE id = (SELECT probe_id FROM lead WHERE id = $1) RETURNING 1)
		SELECT count(*) FROM u`, bare,
		`[{"signal": "instagram", "value": "instagram:instagram:inline-script:instagram.com", "fetch_id": 1},
		  {"signal": "instagram", "value": "instagram:instagram:inline-script:www.instagram.com", "fetch_id": 1},
		  {"signal": "instagram", "value": "instagram:instagram:link:https://www.instagram.com/", "fetch_id": 1}]`); n != 1 {
		t.Fatalf("bare lead's probe: %d rows updated, want 1", n)
	}
	f.ownerRun(t, `INSERT INTO org_fact (org_id, kind, value, normalized, fetch_id, derived_by) VALUES ($1, 'social', $2, $2, $3, 'enrich@1')`,
		bareOrg, "https://instagram.com/", f.fid)
	// Two releases of its map listing; the latest names the place and the town.
	imp := ownerInt(t, f.owner, `INSERT INTO place_import (source, release, campaign_id, lines, kept, blank, malformed, not_cafe,
		low_confidence, other_country, no_name, stored, already, chains, orgs, sites, actor)
		VALUES ('overture', 'r1', 1, 2, 2, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 0, 'system') RETURNING id`)
	f.ownerRun(t, `INSERT INTO place (import_id, source, source_id, release, name, category, confidence, org_id, locality, socials)
		VALUES ($1, 'overture', 'p1', 'r1', 'Old Name', 'cafe', 0.9, $2, 'Old Town', $3::jsonb)`, imp, org,
		`["`+igURL("from_place.example")+`", "https://www.facebook.com/cafe-a"]`)
	f.ownerRun(t, `INSERT INTO place (import_id, source, source_id, release, name, category, confidence, org_id, locality, socials)
		VALUES ($1, 'overture', 'p1', 'r2', 'Cafe Exemplu', 'cafe', 0.9, $2, 'Exampleton', $3::jsonb)`, imp, org,
		`["HTTPS://WWW.INSTAGRAM.COM/FROM_FACT.EXAMPLE/"]`)
	f.ownerRun(t, `INSERT INTO place (import_id, source, source_id, release, name, category, confidence, org_id, socials)
		VALUES ($1, 'overture', 'p3', 'r1', '', 'cafe', 0.9, $2, $3::jsonb)`, imp, bareOrg, `["instagram.com", "https://instagr.am"]`)
	if err := f.q.SetPlaceType(ctx, lead, leadstore.PlaceUnclear, leadstore.PlaceFromJev, "q", igSHA(7), leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	if err := f.q.SetPlaceType(ctx, bare, leadstore.PlaceRoastery, leadstore.PlaceFromOperator, "", nil, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	// The erased org: its domain is suppressed (ForgetOrg writes the same suppression).
	if _, err := f.q.Suppress(ctx, leadstore.DefaultAccountID, leadstore.SuppressDomain, "cafe-gone.example", leadstore.ReasonManual, "forget: test"); err != nil {
		t.Fatal(err)
	}

	keys, err := f.q.IGLeadKeys(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	byID := map[int64]leadstore.IGLeadKey{}
	for _, k := range keys {
		ids = append(ids, k.LeadID)
		byID[k.LeadID] = k
	}
	if !slices.Equal(ids, []int64{lead, bare}) || slices.Contains(ids, drupal) || slices.Contains(ids, gone) {
		t.Fatalf("keys for leads %v, want %v", ids, []int64{lead, bare})
	}
	k := byID[lead]
	wantHandles := []string{"from_fact.example", "from_place.example", "from_probe.example", "from_route.example"}
	if !slices.Equal(k.Handles, wantHandles) {
		t.Errorf("handles = %v, want %v", k.Handles, wantHandles)
	}
	if k.OrgID != org || k.Domain != "cafe-a.example" || k.PlaceName != "Cafe Exemplu" || k.Town != "Exampleton" ||
		k.PlaceType != leadstore.PlaceCafe || k.JevPlaceType != leadstore.PlaceUnclear || k.RawHandles != nil {
		t.Errorf("key = %+v", k)
	}
	if b := byID[bare]; len(b.Handles) != 0 || b.Handles == nil || b.PlaceType != leadstore.PlaceRoastery || b.JevPlaceType != "" || b.Town != "" || b.PlaceName != "" {
		t.Errorf("bare key = %+v", b)
	}

	// A map listing's org with no website of its own has no domain to match.
	f.ownerRun(t, `UPDATE org SET domain = $2 WHERE id = $1`, f.leadOf(t, bare).OrgID, leadstore.PlaceOrgPrefix+"p2")
	if keys, err = f.q.IGLeadKeys(ctx, 0); err != nil || len(keys) != 2 || keys[1].Domain != "" {
		t.Errorf("listing org: %+v %v", keys, err)
	}
	campaign := ownerInt(t, f.owner, `SELECT campaign_id FROM lead WHERE id = $1`, lead)
	if keys, err := f.q.IGLeadKeys(ctx, campaign); err != nil || len(keys) != 2 {
		t.Errorf("its campaign: %d keys %v", len(keys), err)
	}
	if keys, err := f.q.IGLeadKeys(ctx, campaign+1000); err != nil || len(keys) != 0 {
		t.Errorf("another campaign: %d keys %v", len(keys), err)
	}
}

// TestInsertRouteInstagram (Decision 8): an instagram route is stored in its one canonical
// form, written with no fetch only as the import's; RoutesFor puts Instagram after LinkedIn.
func TestInsertRouteInstagram(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.cafeLead(t, "cafe-a.example", nil, "info@cafe-a.example")
	org := f.leadOf(t, lead).OrgID
	ins := func(r leadstore.RouteRow) (bool, error) { r.OrgID = org; return f.q.InsertRoute(ctx, r) }

	if ok, err := ins(leadstore.RouteRow{Kind: leadstore.RouteInstagram, Value: "@Cafe.Exemplu", DerivedBy: leadstore.DerivedByInstagram}); err != nil || !ok {
		t.Fatalf("import route: %v %v", ok, err)
	}
	if ok, err := ins(leadstore.RouteRow{Kind: leadstore.RouteInstagram, Value: "https://instagram.com/cafe.exemplu?hl=ro", FetchID: f.fid, DerivedBy: "enrich@1"}); err != nil || ok {
		t.Fatalf("the same profile in another form: %v %v, want kept as it was", ok, err)
	}
	for name, c := range map[string]struct {
		row  leadstore.RouteRow
		want error
	}{
		"no fetch, not the import's": {leadstore.RouteRow{Kind: leadstore.RouteInstagram, Value: "bean.example", DerivedBy: "enrich@1"}, leadstore.ErrRouteNoSource},
		"the import's on a form":     {leadstore.RouteRow{Kind: leadstore.RouteForm, Value: "https://cafe-a.example/c", DerivedBy: leadstore.DerivedByInstagram}, leadstore.ErrRouteNoSource},
		"an email with no fetch":     {leadstore.RouteRow{Kind: leadstore.RouteEmail, Value: "a@cafe-a.example", DerivedBy: "contacts@1"}, leadstore.ErrRouteNoSource},
		"a post, not a profile":      {leadstore.RouteRow{Kind: leadstore.RouteInstagram, Value: "https://www.instagram.com/p/AbC/", DerivedBy: leadstore.DerivedByInstagram}, leadstore.ErrEmptyRoute},
	} {
		if _, err := ins(c.row); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if _, err := ins(leadstore.RouteRow{Kind: leadstore.RouteLinkedIn, Value: t12LinkedIn("company", "cafe-a"), FetchID: f.fid, DerivedBy: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ins(leadstore.RouteRow{Kind: leadstore.RouteForm, Value: "https://cafe-a.example/contact", FetchID: f.fid, DerivedBy: "x"}); err != nil {
		t.Fatal(err)
	}
	routes, err := f.q.RoutesFor(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range routes {
		got = append(got, r.Kind+" "+r.Value)
		if r.Kind == leadstore.RouteInstagram && (r.FetchID != 0 || r.DerivedBy != leadstore.DerivedByInstagram) {
			t.Errorf("instagram route = %+v, want the import's with no fetch", r)
		}
	}
	want := []string{"email info@cafe-a.example", "form https://cafe-a.example/contact",
		"linkedin " + t12LinkedIn("company", "cafe-a"), "instagram https://www.instagram.com/cafe.exemplu/"}
	if !slices.Equal(got, want) {
		t.Errorf("routes\n got %v\nwant %v", got, want)
	}
}
