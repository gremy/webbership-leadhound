package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// mailDeps are the outreach mailbox's outside world: DNS, IMAP, SMTP and the clock's
// sleeps. Tests replace leadsMail with fakes, so no test reaches a real server.
type mailDeps struct {
	resolver   mailbox.Resolver
	openReader func(ctx context.Context, env leadconfig.Env) (mailbox.Reader, error)
	smtp       func(env leadconfig.Env) mailSender
	// The per-mailbox world: each sending mailbox's own servers and login (addr is
	// host:port). smtpFor builds its sender, readerFor dials and logs in to its IMAP,
	// probeSMTP and probeIMAP test its logins without sending.
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
	openReader: func(ctx context.Context, env leadconfig.Env) (mailbox.Reader, error) {
		r := &mailbox.IMAPReader{Addr: env.IMAPAddr, User: env.User, Password: env.Password}
		if err := r.Dial(ctx); err != nil {
			return nil, err
		}
		return r, nil
	},
	smtp: func(env leadconfig.Env) mailSender {
		return &mailbox.SMTPSender{Addr: env.SMTPAddr, User: env.User, Password: env.Password}
	},
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

// sendingDomain is the part after "@" in LEADHOUND_OUTREACH_USER, lower case; ok is false
// without a usable address.
func sendingDomain(env leadconfig.Env) (string, bool) {
	user := strings.TrimSpace(env.User.Reveal())
	i := strings.LastIndexByte(user, '@')
	if i <= 0 || i == len(user)-1 {
		return "", false
	}
	return strings.ToLower(user[i+1:]), true
}

// dkimSelector is sender.dkim_selector, leadconfig.DefaultDKIMSelector when blank.
func dkimSelector(cfg leadconfig.Config) string {
	if s := strings.TrimSpace(cfg.Sender.DKIMSelector); s != "" {
		return s
	}
	return leadconfig.DefaultDKIMSelector
}

// checkSender runs the DNS check of the sending domain and records it (domain hashed).
func checkSender(ctx context.Context, lq leadstore.Queries, domain, selector string, now time.Time) (mailbox.DNSResult, error) {
	res := mailbox.CheckDNS(ctx, leadsMail.resolver, domain, selector)
	if err := lq.RecordSenderCheck(ctx, leadstore.SenderDomainSHA256(domain), res, now); err != nil {
		return res, err
	}
	return res, nil
}

// checkSenderFor runs the DNS check of a mailbox's domain with its own DKIM selector and
// records it with that selector (domain hashed), which is what a mailbox's readiness reads.
func checkSenderFor(ctx context.Context, lq leadstore.Queries, domain, selector string, now time.Time) (mailbox.DNSResult, error) {
	res := mailbox.CheckDNS(ctx, leadsMail.resolver, domain, selector)
	if err := lq.RecordSenderCheckSelector(ctx, leadstore.SenderDomainSHA256(domain), selector, res, now); err != nil {
		return res, err
	}
	return res, nil
}

// errMailboxCreds: a mailbox's sealed password cannot be used, so mailboxCreds paused it.
var errMailboxCreds = errors.New("the mailbox is paused")

// mailboxCreds opens mb's sealed password with key and returns it only when it was sealed
// by key's public key for exactly mb's current address, login and servers. Otherwise it
// pauses mb (actor system, with the reason the operator reads) and returns an error that
// wraps errMailboxCreds and never carries the password. It is the only place in this
// command that opens a sealed password.
func mailboxCreds(ctx context.Context, lq leadstore.Queries, key secretbox.PrivateKey, mb leadstore.Mailbox) (secretbox.Credentials, error) {
	reason := "re-enter the password: it was saved under a different key"
	if mb.SealedBy == secretbox.Fingerprint(key.Public()) {
		creds, err := secretbox.Open(key, mb.PasswordSealed)
		switch {
		case err != nil:
			reason = "re-enter the password: it could not be opened"
		case !creds.Matches(mb.Address, mb.Login, mb.SMTPAddr, mb.IMAPAddr):
			reason = "re-enter the password: the mailbox's settings changed since it was saved"
		default:
			return creds, nil
		}
	}
	refused := fmt.Errorf("%w: mailbox %d: %s", errMailboxCreds, mb.ID, reason)
	if err := lq.SetMailboxState(ctx, mb.ID, leadstore.MailboxPaused, reason, leadstore.ScopeMailbox, leadstore.ActorSystem); err != nil &&
		!errors.Is(err, leadstore.ErrMailboxRetired) {
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

// printDNS writes one line per record and the note (which never names the domain).
func printDNS(w io.Writer, r mailbox.DNSResult) {
	_, _ = fmt.Fprintf(w, "SPF %s\nDKIM %s\nDMARC %s\n", passFail(r.SPF), passFail(r.DKIM), passFail(r.DMARC))
	if r.Note != "" {
		_, _ = fmt.Fprintf(w, "Note: %s\n", r.Note)
	}
}

// runLeadsSenders is `leads senders check`: SPF, DKIM (sender.dkim_selector) and DMARC for
// the sending domain, the part after "@" in LEADHOUND_OUTREACH_USER. It records the check
// and prints pass or fail per record, never the domain. A failing record still exits 0: the
// check ran, and send reads its row.
func runLeadsSenders(ctx context.Context, args []string) error {
	const usage = "usage: leadhound leads senders check"
	if len(args) != 1 || args[0] != "check" {
		return usageError("%s", usage)
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	domain, ok := sendingDomain(setup.env)
	if !ok {
		return usageError("LEADHOUND_OUTREACH_USER is not set to the outreach mailbox address; the check reads the sending domain from it")
	}
	res, err := checkSender(ctx, leadstore.NewQueries(s.Pool()), domain, dkimSelector(setup.leads), leadsNow())
	if err != nil {
		return err
	}
	if res.OK() {
		_, _ = fmt.Fprintln(stdout, "Sending domain DNS: all three records pass; send may run today.")
	} else {
		_, _ = fmt.Fprintln(stdout, "Sending domain DNS: a record fails, so send will not send until it passes.")
	}
	printDNS(stdout, res)
	return nil
}
