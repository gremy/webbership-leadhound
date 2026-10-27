package leadrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// Input is everything the relevance question reads about one lead. It holds no email
// address and no phone number by design: only whether a phone exists and which channels
// are present. Free text (site name, description) may still hold contact-shaped text,
// which State removes.
type Input struct {
	Kind        string    // a leadstore Lead* kind
	Major       int       // Drupal major version; 0 when unknown
	Version     string    // the exact version, e.g. "7.98"; "" when unknown
	EOL         time.Time // the major version's end-of-life date; zero when unknown
	Country     string    // ISO code, e.g. "GB"; "" when unknown
	ListRank    int       // rank in the top-sites list; 0 when not listed
	SiteName    string
	Description string
	Channels    []string // the channels present, e.g. email, form
	HasPhone    bool
	SocialLinks int // how many social links the site has
}

// The longest site name and description State keeps, in runes.
const (
	MaxSiteName    = 200
	MaxDescription = 1000
)

// DateLayout is how a golden line writes the end-of-life date.
const DateLayout = "2006-01-02"

// State is the decider's whole view of a lead: a fixed English layout, one fact per line,
// every line always present. Dates are written as dates ("9 December 2026"), never
// relative to today, so the state of an unchanged lead does not change as time passes.
// Email- and phone-shaped text is removed from every free-text field, and channel names
// are sorted, so the same facts always give the same state.
func State(in Input) string {
	var b strings.Builder
	line := func(label, value string) {
		b.WriteString(label)
		b.WriteString(": ")
		b.WriteString(value)
		b.WriteByte('\n')
	}
	line("Lead type", kindWords(in.Kind))
	line("Drupal major version", positiveOr(in.Major, "unknown"))
	line("Exact Drupal version", orUnknown(clean(in.Version, 40)))
	eol := "unknown"
	if !in.EOL.IsZero() {
		eol, _ = outreach.FormatDate(in.EOL, outreach.LangEnglish) // English always has wording
	}
	line("End-of-life date of that major version", eol)
	line("Country", orUnknown(clean(in.Country, 40)))
	line("Top-sites list rank", positiveOr(in.ListRank, "not in the list"))
	line("Site name", orUnknown(clean(in.SiteName, MaxSiteName)))
	line("Description", orUnknown(clean(in.Description, MaxDescription)))
	line("Contact channels", channelWords(in.Channels))
	line("Phone number on the site", yesNo(in.HasPhone))
	line("Social links", strconv.Itoa(max(in.SocialLinks, 0)))
	return strings.TrimSuffix(b.String(), "\n")
}

// kindWords is a lead kind in words.
func kindWords(kind string) string {
	switch kind {
	case leadstore.LeadUnsupported:
		return "a site on an out-of-support Drupal version"
	case leadstore.LeadD10:
		return "a Drupal 10 site"
	case leadstore.LeadAgencyPartner:
		return "an agency that sells Drupal upgrades"
	case leadstore.LeadHand:
		return "a site added by hand"
	}
	return orUnknown(clean(strings.ReplaceAll(kind, "_", " "), 40))
}

// channelWords lists the channels present, sorted and without repeats, or "none".
func channelWords(chs []string) string {
	var out []string
	for _, c := range chs {
		c = clean(c, 40)
		if c == "" || c == leadstore.ChannelNone || slices.Contains(out, c) {
			continue
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return "none"
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

func positiveOr(n int, none string) string {
	if n <= 0 {
		return none
	}
	return strconv.Itoa(n)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

var (
	// emailLike is any run with something on both sides of an @, so an address cut short
	// or wrapped in punctuation goes too.
	emailLike = regexp.MustCompile(`[^\s@]+@[^\s@]+`)
	// obfuscatedEmail is "name [at] domain [dot] tld" and "name at domain dot tld".
	obfuscatedEmail = regexp.MustCompile(`(?i)[\w.+-]+\s*[\[({]\s*at\s*[\])}]\s*[\w-]+(?:\s*[\[({]\s*dot\s*[\])}]\s*[\w-]+)+` +
		`|(?i)\b[\w.+-]+\s+at\s+[\w-]+(?:\s+dot\s+[\w-]+)+\b`)
	// phoneLike is eight or more digits with only phone separators between them (D7 keeps
	// numbers of 8 to 15 digits); a longer run goes whole, so no tail of a number survives.
	phoneLike = regexp.MustCompile(`\+?\(?\d(?:[\s().\-/]*\d){7,}`)
	spaces    = regexp.MustCompile(`\s+`)
)

// clean removes email- and phone-shaped text, folds every run of whitespace (newlines
// included) to one space, and cuts the result to maxRunes. The contacts go before the cut,
// so the cut can never leave part of one behind.
func clean(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "")
	s = emailLike.ReplaceAllString(s, " ")
	s = obfuscatedEmail.ReplaceAllString(s, " ")
	s = phoneLike.ReplaceAllString(s, " ")
	s = strings.TrimSpace(spaces.ReplaceAllString(s, " "))
	if utf8.RuneCountInString(s) > maxRunes {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:maxRunes])) + "…"
	}
	return s
}

// inputJSON is Input as a golden line writes it, with the date as a plain date.
type inputJSON struct {
	Kind        string   `json:"kind"`
	Major       int      `json:"major"`
	Version     string   `json:"version"`
	EOL         string   `json:"eol"` // DateLayout, or "" when unknown
	Country     string   `json:"country"`
	ListRank    int      `json:"list_rank"`
	SiteName    string   `json:"site_name"`
	Description string   `json:"description"`
	Channels    []string `json:"channels"`
	HasPhone    bool     `json:"has_phone"`
	SocialLinks int      `json:"social_links"`
}

// MarshalJSON writes in as a golden line's "input", the date as 2006-01-02.
func (in Input) MarshalJSON() ([]byte, error) {
	j := inputJSON{Kind: in.Kind, Major: in.Major, Version: in.Version, Country: in.Country,
		ListRank: in.ListRank, SiteName: in.SiteName, Description: in.Description,
		Channels: in.Channels, HasPhone: in.HasPhone, SocialLinks: in.SocialLinks}
	if !in.EOL.IsZero() {
		j.EOL = in.EOL.Format(DateLayout)
	}
	return json.Marshal(j)
}

// UnmarshalJSON reads a golden line's "input" strictly: an unknown field, a date not
// written as 2006-01-02 or a negative count is an error.
func (in *Input) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var j inputJSON
	if err := dec.Decode(&j); err != nil {
		return fmt.Errorf("input: %w", err)
	}
	if j.Major < 0 || j.ListRank < 0 || j.SocialLinks < 0 {
		return errors.New("input: major, list_rank and social_links must not be negative")
	}
	var eol time.Time
	if j.EOL != "" {
		t, err := time.Parse(DateLayout, j.EOL)
		if err != nil {
			return fmt.Errorf("input: eol %q is not a date written as %s", j.EOL, DateLayout)
		}
		eol = t
	}
	*in = Input{Kind: j.Kind, Major: j.Major, Version: j.Version, EOL: eol, Country: j.Country,
		ListRank: j.ListRank, SiteName: j.SiteName, Description: j.Description,
		Channels: j.Channels, HasPhone: j.HasPhone, SocialLinks: j.SocialLinks}
	return nil
}
