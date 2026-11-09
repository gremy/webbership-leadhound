//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// businessKindName is the name the decider answers the business-kind question under.
var businessKindName = leadrate.BusinessKindAIQuestion(leadstore.TypeDrupal).Name

// businessKindAnswer scripts stub to answer kind at confidence for the lead whose state is state.
func businessKindAnswer(stub *ai.Fake, state, kind string, confidence float64) {
	answers := stub.ByState[state]
	if answers == nil {
		answers = map[string]ai.Answer{}
		stub.ByState[state] = answers
	}
	answers[businessKindName] = ai.Answer{Choice: kind, Confidence: confidence}
}

// namedDrupalLead is the woodgrove lead with a name of its own, so its state differs from the
// other Drupal leads' and a script for it answers it alone.
func namedDrupalLead(t *testing.T) int64 {
	t.Helper()
	ownerExec(t, `UPDATE org SET name = 'Woodgrove Example' WHERE domain = 'woodgrove.example'`)
	return leadIDOf(t, "woodgrove.example")
}

// businessKindRows counts lead_business_kind rows matching where.
func businessKindRows(t *testing.T, where string) int {
	t.Helper()
	return dbCount(t, `SELECT count(*) FROM lead_business_kind WHERE `+where)
}

// latestBusinessKind is the kind of lead id's latest row.
func latestBusinessKind(t *testing.T, id int64) string {
	t.Helper()
	var kind string
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT kind FROM lead_business_kind WHERE lead_id = $1 ORDER BY id DESC LIMIT 1`, id).Scan(&kind); err != nil {
		t.Fatalf("latest kind of lead %d: %v", id, err)
	}
	return kind
}

// kindFailure answers from a script until a number of business-kind questions were
// asked, then answers each further one with err. Every other question stays scripted.
type kindFailure struct {
	*ai.Fake
	allowed int
	err     error
	asked   int
}

// Decide counts business-kind questions and fails the ones past allowed.
func (d *kindFailure) Decide(ctx context.Context, state string, qs []ai.Question) (map[string]ai.Answer, ai.Usage, error) {
	if len(qs) == 1 && qs[0].Name == businessKindName {
		d.asked++
		if d.asked > d.allowed {
			return nil, ai.Usage{}, d.err
		}
	}
	return d.Fake.Decide(ctx, state, qs)
}

// TestBusinessKindColdStartAndRerun: the first run asks about each open lead once, café and
// Drupal alike, under its own type's question, and stores every answer with its question, its
// confidence and a 32-byte hash. A rerun with nothing changed makes no call and writes nothing.
func TestBusinessKindColdStartAndRerun(t *testing.T) {
	cafeID, _ := placeTypeScene(t)
	drupalID := namedDrupalLead(t)
	stub := placeTypeStub()
	businessKindAnswer(stub, placeTypeState(t, cafeID), leadrate.BusinessKindBakeryCafe, 0.9)
	businessKindAnswer(stub, placeTypeState(t, drupalID), leadrate.BusinessKindEducation, 0.8)

	sum, ps, ks := rateRunAll(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if sum.Rated != 8 || !ps.Ran || !ks.Ran || ks.Calls != 8 || ks.Due != 0 || ks.StopReason != "" || ks.Named != 2 || ks.Unclear != 6 {
		t.Fatalf("first run: relevance %+v, place type %+v, business kind %+v", sum, ps, ks)
	}
	if n := businessKindRows(t, `question = 'business-kind@1' AND length(input_sha256) = 32`); n != 8 {
		t.Errorf("%d rows as written, want 8", n)
	}
	if got := latestBusinessKind(t, cafeID); got != leadrate.BusinessKindBakeryCafe {
		t.Errorf("the café lead's kind = %q", got)
	}
	if got := latestBusinessKind(t, drupalID); got != leadrate.BusinessKindEducation {
		t.Errorf("the Drupal lead's kind = %q", got)
	}
	if n := businessKindRows(t, fmt.Sprintf(`lead_id = %d AND confidence = 0.8::real`, drupalID)); n != 1 {
		t.Errorf("%d rows hold Jev's confidence, want 1", n)
	}

	calls := stub.Calls()
	_, ps, ks = rateRunAll(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if ks.Calls != 0 || ks.Unchanged != 8 || ks.Due != 0 || ps.Calls != 0 || stub.Calls() != calls {
		t.Errorf("rerun: %+v and %d decider calls, want none asked and 8 unchanged", ks, stub.Calls()-calls)
	}
	if n := businessKindRows(t, `true`); n != 8 {
		t.Errorf("%d rows after the rerun, want still 8", n)
	}
}

// TestBusinessKindAsksEachTypesOwnOptions: a café lead is offered the café kinds and a Drupal
// lead the Drupal kinds, so a café kind scripted for a Drupal lead is refused by the question.
func TestBusinessKindAsksEachTypesOwnOptions(t *testing.T) {
	cafeID, _ := placeTypeScene(t)
	drupalID := namedDrupalLead(t)
	stub := placeTypeStub()
	businessKindAnswer(stub, placeTypeState(t, cafeID), leadrate.BusinessKindEducation, 0.9)
	businessKindAnswer(stub, placeTypeState(t, drupalID), leadrate.BusinessKindBakeryCafe, 0.9)

	_, _, ks := rateRunAll(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if ks.Failures != 2 || ks.Due != 2 {
		t.Errorf("%+v, want both off-type answers failed and still due", ks)
	}
	if n := businessKindRows(t, fmt.Sprintf(`lead_id IN (%d, %d)`, cafeID, drupalID)); n != 0 {
		t.Errorf("%d rows on the off-type leads, want 0", n)
	}
}

// TestBusinessKindChangedInputAsksAgain: a changed input asks about that lead only and appends a
// row, the newest of which decides. An unclear answer with the same input is not asked again.
func TestBusinessKindChangedInputAsksAgain(t *testing.T) {
	cafeID, unclearID := placeTypeScene(t)
	stub := placeTypeStub()
	businessKindAnswer(stub, placeTypeState(t, cafeID), leadrate.BusinessKindBakeryCafe, 0.9)
	rateRunAll(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if got := latestBusinessKind(t, unclearID); got != leadrate.BusinessKindUnclear {
		t.Fatalf("the lead with no script holds %q, want unclear", got)
	}

	setLeadKind(t, "lucerne.example", cafe.KindHasLoyalty) // the state's lead type line changes
	businessKindAnswer(stub, placeTypeState(t, cafeID), leadrate.BusinessKindVenueCafe, 0.9)
	_, _, ks := rateRunAll(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if ks.Calls != 1 || ks.Named != 1 || ks.Unchanged != 7 {
		t.Errorf("after a changed input: %+v, want that lead asked and the unclear one left alone", ks)
	}
	if n := businessKindRows(t, fmt.Sprintf(`lead_id = %d`, cafeID)); n != 2 {
		t.Errorf("%d rows on the changed lead, want 2", n)
	}
	if n := businessKindRows(t, fmt.Sprintf(`lead_id = %d`, unclearID)); n != 1 {
		t.Errorf("%d rows on the unclear lead, want 1", n)
	}
	if got := latestBusinessKind(t, cafeID); got != leadrate.BusinessKindVenueCafe {
		t.Errorf("the changed lead's latest kind = %q", got)
	}
}

// TestBusinessKindNeverAskedLeads: a lead skipped, batched, contacted or suppressed keeps what it
// holds and is never asked, a selftest lead is never asked, and an erased org's rows are gone and
// its lead is not asked again. The one open lead left is asked.
func TestBusinessKindNeverAskedLeads(t *testing.T) {
	placeTypeScene(t)
	closed := map[string]string{
		"fourthcoffee.example": leadstore.LeadSkipped,
		"margie.example":       leadstore.LeadBatched,
		"humongous.example":    leadstore.LeadContacted,
		"cpandl.example":       leadstore.LeadSuppressed,
	}
	var closedIDs []int64
	for domain, state := range closed {
		id := leadIDOf(t, domain)
		closedIDs = append(closedIDs, id)
		ownerExec(t, `UPDATE lead SET state = $2 WHERE id = $1`, id, state)
	}
	self := leadIDOf(t, "woodgrove.example")
	ownerExec(t, `UPDATE lead SET kind = 'selftest', probe_id = NULL WHERE id = $1`, self)
	erased := leadIDOf(t, "bellows.example")

	stub := placeTypeStub()
	_, _, ks := rateRunAll(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if ks.Calls != 3 {
		t.Errorf("%+v, want the three open leads asked: bellows and the two cafés", ks)
	}
	for _, id := range closedIDs {
		if n := businessKindRows(t, fmt.Sprintf(`lead_id = %d`, id)); n != 0 {
			t.Errorf("%d rows on closed lead %d, want 0", n, id)
		}
	}
	if n := businessKindRows(t, fmt.Sprintf(`lead_id = %d`, self)); n != 0 {
		t.Errorf("%d rows on the selftest lead, want 0", n)
	}
	if n := businessKindRows(t, fmt.Sprintf(`lead_id = %d`, erased)); n != 1 {
		t.Fatalf("%d rows on the lead about to be erased, want 1", n)
	}

	t.Setenv("LEADHOUND_MIGRATE_DATABASE_URL", storetest.MigrateDSN()) // forget runs as the owner
	mustRun(t, "leads", "forget", "org", fmt.Sprint(orgIDOf(t, "bellows.example")), "--reason", "asked to be forgotten")
	if n := businessKindRows(t, fmt.Sprintf(`lead_id = %d`, erased)); n != 0 {
		t.Errorf("%d rows after forgetting the org, want 0", n)
	}
	_, _, ks = rateRunAll(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if ks.Calls != 0 || ks.Unchanged != 2 {
		t.Errorf("after the erasure: %+v, want the erased lead left alone", ks)
	}
	if n := businessKindRows(t, fmt.Sprintf(`lead_id = %d`, erased)); n != 0 {
		t.Errorf("%d rows after the rerun on the erased lead, want 0", n)
	}
}

// TestBusinessKindUnavailableStops: Jev unavailable mid-pass stops the pass with the throttle
// reason, the rows stored before it stay and the next run asks only what was left.
func TestBusinessKindUnavailableStops(t *testing.T) {
	placeTypeScene(t)
	d := &kindFailure{Fake: placeTypeStub(), allowed: 3, err: fmt.Errorf("jev: %w", ai.ErrUnavailable)}
	_, _, ks := rateRunAll(t, d, rateDeciderJev, defaultRateMax, 1000)
	if ks.StopReason != scoring.StopThrottled || ks.Calls != 4 || ks.Failures != 1 || ks.Due != 5 {
		t.Fatalf("%+v, want stopped throttled after 3 stored and 1 failed, 5 still due", ks)
	}
	if n := businessKindRows(t, `true`); n != 3 {
		t.Errorf("%d rows, want the 3 stored before the failure", n)
	}
	_, _, ks = rateRunAll(t, placeTypeStub(), rateDeciderJev, defaultRateMax, 1000)
	if ks.Calls != 5 || ks.Unchanged != 3 || ks.StopReason != "" {
		t.Errorf("the next run: %+v, want the 5 left asked", ks)
	}
}

// TestBusinessKindFailuresInARowStop: failed calls in a row stop the pass, as they stop the others.
func TestBusinessKindFailuresInARowStop(t *testing.T) {
	placeTypeScene(t)
	d := &kindFailure{Fake: placeTypeStub(), err: errors.New("jev: refused")}
	_, _, ks := rateRunAll(t, d, rateDeciderJev, defaultRateMax, 1000)
	if ks.StopReason != rateStopFailures || ks.Calls != rateFailuresInARow || ks.Failures != rateFailuresInARow || ks.Due != 8 {
		t.Errorf("%+v, want stopped after %d failures with all 8 due", ks, rateFailuresInARow)
	}
	if n := businessKindRows(t, `true`); n != 0 {
		t.Errorf("%d rows, want 0", n)
	}
}

// TestBusinessKindLowConfidenceIsUnclear: a known option below 0.6 is stored as unclear with
// Jev's own confidence, and 0.6 itself keeps the option.
func TestBusinessKindLowConfidenceIsUnclear(t *testing.T) {
	low, edge := placeTypeScene(t)
	stub := placeTypeStub()
	businessKindAnswer(stub, placeTypeState(t, low), leadrate.BusinessKindBakeryCafe, 0.59)
	businessKindAnswer(stub, placeTypeState(t, edge), leadrate.BusinessKindBakeryCafe, 0.6)
	_, _, ks := rateRunAll(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if ks.Named != 1 || ks.Unclear != 7 {
		t.Errorf("%+v, want 1 named and 7 unclear", ks)
	}
	if got := latestBusinessKind(t, low); got != leadrate.BusinessKindUnclear {
		t.Errorf("at 0.59 the kind is %q, want unclear", got)
	}
	if n := businessKindRows(t, fmt.Sprintf(`lead_id = %d AND confidence = 0.59::real`, low)); n != 1 {
		t.Errorf("%d rows hold 0.59, want 1", n)
	}
	if got := latestBusinessKind(t, edge); got != leadrate.BusinessKindBakeryCafe {
		t.Errorf("at 0.6 the kind is %q, want the option", got)
	}
}

// TestBusinessKindFakeStoresNothing: the fake decider, through the command and directly, asks and
// stores no business kind, and the summary has no business-kind line.
func TestBusinessKindFakeStoresNothing(t *testing.T) {
	placeTypeScene(t)
	out := mustRun(t, "leads", "rate", "--decider", "fake")
	if strings.Contains(out, "business kind:") {
		t.Errorf("a fake run printed a business-kind line:\n%s", out)
	}
	if _, _, ks := rateRunAll(t, placeTypeStub(), rateDeciderFake, defaultRateMax, 1000); ks.Ran {
		t.Errorf("a fake run ran the business-kind pass: %+v", ks)
	}
	if n := businessKindRows(t, `true`); n != 0 {
		t.Errorf("%d rows from the fake decider, want 0", n)
	}
}

// TestBusinessKindSpendsLast: relevance spends first and the place type next. The kind pass's
// budget is what they leave of --max, it does not run when either stopped short, and it stops at
// the leads cap with no call.
func TestBusinessKindSpendsLast(t *testing.T) {
	placeTypeScene(t)
	stub := placeTypeStub()

	// Relevance stops short.
	sum, ps, ks := rateRunAll(t, stub, rateDeciderJev, 3, 1000)
	if sum.StopReason != rateStopMax || ps.Ran || ks.Ran {
		t.Fatalf("--max 3: relevance %+v, place type %+v, business kind %+v", sum, ps, ks)
	}
	// Five leads still need relevance and two a place type, so --max 6 stops the place type.
	sum, ps, ks = rateRunAll(t, stub, rateDeciderJev, 6, 1000)
	if sum.Calls != 5 || ps.StopReason != rateStopMax || ks.Ran {
		t.Fatalf("--max 6: relevance %+v, place type %+v, business kind %+v", sum, ps, ks)
	}
	if n := businessKindRows(t, `true`); n != 0 {
		t.Fatalf("%d rows while an earlier pass was short, want 0", n)
	}
	// One place type is left, so --max 3 leaves the kind pass two calls of its eight.
	sum, ps, ks = rateRunAll(t, stub, rateDeciderJev, 3, 1000)
	if sum.Calls != 0 || ps.Calls != 1 || ks.Calls != 2 || ks.Due != 6 || ks.StopReason != rateStopMax {
		t.Fatalf("--max 3 again: relevance %+v, place type %+v, business kind %+v", sum, ps, ks)
	}
	// Only the kind pass has anything left, so it gets the whole of --max.
	_, ps, ks = rateRunAll(t, stub, rateDeciderJev, 4, 1000)
	if ps.Calls != 0 || ks.Calls != 4 || ks.Due != 2 || ks.StopReason != rateStopMax {
		t.Fatalf("--max 4: place type %+v, business kind %+v", ps, ks)
	}
	if n := businessKindRows(t, `true`); n != 6 {
		t.Errorf("%d rows, want 6", n)
	}
}

// TestBusinessKindCapSpent: with the leads cap spent by the earlier passes, the kind pass makes no
// call and stops at the cap.
func TestBusinessKindCapSpent(t *testing.T) {
	placeTypeScene(t)
	sum, ps, ks := rateRunAll(t, placeTypeStub(), rateDeciderJev, defaultRateMax, 10)
	if sum.Rated != 8 || ps.Calls != 2 || !ks.Ran || ks.Calls != 0 || ks.StopReason != scoring.StopSpendCap || ks.Due != 8 {
		t.Errorf("relevance %+v, place type %+v, business kind %+v; want the cap spent before the kind pass", sum, ps, ks)
	}
	if n := businessKindRows(t, `true`); n != 0 {
		t.Errorf("%d rows, want 0", n)
	}
}

// TestBusinessKindAndPlaceTypeShareTheHashRule: both passes, driven on the same two café leads,
// skip the unchanged one and ask about the changed one, and each stored hash is its own
// question's hash of the input it read.
func TestBusinessKindAndPlaceTypeShareTheHashRule(t *testing.T) {
	changed, unchanged := placeTypeScene(t)
	stub := placeTypeStub()
	rateRunAll(t, stub, rateDeciderJev, defaultRateMax, 1000)

	_, ps, ks := rateRunAll(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if ps.Calls != 0 || ks.Calls != 0 || ps.Unchanged != 2 || ks.Unchanged != 8 {
		t.Fatalf("nothing changed: place type %+v, business kind %+v", ps, ks)
	}

	setLeadKind(t, "lucerne.example", cafe.KindHasLoyalty)
	_, ps, ks = rateRunAll(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if ps.Calls != 1 || ps.Unchanged != 1 || ks.Calls != 1 || ks.Unchanged != 7 {
		t.Fatalf("one input changed: place type %+v, business kind %+v", ps, ks)
	}
	for _, id := range []int64{changed, unchanged} {
		var kindSHA, placeSHA []byte
		if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT
			(SELECT input_sha256 FROM lead_business_kind WHERE lead_id = $1 ORDER BY id DESC LIMIT 1),
			(SELECT input_sha256 FROM lead_place_type WHERE lead_id = $1 ORDER BY id DESC LIMIT 1)`, id).Scan(&kindSHA, &placeSHA); err != nil {
			t.Fatal(err)
		}
		in := businessKindInput(t, id)
		if !bytes.Equal(kindSHA, leadrate.BusinessKindHash(stub.Name(), in)) {
			t.Errorf("lead %d: the stored kind hash is not BusinessKindHash of its input", id)
		}
		if !bytes.Equal(placeSHA, leadrate.PlaceTypeHash(stub.Name(), in)) {
			t.Errorf("lead %d: the stored place hash is not PlaceTypeHash of its input", id)
		}
	}
}

// businessKindInput is the input the rating passes read for open lead id.
func businessKindInput(t *testing.T, id int64) leadrate.Input {
	t.Helper()
	for _, l := range placeTypeOpenLeads(t) {
		if l.LeadID == id {
			return rateInput(l, leadconfig.Config{})
		}
	}
	t.Fatalf("lead %d is not open", id)
	return leadrate.Input{}
}

// TestBusinessKindListMatchesTheCheck: every kind in leadrate.BusinessKinds is accepted by the
// migration's CHECK, a value off the list is refused, and the CHECK names nothing else.
func TestBusinessKindListMatchesTheCheck(t *testing.T) {
	ctx := context.Background()
	placeTypeScene(t)
	lead := leadIDOf(t, "woodgrove.example")
	lq := leadstoretest.Q(t)
	sha := bytes.Repeat([]byte{7}, 32)
	for _, kind := range leadrate.BusinessKinds() {
		if err := lq.SetBusinessKind(ctx, lead, kind, leadrate.BusinessKindQuestion, sha, 0.9); err != nil {
			t.Errorf("kind %q: %v", kind, err)
		}
	}
	err := lq.SetBusinessKind(ctx, lead, "kind_off_the_list", leadrate.BusinessKindQuestion, sha, 0.9)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Errorf("a kind off the list: %v, want 23514", err)
	}

	var def string
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'lead_business_kind_kind_check'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	var inCheck []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(def, -1) {
		inCheck = append(inCheck, m[1])
	}
	want := slices.Clone(leadrate.BusinessKinds())
	slices.Sort(inCheck)
	slices.Sort(want)
	if !slices.Equal(inCheck, want) {
		t.Errorf("the CHECK holds %v, the Go list %v", inCheck, want)
	}
}

// TestRateResultSumsThePasses: the run row counts every pass's calls and failures, names the first
// stop reason and notes each pass that ran.
func TestRateResultSumsThePasses(t *testing.T) {
	sum := rateSummary{Rated: 3, Calls: 3, Failures: 1}
	ps := placeSummary{Ran: true, Calls: 2, Failures: 1}
	ks := businessSummary{Ran: true, Named: 2, Unclear: 1, Unchanged: 4, Due: 5, Calls: 6, Failures: 3, StopReason: scoring.StopThrottled}
	r := rateResult(sum, ps, ks)
	if r.Requests != 11 || r.Failures != 5 || r.StopReason != scoring.StopThrottled {
		t.Errorf("%+v, want 11 requests, 5 failures, stopped throttled", r)
	}
	for _, want := range []string{"3 rated", "place type:", "business kind: 2 named, 1 unclear, 4 unchanged, 5 still due"} {
		if !strings.Contains(r.Note, want) {
			t.Errorf("note %q lacks %q", r.Note, want)
		}
	}
	if r := rateResult(sum, placeSummary{}, businessSummary{}); strings.Contains(r.Note, "business kind") || strings.Contains(r.Note, "place type") {
		t.Errorf("a pass that did not run is in the note %q", r.Note)
	}
	var line bytes.Buffer
	printBusinessKindSummary(&line, businessSummary{})
	if line.Len() != 0 {
		t.Errorf("a pass that did not run printed %q", line.String())
	}
	printBusinessKindSummary(&line, ks)
	if want := "  business kind: named 2 · unclear 1 · unchanged 4 · still due 5 · calls 6 · failed calls 3 · stop throttled\n"; line.String() != want {
		t.Errorf("line %q, want %q", line.String(), want)
	}
}

// TestBusinessKindReachesNoEmailOrModel: template rendering and the Claude writer's fact sheet and
// prompts are byte for byte the same with a lead_business_kind row on every lead as without one,
// so the table reaches neither a sent word nor a model that writes them.
func TestBusinessKindReachesNoEmailOrModel(t *testing.T) {
	ctx := context.Background()
	leadsScene(t)
	lq := leadstoretest.Q(t)
	leadID := leadIDOf(t, "northwind.example")

	cfg, err := leadconfig.Load(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := campaign.Load(filepath.Join(examplesDir, campaign.Dir, "drupal-eol", campaign.FileName))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := filePlan(examplesDir, spec, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// writer is what Claude reads about the lead: the sheet, the system prompt and the user turn.
	writer := func() []byte {
		t.Helper()
		c, err := leadCandidate(ctx, lq, leadID)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := lq.LeadEvidence(ctx, []int64{leadID})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		for _, phone := range []bool{false, true} {
			in, err := draftCtx{cfg: cfg, plan: plan, campaignID: 1}.writeInput(ctx, lq, plan, c, ev[leadID], phone)
			if err != nil {
				t.Fatal(err)
			}
			out.Write(in.Sheet.Canonical())
			out.WriteString(outreach.SystemPrompt(in))
			out.WriteString(outreach.UserTurn(in))
		}
		return out.Bytes()
	}
	// rendered is what the template path writes for a fresh batch, which it then discards, so the
	// leads are open again for the next call.
	rendered := func() string {
		t.Helper()
		mustRun(t, "leads", "batch", "new")
		id := dbCount(t, `SELECT max(id)::int FROM outreach_batch`)
		var out string
		if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT coalesce(string_agg(
			concat_ws(E'\x1f', lead_id, step, channel, subject, body_text, slots::text), E'\x1e' ORDER BY lead_id, step, id), '')
			FROM outreach_message WHERE batch_id = $1`, id).Scan(&out); err != nil {
			t.Fatal(err)
		}
		if out == "" {
			t.Fatal("the batch holds no messages, so nothing is compared")
		}
		mustRun(t, "leads", "batch", "discard", fmt.Sprint(id))
		return out
	}

	wantWriter, wantRendered := writer(), rendered()
	if len(wantWriter) == 0 {
		t.Fatal("the writer's input is empty, so nothing is compared")
	}
	open, err := lq.BusinessKindLeads(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) == 0 {
		t.Fatal("no open lead to give a kind to")
	}
	sha := bytes.Repeat([]byte{7}, 32)
	for _, o := range open {
		if err := lq.SetBusinessKind(ctx, o.LeadID, leadrate.BusinessKindEducation, leadrate.BusinessKindQuestion, sha, 0.9); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(writer(), wantWriter) {
		t.Error("a business-kind row changed the Claude writer's sheet or prompts")
	}
	if got := rendered(); got != wantRendered {
		t.Errorf("a business-kind row changed the rendered messages:\n got %s\nwant %s", got, wantRendered)
	}
}
