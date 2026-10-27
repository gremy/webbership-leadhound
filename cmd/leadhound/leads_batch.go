package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

func init() {
	leadsCommands["batch"] = runLeadsBatch
	leadsCommands["done"] = runLeadsDone
	leadsCommands["mark-contacted"] = runLeadsMarkContacted
	leadsCommands["resolve"] = runLeadsResolve
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

const leadsBatchUsage = "usage: leadhound leads batch new [--size N] | show N [--item K] | approve N --hash H [--skip 3,5] | discard N"

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
	case "discard":
		return runBatchDiscard(ctx, args[1:])
	}
	return usageError("unknown batch subcommand %q; %s", args[0], leadsBatchUsage)
}

// batchMode is whether a new batch is shadow: unless LEADHOUND_OUTREACH is live (RA1). Each
// draft's Message-ID ends in its own mailbox's domain.
func batchMode(env leadconfig.Env) (shadow bool) {
	return !env.Live()
}

// runBatchNew is `leads batch new [--size N]`: newBatch, which drafts the whole sequence
// for the top candidate leads (kinds_priority, then list rank), a live batch sized to what
// the ready mailboxes can send, drops a lead whose draft fails Check and writes the batch;
// never while another batch of its kind waits for approval. Before a live batch, and
// outside its transaction, it runs today's DNS check of every active mailbox's domain and
// selector that has no pass today (decision 5: the 06:15 timer runs before any send run),
// which newBatch itself never does.
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
	loc, err := leadsZone()
	if err != nil {
		return err
	}
	bs := newBatchSetup(setup, loc)
	now := leadsNow()
	if !bs.shadow {
		if err := checkMailboxDomains(ctx, leadstore.NewQueries(s.Pool()), loc, now); err != nil {
			return err
		}
	}
	out, err := makeBatch(ctx, s, bs, now, NewBatchRequest{Size: *size})
	if errors.Is(err, console.ErrUserFixable) {
		return usageError("%s", strings.TrimPrefix(err.Error(), console.ErrUserFixable.Error()+": "))
	}
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseBatchWaiting:
			_, _ = fmt.Fprintf(stdout, "Batch %d still waits for approval; no new batch.\n", r.BatchID)
			return nil
		case RefuseNoStarred:
			// A normal morning when nothing is starred: the batch timer must not fail on it.
			_, _ = fmt.Fprintln(stdout, noStarredWords)
			return nil
		case RefuseSettingsIgnored:
			// Not a normal morning: the batch timer fails (exit 2), so it shows in systemctl.
			return usageError("%s", settingsIgnoredWords)
		case RefuseNoRoom:
			if r.Sizing.Waiting == 0 {
				_, _ = fmt.Fprintf(stdout, "The warm-up ramp sends nothing over the next %d days; no new batch.\n", r.Sizing.Days)
			} else {
				_, _ = fmt.Fprintln(stdout, "Enough approved mail waits for the next days; no new batch.")
			}
			_, _ = fmt.Fprintf(stdout, "Sizing: %s.\n", r.Sizing.Words())
			return nil
		}
	}
	if err != nil {
		return err
	}
	for _, d := range out.Dropped {
		_, _ = fmt.Fprintf(stdout, "dropped %s\n", d)
	}
	if out.BatchID == 0 {
		_, _ = fmt.Fprintln(stdout, "No batch: no candidate lead has a draft that passes the checks.")
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "Batch %d (%s): %s. Read: leadhound leads batch show %d\n",
		out.BatchID, batchKind(out.Shadow), out.Counts, out.BatchID)
	if out.Sizing != nil {
		_, _ = fmt.Fprintf(stdout, "Sizing: %s.\n", out.Sizing.Words())
	}
	return nil
}

// checkMailboxDomains runs and records today's DNS check (checkSenderFor) once for each
// address host and selector of an active mailbox that has no pass since the start of the
// operator's day. A failed check is recorded too, so newBatch can say what is missing.
func checkMailboxDomains(ctx context.Context, lq leadstore.Queries, loc *time.Location, now time.Time) error {
	mbs, err := lq.Mailboxes(ctx, startOfDay(now, loc))
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range mbs {
		key := m.Host() + "\x00" + m.DKIMSelector
		if m.State != leadstore.MailboxActive || m.DNSPassedToday || seen[key] {
			continue
		}
		seen[key] = true
		if _, err := checkSenderFor(ctx, lq, m.Host(), m.DKIMSelector, now); err != nil {
			return err
		}
	}
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
func batchCounts(drafts []leadstore.DraftRow) string { return countDrafts(drafts).String() }

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
	lang := cfg.Policy(c.Country).Lang
	if line, ok := cfg.Sender.OptOutLine(lang); ok {
		s.OptOutLine = outreach.ConfigSlot(line)
	}
	s.OrgName = outreach.OrgNameSlot(outreach.Slot{Value: orgNameValue(c.OrgName), Source: org}, s.Domain)
	switch {
	case ev.ProbeID != nil:
		probe := outreach.Provenance{Table: slotProbe, ID: *ev.ProbeID}
		var (
			line   string
			fetch  leadstore.EvidenceFetch
			stored drupal.Signal
		)
		for _, sig := range ev.Signals {
			ds := drupal.Signal{Name: sig.Signal.Signal, Value: sig.Value}
			l, err := outreach.EvidenceLine(ds, ev.Major, lang)
			if err == nil && sig.Fetch.ID > 0 {
				line, fetch, stored = l, sig.Fetch, ds
				break
			}
		}
		if line == "" {
			return outreach.Slots{}, fmt.Errorf("no stored signal names Drupal %d", ev.Major)
		}
		s.EvidenceLine = outreach.Slot{Value: line, Source: probe}
		s.EvidenceSignal = stored
		s.FindingURL = outreach.Slot{Value: cmpOr(fetch.FinalURL, fetch.URL), Source: outreach.Provenance{Table: slotFetch, ID: fetch.ID}}
		s.Host = outreach.Slot{Value: cmpOr(ev.FinalHost, c.Domain), Source: probe}
		s.Major = outreach.Slot{Value: strconv.Itoa(ev.Major), Source: probe}
		if ev.ExactVersion != "" {
			s.Exact = outreach.Slot{Value: ev.ExactVersion, Source: probe}
		}
		if eol, ok := cfg.Facts.EOL(ev.Major); ok && !eol.IsZero() {
			// A language without date wording leaves the slot empty: a template using it gets no draft.
			if d, err := outreach.FormatDate(eol.Time, lang); err == nil {
				s.EOLDate = outreach.ConfigSlot(d)
			}
		}
	case ev.AgencyFindingID != nil:
		finding := outreach.Provenance{Table: slotFinding, ID: *ev.AgencyFindingID}
		line, err := outreach.AgencyEvidenceLine(ev.FindingURL, lang)
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

// BatchItem is one lead of a batch and its messages, numbered as ApproveBatch's skip list.
type BatchItem struct {
	N     int
	Lead  leadstore.Lead
	Org   leadstore.Org
	Route string // the lead's route value (an address or a form page)
	Msgs  []leadstore.Message
}

// batchItems groups a batch's messages by lead, in BatchMessages order, numbered from 1.
func batchItems(ctx context.Context, lq leadstore.Queries, msgs []leadstore.Message) ([]BatchItem, error) {
	var items []BatchItem
	for _, m := range msgs {
		if len(items) == 0 || items[len(items)-1].Lead.ID != m.LeadID {
			l, err := lq.LeadByID(ctx, m.LeadID)
			if err != nil {
				return nil, err
			}
			o, err := lq.OrgByID(ctx, l.OrgID)
			if err != nil {
				return nil, err
			}
			it := BatchItem{N: len(items) + 1, Lead: l, Org: o}
			if l.RouteID != nil {
				routes, err := lq.RoutesFor(ctx, o.ID)
				if err != nil {
					return nil, err
				}
				for _, r := range routes {
					if r.ID == *l.RouteID {
						it.Route = r.Value
					}
				}
			}
			items = append(items, it)
		}
		items[len(items)-1].Msgs = append(items[len(items)-1].Msgs, m)
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
	v, err := batchView(ctx, leadstore.NewQueries(s.Pool()), id)
	if r := asRefusal(err); r != nil && r.Kind == RefuseBatchNotFound {
		return usageError("no batch %d", id)
	}
	if err != nil {
		return err
	}
	if *item < 0 || *item > len(v.Items) {
		return usageError("batch %d has items 1 to %d", id, len(v.Items))
	}
	printBatch(stdout, v, *item)
	return nil
}

const showDate = "2 Jan 2006"

// printBatch writes the batch for reading. A batch whose rows no longer hash to the content
// hash batch new stored gets no approve line, nor does an approved or discarded one.
func printBatch(w io.Writer, v BatchView, only int) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	b, items, ev := v.Batch, v.Items, v.Evidence
	status := "awaiting your approval"
	switch {
	case b.ApprovedAt != nil:
		status = "approved " + b.ApprovedAt.UTC().Format(showDate)
	case b.DiscardedAt != nil:
		status = "discarded " + b.DiscardedAt.UTC().Format(showDate)
	}
	p("Batch %d (%s) · %s · %s · created %s\n", b.ID, batchKind(b.Shadow), plural(len(items), "lead", "leads"), status, b.CreatedAt.UTC().Format(showDate))
	p("Decide: read each item's finding and drafts; approve the batch, leaving out any item by its number.\n")
	if b.Shadow {
		p("Shadow batch: nothing is sent and its leads stay new.\n")
	}
	for _, it := range items {
		if only != 0 && it.N != only {
			continue
		}
		p("\n%d. %s · %s · %s\n", it.N, it.Org.Domain, it.Lead.Kind, it.Lead.Channel)
		printEvidence(w, ev[it.Lead.ID])
		for _, m := range it.Msgs {
			p("\n   -- %s --\n", messageLabel(m, len(it.Msgs)))
			if m.Channel == "email" && m.FromAddress != "" {
				p("   From: %s\n", fromLine(m.FromName, m.FromAddress))
			}
			switch {
			case m.ToAddress != "":
				p("   To: %s\n", m.ToAddress)
			case m.Channel == "form" && it.Route != "":
				p("   Form page (the lead's current route): %s\n", it.Route)
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
	case v.Changed:
		p("Batch changed since it was drafted: its messages no longer match what batch new wrote, so it cannot be approved. Draft a new one: leadhound leads batch new\n")
	case b.ApprovedAt != nil:
		p("Approved %s; nothing to approve.\n", b.ApprovedAt.UTC().Format(showDate))
	case b.DiscardedAt != nil:
		p("Discarded %s; nothing to approve. Draft a new one: leadhound leads batch new\n", b.DiscardedAt.UTC().Format(showDate))
	case only != 0:
		p("To approve, read the whole batch: leadhound leads batch show %d\n", b.ID)
	default:
		p("Approve: leadhound leads batch approve %d --hash %s\n", b.ID, v.HashPrefix())
	}
}

// fromLine is "Name <address>", or the bare address with no name.
func fromLine(name, address string) string {
	if name = strings.TrimSpace(name); name == "" {
		return address
	}
	return name + " <" + address + ">"
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

// runBatchApprove is `leads batch approve N --hash H [--skip 3,5]`: approveBatch, which
// approves what batch show printed (the hash binds it, RA2). Each first email is due at the
// next business slot in its recipient's time zone; follow-ups and the manual items have no
// due time.
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
	out, err := approveBatch(ctx, s, setup.leads, leadsNow, ApproveRequest{ID: id, HashPrefix: *hash, Skip: skip})
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseBatchNotFound:
			return usageError("no batch %d", id)
		case RefuseHashMismatch:
			return usageError("batch %d: the hash does not match what batch show printed; nothing was approved. Read it again: leadhound leads batch show %d", id, id)
		case RefuseBatchChanged:
			return usageError("batch %d changed since it was drafted (its messages no longer match its hash); nothing was approved. Draft a new one: leadhound leads batch new", id)
		case RefuseBadSkip:
			return usageError("batch %d has items 1 to %d; nothing was approved", id, r.Items)
		case RefuseBatchDiscarded:
			return usageError("batch %d was discarded; nothing was approved. Draft a new one: leadhound leads batch new", id)
		}
	}
	if err != nil {
		return err
	}
	if out.Approval.Already {
		_, _ = fmt.Fprintf(stdout, "Batch %d was already approved; nothing changed.\n", id)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "Batch %d (%s) approved: %s approved, %s skipped.\n", id, batchKind(out.Shadow),
		plural(out.Approval.Approved, "message", "messages"), plural(out.Approval.Skipped, "message", "messages"))
	if !out.FirstDue.IsZero() {
		_, _ = fmt.Fprintf(stdout, "First emails are due from %s, each in its recipient's business hours. Follow-ups get their time when send runs.\n",
			out.FirstDue.UTC().Format("Mon 2 Jan 15:04 MST"))
	}
	if len(out.Manual) > 0 {
		ids := make([]string, len(out.Manual))
		for i, m := range out.Manual {
			ids[i] = strconv.FormatInt(m, 10)
		}
		_, _ = fmt.Fprintf(stdout, "Contact forms and LinkedIn notes wait for you (messages %s): after using one, run leadhound leads done <message>.\n", strings.Join(ids, ", "))
	}
	if out.Shadow {
		_, _ = fmt.Fprintln(stdout, "Shadow batch: send writes these to the outbox only, and the leads stay new.")
	}
	return nil
}

// runBatchDiscard is `leads batch discard N`: discardBatch, which throws an unapproved batch
// away so its leads can be drafted again.
func runBatchDiscard(ctx context.Context, args []string) error {
	const usage = "usage: leadhound leads batch discard N"
	fs := flag.NewFlagSet("leads batch discard", flag.ContinueOnError)
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
	out, err := discardBatch(ctx, s, id)
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseBatchNotFound:
			return usageError("no batch %d", id)
		case RefuseBatchApproved:
			return usageError("batch %d is approved; an approved batch is never discarded (pause sending, or suppress a lead, to stop its emails)", id)
		}
	}
	if err != nil {
		return err
	}
	if out.Already {
		_, _ = fmt.Fprintf(stdout, "Batch %d was already discarded; nothing changed.\n", id)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "Batch %d (%s) discarded: %s cancelled, %s back to new.\n", id, batchKind(out.Shadow),
		plural(int(out.Cancelled), "message", "messages"), plural(out.LeadsBack, "lead", "leads"))
	return nil
}

// runLeadsDone is `leads done K`: the operator used contact-form or LinkedIn message K.
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
	d, err := markDone(ctx, s, id)
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseMessageNotFound:
			return usageError("no message %d", id)
		case RefuseNotManual:
			return usageError("message %d is an email; send handles emails", id)
		case RefuseMessageState:
			return usageError("message %d is not an approved contact-form or LinkedIn item; approve its batch first", id)
		}
	}
	if err != nil {
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

// runLeadsMarkContacted is `leads mark-contacted DOMAIN [--note …]` (RA15): the operator reached
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
	c, err := markContacted(ctx, s, pos[0], *note)
	if r := asRefusal(err); r != nil && r.Kind == RefuseNotDomain {
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

// runLeadsResolve is `leads resolve K sent|not-sent`: the operator checked email K (maybe
// sent, or left sending by a broken run) in the mailbox's Sent folder. resolveMaybeSent
// records it under the send lock.
func runLeadsResolve(ctx context.Context, args []string) error {
	const usage = "usage: leadhound leads resolve MESSAGE sent|not-sent (after checking the mailbox's Sent folder)"
	fs := flag.NewFlagSet("leads resolve", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || (pos[1] != "sent" && pos[1] != "not-sent") {
		return usageError("%s", usage)
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id < 1 {
		return usageError("%q is not a message number; %s", pos[0], usage)
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	loc, err := leadsZone()
	if err != nil {
		return err
	}
	out, err := resolveMaybeSent(ctx, s, setup, loc, id, pos[1] == "sent")
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseSendRunning:
			return usageError("A send run is in progress; try again in a few minutes.")
		case RefuseMessageNotFound:
			return usageError("no message %d", id)
		case RefuseMessageState:
			return usageError("message %d is not a live email that may have gone out (maybe sent, or left sending); nothing changed", id)
		case RefuseSelftest:
			return usageError("message %d is a selftest's; a selftest is never checked by hand, run a new one: leadhound leads selftest", id)
		}
	}
	if err != nil {
		return err
	}
	if !out.Sent {
		_, _ = fmt.Fprintf(stdout, "Message %d not sent: it is approved again and goes at the next send run in its recipient's hours.\n", id)
		return nil
	}
	lead := fmt.Sprintf("lead %d is now contacted", out.LeadID)
	if !out.LeadMoved {
		lead = fmt.Sprintf("lead %d keeps its state", out.LeadID)
	}
	_, _ = fmt.Fprintf(stdout, "Message %d sent %s; %s, and its follow-ups go on as after any send.\n", id,
		out.SentAt.UTC().Format("Mon 2 Jan 15:04 MST"), lead)
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
