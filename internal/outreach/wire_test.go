package outreach

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// apiSchemaKeywords are the JSON-schema keywords WriterSchema may use: the subset the API's
// structured outputs enforce (no maxItems, minItems only 0 or 1, no string lengths).
var apiSchemaKeywords = []string{"type", "properties", "required", "additionalProperties", "items", "minItems"}

// apiValidate is the API's side of the schema as far as WriterSchema uses it: what a
// constrained answer can and cannot be. It returns the first violation, "" for none.
func apiValidate(schema map[string]any, v any, at string) string {
	switch schema["type"] {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return at + ": not an object"
		}
		props, _ := schema["properties"].(map[string]any)
		for k := range obj {
			if _, ok := props[k]; !ok {
				return at + ": unexpected property " + k
			}
		}
		req, _ := schema["required"].([]string)
		for _, k := range req {
			if _, ok := obj[k]; !ok {
				return at + ": missing required " + k
			}
		}
		for k, sub := range props {
			if val, ok := obj[k]; ok {
				if e := apiValidate(sub.(map[string]any), val, at+"."+k); e != "" {
					return e
				}
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return at + ": not an array"
		}
		if m, ok := schema["minItems"].(int); ok && len(arr) < m {
			return fmt.Sprintf("%s: %d items, at least %d", at, len(arr), m)
		}
		for i, it := range arr {
			if e := apiValidate(schema["items"].(map[string]any), it, fmt.Sprintf("%s[%d]", at, i)); e != "" {
				return e
			}
		}
	case "string":
		if _, ok := v.(string); !ok {
			return at + ": not a string"
		}
	default:
		return at + ": unknown type"
	}
	return ""
}

// schemaKeywords collects every keyword a schema node and its children use.
func schemaKeywords(schema map[string]any, into map[string]bool) {
	for k, v := range schema {
		into[k] = true
		switch k {
		case "properties":
			for _, sub := range v.(map[string]any) {
				schemaKeywords(sub.(map[string]any), into)
			}
		case "items":
			schemaKeywords(v.(map[string]any), into)
		case "minItems":
			if n := v.(int); n != 0 && n != 1 {
				into[fmt.Sprintf("minItems=%d", n)] = true
			}
		}
	}
}

func decodeAny(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestWriterSchemaRequiresEveryStep: the schema for a lead requires exactly the steps it is
// asked for, each with at least one paragraph, and exactly three talking points when the
// sequence has a call; it uses only keywords the API supports.
func TestWriterSchemaRequiresEveryStep(t *testing.T) {
	r := GroundRules{StepRules: StepRules{EmailSteps: []int{4, 0, 3, 1}}, CallStep: true}
	s := WriterSchema(r)
	steps := s["properties"].(map[string]any)["steps"].(map[string]any)
	if got := steps["required"].([]string); !slices.Equal(got, []string{"step_0", "step_1", "step_3", "step_4"}) || steps["additionalProperties"] != false {
		t.Fatalf("steps required %v", got)
	}
	st := steps["properties"].(map[string]any)["step_0"].(map[string]any)
	if p := st["properties"].(map[string]any)["paragraphs"].(map[string]any); p["minItems"] != 1 {
		t.Fatalf("paragraphs %v", p)
	}
	tps := s["properties"].(map[string]any)["talking_points"].(map[string]any)
	if got := tps["required"].([]string); !slices.Equal(got, []string{"point_1", "point_2", "point_3"}) {
		t.Fatalf("talking points required %v", got)
	}
	if got := s["required"].([]string); !slices.Equal(got, []string{"steps", "talking_points"}) {
		t.Fatalf("top required %v", got)
	}
	used := map[string]bool{}
	schemaKeywords(s, used)
	for k := range used {
		if !slices.Contains(apiSchemaKeywords, k) {
			t.Errorf("keyword %q is outside the API's structured-output subset", k)
		}
	}

	noCall := WriterSchema(GroundRules{StepRules: StepRules{EmailSteps: []int{2, 3}}})
	if _, ok := noCall["properties"].(map[string]any)["talking_points"]; ok || !slices.Equal(noCall["required"].([]string), []string{"steps"}) {
		t.Fatalf("no call step still asks for talking points: %v", noCall)
	}
	if got := noCall["properties"].(map[string]any)["steps"].(map[string]any)["required"].([]string); !slices.Equal(got, []string{"step_2", "step_3"}) {
		t.Fatalf("call_first steps %v", got)
	}
}

// TestWriterSchemaRefusesTheSkeleton: the walk's first-try skeleton (one step, empty
// paragraphs) is refused by the API side of the schema in either shape, a clean draft passes
// it, and Ground still refuses the skeleton when it gets that far.
func TestWriterSchemaRefusesTheSkeleton(t *testing.T) {
	in := writerInput(t, "d7")
	schema := WriterSchema(in.Rules)
	skeleton := WrittenDraft{Steps: []WrittenStep{{Step: 0, Subject: "Drupal 7", Paragraphs: []Paragraph{}}}, TalkingPoints: []TalkingPoint{}}

	old, err := json.Marshal(skeleton)
	if err != nil {
		t.Fatal(err)
	}
	if e := apiValidate(schema, decodeAny(t, old), "$"); e == "" {
		t.Fatal("the old-shape skeleton passes the schema")
	}
	wire, err := EncodeWireDraft(skeleton)
	if err != nil {
		t.Fatal(err)
	}
	if e := apiValidate(schema, decodeAny(t, wire), "$"); !strings.Contains(e, "missing required") {
		t.Fatalf("the wire skeleton: %q", e)
	}
	oneStepFull := skeleton
	oneStepFull.Steps = []WrittenStep{{Step: 0, Subject: "x", Paragraphs: nil}}
	b, _ := EncodeWireDraft(oneStepFull)
	if e := apiValidate(WriterSchema(GroundRules{StepRules: StepRules{EmailSteps: []int{0}}}), decodeAny(t, b), "$"); !strings.Contains(e, "at least 1") {
		t.Fatalf("empty paragraphs: %q", e)
	}
	if e := apiValidate(schema, decodeAny(t, writerFixture(t, "clean_d7")), "$"); e != "" {
		t.Fatalf("the clean draft fails the schema: %s", e)
	}

	d, err := DecodeWireDraft(wire)
	if err != nil {
		t.Fatal(err)
	}
	fails := Ground(d, in.Sheet, in.Rules)
	var missing, empty bool
	for _, f := range fails {
		missing = missing || (f.Code == CodeSchema && strings.Contains(f.Msg, "is missing"))
		empty = empty || (f.Code == CodeSchema && strings.Contains(f.Msg, "no paragraphs"))
	}
	if !missing || !empty {
		t.Fatalf("Ground on the skeleton: %v", fails)
	}
}

// TestWriterOldShapeAnswerIsRefused: a caller answering in the old list shape is a G1 failed
// attempt ("does not follow the schema"), then the fallback; the writer never reads it.
func TestWriterOldShapeAnswerIsRefused(t *testing.T) {
	old, _ := json.Marshal(WrittenDraft{Steps: []WrittenStep{{Step: 0, Subject: "Drupal 7", Paragraphs: []Paragraph{}}}, TalkingPoints: []TalkingPoint{}})
	s := &scripted{replies: []scriptReply{{json: old}, {json: old}}}
	res, err := (&Writer{Caller: s}).WriteLead(t.Context(), writerInput(t, "d7"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != nil || res.Fallback == "" || len(res.Attempts) != 2 || res.Attempts[0].Failures[0].Code != CodeSchema {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(s.reqs[1].Prompt, string(old)) {
		t.Fatal("the retry does not show the previous answer")
	}
}

// TestWireDraftRoundTrip: the wire shape decodes to the same draft the writer stores, steps
// and points in order, and refuses keys not of its form.
func TestWireDraftRoundTrip(t *testing.T) {
	want, err := DecodeWireDraft(writerFixture(t, "clean_d7"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := EncodeWireDraft(want)
	got, err := DecodeWireDraft(b)
	if err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) || len(got.Steps) < 2 || got.Steps[0].Step > got.Steps[1].Step {
		t.Fatalf("round trip %s", gb)
	}
	for _, bad := range []string{
		`{"steps":{"step_x":{"subject":"","paragraphs":[]}}}`,
		`{"steps":{"step_01":{"subject":"","paragraphs":[]}}}`,
		`{"steps":{"step_0":{"subject":"","paragraphs":[],"extra":1}}}`,
		`{"steps":{"step_0":{"subject":"","paragraphs":null}}}`,
		`{"steps":{},"talking_points":{"point_0":{"text":"","cites":[]}}}`,
		`{"talking_points":{}}`,
		`{"steps":{}} {}`,
	} {
		if _, err := DecodeWireDraft([]byte(bad)); err == nil {
			t.Errorf("decoded %s", bad)
		}
	}
}
