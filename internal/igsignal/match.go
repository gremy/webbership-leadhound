package igsignal

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/places"
)

// LeadKey is a café lead as the matcher reads it (leadstore.IGLeadKeys).
type LeadKey = leadstore.IGLeadKey

// Result is what a profile matches. Tier is leadstore.IGTierHandle, IGTierDomain, IGTierName or
// IGTierNone. LeadIDs is the one lead matched, or two or more tied at Tier (the candidates;
// stored with no lead, for review), or none for IGTierNone.
type Result struct {
	Tier    string
	LeadIDs []int64
	Detail  string // plain words on what matched: "handle", "website cafe.example", "name and town"
	// OwnCustomer: the profile shows the operator's own loyalty product (the own vendor
	// cfg.WithOwnVendor added): a customer, never queued for review.
	OwnCustomer bool
	// VendorAccount is the vendor id when the handle is a vendor's own Instagram account
	// (signals.yaml): it matches no lead and is never queued for review.
	VendorAccount string
}

// Strong reports whether r attaches a profile's signals: a handle or domain match naming one
// lead. A name match, a tie or no match waits for the operator.
func (r Result) Strong() bool {
	return len(r.LeadIDs) == 1 && (r.Tier == leadstore.IGTierHandle || r.Tier == leadstore.IGTierDomain)
}

// Store is r as the ig_match row leadstore.SetIGMatch takes (actor system).
func (r Result) Store(handle string) leadstore.IGMatch {
	m := leadstore.IGMatch{Handle: handle, Tier: r.Tier, Detail: r.Detail, Actor: leadstore.ActorSystem}
	switch len(r.LeadIDs) {
	case 0:
		m.Tier = leadstore.IGTierNone
	case 1:
		m.LeadID = r.LeadIDs[0]
	default:
		m.Candidates = slices.Clone(r.LeadIDs)
	}
	return m
}

// linkPages are the link-in-bio pages places' shared list does not already hold (it holds
// linktr.ee, linkin.bio, Instagram, Facebook, WhatsApp and Google's maps): a profile linking
// one names no website of its own.
var linkPages = []string{"beacons.ai", "bio.link", "taplink.cc", "lnk.bio", "threads.net", "threads.com"}

// Matcher matches profiles to one set of café leads (Decision 3 of the Instagram plan):
//
//   - handle (strong): the profile's handle is one the lead already holds (LeadKey.Handles).
//   - domain (strong): the registrable domain of the profile's external link, as
//     places.DomainKey reads it (the helper the org's own domain was made with), is the lead's
//     domain. A shared platform, a link-in-bio page, social and messaging hosts and every
//     vendor host in the signal list (ordering, loyalty, the own vendor) is no website.
//   - name (weak): the normalised name (NameKey) of the profile is the lead's org, site or
//     place name, and either the lead's town is a whole word of the profile's name, bio or
//     category, or no other lead carries that name.
//
// The first tier with any lead decides; two or more leads there are candidates. A following
// row (MatchFollow) has only a handle and a name, so only those tiers. A capture's
// source.lead_id is never read: lead ids differ between databases.
type Matcher struct {
	cfg      *cafe.Config
	byHandle map[string][]int64
	byDomain map[string][]int64
	byName   map[string][]named
	vendorIG map[string]string // a vendor's own Instagram handle → vendor id
	vendorHs []string          // every vendor host in cfg
}

type named struct {
	leadID int64
	town   string // NameKey of the lead's town; empty without one
}

// NewMatcher indexes leads for matching, with the signal list cfg (nil: cafe.DefaultConfig;
// pass the copy WithOwnVendor made so the own product's accounts are seen as customers).
func NewMatcher(cfg *cafe.Config, leads []LeadKey) *Matcher {
	if cfg == nil {
		cfg = cafe.DefaultConfig()
	}
	m := &Matcher{cfg: cfg, byHandle: map[string][]int64{}, byDomain: map[string][]int64{}, byName: map[string][]named{},
		vendorIG: map[string]string{}}
	for _, v := range cfg.Vendors {
		for _, e := range v.Match {
			host, _, hasPath := strings.Cut(strings.ToLower(strings.TrimSpace(e)), "/")
			if v.Kind != cafe.SigInstagram {
				m.vendorHs = append(m.vendorHs, host)
				if h, ok := enrich.InstagramHandle(e); ok && hasPath {
					m.vendorIG[h] = v.ID
				}
			}
		}
	}
	for _, l := range leads {
		for _, h := range l.Handles {
			if h, ok := enrich.InstagramHandle(h); ok {
				m.byHandle[h] = appendOnce(m.byHandle[h], l.LeadID)
			}
		}
		if d := strings.ToLower(strings.TrimSpace(l.Domain)); d != "" {
			m.byDomain[d] = appendOnce(m.byDomain[d], l.LeadID)
		}
		town := NameKey(l.Town)
		for _, n := range []string{l.OrgName, l.SiteName, l.PlaceName} {
			k := NameKey(n)
			if k == "" || slices.ContainsFunc(m.byName[k], func(x named) bool { return x.leadID == l.LeadID }) {
				continue
			}
			m.byName[k] = append(m.byName[k], named{leadID: l.LeadID, town: town})
		}
	}
	return m
}

func appendOnce(ids []int64, id int64) []int64 {
	if slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}

// NameKey is a name as the matcher compares it: cafe.NormalizeName, the same normalisation
// leadstore's duplicate check keys a place's name on (a contract test holds the two together).
func NameKey(s string) string { return cafe.NormalizeName(s) }

// Match matches one profile capture.
func (m *Matcher) Match(p Profile) Result {
	var r Result
	if id, _, ok := Vendor(p, m.cfg); ok && id == cafe.OwnVendorID {
		r.OwnCustomer = true
	}
	handle, ok := enrich.InstagramHandle(p.Handle)
	if !ok {
		r.Tier = leadstore.IGTierNone
		return r
	}
	if v, ok := m.vendorIG[handle]; ok {
		r.Tier, r.VendorAccount, r.Detail = leadstore.IGTierNone, v, "a vendor's own account"
		return r
	}
	if ids := m.byHandle[handle]; len(ids) > 0 {
		return m.decided(r, leadstore.IGTierHandle, ids, "handle")
	}
	if d := m.Domain(p.ExternalURL); d != "" {
		if ids := m.byDomain[d]; len(ids) > 0 {
			return m.decided(r, leadstore.IGTierDomain, ids, "website "+d)
		}
	}
	if ids, detail := m.byNameAndTown(p); len(ids) > 0 {
		return m.decided(r, leadstore.IGTierName, ids, detail)
	}
	r.Tier = leadstore.IGTierNone
	return r
}

// MatchFollow matches one account kept from a following list: by its handle and its name only.
func (m *Matcher) MatchFollow(f Follow) Result {
	return m.Match(Profile{Handle: f.Handle, FullName: f.FullName})
}

func (m *Matcher) decided(r Result, tier string, ids []int64, detail string) Result {
	r.Tier, r.LeadIDs, r.Detail = tier, slices.Sorted(slices.Values(ids)), detail
	if len(ids) > 1 {
		r.Detail = fmt.Sprintf("%s, %d leads tie", detail, len(ids))
	}
	return r
}

// Domain is the registrable domain of a profile's external link as the domain tier reads it;
// empty for no link, a link that does not parse, or one that is no website of the café's own.
func (m *Matcher) Domain(link string) string {
	h := places.Host(link)
	if h == "" || places.IsShared(link) {
		return ""
	}
	for _, d := range linkPages {
		if places.HostIs(h, d) {
			return ""
		}
	}
	for _, d := range m.vendorHs {
		if places.HostIs(h, d) {
			return ""
		}
	}
	return places.DomainKey(link)
}

// byNameAndTown is the name tier: the leads whose name is the profile's, kept when the
// lead's town is a whole word of the profile's name, bio or category, or when the name is
// the lead's alone.
func (m *Matcher) byNameAndTown(p Profile) ([]int64, string) {
	leads := m.byName[NameKey(p.FullName)]
	if len(leads) == 0 {
		return nil, ""
	}
	text := " " + NameKey(p.FullName+" "+p.Bio+" "+p.Category) + " "
	var ids []int64
	for _, l := range leads {
		if l.town != "" && strings.Contains(text, " "+l.town+" ") {
			ids = append(ids, l.leadID)
		}
	}
	if len(ids) > 0 {
		return ids, "name and town"
	}
	if len(leads) == 1 {
		return []int64{leads[0].leadID}, "name, the only lead by it"
	}
	return nil, ""
}
