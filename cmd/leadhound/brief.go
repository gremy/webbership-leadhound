package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/mail"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["brief"] = runBrief }

const briefUsage = "usage: leadhound brief [--send] [--day YYYY-MM-DD] [--allow-fake] [--outbox DIR]"

// defaultOutbox is where a brief that is not sent is written, relative to the working
// directory: the repository's gitignored data/ in development.
var defaultOutbox = filepath.Join("data", "outbox")

// outboxRecipient stands in when LEADHOUND_MAIL_TO is unset: the outbox checks a recipient
// but never writes it down.
const outboxRecipient = "brief@example.invalid"

// briefFromName is the sender's display name.
const briefFromName = "LeadHound"

// newMailer is the mailer --send uses; tests replace it.
var newMailer = func(cfg config.Config) mail.Mailer {
	return &mail.Resend{APIKey: cfg.ResendAPIKey, From: cfg.MailFrom}
}

// briefClients is how the brief reaches LinkedIn for its recheck; tests replace it.
var briefClients = linkedinClients

// runBrief is `leadhound brief [--send] [--day D] [--allow-fake] [--outbox DIR]`.
//
// It first rechecks the jobs about to be listed (only when LEADHOUND_LIVE allows
// LinkedIn; a failed recheck is a health line, not a stop) and leaves out any still
// unchecked, then composes the day's brief and prints it. Without --send it writes the
// brief to the outbox and nothing to brief_log. With --send it writes the day's brief_log
// row unsent, sends it with the idempotency key brief-<day>, and on success marks it sent
// and shortlists the listed jobs that were new. A day already sent is refused (exit 0);
// a failed send exits 1 and leaves the row unsent, so a re-run delivers.
//
// A re-run of a day with an unsent row is a retry: no recheck, no ranking, the stored
// brief resent as it is under its stored key, and its stored jobs marked. Only a day
// whose one try the provider refused (nothing went out) is composed afresh, without a
// recheck and under a new key.
func runBrief(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("brief", flag.ContinueOnError)
	send := fs.Bool("send", false, "send the brief by email and log it")
	dayFlag := fs.String("day", "", "the brief's day, YYYY-MM-DD (default today in LEADHOUND_TZ)")
	allowFake := fs.Bool("allow-fake", false, "rank with the fake decider's verdicts (development only)")
	outbox := fs.String("outbox", defaultOutbox, "where a brief that is not sent is written")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("%s", briefUsage)
	}
	required := []string{"LEADHOUND_DATABASE_URL", "LEADHOUND_DATA_DIR"}
	if *send {
		required = append(required, "RESEND_API_KEY", "LEADHOUND_MAIL_FROM", "LEADHOUND_MAIL_TO")
	}
	cfg, err := loadConfig(required...)
	if err != nil {
		return err
	}
	if *send {
		if mail.Check(cfg.MailTo.Reveal()) != nil {
			return usageError("LEADHOUND_MAIL_TO must be one plain address")
		}
		if mail.Check(cfg.MailFrom.Reveal()) != nil {
			return usageError("LEADHOUND_MAIL_FROM must be one plain address")
		}
	}
	prof, err := loadProfileDir(cfg.DataDir)
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return usageError("LEADHOUND_TZ: %v", err)
	}
	day := time.Now().In(loc)
	if *dayFlag != "" {
		if day, err = time.ParseInLocation(time.DateOnly, *dayFlag, loc); err != nil {
			return usageError("--day must be YYYY-MM-DD, not %q", *dayFlag)
		}
	}
	dayStr := day.Format(time.DateOnly)

	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer s.Close()
	q := s.Q()
	runID, err := q.StartRun(ctx, "brief", "")
	if err != nil {
		return err
	}
	note := "did not finish"
	defer func() {
		_ = q.FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{Note: note})
	}()

	refused := false // the day's one try was refused: compose afresh, recheck nothing
	if *send {
		prev, err := q.BriefByDay(ctx, dayStr)
		switch {
		case err == nil && prev.SentAt != nil:
			note = "already sent; nothing sent"
			_, _ = fmt.Fprintf(stdout, "brief: the brief for %s was already sent at %s; nothing sent\n", dayStr, prev.SentAt.In(loc).Format("15:04"))
			return nil
		case err == nil && prev.SendState != store.BriefRefused:
			_, _ = fmt.Fprint(stdout, prev.BodyText)
			_, _ = fmt.Fprintf(stdout, "\nbrief: resending the unsent brief of %s as it was composed\n", dayStr)
			return sendBrief(ctx, s, cfg, prev, &note)
		case err == nil:
			refused = true
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
	}

	// LI4: recheck what is about to be listed, live only, then leave out what is still
	// unchecked.
	recheckOff := true
	newClient, source, err := briefClients(cfg, fetchFlags{}, recheckBudgets(cfg))
	switch {
	case errors.Is(err, errLiveOff):
	case err != nil:
		return err
	default:
		recheckOff = false
		if !refused {
			out := recheckListed(ctx, s, cfg, prof, loc, day, *allowFake, newClient, source)
			_, _ = fmt.Fprintln(notes, out.summary())
		}
		if _, err := leaveOutStale(ctx, q, prof, loc, day, *allowFake, source); err != nil {
			return err
		}
	}

	in, err := buildInput(ctx, q, prof, loc, day, *allowFake, recheckOff)
	if err != nil {
		return err
	}
	in.ConsoleURL = briefConsoleURL(cfg.ConsoleURL)
	subject, text, html := brief.Compose(in)
	_, _ = fmt.Fprint(stdout, text)
	ids := in.JobIDs()

	if !*send {
		to := cfg.MailTo.Reveal()
		if to == "" {
			to = outboxRecipient
		}
		ob := &mail.Outbox{Dir: *outbox, Location: loc}
		name, err := ob.Send(ctx, mail.Message{To: to, Subject: subject, Text: text, HTML: html,
			FromName: briefFromName, IdempotencyKey: "brief-" + dayStr})
		if err != nil {
			return err
		}
		note = "dry run: " + name
		_, _ = fmt.Fprintf(stdout, "\nbrief: written to %s; not sent, not logged\n", filepath.Join(*outbox, name))
		return nil
	}

	key := "brief-" + dayStr
	if refused {
		// The refused try's key may be kept by the provider with its payload.
		key = fmt.Sprintf("brief-%s-r%d", dayStr, runID)
	}
	b, err := q.UpsertBrief(ctx, dayStr, store.BriefDraft{Subject: subject, Text: text, HTML: html, Key: key, JobIDs: ids})
	switch {
	case errors.Is(err, store.ErrBriefSent):
		note = "already sent; nothing sent"
		_, _ = fmt.Fprintf(stdout, "brief: the brief for %s was already sent; nothing sent\n", dayStr)
		return nil
	case errors.Is(err, store.ErrBriefPending):
		note = "another run wrote the brief first; nothing sent"
		return fmt.Errorf("brief %s: another run wrote this day's brief while this one composed; a re-run resends it: %w", dayStr, err)
	case err != nil:
		return err
	}
	return sendBrief(ctx, s, cfg, b, &note)
}

// briefConsoleURL is the console URL the footer names: u when it is a usable https URL,
// else "" (the footer names the track commands). An unusable one gets one notes line and
// never stops the brief.
func briefConsoleURL(u string) string {
	if u == "" {
		return ""
	}
	if config.ValidConsoleURL(u) != nil {
		_, _ = fmt.Fprintln(notes, "brief: LEADHOUND_CONSOLE_URL is not an https URL; the footer names the track commands")
		return ""
	}
	return u
}

// sendBrief sends the stored brief b as it is, under its stored key. On success, or when
// the provider says the key's first try went out, it marks the day sent and shortlists
// b's jobs that were new. A failure leaves the day unsent; one the provider refused on the
// payload's only try is recorded as refused, so the next run may compose afresh.
func sendBrief(ctx context.Context, s *store.Store, cfg config.Config, b store.BriefLog, note *string) error {
	q := s.Q()
	prevState, err := q.BeginBriefSend(ctx, b.Day)
	if err != nil {
		return err
	}
	m := newMailer(cfg)
	providerID, err := m.Send(ctx, mail.Message{To: cfg.MailTo.Reveal(), Subject: b.Subject, Text: b.BodyText, HTML: b.BodyHTML,
		FromName: briefFromName, IdempotencyKey: b.Key()})
	switch {
	case err == nil:
	case mail.AlreadySent(err):
		// The key's first send went out on an earlier try that did not get to record it.
		providerID = ""
	default:
		*note = "send failed: " + err.Error()
		if !mail.Uncertain(err) && prevState == store.BriefPending {
			if rerr := q.MarkBriefRefused(context.WithoutCancel(ctx), b.Day); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
		return fmt.Errorf("brief %s: send through %s failed; the brief stays unsent, so a re-run sends it: %w", b.Day, m.Provider(), err)
	}
	var shortlisted int
	err = s.InTx(ctx, func(tq store.Queries) error {
		if err := tq.MarkBriefSent(ctx, b.Day, providerID); err != nil {
			return err
		}
		var err error
		shortlisted, err = tq.ShortlistNew(ctx, b.JobIDs, "listed in the brief of "+b.Day)
		return err
	})
	if err != nil {
		return err
	}
	*note = fmt.Sprintf("sent through %s · %d listed · %d shortlisted", m.Provider(), len(b.JobIDs), shortlisted)
	_, _ = fmt.Fprintf(stdout, "\nbrief: %s\n", *note)
	return nil
}
