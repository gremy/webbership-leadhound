package leadrate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Golden is one golden line: a lead's facts and the band a person gave it. A nil Label is
// not yet labelled: Evaluate skips it and counts it.
type Golden struct {
	ID    string  `json:"id"`
	Input Input   `json:"input"`
	Label *string `json:"label"`
	Note  string  `json:"note"`
}

// ErrNoGolden means the golden file holds no lines.
var ErrNoGolden = errors.New("leadrate: no golden lines")

// LoadGolden reads the golden set at path, one JSON object per line, strictly: an unknown
// field, trailing data on a line, a missing kind, a label that is not a band or null, and
// an empty, spaced or repeated id are each an error naming the line. Blank lines are
// skipped.
func LoadGolden(path string) ([]Golden, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator's own golden file
	if err != nil {
		return nil, fmt.Errorf("leadrate: golden: %w", err)
	}
	name := filepath.Base(path)
	var out []Golden
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		g, err := parseGolden(line)
		if err != nil {
			return nil, fmt.Errorf("leadrate: golden %s line %d: %w", name, n, err)
		}
		if seen[g.ID] {
			return nil, fmt.Errorf("leadrate: golden %s line %d: id %q appears twice", name, n, g.ID)
		}
		seen[g.ID] = true
		out = append(out, g)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("leadrate: golden %s: %w", name, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("leadrate: golden %s: %w", name, ErrNoGolden)
	}
	return out, nil
}

func parseGolden(line string) (Golden, error) {
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	var g Golden
	if err := dec.Decode(&g); err != nil {
		return Golden{}, err
	}
	if dec.More() {
		return Golden{}, errors.New("trailing data after the object")
	}
	if g.ID == "" || strings.ContainsFunc(g.ID, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return Golden{}, fmt.Errorf("id %q must be non-empty with no spaces", g.ID)
	}
	if g.Input.Kind == "" {
		return Golden{}, fmt.Errorf("lead %s: input has no kind", g.ID)
	}
	if g.Label != nil {
		if _, ok := Weight(*g.Label); !ok {
			return Golden{}, fmt.Errorf("lead %s: label %q, want one of %s or null", g.ID, *g.Label, strings.Join(Bands(), ", "))
		}
	}
	return g, nil
}
