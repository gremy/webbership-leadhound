package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// The Leads list's triage from the console: an operator's skip, the undo of that skip, his
// Good, and his correction of a café lead's place type. Each outcome names the lead by
// labelName. A write that changes nothing is a refusal, never a success message, so a row is
// not taken off the page and offered an undo when the lead did not move.

// triageLead reads lead id as the triage needs it, whatever its state; a lead that is not
// there, or a selftest one, is refused.
func (b *backend) triageLead(ctx context.Context, id int64) (leadstore.RateLead, error) {
	l, err := b.lq().RateLeadByID(ctx, id)
	if errors.Is(err, leadstore.ErrLeadNotFound) {
		return leadstore.RateLead{}, refused("There is no lead %d.", id)
	}
	return l, err
}

// triageSkip skips lead id. A café lead's skip carries its facts, so a skip of one with no
// current call also saves the call "no"; a Drupal lead's carries none and never writes a call.
func (b *backend) triageSkip(ctx context.Context, setup leadsSetup, id int64) (console.LeadOutcome, error) {
	l, err := b.triageLead(ctx, id)
	if err != nil {
		return console.LeadOutcome{}, err
	}
	var input []byte
	isCafe := leadstore.TypeOf(l.Kind) == leadstore.TypeCafe
	if isCafe {
		if input, err = callInput(l, setup); err != nil {
			return console.LeadOutcome{}, err
		}
	}
	r, err := b.lq().TriageSkip(ctx, id, input)
	if err != nil {
		return console.LeadOutcome{}, err
	}
	name := labelName(l, id)
	switch {
	case !r.Moved:
		return console.LeadOutcome{}, refused("%s was not skipped; nothing changed. %s", name, notMovedWhy(r.State))
	case r.CallWritten:
		return console.LeadOutcome{Message: fmt.Sprintf("Skipped %s, and saved your call on it: no.", name)}, nil
	case isCafe:
		return console.LeadOutcome{Message: fmt.Sprintf("Skipped %s; your call on it stays as it was.", name)}, nil
	}
	return console.LeadOutcome{Message: fmt.Sprintf("Skipped %s; no call was saved.", name)}, nil
}

// notMovedWhy says why a lead in state cannot be skipped, naming where it stands.
func notMovedWhy(state string) string {
	switch state {
	case leadstore.LeadBatched:
		return "It is already in a batch, and it stays there unless you discard that batch or suppress the lead."
	case leadstore.LeadSkipped:
		return "It is already skipped."
	}
	return "Only a new lead can be skipped, and this one's state is " + leadStateWords(state) + "."
}

// triageUndo undoes the operator's own skip of lead id. The call is cleared only when the
// call the skip saved is still the lead's latest, so any other call of his stays.
func (b *backend) triageUndo(ctx context.Context, id int64) (console.LeadOutcome, error) {
	l, err := b.triageLead(ctx, id)
	if err != nil {
		return console.LeadOutcome{}, err
	}
	name := labelName(l, id)
	r, err := b.lq().TriageUndoSkip(ctx, id)
	if errors.Is(err, leadstore.ErrSystemSkip) {
		return console.LeadOutcome{}, refused("%s was skipped by the system, not by you, so there is no skip of yours to undo; nothing changed.", name)
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	switch {
	case !r.Moved:
		return console.LeadOutcome{}, refused("%s is not skipped, so there is no skip to undo; nothing changed. Where it stands: %s.", name, leadStateWords(r.State))
	case r.CallCleared:
		return console.LeadOutcome{Message: fmt.Sprintf("%s is back as a new lead, and the call of no that the skip saved is cleared.", name)}, nil
	}
	return console.LeadOutcome{Message: fmt.Sprintf("%s is back as a new lead; no call was cleared.", name)}, nil
}

// triageGood makes the operator's call on café lead id "likely", without moving the lead. A
// call of "strong" stays, and a call already "likely" writes nothing.
func (b *backend) triageGood(ctx context.Context, setup leadsSetup, id int64) (console.LeadOutcome, error) {
	l, err := b.triageLead(ctx, id)
	if err != nil {
		return console.LeadOutcome{}, err
	}
	input, err := callInput(l, setup)
	if err != nil {
		return console.LeadOutcome{}, err
	}
	name := labelName(l, id)
	r, err := b.lq().TriageGood(ctx, id, input)
	if errors.Is(err, leadstore.ErrNotCafeLead) {
		return console.LeadOutcome{}, refused("%s is not a café lead, so Good does not apply to it; your call was not changed.", name)
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	switch {
	case r.Band == leadstore.BandStrong:
		return console.LeadOutcome{Message: fmt.Sprintf("Your call on %s stays strong; nothing changed.", name)}, nil
	case !r.Written:
		return console.LeadOutcome{Message: fmt.Sprintf("Your call on %s is already %s; nothing changed.", name, r.Band)}, nil
	}
	return console.LeadOutcome{Message: fmt.Sprintf("Saved your call on %s: %s.", name, r.Band)}, nil
}

// correctPlaceType appends the operator's own answer on café lead id's place type. It writes
// no call and does not move the lead, and it outranks whatever Jev or the name said.
func (b *backend) correctPlaceType(ctx context.Context, id int64, placeType string) (console.LeadOutcome, error) {
	l, err := b.triageLead(ctx, id)
	if err != nil {
		return console.LeadOutcome{}, err
	}
	name := labelName(l, id)
	err = b.lq().SetPlaceType(ctx, id, placeType, leadstore.PlaceFromOperator, "", nil, leadstore.ActorGremy)
	if errors.Is(err, leadstore.ErrNotCafeLead) {
		return console.LeadOutcome{}, refused("%s is not a café lead, so it has no place type; nothing changed.", name)
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	words := "a coffee shop"
	if placeType == leadstore.PlaceRoastery {
		words = "a roastery or online store"
	}
	return console.LeadOutcome{Message: fmt.Sprintf("Marked %s as %s; its call and state did not change.", name, words)}, nil
}
