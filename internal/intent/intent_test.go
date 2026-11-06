package intent

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func ago(days int) time.Time { return now.Add(-time.Duration(days) * day) }

var cues = []string{"cat", "pret", "pretul", "price", "cost", "costa", "info", "detalii", "details", "dm", "cum", "how", "demo", "oferta"}

var names = map[string]string{"stampino": "Stampino", "pick": "Pick Coupons", "cardora": "Cardora", "webbership": "Webbership"}

func base() Inputs {
	return Inputs{Now: now, PlaceType: "cafe", OwnVendor: "webbership", QuestionCues: cues,
		VendorName: func(id string) string { return names[id] }}
}

func with(f func(*Inputs)) Inputs { in := base(); f(&in); return in }

func comment(v string, days int, text string) Touch {
	return Touch{Vendor: v, Kind: KindComment, At: ago(days), Text: text}
}

func tag(v, kind string, days int) Touch { return Touch{Vendor: v, Kind: kind, At: ago(days)} }

func follows(vs ...string) []Dated {
	out := make([]Dated, len(vs))
	for i, v := range vs {
		out[i] = Dated{Vendor: v, At: ago(3)}
	}
	return out
}

func TestClassifyRules(t *testing.T) {
	cases := []struct {
		name   string
		in     Inputs
		class  string
		rule   int
		reason string // substring
		held   bool
	}{
		{"1 call wins", with(func(in *Inputs) { in.Call = Paper; in.CallNote = "saw cards\nat the bar" }), Paper, 1, "your call: saw cards at the bar", false},
		{"1 call beats own vendor", with(func(in *Inputs) { in.Call = Shopping; in.RuleVendor = "webbership" }), Shopping, 1, "your call", false},
		{"1 call out is held", with(func(in *Inputs) { in.Call = Out }), Out, 1, "your call", true},
		{"1 unknown call ignored", with(func(in *Inputs) { in.Call = "bogus" }), Cold, 11, "no loyalty signal", false},
		{"2 own via rule vendor", with(func(in *Inputs) { in.RuleVendor = "webbership"; in.Follows = follows("stampino", "pick") }), Out, 2, "already a Webbership customer", true},
		{"2 own via site", with(func(in *Inputs) { in.SiteVendors = []string{"webbership"} }), Out, 2, "Webbership customer", true},
		{"2 own via touch", with(func(in *Inputs) { in.Touches = []Touch{tag("webbership", KindUsertag, 10)} }), Out, 2, "Webbership customer", true},
		{"2 own comment is not use", with(func(in *Inputs) { in.Touches = []Touch{comment("webbership", 10, "cât costă?")} }), Cold, 11, "", false},
		{"3 not coffee", with(func(in *Inputs) { in.JevPlace = "not_coffee"; in.Follows = follows("stampino", "pick") }), Out, 3, "not a coffee place", true},
		{"4 roastery + follows 2", with(func(in *Inputs) { in.PlaceType = "roastery"; in.Follows = follows("stampino", "pick") }), Out, 4, "a roastery", false},
		{"5 asking comment", with(func(in *Inputs) { in.Touches = []Touch{comment("stampino", 6, "cât costă?")} }), Shopping, 5, "asked Stampino on 31 Oct: «cât costă?»", false},
		{"5 asking word, no ?", with(func(in *Inputs) { in.Touches = []Touch{comment("stampino", 6, "Prețul pentru 2 locații")} }), Shopping, 5, "asked Stampino", false},
		{"5 beats just chose", with(func(in *Inputs) {
			in.Touches = []Touch{comment("stampino", 6, "info?"), tag("pick", KindUsertag, 2)}
		}), Shopping, 5, "asked Stampino", false},
		{"6 usertag 59 days", with(func(in *Inputs) { in.Touches = []Touch{tag("stampino", KindUsertag, 59)} }), JustChose, 6, "Stampino tagged it on", true},
		{"6 mention", with(func(in *Inputs) { in.Touches = []Touch{tag("stampino", KindMention, 4)} }), JustChose, 6, "Stampino mentioned it on 2 Nov", true},
		{"6 tagged beats follows 2", with(func(in *Inputs) {
			in.Touches = []Touch{tag("stampino", KindTagged, 4)}
			in.Follows = follows("stampino", "pick")
		}), JustChose, 6, "it tagged Stampino on 2 Nov", true},
		{"9 tag 61 days is switch", with(func(in *Inputs) { in.Touches = []Touch{tag("stampino", KindUsertag, 61)} }), Switch, 9, "runs Stampino (tag on", false},
		{"11 tag 366 days ignored", with(func(in *Inputs) { in.Touches = []Touch{tag("stampino", KindUsertag, 366)} }), Cold, 11, "", false},
		{"7 follows 2", with(func(in *Inputs) { in.Follows = follows("stampino", "pick") }), Shopping, 7, "follows Pick Coupons and Stampino", false},
		{"7 follows 2 + paper", with(func(in *Inputs) { in.Follows = follows("stampino", "pick"); in.JevVendor = "paper_card" }), Shopping, 7, "follows", false},
		{"8 paper card", with(func(in *Inputs) { in.JevVendor = "paper_card" }), Paper, 8, "paper stamp card", false},
		{"8 paper + named vendor", with(func(in *Inputs) { in.JevVendor = "paper_card"; in.RuleVendor = "cardora" }), Paper, 8, "paper", false},
		{"8 unnamed app", with(func(in *Inputs) { in.JevVendor = "unnamed_app_or_qr" }), Paper, 8, "app or QR", false},
		{"8 runs scheme no vendor", with(func(in *Inputs) { in.JevLoyalty = "runs_scheme" }), Paper, 8, "loyalty scheme", false},
		{"9 runs scheme + vendor", with(func(in *Inputs) { in.JevLoyalty = "runs_scheme"; in.SiteVendors = []string{"cardora"} }), Switch, 9, "runs Cardora (website)", false},
		{"8 site word", with(func(in *Inputs) { in.SiteWord = true }), Paper, 8, "website", false},
		{"9 site word + rule vendor", with(func(in *Inputs) { in.SiteWord = true; in.RuleVendor = "cardora" }), Switch, 9, "runs Cardora (profile)", false},
		{"9 jev vendor", with(func(in *Inputs) { in.JevVendor = "cardora"; in.SiteVendors = []string{"cardora"} }), Switch, 9, "runs Cardora (Instagram, website)", false},
		{"fake-free: JevVendor empty, no paper", with(func(in *Inputs) { in.JevVendor = "" }), Cold, 11, "", false},
		{"jev none is not named", with(func(in *Inputs) { in.JevVendor = "none" }), Cold, 11, "", false},
		{"10 follows 1", with(func(in *Inputs) { in.Follows = follows("stampino") }), Curious, 10, "follows Stampino", false},
		{"10 prepaid", with(func(in *Inputs) { in.Prepaid = true }), Curious, 10, "prepaid", false},
		{"10 thanks comment", with(func(in *Inputs) { in.Touches = []Touch{comment("stampino", 30, "mulțumim Stampino!")} }), Curious, 10, "commented on Stampino", false},
		{"10 asking at 91 days", with(func(in *Inputs) { in.Touches = []Touch{comment("stampino", 91, "cât costă?")} }), Curious, 10, "commented", false},
		{"10 asking at 120 never switch", with(func(in *Inputs) { in.Touches = []Touch{comment("stampino", 120, "cât costă?")} }), Curious, 10, "", false},
		{"10 empty text never asking", with(func(in *Inputs) { in.Touches = []Touch{comment("stampino", 5, "")} }), Curious, 10, "", false},
		{"11 comment 366 days ignored", with(func(in *Inputs) { in.Touches = []Touch{comment("stampino", 366, "cât?")} }), Cold, 11, "", false},
		{"11 cold, hint only", with(func(in *Inputs) { in.FollowedBy = follows("stampino") }), Cold, 11, "no loyalty signal yet", false},
		{"cue is a whole word", with(func(in *Inputs) { in.Touches = []Touch{comment("stampino", 5, "categoric frumos")} }), Curious, 10, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Classify(c.in)
			if r.Class != c.class || r.Rule != c.rule {
				t.Fatalf("class %q rule %d, want %q rule %d (reason %q)", r.Class, r.Rule, c.class, c.rule, r.Reason)
			}
			if !strings.Contains(r.Reason, c.reason) {
				t.Errorf("reason %q lacks %q", r.Reason, c.reason)
			}
			if r.Held != c.held {
				t.Errorf("held %v, want %v", r.Held, c.held)
			}
			if r.Held && !r.HeldAt(now) {
				t.Errorf("held result not held at now")
			}
			checkReason(t, r.Reason)
		})
	}
}

func checkReason(t *testing.T, s string) {
	t.Helper()
	if s == "" || len(s) > 300 || strings.ContainsAny(s, "\n\r") {
		t.Errorf("bad reason (%d bytes): %q", len(s), s)
	}
}

func TestHeldUntil(t *testing.T) {
	r := Classify(with(func(in *Inputs) { in.Touches = []Touch{tag("stampino", KindUsertag, 59)} }))
	if r.HeldUntil == nil || !r.HeldUntil.Equal(ago(59).Add(60*day)) {
		t.Fatalf("held_until %v", r.HeldUntil)
	}
	if !r.HeldAt(now) || r.HeldAt(now.Add(2*day)) {
		t.Errorf("hold should end at tag + 60 days")
	}
	if r.SignalAt == nil || !r.SignalAt.Equal(ago(59)) {
		t.Errorf("signal_at %v", r.SignalAt)
	}

	c := Classify(with(func(in *Inputs) { in.Call = JustChose }))
	if c.HeldUntil == nil || !c.HeldUntil.Equal(now.Add(60*day)) || !c.Held {
		t.Errorf("call held_until %v", c.HeldUntil)
	}
	own := now.Add(10 * day)
	c = Classify(with(func(in *Inputs) { in.Call = JustChose; in.CallHeldUntil = &own }))
	if c.HeldUntil == nil || !c.HeldUntil.Equal(own) {
		t.Errorf("call's own held_until not kept: %v", c.HeldUntil)
	}

	ro := Classify(with(func(in *Inputs) { in.PlaceType = "roastery" }))
	if ro.Held || ro.HeldAt(now) || ro.HeldUntil != nil {
		t.Errorf("roastery out must not be held by intent")
	}
	if Classify(base()).HeldAt(now) {
		t.Errorf("cold held")
	}
}

func TestScore(t *testing.T) {
	cases := []struct {
		name string
		in   Inputs
		want int
	}{
		{"nothing", base(), 0},
		{"follows 2 each", with(func(in *Inputs) { in.Follows = follows("stampino", "pick", "stampino") }), 4},
		{"hint 1", with(func(in *Inputs) { in.FollowedBy = follows("stampino") }), 1},
		{"prepaid 1", with(func(in *Inputs) { in.Prepaid = true }), 1},
		{"named sources 1 each", with(func(in *Inputs) {
			in.JevVendor = "cardora"
			in.RuleVendor = "cardora"
			in.SiteVendors = []string{"cardora", "stampino"}
		}), 4},
		{"comment 31 days", with(func(in *Inputs) { in.Touches = []Touch{comment("stampino", 31, "hi")} }), 3},
		{"comment 30 days doubles", with(func(in *Inputs) { in.Touches = []Touch{comment("stampino", 30, "hi")} }), 6},
		{"tag 31 days", with(func(in *Inputs) { in.Touches = []Touch{tag("stampino", KindMention, 31)} }), 2},
		{"tag 10 days doubles", with(func(in *Inputs) { in.Touches = []Touch{tag("stampino", KindTagged, 10)} }), 4},
		{"366 days ignored", with(func(in *Inputs) { in.Touches = []Touch{tag("stampino", KindTagged, 366), comment("pick", 400, "x")} }), 0},
		{"call uses derived score", with(func(in *Inputs) { in.Call = Cold; in.Follows = follows("stampino") }), 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.in).Score; got != c.want {
				t.Errorf("score %d, want %d", got, c.want)
			}
		})
	}
}

func TestOverlapUnionOfHandles(t *testing.T) {
	// Two handles (a roastery and a café account) both follow Stampino: counted once.
	in := with(func(in *Inputs) {
		in.Follows = []Dated{{"stampino", ago(5)}, {"stampino", ago(2)}, {"pick", ago(9)}, {"webbership", ago(1)}}
	})
	r := Classify(in)
	if r.Overlap != 2 || r.Class != Shopping {
		t.Fatalf("overlap %d class %s", r.Overlap, r.Class)
	}
	if r.SignalAt == nil || !r.SignalAt.Equal(ago(2)) {
		t.Errorf("signal_at %v, want latest capture", r.SignalAt)
	}
	one := Classify(with(func(in *Inputs) { in.Follows = []Dated{{"stampino", ago(5)}, {"stampino", ago(2)}} }))
	if one.Overlap != 1 || one.Class != Curious {
		t.Errorf("same vendor twice: overlap %d class %s", one.Overlap, one.Class)
	}
}

func TestReasonBounded(t *testing.T) {
	long := strings.Repeat("ăbc\ndef ", 100) + "?"
	r := Classify(with(func(in *Inputs) { in.Touches = []Touch{comment("stampino", 3, long)} }))
	checkReason(t, r.Reason)
	if !strings.Contains(r.Reason, "«") || !strings.Contains(r.Reason, "…»") {
		t.Errorf("quote not cut: %q", r.Reason)
	}
	q := r.Reason[strings.Index(r.Reason, "«")+len("«") : strings.Index(r.Reason, "…»")]
	if n := len([]rune(q)); n > 60 {
		t.Errorf("quote %d runes", n)
	}
	c := Classify(with(func(in *Inputs) { in.Call = Out; in.CallNote = strings.Repeat("ț long note\n", 60) }))
	checkReason(t, c.Reason)
	// No VendorName: ids stand in.
	n := Classify(Inputs{Now: now, Follows: follows("stampino", "pick")})
	if n.Reason != "follows pick and stampino" {
		t.Errorf("reason %q", n.Reason)
	}
}

func TestDeterministic(t *testing.T) {
	in := with(func(in *Inputs) {
		in.Follows = []Dated{{"stampino", ago(5)}, {"pick", ago(9)}, {"cardora", ago(1)}}
		in.FollowedBy = []Dated{{"pick", ago(4)}, {"stampino", ago(8)}}
		in.Touches = []Touch{
			comment("stampino", 20, "frumos"), comment("pick", 20, "super"), comment("cardora", 50, "cum funcționează?"),
			comment("pick", 50, "cât costă?"), tag("stampino", KindUsertag, 80), tag("pick", KindMention, 80),
		}
		in.SiteVendors = []string{"stampino", "cardora", "pick"}
	})
	want := Classify(in)
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // G404: a seeded source, so each shuffle repeats
	for i := 0; i < 50; i++ {
		s := in
		s.Follows = append([]Dated(nil), in.Follows...)
		s.FollowedBy = append([]Dated(nil), in.FollowedBy...)
		s.Touches = append([]Touch(nil), in.Touches...)
		s.SiteVendors = append([]string(nil), in.SiteVendors...)
		rng.Shuffle(len(s.Follows), func(a, b int) { s.Follows[a], s.Follows[b] = s.Follows[b], s.Follows[a] })
		rng.Shuffle(len(s.FollowedBy), func(a, b int) { s.FollowedBy[a], s.FollowedBy[b] = s.FollowedBy[b], s.FollowedBy[a] })
		rng.Shuffle(len(s.Touches), func(a, b int) { s.Touches[a], s.Touches[b] = s.Touches[b], s.Touches[a] })
		rng.Shuffle(len(s.SiteVendors), func(a, b int) { s.SiteVendors[a], s.SiteVendors[b] = s.SiteVendors[b], s.SiteVendors[a] })
		if got := Classify(s); !reflect.DeepEqual(got, want) {
			t.Fatalf("shuffle %d: %+v, want %+v", i, got, want)
		}
	}
	// The two same-day asking comments tie on date; vendor id decides.
	if !strings.HasPrefix(want.Reason, "asked Cardora") {
		t.Errorf("tie not broken by vendor id: %q", want.Reason)
	}
}

func TestRank(t *testing.T) {
	want := map[string]int{Shopping: 1, Paper: 2, Switch: 3, Curious: 4, Cold: 5, JustChose: 6, Out: 7, "": 5, "bogus": 5}
	for c, n := range want {
		if got := Rank(c); got != n {
			t.Errorf("Rank(%q) = %d, want %d", c, got, n)
		}
	}
	if Rules != "intent@1" {
		t.Errorf("Rules %q", Rules)
	}
}

func TestWordsFold(t *testing.T) {
	got := words("Cât COSTĂ, prețul? Îți ştiu ţara-ș")
	want := []string{"cat", "costa", "pretul", "iti", "stiu", "tara", "s"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("words = %v", got)
	}
}
