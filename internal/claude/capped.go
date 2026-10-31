package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
)

// Worst-case allowances. Input tokens are bounded by bytes (no tokenizer yields more tokens
// than UTF-8 bytes for ordinary text), plus a fixed overhead for the tool definitions and
// the structured-output instructions the API adds; every input token is priced as a cache
// write, the dearest input rate.
const (
	inputOverheadTokens   = 4096
	SearchResultAllowance = 5000 // input tokens one web search can bring back
)

// Capped wraps a Caller with Claude spend caps in micro-dollars, counted in ai_usage on the
// calendar day in Loc. Before each call (and each pause_turn continuation) it computes that
// call's worst-case cost and refuses with ai.ErrSpendCap, without calling, if today's spend
// for Purpose plus the worst case would pass DailyCapMicros, today's spend across every
// Claude purpose would pass TotalCapMicros (when set), or the lead's spend for Purpose would
// pass LeadCapMicros (when the request names a lead). After a billed call it records the
// real cost. Calls through one Capped run one at a time; when Store is a UsageLocker the
// read, the call and the record run under its lock, so processes cannot overshoot together.
//
// A request sent with no answer back (ErrNoAnswer: a timeout or a dropped connection) may
// still have been billed, so it is recorded provisionally at that request's worst case: a
// row with no tokens and that cost (UsageRow.Provisional). The cap then counts it like any
// other spend. A 4xx, 429 or 5xx answer is not billed and records nothing.
//
// A research continuation refused by the caps after an earlier leg was billed is
// ErrCappedMidTurn, not ai.ErrSpendCap: money was spent, so the caller must not treat it as
// a pre-flight refusal and ask again.
//
// A zero DailyCapMicros or LeadCapMicros refuses every call it applies to. A failed usage
// read or write is ai.ErrUsageStore and the caller must stop.
//
// OnLimit, when set, is told in one plain line each time Claude cannot be called for money's
// sake: the daily cap of Purpose or the total cap refused a call, or the API answered that the
// account's spend limit or credit is used up (ErrSpendLimit, ErrCreditLow). It is how such a
// refusal reaches the console's Status page, which holds no key. A lead's own cap is not told
// (the next lead may go), nor a zero cap (that is a purpose switched off, not a limit reached).
// nil tells no one.
type Capped struct {
	Client         Caller
	Store          UsageStore
	AccountID      int64
	Purpose        string // PurposeDrafting, PurposeResearch or PurposeDraftsEval
	DailyCapMicros int64
	TotalCapMicros int64 // 0 = no ceiling across purposes
	LeadCapMicros  int64
	Loc            *time.Location
	Now            func() time.Time // time.Now when nil
	OnLimit        func(ctx context.Context, msg string)

	mu sync.Mutex
}

var _ Caller = (*Capped)(nil)

// ErrCappedMidTurn is a research turn stopped by the caps before a pause_turn continuation,
// after earlier legs were billed (and recorded). It deliberately does not wrap
// ai.ErrSpendCap, which means nothing was called.
var ErrCappedMidTurn = errors.New("claude: spend cap reached mid-turn, after billed legs")

// WorstWrite is the most r can cost.
func WorstWrite(r WriteRequest) (int64, error) {
	in := len(r.System) + len(r.Prompt) + schemaBytes(r.Schema) + inputOverheadTokens
	return Cost(Usage{Model: r.Model, CacheWriteTokens: in, OutputTokens: r.MaxTokens})
}

// WorstResearchLeg is the most leg number leg (0 = the first request, 1 and 2 the
// pause_turn continuations) of r can cost: its prompt, everything earlier legs and this leg
// can retrieve (max_uses × SearchResultAllowance per search, max_uses × max_content_tokens
// per fetch), earlier legs' output, max_tokens of output and max_uses searches.
func WorstResearchLeg(r ResearchRequest, leg int) (int64, error) {
	retrieved := r.SearchMaxUses*SearchResultAllowance + r.FetchMaxUses*r.FetchMaxContentTokens
	in := len(r.System) + len(r.Prompt) + inputOverheadTokens + (leg+1)*retrieved + leg*r.MaxTokens
	return Cost(Usage{Model: r.Model, CacheWriteTokens: in, OutputTokens: r.MaxTokens, WebSearches: r.SearchMaxUses})
}

func schemaBytes(s map[string]any) int {
	b, err := json.Marshal(s)
	if err != nil {
		return len(fmt.Sprint(s)) * 2
	}
	return len(b)
}

// Write checks the caps, calls the wrapped Caller and records the cost.
func (c *Capped) Write(ctx context.Context, r WriteRequest) (WriteResult, Usage, error) {
	var res WriteResult
	w, werr := WorstWrite(r)
	u, err := c.run(ctx, r.LeadID, r.Model, func() (int64, error) { return w, werr },
		func(_ *budget) (Usage, int64, error) {
			var (
				u   Usage
				err error
			)
			res, u, err = c.Client.Write(ctx, r)
			return u, w, err
		})
	return res, u, err
}

// Research checks the caps before the first request and before each continuation, calls
// the wrapped Caller and records the cost of every leg billed.
//
// Each leg is its own request that re-sends the whole turn so far as input, so it is billed
// for all of it again: WorstResearchLeg(r, leg) is that one leg's worst case, and a
// continuation reserves it in full on top of what the earlier legs actually cost.
func (c *Capped) Research(ctx context.Context, r ResearchRequest) (ResearchResult, Usage, error) {
	var res ResearchResult
	u, err := c.run(ctx, r.LeadID, r.Model, func() (int64, error) { return WorstResearchLeg(r, 0) },
		func(b *budget) (Usage, int64, error) {
			sent := 0 // the leg in flight
			r.beforeLeg = func(leg int, sofar Usage) error {
				spent, err := costOf(sofar)
				if err != nil {
					return err
				}
				worst, err := WorstResearchLeg(r, leg)
				if err != nil {
					return err
				}
				if err := b.check(spent + worst); err != nil {
					// %v, not %w: earlier legs were billed, so this is not ai.ErrSpendCap.
					return fmt.Errorf("claude: research: continuation %d not sent: %v: %w", leg, err, ErrCappedMidTurn)
				}
				sent = leg
				return nil
			}
			var (
				u   Usage
				err error
			)
			res, u, err = c.Client.Research(ctx, r)
			// Leg 0 passed the same pricing in run, and later legs in beforeLeg.
			inFlight, _ := WorstResearchLeg(r, sent)
			return u, inFlight, err
		})
	return res, u, err
}

// costOf prices u, with a fake model costing nothing.
func costOf(u Usage) (int64, error) {
	if !u.billed() {
		return 0, nil
	}
	return Cost(u)
}

// limitReached tells OnLimit msg, when it is set.
func (c *Capped) limitReached(ctx context.Context, msg string) {
	if c.OnLimit != nil {
		c.OnLimit(context.WithoutCancel(ctx), msg)
	}
}

// budget is what the caps leave for this call, read once under the lock.
type budget struct {
	ctx                    context.Context
	c                      *Capped
	day                    time.Time
	purposeSpent, allSpent int64
	leadSpent              int64
	lead                   bool
}

// check refuses when spending extra more (on top of what was read) would pass a cap.
func (b *budget) check(extra int64) error {
	c := b.c
	if b.purposeSpent+extra > c.DailyCapMicros {
		if c.DailyCapMicros > 0 {
			c.limitReached(b.ctx, fmt.Sprintf("Daily cap reached on %s: %s spent of %s today", c.Purpose,
				dollars(b.purposeSpent), dollars(c.DailyCapMicros)))
		}
		return fmt.Errorf("claude: %s: %s spent of %s on %s, next call up to %s: %w", c.Purpose,
			usd(b.purposeSpent), usd(c.DailyCapMicros), b.day.Format(time.DateOnly), usd(extra), ai.ErrSpendCap)
	}
	if c.TotalCapMicros > 0 && b.allSpent+extra > c.TotalCapMicros {
		c.limitReached(b.ctx, fmt.Sprintf("Daily cap across all Claude work reached: %s spent of %s today",
			dollars(b.allSpent), dollars(c.TotalCapMicros)))
		return fmt.Errorf("claude: all purposes: %s spent of %s on %s, next call up to %s: %w",
			usd(b.allSpent), usd(c.TotalCapMicros), b.day.Format(time.DateOnly), usd(extra), ai.ErrSpendCap)
	}
	if b.lead && b.leadSpent+extra > c.LeadCapMicros {
		return fmt.Errorf("claude: %s: lead spent %s of %s, next call up to %s: %w", c.Purpose,
			usd(b.leadSpent), usd(c.LeadCapMicros), usd(extra), ai.ErrSpendCap)
	}
	return nil
}

func usd(micros int64) string { return fmt.Sprintf("$%.4f", float64(micros)/1e6) }

// dollars is micros in whole cents, for a line the operator reads.
func dollars(micros int64) string { return fmt.Sprintf("$%.2f", float64(micros)/1e6) }

// run checks the caps against worst(), then runs call, which returns the usage billed, the
// worst case of the request in flight when it failed (recorded provisionally when the error
// is ErrNoAnswer) and the call's error.
func (c *Capped) run(ctx context.Context, leadID int64, model string, worst func() (int64, error),
	call func(*budget) (Usage, int64, error)) (Usage, error) {
	if c.Loc == nil {
		return Usage{}, errors.New("claude: capped: nil location")
	}
	switch c.Purpose {
	case PurposeDrafting, PurposeResearch, PurposeDraftsEval:
	default:
		return Usage{}, fmt.Errorf("claude: capped: unknown purpose %q", c.Purpose)
	}
	w, err := worst()
	if err != nil {
		return Usage{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	local := now().In(c.Loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.Loc)

	var (
		usage   Usage
		callErr error // the cap's or the call's outcome, returned as it is
	)
	// body returns only usage-store failures, so a locker keeps the recorded row whatever
	// the call said.
	body := func(st UsageStore) error {
		b := &budget{ctx: ctx, c: c, day: day, lead: leadID != 0}
		var err error
		if b.purposeSpent, err = st.ClaudeSpendOn(ctx, c.AccountID, c.Purpose, day); err != nil {
			return fmt.Errorf("claude: capped: read usage: %w: %w", ai.ErrUsageStore, err)
		}
		if c.TotalCapMicros > 0 {
			if b.allSpent, err = st.ClaudeSpendOn(ctx, c.AccountID, "", day); err != nil {
				return fmt.Errorf("claude: capped: read usage: %w: %w", ai.ErrUsageStore, err)
			}
		}
		if b.lead {
			if b.leadSpent, err = st.ClaudeSpendForLead(ctx, leadID, c.Purpose); err != nil {
				return fmt.Errorf("claude: capped: read lead usage: %w: %w", ai.ErrUsageStore, err)
			}
		}
		if callErr = b.check(w); callErr != nil {
			return nil
		}
		var inFlight int64
		usage, inFlight, callErr = call(b)
		if line := accountLimitLine(callErr); line != "" {
			c.limitReached(ctx, line)
		}
		// Records outlive a cancelled context: the money was spent either way.
		rctx := context.WithoutCancel(ctx)
		if usage.billed() && usage.Model != FakeModel {
			cost, err := Cost(usage)
			if err != nil {
				// A billed call on a model with no price: stop rather than count it as free.
				return fmt.Errorf("claude: capped: price usage: %w: %w", ai.ErrUsageStore, err)
			}
			row := UsageRow{
				AccountID: c.AccountID, LeadID: leadID, Purpose: c.Purpose, Model: usage.Model, Day: day,
				InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
				CacheReadTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheWriteTokens,
				WebSearches: usage.WebSearches, CostMicros: cost, Batch: usage.Batch,
			}
			if err := st.RecordClaudeUsage(rctx, row); err != nil {
				// The call was billed but not counted; stopping keeps the cap from under-counting.
				return fmt.Errorf("claude: capped: record usage: %w: %w", ai.ErrUsageStore, err)
			}
		}
		if errors.Is(callErr, ErrNoAnswer) && inFlight > 0 && model != FakeModel {
			row := UsageRow{AccountID: c.AccountID, LeadID: leadID, Purpose: c.Purpose, Model: model, Day: day,
				CostMicros: inFlight}
			if err := st.RecordClaudeUsage(rctx, row); err != nil {
				return fmt.Errorf("claude: capped: record provisional usage: %w: %w", ai.ErrUsageStore, err)
			}
		}
		return nil
	}

	if l, ok := c.Store.(UsageLocker); ok {
		err = l.WithClaudeUsageLock(ctx, body)
		if err != nil && !errors.Is(err, ai.ErrUsageStore) {
			err = fmt.Errorf("claude: capped: usage lock: %w: %w", ai.ErrUsageStore, err)
		}
	} else {
		err = body(c.Store)
	}
	if err != nil {
		return Usage{}, err
	}
	return usage, callErr
}
