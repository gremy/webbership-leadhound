package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() {
	leadsCommands["batch"] = runLeadsBatch
	leadsCommands["done"] = runLeadsDone
	leadsCommands["mark-contacted"] = runLeadsMarkContacted
}

// leadsNow is the clock the batch commands schedule by; tests replace it.
var leadsNow = time.Now

// commandLeadsBatch is the run row of `leads batch new`.
const commandLeadsBatch = "leads-batch-new"

// The provenance tables a slot names.
const (
	slotOrg      = "org"
	slotProbe    = "site_probe"
	slotFetch    = "http_fetch"
	slotFinding  = "agency_finding"
	evidenceSpan = 50 // leads whose evidence one query reads
)

const leadsBatchUsage = "usage: leadhound leads batch new [--size N] | show N [--item K] | approve N --hash H [--skip 3,5]"

func runLeadsBatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError(leadsBatchUsage)
	}
	switch args[0] {
	case "new":
		return runBatchNew(ctx, args[1:])
	case "show":
		return runBatchShow(ctx, args[1:])
	case "approve":
		return runBatchApprove(ctx, args[1:])
	}
	return usageError("unknown batch subcommand %q; %s", args[0], leadsBatchUsage)
}

// batchMode is whether a new batch is shadow, and the domain its Message-IDs end in. A
// batch is shadow unless LEADHOUND_OUTREACH is live and LEADHOUND_OUTREACH_USER names a
// mailbox (RA1).
func batchMode(env leadconfig.Env) (shadow bool, idDomain string) {
	idDomain = outreach.MessageIDDomain(env.User.Reveal())
	return !env.Live() || idDomain == outreach.ShadowDomain, idDomain
}

// runBatchNew is `leads batch new [--size N]`: it drafts the whole sequence for the top N
// candidate leads (kinds_priority, then list rank), drops a lead whose draft fails Check
// and writes the batch.
func runBatchNew(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads batch new", flag.ContinueOnError)
	size := fs.Int("size", 0, "most leads in the batch (default caps.batch_size)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *size < 0 {
		return usageError(leadsBatchUsage)
	}
	s, setup, err := leadsOpen(ctx, leadsForOutreach)
	if err != nil {
		return err
	}
	defer s.Close()
	cfg := setup.leads
	n := *size
	if n == 0 {
		n = cfg.Caps.BatchSize
	}
	shadow, idDomain := batchMode(setup.env)
	if setup.env.Live() && shadow {
		_, _ = fmt.Fprintln(stdout, "LEADHOUND_OUTREACH is live but LEADHOUND_OUTREACH_USER names no mailbox: this batch is shadow.")
	}
	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsBatch, sourceLeads)
	if err != nil {
		return err
	}
	lq := leadstore.NewQueries(s.Pool())
	var (
		drafts  []leadstore.DraftRow
		dropped []string
		batchID int64
	)
	runErr := func() error {
		cands, err := lq.CandidateLeads(ctx, leadstore.CandidateOptions{KindsPriority: cfg.KindsPriority, IncludeReserved: shadow})
		if err != nil {
			return err
		}
		taken := 0
		for start := 0; start < len(cands) && taken < n; start += evidenceSpan {
			span := cands[start:min(start+evidenceSpan, len(cands))]
			ids := make([]int64, len(span))
			for i, c := range span {
				ids[i] = c.LeadID
			}
			ev, err := lq.LeadEvidence(ctx, ids)
			if err != nil {
				return err
			}
			for _, c := range span {
				if taken >= n {
					break
				}
				ds, err := draftLead(c, ev[c.LeadID], cfg, idDomain, rand.Reader)
				if err != nil {
					dropped = append(dropped, fmt.Sprintf("%s (%s, %s): %v", c.Domain, c.Kind, c.Channel, err))
					continue
				}
				drafts = append(drafts, ds...)
				taken++
			}
		}
		if len(drafts) == 0 {
			return nil
		}
		tsha, _ := hex.DecodeString(cfg.Templates.SHA256)
		return leadsInTx(ctx, s.Pool(), func(tq leadstore.Queries) error {
			var err error
			batchID, _, err = tq.CreateBatch(ctx, shadow, drafts, tsha)
			return err
		})
	}()
	result := store.RunResult{Note: fmt.Sprintf("batch %d, %d messages, %d dropped", batchID, len(drafts), len(dropped))}
	if runErr != nil {
		result.Note = runErr.Error()
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, result); err != nil && runErr == nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	for _, d := range dropped {
		_, _ = fmt.Fprintf(stdout, "dropped %s\n", d)
	}
	if batchID == 0 {
		_, _ = fmt.Fprintln(stdout, "No batch: no candidate lead has a draft that passes the checks.")
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "Batch %d (%s): %s. Read: leadhound leads batch show %d\n",
		batchID, batchKind(shadow), batchCounts(drafts), batchID)
	return nil
}

func batchKind(shadow bool) string {
	if shadow {
		return "shadow"
	}
	return "live"
}

// batchCounts says "8 emails, 2 contact forms, 1 LinkedIn note": email leads, form texts
// and LinkedIn notes.
func batchCounts(drafts []leadstore.DraftRow) string {
	var emails, forms, notes int
	for _, d := range drafts {
		switch {
		case d.Channel == "email" && d.Step == 0:
			emails++
		case d.Channel == "form":
			forms++
		case d.Channel == "linkedin":
			notes++
		}
	}
	return fmt.Sprintf("%s, %s, %s", plural(emails, "email", "emails"),
		plural(forms, "contact form", "contact forms"), plural(notes, "LinkedIn note", "LinkedIn notes"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// leadSlots fills every slot one lead's drafts may use from its stored rows, each with the
// row it came from.
func leadSlots(c leadstore.Candidate, ev leadstore.Evidence, cfg leadconfig.Config) (outreach.Slots, error) {
	org := outreach.Provenance{Table: slotOrg, ID: c.OrgID}
	s := outreach.Slots{
		Domain:       outreach.Slot{Value: c.Domain, Source: org},
		SenderName:   outreach.ConfigSlot(cfg.Sender.Name),
		IdentityLine: outreach.ConfigSlot(cfg.Sender.IdentityLine),
	}
	// A language without an opt-out line leaves the slot empty, so Check stops its emails.
	if line, ok := cfg.Sender.OptOutLine(cfg.Policy(c.Country).Lang); ok {
		s.OptOutLine = outreach.ConfigSlot(line)
	}
	s.OrgName = outreach.OrgNameSlot(outreach.Slot{Value: orgNameValue(c.OrgName), Source: org}, s.Domain)
	switch {
	case ev.ProbeID != nil:
		probe := outreach.Provenance{Table: slotProbe, ID: *ev.ProbeID}
		var (
			line  string
			fetch leadstore.EvidenceFetch
		)
		for _, sig := range ev.Signals {
			l, err := outreach.EvidenceLine(drupal.Signal{Name: sig.Signal.Signal, Value: sig.Value}, ev.Major)
			if err == nil && sig.Fetch.ID > 0 {
				line, fetch = l, sig.Fetch
				break
			}
		}
		if line == "" {
			return outreach.Slots{}, fmt.Errorf("no stored signal names Drupal %d", ev.Major)
		}
		s.EvidenceLine = outreach.Slot{Value: line, Source: probe}
		s.FindingURL = outreach.Slot{Value: cmpOr(fetch.FinalURL, fetch.URL), Source: outreach.Provenance{Table: slotFetch, ID: fetch.ID}}
		s.Host = outreach.Slot{Value: cmpOr(ev.FinalHost, c.Domain), Source: probe}
		s.Major = outreach.Slot{Value: strconv.Itoa(ev.Major), Source: probe}
		if ev.ExactVersion != "" {
			s.Exact = outreach.Slot{Value: ev.ExactVersion, Source: probe}
		}
		if eol, ok := cfg.Facts.EOL(ev.Major); ok && !eol.IsZero() {
			s.EOLDate = outreach.ConfigSlot(outreach.FormatDate(eol.Time))
		}
	case ev.AgencyFindingID != nil:
		finding := outreach.Provenance{Table: slotFinding, ID: *ev.AgencyFindingID}
		line, err := outreach.AgencyEvidenceLine(ev.FindingURL)
		if err != nil {
			return outreach.Slots{}, err
		}
		s.EvidenceLine = outreach.Slot{Value: line, Source: finding}
		s.FindingURL = outreach.Slot{Value: ev.FindingURL, Source: finding}
		s.Host = outreach.Slot{Value: c.Domain, Source: org}
	default:
		return outreach.Slots{}, errors.New("the lead has no finding on file")
	}
	return s, nil
}

// maxOrgNameRunes is the longest organisation name a draft uses.
const maxOrgNameRunes = 80

// orgNameValue is the stored org name as a draft may use it: trimmed, at most
// maxOrgNameRunes runes, and "" (so the slot falls back to the domain) when the name is
// blank or is itself a domain or URL.
func orgNameValue(name string) string {
	n := strings.Join(strings.Fields(name), " ")
	if n == "" || strings.Contains(n, "://") || strings.HasPrefix(strings.ToLower(n), "www.") ||
		(!strings.Contains(n, " ") && strings.Contains(strings.Trim(n, "."), ".")) {
		return ""
	}
	if r := []rune(n); len(r) > maxOrgNameRunes {
		n = strings.TrimSpace(string(r[:maxOrgNameRunes]))
	}
	return n
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// draftLead renders and checks one lead's whole sequence: the three email steps, threaded
// to the first email's Message-ID, for an email lead; the contact-form text (step 0) and
// the LinkedIn note (step 1) for a form lead. Any render or Check problem drops the lead.
func draftLead(c leadstore.Candidate, ev leadstore.Evidence, cfg leadconfig.Config, idDomain string, rnd io.Reader) ([]leadstore.DraftRow, error) {
	slots, err := leadSlots(c, ev, cfg)
	if err != nil {
		return nil, err
	}
	policy := cfg.Policy(c.Country)
	set, ok := cfg.Templates.Set(c.Kind, policy.Lang)
	if !ok {
		return nil, fmt.Errorf("no %s templates in language %q", c.Kind, policy.Lang)
	}
	type text struct {
		tmpl    outreach.Template
		channel string
		step    int
	}
	var texts []text
	switch c.Channel {
	case leadconfig.ChannelEmail:
		footer := cfg.Templates.Footers[policy.Footer]
		if len(set.Steps) != leadconfig.Steps || footer == nil {
			return nil, fmt.Errorf("no email templates or footer for country %q", c.Country)
		}
		if c.RouteKind != leadstore.RouteEmail || c.Route == "" {
			return nil, errors.New("the lead has no email route")
		}
		for i, e := range set.Steps {
			texts = append(texts, text{outreach.EmailTemplate(e, footer), "email", i})
		}
	case leadconfig.ChannelForm:
		if set.Form == nil || set.LinkedIn == nil {
			return nil, fmt.Errorf("no form or LinkedIn template in language %q", policy.Lang)
		}
		texts = append(texts, text{outreach.FormTemplate(set.Form), "form", 0}, text{outreach.LinkedInTemplate(set.LinkedIn), "linkedin", 1})
	default:
		return nil, fmt.Errorf("channel %q has no drafts", c.Channel)
	}
	var out []leadstore.DraftRow
	first := ""
	for _, t := range texts {
		d, err := outreach.Render(t.tmpl, slots)
		if err != nil {
			return nil, fmt.Errorf("%s step %d: %w", t.channel, t.step, err)
		}
		checkStep := t.step
		if t.channel != "email" {
			checkStep = 0
		}
		if p := outreach.Check(d, checkStep, cfg); len(p) > 0 {
			msgs := make([]string, len(p))
			for i, x := range p {
				msgs[i] = x.String()
			}
			return nil, fmt.Errorf("%s step %d: %s", t.channel, t.step, strings.Join(msgs, "; "))
		}
		raw, err := slotsJSON(d.Slots)
		if err != nil {
			return nil, err
		}
		row := leadstore.DraftRow{LeadID: c.LeadID, Step: t.step, Channel: t.channel, Subject: d.Subject, Body: d.Body, Slots: raw}
		if t.channel == "email" {
			row.To = c.Route
			if row.MessageID, err = outreach.NewMessageID(idDomain, rnd); err != nil {
				return nil, err
			}
			if t.step == 0 {
				first = row.MessageID
			} else {
				row.InReplyTo = first
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// slotJSON is one slot as stored in outreach_message.slots.
type slotJSON struct {
	Value string `json:"value"`
	Table string `json:"table"`
	ID    int64  `json:"id,omitempty"`
}

// slotsJSON writes every non-empty slot with the row it came from.
func slotsJSON(s outreach.Slots) (json.RawMessage, error) {
	m := map[string]slotJSON{}
	for _, ns := range []struct {
		name string
		slot outreach.Slot
	}{
		{"OrgName", s.OrgName}, {"Domain", s.Domain}, {"Host", s.Host}, {"Major", s.Major}, {"Exact", s.Exact},
		{"EvidenceLine", s.EvidenceLine}, {"FindingURL", s.FindingURL}, {"EOLDate", s.EOLDate},
		{"SenderName", s.SenderName}, {"IdentityLine", s.IdentityLine}, {"OptOutLine", s.OptOutLine}, {"Footer", s.Footer},
	} {
		if ns.slot.Value != "" {
			m[ns.name] = slotJSON{Value: ns.slot.Value, Table: ns.slot.Source.Table, ID: ns.slot.Source.ID}
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("leads: slots: %w", err)
	}
	return b, nil
}

// batchItem is one lead of a batch and its messages, numbered as ApproveBatch's skip list.
type batchItem struct {
	n     int
	lead  leadstore.Lead
	org   leadstore.Org
	route string // the lead's route value (an address or a form page)
	msgs  []leadstore.Message
}

// batchItems groups a batch's messages by lead, in BatchMessages order, numbered from 1.
func batchItems(ctx context.Context, lq leadstore.Queries, msgs []leadstore.Message) ([]batchItem, error) {
	var items []batchItem
	for _, m := range msgs {
		if len(items) == 0 || items[len(items)-1].lead.ID != m.LeadID {
			l, err := lq.LeadByID(ctx, m.LeadID)
			if err != nil {
				return nil, err
			}
			o, err := lq.OrgByID(ctx, l.OrgID)
			if err != nil {
				return nil, err
			}
			it := batchItem{n: len(items) + 1, lead: l, org: o}
			if l.RouteID != nil {
				routes, err := lq.RoutesFor(ctx, o.ID)
				if err != nil {
					return nil, err
				}
				for _, r := range routes {
					if r.ID == *l.RouteID {
						it.route = r.Value
					}
				}
			}
			items = append(items, it)
		}
		items[len(items)-1].msgs = append(items[len(items)-1].msgs, m)
	}
	return items, nil
}

// parseBatchID reads the batch number argument.
func parseBatchID(pos []string, usage string) (int64, error) {
	if len(pos) != 1 {
		return 0, usageError("%s", usage)
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id < 1 {
		return 0, usageError("%q is not a batch number; %s", pos[0], usage)
	}
	return id, nil
}

// runBatchShow is `leads batch show N [--item K]`: every draft in full, numbered by lead,
// each with the finding it rests on, ending with the approve line that binds the hash.
func runBatchShow(ctx context.Context, args []string) error {
	const usage = "usage: leadhound leads batch show N [--item K]"
	fs := flag.NewFlagSet("leads batch show", flag.ContinueOnError)
	item := fs.Int("item", 0, "show only this item")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	id, err := parseBatchID(pos, usage)
	if err != nil {
		return err
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	b, err := lq.BatchByID(ctx, id)
	if errors.Is(err, leadstore.ErrBatchNotFound) {
		return usageError("no batch %d", id)
	}
	if err != nil {
		return err
	}
	msgs, err := lq.BatchMessages(ctx, id)
	if err != nil {
		return err
	}
	items, err := batchItems(ctx, lq, msgs)
	if err != nil {
		return err
	}
	if *item < 0 || *item > len(items) {
		return usageError("batch %d has items 1 to %d", id, len(items))
	}
	leadIDs := make([]int64, len(items))
	for i, it := range items {
		leadIDs[i] = it.lead.ID
	}
	ev, err := lq.LeadEvidence(ctx, leadIDs)
	if err != nil {
		return err
	}
	sum, err := lq.BatchContentSHA256(ctx, id)
	if err != nil {
		return err
	}
	printBatch(stdout, b, items, ev, *item, !bytes.Equal(sum, b.ContentSHA256))
	return nil
}

const showDate = "2 Jan 2006"

// printBatch writes the batch for reading. changed means its rows no longer hash to the
// content hash batch new stored, so no approve line is printed.
func printBatch(w io.Writer, b leadstore.Batch, items []batchItem, ev map[int64]leadstore.Evidence, only int, changed bool) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	status := "awaiting your approval"
	if b.ApprovedAt != nil {
		status = "approved " + b.ApprovedAt.UTC().Format(showDate)
	}
	p("Batch %d (%s) · %s · %s · created %s\n", b.ID, batchKind(b.Shadow), plural(len(items), "lead", "leads"), status, b.CreatedAt.UTC().Format(showDate))
	p("Decide: read each item's finding and drafts; approve the batch, leaving out any item by its number.\n")
	if b.Shadow {
		p("Shadow batch: nothing is sent and its leads stay new.\n")
	}
	for _, it := range items {
		if only != 0 && it.n != only {
			continue
		}
		p("\n%d. %s · %s · %s\n", it.n, it.org.Domain, it.lead.Kind, it.lead.Channel)
		printEvidence(w, ev[it.lead.ID])
		for _, m := range it.msgs {
			p("\n   -- %s --\n", messageLabel(m, len(it.msgs)))
			switch {
			case m.ToAddress != "":
				p("   To: %s\n", m.ToAddress)
			case m.Channel == "form" && it.route != "":
				p("   Form page (the lead's current route): %s\n", it.route)
			}
			if m.Subject != "" {
				p("   Subject: %s\n\n", m.Subject)
			}
			for _, line := range strings.Split(strings.TrimRight(m.BodyText, "\n"), "\n") {
				if line = strings.TrimRight(line, " \t\r"); line == "" {
					p("\n")
				} else {
					p("   %s\n", line)
				}
			}
		}
	}
	p("\n")
	switch {
	case changed:
		p("Batch changed since it was drafted: its messages no longer match what batch new wrote, so it cannot be approved. Draft a new one: leadhound leads batch new\n")
	case b.ApprovedAt != nil:
		p("Approved %s; nothing to approve.\n", b.ApprovedAt.UTC().Format(showDate))
	case only != 0:
		p("To approve, read the whole batch: leadhound leads batch show %d\n", b.ID)
	default:
		p("Approve: leadhound leads batch approve %d --hash %s\n", b.ID, hex.EncodeToString(b.ContentSHA256)[:leadstore.MinHashPrefix])
	}
}

func messageLabel(m leadstore.Message, n int) string {
	var label string
	switch m.Channel {
	case "email":
		label = fmt.Sprintf("email %d of %d · message %d", m.Step+1, n, m.ID)
		if m.InReplyTo != "" {
			label += " · a reply in the same thread"
		}
	case "form":
		label = fmt.Sprintf("contact form text · message %d · when used: leadhound leads done %d", m.ID, m.ID)
	case "linkedin":
		label = fmt.Sprintf("LinkedIn note · message %d · when used: leadhound leads done %d", m.ID, m.ID)
	}
	if m.State != leadstore.MsgDraft {
		label += " · " + m.State
		if m.CancelReason != "" {
			label += " (" + m.CancelReason + ")"
		}
	}
	return label
}

func printEvidence(w io.Writer, e leadstore.Evidence) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	switch {
	case e.ProbeID != nil:
		version := fmt.Sprintf("Drupal %d", e.Major)
		if e.ExactVersion != "" {
			version += " (" + e.ExactVersion + ")"
		}
		p("   Finding: %s on %s, probe %d of %s\n", version, cmpOr(e.FinalHost, "the site"), *e.ProbeID, e.ProbedAt.UTC().Format(showDate))
		for _, s := range e.Signals {
			sig := s.Signal.Signal
			if s.Value != "" {
				sig += " = " + s.Value
			}
			p("   Evidence: %s · %s · fetched %s\n", sig, cmpOr(s.Fetch.FinalURL, s.Fetch.URL), fetchedOn(s.Fetch))
		}
	case e.AgencyFindingID != nil:
		p("   Finding: agency page %s says %q\n", e.FindingURL, e.Phrase)
		p("   Evidence: %s · fetched %s\n", cmpOr(e.FindingFetch.FinalURL, e.FindingFetch.URL), fetchedOn(e.FindingFetch))
	default:
		p("   Finding: none on file\n")
	}
}

func fetchedOn(f leadstore.EvidenceFetch) string {
	if f.FetchedAt.IsZero() {
		return "(fetch not on file)"
	}
	return f.FetchedAt.UTC().Format(showDate)
}

// parseSkip reads --skip "3,5".
func parseSkip(v string) ([]int, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	var out []int
	for _, f := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			return nil, usageError("--skip %q: item numbers are whole numbers from 1, comma-separated", v)
		}
		out = append(out, n)
	}
	return out, nil
}

// nextBusinessSlot is the first moment at or after now inside the business hours, in loc:
// now itself when it falls inside them, else the start of the next business day's window.
// It returns the zero time when the hours do not parse or no day in the next two weeks is
// a business day.
func nextBusinessSlot(now time.Time, loc *time.Location, bh leadconfig.BusinessHours) time.Time {
	start, err1 := time.Parse("15:04", bh.Start)
	end, err2 := time.Parse("15:04", bh.End)
	if err1 != nil || err2 != nil {
		return time.Time{}
	}
	t := now.In(loc)
	for i := range 15 {
		d := t.AddDate(0, 0, i)
		if !slices.Contains(bh.Days, d.Weekday().String()[:3]) {
			continue
		}
		open := time.Date(d.Year(), d.Month(), d.Day(), start.Hour(), start.Minute(), 0, 0, loc)
		shut := time.Date(d.Year(), d.Month(), d.Day(), end.Hour(), end.Minute(), 0, 0, loc)
		switch {
		case i > 0 || t.Before(open):
			return open
		case t.Before(shut):
			return t
		}
	}
	return time.Time{}
}

// runBatchApprove is `leads batch approve N --hash H [--skip 3,5]`: it approves what batch
// show printed (the hash binds it, RA2). Each first email is due at the next business slot
// in its recipient's time zone; follow-ups and the manual items have no due time.
func runBatchApprove(ctx context.Context, args []string) error {
	const usage = "usage: leadhound leads batch approve N --hash H [--skip 3,5]"
	fs := flag.NewFlagSet("leads batch approve", flag.ContinueOnError)
	hash := fs.String("hash", "", "the hash batch show printed")
	skipFlag := fs.String("skip", "", "item numbers to leave out, comma-separated")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	id, err := parseBatchID(pos, usage)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*hash) == "" {
		return usageError("--hash is required: run leadhound leads batch show %d and copy its approve line", id)
	}
	skip, err := parseSkip(*skipFlag)
	if err != nil {
		return err
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	b, err := lq.BatchByID(ctx, id)
	if errors.Is(err, leadstore.ErrBatchNotFound) {
		return usageError("no batch %d", id)
	}
	if err != nil {
		return err
	}
	msgs, err := lq.BatchMessages(ctx, id)
	if err != nil {
		return err
	}
	items, err := batchItems(ctx, lq, msgs)
	if err != nil {
		return err
	}
	locs := map[int64]*time.Location{}
	for _, it := range items {
		locs[it.lead.ID] = setup.leads.Policy(it.org.Country).Location()
	}
	now := leadsNow()
	var (
		firstDue time.Time
		manual   []int64
	)
	due := func(m leadstore.Message) time.Time {
		if m.Channel != "email" {
			manual = append(manual, m.ID)
			return time.Time{}
		}
		if m.Step != 0 {
			return time.Time{}
		}
		t := nextBusinessSlot(now, locs[m.LeadID], setup.leads.Schedule.BusinessHours)
		if !t.IsZero() && (firstDue.IsZero() || t.Before(firstDue)) {
			firstDue = t
		}
		return t
	}
	var a leadstore.Approval
	err = leadsInTx(ctx, s.Pool(), func(tq leadstore.Queries) error {
		var err error
		a, err = tq.ApproveBatch(ctx, id, *hash, skip, due)
		return err
	})
	switch {
	case errors.Is(err, leadstore.ErrHashMismatch), errors.Is(err, leadstore.ErrHashTooShort):
		return usageError("batch %d: the hash does not match what batch show printed; nothing was approved. Read it again: leadhound leads batch show %d", id, id)
	case errors.Is(err, leadstore.ErrBatchTampered):
		return usageError("batch %d changed since it was drafted (its messages no longer match its hash); nothing was approved. Draft a new one: leadhound leads batch new", id)
	case errors.Is(err, leadstore.ErrBadSkip):
		return usageError("batch %d has items 1 to %d; nothing was approved", id, len(items))
	case err != nil:
		return err
	}
	if a.Already {
		_, _ = fmt.Fprintf(stdout, "Batch %d was already approved; nothing changed.\n", id)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "Batch %d (%s) approved: %s approved, %s skipped.\n", id, batchKind(b.Shadow),
		plural(a.Approved, "message", "messages"), plural(a.Skipped, "message", "messages"))
	if !firstDue.IsZero() {
		_, _ = fmt.Fprintf(stdout, "First emails are due from %s, each in its recipient's business hours. Follow-ups get their time when send runs.\n",
			firstDue.UTC().Format("Mon 2 Jan 15:04 MST"))
	}
	if len(manual) > 0 {
		ids := make([]string, len(manual))
		for i, m := range manual {
			ids[i] = strconv.FormatInt(m, 10)
		}
		_, _ = fmt.Fprintf(stdout, "Contact forms and LinkedIn notes wait for you (messages %s): after using one, run leadhound leads done <message>.\n", strings.Join(ids, ", "))
	}
	if b.Shadow {
		_, _ = fmt.Fprintln(stdout, "Shadow batch: send writes these to the outbox only, and the leads stay new.")
	}
	return nil
}

// runLeadsDone is `leads done K`: Gremy used contact-form or LinkedIn message K.
func runLeadsDone(ctx context.Context, args []string) error {
	const usage = "usage: leadhound leads done MESSAGE (the message number batch show prints)"
	fs := flag.NewFlagSet("leads done", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("%s", usage)
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id < 1 {
		return usageError("%q is not a message number; %s", pos[0], usage)
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	var d leadstore.Done
	err = leadsInTx(ctx, s.Pool(), func(lq leadstore.Queries) error {
		var err error
		d, err = lq.MarkDone(ctx, id, leadstore.ActorGremy)
		return err
	})
	switch {
	case errors.Is(err, leadstore.ErrMessageNotFound):
		return usageError("no message %d", id)
	case errors.Is(err, leadstore.ErrNotManual):
		return usageError("message %d is an email; send handles emails", id)
	case errors.Is(err, leadstore.ErrMessageState):
		return usageError("message %d is not an approved contact-form or LinkedIn item; approve its batch first", id)
	case err != nil:
		return err
	}
	switch {
	case d.Shadow:
		_, _ = fmt.Fprintf(stdout, "Message %d done (shadow batch: lead %d stays %s).\n", id, d.LeadID, d.LeadState)
	case d.Moved:
		_, _ = fmt.Fprintf(stdout, "Message %d done; lead %d is now contacted.\n", id, d.LeadID)
	default:
		_, _ = fmt.Fprintf(stdout, "Message %d done; lead %d stays %s.\n", id, d.LeadID, d.LeadState)
	}
	return nil
}

// runLeadsMarkContacted is `leads mark-contacted DOMAIN [--note …]` (RA15): Gremy reached
// DOMAIN by hand, so no batch drafts it again.
func runLeadsMarkContacted(ctx context.Context, args []string) error {
	const usage = "usage: leadhound leads mark-contacted DOMAIN [--note TEXT]"
	fs := flag.NewFlagSet("leads mark-contacted", flag.ContinueOnError)
	note := fs.String("note", "", "what you did, kept in the lead's history")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("%s", usage)
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	var c leadstore.Contacted
	err = leadsInTx(ctx, s.Pool(), func(lq leadstore.Queries) error {
		var err error
		c, err = lq.MarkContacted(ctx, pos[0], *note)
		return err
	})
	if errors.Is(err, leadstore.ErrNoDomain) {
		return usageError("%q is not a domain; %s", pos[0], usage)
	}
	if err != nil {
		return err
	}
	org := "existing org"
	if c.OrgCreated {
		org = "new org"
	}
	switch {
	case c.Moved:
		_, _ = fmt.Fprintf(stdout, "Lead %d (%s, %s) is now contacted by hand; %s cancelled.\n", c.LeadID, c.LeadKind, org,
			plural(int(c.Cancelled), "pending message", "pending messages"))
	default:
		_, _ = fmt.Fprintf(stdout, "Lead %d (%s, %s) stays %s; nothing changed.\n", c.LeadID, c.LeadKind, org, c.State)
	}
	return nil
}

// outboxMessage is the mailbox message for an email row: from the outreach mailbox (from,
// with the sender's name), opt-out to the same mailbox, a follow-up threaded to its first
// email.
func outboxMessage(m leadstore.Message, senderName, from string, date time.Time) mailbox.Message {
	mm := mailbox.Message{
		FromName: senderName, FromAddr: from, To: m.ToAddress, Subject: m.Subject, Body: m.BodyText,
		MessageID: m.MessageID, InReplyTo: m.InReplyTo, ListUnsubscribe: from, Date: date,
	}
	if m.InReplyTo != "" {
		mm.References = []string{m.InReplyTo}
	}
	return mm
}
