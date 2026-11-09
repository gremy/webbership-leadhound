//go:build integration

package leadstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// businessKindSHA is a fixed 32-byte input hash for a test row.
func businessKindSHA(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// allBusinessKinds is the closed list of migration 00034, written out so a change to its CHECK
// fails here.
var allBusinessKinds = []string{
	"specialty_coffee", "neighbourhood_cafe", "bakery_cafe", "restaurant_cafe", "bar_lounge", "venue_cafe", "roastery_shop",
	"education", "government", "healthcare", "nonprofit", "company", "media", "agency", "personal",
	"unclear",
}

// TestMigrationBusinessKindDownUp: the checks and the index exist; the app may only append; down
// drops the table with its derived rows and leaves the lead alone, and up brings it back empty
// with the same grants.
func TestMigrationBusinessKindDownUp(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	owner := f.owner
	for _, c := range []string{"lead_business_kind_kind_check", "lead_business_kind_question_check", "lead_business_kind_confidence_check"} {
		if n := ownerInt(t, owner, `SELECT count(*) FROM pg_constraint WHERE conname = $1`, c); n != 1 {
			t.Errorf("constraint %s: %d, want 1", c, n)
		}
	}
	if def := ownerString(t, owner, `SELECT indexdef FROM pg_indexes WHERE indexname = 'lead_business_kind_lead_idx'`); def !=
		"CREATE INDEX lead_business_kind_lead_idx ON public.lead_business_kind USING btree (lead_id, id DESC)" {
		t.Errorf("index = %s", def)
	}
	if !appHas(t, owner, "lead_business_kind", "INSERT") {
		t.Error("lead_business_kind: the app wants INSERT")
	}
	for _, priv := range []string{"UPDATE", "DELETE", "TRUNCATE"} {
		if appHas(t, owner, "lead_business_kind", priv) {
			t.Errorf("lead_business_kind: the app holds %s", priv)
		}
	}
	lead := f.siteLead(t, "kind-down-up.example", nil, 7, "")
	if err := f.q.SetBusinessKind(ctx, lead, "company", "business-kind@1", businessKindSHA("a"), 0.9); err != nil {
		t.Fatal(err)
	}
	app := storetest.AppConn(t)
	if _, err := app.Exec(ctx, `UPDATE lead_business_kind SET kind = 'unclear'`); sqlState(err) != "42501" {
		t.Errorf("app rewrites a kind: %v, want 42501", err)
	}
	if _, err := app.Exec(ctx, `DELETE FROM lead_business_kind`); sqlState(err) != "42501" {
		t.Errorf("app deletes a kind: %v, want 42501", err)
	}

	p := leadsProvider(t)
	t.Cleanup(func() { migrateAll(t) })
	if _, err := p.DownTo(ctx, businessKindVersion-1); err != nil {
		t.Fatalf("down to %d: %v", businessKindVersion-1, err)
	}
	var reg *string
	if err := owner.QueryRow(ctx, `SELECT to_regclass('public.lead_business_kind')::text`).Scan(&reg); err != nil || reg != nil {
		t.Errorf("lead_business_kind after down: %v %v, want gone", reg, err)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM lead WHERE id = $1`, lead); n != 1 {
		t.Errorf("the lead after down: %d, want 1", n)
	}
	migrateAll(t)
	assertLeadPrivileges(t, owner, "after 00034 down and up")
	if n := ownerInt(t, owner, `SELECT count(*) FROM lead_business_kind`); n != 0 {
		t.Errorf("rows after down and up: %d, want 0", n)
	}
}

// TestBusinessKindChecks: the CHECKs take every kind of the closed list and the confidence edges,
// and refuse anything else.
func TestBusinessKindChecks(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "kind-checks.example", nil, 7, "")
	sha := businessKindSHA("checks")
	for _, k := range allBusinessKinds {
		if err := f.q.SetBusinessKind(ctx, lead, k, "business-kind@1", sha, 0.5); err != nil {
			t.Errorf("kind %s: %v", k, err)
		}
	}
	for _, c := range []float64{0, 1} {
		if err := f.q.SetBusinessKind(ctx, lead, "unclear", "business-kind@2", sha, c); err != nil {
			t.Errorf("confidence %v: %v", c, err)
		}
	}
	for name, c := range map[string]struct {
		kind, question string
		confidence     float64
	}{
		"a kind off the list": {"coffee", "business-kind@1", 0.5},
		"an empty kind":       {"", "business-kind@1", 0.5},
		"a question no name":  {"company", "place-type@1", 0.5},
		"a question version":  {"company", "business-kind@x", 0.5},
		"confidence above":    {"company", "business-kind@1", 1.5},
		"confidence below":    {"company", "business-kind@1", -0.1},
	} {
		if err := f.q.SetBusinessKind(ctx, lead, c.kind, c.question, sha, c.confidence); sqlState(err) != "23514" {
			t.Errorf("%s: %v, want 23514", name, err)
		}
	}
}

// TestSetBusinessKind: each answer appends a row whatever the lead's state, holding what it was
// given; a missing lead, a selftest lead and a hash of the wrong length write nothing.
func TestSetBusinessKind(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.cafeLead(t, "kind-set.example", nil, "")
	if err := f.q.SetBusinessKind(ctx, lead, "bakery_cafe", "business-kind@1", businessKindSHA("one"), 0.75); err != nil {
		t.Fatal(err)
	}
	f.ownerRun(t, `UPDATE lead SET state = 'skipped' WHERE id = $1`, lead)
	if err := f.q.SetBusinessKind(ctx, lead, "unclear", "business-kind@1", businessKindSHA("two"), 0.25); err != nil {
		t.Fatal(err)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_business_kind WHERE lead_id = $1`, lead); n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_business_kind WHERE lead_id = $1 AND kind = 'bakery_cafe'
		AND question = 'business-kind@1' AND confidence = 0.75 AND input_sha256 = $2`, lead, businessKindSHA("one")); n != 1 {
		t.Errorf("the first row does not hold what it was given (%d)", n)
	}

	before := ownerInt(t, f.owner, `SELECT count(*) FROM lead_business_kind`)
	if err := f.q.SetBusinessKind(ctx, 999_999, "company", "business-kind@1", businessKindSHA("x"), 0.9); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("a missing lead: %v, want ErrLeadNotFound", err)
	}
	for _, sha := range [][]byte{nil, {1, 2, 3}} {
		if err := f.q.SetBusinessKind(ctx, lead, "company", "business-kind@1", sha, 0.9); !errors.Is(err, leadstore.ErrBadBusinessKind) {
			t.Errorf("a %d-byte hash: %v, want ErrBadBusinessKind", len(sha), err)
		}
	}
	test := f.siteLead(t, "kind-selftest.example", nil, 7, "")
	f.ownerRun(t, `UPDATE org SET kind = 'selftest' WHERE id = (SELECT org_id FROM lead WHERE id = $1)`, test)
	if err := f.q.SetBusinessKind(ctx, test, "company", "business-kind@1", businessKindSHA("x"), 0.9); !errors.Is(err, leadstore.ErrBadBusinessKind) {
		t.Errorf("a selftest lead: %v, want ErrBadBusinessKind", err)
	}
	if after := ownerInt(t, f.owner, `SELECT count(*) FROM lead_business_kind`); after != before {
		t.Errorf("refused answers wrote %d rows", after-before)
	}
}

// TestBusinessKindLeads: every open lead of either rating type comes back in id order with its
// latest hash, nil when never asked; a lead that is not open, a selftest lead and another
// campaign's lead do not, and 0 reads every campaign's.
func TestBusinessKindLeads(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	never := f.cafeLead(t, "kind-never.example", nil, "")
	asked := f.siteLead(t, "kind-asked.example", nil, 7, "")
	skipped := f.cafeLead(t, "kind-skipped.example", nil, "")
	test := f.siteLead(t, "kind-test.example", nil, 7, "")
	other := f.cafeLead(t, "kind-other.example", nil, "")
	f.ownerRun(t, `UPDATE lead SET state = 'skipped' WHERE id = $1`, skipped)
	f.ownerRun(t, `UPDATE org SET kind = 'selftest' WHERE id = (SELECT org_id FROM lead WHERE id = $1)`, test)
	f.ownerRun(t, `INSERT INTO campaign (id, account_id, slug, name) VALUES (2, 1, 'other', 'Other')`)
	f.ownerRun(t, `UPDATE lead SET campaign_id = 2 WHERE id = $1`, other)
	for _, sha := range []string{"old", "new"} {
		if err := f.q.SetBusinessKind(ctx, asked, "company", "business-kind@1", businessKindSHA(sha), 0.9); err != nil {
			t.Fatal(err)
		}
	}

	check := func(campaign int64, want ...int64) {
		t.Helper()
		got, err := f.q.BusinessKindLeads(ctx, campaign)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("campaign %d: %+v, want leads %v", campaign, got, want)
		}
		for i, l := range got {
			if l.LeadID != want[i] {
				t.Errorf("campaign %d row %d: lead %d, want %d", campaign, i, l.LeadID, want[i])
			}
			switch l.LeadID {
			case asked:
				if !bytes.Equal(l.LatestSHA, businessKindSHA("new")) {
					t.Errorf("the asked lead's hash is not the latest row's: %x", l.LatestSHA)
				}
			default:
				if l.LatestSHA != nil {
					t.Errorf("lead %d was never asked but has hash %x", l.LeadID, l.LatestSHA)
				}
			}
		}
	}
	check(1, never, asked)
	check(2, other)
	check(0, never, asked, other)
}

// TestListExtrasBusinessKind: the latest row's kind reads on the lead, an unclear answer included,
// and a lead with no row reads empty.
func TestListExtrasBusinessKind(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	changed := f.cafeLead(t, "kind-changed.example", nil, "")
	unclear := f.siteLead(t, "kind-unclear.example", nil, 7, "")
	none := f.cafeLead(t, "kind-none.example", nil, "")
	for _, a := range []struct {
		lead int64
		kind string
	}{{changed, "bakery_cafe"}, {changed, "specialty_coffee"}, {unclear, "company"}, {unclear, "unclear"}} {
		if err := f.q.SetBusinessKind(ctx, a.lead, a.kind, "business-kind@1", businessKindSHA(a.kind), 0.9); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.q.ListExtras(ctx, []int64{changed, unclear, none})
	if err != nil {
		t.Fatal(err)
	}
	for lead, want := range map[int64]string{changed: "specialty_coffee", unclear: "unclear", none: ""} {
		if got[lead].BusinessKind != want {
			t.Errorf("lead %d: kind %q, want %q", lead, got[lead].BusinessKind, want)
		}
	}
}
