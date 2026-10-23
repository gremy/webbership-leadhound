// Package pipeline holds the steps every source shares once a posting has been fetched and
// parsed.
//
// Observe is the only way a job_observation is written: the LinkedIn collector, the recheck
// and the ATS collectors all call it, so every observation is sanitised the same way and
// always arrives with its gate results.
package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/postingguard"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// Input is what one fetch showed about one job, as its parser read it.
type Input struct {
	JobID     int64
	FetchID   int64
	DerivedBy string // the parser, e.g. "linkedin-parser@1"

	Title           string // Observe cleans it to one line
	Company         string // the company's name, for company rules; Observe cleans it to one line
	Location        string // Observe cleans it to one line
	DescriptionHTML string // raw description markup; Observe sanitises it
	Seniority       string
	EmploymentType  string
	Industries      string
	ApplicantCount  *int
	ApplicantText   string
	PostedText      string
	PostedAtEst     *time.Time
	ApplyType       string
	ApplyURL        string
	Closed          bool

	Gates gates.CandidateGates
}

// ObserveResult is what Observe wrote.
type ObserveResult struct {
	ObservationID int64
	Lang          string
	Gates         []gates.Result
}

// evaluate is gates.Evaluate; a test replaces it to prove a failed gate write rolls the
// observation back.
var evaluate = gates.Evaluate

// oneLine cleans a one-line field (title, company, location) with
// postingguard.CleanField: invisible runes removed and whitespace folded to one line, never
// quoted, so neither the stored value nor a gate snippet quoting it carries what a reader
// cannot see, and a title rule anchored with ^ still matches.
func oneLine(s string) string { return postingguard.CleanField(s) }

// Observe sanitises the posting, detects its language, and in one transaction of its own
// writes the job_observation and then one gate_result per gate verdict, using the active
// stored rules. also, when not nil, runs last in that same transaction with what was
// written (the collector writes its `new` status event there). Any error, also's
// included, rolls everything back, so an observation without its gate results can never
// be committed, and Observe cannot be run inside a caller's transaction that might not
// commit it.
func Observe(ctx context.Context, s *store.Store, in Input, also func(q store.Queries, res ObserveResult) error) (ObserveResult, error) {
	var res ObserveResult
	err := s.InTx(ctx, func(q store.Queries) error {
		var err error
		res, err = observeTx(ctx, q, in)
		if err != nil {
			return err
		}
		if also != nil {
			return also(q, res)
		}
		return nil
	})
	if err != nil {
		return ObserveResult{}, err
	}
	return res, nil
}

// observeTx is Observe's body, run on the transaction Observe opened.
func observeTx(ctx context.Context, q store.Queries, in Input) (ObserveResult, error) {
	title, company, location := oneLine(in.Title), oneLine(in.Company), oneLine(in.Location)
	clean := postingguard.Clean(in.DescriptionHTML, true)
	langText := clean.Visible
	if strings.TrimSpace(langText) == "" {
		langText = title // as the language gate does (see evaluate below)
	}
	lang, conf := gates.DetectLanguage(langText)

	stored, err := q.ActiveRules(ctx)
	if err != nil {
		return ObserveResult{}, fmt.Errorf("pipeline: observe job %d: %w", in.JobID, err)
	}
	rules := make([]gates.Rule, len(stored))
	for i, r := range stored {
		rules[i] = gates.Rule{ID: r.ID, Kind: r.Kind, Field: r.Field, Pattern: r.Pattern, Reason: r.Reason}
	}

	obsID, err := q.InsertObservation(ctx, store.ObservationInsert{
		JobID:            in.JobID,
		FetchID:          in.FetchID,
		DerivedBy:        in.DerivedBy,
		Title:            title,
		LocationText:     location,
		DescriptionClean: clean.Visible,
		GuardFindings:    clean.Findings.CountsJSON(),
		Lang:             lang,
		LangConfidence:   float32(conf),
		Seniority:        in.Seniority,
		EmploymentType:   in.EmploymentType,
		Industries:       in.Industries,
		ApplicantCount:   in.ApplicantCount,
		ApplicantText:    in.ApplicantText,
		PostedText:       in.PostedText,
		PostedAtEst:      in.PostedAtEst,
		ApplyType:        in.ApplyType,
		ApplyURL:         in.ApplyURL,
		Closed:           in.Closed,
	})
	if err != nil {
		return ObserveResult{}, fmt.Errorf("pipeline: observe job %d: %w", in.JobID, err)
	}

	// Every gate reads cleaned text only, so every snippet is cleaned text.
	// langText is the text the language gate reads (the cleaned description, or the cleaned
	// title when it is blank), so the gate reuses this detection rather than repeating it,
	// and the stored lang is the gate's basis.
	results := evaluate(gates.Observation{
		Title: title, Company: company, Location: location, Description: clean.Visible,
		Lang: lang, LangConfidence: conf,
	}, in.Gates, rules)
	if len(results) == 0 { // the language gate always runs; never commit an ungated observation
		return ObserveResult{}, fmt.Errorf("pipeline: observe job %d: the gates gave no result", in.JobID)
	}
	for _, r := range results {
		if _, err := q.InsertGateResult(ctx, store.GateResultInsert{
			JobID: in.JobID, ObservationID: obsID, Gate: r.Gate, Verdict: r.Verdict,
			Reason: r.Reason, Snippet: r.Snippet, RuleID: r.RuleID, DerivedBy: gates.DerivedBy,
		}); err != nil {
			return ObserveResult{}, fmt.Errorf("pipeline: observe job %d: %w", in.JobID, err)
		}
	}
	return ObserveResult{ObservationID: obsID, Lang: lang, Gates: results}, nil
}
