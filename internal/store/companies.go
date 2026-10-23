package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrEmptyName is returned for a company name with no letters or digits.
var ErrEmptyName = errors.New("store: company name has no letters or digits")

// NormName is a company's matching key: lower case, trimmed, with every run of punctuation,
// symbols and spaces collapsed to one space.
func NormName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// UpsertCompany returns the id of the company whose NormName matches name, creating it with
// name and sourceURL when new. linkedinSlug is stored only while the row has none: the first
// slug seen stays.
func (q Queries) UpsertCompany(ctx context.Context, name, sourceURL, linkedinSlug string) (int64, error) {
	norm := NormName(name)
	if norm == "" {
		return 0, fmt.Errorf("store: upsert company %q: %w", name, ErrEmptyName)
	}
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO company (name, norm_name, source_url, linkedin_slug) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (norm_name) DO UPDATE SET linkedin_slug =
		   CASE WHEN company.linkedin_slug = '' THEN EXCLUDED.linkedin_slug ELSE company.linkedin_slug END
		 RETURNING id`, strings.TrimSpace(name), norm, sourceURL, linkedinSlug).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: upsert company %q: %w", name, err)
	}
	return id, nil
}
