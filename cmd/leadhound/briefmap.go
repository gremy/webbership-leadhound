package main

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// maxNoteKeywords is how many matched keywords a criterion's note names.
const maxNoteKeywords = 3

// briefSource is one source the brief reports health for. A source that is not always
// shown appears only on a day it ran, or when it has jobs left out as not seen on their
// board. ats names the ATS of an ATS source: it has no canary, its jobs follow the
// not-seen rule, and its failed run's note starts "<ats>: failed: ".
type briefSource struct {
	source, name string
	always       bool
	ats          string
}

var briefSources = []briefSource{
	{linkedin.Source, "LinkedIn", true, ""},
	{linkedin.SourceFixture, "LinkedIn fixtures", false, ""}, // a development dry run
	{ats.Greenhouse, "Greenhouse", false, ats.Greenhouse},
	{ats.Lever, "Lever", false, ats.Lever},
	{ats.Ashby, "Ashby", false, ats.Ashby},
	{ats.SourceFixture(ats.Greenhouse), "Greenhouse fixtures", false, ats.Greenhouse},
	{ats.SourceFixture(ats.Lever), "Lever fixtures", false, ats.Lever},
	{ats.SourceFixture(ats.Ashby), "Ashby fixtures", false, ats.Ashby},
}

// atsSources are the sources of briefSources that are ATS boards.
func atsSources() []string {
	var out []string
	for _, bs := range briefSources {
		if bs.ats != "" {
			out = append(out, bs.source)
		}
	}
	return out
}

// briefCandidates reads the store into what the brief ranks for day (a time on that
// calendar day in loc). An open ATS job whose board has not listed it since
// brief.BoardStaleDays before the day's start is set apart in notSeen, with its source in
// notSeenSrc: the board may be failing or gone, so the job is not listed, but its listing
// stays open, since the board may come back (A14, the ATS twin of leaveOutStale).
func briefCandidates(ctx context.Context, q store.Queries, loc *time.Location, day time.Time, allowFake bool) (cands, notSeen []scoring.Candidate, notSeenSrc map[int64]string, err error) {
	ins, err := q.RankInputs(ctx, store.RankOptions{AllowFake: allowFake})
	if err != nil {
		return nil, nil, nil, err
	}
	rules, err := q.ActiveRules(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	y, m, d := day.In(loc).Date()
	cutoff := time.Date(y, m, d, 0, 0, 0, 0, loc).AddDate(0, 0, -brief.BoardStaleDays)
	notSeenSrc, err = q.OpenJobsNotSeenSince(ctx, atsSources(), cutoff)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, c := range scoring.CandidatesFrom(ins, rules) {
		if _, ok := notSeenSrc[c.JobID]; ok {
			notSeen = append(notSeen, c)
		} else {
			cands = append(cands, c)
		}
	}
	return cands, notSeen, notSeenSrc, nil
}

// notSeenBySource counts, per source, the jobs set apart as not seen on their board that
// would otherwise be listed: those the ranking would put in either tier.
func notSeenBySource(notSeen []scoring.Candidate, src map[int64]string, loc *time.Location) map[string]int {
	all := len(notSeen)
	r := scoring.Rank(notSeen, loc, scoring.Caps{Apply: all, YourCall: all})
	out := map[string]int{}
	for _, rk := range append(r.Apply, r.YourCall...) {
		out[src[rk.JobID]]++
	}
	return out
}

// buildInput reads the store into the brief for day (a time on that calendar day in loc):
// the ranking, today's skip counts, the source health, and the scoring and recheck state.
func buildInput(ctx context.Context, q store.Queries, prof candidate.Config, loc *time.Location, day time.Time, allowFake, recheckOff bool) (brief.Input, error) {
	cands, notSeen, notSeenSrc, err := briefCandidates(ctx, q, loc, day, allowFake)
	if err != nil {
		return brief.Input{}, err
	}
	notSeenN := notSeenBySource(notSeen, notSeenSrc, loc)
	r := scoring.Rank(cands, loc, briefCaps(prof))
	var ids []int64
	for _, rk := range append(append([]scoring.Ranked{}, r.Apply...), r.YourCall...) {
		ids = append(ids, rk.JobID)
	}
	if r.RunnerUp != nil {
		ids = append(ids, r.RunnerUp.JobID)
	}
	sums, err := q.JobSummaries(ctx, ids)
	if err != nil {
		return brief.Input{}, err
	}
	kws := prof.Candidate.EvidenceKeywords
	y, m, d := day.In(loc).Date()
	in := brief.Input{
		Day:         time.Date(y, m, d, 0, 0, 0, 0, loc),
		RecheckOff:  recheckOff,
		Unreachable: r.Skipped[scoring.SkipUnreachable],
		Unscored:    r.Skipped[scoring.SkipUnscored],
	}
	in.Skipped = scoring.SkippedOn(cands, loc, in.Day)
	for _, rk := range r.Apply {
		in.Apply = append(in.Apply, briefJob(rk.Candidate, sums[rk.JobID].URL, kws))
	}
	for _, rk := range r.YourCall {
		in.YourCall = append(in.YourCall, briefJob(rk.Candidate, sums[rk.JobID].URL, kws))
	}
	if ru := r.RunnerUp; ru != nil {
		in.RunnerUp = &brief.RunnerUp{
			Job:             briefJob(ru.Candidate, sums[ru.JobID].URL, kws),
			FailedCriterion: ru.FailedCriterion,
			Note:            strings.Join(matchedKeywords(ru.Description, kws[ru.FailedCriterion]), ", "),
		}
	}
	for _, bs := range briefSources {
		src, ran, err := sourceHealth(ctx, q, bs, loc, in.Day)
		if err != nil {
			return brief.Input{}, err
		}
		src.NotSeen = notSeenN[bs.source]
		if ran || bs.always || src.NotSeen > 0 {
			in.Sources = append(in.Sources, src)
		}
	}
	in.Scoring = brief.ScoringNotRun
	score, err := q.LatestRunToday(ctx, "score", "", loc, in.Day)
	switch {
	case err == nil:
		in.Scoring = scoringState(score)
	case !errors.Is(err, store.ErrNotFound):
		return brief.Input{}, err
	}
	in.Recheck = brief.RecheckNotRun
	if !recheckOff {
		rc, err := q.LatestRunToday(ctx, linkedin.CommandRecheck, linkedin.Source, loc, in.Day)
		switch {
		case err == nil:
			in.Recheck = recheckState(rc)
		case !errors.Is(err, store.ErrNotFound):
			return brief.Input{}, err
		}
	}
	return in, nil
}

// scoreErrorSuffix ends the note of a score run that returned an error (see scoreRun).
const scoreErrorSuffix = " · error"

// scoringState reads a score run: a clean finish is ok, the cap and a busy provider name
// themselves, and a run that ended in an error, has not finished or stopped for any other
// reason failed.
func scoringState(r store.Run) string {
	switch {
	case r.FinishedAt == nil, strings.HasSuffix(r.Note, scoreErrorSuffix):
		return brief.ScoringFailed
	case r.StopReason == "":
		return brief.ScoringOK
	case r.StopReason == scoring.StopSpendCap:
		return brief.ScoringSpendCap
	case r.StopReason == scoring.StopThrottled:
		return brief.ScoringThrottled
	}
	return brief.ScoringFailed
}

// recheckState reads a recheck run: one that has not finished or whose note starts
// "failed: " failed, a clean finish is ok, and any other stop names itself.
func recheckState(r store.Run) string {
	switch {
	case r.FinishedAt == nil, strings.HasPrefix(r.Note, "failed: "):
		return brief.RecheckFailed
	case r.StopReason == "":
		return brief.RecheckOK
	}
	return r.StopReason
}

// briefJob maps one ranked candidate: each criterion's verdict with the keywords it
// matched, up to two evidence lines, and what is unclear (each flagged gate with its stored
// snippet unless it looks like a contact, then each unclear criterion).
func briefJob(c scoring.Candidate, url string, kws map[string][]string) brief.Job {
	j := brief.Job{
		ID: c.JobID, Title: c.Title, Company: c.Company, Location: c.Location, URL: url,
		ApplicantCount: c.ApplicantCount, PostedAt: c.PostedAtEst, ApplyType: c.ApplyType,
	}
	verdicts := map[string]string{}
	for _, cv := range c.Criteria {
		verdicts[cv.Criterion] = cv.Verdict
	}
	for _, name := range candidate.CriterionNames {
		v, ok := verdicts[name]
		if !ok {
			continue
		}
		j.Criteria = append(j.Criteria, brief.Criterion{Name: name, Verdict: v,
			Note: strings.Join(matchedKeywords(c.Description, kws[name]), ", ")})
	}
	j.Evidence = evidence(c, kws)
	for _, g := range c.Gates {
		if g.Verdict == gates.Flag {
			p := brief.Point{Name: g.Gate}
			if !contactLike.MatchString(g.Snippet) {
				p.Snippet = g.Snippet
			}
			j.Unclear = append(j.Unclear, p)
		}
	}
	for _, name := range candidate.CriterionNames {
		if verdicts[name] == scoring.Unclear {
			j.Unclear = append(j.Unclear, brief.Point{Name: name})
		}
	}
	return j
}

// contactLike matches what a brief never quotes: an email address or a profile link.
var contactLike = regexp.MustCompile(`@|/in/`)

// evidence is up to two distinct lines backing the criteria, in criterion order (the
// title criterion would only quote the title back). A line that looks like a contact is
// never quoted.
func evidence(c scoring.Candidate, kws map[string][]string) []string {
	ev := scoring.Evidence{Title: c.Title, Description: c.Description, Keywords: kws}
	for _, g := range c.Gates {
		ev.Gates = append(ev.Gates, store.GateResult{Gate: g.Gate, Verdict: g.Verdict, Snippet: g.Snippet})
	}
	var out []string
	for _, name := range candidate.CriterionNames {
		if name == "title" {
			continue
		}
		for _, l := range scoring.EvidenceLines(name, ev) {
			if len(out) == 2 {
				return out
			}
			if !contactLike.MatchString(l) && !slices.ContainsFunc(out, func(o string) bool { return overlaps(o, l) }) {
				out = append(out, l)
			}
		}
	}
	return out
}

// overlapRunes is how long a shared run of text makes two evidence lines the same passage.
const overlapRunes = 30

// overlaps reports whether two evidence lines quote the same passage: one holds the
// other's opening overlapRunes runes (ellipses aside). A posting with no line breaks gives
// windows of one long line, and quoting it twice says nothing new.
func overlaps(a, b string) bool {
	a, b = strings.Trim(a, "… "), strings.Trim(b, "… ")
	head := func(s string) string {
		r := []rune(s)
		return string(r[:min(len(r), overlapRunes)])
	}
	return strings.Contains(a, head(b)) || strings.Contains(b, head(a))
}

// matchedKeywords lists the keywords found in text as whole words, any case, in the
// keywords' own order and spelling, at most maxNoteKeywords.
func matchedKeywords(text string, keywords []string) []string {
	var out []string
	for _, k := range keywords {
		k = strings.TrimSpace(k)
		if k == "" || slices.Contains(out, k) {
			continue
		}
		re := regexp.MustCompile(`(?i)(?:^|[^\pL\pN])` + regexp.QuoteMeta(k) + `(?:$|[^\pL\pN])`)
		if re.MatchString(text) {
			out = append(out, k)
			if len(out) == maxNoteKeywords {
				break
			}
		}
	}
	return out
}

// sourceHealth is one source's state on day: paused when it was throttled or blocked that
// day, else read from its latest collect run, with the jobs first seen that day and, for a
// source that has one, the canary's outcome. ran reports whether the source collected that
// day at all.
func sourceHealth(ctx context.Context, q store.Queries, bs briefSource, loc *time.Location, day time.Time) (src brief.Source, ran bool, err error) {
	src = brief.Source{Name: bs.name, State: brief.StateNotRun}
	run, err := q.LatestRunToday(ctx, linkedin.CommandCollect, bs.source, loc, day)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return src, false, err
	default:
		ran = true
		src.State = collectState(run, bs.ats)
	}
	reason, paused, err := q.PausedToday(ctx, bs.source, loc, day)
	if err != nil {
		return src, ran, err
	}
	if paused {
		src.State = reason
	}
	if src.New, err = q.CountJobsFirstSeen(ctx, bs.source, loc, day); err != nil {
		return src, ran, err
	}
	if bs.ats != "" { // an ATS has no canary
		src.NoCanary = true
		return src, ran, nil
	}
	canary, err := q.LatestRunToday(ctx, linkedin.CommandCanary, bs.source, loc, day)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return src, ran, err
	case canaryPassed(canary):
		src.Canary = brief.CanaryOK
	default:
		src.Canary = brief.CanaryFailed
	}
	return src, ran, nil
}

// collectState reads a collect run: a clean finish is ok; a run that failed or has not
// finished did not finish; any other stop names itself. A spent budget is ok for LinkedIn,
// whose search ends on its page budget, but not for an ATS (atsName set), whose budget
// covers every board, so spending it means boards were left unfetched. An ATS's failed run
// notes "<ats>: failed: ".
func collectState(r store.Run, atsName string) string {
	switch {
	case r.FinishedAt == nil:
		return "still_running"
	case strings.HasPrefix(r.Note, "failed: "),
		atsName != "" && strings.HasPrefix(r.Note, atsName+": failed: "):
		return "failed"
	case r.StopReason == "", r.StopReason == "budget" && atsName == "":
		return brief.StateOK
	}
	return r.StopReason
}

// canaryPassed reads a canary run as linkedin.Canary writes it: a pass finishes with no
// stop reason and a card count; a page it never got has no stop reason but a "failed: "
// note.
func canaryPassed(r store.Run) bool {
	return r.FinishedAt != nil && r.StopReason == "" && !strings.HasPrefix(r.Note, "failed: ")
}
