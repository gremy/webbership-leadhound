package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

func init() { leadsCommands["calls"] = runLeadsCalls }

const leadsCallsUsage = "usage: leadhound leads calls list [--campaign SLUG] [--due] | outcome MESSAGE " +
	"connected|no_answer|voicemail|wrong_number|do_not_call|skipped [--note TEXT] [--consent] [--consent-address A]"

// callConsentWording is the sentence stored with an agreement won on a call: the call card's
// own consent line (internal/console/templates/lead_call.html), word for word.
const callConsentWording = "They agreed to get an email"

// runLeadsCalls is `leads calls list | outcome`.
func runLeadsCalls(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError(leadsCallsUsage)
	}
	switch args[0] {
	case "list":
		return runCallsList(ctx, args[1:])
	case "outcome":
		return runCallsOutcome(ctx, args[1:])
	}
	return usageError("unknown calls subcommand %q; %s", args[0], leadsCallsUsage)
}

// runCallsList is `leads calls list [--campaign S] [--due]`: the scheduled call steps (with
// --due, only those due now), oldest due first, one line each. It never prints a number or a
// note: the call card holds them.
func runCallsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads calls list", flag.ContinueOnError)
	slug := fs.String("campaign", "", "only this campaign's calls (default every campaign)")
	due := fs.Bool("due", false, "only the calls due now")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError(leadsCallsUsage)
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	var campaignID int64
	if *slug != "" {
		c, err := activeCampaign(ctx, lq, *slug)
		if err != nil {
			return err
		}
		campaignID = c.ID
	}
	loc, err := leadsZone()
	if err != nil {
		return err
	}
	return listCalls(ctx, lq, setup.leads, loc, leadsNow(), campaignID, *due, stdout)
}

// listCalls prints the call steps of campaign campaignID (0: every campaign) scheduled at
// any time, or with due only those due at now.
func listCalls(ctx context.Context, lq leadstore.Queries, _ leadconfig.Config, loc *time.Location, now time.Time,
	campaignID int64, due bool, w io.Writer) error {
	until := now
	if !due {
		until = now.AddDate(10, 0, 0)
	}
	calls, err := lq.CallsDue(ctx, leadstore.DefaultAccountID, campaignID, until)
	if err != nil {
		return err
	}
	slog.Default().Info("leads calls list", "calls", len(calls), "campaign", campaignID, "due_only", due)
	if len(calls) == 0 {
		if due {
			_, _ = fmt.Fprintln(w, "No call is due now.")
		} else {
			_, _ = fmt.Fprintln(w, "No call is scheduled.")
		}
		return nil
	}
	rules, err := loadCallRules(ctx, lq)
	if err != nil {
		return err
	}
	for _, c := range calls {
		v, err := rules.check(ctx, c, now)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("message %d · %s · step %d · due %s", c.MessageID, callOrgWords(c), c.Step+1,
			c.DueAt.In(loc).Format("Mon 2 Jan 15:04"))
		switch {
		case c.WaitsOnStep != nil:
			line += " · not due yet: waits on " + waitsOnStepWords(*c.WaitsOnStep, c.WaitsOnChannel)
		case v.Callable:
			line += " · callable now"
		default:
			line += " · not callable: " + console.AfterColon(v.Reason)
		}
		if c.Shadow {
			line += " · shadow batch"
		}
		if c.Outcome != "" {
			line += " · last: " + console.OutcomeWords(c.Outcome)
		}
		if v.CampaignOff != "" {
			line += " · " + callsOffShort(c.Country) + ": your call"
		}
		_, _ = fmt.Fprintln(w, line)
	}
	_, _ = fmt.Fprintf(w, "After a call: leadhound leads calls outcome MESSAGE %s\n", strings.Join(campaign.Outcomes, "|"))
	return nil
}

// callOrgWords is "Org name (domain)", or the domain alone when the org has no name.
func callOrgWords(c leadstore.CallRow) string {
	if n := strings.TrimSpace(c.OrgName); n != "" && n != c.OrgDomain {
		return n + " (" + c.OrgDomain + ")"
	}
	return c.OrgDomain
}

// runCallsOutcome is `leads calls outcome MESSAGE OUTCOME [--note TEXT] [--consent]
// [--consent-address A]`: it records what happened on call MESSAGE and moves the lead's
// sequence on. A refusal (not a call, not due yet, the sequence ended, already recorded)
// is a usage error (exit 2) naming why; nothing is written then.
func runCallsOutcome(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads calls outcome", flag.ContinueOnError)
	note := fs.String("note", "", "one line on how it went (at most 500 characters)")
	consent := fs.Bool("consent", false, "connected only: they agreed to get an email at the address on file")
	address := fs.String("consent-address", "", "connected only: they agreed to get an email at this address")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageError(leadsCallsUsage)
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id <= 0 {
		return usageError("MESSAGE must be a message number; %s", leadsCallsUsage)
	}
	outcome := pos[1]
	if !slices.Contains(campaign.Outcomes, outcome) {
		return usageError("outcome %q must be one of %s", outcome, strings.Join(campaign.Outcomes, ", "))
	}
	agreed := *consent || strings.TrimSpace(*address) != ""
	if agreed && outcome != campaign.OutcomeConnected {
		return usageError("--consent and --consent-address go only with connected")
	}
	s, setup, err := leadsOpen(ctx, leadsForOutreach)
	if err != nil {
		return err
	}
	defer s.Close()
	loc, err := leadsZone()
	if err != nil {
		return err
	}
	line, err := recordCall(ctx, leadstore.NewQueries(s.Pool()), setup.leads, leadsNow(), loc, id, outcome, *note, agreed, *address)
	var r *callRefusalError
	switch {
	case errors.As(err, &r):
		return usageError("%s", r.words)
	case errors.Is(err, errCallNotFound):
		return usageError("Message %d is not a call. Nothing was saved.", id)
	case err != nil:
		return err
	}
	_, _ = fmt.Fprintln(stdout, line)
	return nil
}

// errCallNotFound is recordCall's error for a message that is not a call of the account.
var errCallNotFound = errors.New("no such call")

// callRefusalError is an outcome the store refused, in plain words; nothing was written.
type callRefusalError struct{ words string }

func (e *callRefusalError) Error() string { return e.words }

// recordCall records outcome on call id at now with the lead's own calendar (its country's
// zone and the business hours), consent (connected only) at address ("" for the lead's own
// email address), and returns the line saying what happens next, its times in the operator's
// zone opLoc (as every other time the console and the CLI show). A refusal is a
// *callRefusalError; a message that is no call of the account wraps errCallNotFound. It logs
// ids and counts only, never the number or the note.
func recordCall(ctx context.Context, lq leadstore.Queries, cfg leadconfig.Config, now time.Time, opLoc *time.Location,
	id int64, outcome, note string, consent bool, address string) (string, error) {
	c, err := lq.CallByID(ctx, leadstore.DefaultAccountID, id)
	if errors.Is(err, leadstore.ErrMessageNotFound) {
		return "", fmt.Errorf("leads calls: message %d: %w", id, errCallNotFound)
	}
	if err != nil {
		return "", err
	}
	var in *leadstore.ConsentInput
	if consent {
		addr := strings.TrimSpace(address)
		if addr == "" {
			if addr, err = leadEmail(ctx, lq, c.LeadID); err != nil {
				return "", err
			}
			if addr == "" {
				return "", &callRefusalError{"There is no email address on file for this lead: type the address they gave. Nothing was saved."}
			}
		}
		in = &leadstore.ConsentInput{Address: addr, Wording: callConsentWording}
	}
	loc, bh := cfg.Policy(c.Country).Location(), cfg.Schedule.BusinessHours
	cal := campaign.CalendarFunc(func(from time.Time, n int) time.Time { return outreach.FollowupDue(from, n, loc, bh) })
	rec, err := lq.RecordCallOutcome(ctx, leadstore.DefaultAccountID, id, outcome, strings.TrimSpace(note), in, leadstore.ActorGremy, now, cal)
	if words := callRefusalWords(err); words != "" {
		slog.Default().Info("leads calls outcome refused", "message", id, "outcome", outcome)
		return "", &callRefusalError{words}
	}
	if err != nil {
		return "", err
	}
	slog.Default().Info("leads calls outcome", "message", id, "lead", rec.LeadID, "outcome", outcome,
		"due", len(rec.Due), "cancelled", len(rec.Cancelled), "suppressed", rec.Suppressed, "consent", rec.ConsentID > 0)
	if rec.Suppressed || outcome == campaign.OutcomeWrongNumber {
		// The do-not-call suppression or the bad number may have taken a call out of another
		// lead's sequence: move those on now, each with its own calendar.
		if _, err := lq.HealSequences(ctx, now, leadCalendarOf(cfg)); err != nil {
			return "", err
		}
	}
	steps, err := lq.SequenceOf(ctx, rec.LeadID)
	if err != nil {
		return "", err
	}
	return outcomeLine(rec, steps, c.Step, opLoc), nil
}

// leadCalendarOf is the business-day calendar of a lead in country under cfg (its
// country's zone and the business hours), as send and recordCall build it.
func leadCalendarOf(cfg leadconfig.Config) leadstore.LeadCalendar {
	return func(_, country string) campaign.Calendar {
		loc, bh := cfg.Policy(country).Location(), cfg.Schedule.BusinessHours
		return campaign.CalendarFunc(func(from time.Time, n int) time.Time { return outreach.FollowupDue(from, n, loc, bh) })
	}
}

// leadEmail is lead id's email route, "" when it has none.
func leadEmail(ctx context.Context, lq leadstore.Queries, id int64) (string, error) {
	l, err := lq.LeadByID(ctx, id)
	if err != nil {
		return "", err
	}
	routes, err := lq.RoutesFor(ctx, l.OrgID)
	if err != nil {
		return "", err
	}
	for _, r := range routes {
		if r.Kind == leadstore.RouteEmail && (l.RouteID == nil || r.ID == *l.RouteID) {
			return r.Value, nil
		}
	}
	return "", nil
}

// callRefusalWords is a store refusal of an outcome in plain words; "" when err is none.
func callRefusalWords(err error) string {
	why := func() string {
		msg := err.Error()
		if i := strings.LastIndex(msg, ": "); i >= 0 {
			msg = msg[:i]
		}
		if i := strings.LastIndex(msg, ": "); i >= 0 {
			msg = msg[i+2:]
		}
		return msg
	}
	switch {
	case err == nil:
		return ""
	case errors.Is(err, leadstore.ErrNotCall):
		return "That message is not a call. Nothing was saved."
	case errors.Is(err, leadstore.ErrNotDue):
		var nd *leadstore.NotDueError
		switch {
		case errors.As(err, &nd) && nd.WaitsOnStep >= 0:
			return notDueWaitsWords(nd.WaitsOnStep, nd.WaitsOnChannel) + " Nothing was saved."
		case errors.As(err, &nd) && nd.DueAt == nil:
			return "Not due yet: this call waits on an earlier step. Nothing was saved."
		}
		return "Not due yet. Nothing was saved."
	case errors.Is(err, leadstore.ErrSequenceEnded):
		if strings.Contains(err.Error(), "They replied") {
			return "They replied: the sequence has ended. Nothing was saved."
		}
		return "The sequence has ended (" + strings.ReplaceAll(why(), "_", " ") + "). Nothing was saved."
	case errors.Is(err, leadstore.ErrCallAdvanced):
		msg := err.Error()
		first := ""
		if i := strings.Index(msg, "already recorded as "); i >= 0 {
			first, _, _ = strings.Cut(msg[i+len("already recorded as "):], ":")
		}
		return "Already recorded as " + console.OutcomeWords(first) + ", and the sequence has moved on. Nothing was saved."
	case errors.Is(err, leadstore.ErrMessageState):
		return "That call is not waiting for an outcome (its batch is not approved, or it was left out). Nothing was saved."
	case errors.Is(err, leadstore.ErrBadOutcome):
		switch {
		case strings.Contains(err.Error(), "note"):
			return "The note is longer than 500 characters; shorten it. Nothing was saved."
		case strings.Contains(err.Error(), "address"):
			return "That email address does not look right. Nothing was saved."
		}
		return "That outcome cannot be recorded. Nothing was saved."
	}
	return ""
}

// outcomeLine says what was saved and what the sequence did after it, read from the lead's
// steps as they stand after the outcome (steps; callStep is the call's step, loc the
// operator's zone): the outcome in the buttons' own words, then the next step after the call and when it is due, or that the sequence ended.
func outcomeLine(rec leadstore.CallRecorded, steps []leadstore.SequenceStep, callStep int, loc *time.Location) string {
	words := "Saved: " + console.OutcomeWords(rec.Outcome) + "."
	if rec.Shadow {
		words += " (shadow batch)"
	}
	words += " " + afterCallWords(steps, callStep, loc)
	if rec.Suppressed {
		words += " Their number is on the do-not-call list now."
	}
	if rec.ConsentID > 0 {
		words += " Their agreement to get an email is recorded."
	}
	return words
}

// afterCallWords is what happens after call step callStep, from the steps that follow it: the
// first one still to go and when it is due, else that the sequence ended (with what it
// cancelled).
func afterCallWords(steps []leadstore.SequenceStep, callStep int, loc *time.Location) string {
	final := -1
	for _, st := range steps {
		final = max(final, st.Step)
	}
	cancelled, cancelledCall := 0, false
	for _, st := range steps {
		if st.Step <= callStep || st.MessageID == 0 {
			continue
		}
		switch st.State {
		case leadstore.MsgApproved:
			name := stepName(st, final)
			if st.DueAt == nil {
				return name + " has no time yet; it is scheduled once the step before it is done."
			}
			return name + " is due " + st.DueAt.In(loc).Format("Mon 2 Jan 15:04") + "."
		case leadstore.MsgCancelled:
			cancelled++
			cancelledCall = cancelledCall || st.Type == campaign.StepCall
		}
	}
	switch {
	case cancelled > 0 && cancelledCall:
		return "The sequence has ended: the remaining steps are cancelled."
	case cancelled > 0:
		return "The sequence has ended: the remaining emails are cancelled."
	}
	return "The sequence has ended; nothing more goes to this lead."
}

// stepName names a step that follows a call, with a capital: the "tried to reach you" email,
// the final email, the next call, or the email at its 1-based step.
func stepName(st leadstore.SequenceStep, final int) string {
	switch {
	case st.Type == campaign.StepCall:
		return "The next call"
	case st.When == campaign.WhenCallNotConnected:
		return `The "tried to reach you" email`
	case st.Step == final:
		return "The final email"
	}
	return fmt.Sprintf("The email at step %d", st.Step+1)
}
