// Package outreach turns organisations into leads and leads into outreach. It is pure: the
// command reads the store, maps its rows into this package's input types and writes back
// what these functions decide, so outreach never imports leadstore.
package outreach

import (
	"slices"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// Org kinds (org.kind).
const (
	OrgKindSiteOwner = "site_owner"
	OrgKindAgency    = "agency"
	OrgKindSelftest  = "selftest"
)

// Route kinds (contact_route.kind).
const (
	RouteEmail = "email"
	RouteForm  = "form"
)

// Lead states (lead.state).
const (
	LeadNew        = "new"
	LeadBatched    = "batched"
	LeadContacted  = "contacted"
	LeadReplied    = "replied"
	LeadBounced    = "bounced"
	LeadOptedOut   = "opted_out"
	LeadSuppressed = "suppressed"
	LeadSkipped    = "skipped"
	LeadClosed     = "closed"
)

// Reasons Classify gives for an org that is not a lead; the command counts them.
const (
	ReasonCurrent        = "already current"
	ReasonVersionUnknown = "version unknown"
	ReasonNotDrupal      = "not drupal"
	ReasonNoFinding      = "no finding"
	ReasonPublicBody     = "public body"
	ReasonSelftest       = "selftest"
	ReasonOrgKind        = "unknown org kind"
)

// Org is the part of an org row Classify reads.
type Org struct {
	ID      int64
	Kind    string // site_owner, agency or selftest
	Domain  string // registrable domain
	Country string // ISO 3166-1 alpha-2, '' when unknown
}

// Probe is the part of a site_probe row (the site's last probe) Classify reads.
type Probe struct {
	ID        int64
	IsDrupal  bool
	Major     int    // 0 = unknown
	Ambiguous string // non-empty when the major could not be told apart, e.g. "10-or-11"
}

// AgencyFinding is the agency_finding row an agency lead rests on.
type AgencyFinding struct {
	ID int64
}

// Route is the part of a contact_route row Classify reads. Every stored email route is a
// role address (DECISION 7), so any email route with MXOK is a usable one.
type Route struct {
	ID    int64
	Kind  string // email or form
	Value string
	MXOK  bool
}

// Lead is one lead row to upsert. It has exactly one of ProbeID (site leads) and
// AgencyFindingID (agency leads), as the lead finding CHECK requires. Channel may be
// none: the lead is still recorded, with RouteID nil, so it is counted and stays one
// lead per org; CandidateLeads leaves channel none out of batches.
//
// Reserved marks an org on a reserved TLD (.example, .invalid, .test, .localhost). Such
// orgs are classified normally so fixtures exercise every rule, and the caller leaves
// them out of real batches and the brief (RA25); ReservedDomain is the same test.
type Lead struct {
	OrgID           int64
	Kind            string // unsupported, d10 or agency_partner
	Channel         string // email, form or none
	ProbeID         *int64
	AgencyFindingID *int64
	RouteID         *int64
	Reserved        bool
}

// Classify decides whether org is a lead, of which kind, and on which channel. probe is
// the site's last probe (nil when there is none), finding the agency's finding (nil when
// there is none), routes the org's contact routes in the store's order. When it is not a
// lead, Classify returns the zero Lead, one of the Reason constants and false.
//
// The rules, in order: selftest orgs are never leads; a public-body domain (one of
// cfg.PublicBodySuffixes, or under one) is not; an agency is an agency_partner lead when it
// has a finding; a site owner is unsupported for Drupal 6 to 9 and d10 for Drupal 10,
// while Drupal 11, an unknown or ambiguous major, and anything outside 6 to 11 are not
// leads. The channel follows PickChannel.
func Classify(org Org, probe *Probe, finding *AgencyFinding, routes []Route, cfg leadconfig.Config) (Lead, string, bool) {
	if org.Kind == OrgKindSelftest {
		return Lead{}, ReasonSelftest, false
	}
	if PublicBody(org.Domain, cfg.PublicBodySuffixes) {
		return Lead{}, ReasonPublicBody, false
	}
	l := Lead{OrgID: org.ID, Reserved: ReservedDomain(org.Domain)}
	switch org.Kind {
	case OrgKindAgency:
		if finding == nil {
			return Lead{}, ReasonNoFinding, false
		}
		l.Kind = leadconfig.KindAgencyPartner
		l.AgencyFindingID = &finding.ID
	case OrgKindSiteOwner:
		kind, reason := leadSiteKind(probe)
		if reason != "" {
			return Lead{}, reason, false
		}
		l.Kind = kind
		l.ProbeID = &probe.ID
	default:
		return Lead{}, ReasonOrgKind, false
	}
	l.Channel, l.RouteID = PickChannel(cfg.Policy(org.Country).Channel, routes)
	return l, "", true
}

// leadSiteKind maps a site's probe to a lead kind, or to the reason it is not a lead.
func leadSiteKind(p *Probe) (kind, reason string) {
	switch {
	case p == nil:
		return "", ReasonNoFinding
	case !p.IsDrupal:
		return "", ReasonNotDrupal
	case p.Ambiguous != "":
		return "", ReasonVersionUnknown
	case p.Major >= 6 && p.Major <= 9:
		return leadconfig.KindUnsupported, ""
	case p.Major == 10:
		return leadconfig.KindD10, ""
	case p.Major == 11:
		return "", ReasonCurrent
	default:
		return "", ReasonVersionUnknown
	}
}

// PickChannel picks the channel and route for a country policy's channel. An email policy
// takes the first email route with an MX record, else falls back to the first form route,
// else none. A form policy takes the first form route, else none; it never upgrades to
// email. Any other policy (none, or a country missing from the policy) is none.
func PickChannel(policy string, routes []Route) (string, *int64) {
	first := func(kind string, needMX bool) *int64 {
		for i := range routes {
			if routes[i].Kind == kind && (!needMX || routes[i].MXOK) {
				id := routes[i].ID
				return &id
			}
		}
		return nil
	}
	switch policy {
	case leadconfig.ChannelEmail:
		if id := first(RouteEmail, true); id != nil {
			return leadconfig.ChannelEmail, id
		}
		if id := first(RouteForm, false); id != nil {
			return leadconfig.ChannelForm, id
		}
	case leadconfig.ChannelForm:
		if id := first(RouteForm, false); id != nil {
			return leadconfig.ChannelForm, id
		}
	}
	return leadconfig.ChannelNone, nil
}

// PublicBody reports whether domain is one of suffixes (public_body_suffixes) or lies
// under one, ignoring case, white space and a trailing dot.
func PublicBody(domain string, suffixes []string) bool {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	for _, s := range suffixes {
		s = strings.ToLower(strings.Trim(s, ". "))
		if s != "" && (d == s || strings.HasSuffix(d, "."+s)) {
			return true
		}
	}
	return false
}

// leadReservedTLDs are the RFC 2606 TLDs fixtures use (RA25).
var leadReservedTLDs = []string{"example", "invalid", "test", "localhost"}

// ReservedDomain reports whether domain is on a reserved TLD, so it is fixture data that
// never reaches a real batch, the brief or a send (RA25).
func ReservedDomain(domain string) bool {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if d == "" {
		return false
	}
	return slices.Contains(leadReservedTLDs, d[strings.LastIndexByte(d, '.')+1:])
}

// CanBatch reports whether a new live batch may draft a lead in state: only a new lead.
// A batched or contacted lead is not re-drafted, and replied, bounced, opted_out,
// suppressed, skipped and closed leads never are.
func CanBatch(state string) bool { return state == LeadNew }

// CanApprove reports whether approving a batch binds a lead's message: only while the lead
// is batched. Approving an old batch skips a lead that has since replied, bounced, opted
// out or been suppressed.
func CanApprove(state string) bool { return state == LeadBatched }

// CanSend reports whether send may send a lead's due message: a batched lead's first
// message or a contacted lead's follow-ups. Every other state is refused.
func CanSend(state string) bool { return state == LeadBatched || state == LeadContacted }
