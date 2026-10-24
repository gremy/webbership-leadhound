package mailbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// The sync logic (Task 5.3) is tested against a fake Reader; this proves the real one
// fits the same interface.
var _ Reader = (*IMAPReader)(nil)

func TestNeedsRescan(t *testing.T) {
	cases := []struct {
		name   string
		cursor Cursor
		uidv   uint32
		want   bool
	}{
		{"no cursor yet", Cursor{}, 7, true},
		{"same validity", Cursor{UIDValidity: 7, LastUID: 40}, 7, false},
		{"validity changed", Cursor{UIDValidity: 7, LastUID: 40}, 8, true},
		{"validity lowered", Cursor{UIDValidity: 9, LastUID: 40}, 8, true},
		{"server reports none", Cursor{UIDValidity: 7, LastUID: 40}, 0, true},
	}
	for _, c := range cases {
		if got := NeedsRescan(c.cursor, c.uidv); got != c.want {
			t.Errorf("%s: NeedsRescan(%+v, %d) = %v, want %v", c.name, c.cursor, c.uidv, got, c.want)
		}
	}
}

func TestSpecialMailboxes(t *testing.T) {
	got := specialMailboxes([]*imap.ListData{
		{Mailbox: "INBOX"},
		{Mailbox: "Archive", Attrs: []imap.MailboxAttr{imap.MailboxAttrArchive}},
		{Mailbox: "Spam", Attrs: []imap.MailboxAttr{"\\junk"}},
		{Mailbox: "Sent Items", Attrs: []imap.MailboxAttr{imap.MailboxAttrHasNoChildren, imap.MailboxAttrSent}},
		{Mailbox: "Sent Too", Attrs: []imap.MailboxAttr{imap.MailboxAttrSent}},
		{Mailbox: "Junk Gone", Attrs: []imap.MailboxAttr{imap.MailboxAttrJunk, imap.MailboxAttrNoSelect}},
	})
	want := Mailboxes{Inbox: "INBOX", Sent: "Sent Items", Junk: "Spam"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got := specialMailboxes(nil); got != (Mailboxes{Inbox: "INBOX"}) {
		t.Fatalf("no special-use mailboxes: %+v", got)
	}
}

func TestReportSections(t *testing.T) {
	bs := &imap.BodyStructureMultiPart{Subtype: "report", Children: []imap.BodyStructure{
		&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain"},
		&imap.BodyStructureSinglePart{Type: "message", Subtype: "delivery-status"},
		&imap.BodyStructureSinglePart{Type: "text", Subtype: "RFC822-headers"},
	}}
	got := reportSections(bs)
	if len(got) != 2 || got[0].path[0] != 2 || got[1].path[0] != 3 || got[0].message {
		t.Fatalf("got %v, want [[2] [3]]", got)
	}
	encoded := &imap.BodyStructureMultiPart{Subtype: "report", Children: []imap.BodyStructure{
		&imap.BodyStructureSinglePart{Type: "message", Subtype: "delivery-status", Encoding: "BASE64"},
		&imap.BodyStructureSinglePart{Type: "text", Subtype: "rfc822-headers", Encoding: "quoted-printable"},
		&imap.BodyStructureSinglePart{Type: "message", Subtype: "rfc822", Encoding: "7bit"},
	}}
	enc := reportSections(encoded)
	if len(enc) != 3 || enc[0].encoding != "base64" || enc[1].encoding != "quoted-printable" ||
		enc[2].encoding != "" || !enc[2].message {
		t.Fatalf("encodings: %+v", enc)
	}
	single := &imap.BodyStructureSinglePart{Type: "text", Subtype: "plain"}
	if got := reportSections(single); len(got) != 0 {
		t.Fatalf("a plain message has report sections: %v", got)
	}
	if !isReport(bs) || isReport(single) || isReport(nil) {
		t.Fatal("isReport")
	}
}

func TestUIDsAfter(t *testing.T) {
	// IMAP answers "n:*" with the last message even when its UID is below n.
	if got := uidsAfter([]imap.UID{3, 41, 42}, 40); len(got) != 2 || got[0] != 41 || got[1] != 42 {
		t.Fatalf("got %v", got)
	}
}

// fakeIMAP is a loopback TLS listener that greets, answers CAPABILITY and refuses LOGIN
// with a reply that echoes back everything the client sent, as a careless server might.
func fakeIMAP(t *testing.T) (addr string, roots *x509.CertPool) {
	t.Helper()
	cert, roots := testCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeIMAP(conn)
		}
	}()
	return ln.Addr().String(), roots
}

func serveFakeIMAP(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	w := bufio.NewWriter(conn)
	say := func(s string) { _, _ = w.WriteString(s + "\r\n"); _ = w.Flush() }
	say("* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] fake ready")
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		tag, rest, _ := strings.Cut(line, " ")
		cmd, _, _ := strings.Cut(rest, " ")
		switch strings.ToUpper(cmd) {
		case "CAPABILITY":
			say("* CAPABILITY IMAP4rev1 AUTH=PLAIN")
			say(tag + " OK done")
		case "LOGIN":
			say(tag + " NO [AUTHENTICATIONFAILED] rejected: " + rest)
		case "LOGOUT":
			say("* BYE")
			say(tag + " OK bye")
			return
		default:
			say(tag + " BAD unknown " + rest)
		}
	}
}

func testReader(addr string) *IMAPReader {
	return &IMAPReader{Addr: addr, User: config.Secret(testUser), Password: config.Secret(testPass)}
}

// redirect makes the reader dial addr whatever host its Addr names, so the certificate
// is checked against fakeHost.
func redirect(r *IMAPReader, addr string) {
	r.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
}

func TestIMAPErrorsCarryNoSecrets(t *testing.T) {
	addr, roots := fakeIMAP(t)
	secrets := []string{testPass, testUser, "outreach", "pw-Zq81"}

	var errs []error
	ctx := context.Background()

	// Configuration: a wrong port and missing credentials.
	errs = append(errs, testReader(fakeHost+":143").Dial(ctx))
	noCreds := &IMAPReader{Addr: fakeHost + ":993"}
	errs = append(errs, noCreds.Dial(ctx))

	// The server refuses LOGIN and echoes the credentials back.
	r := testReader(fakeHost + ":993")
	redirect(r, addr)
	r.RootCAs = roots
	errs = append(errs, r.Dial(ctx))

	// An untrusted certificate (the system roots do not know the fake's).
	r = testReader(fakeHost + ":993")
	redirect(r, addr)
	errs = append(errs, r.Dial(ctx))

	// Nothing listening.
	r = testReader(fakeHost + ":993")
	redirect(r, "127.0.0.1:1")
	errs = append(errs, r.Dial(ctx))

	// A cancelled context.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	r = testReader(fakeHost + ":993")
	redirect(r, addr)
	errs = append(errs, r.Dial(cctx))

	// Every method before a successful Dial.
	idle := testReader(fakeHost + ":993")
	_, err := idle.ListMailboxes()
	errs = append(errs, err)
	_, err = idle.Select("INBOX")
	errs = append(errs, err)
	_, err = idle.SearchSince(0)
	errs = append(errs, err)
	_, err = idle.FetchHeaders([]uint32{1})
	errs = append(errs, err)
	errs = append(errs, idle.AppendSent([]byte("x")))

	for i, err := range errs {
		if err == nil {
			t.Fatalf("case %d: want an error", i)
		}
		for _, v := range secrets {
			if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(v)) {
				t.Fatalf("case %d: error carries a secret: %v", i, err)
			}
		}
	}
	if !errors.Is(errs[0], ErrIMAPConfig) || !errors.Is(errs[1], ErrIMAPConfig) {
		t.Fatalf("config errors: %v / %v", errs[0], errs[1])
	}
	var ie *IMAPError
	if !errors.As(errs[2], &ie) || ie.Stage != "login" || !strings.Contains(ie.Text, "rejected") || !strings.Contains(ie.Text, config.Secret("").String()) {
		t.Fatalf("login refusal: %v", errs[2])
	}
	if !errors.As(errs[3], &ie) || ie.Stage != "connect" {
		t.Fatalf("untrusted certificate: %v", errs[3])
	}
	if !errors.Is(errs[5], context.Canceled) {
		t.Fatalf("cancelled: %v", errs[5])
	}
	if !errors.Is(errs[6], ErrNotConnected) {
		t.Fatalf("before Dial: %v", errs[6])
	}
}

func TestIMAPReaderPrintsNoSecrets(t *testing.T) {
	r := testReader(fakeHost + ":993")
	s := fmt.Sprintf("%v %+v %#v", r, r, r)
	if strings.Contains(s, testPass) || strings.Contains(s, testUser) {
		t.Fatalf("printed: %s", s)
	}
}
