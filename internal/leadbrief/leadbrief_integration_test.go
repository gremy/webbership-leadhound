//go:build integration

package leadbrief_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/leadbrief"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The brief's day, read in Bucharest: yesterday is 22 October there.
var (
	loc, _    = time.LoadLocation("Europe/Bucharest")
	day       = time.Date(2026, 10, 23, 7, 0, 0, 0, loc)
	yesterday = time.Date(2026, 10, 22, 0, 0, 0, 0, loc)
)

// at is yesterday at hour h, local; a negative h is before yesterday.
func at(h int) time.Time { return yesterday.Add(time.Duration(h) * time.Hour) }

// The fixture's addresses, built so no whole address sits in the source; none of them,
// nor their local parts, may reach the brief.
var (
	alphaTo = "ilinca.vasilescu" + "@" + "alpha.example"
	betaTo  = "marten.oakhollow" + "@" + "beta.example"
	gammaTo = "office" + "@" + "gamma.example"
	deltaTo = "ottoline" + "@" + "delta.invalid"
	selfTo  = "me" + "@" + "self.invalid"
)

type fixture struct {
	t     *testing.T
	owner *pgx.Conn
	run   int64
}

func (f fixture) id(sql string, args ...any) int64 {
	f.t.Helper()
	var id int64
	if err := f.owner.QueryRow(context.Background(), sql+" RETURNING id", args...).Scan(&id); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

// lead adds an org on domain (kind site_owner, or selftest) and its lead.
func (f fixture) lead(domain string, selftest bool) int64 {
	kind, source, lkind := "site_owner", "probe", "hand"
	if selftest {
		kind, source, lkind = "selftest", "selftest", "selftest"
	}
	org := f.id(`INSERT INTO org (kind, domain, source) VALUES ($1, $2, $3)`, kind, domain, source)
	return f.id(`INSERT INTO lead (org_id, kind, channel, state) VALUES ($1, $2, 'email', 'contacted')`, org, lkind)
}

func (f fixture) batch(shadow, approved bool) int64 {
	var appr *time.Time
	if approved {
		a := at(-2)
		appr = &a
	}
	return f.id(`INSERT INTO outreach_batch (shadow, content_sha256, templates_sha256, approved_at) VALUES ($1, '\x00', '\x00', $2)`, shadow, appr)
}

func (f fixture) msg(batch, lead int64, step int, channel, to, state string, sent *time.Time) {
	f.id(`INSERT INTO outreach_message (batch_id, lead_id, step, channel, to_address, body_text, slots, state, sent_at)
	      VALUES ($1, $2, $3, $4, $5, 'body', '{}', $6, $7)`, batch, lead, step, channel, to, state, sent)
}

var uid int64

func (f fixture) event(kind string, lead *int64, fromDomain string, seen time.Time) {
	uid++
	f.id(`INSERT INTO mailbox_event (mailbox, uidvalidity, uid, kind, from_domain, lead_id, seen_at) VALUES ('INBOX', 1, $1, $2, $3, $4, $5)`,
		uid, kind, fromDomain, lead, seen)
}

func (f fixture) probe(domain string, drupal bool, major int, when time.Time) {
	site := f.id(`INSERT INTO site (domain, origin) VALUES ($1, 'manual')`, domain)
	f.id(`INSERT INTO site_probe (site_id, run_id, outcome, is_drupal, major, fetch_ids, derived_by, created_at)
	      VALUES ($1, $2, 'ok', $3, $4, '{}', 'drupal@1', $5)`, site, f.run, drupal, major, when)
}

func tp(t time.Time) *time.Time { return &t }

func newFixture(t *testing.T) fixture {
	t.Helper()
	leadstoretest.Reset(t)
	f := fixture{t: t, owner: storetest.OwnerConn(t)}
	f.run = f.id(`INSERT INTO run (command, source, started_at, finished_at, note) VALUES ('leads-send', 'outreach', $1, $2, $3)`,
		at(10), at(10), "live · sent 0 · refused 0 · maybe_sent 0 · retry 0 · cancelled 0 · skipped 0 · notes dns")
	f.id(`INSERT INTO sender_check (domain_sha256, spf, dkim, dmarc, checked_at) VALUES ('\x01', true, true, false, $1)`, at(9))

	alpha, beta, gamma := f.lead("alpha.example", false), f.lead("beta.example", false), f.lead("gamma.example", false)
	delta, self := f.lead("delta.invalid", false), f.lead("selftest-1", true)

	// A live approved batch: one sent, one maybe sent, one form waiting; the rest is fixture
	// data or outside yesterday.
	live := f.batch(false, true)
	f.msg(live, alpha, 0, "email", alphaTo, "sent", tp(at(11)))
	f.msg(live, alpha, 1, "email", alphaTo, "sent", tp(at(-30)))
	f.msg(live, beta, 0, "email", betaTo, "maybe_sent", tp(at(12)))
	f.msg(live, beta, 1, "email", betaTo, "sending", tp(at(-24*30)))
	f.msg(live, gamma, 0, "form", "", "approved", nil)
	f.msg(live, delta, 0, "email", deltaTo, "sent", tp(at(11)))
	f.msg(live, delta, 1, "form", "", "approved", nil)
	f.msg(live, self, 0, "email", selfTo, "sent", tp(at(11)))
	// A shadow batch: one shadow send counts, the reserved one does not.
	shadow := f.batch(true, true)
	f.msg(shadow, beta, 0, "email", betaTo, "shadow_sent", tp(at(13)))
	f.msg(shadow, delta, 0, "email", deltaTo, "shadow_sent", tp(at(13)))
	f.msg(shadow, gamma, 0, "form", "", "approved", nil)
	// Batch 3 awaits approval: one first email (its follow-up is not an item), one LinkedIn note.
	ready := f.batch(false, false)
	f.msg(ready, gamma, 0, "email", gammaTo, "draft", nil)
	f.msg(ready, gamma, 1, "email", gammaTo, "draft", nil)
	f.msg(ready, alpha, 2, "linkedin", "", "draft", nil)
	f.msg(ready, delta, 2, "email", deltaTo, "draft", nil)
	// A newer batch of fixture leads only is never the one to approve.
	reservedOnly := f.batch(false, false)
	f.msg(reservedOnly, delta, 0, "email", deltaTo, "draft", nil)
	f.msg(reservedOnly, self, 1, "email", selfTo, "draft", nil)

	// The mailbox: an opt-out yesterday and a reply this morning to read; a bounce; one other
	// message; and what is fixture data or too old.
	f.event("opt_out", &beta, "beta.example", at(15))
	f.event("reply", &alpha, "alpha.example", at(30))
	f.event("reply", &alpha, "alpha.example", at(31))
	f.event("bounce", &gamma, "gamma.example", at(16))
	f.event("unmatched", nil, "mail.example", at(17))
	f.event("unmatched", nil, "x.invalid", at(17))
	f.event("unmatched", nil, "old.example", at(-48))
	f.event("reply", &alpha, "alpha.example", at(-48))
	f.event("reply", &delta, "delta.invalid", at(15))
	f.event("opt_out", &delta, "delta.invalid", at(15))
	f.event("reply", &self, "self.invalid", at(15))

	// Yesterday's probes: one Drupal 7, one not Drupal; the reserved one and the old one stay out.
	f.probe("alpha.example", true, 7, at(8))
	f.probe("beta.example", false, 0, at(8))
	f.probe("delta.invalid", true, 10, at(8))
	f.probe("gamma.example", true, 9, at(-30))
	return f
}

// TestLoad reads the section from fixture rows. Fixture data lives on reserved TLDs only,
// so the test narrows the list to .invalid to see the .example rows counted, then proves
// the full list leaves every one of them out (RA25).
func TestLoad(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	q := storetest.AppConn(t)

	got, err := leadbrief.LoadExcluding(ctx, q, day, loc, []string{"invalid"})
	if err != nil {
		t.Fatal(err)
	}
	want := leadbrief.Section{
		Replies: []string{"beta.example", "alpha.example"}, Other: 1,
		Batch:         &leadbrief.BatchReady{ID: 3, Emails: 1, LinkedIn: 1},
		ManualWaiting: 1, Sent: 1, ShadowSent: 1, Bounced: 1, OptedOut: 1,
		Paused: "dns: dmarc missing", CheckByHand: 2,
		Probe: &leadbrief.ProbeLine{Checked: 2, Drupal: 1, ByMajor: map[int]int{7: 1}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("section = %s\nwant      %s", show(got), show(want))
	}

	// A newer shadow batch awaiting approval is the one named, as shadow.
	var alphaID int64
	if err := f.owner.QueryRow(ctx, `SELECT l.id FROM lead l JOIN org o ON o.id = l.org_id WHERE o.domain = 'alpha.example'`).Scan(&alphaID); err != nil {
		t.Fatal(err)
	}
	sb := f.batch(true, false)
	f.msg(sb, alphaID, 0, "email", alphaTo, "draft", nil)
	got, err = leadbrief.LoadExcluding(ctx, q, day, loc, []string{"invalid"})
	if err != nil || got.Batch == nil || *got.Batch != (leadbrief.BatchReady{ID: sb, Shadow: true, Emails: 1}) {
		t.Errorf("newer shadow batch: %s, %v", show(got), err)
	}

	all, err := leadbrief.Load(ctx, q, day, loc)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Replies) != 0 || all.Other != 0 || all.Batch != nil || all.ManualWaiting != 0 || all.Sent != 0 ||
		all.ShadowSent != 0 || all.Bounced != 0 || all.OptedOut != 0 || all.CheckByHand != 0 || all.Probe != nil {
		t.Errorf("reserved TLDs counted: %s", show(all))
	}
	// The pause is the sending run's, not a lead's.
	if all.Paused != "dns: dmarc missing" {
		t.Errorf("paused = %q", all.Paused)
	}
	// An empty list never turns the exclusion off.
	if empty, err := leadbrief.LoadExcluding(ctx, q, day, loc, nil); err != nil || !reflect.DeepEqual(empty, all) {
		t.Errorf("empty list = %s, %v; want Load's %s", show(empty), err, show(all))
	}
}

// TestLoadPaused: a passing sender check since clears a DNS stop; a failed sync and a
// refused SMTP account each pause; a selftest run never counts.
func TestLoadPaused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	q := storetest.AppConn(t)
	paused := func() string {
		t.Helper()
		s, err := leadbrief.Load(ctx, q, day, loc)
		if err != nil {
			t.Fatal(err)
		}
		return s.Paused
	}
	f.id(`INSERT INTO sender_check (domain_sha256, spf, dkim, dmarc, checked_at) VALUES ('\x01', true, true, true, $1)`, at(11))
	if p := paused(); p != "" {
		t.Errorf("after a passing check: %q", p)
	}
	f.id(`INSERT INTO run (command, source, started_at, finished_at, note) VALUES ('leads-send', 'outreach', $1, $1, 'live · sent 0 · notes sync')`, at(12))
	if p := paused(); p != leadbrief.PausedSync {
		t.Errorf("after a failed sync: %q", p)
	}
	f.id(`INSERT INTO run (command, source, started_at, finished_at, note) VALUES ('leads-send', 'outreach', $1, $1, 'live · sent 1 · notes smtp-account')`, at(13))
	if p := paused(); p != leadbrief.PausedAccount {
		t.Errorf("after a refused account: %q", p)
	}
	f.id(`INSERT INTO run (command, source, started_at, finished_at, note) VALUES ('leads-selftest', 'outreach', $1, $1, 'live · sent 1')`, at(14))
	f.id(`INSERT INTO run (command, source, started_at, note) VALUES ('leads-send', 'outreach', $1, '')`, at(15))
	if p := paused(); p != leadbrief.PausedAccount {
		t.Errorf("a selftest or an unfinished run changed the pause: %q", p)
	}
	f.id(`INSERT INTO run (command, source, started_at, finished_at, note) VALUES ('leads-send', 'outreach', $1, $1, 'live · sent 2 · notes sync running')`, at(16))
	if p := paused(); p != "" {
		t.Errorf("a clean run left the pause: %q", p)
	}
}

// TestLoadOperatorPaused: the operator's pause switch is in the section, with its time in
// the brief's zone, until a resume; no row is not paused. Its reason never reaches the brief.
func TestLoadOperatorPaused(t *testing.T) {
	newFixture(t)
	ctx := context.Background()
	q := storetest.AppConn(t)
	load := func() leadbrief.Section {
		t.Helper()
		s, err := leadbrief.Load(ctx, q, day, loc)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := load(); s.OperatorPaused != nil {
		t.Fatalf("cold start paused at %v", s.OperatorPaused)
	}
	lq := leadstoretest.Q(t)
	if err := lq.SetSendPause(ctx, true, "Ilinca"+" asked to wait"); err != nil {
		t.Fatal(err)
	}
	s := load()
	if s.OperatorPaused == nil || s.OperatorPaused.Location() != loc || !s.Todo() {
		t.Fatalf("paused: %v, todo %v", s.OperatorPaused, s.Todo())
	}
	if strings.Contains(show(s), "Ilinca") {
		t.Error("the pause reason reached the section")
	}
	if err := lq.SetSendPause(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	if s := load(); s.OperatorPaused != nil {
		t.Errorf("resumed but paused at %v", s.OperatorPaused)
	}
}

// oldProbeSQL is loadProbe's query before it read the day's probes first: the oracle for
// TestLoadProbeOracle.
const oldProbeSQL = `SELECT p.is_drupal, p.major, count(*)::int FROM site_probe p JOIN site si ON si.id = p.site_id
	 WHERE p.created_at >= $2 AND p.created_at < $3 AND si.domain !~ $1
	 GROUP BY p.is_drupal, p.major`

// TestLoadProbeOracle: the probe line equals the old query's on reserved and other domains,
// Drupal and not, several majors, a site probed twice, and probes on, inside and outside the
// day's bounds.
func TestLoadProbeOracle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	site := func(domain string) int64 {
		return f.id(`INSERT INTO site (domain, origin) VALUES ($1, 'manual')`, domain)
	}
	probe := func(site int64, drupal bool, major int, when time.Time) {
		f.id(`INSERT INTO site_probe (site_id, run_id, outcome, is_drupal, major, fetch_ids, derived_by, created_at)
		      VALUES ($1, $2, 'ok', $3, $4, '{}', 'drupal@1', $5)`, site, f.run, drupal, major, when)
	}
	twice, other, reserved, local := site("twice.example"), site("other.example"), site("res.invalid"), site("box.localhost")
	probe(twice, true, 7, at(1))
	probe(twice, true, 10, at(20))
	probe(other, false, 0, at(0))                      // on the day's first instant: in
	probe(other, true, 0, at(5))                       // Drupal, major unknown
	probe(other, true, 9, at(24))                      // on the next day's first instant: out
	probe(other, true, 8, at(0).Add(-time.Nanosecond)) // a moment before the day: out
	probe(reserved, true, 7, at(3))
	probe(local, true, 11, at(4))
	probe(local, false, 0, at(-1))

	oracle := func(reserved []string) *leadbrief.ProbeLine {
		t.Helper()
		if len(reserved) == 0 {
			reserved = leadstore.ReservedTLDs
		}
		re := `\.(` + strings.Join(reserved, "|") + `)$`
		rows, err := f.owner.Query(ctx, oldProbeSQL, re, yesterday, yesterday.AddDate(0, 0, 1))
		if err != nil {
			t.Fatal(err)
		}
		p := leadbrief.ProbeLine{ByMajor: map[int]int{}}
		var (
			drupal   bool
			major, n int
		)
		if _, err := pgx.ForEachRow(rows, []any{&drupal, &major, &n}, func() error {
			p.Checked += n
			if drupal {
				p.Drupal += n
				p.ByMajor[major] += n
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if p.Checked == 0 {
			return nil
		}
		return &p
	}
	for _, reserved := range [][]string{{"invalid"}, nil} {
		got, err := leadbrief.LoadExcluding(ctx, storetest.AppConn(t), day, loc, reserved)
		if err != nil {
			t.Fatal(err)
		}
		want := oracle(reserved)
		// The full list leaves .example out too, so only the narrowed one sees rows.
		if reserved != nil && (want == nil || want.Checked < 3) {
			t.Fatalf("reserved %v: the oracle saw too little: %+v", reserved, want)
		}
		if !reflect.DeepEqual(got.Probe, want) {
			t.Errorf("reserved %v: probe %+v, want the old query's %+v", reserved, got.Probe, want)
		}
	}
}

// TestBriefSweep: the brief composed from the loaded section, text and HTML, carries no
// "@", no fixture address and none of its local parts.
func TestBriefSweep(t *testing.T) {
	newFixture(t)
	s, err := leadbrief.LoadExcluding(context.Background(), storetest.AppConn(t), day, loc, []string{"invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Todo() {
		t.Fatalf("the fixture section has nothing to do: %s", show(s))
	}
	in := brief.Input{Day: day, Scoring: brief.ScoringOK, Leads: &s,
		Sources: []brief.Source{{Name: "LinkedIn", State: brief.StateOK, Canary: brief.CanaryOK}}}
	subject, text, html := brief.Compose(in)
	out := subject + "\n" + text + "\n" + html
	if !strings.Contains(text, "Read 2 replies in the outreach mailbox: beta.example, alpha.example") {
		t.Fatalf("the brief lacks the replies:\n%s", text)
	}
	forbidden := []string{"@"}
	for _, a := range []string{alphaTo, betaTo, gammaTo, deltaTo, selfTo} {
		local, _, _ := strings.Cut(a, "@")
		forbidden = append(forbidden, a, local)
		forbidden = append(forbidden, strings.Split(local, ".")...)
	}
	for _, w := range forbidden {
		if w == "me" || w == "office" {
			// Too short or too common to sweep for alone; their whole addresses still are.
			continue
		}
		if strings.Contains(strings.ToLower(out), strings.ToLower(w)) {
			t.Errorf("the brief carries %q", w)
		}
	}
}

func show(s leadbrief.Section) string {
	b, p := "nil", "nil"
	if s.Batch != nil {
		b = fmt.Sprintf("%+v", *s.Batch)
	}
	if s.Probe != nil {
		p = fmt.Sprintf("%+v", *s.Probe)
	}
	return fmt.Sprintf("%+v batch=%s probe=%s", s, b, p)
}
