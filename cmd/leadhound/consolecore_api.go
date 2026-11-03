package main

import (
	"context"
	"encoding/hex"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// queueNeverBriefedDays is how far back the queue looks for jobs a skip rule keeps from a brief.
const queueNeverBriefedDays = 30

// consoleAPITokenHash checks LEADHOUND_API_TOKEN_SHA256 and returns it in the lowercase hex the
// console compares against: exactly 64 hex characters, either case; unset is "", which leaves the
// API off. The refusal never repeats the value.
func consoleAPITokenHash(v config.Secret) (string, error) {
	s := v.Reveal()
	if s == "" {
		return "", nil
	}
	if _, err := hex.DecodeString(s); err != nil || len(s) != 64 {
		return "", usageError("LEADHOUND_API_TOKEN_SHA256 must be 64 hex characters, the SHA-256 of the token; leave it unset to keep the API off")
	}
	return strings.ToLower(s), nil
}

// queueRules are the skip rules as the queue reads them: the ranker's own matching, with each
// rule's reason. CompileRules drops a broken pattern, a flag rule and a disabled rule, so none of
// them hides a job.
type queueRules struct {
	compiled scoring.CompiledRules
	reasons  map[int64]string
}

func newQueueRules(rules []store.Rule) queueRules {
	reasons := make(map[int64]string, len(rules))
	for _, r := range rules {
		reasons[r.ID] = r.Reason
	}
	return queueRules{compiled: scoring.CompileRules(rules), reasons: reasons}
}

// match is the reason of the first rule matching j's text, and whether one did.
func (r queueRules) match(j store.NextJob) (string, bool) {
	id := r.compiled.Match(scoring.Candidate{Company: j.Company, Title: j.Title, Description: j.Description, Location: j.Location})
	if id == nil {
		return "", false
	}
	return r.reasons[*id], true
}

// queueHidden is a job the rules hid. Briefed is false for one from the never-briefed read.
type queueHidden struct {
	store.NextJob
	Reason  string
	Briefed bool
}

// queueRescued reports a job whose latest status event is the operator's: `api mark ID maybe`
// writes one with the status unchanged, and no rule hides that job. Collectors write system.
func queueRescued(j store.NextJob) bool { return j.LatestActor != "system" }

// splitQueue divides the two reads by the rules. A briefed job is visible unless a rule matches
// it and it is not rescued. A never-briefed job no rule matches is ignored, because the ranker
// will brief it normally; a matched one is hidden, or listed after the briefed jobs when rescued,
// since otherwise a rescue would make it vanish from both lists.
func splitQueue(briefed, neverBriefed []store.NextJob, rules queueRules) (visible []store.NextJob, hidden []queueHidden) {
	visible = make([]store.NextJob, 0, len(briefed))
	for _, j := range briefed {
		if reason, ok := rules.match(j); ok && !queueRescued(j) {
			hidden = append(hidden, queueHidden{NextJob: j, Reason: reason, Briefed: true})
			continue
		}
		visible = append(visible, j)
	}
	for _, j := range neverBriefed {
		reason, ok := rules.match(j)
		switch {
		case !ok:
		case queueRescued(j):
			visible = append(visible, j)
		default:
			hidden = append(hidden, queueHidden{NextJob: j, Reason: reason})
		}
	}
	return visible, hidden
}

// readQueue reads the queue under rules: the jobs of the briefs since since, and those first seen
// since firstSeen that reached none of them. With no skip rule it reads neither the second list
// nor any description, so the answer is what it would be with no filter at all.
func readQueue(ctx context.Context, q store.Queries, since, firstSeen time.Time, rules []store.Rule) ([]store.NextJob, []queueHidden, error) {
	r := newQueueRules(rules)
	needDesc := r.compiled.NeedsDescription()
	briefed, err := q.NextJobs(ctx, since, needDesc)
	if err != nil {
		return nil, nil, err
	}
	if r.compiled.Empty() {
		return briefed, nil, nil
	}
	never, err := q.NeverBriefedJobs(ctx, since, firstSeen, needDesc, func(j store.NextJob) bool {
		_, ok := r.match(j)
		return ok
	})
	if err != nil {
		return nil, nil, err
	}
	visible, hidden := splitQueue(briefed, never, r)
	return visible, hidden, nil
}

// NextJobs is the jobs to apply to next, from the briefs of the last days days, under the active
// skip rules; limit cuts the visible jobs only, after the filter. The store compares since as a
// calendar date in since's own zone, so it is the backend's local date. BriefDay stays as the
// store gives it, midnight UTC: the console formats it with no zone conversion.
func (b *backend) NextJobs(ctx context.Context, days, limit int) (console.NextJobList, error) {
	q := b.s.Q()
	rules, err := q.ActiveRules(ctx)
	if err != nil {
		return console.NextJobList{}, err
	}
	now := b.now().In(b.loc)
	visible, hidden, err := readQueue(ctx, q, now.AddDate(0, 0, -days), now.AddDate(0, 0, -queueNeverBriefedDays), rules)
	if err != nil {
		return console.NextJobList{}, err
	}
	out := console.NextJobList{Jobs: []console.NextJob{}, Hidden: []console.HiddenJob{}}
	if len(visible) > limit {
		out.MoreVisible = len(visible) - limit
		visible = visible[:limit]
	}
	for _, j := range visible {
		prior := make([]console.PriorJob, 0, len(j.Prior))
		for _, p := range j.Prior {
			prior = append(prior, console.PriorJob{JobID: p.JobID, Title: p.Title, Status: p.Status, Since: p.Since})
		}
		out.Jobs = append(out.Jobs, console.NextJob{JobID: j.JobID, Title: j.Title, Company: j.Company, Location: j.Location, Status: j.Status,
			ListingStatus: j.ListingStatus, URL: j.URL, ApplyURL: j.ApplyURL, ApplyType: j.ApplyType,
			BriefDay: j.BriefDay, BriefN: j.BriefN, Prior: prior})
	}
	for _, h := range hidden {
		out.Hidden = append(out.Hidden, console.HiddenJob{JobID: h.JobID, Company: h.Company, Title: h.Title, Reason: h.Reason, Briefed: h.Briefed})
	}
	return out, nil
}
