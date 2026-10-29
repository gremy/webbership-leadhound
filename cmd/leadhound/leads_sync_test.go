package main

import (
	"net/mail"
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
)

// A fictional campaign's own Romanian opt-out line, in place of leads.yaml's.
const campaignOptOutLine = "Nu e momentul? Scrieți „dezabonare” și nu mai revin."

// TestOptOutLinesOfCampaignLines (FO3): a registered campaign's country opt_out_line is one
// of our lines, listed once; a campaign whose spec does not decode, or has no version, is
// skipped and the rest still count.
func TestOptOutLinesOfCampaignLines(t *testing.T) {
	var cfg leadconfig.Config
	cfg.Sender.OptOutLines = map[string]string{"ro": "Nu vă interesează? Răspundeți cu „dezabonare” și nu vă mai scriu."}
	spec := campaign.Spec{Slug: "test-ro", Countries: map[string]campaign.Country{
		"RO": {Email: true, Lang: "ro", OptOutLine: campaignOptOutLine},
		"MD": {Email: true, Lang: "ro", OptOutLine: campaignOptOutLine},
		"DE": {Email: true, Lang: "de"},
	}}
	b, _, err := campaign.Canonical(spec)
	if err != nil {
		t.Fatal(err)
	}
	cs := []leadstore.Campaign{
		{Slug: "broken", Spec: []byte("{not json")},
		{Slug: "unregistered"},
		{Slug: "test-ro", Spec: b},
	}
	specs := registeredSpecs(cs)
	if len(specs) != 1 || specs[0].Slug != "test-ro" {
		t.Fatalf("registeredSpecs = %+v, want test-ro alone", specs)
	}
	lines := optOutLinesOf(cfg, specs...)
	want := []string{cfg.Sender.OptOutLines["ro"], campaignOptOutLine, mailbox.SelftestLine, mailbox.OldSelftestLine}
	if !slices.Equal(lines, want) {
		t.Fatalf("optOutLinesOf = %q, want %q", lines, want)
	}
	if got := optOutLinesOf(cfg); slices.Contains(got, campaignOptOutLine) {
		t.Fatalf("optOutLinesOf without specs = %q", got)
	}
}

// TestCampaignOptOutLineQuotedIsAReply (FO3): a reply that quotes the campaign's opt-out
// line unprefixed is a reply once that line is ours (and an opt-out without it), while a
// reply whose own words say "dezabonare" is still an opt-out.
func TestCampaignOptOutLineQuotedIsAReply(t *testing.T) {
	var cfg leadconfig.Config
	spec := campaign.Spec{Slug: "test-ro", Countries: map[string]campaign.Country{
		"RO": {Email: true, Lang: "ro", OptOutLine: campaignOptOutLine},
	}}
	lines := optOutLinesOf(cfg, spec)
	h := mail.Header{"Subject": {"Re: salut"}}
	quoted := []byte("Mulțumesc, revin săptămâna viitoare.\n\nBună ziua,\n" + campaignOptOutLine + "\n")
	if got := mailbox.OptOutIn(h, quoted, lines); got == mailbox.OptOutText || got == mailbox.OptOutSubject {
		t.Fatalf("a reply quoting the campaign's opt-out line read as an opt-out (%q)", got)
	}
	if got := mailbox.OptOutIn(h, quoted, optOutLinesOf(cfg)); got != mailbox.OptOutText {
		t.Fatalf("without the campaign line, OptOutIn = %q, want %q (the test would prove nothing)", got, mailbox.OptOutText)
	}
	optOut := []byte("dezabonare\n\nBună ziua,\n" + campaignOptOutLine + "\n")
	if got := mailbox.OptOutIn(h, optOut, lines); got != mailbox.OptOutText {
		t.Fatalf("a reply saying dezabonare: OptOutIn = %q, want %q", got, mailbox.OptOutText)
	}
}
