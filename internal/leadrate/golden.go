package leadrate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
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

// GoldenFromLabels turns the operator's stored calls (leadstore.LabelledLeads) into golden
// lines for Evaluate: the id is the lead's domain ("lead-N" when it has none), the input the
// one the call was made on, read as strictly as a golden file's, the label the call's band,
// and the note "lead N". A cleared call is skipped; an input that does not read, or a band
// that is not one, is an error naming the lead.
func GoldenFromLabels(labels []leadstore.LeadLabel) ([]Golden, error) {
	out := make([]Golden, 0, len(labels))
	for _, l := range labels {
		if l.Band == nil {
			continue
		}
		id := "lead-" + strconv.FormatInt(l.LeadID, 10)
		if l.Domain != "" && !strings.ContainsFunc(l.Domain, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			id = l.Domain
		}
		if _, ok := Weight(*l.Band); !ok {
			return nil, fmt.Errorf("leadrate: call on lead %d: band %q is not one", l.LeadID, *l.Band)
		}
		var in Input
		if err := json.Unmarshal(l.Input, &in); err != nil {
			return nil, fmt.Errorf("leadrate: call on lead %d: %w", l.LeadID, err)
		}
		if in.Kind == "" {
			return nil, fmt.Errorf("leadrate: call on lead %d: input has no kind", l.LeadID)
		}
		band := *l.Band
		out = append(out, Golden{ID: id, Input: in, Label: &band, Note: "lead " + strconv.FormatInt(l.LeadID, 10)})
	}
	return out, nil
}
