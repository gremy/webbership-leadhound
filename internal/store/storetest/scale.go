package storetest

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// The scale database is a separate local database the size of production, seeded once and
// reused by every scale test (go test -tags 'integration scale' -run Scale, make test-scale).
const (
	defaultScaleAppDSN     = "postgres://leadhound_app@localhost/leadhound_scale?sslmode=disable"
	defaultScaleMigrateDSN = "postgres://leadhound_owner@localhost/leadhound_scale?sslmode=disable"
	defaultScaleJobs       = 450000
	minScaleJobs           = 1000

	// scaleVersion names the seed's shape: a change to what the seed writes bumps it, so
	// every scale database seeded before is seeded again.
	scaleVersion = "v1"
	markerPrefix = "leadhound-scale " + scaleVersion

	// scaleLockKey is the advisory lock two processes take before checking or seeding the
	// scale database, so they never seed it at the same time.
	scaleLockKey = 7311041901
)

// ScaleDSN is the app role's DSN for the scale database: TEST_SCALE_DSN, or the local
// default.
func ScaleDSN() string { return envOr("TEST_SCALE_DSN", defaultScaleAppDSN) }

// ScaleMigrateDSN is the owner role's DSN for the scale database: TEST_SCALE_MIGRATE_DSN, or
// the local default.
func ScaleMigrateDSN() string { return envOr("TEST_SCALE_MIGRATE_DSN", defaultScaleMigrateDSN) }

// ScaleJobs is how many jobs the scale seed holds: LEADHOUND_SCALE_JOBS, or 450,000 (a
// wide-net night and then some). Below 1,000 the seed's shape no longer holds, so a smaller
// value, or one that is not a number, gives 1,000.
func ScaleJobs() int {
	n, err := strconv.Atoi(envOr("LEADHOUND_SCALE_JOBS", strconv.Itoa(defaultScaleJobs)))
	if err != nil || n < minScaleJobs {
		return minScaleJobs
	}
	return n
}

// ScaleInfo describes the seeded scale database. Day is the seed's own "today", fixed when
// it was seeded: a scale test reads its dates from here and never from the wall clock, so a
// seed made yesterday is still right today.
type ScaleInfo struct {
	Jobs    int
	Loc     *time.Location // the zone the seed's days are in (config.DefaultTZ)
	Day     time.Time      // midnight of the seeded "today" in Loc
	WideDay time.Time      // midnight of the day before: the wide-net night
	// Leads is the marker's leads part: the leads seed's own version, '' while the lead
	// tables are not seeded (leadstoretest owns it and writes it with WriteScaleMarker).
	Leads string

	// Known jobs, each open, new, current and matching no stored rule.
	AllPassJobID   int64 // every criterion decided pass by the real decider; not in the sent brief
	RunnerUpJobID  int64 // exactly one criterion decided fail, the others pass
	SeenTodayJobID int64 // an ATS job first seen on Day
	NotSeenJobID   int64 // an open ATS job its board has not listed for brief.BoardStaleDays
}

// Marker is the text the scale database's comment holds for info.
func (info ScaleInfo) Marker() string {
	return fmt.Sprintf("%s;jobs=%d;day=%s;leads=%s", markerPrefix, info.Jobs, info.Day.Format(time.DateOnly), info.Leads)
}

var (
	scaleMu   sync.Mutex
	scaleInfo *ScaleInfo
)

// Seeded returns the scale database, migrated (core and leads) and seeded with ScaleJobs
// jobs. A database whose marker names this seed version and job count is reused as it is;
// any other is emptied (every table but goose's, as ResetDB) and seeded afresh as the owner,
// then analysed, and the marker is written last, so a seed that failed part way is redone
// on the next call. When the database is unavailable it skips, or fails under
// TEST_DB_REQUIRED=1.
func Seeded(t testing.TB) ScaleInfo {
	t.Helper()
	scaleMu.Lock()
	defer scaleMu.Unlock()
	if scaleInfo != nil {
		return *scaleInfo
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, ScaleMigrateDSN()); err != nil {
		SkipOrFail(t, "skipping scale test: scale database unavailable: %v", err)
	}
	if err := leadstore.Migrate(ctx, ScaleMigrateDSN()); err != nil {
		t.Fatalf("storetest: scale: leads migrate: %v", err)
	}
	conn := connect(t, ScaleMigrateDSN())
	info, err := seedScale(ctx, conn, ScaleJobs())
	if err != nil {
		t.Fatalf("storetest: scale: %v", err)
	}
	scaleInfo = &info
	return info
}

// WriteScaleMarker writes info's marker as the scale database's comment, as the owner. The
// leads seed calls it once its own tables are seeded, with Leads set.
func WriteScaleMarker(t testing.TB, info ScaleInfo) {
	t.Helper()
	conn := connect(t, ScaleMigrateDSN())
	if err := writeMarker(context.Background(), conn, info.Marker()); err != nil {
		t.Fatalf("storetest: scale: %v", err)
	}
	scaleMu.Lock()
	if scaleInfo != nil {
		scaleInfo.Leads = info.Leads
	}
	scaleMu.Unlock()
}

func seedScale(ctx context.Context, conn *pgx.Conn, jobs int) (ScaleInfo, error) {
	loc, err := time.LoadLocation(config.DefaultTZ)
	if err != nil {
		return ScaleInfo{}, fmt.Errorf("zone: %w", err)
	}
	// The seed's days are local days, so day arithmetic in SQL follows the zone's clock.
	if _, err := conn.Exec(ctx, "SET TIME ZONE '"+config.DefaultTZ+"'"); err != nil {
		return ScaleInfo{}, fmt.Errorf("set time zone: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(scaleLockKey)); err != nil {
		return ScaleInfo{}, fmt.Errorf("lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(scaleLockKey)) }()

	var comment string
	if err := conn.QueryRow(ctx,
		`SELECT coalesce(shobj_description(oid, 'pg_database'), '') FROM pg_database
		 WHERE datname = current_database()`).Scan(&comment); err != nil {
		return ScaleInfo{}, fmt.Errorf("read marker: %w", err)
	}
	info, ok := parseMarker(comment, loc)
	if !ok || info.Jobs != jobs {
		now := time.Now().In(loc)
		info = ScaleInfo{Jobs: jobs, Loc: loc, Day: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)}
		info.WideDay = info.Day.AddDate(0, 0, -1)
		if err := seed(ctx, conn, info); err != nil {
			return ScaleInfo{}, err
		}
		if err := writeMarker(ctx, conn, info.Marker()); err != nil {
			return ScaleInfo{}, err
		}
	}
	if err := knownJobs(ctx, conn, &info); err != nil {
		return ScaleInfo{}, err
	}
	return info, nil
}

// parseMarker reads a marker part by part; ok is false unless it names this seed version
// and carries a job count and a day.
func parseMarker(s string, loc *time.Location) (ScaleInfo, bool) {
	parts := strings.Split(s, ";")
	if parts[0] != markerPrefix {
		return ScaleInfo{}, false
	}
	info := ScaleInfo{Loc: loc}
	for _, p := range parts[1:] {
		k, v, _ := strings.Cut(p, "=")
		switch k {
		case "jobs":
			n, err := strconv.Atoi(v)
			if err != nil {
				return ScaleInfo{}, false
			}
			info.Jobs = n
		case "day":
			d, err := time.ParseInLocation(time.DateOnly, v, loc)
			if err != nil {
				return ScaleInfo{}, false
			}
			info.Day = d
		case "leads":
			info.Leads = v
		}
	}
	if info.Jobs == 0 || info.Day.IsZero() {
		return ScaleInfo{}, false
	}
	info.WideDay = info.Day.AddDate(0, 0, -1)
	return info, true
}

func writeMarker(ctx context.Context, conn *pgx.Conn, marker string) error {
	var db string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}
	// COMMENT takes no parameters; the name is quoted as an identifier and the marker,
	// built here from digits, letters and punctuation, as a literal.
	q := fmt.Sprintf("COMMENT ON DATABASE %s IS '%s'", pgx.Identifier{db}.Sanitize(), strings.ReplaceAll(marker, "'", "''"))
	if _, err := conn.Exec(ctx, q); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}
	return nil
}

// rnd is a deterministic pseudo-random integer in [0, mod) from the bigint expression x and
// salt, so each attribute of a row is drawn independently and every seed is the same.
func rnd(x string, salt, mod int) string {
	return fmt.Sprintf("((hashint8extended(%s, %d) & 9223372036854775807) %% %d)", x, salt, mod)
}

// seed empties the scale database and fills it, set-based, one statement (one transaction)
// per table, shaped like production after a wide-net night. $1 is Day, $2 WideDay, $3 the
// job count wherever a statement takes them.
func seed(ctx context.Context, conn *pgx.Conn, info ScaleInfo) error {
	n := info.Jobs
	stride := n / 301 // spreads the few hundred later status events over the whole id range
	detail := "'" + strings.Join(store.DetailSources, "', '") + "'"
	steps := []struct {
		name string
		sql  string
		args []any
	}{
		{"run", `INSERT INTO run (id, command, source, started_at, finished_at, requests, note)
			SELECT i, (ARRAY['collect', 'score', 'brief'])[1 + i % 3],
			       CASE i % 3 WHEN 0 THEN 'greenhouse' ELSE '' END,
			       $1::timestamptz - (i / 3) * interval '1 day' + interval '1 hour',
			       $1::timestamptz - (i / 3) * interval '1 day' + interval '90 minutes',
			       100, 'seeded'
			FROM generate_series(1, 30) i`, []any{info.Day}},
		// Board answers keep only their size and hash; details and LinkedIn pages keep a
		// ~40 KB body of incompressible text, as large pages do.
		{"http_fetch", `WITH b AS (
			  SELECT convert_to(string_agg(md5(k::text), '' ORDER BY k), 'UTF8') AS body
			  FROM generate_series(1, 1250) k)
			INSERT INTO http_fetch (id, run_id, source, kind, url, status, fetched_at, body, content_type, body_sha256, body_size)
			SELECT i, 1 + i % 30,
			       CASE WHEN i <= 20000 THEN (ARRAY['smartrecruiters', 'greenhouse', 'lever', 'ashby', 'teamtailor', 'personio'])[1 + i % 6]
			            WHEN i <= 25000 THEN 'smartrecruiters' ELSE 'linkedin' END,
			       CASE WHEN i <= 20000 THEN 'board' WHEN i <= 25000 THEN 'detail' WHEN i <= 26000 THEN 'search' ELSE 'detail' END,
			       'https://jobs.example/fetch/' || i, 200,
			       $1::timestamptz + (i % 86400) * interval '1 second',
			       CASE WHEN i <= 20000 THEN ''::bytea ELSE b.body END,
			       'application/json', sha256(b.body), octet_length(b.body)
			FROM generate_series(1, 27000) i, b`, []any{info.WideDay}},
		{"company", `INSERT INTO company (id, name, norm_name, created_at)
			SELECT i, 'Company ' || i, 'company ' || i, $1::timestamptz FROM generate_series(1, 15000) i`, []any{info.WideDay}},
		// Sources in production's proportions; 80% first seen on the wide-net night, 12% on
		// Day, 8% over the 3-12 days before, five eighths of those (when open, from an ATS)
		// not listed on their board since. 90% open, 8% expired, 2% unreachable. Every
		// 1500th title matches the active title rule.
		{"job", `WITH g AS (
			  SELECT i, ` + rnd("i", 1, 1000) + ` AS src, ` + rnd("i", 2, 100) + ` AS age,
			         ` + rnd("i", 3, 100) + ` AS st, ` + rnd("i", 4, 19000) + ` AS board,
			         ` + rnd("i", 5, 86400) + ` AS sec
			  FROM generate_series(1, $3::bigint) i
			), s AS (
			  SELECT i, age, board,
			         CASE WHEN src < 400 THEN 'smartrecruiters' WHEN src < 710 THEN 'greenhouse'
			              WHEN src < 820 THEN 'lever' WHEN src < 910 THEN 'ashby'
			              WHEN src < 970 THEN 'teamtailor' WHEN src < 998 THEN 'personio' ELSE 'linkedin' END AS source,
			         CASE WHEN st < 90 THEN 'open' WHEN st < 98 THEN 'expired' ELSE 'unreachable' END AS status,
			         CASE WHEN age < 80 THEN $2::timestamptz + sec * interval '1 second'
			              WHEN age < 92 THEN $1::timestamptz + (sec % 21600) * interval '1 second'
			              ELSE $1::timestamptz - (3 + age % 10) * interval '1 day' + sec * interval '1 second' END AS first_seen
			  FROM g
			)
			INSERT INTO job (id, source, source_job_id, company_id, title, url, first_seen_at, last_seen_at, listing_status, status_checked_at)
			SELECT i, source,
			       CASE WHEN source = 'linkedin' THEN i::text ELSE 'b' || board || '/' || i END,
			       1 + ` + rnd("i", 6, 15000) + `,
			       CASE WHEN i % 1500 = 0 THEN 'Software Engineering Intern'
			            ELSE (ARRAY['Senior Go Engineer', 'Backend Engineer', 'Platform Engineer', 'Drupal Developer',
			                        'Full Stack Developer', 'Site Reliability Engineer', 'Data Engineer', 'Frontend Developer',
			                        'Engineering Manager', 'Product Designer', 'Sales Manager', 'Customer Success Manager'])[1 + ` + rnd("i", 7, 12) + `] END,
			       'https://jobs.example/' || source || '/' || i,
			       first_seen,
			       CASE WHEN status <> 'open' OR (age >= 92 AND age < 97 AND source <> 'linkedin') THEN first_seen
			            ELSE greatest(first_seen, $1::timestamptz + interval '3 hours') END,
			       status,
			       CASE WHEN status <> 'open' THEN $1::timestamptz END
			FROM s`, []any{info.Day, info.WideDay, n}},
		// One observation per job, a few minutes after it was first seen. 60% of
		// SmartRecruiters jobs come from their board answer and await their detail, with no
		// description; every other has ~2,700 characters of incompressible text. Every
		// 1500th (offset 7) description matches the active description rule. 1% closed.
		{"job_observation", `INSERT INTO job_observation (id, job_id, fetch_id, observed_at, derived_by, title, location_text,
			  description_clean, lang, lang_confidence, apply_type, apply_url, closed)
			SELECT j.id, j.id,
			       CASE WHEN j.source = 'linkedin' THEN 26001 + ` + rnd("j.id", 8, 1000) + `
			            WHEN j.source IN (` + detail + `) AND ` + rnd("j.id", 9, 100) + ` >= 60 THEN 20001 + ` + rnd("j.id", 8, 5000) + `
			            ELSE 1 + ` + rnd("j.id", 8, 20000) + ` END,
			       j.first_seen_at + interval '2 minutes',
			       CASE WHEN j.source = 'linkedin' THEN 'linkedin-parser@1' ELSE j.source || '-parser@1' END,
			       j.title, 'Remote, Europe',
			       CASE WHEN j.source IN (` + detail + `) AND ` + rnd("j.id", 9, 100) + ` < 60 THEN ''
			            ELSE (SELECT string_agg(md5(j.id::text || k::text), ' ') FROM generate_series(1, 80) k)
			                 || CASE WHEN j.id % 1500 = 7 THEN ' active security clearance required' ELSE '' END END,
			       'en', 0.99,
			       CASE WHEN j.source = 'linkedin' THEN 'offsite' ELSE 'ats' END,
			       j.url, ` + rnd("j.id", 10, 100) + ` = 0
			FROM job j`, nil},
		// About 3.5 gates an observation (work_rights on half), production's verdict mix:
		// fails overlap a little, so 11% of jobs fail no gate.
		{"gate_result", `INSERT INTO gate_result (job_id, observation_id, gate, verdict, reason, derived_by, created_at)
			SELECT o.job_id, o.id, g.gate,
			       CASE g.gate
			         WHEN 'workplace' THEN CASE WHEN h.u < 680 THEN 'fail' WHEN h.f < 5 THEN 'flag' ELSE 'pass' END
			         WHEN 'language' THEN CASE WHEN h.u >= 650 AND h.u < 800 THEN 'fail' WHEN h.f >= 5 AND h.f < 24 THEN 'flag' ELSE 'pass' END
			         WHEN 'location' THEN CASE WHEN h.u >= 800 AND h.u < 880 THEN 'fail' WHEN h.f >= 24 AND h.f < 28 THEN 'flag' ELSE 'pass' END
			         ELSE CASE WHEN h.u >= 870 AND h.u < 890 THEN 'fail' ELSE 'pass' END
			       END,
			       'seeded', '` + gates.DerivedBy + `', o.observed_at
			FROM job_observation o
			CROSS JOIN LATERAL (SELECT ` + rnd("o.id", 11, 1000) + ` AS u, ` + rnd("o.id", 12, 100) + ` AS f) h
			CROSS JOIN (VALUES (1, 'workplace'), (2, 'language'), (3, 'location'), (4, 'work_rights')) g(ord, gate)
			WHERE g.gate <> 'work_rights' OR (h.u >= 870 AND h.u < 890) OR o.id % 2 = 0
			ORDER BY o.id, g.ord`, nil},
		{"status_event new", `INSERT INTO status_event (job_id, status, actor, at)
			SELECT id, 'new', 'system', first_seen_at FROM job ORDER BY id`, nil},
		{"status_event later", `INSERT INTO status_event (job_id, status, reason, actor, at)
			SELECT j.id, x.status, 'seeded', x.actor, j.first_seen_at + interval '2 hours'
			FROM (SELECT 3 + g * $1::bigint AS id, (ARRAY['shortlisted', 'applied', 'skipped'])[1 + g % 3] AS status, $2::text AS actor
			      FROM generate_series(1, 300) g
			      UNION ALL
			      SELECT 5 + g * $1::bigint, 'skipped', 'system' FROM generate_series(1, 200) g) x
			JOIN job j ON j.id = x.id`, []any{stride, leadstore.ActorGremy}},
		// 12,000 open, current, gate-passing jobs not awaiting detail, spread over every
		// day: the first 400 decided all pass, the next 300 exactly one fail, 200 one
		// unclear, the rest two or three fails; then 100 more decided by the fake decider.
		{"decision", `WITH c AS (
			  SELECT o.id AS obs, o.job_id, o.observed_at,
			         row_number() OVER (ORDER BY hashint8extended(o.job_id, 13), o.job_id) AS k
			  FROM job_observation o
			  JOIN job j ON j.id = o.job_id
			  JOIN http_fetch f ON f.id = o.fetch_id
			  WHERE j.listing_status = 'open' AND NOT o.closed
			    AND NOT (j.source IN (` + detail + `) AND f.kind = 'board')
			    AND NOT EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = o.id AND g.verdict = 'fail')
			)
			INSERT INTO decision (job_id, observation_id, decider, criterion, verdict, confidence, probabilities, reason, input_sha256, created_at)
			SELECT c.job_id, c.obs, CASE WHEN c.k > 12000 THEN $1 ELSE $2 END, cr.name,
			       CASE WHEN c.k > 12000 OR c.k <= 400 THEN 'pass'
			            WHEN c.k <= 700 THEN CASE WHEN cr.ord = 1 + c.k % 5 THEN 'fail' ELSE 'pass' END
			            WHEN c.k <= 900 THEN CASE WHEN cr.ord = 1 + c.k % 5 THEN 'unclear' ELSE 'pass' END
			            WHEN cr.ord IN (1 + c.k % 5, 1 + (c.k + 1) % 5) OR (c.k % 3 = 0 AND cr.ord = 1 + (c.k + 2) % 5) THEN 'fail'
			            ELSE 'pass' END,
			       0.9, '{"pass": 0.9, "fail": 0.05, "unclear": 0.05}'::jsonb, 'seeded',
			       sha256(convert_to(c.obs::text, 'UTF8')), c.observed_at + interval '1 hour'
			FROM c
			CROSS JOIN (VALUES (1, 'title'), (2, 'stack'), (3, 'seniority'), (4, 'location'), (5, 'work_rights')) cr(ord, name)
			WHERE c.k <= 12100
			ORDER BY c.k, cr.ord`, []any{ai.FakeName, config.DefaultJevModel}},
		{"rule", `INSERT INTO rule (kind, field, pattern, reason, created_at, disabled_at, disabled_reason) VALUES
			('skip', 'title', '\bintern\b', 'seeded title rule', $1::timestamptz, NULL, ''),
			('skip', 'description', 'security clearance', 'seeded description rule', $1::timestamptz, NULL, ''),
			('skip', 'title', 'manager', 'seeded disabled rule', $1::timestamptz, $1::timestamptz, 'seeded')`, []any{info.WideDay}},
		// Yesterday's brief, sent, listing 20 of the all-pass jobs.
		{"brief_log", `INSERT INTO brief_log (day, sent_at, provider_id, subject, body_text, job_ids, send_state)
			SELECT $1::date, $2::timestamptz + interval '6 hours', 'seeded', 'seeded', 'seeded',
			       array_agg(job_id ORDER BY job_id), $3
			FROM (SELECT d.job_id FROM decision d
			      WHERE d.decider = $4
			      GROUP BY d.job_id HAVING bool_and(d.verdict = 'pass')
			      ORDER BY d.job_id LIMIT 20) x`,
			[]any{info.WideDay.Format(time.DateOnly), info.WideDay, store.BriefMaybeSent, config.DefaultJevModel}},
	}
	// The marker goes first, so a seed that fails part way is redone on the next call.
	if err := clearAndTruncate(ctx, conn); err != nil {
		return err
	}
	for _, s := range steps {
		if _, err := conn.Exec(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("seed %s: %w", s.name, err)
		}
	}
	// The explicit ids above leave their sequences behind; a test's own insert must not
	// collide with them.
	for _, tbl := range []string{"run", "http_fetch", "company", "job", "job_observation"} {
		if _, err := conn.Exec(ctx,
			fmt.Sprintf("SELECT setval(pg_get_serial_sequence('%s', 'id'), (SELECT max(id) FROM %s))", tbl, tbl)); err != nil {
			return fmt.Errorf("seed: sequence %s: %w", tbl, err)
		}
	}
	if _, err := conn.Exec(ctx, "ANALYZE"); err != nil {
		return fmt.Errorf("seed: analyze: %w", err)
	}
	return nil
}

// clearAndTruncate drops the marker, then empties every table but goose's and KeptTables, as
// ResetDB does.
func clearAndTruncate(ctx context.Context, conn *pgx.Conn) error {
	var db string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		return fmt.Errorf("seed: clear marker: %w", err)
	}
	if _, err := conn.Exec(ctx, "COMMENT ON DATABASE "+pgx.Identifier{db}.Sanitize()+" IS NULL"); err != nil {
		return fmt.Errorf("seed: clear marker: %w", err)
	}
	rows, err := conn.Query(ctx, resetTablesSQL, KeptTables)
	if err != nil {
		return fmt.Errorf("seed: list tables: %w", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("seed: list tables: %w", err)
	}
	if len(tables) == 0 {
		return nil
	}
	// The names come from pg_tables through quote_ident, never from input.
	if _, err := conn.Exec(ctx, "TRUNCATE "+strings.Join(tables, ", ")+" RESTART IDENTITY CASCADE"); err != nil {
		return fmt.Errorf("seed: truncate: %w", err)
	}
	return nil
}

// knownJobs fills info's known job ids from the seeded data, the lowest id of each kind.
func knownJobs(ctx context.Context, conn *pgx.Conn, info *ScaleInfo) error {
	cutoff := info.Day.AddDate(0, 0, -brief.BoardStaleDays)
	// candidate: open, current observation not closed, status still new, no rule match
	// (titles and descriptions match the seeded rules only at these ids).
	const candidate = `j.listing_status = 'open' AND j.id % 1500 NOT IN (0, 7)
		AND NOT EXISTS (SELECT 1 FROM job_observation o WHERE o.job_id = j.id AND o.closed)
		AND NOT EXISTS (SELECT 1 FROM status_event e WHERE e.job_id = j.id AND e.status <> 'new')`
	const decided = `SELECT j.id FROM job j
		JOIN (SELECT job_id, count(*) FILTER (WHERE verdict = 'fail') AS fails,
		             count(*) FILTER (WHERE verdict = 'pass') AS passes
		      FROM decision WHERE decider = $1 GROUP BY job_id) d ON d.job_id = j.id
		WHERE ` + candidate + ` AND j.last_seen_at >= $2 AND d.fails = $3 AND d.passes = $4
		  AND NOT EXISTS (SELECT 1 FROM brief_log b WHERE j.id = ANY(b.job_ids))
		ORDER BY j.id LIMIT 1`
	queries := []struct {
		name string
		dst  *int64
		sql  string
		args []any
	}{
		{"all-pass", &info.AllPassJobID, decided, []any{config.DefaultJevModel, cutoff, 0, 5}},
		{"runner-up", &info.RunnerUpJobID, decided, []any{config.DefaultJevModel, cutoff, 1, 4}},
		{"seen today", &info.SeenTodayJobID, `SELECT j.id FROM job j
			WHERE ` + candidate + ` AND j.source <> 'linkedin' AND j.first_seen_at >= $1
			ORDER BY j.id LIMIT 1`, []any{info.Day}},
		{"not seen", &info.NotSeenJobID, `SELECT j.id FROM job j
			WHERE ` + candidate + ` AND j.source <> 'linkedin' AND j.last_seen_at < $1
			ORDER BY j.id LIMIT 1`, []any{cutoff}},
	}
	for _, q := range queries {
		if err := conn.QueryRow(ctx, q.sql, q.args...).Scan(q.dst); err != nil {
			return fmt.Errorf("known %s job: %w", q.name, err)
		}
	}
	return nil
}
