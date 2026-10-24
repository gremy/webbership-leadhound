package mailbox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// FirstTextBytes is how much of a message's text is peeked for classification.
const FirstTextBytes = 600

// maxHeaderBytes caps the top-level header peeked per message.
const maxHeaderBytes = 64 << 10

var (
	// ErrIMAPConfig: the reader cannot be used as configured (a port other than 993, or
	// no credentials). Nothing was sent to a server.
	ErrIMAPConfig = errors.New("mailbox: imap reader misconfigured")
	// ErrNotConnected: a Reader method was called before Dial or after Close.
	ErrNotConnected = errors.New("mailbox: imap reader not connected")
	// ErrNoSent: the server names no \Sent mailbox, so AppendSent has nowhere to write.
	ErrNoSent = errors.New("mailbox: no \\Sent mailbox")
)

// Cursor is how far one mailbox has been read: its UIDVALIDITY and the last UID seen.
type Cursor struct {
	UIDValidity uint32
	LastUID     uint32
}

// NeedsRescan says whether a mailbox must be read again by date rather than from the
// cursor's last UID: there is no cursor yet, or the server's UIDVALIDITY is not the one
// the cursor was taken under (UIDs from before mean nothing now), or the server gave none.
func NeedsRescan(c Cursor, uidValidity uint32) bool {
	return c.UIDValidity == 0 || uidValidity == 0 || c.UIDValidity != uidValidity
}

// Mailboxes are the mailboxes sync reads and writes. Sent and Junk come from the
// special-use attributes (RFC 6154) and are "" when the server names none.
type Mailboxes struct {
	Inbox, Sent, Junk string
}

// Fetched is one message as peeked (nothing is marked seen): its top-level header, at most
// the first 600 bytes of its text and, for a multipart/report, its report parts (RA4).
// The caller classifies it and drops it.
type Fetched struct {
	UID       uint32
	Header    []byte
	FirstText []byte
	Report    [][]byte
}

// Reader is the mailbox as sync sees it. Select picks the mailbox the Search and Fetch
// calls read.
type Reader interface {
	ListMailboxes() (Mailboxes, error)
	// Select opens a mailbox read-only and returns its UIDVALIDITY.
	Select(name string) (uidValidity uint32, err error)
	// SearchSince returns the UIDs above afterUID, in ascending order.
	SearchSince(afterUID uint32) ([]uint32, error)
	// SearchSinceDate returns the UIDs of messages received on or after since's date
	// (the rescan after a UIDVALIDITY change).
	SearchSinceDate(since time.Time) ([]uint32, error)
	FetchHeaders(uids []uint32) ([]Fetched, error)
	// AppendSent files a copy of a sent message, marked seen, in the \Sent mailbox.
	AppendSent(raw []byte) error
	Close() error
}

// IMAPReader is a Reader over one authenticated, certificate-verified IMAP session with
// implicit TLS (port 993). User and Password stay redacted however a reader is printed,
// and every error has them removed.
type IMAPReader struct {
	Addr           string // host:port
	User, Password config.Secret
	// RootCAs verifies the server's certificate; nil uses the system roots.
	RootCAs *x509.CertPool

	// dial opens the TCP connection; nil uses a net.Dialer. Tests point it at a fake.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)

	c      *imapclient.Client
	stop   func() bool
	boxes  Mailboxes
	listed bool
}

// IMAPError is every failure past the reader's configuration check. Text has the user and
// the password removed.
type IMAPError struct {
	Stage string // connect, login, list, select, search, fetch, append or logout
	Text  string
	cause error // the context's error when it ended the session
}

func (e *IMAPError) Error() string { return "mailbox: imap " + e.Stage + ": " + e.Text }

// Unwrap returns context.DeadlineExceeded or context.Canceled when one ended the session.
func (e *IMAPError) Unwrap() error { return e.cause }

// Dial connects, verifies the certificate and logs in. The session lasts until Close, or
// until ctx ends, which closes the connection.
func (r *IMAPReader) Dial(ctx context.Context) error {
	host, port, err := net.SplitHostPort(r.Addr)
	if err != nil || host == "" {
		return fmt.Errorf("%w: address is not host:port", ErrIMAPConfig)
	}
	if port != "993" {
		return fmt.Errorf("%w: port %s (993 only)", ErrIMAPConfig, port)
	}
	user, pass := r.User.Reveal(), r.Password.Reveal()
	if user == "" || pass == "" {
		return fmt.Errorf("%w: no credentials", ErrIMAPConfig)
	}
	if r.c != nil {
		return nil
	}
	fail := func(stage string, err error) error {
		e := &IMAPError{Stage: stage}
		if ctx.Err() != nil {
			e.cause = ctx.Err()
			e.Text = ctx.Err().Error()
		} else {
			e.Text = scrub(err.Error(), r.hidden()...)
		}
		return e
	}
	if ctx.Err() != nil {
		return fail("connect", ctx.Err())
	}

	dial := r.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	}
	conn, err := dial(ctx, "tcp", r.Addr)
	if err != nil {
		return fail("connect", err)
	}
	tc := tls.Client(conn, &tls.Config{ServerName: host, RootCAs: r.RootCAs, MinVersion: tls.VersionTLS12,
		NextProtos: []string{"imap"}})
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return fail("connect", err)
	}
	c := imapclient.New(tc, &imapclient.Options{})
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	if err := c.WaitGreeting(); err != nil {
		stop()
		_ = c.Close()
		return fail("connect", err)
	}
	if err := c.Login(user, pass).Wait(); err != nil {
		stop()
		_ = c.Close()
		return fail("login", err)
	}
	r.c, r.stop = c, stop
	return nil
}

// hidden is every form the credentials can take in a server's reply: as given and as an
// IMAP quoted string.
func (r *IMAPReader) hidden() []string {
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	user, pass := r.User.Reveal(), r.Password.Reveal()
	return []string{pass, q.Replace(pass), user, q.Replace(user)}
}

func (r *IMAPReader) fail(stage string, err error) error {
	return &IMAPError{Stage: stage, Text: scrub(err.Error(), r.hidden()...)}
}

func (r *IMAPReader) client() (*imapclient.Client, error) {
	if r.c == nil {
		return nil, ErrNotConnected
	}
	return r.c, nil
}

// ListMailboxes finds INBOX and the \Sent and \Junk special-use mailboxes.
func (r *IMAPReader) ListMailboxes() (Mailboxes, error) {
	c, err := r.client()
	if err != nil {
		return Mailboxes{}, err
	}
	list, err := c.List("", "*", nil).Collect()
	if err != nil {
		return Mailboxes{}, r.fail("list", err)
	}
	r.boxes, r.listed = specialMailboxes(list), true
	return r.boxes, nil
}

// specialMailboxes picks the first selectable mailbox with each special-use attribute.
func specialMailboxes(list []*imap.ListData) Mailboxes {
	m := Mailboxes{Inbox: "INBOX"}
	for _, d := range list {
		var sent, junk, noSelect bool
		for _, a := range d.Attrs {
			switch {
			case strings.EqualFold(string(a), string(imap.MailboxAttrSent)):
				sent = true
			case strings.EqualFold(string(a), string(imap.MailboxAttrJunk)):
				junk = true
			case strings.EqualFold(string(a), string(imap.MailboxAttrNoSelect)),
				strings.EqualFold(string(a), string(imap.MailboxAttrNonExistent)):
				noSelect = true
			}
		}
		if noSelect {
			continue
		}
		if sent && m.Sent == "" {
			m.Sent = d.Mailbox
		}
		if junk && m.Junk == "" {
			m.Junk = d.Mailbox
		}
	}
	return m
}

// Select opens name read-only (EXAMINE), so nothing in it can change.
func (r *IMAPReader) Select(name string) (uint32, error) {
	c, err := r.client()
	if err != nil {
		return 0, err
	}
	data, err := c.Select(name, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return 0, r.fail("select", err)
	}
	return data.UIDValidity, nil
}

// SearchSince returns the UIDs above afterUID.
func (r *IMAPReader) SearchSince(afterUID uint32) ([]uint32, error) {
	c, err := r.client()
	if err != nil {
		return nil, err
	}
	set := imap.UIDSet{imap.UIDRange{Start: imap.UID(afterUID + 1), Stop: 0}} // afterUID+1:*
	data, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{set}}, nil).Wait()
	if err != nil {
		return nil, r.fail("search", err)
	}
	return uidsAfter(data.AllUIDs(), afterUID), nil
}

// SearchSinceDate returns the UIDs of messages received on or after since's date.
func (r *IMAPReader) SearchSinceDate(since time.Time) ([]uint32, error) {
	c, err := r.client()
	if err != nil {
		return nil, err
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{Since: since}, nil).Wait()
	if err != nil {
		return nil, r.fail("search", err)
	}
	return uidsAfter(data.AllUIDs(), 0), nil
}

// uidsAfter keeps the UIDs above after: "n:*" always includes the last message, even when
// its UID is below n.
func uidsAfter(uids []imap.UID, after uint32) []uint32 {
	out := make([]uint32, 0, len(uids))
	for _, u := range uids {
		if uint32(u) > after {
			out = append(out, uint32(u))
		}
	}
	return out
}

var (
	headerSection = &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true,
		Partial: &imap.SectionPartial{Offset: 0, Size: maxHeaderBytes}}
	textSection = &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText, Peek: true,
		Partial: &imap.SectionPartial{Offset: 0, Size: FirstTextBytes}}
)

// FetchHeaders peeks each message's header and first text; for a multipart/report it then
// reads the body structure's delivery-status and headers parts (RA4), each at most
// MaxReportPart bytes. Nothing is marked seen.
func (r *IMAPReader) FetchHeaders(uids []uint32) ([]Fetched, error) {
	c, err := r.client()
	if err != nil {
		return nil, err
	}
	if len(uids) == 0 {
		return nil, nil
	}
	set := make([]imap.UID, len(uids))
	for i, u := range uids {
		set[i] = imap.UID(u)
	}
	bufs, err := c.Fetch(imap.UIDSetNum(set...), &imap.FetchOptions{
		UID:           true,
		BodyStructure: &imap.FetchItemBodyStructure{},
		BodySection:   []*imap.FetchItemBodySection{headerSection, textSection},
	}).Collect()
	if err != nil {
		return nil, r.fail("fetch", err)
	}
	out := make([]Fetched, 0, len(bufs))
	for _, b := range bufs {
		f := Fetched{
			UID:       uint32(b.UID),
			Header:    cut(b.FindBodySection(headerSection), maxHeaderBytes),
			FirstText: cut(b.FindBodySection(textSection), FirstTextBytes),
		}
		if isReport(b.BodyStructure) {
			if f.Report, err = r.fetchReport(c, b.UID, reportSections(b.BodyStructure)); err != nil {
				return nil, err
			}
		}
		out = append(out, f)
	}
	return out, nil
}

func (r *IMAPReader) fetchReport(c *imapclient.Client, uid imap.UID, parts []reportPart) ([][]byte, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	secs := make([]*imap.FetchItemBodySection, len(parts))
	for i, p := range parts {
		size := int64(MaxReportPart)
		if p.encoding != "" {
			// Encoded bytes run longer than the decoded part they hold.
			size = 2 * MaxReportPart
		}
		secs[i] = &imap.FetchItemBodySection{Part: p.path, Peek: true,
			Partial: &imap.SectionPartial{Offset: 0, Size: size}}
		if p.message {
			// A message/rfc822 part: its header only.
			secs[i].Specifier = imap.PartSpecifierHeader
		}
	}
	bufs, err := c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true, BodySection: secs}).Collect()
	if err != nil {
		return nil, r.fail("fetch", err)
	}
	var out [][]byte
	for _, b := range bufs {
		for i, s := range secs {
			if data := b.FindBodySection(s); len(data) > 0 {
				out = append(out, decodePart(data, parts[i].encoding))
			}
		}
	}
	return out, nil
}

func cut(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func isReport(bs imap.BodyStructure) bool {
	return bs != nil && bs.MediaType() == "multipart/report"
}

type reportPart struct {
	path     []int
	message  bool   // message/rfc822 or message/global: fetch its header
	encoding string // "base64" or "quoted-printable" when the part's body is encoded
}

// reportSections lists the parts of a report that carry the delivery status and the
// original message's header.
func reportSections(bs imap.BodyStructure) []reportPart {
	if !isReport(bs) {
		return nil
	}
	var out []reportPart
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		switch part.MediaType() {
		case "message/delivery-status", "message/global-delivery-status",
			"text/rfc822-headers", "message/global-headers":
			out = append(out, reportPart{path: append([]int(nil), path...), encoding: partEncoding(part)})
		case "message/rfc822", "message/global":
			out = append(out, reportPart{path: append([]int(nil), path...), message: true})
		}
		return true
	})
	return out
}

// partEncoding is a single part's Content-Transfer-Encoding when it is one decodePart
// undoes, else "".
func partEncoding(part imap.BodyStructure) string {
	sp, ok := part.(*imap.BodyStructureSinglePart)
	if !ok {
		return ""
	}
	switch enc := strings.ToLower(strings.TrimSpace(sp.Encoding)); enc {
	case "base64", "quoted-printable":
		return enc
	}
	return ""
}

// AppendSent files raw, marked seen, in the \Sent mailbox; ErrNoSent when there is none.
func (r *IMAPReader) AppendSent(raw []byte) error {
	c, err := r.client()
	if err != nil {
		return err
	}
	if !r.listed {
		if _, err := r.ListMailboxes(); err != nil {
			return err
		}
	}
	if r.boxes.Sent == "" {
		return ErrNoSent
	}
	cmd := c.Append(r.boxes.Sent, int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}})
	if _, err := cmd.Write(raw); err != nil {
		_ = cmd.Close()
		return r.fail("append", err)
	}
	if err := cmd.Close(); err != nil {
		return r.fail("append", err)
	}
	if _, err := cmd.Wait(); err != nil {
		return r.fail("append", err)
	}
	return nil
}

// Close logs out and closes the connection.
func (r *IMAPReader) Close() error {
	if r.c == nil {
		return nil
	}
	c := r.c
	r.c, r.listed = nil, false
	defer r.stop()
	logoutErr := c.Logout().Wait()
	closeErr := c.Close()
	if logoutErr != nil {
		return r.fail("logout", logoutErr)
	}
	if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return r.fail("logout", closeErr)
	}
	return nil
}
