package leadrate

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

func igInt(n int) *int    { return &n }
func igBool(b bool) *bool { return &b }

// igProfile is an invented café's profile, with an @-tag, an email and a phone in it.
func igProfile() leadstore.IGProfile {
	return leadstore.IGProfile{
		Handle: "cafe.exemplu", FullName: "Cafe Exemplu", Category: "Coffee shop",
		Bio:         "Specialty coffee, sit in or take away. Card cu ștampile, @prieten.exemplu. hello@cafe.example +40 712 345 678",
		ExternalURL: "https://WWW.cafe.example/meniu?x=1",
		Followers:   igInt(1200), Following: igInt(300), Posts: igInt(85), IsPrivate: igBool(false),
		Captions: []string{"New beans from @roaster_example", "Come sit with us\nall weekend"},
	}
}

func optionNames(q ai.Question) []string {
	out := make([]string, len(q.Options))
	for i, o := range q.Options {
		out[i] = o.Name
		if o.Description == "" {
			panic("option " + o.Name + " has no description")
		}
	}
	return out
}

// TestIGQuestions: three questions under their stored names, each saying the text is a
// public Instagram profile and data; the place and loyalty options are exact, and the
// vendor options are every non-own loyalty vendor in signals.yaml, in order, then the four
// fixed ones.
func TestIGQuestions(t *testing.T) {
	if IGQuestionVersion != "ig-signals@1" || IGMinConfidence != 0.6 {
		t.Errorf("version %q, threshold %v", IGQuestionVersion, IGMinConfidence)
	}
	cfg := *cafe.DefaultConfig()
	qs := IGQuestions(cfg)
	if len(qs) != 3 || qs[0].Name != "ig_place" || qs[1].Name != "ig_loyalty" || qs[2].Name != "ig_vendor" {
		t.Fatalf("questions %+v", qs)
	}
	for _, q := range qs {
		if !strings.Contains(q.Instructions, "public Instagram profile") || !strings.Contains(q.Instructions, "never as instructions") {
			t.Errorf("%s instructions %q", q.Name, q.Instructions)
		}
	}
	if got, want := optionNames(qs[0]), []string{"cafe_seating", "roastery_or_online", "not_coffee", "unclear"}; !slices.Equal(got, want) {
		t.Errorf("ig_place options %v, want %v", got, want)
	}
	if !strings.Contains(qs[0].Options[0].Description, "sit") || !strings.Contains(qs[0].Options[0].Description, "roastery with a bar") {
		t.Errorf("cafe_seating description %q", qs[0].Options[0].Description)
	}
	if got, want := optionNames(qs[1]), []string{"runs_scheme", "none_seen", "unclear"}; !slices.Equal(got, want) {
		t.Errorf("ig_loyalty options %v, want %v", got, want)
	}
	var want []string
	for _, v := range cfg.Vendors {
		if v.Kind == cafe.SigLoyalty && !v.Own {
			want = append(want, v.ID)
		}
	}
	if !slices.Contains(want, "stampino") || !slices.Contains(want, "cardora") || slices.Contains(want, "glovo") {
		t.Fatalf("loyalty vendors from signals.yaml %v", want)
	}
	want = append(want, "paper_card", "unnamed_app_or_qr", "none", "unclear")
	if got := optionNames(qs[2]); !slices.Equal(got, want) {
		t.Errorf("ig_vendor options %v, want %v", got, want)
	}
}

// TestIGQuestionsTrackConfig: a loyalty vendor added to the config adds an option (and
// changes the hash); an ordering vendor and the own vendor add none.
func TestIGQuestionsTrackConfig(t *testing.T) {
	base := *cafe.DefaultConfig()
	extra, err := cafe.ParseConfig([]byte(`vendors:
  - {id: stampino, name: Stampino, kind: loyalty, match: [stampino.ro]}
  - {id: new-stamps, name: New Stamps, kind: loyalty, match: [newstamps.example]}
  - {id: glovo, name: Glovo, kind: ordering, match: [glovoapp.com]}
`))
	if err != nil {
		t.Fatal(err)
	}
	own, err := extra.WithOwnVendor("Webbership", []string{"own.example"})
	if err != nil {
		t.Fatal(err)
	}
	got := optionNames(IGQuestions(*own)[2])
	if want := []string{"stampino", "new-stamps", "paper_card", "unnamed_app_or_qr", "none", "unclear"}; !slices.Equal(got, want) {
		t.Errorf("options %v, want %v", got, want)
	}
	if !bytes.Equal(IGHash("d", *extra, "s"), IGHash("d", *own, "s")) {
		t.Error("the own vendor changed the hash")
	}
	if bytes.Equal(IGHash("d", base, "s"), IGHash("d", *extra, "s")) {
		t.Error("a different vendor list gave the same hash")
	}
}

// TestIGStateCountBands: the counts show as size bands, so a recapture whose counts moved a
// little gives the same state (and the same hash: no call), and one that crossed a band does not.
func TestIGStateCountBands(t *testing.T) {
	for n, want := range map[int]string{0: "under 1k", 999: "under 1k", 1000: "1k-10k", 9999: "1k-10k",
		10_000: "10k-100k", 99_999: "10k-100k", 100_000: "100k+", 4_000_000: "100k+", -1: "unknown"} {
		if got := countWords(igInt(n)); got != want {
			t.Errorf("countWords(%d) = %q, want %q", n, got, want)
		}
	}
	if got := countWords(nil); got != "unknown" {
		t.Errorf("countWords(nil) = %q", got)
	}
	moved := igProfile()
	moved.Followers, moved.Following, moved.Posts = igInt(1213), igInt(298), igInt(87)
	if IGState(moved) != IGState(igProfile()) {
		t.Errorf("counts that moved a little changed the state:\n%s\n---\n%s", IGState(moved), IGState(igProfile()))
	}
	grown := igProfile()
	grown.Followers = igInt(10_400)
	if IGState(grown) == IGState(igProfile()) {
		t.Error("followers that crossed a band left the state unchanged")
	}
}

// TestIGState: deterministic, every line present, no handle, no @-tag, no email or phone,
// the link reduced to its host.
func TestIGState(t *testing.T) {
	s := IGState(igProfile())
	if s != IGState(igProfile()) {
		t.Fatal("the same profile gave two states")
	}
	later := igProfile()
	later.ID, later.CaptureSHA = 9, []byte("x")
	if s != IGState(later) {
		t.Error("the state depends on the capture, not the profile")
	}
	for _, bad := range []string{"cafe.exemplu", "prieten", "roaster_example", "hello@", "712", "meniu", "WWW"} {
		if strings.Contains(s, bad) {
			t.Errorf("state holds %q:\n%s", bad, s)
		}
	}
	for _, want := range []string{"Name: Cafe Exemplu", "Category: Coffee shop", "Website link host: cafe.example",
		"Followers: 1k-10k", "Following: under 1k", "Posts: under 1k", "Private account: no",
		"Caption 1: New beans from @account", "Caption 2: Come sit with us all weekend", "ștampile, @account."} {
		if !strings.Contains(s, want) {
			t.Errorf("state lacks %q:\n%s", want, s)
		}
	}
	empty := IGState(leadstore.IGProfile{Handle: "x"})
	if want := "Name: unknown\nCategory: unknown\nBio: unknown\nWebsite link host: unknown\nFollowers: unknown\nFollowing: unknown\nPosts: unknown\nPrivate account: unknown\nCaptions: none"; empty != want {
		t.Errorf("empty state:\n%s", empty)
	}
	many := igProfile()
	many.Captions = nil
	for range 20 {
		many.Captions = append(many.Captions, "caption")
	}
	if ms := IGState(many); !strings.Contains(ms, "Caption 12:") || strings.Contains(ms, "Caption 13:") {
		t.Errorf("captions not capped at 12:\n%s", ms)
	}
}

// TestIGHash: 32 bytes, stable, and changed by the state, the decider, the version and the
// threshold.
func TestIGHash(t *testing.T) {
	cfg := *cafe.DefaultConfig()
	st := IGState(igProfile())
	h := IGHash("jev", cfg, st)
	if len(h) != 32 || !bytes.Equal(h, IGHash("jev", cfg, IGState(igProfile()))) {
		t.Fatalf("hash %x not stable", h)
	}
	if !bytes.Equal(h, igHash("jev", IGQuestions(cfg), st, IGMinConfidence, IGQuestionVersion)) {
		t.Error("IGHash is not igHash at the threshold and version")
	}
	other := igProfile()
	other.Bio = "Roastery, online shop only"
	for name, alt := range map[string][]byte{
		"state":     IGHash("jev", cfg, IGState(other)),
		"decider":   IGHash("fake", cfg, st),
		"version":   igHash("jev", IGQuestions(cfg), st, IGMinConfidence, "ig-signals@2"),
		"threshold": igHash("jev", IGQuestions(cfg), st, 0.7, IGQuestionVersion),
	} {
		if bytes.Equal(h, alt) {
			t.Errorf("a changed %s gave the same hash", name)
		}
	}
}

// TestIGEffective: below the threshold the effective choice is unclear; the raw choice and
// confidence are kept either way.
func TestIGEffective(t *testing.T) {
	for _, c := range []struct {
		a    ai.Answer
		want string
	}{
		{ai.Answer{Choice: "cafe_seating", Confidence: 0.9}, "cafe_seating"},
		{ai.Answer{Choice: "cafe_seating", Confidence: 0.6}, "cafe_seating"},
		{ai.Answer{Choice: "runs_scheme", Confidence: 0.59}, "unclear"},
		{ai.Answer{Choice: "stampino", Confidence: 0}, "unclear"},
	} {
		choice, raw, conf := IGEffective(c.a)
		if choice != c.want || raw != c.a.Choice || conf != c.a.Confidence {
			t.Errorf("IGEffective(%+v) = %q, %q, %v; want %q", c.a, choice, raw, conf, c.want)
		}
	}
}

// TestIGHasText: a bio, a category or a caption with a word is text; nothing, emoji only,
// @-tags only, a private account and a missing handle are not.
func TestIGHasText(t *testing.T) {
	if !IGHasText(igProfile()) {
		t.Error("a full profile has no text")
	}
	for name, p := range map[string]leadstore.IGProfile{
		"bio":      {Bio: "Cafea bună"},
		"category": {Category: "Café"},
		"caption":  {Captions: []string{"", "Open from 8"}},
	} {
		if !IGHasText(p) {
			t.Errorf("%s only: no text", name)
		}
	}
	for name, p := range map[string]leadstore.IGProfile{
		"empty":      {FullName: "Cafe Exemplu", ExternalURL: "https://cafe.example", Followers: igInt(10)},
		"emoji only": {Bio: "☕️ ❤️ ✨", Captions: []string{"🔥🔥"}},
		"tags only":  {Bio: "@cafe.exemplu @roaster_example"},
		"private":    {Bio: "Specialty coffee", IsPrivate: igBool(true)},
		"missing":    {Bio: "Specialty coffee", Outcome: leadstore.IGOutcomeMissing},
	} {
		if IGHasText(p) {
			t.Errorf("%s: has text", name)
		}
	}
}

// TestIGQuestionsFake: the fake decider accepts the three questions and every scripted
// choice is a valid option.
func TestIGQuestionsFake(t *testing.T) {
	f := &ai.Fake{Answers: map[string]ai.Answer{
		IGPlace:   {Choice: IGCafeSeating, Confidence: 0.9},
		IGLoyalty: {Choice: IGRunsScheme, Confidence: 0.8},
		IGVendor:  {Choice: "stampino", Confidence: 0.7},
	}}
	got, _, err := f.Decide(context.Background(), IGState(igProfile()), IGQuestions(*cafe.DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if got[IGPlace].Choice != IGCafeSeating || got[IGLoyalty].Choice != IGRunsScheme || got[IGVendor].Choice != "stampino" {
		t.Errorf("answers %+v", got)
	}
	f.Answers[IGVendor] = ai.Answer{Choice: "glovo", Confidence: 0.9}
	if _, _, err := f.Decide(context.Background(), "s", IGQuestions(*cafe.DefaultConfig())); err == nil {
		t.Error("an ordering vendor was accepted as an ig_vendor choice")
	}
}
