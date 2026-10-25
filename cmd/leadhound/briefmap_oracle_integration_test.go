//go:build integration

package main

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The contract-drift tests: the streamed ranking (rankPass and every caller of it) against
// the ranking as it was before, read through the test oracle (storetest.OracleRankInputs)
// and ranked by the reference copies below. A difference is a bug in the streamed path.

// driftJobs is how many jobs the drift fixture holds: well past every cap, small enough
// that the oracle reads it in a moment.
const driftJobs = 600

// hv is a deterministic pseudo-random integer in [0, mod) from the bigint expression x and
// salt, as the scale seed draws its attributes.
func hv(x string, salt, mod int) string {
	return fmt.Sprintf("((hashint8extended(%s, %d) & 9223372036854775807) %% %d)", x, salt, mod)
}

// driftFixture fills the emptied test database, as the owner, with driftJobs jobs drawn
// over every source the brief knows (ATS, fixture and LinkedIn), every listing state and
// status, closed observations, jobs observed twice (the later observation's gates are the
// current ones), every gate verdict with and without snippets, all-pass, one-fail, unclear,
// partial and fake-only decisions (older real decisions and newer fake ones on the same
// criteria), postings and applicant counts with ties and unknowns, jobs seen on dayStart's
// day and before, ATS jobs their board has not listed for longer than brief.BoardStaleDays
// (some an hour either side of the cutoff),
// LinkedIn listings due a recheck, and titles and descriptions a skip rule matches. Rules
// are left to the caller (setDriftRules).
func driftFixture(t *testing.T, dayStart time.Time) {
	t.Helper()
	commandDB(t)
	ctx := context.Background()
	conn := storetest.OwnerConn(t)
	stale := brief.BoardStaleDays + 2
	steps := []struct {
		name string
		sql  string
		args []any
	}{
		{"run", `INSERT INTO run (id, command, source, note) VALUES (1, 'collect', 'linkedin', 'seeded')`, nil},
		{"company", `INSERT INTO company (id, name, norm_name) SELECT i, 'Company ' || i, 'company ' || i FROM generate_series(1, 40) i`, nil},
		{"job", `WITH g AS (
			  SELECT i, ` + hv("i", 1, 10) + ` AS src, ` + hv("i", 2, 20) + ` AS st, ` + hv("i", 3, 10) + ` AS age,
			         ` + hv("i", 4, 10) + ` AS days, ` + hv("i", 5, 10) + ` AS seen, ` + hv("i", 18, 10) + ` AS chk
			  FROM generate_series(1, $3::bigint) i
			), s AS (
			  SELECT *, (ARRAY['linkedin', 'linkedin', 'linkedin', 'linkedin-fixture', 'greenhouse', 'lever', 'ashby-fixture',
			                   'smartrecruiters', 'teamtailor', 'personio'])[1 + src] AS source,
			         CASE WHEN age < 4 THEN $1::timestamptz + (i % 600) * interval '1 second'
			              ELSE $1::timestamptz - (1 + days) * interval '1 day' + (i % 600) * interval '1 second' END AS first_seen
			  FROM g
			)
			INSERT INTO job (id, source, source_job_id, company_id, title, url, first_seen_at, last_seen_at, listing_status, status_checked_at)
			SELECT i, source, 'j' || i, 1 + i % 40,
			       CASE WHEN i % 37 = 0 THEN 'Software Engineering Intern'
			            ELSE (ARRAY['Senior Go Engineer', 'Backend Engineer', 'Platform Engineer', 'Drupal Developer'])[1 + i % 4] END,
			       'https://jobs.example/' || source || '/' || i, first_seen,
			       CASE WHEN seen < 3 THEN $1::timestamptz - $4::int * interval '1 day'
			            WHEN seen = 3 THEN $1::timestamptz - $5::int * interval '1 day' - interval '1 hour'
			            WHEN seen = 4 THEN $1::timestamptz - $5::int * interval '1 day' + interval '1 hour'
			            ELSE $2::timestamptz END,
			       CASE WHEN st < 14 THEN 'open' WHEN st < 17 THEN 'expired' ELSE 'unreachable' END,
			       CASE WHEN chk < 5 THEN $2::timestamptz - interval '3 days' ELSE $2::timestamptz END
			FROM s`, []any{dayStart, time.Now(), driftJobs, stale, brief.BoardStaleDays}},
		{"http_fetch", `INSERT INTO http_fetch (id, run_id, source, kind, url, status, fetched_at)
			SELECT id, 1, source, 'detail', url, 200, now() - interval '3 days' FROM job`, nil},
		// The first observation of every job; a second one, later, for a fifth of them,
		// half of those observed again on the day.
		{"observation", `INSERT INTO job_observation (id, job_id, fetch_id, observed_at, derived_by, title, location_text,
			  description_clean, apply_type, closed, posted_at_est, applicant_count)
			SELECT o.id, j.id, j.id, o.at, 'seeded@1', j.title, 'Remote, Europe',
			       md5(o.id::text) || CASE WHEN j.id % 23 = 0 THEN ' requires security clearance' ELSE ' writes Go and Postgres' END,
			       (ARRAY['', 'offsite', 'onsite', 'simple', 'ats'])[1 + ` + hv("o.id", 7, 5) + `],
			       ` + hv("o.id", 8, 25) + ` = 0,
			       CASE WHEN ` + hv("o.id", 9, 4) + ` = 0 THEN NULL ELSE $1::timestamptz - ` + hv("o.id", 10, 4) + ` * interval '1 day' END,
			       CASE WHEN ` + hv("o.id", 11, 3) + ` = 0 THEN NULL ELSE ` + hv("o.id", 12, 4) + `::int * 10 END
			FROM job j
			CROSS JOIN LATERAL (
			  SELECT j.id AS id, j.first_seen_at + interval '1 minute' AS at
			  UNION ALL
			  SELECT 10000 + j.id, CASE WHEN ` + hv("j.id", 19, 2) + ` = 0 THEN greatest(j.first_seen_at + interval '2 hours', $1::timestamptz + (j.id % 300) * interval '1 minute')
			                            ELSE j.first_seen_at + interval '2 hours' END
			  WHERE ` + hv("j.id", 6, 5) + ` = 0) o`, []any{dayStart}},
		{"gate_result", `INSERT INTO gate_result (job_id, observation_id, gate, verdict, reason, snippet, derived_by)
			SELECT o.job_id, o.id, g.gate,
			       CASE WHEN h.v < 6 THEN 'fail' WHEN h.v < 22 THEN 'flag' ELSE 'pass' END,
			       g.gate || ' seeded',
			       CASE WHEN h.v >= 22 THEN '' WHEN h.v % 3 = 0 THEN 'mail jobs@example.invalid' WHEN h.v % 3 = 1 THEN g.gate || ' words' ELSE '' END,
			       'gates@1'
			FROM job_observation o
			CROSS JOIN (VALUES (1, 'language'), (2, 'location'), (3, 'workplace'), (4, 'work_rights'), (5, 'rule')) g(ord, gate)
			CROSS JOIN LATERAL (SELECT ` + hv("o.id * 10 + g.ord", 13, 100) + ` AS v, ` + hv("o.id * 10 + g.ord", 14, 10) + ` AS keep) h
			WHERE h.keep < 8
			ORDER BY o.id, g.ord`, nil},
		// Decisions on the first observation: per job a pattern (all pass, one fail, one
		// unclear, mixed, three criteria only), an older real decision of the other verdict on
		// some, a newer fake all-pass on some, and fake-only jobs.
		{"decision", `WITH c AS (
			  SELECT j.id AS job, ` + hv("j.id", 15, 10) + ` AS who, ` + hv("j.id", 16, 8) + ` AS pat,
			         ` + hv("j.id", 17, 5) + ` AS one, ` + hv("j.id", 20, 4) + ` AS extra
			  FROM job j
			), v AS (
			  SELECT c.*, cr.ord, cr.name,
			         CASE WHEN c.pat < 3 THEN 'pass'
			              WHEN c.pat < 5 THEN CASE WHEN cr.ord = c.one THEN 'fail' ELSE 'pass' END
			              WHEN c.pat = 5 THEN CASE WHEN cr.ord = c.one THEN 'unclear' ELSE 'pass' END
			              ELSE (ARRAY['pass', 'fail', 'unclear'])[1 + ` + hv("c.job * 10 + cr.ord", 21, 3) + `] END AS verdict
			  FROM c CROSS JOIN (VALUES (0, 'title'), (1, 'stack'), (2, 'seniority'), (3, 'location'), (4, 'work_rights')) cr(ord, name)
			  WHERE c.pat <> 7 OR cr.ord < 3
			), d AS (
			  SELECT job, name, CASE WHEN who = 8 THEN $1 ELSE $2 END AS decider, verdict, now() - interval '1 day' AS at FROM v WHERE who < 9
			  UNION ALL
			  SELECT job, name, $2, CASE WHEN verdict = 'pass' THEN 'fail' ELSE 'pass' END, now() - interval '2 days' FROM v WHERE who < 8 AND extra = 1
			  UNION ALL
			  SELECT job, name, $1, 'pass', now() - interval '1 hour' FROM v WHERE who < 8 AND extra = 2
			)
			INSERT INTO decision (job_id, observation_id, decider, criterion, verdict, confidence, probabilities, reason, input_sha256, created_at)
			SELECT job, job, decider, name, verdict, 0.9, '{"pass": 0.9}'::jsonb, name || ' by ' || decider, '\x00'::bytea, at
			FROM d ORDER BY job, name, at`, []any{ai.FakeName, config.DefaultJevModel}},
		{"status new", `INSERT INTO status_event (job_id, status, actor, at) SELECT id, 'new', 'system', first_seen_at FROM job`, nil},
		{"status later", `INSERT INTO status_event (job_id, status, actor, at)
			SELECT j.id, x.status, $1, j.first_seen_at + x.n * interval '1 hour'
			FROM job j
			CROSS JOIN LATERAL (SELECT ` + hv("j.id", 22, 20) + ` AS k) h
			JOIN LATERAL (VALUES (0, 'shortlisted', 1), (1, 'applied', 1), (2, 'skipped', 1), (3, 'shortlisted', 1),
			                     (3, 'interviewing', 2), (4, 'rejected', 1), (5, 'shortlisted', 1)) x(k, status, n) ON x.k = h.k`,
			[]any{leadstore.ActorGremy}},
	}
	for _, s := range steps {
		if _, err := conn.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("drift fixture %s: %v", s.name, err)
		}
	}
	for _, tbl := range []string{"run", "http_fetch", "company", "job", "job_observation"} {
		if _, err := conn.Exec(ctx, fmt.Sprintf("SELECT setval(pg_get_serial_sequence('%s', 'id'), (SELECT max(id) FROM %s))", tbl, tbl)); err != nil {
			t.Fatal(err)
		}
	}
}

// setDriftRules replaces the rules: an active title rule and a disabled one always, and an
// active description rule when desc.
func setDriftRules(t *testing.T, desc bool) {
	t.Helper()
	conn := storetest.OwnerConn(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `DELETE FROM rule`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO rule (kind, field, pattern, reason, disabled_at) VALUES
		('skip', 'title', '\bintern\b', 'seeded', NULL), ('skip', 'title', 'drupal', 'seeded', now())`); err != nil {
		t.Fatal(err)
	}
	if desc {
		if _, err := conn.Exec(ctx, `INSERT INTO rule (kind, field, pattern, reason) VALUES ('skip', 'description', 'security clearance', 'seeded')`); err != nil {
			t.Fatal(err)
		}
	}
}

// refOpenJobsNotSeenSince is store.OpenJobsNotSeenSince as it was, its SQL verbatim.
func refOpenJobsNotSeenSince(t *testing.T, s *store.Store, sources []string, before time.Time) map[int64]string {
	t.Helper()
	out := map[int64]string{}
	rows, err := s.Pool().Query(context.Background(),
		`SELECT id, source FROM job
		 WHERE source = ANY($1) AND listing_status = 'open' AND last_seen_at < $2`, sources, before)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id     int64
		source string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.source)
		return x, err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		out[x.id] = x.source
	}
	return out
}

// refBriefCandidates is briefCandidates as it was, read through the oracle.
func refBriefCandidates(t *testing.T, s *store.Store, loc *time.Location, day time.Time, allowFake bool) (cands, notSeen []scoring.Candidate, notSeenSrc map[int64]string) {
	t.Helper()
	ctx := context.Background()
	ins, err := storetest.OracleRankInputs(ctx, s, allowFake)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := s.Q().ActiveRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	y, m, d := day.In(loc).Date()
	cutoff := time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, -brief.BoardStaleDays)
	notSeenSrc = refOpenJobsNotSeenSince(t, s, atsSources(), cutoff)
	for _, c := range scoring.CandidatesFrom(ins, rules) {
		if _, ok := notSeenSrc[c.JobID]; ok {
			notSeen = append(notSeen, c)
		} else {
			cands = append(cands, c)
		}
	}
	return cands, notSeen, notSeenSrc
}

// refNotSeenBySource is notSeenBySource as it was.
func refNotSeenBySource(notSeen []scoring.Candidate, src map[int64]string, loc *time.Location) map[string]int {
	all := len(notSeen)
	r := scoring.Rank(notSeen, loc, scoring.Caps{Apply: all, YourCall: all})
	out := map[string]int{}
	for _, rk := range append(r.Apply, r.YourCall...) {
		out[src[rk.JobID]]++
	}
	return out
}

// refBriefParts is what the brief built from the ranking as it was: the parts of
// brief.Input the ranking decides, and every source's not-seen count.
func refBriefParts(t *testing.T, s *store.Store, prof candidate.Config, loc *time.Location, day time.Time, allowFake bool) (brief.Input, map[string]int) {
	t.Helper()
	cands, notSeen, notSeenSrc := refBriefCandidates(t, s, loc, day, allowFake)
	notSeenN := refNotSeenBySource(notSeen, notSeenSrc, loc)
	r := scoring.Rank(cands, loc, briefCaps(prof))
	var ids []int64
	for _, rk := range append(append([]scoring.Ranked{}, r.Apply...), r.YourCall...) {
		ids = append(ids, rk.JobID)
	}
	if r.RunnerUp != nil {
		ids = append(ids, r.RunnerUp.JobID)
	}
	sums, err := s.Q().JobSummaries(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	kws := prof.Candidate.EvidenceKeywords
	y, m, d := day.In(loc).Date()
	in := brief.Input{
		Day:         time.Date(y, m, d, 0, 0, 0, 0, loc),
		Unreachable: r.Skipped[scoring.SkipUnreachable],
		Unscored:    r.Skipped[scoring.SkipUnscored],
	}
	in.Skipped = scoring.SkippedOn(cands, loc, in.Day)
	for _, rk := range r.Apply {
		in.Apply = append(in.Apply, briefJob(rk.Candidate, sums[rk.JobID].URL, kws))
	}
	for _, rk := range r.YourCall {
		in.YourCall = append(in.YourCall, briefJob(rk.Candidate, sums[rk.JobID].URL, kws))
	}
	if ru := r.RunnerUp; ru != nil {
		in.RunnerUp = &brief.RunnerUp{
			Job:             briefJob(ru.Candidate, sums[ru.JobID].URL, kws),
			FailedCriterion: ru.FailedCriterion,
			Note:            strings.Join(matchedKeywords(ru.Description, kws[ru.FailedCriterion]), ", "),
		}
	}
	return in, notSeenN
}

// driftProfile is the demo profile with the given caps.
func driftProfile(t *testing.T, apply, yourCall int) candidate.Config {
	t.Helper()
	prof, err := loadProfileDir(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	prof.Brief.MaxApply, prof.Brief.MaxYourCall = apply, yourCall
	return prof
}

func driftStore(t *testing.T) (*store.Store, *time.Location) {
	t.Helper()
	loc, err := time.LoadLocation(config.DefaultTZ)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, loc
}

func dayStartOf(day time.Time, loc *time.Location) time.Time {
	y, m, d := day.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// driftCases are the variants every drift test runs: with and without the description
// rule, the fake decider's decisions read or not.
var driftCases = []struct{ desc, allowFake bool }{{false, false}, {false, true}, {true, false}, {true, true}}

// TestOracleBriefDrift: buildInput gives exactly the brief the old read-everything path
// gave (tiers, runner-up, the day's skips, unscored, unreachable and every source's
// not-seen count), and rankPass keeps exactly the jobs Rank kept, every field filled.
func TestOracleBriefDrift(t *testing.T) {
	s, loc := driftStore(t)
	day := time.Now()
	driftFixture(t, dayStartOf(day, loc))
	ctx := context.Background()
	for _, caps := range [][2]int{{5, 5}, {2, 3}, {50, 50}} {
		prof := driftProfile(t, caps[0], caps[1])
		for _, c := range driftCases {
			name := fmt.Sprintf("caps %v desc %v fake %v", caps, c.desc, c.allowFake)
			setDriftRules(t, c.desc)
			want, wantNotSeen := refBriefParts(t, s, prof, loc, day, c.allowFake)
			got, err := buildInput(ctx, s.Q(), prof, loc, day, c.allowFake, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(want.Apply)+len(want.YourCall) == 0 || want.RunnerUp == nil || len(wantNotSeen) == 0 {
				t.Fatalf("%s: the fixture lists nothing, has no runner-up or nothing not seen; the test proves nothing", name)
			}
			if caps[0] < 50 && len(want.Apply) != caps[0] {
				t.Fatalf("%s: %d in Apply, fewer than the cap; the test proves nothing", name, len(want.Apply))
			}
			for _, f := range []struct {
				name      string
				got, want any
			}{
				{"Apply", got.Apply, want.Apply}, {"YourCall", got.YourCall, want.YourCall}, {"RunnerUp", got.RunnerUp, want.RunnerUp},
				{"Skipped", got.Skipped, want.Skipped}, {"Unscored", got.Unscored, want.Unscored},
				{"Unreachable", got.Unreachable, want.Unreachable}, {"Day", got.Day, want.Day},
			} {
				if !reflect.DeepEqual(f.got, f.want) {
					t.Errorf("%s: %s = %+v\nwant %+v", name, f.name, f.got, f.want)
				}
			}
			gotNotSeen := map[string]int{}
			for _, src := range got.Sources {
				if src.NotSeen > 0 {
					gotNotSeen[strings.ToLower(src.Name)] = src.NotSeen
				}
			}
			wantNamed := map[string]int{}
			for _, bs := range briefSources {
				if n := wantNotSeen[bs.source]; n > 0 {
					wantNamed[strings.ToLower(bs.name)] = n
				}
			}
			if !reflect.DeepEqual(gotNotSeen, wantNamed) {
				t.Errorf("%s: not seen = %v, want %v", name, gotNotSeen, wantNamed)
			}

			// The ranking itself, hydrated, against Rank on the oracle's candidates.
			cands, _, _ := refBriefCandidates(t, s, loc, day, c.allowFake)
			ref := scoring.Rank(cands, loc, briefCaps(prof))
			for _, mode := range []store.DescMode{store.DescWhereRanked, store.DescAll} {
				pass, err := rankPass(ctx, s.Q(), loc, day, c.allowFake, rankOptions{caps: briefCaps(prof), desc: mode, hydrate: true})
				if err != nil {
					t.Fatal(err)
				}
				r := pass.ranking
				if !reflect.DeepEqual(r.Apply, ref.Apply) || !reflect.DeepEqual(r.YourCall, ref.YourCall) || !reflect.DeepEqual(r.RunnerUp, ref.RunnerUp) ||
					r.ApplyTotal != ref.ApplyTotal || r.YourCallTotal != ref.YourCallTotal {
					t.Errorf("%s, mode %d: the kept ranking differs from Rank's", name, mode)
				}
				// Every skip count is exact when every description is read; reading them only
				// where the brief needs them may move a job with a failing gate between the
				// rule and its gate, never anything the brief prints.
				skips := nonZero(r.Skipped)
				want := nonZero(ref.Skipped)
				if mode == store.DescWhereRanked {
					for _, k := range []string{scoring.SkipRule, scoring.SkipLanguage, scoring.SkipLocation, scoring.SkipWorkplace, scoring.SkipWorkRights} {
						delete(skips, k)
						delete(want, k)
					}
				}
				if !reflect.DeepEqual(skips, want) {
					t.Errorf("%s, mode %d: skipped = %v, want %v", name, mode, r.Skipped, ref.Skipped)
				}
			}
		}
	}
}

// nonZero copies m without its zero counts.
func nonZero(m map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range m {
		if v != 0 {
			out[k] = v
		}
	}
	return out
}

// TestOracleRecheckIDsDrift: the jobs the recheck looks at are the ones the old path chose,
// in the same order.
func TestOracleRecheckIDsDrift(t *testing.T) {
	s, loc := driftStore(t)
	day := time.Now()
	driftFixture(t, dayStartOf(day, loc))
	prof := driftProfile(t, 3, 2)
	for _, c := range driftCases {
		setDriftRules(t, c.desc)
		cands, _, _ := refBriefCandidates(t, s, loc, day, c.allowFake)
		unreachable := 0
		for i := range cands {
			if cands[i].ListingStatus == store.ListingUnreachable {
				cands[i].ListingStatus = store.ListingOpen
				unreachable++
			}
		}
		caps := briefCaps(prof)
		r := scoring.Rank(cands, loc, scoring.Caps{Apply: caps.Apply * (1 + recheckSlack), YourCall: caps.YourCall * (1 + recheckSlack)})
		var want []int64
		for _, rk := range append(r.Apply, r.YourCall...) {
			want = append(want, rk.JobID)
		}
		got, err := recheckIDs(context.Background(), s.Q(), prof, loc, day, c.allowFake)
		if err != nil {
			t.Fatal(err)
		}
		if unreachable == 0 || len(want) == 0 {
			t.Fatal("no unreachable job or nothing to recheck; the test proves nothing")
		}
		if !slices.Equal(got, want) {
			t.Errorf("desc %v fake %v: recheckIDs = %v, want %v", c.desc, c.allowFake, got, want)
		}
	}
}

// listing is a job's listing status and when it was last checked.
type listing struct {
	status  string
	checked *time.Time
}

// listings reads every job's listing.
func listings(t *testing.T) map[int64]listing {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(), `SELECT id, listing_status, status_checked_at FROM job`)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]listing{}
	var id int64
	var l listing
	if _, err := pgx.ForEachRow(rows, []any{&id, &l.status, &l.checked}, func() error { out[id] = l; return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

// markedSince is every job whose listing status changed since before, in id order.
func markedSince(t *testing.T, before map[int64]listing) []int64 {
	t.Helper()
	var out []int64
	for id, l := range listings(t) {
		if before[id].status != l.status {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// restore puts the given jobs' listings back as they were in before (marking one also
// stamps its check time).
func restore(t *testing.T, before map[int64]listing, ids []int64) {
	t.Helper()
	for _, id := range ids {
		ownerExecCmd(t, `UPDATE job SET listing_status = $2, status_checked_at = $3 WHERE id = $1`, id, before[id].status, before[id].checked)
	}
}

// TestOracleLeaveOutStaleDrift: leaveOutStale marks exactly the jobs the old loop marked,
// over several rounds, and leaves the same ranking.
func TestOracleLeaveOutStaleDrift(t *testing.T) {
	s, loc := driftStore(t)
	day := time.Now()
	driftFixture(t, dayStartOf(day, loc))
	prof := driftProfile(t, 10, 10)
	// Most LinkedIn listings are due a recheck, so the loop runs several rounds.
	ownerExecCmd(t, `UPDATE job SET status_checked_at = now() - interval '3 days' WHERE source = 'linkedin' AND id % 4 <> 0`)
	ctx := context.Background()
	q := s.Q()
	listed := func(r scoring.Ranking) []int64 {
		var ids []int64
		for _, rk := range append(append([]scoring.Ranked{}, r.Apply...), r.YourCall...) {
			ids = append(ids, rk.JobID)
		}
		if r.RunnerUp != nil {
			ids = append(ids, r.RunnerUp.JobID)
		}
		return ids
	}
	for _, c := range driftCases {
		setDriftRules(t, c.desc)
		before := listings(t)

		// The old loop, ranked through the oracle.
		rounds := 0
		for {
			cands, _, _ := refBriefCandidates(t, s, loc, day, c.allowFake)
			due, err := q.RecheckTargets(ctx, linkedin.Source, listed(scoring.Rank(cands, loc, briefCaps(prof))), time.Now().Add(-linkedin.RecheckAfter))
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, tg := range due {
				if tg.ListingStatus != store.ListingOpen {
					continue
				}
				if err := q.SetListingStatus(ctx, tg.JobID, store.ListingUnreachable); err != nil {
					t.Fatal(err)
				}
				n++
			}
			if n == 0 {
				break
			}
			rounds++
		}
		wantMarked := markedSince(t, before)
		cands, _, _ := refBriefCandidates(t, s, loc, day, c.allowFake)
		wantFinal := listed(scoring.Rank(cands, loc, briefCaps(prof)))
		if rounds < 2 || len(wantMarked) < 3 {
			t.Fatalf("desc %v fake %v: %d rounds marked %d (listed %v); the test proves too little", c.desc, c.allowFake, rounds, len(wantMarked), wantFinal)
		}

		// Back to where it was, then the streamed path.
		restore(t, before, wantMarked)
		n, err := leaveOutStale(ctx, q, prof, loc, day, c.allowFake, linkedin.Source)
		if err != nil {
			t.Fatal(err)
		}
		gotMarked := markedSince(t, before)
		r, err := rankNow(ctx, q, prof, loc, day, c.allowFake)
		if err != nil {
			t.Fatal(err)
		}
		if n != len(wantMarked) || !slices.Equal(gotMarked, wantMarked) || !slices.Equal(listed(r), wantFinal) {
			t.Errorf("desc %v fake %v: marked %d %v, listed %v; want %v, %v", c.desc, c.allowFake, n, gotMarked, listed(r), wantMarked, wantFinal)
		}
		restore(t, before, gotMarked)
	}
}

// TestOracleScoreSummaryDrift: the score run's summary (every job ranked, none set apart)
// prints exactly what the old path printed.
func TestOracleScoreSummaryDrift(t *testing.T) {
	s, loc := driftStore(t)
	driftFixture(t, dayStartOf(time.Now(), loc))
	prof := driftProfile(t, 5, 5)
	ctx := context.Background()
	line := func(r scoring.Ranking) string {
		l := fmt.Sprintf("ranked: apply %d · your call %d · skipped %s", r.ApplyTotal, r.YourCallTotal, skippedSummary(r.Skipped))
		if r.RunnerUp != nil {
			l += fmt.Sprintf(" · runner-up: job %d (failed %s)", r.RunnerUp.JobID, r.RunnerUp.FailedCriterion)
		}
		return l
	}
	for _, c := range driftCases {
		setDriftRules(t, c.desc)
		ins, err := storetest.OracleRankInputs(ctx, s, c.allowFake)
		if err != nil {
			t.Fatal(err)
		}
		rules, err := s.Q().ActiveRules(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := scoring.Rank(scoring.CandidatesFrom(ins, rules), loc, briefCaps(prof))
		got, err := scoreSummary(ctx, s, prof, loc, c.allowFake)
		if err != nil {
			t.Fatal(err)
		}
		if line(got) != line(want) || want.Skipped[scoring.SkipRule] == 0 {
			t.Errorf("desc %v fake %v:\n got %s\nwant %s", c.desc, c.allowFake, line(got), line(want))
		}
	}
}
