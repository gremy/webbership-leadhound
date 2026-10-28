package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

func init() { leadsCommands["calls"] = runLeadsCalls }

const leadsCallsUsage = "usage: leadhound leads calls list [--campaign SLUG] [--due] | outcome MESSAGE " +
	"connected|no_answer|voicemail|wrong_number|do_not_call|skipped [--note TEXT] [--consent] [--consent-address A]"

// callsOffWords is why no call may be made yet (CP3): the call rules (the per-country allow
// list and the screening) arrive later, so every call is blocked, while the cards, the lists
// and the outcomes still work so the flow can be walked in shadow.
const callsOffWords = "Calls are off until the call rules are set up"

// callScreeningWords is the call card's screening status while the call rules are not set up.
const callScreeningWords = "call rules not set up yet: calls are off"

// callConsentWording is the sentence stored with an agreement won on a call: the call card's
// own consent line (internal/console/templates/lead_call.html), word for word.
const callConsentWording = "They agreed to get an email"

// The call window: a call is "callable now" between these local times on a weekday, in the
// lead's country's zone.
const (
	callWindowStart = 9*60 + 30 // 09:30
	callWindowEnd   = 17 * 60   // 17:00
)

// inCallWindow reports whether now falls in the call window in loc.
func inCallWindow(now time.Time, loc *time.Location) bool {
	t := now.In(loc)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	return m >= callWindowStart && m < callWindowEnd
}

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
func listCalls(ctx context.Context, lq leadstore.Queries, cfg leadconfig.Config, loc *time.Location, now time.Time,
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
	_, _ = fmt.Fprintf(w, "%s: you can still record an outcome to walk the flow.\n", callsOffWords)
	if len(calls) == 0 {
		if due {
			_, _ = fmt.Fprintln(w, "No call is due now.")
		} else {
			_, _ = fmt.Fprintln(w, "No call is scheduled.")
		}
		return nil
	}
	pol, err := loadCallPolicies(ctx, lq)
	if err != nil {
		return err
	}
	for _, c := range calls {
		line := fmt.Sprintf("message %d · %s · step %d · due %s", c.MessageID, callOrgWords(c), c.Step+1,
			c.DueAt.In(loc).Format("Mon 2 Jan 15:04"))
		if !c.DueAt.After(now) && !inCallWindow(now, cfg.Policy(c.Country).Location()) {
			line += " · outside calling hours now (09:30 to 17:00 their time, weekdays)"
		}
		if c.Shadow {
			line += " · shadow batch"
		}
		if c.Outcome != "" {
			line += " · last: " + strings.ReplaceAll(c.Outcome, "_", " ")
		}
		if pol.off(c.CampaignID, c.Country) {
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
	line, err := recordCall(ctx, leadstore.NewQueries(s.Pool()), setup.leads, leadsNow(), id, outcome, *note, agreed, *address)
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
// email address), and returns the line saying what happens next. A refusal is a
// *callRefusalError; a message that is no call of the account wraps errCallNotFound. It logs
// ids and counts only, never the number or the note.
func recordCall(ctx context.Context, lq leadstore.Queries, cfg leadconfig.Config, now time.Time,
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
	steps, err := lq.SequenceOf(ctx, rec.LeadID)
	if err != nil {
		return "", err
	}
	return outcomeLine(rec, steps, cfg.Policy(c.Country).Location()), nil
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
		if strings.Contains(err.Error(), "waits on an earlier step") {
			return "Not due yet: this call waits on the email before it. Nothing was saved."
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
		return "Already recorded as " + strings.ReplaceAll(first, "_", " ") + ", and the sequence has moved on. Nothing was saved."
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

// outcomeLine says what was saved and what happens next: the steps that fell due (with
// their time in loc) and that the sequence ended, or that nothing is scheduled.
func outcomeLine(rec leadstore.CallRecorded, steps []leadstore.SequenceStep, loc *time.Location) string {
	words := "Saved: " + strings.ReplaceAll(rec.Outcome, "_", " ") + "."
	if rec.Shadow {
		words += " (shadow batch)"
	}
	byID := map[int64]leadstore.SequenceStep{}
	for _, st := range steps {
		if st.MessageID > 0 {
			byID[st.MessageID] = st
		}
	}
	var next []string
	for _, id := range slices.Sorted(maps.Keys(rec.Due)) {
		st := byID[id]
		kind := "the email"
		if st.Type == campaign.StepCall {
			kind = "the next call"
		}
		when := rec.Due[id]
		if t, err := time.Parse(time.RFC3339, when); err == nil {
			when = t.In(loc).Format("Mon 2 Jan 15:04")
		}
		next = append(next, fmt.Sprintf("%s at step %d is due %s", kind, st.Step+1, when))
	}
	pending := false
	for _, st := range steps {
		if st.State == leadstore.MsgApproved {
			pending = true
		}
	}
	switch {
	case len(next) > 0:
		words += " Next: " + strings.Join(next, "; ") + "."
	case pending:
		words += " The next step waits on the one before it."
	default:
		words += " The sequence has ended; nothing more goes to this lead."
	}
	if rec.Suppressed {
		words += " Their number is on the do-not-call list now."
	}
	if rec.ConsentID > 0 {
		words += " Their agreement to get an email is recorded."
	}
	return words
}
