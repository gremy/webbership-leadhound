// Package scoring asks the decider the candidate's criteria about each job, writes one
// decision per criterion, and ranks the jobs for the brief.
//
// The decider only ever chooses among the candidate's own option descriptions; every word
// of a decision's reason is built here, in code, from those descriptions, lines of the
// cleaned posting and the probabilities.
package scoring

import (
	"bytes"
	"cmp"
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
	// ScoreQueue is the light first pass: every job worth scoring, without descriptions.
	ScoreQueue(ctx context.Context) (store.ScoreQueue, error)
	// ScoreInputsFor loads the full inputs of one batch, in the order of ids.
	ScoreInputsFor(ctx context.Context, ids []int64) ([]store.ScoreInput, error)
	InsertDecisions(ctx context.Context, ds []store.DecisionInsert) error
}

// StoreAdapter is a DecisionStore over the database.
type StoreAdapter struct{ S *store.Store }

// ScoreQueue reads from the pool.
func (a StoreAdapter) ScoreQueue(ctx context.Context) (store.ScoreQueue, error) {
	return a.S.Q().ScoreQueue(ctx)
}

// ScoreInputsFor reads from the pool.
func (a StoreAdapter) ScoreInputsFor(ctx context.Context, ids []int64) ([]store.ScoreInput, error) {
	return a.S.Q().ScoreInputsFor(ctx, ids)
}

// InsertDecisions writes one job's decisions in one transaction.
func (a StoreAdapter) InsertDecisions(ctx context.Context, ds []store.DecisionInsert) error {
	return a.S.InsertDecisions(ctx, ds)
}

// Batch sizes: how many full inputs (descriptions included) Run holds at once.
const (
	// DefaultBatch is the batch when the cap's remaining calls are unknown.
	DefaultBatch = 100
	minBatch     = 10
	maxBatch     = 500
)

// BatchFor sizes a batch to the calls the cap still allows today, with a small margin
// for jobs that turn out unchanged, cost no call or fail on their own: remaining plus a
// tenth plus ten, between 10 and 500. Run loads another batch only while calls remain, so
// the batch bounds memory, not how many jobs are looked at.
func BatchFor(remaining int) int {
	return min(max(remaining+remaining/10+minBatch, minBatch), maxBatch)
}

// Scorer scores the jobs the store's queue holds.
type Scorer struct {
	Store            DecisionStore
	Decider          ai.Decider // wrapped in ai.Capped in production
	Criteria         []candidate.NamedCriterion
	EvidenceKeywords map[string][]string
	Threshold        float64
	// TitleCheck is the profile's three-way title screen (candidate.Config.TitleCheck). Nil
	// keeps every title, as a profile without a screen does.
	TitleCheck func(title string) candidate.TitleVerdict
	// Batch is how many full inputs are loaded at once (BatchFor the cap's remaining
	// calls); DefaultBatch when 0 or less.
	Batch int
}

// Result is what one Run did.
type Result struct {
	Scored     int // jobs decided and written
	Unchanged  int // jobs whose request matched their last decision's hash
	GateFailed int // jobs never sent because a gate failed
	Calls      int // decider calls made, retries included
	Failures   int // calls that failed without stopping the run
	Retried    int // jobs retried with the shorter description
	// TitleAsked counts title-only calls (also in Calls); TitleFailed the jobs the title
	// check failed (a title the screen drops included, which costs no call), and
	// TitlePassed the jobs it let on to full scoring (pass or unclear).
	TitleAsked, TitleFailed, TitlePassed int
	// Backlog counts the jobs still waiting when the run ended: in the queue, with no
	// decision on their current observation, and not decided by this run.
	Backlog    int
	StopReason string
}

// Note is a one-line summary for the run row.
func (r Result) Note() string {
	return fmt.Sprintf("scored %d · unchanged %d · gate failed %d · calls %d · failures %d · retried %d · title check: %d asked · %d failed · %d passed · backlog %d",
		r.Scored, r.Unchanged, r.GateFailed, r.Calls, r.Failures, r.Retried, r.TitleAsked, r.TitleFailed, r.TitlePassed, r.Backlog)
}

// screenRank orders the queue: a kept title (or a dropped one, which costs no call)
// before an unsure one, which may cost two.
func screenRank(v candidate.TitleVerdict) int {
	if v == candidate.TitleUnsure {
		return 1
	}
	return 0
}

// Order sorts the queue in place: kept (and dropped) titles before unsure ones, each
// group newest first (first seen descending, then job id).
func Order(es []store.QueueEntry, check func(string) candidate.TitleVerdict) {
	rank := make(map[int64]int, len(es))
	if check != nil {
		for _, e := range es {
			rank[e.JobID] = screenRank(check(e.Title))
		}
	}
	slices.SortStableFunc(es, func(a, b store.QueueEntry) int {
		if c := cmp.Compare(rank[a.JobID], rank[b.JobID]); c != 0 {
			return c
		}
		if c := b.FirstSeenAt.Compare(a.FirstSeenAt); c != 0 {
			return c
		}
		return cmp.Compare(a.JobID, b.JobID)
	})
}

// Run scores the queue in order, one batch of full inputs at a time: a job with a gate
// fail is never loaded, an unchanged request is not sent again, and every answered job
// gets one decision per criterion. Before that, the title screen sorts the job's title
// (A16): a kept title goes straight to full scoring; an unsure one is first asked the
// title-only question, and a fail there records one title decision and skips full
// scoring; a dropped title (which the collector never stores) records the same fail
// without a call. The daily cap stops the run with StopSpendCap and an exhausted provider
// with StopThrottled; neither is an error, and no further batch is loaded. A request the
// model finds too long is retried once with the description cut to RetryDescription. Only
// a store failure (the cap's usage store included) or a cancelled context is an error.
func (s *Scorer) Run(ctx context.Context) (res Result, _ error) { // named, so the deferred backlog count lands
	queue, err := s.Store.ScoreQueue(ctx)
	if err != nil {
		return res, err
	}
	res.GateFailed = queue.GateFailed
	titleC, hasTitle := titleCriterion(s.Criteria)
	check := s.TitleCheck
	if !hasTitle {
		check = nil
	}
	Order(queue.Entries, check)
	j := &jobScorer{s: s, qs: Questions(s.Criteria), name: s.Decider.Name(), titleC: titleC, tq: TitleQuestion(titleC), check: check}

	decided := map[int64]bool{}
	defer func() {
		for _, e := range queue.Entries {
			if !e.Decided && !decided[e.JobID] {
				res.Backlog++
			}
		}
	}()
	batch := s.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}
	for from := 0; from < len(queue.Entries); from += batch {
		chunk := queue.Entries[from:min(from+batch, len(queue.Entries))]
		ids := make([]int64, len(chunk))
		for i, e := range chunk {
			ids[i] = e.JobID
		}
		inputs, err := s.Store.ScoreInputsFor(ctx, ids)
		if err != nil {
			return res, err
		}
		for _, in := range inputs {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			wrote, stop, err := j.score(ctx, &res, in)
			if wrote {
				decided[in.JobID] = true
			}
			if stop || err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// jobScorer is what one Run needs to score a job.
type jobScorer struct {
	s      *Scorer
	qs     []ai.Question
	name   string
	titleC candidate.NamedCriterion
	tq     ai.Question
	check  func(string) candidate.TitleVerdict
}

// score scores one job. wrote is true when decisions were written; stop is true when the
// run ends here, with err when that is an error.
func (j *jobScorer) score(ctx context.Context, res *Result, in store.ScoreInput) (wrote, stop bool, _ error) {
	s := j.s
	if GateFailed(in.Gates) {
		// The queue leaves gate fails out; a gate written since it was read is caught here.
		res.GateFailed++
		return false, false, nil
	}
	screen := candidate.TitleKeep
	if j.check != nil {
		screen = j.check(in.Title)
	}
	p := Posting{Title: in.Title, Company: in.Company, Location: in.Location,
		Seniority: in.Seniority, EmploymentType: in.EmploymentType, Description: in.Description}
	state := State(p, MaxDescription)
	hash := InputHash(j.name, j.qs, state, s.Threshold, s.EvidenceKeywords)
	short := State(p, RetryDescription)
	shortHash := InputHash(j.name, j.qs, short, s.Threshold, s.EvidenceKeywords)
	var titleHash []byte
	if screen != candidate.TitleKeep {
		// A kept title hashes as before the screen existed, so its decisions stay current.
		hash = ScreenHash(hash, screen, j.tq)
		shortHash = ScreenHash(shortHash, screen, j.tq)
		titleHash = ScreenHash(InputHash(j.name, []ai.Question{j.tq}, TitleState(in.Title), s.Threshold, nil), screen, j.tq)
	}
	// A job last sent with the shorter cut is unchanged too, or it would be retried every
	// run; so is one the title check already failed on these inputs.
	if last := in.LastInputSHA256; last != nil &&
		(bytes.Equal(hash, last) || bytes.Equal(shortHash, last) || bytes.Equal(titleHash, last)) {
		res.Unchanged++
		return false, false, nil
	}

	switch screen {
	case candidate.TitleDrop:
		if err := s.Store.InsertDecisions(ctx, []store.DecisionInsert{DropDecision(in, j.titleC, j.name, titleHash)}); err != nil {
			return false, true, err
		}
		res.TitleFailed++
		res.Scored++
		return true, false, nil
	case candidate.TitleUnsure:
		answers, err := s.decide(ctx, res, TitleState(in.Title), []ai.Question{j.tq})
		if !errors.Is(err, ai.ErrSpendCap) {
			res.TitleAsked++
		}
		if err != nil {
			stop, err := s.callOutcome(ctx, res, err)
			return false, stop, err
		}
		a := answers[j.tq.Name]
		if Verdict(a, s.Threshold) == Fail {
			d := TitleCheckDecision(in, j.titleC, a, s.Threshold, j.name, titleHash)
			if err := s.Store.InsertDecisions(ctx, []store.DecisionInsert{d}); err != nil {
				return false, true, err
			}
			res.TitleFailed++
			res.Scored++
			return true, false, nil
		}
		// Pass or unclear: full scoring asks the title again with the whole posting.
		res.TitlePassed++
	}

	answers, err := s.decide(ctx, res, state, j.qs)
	if errors.Is(err, ai.ErrTooLong) && short != state {
		res.Retried++
		state, hash = short, shortHash
		answers, err = s.decide(ctx, res, state, j.qs)
	}
	if err != nil {
		stop, err := s.callOutcome(ctx, res, err)
		return false, stop, err
	}

	ds := Decisions(in, answers, s.Criteria, s.Threshold, s.EvidenceKeywords, j.name, hash)
	if err := s.Store.InsertDecisions(ctx, ds); err != nil {
		return false, true, err
	}
	res.Scored++
	return true, false, nil
}

// callOutcome sorts a failed decider call: stop is true when the run ends (spend cap,
// unavailable provider, or the error returned with it); otherwise the failure is counted
// and the caller skips the job.
func (s *Scorer) callOutcome(ctx context.Context, res *Result, err error) (stop bool, _ error) {
	switch {
	case errors.Is(err, ai.ErrUsageStore):
		// The cap cannot count: going on would make calls nobody counts.
		return true, err
	case errors.Is(err, ai.ErrSpendCap):
		res.StopReason = StopSpendCap
		return true, nil
	case errors.Is(err, ai.ErrUnavailable):
		res.Failures++
		res.StopReason = StopThrottled
		return true, nil
	case ctx.Err() != nil:
		return true, ctx.Err()
	}
	res.Failures++
	return false, nil
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
