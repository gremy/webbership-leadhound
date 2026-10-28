//go:build integration

package leadstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migration 00010: findings are general, a lead points at one, and kinds and channels are
// rows.

const findingsVersion = 10 // 00010_findings.sql

// migrateLeadsTo empties the database and migrates it to leads version v (core first). The
// test's cleanup brings it back to the latest version, emptied.
func migrateLeadsTo(t *testing.T, owner *pgx.Conn, v int64) *goose.Provider {
	t.Helper()
	dropSchema(t, owner)
	t.Cleanup(func() {
		migrateAll(t)
		leadstoretest.Reset(t)
	})
	if err := store.Migrate(context.Background(), storetest.MigrateDSN()); err != nil {
		t.Fatalf("core migrate: %v", err)
	}
	p := leadsProvider(t)
	if _, err := p.UpTo(context.Background(), v); err != nil {
		t.Fatalf("leads up to %d: %v", v, err)
	}
	return p
}

// findingRow is a finding as the tests compare it.
type findingRow struct {
	OrgID       int64
	Kind        string
	Detector    string
	Subject     map[string]any
	Evidence    []map[string]any
	SourceTable string
	SourceID    int64
}

func leadFinding(t *testing.T, owner *pgx.Conn, leadID int64) findingRow {
	t.Helper()
	var f findingRow
	var subject, evidence []byte
	if err := owner.QueryRow(context.Background(),
		`SELECT f.org_id, f.kind, f.detector, f.subject, f.evidence, f.source_table, f.source_id
		 FROM lead l JOIN finding f ON f.id = l.finding_id WHERE l.id = $1`, leadID).
		Scan(&f.OrgID, &f.Kind, &f.Detector, &subject, &evidence, &f.SourceTable, &f.SourceID); err != nil {
		t.Fatalf("finding of lead %d: %v", leadID, err)
	}
	if err := json.Unmarshal(subject, &f.Subject); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(evidence, &f.Evidence); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestFindingsBackfill: a database at 00009 with a site lead and an agency lead gets one
// finding per source row at 00010, each lead pointing at its own; a hand lead gets none.
func TestFindingsBackfill(t *testing.T) {
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	p := migrateLeadsTo(t, owner, findingsVersion-1)
	// grants.sql runs clean on a database at 00009, before lead.finding_id exists.
	grants, err := os.ReadFile("../../deploy/sql/grants.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, string(grants)); err != nil {
		t.Fatalf("grants.sql at 00009: %v", err)
	}

	run := ownerInt(t, owner, `INSERT INTO run (command) VALUES ('leads-probe') RETURNING id`)
	fetch := ownerInt(t, owner, `INSERT INTO http_fetch (run_id, source, kind, url, status) VALUES ($1, 'leads', 'site', 'https://north.localhost/', 200) RETURNING id`, run)
	site := ownerInt(t, owner, `INSERT INTO site (domain, origin) VALUES ('north.localhost', 'manual') RETURNING id`)
	probe := ownerInt(t, owner, `INSERT INTO site_probe (site_id, run_id, outcome, is_drupal, major, exact_version, signals, fetch_ids, derived_by)
		VALUES ($1, $2, 'ok', true, 7, '7.98', $3::jsonb, ARRAY[$4::bigint], 'drupal@1') RETURNING id`,
		site, run, `[{"signal": "generator", "value": "Drupal 7", "fetch_id": `+strconv.FormatInt(fetch, 10)+`}, {"signal": "robots", "value": "core", "fetch_id": `+strconv.FormatInt(fetch, 10)+`}]`, fetch)
	siteOrg := ownerInt(t, owner, `INSERT INTO org (kind, domain, site_id, source) VALUES ('site_owner', 'north.localhost', $1, 'probe') RETURNING id`, site)
	agOrg := ownerInt(t, owner, `INSERT INTO org (kind, domain, source) VALUES ('agency', 'agency.localhost', 'agencies-file') RETURNING id`)
	af := ownerInt(t, owner, `INSERT INTO agency_finding (org_id, url, phrase, fetch_id, derived_by)
		VALUES ($1, 'https://agency.localhost/upgrades', 'Drupal 7 upgrades', $2, 'agencies@1') RETURNING id`, agOrg, fetch)
	handOrg := ownerInt(t, owner, `INSERT INTO org (kind, domain, source) VALUES ('site_owner', 'hand.localhost', 'probe') RETURNING id`)
	siteLead := ownerInt(t, owner, `INSERT INTO lead (org_id, kind, probe_id, channel) VALUES ($1, 'unsupported', $2, 'none') RETURNING id`, siteOrg, probe)
	agLead := ownerInt(t, owner, `INSERT INTO lead (org_id, kind, agency_finding_id, channel) VALUES ($1, 'agency_partner', $2, 'none') RETURNING id`, agOrg, af)
	handLead := ownerInt(t, owner, `INSERT INTO lead (org_id, kind, channel) VALUES ($1, 'hand', 'none') RETURNING id`, handOrg)

	if _, err := p.UpTo(ctx, findingsVersion); err != nil {
		t.Fatalf("leads up to %d: %v", findingsVersion, err)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM finding`); n != 2 {
		t.Errorf("findings after backfill = %d, want 2 (one per source row)", n)
	}
	gotSite := leadFinding(t, owner, siteLead)
	wantSite := findingRow{OrgID: siteOrg, Kind: "unsupported", Detector: "drupal@1",
		Subject: map[string]any{"major": 7.0, "exact_version": "7.98", "ambiguous": ""},
		Evidence: []map[string]any{
			{"name": "generator", "value": "Drupal 7", "fetch_id": float64(fetch)},
			{"name": "robots", "value": "core", "fetch_id": float64(fetch)},
		},
		SourceTable: leadstore.FindingSourceProbe, SourceID: probe}
	if !reflect.DeepEqual(gotSite, wantSite) {
		t.Errorf("site finding = %+v, want %+v", gotSite, wantSite)
	}
	gotAg := leadFinding(t, owner, agLead)
	wantAg := findingRow{OrgID: agOrg, Kind: "agency_partner", Detector: "agencies@1",
		Subject:     map[string]any{"url": "https://agency.localhost/upgrades", "phrase": "Drupal 7 upgrades"},
		Evidence:    []map[string]any{{"name": "phrase", "value": "Drupal 7 upgrades", "fetch_id": float64(fetch)}},
		SourceTable: leadstore.FindingSourceAgency, SourceID: af}
	if !reflect.DeepEqual(gotAg, wantAg) {
		t.Errorf("agency finding = %+v, want %+v", gotAg, wantAg)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM lead WHERE id = $1 AND finding_id IS NULL`, handLead); n != 1 {
		t.Error("the hand lead got a finding")
	}

	// The Go dual-write writes the same shape the backfill did: a second site lead, built by
	// UpsertLead on the migrated database, compares equal field for field.
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("leads up: %v", err)
	}
	site2 := ownerInt(t, owner, `INSERT INTO site (domain, origin) VALUES ('south.localhost', 'manual') RETURNING id`)
	probe2 := ownerInt(t, owner, `INSERT INTO site_probe (site_id, run_id, outcome, is_drupal, major, exact_version, signals, fetch_ids, derived_by)
		SELECT $1, run_id, outcome, is_drupal, major, exact_version, signals, fetch_ids, derived_by FROM site_probe WHERE id = $2 RETURNING id`, site2, probe)
	org2 := ownerInt(t, owner, `INSERT INTO org (kind, domain, site_id, source) VALUES ('site_owner', 'south.localhost', $1, 'probe') RETURNING id`, site2)
	lead2, _, err := leadstore.NewQueries(owner).UpsertLead(ctx, leadstore.LeadRow{OrgID: org2, Kind: leadstore.LeadUnsupported, ProbeID: &probe2, Channel: leadstore.ChannelNone})
	if err != nil {
		t.Fatal(err)
	}
	got2 := leadFinding(t, owner, lead2)
	wantSite.OrgID, wantSite.SourceID = org2, probe2
	if !reflect.DeepEqual(got2, wantSite) {
		t.Errorf("dual-written finding = %+v, want the backfill's shape %+v", got2, wantSite)
	}

	// Down restores the CHECKs and drops the new objects; up again backfills again.
	if _, err := p.DownTo(ctx, findingsVersion-1); err != nil {
		t.Fatalf("down to %d: %v", findingsVersion-1, err)
	}
	for _, name := range []string{"lead_kind_check", "lead_channel_check", "lead_finding_check"} {
		if keyColumns(t, owner, name) == "" {
			t.Errorf("after down: %s missing", name)
		}
	}
	var gone bool
	if err := owner.QueryRow(ctx, `SELECT to_regclass('public.finding') IS NULL AND to_regclass('public.lead_kind') IS NULL
		AND to_regclass('public.lead_channel') IS NULL AND NOT EXISTS (SELECT 1 FROM information_schema.columns
		  WHERE table_name = 'lead' AND column_name = 'finding_id')`).Scan(&gone); err != nil || !gone {
		t.Errorf("after down: finding objects remain (%v)", err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO lead (org_id, kind, channel) VALUES ($1, 'nope', 'none')`, handOrg+100); sqlState(err) != "23514" {
		t.Errorf("after down, kind nope: err = %v, want 23514 (the CHECK is back)", err)
	}
	if _, err := p.UpTo(ctx, findingsVersion); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM lead WHERE kind <> 'hand' AND finding_id IS NULL`); n != 0 {
		t.Errorf("after down and up: %d real leads without a finding", n)
	}
}

// TestLeadKindIsARow: a kind or channel with no row is refused by its foreign key.
func TestLeadKindIsARow(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	org := ownerInt(t, f.owner, `INSERT INTO org (kind, domain, source) VALUES ('site_owner', 'kind.localhost', 'probe') RETURNING id`)
	// With its finding the row passes lead_finding_check, so the kind's own foreign key decides.
	fid := ownerInt(t, f.owner, `INSERT INTO finding (org_id, kind, detector, observed_at) VALUES ($1, 'hand', 'test@1', now()) RETURNING id`, org)
	app := storetest.AppConn(t)
	if _, err := app.Exec(ctx, `INSERT INTO lead (org_id, kind, finding_id, channel) VALUES ($1, 'nope', $2, 'none')`, org, fid); sqlState(err) != "23503" {
		t.Errorf("kind nope: err = %v, want 23503", err)
	}
	if _, _, err := f.q.UpsertLead(ctx, leadstore.LeadRow{OrgID: org, Kind: leadstore.LeadHand, Channel: "pigeon"}); sqlState(err) != "23503" {
		t.Errorf("channel pigeon: err = %v, want 23503", err)
	}
	var kinds, channels []string
	for q, out := range map[string]*[]string{
		`SELECT name FROM lead_kind ORDER BY name`:    &kinds,
		`SELECT name FROM lead_channel ORDER BY name`: &channels,
	} {
		rows, err := f.owner.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if *out, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			t.Fatal(err)
		}
	}
	// 00021 adds the three café kinds.
	if want := []string{"agency_partner", "cafe_has_loyalty", "cafe_no_digital_loyalty", "cafe_no_website", "d10", "hand", "selftest", "unsupported"}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("lead kinds = %v, want %v", kinds, want)
	}
	if want := []string{"email", "form", "none"}; !reflect.DeepEqual(channels, want) {
		t.Errorf("lead channels = %v, want %v", channels, want)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_kind WHERE outreach`); n != 6 {
		t.Errorf("outreach kinds = %d, want 6", n)
	}
}

// TestFindingIdempotent: building twice writes one finding; a changed probe writes a second
// and moves the lead onto it, keeping the first as history.
func TestFindingIdempotent(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "twice.localhost", intp(1), 7, "info@twice.localhost")
	l := f.leadOf(t, lead)
	row := leadstore.LeadRow{OrgID: l.OrgID, Kind: l.Kind, ProbeID: l.ProbeID, Channel: l.Channel, RouteID: l.RouteID}
	if _, created, err := f.q.UpsertLead(ctx, row); err != nil || created {
		t.Fatalf("second upsert: created %v, %v", created, err)
	}
	if changed, err := f.q.RefreshLead(ctx, row); err != nil || changed {
		t.Fatalf("identical refresh: changed %v, %v", changed, err)
	}
	first, ok, err := f.q.FindingFor(ctx, lead)
	if err != nil || !ok {
		t.Fatalf("finding for lead: %v %v", ok, err)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM finding`); n != 1 {
		t.Fatalf("findings after two builds = %d, want 1", n)
	}
	// A route change alone keeps the finding.
	row.Channel, row.RouteID = leadstore.ChannelNone, nil
	if changed, err := f.q.RefreshLead(ctx, row); err != nil || !changed {
		t.Fatalf("route refresh: changed %v, %v", changed, err)
	}
	if got, _, _ := f.q.FindingFor(ctx, lead); got.ID != first.ID {
		t.Errorf("a route change moved the finding: %d, want %d", got.ID, first.ID)
	}
	p10 := f.reprobe(t, "twice.localhost", 10)
	row.Kind, row.ProbeID = leadstore.LeadD10, &p10
	if changed, err := f.q.RefreshLead(ctx, row); err != nil || !changed {
		t.Fatalf("probe refresh: changed %v, %v", changed, err)
	}
	second, ok, err := f.q.FindingFor(ctx, lead)
	if err != nil || !ok || second.ID == first.ID || second.SourceID == nil || *second.SourceID != p10 || second.Kind != leadstore.LeadD10 {
		t.Fatalf("finding after the new probe = %+v (%v, %v), want a new one from probe %d", second, ok, err, p10)
	}
	all, err := f.q.FindingsFor(ctx, l.OrgID)
	if err != nil || len(all) != 2 || all[0].ID != second.ID || all[1].ID != first.ID {
		t.Errorf("findings for org = %+v (%v), want [second, first]", all, err)
	}
	if again, _ := f.q.RefreshLead(ctx, row); again {
		t.Error("an identical refresh after the probe change reported a change")
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM finding`); n != 2 {
		t.Errorf("findings = %d, want 2", n)
	}
	// A hand lead has none; a missing lead is ErrLeadNotFound.
	hand, err := f.q.MarkContacted(ctx, 0, "byhand.localhost", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.q.FindingFor(ctx, hand.LeadID); err != nil || ok {
		t.Errorf("hand lead finding: ok %v, %v", ok, err)
	}
	if _, _, err := f.q.FindingFor(ctx, 99999); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("missing lead: %v, want ErrLeadNotFound", err)
	}
}

// TestFindingOrgMustMatch: a lead may not point at another org's finding.
func TestFindingOrgMustMatch(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "a.localhost", nil, 7, "")
	b := f.siteLead(t, "b.localhost", nil, 7, "")
	fa, _, err := f.q.FindingFor(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, `UPDATE lead SET finding_id = $1 WHERE id = $2`, fa.ID, b); sqlState(err) != "23503" {
		t.Errorf("lead b on a's finding: err = %v, want 23503", err)
	}
	app := storetest.AppConn(t)
	if _, err := app.Exec(ctx, `UPDATE lead SET finding_id = $1 WHERE id = $2`, fa.ID, b); sqlState(err) != "23503" {
		t.Errorf("as app: err = %v, want 23503", err)
	}
}

// TestResetKeepsReferenceRows: a reset empties the lead tables and keeps the kinds and
// channels, so a lead can be written straight after.
func TestResetKeepsReferenceRows(t *testing.T) {
	leadstoretest.Reset(t)
	leadstoretest.Reset(t)
	owner := storetest.OwnerConn(t)
	if n := ownerInt(t, owner, `SELECT (SELECT count(*) FROM lead_kind) + (SELECT count(*) FROM lead_channel)`); n != 11 {
		t.Fatalf("reference rows after Reset = %d, want 11", n)
	}
	q := leadstoretest.Q(t)
	org := ownerInt(t, owner, `INSERT INTO org (kind, domain, source) VALUES ('site_owner', 'reset.localhost', 'probe') RETURNING id`)
	if _, created, err := q.UpsertLead(context.Background(), leadstore.LeadRow{OrgID: org, Kind: leadstore.LeadHand, Channel: leadstore.ChannelNone}); err != nil || !created {
		t.Fatalf("lead after Reset: %v %v", created, err)
	}
}

// TestNewKindNeedsNoLeadChange: a kind added by one INSERT INTO lead_kind takes leads that
// carry only a finding, with no change to lead.
func TestNewKindNeedsNoLeadChange(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	t.Cleanup(func() {
		leadstoretest.Reset(t)
		if _, err := storetest.OwnerConn(t).Exec(context.Background(), `DELETE FROM lead_kind WHERE name = 'automation'`); err != nil {
			t.Errorf("remove the test kind: %v", err)
		}
	})
	ownerRun(t, owner, `INSERT INTO lead_kind (name, label, outreach) VALUES ('automation', 'Automatable role', true)`)
	org := ownerInt(t, owner, `INSERT INTO org (kind, domain, source) VALUES ('site_owner', 'jobs.localhost', 'probe') RETURNING id`)
	fid := ownerInt(t, owner, `INSERT INTO finding (org_id, kind, detector, subject, observed_at)
		VALUES ($1, 'automation', 'jobad@1', '{"job_id": 1, "roles": ["data entry"]}', now()) RETURNING id`, org)
	app := storetest.AppConn(t)
	var lead int64
	if err := app.QueryRow(ctx, `INSERT INTO lead (org_id, kind, finding_id, channel) VALUES ($1, 'automation', $2, 'none') RETURNING id`,
		org, fid).Scan(&lead); err != nil {
		t.Fatalf("a lead of the new kind with only its finding: %v", err)
	}
	org2 := ownerInt(t, owner, `INSERT INTO org (kind, domain, source) VALUES ('site_owner', 'jobs2.localhost', 'probe') RETURNING id`)
	if _, err := app.Exec(ctx, `INSERT INTO lead (org_id, kind, channel) VALUES ($1, 'automation', 'none')`, org2); sqlState(err) != "23514" {
		t.Errorf("new kind without a finding: err = %v, want 23514", err)
	}
	// Today's kinds keep their exact pointer rule: a finding alone is not enough.
	fid2 := ownerInt(t, owner, `INSERT INTO finding (org_id, kind, detector, observed_at) VALUES ($1, 'd10', 'drupal@1', now()) RETURNING id`, org2)
	if _, err := app.Exec(ctx, `INSERT INTO lead (org_id, kind, finding_id, channel) VALUES ($1, 'd10', $2, 'none')`, org2, fid2); sqlState(err) != "23514" {
		t.Errorf("d10 with a finding but no probe: err = %v, want 23514", err)
	}
}
