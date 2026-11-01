//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadview"
)

// proofT14 returns a good proof to change.
func proofT14(title string, sectors ...string) leadstore.Proof {
	return leadstore.Proof{Title: title, Line: "Harbour Lane Studio moved " + title + " in a week.", Sectors: sectors,
		Actor: leadstore.ActorOperator}
}

func proofT14IDs(ps []leadstore.Proof) []int64 {
	out := []int64{}
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

// TestProofLibrary: ProofsFor gives one account's active proofs of one sector, newest first and
// up to the limit; a retired proof is gone from ProofsFor and ListProofs; a lead with no sector
// gets none. A proof reads back as it was written, with no link as "".
func TestProofLibrary(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	add := func(p leadstore.Proof) int64 {
		t.Helper()
		id, err := f.q.AddProof(ctx, p)
		if err != nil {
			t.Fatalf("add %s: %v", p.Title, err)
		}
		return id
	}
	if got, err := f.q.ListProofs(ctx, 0); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("cold start: %v %v", got, err)
	}
	if got, err := f.q.ProofsFor(ctx, 0, leadview.SectorEducation, 2); err != nil || len(got) != 0 {
		t.Fatalf("cold start for a sector: %v %v", got, err)
	}

	withLink := proofT14("  First  ", leadview.SectorEducation)
	withLink.URL, withLink.Public = "  https://harbour-lane.example/work  ", true
	first := add(withLink)
	both := add(proofT14("Both", leadview.SectorEducation, leadview.SectorGovernment))
	gov := add(proofT14("Government", leadview.SectorGovernment))
	third := add(proofT14("Third", leadview.SectorEducation))
	other := ownerInt(t, f.owner, `INSERT INTO account (slug, name) VALUES ('t14-other', 'Other') RETURNING id`)
	foreign := proofT14("Foreign", leadview.SectorEducation)
	foreign.AccountID = other
	foreignID := add(foreign)

	check := func(what string, got []leadstore.Proof, err error, want ...int64) {
		t.Helper()
		if ids := proofT14IDs(got); err != nil || !slices.Equal(ids, want) {
			t.Errorf("%s: %v %v, want %v", what, ids, err, want)
		}
	}
	got, err := f.q.ProofsFor(ctx, 0, leadview.SectorEducation, 10)
	check("education", got, err, third, both, first)
	got, err = f.q.ProofsFor(ctx, leadstore.DefaultAccountID, leadview.SectorEducation, 2)
	check("education, two", got, err, third, both)
	got, err = f.q.ProofsFor(ctx, 0, leadview.SectorGovernment, 10)
	check("government", got, err, gov, both)
	got, err = f.q.ProofsFor(ctx, 0, leadview.SectorHealthcare, 10)
	check("a sector with no proof", got, err)
	got, err = f.q.ProofsFor(ctx, 0, "", 10)
	check("no sector", got, err)
	got, err = f.q.ProofsFor(ctx, other, leadview.SectorEducation, 10)
	check("the other account", got, err, foreignID)
	got, err = f.q.ListProofs(ctx, 0)
	check("list", got, err, third, gov, both, first)

	p := got[3]
	if p.Title != "First" || p.URL != "https://harbour-lane.example/work" || !p.Public || p.AccountID != leadstore.DefaultAccountID ||
		!slices.Equal(p.Sectors, []string{leadview.SectorEducation}) || p.Actor != leadstore.ActorOperator || p.At.IsZero() ||
		!strings.HasPrefix(p.Line, "Harbour Lane Studio") {
		t.Errorf("read back: %+v", p)
	}
	if got[0].URL != "" || got[0].Public {
		t.Errorf("no link: %+v", got[0])
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM proof WHERE url IS NULL`); n != 4 {
		t.Errorf("proofs with a NULL link = %d, want 4 (no link is NULL, never '')", n)
	}

	// Retired, once, by its own account only.
	if err := f.q.RetireProof(ctx, 0, both, "  out of date  "); err != nil {
		t.Fatal(err)
	}
	if err := f.q.RetireProof(ctx, 0, both, "second reason"); err != nil {
		t.Errorf("retired twice: %v, want no error", err)
	}
	var reason string
	if err := f.owner.QueryRow(ctx, `SELECT reason FROM proof_retire WHERE proof_id = $1`, both).Scan(&reason); err != nil || reason != "out of date" {
		t.Errorf("reason = %q, %v; want the first, trimmed", reason, err)
	}
	got, err = f.q.ProofsFor(ctx, 0, leadview.SectorEducation, 10)
	check("education after a retire", got, err, third, first)
	got, err = f.q.ListProofs(ctx, 0)
	check("list after a retire", got, err, third, gov, first)
	if err := f.q.RetireProof(ctx, 0, foreignID, ""); !errors.Is(err, leadstore.ErrProofNotFound) {
		t.Errorf("another account's proof: %v, want ErrProofNotFound", err)
	}
	if err := f.q.RetireProof(ctx, 0, 999999, ""); !errors.Is(err, leadstore.ErrProofNotFound) {
		t.Errorf("a missing proof: %v, want ErrProofNotFound", err)
	}
	if err := f.q.RetireProof(ctx, 0, third, strings.Repeat("r", leadstore.MaxProofReason+1)); !errors.Is(err, leadstore.ErrBadProof) {
		t.Errorf("a long reason: %v, want ErrBadProof", err)
	}
	if err := f.q.RetireProof(ctx, 0, third, strings.Repeat("é", leadstore.MaxProofReason)); err != nil {
		t.Errorf("a reason at the limit: %v", err)
	}
}

// TestProofRefusals: bad input is refused and writes nothing; every limit counts characters, so
// a title at the limit in two-byte letters goes in.
func TestProofRefusals(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	cases := []struct {
		what string
		mut  func(*leadstore.Proof)
		want error
	}{
		{"blank title", func(p *leadstore.Proof) { p.Title = "  " }, leadstore.ErrBadProof},
		{"long title", func(p *leadstore.Proof) { p.Title = strings.Repeat("t", leadstore.MaxProofTitle+1) }, leadstore.ErrBadProof},
		{"two-line title", func(p *leadstore.Proof) { p.Title = "a\nb" }, leadstore.ErrBadProof},
		{"blank line", func(p *leadstore.Proof) { p.Line = "" }, leadstore.ErrBadProof},
		{"long line", func(p *leadstore.Proof) { p.Line = strings.Repeat("l", leadstore.MaxProofLine+1) }, leadstore.ErrBadProof},
		{"two-line line", func(p *leadstore.Proof) { p.Line = "a\r\nb" }, leadstore.ErrBadProof},
		{"http link", func(p *leadstore.Proof) { p.URL = "http://harbour-lane.example/" }, leadstore.ErrBadProof},
		{"no host", func(p *leadstore.Proof) { p.URL = "https:///x" }, leadstore.ErrBadProof},
		{"a space in the link", func(p *leadstore.Proof) { p.URL = "https://harbour-lane.example/a b" }, leadstore.ErrBadProof},
		{"a login in the link", func(p *leadstore.Proof) { p.URL = "https://me@harbour-lane.example/" }, leadstore.ErrBadProof},
		{"long link", func(p *leadstore.Proof) {
			p.URL = "https://harbour-lane.example/" + strings.Repeat("u", leadstore.MaxProofURL)
		}, leadstore.ErrBadProof},
		{"no sectors", func(p *leadstore.Proof) { p.Sectors = nil }, leadstore.ErrBadProof},
		{"unknown sector", func(p *leadstore.Proof) { p.Sectors = []string{leadview.SectorMedia, "farming"} }, leadstore.ErrBadProof},
		{"blank sector", func(p *leadstore.Proof) { p.Sectors = []string{""} }, leadstore.ErrBadProof},
		{"unknown actor", func(p *leadstore.Proof) { p.Actor = leadstore.ActorGremy }, leadstore.ErrUnknownActor},
	}
	for _, tc := range cases {
		p := proofT14("Refused", leadview.SectorMedia)
		tc.mut(&p)
		if _, err := f.q.AddProof(ctx, p); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.what, err, tc.want)
		}
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM proof`); n != 0 {
		t.Errorf("%d proofs written by refused input", n)
	}
	atLimit := proofT14(strings.Repeat("é", leadstore.MaxProofTitle), leadview.SectorMedia)
	atLimit.Line = strings.Repeat("é", leadstore.MaxProofLine)
	atLimit.URL = "https://harbour-lane.example/" + strings.Repeat("é", leadstore.MaxProofURL-len("https://harbour-lane.example/"))
	if _, err := f.q.AddProof(ctx, atLimit); err != nil {
		t.Errorf("at every limit: %v", err)
	}
}

// TestProofSectorsMatchTheChecks: leadview.Sectors is the closed list the org_sector and proof
// checks hold, name for name, so the two can never drift apart.
func TestProofSectorsMatchTheChecks(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	want := []string{}
	for _, s := range leadview.Sectors {
		want = append(want, s.Name)
	}
	slices.Sort(want)
	re := regexp.MustCompile(`'([a-z]+)'`)
	for _, c := range []struct{ table, constraint string }{
		{"org_sector", "org_sector_sector_check"},
		{"proof", "proof_sectors_check"},
	} {
		var def string
		if err := f.owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conrelid = $1::regclass AND conname = $2`, c.table, c.constraint).Scan(&def); err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, m := range re.FindAllStringSubmatch(def, -1) {
			got = append(got, m[1])
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", c.constraint, got, want)
		}
	}
	if _, err := f.q.AddProof(ctx, proofT14("Every sector", want...)); err != nil {
		t.Errorf("a proof of every sector: %v", err)
	}
}

// sectorT14Org adds a lead on domain and returns its org.
func sectorT14Org(t *testing.T, f outreachFixture, domain string) int64 {
	t.Helper()
	lead := f.siteLead(t, domain, nil, 7, "")
	return ownerInt(t, f.owner, `SELECT org_id FROM lead WHERE id = $1`, lead)
}

// TestSectorOperatorThenRule: with no operator row SectorOf gives the rule's guess, a rule row
// included; the operator's latest choice beats it and any later rule row, and stays on its org.
func TestSectorOperatorThenRule(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	const domain = "harbour-lane.edu.localhost"
	org := sectorT14Org(t, f, domain)
	otherOrg := sectorT14Org(t, f, "other-harbour.localhost")
	sectorOf := func(id int64, domain, kind string) string {
		t.Helper()
		s, err := leadview.SectorOf(ctx, f.q, id, domain, kind)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s, err := f.q.OperatorSector(ctx, org); err != nil || s != "" {
		t.Errorf("cold start: %q %v", s, err)
	}
	if got := sectorOf(org, "harbour-lane.edu", leadstore.LeadUnsupported); got != leadview.SectorEducation {
		t.Errorf("no row, a rule: %q", got)
	}
	if got := sectorOf(org, domain, leadstore.LeadUnsupported); got != "" {
		t.Errorf("no row, no rule: %q", got)
	}
	if err := f.q.SetSector(ctx, org, leadview.SectorMedia, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	if s, err := f.q.OperatorSector(ctx, org); err != nil || s != "" {
		t.Errorf("a rule row is not the operator's: %q %v", s, err)
	}
	if got := sectorOf(org, "harbour-lane.edu", ""); got != leadview.SectorEducation {
		t.Errorf("a rule row changes nothing on read: %q", got)
	}
	for _, s := range []string{leadview.SectorRetail, leadview.SectorNonprofit} {
		if err := f.q.SetSector(ctx, org, s, leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.q.SetSector(ctx, org, leadview.SectorMedia, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	if got := sectorOf(org, "harbour-lane.edu", ""); got != leadview.SectorNonprofit {
		t.Errorf("the operator's latest wins: %q", got)
	}
	if got := sectorOf(otherOrg, "other-harbour.localhost", ""); got != "" {
		t.Errorf("another org: %q", got)
	}
	var source, actor string
	if err := f.owner.QueryRow(ctx, `SELECT source, actor FROM org_sector WHERE org_id = $1 AND sector = 'nonprofit'`, org).
		Scan(&source, &actor); err != nil || source != leadstore.SectorFromOperator || actor != leadstore.ActorOperator {
		t.Errorf("operator row: %s %s %v", source, actor, err)
	}
	if err := f.owner.QueryRow(ctx, `SELECT source, actor FROM org_sector WHERE org_id = $1 AND sector = 'media' LIMIT 1`, org).
		Scan(&source, &actor); err != nil || source != leadstore.SectorFromRule || actor != leadstore.ActorSystem {
		t.Errorf("rule row: %s %s %v", source, actor, err)
	}
}

// TestSectorRefusals: a name off the list, an unknown actor and a missing org write nothing.
func TestSectorRefusals(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	org := sectorT14Org(t, f, "refused-harbour.localhost")
	if err := f.q.SetSector(ctx, org, "farming", leadstore.ActorOperator); !errors.Is(err, leadstore.ErrBadSector) {
		t.Errorf("off the list: %v", err)
	}
	if err := f.q.SetSector(ctx, org, "", leadstore.ActorOperator); !errors.Is(err, leadstore.ErrBadSector) {
		t.Errorf("blank: %v", err)
	}
	if err := f.q.SetSector(ctx, org, leadview.SectorMedia, leadstore.ActorGremy); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("unknown actor: %v", err)
	}
	if err := f.q.SetSector(ctx, 999999, leadview.SectorMedia, leadstore.ActorOperator); !errors.Is(err, leadstore.ErrOrgNotFound) {
		t.Errorf("missing org: %v", err)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM org_sector`); n != 0 {
		t.Errorf("%d rows written by refused input", n)
	}
}
