package main

import (
	"context"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// The console's StepsBackend: the counts behind the morning step bar, read so that each one
// agrees with the page its step links to.

// MorningCounts is the step bar's counts. By hand, Calls and Replies come from LeadsToday
// itself, not the store's raw Today read, because Today moves a call that waits on an earlier
// step out of its due lists and the bar must say what Today says; LeadsToday also carries the
// set-up check, so leads not set up is ErrLeadsNotSetUp. The batch is the newest waiting one
// with the leads of every waiting batch summed, a batch of calls alone included, read as the
// Outreach page reads it. Review is the open leads not yet reviewed over the live campaigns.
func (b *backend) MorningCounts(ctx context.Context) (console.MorningCounts, error) {
	today, err := b.LeadsToday(ctx, "")
	if err != nil {
		return console.MorningCounts{}, err
	}
	c := console.MorningCounts{
		ByHand:  len(today.Manual),
		Calls:   len(today.CallsNow) + len(today.CallsDue) + len(today.CallsOverdue),
		Replies: len(today.Replies),
	}
	batches, err := waitingBatches(ctx, b.s.Pool())
	if err != nil {
		return console.MorningCounts{}, err
	}
	if len(batches) > 0 {
		c.BatchID = batches[0].ID // newest first
	}
	for _, w := range batches {
		c.BatchItems += w.Leads
	}
	if c.ToReview, err = b.lq().OpenNotReviewed(ctx, leadstore.DefaultAccountID); err != nil {
		return console.MorningCounts{}, err
	}
	return c, nil
}
