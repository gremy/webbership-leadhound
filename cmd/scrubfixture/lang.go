package main

import (
	"strings"
	"sync"

	"github.com/pemistahl/lingua-go"
)

// candidateLanguages are the languages detection chooses among: the four the phrase table
// covers plus the ones job ads in this market are most often confused with. Latin and
// Italian are included so filler text that drifts towards lorem ipsum is caught.
var candidateLanguages = []lingua.Language{
	lingua.English, lingua.French, lingua.Spanish, lingua.Dutch,
	lingua.German, lingua.Italian, lingua.Portuguese, lingua.Romanian, lingua.Polish, lingua.Latin,
}

var (
	detectorOnce sync.Once
	detector     lingua.LanguageDetector
)

// detectLanguage returns lingua's ISO 639-1 code for text ("" when it cannot tell).
func detectLanguage(text string) string {
	detectorOnce.Do(func() {
		detector = lingua.NewLanguageDetectorBuilder().FromLanguages(candidateLanguages...).Build()
	})
	l, ok := detector.DetectLanguageOf(text)
	if !ok {
		return ""
	}
	return strings.ToLower(l.IsoCode639_1().String())
}

// detectPhraseLang maps detection onto a phrase-table language; ok is false for any other
// language, and the caller falls back to English.
func detectPhraseLang(text string) (Lang, bool) {
	switch Lang(detectLanguage(text)) {
	case LangEnglish:
		return LangEnglish, true
	case LangFrench:
		return LangFrench, true
	case LangSpanish:
		return LangSpanish, true
	case LangDutch:
		return LangDutch, true
	}
	return "", false
}
