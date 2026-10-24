package mailbox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"regexp"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/gremy/webbership-leadhound/internal/config"
)

const (
	smtpTimeout    = 30 * time.Second // the whole exchange, dial to final reply
	maxSMTPErrText = 300              // runes of a server reply kept in an error
)

// ErrSMTPConfig: the sender cannot be used as configured (a port other than 465 or 587,
// or no credentials). Nothing was sent.
var ErrSMTPConfig = errors.New("mailbox: smtp sender misconfigured")

// SMTPSender submits one message per Send over an authenticated, certificate-verified
// connection: implicit TLS when Addr's port is 465, STARTTLS when it is 587. User and
// Password stay redacted however a sender is printed or logged.
type SMTPSender struct {
	Addr           string // host:port
	User, Password config.Secret
	// RootCAs verifies the server's certificate; nil uses the system roots.
	RootCAs *x509.CertPool

	// dial opens the TCP connection; nil uses a net.Dialer. Tests point it at a fake server.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Kind says what a failed Send means for the message.
type Kind int

const (
	// Retryable: a temporary failure before DATA; nothing was sent.
	Retryable Kind = iota + 1
	// Refused: a permanent refusal before DATA; nothing was sent.
	Refused
	// Uncertain: the failure came during or after DATA; the message may have been delivered.
	Uncertain
	// Account: the mailbox itself was refused (any reply code at AUTH but 421, or a
	// 5xx at MAIL FROM, such as a quota or policy block); nothing was sent, and no other
	// message will go until it is fixed.
	Account
)

func (k Kind) String() string {
	switch k {
	case Retryable:
		return "retryable"
	case Refused:
		return "refused"
	case Uncertain:
		return "uncertain"
	case Account:
		return "account"
	}
	return "unknown"
}

// SendError is every failure of a Send past its configuration check. Text has the
// password, the user, the sender and the recipient removed.
type SendError struct {
	Kind  Kind
	Stage string // connect, auth, mail, rcpt or data
	Code  int    // the SMTP reply code; 0 when the server gave none
	// Bounce: the address itself was refused, so it is to be suppressed: a 550, 551 or 553
	// at RCPT whose enhanced status code is 5.1.x (bad address, RFC 3463). Any other 5xx at
	// RCPT refuses this message only.
	Bounce bool
	Text   string
	cause  error // the context's error when the deadline or a cancel ended the exchange
}

func (e *SendError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("smtp %s: %s: %d %s", e.Kind, e.Stage, e.Code, e.Text)
	}
	return fmt.Sprintf("smtp %s: %s: %s", e.Kind, e.Stage, e.Text)
}

// Unwrap returns context.DeadlineExceeded or context.Canceled when one ended the exchange.
func (e *SendError) Unwrap() error { return e.cause }

// Send submits raw (a complete message, as Build writes it) from the envelope sender from
// to the one recipient to, and returns the server's reply to the final dot. The whole
// exchange ends within 30 s, sooner if ctx ends first.
func (s *SMTPSender) Send(ctx context.Context, from, to string, raw []byte) (reply string, err error) {
	host, port, splitErr := net.SplitHostPort(s.Addr)
	if splitErr != nil || host == "" {
		return "", fmt.Errorf("%w: address is not host:port", ErrSMTPConfig)
	}
	if port != "465" && port != "587" {
		return "", fmt.Errorf("%w: port %s (465 or 587 only)", ErrSMTPConfig, port)
	}
	user, pass := s.User.Reveal(), s.Password.Reveal()
	if user == "" || pass == "" {
		return "", fmt.Errorf("%w: no credentials", ErrSMTPConfig)
	}

	ctx, cancel := context.WithTimeout(ctx, smtpTimeout)
	defer cancel()

	plain := base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + pass))
	hidden := []string{pass, user, to, from, plain,
		base64.StdEncoding.EncodeToString([]byte(user)), base64.StdEncoding.EncodeToString([]byte(pass))}
	stage := "connect"
	fail := func(err error) error { return classify(ctx, stage, err, hidden) }

	dial := s.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", s.Addr)
	if err != nil {
		return "", fail(err)
	}
	// Closing the connection is what ends a blocked read or write at the deadline.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() { _ = conn.Close() }()

	tlsConf := &tls.Config{ServerName: host, RootCAs: s.RootCAs, MinVersion: tls.VersionTLS12}
	var c *smtp.Client
	if port == "465" {
		tc := tls.Client(conn, tlsConf)
		if err := tc.HandshakeContext(ctx); err != nil {
			return "", fail(err)
		}
		c = smtp.NewClient(tc)
	} else {
		if c, err = smtp.NewClientStartTLS(conn, tlsConf); err != nil {
			return "", fail(err)
		}
	}
	defer func() { _ = c.Close() }()
	c.CommandTimeout, c.SubmissionTimeout = smtpTimeout, smtpTimeout

	stage = "auth"
	if err := c.Auth(sasl.NewPlainClient("", user, pass)); err != nil {
		return "", fail(err)
	}
	stage = "mail"
	if err := c.Mail(from, nil); err != nil {
		return "", fail(err)
	}
	stage = "rcpt"
	if err := c.Rcpt(to, nil); err != nil {
		return "", fail(err)
	}
	stage = "data"
	w, err := c.Data()
	if err != nil {
		return "", fail(err)
	}
	if _, err := w.Write(raw); err != nil {
		return "", fail(err)
	}
	resp, err := w.CloseWithResponse()
	if err != nil {
		return "", fail(err)
	}
	_ = c.Quit()
	return scrub(resp.StatusText, hidden...), nil
}

// classify turns a failure at stage into a *SendError. Once DATA has been sent every
// failure is Uncertain. Before it: any reply code at AUTH but 421 (the server closing, which
// says nothing of the account), or a 5xx at MAIL FROM, is Account; a 5xx at RCPT is Refused (Bounce only for 550/551/553 with enhanced code 5.1.x);
// anything else (a 4xx, a dropped connection) is Retryable.
func classify(ctx context.Context, stage string, err error, hidden []string) *SendError {
	e := &SendError{Stage: stage}
	var se *smtp.SMTPError
	switch {
	case ctx.Err() != nil:
		e.cause = ctx.Err()
		e.Text = ctx.Err().Error()
	case errors.As(err, &se):
		e.Code = se.Code
		e.Text = se.Message
	default:
		e.Text = err.Error()
	}
	switch {
	case stage == "data":
		e.Kind = Uncertain
	case stage == "auth" && e.Code != 0 && e.Code != 421, stage == "mail" && e.Code/100 == 5:
		e.Kind = Account
	case e.Code/100 == 5:
		e.Kind = Refused
		e.Bounce = stage == "rcpt" && (e.Code == 550 || e.Code == 551 || e.Code == 553) &&
			se != nil && se.EnhancedCode[0] == 5 && se.EnhancedCode[1] == 1
	default:
		e.Kind = Retryable
	}
	// Before cutting, so no part of a value survives the cut.
	e.Text = scrub(e.Text, hidden...)
	if rs := []rune(e.Text); len(rs) > maxSMTPErrText {
		e.Text = string(rs[:maxSMTPErrText])
	}
	return e
}

// scrub replaces each non-empty value in s, in any letter case, with "[redacted]".
func scrub(s string, values ...string) string {
	for _, v := range values {
		if v != "" {
			s = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(v)).ReplaceAllLiteralString(s, config.Secret("").String())
		}
	}
	return s
}
