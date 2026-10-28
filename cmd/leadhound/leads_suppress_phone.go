package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/dial"
	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// phoneSuppressReasons are the reasons a phone suppression takes from the CLI.
var phoneSuppressReasons = []string{leadstore.ReasonDoNotCall, leadstore.ReasonTPS, leadstore.ReasonCTPS}

// runSuppressAddPhone is `leads suppress add phone +NUMBER --reason do_not_call|tps|ctps`:
// a phone suppression of the account, stored hashed (leadstore.PhoneSuppressionValue), never
// the number itself. The number must carry its country code, so it means one line; every
// stored phone fact that dials as it (kept in national form, say) is suppressed too, since
// a call dials the fact's own value. It never prints the number.
func runSuppressAddPhone(ctx context.Context, raw, reason string) error {
	if reason == leadstore.ReasonManual { // the flag's default for the other kinds
		return usageError("a phone suppression needs --reason do_not_call, tps or ctps")
	}
	if !slices.Contains(phoneSuppressReasons, reason) {
		return usageError("--reason %q is not one of do_not_call, tps, ctps", reason)
	}
	_, norm, ok := enrich.NormalizePhone(raw)
	if !ok || !strings.HasPrefix(norm, "+") {
		return usageError("type the number with its country code (+44 …): a number without one could be any country's")
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	res, err := suppressPhone(ctx, s, norm, reason, suppressFromCLI)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Suppressed phone (%s, stored hashed): %d new · %s reached, %d moved to suppressed, %s cancelled.\n",
		reason, res.Recorded, plural(res.Leads, "lead", "leads"), res.Moved, plural(int(res.Cancelled), "pending message", "pending messages"))
	return nil
}

// phoneSuppressed sums the suppressions one phone suppression wrote.
type phoneSuppressed struct {
	Recorded, Leads, Moved int
	Cancelled              int64
}

// suppressPhone writes phone suppressions of the account for the E.164 number e164 and every
// stored phone fact value that dials as it, with reason and source, in one transaction.
func suppressPhone(ctx context.Context, s *store.Store, e164, reason, source string) (phoneSuppressed, error) {
	var res phoneSuppressed
	tx, err := s.Pool().Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
	lq := leadstore.NewQueries(tx)
	err = func() error {
		forms, err := phoneForms(ctx, tx, e164)
		if err != nil {
			return err
		}
		for _, n := range forms {
			r, err := lq.Suppress(ctx, leadstore.DefaultAccountID, leadstore.SuppressPhone, leadstore.PhoneSuppressionValue(n), reason, source)
			if err != nil {
				return err
			}
			if r.Recorded {
				res.Recorded++
			}
			res.Leads += r.Leads
			res.Moved += r.Moved
			res.Cancelled += r.Cancelled
		}
		return nil
	}()
	if err != nil {
		return phoneSuppressed{}, err
	}
	return res, tx.Commit(ctx)
}

// phoneForms is e164 and every distinct stored phone fact value that dials as it with its
// org's country.
func phoneForms(ctx context.Context, db store.DBTX, e164 string) ([]string, error) {
	rows, err := db.Query(ctx,
		`SELECT DISTINCT f.normalized, o.country FROM org_fact f JOIN org o ON o.id = f.org_id
		 WHERE f.kind = 'phone' AND right(f.normalized, 7) = right($1, 7)`, e164)
	if err != nil {
		return nil, err
	}
	type row struct{ Normalized, Country string }
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		return nil, err
	}
	forms := []string{e164}
	for _, r := range got {
		if r.Normalized != e164 && dial.Number(r.Normalized, r.Country) == e164 && !slices.Contains(forms, r.Normalized) {
			forms = append(forms, r.Normalized)
		}
	}
	return forms, nil
}
