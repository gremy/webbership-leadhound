package leadstoretest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The leads part of the scale database (storetest.Seeded): a year of lead finding and
// outreach on top of the jobs seed, seeded once and reused by every leads scale test.
const (
	// scaleLeadsVersion is the marker's leads part (storetest.ScaleInfo.Leads) for this
	// seed's shape: a change to what the seed writes bumps it, so the lead tables of every
	// scale database seeded before are seeded again.
	scaleLeadsVersion = "v4"

	// scaleLeadsLockKey is the advisory lock two processes take before checking or seeding
	// the lead tables, so they never seed them at the same time.
	scaleLeadsLockKey = 7311041902

	// scaleSeedNote marks the run rows this seed writes; their fetches hang from them, so a
	// reseed removes exactly what the last one wrote from the core tables.
	scaleSeedNote = "leads-seeded"
)

// The seed's sizes.
const (
	scaleSites       = 1_000_000
	scaleProbedSites = 700_000 // sites 1..700,000 probed once; 1..300,000 of them twice
	scaleReprobed    = 300_000
	scaleOrgs        = 9_000 // site owners from the latest Drupal probes, then the agencies
	scaleAgencies    = 52
	scaleLeads       = 5_000
	scaleBatches     = 400 // the first scaleShadow shadow, the rest live, 50 messages each
	scaleShadow      = 60
	scaleMailboxes   = 10 // sending mailboxes, two on each of scaleMailDomains reserved domains
	scaleMailDomains = 5
)

// leadsTables are the tables of the leads migration set, emptied before a reseed.
var leadsTables = []string{
	"mailbox_check", "mailbox_state", "sender_mailbox",
	"lead_priority", "priority_profile", "send_pause", "sender_check", "mailbox_cursor",
	"mailbox_event", "suppression_lift", "suppression", "outreach_message", "outreach_batch", "lead_event", "lead",
	"contact_attempt", "agency_finding", "contact_route", "org", "fetch_header", "site_probe", "site",
}

var (
	scaleMu    sync.Mutex
	scaleReady bool
)

// ScaleSeeded returns the seeded scale database (storetest.Seeded) with its lead tables
// seeded too. Lead tables whose marker part names this seed are reused as they are; any
// other are emptied, with the runs and fetches the last leads seed added to the core
// tables, seeded afresh as the owner and analysed, and the marker's leads part is written
// last, so a seed that failed part way is redone on the next call.
func ScaleSeeded(t testing.TB) storetest.ScaleInfo {
	t.Helper()
	info := storetest.Seeded(t)
	scaleMu.Lock()
	defer scaleMu.Unlock()
	if scaleReady || info.Leads == scaleLeadsVersion {
		scaleReady = true
		info.Leads = scaleLeadsVersion
		return info
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, storetest.ScaleMigrateDSN())
	if err != nil {
		t.Fatalf("leadstoretest: scale: connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(scaleLeadsLockKey)); err != nil {
		t.Fatalf("leadstoretest: scale: lock: %v", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(scaleLeadsLockKey))
	}()
	// Another process may have seeded the lead tables while this one waited for the lock.
	var comment string
	if err := conn.QueryRow(ctx,
		`SELECT coalesce(shobj_description(oid, 'pg_database'), '') FROM pg_database
		 WHERE datname = current_database()`).Scan(&comment); err != nil {
		t.Fatalf("leadstoretest: scale: read marker: %v", err)
	}
	if strings.HasSuffix(comment, ";leads="+scaleLeadsVersion) {
		scaleReady = true
		info.Leads = scaleLeadsVersion
		return info
	}
	info.Leads = ""
	storetest.WriteScaleMarker(t, info) // cleared first: a seed that fails is redone
	if err := seedLeads(ctx, conn, info); err != nil {
		t.Fatalf("leadstoretest: scale: %v", err)
	}
	info.Leads = scaleLeadsVersion
	storetest.WriteScaleMarker(t, info)
	scaleReady = true
	return info
}

// rnd is a deterministic pseudo-random integer in [0, mod) from the bigint expression x and
// salt, so each attribute of a row is drawn independently and every seed is the same.
func rnd(x string, salt, mod int) string {
	return fmt.Sprintf("((hashint8extended(%s, %d) & 9223372036854775807) %% %d)", x, salt, mod)
}

// seedLeads empties the lead tables and fills them, set-based, one statement per table,
// shaped like a year of finding and outreach. $1 is the seed's Day wherever a statement
// takes it; every date is counted back from it.
func seedLeads(ctx context.Context, conn *pgx.Conn, info storetest.ScaleInfo) error {
	runBase, fetchBase, reuse, err := clearLeads(ctx, conn)
	if err != nil {
		return err
	}
	day := info.Day
	// Probe k (1..1,000,000) probes site k, or site k-700,000 for its second probe. The
	// 400,000 probes with k % 5 < 2 kept a fetch: the homepage (k % 5 = 0) or robots.txt.
	site := `CASE WHEN k <= ` + fmt.Sprint(scaleProbedSites) + ` THEN k ELSE k - ` + fmt.Sprint(scaleProbedSites) + ` END`
	fetchOf := fmt.Sprintf("(%d + 2 * (k / 5) + k %% 5)", fetchBase)
	drupal := `(k % 5 < 2 AND ` + rnd("k", 20, 1000) + ` < 40)`
	major := `(ARRAY[6, 7, 7, 7, 7, 7, 8, 8, 9, 9, 9, 10, 10, 10, 10, 10, 11, 11, 11, 0])[1 + ` + rnd("k", 25, 20) + `]`
	// First probes 60 to 364 days before Day, second probes in the 60 days since.
	probedAt := `CASE WHEN k <= ` + fmt.Sprint(scaleProbedSites) + `
		  THEN $1::timestamptz - (60 + ` + rnd("k", 21, 305) + `) * interval '1 day' + ` + rnd("k", 22, 86400) + ` * interval '1 second'
		  ELSE $1::timestamptz - ` + rnd("k", 21, 60) + ` * interval '1 day' + ` + rnd("k", 22, 86400) + ` * interval '1 second' END`
	senderSHA := sha256.Sum256([]byte("sender.example"))
	steps := []struct {
		name string
		sql  string
		args []any
	}{
		// Twelve monthly probe runs and twelve contact runs.
		{"run", `INSERT INTO run (id, command, source, started_at, finished_at, requests, note)
			SELECT $2::bigint + i, CASE WHEN i <= 12 THEN 'leads-sites-probe' ELSE 'leads-contacts' END, 'leads',
			       $1::timestamptz - (i % 12) * interval '30 days', $1::timestamptz - (i % 12) * interval '30 days' + interval '3 hours',
			       1000, '` + scaleSeedNote + `'
			FROM generate_series(1, 24) i`, []any{day, runBase}},
		{"http_fetch", `INSERT INTO http_fetch (id, run_id, source, kind, url, status, fetched_at, content_type)
			SELECT ` + fetchOf + `, $2::bigint + 1 + k % 12, 'probe',
			       CASE WHEN k % 5 = 0 THEN 'site' ELSE 'robots' END,
			       'https://s' || (` + site + `) || '.example/' || CASE WHEN k % 5 = 0 THEN '' ELSE 'robots.txt' END,
			       200, ` + probedAt + `, CASE WHEN k % 5 = 0 THEN 'text/html' ELSE 'text/plain' END
			FROM generate_series(1, ` + fmt.Sprint(scaleSites) + `) k
			WHERE k % 5 < 2`, []any{day, runBase}},
		// Production-like countries; 80% ranked; every site probed but the last 300,000.
		{"site", `INSERT INTO site (id, domain, country, origin, list_rank, added_at, state)
			SELECT i, 's' || i || '.example',
			       (ARRAY['GB', 'GB', 'GB', 'GB', 'GB', 'GB', 'GB', 'DE', 'DE', 'DE', 'NL', 'NL', 'RO', 'RO', 'IE', '', '', '', '', ''])[1 + ` + rnd("i", 23, 20) + `],
			       (ARRAY['tranco', 'majestic', 'httparchive'])[1 + i % 3],
			       CASE WHEN i % 5 <> 0 THEN i::int END,
			       $1::timestamptz - interval '370 days', 'new'
			FROM generate_series(1, ` + fmt.Sprint(scaleSites) + `) i`, []any{day}},
		{"site_probe", `INSERT INTO site_probe (id, site_id, run_id, outcome, final_host, is_drupal, major, exact_version,
			  ambiguous, signals, fetch_ids, site_name, derived_by, created_at)
			SELECT k, ` + site + `, $2::bigint + 1 + k % 12,
			       CASE WHEN ` + drupal + ` THEN 'ok'
			            ELSE (ARRAY['ok', 'ok', 'ok', 'ok', 'ok', 'ok', 'ok', 'transport_error', 'http_error', 'bot_check', 'not_html', 'robots_disallowed'])[1 + ` + rnd("k", 24, 12) + `] END,
			       CASE WHEN k % 10 = 3 THEN 'www.s' || (` + site + `) || '.example' ELSE '' END,
			       ` + drupal + `,
			       CASE WHEN ` + drupal + ` THEN ` + major + ` ELSE 0 END,
			       CASE WHEN ` + drupal + ` AND k % 2 = 0 THEN '7.' || (k % 100) ELSE '' END,
			       CASE WHEN ` + drupal + ` AND ` + rnd("k", 26, 20) + ` = 0 THEN '10-or-11' ELSE '' END,
			       CASE WHEN ` + drupal + ` THEN jsonb_build_array(jsonb_build_object('signal', 'x-generator', 'value', (` + major + `)::text, 'fetch_id', ` + fetchOf + `))
			            ELSE '[]'::jsonb END,
			       CASE WHEN k % 5 < 2 THEN ARRAY[` + fetchOf + `]::bigint[] ELSE '{}'::bigint[] END,
			       CASE WHEN ` + drupal + ` THEN 'Site ' || (` + site + `) ELSE '' END,
			       'drupal@1', ` + probedAt + `
			FROM generate_series(1, ` + fmt.Sprint(scaleSites) + `) k`, []any{day, runBase}},
		// Each probed site points at its latest probe; 1% of them are retired.
		{"site last probe", `UPDATE site SET
			  last_probe_id = CASE WHEN id <= ` + fmt.Sprint(scaleReprobed) + ` THEN id + ` + fmt.Sprint(scaleProbedSites) + ` ELSE id END,
			  state = CASE WHEN id % 100 = 7 THEN 'retired' ELSE 'probed' END
			WHERE id <= ` + fmt.Sprint(scaleProbedSites), nil},
		{"fetch_header", `INSERT INTO fetch_header (fetch_id, headers)
			SELECT (signals->0->>'fetch_id')::bigint, jsonb_build_object('x-generator', jsonb_build_array('Drupal ' || (signals->0->>'value')))
			FROM site_probe WHERE is_drupal`, nil},
		// The first site owners from the sites whose latest probe found Drupal (the rest wait
		// for DrupalProbesWithoutOrg), then the agencies.
		{"org", `INSERT INTO org (id, kind, domain, name, country, site_id, source, source_fetch_id, created_at)
			SELECT row_number() OVER (ORDER BY s.id), 'site_owner', s.domain, p.site_name, s.country, s.id, 'probe',
			       CASE WHEN p.id % 5 = 0 THEN p.fetch_ids[1] END, p.created_at + interval '1 hour'
			FROM site s JOIN site_probe p ON p.id = s.last_probe_id
			WHERE p.is_drupal
			ORDER BY s.id LIMIT ` + fmt.Sprint(scaleOrgs-scaleAgencies), nil},
		{"org agencies", `INSERT INTO org (id, kind, domain, name, country, source, created_at)
			SELECT ` + fmt.Sprint(scaleOrgs-scaleAgencies) + ` + i, 'agency', 'agency' || i || '.example', 'Agency ' || i,
			       CASE WHEN i % 2 = 0 THEN 'GB' ELSE 'DE' END, 'agencies-file', $1::timestamptz - interval '300 days'
			FROM generate_series(1, ` + fmt.Sprint(scaleAgencies) + `) i`, []any{day}},
		// About 2.2 routes an org: info@ (with MX on 7 in 10), a contact form on 6 in 10,
		// office@ on 3 in 10, web@ on 5 in 10; a fifth found in the last 30 days.
		{"contact_route", `INSERT INTO contact_route (org_id, kind, value, fetch_id, mx_ok, derived_by, found_at)
			SELECT o.id, r.kind, CASE WHEN r.kind = 'email' THEN r.local || '@' || o.domain ELSE 'https://' || o.domain || '/contact' END,
			       $2::bigint + 1 + o.id % 400000, r.kind = 'email' AND o.id % 10 < 7, 'contacts@1',
			       $1::timestamptz - CASE WHEN o.id % 5 = 0 THEN o.id % 30 ELSE 31 + o.id % 300 END * interval '1 day'
			FROM org o
			CROSS JOIN (VALUES (1, 'email', 'info'), (2, 'form', ''), (3, 'email', 'office'), (4, 'email', 'web')) r(ord, kind, local)
			WHERE (r.ord = 1 AND o.id % 10 < 8) OR (r.ord = 2 AND o.id % 10 >= 4)
			   OR (r.ord = 3 AND o.id % 10 < 3) OR (r.ord = 4 AND o.id % 10 < 5)
			ORDER BY o.id, r.ord`, []any{day, fetchBase}},
		{"agency_finding", `INSERT INTO agency_finding (org_id, url, phrase, fetch_id, derived_by, created_at)
			SELECT o.id, 'https://' || o.domain || '/drupal-' || n, 'Drupal 7 upgrade', $2::bigint + o.id, 'agencies@1',
			       $1::timestamptz - interval '290 days' + n * interval '1 day'
			FROM org o CROSS JOIN generate_series(1, 2) n
			WHERE o.kind = 'agency' AND (n = 1 OR o.id % 2 = 0)
			ORDER BY o.id, n`, []any{day, fetchBase}},
		{"contact_attempt", `INSERT INTO contact_attempt (org_id, run_id, routes_found, attempted_at)
			SELECT o.id, $2::bigint + 13 + o.id % 12, n % 3, $1::timestamptz - (o.id % 365 + n * 40) * interval '1 day'
			FROM org o CROSS JOIN generate_series(0, 1) n
			WHERE o.id % 4 <> 0 AND (n = 0 OR o.id % 7 = 0)
			ORDER BY o.id, n`, []any{day, runBase}},
		// The first 5,000 orgs that classify into a lead (Drupal 6-10 with a known major,
		// or an agency with a finding), the channel and route as Classify picks them.
		{"lead", `WITH c AS (
			  SELECT o.id AS org_id, o.country, o.created_at,
			         CASE WHEN o.kind = 'agency' THEN 'agency_partner' WHEN p.major = 10 THEN 'd10' ELSE 'unsupported' END AS kind,
			         CASE WHEN o.kind = 'site_owner' THEN p.id END AS probe_id,
			         (SELECT min(f.id) FROM agency_finding f WHERE f.org_id = o.id) AS finding_id,
			         (SELECT r.id FROM contact_route r WHERE r.org_id = o.id AND r.kind = 'email' AND r.mx_ok ORDER BY r.id LIMIT 1) AS email_id,
			         (SELECT r.id FROM contact_route r WHERE r.org_id = o.id AND r.kind = 'form' ORDER BY r.id LIMIT 1) AS form_id
			  FROM org o LEFT JOIN site s ON s.id = o.site_id LEFT JOIN site_probe p ON p.id = s.last_probe_id
			  WHERE o.kind = 'agency' OR (p.major BETWEEN 6 AND 10 AND p.ambiguous = '')
			  ORDER BY o.kind = 'agency' DESC, o.id
			  LIMIT ` + fmt.Sprint(scaleLeads) + `
			), ch AS (
			  SELECT c.*, CASE WHEN c.country = 'GB' AND email_id IS NOT NULL THEN 'email'
			                   WHEN c.country IN ('GB', 'DE') AND form_id IS NOT NULL THEN 'form' ELSE 'none' END AS channel
			  FROM c
			)
			INSERT INTO lead (id, org_id, kind, probe_id, agency_finding_id, channel, route_id, state, state_at, created_at)
			SELECT row_number() OVER (ORDER BY org_id), org_id, kind, probe_id,
			       CASE WHEN kind = 'agency_partner' THEN finding_id END, channel,
			       CASE channel WHEN 'email' THEN email_id WHEN 'form' THEN form_id END,
			       'new', created_at + interval '1 day', created_at + interval '1 day'
			FROM ch ORDER BY org_id`, nil},
		// Half the leads still new; the rest moved on, as a year of outreach leaves them.
		{"lead state", `UPDATE lead SET
			  state = (ARRAY['contacted', 'contacted', 'contacted', 'contacted', 'contacted', 'contacted', 'replied', 'bounced',
			                 'opted_out', 'suppressed', 'skipped', 'skipped', 'closed'])[1 + ` + rnd("id", 27, 13) + `],
			  state_at = state_at + interval '30 days'
			WHERE ` + rnd("id", 28, 2) + ` = 0`, nil},
		{"lead_event", `INSERT INTO lead_event (lead_id, state, reason, actor, at)
			SELECT id, 'new', 'created', 'system', created_at FROM lead
			UNION ALL
			SELECT id, state, 'seeded', CASE WHEN state = 'skipped' THEN 'gremy' ELSE 'system' END, state_at
			FROM lead WHERE state <> 'new'
			ORDER BY 1, 5`, nil},
		// The sending mailboxes: outN@mailD.example, two on each domain, every one checked
		// and passed the day before Day. The sealed bytes are a placeholder no key opens.
		{"sender_mailbox", `INSERT INTO sender_mailbox (id, address, domain, login, smtp_addr, imap_addr, dkim_selector,
			  from_name, password_sealed, sealed_by, created_at)
			SELECT n, 'out' || n || '@' || d.domain, d.domain, 'out' || n || '@' || d.domain,
			       'smtp.' || d.domain || ':465', 'imap.' || d.domain || ':993', 'lh', '',
			       convert_to('placeholder, sealed by no key', 'UTF8'), 'placeholder', $1::timestamptz - interval '401 days'
			FROM generate_series(1, ` + fmt.Sprint(scaleMailboxes) + `) n
			CROSS JOIN LATERAL (SELECT 'mail' || (1 + (n - 1) % ` + fmt.Sprint(scaleMailDomains) + `) || '.example' AS domain) d`, []any{day}},
		{"mailbox_check", `INSERT INTO mailbox_check (mailbox_id, requested_at, actor, checked_at, smtp_ok, imap_ok, folders_ok,
			  spf, dkim, dmarc, outcome)
			SELECT id, $1::timestamptz - interval '1 day', 'operator', $1::timestamptz - interval '1 day' + interval '1 minute',
			       true, true, true, true, true, true, 'passed'
			FROM sender_mailbox ORDER BY id`, []any{day}},
		// Batch b drafted 401-b days before Day and approved the same day: the first 60 the
		// shadow week, the rest live.
		{"outreach_batch", `INSERT INTO outreach_batch (id, shadow, created_at, content_sha256, approved_at, approved_hash, templates_sha256)
			SELECT b, b <= ` + fmt.Sprint(scaleShadow) + `, $1::timestamptz - (401 - b) * interval '1 day',
			       sha256(convert_to('batch ' || b, 'UTF8')),
			       $1::timestamptz - (401 - b) * interval '1 day' + interval '2 hours',
			       sha256(convert_to('batch ' || b, 'UTF8')), sha256(convert_to('templates', 'UTF8'))
			FROM generate_series(1, ` + fmt.Sprint(scaleBatches) + `) b`, []any{day}},
		// 50 messages a batch: 16 leads with three emails, then one with a form and a
		// LinkedIn note. Old live sequences are sent or cancelled; the last batch's first
		// emails are half sent and half due, its follow-ups and manual items wait; a few
		// sends are left to check by hand.
		{"outreach_message", `WITH m AS (
			  SELECT b, j, (b - 1) * 50 + j + 1 AS id,
			         1 + ((b - 1) * 17 + CASE WHEN j < 48 THEN j / 3 ELSE 16 END) % ` + fmt.Sprint(scaleLeads) + ` AS lead_id,
			         CASE WHEN j < 48 THEN j % 3 ELSE j - 48 END AS step,
			         CASE WHEN j < 48 THEN 'email' WHEN j = 48 THEN 'form' ELSE 'linkedin' END AS channel,
			         $1::timestamptz - (401 - b) * interval '1 day' AS drafted
			  FROM generate_series(1, ` + fmt.Sprint(scaleBatches) + `) b CROSS JOIN generate_series(0, 49) j
			), s AS (
			  SELECT m.*, CASE
			           WHEN b <= ` + fmt.Sprint(scaleShadow) + ` THEN CASE WHEN channel = 'email' THEN 'shadow_sent' ELSE 'done' END
			           WHEN b = ` + fmt.Sprint(scaleBatches) + ` THEN CASE WHEN channel <> 'email' THEN 'approved'
			                                 WHEN step = 0 AND j < 24 THEN 'sent' ELSE 'approved' END
			           WHEN channel <> 'email' THEN 'done'
			           WHEN b = 390 AND step = 0 AND j < 9 THEN 'maybe_sent'
			           WHEN b = 399 AND step = 0 AND j = 0 THEN 'sending'
			           WHEN step = 0 THEN 'sent'
			           WHEN ` + rnd("m.id", 29, 10) + ` < 7 - 2 * step THEN 'sent'
			           ELSE 'cancelled' END AS state
			  FROM m
			)
			INSERT INTO outreach_message (id, batch_id, lead_id, mailbox_id, step, channel, to_address, subject, body_text, message_id,
			  slots, approved_sha256, state, due_at, sent_at, cancel_reason, created_at)
			SELECT s.id, s.b, s.lead_id, 1 + s.lead_id % ` + fmt.Sprint(scaleMailboxes) + `, s.step, s.channel,
			       CASE WHEN s.channel = 'email' THEN 'info@' || o.domain ELSE '' END,
			       CASE WHEN s.channel = 'email' THEN 'Your Drupal site' ELSE '' END,
			       repeat('A seeded outreach message body. ', 20),
			       CASE WHEN s.channel = 'email' THEN '<seed-' || s.id || '@sender.example>' ELSE '' END,
			       '{}'::jsonb, sha256(convert_to(s.id::text, 'UTF8')), s.state,
			       CASE WHEN s.state IN ('sent', 'shadow_sent', 'maybe_sent', 'sending', 'cancelled') THEN s.drafted + (1 + 5 * s.step) * interval '1 day'
			            WHEN s.state = 'approved' AND s.channel = 'email' AND s.step = 0 THEN
			              $1::timestamptz + CASE WHEN s.j < 36 THEN interval '9 hours' ELSE interval '1 day' END END,
			       CASE WHEN s.state IN ('sent', 'shadow_sent', 'maybe_sent', 'sending') THEN s.drafted + (1 + 5 * s.step) * interval '1 day' + interval '10 hours' END,
			       CASE WHEN s.state = 'cancelled' THEN 'reply' ELSE '' END,
			       s.drafted
			FROM s JOIN lead l ON l.id = s.lead_id JOIN org o ON o.id = l.org_id
			ORDER BY s.id`, []any{day}},
		// The last batch's leads are batched.
		{"lead batched", `UPDATE lead SET state = 'batched'
			WHERE id IN (SELECT lead_id FROM outreach_message WHERE batch_id = ` + fmt.Sprint(scaleBatches) + `)`, nil},
		// 2,000 suppressions: addresses and domains of seeded sites, every reason.
		{"suppression", `INSERT INTO suppression (kind, value, reason, source, created_at)
			SELECT CASE WHEN i % 5 < 3 THEN 'address' ELSE 'domain' END,
			       CASE WHEN i % 5 < 3 THEN 'info@s' || (i * 487) || '.example' ELSE 's' || (i * 487) || '.example' END,
			       (ARRAY['opt_out', 'bounce', 'replied_no', 'manual'])[1 + i % 4],
			       CASE WHEN i % 4 = 3 THEN 'cli' ELSE 'mailbox:INBOX:' || i END,
			       $1::timestamptz - (i % 365) * interval '1 day'
			FROM generate_series(1, 2000) i`, []any{day}},
		// 50 of them lifted (one in forty, ids 40..2,000), so suppression_active is measured with
		// lifts present.
		{"suppression_lift", `INSERT INTO suppression_lift (suppression_id, reason, actor, source, lifted_at)
			SELECT 40 * i, 'seeded lift: recorded by mistake', 'operator', (ARRAY['cli', 'console'])[1 + i % 2],
			       $1::timestamptz - (i % 30) * interval '1 day'
			FROM generate_series(1, 50) i`, []any{day}},
		// 4,000 mailbox messages over the year, about eleven a day; seven in ten matched
		// to a live first email.
		{"mailbox_event", `WITH e AS (
			  SELECT i, (ARRAY['reply', 'reply', 'auto_reply', 'auto_reply', 'auto_reply', 'bounce', 'bounce', 'opt_out', 'unmatched', 'unmatched'])[1 + i % 10] AS kind,
			         (` + fmt.Sprint(scaleShadow) + ` + 1 + i % ` + fmt.Sprint(scaleBatches-scaleShadow-1) + ` - 1) * 50 + 3 * (i % 16) + 1 AS msg
			  FROM generate_series(1, 4000) i
			)
			INSERT INTO mailbox_event (mailbox_id, mailbox, uidvalidity, uid, kind, from_domain, matched_message_id, lead_id, seen_at)
			SELECT m.mailbox_id, CASE WHEN e.kind = 'unmatched' AND e.i % 3 = 0 THEN 'Junk' ELSE 'INBOX' END, 1, e.i, e.kind,
			       CASE WHEN e.kind = 'unmatched' THEN 'other' || e.i || '.example' ELSE o.domain END,
			       CASE WHEN e.kind <> 'unmatched' THEN m.id END, CASE WHEN e.kind <> 'unmatched' THEN m.lead_id END,
			       $1::timestamptz - (e.i % 365) * interval '1 day' + (e.i % 86400) * interval '1 second'
			FROM e JOIN outreach_message m ON m.id = e.msg JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
			ORDER BY e.i`, []any{day}},
		{"mailbox_cursor", `INSERT INTO mailbox_cursor (mailbox_id, mailbox, uidvalidity, last_uid, synced_at)
			VALUES (1, 'INBOX', 1, 4000, $1::timestamptz - interval '1 hour'), (1, 'Junk', 1, 3999, $1::timestamptz - interval '1 hour')`, []any{day}},
		{"sender_check", `INSERT INTO sender_check (domain_sha256, selector, spf, dkim, dmarc, note, checked_at)
			SELECT $2::bytea, 'lh', true, i % 50 <> 0, true, '', $1::timestamptz - i * interval '1 day' + interval '6 hours'
			FROM generate_series(0, 364) i`, []any{day, senderSHA[:]}},
		// Each mailbox domain's daily check with the mailboxes' selector, a pass on Day among
		// them (sha256 of the domain, as leadstore.SenderDomainSHA256 hashes it).
		{"sender_check mailboxes", `INSERT INTO sender_check (domain_sha256, selector, spf, dkim, dmarc, note, checked_at)
			SELECT sha256(convert_to('mail' || d || '.example', 'UTF8')), 'lh', true, i % 50 <> 7, true, '',
			       $1::timestamptz - i * interval '1 day' + interval '6 hours'
			FROM generate_series(1, ` + fmt.Sprint(scaleMailDomains) + `) d CROSS JOIN generate_series(0, 364) i
			ORDER BY i DESC, d`, []any{day}},
		// Paused and resumed three times; sending is on.
		{"send_pause", `INSERT INTO send_pause (paused, reason, actor, at)
			SELECT i % 2 = 1, CASE WHEN i % 2 = 1 THEN 'seeded pause' ELSE '' END, 'gremy',
			       $1::timestamptz - (200 - 30 * i) * interval '1 day'
			FROM generate_series(1, 6) i`, []any{day}},
		// Hand steering of 200 new leads: pins, defers around Day, and clears.
		{"lead_priority", `INSERT INTO lead_priority (lead_id, action, until, actor, at)
			SELECT l.id, (ARRAY['pin', 'defer', 'defer', 'clear'])[1 + n % 4],
			       CASE WHEN n % 4 IN (1, 2) THEN ($1::timestamptz + (n % 30 - 10) * interval '1 day')::date END,
			       'gremy', $1::timestamptz - (n % 60) * interval '1 day'
			FROM (SELECT id, row_number() OVER (ORDER BY id) AS n FROM lead WHERE state = 'new') l
			WHERE l.n <= 200
			ORDER BY l.n`, []any{day}},
	}
	for _, s := range steps {
		if reuse && (s.name == "run" || s.name == "http_fetch") {
			continue
		}
		if _, err := conn.Exec(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("seed leads %s: %w", s.name, err)
		}
	}
	// The operator's saved profile: 30 rules, scored in SQL for every candidate.
	if _, err := leadstore.NewQueries(conn).SaveProfile(ctx, scaleProfile()); err != nil {
		return fmt.Errorf("seed leads: profile: %w", err)
	}
	// The explicit ids above leave their sequences behind; a test's own insert must not
	// collide with them.
	for _, tbl := range []string{"run", "http_fetch", "site", "site_probe", "org", "lead", "outreach_batch", "outreach_message", "sender_mailbox"} {
		if _, err := conn.Exec(ctx,
			fmt.Sprintf("SELECT setval(pg_get_serial_sequence('%s', 'id'), (SELECT max(id) FROM %s))", tbl, tbl)); err != nil {
			return fmt.Errorf("seed leads: sequence %s: %w", tbl, err)
		}
	}
	if _, err := conn.Exec(ctx, "ANALYZE run, http_fetch, "+strings.Join(leadsTables, ", ")); err != nil {
		return fmt.Errorf("seed leads: analyze: %w", err)
	}
	return nil
}

// scaleProfile is a profile of 30 rules: every kind and major, both channels, four rank
// bands, a bonus for the exact version, twelve countries and two exclusions.
func scaleProfile() leadconfig.Profile {
	return leadconfig.Profile{
		Kinds:    map[string]int{"unsupported": 20, "d10": 10, "agency_partner": 15},
		Majors:   map[int]int{6: 5, 7: 30, 8: 20, 9: 15, 10: 10, 11: -10},
		Channels: map[string]int{"email": 10, "form": 0},
		RankBands: []leadconfig.RankBand{
			{Under: 1000, Points: 25}, {Under: 10000, Points: 15}, {Under: 100000, Points: 8}, {Under: 500000, Points: 3},
		},
		ExactVersion: 5,
		Countries: map[string]int{
			"GB": 20, "DE": 10, "NL": 5, "RO": -5, "IE": 8, "BE": 4, "AT": 3, "CH": 6, "FR": 2, "ES": 1, "IT": 1, "PL": -3,
		},
		ExcludeCountries: []string{"RU", "BY"},
	}
}

// clearLeads empties every lead table and returns the id bases the seed's runs and fetches
// start after. The runs and fetches the last leads seed added to the core tables are kept
// and reused when they are all there (they depend on nothing a reseed changes): deleting
// them checks every one against job_observation's foreign key, which has no index, and takes
// far longer than the seed. Only a part left behind is deleted.
func clearLeads(ctx context.Context, conn *pgx.Conn) (runBase, fetchBase int64, reuse bool, err error) {
	if _, err := conn.Exec(ctx, "TRUNCATE "+strings.Join(leadsTables, ", ")+" RESTART IDENTITY"); err != nil {
		return 0, 0, false, fmt.Errorf("seed leads: truncate: %w", err)
	}
	var runs, fetches int64
	if err := conn.QueryRow(ctx,
		`SELECT coalesce(min(r.id) - 1, 0), count(*),
		   (SELECT coalesce(min(f.id) - 1, 0) FROM http_fetch f WHERE f.run_id IN (SELECT id FROM run WHERE note = $1)),
		   (SELECT count(*) FROM http_fetch f WHERE f.run_id IN (SELECT id FROM run WHERE note = $1))
		 FROM run r WHERE r.note = $1`, scaleSeedNote).Scan(&runBase, &runs, &fetchBase, &fetches); err != nil {
		return 0, 0, false, fmt.Errorf("seed leads: earlier seed: %w", err)
	}
	if runs == 24 && fetches == 2*scaleSites/5 {
		return runBase, fetchBase, true, nil
	}
	if runs > 0 {
		if _, err := conn.Exec(ctx,
			`DELETE FROM http_fetch WHERE run_id IN (SELECT id FROM run WHERE note = $1)`, scaleSeedNote); err != nil {
			return 0, 0, false, fmt.Errorf("seed leads: clear fetches: %w", err)
		}
		if _, err := conn.Exec(ctx, `DELETE FROM run WHERE note = $1`, scaleSeedNote); err != nil {
			return 0, 0, false, fmt.Errorf("seed leads: clear runs: %w", err)
		}
	}
	if err := conn.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM run`).Scan(&runBase); err != nil {
		return 0, 0, false, fmt.Errorf("seed leads: run base: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM http_fetch`).Scan(&fetchBase); err != nil {
		return 0, 0, false, fmt.Errorf("seed leads: fetch base: %w", err)
	}
	return runBase, fetchBase, false, nil
}
