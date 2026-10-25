//go:build integration

package store_test

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// hotIndexes are the indexes 00006 builds for the hot lookups: the score queue's gate check,
// the funnel's date filters, the decision join and the bounded Tracked list.
var hotIndexes = []string{
	"gate_result_observation_idx",
	"gate_result_created_idx",
	"decision_created_idx",
	"decision_job_observation_idx",
	"job_first_seen_idx",
	"status_event_active_idx",
}

const hotIndexesFile = "migrations/00006_hot_indexes.sql"

// TestHotIndexesValid proves every hot index exists after migrate and is usable: a concurrent
// build that failed leaves an index behind that is not valid, which the planner never uses.
func TestHotIndexesValid(t *testing.T) {
	storetest.Migrated(t)
	conn := storetest.AppConn(t)
	for _, name := range hotIndexes {
		var valid, ready bool
		err := conn.QueryRow(context.Background(),
			`SELECT i.indisvalid, i.indisready FROM pg_index i
			 JOIN pg_class c ON c.oid = i.indexrelid
			 JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = 'public' AND c.relname = $1`, name).Scan(&valid, &ready)
		if err != nil {
			t.Errorf("index %s: %v", name, err)
			continue
		}
		if !valid || !ready {
			t.Errorf("index %s: valid=%v ready=%v, want both true", name, valid, ready)
		}
	}
}

// TestHotIndexesRetrySafe reads the migration itself: it runs outside a transaction (CREATE
// INDEX CONCURRENTLY cannot run inside one), bounds its lock wait, and drops each index before
// building it, so a retry after a failed build replaces the invalid index it left.
func TestHotIndexesRetrySafe(t *testing.T) {
	b, err := os.ReadFile(hotIndexesFile)
	if err != nil {
		t.Fatalf("read %s: %v", hotIndexesFile, err)
	}
	src := string(b)
	if !strings.HasPrefix(src, "-- +goose NO TRANSACTION\n") {
		t.Errorf("%s does not start with -- +goose NO TRANSACTION", hotIndexesFile)
	}
	up, _, ok := strings.Cut(src, "-- +goose Down")
	if !ok {
		t.Fatalf("%s has no -- +goose Down", hotIndexesFile)
	}
	if !regexp.MustCompile(`(?m)^SET lock_timeout = '\d+s';$`).MatchString(up) {
		t.Errorf("%s does not set lock_timeout before its builds", hotIndexesFile)
	}
	if regexp.MustCompile(`(?im)^\s*BEGIN\b`).MatchString(up) {
		t.Errorf("%s opens a transaction", hotIndexesFile)
	}
	for _, name := range hotIndexes {
		drop := strings.Index(up, "DROP INDEX CONCURRENTLY IF EXISTS "+name+";")
		create := strings.Index(up, "CREATE INDEX CONCURRENTLY "+name+" ")
		switch {
		case create < 0:
			t.Errorf("%s: no CREATE INDEX CONCURRENTLY %s", hotIndexesFile, name)
		case drop < 0 || drop > create:
			t.Errorf("%s: no DROP INDEX CONCURRENTLY IF EXISTS %s before its CREATE", hotIndexesFile, name)
		}
	}
}
