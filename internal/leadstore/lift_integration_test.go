//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
)

// Every lift test runs as the app role (f.q and appPool), as production does: the owner holds
// UPDATE on suppression and would hide a privilege failure such as SELECT … FOR UPDATE.
// Fixture addresses are reserved names built at run time.

func liftAddr(local, domain string) string { return local + "@" + domain }

// liftDomain is a reserved fixture domain built at run time.
func liftDomain(name string) string { return name + "." + "example" }

// inAppTx runs fn in one app-role transaction and commits it.
func inAppTx(t *testing.T, pool *pgxpool.Pool, fn func(q leadstore.Queries) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(leadstore.NewQueries(tx)); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// liftCount is how many suppression_lift rows there are.
func (f outreachFixture) liftCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.owner.QueryRow(context.Background(), `SELECT count(*) FROM suppression_lift`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// suppressionID returns the id of the one suppression of kind and value with reason.
func (f outreachFixture) suppressionID(t *testing.T, kind, value, reason string) int64 {
	t.Helper()
	var id int64
	if err := f.owner.QueryRow(context.Background(),
		`SELECT max(id) FROM suppression WHERE kind = $1 AND value = $2 AND reason = $3`, kind, value, reason).Scan(&id); err != nil {
		t.Fatalf("suppression %s %s: %v", kind, reason, err)
	}
	return id
}

// liftWorld is one new lead on dom whose route address is suppressed (opt_out), then put back
// to new, so each reader's verdict is about the suppression alone.
type liftWorld struct {
	f     outreachFixture
	dom   string
	addr  string
	lead  int64
	supID int64
}

func newLiftWorld(t *testing.T) liftWorld {
	t.Helper()
	f := newOutreachFixture(t)
	dom := liftDomain("northwind")
	addr := liftAddr("contact", dom)
	lead := f.siteLead(t, dom, intp(1), 7, addr)
	if _, err := f.q.Suppress(context.Background(), leadstore.SuppressAddress, addr, leadstore.ReasonOptOut, "mailbox:1:INBOX:3"); err != nil {
		t.Fatal(err)
	}
	if got := f.leadState(t, lead); got != leadstore.LeadSuppressed {
		t.Fatalf("lead after suppress = %s, want suppressed", got)
	}
	f.setState(t, lead, leadstore.LeadNew)
	return liftWorld{f: f, dom: dom, addr: addr, lead: lead, supID: f.suppressionID(t, leadstore.SuppressAddress, addr, leadstore.ReasonOptOut)}
}

func (w liftWorld) lift(t *testing.T) leadstore.Lifted {
	t.Helper()
	res, err := w.f.q.LiftSuppression(context.Background(), w.supID, w.dom, "a reply, not an opt-out", leadstore.LiftFromCLI)
	if err != nil {
		t.Fatalf("lift: %v", err)
	}
	return res
}

// TestLiftGrid is the status × operation grid: each reader on an active and on a lifted
// suppression. The send-time check and leads suppress check are Task 2.3's cells.
func TestLiftGrid(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"active", "lifted"} {
		lifted := status == "lifted"
		setup := func(t *testing.T) liftWorld {
			t.Helper()
			w := newLiftWorld(t)
			if lifted {
				w.lift(t)
			}
			return w
		}
		t.Run(status+"/lift again", func(t *testing.T) {
			w := setup(t)
			_, err := w.f.q.LiftSuppression(ctx, w.supID, w.dom, "a reply, not an opt-out", leadstore.LiftFromConsole)
			if lifted && !errors.Is(err, leadstore.ErrAlreadyLifted) {
				t.Errorf("second lift: %v, want ErrAlreadyLifted", err)
			}
			if !lifted && err != nil {
				t.Errorf("first lift: %v", err)
			}
			if n := w.f.liftCount(t); n != 1 {
				t.Errorf("lift rows = %d, want 1", n)
			}
		})
		t.Run(status+"/suppress same key", func(t *testing.T) {
			w := setup(t)
			res, err := w.f.q.Suppress(ctx, leadstore.SuppressAddress, w.addr, leadstore.ReasonOptOut, "mailbox:1:INBOX:4")
			if err != nil {
				t.Fatal(err)
			}
			if res.Recorded != lifted {
				t.Errorf("suppress the same key: recorded = %v, want %v", res.Recorded, lifted)
			}
		})
		t.Run(status+"/suppression set", func(t *testing.T) {
			w := setup(t)
			set, err := w.f.q.SuppressionSet(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(set.Addresses, w.addr) == lifted {
				t.Errorf("suppression set %+v: holds the address = %v, want %v", set, !lifted, !lifted)
			}
		})
		t.Run(status+"/candidate query", func(t *testing.T) {
			w := setup(t)
			got := slices.Contains(candidateIDs(t, w.f.q), w.lead)
			if got != lifted {
				t.Errorf("lead is a candidate = %v, want %v", got, lifted)
			}
		})
		t.Run(status+"/draft check", func(t *testing.T) {
			w := setup(t)
			_, _, err := w.f.q.CreateBatch(ctx, true, w.f.drafts(t, w.lead), []byte("templates"))
			if lifted && err != nil {
				t.Errorf("draft after lift: %v", err)
			}
			if !lifted && !errors.Is(err, leadstore.ErrNotDraftable) {
				t.Errorf("draft while suppressed: %v, want ErrNotDraftable", err)
			}
		})
		t.Run(status+"/counts", func(t *testing.T) {
			w := setup(t)
			counts, err := w.f.q.SuppressionCounts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			total := 0
			for _, c := range counts {
				total += c.N
			}
			want := 1
			if lifted {
				want = 0
			}
			if total != want {
				t.Errorf("suppression counts %+v total %d, want %d", counts, total, want)
			}
			n, err := w.f.q.LiftedCount(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if wantLifted := map[bool]int{false: 0, true: 1}[lifted]; n != wantLifted {
				t.Errorf("lifted count = %d, want %d", n, wantLifted)
			}
		})
	}
}

// TestSuppressAfterLiftRecordsAgain is the grid's surprising cell: a real opt-out after a lift
// must suppress, as a new row, and stop the lead again.
func TestSuppressAfterLiftRecordsAgain(t *testing.T) {
	ctx := context.Background()
	w := newLiftWorld(t)
	w.lift(t)
	res, err := w.f.q.Suppress(ctx, leadstore.SuppressAddress, w.addr, leadstore.ReasonOptOut, "mailbox:1:INBOX:9")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Recorded || res.Moved != 1 {
		t.Errorf("suppress after lift = %+v, want a new row and the lead moved", res)
	}
	if got := w.f.leadState(t, w.lead); got != leadstore.LeadSuppressed {
		t.Errorf("lead = %s, want suppressed", got)
	}
	set, _ := w.f.q.SuppressionSet(ctx)
	if !slices.Contains(set.Addresses, w.addr) {
		t.Error("the new suppression is not active")
	}
	active, lifted, err := w.f.q.SuppressionRows(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || len(lifted) != 1 || active[0].ID == lifted[0].ID || lifted[0].ID != w.supID {
		t.Errorf("rows: active %+v lifted %+v", active, lifted)
	}
}

func TestLiftRecordsAndReports(t *testing.T) {
	ctx := context.Background()
	w := newLiftWorld(t)
	w.f.setState(t, w.lead, leadstore.LeadSuppressed) // as the opt-out left it
	// Confirmed by the address's domain, in mixed case with spaces.
	res, err := w.f.q.LiftSuppression(ctx, w.supID, "  NorthWind."+"Example ", "  the selftest reply  ", leadstore.LiftFromCLI)
	if err != nil {
		t.Fatal(err)
	}
	if res.Row.ID != w.supID || res.Row.LiftedAt == nil || res.Row.LiftReason != "the selftest reply" || res.Row.LiftSource != "cli" {
		t.Errorf("lifted row = %+v", res.Row)
	}
	if res.Row.Value != w.addr || res.Row.Reason != leadstore.ReasonOptOut || res.Row.Source != "mailbox:1:INBOX:3" {
		t.Errorf("the suppression row changed: %+v", res.Row)
	}
	if res.LeadsSuppressed != 1 || len(res.StillBlockedBy) != 0 {
		t.Errorf("lifted = %+v, want 1 lead suppressed and nothing still blocking", res)
	}
	if got := w.f.leadState(t, w.lead); got != leadstore.LeadSuppressed {
		t.Errorf("a lift moved the lead to %s", got)
	}
	var actor, source string
	if err := w.f.owner.QueryRow(ctx, `SELECT actor, source FROM suppression_lift WHERE suppression_id = $1`, w.supID).Scan(&actor, &source); err != nil {
		t.Fatal(err)
	}
	if actor != leadstore.ActorOperator || source != leadstore.LiftFromCLI {
		t.Errorf("lift row actor %q source %q", actor, source)
	}
	got, err := w.f.q.SuppressionByID(ctx, w.supID)
	if err != nil || got.LiftedAt == nil {
		t.Errorf("by id: %+v %v", got, err)
	}
	if _, err := w.f.q.SuppressionByID(ctx, 999); !errors.Is(err, leadstore.ErrNoSuppression) {
		t.Errorf("unknown id: %v", err)
	}

	// A domain suppression is confirmed by its domain.
	dom := liftDomain("contoso")
	if _, err := w.f.q.Suppress(ctx, leadstore.SuppressDomain, dom, leadstore.ReasonManual, "cli"); err != nil {
		t.Fatal(err)
	}
	id := w.f.suppressionID(t, leadstore.SuppressDomain, dom, leadstore.ReasonManual)
	if _, err := w.f.q.LiftSuppression(ctx, id, strings.ToUpper(dom), "typed by mistake", leadstore.LiftFromConsole); err != nil {
		t.Errorf("domain lift: %v", err)
	}
}

func TestLiftStillBlockedByDomain(t *testing.T) {
	ctx := context.Background()
	w := newLiftWorld(t)
	if _, err := w.f.q.Suppress(ctx, leadstore.SuppressDomain, w.dom, leadstore.ReasonManual, "cli"); err != nil {
		t.Fatal(err)
	}
	domID := w.f.suppressionID(t, leadstore.SuppressDomain, w.dom, leadstore.ReasonManual)
	res := w.lift(t)
	if len(res.StillBlockedBy) != 1 || res.StillBlockedBy[0].ID != domID || res.StillBlockedBy[0].Kind != leadstore.SuppressDomain {
		t.Errorf("still blocked by %+v, want the domain row #%d", res.StillBlockedBy, domID)
	}
}

func TestLiftRefusals(t *testing.T) {
	ctx := context.Background()
	w := newLiftWorld(t)
	if _, err := w.f.q.LiftSuppression(ctx, 999, w.dom, "a fine reason", leadstore.LiftFromCLI); !errors.Is(err, leadstore.ErrNoSuppression) {
		t.Errorf("unknown id: %v, want ErrNoSuppression", err)
	}
	if _, err := w.f.q.LiftSuppression(ctx, w.supID, liftDomain("contoso"), "a fine reason", leadstore.LiftFromCLI); !errors.Is(err, leadstore.ErrLiftDomainMismatch) {
		t.Errorf("wrong domain: %v, want ErrLiftDomainMismatch", err)
	}
	for _, reason := range []string{"", "   ", "ab", strings.Repeat("r", 301), "two\nlines", "carriage\rreturn",
		"see " + liftAddr("contact", w.dom)} {
		if _, err := w.f.q.LiftSuppression(ctx, w.supID, w.dom, reason, leadstore.LiftFromCLI); !errors.Is(err, leadstore.ErrLiftReason) {
			t.Errorf("reason %q: %v, want ErrLiftReason", reason, err)
		}
	}
	if n := w.f.liftCount(t); n != 0 {
		t.Fatalf("a refused lift wrote %d rows", n)
	}
	// 300 characters is the most a reason may hold.
	if _, err := w.f.q.LiftSuppression(ctx, w.supID, w.dom, strings.Repeat("r", 300), leadstore.LiftFromCLI); err != nil {
		t.Errorf("a 300-character reason: %v", err)
	}
	// The CHECK is the backstop: a raw insert of a bad reason fails as the app role.
	if _, err := w.f.q.Suppress(ctx, leadstore.SuppressDomain, liftDomain("fabrikam"), leadstore.ReasonManual, "cli"); err != nil {
		t.Fatal(err)
	}
	other := w.f.suppressionID(t, leadstore.SuppressDomain, liftDomain("fabrikam"), leadstore.ReasonManual)
	app := appPool(t)
	for _, reason := range []string{"ab", "a\nreason", "at " + liftAddr("contact", w.dom)} {
		_, err := app.Exec(ctx, `INSERT INTO suppression_lift (suppression_id, reason, actor, source) VALUES ($1, $2, 'operator', 'cli')`, other, reason)
		if got := sqlState(err); got != "23514" {
			t.Errorf("raw insert of reason %q: %v (SQLSTATE %q), want 23514 check_violation", reason, err, got)
		}
	}
	for _, bad := range [][2]string{{"gremy", "cli"}, {"operator", "api"}} {
		_, err := app.Exec(ctx, `INSERT INTO suppression_lift (suppression_id, reason, actor, source) VALUES ($1, 'a reason', $2, $3)`, other, bad[0], bad[1])
		if got := sqlState(err); got != "23514" {
			t.Errorf("raw insert actor %q source %q: %v, want 23514", bad[0], bad[1], err)
		}
	}
	if _, err := app.Exec(ctx, `UPDATE suppression_lift SET reason = 'rewritten'`); sqlState(err) != "42501" {
		t.Errorf("app UPDATE of suppression_lift: %v, want 42501", err)
	}
}

// TestLiftConcurrent: two app-role transactions lift one id; the second waits on the first's
// insert and, once it commits, records nothing (ON CONFLICT DO NOTHING, no FOR UPDATE).
func TestLiftConcurrent(t *testing.T) {
	ctx := context.Background()
	w := newLiftWorld(t)
	a, b := appPool(t), appPool(t)
	txA, err := a.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	if _, err := leadstore.NewQueries(txA).LiftSuppression(ctx, w.supID, w.dom, "first lift", leadstore.LiftFromCLI); err != nil {
		t.Fatalf("lift A: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- inAppTx(t, b, func(q leadstore.Queries) error {
			_, err := q.LiftSuppression(ctx, w.supID, w.dom, "second lift", leadstore.LiftFromConsole)
			return err
		})
	}()
	// Wait until B is blocked on A's row in the unique index (seen from the app role: only
	// the same role sees another session's wait).
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := a.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%suppression_lift%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second lift never waited on the first")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, leadstore.ErrAlreadyLifted) {
		t.Errorf("lift B: %v, want ErrAlreadyLifted", err)
	}
	if n := w.f.liftCount(t); n != 1 {
		t.Errorf("lift rows = %d, want 1", n)
	}
}

func TestOptOutEvents(t *testing.T) {
	ctx := context.Background()
	f := newOutreachFixture(t)
	evs, err := f.q.OptOutEvents(ctx)
	if err != nil || len(evs) != 0 {
		t.Fatalf("cold start: %+v %v, want none", evs, err)
	}
	pool := appPool(t)
	mb := leadstoretest.SeedMailbox(t, f.q, liftAddr("out", liftDomain("mail")))
	apply := func(validity, uid uint32, ref *leadstore.SentRef, at time.Time) {
		t.Helper()
		if err := inAppTx(t, pool, func(q leadstore.Queries) error {
			_, err := q.ApplyIncomingFor(ctx, mb.ID, leadstore.Incoming{Mailbox: "INBOX", UIDValidity: validity, UID: uid,
				Kind: mailbox.KindOptOut, FromDomain: ref.OrgDomain, Matched: ref, At: at})
			return err
		}); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

	// One opt-out from a site lead: its address and domain suppressions, and where it went.
	dom := liftDomain("northwind")
	lead, ref := f.sentLead(t, dom, liftAddr("contact", dom))
	apply(1, 3, &ref, at)
	evs, err = f.q.OptOutEvents(ctx)
	if err != nil || len(evs) != 1 {
		t.Fatalf("one opt-out: %+v %v", evs, err)
	}
	e := evs[0]
	if e.MailboxID != mb.ID || e.Folder != "INBOX" || e.UIDValidity != 1 || e.UID != 3 || e.FromDomain != dom ||
		e.LeadID == nil || *e.LeadID != lead || e.ToAddress != liftAddr("contact", dom) || e.Ambiguous {
		t.Errorf("event = %+v", e)
	}
	kinds := []string{}
	for _, s := range e.Suppressions {
		kinds = append(kinds, s.Kind+" "+s.Value)
	}
	if want := []string{"address " + liftAddr("contact", dom), "domain " + dom}; !slices.Equal(kinds, want) {
		t.Errorf("suppressions %v, want %v", kinds, want)
	}

	// A legacy event (source mailbox:INBOX:<uid>, before 00007) matches no suppression.
	app := appPool(t)
	if _, err := app.Exec(ctx, `INSERT INTO mailbox_event (mailbox_id, mailbox, uidvalidity, uid, kind, from_domain, seen_at)
		VALUES ($1, 'INBOX', 1, 5, 'opt_out', $2, $3)`, mb.ID, liftDomain("legacy"), at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, `INSERT INTO suppression (kind, value, reason, source) VALUES ('domain', $1, 'opt_out', 'mailbox:INBOX:5')`,
		liftDomain("legacy")); err != nil {
		t.Fatal(err)
	}

	// Another opt-out at the same folder and UID under a new UIDVALIDITY: the same source,
	// told apart by transaction.
	dom2 := liftDomain("contoso")
	_, ref2 := f.sentLead(t, dom2, liftAddr("contact", dom2))
	apply(2, 3, &ref2, at.Add(2*time.Hour))

	evs, err = f.q.OptOutEvents(ctx)
	if err != nil || len(evs) != 3 {
		t.Fatalf("three opt-outs: %+v %v", evs, err)
	}
	if evs[1].UID != 5 || len(evs[1].Suppressions) != 0 {
		t.Errorf("legacy event = %+v, want no suppression", evs[1])
	}
	for i, d := range map[int]string{0: dom, 2: dom2} {
		ev := evs[i]
		if ev.Ambiguous || len(ev.Suppressions) != 2 {
			t.Errorf("event %d = %+v, want its own two suppressions", i, ev)
			continue
		}
		for _, s := range ev.Suppressions {
			if !strings.HasSuffix(s.Value, d) {
				t.Errorf("event %d holds %s %s, not its own", i, s.Kind, s.Value)
			}
		}
	}

	// A suppression of that shared source written outside the event's transaction cannot be
	// told apart: both events carry it, marked Ambiguous. A lift shows on the row.
	if _, err := app.Exec(ctx, `INSERT INTO suppression (kind, value, reason, source) VALUES ('domain', $1, 'opt_out', 'mailbox:'||$2::bigint||':INBOX:3')`,
		liftDomain("fabrikam"), mb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.LiftSuppression(ctx, evs[0].Suppressions[0].ID, dom, "a reply, not an opt-out", leadstore.LiftFromCLI); err != nil {
		t.Fatal(err)
	}
	evs, err = f.q.OptOutEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 2} {
		if !evs[i].Ambiguous || len(evs[i].Suppressions) != 3 {
			t.Errorf("event %d = %+v, want ambiguous with three suppressions", i, evs[i])
		}
	}
	if evs[1].Ambiguous {
		t.Error("the legacy event is marked ambiguous")
	}
	if evs[0].Suppressions[0].LiftedAt == nil {
		t.Error("the lifted suppression does not show its lift")
	}
}
