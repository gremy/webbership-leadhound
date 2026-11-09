package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestCampaignViewCard: a spec's card words reach the view; a spec with no card leaves them
// blank, and so does a campaign with no spec at all.
func TestCampaignViewCard(t *testing.T) {
	spec := campaign.Spec{Card: &campaign.Card{Pitch: "A fictive pitch", Pain: "A fictive pain", Criteria: []string{"One", "Two"}}}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	v, err := campaignView(leadstore.Campaign{Slug: "fictive", Spec: raw}, "file", nil, leadconfig.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Pitch != "A fictive pitch" || v.Pain != "A fictive pain" || !slices.Equal(v.Criteria, []string{"One", "Two"}) {
		t.Errorf("card words %q, %q, %q", v.Pitch, v.Pain, v.Criteria)
	}
	raw, err = json.Marshal(campaign.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []leadstore.Campaign{{Slug: "fictive", Spec: raw}, {Slug: "fictive-bare"}} {
		v, err := campaignView(c, "", nil, leadconfig.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if v.Pitch != "" || v.Pain != "" || v.Criteria != nil {
			t.Errorf("%s: card words with no card: %+v", c.Slug, v)
		}
	}
}
