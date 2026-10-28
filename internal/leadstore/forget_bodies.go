package leadstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ForgottenPages counts what ForgetOrgPages did; a second run on the same org counts zero.
type ForgottenPages struct {
	BodiesBlanked int64 // http_fetch bodies emptied (the rows, their URL, size and hash stay)
	OtherFacts    int64 // org_fact description and social rows deleted
}

// ForgetOrgPages is the second half of `leads forget` beside ForgetOrg: it empties the kept
// page bodies of org orgID (the pages of its domain and its subdomains, and every page its
// facts, routes, probes or source cite), so no copy of a number read from them is left, and
// deletes its description and social facts. Run it on the owner's connection, like
// ForgetOrg (the app role may not delete facts). The http_fetch rows themselves stay: other
// rows reference them, and their URL, status, size and hash name no person. Idempotent: an
// emptied body is not counted again. A missing org is ErrOrgNotFound.
func (q Queries) ForgetOrgPages(ctx context.Context, orgID int64) (ForgottenPages, error) {
	var res ForgottenPages
	err := q.inTx(ctx, func(tq Queries) error {
		var domain string
		err := tq.db.QueryRow(ctx, `SELECT domain FROM org WHERE id = $1 FOR UPDATE`, orgID).Scan(&domain)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOrgNotFound
		}
		if err != nil {
			return err
		}
		tag, err := tq.db.Exec(ctx,
			`WITH o AS (SELECT id, source_fetch_id, site_id FROM org WHERE id = $1),
			 cited AS (
			   SELECT o.source_fetch_id AS fetch_id FROM o WHERE o.source_fetch_id IS NOT NULL
			   UNION SELECT f.fetch_id FROM org_fact f JOIN o ON f.org_id = o.id
			   UNION SELECT r.fetch_id FROM contact_route r JOIN o ON r.org_id = o.id
			   UNION SELECT x.fetch_id FROM o JOIN lead l ON l.org_id = o.id JOIN site_probe p ON p.id = l.probe_id
			     CROSS JOIN LATERAL unnest(p.fetch_ids) AS x(fetch_id)
			   UNION SELECT x.fetch_id FROM o JOIN site s ON s.id = o.site_id JOIN site_probe p ON p.id = s.last_probe_id
			     CROSS JOIN LATERAL unnest(p.fetch_ids) AS x(fetch_id)
			 )
			 UPDATE http_fetch h SET body = ''::bytea
			 WHERE length(h.body) > 0 AND (h.id IN (SELECT fetch_id FROM cited)
			   OR `+hostIsSQL("h.url", "$2")+` OR `+hostIsSQL("h.final_url", "$2")+`)`, orgID, domain)
		if err != nil {
			return err
		}
		res.BodiesBlanked = tag.RowsAffected()
		tag, err = tq.db.Exec(ctx, `DELETE FROM org_fact WHERE org_id = $1 AND kind IN ('description', 'social')`, orgID)
		if err != nil {
			return err
		}
		res.OtherFacts = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return ForgottenPages{}, fmt.Errorf("leadstore: forget org %d pages: %w", orgID, err)
	}
	return res, nil
}

// hostIsSQL is a SQL condition true when the URL expression u's host is domain (an SQL
// expression) or one of its subdomains, case folded.
func hostIsSQL(u, domain string) string {
	host := `lower(substring(` + u + ` from '^[A-Za-z][A-Za-z0-9+.-]*://(?:[^/@]*@)?([^/:?#]+)'))`
	return `(` + host + ` = lower(` + domain + `) OR ` + host + ` LIKE '%.' || lower(` + domain + `))`
}
