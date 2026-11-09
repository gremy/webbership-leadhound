package campaign

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A spec with no card keeps its hash: Card is a nil pointer with omitempty, so the canonical
// form never names it.
func TestNilCardKeepsOldHash(t *testing.T) {
	s := validSpec(t)
	if s.Card != nil {
		t.Fatalf("valid.yaml holds no card, got %+v", s.Card)
	}
	j, h1, err := Canonical(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(j), `"card"`) {
		t.Errorf("canonical form of a spec without a card names it: %s", j)
	}
	s.Card = &Card{Pitch: "A demo pitch."}
	if _, h2, _ := Canonical(s); h1 == h2 {
		t.Error("a spec with a card kept the hash of the one without")
	}
}

// Every fixture spec, with its card dropped, hashes exactly as it did before the card field
// existed, so registering a spec without a card makes no new campaign version. The hashes
// were computed from the code before Card was added (9 Nov 2026). A new fixture fails here
// until its hash is pinned.
func TestFixtureSpecsWithoutCardHashAsBefore(t *testing.T) {
	before := map[string]string{
		"../../examples/campaigns/cafes-loyalty/campaign.yaml": "27f18ec6802bdbc9031bf8ff2bffb5501a440d6f9e89e6d3b6d81e03046861e3",
		"../../examples/campaigns/drupal-eol/campaign.yaml":    "02b27c480a5a4e732d4ada528aa9868c0ff0ea833b5a8a6ab33dd60b80b846ba",
		"testdata/drafting.yaml":                               "0a064a207e78bf7e7d3da7f93f59f26ea0a6e2523c5bfaa015a9328156f3ff16",
		"testdata/reordered.yaml":                              "e66f636617d0c3b47483a5c333b2284911ceccf9910b3f2eae9861708f75094d",
		"testdata/valid.yaml":                                  "e66f636617d0c3b47483a5c333b2284911ceccf9910b3f2eae9861708f75094d",
	}
	paths, err := filepath.Glob(filepath.Join(examplesDir, "campaigns", "*", FileName))
	if err != nil {
		t.Fatal(err)
	}
	more, err := filepath.Glob(filepath.Join("testdata", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, p := range append(paths, more...) {
		s, err := Load(p)
		if err != nil {
			continue // a fixture that is invalid on purpose has no registered hash
		}
		key := filepath.ToSlash(p)
		want, ok := before[key]
		if !ok {
			t.Errorf("%s: no pinned hash; add it here", key)
			continue
		}
		seen++
		s.Card = nil
		j, got, err := Canonical(s)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: hash without its card = %s; want %s\n%s", key, got, want, j)
		}
	}
	if seen != len(before) {
		t.Errorf("checked %d fixture specs; want %d", seen, len(before))
	}
}

func TestCardDecode(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "valid.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b) + "card:\n  pitch: A demo pitch.\n  pain: A demo pain.\n  criteria: [one, two]\n"
	s, err := Decode([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if s.Card == nil || s.Card.Pitch != "A demo pitch." || s.Card.Pain != "A demo pain." || len(s.Card.Criteria) != 2 {
		t.Errorf("card not decoded: %+v", s.Card)
	}
	if err := s.Check(known); err != nil {
		t.Errorf("Check: %v", err)
	}
	if _, err := Decode([]byte(string(b) + "card:\n  pitches: x\n")); err == nil || !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown card key decoded: %v", err)
	}
}

func TestCardCheckRules(t *testing.T) {
	ok := func() *Card {
		return &Card{Pitch: "A demo pitch.", Pain: "A demo pain.", Criteria: []string{"one", "two"}}
	}
	cases := []struct {
		name string
		edit func(*Card)
		want string
	}{
		{"pitch too long", func(c *Card) { c.Pitch = strings.Repeat("a", 161) }, "card.pitch"},
		{"pain too long", func(c *Card) { c.Pain = strings.Repeat("a", 161) }, "card.pain"},
		{"pitch line break", func(c *Card) { c.Pitch = "a\nb" }, "card.pitch"},
		{"pain line break", func(c *Card) { c.Pain = "a\r\nb" }, "card.pain"},
		{"too many criteria", func(c *Card) { c.Criteria = []string{"a", "b", "c", "d", "e", "f"} }, "card.criteria"},
		{"criterion too long", func(c *Card) { c.Criteria = []string{strings.Repeat("a", 81)} }, "card.criteria"},
		{"criterion line break", func(c *Card) { c.Criteria = []string{"a\nb"} }, "card.criteria"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec(t)
			s.Card = ok()
			tc.edit(s.Card)
			err := s.Check(known)
			if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Check = %v; want an error naming %q", err, tc.want)
			}
		})
	}

	// Characters are counted as runes, so 160 two-byte letters still pass.
	s := validSpec(t)
	s.Card = ok()
	s.Card.Pitch = strings.Repeat("ă", 160)
	s.Card.Pain = strings.Repeat("ș", 160)
	s.Card.Criteria = []string{strings.Repeat("î", 80), "b", "c", "d", "e"}
	if err := s.Check(known); err != nil {
		t.Errorf("a card at every limit: %v", err)
	}
}
