//go:build integration

package leadstore_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/igsignal"
	"github.com/gremy/webbership-leadhound/internal/intent"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// SyncIntent, SetIntentCall and LatestIntents (migration 00033, intent@1). Every café handle
// here is invented (privacycheck: handles naming example or exemplu); the vendor accounts are
// signals.yaml's own.

const (
	igStampino = "stampino.app"
	igPick     = "pick.coupons"
	igTuanis   = "tuanis.eu"
	igOwn      = "own_loyalty.example" // the own vendor's account in these tests
	igOrdering = "ordering_app.example"
)

// intentConfig is IntentConfig as the commands build it, from igsignal on ccfg.
func intentConfig(ccfg *cafe.Config) leadstore.IntentConfig {
	m := igsignal.NewMatcher(ccfg, nil)
	prof := func(p leadstore.IGProfile) igsignal.Profile {
		return igsignal.Profile{Handle: p.Handle, FullName: p.FullName, Bio: p.Bio, ExternalURL: p.ExternalURL, Captions: p.Captions}
	}
	return leadstore.IntentConfig{
		Cafe:          ccfg,
		VendorAccount: func(h string) string { return m.MatchFollow(igsignal.Follow{Handle: h}).VendorAccount },
		RuleVendor: func(p leadstore.IGProfile) string {
			id, _, _ := igsignal.Vendor(prof(p), ccfg)
			return id
		},
		Prepaid: func(p leadstore.IGProfile) bool { return igsignal.Prepaid(prof(p), ccfg) },
	}
}

// ownConfig is the default signal list with an own vendor, as cafeConfig builds it.
func ownConfig(t *testing.T) *cafe.Config {
	t.Helper()
	c, err := cafe.DefaultConfig().WithOwnVendor("Own loyalty", []string{"own-loyalty.example", "instagram.com/" + igOwn})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// orderingConfig is ownConfig plus an ordering vendor with its own Instagram account.
func orderingConfig(t *testing.T) *cafe.Config {
	t.Helper()
	base := *cafe.DefaultConfig()
	base.Vendors = append(slices.Clone(base.Vendors), cafe.Vendor{ID: "ordering-example", Name: "Ordering Example",
		Kind: cafe.SigOrdering, Match: []string{"ordering.example", "instagram.com/" + igOrdering}})
	raw, err := yaml.Marshal(&base)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cafe.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c, err = c.WithOwnVendor("Own loyalty", []string{"own-loyalty.example", "instagram.com/" + igOwn}); err != nil {
		t.Fatal(err)
	}
	return c
}

type intentScene struct {
	*igPlaceScene
	cfg leadstore.IntentConfig
	now time.Time
}

func newIntentScene(t *testing.T) *intentScene {
	t.Helper()
	return &intentScene{igPlaceScene: newIGPlaceScene(t), cfg: intentConfig(ownConfig(t)),
		now: time.Now().UTC().Truncate(time.Second)}
}

// cafe adds a café lead on domain with handle strongly matched to it (none when handle is "").
func (s *intentScene) cafe(t *testing.T, domain, handle string) int64 {
	t.Helper()
	id := s.cafeLead(t, domain, nil, "")
	if handle != "" {
		s.capture(t, handle)
		s.match(t, handle, id, leadstore.IGTierHandle)
	}
	return id
}

// follows puts handle on vendor's followers list, captured at.
func (s *intentScene) follows(t *testing.T, vendor, handle string, at time.Time) {
	t.Helper()
	s.next++
	if _, err := s.q.InsertIGFollows(s.ctx, vendor, leadstore.IGFollowers, at, igSHA(s.next), []leadstore.IGFollowRow{{Handle: handle}}); err != nil {
		t.Fatalf("%s follows %s: %v", handle, vendor, err)
	}
}

// touch stores one touch of vendor on handle.
func (s *intentScene) touch(t *testing.T, vendor, handle, kind, text string, at time.Time) {
	t.Helper()
	s.next++
	ref := "r" + string(rune('a'+s.next%26)) + at.Format("20060102150405")
	if _, err := s.q.InsertIGTouches(s.ctx, vendor, at, igSHA(s.next), []leadstore.IGTouch{
		{Handle: handle, Kind: kind, Ref: ref, PostCode: "P" + ref, At: at, Text: text}}); err != nil {
		t.Fatalf("touch %s %s %s: %v", vendor, kind, handle, err)
	}
}

// answer captures handle and gives it a Jev answer of model on question.
func (s *intentScene) answer(t *testing.T, handle, question, choice, model string) {
	t.Helper()
	profile := s.capture(t, handle)
	s.next++
	conf := 0.9
	if _, err := s.q.AddIGJudgment(s.ctx, leadstore.IGJudgment{ProfileID: profile, Question: question, Choice: choice,
		RawChoice: choice, Confidence: &conf, Source: leadstore.IGFromJev, Model: model, InputSHA: igSHA(s.next)}); err != nil {
		t.Fatalf("answer %s %s: %v", handle, question, err)
	}
}

func (s *intentScene) syncAt(t *testing.T, now time.Time, dry bool) leadstore.IntentSync {
	t.Helper()
	got, err := s.q.SyncIntent(s.ctx, s.cfg, 0, now, dry)
	if err != nil {
		t.Fatalf("sync intent: %v", err)
	}
	return got
}

func (s *intentScene) sync(t *testing.T) leadstore.IntentSync { return s.syncAt(t, s.now, false) }

func (s *intentScene) intentOf(t *testing.T, lead int64) leadstore.IntentRow {
	t.Helper()
	got, err := s.q.LatestIntents(s.ctx, []int64{lead})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := got[lead]
	if !ok {
		t.Fatalf("lead %d has no intent row read", lead)
	}
	return r
}

func (s *intentScene) rows(t *testing.T, lead int64) int64 {
	t.Helper()
	return ownerInt(t, s.owner, `SELECT count(*) FROM lead_intent WHERE lead_id = $1`, lead)
}

func (s *intentScene) call(t *testing.T, lead int64, class *string, note string) {
	t.Helper()
	if err := s.q.SetIntentCall(s.ctx, lead, class, note); err != nil {
		t.Fatalf("call %d: %v", lead, err)
	}
}

func strp(s string) *string { return &s }

// changed is the lead ids of a sync's changes.
func changed(got leadstore.IntentSync) []int64 {
	ids := []int64{}
	for _, c := range got.Changes {
		ids = append(ids, c.LeadID)
	}
	return ids
}

// TestIntentColdStart: the first sync on a store with no lead_intent rows writes only the
// non-cold leads; a re-run writes nothing; a dry run reports what it would write and writes
// nothing.
func TestIntentColdStart(t *testing.T) {
	s := newIntentScene(t)
	cold1 := s.cafe(t, "cold-a.example", "cold_a.example")
	cold2 := s.cafe(t, "cold-b.example", "")
	shop := s.cafe(t, "shop.example", "shop.example")
	s.follows(t, igStampino, "shop.example", s.now.Add(-48*time.Hour))
	s.follows(t, igPick, "shop.example", s.now.Add(-24*time.Hour))
	paper := s.cafe(t, "paper.example", "")
	s.answer(t, "paper.example", leadstore.IGVendor, "paper_card", igRealModel)
	s.match(t, "paper.example", paper, leadstore.IGTierHandle)

	dry := s.syncAt(t, s.now, true)
	if got := changed(dry); !slices.Equal(got, []int64{shop, paper}) {
		t.Errorf("dry run changes = %v, want %v", got, []int64{shop, paper})
	}
	if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_intent`); n != 0 {
		t.Fatalf("dry run wrote %d rows", n)
	}
	first := s.sync(t)
	if got := changed(first); !slices.Equal(got, []int64{shop, paper}) {
		t.Errorf("first sync changes = %v, want %v", got, []int64{shop, paper})
	}
	if first.Leads != 4 || first.Classes[intent.Cold] != 2 || first.Classes[intent.Shopping] != 1 || first.Classes[intent.Paper] != 1 {
		t.Errorf("counts = %d leads %v", first.Leads, first.Classes)
	}
	for _, l := range []int64{cold1, cold2} {
		if n := s.rows(t, l); n != 0 {
			t.Errorf("cold lead %d got %d rows", l, n)
		}
		if r := s.intentOf(t, l); r.Class != intent.Cold || r.Derived != "" || r.Rank != 5 || r.Held {
			t.Errorf("cold lead %d reads %+v", l, r)
		}
	}
	if r := s.intentOf(t, shop); r.Class != intent.Shopping || r.Overlap != 2 || r.Rank != 1 || r.Yours {
		t.Errorf("shop reads %+v", r)
	}
	if again := s.sync(t); len(again.Changes) != 0 {
		t.Errorf("re-run changes = %v, want none", changed(again))
	}
	if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_intent`); n != 2 {
		t.Errorf("rows after re-run = %d, want 2", n)
	}
}

// TestIntentGrid is the status × operation grid: derived row vs your call vs cleared call vs
// expiry.
func TestIntentGrid(t *testing.T) {
	type cell struct {
		name string
		run  func(t *testing.T, s *intentScene)
	}
	cells := []cell{
		{"no row, sync cold: nothing written", func(t *testing.T, s *intentScene) {
			l := s.cafe(t, "grid-cold.example", "grid_cold.example")
			if got := s.sync(t); len(got.Changes) != 0 || s.rows(t, l) != 0 {
				t.Errorf("cold sync wrote %v", changed(got))
			}
		}},
		{"shopping, sync no change: 0 rows", func(t *testing.T, s *intentScene) {
			l := s.cafe(t, "grid-shop.example", "grid_shop.example")
			s.follows(t, igStampino, "grid_shop.example", s.now.Add(-time.Hour))
			s.follows(t, igPick, "grid_shop.example", s.now.Add(-time.Hour))
			s.sync(t)
			if got := s.sync(t); len(got.Changes) != 0 || s.rows(t, l) != 1 {
				t.Errorf("second sync wrote %v; rows %d", changed(got), s.rows(t, l))
			}
		}},
		{"shopping, the follows drop to 1: curious row", func(t *testing.T, s *intentScene) {
			// Follower captures only add (a list read is partial, never proof of an unfollow), so the
			// follows drop when a handle leaves the lead: its new match names no lead.
			l := s.cafe(t, "grid-drop.example", "grid_drop.example")
			s.capture(t, "grid_drop2.example")
			s.match(t, "grid_drop2.example", l, leadstore.IGTierHandle)
			s.follows(t, igStampino, "grid_drop.example", s.now.Add(-time.Hour))
			s.follows(t, igPick, "grid_drop2.example", s.now.Add(-time.Hour))
			s.sync(t)
			if r := s.intentOf(t, l); r.Class != intent.Shopping || r.Overlap != 2 {
				t.Fatalf("before: %+v", r)
			}
			if _, err := s.q.SetIGMatch(s.ctx, leadstore.IGMatch{Handle: "grid_drop2.example", Tier: leadstore.IGTierNone, Actor: leadstore.ActorGremy}); err != nil {
				t.Fatal(err)
			}
			got := s.sync(t)
			if r := s.intentOf(t, l); r.Class != intent.Curious || r.Overlap != 1 || len(got.Changes) != 1 || got.Changes[0].From != intent.Shopping {
				t.Errorf("after the drop: %+v, changes %+v", r, got.Changes)
			}
		}},
		{"your call out, sync says shopping: out wins on read, derived row written", func(t *testing.T, s *intentScene) {
			l := s.cafe(t, "grid-call.example", "grid_call.example")
			s.follows(t, igStampino, "grid_call.example", s.now.Add(-time.Hour))
			s.follows(t, igPick, "grid_call.example", s.now.Add(-time.Hour))
			s.call(t, l, strp(intent.Out), "  told me\nno thanks ")
			got := s.sync(t)
			if !slices.Equal(changed(got), []int64{l}) || got.Changes[0].Class != intent.Shopping {
				t.Errorf("sync under a call wrote %+v, want the derived shopping row", got.Changes)
			}
			r := s.intentOf(t, l)
			if r.Class != intent.Out || r.Derived != intent.Shopping || !r.Yours || !r.Held || r.Note != "told me no thanks" || r.Rank != 7 {
				t.Errorf("under the call: %+v", r)
			}
		}},
		{"your call, cleared: the derived class reads again", func(t *testing.T, s *intentScene) {
			l := s.cafe(t, "grid-clear.example", "grid_clear.example")
			s.follows(t, igStampino, "grid_clear.example", s.now.Add(-time.Hour))
			s.follows(t, igPick, "grid_clear.example", s.now.Add(-time.Hour))
			s.call(t, l, strp(intent.JustChose), "chose Cardora")
			s.sync(t)
			r := s.intentOf(t, l)
			if r.Class != intent.JustChose || !r.Held || r.HeldUntil == nil || r.HeldUntil.Sub(s.now) < 59*24*time.Hour {
				t.Fatalf("just_chose call: %+v", r)
			}
			s.call(t, l, nil, "ignored")
			r = s.intentOf(t, l)
			if r.Class != intent.Shopping || r.Yours || r.Held || r.Note != "" || r.HeldUntil != nil {
				t.Errorf("after clear: %+v", r)
			}
			if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_intent_call WHERE lead_id = $1`, l); n != 2 {
				t.Errorf("call rows = %d, want 2", n)
			}
			s.call(t, l, nil, "") // a clear with no standing call writes nothing
			if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_intent_call WHERE lead_id = $1`, l); n != 2 {
				t.Errorf("call rows after a second clear = %d, want 2", n)
			}
		}},
		{"just_chose, held_until passed: the next sync writes switch", func(t *testing.T, s *intentScene) {
			l := s.cafe(t, "grid-chose.example", "grid_chose.example")
			at := s.now.Add(-10 * 24 * time.Hour)
			s.touch(t, igStampino, "grid_chose.example", leadstore.IGTouchUsertag, "", at)
			s.sync(t)
			r := s.intentOf(t, l)
			if r.Class != intent.JustChose || !r.Held || r.HeldUntil == nil || !r.HeldUntil.Equal(at.Add(60*24*time.Hour)) || r.Rank != 6 {
				t.Fatalf("just chose: %+v", r)
			}
			got := s.syncAt(t, s.now.Add(55*24*time.Hour), false)
			r = s.intentOf(t, l)
			if r.Class != intent.Switch || r.Held || r.HeldUntil != nil || len(got.Changes) != 1 || got.Changes[0].From != intent.JustChose {
				t.Errorf("past the hold: %+v, changes %+v", r, got.Changes)
			}
		}},
	}
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) { c.run(t, newIntentScene(t)) })
	}
}

// TestIntentFakeJevIgnored: a fake paper_card answer is no signal (cold, no row); the same
// answer from real Jev is paper.
func TestIntentFakeJevIgnored(t *testing.T) {
	s := newIntentScene(t)
	fake := s.cafe(t, "fake-jev.example", "")
	s.answer(t, "fake_jev.example", leadstore.IGVendor, "paper_card", leadstore.IGFakeModel)
	s.answer(t, "fake_jev.example", leadstore.IGLoyalty, "runs_scheme", leadstore.IGFakeModel)
	s.match(t, "fake_jev.example", fake, leadstore.IGTierHandle)
	realLead := s.cafe(t, "real-jev.example", "")
	s.answer(t, "real_jev.example", leadstore.IGVendor, "paper_card", igRealModel)
	s.match(t, "real_jev.example", realLead, leadstore.IGTierHandle)
	s.sync(t)
	if r := s.intentOf(t, fake); r.Class != intent.Cold || s.rows(t, fake) != 0 {
		t.Errorf("fake paper_card: %+v, %d rows; want cold, none", r, s.rows(t, fake))
	}
	if r := s.intentOf(t, realLead); r.Class != intent.Paper {
		t.Errorf("real paper_card: %+v, want paper", r)
	}
}

// TestIntentOrderingVendorFollowers: a follower on an ordering vendor's list is no follow;
// only the loyalty vendor counts, so the café is curious with overlap 1, not shopping.
func TestIntentOrderingVendorFollowers(t *testing.T) {
	s := newIntentScene(t)
	s.cfg = intentConfig(orderingConfig(t))
	if v := s.cfg.VendorAccount(igOrdering); v != "ordering-example" {
		t.Fatalf("the ordering vendor's account maps to %q; the test needs it seen", v)
	}
	l := s.cafe(t, "orders.example", "orders.example")
	s.follows(t, igOrdering, "orders.example", s.now.Add(-time.Hour))
	s.follows(t, igStampino, "orders.example", s.now.Add(-time.Hour))
	in, err := s.q.IntentInputs(s.ctx, s.cfg, 0, s.now)
	if err != nil {
		t.Fatal(err)
	}
	if f := in[l].Follows; len(f) != 1 || f[0].Vendor != "stampino" {
		t.Errorf("follows = %+v, want stampino only", f)
	}
	s.sync(t)
	if r := s.intentOf(t, l); r.Class != intent.Curious || r.Overlap != 1 {
		t.Errorf("ordering + one loyalty follow: %+v, want curious, overlap 1", r)
	}
}

// TestIntentTwoHandles: a café holding two strongly matched handles reads the union of their
// signals, and a vendor both follow counts once.
func TestIntentTwoHandles(t *testing.T) {
	s := newIntentScene(t)
	l := s.cafe(t, "two.example", "two_roastery.example")
	s.capture(t, "two_cafe.example")
	s.match(t, "two_cafe.example", l, leadstore.IGTierOperator)
	s.follows(t, igStampino, "two_roastery.example", s.now.Add(-time.Hour))
	s.follows(t, igStampino, "two_cafe.example", s.now.Add(-time.Hour))
	s.sync(t)
	if r := s.intentOf(t, l); r.Class != intent.Curious || r.Overlap != 1 {
		t.Errorf("both follow Stampino: %+v, want curious with overlap 1 (once per vendor)", r)
	}
	s.follows(t, igTuanis, "two_cafe.example", s.now.Add(-time.Hour))
	s.sync(t)
	if r := s.intentOf(t, l); r.Class != intent.Shopping || r.Overlap != 2 {
		t.Errorf("Stampino on one, Tuanis on the other: %+v, want shopping with overlap 2", r)
	}
	// A weak (name) match of a third handle adds nothing.
	s.capture(t, "two_weak.example")
	s.match(t, "two_weak.example", l, leadstore.IGTierName)
	s.follows(t, igPick, "two_weak.example", s.now.Add(-time.Hour))
	s.sync(t)
	if r := s.intentOf(t, l); r.Overlap != 2 {
		t.Errorf("a weak match counted: %+v", r)
	}
}

// TestIntentOwnVendorAndRoastery: a touch from the own vendor's account is out and held; a
// roastery is out and not held by intent.
func TestIntentOwnVendorAndRoastery(t *testing.T) {
	s := newIntentScene(t)
	own := s.cafe(t, "customer.example", "customer.example")
	s.touch(t, igOwn, "customer.example", leadstore.IGTouchMention, "", s.now.Add(-400*time.Hour))
	roast := s.cafe(t, "roast.example", "roast.example")
	s.place(t, roast, leadstore.PlaceRoastery, leadstore.PlaceFromOperator)
	s.sync(t)
	if r := s.intentOf(t, own); r.Class != intent.Out || !r.Held || r.Reason != "already a Webbership customer" {
		t.Errorf("own vendor: %+v, want out and held", r)
	}
	r := s.intentOf(t, roast)
	if r.Class != intent.Out || r.Held || r.Rank != 7 {
		t.Errorf("roastery: %+v, want out, not held", r)
	}
	// The hold tells a roastery out by its reason: pin the words against the rules.
	if c := intent.Classify(intent.Inputs{Now: s.now, PlaceType: leadstore.PlaceRoastery}); r.Reason != c.Reason || c.Held {
		t.Errorf("roastery reason %q, intent@1 says %q (held %v)", r.Reason, c.Reason, c.Held)
	}
	// Your out on the roastery holds it.
	s.call(t, roast, strp(intent.Out), "")
	if r := s.intentOf(t, roast); !r.Held || !r.Yours {
		t.Errorf("your out on a roastery: %+v, want held", r)
	}
}

// TestIntentNonCafeInvariant: after a sync on a mixed seed (a Drupal lead with café-shaped
// signals), no non-café lead holds a lead_intent row; a call on one is refused.
func TestIntentNonCafeInvariant(t *testing.T) {
	s := newIntentScene(t)
	drupal := s.siteLead(t, "drupal-intent.example", nil, 7, "")
	s.capture(t, "drupal_intent.example")
	s.match(t, "drupal_intent.example", drupal, leadstore.IGTierHandle)
	s.follows(t, igStampino, "drupal_intent.example", s.now.Add(-time.Hour))
	s.follows(t, igPick, "drupal_intent.example", s.now.Add(-time.Hour))
	c := s.cafe(t, "mixed-cafe.example", "mixed_cafe.example")
	s.follows(t, igStampino, "mixed_cafe.example", s.now.Add(-time.Hour))
	s.sync(t)
	kinds := []string{}
	for _, k := range cafe.Kinds {
		kinds = append(kinds, k.Kind)
	}
	if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_intent i JOIN lead l ON l.id = i.lead_id
		WHERE NOT l.kind = ANY($1::text[])`, kinds); n != 0 {
		t.Errorf("%d lead_intent rows on non-café leads", n)
	}
	if s.rows(t, c) != 1 {
		t.Errorf("the café lead got %d rows, want 1", s.rows(t, c))
	}
	if r := s.intentOf(t, drupal); r.Class != intent.Cold || r.Rank != 5 || r.Held {
		t.Errorf("drupal lead reads %+v, want cold rank 5", r)
	}
	if err := s.q.SetIntentCall(s.ctx, drupal, strp(intent.Out), ""); !errors.Is(err, leadstore.ErrNotCafeLead) {
		t.Errorf("call on a Drupal lead: %v, want ErrNotCafeLead", err)
	}
}

// TestIntentCallRefusals: an unknown class, an unknown lead and a long note are refused with
// their errors and write nothing.
func TestIntentCallRefusals(t *testing.T) {
	s := newIntentScene(t)
	l := s.cafe(t, "refuse.example", "")
	if err := s.q.SetIntentCall(s.ctx, l, strp("hot"), ""); !errors.Is(err, leadstore.ErrUnknownIntentClass) {
		t.Errorf("unknown class: %v", err)
	}
	if err := s.q.SetIntentCall(s.ctx, 99999999, strp(intent.Out), ""); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("unknown lead: %v", err)
	}
	long := make([]byte, leadstore.IntentMaxNote+1)
	for i := range long {
		long[i] = 'x'
	}
	if err := s.q.SetIntentCall(s.ctx, l, strp(intent.Out), string(long)); !errors.Is(err, leadstore.ErrBadIntentNote) {
		t.Errorf("long note: %v", err)
	}
	if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_intent_call`); n != 0 {
		t.Errorf("refusals wrote %d calls", n)
	}
	if _, err := s.q.SyncIntent(s.ctx, leadstore.IntentConfig{}, 0, s.now, true); !errors.Is(err, leadstore.ErrBadIntentConfig) {
		t.Errorf("empty config: %v", err)
	}
}
