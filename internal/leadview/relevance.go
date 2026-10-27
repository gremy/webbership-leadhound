package leadview

import (
	"strconv"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// RelevanceWords is a lead's latest relevance rating in plain words: its score (0 to 100)
// and band, "72, likely match"; a nil score is "not rated yet". While the rating has not
// passed the trust gate (trusted false) the words end in "early number":
// "72, likely match, early number". An empty or unknown band leaves just the number.
func RelevanceWords(score *int, band string, trusted bool) string {
	if score == nil {
		return "not rated yet"
	}
	s := strconv.Itoa(*score)
	if w := bandWords(band); w != "" {
		s += ", " + w
	}
	if !trusted {
		s += ", early number"
	}
	return s
}

// bandWords is a relevance band in words, or "" for one it does not know.
func bandWords(band string) string {
	switch band {
	case leadstore.BandStrong:
		return "strong match"
	case leadstore.BandLikely:
		return "likely match"
	case leadstore.BandMaybe:
		return "maybe a match"
	case leadstore.BandUnlikely:
		return "unlikely match"
	case leadstore.BandNo:
		return "no match"
	}
	return ""
}
