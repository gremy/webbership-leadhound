package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadbrief"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// The console's side of the jobs page, the rule forms and the leads pages: the backend reads
// and writes through the cores the commands run (trackcore.go, leadscore.go) and words what
// they did itself, never naming a command. leads.yaml is read on every leads request, so a
// server without it serves the jobs pages as before.

// The console's limits on what it lists and takes.
const (
	consoleListCap    = 200 // most manual items or emails to check the leads page lists
	consoleHistoryCap = 50  // most status changes the job page lists
	consoleQueue      = leadstore.NextUpPage
	consolePreview    = 20 // leads the priorities page previews
	consoleDeferDays  = 14 // the default push-back
	consoleMaxText    = 500
	consoleEvidence   = 3 // most evidence lines per criterion on the job page
	// consoleMailboxText is the longest address, login, server, selector, From name or
	// domain a Mailboxes form takes, as the form's maxlength.
	consoleMailboxText = 320
)

// refused is the console's refusal with words, wrapped as one the operator can fix.
func refused(format string, a ...any) error {
	return fmt.Errorf("%w: %s", console.ErrUserFixable, fmt.Sprintf(format, a...))
}

// Job is one job's whole record: its facts and current observation, each criterion's
// verdict with the keywords it matched and the lines that back it, what is unclear (flagged
// gates with their snippets unless they look like a contact, as the brief shows them), and
// its status history.
func (b *backend) Job(ctx context.Context, id int64) (console.JobPage, error) {
	q := b.s.Q()
	sum, err := q.JobSummaryByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return console.JobPage{}, console.ErrNotFound
	}
	if err != nil {
		return console.JobPage{}, err
	}
	p := console.JobPage{JobID: id, Title: sum.Title, Company: sum.Company, URL: sum.URL, ListingStatus: sum.ListingStatus, Status: sum.Status}
	if p.History, err = statusHistory(ctx, b.s.Pool(), id); err != nil {
		return console.JobPage{}, err
	}
	obs, err := q.LatestObservation(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return p, nil
	}
	if err != nil {
		return console.JobPage{}, err
	}
	gates, err := q.LatestGateResults(ctx, id)
	if err != nil {
		return console.JobPage{}, err
	}
	decisions, err := q.LatestDecisions(ctx, id)
	if err != nil {
		return console.JobPage{}, err
	}
	c := scoring.Candidate{JobID: id, Title: obs.Title, Company: sum.Company, Location: obs.LocationText, Description: obs.DescriptionClean,
		ListingStatus: sum.ListingStatus, Status: sum.Status, Closed: obs.Closed, PostedAtEst: obs.PostedAtEst,
		ApplicantCount: obs.ApplicantCount, ApplyType: obs.ApplyType}
	for _, g := range gates {
		c.Gates = append(c.Gates, scoring.GateVerdict{Gate: g.Gate, Verdict: g.Verdict, Reason: g.Reason, Snippet: g.Snippet})
	}
	for _, d := range decisions {
		c.Criteria = append(c.Criteria, scoring.CriterionVerdict{Criterion: d.Criterion, Verdict: d.Verdict, Reason: d.Reason})
	}
	kws := b.prof.Candidate.EvidenceKeywords
	j := briefJob(c, sum.URL, kws)
	p.Title, p.Location, p.ApplyURL = cmpOr(obs.Title, sum.Title), obs.LocationText, obs.ApplyURL
	p.ApplicantCount, p.PostedAt, p.ApplyType, p.Description = obs.ApplicantCount, obs.PostedAtEst, obs.ApplyType, obs.DescriptionClean
	ev := scoring.Evidence{Title: c.Title, Description: c.Description, Gates: gates, Keywords: kws}
	for _, cr := range j.Criteria {
		jc := console.JobCriterion{Name: cr.Name, Verdict: cr.Verdict, Keywords: cr.Note}
		for _, l := range scoring.EvidenceLines(cr.Name, ev) {
			if len(jc.Evidence) == consoleEvidence {
				break
			}
			if !contactLike.MatchString(l) && !slices.Contains(jc.Evidence, l) {
				jc.Evidence = append(jc.Evidence, l)
			}
		}
		p.Criteria = append(p.Criteria, jc)
	}
	for _, u := range j.Unclear {
		p.Unclear = append(p.Unclear, console.JobPoint{Name: u.Name, Snippet: u.Snippet})
	}
	return p, nil
}

// statusHistory is the job's latest consoleHistoryCap status changes, newest first.
func statusHistory(ctx context.Context, db store.DBTX, jobID int64) ([]console.StatusLine, error) {
	rows, err := db.Query(ctx,
		`SELECT status, reason, actor, at FROM status_event WHERE job_id = $1 ORDER BY at DESC, id DESC LIMIT $2`, jobID, consoleHistoryCap)
	if err != nil {
		return nil, fmt.Errorf("console: status history: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (console.StatusLine, error) {
		var l console.StatusLine
		err := r.Scan(&l.Status, &l.Reason, &l.Actor, &l.At)
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("console: status history: %w", err)
	}
	return out, nil
}

// AddRule adds a rule through ruleAdd, with the same checks and words as `rules add`, and
// starts the dashboard's rebuild, as a mark does.
func (b *backend) AddRule(ctx context.Context, req console.RuleRequest) (string, error) {
	if req.Kind == "" || req.Field == "" || req.Pattern == "" || strings.TrimSpace(req.Reason) == "" {
		return "", refused("A rule needs a kind, a field, a pattern and a reason.")
	}
	if utf8.RuneCountInString(req.Pattern) > consoleMaxText || utf8.RuneCountInString(req.Reason) > consoleMaxText {
		return "", refused("The pattern and the reason take at most %d characters each.", consoleMaxText)
	}
	id, err := ruleAdd(ctx, b.s, RuleAddRequest{Kind: req.Kind, Field: req.Field, Pattern: req.Pattern, Reason: req.Reason})
	if r := asRefusal(err); r != nil {
		return "", refused("%s", r.Detail)
	}
	if err != nil {
		return "", err
	}
	b.marked()
	return fmt.Sprintf("rule %d added", id), nil
}

// DisableRule disables rule id through ruleDisable, with the words of `rules disable`, and
// starts the dashboard's rebuild.
func (b *backend) DisableRule(ctx context.Context, id int64, reason string) (string, error) {
	if strings.TrimSpace(reason) == "" {
		return "", refused("Say why the rule is disabled.")
	}
	if utf8.RuneCountInString(reason) > consoleMaxText {
		return "", refused("The reason takes at most %d characters.", consoleMaxText)
	}
	err := ruleDisable(ctx, b.s, id, reason)
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseRuleNotFound:
			return "", refused("rule %d does not exist", id)
		case RefuseRuleDisabled:
			return "", refused("rule %d is already disabled; add it again to undo", id)
		default:
			return "", refused("%s", r.Detail)
		}
	}
	if err != nil {
		return "", err
	}
	b.marked()
	return fmt.Sprintf("rule %d disabled", id), nil
}

// leadsSetup reads leads.yaml and the outreach settings for one request. Any problem is
// ErrLeadsNotSetUp: the problems stay out of the page and the log (check-config names them).
func (b *backend) leadsSetup(load leadsLoad) (leadsSetup, error) {
	setup, err := leadsEnv(b.cfg, load)
	if err != nil {
		return leadsSetup{}, console.ErrLeadsNotSetUp
	}
	return setup, nil
}

// lq is the leads queries over the pool.
func (b *backend) lq() leadstore.Queries { return leadstore.NewQueries(b.s.Pool()) }

// leadsSummary is the dashboard's leads card, nil when leads are not set up here or their
// tables cannot be read; the jobs dashboard never fails for it.
func (b *backend) leadsSummary(ctx context.Context, now time.Time) *console.LeadsSummary {
	if _, err := b.leadsSetup(leadsForProbe); err != nil {
		return nil
	}
	sec, err := leadbrief.Load(ctx, b.s.Pool(), now, b.loc)
	if err != nil {
		slog.Default().Warn("console leads summary", "err", err.Error())
		return nil
	}
	sum := &console.LeadsSummary{Paused: sec.OperatorPaused, Stopped: sec.Paused, Replies: len(sec.Replies), ManualWaiting: sec.ManualWaiting,
		CheckByHand: sec.CheckByHand, SentYesterday: sec.Sent, Bounced: sec.Bounced, OptedOut: sec.OptedOut}
	if bt := sec.Batch; bt != nil {
		sum.Batch = &console.WaitingBatch{ID: bt.ID, Shadow: bt.Shadow, Emails: bt.Emails, Forms: bt.Forms, LinkedIn: bt.LinkedIn}
	}
	return sum
}

// Leads is the leads overview: the brief's leads section, the sending state, every batch
// waiting for approval, the manual items and the emails to check by hand, and the
// suppression list with addresses masked as `leads suppress list` masks them.
func (b *backend) Leads(ctx context.Context) (console.LeadsPage, error) {
	setup, err := b.leadsSetup(leadsForProbe)
	if err != nil {
		return console.LeadsPage{}, err
	}
	now := b.now()
	lq := b.lq()
	sec, err := leadbrief.Load(ctx, b.s.Pool(), now, b.loc)
	if err != nil {
		return console.LeadsPage{}, err
	}
	p := console.LeadsPage{Day: startOfDay(now, b.loc), Replies: sec.Replies, Other: sec.Other}
	if pr := sec.Probe; pr != nil {
		p.Checked, p.Drupal = pr.Checked, pr.Drupal
	}
	sn := console.Sending{SetUp: setup.env.Live(), SentYesterday: sec.Sent, Bounced: sec.Bounced, OptedOut: sec.OptedOut, Stopped: sec.Paused}
	if sn.Paused, sn.PauseReason, sn.PausedAt, err = lq.SendPaused(ctx); err != nil {
		return console.LeadsPage{}, err
	}
	if sn.SentToday, err = lq.SentSince(ctx, startOfDay(now, b.loc)); err != nil {
		return console.LeadsPage{}, err
	}
	if sn.CapToday, sn.DNS, sn.Mailboxes, err = mailboxesSending(ctx, lq, b.s.Pool(), setup.leads, b.loc, now); err != nil {
		return console.LeadsPage{}, err
	}
	p.Sending = sn
	if p.Batches, err = waitingBatches(ctx, b.s.Pool()); err != nil {
		return console.LeadsPage{}, err
	}
	if p.Manual, err = manualItems(ctx, b.s.Pool()); err != nil {
		return console.LeadsPage{}, err
	}
	if p.CheckByHand, err = checkItems(ctx, b.s.Pool()); err != nil {
		return console.LeadsPage{}, err
	}
	counts, err := lq.SuppressionCounts(ctx)
	if err != nil {
		return console.LeadsPage{}, err
	}
	for _, c := range counts {
		p.Suppressions.Counts = append(p.Suppressions.Counts, console.SuppressionCount{Kind: c.Kind, Reason: c.Reason, N: c.N})
	}
	active, lifted, err := lq.SuppressionRows(ctx, consoleSuppressionRows)
	if err != nil {
		return console.LeadsPage{}, err
	}
	for _, r := range active {
		p.Suppressions.Rows = append(p.Suppressions.Rows, suppressionRow(r))
	}
	for _, r := range lifted {
		p.Suppressions.Lifted = append(p.Suppressions.Lifted, suppressionRow(r))
	}
	// More is the active ones past the cap: the counts hold every active suppression.
	total := 0
	for _, c := range counts {
		total += c.N
	}
	p.Suppressions.More = max(total-len(active), 0)
	return p, nil
}

// consoleSuppressionRows caps each of the Leads page's suppression lists.
const consoleSuppressionRows = 200

// suppressionRow is a suppression as the Leads page lists it, its value shown as
// `leads suppress list` shows it (addresses masked).
func suppressionRow(r leadstore.SuppressionRow) console.SuppressionRow {
	return console.SuppressionRow{ID: r.ID, Kind: r.Kind, Value: shown(r.Kind, r.Value), Reason: r.Reason, Source: r.Source,
		At: r.CreatedAt, LiftedAt: r.LiftedAt, LiftReason: r.LiftReason, LiftSource: r.LiftSource}
}

// mailboxesSending is the Leads page's sending card over the stored mailboxes, read from
// the start of the operator's day: capToday sums today's cap of every mailbox ready for new
// leads (readyForLeads), each domain's share at most outreach.DomainCap; dns is shown as
// passed only when every active mailbox's host and selector passed today (then the
// earliest of those passes), else today's latest failed check of the first one that did not
// pass, and nil when one has no check today or no mailbox is active; rows is every mailbox
// not retired, grouped by its registrable domain (domains in order, then by id), with its
// own sends, limit and records today. It reads stored rows only: the console never reaches
// the network.
func mailboxesSending(ctx context.Context, lq leadstore.Queries, db store.DBTX, cfg leadconfig.Config, loc *time.Location, now time.Time) (
	capToday int, dns *console.SenderCheck, rows []console.SendingMailbox, err error) {
	today := startOfDay(now, loc)
	mbs, err := lq.Mailboxes(ctx, today)
	if err != nil {
		return 0, nil, nil, err
	}
	checks := hostChecks{db: db, since: today}
	byDomain := map[string]int{}
	var active []leadstore.MailboxStatus
	for _, m := range mbs {
		if m.State == leadstore.MailboxRetired {
			continue
		}
		mbCap, _ := mailboxRamp(cfg, m, loc, now)
		row := console.SendingMailbox{Address: m.Address, Domain: m.Domain, SentToday: m.SentToday, CapToday: mbCap}
		row.State, row.Reason = consoleMailboxState(m)
		if row.DNS, err = checks.of(ctx, m); err != nil {
			return 0, nil, nil, err
		}
		rows = append(rows, row)
		if m.State != leadstore.MailboxActive {
			continue
		}
		active = append(active, m)
		if readyForLeads(m) {
			byDomain[strings.ToLower(strings.TrimSpace(m.Domain))] += mbCap
		}
	}
	for _, n := range byDomain {
		capToday += min(n, outreach.DomainCap(cfg))
	}
	slices.SortStableFunc(rows, func(a, b console.SendingMailbox) int { return strings.Compare(a.Domain, b.Domain) })
	for _, m := range active {
		c, err := checks.of(ctx, m)
		if err != nil {
			return 0, nil, nil, err
		}
		switch {
		case c == nil:
			return capToday, nil, rows, nil
		case !c.SPF || !c.DKIM || !c.DMARC:
			if dns == nil || dns.SPF && dns.DKIM && dns.DMARC {
				dns = c
			}
		case dns == nil || dns.SPF && dns.DKIM && dns.DMARC && c.At.Before(dns.At):
			dns = c
		}
	}
	return capToday, dns, rows, nil
}

// hostChecks reads today's record check of each mailbox host and selector once.
type hostChecks struct {
	db    store.DBTX
	since time.Time
	seen  map[[2]string]*console.SenderCheck
}

// of is today's latest record check of m's host (Mailbox.Host, never the registrable
// domain) with its selector, nil when none ran today.
func (h *hostChecks) of(ctx context.Context, m leadstore.MailboxStatus) (*console.SenderCheck, error) {
	key := [2]string{m.Host(), strings.TrimSpace(m.DKIMSelector)}
	if c, ok := h.seen[key]; ok {
		return c, nil
	}
	c, err := senderCheckSince(ctx, h.db, leadstore.SenderDomainSHA256(key[0]), key[1], h.since)
	if err != nil {
		return nil, err
	}
	if h.seen == nil {
		h.seen = map[[2]string]*console.SenderCheck{}
	}
	h.seen[key] = c
	return c, nil
}

// mailboxRamp is mailbox m's ramp limit today (the operator's now, in loc) and its day of
// the ramp: the calendar days since its first live send, from 1, as the ramp counts them;
// 0 before its first live send. The ramp counts from that send only with caps.start_date
// first-send (outreach.CapOn).
func mailboxRamp(cfg leadconfig.Config, m leadstore.MailboxStatus, loc *time.Location, now time.Time) (capToday, rampDay int) {
	ml := outreach.MailboxLoad{ID: m.ID, Domain: m.Domain}
	local := now.In(loc)
	if m.FirstLiveSend != nil {
		fs := m.FirstLiveSend.In(loc)
		ml.FirstSend = &fs
		date := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }
		rampDay = max(1, int(date(local).Sub(date(fs))/(24*time.Hour))+1)
	}
	return outreach.MailboxCap(cfg, ml, local), rampDay
}

// consoleMailboxState is m's state as the console names it: retired, paused (with its
// reason), checking while a check waits or runs, ready when ready for new leads
// (readyForLeads), else needs a check.
func consoleMailboxState(m leadstore.MailboxStatus) (state, reason string) {
	switch {
	case m.State == leadstore.MailboxRetired:
		return console.MailboxRetired, ""
	case m.State == leadstore.MailboxPaused:
		return console.MailboxPaused, m.Reason
	case m.LastCheck != nil && m.LastCheck.Outcome == leadstore.CheckRequested:
		return console.MailboxChecking, ""
	case readyForLeads(m):
		return console.MailboxReady, ""
	}
	return console.MailboxNeedsCheck, ""
}

// senderCheckSince is the latest check of the domain whose hash is domainSHA with DKIM
// selector selector made at or after since, nil when there is none. A row with no selector
// (the single-mailbox check) never counts, as in leadstore's LatestSenderCheckSelector.
func senderCheckSince(ctx context.Context, db store.DBTX, domainSHA []byte, selector string, since time.Time) (*console.SenderCheck, error) {
	var c console.SenderCheck
	err := db.QueryRow(ctx,
		`SELECT checked_at, spf, dkim, dmarc FROM sender_check
		 WHERE domain_sha256 = $1 AND selector = $2 AND selector <> '' AND checked_at >= $3
		 ORDER BY checked_at DESC, id DESC LIMIT 1`, domainSHA, selector, since).Scan(&c.At, &c.SPF, &c.DKIM, &c.DMARC)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("console: sender check: %w", err)
	}
	return &c, nil
}

// waitingBatches is every batch neither approved nor discarded, newest first, with its
// leads and items counted by channel (a follow-up is not an item).
func waitingBatches(ctx context.Context, db store.DBTX) ([]console.WaitingBatch, error) {
	rows, err := db.Query(ctx,
		`SELECT b.id, b.shadow, b.created_at, count(DISTINCT m.lead_id)::int,
		   count(*) FILTER (WHERE m.channel = 'email' AND m.step = 0)::int,
		   count(*) FILTER (WHERE m.channel = 'form')::int,
		   count(*) FILTER (WHERE m.channel = 'linkedin')::int
		 FROM outreach_batch b JOIN outreach_message m ON m.batch_id = b.id
		 WHERE b.approved_at IS NULL AND b.discarded_at IS NULL
		 GROUP BY b.id, b.shadow, b.created_at ORDER BY b.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("console: waiting batches: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (console.WaitingBatch, error) {
		var w console.WaitingBatch
		err := r.Scan(&w.ID, &w.Shadow, &w.CreatedAt, &w.Leads, &w.Emails, &w.Forms, &w.LinkedIn)
		return w, err
	})
	if err != nil {
		return nil, fmt.Errorf("console: waiting batches: %w", err)
	}
	return out, nil
}

// manualItems is the approved contact forms and LinkedIn notes of live batches not yet
// marked done, oldest first, with the form page for a contact form.
func manualItems(ctx context.Context, db store.DBTX) ([]console.ManualItem, error) {
	rows, err := db.Query(ctx,
		`SELECT m.id, m.batch_id, o.domain, m.channel, CASE WHEN m.channel = 'form' THEN coalesce(r.value, '') ELSE '' END
		 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		 LEFT JOIN contact_route r ON r.id = l.route_id
		 WHERE NOT b.shadow AND b.approved_at IS NOT NULL AND m.state = 'approved' AND m.channel IN ('form', 'linkedin')
		 ORDER BY m.id LIMIT $1`, consoleListCap)
	if err != nil {
		return nil, fmt.Errorf("console: manual items: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (console.ManualItem, error) {
		var it console.ManualItem
		err := r.Scan(&it.MessageID, &it.BatchID, &it.Domain, &it.Channel, &it.Route)
		return it, err
	})
	if err != nil {
		return nil, fmt.Errorf("console: manual items: %w", err)
	}
	return out, nil
}

// checkItems is the live emails that may have gone out (maybe_sent, or left sending),
// oldest first, a selftest's left out (it is never checked by hand); Step counts from 1.
func checkItems(ctx context.Context, db store.DBTX) ([]console.CheckItem, error) {
	rows, err := db.Query(ctx,
		`SELECT m.id, o.domain, m.step + 1, m.state, m.sent_at
		 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		 WHERE NOT b.shadow AND m.state IN ('maybe_sent', 'sending') AND l.kind <> 'selftest'
		 ORDER BY m.id LIMIT $1`, consoleListCap)
	if err != nil {
		return nil, fmt.Errorf("console: check by hand: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (console.CheckItem, error) {
		var it console.CheckItem
		err := r.Scan(&it.MessageID, &it.Domain, &it.Step, &it.State, &it.At)
		return it, err
	})
	if err != nil {
		return nil, fmt.Errorf("console: check by hand: %w", err)
	}
	return out, nil
}

// LeadBatch is batch id in full through batchView, with the hash an approval names only
// when the batch can be approved as recomputed for this read.
func (b *backend) LeadBatch(ctx context.Context, id int64) (console.LeadBatch, error) {
	if _, err := b.leadsSetup(leadsForProbe); err != nil {
		return console.LeadBatch{}, err
	}
	v, err := batchView(ctx, b.lq(), id)
	if r := asRefusal(err); r != nil && r.Kind == RefuseBatchNotFound {
		return console.LeadBatch{}, console.ErrNotFound
	}
	if err != nil {
		return console.LeadBatch{}, err
	}
	out := console.LeadBatch{ID: v.Batch.ID, Shadow: v.Batch.Shadow, CreatedAt: v.Batch.CreatedAt, ApprovedAt: v.Batch.ApprovedAt,
		DiscardedAt: v.Batch.DiscardedAt, Changed: v.Changed}
	if v.Approvable() {
		out.HashPrefix = v.HashPrefix()
	}
	for _, it := range v.Items {
		li := console.LeadItem{N: it.N, LeadID: it.Lead.ID, Domain: it.Org.Domain, Kind: it.Lead.Kind, Channel: it.Lead.Channel, Route: it.Route,
			Finding: findingLines(v.Evidence[it.Lead.ID])}
		for _, m := range it.Msgs {
			lm := console.LeadMessage{ID: m.ID, Label: consoleMessageLabel(m, len(it.Msgs)), Channel: m.Channel, To: m.ToAddress,
				Subject: m.Subject, Body: strings.TrimRight(m.BodyText, "\n"), CancelReason: m.CancelReason}
			if m.Channel == "email" && m.FromAddress != "" {
				lm.From = console.MessageFrom{Name: m.FromName, Address: m.FromAddress}
			}
			if m.State != leadstore.MsgDraft {
				lm.State = m.State
			}
			li.Messages = append(li.Messages, lm)
		}
		out.Items = append(out.Items, li)
	}
	return out, nil
}

// findingLines is the finding and its evidence as `batch show` prints them (printEvidence),
// one line each.
func findingLines(e leadstore.Evidence) []string {
	var buf bytes.Buffer
	printEvidence(&buf, e)
	var out []string
	for l := range strings.Lines(buf.String()) {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// consoleMessageLabel names a message in the console's words: which email of how many, or
// the manual item it is, and its number.
func consoleMessageLabel(m leadstore.Message, n int) string {
	switch m.Channel {
	case "email":
		l := fmt.Sprintf("Email %d of %d · message %d", m.Step+1, n, m.ID)
		if m.InReplyTo != "" {
			l += " · a reply in the same thread"
		}
		return l
	case "form":
		return fmt.Sprintf("Contact form text · message %d", m.ID)
	case "linkedin":
		return fmt.Sprintf("LinkedIn note · message %d", m.ID)
	}
	return fmt.Sprintf("Message %d", m.ID)
}

// candidateOptions is what the queue is read with: the order a live batch drafts in.
func (b *backend) candidateOptions(cfg leadconfig.Config, now time.Time) leadstore.CandidateOptions {
	return leadstore.CandidateOptions{KindsPriority: cfg.KindsPriority, Profile: cfg.PriorityProfile(), Today: now.In(b.loc),
		IncludeReserved: leadsMail.allowReserved}
}

// LeadsNext is the next consoleQueue leads, in the order the next live batch drafts them.
func (b *backend) LeadsNext(ctx context.Context) (console.LeadsQueue, error) {
	setup, err := b.leadsSetup(leadsForProbe)
	if err != nil {
		return console.LeadsQueue{}, err
	}
	now := b.now()
	cands, err := b.lq().NextUp(ctx, b.candidateOptions(setup.leads, now), consoleQueue)
	if err != nil {
		return console.LeadsQueue{}, err
	}
	until := startOfDay(now, b.loc).AddDate(0, 0, consoleDeferDays)
	return console.LeadsQueue{DeferUntil: until.Format(time.DateOnly), Leads: queueLeads(cands)}, nil
}

func queueLeads(cands []leadstore.Candidate) []console.QueueLead {
	out := make([]console.QueueLead, 0, len(cands))
	for _, c := range cands {
		v := "agency page"
		if c.ProbeID != nil {
			v = "Drupal " + strconv.Itoa(c.Major)
			if c.ExactVersion != "" {
				v += " (" + c.ExactVersion + ")"
			}
		}
		out = append(out, console.QueueLead{LeadID: c.LeadID, Domain: c.Domain, Kind: c.Kind, Version: v, Country: c.Country,
			Channel: c.Channel, Score: c.Score, Why: c.Why, Pinned: c.Pinned})
	}
	return out
}

// LeadPriorities is the profile in use as a form, or edit, with the top consolePreview
// leads under it. An edit is previewed by saving it inside a transaction that is rolled
// back, so the queue is read by the same code a batch drafts with and nothing stays.
func (b *backend) LeadPriorities(ctx context.Context, edit *console.PriorityForm) (console.PrioritiesPage, error) {
	setup, err := b.leadsSetup(leadsForProbe)
	if err != nil {
		return console.PrioritiesPage{}, err
	}
	now := b.now()
	opts := b.candidateOptions(setup.leads, now)
	if edit == nil {
		p, fromDB, err := b.lq().EffectiveProfile(ctx, setup.leads.PriorityProfile())
		if err != nil {
			return console.PrioritiesPage{}, err
		}
		cands, err := b.lq().NextUp(ctx, opts, consolePreview)
		if err != nil {
			return console.PrioritiesPage{}, err
		}
		return console.PrioritiesPage{FromDB: fromDB, Form: profileForm(p), Preview: queueLeads(cands)}, nil
	}
	p, problems := parseProfileForm(*edit)
	page := console.PrioritiesPage{Edited: true, Form: normalForm(*edit), Problems: problems}
	if len(problems) > 0 {
		return page, nil
	}
	tx, err := b.s.Pool().Begin(ctx)
	if err != nil {
		return console.PrioritiesPage{}, fmt.Errorf("console: preview: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // the edit never stays
	tq := leadstore.NewQueries(tx)
	if _, err := tq.SaveProfile(ctx, p); err != nil {
		return console.PrioritiesPage{}, err
	}
	cands, err := tq.NextUp(ctx, opts, consolePreview)
	if err != nil {
		return console.PrioritiesPage{}, err
	}
	page.Preview = queueLeads(cands)
	return page, nil
}

// The form's point groups, in the order the page shows them.
func pointFields() []console.PointField {
	var out []console.PointField
	for _, k := range leadconfig.Kinds {
		out = append(out, console.PointField{Key: "kind." + k, Group: "Kind", Label: strings.ReplaceAll(k, "_", " ")})
	}
	for m := leadconfig.MinPriorityMajor; m <= leadconfig.MaxPriorityMajor; m++ {
		out = append(out, console.PointField{Key: "major." + strconv.Itoa(m), Group: "Drupal", Label: strconv.Itoa(m)})
	}
	for _, c := range leadconfig.PriorityChannels {
		out = append(out, console.PointField{Key: "channel." + c, Group: "Channel", Label: c})
	}
	return out
}

// profileForm is p as the form shows it.
func profileForm(p leadconfig.Profile) console.PriorityForm {
	f := console.PriorityForm{Points: pointFields()}
	for i, pf := range f.Points {
		group, key, _ := strings.Cut(pf.Key, ".")
		var v int
		var ok bool
		switch group {
		case "kind":
			v, ok = p.Kinds[key]
		case "major":
			m, _ := strconv.Atoi(key)
			v, ok = p.Majors[m]
		case "channel":
			v, ok = p.Channels[key]
		}
		if ok {
			f.Points[i].Value = strconv.Itoa(v)
		}
	}
	if p.ExactVersion != 0 {
		f.ExactVersion = strconv.Itoa(p.ExactVersion)
	}
	var countries []string
	for _, c := range slices.Sorted(maps.Keys(p.Countries)) {
		countries = append(countries, fmt.Sprintf("%s %d", c, p.Countries[c]))
	}
	f.Countries = strings.Join(countries, ", ")
	var bands []string
	for _, rb := range p.RankBands {
		bands = append(bands, fmt.Sprintf("%d %d", rb.Under, rb.Points))
	}
	f.RankBands = strings.Join(bands, ", ")
	f.Exclude = strings.Join(p.ExcludeCountries, ", ")
	return f
}

// normalForm is edit with its points in the page's order and labels, keeping what was typed.
func normalForm(edit console.PriorityForm) console.PriorityForm {
	typed := map[string]string{}
	for _, p := range edit.Points {
		typed[p.Key] = p.Value
	}
	f := edit
	f.Points = pointFields()
	for i := range f.Points {
		f.Points[i].Value = typed[f.Points[i].Key]
	}
	return f
}

// parseProfileForm reads the form into a profile and lists every problem: values that are
// not whole numbers, pairs that do not read, and what Profile.Problems finds.
func parseProfileForm(f console.PriorityForm) (leadconfig.Profile, []string) {
	var p leadconfig.Profile
	var problems []string
	num := func(what, v string) (int, bool) {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %q is not a whole number", what, v))
			return 0, false
		}
		return n, true
	}
	labels := map[string]string{}
	for _, pf := range pointFields() {
		labels[pf.Key] = pf.Group + " " + pf.Label
	}
	for _, pf := range f.Points {
		if strings.TrimSpace(pf.Value) == "" {
			continue
		}
		label, known := labels[pf.Key]
		if !known {
			problems = append(problems, fmt.Sprintf("%q is not a priority rule", pf.Key))
			continue
		}
		n, ok := num(label, pf.Value)
		if !ok {
			continue
		}
		group, key, _ := strings.Cut(pf.Key, ".")
		switch group {
		case "kind":
			if p.Kinds == nil {
				p.Kinds = map[string]int{}
			}
			p.Kinds[key] = n
		case "major":
			if p.Majors == nil {
				p.Majors = map[int]int{}
			}
			m, _ := strconv.Atoi(key)
			p.Majors[m] = n
		case "channel":
			if p.Channels == nil {
				p.Channels = map[string]int{}
			}
			p.Channels[key] = n
		}
	}
	if v := strings.TrimSpace(f.ExactVersion); v != "" {
		p.ExactVersion, _ = num("Exact version known", v)
	}
	pairs := func(what, s string, each func(a string, b int)) {
		for part := range strings.SplitSeq(s, ",") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			fs := strings.Fields(part)
			if len(fs) != 2 {
				problems = append(problems, fmt.Sprintf("%s: %q is not two values", what, part))
				continue
			}
			if n, ok := num(what, fs[1]); ok {
				each(fs[0], n)
			}
		}
	}
	pairs("Countries", f.Countries, func(c string, n int) {
		if p.Countries == nil {
			p.Countries = map[string]int{}
		}
		p.Countries[strings.ToUpper(c)] = n
	})
	pairs("Site rank bands", f.RankBands, func(u string, n int) {
		if under, ok := num("Site rank bands", u); ok {
			p.RankBands = append(p.RankBands, leadconfig.RankBand{Under: under, Points: n})
		}
	})
	for c := range strings.SplitSeq(f.Exclude, ",") {
		if c = strings.ToUpper(strings.TrimSpace(c)); c != "" {
			p.ExcludeCountries = append(p.ExcludeCountries, c)
		}
	}
	return p, append(problems, p.Problems()...)
}

// LeadAct runs one write through its core and words what it did. Every refusal is
// ErrUserFixable with the console's words; nothing here approves a batch on its own or
// sends anything.
func (b *backend) LeadAct(ctx context.Context, a console.LeadAction) (console.LeadOutcome, error) {
	load := leadsForProbe
	if a.Do == console.LeadNewBatch {
		load = leadsForOutreach
	}
	setup, err := b.leadsSetup(load)
	if err != nil {
		return console.LeadOutcome{}, err
	}
	for _, s := range []string{a.Reason, a.Note, a.Value} {
		if utf8.RuneCountInString(s) > consoleMaxText {
			return console.LeadOutcome{}, refused("Each field takes at most %d characters; nothing was changed.", consoleMaxText)
		}
	}
	switch a.Do {
	case console.LeadApprove:
		return b.approve(ctx, setup, a)
	case console.LeadDiscard:
		return b.discard(ctx, a.ID)
	case console.LeadNewBatch:
		return b.newBatch(ctx, setup)
	case console.LeadResolve:
		return b.resolve(ctx, setup, a)
	case console.LeadDone:
		return b.done(ctx, a.ID)
	case console.LeadSuppress:
		return b.suppress(ctx, a)
	case console.LeadLift:
		return b.lift(ctx, a)
	case console.LeadContacted:
		return b.contacted(ctx, a)
	case console.LeadPause, console.LeadResume:
		return b.setSending(ctx, a.Do == console.LeadPause, a.Reason)
	case console.LeadPin, console.LeadDefer, console.LeadSkip, console.LeadClear:
		return b.steer(ctx, a)
	case console.LeadSavePriority:
		p, problems := parseProfileForm(a.Form)
		if len(problems) > 0 {
			return console.LeadOutcome{}, refused("Nothing was saved: %s.", strings.Join(problems, "; "))
		}
		id, err := b.lq().SaveProfile(ctx, p)
		if err != nil {
			return console.LeadOutcome{}, err
		}
		return console.LeadOutcome{Message: fmt.Sprintf("Priorities saved (version %d); the next batch drafts in this order.", id)}, nil
	case console.LeadResetPrio:
		if _, err := b.lq().ResetProfile(ctx); err != nil {
			return console.LeadOutcome{}, err
		}
		return console.LeadOutcome{Message: "Priorities reset to the leads settings file's defaults."}, nil
	}
	return console.LeadOutcome{}, refused("That is not something the leads pages do.")
}

func (b *backend) approve(ctx context.Context, setup leadsSetup, a console.LeadAction) (console.LeadOutcome, error) {
	out, err := approveBatch(ctx, b.s, setup.leads, b.now, ApproveRequest{ID: a.ID, HashPrefix: a.Hash, Skip: a.Skip})
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseBatchNotFound:
			return console.LeadOutcome{}, refused("There is no batch %d.", a.ID)
		case RefuseHashMismatch:
			return console.LeadOutcome{}, refused("Batch %d is not the one you read; nothing was approved. Read it again below.", a.ID)
		case RefuseBatchChanged:
			return console.LeadOutcome{}, refused("Batch %d changed since it was drafted, so it cannot be approved; nothing was approved. Discard it and make a new one.", a.ID)
		case RefuseBadSkip:
			return console.LeadOutcome{}, refused("Batch %d has items 1 to %d; nothing was approved.", a.ID, r.Items)
		case RefuseBatchDiscarded:
			return console.LeadOutcome{}, refused("Batch %d was discarded; nothing was approved.", a.ID)
		}
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	if out.Approval.Already {
		return console.LeadOutcome{Message: fmt.Sprintf("Batch %d was already approved; nothing changed.", a.ID)}, nil
	}
	msg := fmt.Sprintf("Batch %d (%s) approved: %s approved, %s left out.", a.ID, batchKind(out.Shadow),
		plural(out.Approval.Approved, "message", "messages"), plural(out.Approval.Skipped, "message", "messages"))
	if !out.FirstDue.IsZero() {
		msg += fmt.Sprintf(" First emails go from %s, each in its recipient's business hours.", out.FirstDue.In(b.loc).Format("Mon 2 Jan 15:04"))
	}
	if len(out.Manual) > 0 {
		msg += " Its contact forms and LinkedIn notes wait for you on the leads page."
	}
	if out.Shadow {
		msg += " A shadow batch: nothing is sent, and its leads stay new."
	}
	return console.LeadOutcome{Message: msg}, nil
}

func (b *backend) discard(ctx context.Context, id int64) (console.LeadOutcome, error) {
	out, err := discardBatch(ctx, b.s, id)
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseBatchNotFound:
			return console.LeadOutcome{}, refused("There is no batch %d.", id)
		case RefuseBatchApproved:
			return console.LeadOutcome{}, refused("Batch %d is approved, so it is never discarded. Pause sending, or suppress a lead, to stop its emails.", id)
		}
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	if out.Already {
		return console.LeadOutcome{Message: fmt.Sprintf("Batch %d was already discarded; nothing changed.", id)}, nil
	}
	return console.LeadOutcome{Message: fmt.Sprintf("Batch %d (%s) discarded: %s cancelled, %s back in the queue.", id, batchKind(out.Shadow),
		plural(int(out.Cancelled), "message", "messages"), plural(out.LeadsBack, "lead", "leads"))}, nil
}

// newBatch is makeBatch for a live batch only: the console never drafts a shadow batch.
func (b *backend) newBatch(ctx context.Context, setup leadsSetup) (console.LeadOutcome, error) {
	out, err := makeBatch(ctx, b.s, newBatchSetup(setup, b.loc), b.now(), NewBatchRequest{LiveOnly: true})
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseNotLive:
			return console.LeadOutcome{}, refused("Sending isn't set up on this server yet.")
		case RefuseBatchWaiting:
			return console.LeadOutcome{}, refused("Batch %d still waits for approval; no new batch.", r.BatchID)
		case RefuseNoRoom:
			if r.Sizing.Waiting == 0 {
				return console.LeadOutcome{}, refused("The warm-up ramp sends nothing over the next %d days; no new batch.", r.Sizing.Days)
			}
			return console.LeadOutcome{}, refused("Enough approved mail waits for the next days; no new batch. %s.", r.Sizing.Words())
		}
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	var dropped []string
	for _, d := range out.Dropped {
		dropped = append(dropped, d.String())
	}
	if out.BatchID == 0 {
		msg := "No batch: no candidate lead has a draft that passes the checks."
		if len(dropped) > 0 {
			msg += " Left out: " + strings.Join(dropped, "; ") + "."
		}
		return console.LeadOutcome{Message: msg}, nil
	}
	msg := fmt.Sprintf("Batch %d: %s. Read it and approve it below.", out.BatchID, out.Counts)
	if out.Sizing != nil {
		msg += " " + out.Sizing.Words() + "."
	}
	if len(dropped) > 0 {
		msg += " Left out: " + strings.Join(dropped, "; ") + "."
	}
	return console.LeadOutcome{Message: msg, BatchID: out.BatchID}, nil
}

func (b *backend) resolve(ctx context.Context, setup leadsSetup, a console.LeadAction) (console.LeadOutcome, error) {
	out, err := resolveMaybeSent(ctx, b.s, setup, b.loc, a.ID, a.Sent)
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseSendRunning:
			return console.LeadOutcome{}, refused("A send run is in progress; try again in a few minutes.")
		case RefuseMessageNotFound:
			return console.LeadOutcome{}, refused("There is no message %d.", a.ID)
		case RefuseMessageState:
			return console.LeadOutcome{}, refused("Message %d is not an email that may have gone out; nothing changed.", a.ID)
		case RefuseSelftest:
			return console.LeadOutcome{}, refused("Message %d belongs to a selftest; a selftest is never checked by hand.", a.ID)
		}
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	if !out.Sent {
		return console.LeadOutcome{Message: fmt.Sprintf("Message %d not sent: it is approved again and goes at the next send run in its recipient's hours.", a.ID)}, nil
	}
	lead := "its lead is now contacted"
	if !out.LeadMoved {
		lead = "its lead keeps its state"
	}
	return console.LeadOutcome{Message: fmt.Sprintf("Message %d sent %s; %s, and its follow-ups go on as after any send.", a.ID,
		out.SentAt.In(b.loc).Format("Mon 2 Jan 15:04"), lead)}, nil
}

func (b *backend) done(ctx context.Context, id int64) (console.LeadOutcome, error) {
	d, err := markDone(ctx, b.s, id)
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseMessageNotFound:
			return console.LeadOutcome{}, refused("There is no message %d.", id)
		case RefuseNotManual:
			return console.LeadOutcome{}, refused("Message %d is an email; sending handles emails.", id)
		case RefuseMessageState:
			return console.LeadOutcome{}, refused("Message %d is not an approved contact form or LinkedIn note.", id)
		}
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	switch {
	case d.Shadow:
		return console.LeadOutcome{Message: fmt.Sprintf("Message %d done (a shadow batch: its lead stays %s).", id, d.LeadState)}, nil
	case d.Moved:
		return console.LeadOutcome{Message: fmt.Sprintf("Message %d done; its lead is now contacted.", id)}, nil
	}
	return console.LeadOutcome{Message: fmt.Sprintf("Message %d done; its lead stays %s.", id, d.LeadState)}, nil
}

func (b *backend) suppress(ctx context.Context, a console.LeadAction) (console.LeadOutcome, error) {
	if a.Kind != leadstore.SuppressAddress && a.Kind != leadstore.SuppressDomain {
		return console.LeadOutcome{}, refused("Choose whether it is an address or a domain.")
	}
	value := strings.ToLower(strings.TrimSpace(a.Value))
	res, err := suppressAdd(ctx, b.s, a.Kind, value, a.Reason, suppressFromConsole)
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseSuppressReason:
			return console.LeadOutcome{}, refused("Choose a reason from the list.")
		case RefuseNotRegistrable:
			return console.LeadOutcome{}, refused("%s has no registrable domain (a bare public suffix cannot be suppressed).", value)
		case RefuseBadValue:
			return console.LeadOutcome{}, refused("%s is not a usable %s.", shown(a.Kind, value), a.Kind)
		}
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	rec := "recorded"
	if !res.Recorded {
		rec = "already on file"
	}
	return console.LeadOutcome{Message: fmt.Sprintf("Suppressed %s %s (%s): %s · %s reached, %d moved to suppressed, %s cancelled.",
		a.Kind, shown(a.Kind, value), a.Reason, rec, plural(res.Leads, "lead", "leads"), res.Moved,
		plural(int(res.Cancelled), "pending message", "pending messages"))}, nil
}

// lift is liftSuppression from the console (FO6), worded by liftWords as the CLI words it.
func (b *backend) lift(ctx context.Context, a console.LeadAction) (console.LeadOutcome, error) {
	res, err := liftSuppression(ctx, b.s, a.ID, a.Value, a.Reason, leadstore.LiftFromConsole)
	if asRefusal(err) != nil {
		return console.LeadOutcome{}, refused("%s", liftWords(res, err, forConsole))
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	return console.LeadOutcome{Message: liftWords(res, nil, forConsole)}, nil
}

func (b *backend) contacted(ctx context.Context, a console.LeadAction) (console.LeadOutcome, error) {
	c, err := markContacted(ctx, b.s, a.Value, a.Note)
	if r := asRefusal(err); r != nil && r.Kind == RefuseNotDomain {
		return console.LeadOutcome{}, refused("%q is not a domain.", a.Value)
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	if c.Moved {
		return console.LeadOutcome{Message: fmt.Sprintf("%s is now contacted by hand; %s cancelled. No batch drafts it again.", a.Value,
			plural(int(c.Cancelled), "pending message", "pending messages"))}, nil
	}
	return console.LeadOutcome{Message: fmt.Sprintf("%s stays %s; nothing changed.", a.Value, c.State)}, nil
}

// SendSwitch is what setSendPause did.
type SendSwitch struct {
	Changed bool      // a row was written
	Since   time.Time // when the switch was last set, for an unchanged pause
}

// setSendPause sets the operator's pause switch to pause, with reason, unless it already
// is (leadstore.SetSendPause appends one row); every send run obeys it before each message.
func setSendPause(ctx context.Context, lq leadstore.Queries, pause bool, reason string) (SendSwitch, error) {
	paused, _, at, err := lq.SendPaused(ctx)
	if err != nil {
		return SendSwitch{}, err
	}
	if paused == pause {
		return SendSwitch{Since: at}, nil
	}
	if err := lq.SetSendPause(ctx, pause, strings.TrimSpace(reason)); err != nil {
		return SendSwitch{}, err
	}
	return SendSwitch{Changed: true}, nil
}

func (b *backend) setSending(ctx context.Context, pause bool, reason string) (console.LeadOutcome, error) {
	sw, err := setSendPause(ctx, b.lq(), pause, reason)
	if err != nil {
		return console.LeadOutcome{}, err
	}
	switch {
	case pause && !sw.Changed:
		return console.LeadOutcome{Message: fmt.Sprintf("Sending is already paused (since %s).", sw.Since.In(b.loc).Format("Mon 2 Jan 15:04"))}, nil
	case !pause && !sw.Changed:
		return console.LeadOutcome{Message: "Sending is not paused."}, nil
	case pause:
		return console.LeadOutcome{Message: "Sending is paused: nothing is sent until you resume."}, nil
	}
	return console.LeadOutcome{Message: "Sending is resumed: the next send run sends what is due."}, nil
}

// steer pins, pushes back, skips or clears one lead of the queue.
func (b *backend) steer(ctx context.Context, a console.LeadAction) (console.LeadOutcome, error) {
	lq := b.lq()
	var err error
	msg := ""
	switch a.Do {
	case console.LeadPin:
		err, msg = lq.PinLead(ctx, a.ID), fmt.Sprintf("Lead %d pinned: it comes first.", a.ID)
	case console.LeadClear:
		err, msg = lq.ClearLeadPriority(ctx, a.ID), fmt.Sprintf("Lead %d is back in its place by score.", a.ID)
	case console.LeadDefer:
		until, perr := time.ParseInLocation(time.DateOnly, a.Until, b.loc)
		if perr != nil {
			return console.LeadOutcome{}, refused("Pick the day to push the lead back to.")
		}
		if until.Before(startOfDay(b.now(), b.loc)) {
			return console.LeadOutcome{}, refused("Pick a day from today on.")
		}
		err, msg = lq.DeferLead(ctx, a.ID, until), fmt.Sprintf("Lead %d pushed back: it comes back after %s.", a.ID, until.Format("Mon 2 Jan"))
	case console.LeadSkip:
		var moved bool
		moved, err = lq.SkipLead(ctx, a.ID)
		msg = fmt.Sprintf("Lead %d skipped for good.", a.ID)
		if err == nil && !moved {
			msg = fmt.Sprintf("Lead %d no longer waits in the queue (a batch may have taken it); nothing changed.", a.ID)
		}
	}
	if errors.Is(err, leadstore.ErrLeadNotFound) {
		return console.LeadOutcome{}, refused("There is no lead %d.", a.ID)
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	return console.LeadOutcome{Message: msg}, nil
}

// The console's side of the Mailboxes pages: reads of the stored mailboxes, and every write
// through the mailbox core (mailboxcore.go) that `leads mailboxes` runs, so both write the
// same rows and refuse with the same words. The console holds only the public key: it seals
// a new password, and never opens one.

// keyMissing reports whether serve has no usable public key, so no password can be sealed.
func (b *backend) keyMissing() bool { return b.pub == (secretbox.PublicKey{}) }

// mailboxesPerDomain is caps.mailboxes_per_domain as addMailbox applies it.
func mailboxesPerDomain(cfg leadconfig.Config) int {
	return min(cmp.Or(cfg.Caps.MailboxesPerDomain, leadconfig.DefaultMailboxesPerDomain), leadconfig.HardMailboxesPerDomain)
}

// mailboxRow is m as one row of the Mailboxes page.
func (b *backend) mailboxRow(cfg leadconfig.Config, m leadstore.MailboxStatus, now time.Time) console.MailboxRow {
	r := console.MailboxRow{ID: m.ID, Address: m.Address, SentToday: m.SentToday}
	r.State, r.Reason = consoleMailboxState(m)
	r.DomainPause = m.State == leadstore.MailboxPaused && m.Scope == leadstore.ScopeDomain
	r.CapToday, r.RampDay = mailboxRamp(cfg, m, b.loc, now)
	if m.LastCheck != nil && m.LastCheck.Outcome != leadstore.CheckRequested {
		r.LastCheck = m.LastCheck.CheckedAt
	}
	return r
}

// mailboxCheckView is m's latest check as its card shows it. A check still requested
// console.CheckNotStartedAfter after it was asked for is NotStarted: its request file was
// missed (written just after the worker's last look), so the card offers Check now, which
// writes it again. requested_at is the database's clock, so it is compared with the
// machine's clock, never the operator's day.
func mailboxCheckView(m leadstore.MailboxStatus) console.MailboxCheckView {
	v := console.MailboxCheckView{MailboxID: m.ID, Retired: m.State == leadstore.MailboxRetired}
	if c := m.LastCheck; c != nil {
		at := c.RequestedAt
		v.Outcome, v.RequestedAt, v.CheckedAt, v.Fix = c.Outcome, &at, c.CheckedAt, c.Fix
		v.SMTPOK, v.IMAPOK, v.FoldersOK, v.SPF, v.DKIM, v.DMARC = c.SMTPOK, c.IMAPOK, c.FoldersOK, c.SPF, c.DKIM, c.DMARC
		v.NotStarted = c.Outcome == leadstore.CheckRequested && time.Since(at) > console.CheckNotStartedAfter
	}
	return v
}

// mailboxStatus is mailbox id as of the operator's today, console.ErrNotFound when there is
// none, with the leads settings.
func (b *backend) mailboxStatus(ctx context.Context, id int64) (leadstore.MailboxStatus, leadsSetup, time.Time, error) {
	setup, err := b.leadsSetup(leadsForProbe)
	if err != nil {
		return leadstore.MailboxStatus{}, leadsSetup{}, time.Time{}, err
	}
	now := b.now()
	ms, err := b.lq().Mailboxes(ctx, startOfDay(now, b.loc))
	if err != nil {
		return leadstore.MailboxStatus{}, leadsSetup{}, time.Time{}, err
	}
	for _, m := range ms {
		if m.ID == id {
			return m, setup, now, nil
		}
	}
	return leadstore.MailboxStatus{}, leadsSetup{}, time.Time{}, console.ErrNotFound
}

// Mailboxes is every mailbox grouped by its registrable domain (in order of each domain's
// first mailbox), each group with its sends today against caps.domain_per_day, its
// mailboxes against caps.mailboxes_per_domain, today's record checks per host and selector
// of its active mailboxes, and whether the domain is paused as a whole: none of its
// mailboxes active and one paused with the domain. It reads stored rows only.
func (b *backend) Mailboxes(ctx context.Context) (console.MailboxesPage, error) {
	setup, err := b.leadsSetup(leadsForProbe)
	if err != nil {
		return console.MailboxesPage{}, err
	}
	now := b.now()
	today := startOfDay(now, b.loc)
	ms, err := b.lq().Mailboxes(ctx, today)
	if err != nil {
		return console.MailboxesPage{}, err
	}
	p := console.MailboxesPage{RetiredEnv: setup.env.Retired, KeyMissing: b.keyMissing()}
	for _, pr := range mailboxPresets {
		p.Presets = append(p.Presets, console.MailboxPreset{Name: pr.Name, SMTP: pr.SMTP, IMAP: pr.IMAP, Selector: pr.Selector})
	}
	checks := hostChecks{db: b.s.Pool(), since: today}
	group := map[string]int{}
	activeOn := map[string]int{}
	listed := map[[3]string]bool{}
	for _, m := range ms {
		i, ok := group[m.Domain]
		if !ok {
			i = len(p.Groups)
			group[m.Domain] = i
			p.Groups = append(p.Groups, console.MailboxDomain{Domain: m.Domain, DomainLimit: outreach.DomainCap(setup.leads),
				MailboxLimit: mailboxesPerDomain(setup.leads)})
		}
		g := &p.Groups[i]
		g.SentToday += m.SentToday
		g.Rows = append(g.Rows, b.mailboxRow(setup.leads, m, now))
		switch m.State {
		case leadstore.MailboxRetired:
			continue
		case leadstore.MailboxPaused:
			if m.Scope == leadstore.ScopeDomain && !g.Paused {
				g.Paused, g.PauseReason = true, m.Reason
			}
		case leadstore.MailboxActive:
			activeOn[m.Domain]++
			key := [3]string{m.Domain, m.Host(), strings.TrimSpace(m.DKIMSelector)}
			if listed[key] {
				break
			}
			listed[key] = true
			c, err := checks.of(ctx, m)
			if err != nil {
				return console.MailboxesPage{}, err
			}
			hc := console.HostCheck{Host: key[1], Selector: key[2]}
			if c != nil {
				at := c.At
				hc.At, hc.SPF, hc.DKIM, hc.DMARC = &at, c.SPF, c.DKIM, c.DMARC
			}
			g.DNS = append(g.DNS, hc)
		}
		g.Mailboxes++
	}
	for i := range p.Groups {
		if g := &p.Groups[i]; activeOn[g.Domain] > 0 {
			g.Paused, g.PauseReason = false, ""
		}
	}
	return p, nil
}

// Mailbox is mailbox id in full, never with its password.
func (b *backend) Mailbox(ctx context.Context, id int64) (console.MailboxPage, error) {
	m, setup, now, err := b.mailboxStatus(ctx, id)
	if err != nil {
		return console.MailboxPage{}, err
	}
	return console.MailboxPage{Row: b.mailboxRow(setup.leads, m, now), Domain: m.Domain, Login: m.Login, SMTPAddr: m.SMTPAddr,
		IMAPAddr: m.IMAPAddr, Selector: m.DKIMSelector, FromName: m.FromName, KeyMissing: b.keyMissing(), Check: mailboxCheckView(m)}, nil
}

// MailboxCheckCard is mailbox id's latest check, read fresh for the polling card.
func (b *backend) MailboxCheckCard(ctx context.Context, id int64) (console.MailboxCheckView, error) {
	m, _, _, err := b.mailboxStatus(ctx, id)
	if err != nil {
		return console.MailboxCheckView{}, err
	}
	return mailboxCheckView(m), nil
}

// MailboxAct runs one Mailboxes write through the mailbox core and words what it did; every
// refusal is the core's, wrapping console.ErrUserFixable. A write that was saved but whose
// check could not be started (the request file) says so and is logged, never a failure.
func (b *backend) MailboxAct(ctx context.Context, a console.MailboxAction) (console.MailboxOutcome, error) {
	setup, err := b.leadsSetup(leadsForProbe)
	if err != nil {
		return console.MailboxOutcome{}, err
	}
	in := a.Input
	for _, s := range []string{in.Address, in.Login, in.SMTPAddr, in.IMAPAddr, in.Selector, in.FromName, in.Preset, a.Domain} {
		if utf8.RuneCountInString(s) > consoleMailboxText {
			return console.MailboxOutcome{}, refused("Each field takes at most %d characters; nothing was changed.", consoleMailboxText)
		}
	}
	if utf8.RuneCountInString(a.Reason) > consoleMaxText {
		return console.MailboxOutcome{}, refused("The reason takes at most %d characters; nothing was changed.", consoleMaxText)
	}
	lq, now := b.lq(), b.now()
	input := MailboxInput{Address: in.Address, Login: in.Login, SMTPAddr: in.SMTPAddr, IMAPAddr: in.IMAPAddr, Selector: in.Selector,
		FromName: in.FromName, Preset: in.Preset, Password: in.Password}
	switch a.Do {
	case console.MailboxAdd:
		id, err := addMailbox(ctx, lq, setup.leads, b.pub, setup.dataDir, now, input)
		switch {
		case err != nil && id > 0:
			slog.Default().Warn("console mailbox check not started", "mailbox", id, "err", err.Error())
			return console.MailboxOutcome{MailboxID: id,
				Message: fmt.Sprintf("Mailbox %d is added, but its check could not be started: press Check now.", id)}, nil
		case err != nil:
			return console.MailboxOutcome{}, err
		}
		return console.MailboxOutcome{MailboxID: id, Message: fmt.Sprintf("Mailbox %d is added; its first check is running.", id)}, nil
	case console.MailboxEdit:
		input.Address, input.Preset = "", ""
		recheck, err := editMailbox(ctx, lq, b.pub, setup.dataDir, now, a.ID, input)
		switch {
		case err != nil && recheck:
			slog.Default().Warn("console mailbox check not started", "mailbox", a.ID, "err", err.Error())
			return console.MailboxOutcome{Message: fmt.Sprintf("Mailbox %d is saved, but its check could not be started: press Check now.", a.ID)}, nil
		case err != nil:
			return console.MailboxOutcome{}, err
		case recheck:
			return console.MailboxOutcome{Message: fmt.Sprintf("Mailbox %d is saved; it is checked again with the new settings.", a.ID)}, nil
		}
		return console.MailboxOutcome{Message: fmt.Sprintf("Mailbox %d is saved.", a.ID)}, nil
	case console.MailboxCheck:
		cid, err := requestCheck(ctx, lq, setup.dataDir, now, a.ID)
		switch {
		case err != nil && cid > 0:
			slog.Default().Warn("console mailbox check not started", "mailbox", a.ID, "err", err.Error())
			return console.MailboxOutcome{Message: "The check is asked for, but could not be started; press Check now again in a minute."}, nil
		case err != nil:
			return console.MailboxOutcome{}, err
		}
		return console.MailboxOutcome{Message: "The check is asked for."}, nil
	case console.MailboxPause, console.MailboxResume:
		pause := a.Do == console.MailboxPause
		sw, err := setMailboxPause(ctx, lq, now, a.ID, pause, a.Reason)
		switch {
		case err != nil:
			return console.MailboxOutcome{}, err
		case pause && !sw.Changed:
			return console.MailboxOutcome{Message: fmt.Sprintf("Mailbox %d is already paused.", a.ID)}, nil
		case !pause && !sw.Changed:
			return console.MailboxOutcome{Message: fmt.Sprintf("Mailbox %d is not paused.", a.ID)}, nil
		case pause:
			return console.MailboxOutcome{Message: fmt.Sprintf("Mailbox %d is paused: nothing is sent from it until you resume it.", a.ID)}, nil
		}
		return console.MailboxOutcome{Message: fmt.Sprintf("Mailbox %d is resumed.", a.ID)}, nil
	case console.MailboxRetire:
		n, err := retireMailbox(ctx, lq, now, a.ID, a.Confirm)
		if err != nil {
			return console.MailboxOutcome{}, err
		}
		return console.MailboxOutcome{Message: fmt.Sprintf("Mailbox %d is retired; %s cancelled.", a.ID, plural(n, "unsent message", "unsent messages"))}, nil
	case console.MailboxPauseDomain, console.MailboxResumeDomain:
		pause := a.Do == console.MailboxPauseDomain
		sw, err := setDomainPause(ctx, lq, now, a.Domain, pause, a.Reason)
		if err != nil {
			return console.MailboxOutcome{}, err
		}
		verb := "resumed"
		if pause {
			verb = "paused"
		}
		msg := fmt.Sprintf("%d mailboxes on %s are %s.", sw.Changed, sw.Domain, verb)
		if sw.Kept > 0 {
			msg += fmt.Sprintf(" %d stay paused on their own: resume each on its page.", sw.Kept)
		}
		return console.MailboxOutcome{Message: msg}, nil
	}
	return console.MailboxOutcome{}, refused("That is not something the Mailboxes pages do.")
}
