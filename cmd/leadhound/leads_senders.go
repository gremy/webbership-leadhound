package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
)

func init() { leadsCommands["senders"] = runLeadsSenders }

// mailSender submits one message; *mailbox.SMTPSender is one.
type mailSender interface {
	Send(ctx context.Context, from, to string, raw []byte) (reply string, err error)
}

// mailDeps are the sending mailboxes' outside world: DNS, IMAP, SMTP and the clock's
// sleeps. Tests replace leadsMail with fakes, so no test reaches a real server.
type mailDeps struct {
	resolver mailbox.Resolver
	// Each sending mailbox's own servers and login (addr is host:port). smtpFor builds its
	// sender, readerFor dials and logs in to its IMAP, probeSMTP and probeIMAP test its
	// logins without sending.
	smtpFor   func(addr string, user, password config.Secret) mailSender
	readerFor func(ctx context.Context, addr string, user, password config.Secret) (mailbox.Reader, error)
	probeSMTP func(ctx context.Context, addr string, user, password config.Secret) error
	probeIMAP func(ctx context.Context, addr string, user, password config.Secret) (mailbox.Mailboxes, error)
	// sleep waits d or until ctx ends (the spacing between two sends).
	sleep func(ctx context.Context, d time.Duration) error
	rand  *rand.Rand
	// syncWait is how long send waits for a sync that is running (RA26); lockPoll is how
	// often it tries again.
	syncWait, lockPoll time.Duration
	// allowReserved lets a live send through to a reserved name (.example …). Only tests
	// set it: every fixture lives on a reserved name, and live send refuses them (RA25).
	allowReserved bool
}

var leadsMail = mailDeps{
	resolver: net.DefaultResolver,
	smtpFor: func(addr string, user, password config.Secret) mailSender {
		return &mailbox.SMTPSender{Addr: addr, User: user, Password: password}
	},
	readerFor: func(ctx context.Context, addr string, user, password config.Secret) (mailbox.Reader, error) {
		r := &mailbox.IMAPReader{Addr: addr, User: user, Password: password}
		if err := r.Dial(ctx); err != nil {
			return nil, err
		}
		return r, nil
	},
	probeSMTP: func(ctx context.Context, addr string, user, password config.Secret) error {
		return (&mailbox.SMTPSender{Addr: addr, User: user, Password: password}).Probe(ctx)
	},
	probeIMAP: func(ctx context.Context, addr string, user, password config.Secret) (mailbox.Mailboxes, error) {
		return mailbox.ProbeIMAP(ctx, &mailbox.IMAPReader{Addr: addr, User: user, Password: password})
	},
	sleep:    sleepCtx,
	rand:     rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x4c48)), // #nosec G404 G115 -- spacing jitter, not a secret
	syncWait: 60 * time.Second,
	lockPoll: time.Second,
}

// sleepCtx waits d, or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// dkimSelector is sender.dkim_selector, leadconfig.DefaultDKIMSelector when blank.
func dkimSelector(cfg leadconfig.Config) string {
	if s := strings.TrimSpace(cfg.Sender.DKIMSelector); s != "" {
		return s
	}
	return leadconfig.DefaultDKIMSelector
}

// checkSenderFor runs the DNS check of a mailbox's host (its address's own host, where its
// records are; Mailbox.Host) with its own DKIM selector and records it with that selector
// (host hashed), which is what a mailbox's readiness reads.
func checkSenderFor(ctx context.Context, lq leadstore.Queries, host, selector string, now time.Time) (mailbox.DNSResult, error) {
	res := mailbox.CheckDNS(ctx, leadsMail.resolver, host, selector)
	if err := lq.RecordSenderCheckSelector(ctx, leadstore.SenderDomainSHA256(host), selector, res, now); err != nil {
		return res, err
	}
	return res, nil
}

// errMailboxCreds: a mailbox's sealed password cannot be used, so mailboxCreds paused it.
var errMailboxCreds = errors.New("the mailbox is paused")

// openCreds opens mb's sealed password with key and returns it only when it was sealed by
// key's public key for exactly mb's current address, login and servers; otherwise the
// reason the operator reads, and never the password. It writes nothing. It is the only
// place in this command that opens a sealed password.
func openCreds(key secretbox.PrivateKey, mb leadstore.Mailbox) (secretbox.Credentials, string) {
	if mb.SealedBy != secretbox.Fingerprint(key.Public()) {
		return secretbox.Credentials{}, "re-enter the password: it was saved under a different key"
	}
	creds, err := secretbox.Open(key, mb.PasswordSealed)
	switch {
	case err != nil:
		return secretbox.Credentials{}, "re-enter the password: it could not be opened"
	case !creds.Matches(mb.Address, mb.Login, mb.SMTPAddr, mb.IMAPAddr):
		return secretbox.Credentials{}, "re-enter the password: the mailbox's settings changed since it was saved"
	}
	return creds, ""
}

// mailboxCreds is openCreds for a job that uses the mailbox: when the password cannot be
// used it pauses mb when it is active (actor system, with the reason the operator reads) and
// returns an error that wraps errMailboxCreds and never carries the password.
func mailboxCreds(ctx context.Context, lq leadstore.Queries, key secretbox.PrivateKey, mb leadstore.Mailbox) (secretbox.Credentials, error) {
	creds, reason := openCreds(key, mb)
	if reason == "" {
		return creds, nil
	}
	refused := fmt.Errorf("%w: mailbox %d: %s", errMailboxCreds, mb.ID, reason)
	// Only an active mailbox gets the pause row: a sync every 15 minutes finds a paused one
	// so again, and the operator's own pause (its reason, its scope) stays the latest row.
	if _, err := lq.PauseMailboxIfActive(ctx, mb.ID, reason, leadstore.ActorSystem); err != nil {
		return secretbox.Credentials{}, errors.Join(refused, err)
	}
	return secretbox.Credentials{}, refused
}

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

// dnsLine is "SPF pass · DKIM fail · DMARC pass": never the domain.
func dnsLine(r mailbox.DNSResult) string {
	return fmt.Sprintf("SPF %s · DKIM %s · DMARC %s", passFail(r.SPF), passFail(r.DKIM), passFail(r.DMARC))
}

// runLeadsSenders is `leads senders check`: SPF, DKIM and DMARC for each unique address host
// and DKIM selector of the mailboxes that are not retired, each checked once and recorded
// with its selector (what a mailbox's readiness reads). It prints one line per host and
// selector, naming its mailboxes by id and never the host, and opens no password. A
// failing record still exits 0: the check ran, and send reads its row.
func runLeadsSenders(ctx context.Context, args []string) error {
	const usage = "usage: leadhound leads senders check"
	if len(args) != 1 || args[0] != "check" {
		return usageError("%s", usage)
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	now := leadsNow()
	boxes, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, now)
	if err != nil {
		return err
	}
	type target struct {
		host, selector string
		ids            []int64
	}
	var targets []*target
	byKey := map[string]*target{}
	for _, mb := range boxes {
		if mb.State == leadstore.MailboxRetired {
			continue
		}
		k := mb.Host() + " " + mb.DKIMSelector
		t, ok := byKey[k]
		if !ok {
			t = &target{host: mb.Host(), selector: mb.DKIMSelector}
			byKey[k] = t
			targets = append(targets, t)
		}
		t.ids = append(t.ids, mb.ID)
	}
	if len(targets) == 0 {
		_, _ = fmt.Fprintln(stdout, "No mailbox to check: add one in the console (Mailboxes) or with leads mailboxes add.")
		return nil
	}
	for _, t := range targets {
		res, err := checkSenderFor(ctx, lq, t.host, t.selector, now)
		if err != nil {
			return err
		}
		verdict := "sending waits until every record passes."
		if res.OK() {
			verdict = "sending may go ahead today."
		}
		line := fmt.Sprintf("%s (selector %s): %s: %s", mailboxIDs(t.ids), t.selector, dnsLine(res), verdict)
		if res.Note != "" {
			line += " Note: " + res.Note
		}
		_, _ = fmt.Fprintln(stdout, line)
	}
	return nil
}

// mailboxIDs is "Mailbox 3" or "Mailboxes 1, 3".
func mailboxIDs(ids []int64) string {
	if len(ids) == 1 {
		return fmt.Sprintf("Mailbox %d", ids[0])
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return "Mailboxes " + strings.Join(parts, ", ")
}
