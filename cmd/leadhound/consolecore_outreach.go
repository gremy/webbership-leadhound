package main

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadview"
)

// The console's OutreachBackend and the lead page's outreach fill. Each write calls the function
// its CLI verb calls (leads_contacts_operator.go, leads_proof.go, leads_proposals.go,
// leads_drafts_make.go), so the two cannot drift.

var _ console.OutreachBackend = (*backend)(nil)

// AddContact adds c to the lead's org and re-picks the org's new leads.
func (b *backend) AddContact(ctx context.Context, c console.NewContact) (string, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return "", err
	}
	return addOperatorContact(ctx, b.s.Pool(), setup.leads, c)
}

// EditContact changes a contact and re-picks the org's new leads.
func (b *backend) EditContact(ctx context.Context, c console.ContactChange) (string, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return "", err
	}
	return editOperatorContact(ctx, b.s.Pool(), setup.leads, c)
}

// RetireContact retires a contact and re-picks the org's new leads.
func (b *backend) RetireContact(ctx context.Context, leadID int64, ref console.ContactRef, note string) (string, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return "", err
	}
	return "", retireOperatorContact(ctx, b.s.Pool(), setup.leads, leadID, ref, note)
}

// messageOfLead is nil when message id is lead leadID's, console.ErrNotFound otherwise.
func (b *backend) messageOfLead(ctx context.Context, leadID, id int64) error {
	var owner int64
	err := b.s.Pool().QueryRow(ctx, `SELECT lead_id FROM outreach_message WHERE id = $1`, id).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != leadID) {
		return console.ErrNotFound
	}
	return err
}

// SaveDraft saves a new text by its channel's road (saveDraftEdit, as `leads drafts edit`).
func (b *backend) SaveDraft(ctx context.Context, e console.DraftEdit) (string, error) {
	setup, err := b.leadsSetup(ctx, leadsForOutreach)
	if err != nil {
		return "", err
	}
	if err := b.messageOfLead(ctx, e.LeadID, e.MessageID); err != nil {
		return "", err
	}
	row, _, err := messageBatch(ctx, b.s, e.MessageID)
	if err != nil {
		return "", err
	}
	var subject *string
	if row.Channel == leadconfig.ChannelEmail {
		subject = &e.Subject
	}
	id, err := saveDraftEdit(ctx, b.s, setup, b.loc, e.MessageID, subject, e.Body)
	if err != nil {
		return "", err
	}
	if id == 0 {
		return "The text is what it already was; nothing changed.", nil
	}
	return "", nil
}

// MarkSent records a form or LinkedIn message sent by hand (markManualSent, as `leads done`).
func (b *backend) MarkSent(ctx context.Context, m console.MarkSentRequest) (string, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return "", err
	}
	if err := b.messageOfLead(ctx, m.LeadID, m.MessageID); err != nil {
		return "", err
	}
	_, err := markManualSent(ctx, b.s, m.MessageID, m.Hash)
	if err == nil {
		return "", nil
	}
	if words := manualSentWords(err); words != "" {
		return "", refused("%s", capitalised(words))
	}
	switch r := asRefusal(err); {
	case r != nil && r.Kind == RefuseMessageNotFound:
		return "", console.ErrNotFound
	case r != nil && r.Kind == RefuseNotManual:
		return "", refused("Only a contact form or LinkedIn message can be marked sent.")
	case errors.Is(err, leadstore.ErrHashTooShort):
		return "", refused("This draft changed; reload.")
	}
	return "", err
}

// RequestDraft queues Draft it for the lead and wakes the drafting service, which holds the key;
// this writes a row and a request file, and is secret-free. A lead that cannot be drafted, or has
// a request open, is refused.
func (b *backend) RequestDraft(ctx context.Context, leadID int64, channel string) (string, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return "", err
	}
	lq := b.lq()
	lead, err := lq.LeadByID(ctx, leadID)
	if errors.Is(err, leadstore.ErrLeadNotFound) {
		return "", console.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	org, err := lq.OrgByID(ctx, lead.OrgID)
	if err != nil {
		return "", err
	}
	if err := draftableNow(ctx, b.s, lq, lead, org); err != nil {
		return "", err
	}
	if _, err := draftItCandidate(ctx, lq, lead, org, channel); err != nil {
		return "", err
	}
	_, err = lq.RequestDraft(ctx, leadID, channel)
	var open *leadstore.DraftRequestOpenError
	if errors.As(err, &open) {
		if open.Status == leadstore.DraftRequestQueued {
			// A press that found its request still queued wakes the service again, in case the
			// first wake was lost; nothing else would run it.
			_ = writeRunRequest(setup.dataDir, draftItAction, b.now())
		}
		return "", refused("A draft was already asked for at %s. It shows here when it is ready.", open.RequestedAt.In(b.loc).Format("15:04"))
	}
	if err != nil {
		return "", err
	}
	if err := writeRunRequest(setup.dataDir, draftItAction, b.now()); err != nil {
		slog.Default().Warn("console draft it request file", "failed", 1)
		return "Asked for, but the drafting service could not be woken. Press Draft it again in a minute to wake it.", nil
	}
	return "", nil
}

// DraftStatus is the lead's latest Draft it request.
func (b *backend) DraftStatus(ctx context.Context, leadID int64) (*console.LeadDraftRequest, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return nil, err
	}
	if _, err := leadOrgID(ctx, b.lq(), leadID); err != nil {
		return nil, err
	}
	return b.draftRequest(ctx, leadID)
}

func (b *backend) draftRequest(ctx context.Context, leadID int64) (*console.LeadDraftRequest, error) {
	r, err := b.lq().DraftRequestFor(ctx, leadID)
	if err != nil || r == nil {
		return nil, err
	}
	out := &console.LeadDraftRequest{Channel: r.Channel, Status: r.Status, Reason: r.Reason, RequestedAt: r.RequestedAt, FinishedAt: r.FinishedAt}
	if r.BatchID != nil {
		out.BatchID = *r.BatchID
	}
	return out, nil
}

// discardableBatch is the batch Discard throws away for a lead: its latest batch neither approved
// nor discarded that holds it alone, 0 for none. A morning batch of many leads is never one.
func (b *backend) discardableBatch(ctx context.Context, leadID int64) (int64, error) {
	var id int64
	err := b.s.Pool().QueryRow(ctx,
		`SELECT b.id FROM outreach_batch b
		 WHERE b.approved_at IS NULL AND b.discarded_at IS NULL
		   AND b.id IN (SELECT m.batch_id FROM outreach_message m WHERE m.lead_id = $1)
		   AND NOT EXISTS (SELECT 1 FROM outreach_message o WHERE o.batch_id = b.id AND o.lead_id <> $1)
		 ORDER BY b.id DESC LIMIT 1`, leadID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("console: discardable batch: %w", err)
	}
	return id, nil
}

// DiscardDraft discards the lead's own unapproved batch (discardBatch, as `leads batch discard`).
func (b *backend) DiscardDraft(ctx context.Context, leadID, batchID int64) (string, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return "", err
	}
	own, err := b.discardableBatch(ctx, leadID)
	if err != nil {
		return "", err
	}
	if own != batchID {
		return "", refused("That draft can no longer be discarded; reload. Nothing was discarded.")
	}
	if _, err := discardBatch(ctx, b.s, batchID); err != nil {
		if asRefusal(err) != nil {
			return "", refused("That draft can no longer be discarded; reload. Nothing was discarded.")
		}
		return "", err
	}
	return "", nil
}

// SetSector records the operator's sector for the lead's org (setLeadSector, as `leads sector set`).
func (b *backend) SetSector(ctx context.Context, leadID int64, sector string) (string, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return "", err
	}
	return "", setLeadSector(ctx, b.lq(), leadID, sector)
}

// AddProposal adds a proposal (addLeadProposal, as `leads proposals add`).
func (b *backend) AddProposal(ctx context.Context, p console.NewProposal) (string, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return "", err
	}
	_, err := addLeadProposal(ctx, b.lq(), p, b.now())
	return "", err
}

// SetProposalStatus records a proposal's status (setLeadProposalStatus, as `leads proposals status`).
func (b *backend) SetProposalStatus(ctx context.Context, c console.ProposalStatusChange) (string, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return "", err
	}
	return "", setLeadProposalStatus(ctx, b.lq(), c, b.now())
}

// Proofs is the proof library and the closed list of sectors.
func (b *backend) Proofs(ctx context.Context) (console.ProofPage, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return console.ProofPage{}, err
	}
	proofs, err := b.lq().ListProofs(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return console.ProofPage{}, err
	}
	page := console.ProofPage{Sectors: sectorOptions()}
	for _, p := range proofs {
		page.Proofs = append(page.Proofs, console.Proof{ID: p.ID, Title: p.Title, Line: p.Line, URL: p.URL, Public: p.Public,
			Sectors: sectorOptionsOf(p.Sectors), At: p.At})
	}
	return page, nil
}

// AddProof adds a line of proof (addProofLine, as `leads proof add`).
func (b *backend) AddProof(ctx context.Context, p console.NewProof) (string, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return "", err
	}
	_, err := addProofLine(ctx, b.lq(), p)
	return "", err
}

// RetireProof takes a proof out of use (retireProofLine, as `leads proof retire`).
func (b *backend) RetireProof(ctx context.Context, proofID int64, reason string) (string, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return "", err
	}
	return "", retireProofLine(ctx, b.lq(), proofID, reason)
}

// countryOff is the country rule StarredStatus names a lead by: on the profile's never-contact
// list, or off in leads.yaml (a country the file does not list is off too). A campaign's own list
// is per lead and StarredStatus judges it per lead; the list's rule takes a country alone.
func (w leadsWeighing) countryOff(country string) bool {
	return slices.Contains(w.profile.ExcludeCountries, country) || w.cfg.Policy(country).Channel == leadconfig.ChannelNone
}

// fillOutreach is the lead page's next action, contacts, drafts, Draft it state, proposals, sector
// and note limit.
func (b *backend) fillOutreach(ctx context.Context, w leadsWeighing, d leadstore.Detail, p *console.LeadDetailPage) error {
	lq := b.lq()
	reach, err := lq.ReachOf(ctx, d.LeadID, w.countryOff)
	if err != nil {
		return err
	}
	p.Reach, p.ReachReason = reach.Bucket, reach.Reason
	lead, err := lq.LeadByID(ctx, d.LeadID)
	if err != nil {
		return err
	}
	if p.Contacts, err = b.leadContacts(ctx, w, d, lead); err != nil {
		return err
	}
	if p.Drafts, err = b.leadDrafts(ctx, d); err != nil {
		return err
	}
	if p.DraftRequest, err = b.draftRequest(ctx, d.LeadID); err != nil {
		return err
	}
	if p.DiscardBatchID, err = b.discardableBatch(ctx, d.LeadID); err != nil {
		return err
	}
	if p.DraftItBlock, err = b.draftItBlock(ctx, lead, p.DraftRequest); err != nil {
		return err
	}
	proposals, err := lq.ProposalsFor(ctx, d.LeadID)
	if err != nil {
		return err
	}
	for _, x := range proposals {
		p.Proposals = append(p.Proposals, console.LeadProposal{ID: x.ID, Title: x.Title, URL: x.URL, ValueCents: x.ValueCents,
			Currency: x.Currency, At: x.At, Status: x.Status, StatusNote: x.StatusNote, StatusAt: x.StatusAt})
	}
	domain := d.Domain
	if strings.HasPrefix(domain, leadstore.PlaceOrgPrefix) {
		domain = "" // a place with no website has a stand-in, not a domain to read
	}
	if p.Sector, err = leadview.SectorOf(ctx, lq, d.OrgID, domain, d.Kind); err != nil {
		return err
	}
	operator, err := lq.OperatorSector(ctx, d.OrgID)
	if err != nil {
		return err
	}
	p.SectorByRule = p.Sector != "" && operator == ""
	p.SectorWords = leadview.SectorWords(p.Sector)
	p.SectorOptions = sectorOptions()
	p.NoteMax = cmp.Or(w.cfg.Sender.LinkedInNoteMax, leadconfig.DefaultLinkedInNoteMax)
	return nil
}

// draftItBlock is why Draft it cannot run, in the words its own refusal uses, with no closing
// stop (the page adds it); "" when the button is offered.
func (b *backend) draftItBlock(ctx context.Context, lead leadstore.Lead, req *console.LeadDraftRequest) (string, error) {
	if req != nil && req.Open() {
		return "Asked for at " + req.RequestedAt.In(b.loc).Format("15:04"), nil
	}
	lq := b.lq()
	org, err := lq.OrgByID(ctx, lead.OrgID)
	if err != nil {
		return "", err
	}
	err = draftableNow(ctx, b.s, lq, lead, org)
	if err == nil {
		_, err = draftItCandidate(ctx, lq, lead, org, "")
	}
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, console.ErrUserFixable):
		return strings.TrimSuffix(draftRefusalWords(err), "."), nil
	}
	return "", err
}

// leadContacts is every contact of the lead's org for its card, retired ones included.
func (b *backend) leadContacts(ctx context.Context, w leadsWeighing, d leadstore.Detail, lead leadstore.Lead) ([]console.LeadContact, error) {
	lq := b.lq()
	stored, err := lq.ContactsFor(ctx, d.OrgID)
	if err != nil {
		return nil, err
	}
	routes := map[string]leadstore.DetailRoute{}
	var addrs []string
	for _, r := range d.Routes {
		routes[r.Kind+"\x00"+r.Value] = r
		if r.Kind == leadstore.RouteEmail {
			addrs = append(addrs, r.Value)
		}
	}
	phones := map[string]string{}
	for _, f := range d.Facts {
		if f.Kind == leadstore.FactPhone {
			phones[f.Value] = f.FinalURL
		}
	}
	checks, err := lq.EmailChecks(ctx, leadstore.DefaultAccountID, addrs, time.Time{})
	if err != nil {
		return nil, err
	}
	now := b.now()
	dayStart := startOfDay(now, b.loc)
	law, lawKnown := w.research.table.Lookup(d.Country)
	personalCare := lawKnown && law.Individuals.Email != compliance.IndividualsSame
	off := w.countryOff(d.Country)
	out := make([]console.LeadContact, 0, len(stored))
	for _, c := range stored {
		lc := console.LeadContact{CID: consoleRefRow(c).String(), Kind: c.Kind, Value: c.Value, Name: c.Name, Role: c.Role,
			PersonID: c.PersonID, Provenance: c.Provenance, FoundAt: c.FoundAt, Note: c.Note, Operator: c.Operator, Retired: c.Retired}
		if c.ReplacedBy != 0 {
			i := slices.IndexFunc(stored, func(x leadstore.Contact) bool { return x.ID == c.ReplacedBy && x.Kind == c.Kind })
			if i >= 0 {
				lc.ReplacedBy = stored[i].Value
			}
		}
		switch c.Kind {
		case leadstore.ContactPerson:
			lc.Value = ""
		case leadstore.ContactPhone:
			lc.PageURL = phones[c.Value]
		default:
			lc.Chosen = !c.Retired && lead.RouteID != nil && *lead.RouteID == c.ID
			if r, ok := routes[c.Kind+"\x00"+c.Value]; ok {
				lc.PageURL = r.FinalURL
				if c.Kind == leadstore.RouteEmail {
					if r.DerivedBy != leadstore.DerivedByOperator {
						lc.MailServer = routeNote(r)
					}
					lc.Check, lc.CheckedAt = emailCheckLine(checks[strings.ToLower(c.Value)], w.cfg.Verify.CatchAllRule(), now, dayStart)
				}
			}
			switch {
			case c.Retired:
			case c.Kind == leadstore.RouteEmail && personalCare && personalAddress(c.Value, w.cfg.RoleLocalParts):
				lc.Warning = "This address looks personal, and this country treats a person's own email differently; it is your call."
			case lc.Chosen && off && c.Operator && c.Kind != leadstore.RouteEmail:
				lc.Warning = "This country is off in your settings; this is your own contact, so it is still used."
			}
		}
		out = append(out, lc)
	}
	return out, nil
}

// personalAddress says address looks like a person's rather than a role's: its local part is not a
// role, nor a role followed by "-" or "." and more, as the route search reads roles.
func personalAddress(address string, roles []string) bool {
	local, _, ok := strings.Cut(strings.ToLower(address), "@")
	if !ok {
		return false
	}
	return !slices.ContainsFunc(roles, func(r string) bool {
		r = strings.ToLower(r)
		return local == r || strings.HasPrefix(local, r+"-") || strings.HasPrefix(local, r+".")
	})
}

// draftExtra is what a draft's card reads of its message and batch beyond the detail's row.
type draftExtra struct {
	ID, BatchID      int64
	To               string
	Slots            json.RawMessage
	Origin           string
	Manual, Approved bool
	Discarded        bool
	Hash             []byte
	HoldsEmail       bool
	LeadState        string
}

// leadDrafts is every message of the lead that is written, not called, in sequence order. A
// discarded batch's messages are gone from the page: they stay on the timeline.
func (b *backend) leadDrafts(ctx context.Context, d leadstore.Detail) ([]console.LeadDraft, error) {
	rows, err := b.s.Pool().Query(ctx,
		`SELECT m.id, m.batch_id, m.to_address, m.slots, b.origin, b.manual, b.approved_at IS NOT NULL, b.discarded_at IS NOT NULL,
		   b.content_sha256, EXISTS (SELECT 1 FROM outreach_message e WHERE e.batch_id = b.id AND e.channel = 'email'), l.state
		 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id
		 WHERE m.lead_id = $1 AND m.channel <> 'phone' ORDER BY m.id`, d.LeadID)
	if err != nil {
		return nil, fmt.Errorf("console: lead drafts: %w", err)
	}
	extras, err := pgx.CollectRows(rows, pgx.RowToStructByPos[draftExtra])
	if err != nil {
		return nil, fmt.Errorf("console: lead drafts: %w", err)
	}
	byID := map[int64]draftExtra{}
	for _, x := range extras {
		byID[x.ID] = x
	}
	type place struct{ emails, linkedIn int }
	seen := map[int64]*place{}
	now := b.now()
	lq := b.lq()
	var out []console.LeadDraft
	for _, m := range d.Messages {
		x, ok := byID[m.ID]
		if !ok || x.Discarded {
			continue
		}
		if seen[m.BatchID] == nil {
			seen[m.BatchID] = &place{}
		}
		at := seen[m.BatchID]
		text, err := lq.EffectiveText(ctx, m.ID)
		if err != nil {
			return nil, err
		}
		writer, fallback := draftOrigin(x.Slots)
		dr := console.LeadDraft{MessageID: m.ID, Channel: m.Channel, Step: m.Step, Subject: text.Subject, Body: text.Body,
			Edited: text.Edited, OriginalSubject: text.OriginalSubject, OriginalBody: text.OriginalBody,
			BatchID: m.BatchID, Origin: x.Origin, Shadow: m.Shadow, Writer: writer, Fallback: fallback,
			State: draftStateWords(m.State), Due: m.DueAt, Sent: m.SentAt}
		if len(x.Hash) > 0 {
			dr.BatchHash = hex.EncodeToString(x.Hash)[:leadstore.MinHashPrefix]
		}
		answered := x.LeadState == leadstore.LeadReplied || x.LeadState == leadstore.LeadBounced || x.LeadState == leadstore.LeadOptedOut
		open := m.State == leadstore.MsgDraft || m.State == leadstore.MsgApproved
		switch m.Channel {
		case leadconfig.ChannelEmail:
			at.emails++
			dr.To = x.To
			dr.Label = "First email"
			if at.emails > 1 {
				dr.Label = fmt.Sprintf("Follow-up %d", at.emails-1)
			}
			dr.Editable = m.State == leadstore.MsgDraft && !x.Approved && !answered
		default:
			dr.TargetURL = x.To
			dr.Subject = ""
			if m.Channel == leadconfig.ChannelLinkedIn {
				at.linkedIn++
				dr.Note = at.linkedIn == 1
			}
			dr.Label = console.ManualLabel(m.Channel, at.linkedIn)
			dr.Editable = open && !answered
			dr.CanMarkSent = open && !answered && !m.Shadow && (x.Approved || (x.Manual && !x.HoldsEmail))
			dr.DueNow = dr.CanMarkSent && (m.DueAt == nil || !m.DueAt.After(now))
		}
		out = append(out, dr)
	}
	return out, nil
}

// draftStateWords is a message's state as its card says it.
func draftStateWords(state string) string {
	switch state {
	case leadstore.MsgDraft:
		return "Draft"
	case leadstore.MsgApproved:
		return "Approved"
	case "done":
		return "Marked sent"
	case leadstore.MsgMaybeSent:
		return "May have gone"
	}
	return capitalised(strings.ReplaceAll(state, "_", " "))
}
