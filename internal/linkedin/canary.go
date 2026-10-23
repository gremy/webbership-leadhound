package linkedin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// CommandCanary is run.command for the canary.
const CommandCanary = "canary"

// CanaryMinCards is how many parsed cards a canary needs to pass.
const CanaryMinCards = 5

// Canary makes one search request a day to prove the parser still reads LinkedIn's
// markup. A 200 HTML page it cannot read at least CanaryMinCards cards from records
// parser_broke; a page it never got (a transport error, another status, a body over the
// limit) records no stop reason and a "failed: ..." note, and a breaker stop keeps its own
// reason.
type Canary struct {
	Store     *store.Store
	NewClient func(rec fetch.Recorder) *fetch.Client // called once per run
	Now       func() time.Time                       // nil: time.Now
	Loc       *time.Location                         // LEADHOUND_TZ; nil: UTC
	Log       *slog.Logger                           // nil: slog.Default()
	Source    string                                 // as Collector.Source; "": Source
}

// CanaryResult is what one canary did.
type CanaryResult struct {
	RunID      int64 // 0 when skipped
	Skipped    bool  // a canary already ran today; nothing was requested or written
	Paused     bool  // the source is paused today; a run was recorded, nothing requested
	Passed     bool
	Cards      int
	StopReason string
	Note       string
}

// Run searches keywords in location once (last 24 hours, page 1). A failed canary is a
// CanaryResult with StopReason set and a nil error.
func (c *Canary) Run(ctx context.Context, keywords, location string) (CanaryResult, error) {
	col := Collector{Now: c.Now, Loc: c.Loc, Log: c.Log, Source: c.Source} // for its defaults
	q := c.Store.Q()

	reason, paused, err := q.PausedToday(ctx, col.source(), col.loc(), col.now())
	if err != nil {
		return CanaryResult{}, fmt.Errorf("linkedin: canary: %w", err)
	}
	if paused {
		id, note, err := RecordPause(ctx, q, CommandCanary, col.source(), reason)
		if err != nil {
			return CanaryResult{}, fmt.Errorf("linkedin: canary: %w", err)
		}
		col.log().Info("canary paused", "reason", reason)
		return CanaryResult{RunID: id, Paused: true, StopReason: reason, Note: note}, nil
	}

	ran, err := q.HasRunToday(ctx, CommandCanary, col.source(), col.loc(), col.now())
	if err != nil {
		return CanaryResult{}, fmt.Errorf("linkedin: canary: %w", err)
	}
	if ran {
		col.log().Info("canary already ran today")
		return CanaryResult{Skipped: true, Note: "already ran today"}, nil
	}

	runID, err := q.StartRun(ctx, CommandCanary, col.source())
	if err != nil {
		return CanaryResult{}, fmt.Errorf("linkedin: canary: %w", err)
	}
	client := c.NewClient(store.NewFetchRecorder(q, runID, col.source()))
	res := CanaryResult{RunID: runID}

	u := SearchURL(keywords, location, tprSteady, 1)
	resp, err := client.Get(ctx, fetch.KindCanary, u)
	var open *fetch.OpenError
	switch {
	case errors.As(err, &open):
		res.StopReason, res.Note = string(open.Reason), "fetching stopped: "+string(open.Reason)
	case err != nil || resp.Status != http.StatusOK || !isHTML(resp.ContentType):
		res.Note = "failed: " + failureCause(resp, err)
	default:
		cards, perr := ParseSearch(resp.Body)
		res.Cards = len(cards)
		switch {
		case perr != nil:
			res.StopReason, res.Note = stopReasonParserBroke, "parser broke: "+perr.Error()
		case len(cards) < CanaryMinCards:
			res.StopReason = stopReasonParserBroke
			res.Note = fmt.Sprintf("%d cards, fewer than %d", len(cards), CanaryMinCards)
		default:
			res.Passed, res.Note = true, fmt.Sprintf("%d cards", len(cards))
		}
	}

	finishErr := q.FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{
		Requests: client.Breaker.Requests(), Failures: client.Breaker.Failures(),
		StopReason: res.StopReason, Note: res.Note,
	})
	if ctx.Err() != nil && err != nil {
		return res, errors.Join(fmt.Errorf("linkedin: canary: %w", err), finishErr)
	}
	if finishErr != nil {
		return res, fmt.Errorf("linkedin: canary: %w", finishErr)
	}
	if res.Passed {
		col.log().Info("canary passed", "cards", res.Cards)
	} else {
		col.log().Warn("canary failed", "stop_reason", res.StopReason, "note", res.Note)
	}
	return res, nil
}

// isHTML reports whether a response's content type is HTML; a missing one counts as HTML.
func isHTML(contentType string) bool {
	return contentType == "" || strings.Contains(strings.ToLower(contentType), "html")
}

// failureCause is the short cause of a canary that got no page to read.
func failureCause(resp fetch.Response, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err.Error()
	case errors.Is(err, fetch.ErrBodyTooLarge):
		return fmt.Sprintf("status %d, body over the size limit", resp.Status)
	case resp.Status == 0 && err != nil:
		return "no response: " + err.Error()
	case resp.Status != http.StatusOK:
		return fmt.Sprintf("status %d", resp.Status)
	default:
		return fmt.Sprintf("status %d, not an HTML page (%s)", resp.Status, resp.ContentType)
	}
}
