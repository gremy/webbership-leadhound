package console

import (
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Contact research on the lead page (CP3b): the console shows what the research job did
// and asks it to look again; it never holds the Claude key and never calls Claude.

// researchAnchor is the "Contact research" line's id: "Look again" comes back to it.
const researchAnchor = "contact-research"

// researchLine is the latest attempt in plain words, times in loc: "Looking since …",
// "Found 1 email on …", "Found nothing on …", "Paused: …". "" when never researched.
func researchLine(r LeadResearch, loc *time.Location) string {
	at := func(layout string) string {
		if r.At == nil {
			return ""
		}
		return r.At.In(loc).Format(layout)
	}
	on := func() string {
		if d := at("2 Jan 2006"); d != "" {
			return " on " + d
		}
		return ""
	}
	switch r.Status {
	case ResearchQueued:
		if w := at("Mon 2 Jan 15:04"); w != "" {
			return "Asked " + w + ": waiting for the research job to look for " + wantPhrase(r.Want) + "."
		}
		return "Asked: waiting for the research job to look for " + wantPhrase(r.Want) + "."
	case ResearchLooking:
		if w := at("Mon 2 Jan 15:04"); w != "" {
			return "Looking for " + wantPhrase(r.Want) + " since " + w + "."
		}
		return "Looking for " + wantPhrase(r.Want) + " now."
	case ResearchFound:
		return "Found " + foundWords(r.Verified, r.Want) + on() + "."
	case ResearchNothing:
		return "Found nothing" + on() + "."
	case ResearchCapped:
		return "Paused" + on() + ": today's spend cap was reached before it looked."
	case ResearchError:
		return "Stopped by an error" + on() + "."
	}
	return ""
}

// wantPhrase is what an attempt looks for, after "look for": "an email", "a business
// phone", "an email or a business phone".
func wantPhrase(want []string) string {
	var w []string
	for _, x := range want {
		switch x {
		case WantEmail:
			w = append(w, "an email")
		case WantPhone:
			w = append(w, "a business phone")
		}
	}
	if len(w) == 0 {
		return "a contact"
	}
	return strings.Join(w, " or ")
}

// foundWords is n contacts of what was wanted: "1 email", "2 business phones", "3 contacts".
func foundWords(n int, want []string) string {
	one, many := "contact", "contacts"
	if len(want) == 1 {
		switch want[0] {
		case WantEmail:
			one, many = "email", "emails"
		case WantPhone:
			one, many = "business phone", "business phones"
		}
	}
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// noticeText is an outcome line split for the page: its first line, then any list the
// backend put under it one item per line ("1. …", numbered by the page itself).
type noticeText struct {
	Head  string
	Items []string
}

// noticeLines splits msg at its line breaks: the first line is the head, every other
// non-empty line an item with its leading "N. " dropped (the page numbers the list).
func noticeLines(msg string) noticeText {
	parts := strings.Split(msg, "\n")
	out := noticeText{Head: parts[0]}
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i := strings.Index(p, ". "); i > 0 && strings.IndexFunc(p[:i], func(r rune) bool { return !unicode.IsDigit(r) }) < 0 {
			p = p[i+2:]
		}
		out.Items = append(out.Items, p)
	}
	return out
}
