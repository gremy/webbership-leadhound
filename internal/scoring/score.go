// Package scoring asks the decider the candidate's criteria about each job, writes one
// decision per criterion, and ranks the jobs for the brief.
//
// The decider only ever chooses among the candidate's own option descriptions; every word
// of a decision's reason is built here, in code, from those descriptions, lines of the
// cleaned posting and the probabilities.
package scoring

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// Verdicts, as decision.verdict stores them.
const (
	Pass    = "pass"
	Fail    = "fail"
	Unclear = "unclear"
)

const (
	// MaxDescription is how much of the cleaned description the state carries, in
	// characters.
	MaxDescription = 24000
	// RetryDescription is the shorter cut a request the model found too long is retried
	// with, once.
	RetryDescription = 12000
)

// Stop reasons Run reports, as run.stop_reason stores them.
const (
	StopSpendCap  = "spend_cap"
	StopThrottled = "throttled" // the provider stayed rate-limited or overloaded
)

// Posting is the part of an observation the state is built from.
type Posting struct {
	Title, Company, Location, Seniority, EmploymentType, Description string
}

// State is the plain text sent to the decider: title, company, location, seniority and
// employment type, then the cleaned description cut to maxDescription characters. It
// carries one job and nothing about the candidate.
func State(p Posting, maxDescription int) string {
	var b strings.Builder
	for _, f := range []struct{ label, value string }{
		{"Title", p.Title}, {"Company", p.Company}, {"Location", p.Location},
		{"Seniority", p.Seniority}, {"Employment type", p.EmploymentType},
	} {
		b.WriteString(f.label)
		b.WriteString(": ")
		b.WriteString(f.value)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(firstRunes(p.Description, maxDescription))
	return b.String()
}

// Questions turns the criteria into one choice question each, named after the criterion,
// whose options are the criterion's own pass, fail and unclear descriptions.
func Questions(cs []candidate.NamedCriterion) []ai.Question {
	qs := make([]ai.Question, len(cs))
	for i, c := range cs {
		qs[i] = ai.Question{
			Name:         c.Name,
			Instructions: fmt.Sprintf("Which description fits this job posting best on %s?", strings.ReplaceAll(c.Name, "_", " ")),
			Options: []ai.Option{
				{Name: Pass, Description: c.Pass},
				{Name: Fail, Description: c.Fail},
				{Name: Unclear, Description: c.Unclear},
			},
		}
	}
	return qs
}

// InputHash is the hash of everything a decision depends on: the decider's name, every
// question with its options, the state, the confidence threshold the verdict is cut at and
// the evidence keywords its reason is built from (by criterion name, each list in its own
// order). A changed posting, criterion, decider, threshold or keyword asks again; an
// unchanged one does not.
func InputHash(decider string, qs []ai.Question, state string, threshold float64, keywords map[string][]string) []byte {
	h := sha256.New()
	field := func(s string) {
		_, _ = fmt.Fprintf(h, "%d:%s", len(s), s) // length-prefixed, so fields cannot run together
	}
	field(decider)
	for _, q := range qs {
		field(q.Name)
		field(q.Instructions)
		for _, o := range q.Options {
			field(o.Name)
			field(o.Description)
		}
	}
	field(state)
	field(strconv.FormatFloat(threshold, 'g', -1, 64))
	names := slices.Sorted(maps.Keys(keywords))
	field(strconv.Itoa(len(names)))
	for _, n := range names {
		field(n)
		field(strconv.Itoa(len(keywords[n])))
		for _, k := range keywords[n] {
			field(k)
		}
	}
	return h.Sum(nil)
}

// Verdict maps an answer to a verdict: the chosen option, or unclear when the confidence
// is below threshold.
func Verdict(a ai.Answer, threshold float64) string {
	if a.Confidence < threshold {
		return Unclear
	}
	switch a.Choice {
	case Pass, Fail:
		return a.Choice
	}
	return Unclear
}

// GateFailed reports whether any gate result is a fail.
func GateFailed(gs []store.GateResult) bool {
	for _, g := range gs {
		if g.Verdict == gates.Fail {
			return true
		}
	}
	return false
}

// DecisionStore is what Scorer reads and writes (*store.Store through StoreAdapter).
type DecisionStore interface {
	ScoreInputs(ctx context.Context) ([]store.ScoreInput, error)
	InsertDecisions(ctx context.Context, ds []store.DecisionInsert) error
}

// StoreAdapter is a DecisionStore over the database.
type StoreAdapter struct{ S *store.Store }

// ScoreInputs reads from the pool.
func (a StoreAdapter) ScoreInputs(ctx context.Context) ([]store.ScoreInput, error) {
	return a.S.Q().ScoreInputs(ctx)
}

// InsertDecisions writes one job's decisions in one transaction.
func (a StoreAdapter) InsertDecisions(ctx context.Context, ds []store.DecisionInsert) error {
	return a.S.InsertDecisions(ctx, ds)
}

// Scorer scores every job ScoreInputs returns.
type Scorer struct {
	Store            DecisionStore
	Decider          ai.Decider // wrapped in ai.Capped in production
	Criteria         []candidate.NamedCriterion
	EvidenceKeywords map[string][]string
	Threshold        float64
}

// Result is what one Run did.
type Result struct {
	Scored     int // jobs decided and written
	Unchanged  int // jobs whose request matched their last decision's hash
	GateFailed int // jobs never sent because a gate failed
	Calls      int // decider calls made, retries included
	Failures   int // calls that failed without stopping the run
	Retried    int // jobs retried with the shorter description
	StopReason string
}

// Note is a one-line summary for the run row.
func (r Result) Note() string {
	return fmt.Sprintf("scored %d · unchanged %d · gate failed %d · calls %d · failures %d · retried %d",
		r.Scored, r.Unchanged, r.GateFailed, r.Calls, r.Failures, r.Retried)
}

// Run scores each job: a job with a gate fail is never sent, an unchanged request is not
// sent again, and every answered job gets one decision per criterion. The daily cap stops
// the run with StopSpendCap and an exhausted provider with StopThrottled; neither is an
// error. A request the model finds too long is retried once with the description cut to
// RetryDescription. Only a store failure (the cap's usage store included) or a cancelled
// context is an error.
func (s *Scorer) Run(ctx context.Context) (Result, error) {
	var res Result
	inputs, err := s.Store.ScoreInputs(ctx)
	if err != nil {
		return res, err
	}
	qs := Questions(s.Criteria)
	name := s.Decider.Name()
	for _, in := range inputs {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if GateFailed(in.Gates) {
			res.GateFailed++
			continue
		}
		p := Posting{Title: in.Title, Company: in.Company, Location: in.Location,
			Seniority: in.Seniority, EmploymentType: in.EmploymentType, Description: in.Description}
		state := State(p, MaxDescription)
		hash := InputHash(name, qs, state, s.Threshold, s.EvidenceKeywords)
		short := State(p, RetryDescription)
		shortHash := InputHash(name, qs, short, s.Threshold, s.EvidenceKeywords)
		// A job last sent with the shorter cut is unchanged too, or it would be retried every run.
		if in.LastInputSHA256 != nil && (bytes.Equal(hash, in.LastInputSHA256) || bytes.Equal(shortHash, in.LastInputSHA256)) {
			res.Unchanged++
			continue
		}

		answers, err := s.decide(ctx, &res, state, qs)
		if errors.Is(err, ai.ErrTooLong) && short != state {
			res.Retried++
			state, hash = short, shortHash
			answers, err = s.decide(ctx, &res, state, qs)
		}
		switch {
		case err == nil:
		case errors.Is(err, ai.ErrUsageStore):
			// The cap cannot count: going on would make calls nobody counts.
			return res, err
		case errors.Is(err, ai.ErrSpendCap):
			res.StopReason = StopSpendCap
			return res, nil
		case errors.Is(err, ai.ErrUnavailable):
			res.Failures++
			res.StopReason = StopThrottled
			return res, nil
		case ctx.Err() != nil:
			return res, ctx.Err()
		default:
			res.Failures++
			continue
		}

		ds := Decisions(in, answers, s.Criteria, s.Threshold, s.EvidenceKeywords, name, hash)
		if err := s.Store.InsertDecisions(ctx, ds); err != nil {
			return res, err
		}
		res.Scored++
	}
	return res, nil
}

// decide makes one call and counts it unless the cap refused it before any call.
func (s *Scorer) decide(ctx context.Context, res *Result, state string, qs []ai.Question) (map[string]ai.Answer, error) {
	answers, _, err := s.Decider.Decide(ctx, state, qs)
	if !errors.Is(err, ai.ErrSpendCap) {
		res.Calls++
	}
	return answers, err
}

// Decisions builds one decision per criterion from the answers.
func Decisions(in store.ScoreInput, answers map[string]ai.Answer, cs []candidate.NamedCriterion,
	threshold float64, keywords map[string][]string, decider string, hash []byte) []store.DecisionInsert {
	ev := Evidence{Title: in.Title, Description: in.Description, Gates: in.Gates, Keywords: keywords}
	out := make([]store.DecisionInsert, 0, len(cs))
	for _, c := range cs {
		a := answers[c.Name]
		v := Verdict(a, threshold)
		out = append(out, store.DecisionInsert{
			JobID: in.JobID, ObservationID: in.ObservationID, Decider: decider,
			Criterion: c.Name, Verdict: v, Confidence: a.Confidence, Probabilities: a.Probabilities,
			Reason: Reason(c, a, v, threshold, ev), InputSHA256: hash,
		})
	}
	return out
}

// firstRunes returns s cut to at most n runes, never splitting one.
func firstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
