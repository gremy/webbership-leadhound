package outreach

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// The answer's wire shape. The structured-output subset the API enforces has no maxItems and
// no minItems above 1, so a list of steps cannot be made to hold every step: the model could
// (and did) answer with one step and empty paragraphs, schema-valid. Instead each step to
// write is its own required property ("step_0", "step_3"), each step's paragraphs need at
// least one item, and the call step's talking points are three required properties
// ("point_1".."point_3"). The schema therefore varies per lead with the steps it is asked for;
// the system prompt does not. The writer converts the answer to WrittenDraft, the shape every
// stored generation keeps.
const (
	wireStepPrefix  = "step_"
	wirePointPrefix = "point_"
)

// WireStepKey is step's property name in the answer ("step_3").
func WireStepKey(step int) string { return wireStepPrefix + strconv.Itoa(step) }

// WirePointKey is talking point i's property name (0-based i: "point_1").
func WirePointKey(i int) string { return wirePointPrefix + strconv.Itoa(i+1) }

// WriterSchema is the strict JSON schema of the answer for rules: one required property per
// email step in rules.EmailSteps, each with a subject and at least one paragraph (text, cites
// and pain_id, all required so G5b can read the pain), and, when rules.CallStep, exactly
// TalkingPoints talking points as required properties. Without a call step the answer has no
// talking_points at all. Only keywords the API's structured outputs support are used (type,
// properties, required, additionalProperties false, items, minItems 1).
func WriterSchema(rules GroundRules) map[string]any {
	str := map[string]any{"type": "string"}
	cites := map[string]any{"type": "array", "items": str}
	obj := func(props map[string]any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	para := obj(map[string]any{"text": str, "cites": cites, "pain_id": str}, []string{"text", "cites", "pain_id"})
	step := obj(map[string]any{
		"subject":    str,
		"paragraphs": map[string]any{"type": "array", "items": para, "minItems": 1},
	}, []string{"subject", "paragraphs"})
	steps := slices.Clone(rules.EmailSteps)
	slices.Sort(steps)
	steps = slices.Compact(steps)
	stepProps := map[string]any{}
	stepKeys := make([]string, 0, len(steps))
	for _, s := range steps {
		k := WireStepKey(s)
		stepProps[k] = step
		stepKeys = append(stepKeys, k)
	}
	props := map[string]any{"steps": obj(stepProps, stepKeys)}
	required := []string{"steps"}
	if rules.CallStep {
		tp := obj(map[string]any{"text": str, "cites": cites}, []string{"text", "cites"})
		tpProps := map[string]any{}
		tpKeys := make([]string, TalkingPoints)
		for i := range TalkingPoints {
			tpKeys[i] = WirePointKey(i)
			tpProps[tpKeys[i]] = tp
		}
		props["talking_points"] = obj(tpProps, tpKeys)
		required = append(required, "talking_points")
	}
	return obj(props, required)
}

type wireStep struct {
	Subject    string          `json:"subject"`
	Paragraphs []wireParagraph `json:"paragraphs"`
}

// wireParagraph is Paragraph with pain_id always present, as the schema requires it.
type wireParagraph struct {
	Text   string   `json:"text"`
	Cites  []string `json:"cites"`
	PainID string   `json:"pain_id"`
}

type wireDraft struct {
	Steps         map[string]wireStep     `json:"steps"`
	TalkingPoints map[string]TalkingPoint `json:"talking_points"`
}

// DecodeWireDraft reads the model's answer in the wire shape strictly (unknown fields,
// trailing data, a step or point key not of its form, a null field: errors) and returns it as
// a WrittenDraft, steps and talking points in order. It checks the shape only: which steps
// and how many paragraphs and points are G1's, so a missing step is counted there.
func DecodeWireDraft(b []byte) (WrittenDraft, error) {
	var w wireDraft
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return WrittenDraft{}, fmt.Errorf("outreach: answer: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return WrittenDraft{}, errors.New("outreach: answer: trailing data after the JSON object")
	}
	if w.Steps == nil {
		return WrittenDraft{}, errors.New("outreach: answer: no steps object")
	}
	d := WrittenDraft{TalkingPoints: []TalkingPoint{}}
	for k, st := range w.Steps {
		n, err := wireIndex(k, wireStepPrefix, 0)
		if err != nil {
			return WrittenDraft{}, err
		}
		if st.Paragraphs == nil {
			return WrittenDraft{}, fmt.Errorf("outreach: answer: %s has no paragraphs list", k)
		}
		paras := make([]Paragraph, 0, len(st.Paragraphs))
		for _, p := range st.Paragraphs {
			if p.Cites == nil {
				return WrittenDraft{}, fmt.Errorf("outreach: answer: a paragraph of %s has no cites list", k)
			}
			paras = append(paras, Paragraph(p))
		}
		d.Steps = append(d.Steps, WrittenStep{Step: n, Subject: st.Subject, Paragraphs: paras})
	}
	slices.SortFunc(d.Steps, func(a, b WrittenStep) int { return a.Step - b.Step })
	type point struct {
		n  int
		tp TalkingPoint
	}
	var points []point
	for k, tp := range w.TalkingPoints {
		n, err := wireIndex(k, wirePointPrefix, 1)
		if err != nil {
			return WrittenDraft{}, err
		}
		if tp.Cites == nil {
			return WrittenDraft{}, fmt.Errorf("outreach: answer: %s has no cites list", k)
		}
		points = append(points, point{n, tp})
	}
	slices.SortFunc(points, func(a, b point) int { return a.n - b.n })
	for _, p := range points {
		d.TalkingPoints = append(d.TalkingPoints, p.tp)
	}
	return d, nil
}

// wireIndex reads the number of key prefix<n>, n ≥ lowest, written canonically.
func wireIndex(key, prefix string, lowest int) (int, error) {
	s, ok := strings.CutPrefix(key, prefix)
	n, err := strconv.Atoi(s)
	if !ok || err != nil || n < lowest || strconv.Itoa(n) != s {
		return 0, fmt.Errorf("outreach: answer: unexpected key %q", key)
	}
	return n, nil
}

// EncodeWireDraft is d in the wire shape: what a fake caller answers with. A repeated step
// keeps its last entry (the wire shape cannot repeat a step); talking points are numbered in
// order and left out when there are none.
func EncodeWireDraft(d WrittenDraft) ([]byte, error) {
	w := wireDraft{Steps: map[string]wireStep{}}
	for _, st := range d.Steps {
		paras := make([]wireParagraph, 0, len(st.Paragraphs))
		for _, p := range st.Paragraphs {
			paras = append(paras, wireParagraph(p))
		}
		w.Steps[WireStepKey(st.Step)] = wireStep{Subject: st.Subject, Paragraphs: paras}
	}
	if len(d.TalkingPoints) > 0 {
		w.TalkingPoints = map[string]TalkingPoint{}
		for i, tp := range d.TalkingPoints {
			w.TalkingPoints[WirePointKey(i)] = tp
		}
	}
	type out struct {
		Steps         map[string]wireStep     `json:"steps"`
		TalkingPoints map[string]TalkingPoint `json:"talking_points,omitempty"`
	}
	b, err := json.Marshal(out(w))
	if err != nil {
		return nil, fmt.Errorf("outreach: encode answer: %w", err)
	}
	return b, nil
}
