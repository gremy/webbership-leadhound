package igsignal

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/enrich"
)

// Where Vendor saw a loyalty vendor.
const (
	VendorViaLink = "link" // the profile's external link
	VendorViaBio  = "bio"  // a web address written in the bio
	VendorViaTag  = "tag"  // an @-tag in the bio or a caption
	VendorViaWord = "word" // a vendor's ig_words beside a loyalty cue in the bio or a caption
)

var (
	// bioURLRe finds a web address in free text, with or without its scheme, not inside an
	// @-tag or an email address.
	bioURLRe = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}@._-])((?:https?://)?(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}(?:/[^\s]*)?)`)
	// tagRe finds an @-tag: an @ not inside a word or an email address, then a handle.
	tagRe = regexp.MustCompile(`(?:^|[^\p{L}\p{N}._@])@([A-Za-z0-9._]{1,30})`)
)

// Vendor is the loyalty vendor a profile shows by rule, from signals.yaml's loyalty vendors
// in cfg (nil: cafe.DefaultConfig; pass the copy WithOwnVendor made to see the operator's own
// product): its external link, then each web address in its bio, then each @-tag in its bio
// and captions, each tested against every vendor's hosts and Instagram account, then each
// vendor's ig_words in a bio or caption that also holds a loyalty cue (wordVendors). The first
// vendor seen is the answer, except that the own vendor anywhere wins (the café is a
// customer). ok is false when none is seen. A rule vendor outranks Jev's ig_vendor answer.
func Vendor(p Profile, cfg *cafe.Config) (id, via string, ok bool) {
	if cfg == nil {
		cfg = cafe.DefaultConfig()
	}
	type hit struct {
		id, via string
		own     bool
	}
	var hits []hit
	see := func(link, via string) {
		for _, v := range cfg.VendorsAt(link) {
			if v.Kind == cafe.SigLoyalty {
				hits = append(hits, hit{v.ID, via, v.Own})
			}
		}
	}
	if p.ExternalURL != "" {
		see(p.ExternalURL, VendorViaLink)
	}
	for _, m := range bioURLRe.FindAllStringSubmatch(p.Bio, -1) {
		see(webAddress(m[1]), VendorViaBio)
	}
	for _, text := range append([]string{p.Bio}, p.Captions...) {
		for _, m := range tagRe.FindAllStringSubmatch(text, -1) {
			tag := strings.TrimRight(strings.ToLower(m[1]), ".")
			if u := enrich.InstagramURL(tag); u != "" {
				see(u, VendorViaTag) // the vendor's own account (instagram.com/<handle> in signals.yaml)
			}
			if strings.Contains(tag, ".") {
				see("https://"+tag+"/", VendorViaTag) // a tag spelled as the vendor's host (@cardora.ro)
			}
		}
	}
	for _, text := range append([]string{p.Bio}, p.Captions...) {
		for _, id := range wordVendors(text, cfg) {
			v := cfg.Vendor(id)
			hits = append(hits, hit{v.ID, VendorViaWord, v.Own})
		}
	}
	for _, h := range hits {
		if h.own {
			return h.id, h.via, true // nothing outranks the own vendor
		}
	}
	if len(hits) == 0 {
		return "", "", false
	}
	return hits[0].id, hits[0].via, true
}

// webAddress is an address from free text as a link: closing punctuation dropped, https
// added when it has no scheme.
func webAddress(s string) string {
	s = strings.TrimRight(s, ".,;:!?)]}'\"")
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return s
}

// wordVendors are the loyalty vendors one text names by their ig_words: a word counts only
// as a whole word, case-sensitive ("PICK", never "pick up"), and only when the same text also
// holds a word that starts with one of instagram.loyalty_cues (folded: "Partener", "recompensă").
func wordVendors(text string, cfg *cafe.Config) []string {
	raw := words(text)
	if len(raw) == 0 || !hasCue(text, cfg.Instagram.LoyaltyCues) {
		return nil
	}
	var ids []string
	for _, v := range cfg.Vendors {
		if v.Kind != cafe.SigLoyalty {
			continue
		}
		for _, w := range v.IGWords {
			if slices.Contains(raw, w) {
				ids = append(ids, v.ID)
				break
			}
		}
	}
	return ids
}

// hasCue reports whether a word of the folded text starts with one of cues.
func hasCue(text string, cues []string) bool {
	for _, w := range words(fold(text)) {
		for _, c := range cues {
			if strings.HasPrefix(w, c) {
				return true
			}
		}
	}
	return false
}

// Prepaid reports whether a profile shows a prepaid offer: a whole word of
// instagram.prepaid_words in its bio or a caption, accents dropped and lower case
// (cfg nil: cafe.DefaultConfig).
func Prepaid(p Profile, cfg *cafe.Config) bool {
	if cfg == nil {
		cfg = cafe.DefaultConfig()
	}
	for _, text := range append([]string{p.Bio}, p.Captions...) {
		if HasWord(text, cfg.Instagram.PrepaidWords) {
			return true
		}
	}
	return false
}

// HasWord reports whether text holds one of list as a whole word, both sides read with every
// accent mark dropped and lower case ("Cât costă?" holds "cat" and "costa"). Words are runs of
// letters and digits. The list's words are expected folded already (signals.yaml's
// instagram lists are lower-case ASCII).
func HasWord(text string, list []string) bool {
	for _, w := range words(fold(text)) {
		if slices.Contains(list, w) {
			return true
		}
	}
	return false
}

// words are text's runs of letters and digits, as written.
func words(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
