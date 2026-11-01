package leadrate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestGoldenFromLabels: stored calls become golden lines named by domain, with the input
// they were made on and their band; a cleared call is skipped; a lead with no usable domain
// is named by its id; an input that does not read, one with no kind, or a band that is not
// one is an error.
func TestGoldenFromLabels(t *testing.T) {
	in := Input{Kind: leadstore.LeadUnsupported, Major: 7, Version: "7.98", EOL: time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC),
		Country: "GB", SiteName: "Northwind", Channels: []string{"email"}, HasPhone: true, SocialLinks: 2}
	doc, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	s := func(v string) *string { return &v }
	got, err := GoldenFromLabels([]leadstore.LeadLabel{
		{LeadID: 5, Domain: "northwind.example", Band: s("likely"), Input: doc},
		{LeadID: 6, Domain: "gone.example"}, // cleared
		{LeadID: 7, Domain: "two words", Band: s("no"), Input: []byte(`{"kind":"d10"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d lines, want 2: %+v", len(got), got)
	}
	if g := got[0]; g.ID != "northwind.example" || *g.Label != "likely" || g.Note != "lead 5" || !reflect.DeepEqual(g.Input, in) {
		t.Errorf("first line %+v", g)
	}
	if g := got[1]; g.ID != "lead-7" || *g.Label != "no" || g.Input.Kind != leadstore.LeadD10 {
		t.Errorf("second line %+v", g)
	}
	for name, l := range map[string]leadstore.LeadLabel{
		"bad band":      {LeadID: 1, Band: s("great"), Input: doc},
		"bad input":     {LeadID: 1, Band: s("maybe"), Input: []byte(`{"kind":"d10","colour":"red"}`)},
		"no kind":       {LeadID: 1, Band: s("maybe"), Input: []byte(`{}`)},
		"broken input":  {LeadID: 1, Band: s("maybe"), Input: []byte(`{`)},
		"date not date": {LeadID: 1, Band: s("maybe"), Input: []byte(`{"kind":"d10","eol":"soon"}`)},
	} {
		if _, err := GoldenFromLabels([]leadstore.LeadLabel{l}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestDescribe: every band has its plain description in each type's question, the one the
// decider reads; the café descriptions are the café question's own and name no Drupal; a
// type that is not known reads as Drupal; a name that is not a band has none.
func TestDescribe(t *testing.T) {
	for i, b := range bands {
		if Describe(leadstore.TypeDrupal, b.name) != b.description || b.description == "" {
			t.Errorf("drupal %s: %q", b.name, Describe(leadstore.TypeDrupal, b.name))
		}
		c := Describe(leadstore.TypeCafe, b.name)
		if c != cafeBands[i].description || c == "" || c == b.description || strings.Contains(c, "Drupal") {
			t.Errorf("cafe %s: %q", b.name, c)
		}
		if cafeBands[i].name != b.name || cafeBands[i].weight != b.weight {
			t.Errorf("cafe band %d is %s/%d, want %s/%d", i, cafeBands[i].name, cafeBands[i].weight, b.name, b.weight)
		}
		if Describe("no_such_type", b.name) != b.description {
			t.Errorf("an unknown type does not read as drupal for %s", b.name)
		}
	}
	for _, ty := range leadstore.RatingTypes {
		if Describe(ty, "great") != "" {
			t.Errorf("%s: a name that is not a band has a description", ty)
		}
	}
}
