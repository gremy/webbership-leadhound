//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The sending mailboxes (00007): reserved names only, placeholder sealed bytes.

func newMailbox(address string) leadstore.Mailbox {
	return leadstore.Mailbox{
		Address: address, Login: address,
		SMTPAddr: "smtp.mail.example:465", IMAPAddr: "imap.mail.example:993",
		DKIMSelector: "lh", PasswordSealed: []byte("sealed-placeholder"), SealedBy: "fp-one",
	}
}

func addMailbox(t *testing.T, q leadstore.Queries, address string) int64 {
	t.Helper()
	id, err := q.AddMailbox(context.Background(), newMailbox(address), leadstore.ActorOperator)
	if err != nil {
		t.Fatalf("add mailbox %s: %v", address, err)
	}
	return id
}

func statusOf(t *testing.T, q leadstore.Queries, id int64, since time.Time) leadstore.MailboxStatus {
	t.Helper()
	all, err := q.Mailboxes(context.Background(), since)
	if err != nil {
		t.Fatalf("mailboxes: %v", err)
	}
	for _, s := range all {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("mailbox %d not listed", id)
	return leadstore.MailboxStatus{}
}

func TestMailboxAppRoleCannotDelete(t *testing.T) {
	leadstoretest.Reset(t)
	app := storetest.AppConn(t)
	for _, q := range []string{
		"DELETE FROM sender_mailbox",
		"DELETE FROM mailbox_state",
		"DELETE FROM mailbox_check",
		"TRUNCATE sender_mailbox",
		"TRUNCATE mailbox_state",
		"TRUNCATE mailbox_check",
	} {
		t.Run(q, func(t *testing.T) {
			_, err := app.Exec(context.Background(), q)
			if got := sqlState(err); got != "42501" {
				t.Fatalf("%s as app: err = %v (SQLSTATE %q), want 42501 insufficient_privilege", q, err, got)
			}
		})
	}
}

func TestMailboxAppRoleCannotUpdateFixedColumns(t *testing.T) {
	leadstoretest.Reset(t)
	app := storetest.AppConn(t)
	for _, q := range []string{
		"UPDATE mailbox_state SET state = 'active'",
		"UPDATE mailbox_state SET reason = ''",
		"UPDATE outreach_message SET mailbox_id = NULL",
		"UPDATE outreach_message SET from_name = ''",
		"UPDATE sender_mailbox SET address = ''",
		"UPDATE sender_mailbox SET domain = ''",
		"UPDATE sender_mailbox SET created_at = now()",
		"UPDATE mailbox_check SET mailbox_id = 1",
		"UPDATE mailbox_check SET requested_at = now()",
		"UPDATE mailbox_check SET actor = 'system'",
	} {
		t.Run(q, func(t *testing.T) {
			_, err := app.Exec(context.Background(), q)
			if got := sqlState(err); got != "42501" {
				t.Fatalf("%s as app: err = %v (SQLSTATE %q), want 42501 insufficient_privilege", q, err, got)
			}
		})
	}
}

// mailboxGrants is the app role's UPDATE on the new columns: table → may, may not.
var mailboxGrants = map[string][2][]string{
	"sender_mailbox": {{"login", "smtp_addr", "imap_addr", "dkim_selector", "from_name", "password_sealed", "sealed_by"},
		{"id", "address", "domain", "created_at"}},
	"mailbox_check": {{"checked_at", "smtp_ok", "imap_ok", "folders_ok", "spf", "dkim", "dmarc", "fix", "outcome"},
		{"id", "mailbox_id", "requested_at", "actor"}},
	"outreach_message": {{"hard_bounce"}, {"mailbox_id", "from_name"}},
}

func assertMailboxGrants(t *testing.T, when string) {
	t.Helper()
	owner := storetest.OwnerConn(t)
	for table, cols := range mailboxGrants {
		if appHas(t, owner, table, "UPDATE") {
			t.Errorf("%s: leadhound_app has table-wide UPDATE on %s", when, table)
		}
		for _, c := range cols[0] {
			if !appHasColumn(t, owner, table, c, "UPDATE") {
				t.Errorf("%s: leadhound_app lacks UPDATE on %s.%s", when, table, c)
			}
		}
		for _, c := range cols[1] {
			if appHasColumn(t, owner, table, c, "UPDATE") {
				t.Errorf("%s: leadhound_app has UPDATE on %s.%s, want none", when, table, c)
			}
		}
	}
	for _, table := range []string{"sender_mailbox", "mailbox_state", "mailbox_check"} {
		if !appHas(t, owner, table, "INSERT") || !appHas(t, owner, table, "SELECT") {
			t.Errorf("%s: leadhound_app lacks INSERT or SELECT on %s", when, table)
		}
		if appHas(t, owner, table, "DELETE") || appHas(t, owner, table, "TRUNCATE") {
			t.Errorf("%s: leadhound_app may DELETE or TRUNCATE %s", when, table)
		}
	}
	if appHas(t, owner, "mailbox_state", "UPDATE") {
		t.Errorf("%s: leadhound_app has UPDATE on mailbox_state, want none (append-only)", when)
	}
}

// TestMailboxGrants: the column grants hold after a migrate, after grants.sql (which
// re-grants every table first) and after a migrate again.
func TestMailboxGrants(t *testing.T) {
	leadstoretest.Reset(t)
	assertMailboxGrants(t, "after migrate")
	grants, err := os.ReadFile("../../deploy/sql/grants.sql")
	if err != nil {
		t.Fatalf("read grants.sql: %v", err)
	}
	if _, err := storetest.OwnerConn(t).Exec(context.Background(), string(grants)); err != nil {
		t.Fatalf("run grants.sql: %v", err)
	}
	assertMailboxGrants(t, "after grants.sql")
	if err := leadstore.Migrate(context.Background(), storetest.MigrateDSN()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	assertMailboxGrants(t, "after grants.sql then migrate")

	// And in practice: the app role marks a hard bounce.
	f := newOutreachFixture(t)
	lead := f.siteLead(t, "bounce.example", nil, 7, "info@bounce.example")
	batchID, _ := f.batch(t, false, lead)
	ms, err := f.q.BatchMessages(context.Background(), batchID)
	if err != nil || len(ms) == 0 {
		t.Fatalf("batch messages: %v", err)
	}
	if err := f.q.MarkHardBounce(context.Background(), ms[0].ID); err != nil {
		t.Fatalf("mark hard bounce: %v", err)
	}
	var hb bool
	if err := f.owner.QueryRow(context.Background(), "SELECT hard_bounce FROM outreach_message WHERE id = $1", ms[0].ID).Scan(&hb); err != nil || !hb {
		t.Fatalf("hard_bounce = %v (%v), want true", hb, err)
	}
	if err := f.q.MarkHardBounce(context.Background(), 999999); !errors.Is(err, leadstore.ErrMessageNotFound) {
		t.Errorf("mark hard bounce of a missing message: %v, want ErrMessageNotFound", err)
	}
}

func TestMailboxesColdStart(t *testing.T) {
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	ctx := context.Background()
	all, err := q.Mailboxes(ctx, time.Now())
	if err != nil || len(all) != 0 {
		t.Fatalf("mailboxes on an empty table = %v, %v; want none, no error", all, err)
	}
	if n, err := q.MailboxesOnDomain(ctx, "mail.example"); err != nil || n != 0 {
		t.Errorf("mailboxes on domain = %d, %v", n, err)
	}
	if cs, err := q.RequestedChecks(ctx); err != nil || len(cs) != 0 {
		t.Errorf("requested checks = %v, %v", cs, err)
	}
	for name, fn := range map[string]func() (map[int64]int, error){
		"sent since": func() (map[int64]int, error) { return q.SentSinceByMailbox(ctx, time.Time{}) },
		"waiting":    func() (map[int64]int, error) { return q.WaitingByMailbox(ctx) },
	} {
		if m, err := fn(); err != nil || len(m) != 0 {
			t.Errorf("%s = %v, %v", name, m, err)
		}
	}
	if m, err := q.FirstLiveSendByMailbox(ctx); err != nil || len(m) != 0 {
		t.Errorf("first live send = %v, %v", m, err)
	}
	if _, err := q.MailboxByID(ctx, 1); !errors.Is(err, leadstore.ErrMailboxNotFound) {
		t.Errorf("mailbox by id on an empty table: %v, want ErrMailboxNotFound", err)
	}
}

func TestAddMailbox(t *testing.T) {
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	ctx := context.Background()
	m := newMailbox("  Out@Sub.Mail.Example ")
	m.FromName = "The Operator"
	id, err := q.AddMailbox(ctx, m, leadstore.ActorOperator)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	got, err := q.MailboxByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != "out@sub.mail.example" || got.Domain != "mail.example" || got.FromName != "The Operator" ||
		string(got.PasswordSealed) != "sealed-placeholder" || got.SealedBy != "fp-one" || got.CreatedAt.IsZero() {
		t.Errorf("stored mailbox = %+v", got)
	}
	// The add requested its first check, in the same statement.
	cs, err := q.RequestedChecks(ctx)
	if err != nil || len(cs) != 1 || cs[0].MailboxID != id || cs[0].Actor != leadstore.ActorOperator || cs[0].Outcome != leadstore.CheckRequested {
		t.Fatalf("requested checks after add = %+v, %v", cs, err)
	}
	st := statusOf(t, q, id, time.Now())
	if st.State != leadstore.MailboxActive || st.Scope != leadstore.ScopeMailbox || !st.StateAt.IsZero() ||
		st.LastCheck == nil || st.LastCheck.Outcome != leadstore.CheckRequested || st.FirstLiveSend != nil || st.SentToday != 0 || st.DNSPassedToday {
		t.Errorf("status of a new mailbox = %+v", st)
	}

	if _, err := q.AddMailbox(ctx, newMailbox("out@sub.mail.example"), leadstore.ActorOperator); !errors.Is(err, leadstore.ErrMailboxExists) {
		t.Errorf("second add of the same address: %v, want ErrMailboxExists", err)
	}
	for name, edit := range map[string]func(*leadstore.Mailbox){
		"no at":          func(m *leadstore.Mailbox) { m.Address = "nobody.example" },
		"two ats":        func(m *leadstore.Mailbox) { m.Address = "a@b@mail.example" },
		"no sealed":      func(m *leadstore.Mailbox) { m.PasswordSealed = nil },
		"no fingerprint": func(m *leadstore.Mailbox) { m.SealedBy = "" },
		"no login":       func(m *leadstore.Mailbox) { m.Login = "" },
		"no selector":    func(m *leadstore.Mailbox) { m.DKIMSelector = "" },
		"from name crlf": func(m *leadstore.Mailbox) { m.FromName = "A\r\nBcc: x@mail.example" },
		"wrong domain":   func(m *leadstore.Mailbox) { m.Domain = "other.example" },
	} {
		m := newMailbox("bad@mail.example")
		edit(&m)
		if _, err := q.AddMailbox(ctx, m, leadstore.ActorOperator); !errors.Is(err, leadstore.ErrBadMailbox) {
			t.Errorf("%s: add = %v, want ErrBadMailbox", name, err)
		}
	}
	if _, err := q.AddMailbox(ctx, newMailbox("x@mail.example"), "someone"); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("unknown actor: %v", err)
	}
	m = newMailbox("port@mail.example")
	m.SMTPAddr = "smtp.mail.example:25"
	if _, err := q.AddMailbox(ctx, m, leadstore.ActorOperator); err == nil || sqlState(err) != "23514" {
		t.Errorf("smtp port 25: %v, want a check violation", err)
	}
}

func TestMailboxLatestStateWins(t *testing.T) {
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	ctx := context.Background()
	id := addMailbox(t, q, "a@mail.example")
	steps := []struct{ state, reason, actor string }{
		{leadstore.MailboxPaused, "2 bounces", leadstore.ActorSystem},
		{leadstore.MailboxActive, "", leadstore.ActorOperator},
		{leadstore.MailboxPaused, "by hand", leadstore.ActorOperator},
	}
	for _, s := range steps {
		if err := q.SetMailboxState(ctx, id, s.state, s.reason, leadstore.ScopeMailbox, s.actor); err != nil {
			t.Fatalf("set %s: %v", s.state, err)
		}
	}
	st := statusOf(t, q, id, time.Now())
	if st.State != leadstore.MailboxPaused || st.Reason != "by hand" || st.Scope != leadstore.ScopeMailbox || st.StateAt.IsZero() {
		t.Errorf("latest state = %+v, want paused by hand", st)
	}
	var rows int
	if err := storetest.OwnerConn(t).QueryRow(ctx, "SELECT count(*) FROM mailbox_state WHERE mailbox_id = $1", id).Scan(&rows); err != nil || rows != 3 {
		t.Errorf("state rows = %d (%v), want 3 (append-only history)", rows, err)
	}
	if err := q.SetMailboxState(ctx, id, "sleeping", "", leadstore.ScopeMailbox, leadstore.ActorOperator); !errors.Is(err, leadstore.ErrUnknownMailboxState) {
		t.Errorf("unknown state: %v", err)
	}
	if err := q.SetMailboxState(ctx, id, leadstore.MailboxPaused, "", "planet", leadstore.ActorOperator); !errors.Is(err, leadstore.ErrUnknownMailboxState) {
		t.Errorf("unknown scope: %v", err)
	}
	if err := q.SetMailboxState(ctx, id, leadstore.MailboxPaused, "", leadstore.ScopeMailbox, leadstore.ActorGremy); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("old actor name on a new table: %v, want ErrUnknownActor", err)
	}
	if err := q.SetMailboxState(ctx, 999, leadstore.MailboxPaused, "", leadstore.ScopeMailbox, leadstore.ActorOperator); !errors.Is(err, leadstore.ErrMailboxNotFound) {
		t.Errorf("missing mailbox: %v", err)
	}
}

func TestMailboxRetiredIsFinal(t *testing.T) {
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	ctx := context.Background()
	id := addMailbox(t, q, "a@mail.example")
	if err := q.SetMailboxState(ctx, id, leadstore.MailboxRetired, "done with it", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{leadstore.MailboxActive, leadstore.MailboxPaused, leadstore.MailboxRetired} {
		if err := q.SetMailboxState(ctx, id, s, "", leadstore.ScopeMailbox, leadstore.ActorOperator); !errors.Is(err, leadstore.ErrMailboxRetired) {
			t.Errorf("%s after retired: %v, want ErrMailboxRetired", s, err)
		}
	}
	if st := statusOf(t, q, id, time.Now()); st.State != leadstore.MailboxRetired {
		t.Errorf("state = %s, want retired", st.State)
	}
	if n, err := q.MailboxesOnDomain(ctx, "mail.example"); err != nil || n != 0 {
		t.Errorf("non-retired on domain = %d, %v; want 0", n, err)
	}
}

func TestMailboxDomainState(t *testing.T) {
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	ctx := context.Background()
	a := addMailbox(t, q, "a@mail.example")
	b := addMailbox(t, q, "b@mail.example")
	gone := addMailbox(t, q, "c@mail.example")
	other := addMailbox(t, q, "d@other.example")
	if err := q.SetMailboxState(ctx, gone, leadstore.MailboxRetired, "", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	if n, err := q.MailboxesOnDomain(ctx, "Mail.Example"); err != nil || n != 2 {
		t.Errorf("non-retired on domain = %d, %v; want 2", n, err)
	}
	n, err := q.SetDomainState(ctx, "mail.example", leadstore.MailboxPaused, "policy block 5.7.1", leadstore.ActorSystem)
	if err != nil || n != 2 {
		t.Fatalf("set domain state = %d, %v; want 2 rows", n, err)
	}
	for _, id := range []int64{a, b} {
		st := statusOf(t, q, id, time.Now())
		if st.State != leadstore.MailboxPaused || st.Scope != leadstore.ScopeDomain || st.Reason != "policy block 5.7.1" {
			t.Errorf("mailbox %d after domain pause = %+v", id, st)
		}
	}
	if st := statusOf(t, q, gone, time.Now()); st.State != leadstore.MailboxRetired {
		t.Errorf("retired mailbox changed by a domain pause: %+v", st)
	}
	if st := statusOf(t, q, other, time.Now()); st.State != leadstore.MailboxActive {
		t.Errorf("another domain's mailbox changed: %+v", st)
	}
	if _, err := q.SetDomainState(ctx, "mail.example", "nap", "", leadstore.ActorSystem); !errors.Is(err, leadstore.ErrUnknownMailboxState) {
		t.Errorf("unknown state: %v", err)
	}
}

func TestMailboxConnectionEditNeedsSeal(t *testing.T) {
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	ctx := context.Background()
	id := addMailbox(t, q, "a@mail.example")
	s := func(v string) *string { return &v }

	// Name and selector edits need no password.
	if err := q.UpdateMailbox(ctx, id, leadstore.MailboxEdit{FromName: s("New Name"), DKIMSelector: s("s2")}); err != nil {
		t.Fatalf("edit name and selector: %v", err)
	}
	// Setting a connection field to its current value is no change.
	if err := q.UpdateMailbox(ctx, id, leadstore.MailboxEdit{Login: s("a@mail.example")}); err != nil {
		t.Fatalf("same login: %v", err)
	}
	for name, e := range map[string]leadstore.MailboxEdit{
		"login": {Login: s("other@mail.example")},
		"smtp":  {SMTPAddr: s("smtp.evil.invalid:465")},
		"imap":  {IMAPAddr: s("imap.evil.invalid:993")},
	} {
		if err := q.UpdateMailbox(ctx, id, e); !errors.Is(err, leadstore.ErrSealRequired) {
			t.Errorf("%s edit without a new sealed value: %v, want ErrSealRequired", name, err)
		}
	}
	got, _ := q.MailboxByID(ctx, id)
	if got.FromName != "New Name" || got.DKIMSelector != "s2" || got.Login != "a@mail.example" ||
		got.SMTPAddr != "smtp.mail.example:465" || string(got.PasswordSealed) != "sealed-placeholder" {
		t.Errorf("after refused edits = %+v", got)
	}
	// With a new sealed value the connection edit goes through.
	if err := q.UpdateMailbox(ctx, id, leadstore.MailboxEdit{
		SMTPAddr: s("smtp2.mail.example:587"), PasswordSealed: []byte("sealed-two"), SealedBy: "fp-two",
	}); err != nil {
		t.Fatalf("edit with a new seal: %v", err)
	}
	got, _ = q.MailboxByID(ctx, id)
	if got.SMTPAddr != "smtp2.mail.example:587" || string(got.PasswordSealed) != "sealed-two" || got.SealedBy != "fp-two" {
		t.Errorf("after the sealed edit = %+v", got)
	}
	if err := q.UpdateMailbox(ctx, id, leadstore.MailboxEdit{PasswordSealed: []byte("x")}); !errors.Is(err, leadstore.ErrBadMailbox) {
		t.Errorf("sealed value without a fingerprint: %v, want ErrBadMailbox", err)
	}
	if err := q.UpdateMailbox(ctx, id, leadstore.MailboxEdit{FromName: s("A\nB")}); !errors.Is(err, leadstore.ErrBadMailbox) {
		t.Errorf("from name with a line break: %v, want ErrBadMailbox", err)
	}
	if err := q.UpdateMailbox(ctx, 999, leadstore.MailboxEdit{FromName: s("x")}); !errors.Is(err, leadstore.ErrMailboxNotFound) {
		t.Errorf("missing mailbox: %v", err)
	}
}

func TestMailboxChecks(t *testing.T) {
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	ctx := context.Background()
	id := addMailbox(t, q, "a@mail.example")
	cs, _ := q.RequestedChecks(ctx)
	if len(cs) != 1 {
		t.Fatalf("requested = %d, want 1", len(cs))
	}
	// A second request while one waits returns the waiting one.
	again, err := q.RequestCheck(ctx, id, leadstore.ActorOperator)
	if err != nil || again != cs[0].ID {
		t.Fatalf("request while one waits = %d, %v; want %d", again, err, cs[0].ID)
	}
	yes, no := true, false
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	if err := q.FinishCheck(ctx, cs[0].ID, leadstore.MailboxCheckResult{
		SMTPOK: &yes, IMAPOK: &yes, FoldersOK: &no, SPF: &yes, DKIM: &yes, DMARC: &yes,
		Fix: "create a Junk folder", Outcome: leadstore.CheckFailed,
	}, at); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := q.FinishCheck(ctx, cs[0].ID, leadstore.MailboxCheckResult{Outcome: leadstore.CheckPassed}, at); !errors.Is(err, leadstore.ErrCheckNotRequested) {
		t.Errorf("finish twice: %v, want ErrCheckNotRequested", err)
	}
	if err := q.FinishCheck(ctx, cs[0].ID, leadstore.MailboxCheckResult{Outcome: leadstore.CheckRequested}, at); !errors.Is(err, leadstore.ErrBadMailbox) {
		t.Errorf("finish as requested: %v, want ErrBadMailbox", err)
	}
	if left, _ := q.RequestedChecks(ctx); len(left) != 0 {
		t.Errorf("requested after finish = %v", left)
	}
	st := statusOf(t, q, id, at)
	c := st.LastCheck
	if c == nil || c.Outcome != leadstore.CheckFailed || c.CheckedAt == nil || !c.CheckedAt.Equal(at) ||
		c.FoldersOK == nil || *c.FoldersOK || c.SMTPOK == nil || !*c.SMTPOK || c.Fix != "create a Junk folder" {
		t.Fatalf("last check = %+v", c)
	}
	next, err := q.RequestCheck(ctx, id, leadstore.ActorSystem)
	if err != nil || next == cs[0].ID {
		t.Fatalf("new request = %d, %v", next, err)
	}
	if st := statusOf(t, q, id, at); st.LastCheck == nil || st.LastCheck.ID != next || st.LastCheck.Outcome != leadstore.CheckRequested ||
		st.LastCheck.SMTPOK != nil {
		t.Errorf("last check after a new request = %+v", st.LastCheck)
	}
	if _, err := q.RequestCheck(ctx, 999, leadstore.ActorOperator); !errors.Is(err, leadstore.ErrMailboxNotFound) {
		t.Errorf("request for a missing mailbox: %v", err)
	}
}

func TestMailboxDNSPassedTodayPerSelector(t *testing.T) {
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	ctx := context.Background()
	a := addMailbox(t, q, "a@mail.example") // selector lh
	b := addMailbox(t, q, "b@mail.example")
	if err := q.UpdateMailbox(ctx, b, leadstore.MailboxEdit{DKIMSelector: func(v string) *string { return &v }("s2")}); err != nil {
		t.Fatal(err)
	}
	c := addMailbox(t, q, "c@other.example") // selector lh, another domain
	today := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	sha := leadstore.SenderDomainSHA256("mail.example")
	pass := mailbox.DNSResult{SPF: true, DKIM: true, DMARC: true}
	// Yesterday's pass and today's failure for lh; today's pass for s2.
	for _, r := range []struct {
		sel string
		res mailbox.DNSResult
		at  time.Time
	}{
		{"lh", pass, today.Add(-time.Hour)},
		{"lh", mailbox.DNSResult{SPF: true, DMARC: true, Note: "no dkim record"}, today.Add(6 * time.Hour)},
		{"s2", pass, today.Add(6 * time.Hour)},
	} {
		if err := q.RecordSenderCheckSelector(ctx, sha, r.sel, r.res, r.at); err != nil {
			t.Fatal(err)
		}
	}
	for _, w := range []struct {
		id   int64
		want bool
	}{{a, false}, {b, true}, {c, false}} {
		if st := statusOf(t, q, w.id, today); st.DNSPassedToday != w.want {
			t.Errorf("mailbox %d DNSPassedToday = %v, want %v", w.id, st.DNSPassedToday, w.want)
		}
	}
	for _, w := range []struct {
		sel   string
		since time.Time
		want  bool
	}{{"lh", today, false}, {"lh", today.Add(-2 * time.Hour), true}, {"s2", today, true}, {"", today, false}} {
		if ok, err := q.SenderCheckPassedSinceSelector(ctx, sha, w.sel, w.since); err != nil || ok != w.want {
			t.Errorf("passed since %s for %q = %v, %v; want %v", w.since, w.sel, ok, err, w.want)
		}
	}
	// A selector-less pass (the old single-mailbox check) passes no mailbox.
	if err := q.RecordSenderCheck(ctx, leadstore.SenderDomainSHA256("other.example"), pass, today.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, q, c, today); st.DNSPassedToday {
		t.Errorf("a pass with no selector counted for mailbox %d", c)
	}
	// The old query still reads every row of the domain, whatever its selector.
	if ok, err := q.SenderCheckPassedSince(ctx, sha, today); err != nil || !ok {
		t.Errorf("SenderCheckPassedSince = %v, %v; want true", ok, err)
	}
}

// liveMessages makes one live approved batch of lead's two steps, assigns them to mailbox
// mb as the owner (the app role may not), and returns their ids by step.
func (f outreachFixture) liveMessages(t *testing.T, lead, mb int64, shadow bool) []int64 {
	t.Helper()
	ctx := context.Background()
	batchID, hash := f.batch(t, shadow, lead)
	f.approve(t, batchID, hash)
	ms, err := f.q.BatchMessages(ctx, batchID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, m := range ms {
		if _, err := f.owner.Exec(ctx, "UPDATE outreach_message SET mailbox_id = $1 WHERE id = $2", mb, m.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	return ids
}

func (f outreachFixture) setMsg(t *testing.T, id int64, state string, sentAt *time.Time, hardBounce bool) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(),
		"UPDATE outreach_message SET state = $2, sent_at = $3, hard_bounce = $4 WHERE id = $1", id, state, sentAt, hardBounce); err != nil {
		t.Fatal(err)
	}
}

func TestMailboxSendCounts(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := addMailbox(t, f.q, "a@mail.example")
	b := addMailbox(t, f.q, "b@mail.example")
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	at := func(h int) *time.Time { v := day.Add(time.Duration(h) * time.Hour); return &v }

	l1 := f.siteLead(t, "one.example", nil, 7, "info@one.example")
	l2 := f.siteLead(t, "two.example", nil, 7, "info@two.example")
	l3 := f.siteLead(t, "three.example", nil, 7, "info@three.example")
	l4 := f.siteLead(t, "four.example", nil, 7, "info@four.example")
	m1 := f.liveMessages(t, l1, a, false)
	m2 := f.liveMessages(t, l2, a, false)
	m3 := f.liveMessages(t, l3, b, false)
	m4 := f.liveMessages(t, l4, b, true)       // shadow: never counts
	f.setMsg(t, m1[0], "sent", at(-30), false) // yesterday: first live send of a
	f.setMsg(t, m2[0], "sent", at(9), true)
	f.setMsg(t, m2[1], "maybe_sent", at(10), false)
	f.setMsg(t, m3[0], "refused", at(11), true)
	f.setMsg(t, m4[0], "shadow_sent", at(8), true)
	// Left: m1[1] approved (a), m3[1] approved (b), m4[1] approved in a shadow batch.

	sent, err := f.q.SentSinceByMailbox(ctx, day)
	if err != nil || sent[a] != 2 || sent[b] != 0 || len(sent) != 1 {
		t.Errorf("sent since by mailbox = %v, %v; want a:2", sent, err)
	}
	first, err := f.q.FirstLiveSendByMailbox(ctx)
	if err != nil || len(first) != 1 || !first[a].Equal(*at(-30)) {
		t.Errorf("first live send = %v, %v; want a at %v", first, err, at(-30))
	}
	waiting, err := f.q.WaitingByMailbox(ctx)
	if err != nil || waiting[a] != 2 || waiting[b] != 1 {
		t.Errorf("waiting = %v, %v; want a:2 (one approved, one maybe sent), b:1", waiting, err)
	}
	st := statusOf(t, f.q, a, day)
	if st.SentToday != 2 || st.FirstLiveSend == nil || !st.FirstLiveSend.Equal(*at(-30)) {
		t.Errorf("status of a = sent today %d, first %v", st.SentToday, st.FirstLiveSend)
	}
	if st := statusOf(t, f.q, b, day); st.SentToday != 0 || st.FirstLiveSend != nil {
		t.Errorf("status of b = sent today %d, first %v", st.SentToday, st.FirstLiveSend)
	}

	// Hard bounces in the last n live sends, newest first.
	for _, w := range []struct {
		mb   int64
		n    int
		want int
	}{{a, 1, 0}, {a, 2, 1}, {a, 50, 1}, {b, 50, 1}} {
		if got, err := f.q.HardBouncesInLastSends(ctx, w.mb, w.n); err != nil || got != w.want {
			t.Errorf("hard bounces of %d in last %d = %d, %v; want %d", w.mb, w.n, got, err, w.want)
		}
	}

	// Cancelling a mailbox's pending messages leaves the other mailbox's alone.
	n, err := f.q.CancelMailboxPending(ctx, a, "mailbox retired")
	if err != nil || n != 1 {
		t.Fatalf("cancel pending of a = %d, %v; want 1", n, err)
	}
	var state, reason string
	if err := f.owner.QueryRow(ctx, "SELECT state, cancel_reason FROM outreach_message WHERE id = $1", m1[1]).Scan(&state, &reason); err != nil ||
		state != "cancelled" || reason != "mailbox retired" {
		t.Errorf("a's follow-up = %s %q (%v)", state, reason, err)
	}
	if err := f.owner.QueryRow(ctx, "SELECT state FROM outreach_message WHERE id = $1", m3[1]).Scan(&state); err != nil || state != "approved" {
		t.Errorf("b's follow-up = %s (%v), want approved", state, err)
	}
}

func TestSeedMailbox(t *testing.T) {
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	m := leadstoretest.SeedMailbox(t, q, "seed@mail.example")
	if m.ID == 0 || m.Address != "seed@mail.example" || m.Domain != "mail.example" || len(m.PasswordSealed) == 0 {
		t.Fatalf("seeded = %+v", m)
	}
	st := statusOf(t, q, m.ID, time.Now())
	if st.State != leadstore.MailboxActive || st.LastCheck == nil || st.LastCheck.Outcome != leadstore.CheckPassed {
		t.Errorf("seeded status = %+v", st)
	}
}

// TestMailboxDNSByHostAndDomainPause: a mailbox on a subdomain reads the DNS pass of its own
// host, not its registrable domain's; a domain pause skips a mailbox already paused, and a
// pause by PauseMailboxIfActive writes once.
func TestMailboxDNSByHostAndDomainPause(t *testing.T) {
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	ctx := context.Background()
	x := addMailbox(t, q, "x@mail.brand.example")
	y := addMailbox(t, q, "y@brand.example")
	today := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	pass := mailbox.DNSResult{SPF: true, DKIM: true, DMARC: true}
	if err := q.RecordSenderCheckSelector(ctx, leadstore.SenderDomainSHA256("brand.example"), "lh", pass, today.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, q, x, today); st.Domain != "brand.example" || st.Host() != "mail.brand.example" || st.DNSPassedToday {
		t.Errorf("x = %q %q passed %v; want brand.example, mail.brand.example, not passed by the domain's row", st.Domain, st.Host(), st.DNSPassedToday)
	}
	if err := q.RecordSenderCheckSelector(ctx, leadstore.SenderDomainSHA256("mail.brand.example"), "lh", pass, today.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, q, x, today); !st.DNSPassedToday {
		t.Error("x does not read its host's pass")
	}
	for range 2 {
		if _, err := q.PauseMailboxIfActive(ctx, x, "2 hard bounces", leadstore.ActorSystem); err != nil {
			t.Fatal(err)
		}
	}
	n, err := q.SetDomainState(ctx, "brand.example", leadstore.MailboxPaused, "policy block 5.7.1", leadstore.ActorSystem)
	if err != nil || n != 1 {
		t.Fatalf("domain pause = %d, %v; want 1 row (y alone)", n, err)
	}
	if st := statusOf(t, q, x, today); st.Reason != "2 hard bounces" || st.Scope != leadstore.ScopeMailbox {
		t.Errorf("x after the domain pause = %s %q, want its own pause", st.Scope, st.Reason)
	}
	if st := statusOf(t, q, y, today); st.State != leadstore.MailboxPaused || st.Scope != leadstore.ScopeDomain {
		t.Errorf("y after the domain pause = %+v", st)
	}
	var rows int
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT count(*) FROM mailbox_state WHERE mailbox_id = $1`, x).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("x has %d state rows (%v), want 1", rows, err)
	}
}
