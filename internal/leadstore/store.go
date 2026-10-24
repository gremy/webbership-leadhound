// Package leadstore holds the Postgres access for Drupal migration leads: its own embedded
// migration set, versioned in goose_db_version_leads beside the core set, and the
// hand-written queries over the lead tables.
//
// The lead tables live in the same database as the core tables and may reference run and
// http_fetch. The app role may SELECT, INSERT and UPDATE; UPDATE is revoked on the
// append-only tables, and there is no DELETE.
package leadstore

import "github.com/gremy/webbership-leadhound/internal/store"

// Queries holds the lead queries over one store.DBTX, so lead writes can share a
// transaction with the core queries (store.Store.InTx).
type Queries struct {
	db store.DBTX
}

// NewQueries returns the lead query helpers over db.
func NewQueries(db store.DBTX) Queries { return Queries{db: db} }
