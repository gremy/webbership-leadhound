package mailbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The steps of a mailbox check, as FixFor names them.
const (
	StepSMTP = "smtp" // the sending login
	StepIMAP = "imap" // the reading login and its folders
	StepDNS  = "dns"  // the sending domain's records
)

// probeTimeout bounds a whole IMAP probe, dial to logout.
const probeTimeout = 30 * time.Second

// ErrFoldersMissing: the IMAP login worked but the server names no \Sent or no \Junk
// special-use folder (RFC 6154).
var ErrFoldersMissing = errors.New("mailbox: no \\Sent or \\Junk folder")

// Probe tests the sending login without sending: it dials (the same TLS rules as Send),
// says EHLO, authenticates and quits. It never says MAIL FROM. A refused login is an
// Account *SendError; a network or certificate failure is Retryable.
func (s *SMTPSender) Probe(ctx context.Context) error {
	host, port, user, pass, err := s.settings()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, smtpTimeout)
	defer cancel()
	c, done, err := s.login(ctx, host, port, user, pass, credentialForms(user, pass))
	if err != nil {
		return err
	}
	defer done()
	// The login is proven; a failed QUIT says nothing more about the mailbox.
	_ = c.Quit()
	return nil
}

// ProbeIMAP tests the reading login: it dials and logs in, lists the folders and logs out.
// The folders are returned even when the error is ErrFoldersMissing.
func ProbeIMAP(ctx context.Context, r *IMAPReader) (Mailboxes, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := r.Dial(ctx); err != nil {
		return Mailboxes{}, err
	}
	boxes, err := r.ListMailboxes()
	// The login and the listing are what is being tested; a failed logout is not.
	_ = r.Close()
	if err != nil {
		return Mailboxes{}, err
	}
	if boxes.Sent == "" || boxes.Junk == "" {
		return boxes, ErrFoldersMissing
	}
	return boxes, nil
}

// IsPolicyBlock: err is a refusal at RCPT with an enhanced status code 5.7.x (a policy
// block of the sender, not a bad address), which pauses every mailbox on its domain.
func IsPolicyBlock(err error) bool {
	var se *SendError
	return errors.As(err, &se) && se.Kind == Refused && se.Stage == "rcpt" &&
		se.Enhanced[0] == 5 && se.Enhanced[1] == 7
}

// DNSError is a DNS check with a record missing; FixFor names each one.
type DNSError struct {
	Result           DNSResult
	Domain, Selector string
}

func (e *DNSError) Error() string {
	var missing []string
	for _, r := range []struct {
		ok   bool
		name string
	}{{e.Result.SPF, "spf"}, {e.Result.DKIM, "dkim"}, {e.Result.DMARC, "dmarc"}} {
		if !r.ok {
			missing = append(missing, r.name)
		}
	}
	return "mailbox: dns records missing: " + strings.Join(missing, ", ")
}

// Err is nil when all three records are in place, else a *DNSError for domain and
// selector (the values CheckDNS was given).
func (r DNSResult) Err(domain, selector string) error {
	if r.OK() {
		return nil
	}
	return &DNSError{Result: r, Domain: domain, Selector: selector}
}

// FixFor says in plain words what the operator can do about a failed check step. It is
// built only from fixed sentences, the reply code and host (or, for DNS, the domain and
// selector the caller gave), never from a server's reply text, so it carries no
// credential. host comes from the mailbox's settings. It is "" when err is nil.
func FixFor(step string, err error, host string) string {
	if err == nil {
		return ""
	}
	const (
		login = "The server refused the login. Check the password (some providers need an app password)."
		cert  = "The server's certificate did not check out for %s."
		reach = "Could not reach %s. Check the server name and port."
	)
	var (
		se *SendError
		ie *IMAPError
		de *DNSError
	)
	switch {
	case errors.Is(err, ErrFoldersMissing):
		return "Signed in, but the Sent or Junk folder was not found; replies in Junk would be missed."
	case errors.As(err, &de):
		return dnsFix(de)
	case errors.Is(err, ErrSMTPConfig):
		return fmt.Sprintf("The sending settings for %s are incomplete: use port 465 or 587, with a user and a password.", host)
	case errors.Is(err, ErrIMAPConfig):
		return fmt.Sprintf("The reading settings for %s are incomplete: use port 993, with a user and a password.", host)
	case errors.As(err, &se):
		switch {
		case se.cert:
			return fmt.Sprintf(cert, host)
		case se.Kind == Account && se.Stage == "auth":
			return login
		case se.Kind == Account:
			return fmt.Sprintf("The server refused this mailbox as a sender (reply %d). Check the account with the provider; it may be over quota or blocked.", se.Code)
		case se.Code == 0:
			return fmt.Sprintf(reach, host)
		case se.Code/100 == 4:
			return fmt.Sprintf("The server at %s asked to try again later (reply %d).", host, se.Code)
		}
		return fmt.Sprintf("The server at %s refused the check (reply %d).", host, se.Code)
	case errors.As(err, &ie):
		switch ie.Stage {
		case "connect":
			// The text of a connect failure is Go's own (the dial or the TLS handshake),
			// read here only to tell the two apart; none of it reaches the fix.
			if ie.cause == nil && (strings.Contains(ie.Text, "x509:") || strings.Contains(ie.Text, "tls: failed to verify")) {
				return fmt.Sprintf(cert, host)
			}
			return fmt.Sprintf(reach, host)
		case "login":
			return login
		case "list":
			return fmt.Sprintf("Signed in to %s, but listing the folders failed. Try the check again.", host)
		}
	}
	return fmt.Sprintf("The %s check for %s failed. Try it again; if it keeps failing, check the settings.", stepName(step), host)
}

func stepName(step string) string {
	switch step {
	case StepSMTP:
		return "sending"
	case StepIMAP:
		return "reading"
	case StepDNS:
		return "domain"
	}
	return "mailbox"
}

// dnsFix names each missing record where it belongs, one sentence each.
func dnsFix(e *DNSError) string {
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(e.Domain)), ".")
	selector := strings.TrimSpace(e.Selector)
	if domain == "" {
		return "No sending domain is set for this mailbox."
	}
	var out []string
	if !e.Result.SPF {
		out = append(out, fmt.Sprintf("Add an SPF record: a TXT record at %s starting v=spf1 that names the mail provider.", domain))
	}
	if !e.Result.DKIM {
		if selector == "" || strings.ContainsAny(selector, ". \t") {
			out = append(out, "Set the DKIM selector to the single name the mail provider gives (the part before ._domainkey).")
		} else {
			out = append(out, fmt.Sprintf("Add the DKIM record: a TXT record at %s._domainkey.%s starting v=DKIM1, as the mail provider gives it.", selector, domain))
		}
	}
	if !e.Result.DMARC {
		out = append(out, fmt.Sprintf("Add a DMARC record: a TXT record at _dmarc.%s starting v=DMARC1.", domain))
	}
	return strings.Join(out, " ")
}
