package mailbox

import (
	"math/rand/v2"
	"net/mail"
	"strings"
	"testing"
)

// verdict is a fixture's expected answer, written by hand (FO5): never derived from the
// code under test.
type verdict string

const (
	realOptOut verdict = "realOptOut"
	notOptOut  verdict = "notOptOut"
)

// Our own opt-out lines as leads.yaml carries them, and the address our quoted emails
// come from (a reserved name, built at run time).
var (
	enLine        = `Not relevant? Reply "unsubscribe" and I won't write again.`
	roLine        = "Nu vă interesează? Răspundeți cu „dezabonare” și nu vă mai scriu."
	testOwnLines  = []string{enLine, roLine, SelftestLine, OldSelftestLine}
	senderAddr    = "x" + "@" + "sender.example"
	ourBody       = "Hello Ana,\n\nI saw your site still runs Drupal 7. I help teams move to Drupal 11.\n\nGremy\n\n"
	oldSelftest   = "Leadhound selftest, 26 Oct 2026 18:10 EET.\n\n" + OldSelftestLine + "\n"
	attrEN        = "On Mon, 26 Oct 2026 at 18:10, Sam <" + senderAddr + "> wrote:"
	attrRO        = "În lun., 26 oct. 2026 la 18:10, Sam <" + senderAddr + "> a scris:"
	attrDE        = "Am 26.10.2026 um 18:10 schrieb Sam <" + senderAddr + ">:"
	attrNL        = "Op ma 26 okt. 2026 om 18:10 schreef Sam <" + senderAddr + ">:"
	attrFR        = "Le lun. 26 oct. 2026 à 18:10, Sam <" + senderAddr + "> a écrit :"
	outlookHeader = "From: Sam <" + senderAddr + ">\nSent: Monday, 26 October 2026 18:10\nTo: Ana\nSubject: Drupal 7\n\n"
	outlookRO     = "De la: Sam <" + senderAddr + ">\nTrimis: luni, 26 octombrie 2026 18:10\nCătre: Ana\nSubiect: Drupal 7\n\n"
)

// quote prefixes every line of s with depth "> " markers.
func quote(s string, depth int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.Repeat(">", depth) + " " + l
	}
	return strings.Join(lines, "\n") + "\n"
}

type ownFixture struct {
	name string
	text string
	html bool
	want verdict
}

// ownTextFixtures are real-world reply shapes, each tagged by hand (FO5). Task 1.3's
// verdict test wraps every one as a full message.
var ownTextFixtures = []ownFixture{
	// The live incident of 26 Oct 2026: the operator's reply to the old selftest.
	{"live selftest reply, Gmail", "Got it.\n\n" + attrEN + "\n" + quote(oldSelftest, 1), false, notOptOut},
	{"live selftest reply, Outlook", "Got it.\n\n" + outlookHeader + oldSelftest, false, notOptOut},
	{"outreach reply quoting our en line", "Got it, thanks.\n\n" + attrEN + "\n" + quote(ourBody+enLine, 1), false, notOptOut},

	// Traps from the review: the sender's own line must survive.
	{"own line starting On, then attribution", "On second thought, unsubscribe me.\n" + attrEN + "\n> Hello\n", false, realOptOut},
	{"own line starting In, then ro attribution", "In any case, please unsubscribe.\n" + attrRO + "\n> Bună ziua\n", false, realOptOut},
	{"inline under an Outlook quote", "Thanks.\n\n" + outlookHeader + ourBody + enLine + "\n\nunsubscribe\n", false, realOptOut},
	{"inline under an Outlook HTML quote", `<div>Thanks</div><div id="divRplyFwdMsg"><b>From:</b> Sam<br><b>Sent:</b> Monday</div>` +
		`<div>Hello Ana,<br>Not relevant? Reply &quot;unsubscribe&quot; and I won&#39;t write again.<br><br>unsubscribe</div>`, true, realOptOut},

	{"wrapped attribution", "Thanks, interesting.\n\nOn Mon, 26 Oct 2026 at 18:10, Sam <" + senderAddr + ">\nwrote:\n" + quote(ourBody+enLine, 1), false, notOptOut},
	{"wrapped attribution, quote unmarked", "Thanks, interesting.\n\nOn Mon, 26 Oct 2026 at 18:10, Sam <" + senderAddr + ">\nwrote:\n" + ourBody + enLine + "\n", false, notOptOut},
	{"Outlook top-post", "Thanks, interesting.\n\n" + outlookHeader + ourBody + enLine + "\n", false, notOptOut},
	{"Outlook Original Message", "Thanks.\n\n-----Original Message-----\nFrom: Sam\nSent: Monday\n\n" + ourBody + enLine + "\n", false, notOptOut},
	{"Outlook underscore rule", "Thanks.\n\n________________________________\n" + outlookHeader + ourBody + enLine + "\n", false, notOptOut},
	{"Romanian Outlook De la / Trimis", "Mulțumesc.\n\n" + outlookRO + ourBody + roLine + "\n", false, notOptOut},
	{"Gmail Romanian", "Mulțumesc.\n\n" + attrRO + "\n" + quote(ourBody+roLine, 1), false, notOptOut},
	{"German attribution", "Danke.\n\n" + attrDE + "\n" + quote(ourBody+enLine, 1), false, notOptOut},
	{"Dutch attribution", "Bedankt.\n\n" + attrNL + "\n" + quote(ourBody+enLine, 1), false, notOptOut},
	{"French attribution", "Merci.\n\n" + attrFR + "\n" + quote(ourBody+enLine, 1), false, notOptOut},
	{"re-quoted at depth 2", "Yes.\n\n" + attrEN + "\n" + quote("No.\n"+quote(ourBody+enLine, 1), 1), false, notOptOut},

	// A client that quotes without markers.
	{"our en line unquoted, straight quotes", "Thanks.\n\n" + ourBody + enLine + "\n", false, notOptOut},
	{"our en line unquoted, curly quotes", "Thanks.\n\n" + ourBody + "Not relevant? Reply “unsubscribe” and I won’t write again.\n", false, notOptOut},
	{"our en line wrapped", "Thanks.\n\n" + ourBody + "Not relevant? Reply \"unsubscribe\"\nand I won't write again.\n", false, notOptOut},
	{"our en line in another case", "Thanks.\n\n" + strings.ToUpper(enLine) + "\n", false, notOptOut},
	{"our ro line unquoted, straight quotes", "Mulțumesc.\n\n" + ourBody + "Nu vă interesează? Răspundeți cu \"dezabonare\" și nu vă mai scriu.\n", false, notOptOut},
	{"our ro line wrapped", "Mulțumesc.\n\n" + ourBody + "Nu vă interesează? Răspundeți cu „dezabonare”\nși nu vă mai scriu.\n", false, notOptOut},

	// The 600-byte peek cut our line.
	{"our line cut mid-word", "Thanks.\n\n" + ourBody + `Not relevant? Reply "unsubscr`, false, notOptOut},
	{"our line cut after the word", "Thanks.\n\n" + ourBody + `Not relevant? Reply "unsubscribe" and I w`, false, notOptOut},
	{"our line cut inside a curly quote", "Thanks.\n\n" + ourBody + "Nu vă interesează? Răspundeți cu „dezabonare\xe2\x80", false, notOptOut},
	{"a cut under 12 characters stays", "Thanks.\n\n" + ourBody + "Not relev", false, notOptOut},

	// HTML.
	{"HTML gmail_quote", `<div dir="ltr">Thanks</div><br><div class="gmail_quote"><div>` + attrEN + `</div><blockquote>Hello Ana,<br>` +
		`Not relevant? Reply &quot;unsubscribe&quot; and I won&#39;t write again.</blockquote></div>`, true, notOptOut},
	{"HTML own word before a blockquote", `<p>unsubscribe</p><blockquote>Hello</blockquote>`, false, realOptOut},

	// Real opt-outs that must stay.
	{"top-posted unsubscribe", "Unsubscribe please.\n\n" + attrEN + "\n" + quote(ourBody+enLine, 1), false, realOptOut},
	{"bottom-posted unsubscribe", attrEN + "\n" + quote(ourBody+enLine, 1) + "\nunsubscribe\n", false, realOptOut},
	{"Bitte abmelden", "Bitte abmelden.\n\n" + attrDE + "\n" + quote(ourBody, 1), false, realOptOut},
	{"Graag afmelden", "Graag afmelden", false, realOptOut},
	{"remove me over a quote", "Please remove me\n\n" + attrEN + "\n" + quote(ourBody, 1), false, realOptOut},
	{"dezabonare alone", "dezabonare", false, realOptOut},
	{"dezabonați", "Vă rog să mă dezabonați", false, realOptOut},
	{"own copy of our line plus a yes", enLine + "\nyes, unsubscribe", false, realOptOut},

	// FO1 additions.
	{"just Stop.", "Stop.", false, realOptOut},
	{"Stop. over a quote", "Stop.\n\n" + attrEN + "\n" + quote(ourBody, 1), false, realOptOut},
	{"Stop. over a wrapped attribution", "Stop.\n\nOn Mon, 26 Oct 2026 at 18:10, Sam\nwrote:\n" + quote(ourBody, 1), false, realOptOut},
	{"dezabonați-mă", "Vă rog, dezabonați-mă", false, realOptOut},
	{"dezabonez", "dezabonez", false, realOptOut},
	{"dezabonează", "Vă rog să mă dezabonează cineva", false, realOptOut},
	{"dezabonati, no diacritics", "va rog sa ma dezabonati", false, realOptOut},
	{"Bitte austragen", "Bitte austragen", false, realOptOut},
	{"Abbestellen", "Abbestellen", false, realOptOut},
	{"please opt out", "please opt out", false, realOptOut},
	{"opt-out", "opt-out", false, realOptOut},
	{"stop inside a sentence", "We will stop using Drupal 7", false, notOptOut},
	{"a > b is the sender's", "a > b, so please unsubscribe", false, realOptOut},
}

// ownVerdict is the FO1 decision on OwnText's result: a word in own or tail, or own just
// "stop".
func ownVerdict(text string, html bool, lines []string) bool {
	own, tail := OwnText([]byte(text), html, lines)
	return HasOptOutWord(string(own)) || HasOptOutWord(string(tail)) || isJustStop(own)
}

func TestOwnTextFixtures(t *testing.T) {
	for _, c := range ownTextFixtures {
		t.Run(c.name, func(t *testing.T) {
			if got := ownVerdict(c.text, c.html, testOwnLines); got != (c.want == realOptOut) {
				own, tail := OwnText([]byte(c.text), c.html, testOwnLines)
				t.Fatalf("want %s, got opt-out=%v\nown: %q\ntail: %q", c.want, got, own, tail)
			}
		})
	}
}

func TestOwnTextQuoteStrippingAlone(t *testing.T) {
	// With no lines of ours, quote stripping alone still clears the quoted shapes.
	for _, text := range []string{
		"Got it.\n\n" + attrEN + "\n" + quote(oldSelftest, 1),
		"Got it.\n\n" + attrRO + "\n" + quote(ourBody+roLine, 2),
	} {
		if ownVerdict(text, false, nil) {
			t.Fatalf("quoted reply read as an opt-out: %q", text)
		}
	}
}

func TestOwnTextKeepsStructure(t *testing.T) {
	own, tail := OwnText([]byte("Hi,\r\nsee below.\r\n\r\n"+outlookHeader+"Body\r\n"), false, nil)
	if string(own) != "Hi,\nsee below.\n" {
		t.Fatalf("own: %q", own)
	}
	if !strings.HasPrefix(string(tail), "From: Sam") || strings.Contains(string(tail), "\r") {
		t.Fatalf("tail: %q", tail)
	}
	// Our line goes as one space, never gluing its neighbours.
	own, _ = OwnText([]byte("un"+enLine+"subscribe"), false, []string{enLine})
	if string(own) != "un subscribe" {
		t.Fatalf("glued: %q", own)
	}
}

func TestNormOptOut(t *testing.T) {
	got := normOptOut("  Nu vă INTERESEAZĂ?\n Răspundeți cu „dezabonare”  «x» 'y' \"z\" ‘w’ “v”  ")
	if want := "nu vă interesează? răspundeți cu dezabonare x y z w v"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// ownCase is one generated reply: the text, whether it is HTML, and whether the sender's
// own part holds an opt-out word (the property's expected answer, known by construction).
type ownCase struct {
	text  string
	html  bool
	optIn bool
	shape string
}

var (
	propPlain = []string{"Thanks, sounds interesting.", "Can you call me on Tuesday?", "We will stop using Drupal 7 next year.",
		"Mulțumesc, revin.", "On Tuesday works for me.", "In principle yes.", "Le budget est serré."}
	propWord = []string{"Please unsubscribe me.", "Dezabonare, vă rog.", "Bitte abmelden.", "remove me please",
		"please opt out", "On second thought, unsubscribe me.", "In any case, please unsubscribe.", "Vă rog să mă dezabonați."}
	propAttr = []string{attrEN, attrRO, attrDE, attrNL, attrFR}
)

// ourLineVariant renders one of our lines straight, with curly quotes, wrapped or cut.
func ourLineVariant(r *rand.Rand, cut bool) string {
	l := []string{enLine, roLine, SelftestLine}[r.IntN(3)]
	switch r.IntN(3) {
	case 1:
		l = strings.NewReplacer(`"`, "“", "'", "’", "„", `"`, "”", `"`).Replace(l)
	case 2:
		if i := strings.LastIndex(l[:len(l)/2], " "); i > 0 {
			l = l[:i] + "\n" + l[i+1:]
		}
	}
	if cut {
		l = l[:12+r.IntN(len(l)-12)]
	}
	return l
}

// genOwnCase builds a reply from parts (FO5): an own line with or without a word, at the
// top, the bottom or inline under our quoted line, in one of the FO2 quote shapes.
func genOwnCase(r *rand.Rand) ownCase {
	optIn := r.IntN(2) == 0
	own := propPlain[r.IntN(len(propPlain))]
	if optIn {
		own = propWord[r.IntN(len(propWord))]
	}
	pos := []string{"top", "bottom", "inline"}[r.IntN(3)]
	cut := pos == "top" && r.IntN(4) == 0
	ours := ourBody + ourLineVariant(r, cut)
	attr := propAttr[r.IntN(len(propAttr))]
	if r.IntN(4) == 0 { // a wrapped attribution
		if i := strings.LastIndex(attr, " "); i > 0 {
			attr = attr[:i] + "\n" + attr[i+1:]
		}
	}
	shape := []string{"gmail", "outlook", "outlook-ro", "original", "rule", "html", "unmarked"}[r.IntN(7)]
	var quoted string
	switch shape {
	case "gmail":
		depth := 1 + r.IntN(3)
		if pos == "inline" {
			quoted = attr + "\n" + quote(ours, depth) + own + "\n" + quote("Sam", depth)
		} else {
			quoted = attr + "\n" + quote(ours, depth)
		}
	case "outlook", "outlook-ro", "original", "rule":
		hdr := map[string]string{"outlook": outlookHeader, "outlook-ro": outlookRO,
			"original": "-----Original Message-----\nFrom: Sam\nSent: Monday\n\n",
			"rule":     "________________________________\n" + outlookHeader}[shape]
		quoted = hdr + ours
		if pos == "inline" {
			quoted += "\n\n" + own + "\n"
		}
	case "unmarked":
		quoted = ours
		if pos == "inline" {
			quoted += "\n\n" + own + "\n"
		}
	case "html":
		h := `<div class="gmail_quote"><div>` + attr + `</div><blockquote>` +
			strings.ReplaceAll(strings.NewReplacer(`"`, "&quot;", "'", "&#39;").Replace(ours), "\n", "<br>")
		if pos == "inline" {
			h += "<br>" + own
		}
		h += "</blockquote></div>"
		switch pos {
		case "top":
			return ownCase{`<div dir="ltr">` + own + `</div>` + h, true, optIn, shape + "/" + pos}
		case "bottom":
			return ownCase{h + `<div>` + own + `</div>`, true, optIn, shape + "/" + pos}
		}
		return ownCase{`<div dir="ltr">Hi</div>` + h, true, optIn, shape + "/" + pos}
	}
	switch pos {
	case "top":
		return ownCase{own + "\n\n" + quoted, false, optIn, shape + "/" + pos}
	case "bottom":
		return ownCase{quoted + "\n" + own + "\n", false, optIn, shape + "/" + pos}
	}
	return ownCase{"Hi.\n\n" + quoted, false, optIn, shape + "/" + pos}
}

// checkOwnProperty runs n generated cases through verdict and returns the first that
// breaks the property (verdict is an opt-out exactly when the own part holds a word).
func checkOwnProperty(seed uint64, n int, verdict func(text string, html bool) bool) (ownCase, bool) {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) // #nosec G404 -- seeded on purpose: a failing case must replay
	for range n {
		c := genOwnCase(r)
		if verdict(c.text, c.html) != c.optIn {
			return c, false
		}
	}
	return ownCase{}, true
}

// TestOwnTextProperty (FO5): never miss an opt-out in the sender's own words, never read
// one from our quoted text.
func TestOwnTextProperty(t *testing.T) {
	const seed = 20261026
	if c, ok := checkOwnProperty(seed, 5000, func(text string, html bool) bool { return ownVerdict(text, html, testOwnLines) }); !ok {
		own, tail := OwnText([]byte(c.text), c.html, testOwnLines)
		t.Fatalf("seed %d: %s: want opt-out=%v\ntext: %q\nown: %q\ntail: %q", seed, c.shape, c.optIn, c.text, own, tail)
	}
	// The same property through OptOutIn: exactly OptOutText when the own part holds a word.
	viaOptOutIn := func(text string, html bool) bool {
		ctype := "text/plain; charset=UTF-8"
		if html {
			ctype = "text/html; charset=UTF-8"
		}
		return OptOutIn(mail.Header{"Content-Type": {ctype}, "Subject": {"Re: Drupal 7"}}, []byte(text), testOwnLines) == OptOutText
	}
	if c, ok := checkOwnProperty(seed, 5000, viaOptOutIn); !ok {
		t.Fatalf("seed %d via OptOutIn: %s: want opt-out=%v\ntext: %q", seed, c.shape, c.optIn, c.text)
	}
}

// greedyOwnText is a deliberately over-greedy stripper: everything after the first
// attribution or quote header goes. The property must catch it.
func greedyOwnText(text string, html bool) bool {
	lines := strings.Split(text, "\n")
	var keep []string
	for _, l := range lines {
		if attributionRe.MatchString(l) || quoteFromRe.MatchString(l) || strings.Contains(l, "gmail_quote") ||
			strings.HasPrefix(l, "-----") || strings.HasPrefix(l, "____") || attributionStart.MatchString(l) && !HasOptOutWord(l) {
			break
		}
		keep = append(keep, l)
	}
	own, _ := OwnText([]byte(strings.Join(keep, "\n")), html, testOwnLines)
	return HasOptOutWord(string(own))
}

func TestOwnTextPropertyCatchesAGreedyStripper(t *testing.T) {
	if c, ok := checkOwnProperty(20261026, 5000, greedyOwnText); ok {
		t.Fatal("the property passed an over-greedy stripper: it cannot catch a missed opt-out")
	} else if !c.optIn {
		t.Fatalf("the greedy stripper should fail by missing an opt-out, failed on: %s %q", c.shape, c.text)
	}
}
