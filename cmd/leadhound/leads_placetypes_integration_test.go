//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// placeTypesImport imports two fictive cafés with no website, one with a roastery word in its
// name and one without, then builds their leads. It returns the data dir.
func placeTypesImport(t *testing.T) string {
	t.Helper()
	dir := leadsDB(t)
	cafesDataDir(t, dir)
	var b strings.Builder
	for i, name := range []string{"Fictive Hill Roasters", "Fictive Corner Cafe"} {
		fmt.Fprintf(&b, `{"id": "fictive-place-%d", "names": {"primary": %q}, "categories": {"primary": "cafe"}, "confidence": 0.9, `+
			`"addresses": [{"freeform": "%d Example Street", "locality": "Exampleton", "postcode": "EX1 1AA", "region": "ENG", "country": "GB"}], `+
			`"sources": [{"dataset": "demo", "record_id": "f-%d"}], "geometry": {"type": "Point", "coordinates": [-1.5, 52.4]}}`+"\n", i, name, i+1, i)
	}
	file := filepath.Join(dir, "places.fictive.ndjson")
	if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	mustRun(t, "leads", "campaigns", "register")
	mustRun(t, "leads", "places", "import", "overture", "--campaign", "cafes-loyalty", "--release", placesRelease, file)
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	return dir
}

// placeRows lists every lead_place_type row as "org name|type|source|detail|actor", in order.
func placeRows(t *testing.T) []string {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT o.name || '|' || t.place_type || '|' || t.source || '|' || t.detail || '|' || t.actor
		 FROM lead_place_type t JOIN lead l ON l.id = t.lead_id JOIN org o ON o.id = l.org_id ORDER BY t.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// forgetPlaceRows puts the database where an existing install stands before apply first runs:
// leads built, no name rows.
func forgetPlaceRows(t *testing.T) {
	t.Helper()
	if _, err := storetest.OwnerConn(t).Exec(context.Background(), `DELETE FROM lead_place_type`); err != nil {
		t.Fatal(err)
	}
}

// leadSnapshot is every lead's state, kind and call count plus the lead_event count: what no
// name row may change.
func leadSnapshot(t *testing.T) string {
	t.Helper()
	var s string
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT coalesce(string_agg(l.id || ':' || l.state || ':' || l.kind || ':' ||
		   (SELECT count(*) FROM lead_label x WHERE x.lead_id = l.id), ',' ORDER BY l.id), '') || ' events ' ||
		   (SELECT count(*) FROM lead_event) FROM lead l`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// placeLeadID is the id of the lead of the org called name.
func placeLeadID(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT l.id FROM lead l JOIN org o ON o.id = l.org_id WHERE o.name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("lead of %s: %v", name, err)
	}
	return id
}

const roasterRow = "Fictive Hill Roasters|roastery|name|roasters|system"

// TestPlaceTypesReportWritesNothing: before apply has run the report names what would match,
// writes no row, and honours --sample and --campaign; a bad call is a usage error.
func TestPlaceTypesReportWritesNothing(t *testing.T) {
	placeTypesImport(t)
	forgetPlaceRows(t)
	before := leadSnapshot(t)
	out := mustRun(t, "leads", "place-types", "report", "--campaign", "cafes-loyalty", "--sample", "5")
	wantLines(t, "report", out,
		"leads place-types report: campaign cafes-loyalty, 2 café leads, in any state",
		"  coffee shops: 2",
		"  roasteries: 0, of them 0 from a name match",
		"  names with a roastery word and no name row yet: 1 (apply would write these)",
		"  names with a roastery word: 1",
		"  Fictive Hill Roasters (roasters)",
	)
	if got := placeRows(t); len(got) != 0 {
		t.Errorf("the report wrote rows: %v", got)
	}
	if got := leadSnapshot(t); got != before {
		t.Errorf("the report changed the leads: %s, was %s", got, before)
	}
	if out := mustRun(t, "leads", "place-types", "report", "--sample", "0"); strings.Contains(out, "Fictive Hill") {
		t.Errorf("--sample 0 printed a name:\n%s", out)
	} else if !strings.Contains(out, "all café campaigns, 2 café leads") {
		t.Errorf("no --campaign does not read every café campaign:\n%s", out)
	}
	for _, args := range [][]string{{"report", "--sample", "-1"}, {"report", "extra"}, {"apply", "extra"}, {"nope"}, {}} {
		code, _, errOut := leadsRun(t, append([]string{"leads", "place-types"}, args...)...)
		if code == 0 || !strings.Contains(errOut, "place-types") {
			t.Errorf("place-types %q: exit %d, %q", args, code, errOut)
		}
	}
	if code, _, errOut := leadsRun(t, "leads", "place-types", "report", "--campaign", "no-such"); code == 0 || !strings.Contains(errOut, "no campaign is called") {
		t.Errorf("an unknown campaign: exit %d, %q", code, errOut)
	}
}

// TestPlaceTypesApply: apply writes one name row for the roastery name and none for the café
// (a name word never writes a café row), a rerun writes zero and says so, the effective type
// follows, and neither run touches a lead's state or call.
func TestPlaceTypesApply(t *testing.T) {
	placeTypesImport(t)
	forgetPlaceRows(t)
	before := leadSnapshot(t)
	if out := mustRun(t, "leads", "place-types", "apply", "--campaign", "cafes-loyalty"); !strings.Contains(out, "1 name row written") {
		t.Errorf("apply:\n%s", out)
	}
	if got := placeRows(t); !slices.Equal(got, []string{roasterRow}) {
		t.Errorf("rows after apply: %v", got)
	}
	if out := mustRun(t, "leads", "place-types", "apply"); !strings.Contains(out, "0 rows written") {
		t.Errorf("rerun:\n%s", out)
	}
	if got := placeRows(t); len(got) != 1 {
		t.Errorf("the rerun wrote: %v", got)
	}
	if got := leadSnapshot(t); got != before {
		t.Errorf("apply changed the leads: %s, was %s", got, before)
	}
	wantLines(t, "report after apply", mustRun(t, "leads", "place-types", "report"),
		"  coffee shops: 1",
		"  roasteries: 1, of them 1 from a name match",
		"  names with a roastery word and no name row yet: 0 (apply would write these)",
	)
}

// TestPlaceTypesLeaveDrupalLeadsAlone: a Drupal lead whose org carries a roastery word gets no
// row from apply and is not counted by the report.
func TestPlaceTypesLeaveDrupalLeadsAlone(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	cafesDataDir(t, dir)
	batchFixture(t)
	mustRun(t, "leads", "campaigns", "register")
	mustRun(t, "leads", "build")
	owner := storetest.OwnerConn(t)
	tag, err := owner.Exec(ctx, `UPDATE org SET name = 'Fictive Roastery' WHERE id IN (SELECT org_id FROM lead)`)
	if err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("name the Drupal orgs: %d, %v", tag.RowsAffected(), err)
	}
	before := leadSnapshot(t)
	if out := mustRun(t, "leads", "place-types", "report"); !strings.Contains(out, "0 café leads") {
		t.Errorf("the report counted a Drupal lead:\n%s", out)
	}
	if out := mustRun(t, "leads", "place-types", "apply"); !strings.Contains(out, "0 rows written") {
		t.Errorf("apply:\n%s", out)
	}
	if got := placeRows(t); len(got) != 0 {
		t.Errorf("a Drupal lead got a row: %v", got)
	}
	if got := leadSnapshot(t); got != before {
		t.Errorf("apply changed a Drupal lead: %s, was %s", got, before)
	}
}

// TestPlaceTypesBuildTags: the build that creates a lead tags it, so apply has nothing left;
// a rebuild writes no second row and creates no café row.
func TestPlaceTypesBuildTags(t *testing.T) {
	placeTypesImport(t)
	if got := placeRows(t); !slices.Equal(got, []string{roasterRow}) {
		t.Errorf("rows after the build: %v", got)
	}
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	if got := placeRows(t); len(got) != 1 {
		t.Errorf("rows after a rebuild: %v", got)
	}
	if out := mustRun(t, "leads", "place-types", "apply"); !strings.Contains(out, "0 rows written") {
		t.Errorf("apply after a build:\n%s", out)
	}
}

// TestPlaceTypesOperatorCorrectionSurvives: an operator's correction to café keeps deciding
// the type through apply and a rebuild, and neither writes another row.
func TestPlaceTypesOperatorCorrectionSurvives(t *testing.T) {
	ctx := context.Background()
	placeTypesImport(t)
	id := placeLeadID(t, "Fictive Hill Roasters")
	q := leadstoretest.Q(t)
	if err := q.SetPlaceType(ctx, id, leadstore.PlaceCafe, leadstore.PlaceFromOperator, "", nil, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "place-types", "apply")
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	if got := placeRows(t); len(got) != 2 {
		t.Errorf("rows: %v, want the name row and the correction", got)
	}
	types, err := q.PlaceTypes(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if r := types[id]; r.Type != leadstore.PlaceCafe || r.Source != leadstore.PlaceFromOperator {
		t.Errorf("after apply and a rebuild: %+v, want the operator's café", r)
	}
	wantLines(t, "report", mustRun(t, "leads", "place-types", "report"), "  coffee shops: 2", "  roasteries: 0, of them 0 from a name match")
}

// siteNameOf sets the site's own name on the latest probe of the org on domain.
func siteNameOf(t *testing.T, domain, name string) {
	t.Helper()
	if _, err := storetest.OwnerConn(t).Exec(context.Background(),
		`UPDATE site_probe SET site_name = $2 WHERE id =
		   (SELECT s.last_probe_id FROM site s JOIN org o ON o.site_id = s.id WHERE o.domain = $1)`, domain, name); err != nil {
		t.Fatal(err)
	}
}

// orgNameOf sets the name of the org on domain, as its map listing would give it.
func orgNameOf(t *testing.T, domain, name string) {
	t.Helper()
	if _, err := storetest.OwnerConn(t).Exec(context.Background(), `UPDATE org SET name = $2 WHERE domain = $1`, domain, name); err != nil {
		t.Fatal(err)
	}
}

// demoPlacesProbed imports the demo places and probes their sites, with no lead built yet.
func demoPlacesProbed(t *testing.T) {
	t.Helper()
	dir := leadsDB(t)
	cafesDataDir(t, dir)
	importDemoPlaces(t, dir)
	sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", cafeFixtures(t))
}

// TestPlaceTypesLookInBothNames: a word only in the map listing's name, under a generic site
// title, matches; so does a word only in the site's name; with a word in each, the map
// listing's wins. Report and apply agree with the build, and the sample prints the name that
// matched.
func TestPlaceTypesLookInBothNames(t *testing.T) {
	demoPlacesProbed(t)
	orgNameOf(t, "harbour-lane-coffee.example", "Fictive Harbour Roastery")
	siteNameOf(t, "harbour-lane-coffee.example", "Home")
	orgNameOf(t, "bean-house.example", "Fictive Bean House")
	siteNameOf(t, "bean-house.example", "Fictive Bean Torrefazione")
	orgNameOf(t, "cafeneaua-exemplu.example", "Fictive Roasters One")
	siteNameOf(t, "cafeneaua-exemplu.example", "Fictive Prajitorie Two")
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	want := []string{
		"Fictive Harbour Roastery|roastery|name|roastery|system",
		"Fictive Bean House|roastery|name|torrefazione|system",
		"Fictive Roasters One|roastery|name|roasters|system",
	}
	got := placeRows(t)
	slices.Sort(got)
	sorted := slices.Sorted(slices.Values(want))
	if !slices.Equal(got, sorted) {
		t.Errorf("rows after the build: %v, want %v", got, sorted)
	}

	forgetPlaceRows(t)
	wantLines(t, "report", mustRun(t, "leads", "place-types", "report", "--sample", "10"),
		"  names with a roastery word and no name row yet: 3 (apply would write these)",
		"  names with a roastery word: 3",
		"  Fictive Harbour Roastery (roastery)",
		"  Fictive Bean Torrefazione (torrefazione)",
		"  Fictive Roasters One (roasters)",
	)
	if out := mustRun(t, "leads", "place-types", "apply"); !strings.Contains(out, "3 name rows written") {
		t.Errorf("apply:\n%s", out)
	}
	got = placeRows(t)
	slices.Sort(got)
	if !slices.Equal(got, sorted) {
		t.Errorf("rows after apply: %v, want %v", got, sorted)
	}
	if out := mustRun(t, "leads", "place-types", "apply"); !strings.Contains(out, "0 rows written") {
		t.Errorf("rerun:\n%s", out)
	}
}

// TestPlaceTypesBuildRefreshAndSkips: a refreshed lead still new is tagged when a name brings a
// word it has no row for. A lead the build skips gets no row, nor does one past new.
func TestPlaceTypesBuildRefreshAndSkips(t *testing.T) {
	ctx := context.Background()
	demoPlacesProbed(t)
	// The own vendor's customer is a Skip, and it would match if the build looked at it.
	orgNameOf(t, "copper-kettle.example", "Fictive Skipped Roastery")
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	if got := placeRows(t); len(got) != 0 {
		t.Fatalf("rows after the first build: %v", got)
	}
	siteNameOf(t, "harbour-lane-coffee.example", "Fictive Torrefazione")
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	got := placeRows(t)
	if len(got) != 1 || !strings.HasSuffix(got[0], "|roastery|name|torrefazione|system") {
		t.Errorf("rows after the rebuild: %v", got)
	}
	// A lead past new is left alone by the build, as it leaves everything else about it.
	orgNameOf(t, "harbour-lane-coffee.example", "Fictive Harbour Roastery")
	if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE lead SET state = 'batched'
		WHERE org_id = (SELECT id FROM org WHERE domain = 'harbour-lane-coffee.example')`); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	if after := placeRows(t); len(after) != 1 {
		t.Errorf("the build tagged a lead past new: %v", after)
	}
}
