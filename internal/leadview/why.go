package leadview

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// WhyLines words each reason a lead scored (leadstore.ScoreReasons) as one line with its
// points, in the order given:
//
//	Lead type, out-of-support Drupal: +20
//	Runs Drupal 10: +30
//	Exact version known, 7.98: +5
//	Country you want, GB: +10
//	Country you rank lower, NL: -10
//	Has an email address: +5
//	Big site, list rank under 10,000: +15
func WhyLines(reasons []leadstore.Reason) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, fmt.Sprintf("%s: %+d", whyWords(r), r.Points))
	}
	return out
}

// whyWords is the plain words for one reason, without its points.
func whyWords(r leadstore.Reason) string {
	switch r.Kind {
	case leadstore.ReasonKind:
		return "Lead type, " + kindWords(r.Label)
	case leadstore.ReasonMajor:
		return "Runs Drupal " + r.Label
	case leadstore.ReasonExactVersion:
		return "Exact version known, " + r.Label
	case leadstore.ReasonCountry:
		if r.Points < 0 {
			return "Country you rank lower, " + r.Label
		}
		return "Country you want, " + r.Label
	case leadstore.ReasonChannel:
		switch r.Label {
		case leadstore.ChannelEmail:
			return "Has an email address"
		case leadstore.ChannelForm:
			return "Has a contact form"
		}
		return "Reached by " + r.Label
	case leadstore.ReasonRankBand:
		if n, err := strconv.Atoi(r.Label); err == nil {
			return "Big site, list rank under " + groupDigits(n)
		}
		return "Big site, list rank under " + r.Label
	}
	return strings.ReplaceAll(r.Kind+" "+r.Label, "_", " ")
}

// kindWords is a lead kind in words.
func kindWords(kind string) string {
	switch kind {
	case leadstore.LeadUnsupported:
		return "out-of-support Drupal"
	case leadstore.LeadD10:
		return "Drupal 10 site"
	case leadstore.LeadAgencyPartner:
		return "agency that sells Drupal upgrades"
	}
	return strings.ReplaceAll(kind, "_", " ")
}

// groupDigits writes n with a comma between each group of three digits: 10,000.
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
