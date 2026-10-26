//go:build integration

package main

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// `leads suppress review` (FO6, FO8): it re-reads each recorded opt-out from its mailbox and
// lists the ones that now read as replies with their lift lines, writing nothing. Every
// fixture is recorded "the old way": through ApplyIncomingFor with kind opt_out, as the
// classifier before FO1-FO3 recorded it, so the source is mailbox:<id>:INBOX:<uid>. Every
// address is built at run time from reserved names.

// ourEnLine is examples/leads.yaml's English opt-out line.
var ourEnLine = `Not the right time? Reply "unsubscribe" and I will not write to you again.`

// quotedReply is a reply that only quotes our email (and its opt-out line) under a Gmail
// attribution: the false opt-out the old classifier recorded.
func quotedReply() string {
	return "Thanks, let's talk next week.\n\nOn Mon, 26 Oct 2026 at 18:10, Sam <" + testUser + "> wrote:\n" +
		"> Hello,\n> I saw your site still runs Drupal 7.\n> " + ourEnLine + "\n"
}

// realOptOut is a reply whose own words ask to hear no more, above the same quote.
func realOptOut() string {
	return "unsubscribe\n\nOn Mon, 26 Oct 2026 at 18:10, Sam <" + testUser + "> wrote:\n> Hello,\n> " + ourEnLine + "\n"
}

// oldSelftestReply is the live incident's shape: the operator's reply to a selftest sent
// before FO3, quoting its word "unsubscribe".
func oldSelftestReply() string {
	return "Got it.\n\nOn Mon, 26 Oct 2026 at 18:10, Sam <" + testUser + "> wrote:\n" +
		"> Leadhound selftest, 26 Oct 2026 18:10 EET.\n>\n> " + mailbox.OldSelftestLine + "\n"
}

// sentRefTo is the sent message to address.
func sentRefTo(t *testing.T, w *sendWorld, address string) leadstore.SentRef {
	t.Helper()
	refs, err := w.lq.SentRefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.ToAddress == address {
			return r
		}
	}
	t.Fatalf("no sent message to the fixture address of %s", maskAddress(address))
	return leadstore.SentRef{}
}

// recordOldOptOut delivers a message from fromDomain into w's INBOX and records it as the old
// classifier did: an opt_out matched to ref, with its suppressions. It returns the event id
// and the message's UID.
func recordOldOptOut(t *testing.T, w *sendWorld, ref leadstore.SentRef, fromDomain, text string) (int64, uint32) {
	t.Helper()
	ctx := context.Background()
	w.box.deliver("INBOX", replyHeader(fromDomain, ref.MessageID, "Re: hello"), text)
	w.box.mu.Lock()
	uid := uint32(len(w.box.msgs["INBOX"])) // #nosec G115 -- a handful of test messages
	validity := w.box.validity["INBOX"]
	w.box.mu.Unlock()
	if err := leadsInTx(ctx, storetestPool(t), func(q leadstore.Queries) error {
		_, err := q.ApplyIncomingFor(ctx, w.mailbox.ID, leadstore.Incoming{Mailbox: "INBOX", UIDValidity: validity, UID: uid,
			Kind: mailbox.KindOptOut, FromDomain: fromDomain, Matched: &ref, At: leadsNow()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	id := int64(w.count(t, `SELECT id::int FROM mailbox_event WHERE mailbox_id = $1 AND mailbox = 'INBOX' AND uid = $2`, w.mailbox.ID, int64(uid)))
	return id, uid
}

// dropMessage removes uid from w's INBOX, as a deleted message.
func dropMessage(w *sendWorld, uid uint32) {
	w.box.mu.Lock()
	defer w.box.mu.Unlock()
	w.box.msgs["INBOX"] = slices.DeleteFunc(w.box.msgs["INBOX"], func(f mailbox.Fetched) bool { return f.UID == uid })
}

// sentLead sends one live first email to info@<domain> and returns its sent message.
func sentLead(t *testing.T, w *sendWorld, domain string) leadstore.SentRef {
	t.Helper()
	w.batch(t, false, map[int64]string{w.lead(t, domain): domain})
	w.mustRun(t, "leads", "send")
	return sentRefTo(t, w, "info"+"@"+domain)
}

// review runs `leads suppress review` and fails on any unmasked address in its output.
func review(t *testing.T, w *sendWorld) (int, string) {
	t.Helper()
	code, out, errOut := w.runSplit(t, "leads", "suppress", "review")
	t.Logf("review exited %d:\n%s%s", code, out, errOut)
	if unmaskedAddress.MatchString(out + errOut) {
		t.Errorf("review printed an address:\n%s%s", out, errOut)
	}
	return code, out + errOut
}

// unmaskedAddress is an @ after a character a masked address never has there ("j***@").
var unmaskedAddress = regexp.MustCompile(`[A-Za-z0-9._%+-]@`)

func liftLine(id int64, domain string) string {
	return fmt.Sprintf(`  To lift: leads suppress lift %d --domain %s --reason "reply quoted our opt-out line"`, id, domain)
}

func noKey(t *testing.T) {
	t.Helper()
	t.Setenv(secretbox.KeyFileEnv, "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")
}

// TestSuppressReviewColdStart: no opt-out recorded → the line, exit 0, and the key is never
// loaded (its env is unset).
func TestSuppressReviewColdStart(t *testing.T) {
	w := newSendWorld(t, true, 20)
	noKey(t)
	code, out := review(t, w)
	if code != 0 || strings.TrimSpace(out) != "No opt-outs to review." {
		t.Errorf("review exited %d:\n%s", code, out)
	}
	if code, out := w.run(t, "leads", "suppress", "review", "extra"); code != 2 || !strings.Contains(out, "review") {
		t.Errorf("review with an argument exited %d:\n%s", code, out)
	}
	w.sweep(t)
}

// TestSuppressReviewMissingKey: events but no key → the one line, exit 1, nothing read.
func TestSuppressReviewMissingKey(t *testing.T) {
	w := newSendWorld(t, true, 20)
	ref := sentLead(t, w, "nokey.example")
	recordOldOptOut(t, w, ref, "nokey.example", quotedReply())
	opens := w.box.opens
	noKey(t)
	code, out := review(t, w)
	if code != 1 || !strings.HasPrefix(out, "No review: the mailbox key is missing (") || !strings.Contains(out, "); nothing was read.\n") {
		t.Errorf("review exited %d:\n%s", code, out)
	}
	if w.box.opens != opens {
		t.Error("the mailbox was opened without a key")
	}
}

// TestSuppressReviewFalseAndReal: the live selftest incident recorded the old way reads as a
// reply with its exact lift line; a quoted reply from a lead lists both its rows; a real
// opt-out is counted as still one; a lifted row shows as already lifted.
func TestSuppressReviewFalseAndReal(t *testing.T) {
	w := newSendWorld(t, true, 20)
	w.mustRun(t, "leads", "selftest", "--to", testOperator)
	self := sentRefTo(t, w, testOperator)
	selfEv, selfUID := recordOldOptOut(t, w, self, testSenderDomain, oldSelftestReply())
	quotedRef := sentLead(t, w, "quoted.example")
	quotedEv, _ := recordOldOptOut(t, w, quotedRef, "quoted.example", quotedReply())
	realRef := sentLead(t, w, "optout.example")
	recordOldOptOut(t, w, realRef, "optout.example", realOptOut())

	selfRow := suppressionID(t, w, leadstore.SuppressAddress, testOperator)
	qAddr := suppressionID(t, w, leadstore.SuppressAddress, "info"+"@"+"quoted.example")
	qDom := suppressionID(t, w, leadstore.SuppressDomain, "quoted.example")
	day := leadsDay(leadsNow())

	code, out := review(t, w)
	if code != 0 {
		t.Fatalf("review exited %d:\n%s", code, out)
	}
	wantLines(t, "review", out,
		fmt.Sprintf("Event %d · mailbox %d INBOX uid %d · your selftest · %s: now a reply (the opt-out word was only in quoted text).\n",
			selfEv, w.mailbox.ID, selfUID, day),
		liftLine(selfRow, testSenderDomain)+"\n",
		fmt.Sprintf("Event %d · mailbox %d INBOX uid", quotedEv, w.mailbox.ID),
		"· quoted.example · "+day+": now a reply",
		liftLine(qAddr, "quoted.example")+"\n",
		liftLine(qDom, "quoted.example")+"\n",
		"1 still read as opt-outs (subject or their own words): no change.\n",
		"0 could not be re-read.\n")
	if !strings.HasSuffix(out, "Nothing was lifted: review and lift by hand.\n") {
		t.Errorf("review does not end with the nothing-lifted line:\n%s", out)
	}
	// Decision first: the replies come before the counts.
	if strings.Index(out, "now a reply") > strings.Index(out, "still read as opt-outs") {
		t.Errorf("review is not decision-first:\n%s", out)
	}
	if strings.Contains(out, "optout.example") {
		t.Errorf("a real opt-out got a line of its own:\n%s", out)
	}

	// The operator lifts the selftest row; the review then shows it as lifted.
	w.mustRun(t, "leads", "suppress", "lift", fmt.Sprint(selfRow), "--domain", testSenderDomain, "--reason", "reply quoted our opt-out line")
	_, out = review(t, w)
	wantLines(t, "review after lift", out, fmt.Sprintf("  #%d already lifted\n", selfRow))
	if strings.Contains(out, liftLine(selfRow, testSenderDomain)) {
		t.Errorf("a lifted row got a lift line:\n%s", out)
	}
	w.sweep(t, "info"+"@"+"quoted.example", "info"+"@"+"optout.example")
}

// TestSuppressReviewCannotReread: a renumbered folder, a deleted message, a retired mailbox
// and a mailbox with no password each leave the event not re-read, with the reason.
func TestSuppressReviewCannotReread(t *testing.T) {
	cases := []struct {
		name  string
		spoil func(t *testing.T, w *sendWorld, uid uint32)
		want  string
	}{
		{"renumbered", func(_ *testing.T, w *sendWorld, _ uint32) {
			w.box.mu.Lock()
			w.box.validity["INBOX"]++
			w.box.mu.Unlock()
		}, "can't re-read: the folder was renumbered\n"},
		{"deleted", func(_ *testing.T, w *sendWorld, uid uint32) { dropMessage(w, uid) },
			"can't re-read: the message is no longer in INBOX\n"},
		{"retired", func(t *testing.T, w *sendWorld, _ uint32) {
			if err := w.lq.SetMailboxState(context.Background(), w.mailbox.ID, leadstore.MailboxRetired, "test", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
				t.Fatal(err)
			}
		}, "can't re-read: mailbox %d is retired\n"},
		{"no password", func(t *testing.T, w *sendWorld, _ uint32) {
			ownerExec(t, `UPDATE sender_mailbox SET password_sealed = '', sealed_by = '' WHERE id = $1`, w.mailbox.ID)
		}, "can't re-read: mailbox %d has no password\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newSendWorld(t, true, 20)
			ref := sentLead(t, w, "gone.example")
			ev, uid := recordOldOptOut(t, w, ref, "gone.example", quotedReply())
			c.spoil(t, w, uid)
			code, out := review(t, w)
			want := c.want
			if strings.Contains(want, "%d") {
				want = fmt.Sprintf(want, w.mailbox.ID)
			}
			wantLines(t, c.name, out, "No recorded opt-out reads as a reply now.\n", "1 could not be re-read:\n",
				fmt.Sprintf("  Event %d · mailbox %d INBOX uid %d · gone.example · ", ev, w.mailbox.ID, uid), want,
				"Nothing was lifted: review and lift by hand.\n")
			if code != 0 || strings.Contains(out, "To lift:") {
				t.Errorf("review exited %d:\n%s", code, out)
			}
			if n := w.count(t, `SELECT count(*)::int FROM mailbox_state WHERE mailbox_id = $1 AND actor = 'system'`, w.mailbox.ID); n != 0 {
				t.Errorf("the review paused the mailbox (%d system state rows)", n)
			}
		})
	}
}

// TestReviewWillNotLiftASharedRow (FO8): a row that another opt-out also leans on gets no
// lift line, whether that opt-out still reads as one or could not be re-read.
func TestReviewWillNotLiftASharedRow(t *testing.T) {
	t.Run("address row", func(t *testing.T) {
		w := newSendWorld(t, true, 20)
		ref := sentLead(t, w, "shared.example")
		falseEv, _ := recordOldOptOut(t, w, ref, "shared.example", quotedReply())
		realEv, _ := recordOldOptOut(t, w, ref, "shared.example", realOptOut())
		addr := suppressionID(t, w, leadstore.SuppressAddress, "info"+"@"+"shared.example")
		dom := suppressionID(t, w, leadstore.SuppressDomain, "shared.example")
		_, out := review(t, w)
		wantLines(t, "shared address", out, fmt.Sprintf("Event %d · ", falseEv),
			fmt.Sprintf("  #%d also covers event %d (still an opt-out): do not lift.\n", addr, realEv),
			fmt.Sprintf("  #%d also covers event %d (still an opt-out): do not lift.\n", dom, realEv),
			"1 still read as opt-outs")
		if strings.Contains(out, "To lift:") {
			t.Errorf("a shared row got a lift line:\n%s", out)
		}
		w.sweep(t, "info"+"@"+"shared.example")
	})
	t.Run("domain row", func(t *testing.T) {
		// Two people at one company: a false opt-out to info@acme, and a real one from
		// someone else at acme answering our email to another lead.
		w := newSendWorld(t, true, 20)
		acme := sentLead(t, w, "acme.example")
		other := sentLead(t, w, "other.example")
		falseEv, _ := recordOldOptOut(t, w, acme, "acme.example", quotedReply())
		realEv, _ := recordOldOptOut(t, w, other, "acme.example", realOptOut())
		addr := suppressionID(t, w, leadstore.SuppressAddress, "info"+"@"+"acme.example")
		dom := suppressionID(t, w, leadstore.SuppressDomain, "acme.example")
		_, out := review(t, w)
		wantLines(t, "shared domain", out, fmt.Sprintf("Event %d · ", falseEv),
			liftLine(addr, "acme.example")+"\n",
			fmt.Sprintf("  #%d also covers event %d (still an opt-out): do not lift.\n", dom, realEv))
		if strings.Contains(out, liftLine(dom, "acme.example")) {
			t.Errorf("the shared domain row got a lift line:\n%s", out)
		}
		w.sweep(t, "info"+"@"+"acme.example", "info"+"@"+"other.example")
	})
	t.Run("not re-readable", func(t *testing.T) {
		w := newSendWorld(t, true, 20)
		ref := sentLead(t, w, "shared.example")
		recordOldOptOut(t, w, ref, "shared.example", quotedReply())
		realEv, realUID := recordOldOptOut(t, w, ref, "shared.example", realOptOut())
		dropMessage(w, realUID)
		addr := suppressionID(t, w, leadstore.SuppressAddress, "info"+"@"+"shared.example")
		_, out := review(t, w)
		wantLines(t, "unreadable", out,
			fmt.Sprintf("  #%d also covers event %d (could not be re-read): do not lift.\n", addr, realEv),
			"1 could not be re-read:\n", "can't re-read: the message is no longer in INBOX\n")
		if strings.Contains(out, "To lift:") {
			t.Errorf("a row leaned on by an unread opt-out got a lift line:\n%s", out)
		}
	})
}

// dbSnapshot is everything a review must leave as it was: the row count of every base table,
// the full contents of mailbox_cursor (a cursor move changes no count) and the last lift id.
func dbSnapshot(t *testing.T) map[string]string {
	t.Helper()
	ctx := context.Background()
	conn := storetest.OwnerConn(t)
	rows, err := conn.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	if len(tables) < 20 {
		t.Fatalf("only %d tables found", len(tables))
	}
	snap := map[string]string{}
	for _, tb := range tables {
		var n int64
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM "`+tb+`"`).Scan(&n); err != nil { // tb is a catalog name
			t.Fatalf("%s: %v", tb, err)
		}
		snap["count "+tb] = fmt.Sprint(n)
	}
	var cursor, lift string
	if err := conn.QueryRow(ctx, `SELECT coalesce(string_agg(row_to_json(c)::text, ' | ' ORDER BY row_to_json(c)::text), '')
		FROM mailbox_cursor c`).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT coalesce(max(id), 0)::text FROM suppression_lift`).Scan(&lift); err != nil {
		t.Fatal(err)
	}
	snap["mailbox_cursor"], snap["max suppression_lift.id"] = cursor, lift
	return snap
}

// TestReviewWritesNothing: a review over a false opt-out, a real one, an unreadable one and
// a mailbox whose password was sealed under another key (mailboxCreds would pause it)
// leaves every table, the cursors and the lifts exactly as they were.
func TestReviewWritesNothing(t *testing.T) {
	w := newSendWorld(t, true, 20)
	recordOldOptOut(t, w, sentLead(t, w, "quoted.example"), "quoted.example", quotedReply())
	recordOldOptOut(t, w, sentLead(t, w, "optout.example"), "optout.example", realOptOut())
	_, uid := recordOldOptOut(t, w, sentLead(t, w, "gone.example"), "gone.example", quotedReply())
	dropMessage(w, uid)
	// A second mailbox with an opt-out, its password sealed under another key.
	b := seedMailbox(t, w.lq, bLogin)
	ownerExec(t, `INSERT INTO mailbox_event (mailbox, uidvalidity, uid, kind, from_domain, seen_at, mailbox_id)
		VALUES ('INBOX', 7, 1, 'opt_out', 'bkey.example', now(), $1)`, b.ID)
	ownerExec(t, `UPDATE sender_mailbox SET sealed_by = 'another-key' WHERE id = $1`, b.ID)

	before := dbSnapshot(t)
	code, out := review(t, w)
	after := dbSnapshot(t)
	if code != 0 {
		t.Fatalf("review exited %d:\n%s", code, out)
	}
	wantLines(t, "review", out, "now a reply", "1 still read as opt-outs", "2 could not be re-read:",
		fmt.Sprintf("mailbox %d: re-enter the password: it was saved under a different key", b.ID))
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: %s before the review, %s after", k, v, after[k])
		}
	}
	if len(after) != len(before) {
		t.Errorf("%d snapshot keys before, %d after", len(before), len(after))
	}
}
