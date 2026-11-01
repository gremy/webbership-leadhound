package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// TestResearchCommandParsing: every malformed `leads research` line is a usage error before
// any config or database is read; the well-formed ones parse to their values.
func TestResearchCommandParsing(t *testing.T) {
	ctx := context.Background()
	for _, args := range [][]string{
		{}, {"nope"},
		{"request"}, {"request", "abc"}, {"request", "0"}, {"request", "-3"}, {"request", "1", "2"},
		{"list", "extra"}, {"list", "--status", "bogus"}, {"list", "--limit", "0"},
		{"run", "--max", "0"}, {"run", "extra"}, {"run", "--max", "x"},
	} {
		if err := runLeadsResearch(ctx, args); !errors.Is(err, errUsage) {
			t.Errorf("leads research %q: %v, want a usage error", args, err)
		}
	}
	if id, li, err := parseResearchRequest([]string{"42"}); err != nil || id != 42 || li {
		t.Errorf("request 42: %d %v %v", id, li, err)
	}
	if id, li, err := parseResearchRequest([]string{"--linkedin", "42"}); err != nil || id != 42 || !li {
		t.Errorf("request --linkedin 42: %d %v %v", id, li, err)
	}
	if o, err := parseResearchList([]string{"--status", "capped", "--limit", "7"}); err != nil || o.Status != leadstore.ResearchCapped || o.Limit != 7 {
		t.Errorf("list --status capped --limit 7: %+v %v", o, err)
	}
	if o, err := parseResearchList(nil); err != nil || o.Status != "" || o.Limit != 50 {
		t.Errorf("list: %+v %v", o, err)
	}
	if n, err := parseResearchRun(nil); err != nil || n != defaultResearchMax {
		t.Errorf("run: %d %v", n, err)
	}
	if n, err := parseResearchRun([]string{"--max", "3"}); err != nil || n != 3 {
		t.Errorf("run --max 3: %d %v", n, err)
	}
	if _, ok := leadsCommands["research"]; !ok {
		t.Error(`leadsCommands["research"] is not registered`)
	}
}

// TestResearchRunLiveOffClaimsNothing: without LEADHOUND_LIVE=1 a research run says so and
// exits 0 before it opens the database (the DSN points at a closed port) or builds a caller.
func TestResearchRunLiveOffClaimsNothing(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	saved := newResearchCaller
	t.Cleanup(func() { newResearchCaller = saved })
	newResearchCaller = nil // calling it would panic
	for _, live := range []string{"", "ats"} {
		t.Setenv("LEADHOUND_LIVE", live)
		out := captureStdout(t)
		var stderr bytes.Buffer
		if code := run(context.Background(), []string{"leads", "research", "run"}, &stderr); code != 0 {
			t.Fatalf("LEADHOUND_LIVE=%q: exit %d: %s", live, code, stderr.String())
		}
		if strings.TrimSpace(out.String()) != researchLiveOffWords {
			t.Errorf("LEADHOUND_LIVE=%q: %q, want %q", live, out.String(), researchLiveOffWords)
		}
	}
}

// TestResearchStatusMapping: cap → capped, another error → error, none verified → nothing,
// else found.
func TestResearchStatusMapping(t *testing.T) {
	for _, c := range []struct {
		err      error
		verified int
		want     string
	}{
		{fmt.Errorf("research: org 1: %w", ai.ErrSpendCap), 0, leadstore.ResearchCapped},
		{fmt.Errorf("research: org 1: %w", ai.ErrBadAnswer), 0, leadstore.ResearchError},
		{errors.New("breaker"), 1, leadstore.ResearchError},
		{nil, 0, leadstore.ResearchNothing},
		{nil, 2, leadstore.ResearchFound},
	} {
		if got := researchStatusOf(c.err, c.verified); got != c.want {
			t.Errorf("researchStatusOf(%v, %d) = %s, want %s", c.err, c.verified, got, c.want)
		}
	}
	for _, c := range []struct {
		emails, phones, linkedIn, verified int
		want                               string
	}{
		{1, 0, 0, 1, "1 email"},
		{1, 2, 0, 3, "1 email and 2 phones"},
		{0, 0, 2, 2, "2 LinkedIn pages"},
		{1, 0, 1, 2, "1 email and 1 LinkedIn page"},
		{0, 0, 0, 1, "1 contact, nothing new"},
	} {
		if got := foundWords(c.emails, c.phones, c.linkedIn, c.verified); got != c.want {
			t.Errorf("foundWords(%+v) = %q", c, got)
		}
	}
}

// TestStarredRefusalWords: nothing starred keeps the one line; otherwise one line per
// starred lead, in order, each reason in plain words with its detail and date, and no code
// of the store (no_email_yet, country_rule ...) ever shown.
func TestStarredRefusalWords(t *testing.T) {
	if got := noStarredText(&LeadRefusal{Kind: RefuseNoStarred}); got != noStarredWords {
		t.Errorf("nothing starred: %q", got)
	}
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	st := []leadstore.StarredState{
		{Domain: "a.example", Reason: leadstore.StarredNoEmailYet},
		{Domain: "b.example", Reason: leadstore.StarredLooking},
		{Domain: "c.example", Reason: leadstore.StarredFoundNothing, Date: &day},
		{Domain: "d.example", Reason: leadstore.StarredCountryRule, Detail: "US is off in leads.yaml"},
		{Domain: "e.example", Reason: leadstore.StarredSuppressed},
		{Domain: "f.example", Reason: leadstore.StarredDeferred, Date: &later},
		{Domain: "g.example", Reason: leadstore.StarredRankedOut, Detail: "skip_rank_over is 1000"},
		{Domain: "h.example", Reason: leadstore.StarredNotNew, Detail: "it is skipped"},
		{Domain: "i.example", Reason: leadstore.StarredOther, Detail: "kind d10 is turned off"},
		{Domain: "j.example", OrgID: 9, Reason: leadstore.StarredReady},
		{Domain: "k.example", Reason: leadstore.StarredNoEmailYet, Detail: "research found a contact: the next leads build gives it its channel"},
	}
	st = markOpenElsewhere(st, map[int64]bool{9: true}, []DroppedLead{{Domain: "j.example", Why: "replied in campaign drupal"}})
	got := noStarredText(&LeadRefusal{Kind: RefuseNoStarred, Starred: st})
	want := noStarredHead + `
1. a.example — no email yet
2. b.example — looking for a contact
3. c.example — research found nothing, 20 Oct 2026
4. d.example — country rule: US is off in leads.yaml
5. e.example — suppressed
6. f.example — deferred, until 3 Nov 2026
7. g.example — ranked out: skip_rank_over is 1000
8. h.example — no longer new: it is skipped
9. i.example — left out: kind d10 is turned off
10. j.example — open in another campaign: replied in campaign drupal
11. k.example — no email yet: research found a contact: the next leads build gives it its channel`
	if got != want {
		t.Errorf("refusal words:\n%s\nwant:\n%s", got, want)
	}
	for _, code := range []string{"no_email_yet", "found_nothing", "country_rule", "ranked_out", "not_new", "open_elsewhere"} {
		if strings.Contains(got, code) {
			t.Errorf("refusal shows the code %q", code)
		}
	}
}

// TestResearchStoreErrorWords: a store error is named by its SQLSTATE (and constraint) or its
// sentinel's name, never by its own text, which can quote a found value.
func TestResearchStoreErrorWords(t *testing.T) {
	const leak = "leak-sentinel" + "@" + "example.invalid"
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("leadstore: insert route: %w", &pgconn.PgError{Code: "23505", ConstraintName: "contact_route_org_kind_value_key",
			Message: "duplicate key " + leak, Detail: "Key (org_id, kind, value)=(1, email, " + leak + ") already exists."}),
			"(SQLSTATE 23505 on contact_route_org_kind_value_key)"},
		{&pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type bigint: "` + leak + `"`}, "(SQLSTATE 22P02)"},
		{fmt.Errorf("leadstore: insert fact %s: %w", leak, leadstore.ErrFactTooLong), "(ErrFactTooLong)"},
		{fmt.Errorf("store: %s: %w", leak, store.ErrFetchBodyMismatch), "(ErrFetchBodyMismatch)"},
		{fmt.Errorf("x %s: %w", leak, context.Canceled), "(context canceled)"},
		{errors.New("driver said " + leak), "(unclassified)"},
	} {
		got := researchStoreError("storing a route", c.err)
		if strings.Contains(got.Error(), leak) || strings.Contains(fmt.Sprintf("%+v %#v", got, got), leak) {
			t.Errorf("%v: words %q name the value", c.err, got)
		}
		if !strings.Contains(got.Error(), "the store failed storing a route "+c.want) {
			t.Errorf("%v: words %q, want %q", c.err, got, c.want)
		}
		if errors.Unwrap(got) != nil {
			t.Errorf("%v: researchStoreError unwraps to the raw error", c.err)
		}
	}
	if researchStoreError("x", nil) != nil {
		t.Error("nil error: want nil")
	}
}

// failTransport fails the test on every request it is asked to make.
type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("asked %s", r.URL)
	return nil, errors.New("no network in this test")
}

// recordHostTransport answers every request with a 200 and keeps the hosts asked.
type recordHostTransport struct{ hosts *[]string }

func (h recordHostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*h.hosts = append(*h.hosts, r.URL.Hostname())
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
}

// TestLinkedInRefusedOnTheLeadsSideOnly: every client the leads commands build refuses
// linkedin.com; the job search's reaches it as it always has.
func TestLinkedInRefusedOnTheLeadsSideOnly(t *testing.T) {
	cfg := config.Config{Live: config.LiveAll}
	mk, err := siteClients(cfg, fetchFlags{}, rate.NewLimiter(rate.Inf, 1))
	if err != nil {
		t.Fatal(err)
	}
	site := mk(verifyRecorder{})
	site.HTTP.Transport = failTransport{t}
	if _, err := site.Get(context.Background(), fetch.KindSite, "https://uk.linkedin.com/company/x"); !errors.Is(err, fetch.ErrLinkedInRefused) {
		t.Errorf("site client: %v, want ErrLinkedInRefused", err)
	}
	mkJob, _, err := linkedinClients(cfg, fetchFlags{}, fetch.Budgets{fetch.KindCompany: 1})
	if err != nil {
		t.Fatal(err)
	}
	job := mkJob(verifyRecorder{})
	job.Jitter = 0
	var hosts []string
	job.HTTP.Transport = recordHostTransport{&hosts}
	if _, err := job.Get(context.Background(), fetch.KindCompany, "https://www.linkedin.com/company/x"); err != nil || !slices.Equal(hosts, []string{"www.linkedin.com"}) {
		t.Errorf("job client: %v, asked %v", err, hosts)
	}
}
