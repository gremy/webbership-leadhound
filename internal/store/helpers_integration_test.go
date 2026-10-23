//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// appStore empties the test database and opens a Store as the app role, which has no DELETE:
// every helper exercised through it is proven to need none.
func appStore(t *testing.T) *store.Store {
	t.Helper()
	storetest.ResetDB(t)
	s, err := store.Open(context.Background(), storetest.AppDSN())
	if err != nil {
		storetest.SkipOrFail(t, "skipping integration test: open app store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// ownerExec runs a statement as the owner, for test setup the app role may not do (such as
// back-dating a timestamp).
func ownerExec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := storetest.OwnerConn(t).Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("owner exec %q: %v", q, err)
	}
}
