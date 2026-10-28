// Package outreach turns one lead's stored facts into drafts and checks them. A draft is
// either the operator's own template with slots filled from stored rows, or written by
// Claude (writer.go) from a fact sheet of stored rows: Claude writes, it never decides a
// fact. Every claim must cite a stored fact, the deterministic grounding check (ground.go)
// refuses anything else, and both kinds render through the same footer and Check.
package outreach

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
	"time"

	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// Provenance names the stored row a slot value came from. Values taken from leads.yaml
// (the sender, the identity and opt-out lines, the footer, the end-of-life dates) carry
// ConfigSource: table "config" and no row id.
type Provenance struct {
	Table string
	ID    int64
}

// ConfigSource is the provenance of a value read from leads.yaml.
var ConfigSource = Provenance{Table: "config"}

// Slot is one value a template may use, with where it came from.
type Slot struct {
	Value  string
	Source Provenance
}

// ConfigSlot is a slot whose value comes from leads.yaml.
func ConfigSlot(v string) Slot { return Slot{Value: v, Source: ConfigSource} }

// Slots are every value a template may use, in leadconfig.SlotNames order. An empty slot is
// a missing fact: a template that uses it does not render. Render fills Footer itself.
type Slots struct {
	OrgName      Slot
	Domain       Slot
	Host         Slot
	Major        Slot
	Exact        Slot
	EvidenceLine Slot
	FindingURL   Slot
	EOLDate      Slot
	SenderName   Slot
	IdentityLine Slot
	OptOutLine   Slot
	Footer       Slot

	// PrivacyLine is the footer's last line naming the sender's privacy notice
	// (leadconfig.Sender.PrivacyLine); not a template slot: Render adds it under the
	// rendered footer of every email when it is set, and nothing when it is empty.
	PrivacyLine Slot

	// EvidenceSignal is the stored signal EvidenceLine was built from; it is not a template
	// slot. Check allows a number inside EvidenceLine only when it equals this signal's
	// value, so a core-files line ("… at version 10.6.14") passes with or without Exact.
	EvidenceSignal drupal.Signal
}

type renderNamedSlot struct {
	name string
	slot Slot
}

// named lists the slots by their template names, in leadconfig.SlotNames order.
func (s Slots) named() []renderNamedSlot {
	return []renderNamedSlot{
		{"OrgName", s.OrgName}, {"Domain", s.Domain}, {"Host", s.Host}, {"Major", s.Major},
		{"Exact", s.Exact}, {"EvidenceLine", s.EvidenceLine}, {"FindingURL", s.FindingURL},
		{"EOLDate", s.EOLDate}, {"SenderName", s.SenderName}, {"IdentityLine", s.IdentityLine},
		{"OptOutLine", s.OptOutLine}, {"Footer", s.Footer},
	}
}

// field returns the slot with a template name, or nil for an unknown name.
func (s *Slots) field(name string) *Slot {
	switch name {
	case "OrgName":
		return &s.OrgName
	case "Domain":
		return &s.Domain
	case "Host":
		return &s.Host
	case "Major":
		return &s.Major
	case "Exact":
		return &s.Exact
	case "EvidenceLine":
		return &s.EvidenceLine
	case "FindingURL":
		return &s.FindingURL
	case "EOLDate":
		return &s.EOLDate
	case "SenderName":
		return &s.SenderName
	case "IdentityLine":
		return &s.IdentityLine
	case "OptOutLine":
		return &s.OptOutLine
	case "Footer":
		return &s.Footer
	}
	return nil
}

// OrgNameSlot is the organisation's name, or its domain when the name is blank.
func OrgNameSlot(name, domain Slot) Slot {
	if strings.TrimSpace(name.Value) == "" {
		return domain
	}
	return name
}

// Languages EvidenceLine, AgencyEvidenceLine and FormatDate write. Any other language is
// an error, so a lead in it gets no draft until its wording is added here.
const (
	LangEnglish  = "en"
	LangRomanian = "ro"
)

var renderMonthsRO = [12]string{
	"ianuarie", "februarie", "martie", "aprilie", "mai", "iunie",
	"iulie", "august", "septembrie", "octombrie", "noiembrie", "decembrie",
}

// FormatDate writes a date as the templates state it: "9 December 2026" in English,
// "9 decembrie 2026" in Romanian.
func FormatDate(t time.Time, lang string) (string, error) {
	switch lang {
	case LangEnglish:
		return t.Format("2 January 2006"), nil
	case LangRomanian:
		return fmt.Sprintf("%d %s %d", t.Day(), renderMonthsRO[t.Month()-1], t.Year()), nil
	}
	return "", fmt.Errorf("outreach: date: no wording for language %q", lang)
}

// renderEvidenceWords holds each signal's clause per language; %s is the version.
var renderEvidenceWords = map[string]map[string]string{
	LangEnglish: {
		drupal.SigMetaGenerator: "the page's Generator tag names Drupal %s",
		drupal.SigXGenerator:    "the X-Generator header names Drupal %s",
		drupal.SigCoreAssetV:    "the page loads Drupal's core files at version %s",
		drupal.SigD7Settings:    "the site's settings block is Drupal %s's",
		"agency":                "your page at %s offers Drupal upgrades",
	},
	LangRomanian: {
		drupal.SigMetaGenerator: "eticheta Generator a paginii indică Drupal %s",
		drupal.SigXGenerator:    "antetul X-Generator indică Drupal %s",
		drupal.SigCoreAssetV:    "pagina încarcă fișierele de bază Drupal la versiunea %s",
		drupal.SigD7Settings:    "blocul de setări al site-ului este cel din Drupal %s",
		"agency":                "pagina de pe %s oferă actualizări Drupal",
	},
}

func renderEvidenceFormat(lang, key string) (string, error) {
	words, ok := renderEvidenceWords[lang]
	if !ok {
		return "", fmt.Errorf("outreach: evidence: no wording for language %q", lang)
	}
	return words[key], nil
}

// EvidenceLine states the one signal a finding rests on, in lang, as a clause the
// templates use ("I noticed that <line> on <url>"). Only signals that name the version can
// carry a claim, and the version they name must agree with major; anything else is an
// error, so a finding without such a signal gets no draft. The caller puts sig in
// Slots.EvidenceSignal, which is what lets Check accept the version the line names.
func EvidenceLine(sig drupal.Signal, major int, lang string) (string, error) {
	format, err := renderEvidenceFormat(lang, sig.Name)
	if err != nil {
		return "", err
	}
	m := strconv.Itoa(major)
	if major <= 0 {
		return "", fmt.Errorf("outreach: evidence %s: no major version", sig)
	}
	switch sig.Name {
	case drupal.SigMetaGenerator, drupal.SigXGenerator:
		if sig.Value == m {
			return fmt.Sprintf(format, m), nil
		}
	case drupal.SigCoreAssetV:
		if strings.HasPrefix(sig.Value, m+".") {
			return fmt.Sprintf(format, sig.Value), nil
		}
	case drupal.SigD7Settings:
		if major == 7 || major == 6 {
			return fmt.Sprintf(format, m), nil
		}
	default:
		return "", fmt.Errorf("outreach: evidence %s: the signal names no version", sig)
	}
	return "", fmt.Errorf("outreach: evidence %s: does not name Drupal %d", sig, major)
}

// AgencyEvidenceLine states an agency's finding in lang as a clause the templates use ("I
// saw that <line> (<url>)"): the host of the stored finding page, without "www.", and that
// it offers Drupal upgrades, which is what the agency finding pattern matched there. The
// URL itself stays in the FindingURL slot. A URL that is not absolute http(s), or a
// language without wording, is an error, so such a finding gets no draft.
func AgencyEvidenceLine(findingURL, lang string) (string, error) {
	format, err := renderEvidenceFormat(lang, "agency")
	if err != nil {
		return "", err
	}
	u, err := url.Parse(strings.TrimSpace(findingURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return "", fmt.Errorf("outreach: agency evidence: %q is not an absolute http(s) URL", findingURL)
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if !renderDomainOK(host) {
		return "", fmt.Errorf("outreach: agency evidence: %q has no usable host", findingURL)
	}
	return fmt.Sprintf(format, host), nil
}

// DraftKind says which text a draft is: an email step, a contact-form message or a
// LinkedIn note.
type DraftKind int

// The draft kinds.
const (
	DraftEmail DraftKind = iota
	DraftForm
	DraftLinkedIn
)

// Template is one text to render. An email has a subject, a body and a footer; a form or
// LinkedIn text has a body only, and may not use the Footer slot.
type Template struct {
	Kind    DraftKind
	Subject *template.Template
	Body    *template.Template
	Footer  *template.Template
}

// EmailTemplate is one email step with the country's footer.
func EmailTemplate(e leadconfig.Email, footer *template.Template) Template {
	return Template{Kind: DraftEmail, Subject: e.Subject, Body: e.Body, Footer: footer}
}

// FormTemplate is a contact-form message.
func FormTemplate(t *template.Template) Template { return Template{Kind: DraftForm, Body: t} }

// LinkedInTemplate is a LinkedIn note.
func LinkedInTemplate(t *template.Template) Template {
	return Template{Kind: DraftLinkedIn, Body: t}
}

// Draft is one rendered text.
type Draft struct {
	Kind    DraftKind
	Subject string // empty for a form or LinkedIn text
	Body    string // the whole text, footer included
	// BodyAboveFooter is the copy the word bounds count: the body up to the footer. For a
	// form or LinkedIn text it equals Body.
	BodyAboveFooter string
	// Slots are the values rendered, Footer filled with the rendered footer.
	Slots Slots
	// Used names the slots the subject, body and footer read, in SlotNames order.
	Used []string
	// FixedText is the templates' own text with every action left out: the subject, body
	// and footer text the operator wrote. Check allows the numbers it holds.
	FixedText string
	// MessageID is set by the caller with NewMessageID; Render leaves it empty.
	MessageID string
}

// renderFooterMark stands in for the footer while the body renders, so the copy above it
// can be cut out. It holds a NUL, which no template or stored value carries.
const renderFooterMark = "\x00leadhound-footer\x00"

// Render fills a template from slots. Every template runs with missingkey=error over the
// non-empty slots only, so a template that uses a missing fact returns an error and no
// draft. For an email the footer renders on its own first (every slot but Footer), then
// sits where the body names {{.Footer}}, or after a blank line when the body does not;
// nothing but white space may follow it.
func Render(t Template, s Slots) (Draft, error) {
	if t.Body == nil || (t.Kind == DraftEmail && (t.Subject == nil || t.Footer == nil)) {
		return Draft{}, errors.New("outreach: render: the template is incomplete")
	}
	values := map[string]string{}
	for _, ns := range s.named() {
		if ns.name != "Footer" && ns.slot.Value != "" {
			values[ns.name] = ns.slot.Value
		}
	}
	d := Draft{Kind: t.Kind}
	trees := []*template.Template{t.Body}
	s.Footer = Slot{}
	if t.Kind == DraftEmail {
		trees = append(trees, t.Subject, t.Footer)
		footer, err := renderExec(t.Footer, values)
		if err != nil {
			return Draft{}, err
		}
		footer = strings.TrimSpace(footer)
		if line := strings.TrimSpace(s.PrivacyLine.Value); line != "" {
			footer += "\n" + line
		}
		s.Footer = ConfigSlot(footer)
		subject, err := renderExec(t.Subject, values)
		if err != nil {
			return Draft{}, err
		}
		if d.Subject = strings.TrimSpace(subject); strings.ContainsAny(d.Subject, "\r\n") {
			return Draft{}, errors.New("outreach: render: the subject spans more than one line")
		}
		values["Footer"] = renderFooterMark
	}
	body, err := renderExec(t.Body, values)
	if err != nil {
		return Draft{}, err
	}
	if t.Kind == DraftEmail {
		above, below, found := strings.Cut(body, renderFooterMark)
		switch {
		case strings.Contains(below, renderFooterMark):
			return Draft{}, errors.New("outreach: render: the body uses the footer more than once")
		case strings.TrimSpace(below) != "":
			return Draft{}, errors.New("outreach: render: the body goes on after the footer")
		case !found:
			above = strings.TrimRight(above, " \t\r\n") + "\n\n"
		}
		d.BodyAboveFooter = above
		body = above + s.Footer.Value + "\n"
	} else {
		body = strings.TrimSpace(body) + "\n"
		d.BodyAboveFooter = body
	}
	d.Body = body
	d.Slots = s

	used := map[string]bool{}
	var fixed []string
	for _, tm := range trees {
		renderWalk(tm.Root, used, &fixed)
	}
	for _, n := range leadconfig.SlotNames {
		if used[n] {
			d.Used = append(d.Used, n)
		}
	}
	d.FixedText = strings.Join(fixed, "\n")
	return d, nil
}

func renderExec(t *template.Template, values map[string]string) (string, error) {
	var b bytes.Buffer
	if err := t.Execute(&b, values); err != nil {
		return "", fmt.Errorf("outreach: render: %w", err)
	}
	return b.String(), nil
}

// renderWalk collects the slot names a template reads and its literal text.
func renderWalk(n parse.Node, used map[string]bool, fixed *[]string) {
	switch n := n.(type) {
	case nil:
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			renderWalk(c, used, fixed)
		}
	case *parse.TextNode:
		*fixed = append(*fixed, string(n.Text))
	case *parse.ActionNode:
		renderWalk(n.Pipe, used, fixed)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		for _, c := range n.Cmds {
			renderWalk(c, used, fixed)
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			renderWalk(a, used, fixed)
		}
	case *parse.FieldNode:
		if len(n.Ident) > 0 {
			used[n.Ident[0]] = true
		}
	case *parse.IfNode:
		renderBranch(&n.BranchNode, used, fixed)
	case *parse.RangeNode:
		renderBranch(&n.BranchNode, used, fixed)
	case *parse.WithNode:
		renderBranch(&n.BranchNode, used, fixed)
	case *parse.TemplateNode:
		renderWalk(n.Pipe, used, fixed)
	}
}

func renderBranch(b *parse.BranchNode, used map[string]bool, fixed *[]string) {
	renderWalk(b.Pipe, used, fixed)
	renderWalk(b.List, used, fixed)
	renderWalk(b.ElseList, used, fixed)
}

// ShadowDomain ends the Message-IDs of a shadow batch, made without an outreach mailbox.
const ShadowDomain = "shadow.invalid"

// MessageIDDomain is the part after "@" in a sending mailbox's address, lower-cased;
// without a usable address it is ShadowDomain.
func MessageIDDomain(outreachUser string) string {
	i := strings.LastIndexByte(outreachUser, '@')
	if i < 0 {
		return ShadowDomain
	}
	d := strings.ToLower(strings.TrimSpace(outreachUser[i+1:]))
	if !renderDomainOK(d) {
		return ShadowDomain
	}
	return d
}

func renderDomainOK(d string) bool {
	return strings.Contains(d, ".") && !strings.ContainsFunc(d, func(r rune) bool {
		return r != '.' && r != '-' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	}) && !slices.Contains(strings.Split(d, "."), "")
}

// NewMessageID makes a Message-ID, 16 random bytes as 32 hex digits, then "@" and the
// sending domain: "<hex>@<domain>", without the angle brackets the header adds.
func NewMessageID(domain string, rnd io.Reader) (string, error) {
	if !renderDomainOK(domain) {
		return "", fmt.Errorf("outreach: message id: %q is not a domain", domain)
	}
	b := make([]byte, 16)
	if _, err := io.ReadFull(rnd, b); err != nil {
		return "", fmt.Errorf("outreach: message id: %w", err)
	}
	return hex.EncodeToString(b) + "@" + domain, nil
}
