//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// placeLine is one Overture NDJSON line of a GB café: id, name, website ("" for none) and
// brand ("" for none).
func placeLine(t *testing.T, id, name, website, brand string) string {
	t.Helper()
	p := map[string]any{
		"id": id, "names": map[string]any{"primary": name}, "categories": map[string]any{"primary": "cafe"},
		"confidence": 0.9, "addresses": []any{map[string]any{"locality": "Exampleton", "region": "ENG", "country": "GB"}},
		"sources": []any{map[string]any{"dataset": "demo", "record_id": id}},
	}
	if website != "" {
		p["websites"] = []string{website}
	}
	if brand != "" {
		p["brand"] = map[string]any{"names": map[string]any{"primary": brand}}
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// placesFile writes lines to name in a temp dir and returns its path.
func placesFile(t *testing.T, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	return path
}

func importPlaces(t *testing.T, release, path string) string {
	t.Helper()
	return mustRun(t, "leads", "places", "import", "overture", "--campaign", "cafes-loyalty", "--release", release, path)
}

// TestPlacesChainAcrossFiles: a chain whose places are split across two export files of one
// release is one chain. The first file's three same-name cafés (under the campaign's limit
// of five) are stored as independent with orgs; the second file's three more are chains (the
// index counts the release's stored places too), with no org; and the café build then makes
// no lead for the first three orgs either, their latest place now a chain in its release. A
// later release that stores an independent café's place as a chain (here: under a brand)
// leaves that org without a lead too.
func TestPlacesChainAcrossFiles(t *testing.T) {
	dir := leadsDB(t)
	cafesDataDir(t, dir)
	mustRun(t, "leads", "campaigns", "register")
	const rel, rel2 = "2026-10-21.0", "2026-11-18.0"

	a := placesFile(t, "part-a.ndjson",
		placeLine(t, "chain-a1", "Bean Example Co", "https://a1.bean-example.example/", ""),
		placeLine(t, "chain-a2", "Bean Example Co", "https://a2-bean.example/", ""),
		placeLine(t, "chain-a3", "Bean Example Co", "https://a3-bean.example/", ""),
		placeLine(t, "solo-1", "Solo Example Cafe", "https://solo-cafe.example/", ""),
	)
	wantLines(t, "file A", importPlaces(t, rel, a),
		"  stored 4 places (0 already in this release) · 0 chains skipped (no lead) · 4 orgs and 4 sites made")
	if out := mustRun(t, "leads", "build", "--campaign", "cafes-loyalty"); strings.Contains(out, "a chain") {
		t.Errorf("build after file A finds a chain:\n%s", out)
	}

	b := placesFile(t, "part-b.ndjson",
		placeLine(t, "chain-b1", "Bean Example Co", "https://b1-bean.example/", ""),
		placeLine(t, "chain-b2", "Bean Example Co", "https://b2-bean.example/", ""),
		placeLine(t, "chain-b3", "Bean Example Co", "https://b3-bean.example/", ""),
	)
	wantLines(t, "file B", importPlaces(t, rel, b),
		"  stored 3 places (0 already in this release) · 3 chains skipped (no lead) · 0 orgs and 0 sites made")
	if n := dbCount(t, `SELECT count(*) FROM place WHERE source_id LIKE 'chain-b%' AND chain_rule = 'name' AND org_id IS NULL`); n != 3 {
		t.Errorf("%d of file B's places stored as name chains with no org, want 3", n)
	}
	build := mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	wantLines(t, "build after file B", build, "a chain (name) 3")
	if n := dbCount(t, `SELECT count(*) FROM lead l JOIN org o ON o.id = l.org_id WHERE o.domain LIKE '%bean%' AND l.state = 'new'`); n != 0 {
		t.Errorf("%d new leads on the chain's orgs, want 0", n)
	}

	// The next release lists the independent café under a brand: stored as a chain with no
	// org, it is the org's latest place, so the org gets no lead.
	c := placesFile(t, "next.ndjson", placeLine(t, "solo-1", "Solo Example Cafe", "https://solo-cafe.example/", "Solo Example Brand"))
	wantLines(t, "next release", importPlaces(t, rel2, c), "· 1 chains skipped (no lead)")
	wantLines(t, "build after the next release", mustRun(t, "leads", "build", "--campaign", "cafes-loyalty"), "a chain (brand) 1", "a chain (name) 3")
}

// TestPlacesSkipForgottenDomain: after `leads forget org`, a new release of the same café
// stores no place row for it (no phones or emails gathered again) and makes no org or site;
// the import says how many it left out; another café in the file is stored as usual.
func TestPlacesSkipForgottenDomain(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	cafesDataDir(t, dir)
	mustRun(t, "leads", "campaigns", "register")
	first := placesFile(t, "first.ndjson", placeLine(t, "gone-1", "Gone Example Cafe", "https://www.gone-cafe.example/menu", ""))
	importPlaces(t, "2026-10-21.0", first)
	var orgID int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT id FROM org WHERE domain = 'gone-cafe.example'`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADHOUND_MIGRATE_DATABASE_URL", storetest.MigrateDSN()) // forget runs as the owner
	mustRun(t, "leads", "forget", "org", fmt.Sprint(orgID), "--reason", "asked to be forgotten")

	next := placesFile(t, "next.ndjson",
		placeLine(t, "gone-1", "Gone Example Cafe", "https://gone-cafe.example/", ""),
		placeLine(t, "gone-2", "Gone Example Cafe Two", "https://shop.gone-cafe.example/", ""),
		placeLine(t, "kept-1", "Kept Example Cafe", "https://kept-cafe.example/", ""),
	)
	wantLines(t, "next release", importPlaces(t, "2026-11-18.0", next),
		"  stored 1 places (0 already in this release) · 0 chains skipped (no lead) · 1 orgs and 1 sites made",
		"  not stored: 2 places whose café is suppressed, by its website's domain or an earlier listing (forgotten or opted out)\n")
	if n := dbCount(t, `SELECT count(*) FROM place WHERE release = '2026-11-18.0' AND source_id LIKE 'gone-%'`); n != 0 {
		t.Errorf("%d places of the forgotten café stored in the new release, want 0", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM org WHERE domain LIKE '%gone-cafe.example' AND id <> `+fmt.Sprint(orgID)); n != 0 {
		t.Errorf("%d new orgs on the forgotten domain", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM place WHERE release = '2026-11-18.0' AND source_id = 'kept-1' AND org_id IS NOT NULL`); n != 1 {
		t.Errorf("the other café: %d stored, want 1", n)
	}
	var note string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT coalesce(note, '') FROM run WHERE command = 'leads-places-import' ORDER BY id DESC LIMIT 1`).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "2 suppressed") {
		t.Errorf("run note %q lacks the suppressed count", note)
	}
}
