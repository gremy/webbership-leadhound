package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
)

// InsertFetch writes one http_fetch row for runID and returns its id. The body is kept
// exactly as it arrived, except for a board answer (fetch.KindBoard): thousands are read a
// day, so only its size and hash are kept and AttachFetchBody adds the body when the parser
// broke. final_url is empty when it equals the URL; a response without a hash gets one
// computed; a transport error (status 0) keeps body_sha256 and body_size NULL.
func (q Queries) InsertFetch(ctx context.Context, runID int64, source string, r fetch.Record) (int64, error) {
	finalURL := r.FinalURL
	if finalURL == r.URL {
		finalURL = ""
	}
	body := r.Body
	if body == nil {
		body = []byte{} // NOT NULL: a nil slice would be sent as NULL
	}
	sum := r.SHA256
	var size *int
	if r.Status != 0 {
		if sum == nil {
			s := sha256.Sum256(body)
			sum = s[:]
		}
		n := len(body)
		size = &n
	}
	if r.Kind == fetch.KindBoard {
		body = []byte{}
	}
	at := r.FetchedAt
	if at.IsZero() {
		at = time.Now()
	}
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO http_fetch (run_id, source, kind, url, final_url, status, fetched_at, body, content_type, body_sha256, body_size, error)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING id`,
		runID, source, string(r.Kind), r.URL, finalURL, r.Status, at, body, r.ContentType, sum, size, r.Err).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert fetch %s: %w", r.Kind, err)
	}
	return id, nil
}

// ErrFetchBodyMismatch is returned by AttachFetchBody when no fetch row has that id and
// that body's hash.
var ErrFetchBodyMismatch = errors.New("store: no fetch with that id and body hash")

// AttachFetchBody keeps body on the http_fetch row fetchID, for a board answer whose parser
// broke (InsertFetch keeps no board bodies). The row's body_sha256 must be the body's hash,
// so only the bytes that arrived can be attached; otherwise ErrFetchBodyMismatch.
func (q Queries) AttachFetchBody(ctx context.Context, fetchID int64, body []byte) error {
	if body == nil {
		body = []byte{}
	}
	sum := sha256.Sum256(body)
	tag, err := q.db.Exec(ctx,
		`UPDATE http_fetch SET body = $2 WHERE id = $1 AND body_sha256 = $3`, fetchID, body, sum[:])
	if err != nil {
		return fmt.Errorf("store: attach fetch body %d: %w", fetchID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: attach fetch body %d: %w", fetchID, ErrFetchBodyMismatch)
	}
	return nil
}

// FetchRecorder is the fetch.Recorder over the http_fetch table for one run and source. It
// remembers the id of the latest row per requested URL, so the collector can link an
// observation to the fetch it came from. It is safe for concurrent use.
type FetchRecorder struct {
	q      Queries
	runID  int64
	source string

	mu    sync.Mutex
	last  int64
	byURL map[string]int64
}

var _ fetch.Recorder = (*FetchRecorder)(nil)

// NewFetchRecorder returns a recorder writing rows for runID and source through q.
func NewFetchRecorder(q Queries, runID int64, source string) *FetchRecorder {
	return &FetchRecorder{q: q, runID: runID, source: source, byURL: map[string]int64{}}
}

// Record writes r as an http_fetch row.
func (f *FetchRecorder) Record(ctx context.Context, r fetch.Record) error {
	id, err := f.q.InsertFetch(ctx, f.runID, f.source, r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.last = id
	f.byURL[r.URL] = id
	f.mu.Unlock()
	return nil
}

// LastID is the id of the latest row this recorder wrote, or 0 before any.
func (f *FetchRecorder) LastID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// LastIDFor is the id of the latest row written for the requested URL rawURL.
func (f *FetchRecorder) LastIDFor(rawURL string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byURL[rawURL]
	return id, ok
}
