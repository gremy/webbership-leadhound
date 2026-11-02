package main

import (
	"context"
	"encoding/hex"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/console"
)

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

// NextJobs is the jobs to apply to next, from the briefs of the last days days. The store compares
// since as a calendar date in since's own zone, so it is the backend's local date. BriefDay stays
// as the store gives it, midnight UTC: the console formats it with no zone conversion.
func (b *backend) NextJobs(ctx context.Context, days, limit int) ([]console.NextJob, error) {
	jobs, err := b.s.Q().NextJobs(ctx, b.now().In(b.loc).AddDate(0, 0, -days), limit)
	if err != nil {
		return nil, err
	}
	out := make([]console.NextJob, 0, len(jobs))
	for _, j := range jobs {
		prior := make([]console.PriorJob, 0, len(j.Prior))
		for _, p := range j.Prior {
			prior = append(prior, console.PriorJob{JobID: p.JobID, Title: p.Title, Status: p.Status, Since: p.Since})
		}
		out = append(out, console.NextJob{JobID: j.JobID, Title: j.Title, Company: j.Company, Location: j.Location, Status: j.Status,
			ListingStatus: j.ListingStatus, URL: j.URL, ApplyURL: j.ApplyURL, ApplyType: j.ApplyType,
			BriefDay: j.BriefDay, BriefN: j.BriefN, Prior: prior})
	}
	return out, nil
}
