package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/idna"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// The sending mailboxes' shared core: the console and `leads mailboxes` call these same
// functions, so both write identical rows and refuse with the same words (each refusal
// wraps console.ErrUserFixable through refused). The console holds only the public key:
// it seals, it never opens. Opening a sealed password happens in runChecks (through
// mailboxCreds), which only the mailbox-check worker runs.

// mailboxCheckAction is the request file the console writes for the mailbox-check job
// (<data dir>/run/mailbox-check), as Run now does for its jobs.
const mailboxCheckAction = "mailbox-check"

// MailboxPreset is a mail provider's public servers and DKIM selector, which fill the add
// form. Host names live here only, never in deploy/ or the docs.
type MailboxPreset struct {
	Name, SMTP, IMAP, Selector string
}

// mailboxPresets are the providers whose servers were read from their own documentation
// (Spacemail: its help pages on mail client settings and DKIM). A new preset is added only
// after reading that provider's docs. Other fills nothing.
var mailboxPresets = []MailboxPreset{
	{Name: "Spacemail", SMTP: "mail.spacemail.com:465", IMAP: "mail.spacemail.com:993", Selector: "spacemail"},
	{Name: "Other"},
}

// mailboxPreset finds a preset by name, in any case.
func mailboxPreset(name string) (MailboxPreset, bool) {
	for _, p := range mailboxPresets {
		if strings.EqualFold(p.Name, strings.TrimSpace(name)) {
			return p, true
		}
	}
	return MailboxPreset{}, false
}

func presetNames() string {
	names := make([]string, len(mailboxPresets))
	for i, p := range mailboxPresets {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

// MailboxInput is a mailbox as the operator enters it, in the console's form or on the
// CLI. On add, a blank login is the address, blank servers and selector come from Preset
// (the selector else from leads.yaml). On edit, a blank login, server or selector keeps
// the current one, a blank Password keeps the sealed one, and FromName is taken as given
// (blank: leads.yaml's sender.name).
type MailboxInput struct {
	Address, Login, SMTPAddr, IMAPAddr, Selector, FromName, Preset string
	Password                                                       config.Secret
}

var (
	smtpPorts = []string{"465", "587"}
	imapPorts = []string{"993"}
)

// normalizeMailboxAddress is the address in lower case with its domain in punycode, and its
// registrable domain.
func normalizeMailboxAddress(raw string) (addr, domain string, err error) {
	a := strings.ToLower(strings.TrimSpace(raw))
	bad := refused("%q is not one mailbox address: write it as name@domain.", strings.TrimSpace(raw))
	local, host, ok := strings.Cut(a, "@")
	if !ok || local == "" || host == "" || strings.Contains(host, "@") ||
		strings.ContainsFunc(a, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(`<>,;"()[]\`, r)
		}) {
		return "", "", bad
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", "", bad
	}
	if domain, err = sitelist.Registrable(ascii); err != nil {
		return "", "", bad
	}
	return local + "@" + ascii, domain, nil
}

// checkServer refuses an address that is not host:port with one of ports. what is
// "sending" or "reading".
func checkServer(what, addr string, ports []string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || host == "" || strings.ContainsFunc(host, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune("/@", r) }) ||
		!slices.Contains(ports, port) {
		return refused("The %s server must be host:port with port %s.", what, strings.Join(ports, " or "))
	}
	return nil
}

func checkMailboxFromName(name string) error {
	if strings.ContainsAny(name, "\r\n") {
		return refused("The From name cannot hold a line break.")
	}
	return nil
}

func checkPublicKey(pub secretbox.PublicKey) error {
	if pub == (secretbox.PublicKey{}) {
		return refused("%s is not set, so the password cannot be sealed; see deploy/README.md, Mailboxes.", secretbox.PublicKeyEnv)
	}
	return nil
}

// sealFor seals password for exactly mb's address, login and servers, with pub.
func sealFor(pub secretbox.PublicKey, mb leadstore.Mailbox, password config.Secret) ([]byte, string, error) {
	sealed, err := secretbox.Seal(pub, secretbox.Credentials{
		Address: mb.Address, Login: mb.Login, SMTPAddr: mb.SMTPAddr, IMAPAddr: mb.IMAPAddr, Password: password,
	}, nil)
	if err != nil {
		return nil, "", err
	}
	return sealed, secretbox.Fingerprint(pub), nil
}

// mailboxFromInput is the mailbox in to be added, filled from its preset and cfg, with
// every field checked.
func mailboxFromInput(cfg leadconfig.Config, in MailboxInput) (leadstore.Mailbox, error) {
	addr, domain, err := normalizeMailboxAddress(in.Address)
	if err != nil {
		return leadstore.Mailbox{}, err
	}
	m := leadstore.Mailbox{
		Address: addr, Domain: domain, Login: strings.TrimSpace(in.Login),
		SMTPAddr: strings.TrimSpace(in.SMTPAddr), IMAPAddr: strings.TrimSpace(in.IMAPAddr),
		DKIMSelector: strings.TrimSpace(in.Selector), FromName: strings.TrimSpace(in.FromName),
	}
	if strings.TrimSpace(in.Preset) != "" {
		p, ok := mailboxPreset(in.Preset)
		if !ok {
			return leadstore.Mailbox{}, refused("There is no preset %s; the presets are %s.", strings.TrimSpace(in.Preset), presetNames())
		}
		m.SMTPAddr = cmp.Or(m.SMTPAddr, p.SMTP)
		m.IMAPAddr = cmp.Or(m.IMAPAddr, p.IMAP)
		m.DKIMSelector = cmp.Or(m.DKIMSelector, p.Selector)
	}
	m.Login = cmp.Or(m.Login, m.Address)
	m.DKIMSelector = cmp.Or(m.DKIMSelector, dkimSelector(cfg))
	switch {
	case m.SMTPAddr == "":
		return leadstore.Mailbox{}, refused("Give the sending server as host:port, or choose a preset.")
	case m.IMAPAddr == "":
		return leadstore.Mailbox{}, refused("Give the reading server as host:port, or choose a preset.")
	}
	if err := checkServer("sending", m.SMTPAddr, smtpPorts); err != nil {
		return leadstore.Mailbox{}, err
	}
	if err := checkServer("reading", m.IMAPAddr, imapPorts); err != nil {
		return leadstore.Mailbox{}, err
	}
	if strings.ContainsFunc(m.Login, unicode.IsControl) || strings.ContainsFunc(m.DKIMSelector, unicode.IsSpace) {
		return leadstore.Mailbox{}, refused("The login and the DKIM selector cannot hold spaces or line breaks.")
	}
	return m, checkMailboxFromName(m.FromName)
}

// mailboxStatusByID is mailbox id with its latest state and check, for the operator's day
// starting at since; a missing one is refused.
func mailboxStatusByID(ctx context.Context, lq leadstore.Queries, id int64, since time.Time) (leadstore.MailboxStatus, error) {
	ms, err := lq.Mailboxes(ctx, since)
	if err != nil {
		return leadstore.MailboxStatus{}, err
	}
	for _, m := range ms {
		if m.ID == id {
			return m, nil
		}
	}
	return leadstore.MailboxStatus{}, refused("No mailbox %d.", id)
}

// addMailbox adds the mailbox in: the address normalised (lower case, the domain in
// punycode), its registrable domain derived, the credentials sealed with pub for exactly
// the stored address, login and servers, and its first check requested in the same
// statement; then it writes the request file for the mailbox-check job. It refuses a bad
// address, a reserved name in a real run, an address already added, a domain at
// caps.mailboxes_per_domain, ports other than 465/587 and 993, a From name with a line
// break and an empty password. It returns the new mailbox's id.
func addMailbox(ctx context.Context, lq leadstore.Queries, cfg leadconfig.Config, pub secretbox.PublicKey, dataDir string,
	now time.Time, in MailboxInput) (int64, error) {
	m, err := mailboxFromInput(cfg, in)
	if err != nil {
		return 0, err
	}
	if in.Password.Reveal() == "" {
		return 0, refused("The password is empty: enter the mailbox's password.")
	}
	if err := checkPublicKey(pub); err != nil {
		return 0, err
	}
	if outreach.ReservedDomain(m.Domain) && !leadsMail.allowReserved {
		return 0, refused("%s is a reserved test name; a real mailbox needs a real domain.", m.Domain)
	}
	ms, err := lq.Mailboxes(ctx, now)
	if err != nil {
		return 0, err
	}
	onDomain := 0
	for _, x := range ms {
		if x.Address == m.Address {
			return 0, refused("%s is already mailbox %d.", m.Address, x.ID)
		}
		if x.Domain == m.Domain && x.State != leadstore.MailboxRetired {
			onDomain++
		}
	}
	limit := min(cmp.Or(cfg.Caps.MailboxesPerDomain, leadconfig.DefaultMailboxesPerDomain), leadconfig.HardMailboxesPerDomain)
	if onDomain >= limit {
		return 0, refused("%s already has %d mailboxes, the most leads.yaml allows (caps.mailboxes_per_domain). "+
			"Mail providers judge reputation by domain, so a new mailbox is safer on another domain.", m.Domain, onDomain)
	}
	if m.PasswordSealed, m.SealedBy, err = sealFor(pub, m, in.Password); err != nil {
		return 0, err
	}
	id, err := lq.AddMailbox(ctx, m, leadstore.ActorOperator)
	if errors.Is(err, leadstore.ErrMailboxExists) {
		return 0, refused("%s is already a mailbox.", m.Address)
	}
	if err != nil {
		return 0, err
	}
	if err := writeRunRequest(dataDir, mailboxCheckAction, now); err != nil {
		return id, fmt.Errorf("mailbox %d is added, but its check could not be started: %w", id, err)
	}
	return id, nil
}

// editMailbox changes mailbox id as in says (see MailboxInput). A change of login or server
// needs the password again, because the seal binds the password to where it may be used; a
// new password is sealed for the new fields. A new password, a connection change or a new
// selector requests a check (and writes the request file); recheck says whether it did.
// The address never changes, and a retired mailbox is not edited.
func editMailbox(ctx context.Context, lq leadstore.Queries, pub secretbox.PublicKey, dataDir string, now time.Time,
	id int64, in MailboxInput) (recheck bool, err error) {
	st, err := mailboxStatusByID(ctx, lq, id, now)
	if err != nil {
		return false, err
	}
	if st.State == leadstore.MailboxRetired {
		return false, refused("Mailbox %d is retired; add a new mailbox instead.", id)
	}
	if strings.TrimSpace(in.Address) != "" {
		if addr, _, err := normalizeMailboxAddress(in.Address); err != nil || addr != st.Address {
			return false, refused("A mailbox's address never changes; retire it and add a new one.")
		}
	}
	m := st.Mailbox
	m.Login = cmp.Or(strings.TrimSpace(in.Login), st.Login)
	m.SMTPAddr = cmp.Or(strings.TrimSpace(in.SMTPAddr), st.SMTPAddr)
	m.IMAPAddr = cmp.Or(strings.TrimSpace(in.IMAPAddr), st.IMAPAddr)
	m.DKIMSelector = cmp.Or(strings.TrimSpace(in.Selector), st.DKIMSelector)
	m.FromName = strings.TrimSpace(in.FromName)
	if err := checkServer("sending", m.SMTPAddr, smtpPorts); err != nil {
		return false, err
	}
	if err := checkServer("reading", m.IMAPAddr, imapPorts); err != nil {
		return false, err
	}
	if strings.ContainsFunc(m.Login, unicode.IsControl) || strings.ContainsFunc(m.DKIMSelector, unicode.IsSpace) {
		return false, refused("The login and the DKIM selector cannot hold spaces or line breaks.")
	}
	if err := checkMailboxFromName(m.FromName); err != nil {
		return false, err
	}
	needPassword := refused("A change of login or server needs the password again: the password is sealed to the servers it may be used with.")
	connChanged := m.Login != st.Login || m.SMTPAddr != st.SMTPAddr || m.IMAPAddr != st.IMAPAddr
	newPassword := in.Password.Reveal() != ""
	if connChanged && !newPassword {
		return false, needPassword
	}
	edit := leadstore.MailboxEdit{Login: &m.Login, SMTPAddr: &m.SMTPAddr, IMAPAddr: &m.IMAPAddr, DKIMSelector: &m.DKIMSelector, FromName: &m.FromName}
	if newPassword {
		if err := checkPublicKey(pub); err != nil {
			return false, err
		}
		if edit.PasswordSealed, edit.SealedBy, err = sealFor(pub, m, in.Password); err != nil {
			return false, err
		}
	}
	if err := lq.UpdateMailbox(ctx, id, edit); err != nil {
		if errors.Is(err, leadstore.ErrSealRequired) {
			return false, needPassword
		}
		return false, err
	}
	if connChanged || m.DKIMSelector != st.DKIMSelector {
		// Where a mailbox signs in and sends from is the one change that can redirect its
		// mail, so the operator can read every such change back; never the password or seal.
		slog.Default().Info("mailbox connection changed", "mailbox", id,
			"login_was", st.Login, "login", m.Login, "smtp_addr_was", st.SMTPAddr, "smtp_addr", m.SMTPAddr,
			"imap_addr_was", st.IMAPAddr, "imap_addr", m.IMAPAddr, "selector_was", st.DKIMSelector, "selector", m.DKIMSelector)
	}
	if !newPassword && !connChanged && m.DKIMSelector == st.DKIMSelector {
		return false, nil
	}
	if _, err := lq.RequestCheck(ctx, id, leadstore.ActorOperator); err != nil {
		return false, err
	}
	if err := writeRunRequest(dataDir, mailboxCheckAction, now); err != nil {
		return true, fmt.Errorf("mailbox %d is saved, but its check could not be started: %w", id, err)
	}
	return true, nil
}

// MailboxSwitch is what a pause or resume of one mailbox did: Changed is false when it was
// already so (then Since and Reason are the standing pause's, for a pause).
type MailboxSwitch struct {
	Changed bool
	Since   time.Time
	Reason  string
}

// setMailboxPause pauses (with reason) or resumes mailbox id; setting it to what it already
// is writes nothing. A retired mailbox is refused.
func setMailboxPause(ctx context.Context, lq leadstore.Queries, now time.Time, id int64, pause bool, reason string) (MailboxSwitch, error) {
	st, err := mailboxStatusByID(ctx, lq, id, now)
	if err != nil {
		return MailboxSwitch{}, err
	}
	switch {
	case st.State == leadstore.MailboxRetired:
		return MailboxSwitch{}, refused("Mailbox %d is retired; a retired mailbox is never paused or resumed.", id)
	case pause && st.State == leadstore.MailboxPaused:
		return MailboxSwitch{Since: st.StateAt, Reason: st.Reason}, nil
	case !pause && st.State == leadstore.MailboxActive:
		return MailboxSwitch{}, nil
	}
	state, why := leadstore.MailboxActive, ""
	if pause {
		state, why = leadstore.MailboxPaused, strings.TrimSpace(reason)
	}
	if err := lq.SetMailboxState(ctx, id, state, why, leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		if errors.Is(err, leadstore.ErrMailboxRetired) {
			return MailboxSwitch{}, refused("Mailbox %d is retired; a retired mailbox is never paused or resumed.", id)
		}
		return MailboxSwitch{}, err
	}
	return MailboxSwitch{Changed: true}, nil
}

// DomainSwitch is what a pause or resume of a domain did: Changed mailboxes were switched;
// Kept are paused on their own, which a domain resume leaves paused.
type DomainSwitch struct {
	Domain        string
	Changed, Kept int
}

// normalizeDomain is a domain as the operator types it, in lower case and punycode, as its
// registrable domain when it has one.
func normalizeDomain(raw string) string {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if ascii, err := idna.Lookup.ToASCII(d); err == nil {
		d = ascii
	}
	if r, err := sitelist.Registrable(d); err == nil {
		return r
	}
	return d
}

// setDomainPause pauses (with reason) every active mailbox on domain, or resumes every one
// paused for the domain (scope domain); a mailbox paused on its own stays paused. A domain
// with no mailbox that is not retired is refused.
func setDomainPause(ctx context.Context, lq leadstore.Queries, now time.Time, domain string, pause bool, reason string) (DomainSwitch, error) {
	d := normalizeDomain(domain)
	ms, err := lq.Mailboxes(ctx, now)
	if err != nil {
		return DomainSwitch{}, err
	}
	sw := DomainSwitch{Domain: d}
	found := false
	for _, m := range ms {
		if m.Domain != d || m.State == leadstore.MailboxRetired {
			continue
		}
		found = true
		var state, why string
		switch {
		case pause && m.State == leadstore.MailboxActive:
			state, why = leadstore.MailboxPaused, strings.TrimSpace(reason)
		case !pause && m.State == leadstore.MailboxPaused && m.Scope == leadstore.ScopeDomain:
			state = leadstore.MailboxActive
		case !pause && m.State == leadstore.MailboxPaused:
			sw.Kept++
			continue
		default:
			continue
		}
		if err := lq.SetMailboxState(ctx, m.ID, state, why, leadstore.ScopeDomain, leadstore.ActorOperator); err != nil && !errors.Is(err, leadstore.ErrMailboxRetired) {
			return sw, err
		}
		sw.Changed++
	}
	if !found {
		return DomainSwitch{}, refused("No mailbox is on %s.", d)
	}
	return sw, nil
}

// retireMailbox retires mailbox id for good and cancels its unsent messages with reason
// "mailbox retired" (a thread must come from the same address); it returns how many it
// cancelled. Without confirm it is refused. Retiring a retired mailbox again only cancels
// what is left.
func retireMailbox(ctx context.Context, lq leadstore.Queries, now time.Time, id int64, confirm bool) (int, error) {
	st, err := mailboxStatusByID(ctx, lq, id, now)
	if err != nil {
		return 0, err
	}
	if !confirm {
		return 0, refused("Retiring mailbox %d is final: it never sends again and its unsent messages are cancelled. Confirm to retire it.", id)
	}
	if st.State != leadstore.MailboxRetired {
		if err := lq.SetMailboxState(ctx, id, leadstore.MailboxRetired, "", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil &&
			!errors.Is(err, leadstore.ErrMailboxRetired) {
			return 0, err
		}
	}
	return lq.CancelMailboxPending(ctx, id, "mailbox retired")
}

// requestCheck asks for a check of mailbox id (one waiting check at most) and writes the
// request file for the mailbox-check job; it returns the check's id. A retired mailbox is
// refused.
func requestCheck(ctx context.Context, lq leadstore.Queries, dataDir string, now time.Time, id int64) (int64, error) {
	st, err := mailboxStatusByID(ctx, lq, id, now)
	if err != nil {
		return 0, err
	}
	if st.State == leadstore.MailboxRetired {
		return 0, refused("Mailbox %d is retired; it is not checked.", id)
	}
	cid, err := lq.RequestCheck(ctx, id, leadstore.ActorOperator)
	if err != nil {
		return 0, err
	}
	if err := writeRunRequest(dataDir, mailboxCheckAction, now); err != nil {
		return cid, fmt.Errorf("mailbox %d: the check could not be started: %w", id, err)
	}
	return cid, nil
}

// CheckDone is one check runChecks ran.
type CheckDone struct {
	MailboxID, CheckID int64
	Outcome, Fix       string
}

// runChecks runs every requested check, and again until none is left, so a request made
// while it runs is answered by the same run. Per check: the sealed password (mailboxCreds:
// unusable → did_not_run with "re-enter the password", the mailbox paused), the SMTP login
// (no message is sent), the IMAP login and its Sent and Junk folders, and the DNS of the
// mailbox's domain with its own selector (recorded, so a pass makes it ready today). The
// fix is the first failing step's mailbox.FixFor.
func runChecks(ctx context.Context, lq leadstore.Queries, key secretbox.PrivateKey, now time.Time) ([]CheckDone, error) {
	var done []CheckDone
	for {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		cs, err := lq.RequestedChecks(ctx)
		if err != nil {
			return done, err
		}
		if len(cs) == 0 {
			return done, nil
		}
		ms, err := lq.Mailboxes(ctx, now)
		if err != nil {
			return done, err
		}
		byID := make(map[int64]leadstore.MailboxStatus, len(ms))
		for _, m := range ms {
			byID[m.ID] = m
		}
		for _, c := range cs {
			r, err := checkMailbox(ctx, lq, key, byID[c.MailboxID], now)
			if err != nil {
				return done, err
			}
			if err := lq.FinishCheck(ctx, c.ID, r, now); err != nil {
				if errors.Is(err, leadstore.ErrCheckNotRequested) {
					continue
				}
				return done, err
			}
			done = append(done, CheckDone{MailboxID: c.MailboxID, CheckID: c.ID, Outcome: r.Outcome, Fix: r.Fix})
		}
	}
}

// checkMailbox runs one check of st; an error is the database's, never a step's.
func checkMailbox(ctx context.Context, lq leadstore.Queries, key secretbox.PrivateKey, st leadstore.MailboxStatus, now time.Time) (leadstore.MailboxCheckResult, error) {
	if st.ID == 0 || st.State == leadstore.MailboxRetired {
		return leadstore.MailboxCheckResult{Outcome: leadstore.CheckDidNotRun, Fix: "This mailbox is retired."}, nil
	}
	creds, err := mailboxCreds(ctx, lq, key, st.Mailbox)
	if errors.Is(err, errMailboxCreds) {
		return leadstore.MailboxCheckResult{Outcome: leadstore.CheckDidNotRun, Fix: credsFix(err)}, nil
	}
	if err != nil {
		return leadstore.MailboxCheckResult{}, err
	}
	user := config.Secret(creds.Login)
	smtpErr := leadsMail.probeSMTP(ctx, st.SMTPAddr, user, creds.Password)
	_, imapErr := leadsMail.probeIMAP(ctx, st.IMAPAddr, user, creds.Password)
	res, err := checkSenderFor(ctx, lq, st.Host(), st.DKIMSelector, now)
	if err != nil {
		return leadstore.MailboxCheckResult{}, err
	}
	r := leadstore.MailboxCheckResult{SMTPOK: ptrTo(smtpErr == nil), SPF: ptrTo(res.SPF), DKIM: ptrTo(res.DKIM), DMARC: ptrTo(res.DMARC)}
	imapOK := imapErr == nil || errors.Is(imapErr, mailbox.ErrFoldersMissing)
	r.IMAPOK = ptrTo(imapOK)
	if imapOK {
		r.FoldersOK = ptrTo(imapErr == nil)
	}
	dnsErr := res.Err(st.Host(), st.DKIMSelector)
	switch {
	case smtpErr != nil:
		r.Fix = mailbox.FixFor(mailbox.StepSMTP, smtpErr, serverHost(st.SMTPAddr))
	case imapErr != nil:
		r.Fix = mailbox.FixFor(mailbox.StepIMAP, imapErr, serverHost(st.IMAPAddr))
	case dnsErr != nil:
		r.Fix = mailbox.FixFor(mailbox.StepDNS, dnsErr, st.Host())
	}
	r.Outcome = leadstore.CheckPassed
	if r.Fix != "" {
		r.Outcome = leadstore.CheckFailed
	}
	return r, nil
}

func ptrTo(b bool) *bool { return &b }

// serverHost is the host of a host:port.
func serverHost(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// credsFix turns the reason mailboxCreds paused a mailbox for into a sentence for the
// check's fix: "Re-enter the password: it was saved under a different key."
func credsFix(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	i := strings.Index(s, "re-enter the password")
	if i < 0 {
		return "Re-enter the password."
	}
	return "R" + strings.TrimSuffix(s[i+1:], ".") + "."
}

// runChecksCommand is the mailbox-check job (`leads mailboxes check --requested`): it loads
// the private key (the systemd credential, or LEADHOUND_MAILBOX_KEY_FILE locally), runs
// every requested check and prints one line per check.
func runChecksCommand(ctx context.Context, getenv func(string) string) error {
	return checksCommand(ctx, getenv, 0)
}

// checksCommand is runChecksCommand; with id > 0 it first requests a check of mailbox id
// (`leads mailboxes check ID`). The key is loaded before anything is written.
func checksCommand(ctx context.Context, getenv func(string) string, id int64) error {
	key, err := secretbox.LoadPrivateKey(getenv)
	if err != nil {
		return usageError("%v", err)
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	now := leadsNow()
	if id > 0 {
		st, err := mailboxStatusByID(ctx, lq, id, now)
		if err != nil {
			return mailboxCLIError(err)
		}
		if st.State == leadstore.MailboxRetired {
			return mailboxCLIError(refused("Mailbox %d is retired; it is not checked.", id))
		}
		if _, err := lq.RequestCheck(ctx, id, leadstore.ActorOperator); err != nil {
			return err
		}
	}
	done, err := runChecks(ctx, lq, key, now)
	for _, d := range done {
		switch d.Outcome {
		case leadstore.CheckPassed:
			_, _ = fmt.Fprintf(stdout, "Mailbox %d: check passed.\n", d.MailboxID)
		case leadstore.CheckFailed:
			_, _ = fmt.Fprintf(stdout, "Mailbox %d: check failed: %s\n", d.MailboxID, d.Fix)
		default:
			_, _ = fmt.Fprintf(stdout, "Mailbox %d: check did not run: %s\n", d.MailboxID, d.Fix)
		}
	}
	if err != nil {
		return err
	}
	if len(done) == 0 {
		_, _ = fmt.Fprintln(stdout, "No mailbox check is waiting.")
	}
	return nil
}
