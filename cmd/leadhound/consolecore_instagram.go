package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/igsignal"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// The console's Instagram: the card on a café lead's page and the review list. Both read what
// `leads ig import` stored; the review list's calls run the same igOperator* functions as
// `leads ig match`, `dismiss` and `want`, so the console and the CLI cannot disagree. Nothing
// here changes a lead or its state. The place type moves only through the match: like `leads
// ig match`, it ends with SyncIGPlaceTypes, so the lead takes Instagram's read of the handle
// (the operator's own place call still outranks it), and the message says which lead moved.

// igPlaceWords is Instagram's place call in words; "" for no call.
func igPlaceWords(choice string) string {
	switch choice {
	case leadrate.IGCafeSeating:
		return "a café with seating"
	case leadrate.IGRoasteryOrOnline:
		return "a roastery or online shop"
	case leadrate.IGNotCoffee:
		return "not a café"
	}
	return choice // unclear, or ""
}

// igLoyaltyWords is Instagram's loyalty call in words.
func igLoyaltyWords(choice string) string {
	switch choice {
	case leadrate.IGRunsScheme:
		return "runs a loyalty or stamp scheme"
	case leadrate.IGNoneSeen:
		return "none seen"
	}
	return choice
}

// igVendorWords is the vendor answer in words: a vendor by the name signals.yaml gives it (the
// own vendor by its name in cfg), or what Jev saw without a vendor named.
func igVendorWords(cfg *cafe.Config, choice string) string {
	switch choice {
	case leadrate.IGPaperCard:
		return "a paper card"
	case leadrate.IGUnnamedAppOrQR:
		return "an app or QR code, not named"
	case leadrate.IGNoVendor:
		return "none"
	case leadrate.IGUnclear, "":
		return choice
	}
	if v := cfg.Vendor(choice); v != nil && v.Name != "" {
		return v.Name
	}
	return choice
}

// igVendorVia is how a rule saw its vendor, in words: a link (the profile's link or one in its
// bio) or an @-tag; "" for Jev's answer or a rule that saw no text.
func igVendorVia(j leadstore.IGJudgment) string {
	if j.Source != leadstore.IGFromRule {
		return ""
	}
	switch j.Detail {
	case "from a " + igsignal.VendorViaLink, "from a " + igsignal.VendorViaBio:
		return "from a link"
	case "from a " + igsignal.VendorViaTag:
		return "from a tag"
	}
	return ""
}

// igMatchedWords is how a strong match was made, in words.
func igMatchedWords(tier string) string {
	switch tier {
	case leadstore.IGTierHandle:
		return "its handle"
	case leadstore.IGTierDomain:
		return "its website"
	case leadstore.IGTierOperator:
		return "your call"
	}
	return ""
}

// leadInstagram is lead id's Instagram card, nil when no strong or operator match names it.
// placeType is the lead's effective place type, which the card's disagreement line reads.
func (b *backend) leadInstagram(ctx context.Context, cfg leadconfig.Config, id int64, placeType string) (*console.LeadInstagramView, error) {
	ig, err := b.lq().IGForLead(ctx, id)
	if err != nil || ig == nil {
		return nil, err
	}
	ccfg, err := cafeConfig(cfg)
	if err != nil {
		return nil, err
	}
	v := &console.LeadInstagramView{Handle: ig.Match.Handle, URL: enrich.InstagramURL(ig.Match.Handle), Matched: igMatchedWords(ig.Match.Tier)}
	if p := ig.Profile; p != nil {
		at := p.CapturedAt
		v.CapturedAt = &at
		v.Missing = p.Outcome == leadstore.IGOutcomeMissing
		v.Private = p.IsPrivate != nil && *p.IsPrivate
		if p.Followers != nil {
			v.Followers, v.Counted = *p.Followers, true
		}
	}
	place := ig.Judgments[leadstore.IGPlace].Choice
	vendor := ig.Judgments[leadstore.IGVendor]
	v.Place, v.Loyalty = igPlaceWords(place), igLoyaltyWords(ig.Judgments[leadstore.IGLoyalty].Choice)
	v.Vendor, v.VendorVia = igVendorWords(ccfg, vendor.Choice), igVendorVia(vendor)
	for _, h := range ig.Hints {
		if h.Kind != leadstore.IGHintFollowedByVendor {
			continue
		}
		if w := "followed by " + igVendorWords(ccfg, h.Value); !slices.Contains(v.Hints, w) {
			v.Hints = append(v.Hints, w)
		}
	}
	v.Disagree = igPlaceDisagrees(place, placeType)
	return v, nil
}

// InstagramReview is the review list at status, by handle, with the count at each status and
// each row's candidate leads named. A candidate that is no longer a café lead Instagram can be
// matched to is shown without the button.
func (b *backend) InstagramReview(ctx context.Context, status string) (console.IGReviewPage, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return console.IGReviewPage{}, err
	}
	ccfg, err := cafeConfig(setup.leads)
	if err != nil {
		return console.IGReviewPage{}, err
	}
	if !slices.Contains(console.IGReviewStatuses, status) {
		return console.IGReviewPage{}, fmt.Errorf("console instagram: unknown status %q", status)
	}
	lq := b.lq()
	items, err := lq.IGReview(ctx, "")
	if err != nil {
		return console.IGReviewPage{}, err
	}
	keys, err := lq.IGLeadKeys(ctx, 0)
	if err != nil {
		return console.IGReviewPage{}, err
	}
	byLead := make(map[int64]leadstore.IGLeadKey, len(keys))
	for _, k := range keys {
		byLead[k.LeadID] = k
	}
	p := console.IGReviewPage{Status: status, Counts: map[string]int{}}
	for _, it := range items {
		p.Counts[it.Status]++
		if it.Status != status {
			continue
		}
		row := console.IGReviewRow{Handle: it.Handle, URL: enrich.InstagramURL(it.Handle), Name: it.FollowName, Place: igPlaceWords(it.Place),
			Loyalty: igReviewWords(it.Loyalty, igLoyaltyWords), Vendor: igReviewWords(it.Vendor, func(c string) string { return igVendorWords(ccfg, c) })}
		if it.Profile != nil {
			row.Name, row.Category = cmp.Or(it.Profile.FullName, row.Name), it.Profile.Category
		}
		for _, id := range it.Candidates {
			c := console.IGCandidate{LeadID: id, Name: "Lead " + strconv.FormatInt(id, 10)}
			if k, ok := byLead[id]; ok {
				c.Name, c.Town, c.Matchable = cmp.Or(k.OrgName, k.PlaceName, k.SiteName, c.Name), k.Town, true
			}
			row.Candidates = append(row.Candidates, c)
		}
		p.Rows = append(p.Rows, row)
	}
	return p, nil
}

// igReviewWords is a review row's loyalty or vendor answer choice in words (words), or "" when it
// says nothing worth reading before "Want as lead": never judged, unclear, none seen or none.
func igReviewWords(choice string, words func(string) string) string {
	switch choice {
	case "", leadrate.IGUnclear, leadrate.IGNoneSeen, leadrate.IGNoVendor:
		return ""
	}
	return words(choice)
}

// InstagramAct runs one call on the review list through the CLI's own functions: match is
// `leads ig match`, dismiss and want are `leads ig dismiss|want`. Their refusals (a handle in no
// capture, a lead that is not a café lead) come back as the console's refusals in the same words.
func (b *backend) InstagramAct(ctx context.Context, a console.IGAction) (string, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return "", err
	}
	lq, db := b.lq(), b.s.Pool()
	msg, err := func() (string, error) {
		switch a.Do {
		case console.IGMatch:
			handle, added, sync, err := igOperatorMatch(ctx, lq, db, setup.leads, a.Handle, a.LeadID)
			if err != nil {
				return "", err
			}
			route := "its Instagram route was already on file"
			if added {
				route = "its Instagram route is added"
			}
			msg := fmt.Sprintf("@%s is lead %d now; %s.", handle, a.LeadID, route)
			for _, c := range igPlaceChanges(sync) {
				w := igPlaceChangeWords(c)
				msg += " " + strings.ToUpper(w[:1]) + w[1:] + "."
			}
			return msg, nil
		case console.IGDismiss, console.IGWant:
			status, words := leadstore.IGReviewDismissed, "not a lead"
			if a.Do == console.IGWant {
				status, words = leadstore.IGReviewWanted, "wanted as a lead"
			}
			handle, changed, err := igSetStatus(ctx, lq, db, a.Handle, status)
			if err != nil {
				return "", err
			}
			if !changed {
				return fmt.Sprintf("@%s was already %s; nothing changed.", handle, words), nil
			}
			return fmt.Sprintf("@%s is %s.", handle, words), nil
		}
		return "", fmt.Errorf("console instagram: unknown call %q", a.Do)
	}()
	var r igRefusal
	if errors.As(err, &r) {
		return "", refused("%s; nothing was changed.", r)
	}
	return msg, err
}
