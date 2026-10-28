package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// The console backend of CP4's Drafts tab (/leads/drafts), the gate line on the Campaigns
// tab and the claims of a Claude-written message on the batch page. It words the store's
// rows into the console's own types; nothing here calls Claude. These reads need no leads
// settings: the campaigns, samples and reviews all live in the store.

// DraftSamples is campaign slug's sampled drafts ("" the account's first campaign), newest
// first, at most limit (<= 0: all), with the campaign's gate and the filter's campaigns.
func (b *backend) DraftSamples(ctx context.Context, slug string, limit int) (console.DraftsPage, error) {
	lq := b.lq()
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return console.DraftsPage{}, err
	}
	var page console.DraftsPage
	var c *leadstore.Campaign
	for i := range cs {
		page.Campaigns = append(page.Campaigns, console.FilterOption{Value: cs[i].Slug, Label: cmp.Or(cs[i].Name, cs[i].Slug)})
		if c == nil && (slug == "" || cs[i].Slug == slug) {
			c = &cs[i]
		}
	}
	if c == nil {
		if slug == "" {
			return page, nil // no campaign yet
		}
		return console.DraftsPage{}, refused("No campaign is called %q.", slug)
	}
	st, err := draftGate(ctx, lq, c.ID)
	if err != nil {
		return console.DraftsPage{}, err
	}
	page.Campaign, page.Gate = c.Slug, consoleGate(st)
	gens, err := lq.SampleGenerations(ctx, c.ID, limit)
	if err != nil {
		return console.DraftsPage{}, err
	}
	domains := map[int64]string{}
	for _, g := range gens {
		d, ok := domains[g.LeadID]
		if !ok {
			lc, err := leadCandidate(ctx, lq, g.LeadID)
			if err != nil {
				return console.DraftsPage{}, err
			}
			d, domains[g.LeadID] = lc.Domain, lc.Domain
		}
		page.Samples = append(page.Samples, draftSample(g, d))
	}
	return page, nil
}

// consoleGate is a gate state as the console shows it.
func consoleGate(st outreach.GateState) console.DraftGate {
	return console.DraftGate{Trusted: st.Trusted, Summary: st.Summary()}
}

// draftSample words one sampled generation.
func draftSample(g leadstore.SampledGeneration, domain string) console.DraftSample {
	s := console.DraftSample{GenerationID: g.ID, LeadID: g.LeadID, Domain: domain, At: g.At, Attempt: g.Attempt,
		Fake: g.Model == claude.FakeModel, CheckOK: g.CheckOK, Reviewable: g.CheckOK}
	for _, f := range g.Failures {
		s.Failures = append(s.Failures, console.DraftFailure{Where: console.FailureWhere(f.Step, f.Para), Msg: f.Msg, Code: f.Code})
	}
	for _, f := range g.Facts.Facts {
		s.Facts = append(s.Facts, console.DraftFact{ID: f.ID, Label: f.Label, Value: f.Value, Group: string(f.Group),
			Table: f.Source.Table, RowID: f.Source.ID, ContextOnly: f.EchoGuard})
	}
	if g.Review != nil {
		s.Review = &console.DraftReview{Verdict: g.Review.Verdict, Reasons: g.Review.Reasons, Note: g.Review.Note, At: g.Review.At}
	}
	if g.Output == nil {
		return s
	}
	d, err := outreach.DecodeWrittenDraft(g.Output)
	if err != nil {
		return s
	}
	byID := map[string]outreach.Fact{}
	for _, f := range g.Facts.Facts {
		byID[f.ID] = f
	}
	cites := func(ids []string) []console.DraftCite {
		out := make([]console.DraftCite, 0, len(ids))
		for _, id := range ids {
			f, ok := byID[id]
			out = append(out, console.DraftCite{ID: id, Label: f.Label, Value: f.Value, Table: f.Source.Table, RowID: f.Source.ID,
				Missing: !ok})
		}
		return out
	}
	for _, st := range d.Steps {
		ds := console.DraftStep{Step: st.Step, Subject: st.Subject}
		for _, p := range st.Paragraphs {
			ds.Paragraphs = append(ds.Paragraphs, console.DraftParagraph{Text: p.Text, PainID: p.PainID, Cites: cites(p.Cites)})
		}
		s.Steps = append(s.Steps, ds)
	}
	for _, tp := range d.TalkingPoints {
		s.TalkingPoints = append(s.TalkingPoints, console.DraftParagraph{Text: tp.Text, Cites: cites(tp.Cites)})
	}
	return s
}

// ReviewDraft records the operator's verdict on generation generationID (approve, or reject
// with at least one of leadstore.ReviewReasons; "other" needs a note). The latest review of a
// generation wins. A draft the check refused, an unknown generation or bad input is a
// refusal in plain words.
func (b *backend) ReviewDraft(ctx context.Context, generationID int64, verdict string, reasons []string, note string) error {
	_, err := b.lq().RecordReview(ctx, generationID, verdict, reasons, note, leadstore.ActorOperator)
	switch {
	case errors.Is(err, leadstore.ErrReviewRefused):
		return refused("Draft %d was refused by the check, so it cannot be reviewed.", generationID)
	case errors.Is(err, leadstore.ErrGenerationNotFound):
		return refused("There is no draft %d.", generationID)
	case errors.Is(err, leadstore.ErrBadReview):
		return refused("Choose approve, or reject with at least one reason; \"other\" needs a note of at most %d characters.", leadstore.MaxReviewNote)
	}
	return err
}

// DraftGates is each campaign's draft gate, by slug, for the Campaigns tab
// ("Claude drafts: not trusted yet: 14 of 20 reviewed, 86% approved").
func (b *backend) DraftGates(ctx context.Context) (map[string]console.DraftGate, error) {
	lq := b.lq()
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]console.DraftGate, len(cs))
	for _, c := range cs {
		st, err := draftGate(ctx, lq, c.ID)
		if err != nil {
			return nil, err
		}
		out[c.Slug] = consoleGate(st)
	}
	return out, nil
}

// generationReader reads one draft generation; leadstore.Queries is one.
type generationReader interface {
	GenerationByID(ctx context.Context, id int64) (leadstore.Generation, error)
}

// claudeWriting is the batch page's claims of a message: for a message Claude wrote (slots
// with writer claude), each paragraph of its step (each talking point, for a call row) with
// its cites as stored on the message, the text taken from the generation it came from. nil
// for a template message or slots that cannot be read. gens caches the generations read for
// one page; a generation that cannot be read leaves the paragraphs without text.
func claudeWriting(ctx context.Context, q generationReader, slots json.RawMessage, step int, call bool,
	gens map[int64]*outreach.WrittenDraft) (*console.ClaudeWriting, error) {
	var s claudeSlots
	if len(slots) == 0 || json.Unmarshal(slots, &s) != nil || s.Writer != campaign.WriterClaude {
		return nil, nil
	}
	d, ok := gens[s.GenerationID]
	if !ok {
		g, err := q.GenerationByID(ctx, s.GenerationID)
		switch {
		case errors.Is(err, leadstore.ErrGenerationNotFound):
		case err != nil:
			return nil, err
		case g.Output != nil:
			if wd, err := outreach.DecodeWrittenDraft(g.Output); err == nil {
				d = &wd
			}
		}
		gens[s.GenerationID] = d
	}
	var texts []string
	if d != nil {
		if call {
			for _, tp := range d.TalkingPoints {
				texts = append(texts, tp.Text)
			}
		} else {
			for _, st := range d.Steps {
				if st.Step == step {
					for _, p := range st.Paragraphs {
						texts = append(texts, p.Text)
					}
				}
			}
		}
	}
	w := &console.ClaudeWriting{GenerationID: s.GenerationID}
	for i, p := range s.Paragraphs {
		cp := console.DraftParagraph{PainID: p.PainID}
		if i < len(texts) {
			cp.Text = texts[i]
		}
		for _, id := range p.Cites {
			f, ok := s.Cites[id]
			cp.Cites = append(cp.Cites, console.DraftCite{ID: id, Label: f.Label, Value: f.Value, Table: f.Table, RowID: f.ID, Missing: !ok})
		}
		w.Paragraphs = append(w.Paragraphs, cp)
	}
	return w, nil
}
