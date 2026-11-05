package cafe

import (
	"fmt"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"
)

// placeWord is one configured word: as written in signals.yaml, and in the form names are
// compared in.
type placeWord struct {
	written, normalized string
}

// roasteryWords is the place_words.roastery list of the embedded signals.yaml, parsed once.
var roasteryWords = sync.OnceValue(func() []placeWord {
	words, err := parseRoasteryWords(defaultSignals)
	if err != nil {
		panic(err) // the embedded file is covered by a test
	}
	return words
})

// parseRoasteryWords reads the place_words.roastery list in the signals.yaml shape and checks
// that every word still says something once normalised.
func parseRoasteryWords(data []byte) ([]placeWord, error) {
	var doc struct {
		PlaceWords struct {
			Roastery []string `yaml:"roastery"`
		} `yaml:"place_words"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("cafe signals: %w", err)
	}
	words := make([]placeWord, 0, len(doc.PlaceWords.Roastery))
	for _, w := range doc.PlaceWords.Roastery {
		n := NormalizeName(w)
		if n == "" {
			return nil, fmt.Errorf("cafe signals: place_words.roastery: word %q is empty", w)
		}
		words = append(words, placeWord{written: w, normalized: n})
	}
	return words, nil
}

// RoasteryWord looks for a configured roastery word in a business name, ignoring case and
// diacritics, as whole words. It returns the first match in list order, as signals.yaml writes
// it, and whether there was one. A match is only a suggestion: a café can carry such a word in
// its name, which is why the operator can correct the place type it leads to.
func RoasteryWord(name string) (string, bool) {
	n := NormalizeName(name)
	if n == "" {
		return "", false
	}
	padded := " " + n + " "
	for _, w := range roasteryWords() {
		if strings.Contains(padded, " "+w.normalized+" ") {
			return w.written, true
		}
	}
	return "", false
}
