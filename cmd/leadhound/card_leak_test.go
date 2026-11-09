package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// The card's words are for the console's Campaigns page only. Each field carries its own
// sentinel, so a leak names the field it came from.
var cardSentinels = []string{"zqpitchsentinel", "zqpainsentinel", "zqcriterionsentinel"}

func sentinelCard() *campaign.Card {
	return &campaign.Card{
		Pitch:    "A pitch " + cardSentinels[0] + ".",
		Pain:     "A pain " + cardSentinels[1] + ".",
		Criteria: []string{"A criterion " + cardSentinels[2]},
	}
}

func assertNoCardWords(t *testing.T, where, text string) {
	t.Helper()
	for _, s := range cardSentinels {
		if strings.Contains(text, s) {
			t.Errorf("%s carries the card's words (%s)", where, s)
		}
	}
}

// TestCardNeverReachesAnEmailOrAModel: with a card on the campaign, nothing built for an
// email or a model holds its words: the template rows, the slots, the fact sheet, the Claude
// system prompt and user turn, the written draft and the rating state.
func TestCardNeverReachesAnEmailOrAModel(t *testing.T) {
	for _, phone := range []bool{false, true} {
		w := newDraftWorld(t, "unsupported", 7)
		spec := w.plan.spec
		spec.Card = sentinelCard()
		if err := spec.Check(campaign.CheckOptions{KnownKinds: campaign.KnownKinds()}); err != nil {
			t.Fatalf("the sentinel card fails the spec's checks: %v", err)
		}
		plan, err := filePlan(examplesDir, spec, w.cfg)
		if err != nil {
			t.Fatal(err)
		}
		w.plan = plan
		label := fmt.Sprintf("phone %v: ", phone)

		// The template path.
		rows, err := draftSequence(w.plan, w.c, w.rs.ev, w.cfg, nil, "sender.example", bytes.NewReader(bytes.Repeat([]byte{7}, 4096)))
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			t.Fatal("no template rows drafted")
		}
		assertNoCardWords(t, label+"the template rows", fmt.Sprintf("%+v", rows))
		slots, err := leadSlots(w.c, w.rs.ev, w.cfg, spec)
		if err != nil {
			t.Fatal(err)
		}
		sj, err := slotsJSON(slots)
		if err != nil {
			t.Fatal(err)
		}
		assertNoCardWords(t, label+"the slots", string(sj))

		// The Claude path: the fact sheet, both prompts and the written draft.
		in := w.input(t, phone)
		assertNoCardWords(t, label+"the fact sheet", string(in.Sheet.Canonical()))
		assertNoCardWords(t, label+"the system prompt", outreach.SystemPrompt(in))
		assertNoCardWords(t, label+"the user turn", outreach.UserTurn(in))
		assertNoCardWords(t, label+"the write input", fmt.Sprintf("%+v", in))
		res, err := (&outreach.Writer{Caller: draftFake{}}).WriteLead(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if res.Written == nil {
			t.Fatalf("no written draft: %q", res.Fallback)
		}
		out, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		assertNoCardWords(t, label+"the written draft", string(out))
	}

	// The rating state is built from the lead's own stored facts (rateInput takes no spec),
	// so a campaign's card cannot reach the rating model.
	rank := 3
	l := leadstore.RateLead{Kind: "unsupported", Major: 7, Country: "GB", SiteName: "Northfield Museum Trust",
		Description: "A fictional museum.", HasEmail: true, ListRank: &rank}
	assertNoCardWords(t, "the rating state", leadrate.State(rateInput(l, newDraftWorld(t, "unsupported", 7).cfg)))
}
