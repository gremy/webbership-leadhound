package leadconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
)

// Overrides are the leads settings the console may change, saved as a partial JSON document
// over leads.yaml: a nil field keeps the file's value. They hold only knobs every reader takes
// through the effective config; never-contact countries and the fallback order live in the
// priority profile, and the sender, caps, channels, templates and the rest of the schedule
// stay in the file.
type Overrides struct {
	KindsOff      *[]string           `json:"kinds_off,omitempty"`
	KindsPriority *[]string           `json:"kinds_priority,omitempty"`
	SkipRankOver  *int                `json:"skip_rank_over,omitempty"`
	BatchesFrom   *string             `json:"batches_from,omitempty"`
	Facts         *FactsOverrides     `json:"facts,omitempty"`
	Relevance     *RelevanceOverrides `json:"relevance,omitempty"`
	Verify        *VerifyOverrides    `json:"verify,omitempty"`
	Schedule      *ScheduleOverrides  `json:"schedule,omitempty"`
}

// ScheduleOverrides are the schedule settings the console may change: the sending window,
// whole (start, end and days together).
type ScheduleOverrides struct {
	BusinessHours *BusinessHours `json:"business_hours,omitempty"`
}

// FactsOverrides are the end-of-life dates the console may change.
type FactsOverrides struct {
	D6EOL  *Date `json:"d6_eol,omitempty"`
	D7EOL  *Date `json:"d7_eol,omitempty"`
	D8EOL  *Date `json:"d8_eol,omitempty"`
	D9EOL  *Date `json:"d9_eol,omitempty"`
	D10EOL *Date `json:"d10_eol,omitempty"`
}

// RelevanceOverrides are the relevance settings the console may change.
type RelevanceOverrides struct {
	Rating          *bool `json:"rating,omitempty"`
	LeadsDailyCalls *int  `json:"leads_daily_calls,omitempty"`
}

// VerifyOverrides are the email check settings the console may change.
type VerifyOverrides struct {
	CatchAll *string `json:"catch_all,omitempty"`
}

// Problem is one reason a config or a settings change is refused, in plain words that name
// the setting.
type Problem struct {
	Message string
}

func (p Problem) String() string { return p.Message }

func problemsOf(list []string) []Problem {
	if len(list) == 0 {
		return nil
	}
	out := make([]Problem, len(list))
	for i, m := range list {
		out[i] = Problem{Message: m}
	}
	return out
}

// DecodeOverrides reads saved settings strictly: a key Overrides does not have, a value of
// the wrong type or a second JSON value is refused as ErrInvalid in plain words. Empty input
// and JSON null are no overrides.
func DecodeOverrides(b []byte) (Overrides, error) {
	var o Overrides
	if len(bytes.TrimSpace(b)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return Overrides{}, settingsInvalid(decodeProblem(err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Overrides{}, settingsInvalid("the saved settings hold more than one JSON value")
	}
	return o, nil
}

func settingsInvalid(msg string) error {
	return fmt.Errorf("leadconfig: settings: %w: %s", ErrInvalid, msg)
}

// decodeProblem words a JSON decode error for the operator.
func decodeProblem(err error) string {
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	switch {
	case errors.As(err, &typeErr):
		return fmt.Sprintf("%s must be %s, not %s", typeErr.Field, valueWords(typeErr.Type), typeErr.Value)
	case errors.As(err, &syntaxErr):
		return fmt.Sprintf("the saved settings are not valid JSON: %v", err)
	}
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return fmt.Sprintf("%s is not a setting the console can change", name)
	}
	return fmt.Sprintf("the saved settings cannot be read: %v", err)
}

// valueWords names the kind of value a Go type takes, in plain words.
func valueWords(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int64:
		return "a whole number"
	case reflect.Slice:
		return "a list"
	case reflect.Struct:
		if t == reflect.TypeFor[Date]() {
			return "a date like 2026-01-31"
		}
		return "a group of settings"
	}
	return "text"
}

// Apply returns cfg with every set override in place. The result shares no slice or map with
// cfg or o.
func (o Overrides) Apply(cfg Config) Config {
	c := cfg.Clone()
	if o.KindsOff != nil {
		c.KindsOff = slices.Clone(*o.KindsOff)
	}
	if o.KindsPriority != nil {
		c.KindsPriority = slices.Clone(*o.KindsPriority)
	}
	if o.SkipRankOver != nil {
		c.SkipRankOver = *o.SkipRankOver
	}
	if o.BatchesFrom != nil {
		c.BatchSource = *o.BatchesFrom
	}
	if f := o.Facts; f != nil {
		for _, d := range []struct {
			from *Date
			to   *Date
		}{
			{f.D6EOL, &c.Facts.D6EOL}, {f.D7EOL, &c.Facts.D7EOL}, {f.D8EOL, &c.Facts.D8EOL},
			{f.D9EOL, &c.Facts.D9EOL}, {f.D10EOL, &c.Facts.D10EOL},
		} {
			if d.from != nil {
				*d.to = *d.from
			}
		}
	}
	if r := o.Relevance; r != nil {
		if r.Rating != nil {
			on := *r.Rating
			c.Relevance.Rating = &on
		}
		if r.LeadsDailyCalls != nil {
			c.Relevance.LeadsDailyCalls = *r.LeadsDailyCalls
		}
	}
	if v := o.Verify; v != nil && v.CatchAll != nil {
		c.Verify.CatchAll = *v.CatchAll
	}
	if s := o.Schedule; s != nil && s.BusinessHours != nil {
		bh := *s.BusinessHours
		bh.Days = slices.Clone(bh.Days)
		c.Schedule.BusinessHours = bh
	}
	return c
}

// Effective applies o to cfg and checks the result with every check Load runs; for a config
// Load returned, that reads the templates the merged policy needs, so the result carries
// them. On any problem it returns the problems and a zero Config, and the caller keeps cfg.
func (o Overrides) Effective(cfg Config) (Config, []Problem) {
	merged := o.Apply(cfg)
	if p := merged.check(); len(p) > 0 {
		return Config{}, problemsOf(p)
	}
	return merged, nil
}

// Validate reports every problem with cfg once o is applied: the same rules, in the same
// words, as Load on a file holding the merged values.
func (o Overrides) Validate(cfg Config) []Problem {
	_, p := o.Effective(cfg)
	return p
}

// LeadsCapProblems reports relevance.leads_daily_calls above envCap
// (LEADHOUND_JEV_LEADS_DAILY_CALL_CAP): a setting may lower the server's cap, never raise it.
func (cfg Config) LeadsCapProblems(envCap int) []Problem {
	if cfg.Relevance.LeadsDailyCalls > envCap {
		return []Problem{{Message: fmt.Sprintf("The server allows at most %d a day; raise it in .env first.", envCap)}}
	}
	return nil
}
