//go:build integration

package main

import (
	"context"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/research"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestRetiredContactsInManualAndResearch: the manual list leaves out a form message to a retired
// route, and research finding a retired number again records no line for it, only its cost; a
// claim brings both back.
func TestRetiredContactsInManualAndResearch(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	owner := storetest.OwnerConn(t)
	id := func(q string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := owner.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	const page, number = "https://retired.localhost/contact", "+441632960071"
	agency, err := leadstore.NewQueries(s.Pool()).UpsertAgency(ctx, leadstore.AgencyImport{Name: "Retired", Website: "https://retired.localhost/", Country: "GB"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	org := agency.OrgID
	fetchRun := id(`INSERT INTO run (command) VALUES ('collect') RETURNING id`)
	page0 := id(`INSERT INTO http_fetch (run_id, source, kind, url, status, fetched_at, body_sha256)
		VALUES ($1, 'linkedin', 'site', $2, 200, now(), '\x00') RETURNING id`, fetchRun, page)
	route := id(`INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by) VALUES ($1, 'form', $2, $3, 'contacts@1') RETURNING id`, org, page, page0)
	fact := id(`INSERT INTO org_fact (org_id, kind, value, normalized, fetch_id, derived_by) VALUES ($1, 'phone', $2, $2, $3, 'enrich@1') RETURNING id`, org, number, page0)
	lead := id(`INSERT INTO lead (org_id, kind, channel, route_id, state, campaign_id) VALUES ($1, 'hand', 'form', $2, 'batched', 1) RETURNING id`, org, route)
	batch := id(`INSERT INTO outreach_batch (shadow, content_sha256, templates_sha256, approved_at, approved_hash)
		VALUES (false, '\x00', '\x00', now(), '\x00') RETURNING id`)
	mb := seedMailbox(t, leadstore.NewQueries(s.Pool()), testUser)
	msg := id(`INSERT INTO outreach_message (batch_id, lead_id, step, channel, to_address, body_text, slots, mailbox_id, state)
		VALUES ($1, $2, 0, 'form', $3, 'text', '{}', $4, 'approved') RETURNING id`, batch, lead, page, mb.ID)

	mark := func(column string, target int64, m string) {
		t.Helper()
		// Written directly: a retire through the store would cancel the message (TestRetireCancelsUnsent),
		// and this is the list's own check on a message that slipped past it.
		ownerExecCmd(t, `INSERT INTO contact_mark (`+column+`, mark, actor) VALUES ($1, $2, 'operator')`, target, m)
	}
	listed := func() bool {
		t.Helper()
		items, err := manualItems(ctx, s.Pool())
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.MessageID == msg {
				return it.Route == page
			}
		}
		return false
	}
	found := func() (lines, costOnly int64) {
		t.Helper()
		rr := researchRun{pool: s.Pool()}
		res := research.Result{Model: "fake", CostMicros: 7, Phones: []research.Phone{{
			Fact: leadstore.FactRow{OrgID: org, Kind: leadstore.FactPhone, Value: number, Normalized: number}, LineKind: leadstore.LineDirect}}}
		if err := rr.recordPhoneResearch(ctx, org, res); err != nil {
			t.Fatal(err)
		}
		return id(`SELECT count(*) FROM phone_research WHERE org_id = $1 AND fact_id = $2`, org, fact),
			id(`SELECT count(*) FROM phone_research WHERE org_id = $1 AND fact_id IS NULL AND cost_micros = 7`, org)
	}

	if !listed() {
		t.Error("the form message is not listed before any mark")
	}
	if l, c := found(); l != 1 || c != 0 {
		t.Errorf("research on a live number: %d lines, %d cost rows, want 1 and 0", l, c)
	}
	mark("route_id", route, leadstore.MarkRetire)
	mark("fact_id", fact, leadstore.MarkRetire)
	if listed() {
		t.Error("the form message to a retired route is listed")
	}
	if l, c := found(); l != 1 || c != 1 {
		t.Errorf("research on a retired number: %d lines, %d cost rows, want 1 and 1", l, c)
	}
	mark("route_id", route, leadstore.MarkClaim)
	mark("fact_id", fact, leadstore.MarkClaim)
	if !listed() {
		t.Error("the form message is not listed after the claim")
	}
	if l, c := found(); l != 2 || c != 1 {
		t.Errorf("research on a claimed number: %d lines, %d cost rows, want 2 and 1", l, c)
	}
}
