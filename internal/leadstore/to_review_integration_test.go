//go:build integration

package leadstore_test

import (
	"context"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestOpenNotReviewed: the leads the step bar's Review counts are, summed over the live
// campaigns, exactly Total - Reviewed of CampaignProgress; a retired campaign's leads and the
// selftest lead count nowhere, and an empty database counts none.
func TestOpenNotReviewed(t *testing.T) {
	f := newTriageFixture(t)
	ctx := context.Background()
	if n, err := f.q.OpenNotReviewed(ctx, 0); err != nil || n != 0 {
		t.Fatalf("empty: %d, %v, want 0", n, err)
	}
	campaign := f.leadOf(t, f.cafe(t)).CampaignID
	f.cafe(t)
	skipped := f.cafe(t)
	f.skip(t, skipped, triageInput)
	called := f.cafe(t)
	f.call(t, called, leadstore.BandMaybe)
	undone := f.cafe(t)
	f.skip(t, undone, triageInput)
	if _, err := f.q.TriageUndoSkip(ctx, undone); err != nil {
		t.Fatal(err)
	}
	cleared := f.cafe(t)
	f.call(t, cleared, leadstore.BandNo)
	f.call(t, cleared, "")
	f.drupal(t)
	f.selftest(t)
	f.ownerRun(t, `UPDATE lead SET campaign_id = $1 WHERE kind = 'selftest'`, campaign)

	other := register(t, f.q, 0, "second-campaign", `{"slug": "second-campaign"}`)
	elsewhere := f.cafe(t)
	f.ownerRun(t, `UPDATE lead SET campaign_id = $1 WHERE id = $2`, other, elsewhere)
	retired := register(t, f.q, 0, "retired-campaign", `{"slug": "retired-campaign"}`)
	gone := f.cafe(t)
	f.ownerRun(t, `UPDATE lead SET campaign_id = $1 WHERE id = $2`, retired, gone)
	f.ownerRun(t, `UPDATE campaign SET retired_at = now() WHERE id = $1`, retired)

	want := 0
	for _, id := range []int64{campaign, other} {
		reviewed, total, err := f.q.CampaignProgress(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		want += total - reviewed
	}
	n, err := f.q.OpenNotReviewed(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Two new cafés, the undone skip, the cleared call and the Drupal lead, plus the second
	// campaign's café.
	if n != want || n != 6 {
		t.Errorf("open not reviewed %d, want %d from CampaignProgress and 6 by hand", n, want)
	}
}
