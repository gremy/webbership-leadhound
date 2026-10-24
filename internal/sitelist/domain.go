// Package sitelist parses public site lists (Tranco, the Majestic Million, an HTTP Archive
// export) into one entry per registrable domain, and maps a domain to its country by TLD.
package sitelist

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// Entry is one registrable domain from a list. Rank is the list's rank (1 is best), or 0
// when the list has none.
type Entry struct {
	Domain string
	Rank   int
}

// Tally counts the rows a parse did not return: Bad rows could not be read (malformed,
// no usable domain), Filtered rows were readable but dropped by keep. Blank lines and
// duplicates count in neither.
type Tally struct {
	Bad      int
	Filtered int
}

// ErrNotRegistrable is returned by Registrable for a host that has no registrable domain.
var ErrNotRegistrable = errors.New("sitelist: not a registrable domain")

// Registrable returns the registrable domain (eTLD+1) of host, in lower-case ASCII
// (IDNA ToASCII), so www.northwind.example and northwind.example. both give
// northwind.example. An IP address, a host with a path, port or space, and a bare
// public suffix are errors.
func Registrable(host string) (string, error) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" || strings.ContainsAny(h, "/:@?# \t\r\n") {
		return "", fmt.Errorf("%w: %q", ErrNotRegistrable, host)
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return "", fmt.Errorf("%w: %q is an IP address", ErrNotRegistrable, host)
	}
	ascii, err := idna.Lookup.ToASCII(h)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", ErrNotRegistrable, host, err)
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(ascii)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", ErrNotRegistrable, host, err)
	}
	return d, nil
}

// countryByTLD maps a country-code top-level domain to its ISO 3166-1 alpha-2 code.
// It covers European ccTLDs; uk is the one whose code differs (GB).
var countryByTLD = map[string]string{
	"uk": "GB", "ie": "IE", "nl": "NL", "be": "BE", "lu": "LU", "fr": "FR",
	"de": "DE", "at": "AT", "ch": "CH", "li": "LI", "it": "IT", "es": "ES",
	"pt": "PT", "ro": "RO", "pl": "PL", "cz": "CZ", "sk": "SK", "hu": "HU",
	"si": "SI", "hr": "HR", "bg": "BG", "gr": "GR", "cy": "CY", "mt": "MT",
	"se": "SE", "dk": "DK", "no": "NO", "fi": "FI", "is": "IS", "ee": "EE",
	"lv": "LV", "lt": "LT", "rs": "RS", "md": "MD", "ua": "UA",
}

// CountryOf returns the ISO alpha-2 country of domain's last label (uk gives GB), or ""
// when that label is not in the table.
func CountryOf(domain string) string {
	return countryByTLD[lastLabel(domain)]
}

// KeepTLDs returns a keep filter for the parsers: it keeps a domain whose last label is
// in tlds (case and a leading dot ignored). An entry with a dot, such as co.uk, keeps
// domains ending in it. An empty list keeps nothing.
func KeepTLDs(tlds []string) func(string) bool {
	suffixes := make([]string, 0, len(tlds))
	for _, t := range tlds {
		if t = strings.Trim(strings.ToLower(strings.TrimSpace(t)), "."); t != "" {
			suffixes = append(suffixes, "."+t)
		}
	}
	return func(domain string) bool {
		d := "." + strings.ToLower(domain)
		for _, s := range suffixes {
			if strings.HasSuffix(d, s) {
				return true
			}
		}
		return false
	}
}

func lastLabel(domain string) string {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	return d[strings.LastIndexByte(d, '.')+1:]
}

// collector dedupes entries by domain, keeping the best (lowest non-zero) rank.
type collector struct {
	keep  func(string) bool
	index map[string]int
	out   []Entry
	tally Tally
}

func newCollector(keep func(string) bool) *collector {
	return &collector{keep: keep, index: map[string]int{}}
}

// add normalises host and records it with rank; a host with no registrable domain is
// counted bad.
func (c *collector) add(host string, rank int) {
	d, err := Registrable(host)
	if err != nil {
		c.tally.Bad++
		return
	}
	if c.keep != nil && !c.keep(d) {
		c.tally.Filtered++
		return
	}
	if i, ok := c.index[d]; ok {
		if rank > 0 && (c.out[i].Rank == 0 || rank < c.out[i].Rank) {
			c.out[i].Rank = rank
		}
		return
	}
	c.index[d] = len(c.out)
	c.out = append(c.out, Entry{Domain: d, Rank: rank})
}

// entries returns the result sorted by rank (0 last), then domain.
func (c *collector) entries() []Entry {
	sort.Slice(c.out, func(i, j int) bool {
		a, b := c.out[i], c.out[j]
		if a.Rank != b.Rank {
			if a.Rank == 0 || b.Rank == 0 {
				return b.Rank == 0
			}
			return a.Rank < b.Rank
		}
		return a.Domain < b.Domain
	})
	return c.out
}
