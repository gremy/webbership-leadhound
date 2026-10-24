// Package mailbox builds and delivers outreach email: one plain-text RFC 5322 message per
// draft, written to a local outbox in shadow and sent over SMTP when live.
package mailbox

import (
	"bytes"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrInvalid wraps every reason Build refuses a message.
var ErrInvalid = errors.New("mailbox: invalid message")

// Message is one outreach email. Message-IDs are written without the angle brackets the
// headers add.
type Message struct {
	FromName string // the sender's display name
	FromAddr string // the outreach mailbox
	To       string // the recipient address
	Subject  string
	Body     string // plain text; any line ending
	// MessageID is this message's id; InReplyTo and References thread a follow-up to the
	// first email (both empty for the first email).
	MessageID  string
	InReplyTo  string
	References []string
	// ListUnsubscribe is the address an opt-out goes to, written as
	// List-Unsubscribe: <mailto:<address>?subject=unsubscribe>.
	ListUnsubscribe string
	Date            time.Time
}

// Build writes m as a complete message: From, To, Subject (Q-encoded when it is not ASCII),
// Date, Message-ID, In-Reply-To and References (a follow-up only), List-Unsubscribe,
// MIME-Version, a text/plain utf-8 body in quoted-printable, every line ended by CRLF. It
// adds nothing else: no HTML, no images, no links of its own, no tracking of any kind.
func Build(m Message) ([]byte, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	h := func(name, value string) { b.WriteString(name + ": " + value + "\r\n") }
	h("From", (&mail.Address{Name: m.FromName, Address: m.FromAddr}).String())
	h("To", (&mail.Address{Address: m.To}).String())
	h("Subject", encodeSubject(m.Subject))
	h("Date", m.Date.Format(time.RFC1123Z))
	h("Message-ID", "<"+m.MessageID+">")
	if m.InReplyTo != "" {
		h("In-Reply-To", "<"+m.InReplyTo+">")
	}
	if len(m.References) > 0 {
		refs := make([]string, len(m.References))
		for i, r := range m.References {
			refs[i] = "<" + r + ">"
		}
		h("References", strings.Join(refs, " "))
	}
	h("List-Unsubscribe", "<mailto:"+m.ListUnsubscribe+"?subject=unsubscribe>")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")

	body := strings.ReplaceAll(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\r", "\n")
	body = strings.TrimRight(body, "\n") + "\n"
	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(body)); err != nil {
		return nil, fmt.Errorf("mailbox: build: %w", err)
	}
	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("mailbox: build: %w", err)
	}
	return b.Bytes(), nil
}

// encodeSubject Q-encodes a subject that is not plain ASCII.
func encodeSubject(s string) string {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

// check refuses a message whose headers would be wrong or could be split.
func (m Message) check() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
	}
	for _, f := range []struct{ name, v string }{
		{"from name", m.FromName}, {"from", m.FromAddr}, {"to", m.To}, {"subject", m.Subject},
		{"message id", m.MessageID}, {"in-reply-to", m.InReplyTo}, {"list-unsubscribe", m.ListUnsubscribe},
	} {
		if strings.ContainsAny(f.v, "\r\n") {
			return bad("the %s spans more than one line", f.name)
		}
	}
	for _, r := range m.References {
		if strings.ContainsAny(r, "\r\n") {
			return bad("a reference spans more than one line")
		}
	}
	for _, f := range []struct{ name, v string }{
		{"from", m.FromAddr}, {"to", m.To}, {"list-unsubscribe", m.ListUnsubscribe},
	} {
		a, err := mail.ParseAddress(f.v)
		if err != nil || a.Address != f.v || a.Name != "" {
			// The address is private: the error names the field only.
			return bad("the %s address is not a bare address", f.name)
		}
	}
	if strings.TrimSpace(m.Subject) == "" {
		return bad("the subject is empty")
	}
	if strings.TrimSpace(m.Body) == "" {
		return bad("the body is empty")
	}
	if m.Date.IsZero() {
		return bad("the date is not set")
	}
	ids := append([]string{m.MessageID}, m.References...)
	if m.InReplyTo != "" {
		ids = append(ids, m.InReplyTo)
	}
	for i, id := range ids {
		if i == 0 && id == "" {
			return bad("the message id is empty")
		}
		if !msgIDOK(id) {
			return bad("%q is not a message id (local@domain, no brackets)", id)
		}
	}
	if (m.InReplyTo == "") != (len(m.References) == 0) {
		return bad("a follow-up needs both In-Reply-To and References")
	}
	return nil
}

func msgIDOK(id string) bool {
	local, dom, ok := strings.Cut(id, "@")
	return ok && local != "" && dom != "" && !strings.ContainsAny(id, "<> \t\r\n\"\\") && !strings.Contains(dom, "@")
}
