//go:build integration

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/console"
)

// TestDraftsConsolePage: the Drafts tab's backend takes "" as the first campaign with the
// filter's campaigns, refuses an unknown slug in words, and says nothing for a campaign with
// no samples.
func TestDraftsConsolePage(t *testing.T) {
	draftsWorld(t)
	ctx := context.Background()
	b := testBackend(t)
	page, err := b.DraftSamples(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Campaign != campaign.LegacySlug || len(page.Campaigns) == 0 || page.Campaigns[0].Value != campaign.LegacySlug ||
		len(page.Samples) != 0 || !strings.HasPrefix(page.Gate.Summary, "Claude drafts: not trusted yet") {
		t.Fatalf("page %+v", page)
	}
	if _, err := b.DraftSamples(ctx, "nope", 0); !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), `No campaign is called "nope".`) {
		t.Fatalf("unknown campaign: %v", err)
	}
	if err := b.ReviewDraft(ctx, 999999, console.VerdictApprove, nil, ""); !errors.Is(err, console.ErrUserFixable) ||
		!strings.Contains(err.Error(), "There is no draft 999999.") {
		t.Fatalf("unknown draft: %v", err)
	}
}

// TestDraftsConsoleBatchClaims: the batch page's backend gives every message Claude wrote its
// claims (each paragraph's text and cited facts, the call card's talking points) and leaves a
// template message without; a console batch of a writer: claude campaign says in its line
// how many leads were drafted from templates and why.
func TestDraftsConsoleBatchClaims(t *testing.T) {
	dir, _ := claudeWorld(t)
	ctx := context.Background()
	useCaller(t, &draftFakeCounting{})
	mustRun(t, "leads", "batch", "new")
	b := leadsBackend(t)
	lb, err := b.LeadBatch(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	claudeIDs := map[int64]bool{}
	for _, m := range batchMessages(t, 1) {
		if _, ok := isClaude(m); ok {
			claudeIDs[m.id] = true
		}
	}
	var claude, plain, call int
	for _, it := range lb.Items {
		for _, m := range it.Messages {
			if !claudeIDs[m.ID] {
				if m.Claude != nil {
					t.Fatalf("template message %d has claims", m.ID)
				}
				plain++
				continue
			}
			claude++
			if m.Claude == nil || len(m.Claude.Paragraphs) == 0 {
				t.Fatalf("Claude message %d has no claims", m.ID)
			}
			if m.Channel == "phone" {
				call++
			}
			for _, p := range m.Claude.Paragraphs {
				if p.Text == "" {
					t.Fatalf("message %d: a paragraph without its text: %+v", m.ID, m.Claude)
				}
				for _, c := range p.Cites {
					if c.Missing || c.Value == "" || c.Table == "" {
						t.Fatalf("message %d: cite %+v", m.ID, c)
					}
				}
				if !strings.Contains(m.Body, p.Text) {
					t.Fatalf("message %d: paragraph %q is not in the body", m.ID, p.Text)
				}
			}
		}
	}
	if claude == 0 || call != 1 {
		t.Fatalf("%d Claude messages (%d calls), %d template", claude, call, plain)
	}

	mustRun(t, "leads", "batch", "discard", "1")
	goLive(t)
	setCaps(t, dir, "2026-10-01", "[{from_day: 0, per_day: 10}]")
	defer t.Setenv("LEADHOUND_OUTREACH", "")
	out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadNewBatch})
	if err != nil || out.BatchID == 0 || !strings.Contains(out.Message, "drafted from templates: "+fbOff) {
		t.Fatalf("console new batch: %+v, %v", out, err)
	}
}
