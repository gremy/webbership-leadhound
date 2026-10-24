// Package agencies reads the private agency list, agencies.yaml in LEADHOUND_DATA_DIR,
// and finds the phrase on an agency's evidence page that matches the finding pattern.
// The repository ships a fictional list in examples/agencies.demo.yaml.
package agencies

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	"golang.org/x/net/html"

	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// FileName is the list's name inside the data directory.
const FileName = "agencies.yaml"

// PhraseRunes is the most runes FindingPhrase returns, the match included.
const PhraseRunes = 120

// Agency is one row of the list. Domain is not read from the file: LoadFile sets it to
// the registrable domain of Website.
type Agency struct {
	Name          string `yaml:"name"`
	Website       string `yaml:"website"`
	Country       string `yaml:"country"` // ISO 3166-1 alpha-2, or "" when not known
	EvidenceURL   string `yaml:"evidence_url"`
	DrupalOrgSlug string `yaml:"drupalorg_slug"`
	Domain        string `yaml:"-"`
}

var countryRe = regexp.MustCompile(`^[A-Z]{2}$`)

// LoadFile reads the list at path: a YAML sequence of agencies, unknown keys refused.
// A row without a website is counted in skipped and not returned. problems lists every
// problem in the file at once; when it is not empty, agencies is nil.
func LoadFile(path string) (agencies []Agency, skipped int, problems []string) {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator's own list file
	if err != nil {
		return nil, 0, []string{err.Error()}
	}
	var rows []Agency
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&rows); err != nil {
		var te *yaml.TypeError
		switch {
		case errors.Is(err, io.EOF):
			return nil, 0, []string{path + ": the file is empty"}
		case errors.As(err, &te):
			problems = append(problems, te.Errors...) // the rows still decoded; keep checking
		default:
			return nil, 0, []string{err.Error()}
		}
	}

	seen := map[string]string{} // domain → the row that first named it
	for i, a := range rows {
		label := fmt.Sprintf("row %d", i+1)
		if a.Name != "" {
			label += " (" + a.Name + ")"
		}
		bad := func(format string, args ...any) {
			problems = append(problems, label+": "+fmt.Sprintf(format, args...))
		}
		if strings.TrimSpace(a.Name) == "" {
			bad("name is missing")
		}
		if a.Country != "" && !countryRe.MatchString(a.Country) {
			bad("country %q is not a two-letter upper-case code", a.Country)
		}
		if a.EvidenceURL != "" {
			if _, err := webURL(a.EvidenceURL); err != nil {
				bad("evidence_url: %v", err)
			}
		}
		if strings.TrimSpace(a.Website) == "" {
			skipped++
			continue
		}
		u, err := webURL(a.Website)
		if err != nil {
			bad("website: %v", err)
			continue
		}
		d, err := sitelist.Registrable(u.Hostname())
		if err != nil {
			bad("website: %v", err)
			continue
		}
		if d == drupalOrg {
			bad("website is a drupal.org page; use the organisation's own site")
			continue
		}
		if first, dup := seen[d]; dup {
			bad("website %s has the same domain as %s", d, first)
			continue
		}
		seen[d] = label
		a.Domain = d
		agencies = append(agencies, a)
	}
	if len(problems) > 0 {
		return nil, skipped, problems
	}
	return agencies, skipped, nil
}

const drupalOrg = "drupal.org"

// IsDrupalOrg reports whether rawURL is an http or https URL whose host's registrable
// domain is drupal.org (drupal.org, www.drupal.org, new.drupal.org and the like).
func IsDrupalOrg(rawURL string) bool {
	u, err := webURL(rawURL)
	if err != nil {
		return false
	}
	d, err := sitelist.Registrable(u.Hostname())
	return err == nil && d == drupalOrg
}

// webURL parses an absolute http or https URL with a host.
func webURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%q is not an http or https URL", raw)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("%q has no host", raw)
	}
	return u, nil
}

// FindingPhrase returns the first match of re in the page's visible text (script, style,
// noscript and template contents left out, entities decoded, whitespace folded), with the
// text around it, at most PhraseRunes runes in all. A match longer than that is cut.
func FindingPhrase(body []byte, re *regexp.Regexp) (phrase string, ok bool) {
	if re == nil {
		return "", false
	}
	text := visibleText(body)
	loc := re.FindStringIndex(text)
	if loc == nil || loc[0] == loc[1] {
		return "", false
	}
	before := []rune(text[:loc[0]])
	match := []rune(text[loc[0]:loc[1]])
	after := []rune(text[loc[1]:])
	if len(match) >= PhraseRunes {
		return strings.TrimSpace(string(match[:PhraseRunes])), true
	}
	room := PhraseRunes - len(match)
	nb := min(room/2, len(before))
	na := min(room-nb, len(after))
	nb = min(room-na, len(before)) // give the unused room after back to before
	out := string(before[len(before)-nb:]) + string(match) + string(after[:na])
	return strings.TrimSpace(out), true
}

// hidden are the elements whose text a reader never sees.
var hidden = map[string]bool{"script": true, "style": true, "noscript": true, "template": true}

// visibleText is the page's text outside hidden elements, whitespace folded to single
// spaces. Invalid UTF-8 is replaced first, so the result is always valid.
func visibleText(body []byte) string {
	if !utf8.Valid(body) {
		body = bytes.ToValidUTF8(body, []byte("�"))
	}
	z := html.NewTokenizer(bytes.NewReader(body))
	depth := 0 // how many hidden elements are open
	var parts []string
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
		case html.StartTagToken:
			if name, _ := z.TagName(); hidden[string(name)] {
				depth++
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); hidden[string(name)] && depth > 0 {
				depth--
			}
		case html.TextToken:
			if depth == 0 {
				parts = append(parts, string(z.Text()))
			}
		}
	}
}
