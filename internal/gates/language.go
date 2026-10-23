package gates

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/pemistahl/lingua-go"
)

// minConfidence is the detector confidence below which a posting's language is unclear.
const minConfidence = 0.6

// maxDetectRunes caps the text the detector reads; a posting's opening is enough, and
// high-accuracy detection grows with length.
const maxDetectRunes = 3000

var (
	detectorOnce sync.Once
	detector     lingua.LanguageDetector
)

// buildDetector builds the detector for the languages postings arrive in, in
// high-accuracy mode.
func buildDetector() lingua.LanguageDetector {
	return lingua.NewLanguageDetectorBuilder().
		FromLanguages(lingua.English, lingua.French, lingua.Spanish, lingua.German, lingua.Dutch,
			lingua.Romanian, lingua.Italian, lingua.Portuguese, lingua.Polish).
		Build()
}

func getDetector() lingua.LanguageDetector {
	detectorOnce.Do(func() { detector = buildDetector() })
	return detector
}

// DetectLanguage returns the most likely language of text as a lower-case ISO 639-1
// code with its confidence (0–1). Blank text gives "", 0.
func DetectLanguage(text string) (iso string, confidence float64) {
	lang, conf := detect(text)
	if conf == 0 {
		return "", 0
	}
	return strings.ToLower(lang.IsoCode639_1().String()), conf
}

func detect(text string) (lingua.Language, float64) {
	text = truncate(squash(text), maxDetectRunes)
	if text == "" {
		return lingua.Unknown, 0
	}
	vals := getDetector().ComputeLanguageConfidenceValues(text)
	if len(vals) == 0 || vals[0].Value() == 0 {
		return lingua.Unknown, 0
	}
	return vals[0].Language(), vals[0].Value()
}

// languageAsk matches an explicit demand for a European language other than English:
// "fluent German", "C1 Dutch", "business-level French", "Spanish is required", "Italian
// is a must".
var languageAsk = regexp.MustCompile(`(?i)\b(?:(?:fluent|fluency|native|C1|C2|business[- ]level)\s+(?:in\s+)?(German|French|Dutch|Spanish|Italian|Polish|Portuguese)\b|(German|French|Dutch|Spanish|Italian|Polish|Portuguese)\s+(?:is\s+)?(?:required|a\s+must)\b)`)

var languageISO = map[string]string{
	"german": "de", "french": "fr", "dutch": "nl", "spanish": "es",
	"italian": "it", "polish": "pl", "portuguese": "pt",
}

func evalLanguage(obs Observation, c CandidateGates) Result {
	text := obs.Description
	if strings.TrimSpace(text) == "" {
		text = obs.Title
	}
	var (
		lang lingua.Language
		conf float64
	)
	if obs.Lang != "" {
		lang = lingua.GetLanguageFromIsoCode639_1(lingua.GetIsoCode639_1FromValue(obs.Lang))
		conf = obs.LangConfidence
		if lang == lingua.Unknown {
			conf = 0
		}
	} else {
		lang, conf = detect(text)
	}
	if conf < minConfidence {
		reason := "language unclear"
		if conf > 0 {
			reason = fmt.Sprintf("language unclear: best guess %s at %.0f%%", lang, conf*100)
		}
		return Result{Gate: GateLanguage, Verdict: Flag, Reason: reason, Snippet: opening(text)}
	}
	if len(c.Languages) == 0 {
		// No language limit: any language passes.
		return Result{Gate: GateLanguage, Verdict: Pass, Reason: "posting is in " + lang.String(), Snippet: opening(text)}
	}
	iso := strings.ToLower(lang.IsoCode639_1().String())
	if !slices.Contains(lower(c.Languages), iso) {
		return Result{Gate: GateLanguage, Verdict: Fail, Reason: "posting is in " + lang.String(), Snippet: opening(text)}
	}
	for _, m := range languageAsk.FindAllStringSubmatchIndex(text, -1) {
		name := ""
		switch {
		case m[2] >= 0:
			name = text[m[2]:m[3]]
		case m[4] >= 0:
			name = text[m[4]:m[5]]
		}
		if slices.Contains(lower(c.Languages), languageISO[strings.ToLower(name)]) {
			continue
		}
		return Result{
			Gate:    GateLanguage,
			Verdict: Flag,
			Reason:  fmt.Sprintf("posting is in %s but asks for fluent %s", lang, titleCase(name)),
			Snippet: snippet(text, m[0], m[1]),
		}
	}
	return Result{Gate: GateLanguage, Verdict: Pass, Reason: "posting is in " + lang.String(), Snippet: opening(text)}
}

func lower(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(strings.TrimSpace(s))
	}
	return out
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	s = strings.ToLower(s)
	return strings.ToUpper(s[:1]) + s[1:]
}
