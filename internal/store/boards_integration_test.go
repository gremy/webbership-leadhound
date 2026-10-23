//go:build integration

package store_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

func boardImport(atsName, slug, region, name, origin string) store.BoardImport {
	return store.BoardImport{BoardKey: store.BoardKey{ATS: atsName, Slug: slug, Region: region}, Name: name, Origin: origin}
}

// boardRow reads one registry row as the owner.
func boardRow(t *testing.T, s *store.Store, atsName, slug, region string) store.Board {
	t.Helper()
	bs, err := s.Q().ListBoards(context.Background(), store.BoardFilter{ATS: atsName})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		if b.Slug == slug && b.Region == region {
			return b
		}
	}
	t.Fatalf("no board %s/%s region %q", atsName, slug, region)
	return store.Board{}
}

// TestImportBoardsIdempotentAndOriginPrecedence: a second import adds nothing; an origin
// only moves up (manual > found > seed-list = commoncrawl); a re-import never resets the
// state or the checks; slugs are stored lower-case, and the same slug in another region is
// another board.
func TestImportBoardsIdempotentAndOriginPrecedence(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()

	first := []store.BoardImport{
		boardImport("greenhouse", "ExampleCorp", "", "Example Corp", store.OriginSeedList),
		boardImport("lever", "samplesoft", "", "", store.OriginCommonCrawl),
		boardImport("lever", "samplesoft", "eu", "", store.OriginCommonCrawl),
		boardImport("ashby", "demolabs", "", "Demo Labs", store.OriginManual),
		boardImport("ashby", "DEMOLABS", "", "", store.OriginSeedList), // the same board again
	}
	added, known, err := q.ImportBoards(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if added != 4 || known != 1 {
		t.Fatalf("first import: added %d, known %d; want 4, 1", added, known)
	}
	if b := boardRow(t, s, "greenhouse", "examplecorp", ""); b.State != store.BoardUnverified || b.Origin != store.OriginSeedList || b.Name != "Example Corp" {
		t.Errorf("greenhouse row = %+v", b)
	}

	// The same file again: nothing added, nothing changed.
	added, known, err = q.ImportBoards(ctx, first)
	if err != nil || added != 0 || known != 5 {
		t.Fatalf("second import: added %d, known %d, err %v; want 0, 5", added, known, err)
	}

	// A checked board keeps its state and checks through a re-import.
	now := time.Date(2030, 5, 6, 10, 0, 0, 0, time.UTC)
	if _, err := q.RecordBoardCheck(ctx, store.BoardKey{ATS: "greenhouse", Slug: "examplecorp"}, store.CheckOK, 7, time.UTC, now); err != nil {
		t.Fatal(err)
	}

	// Origins: seed-list → manual moves up; manual → seed-list stays; commoncrawl →
	// seed-list (same rank) stays; commoncrawl → found moves up. An empty name is filled.
	added, known, err = q.ImportBoards(ctx, []store.BoardImport{
		boardImport("greenhouse", "examplecorp", "", "Other Name", store.OriginManual),
		boardImport("ashby", "demolabs", "", "", store.OriginSeedList),
		boardImport("lever", "samplesoft", "", "SampleSoft", store.OriginSeedList),
		boardImport("lever", "samplesoft", "eu", "", store.OriginFound),
	})
	if err != nil || added != 0 || known != 4 {
		t.Fatalf("precedence import: added %d, known %d, err %v", added, known, err)
	}
	for _, tt := range []struct {
		ats, slug, region, origin, name string
	}{
		{"greenhouse", "examplecorp", "", store.OriginManual, "Example Corp"},
		{"ashby", "demolabs", "", store.OriginManual, "Demo Labs"},
		{"lever", "samplesoft", "", store.OriginCommonCrawl, "SampleSoft"},
		{"lever", "samplesoft", "eu", store.OriginFound, ""},
	} {
		b := boardRow(t, s, tt.ats, tt.slug, tt.region)
		if b.Origin != tt.origin || b.Name != tt.name {
			t.Errorf("%s/%s %q: origin %q name %q; want %q, %q", tt.ats, tt.slug, tt.region, b.Origin, b.Name, tt.origin, tt.name)
		}
	}
	gh := boardRow(t, s, "greenhouse", "examplecorp", "")
	if gh.State != store.BoardOK || gh.CheckedAt == nil || gh.PostingsLast == nil || *gh.PostingsLast != 7 {
		t.Errorf("re-import reset the checked board: %+v", gh)
	}

	// Many rows go in chunks and still count right.
	var many []store.BoardImport
	for i := range 2500 {
		many = append(many, boardImport("lever", fmt.Sprintf("bulk-%d", i), "", "", store.OriginSeedList))
	}
	added, known, err = q.ImportBoards(ctx, many)
	if err != nil || added != 2500 || known != 0 {
		t.Fatalf("bulk import: added %d, known %d, err %v", added, known, err)
	}
}

// TestRecordBoardCheckTransitions: ok resets misses; a miss counts once per local day; three
// misses on separate days retire a board that is not manual; a manual board keeps its
// outcome state and is flagged; a board missing from the registry is never inserted.
func TestRecordBoardCheckTransitions(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	if _, _, err := q.ImportBoards(ctx, []store.BoardImport{
		boardImport("greenhouse", "examplecorp", "", "", store.OriginSeedList),
		boardImport("ashby", "demolabs", "", "", store.OriginManual),
	}); err != nil {
		t.Fatal(err)
	}
	gh := store.BoardKey{ATS: "greenhouse", Slug: "ExampleCorp"} // case is ignored
	manual := store.BoardKey{ATS: "ashby", Slug: "demolabs"}
	// A zone east of UTC: 23:30 UTC on the 1st is already the 2nd there.
	loc := time.FixedZone("east", 2*60*60)
	day := func(d, h int) time.Time { return time.Date(2030, 1, d, h, 0, 0, 0, time.UTC) }

	step := func(k store.BoardKey, outcome string, at time.Time, wantState string, wantMisses int) store.BoardChange {
		t.Helper()
		ch, err := q.RecordBoardCheck(ctx, k, outcome, 3, loc, at)
		if err != nil {
			t.Fatal(err)
		}
		if !ch.Found || ch.New != wantState || ch.Misses != wantMisses {
			t.Fatalf("%v %s at %v: %+v; want state %s misses %d", k, outcome, at, ch, wantState, wantMisses)
		}
		return ch
	}

	ch := step(gh, store.CheckOK, day(1, 8), store.BoardOK, 0)
	if !ch.Changed() || ch.Old != store.BoardUnverified {
		t.Errorf("first ok: %+v", ch)
	}
	step(gh, store.CheckNotFound, day(1, 9), store.BoardNotFound, 1)
	step(gh, store.CheckError, day(1, 12), store.BoardError, 1)       // same local day: no new miss
	step(gh, store.CheckError, day(1, 23), store.BoardError, 2)       // 01:00 on the 2nd, local
	step(gh, store.CheckNotFound, day(2, 10), store.BoardNotFound, 2) // still the 2nd, local
	ch = step(gh, store.CheckNotFound, day(3, 10), store.BoardRetired, 3)
	if !ch.Changed() || ch.Old != store.BoardNotFound || ch.Flagged {
		t.Errorf("retire: %+v", ch)
	}
	row := boardRow(t, s, "greenhouse", "examplecorp", "")
	if row.PostingsLast == nil || *row.PostingsLast != 3 || row.LastOKAt == nil || !row.LastOKAt.Equal(day(1, 8)) || !row.CheckedAt.Equal(day(3, 10)) {
		t.Errorf("retired row = %+v", row)
	}
	// A retired board that answers again comes back.
	step(gh, store.CheckOK, day(4, 10), store.BoardOK, 0)

	// An ok in between resets the count.
	step(gh, store.CheckError, day(5, 10), store.BoardError, 1)
	step(gh, store.CheckError, day(6, 10), store.BoardError, 2)
	step(gh, store.CheckOK, day(7, 10), store.BoardOK, 0)
	step(gh, store.CheckError, day(8, 10), store.BoardError, 1)

	// A manual board is never retired, only flagged.
	step(manual, store.CheckNotFound, day(1, 10), store.BoardNotFound, 1)
	step(manual, store.CheckNotFound, day(2, 10), store.BoardNotFound, 2)
	ch = step(manual, store.CheckNotFound, day(3, 10), store.BoardNotFound, 3)
	if !ch.Flagged || !ch.Manual || ch.Changed() {
		t.Errorf("manual third miss: %+v", ch)
	}
	ch = step(manual, store.CheckError, day(4, 10), store.BoardError, 4)
	if !ch.Flagged {
		t.Errorf("manual fourth miss: %+v", ch)
	}

	// Unknown boards are left out, never inserted.
	ch, err := q.RecordBoardCheck(ctx, store.BoardKey{ATS: "lever", Slug: "nowhere"}, store.CheckOK, 1, loc, day(1, 10))
	if err != nil || ch.Found {
		t.Errorf("unknown board: %+v, %v", ch, err)
	}
	counts, err := q.CountBoards(ctx, store.BoardFilter{})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, c := range counts {
		total += c.N
	}
	if total != 2 {
		t.Errorf("boards = %d, want 2", total)
	}
	if _, err := q.RecordBoardCheck(ctx, gh, "maybe", 0, loc, day(9, 10)); err == nil {
		t.Error("unknown outcome accepted")
	}
}

// TestBoardSelection: collect reads ok boards and manual boards that are not retired; list
// filters by state and ats, leaves retired out on request, orders and limits.
func TestBoardSelection(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	if _, _, err := q.ImportBoards(ctx, []store.BoardImport{
		boardImport("greenhouse", "okboard", "", "", store.OriginSeedList),
		boardImport("greenhouse", "unverified", "", "", store.OriginSeedList),
		boardImport("lever", "manual-missing", "", "", store.OriginManual),
		boardImport("lever", "manual-retired", "", "", store.OriginManual),
		boardImport("ashby", "retired", "", "", store.OriginSeedList),
	}); err != nil {
		t.Fatal(err)
	}
	ownerExec(t, `UPDATE board SET state = 'ok', checked_at = now() WHERE slug = 'okboard'`)
	ownerExec(t, `UPDATE board SET state = 'not_found', checked_at = now() - interval '1 day' WHERE slug = 'manual-missing'`)
	ownerExec(t, `UPDATE board SET state = 'retired' WHERE slug IN ('manual-retired', 'retired')`)

	slugs := func(bs []store.Board) []string {
		var out []string
		for _, b := range bs {
			out = append(out, b.Slug)
		}
		return out
	}
	got, err := q.CollectBoards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"okboard", "manual-missing"}; !slices.Equal(slugs(got), want) {
		t.Errorf("collect boards = %v, want %v", slugs(got), want)
	}
	for _, tt := range []struct {
		f    store.BoardFilter
		want []string
	}{
		{store.BoardFilter{}, []string{"okboard", "unverified", "manual-missing", "manual-retired", "retired"}},
		{store.BoardFilter{NotRetired: true}, []string{"okboard", "unverified", "manual-missing"}},
		{store.BoardFilter{State: "retired", NotRetired: true}, []string{"manual-retired", "retired"}},
		{store.BoardFilter{ATS: "greenhouse"}, []string{"okboard", "unverified"}},
		{store.BoardFilter{NotRetired: true, Order: store.BoardOrderOldest}, []string{"unverified", "manual-missing", "okboard"}},
		{store.BoardFilter{NotRetired: true, Order: store.BoardOrderOldest, Limit: 2}, []string{"unverified", "manual-missing"}},
	} {
		got, err := q.ListBoards(ctx, tt.f)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(slugs(got), tt.want) {
			t.Errorf("ListBoards(%+v) = %v, want %v", tt.f, slugs(got), tt.want)
		}
	}
	if got, err := q.ListBoards(ctx, store.BoardFilter{NotRetired: true, Order: store.BoardOrderRandom}); err != nil || len(got) != 3 {
		t.Errorf("random order: %d boards, %v", len(got), err)
	}
	if _, err := q.ListBoards(ctx, store.BoardFilter{Order: "newest"}); err == nil {
		t.Error("unknown order accepted")
	}
	counts, err := q.CountBoards(ctx, store.BoardFilter{ATS: "greenhouse"})
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 2 || counts[0] != (store.BoardCount{ATS: "greenhouse", State: "ok", N: 1}) {
		t.Errorf("counts = %+v", counts)
	}
}

// TestBoardTableChecksAndNoDelete: the board table refuses unknown values and upper-case
// slugs, and the app role cannot delete a board (a dead board is retired).
func TestBoardTableChecksAndNoDelete(t *testing.T) {
	ctx := context.Background()
	storetest.ResetDB(t)
	app := storetest.AppConn(t)
	if _, err := app.Exec(ctx, `INSERT INTO board (ats, slug, origin) VALUES ('lever', 'fine', 'manual')`); err != nil {
		t.Fatalf("valid insert: %v", err)
	}
	for name, q := range map[string]string{
		"ats":    `INSERT INTO board (ats, slug, origin) VALUES ('workday', 'x', 'manual')`,
		"slug":   `INSERT INTO board (ats, slug, origin) VALUES ('lever', 'Upper', 'manual')`,
		"empty":  `INSERT INTO board (ats, slug, origin) VALUES ('lever', '', 'manual')`,
		"region": `INSERT INTO board (ats, slug, region, origin) VALUES ('lever', 'x', 'us', 'manual')`,
		"origin": `INSERT INTO board (ats, slug, origin) VALUES ('lever', 'x', 'guess')`,
		"state":  `INSERT INTO board (ats, slug, origin, state) VALUES ('lever', 'x', 'manual', 'dead')`,
		"misses": `INSERT INTO board (ats, slug, origin, misses) VALUES ('lever', 'x', 'manual', -1)`,
	} {
		if _, err := app.Exec(ctx, q); sqlState(err) != "23514" {
			t.Errorf("%s: err = %v, want 23514 check_violation", name, err)
		}
	}
	if _, err := app.Exec(ctx, `INSERT INTO board (ats, slug, origin) VALUES ('lever', 'fine', 'found')`); sqlState(err) != "23505" {
		t.Errorf("duplicate: err = %v, want 23505 unique_violation", err)
	}
	if _, err := app.Exec(ctx, `DELETE FROM board`); sqlState(err) != "42501" {
		t.Errorf("DELETE FROM board as app: err = %v, want 42501", err)
	}
	if _, err := app.Exec(ctx, `UPDATE board SET state = 'retired'`); err != nil {
		t.Errorf("UPDATE board as app: %v", err)
	}
}

// TestRecordBoardCheckNoVerdict: a no-verdict outcome stamps checked_at and nothing else: no
// miss, no state change, last_ok_at and postings_last kept, even on a board one miss from
// retiring.
func TestRecordBoardCheckNoVerdict(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	if _, _, err := q.ImportBoards(ctx, []store.BoardImport{boardImport("greenhouse", "examplecorp", "", "", store.OriginSeedList)}); err != nil {
		t.Fatal(err)
	}
	k := store.BoardKey{ATS: "greenhouse", Slug: "examplecorp"}
	day := func(d int) time.Time { return time.Date(2030, 1, d, 10, 0, 0, 0, time.UTC) }
	for i, outcome := range []string{store.CheckOK, store.CheckError, store.CheckError} {
		if _, err := q.RecordBoardCheck(ctx, k, outcome, 5, time.UTC, day(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	before := boardRow(t, s, "greenhouse", "examplecorp", "")
	if before.State != store.BoardError || before.Misses != 2 {
		t.Fatalf("setup row = %+v", before)
	}
	ch, err := q.RecordBoardCheck(ctx, k, store.CheckNoVerdict, 99, time.UTC, day(4))
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Found || ch.Changed() || ch.Old != store.BoardError || ch.New != store.BoardError || ch.Misses != 2 || ch.Flagged {
		t.Errorf("no verdict change = %+v", ch)
	}
	after := boardRow(t, s, "greenhouse", "examplecorp", "")
	if after.CheckedAt == nil || !after.CheckedAt.Equal(day(4)) {
		t.Errorf("checked_at = %v, want %v", after.CheckedAt, day(4))
	}
	if after.State != before.State || after.Misses != before.Misses ||
		after.LastMissDay == nil || !after.LastMissDay.Equal(*before.LastMissDay) ||
		after.LastOKAt == nil || !after.LastOKAt.Equal(day(1)) ||
		after.PostingsLast == nil || *after.PostingsLast != 5 {
		t.Errorf("no verdict changed more than checked_at: before %+v, after %+v", before, after)
	}
	// A manual board past the retire count is not flagged again by a no-verdict.
	if _, _, err := q.ImportBoards(ctx, []store.BoardImport{boardImport("ashby", "demolabs", "", "", store.OriginManual)}); err != nil {
		t.Fatal(err)
	}
	ownerExec(t, `UPDATE board SET state = 'not_found', misses = 3 WHERE slug = 'demolabs'`)
	ch, err = q.RecordBoardCheck(ctx, store.BoardKey{ATS: "ashby", Slug: "demolabs"}, store.CheckNoVerdict, 0, time.UTC, day(5))
	if err != nil || !ch.Found || !ch.Manual || ch.Flagged || ch.New != store.BoardNotFound || ch.Misses != 3 {
		t.Errorf("manual no verdict = %+v, %v", ch, err)
	}
	// An unknown board is left out, never inserted.
	if ch, err := q.RecordBoardCheck(ctx, store.BoardKey{ATS: "lever", Slug: "nowhere"}, store.CheckNoVerdict, 0, time.UTC, day(5)); err != nil || ch.Found {
		t.Errorf("unknown board: %+v, %v", ch, err)
	}
}

// TestImportManualRevivesRetired: an import that raises a retired board to manual brings it
// back (unverified, misses 0, no miss day) and counts it as revived; a retired board
// re-imported at a lower origin, or already manual, stays retired.
func TestImportManualRevivesRetired(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	if _, _, err := q.ImportBoards(ctx, []store.BoardImport{
		boardImport("greenhouse", "comeback", "", "", store.OriginSeedList),
		boardImport("lever", "staysdead", "", "", store.OriginCommonCrawl),
		boardImport("ashby", "manualdead", "", "", store.OriginManual),
	}); err != nil {
		t.Fatal(err)
	}
	ownerExec(t, `UPDATE board SET state = 'retired', misses = 3, last_miss_day = '2030-01-03', checked_at = now(), postings_last = 4`)

	tally, err := q.ImportBoardsTally(ctx, []store.BoardImport{
		boardImport("greenhouse", "comeback", "", "", store.OriginManual),
		boardImport("lever", "staysdead", "", "", store.OriginFound),
		boardImport("ashby", "manualdead", "", "", store.OriginManual),
		boardImport("ashby", "fresh", "", "", store.OriginManual),
	})
	if err != nil {
		t.Fatal(err)
	}
	if tally != (store.ImportTally{Added: 1, Known: 3, Revived: 1}) {
		t.Errorf("tally = %+v, want added 1, known 3, revived 1", tally)
	}
	back := boardRow(t, s, "greenhouse", "comeback", "")
	if back.Origin != store.OriginManual || back.State != store.BoardUnverified || back.Misses != 0 || back.LastMissDay != nil ||
		back.CheckedAt == nil || back.PostingsLast == nil || *back.PostingsLast != 4 {
		t.Errorf("revived row = %+v", back)
	}
	if b := boardRow(t, s, "lever", "staysdead", ""); b.Origin != store.OriginFound || b.State != store.BoardRetired || b.Misses != 3 {
		t.Errorf("found re-import = %+v, want retired kept", b)
	}
	if b := boardRow(t, s, "ashby", "manualdead", ""); b.State != store.BoardRetired {
		t.Errorf("already-manual re-import = %+v, want retired kept", b)
	}
	// The same import again revives nothing more.
	if tally, err := q.ImportBoardsTally(ctx, []store.BoardImport{boardImport("greenhouse", "comeback", "", "", store.OriginManual)}); err != nil || tally != (store.ImportTally{Known: 1}) {
		t.Errorf("second tally = %+v, %v", tally, err)
	}
}
