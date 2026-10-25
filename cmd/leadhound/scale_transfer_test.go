//go:build scale

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The transfer's scale test exports the leads scale seed (leadstoretest.ScaleSeeded: a
// million sites and probes, 400,000 fetches, 9,000 orgs) in a read-only transaction and
// imports it into a database of its own, created and dropped here, measuring the Go heap
// of each. The import runs on the server under GOMEMLIMIT=450MiB.

// maxTransferImportHeap is the most Go heap the import may hold at once.
const maxTransferImportHeap = 300 << 20

// transferScaleDB is the import's own database.
const transferScaleDB = "leadhound_scale_transfer"

// transferAdminDSN is a role that may create databases: TEST_TRANSFER_ADMIN_DSN, or the
// local superuser over the socket (make db-local's).
func transferAdminDSN() string {
	if v := os.Getenv("TEST_TRANSFER_ADMIN_DSN"); v != "" {
		return v
	}
	return "postgres://localhost/postgres?sslmode=disable"
}

// withDB is dsn pointed at database name.
func withDB(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// transferTargetDB creates the empty import database with the roles' grants and both
// migration sets, dropped when the test ends, and returns its app-role DSN.
func transferTargetDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, transferAdminDSN())
	if err != nil {
		storetest.SkipOrFail(t, "skipping transfer scale test: admin connection: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	drop := func() error {
		_, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+transferScaleDB+" WITH (FORCE)")
		return err
	}
	if err := drop(); err != nil {
		t.Fatalf("drop %s: %v", transferScaleDB, err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+transferScaleDB+" OWNER leadhound_owner"); err != nil {
		t.Fatalf("create %s: %v", transferScaleDB, err)
	}
	t.Cleanup(func() {
		if err := drop(); err != nil {
			t.Errorf("drop %s: %v", transferScaleDB, err)
		}
	})
	grants, err := os.ReadFile("../../deploy/sql/grants.sql")
	if err != nil {
		t.Fatal(err)
	}
	g, err := pgx.Connect(ctx, withDB(t, transferAdminDSN(), transferScaleDB))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close(context.Background()) }()
	if _, err := g.PgConn().Exec(ctx, string(grants)).ReadAll(); err != nil {
		t.Fatalf("grants: %v", err)
	}
	owner := withDB(t, storetest.ScaleMigrateDSN(), transferScaleDB)
	if err := store.Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := leadstore.Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	return withDB(t, storetest.ScaleDSN(), transferScaleDB)
}

func TestScaleLeadsTransfer(t *testing.T) {
	leadstoretest.ScaleSeeded(t)
	saved := transferReservedTLDs
	transferReservedTLDs = []string{"invalid"} // the seed's sites are on .example
	t.Cleanup(func() { transferReservedTLDs = saved })
	savedOut := stdout
	var out bytes.Buffer
	stdout = &out
	t.Cleanup(func() { stdout = savedOut })
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "transfer")

	// The export reads the seed in one read-only transaction. It is called past the refusal
	// of live sends, which the seed holds on purpose (the integration test covers it).
	src, err := store.Open(ctx, storetest.ScaleDSN())
	if err != nil {
		storetest.SkipOrFail(t, "skipping transfer scale test: open scale store: %v", err)
	}
	t.Cleanup(src.Close)
	exportPeak, exportTook := measure(t, func() error {
		tx, err := src.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '30min'"); err != nil {
			return err
		}
		return exportTx(ctx, tx, dir)
	})
	m, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]int{}
	var size int64
	for _, f := range m.Files {
		rows[f.Name], size = f.Rows, size+f.Bytes
	}
	t.Logf("export: %v, peak heap %.1f MB, %s on disk; rows %v", exportTook.Round(time.Millisecond),
		float64(exportPeak)/(1<<20), humanBytes(size), rows)

	dst, err := store.Open(ctx, transferTargetDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dst.Close)
	importPeak, importTook := measure(t, func() error { return transferImport(ctx, dst.Pool(), dir) })
	t.Logf("import: %v, peak heap %.1f MB (budget %d MB)", importTook.Round(time.Millisecond),
		float64(importPeak)/(1<<20), maxTransferImportHeap>>20)
	if importPeak > maxTransferImportHeap {
		t.Errorf("import: peak heap %.1f MB, over its budget of %d MB", float64(importPeak)/(1<<20), maxTransferImportHeap>>20)
	}

	counts := func() map[string]int {
		got := map[string]int{}
		for _, tbl := range []string{"site", "site_probe", "http_fetch", "fetch_header", "org", "contact_route",
			"agency_finding", "contact_attempt", "lead", "lead_event", "suppression", "run"} {
			var n int
			if err := dst.Pool().QueryRow(ctx, "SELECT count(*) FROM "+tbl).Scan(&n); err != nil { // tbl is a test constant
				t.Fatal(err)
			}
			got[tbl] = n
		}
		return got
	}
	first := counts()
	for tbl, file := range map[string]string{"site": tfSites, "site_probe": tfProbes, "http_fetch": tfFetches, "org": tfOrgs,
		"contact_route": tfRoutes, "agency_finding": tfFindings, "contact_attempt": tfAttempts, "lead": tfLeads,
		"lead_event": tfEvents, "suppression": tfSuppressions} {
		if first[tbl] != rows[file] {
			t.Errorf("%s: %d rows imported, the export has %d", tbl, first[tbl], rows[file])
		}
	}
	var pointed int
	if err := dst.Pool().QueryRow(ctx, `SELECT count(*) FROM site WHERE last_probe_id IS NOT NULL`).Scan(&pointed); err != nil {
		t.Fatal(err)
	}
	t.Logf("import counts %v; sites pointed at their last probe %d", first, pointed)

	// A second import changes nothing; its time is the cost of matching everything.
	againPeak, againTook := measure(t, func() error { return transferImport(ctx, dst.Pool(), dir) })
	t.Logf("second import: %v, peak heap %.1f MB", againTook.Round(time.Millisecond), float64(againPeak)/(1<<20))
	if againPeak > maxTransferImportHeap {
		t.Errorf("second import: peak heap %.1f MB, over its budget", float64(againPeak)/(1<<20))
	}
	if again := counts(); !mapsEqualInt(again, first) {
		t.Errorf("second import changed row counts: %v, after the first %v", again, first)
	}
	b, _ := json.Marshal(m.Files)
	t.Logf("manifest files: %s", b)
}

func mapsEqualInt(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
