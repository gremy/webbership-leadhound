package mailbox

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"regexp"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// MessageKind is what an incoming message is, for the stop-on-reply rules.
type MessageKind string

const (
	// KindReply: a person answered one of our messages (by thread or by domain).
	KindReply MessageKind = "reply"
	// KindAutoReply: an automatic answer (out-of-office, list, bulk); recorded only.
	KindAutoReply MessageKind = "auto_reply"
	// KindBounce: a delivery-status report or a mailer-daemon notice.
	KindBounce MessageKind = "bounce"
	// KindOptOut: a matched message asking to hear no more.
	KindOptOut MessageKind = "opt_out"
	// KindUnmatched: anything else, an opt-out with no match to us included.
	KindUnmatched MessageKind = "unmatched"
)

// MaxReportPart is the most of one report part (RA4) that is read.
const MaxReportPart = 64 << 10

// Header is what Classify reads of a message: its top-level header fields and, for a
// multipart/report, the peeked delivery-status and headers parts (each at most
// MaxReportPart bytes; nil for any other message).
type Header struct {
	Fields mail.Header
	Report [][]byte
}

// ParseHeader reads a raw header block (as BODY.PEEK[HEADER] returns it) and keeps the
// report parts alongside it.
func ParseHeader(raw []byte, report [][]byte) (Header, error) {
	if !bytes.HasSuffix(raw, []byte("\r\n\r\n")) && !bytes.HasSuffix(raw, []byte("\n\n")) {
		raw = append(append([]byte{}, raw...), "\r\n\r\n"...)
	}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Header{}, fmt.Errorf("mailbox: header: %w", err)
	}
	return Header{Fields: m.Header, Report: report}, nil
}

// Result is the whole of what Classify keeps of a message. Nothing of the body is in it.
type Result struct {
	Kind MessageKind
	// MatchedMessageID is our Message-ID the message answers (no angle brackets): from
	// In-Reply-To or References, or, for a bounce, from the report's headers part. It is
	// empty for a message matched by domain only.
	MatchedMessageID string
	// FromDomain is the registrable domain of the From address, never the address.
	FromDomain string
	// FinalRecipient is the address a bounce reports as failed (bounce only).
	FinalRecipient string
}

var (
	// optOutWords (RA5, FO1): Go's \b is ASCII-only, so the Romanian forms that end in a
	// letter with a diacritic (dezabonează, dezabonați) are matched by their stem, with no
	// boundary after it (it errs toward opt-out).
	optOutWords = regexp.MustCompile(`(?i)\b(unsubscribe|remove\s+me|dezabonare|dezabona|dezabonez|dezabonati|abmelden|afmelden|abbestellen|austragen|opt[\s-]?out)\b|(?i)\b(dezaboneaz[aă]|dezabona[tț]i)`)
	msgIDRe     = regexp.MustCompile(`<([^<>\s]+)>`)
	wordDecoder = new(mime.WordDecoder)
)

// Classify sorts one message, in this order: bounce, auto_reply, opt_out, reply,
// unmatched. firstText is at most the first 600 bytes of the text; it is read and
// dropped. ownLines are our own opt-out lines (sender.opt_out_lines and the selftest
// sentence), taken out of the text before the opt-out words are matched (FO3).
// ourIDs says whether a Message-ID is one we sent; contactedDomain whether a
// registrable domain is one we wrote to. An opt-out acts only when it is matched to us,
// so an unmatched one is unmatched.
func Classify(h Header, firstText []byte, ownLines []string, ourIDs func(id string) bool, contactedDomain func(domain string) bool) Result {
	f := h.Fields
	if f == nil {
		f = mail.Header{}
	}
	res := Result{FromDomain: fromDomain(f)}
	threadID := threadMatch(f, ourIDs)

	if isBounce(f) {
		res.Kind = KindBounce
		id, final := readReport(h.Report)
		if id != "" && ourIDs(id) {
			res.MatchedMessageID = id
		}
		res.FinalRecipient = final
		return res
	}
	if isAutomatic(f) {
		res.Kind = KindAutoReply
		res.MatchedMessageID = threadID
		return res
	}
	matched := threadID != "" || (res.FromDomain != "" && contactedDomain(res.FromDomain))
	res.MatchedMessageID = threadID
	switch {
	case !matched:
		res.Kind = KindUnmatched
		res.MatchedMessageID = ""
	case isOptOut(f, firstText, ownLines):
		res.Kind = KindOptOut
	default:
		res.Kind = KindReply
	}
	return res
}

// isBounce: a delivery-status report (RFC 3464), or a null return path from a mailer.
func isBounce(f mail.Header) bool {
	mt, params, err := mime.ParseMediaType(f.Get("Content-Type"))
	if err == nil && mt == "multipart/report" && strings.EqualFold(params["report-type"], "delivery-status") {
		return true
	}
	if strings.TrimSpace(f.Get("Return-Path")) != "<>" {
		return false
	}
	addr, err := mail.ParseAddress(f.Get("From"))
	if err != nil {
		return false
	}
	local, _, _ := strings.Cut(strings.ToLower(addr.Address), "@")
	return local == "mailer-daemon" || local == "postmaster"
}

// isAutomatic: RFC 3834's Auto-Submitted (anything but "no"), the X-Autoreply and
// X-Autorespond markers, or a Precedence of auto_reply, bulk or junk.
func isAutomatic(f mail.Header) bool {
	if v, ok := f["Auto-Submitted"]; ok && len(v) > 0 {
		token, _, _ := strings.Cut(v[0], ";")
		if !strings.EqualFold(strings.TrimSpace(token), "no") {
			return true
		}
	}
	if _, ok := f["X-Autoreply"]; ok {
		return true
	}
	if _, ok := f["X-Autorespond"]; ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(f.Get("Precedence"))) {
	case "auto_reply", "bulk", "junk":
		return true
	}
	return false
}

// OptOutWhere is where OptOutIn found an opt-out.
type OptOutWhere string

const (
	// OptOutNone: no opt-out word anywhere.
	OptOutNone OptOutWhere = ""
	// OptOutSubject: the subject asks to hear no more.
	OptOutSubject OptOutWhere = "subject"
	// OptOutText: the sender's own words ask to hear no more (FO1).
	OptOutText OptOutWhere = "text"
	// OptOutQuoted: an opt-out word is in the text, but only in quoted text or in our own
	// lines: the old rule (RA5) would have read it as an opt-out; it is a reply.
	OptOutQuoted OptOutWhere = "quoted"
)

// OptOutIn says where a message asks to hear no more. The subject rules come first (RA5,
// unchanged): an opt-out word in the decoded subject, or a subject that is just "stop".
// Then the text, decided on decoded text only (FO7): the first text part of a multipart
// (FO4), or a single part decoded by its top-level Content-Transfer-Encoding; the text as
// it came only when nothing decoded. An opt-out word in the sender's own words, above or
// below a quote (OwnText: FO2 quotes out, FO3 our lines out), or own words that are just
// "stop", is OptOutText. A word found only in the raw views the old rule read, or in the
// decoded text before quotes were taken out, is OptOutQuoted.
func OptOutIn(f mail.Header, firstText []byte, ownLines []string) OptOutWhere {
	subject := f.Get("Subject")
	if dec, err := wordDecoder.DecodeHeader(subject); err == nil {
		subject = dec
	}
	if strings.EqualFold(strings.TrimSpace(subject), "stop") || optOutWords.MatchString(subject) {
		return OptOutSubject
	}
	cte := f.Get("Content-Transfer-Encoding")
	body, html := firstTextPart(firstText, f.Get("Content-Type"), cte)
	if len(bytes.TrimSpace(body)) == 0 {
		body, html = firstText, false
	}
	own, tail := OwnText(body, html, ownLines)
	if optOutWords.Match(own) || optOutWords.Match(tail) || isJustStop(own) {
		return OptOutText
	}
	for _, text := range append(textViews(firstText, cte), body) {
		if optOutWords.Match(text) {
			return OptOutQuoted
		}
	}
	return OptOutNone
}

// isOptOut (RA5, FO1-FO3): the subject asks to hear no more, or the sender's own words do;
// an opt-out word that sits only in quoted text or in our own opt-out line is not one.
func isOptOut(f mail.Header, firstText []byte, ownLines []string) bool {
	where := OptOutIn(f, firstText, ownLines)
	return where == OptOutSubject || where == OptOutText
}

// textViews returns the first text as it came and, decoded, as quoted-printable and (when
// the message says so) base64, so an encoded word is still seen. A cut at 600 bytes can
// leave the encoding unfinished; whatever decodes is used.
func textViews(text []byte, cte string) [][]byte {
	views := [][]byte{text}
	if qp, _ := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(text))); len(qp) > 0 {
		views = append(views, qp)
	}
	if strings.EqualFold(strings.TrimSpace(cte), "base64") {
		if dec := decodeBase64Prefix(text); len(dec) > 0 {
			views = append(views, dec)
		}
	}
	return views
}

// decodePart undoes a report part's Content-Transfer-Encoding (base64 or
// quoted-printable; anything else is taken as it is) and caps the result at
// MaxReportPart. A part cut short by the fetch decodes as far as it goes.
func decodePart(data []byte, encoding string) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		data = decodeBase64Prefix(data)
	case "quoted-printable":
		// A cut can end inside an escape; keep what decoded before it.
		data, _ = io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
	}
	return cut(data, MaxReportPart)
}

// decodeBase64Prefix decodes base64 with line breaks and spaces removed, dropping an
// unfinished last quantum; it returns nil when the data is not base64.
func decodeBase64Prefix(text []byte) []byte {
	clean := bytes.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, text)
	clean = clean[:len(clean)/4*4]
	dec, err := base64.StdEncoding.DecodeString(string(clean))
	if err != nil {
		return nil
	}
	return dec
}

// threadMatch returns the first of our Message-IDs named in In-Reply-To or References.
func threadMatch(f mail.Header, ourIDs func(string) bool) string {
	for _, v := range []string{f.Get("In-Reply-To"), f.Get("References")} {
		for _, m := range msgIDRe.FindAllStringSubmatch(v, -1) {
			if ourIDs(m[1]) {
				return m[1]
			}
		}
	}
	return ""
}

// fromDomain is the registrable domain of the From address, or "".
func fromDomain(f mail.Header) string {
	list, err := f.AddressList("From")
	if err != nil || len(list) == 0 {
		return ""
	}
	return addrDomain(list[0].Address)
}

func addrDomain(addr string) string {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 {
		return ""
	}
	d, err := sitelist.Registrable(addr[i+1:])
	if err != nil {
		return ""
	}
	return d
}

// readReport reads the original Message-ID (from the text/rfc822-headers or
// message/rfc822 part) and the Final-Recipient address (from the delivery-status part).
// Each part is a series of header blocks; the first value of each field wins.
func readReport(parts [][]byte) (messageID, finalRecipient string) {
	for _, p := range parts {
		if len(p) > MaxReportPart {
			p = p[:MaxReportPart]
		}
		r := textproto.NewReader(bufio.NewReader(bytes.NewReader(p)))
		for {
			block, err := r.ReadMIMEHeader()
			if messageID == "" {
				if m := msgIDRe.FindStringSubmatch(block.Get("Message-Id")); m != nil {
					messageID = m[1]
				}
			}
			if finalRecipient == "" {
				finalRecipient = reportAddress(block.Get("Final-Recipient"))
			}
			// The end of the part, or a line that is not a header (a message/rfc822
			// part's body), ends it.
			if err != nil {
				break
			}
		}
	}
	return messageID, finalRecipient
}

// reportAddress reads "rfc822; user@host" (RFC 3464 §2.3.2), the address in lower case.
func reportAddress(v string) string {
	typ, addr, ok := strings.Cut(v, ";")
	if !ok || !strings.EqualFold(strings.TrimSpace(typ), "rfc822") {
		return ""
	}
	addr = strings.Trim(strings.TrimSpace(addr), "<>")
	a, err := mail.ParseAddress(addr)
	if err != nil || a.Address != addr {
		return ""
	}
	return strings.ToLower(addr)
}
