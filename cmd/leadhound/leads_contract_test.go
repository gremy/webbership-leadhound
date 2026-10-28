package main

import (
	"encoding/hex"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// contractBatchSHA is the content hash of the fixture batch below as the tree before
// campaigns (CP1) drafted it: the same test run on that tree printed it. Campaigns change no
// draft, so the hash must not move. If it does, a draft changed: find why before touching
// this value.
const contractBatchSHA = "e95d33f10646003169a7d30d971ddbbb0f19541c0c7db344aba67ff0ebbb72d3"

// contractBatch drafts the fixture batch: a Drupal 7 email lead, a Drupal 10 contact-form
// lead and an agency email lead, through draftLead with the examples config and Message-IDs
// of zero bytes, each frozen to mailbox 1 as newBatch does.
func contractBatch(t *testing.T) []leadstore.DraftRow {
	t.Helper()
	cfg := demoLeadsConfig(t)
	type lead struct {
		c  leadstore.Candidate
		ev leadstore.Evidence
	}
	leads := []lead{
		{
			leadstore.Candidate{LeadID: 3, OrgID: 2, Domain: "northwind.example", Country: "GB", Kind: leadstore.LeadUnsupported,
				Channel: leadstore.ChannelEmail, RouteKind: leadstore.RouteEmail, Route: "info@northwind.example"},
			leadstore.Evidence{LeadID: 3, ProbeID: i64(5), FinalHost: "northwind.example", Major: 7,
				Signals: []leadstore.EvidenceSignal{{Signal: leadstore.Signal{Signal: "Drupal.settings", FetchID: 11}, Fetch: demoFetch}}},
		},
		{
			leadstore.Candidate{LeadID: 4, OrgID: 3, Domain: "contoso.example", Country: "DE", Kind: leadstore.LeadD10,
				Channel: leadstore.ChannelForm, RouteKind: leadstore.RouteForm, Route: "https://contoso.example/contact"},
			leadstore.Evidence{ProbeID: i64(6), Major: 10, ExactVersion: "10.6.14",
				Signals: []leadstore.EvidenceSignal{{Signal: leadstore.Signal{Signal: "core-asset-v", Value: "10.6.14", FetchID: 12},
					Fetch: leadstore.EvidenceFetch{ID: 12, URL: "https://contoso.example/"}}}},
		},
		{
			leadstore.Candidate{LeadID: 5, OrgID: 4, Domain: "wingtip.example", OrgName: "Wingtip Digital", Country: "GB",
				Kind: leadstore.LeadAgencyPartner, Channel: leadstore.ChannelEmail, RouteKind: leadstore.RouteEmail, Route: "hello@wingtip.example"},
			leadstore.Evidence{AgencyFindingID: i64(9), FindingURL: "https://www.wingtip.example/services/drupal-upgrades"},
		},
	}
	var drafts []leadstore.DraftRow
	for _, l := range leads {
		ds, err := draftLead(l.c, l.ev, cfg, "sender.example")
		if err != nil {
			t.Fatal(err)
		}
		for i := range ds {
			ds[i].MailboxID, ds[i].FromName = 1, cfg.Sender.Name
		}
		drafts = append(drafts, ds...)
	}
	return drafts
}

// TestBatchHashContract: the fixture batch drafts and hashes exactly as before campaigns.
func TestBatchHashContract(t *testing.T) {
	drafts := contractBatch(t)
	sum, err := leadstore.ContentSHA256(drafts)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sum); got != contractBatchSHA {
		t.Errorf("fixture batch hash = %s, want %s (before campaigns): a draft changed", got, contractBatchSHA)
	}
	if len(drafts) != 3+2+3 {
		t.Errorf("%d drafts, want 8", len(drafts))
	}
}
