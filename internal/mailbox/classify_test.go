package mailbox

import (
	"bytes"
	"encoding/base64"
	"mime/quotedprintable"
	"strings"
	"testing"
)

// Every address and Message-ID here is a reserved name, built at run time.
var (
	ourID      = "lh-1a2b" + "@" + "sender.example"
	ourOtherID = "lh-9z8y" + "@" + "sender.example"
	leadAddr   = "contact" + "@" + "northwind.example"
	colleague  = "ana.pop" + "@" + "mail.northwind.example"
	stranger   = "someone" + "@" + "fabrikam.example"
	daemon     = "MAILER-DAEMON" + "@" + "mx.sender.example"
)

func ours(id string) bool { return id == ourID || id == ourOtherID }

func contacted(domain string) bool { return domain == "northwind.example" }

// hdr builds a raw header block from name/value pairs, CRLF-ended, with the blank line.
func hdr(kv ...string) []byte {
	var b bytes.Buffer
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteString(kv[i] + ": " + kv[i+1] + "\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// dsn builds a delivery-status report as Fetched would hold it: the raw header, the first
// 600 bytes of the text (all MIME preamble and the human-readable part here) and the two
// report parts peeked by section.
func dsn(t *testing.T) (raw, firstText []byte, report [][]byte) {
	t.Helper()
	raw = hdr(
		"From", "Mail Delivery System <"+daemon+">",
		"To", "outreach"+"@"+"sender.example",
		"Subject", "Undelivered Mail Returned to Sender",
		"Return-Path", "<>",
		"Content-Type", `multipart/report; report-type=delivery-status; boundary="b1"`,
	)
	human := "--b1\r\nContent-Type: text/plain\r\n\r\n" + strings.Repeat("This is the mail system. ", 40) + "\r\n"
	status := "Reporting-MTA: dns; mx.sender.example\r\n\r\n" +
		"Final-Recipient: rfc822; " + leadAddr + "\r\n" +
		"Action: failed\r\nStatus: 5.1.1\r\n"
	headers := "From: Glitch <outreach" + "@" + "sender.example>\r\n" +
		"To: " + leadAddr + "\r\n" +
		"Subject: Drupal 7\r\n" +
		"Message-ID: <" + ourID + ">\r\n\r\n"
	body := human + "--b1\r\nContent-Type: message/delivery-status\r\n\r\n" + status +
		"--b1\r\nContent-Type: text/rfc822-headers\r\n\r\n" + headers + "--b1--\r\n"
	if strings.Index(body, "Message-ID") <= 600 {
		t.Fatal("the fixture's headers part must sit past byte 600")
	}
	return raw, []byte(body[:600]), [][]byte{[]byte(status), []byte(headers)}
}

func TestClassify(t *testing.T) {
	inReply := []string{"In-Reply-To", "<" + ourID + ">", "References", "<" + ourOtherID + "> <" + ourID + ">"}
	from := func(addr string, kv ...string) []byte {
		return hdr(append([]string{"From", "Ana <" + addr + ">", "Subject", "Re: Drupal 7"}, kv...)...)
	}
	cases := []struct {
		name      string
		raw       []byte
		text      string
		want      MessageKind
		wantID    string
		wantDom   string
		wantFinal string
	}{
		{"reply by In-Reply-To", from(stranger, inReply...), "Sounds good, call me.", KindReply, ourID, "fabrikam.example", ""},
		{"reply by References only", from(stranger, "References", "<other"+"@"+"x.example> <"+ourOtherID+">"),
			"Yes.", KindReply, ourOtherID, "fabrikam.example", ""},
		{"reply from a colleague at the same domain", from(colleague), "My colleague forwarded this.",
			KindReply, "", "northwind.example", ""},
		{"nu mai is a reply, not an opt-out", from(leadAddr, inReply...), "Mulțumim, nu mai avem pe nimeni pe Drupal.",
			KindReply, ourID, "northwind.example", ""},
		{"stop inside the text is a reply", from(leadAddr, inReply...), "We will stop using Drupal 7 next year.",
			KindReply, ourID, "northwind.example", ""},
		{"opt-out: unsubscribe in the subject", hdr("From", leadAddr, "Subject", "unsubscribe"), "",
			KindOptOut, "", "northwind.example", ""},
		{"opt-out: remove me in the text", from(leadAddr, inReply...), "Please remove   me from your list.",
			KindOptOut, ourID, "northwind.example", ""},
		{"opt-out: dezabonare", from(leadAddr, inReply...), "Dezabonare, vă rog.", KindOptOut, ourID, "northwind.example", ""},
		{"opt-out: dezabona", from(leadAddr, inReply...), "Vă rog să mă dezabonați.", KindOptOut, ourID, "northwind.example", ""},
		{"opt-out: abmelden", from(leadAddr, inReply...), "Bitte abmelden.", KindOptOut, ourID, "northwind.example", ""},
		{"opt-out: afmelden", from(leadAddr, inReply...), "Graag afmelden.", KindOptOut, ourID, "northwind.example", ""},
		{"opt-out: Q-encoded subject", hdr("From", leadAddr, "Subject", "=?utf-8?q?Dezabonare_v=C4=83_rog?="), "",
			KindOptOut, "", "northwind.example", ""},
		{"opt-out: quoted-printable soft break", hdr("From", leadAddr, "Subject", "Re: Drupal 7",
			"Content-Transfer-Encoding", "quoted-printable"), "Please unsub=\r\nscribe me.", KindOptOut, "", "northwind.example", ""},
		{"opt-out: bare stop as the whole subject", hdr("From", leadAddr, "Subject", " STOP "), "",
			KindOptOut, "", "northwind.example", ""},
		{"stop in a longer subject is a reply", hdr("From", leadAddr, "Subject", "Re: stop the Drupal 7 work?"), "",
			KindReply, "", "northwind.example", ""},
		{"opt-out with no match cannot act", hdr("From", stranger, "Subject", "unsubscribe"), "",
			KindUnmatched, "", "fabrikam.example", ""},
		{"out-of-office", from(leadAddr, append(inReply, "Auto-Submitted", "auto-replied")...),
			"I am out of the office. Please remove me... no, unsubscribe.", KindAutoReply, ourID, "northwind.example", ""},
		{"Auto-Submitted: no is not automatic", from(leadAddr, append(inReply, "Auto-Submitted", "No")...),
			"Thanks.", KindReply, ourID, "northwind.example", ""},
		{"X-Autoreply", from(leadAddr, "X-Autoreply", "yes"), "Away.", KindAutoReply, "", "northwind.example", ""},
		{"X-Autorespond", from(leadAddr, "X-Autorespond", "1"), "Away.", KindAutoReply, "", "northwind.example", ""},
		{"Precedence: bulk", from(stranger, "Precedence", "bulk"), "News.", KindAutoReply, "", "fabrikam.example", ""},
		{"Precedence: auto_reply", from(leadAddr, "Precedence", "auto_reply"), "", KindAutoReply, "", "northwind.example", ""},
		{"Precedence: junk", from(stranger, "Precedence", "junk"), "", KindAutoReply, "", "fabrikam.example", ""},
		{"Precedence: list is not automatic", from(stranger, "Precedence", "list"), "", KindUnmatched, "", "fabrikam.example", ""},
		{"null return path from postmaster is a bounce", hdr("From", "postmaster"+"@"+"mx.fabrikam.example",
			"Return-Path", "<>", "Subject", "Delivery failure"), "", KindBounce, "", "fabrikam.example", ""},
		{"null return path from a person is not a bounce", hdr("From", stranger, "Return-Path", "<>",
			"Subject", "Hello"), "", KindUnmatched, "", "fabrikam.example", ""},
		{"unmatched", hdr("From", stranger, "Subject", "A question"), "Hello there.", KindUnmatched, "", "fabrikam.example", ""},
		{"no From at all", hdr("Subject", "x"), "", KindUnmatched, "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, err := ParseHeader(c.raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := Classify(h, []byte(c.text), nil, ours, contacted)
			want := Result{Kind: c.want, MatchedMessageID: c.wantID, FromDomain: c.wantDom, FinalRecipient: c.wantFinal}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestClassifyBounceReadsTheReportParts(t *testing.T) {
	raw, first, report := dsn(t)
	h, err := ParseHeader(raw, report)
	if err != nil {
		t.Fatal(err)
	}
	got := Classify(h, first, nil, ours, contacted)
	want := Result{Kind: KindBounce, MatchedMessageID: ourID, FromDomain: "sender.example", FinalRecipient: leadAddr}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// Without the parts the first 600 bytes alone cannot match it.
	h, _ = ParseHeader(raw, nil)
	if got := Classify(h, first, nil, ours, contacted); got.MatchedMessageID != "" || got.FinalRecipient != "" || got.Kind != KindBounce {
		t.Fatalf("without the report parts: %+v", got)
	}

	// A report that is not ours keeps its final recipient and carries no match.
	h, _ = ParseHeader(raw, report)
	if got := Classify(h, first, nil, func(string) bool { return false }, contacted); got.MatchedMessageID != "" || got.FinalRecipient != leadAddr {
		t.Fatalf("a foreign report: %+v", got)
	}
}

func TestClassifyBounceByContentTypeOnly(t *testing.T) {
	// A report from an ordinary-looking sender still classifies by its Content-Type.
	raw := hdr("From", stranger, "Subject", "Report",
		"Content-Type", `multipart/report; boundary=x; report-type="delivery-status"`)
	h, err := ParseHeader(raw, [][]byte{[]byte("Final-Recipient: RFC822;<" + leadAddr + ">\r\n")})
	if err != nil {
		t.Fatal(err)
	}
	got := Classify(h, nil, nil, ours, contacted)
	if got.Kind != KindBounce || got.FinalRecipient != leadAddr {
		t.Fatalf("got %+v", got)
	}
	// Another report type (a read receipt) is not a bounce.
	raw = hdr("From", stranger, "Content-Type", `multipart/report; report-type=disposition-notification; boundary=x`)
	h, _ = ParseHeader(raw, nil)
	if got := Classify(h, nil, nil, ours, contacted); got.Kind == KindBounce {
		t.Fatalf("a disposition notification classified as a bounce: %+v", got)
	}
}

func TestClassifyReturnsNoAddressOrText(t *testing.T) {
	raw := hdr("From", "Ana <"+leadAddr+">", "Subject", "Re: Drupal 7", "In-Reply-To", "<"+ourID+">")
	h, _ := ParseHeader(raw, nil)
	got := Classify(h, []byte("secret body words"), nil, ours, contacted)
	for _, f := range []string{got.MatchedMessageID, got.FromDomain, got.FinalRecipient, string(got.Kind)} {
		if strings.Contains(f, "contact") || strings.Contains(f, "secret") || strings.Contains(f, "@northwind") {
			t.Fatalf("result carries an address or body text: %+v", got)
		}
	}
}

func TestParseHeaderRejectsGarbage(t *testing.T) {
	if _, err := ParseHeader([]byte("not a header line\r\n\r\n"), nil); err == nil {
		t.Fatal("want an error for a malformed header")
	}
}

// encodeQP writes s as quoted-printable with short lines, so soft breaks fall inside the
// fields that must be read.
func encodeQP(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := quotedprintable.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Re-wrap at 20 characters with soft breaks.
	var out bytes.Buffer
	for _, line := range strings.SplitAfter(b.String(), "\r\n") {
		for len(line) > 22 && !strings.HasPrefix(line[18:], "=") && !strings.Contains(line[17:20], "=") {
			out.WriteString(line[:18] + "=\r\n")
			line = line[18:]
		}
		out.WriteString(line)
	}
	return out.Bytes()
}

func encodeB64(s string) []byte {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var out strings.Builder
	for len(enc) > 76 {
		out.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	out.WriteString(enc + "\r\n")
	return []byte(out.String())
}

func TestClassifyEncodedDSN(t *testing.T) {
	raw, first, report := dsn(t)
	for _, c := range []struct {
		name     string
		encoding string
		encode   func(string) []byte
	}{
		{"base64", "base64", encodeB64},
		{"quoted-printable", "Quoted-Printable", func(s string) []byte { return encodeQP(t, s) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var parts [][]byte
			for _, p := range report {
				enc := c.encode(string(p))
				if bytes.Contains(enc, []byte(ourID)) && bytes.Contains(enc, []byte("Final-Recipient: rfc822; "+leadAddr)) {
					t.Fatal("the encoding must hide the fields")
				}
				parts = append(parts, decodePart(enc, c.encoding))
			}
			h, err := ParseHeader(raw, parts)
			if err != nil {
				t.Fatal(err)
			}
			got := Classify(h, first, nil, ours, contacted)
			want := Result{Kind: KindBounce, MatchedMessageID: ourID, FromDomain: "sender.example", FinalRecipient: leadAddr}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestDecodePart(t *testing.T) {
	big := strings.Repeat("Final-Recipient: rfc822; x"+"@"+"y.example\r\n", MaxReportPart/20)
	if got := decodePart(encodeB64(big), "base64"); len(got) != MaxReportPart || !strings.HasPrefix(big, string(got)) {
		t.Fatalf("base64: %d bytes, want the first %d", len(got), MaxReportPart)
	}
	// A base64 part cut mid-quantum decodes as far as it goes.
	enc := encodeB64("Message-ID: <" + ourID + ">\r\n")
	if got := decodePart(enc[:len(enc)-5], "base64"); !strings.HasPrefix("Message-ID: <"+ourID+">\r\n", string(got)) || len(got) == 0 {
		t.Fatalf("cut base64: %q", got)
	}
	if got := decodePart([]byte("plain =41 text"), "7bit"); string(got) != "plain =41 text" {
		t.Fatalf("7bit changed: %q", got)
	}
	if got := decodePart([]byte("!!!! ####"), "base64"); len(got) != 0 {
		t.Fatalf("garbage base64: %q", got)
	}
}

// replyHdr is a reply to our message from the lead, with extra header fields.
func replyHdr(subject string, kv ...string) []byte {
	return hdr(append([]string{"From", "Ana <" + leadAddr + ">", "Subject", subject, "In-Reply-To", "<" + ourID + ">"}, kv...)...)
}

func TestClassifyOwnText(t *testing.T) {
	selftestReply := "Got it.\n\n" + attrEN + "\n" + quote(oldSelftest, 1)
	cases := []struct {
		name  string
		raw   []byte
		text  string
		lines []string
		want  MessageKind
	}{
		// The live incident of 26 Oct 2026 (red on the old code: it read an opt-out).
		{"live selftest reply", replyHdr("Re: Leadhound selftest"), selftestReply, testOwnLines, KindReply},
		{"live selftest reply, quote stripping alone", replyHdr("Re: Leadhound selftest"), selftestReply, nil, KindReply},
		{"reply quoting our en line", replyHdr("Re: Drupal 7"), "Got it, thanks.\n\n" + attrEN + "\n" + quote(ourBody+enLine, 1),
			testOwnLines, KindReply},
		{"reply quoting our en line, quote stripping alone", replyHdr("Re: Drupal 7"),
			"Got it, thanks.\n\n" + attrEN + "\n" + quote(ourBody+enLine, 1), nil, KindReply},
		// Red on the old code: the base64 part hid the words.
		{"Gmail base64 multipart Romanian opt-out", replyHdr("Re: Drupal 7", "Content-Type", `multipart/alternative; boundary="b1"`),
			gmailAlternative("Dezabonare, vă rog.\r\n", "<div>Dezabonare, vă rog.</div>"), testOwnLines, KindOptOut},
		{"Outlook top-post reply", replyHdr("Re: Drupal 7"), "Thanks, interesting.\n\n" + outlookHeader + ourBody + enLine + "\n",
			testOwnLines, KindReply},
		{"subject wins over a quote-only body", replyHdr("unsubscribe"), "Got it.\n\n" + attrEN + "\n" + quote(ourBody, 1),
			testOwnLines, KindOptOut},
		// FO7: our line soft-wrapped in a quoted-printable part, unquoted.
		{"QP part with our line soft-wrapped", replyHdr("Re: Drupal 7", "Content-Type", `multipart/alternative; boundary="b1"`),
			"--b1\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
				"Thanks.\r\n\r\nNot relevant? Reply =E2=80=9Cunsub=\r\nscribe=E2=80=9D and I won=E2=80=99t write again.\r\n--b1--\r\n",
			testOwnLines, KindReply},
		// FO7: a short plain part, then a Gmail HTML part quoting our line, inside the peek.
		{"plain part then HTML quoting our line", replyHdr("Re: Drupal 7", "Content-Type", `multipart/alternative; boundary="b1"`),
			"--b1\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nThanks.\r\n" +
				"--b1\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n<div>Thanks.</div><div class=\"gmail_quote\"><blockquote>" +
				"Not relevant? Reply &quot;unsubscribe&quot; and I won&#39;t write again.</blockquote></div>\r\n--b1--\r\n",
			testOwnLines, KindReply},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.text) > FirstTextBytes {
				t.Fatalf("fixture is %d bytes, more than the %d-byte peek", len(c.text), FirstTextBytes)
			}
			h, err := ParseHeader(c.raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := Classify(h, []byte(c.text), c.lines, ours, contacted); got.Kind != c.want {
				t.Fatalf("got %s, want %s (OptOutIn: %q)", got.Kind, c.want, OptOutIn(h.Fields, []byte(c.text), c.lines))
			}
		})
	}
}

// wrappedMessage is a fixture as a whole message: header and first text.
type wrappedMessage struct {
	name string
	raw  []byte
	text []byte
	want verdict
}

// wrapFixture wraps one hand-tagged text as the ways a client sends it: as it is, base64
// single part, a base64 part of a multipart/alternative, and a quoted-printable part.
func wrapFixture(t *testing.T, name, subject, text string, html bool, want verdict) []wrappedMessage {
	t.Helper()
	ctype := "text/plain"
	if html {
		ctype = "text/html"
	}
	crlf := strings.ReplaceAll(text, "\n", "\r\n")
	var qp bytes.Buffer
	w := quotedprintable.NewWriter(&qp)
	if _, err := w.Write([]byte(crlf)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	part := func(cte, body string) []byte {
		return []byte("This is a multi-part message in MIME format.\r\n\r\n--b1\r\nContent-Type: " + ctype + "; charset=UTF-8\r\n" +
			"Content-Transfer-Encoding: " + cte + "\r\n\r\n" + body + "\r\n--b1--\r\n")
	}
	multi := []string{"Content-Type", `multipart/alternative; boundary="b1"`}
	return []wrappedMessage{
		{name + "/plain", replyHdr(subject, "Content-Type", ctype+"; charset=UTF-8"), []byte(crlf), want},
		{name + "/base64", replyHdr(subject, "Content-Type", ctype+"; charset=UTF-8", "Content-Transfer-Encoding", "base64"),
			encodeB64(crlf), want},
		{name + "/base64 part", replyHdr(subject, multi...), part("base64", string(encodeB64(crlf))), want},
		{name + "/quoted-printable part", replyHdr(subject, multi...), part("quoted-printable", qp.String()), want},
	}
}

// verdictMessages is every hand-tagged fixture wrapped as whole messages (FO5): the
// OwnText shapes, TestClassify's opt-out and stop cases, and the MIME cases.
func verdictMessages(t *testing.T) []wrappedMessage {
	t.Helper()
	var all []wrappedMessage
	for _, c := range ownTextFixtures {
		all = append(all, wrapFixture(t, c.name, "Re: Drupal 7", c.text, c.html, c.want)...)
	}
	for _, c := range []struct {
		subject, text string
		want          verdict
	}{
		{"unsubscribe", "", realOptOut},
		{"=?utf-8?q?Dezabonare_v=C4=83_rog?=", "", realOptOut},
		{" STOP ", "", realOptOut},
		{"Re: stop the Drupal 7 work?", "", notOptOut},
		{"Re: Drupal 7", "Please remove   me from your list.", realOptOut},
		{"Re: Drupal 7", "Dezabonare, vă rog.", realOptOut},
		{"Re: Drupal 7", "Vă rog să mă dezabonați.", realOptOut},
		{"Re: Drupal 7", "Bitte abmelden.", realOptOut},
		{"Re: Drupal 7", "Graag afmelden.", realOptOut},
		{"Re: Drupal 7", "Please unsubscribe me.", realOptOut},
		{"Re: Drupal 7", "Mulțumim, nu mai avem pe nimeni pe Drupal.", notOptOut},
		{"Re: Drupal 7", "We will stop using Drupal 7 next year.", notOptOut},
		{"Re: Drupal 7", "Sounds good, call me.", notOptOut},
	} {
		all = append(all, wrapFixture(t, "classify "+c.subject+" "+c.text, c.subject, c.text, false, c.want)...)
	}
	for _, c := range legacyCharsetMessages(t) {
		all = append(all, wrappedMessage{c.name, c.raw, c.text, c.want})
	}
	return all
}

type charsetMessage struct {
	name  string
	raw   []byte
	text  []byte
	want  verdict
	where OptOutWhere
}

// legacyCharsetMessages (M1, L1) are whole replies with the charset their client declares:
// a windows-1250 8bit body, an iso-8859-2 quoted-printable body, and UTF-8 with cedilla
// letters, each a plain reply above our ro line under an Outlook Original Message quote.
func legacyCharsetMessages(t *testing.T) []charsetMessage {
	t.Helper()
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	reply := crlf("Mul\xFEumesc, revin.\n\n" + originalMessage + ourBody + roLineCP1250 + "\n")
	// iso-8859-2 has no „ ” marks; clients send straight quotes.
	latin2 := strings.NewReplacer("\x84", `"`, "\x94", `"`).Replace(reply)
	cp := func(cs, cte string) []byte {
		return replyHdr("Re: Drupal 7", "Content-Type", "text/plain; charset="+cs, "Content-Transfer-Encoding", cte)
	}
	return []charsetMessage{
		{"windows-1250 8bit reply over our quoted line", cp("windows-1250", "8bit"), []byte(reply), notOptOut, OptOutQuoted},
		{"iso-8859-2 quoted-printable reply over our quoted line", cp("iso-8859-2", "quoted-printable"),
			encodeQP(t, latin2), notOptOut, OptOutQuoted},
		{"utf-8 cedilla reply over our quoted line", cp("UTF-8", "8bit"),
			[]byte(crlf("Mulţumesc, revin.\n\n" + originalMessage + ourBody + roLineCedilla + "\n")), notOptOut, OptOutQuoted},
		{"windows-1250 8bit own dezabonare over our quoted line", cp("windows-1250", "8bit"),
			[]byte(crlf("Dezabonare, v\xE3 rog.\n\n" + originalMessage + ourBody + roLineCP1250 + "\n")), realOptOut, OptOutText},
		{"iso-8859-2 quoted-printable own dezabonați-mă over our quoted line", cp("iso-8859-2", "quoted-printable"),
			encodeQP(t, strings.Replace(latin2, "Mul\xFEumesc, revin.", "V\xE3 rog, dezabona\xFEi-m\xE3.", 1)), realOptOut, OptOutText},
		{"windows-1250 8bit dezabonează", cp("windows-1250", "8bit"),
			[]byte(crlf("V\xE3 rog s\xE3 m\xE3 dezaboneaz\xE3 cineva.\n")), realOptOut, OptOutText},
	}
}

// TestOptOutInLegacyCharsets (M1, L1): the exact verdict for each legacy-charset reply.
func TestOptOutInLegacyCharsets(t *testing.T) {
	for _, c := range legacyCharsetMessages(t) {
		t.Run(c.name, func(t *testing.T) {
			if len(c.text) > FirstTextBytes {
				t.Fatalf("fixture is %d bytes, more than the %d-byte peek", len(c.text), FirstTextBytes)
			}
			h, err := ParseHeader(c.raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := OptOutIn(h.Fields, c.text, testOwnLines); got != c.where {
				t.Fatalf("got %q, want %q\ntext: %q", got, c.where, c.text)
			}
		})
	}
}

// TestOptOutInVerdicts (FO5): every hand-tagged fixture, in every wrapping, reads as its
// tag says. realOptOut must be subject or text; notOptOut must be quoted or none.
func TestOptOutInVerdicts(t *testing.T) {
	for _, m := range verdictMessages(t) {
		t.Run(m.name, func(t *testing.T) {
			h, err := ParseHeader(m.raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := OptOutIn(h.Fields, m.text, testOwnLines)
			opt := got == OptOutSubject || got == OptOutText
			if opt != (m.want == realOptOut) {
				t.Fatalf("want %s, OptOutIn says %q\ntext: %q", m.want, got, m.text)
			}
		})
	}
}

// TestClassifyAgreesWithOptOutIn: Classify's opt_out and OptOutIn never drift apart.
func TestClassifyAgreesWithOptOutIn(t *testing.T) {
	for _, m := range verdictMessages(t) {
		h, err := ParseHeader(m.raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, lines := range [][]string{testOwnLines, nil} {
			where := OptOutIn(h.Fields, m.text, lines)
			got := Classify(h, m.text, lines, ours, contacted)
			if (got.Kind == KindOptOut) != (where == OptOutSubject || where == OptOutText) {
				t.Fatalf("%s: Classify %s, OptOutIn %q", m.name, got.Kind, where)
			}
		}
	}
}

func TestOptOutInWhere(t *testing.T) {
	h, _ := ParseHeader(replyHdr("Re: Drupal 7"), nil)
	for _, c := range []struct {
		text string
		want OptOutWhere
	}{
		{"Got it.\n\n" + attrEN + "\n" + quote(ourBody+enLine, 1), OptOutQuoted},
		{"Thanks.", OptOutNone},
		{"Please unsubscribe me.", OptOutText},
		{"stop!", OptOutText},
	} {
		if got := OptOutIn(h.Fields, []byte(c.text), testOwnLines); got != c.want {
			t.Fatalf("%q: got %q, want %q", c.text, got, c.want)
		}
	}
	h, _ = ParseHeader(replyHdr("Unsubscribe"), nil)
	if got := OptOutIn(h.Fields, nil, nil); got != OptOutSubject {
		t.Fatalf("subject: got %q", got)
	}
}
