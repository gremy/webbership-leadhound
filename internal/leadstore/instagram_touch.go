package leadstore

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Loyalty vendor touches and followers lists (migration 00033): what a loyalty vendor's own
// Instagram shows about another account. Both tables are append-only for the app; a re-import
// of the same capture stores nothing twice. The intent sync reads them back by handle.

// The kinds of a vendor touch (ig_vendor_touch_kind_check). A comment is the account
// commenting on the vendor's post (Ref is the comment id); a usertag or mention is the vendor's
// post tagging or @-naming the account (Ref is the post code); tagged is the account's own post
// tagging the vendor (Ref is the post code).
const (
	IGTouchComment = "comment"
	IGTouchUsertag = "usertag"
	IGTouchMention = "mention"
	IGTouchTagged  = "tagged"
)

// IGMaxTouchText is the most characters a comment's stored text holds (ig_vendor_touch_text_check).
const IGMaxTouchText = 300

// igRefRE is the shape of a comment id or a post code (ig_vendor_touch_ref_check).
var igRefRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// IGTouch is one dated contact between a vendor's account and Handle (an ig_vendor_touch row).
// Text is a comment's own text, written only for a strongly matched account and only on a
// comment; FullName only for a matched or reviewed account. Empty stores NULL. VendorHandle is
// filled on read; InsertIGTouches takes the vendor as its own argument and ignores it.
type IGTouch struct {
	VendorHandle string
	Handle       string
	Kind         string // IGTouchComment, IGTouchUsertag, IGTouchMention or IGTouchTagged
	Ref          string // the comment id, or the post code
	PostCode     string
	At           time.Time // the comment's created_at, or the post's taken_at
	Text         string
	FullName     string
}

// InsertIGTouches stores the touches of one capture (sha, capturedAt) of vendorHandle's account
// and returns how many rows were new: a touch already stored (the same vendor, kind, handle and
// ref) is not stored twice, whichever capture it came from. A touch outside its shape (a handle
// that is not one, an unknown kind, a ref or post code of the wrong shape, text on a kind other
// than comment or longer than IGMaxTouchText, no date) fails the whole call with ErrBadIG.
func (q Queries) InsertIGTouches(ctx context.Context, vendorHandle string, capturedAt time.Time, sha []byte, touches []IGTouch) (int64, error) {
	vendor, err := igHandle(vendorHandle)
	if err != nil {
		return 0, fmt.Errorf("leadstore: insert ig touches: %w", err)
	}
	if len(sha) != 32 || capturedAt.IsZero() {
		return 0, fmt.Errorf("leadstore: insert ig touches of %s: %w: %d-byte hash", vendor, ErrBadIG, len(sha))
	}
	if len(touches) == 0 {
		return 0, nil
	}
	n := len(touches)
	handles, kinds, refs, posts := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	texts, names := make([]string, n), make([]string, n)
	ats := make([]time.Time, n)
	for i, t := range touches {
		if handles[i], err = igHandle(t.Handle); err != nil {
			return 0, fmt.Errorf("leadstore: insert ig touches of %s: %w", vendor, err)
		}
		if !slices.Contains([]string{IGTouchComment, IGTouchUsertag, IGTouchMention, IGTouchTagged}, t.Kind) ||
			!igRefRE.MatchString(t.Ref) || !igRefRE.MatchString(t.PostCode) || t.At.IsZero() ||
			(t.Text != "" && t.Kind != IGTouchComment) || utf8.RuneCountInString(t.Text) > IGMaxTouchText {
			return 0, fmt.Errorf("leadstore: insert ig touches of %s: %w: touch %d of %s, kind %q", vendor, ErrBadIG, i, handles[i], t.Kind)
		}
		kinds[i], refs[i], posts[i], ats[i], texts[i], names[i] = t.Kind, t.Ref, t.PostCode, t.At, t.Text, t.FullName
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO ig_vendor_touch (vendor_handle, handle, kind, ref, post_code, at, text, full_name, capture_sha256, captured_at)
		 SELECT $1, u.h, u.k, u.r, u.p, u.a, nullif(u.t, ''), nullif(u.n, ''), $2, $3
		 FROM unnest($4::text[], $5::text[], $6::text[], $7::text[], $8::timestamptz[], $9::text[], $10::text[]) AS u(h, k, r, p, a, t, n)
		 ON CONFLICT (vendor_handle, kind, handle, ref) DO NOTHING`,
		vendor, sha, capturedAt, handles, kinds, refs, posts, ats, texts, names)
	if err != nil {
		return 0, fmt.Errorf("leadstore: insert ig touches of %s: %w", vendor, err)
	}
	return tag.RowsAffected(), nil
}

// IGFollowerOf is one vendor account a handle follows, read from that vendor's followers list,
// with the latest capture that showed it.
type IGFollowerOf struct {
	Owner      string    `db:"owner"`
	CapturedAt time.Time `db:"captured_at"`
}

// IGFollowerVendors maps each of handles (as stored; a value that is not a handle is left out)
// to the accounts whose followers lists hold it, by owner, each with its latest capture. A
// handle on no followers list is absent.
func (q Queries) IGFollowerVendors(ctx context.Context, handles []string) (map[string][]IGFollowerOf, error) {
	type row struct {
		Handle string `db:"handle"`
		IGFollowerOf
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT handle, list_owner AS owner, max(captured_at) AS captured_at FROM ig_follow
		 WHERE direction = '`+IGFollowers+`' AND handle = ANY($1)
		 GROUP BY handle, list_owner ORDER BY handle, list_owner`, igHandles(handles))
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig follower vendors: %w", err)
	}
	out := map[string][]IGFollowerOf{}
	for _, r := range rows {
		out[r.Handle] = append(out[r.Handle], r.IGFollowerOf)
	}
	return out, nil
}

// IGTouchesFor maps each of handles (as stored; a value that is not a handle is left out) to its
// vendor touches dated since or later, newest first. A handle with none is absent.
func (q Queries) IGTouchesFor(ctx context.Context, handles []string, since time.Time) (map[string][]IGTouch, error) {
	type row struct {
		VendorHandle string    `db:"vendor_handle"`
		Handle       string    `db:"handle"`
		Kind         string    `db:"kind"`
		Ref          string    `db:"ref"`
		PostCode     string    `db:"post_code"`
		At           time.Time `db:"at"`
		Text         string    `db:"text"`
		FullName     string    `db:"full_name"`
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT vendor_handle, handle, kind, ref, post_code, at, coalesce(text, '') AS text, coalesce(full_name, '') AS full_name
		 FROM ig_vendor_touch WHERE handle = ANY($1) AND at >= $2 ORDER BY handle, at DESC, id DESC`, igHandles(handles), since)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ig touches: %w", err)
	}
	out := map[string][]IGTouch{}
	for _, r := range rows {
		out[r.Handle] = append(out[r.Handle], IGTouch(r))
	}
	return out, nil
}
