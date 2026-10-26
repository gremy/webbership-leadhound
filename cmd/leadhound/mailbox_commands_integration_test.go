//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
)

// The commands that read every sending mailbox, now that the old single-mailbox variables
// are no longer read: leads sync and leads senders check print one line per mailbox (per
// domain and selector for the check), and leads check-config names a retired variable
// still set.

// TestSyncCommandPerMailbox: leads sync reads every mailbox that is not retired with its own
// sealed password and prints one line per mailbox, its findings under it; a mailbox that
// cannot be read is its own line and makes the command fail while the others still sync; a
// missing key fails the command; with no mailbox it says so and needs no key.
func TestSyncCommandPerMailbox(t *testing.T) {
	w := newSyncWorld(t)
	ctx := context.Background()
	w.sendFrom(t, w.a, "reply.example")
	w.mail.boxOf(bLogin).deliver("INBOX", replyHeader("reply.example", w.firstMessageID(t, "reply.example"), "Re: hello"), "Sounds interesting.")

	out := w.mustRun(t, "leads", "sync")
	want := fmt.Sprintf("Mailbox %d: synced, nothing new.\nMailbox %d: synced.\n  1 reply to read in the outreach mailbox: reply.example\n  2 pending messages cancelled.\n", w.a.ID, w.b.ID)
	if out != want {
		t.Errorf("sync:\n got %q\nwant %q", out, want)
	}
	if s := w.leadState(t, "reply.example"); s != leadstore.LeadReplied {
		t.Errorf("reply lead %s, want replied", s)
	}

	// A's seal no longer matches its login: A is its own line, B still syncs, the command fails.
	ownerExec(t, `UPDATE sender_mailbox SET login = 'someone@evil.invalid' WHERE id = $1`, w.a.ID)
	code, out := w.run(t, "leads", "sync")
	wantLines(t, "a mailbox not synced", out,
		fmt.Sprintf("Mailbox %d: not synced: ", w.a.ID), "re-enter the password: the mailbox's settings changed since it was saved",
		fmt.Sprintf("Mailbox %d: synced, nothing new.", w.b.ID))
	if code != 1 {
		t.Errorf("a mailbox not synced: exit %d, want 1:\n%s", code, out)
	}

	// No key: nothing is read.
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv(secretbox.KeyFileEnv, filepath.Join(t.TempDir(), "missing.key"))
	readers := len(w.mail.callsOf("reader"))
	if code, out := w.run(t, "leads", "sync"); code != 1 || !strings.Contains(out, "No sync: the mailbox key is missing") {
		t.Errorf("no key: exit %d:\n%s", code, out)
	}
	if n := len(w.mail.callsOf("reader")); n != readers {
		t.Errorf("a sync without the key read %d mailboxes", n-readers)
	}

	// No mailbox that is not retired: said plainly, and no key is needed.
	for _, id := range []int64{w.a.ID, w.b.ID} {
		if err := w.lq.SetMailboxState(ctx, id, leadstore.MailboxRetired, "gone", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
	}
	if out := w.mustRun(t, "leads", "sync"); out != "No mailbox to sync: add one in the console (Mailboxes) or with leads mailboxes add.\n" {
		t.Errorf("no mailbox:\n%s", out)
	}
	w.sweep(t, "info@reply.example", bLogin, testPassword)
}

// TestSendersCheckPerMailbox: leads senders check checks each unique domain and selector of
// the mailboxes that are not retired once, records it with its selector, and prints one line
// for each, naming the mailboxes and never the domain; it opens no password.
func TestSendersCheckPerMailbox(t *testing.T) {
	w := newSendWorld(t, true, 20)
	ctx := context.Background()
	a := w.mailbox
	b := seedMailbox(t, w.lq, "b@other.example")     // no records at all
	c := seedMailbox(t, w.lq, "c@"+testSenderDomain) // a's domain and selector: one check
	d := seedMailbox(t, w.lq, "d@"+testSenderDomain, withSelector("s9"))
	if err := w.lq.SetMailboxState(ctx, d.ID, leadstore.MailboxRetired, "gone", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secretbox.KeyFileEnv, filepath.Join(t.TempDir(), "missing.key")) // the check needs no key

	out := w.mustRun(t, "leads", "senders", "check")
	pass := fmt.Sprintf("Mailboxes %d, %d (selector sel1): SPF pass · DKIM pass · DMARC pass: sending may go ahead today.\n", a.ID, c.ID)
	fail := fmt.Sprintf("Mailbox %d (selector sel1): SPF fail · DKIM fail · DMARC fail: sending waits until every record passes.", b.ID)
	if !strings.HasPrefix(out, pass) || !strings.Contains(out, fail) || strings.Count(out, "\n") != 2 {
		t.Errorf("check:\n%s\nwant two lines:\n%s\n%s", out, pass, fail)
	}
	for _, dom := range []string{testSenderDomain, "other.example"} {
		if strings.Contains(out, dom) {
			t.Errorf("output names %s:\n%s", dom, out)
		}
	}
	if n := w.count(t, `SELECT count(*) FROM sender_check WHERE selector = 'sel1' AND spf AND dkim AND dmarc AND domain_sha256 = $1`, leadstore.SenderDomainSHA256(testSenderDomain)); n != 1 {
		t.Errorf("%d passing checks of a's domain, want 1", n)
	}
	if n := w.count(t, `SELECT count(*) FROM sender_check WHERE selector = 'sel1' AND NOT spf AND domain_sha256 = $1`, leadstore.SenderDomainSHA256("other.example")); n != 1 {
		t.Errorf("%d failing checks of b's domain, want 1", n)
	}
	if n := w.count(t, `SELECT count(*) FROM sender_check WHERE selector = 's9'`); n != 0 {
		t.Error("a retired mailbox was checked")
	}
	if len(w.mail.calls) != 0 {
		t.Errorf("the check reached a mailbox: %+v", w.mail.calls)
	}

	for _, id := range []int64{a.ID, b.ID, c.ID} {
		if err := w.lq.SetMailboxState(ctx, id, leadstore.MailboxRetired, "gone", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
	}
	if out := w.mustRun(t, "leads", "senders", "check"); out != "No mailbox to check: add one in the console (Mailboxes) or with leads mailboxes add.\n" {
		t.Errorf("no mailbox:\n%s", out)
	}
	if code, _ := w.run(t, "leads", "senders"); code != 2 {
		t.Errorf("senders without check exited %d", code)
	}
	w.sweep(t, "b@other.example")
}

// TestCheckConfigRetiredEnv: check-config says, in one line after its counts, that the old
// single-mailbox variables still set are no longer read, naming the first and never a value.
func TestCheckConfigRetiredEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../examples")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADHOUND_DATABASE_URL", "")
	t.Setenv("LEADHOUND_DATA_DIR", dir)
	for _, k := range leadconfig.RetiredEnv {
		t.Setenv(k, "")
	}
	check := func(t *testing.T) string {
		t.Helper()
		code, out, errOut := leadsRun(t, "leads", "check-config", "--agencies", filepath.Join(dir, "agencies.demo.yaml"))
		if code != 0 {
			t.Fatalf("check-config exited %d: %s", code, errOut)
		}
		for _, v := range []string{testPassword, testUser, "imap.sender"} {
			if strings.Contains(out+errOut, v) {
				t.Errorf("check-config shows a value:\n%s", out)
			}
		}
		return out
	}
	if out := check(t); strings.Contains(out, "no longer read") || strings.Count(out, "\n") != 1 {
		t.Errorf("nothing retired set:\n%s", out)
	}
	t.Setenv(leadconfig.RetiredEnv[1], testPassword)
	if out := check(t); !strings.HasSuffix(out, "\n"+leadconfig.RetiredEnv[1]+" is no longer read: add the mailbox in the console (Mailboxes)\n") {
		t.Errorf("one retired set:\n%s", out)
	}
	t.Setenv(leadconfig.RetiredEnv[0], testUser)
	t.Setenv(leadconfig.RetiredEnv[3], "imap.sender.example:993")
	if out := check(t); !strings.HasSuffix(out, "\n"+leadconfig.RetiredEnv[0]+" (and 2 more) are no longer read: add the mailbox in the console (Mailboxes)\n") {
		t.Errorf("three retired set:\n%s", out)
	}
}
