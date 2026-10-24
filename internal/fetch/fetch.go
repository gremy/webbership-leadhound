// Package fetch is leadhound's one road to the network.
//
// A Client paces every request through a limiter shared by the whole run, sends the
// headers its Profile names, records every response (and every transport error) through
// a Recorder before returning it, and stops for good once its Breaker opens: two 429s in
// a row, a blocked page, too many failures, or a spent budget. Without the matching
// LEADHOUND_LIVE value the client refuses every host but loopback before any request.
package fetch

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Kind is what a request fetches. The values match the http_fetch.kind CHECK constraint.
type Kind string

// The kinds of request.
const (
	KindSearch  Kind = "search"
	KindDetail  Kind = "detail" // a job page; recheck draws from this budget too
	KindBoard   Kind = "board"
	KindCanary  Kind = "canary"
	KindCompany Kind = "company"
	KindSite    Kind = "site"
	KindRobots  Kind = "robots"
)

func (k Kind) valid() bool {
	switch k {
	case KindSearch, KindDetail, KindBoard, KindCanary, KindCompany, KindSite, KindRobots:
		return true
	}
	return false
}

// Errors returned by Get. Each is wrapped with the kind and URL of the request.
var (
	ErrBreakerOpen  = errors.New("fetch: breaker open")
	ErrLiveOff      = errors.New("fetch: live fetching is off for this host")
	ErrStatus       = errors.New("fetch: unexpected status")
	ErrBodyTooLarge = errors.New("fetch: body over the size limit")
	ErrUnknownKind  = errors.New("fetch: unknown kind")
	ErrBadURL       = errors.New("fetch: not an http or https URL with a host")
	ErrNoRecorder   = errors.New("fetch: no recorder, so nothing may be fetched")
)

// Response is one HTTP response as it arrived.
type Response struct {
	Status      int
	FinalURL    string // the URL after redirects
	Body        []byte // at most the client's body limit
	ContentType string
	Header      http.Header // the allowlisted response headers only (see headerAllowlist)
	FetchedAt   time.Time
}

// Record is one http_fetch row: a response, or a transport error with Status 0.
type Record struct {
	Kind        Kind
	URL         string
	FinalURL    string // "" when it equals URL
	Status      int    // 0 = transport error
	Body        []byte
	ContentType string
	SHA256      []byte      // nil when there is no response
	Err         string      // "" when nothing went wrong
	Header      http.Header // the allowlisted response headers; nil when there is no response
	FetchedAt   time.Time
}

// Recorder stores Records. The store implements it over the http_fetch table; Get calls it
// for every response before returning, and returns its error.
type Recorder interface {
	Record(ctx context.Context, r Record) error
}

// Clock is the time source Get waits on, so tests can count waits without waiting.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
