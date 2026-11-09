//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
)

// fictionalCard is a card block for the test's own copy of cafes-loyalty, in the shape the
// example's card takes.
const fictionalCard = "card:\n" +
	"  pitch: \"A fictional stamp card for a test café, kept on the phone.\"\n" +
	"  pain: \"Test regulars lose paper cards, so the test owner cannot count returns.\"\n" +
	"  criteria: [A test café, A listed test phone]\n"

// TestBatchPlanRefusesUnregisteredCard: a campaign file that gains a card: block after it
// was registered is refused for batches and for the place import with "changed since it
// was registered", and once leads campaigns register has run again both pass. The card
// never reaches an email, but it is part of the spec's hash, which is why the deploy docs
// warn that a card edit needs a register.
func TestBatchPlanRefusesUnregisteredCard(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	drawFromAll(t, dir)
	cafesDataDir(t, dir)

	// The test's copy starts with no card at all, and is registered that way.
	path := filepath.Join(dir, campaign.Dir, "cafes-loyalty", "campaign.yaml")
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	start, end := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "card:") {
			start = i
		}
		if start >= 0 && i > start && !strings.HasPrefix(l, " ") {
			end = i
			break
		}
	}
	if start < 0 || end < 0 {
		t.Fatalf("the example campaign has no card block to take out:\n%s", raw)
	}
	without := strings.Join(lines[:start], "") + strings.Join(lines[end:], "")
	if err := os.WriteFile(path, []byte(without), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	mustRun(t, "leads", "campaigns", "register")

	// The new card goes in, and nothing is registered after it.
	with := strings.Replace(without, "source:\n", fictionalCard+"source:\n", 1)
	if with == without {
		t.Fatal("the example campaign has no source: line to put the card before")
	}
	if err := os.WriteFile(path, []byte(with), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}

	lq := leadstoretest.Q(t)
	plan := func() error {
		t.Helper()
		c, err := activeCampaign(ctx, lq, "cafes-loyalty")
		if err != nil {
			t.Fatal(err)
		}
		b := leadsBackend(t)
		_, err = resolveBatchPlan(ctx, lq, newBatchSetup(mustSetup(t, b), b.loc), c.ID)
		return err
	}
	const want = "Campaign cafes-loyalty changed since it was registered"
	if err := plan(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("the batch plan with an unregistered card: %v, want %q", err, want)
	}
	importArgs := []string{"leads", "places", "import", "overture", "--campaign", "cafes-loyalty", "--release", placesRelease,
		filepath.Join(dir, "places.demo.ndjson")}
	if code, out, errOut := leadsRun(t, importArgs...); code == 0 || !strings.Contains(errOut, want) {
		t.Fatalf("the place import with an unregistered card exited %d, want a refusal naming %q:\n%s%s", code, want, out, errOut)
	}

	if out := mustRun(t, "leads", "campaigns", "register"); !strings.Contains(out, "cafes-loyalty: new version (") {
		t.Fatalf("register after the card did not take a new version:\n%s", out)
	}
	if err := plan(); err != nil {
		t.Fatalf("the batch plan after register: %v", err)
	}
	mustRun(t, importArgs...)
}
