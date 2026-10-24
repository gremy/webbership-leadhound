//go:build integration

package store_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestBoardTableTakesTheSixATSs: 00005 lets the registry hold SmartRecruiters, Teamtailor and
// Personio boards, and Personio's 'com' region, while every other value is still refused.
func TestBoardTableTakesTheSixATSs(t *testing.T) {
	ctx := context.Background()
	storetest.ResetDB(t)
	app := storetest.AppConn(t)
	for _, row := range [][2]string{
		{"greenhouse", ""}, {"lever", "eu"}, {"ashby", ""},
		{"smartrecruiters", ""}, {"teamtailor", ""}, {"personio", ""}, {"personio", "com"},
	} {
		if _, err := app.Exec(ctx, `INSERT INTO board (ats, slug, region, origin) VALUES ($1, 'example', $2, 'manual')`, row[0], row[1]); err != nil {
			t.Errorf("insert %s region %q: %v", row[0], row[1], err)
		}
	}
	for name, q := range map[string]string{
		"ats":         `INSERT INTO board (ats, slug, origin) VALUES ('workable', 'x', 'manual')`,
		"ats case":    `INSERT INTO board (ats, slug, origin) VALUES ('Teamtailor', 'x', 'manual')`,
		"region":      `INSERT INTO board (ats, slug, region, origin) VALUES ('personio', 'x', 'de', 'manual')`,
		"region case": `INSERT INTO board (ats, slug, region, origin) VALUES ('personio', 'x', 'COM', 'manual')`,
	} {
		if _, err := app.Exec(ctx, q); sqlState(err) != "23514" {
			t.Errorf("%s: err = %v, want 23514 check_violation", name, err)
		}
	}
}

// TestATSMoreMigrationDown: 00005's down section, run in a transaction that is rolled back,
// puts the three-ATS checks back.
func TestATSMoreMigrationDown(t *testing.T) {
	ctx := context.Background()
	storetest.ResetDB(t)
	raw, err := os.ReadFile("migrations/00005_ats_more.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("00005 has no down section")
	}
	owner := storetest.OwnerConn(t)
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT s`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO board (ats, slug, origin) VALUES ('teamtailor', 'x', 'manual')`); sqlState(err) != "23514" {
		t.Errorf("teamtailor after down: err = %v, want 23514", err)
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT s`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO board (ats, slug, region, origin) VALUES ('lever', 'x', 'eu', 'manual')`); err != nil {
		t.Errorf("lever eu after down: %v", err)
	}
}
