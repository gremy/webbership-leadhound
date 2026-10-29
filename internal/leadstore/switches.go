package leadstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The scheduled jobs that have an on/off switch (job_switch, 00023). Sending is not one of
// them: it keeps send_pause as its switch.
const (
	JobBatch    = "batch"
	JobSync     = "sync"
	JobProbe    = "probe"
	JobContacts = "contacts"
	JobResearch = "research"
	JobVerify   = "verify"
)

// Jobs lists every switchable job, in the order the console shows them.
var Jobs = []string{JobBatch, JobSync, JobProbe, JobContacts, JobResearch, JobVerify}

// ErrUnknownJob is a job name that is not in Jobs.
var ErrUnknownJob = errors.New("leadstore: unknown job")

// JobSwitch is one job's switch: the latest job_switch row of the account's job. A job with
// no row is on, with an empty reason and actor and a zero At (never set).
type JobSwitch struct {
	Job    string
	On     bool
	Reason string
	Actor  string
	At     time.Time
}

func checkJob(job string) error {
	if !slices.Contains(Jobs, job) {
		return fmt.Errorf("%w: %q", ErrUnknownJob, job)
	}
	return nil
}

// JobSwitch reads one job's switch for the account (zero account = the default account).
func (q Queries) JobSwitch(ctx context.Context, account int64, job string) (JobSwitch, error) {
	if err := checkJob(job); err != nil {
		return JobSwitch{}, err
	}
	s := JobSwitch{Job: job, On: true}
	err := q.db.QueryRow(ctx,
		`SELECT is_on, reason, actor, at FROM job_switch WHERE account_id = $1 AND job = $2
		 ORDER BY id DESC LIMIT 1`, orAccount(account), job).Scan(&s.On, &s.Reason, &s.Actor, &s.At)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return JobSwitch{Job: job, On: true}, nil
	case err != nil:
		return JobSwitch{}, fmt.Errorf("leadstore: job switch %s: %w", job, err)
	}
	return s, nil
}

// JobSwitches reads every job's switch for the account, one per Jobs entry, in that order.
func (q Queries) JobSwitches(ctx context.Context, account int64) ([]JobSwitch, error) {
	out := make([]JobSwitch, 0, len(Jobs))
	for _, job := range Jobs {
		s, err := q.JobSwitch(ctx, account, job)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// SetJobSwitch turns a job on or off for the account. The table is append-only, so the
// switch's history stays; a write that would not change the job's on/off value is skipped
// (changed false), in the same statement that reads the current value.
func (q Queries) SetJobSwitch(ctx context.Context, account int64, job string, on bool, reason, actor string) (changed bool, err error) {
	if err := checkJob(job); err != nil {
		return false, err
	}
	if actor != ActorGremy && actor != ActorSystem {
		return false, fmt.Errorf("leadstore: set job switch: %w: %q", ErrUnknownActor, actor)
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO job_switch (account_id, job, is_on, reason, actor)
		 SELECT $1, $2, $3, $4, $5
		 WHERE coalesce((SELECT is_on FROM job_switch WHERE account_id = $1 AND job = $2
		                 ORDER BY id DESC LIMIT 1), true) IS DISTINCT FROM $3`,
		orAccount(account), job, on, reason, actor)
	if err != nil {
		return false, fmt.Errorf("leadstore: set job switch %s: %w", job, err)
	}
	return tag.RowsAffected() == 1, nil
}
