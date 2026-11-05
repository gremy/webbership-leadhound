package console

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The remembered campaign's and the flash's cookie names off dev mode.
const (
	campaignCookie = "__Host-lh_leads_campaign"
	flashCookie    = "__Host-lh_flash"
)

// remembered is the remembered-campaign cookie as a browser sends it back.
func remembered(slug string) *http.Cookie {
	return &http.Cookie{Name: campaignCookie, Value: slug, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
}

// TestLeadListCampaignCookie: the cold-start twins of the remembered campaign. A first visit
// with no cookie lists every campaign and sets none; naming a campaign the list offers
// remembers it (HttpOnly, Secure, Lax, 90 days); a visit with the cookie and no campaign
// applies it, flagged as from the cookie so a retired slug is dropped silently by the
// backend; and a campaign the list does not offer is not remembered.
func TestLeadListCampaignCookie(t *testing.T) {
	b := richBackend("Northwind Trading")
	h := newHarness(t, b, false)
	c := h.session(t)

	w := h.do(get("/leads", c))
	if w.Code != http.StatusOK || b.listQ.Campaign != "" || b.listQ.CampaignFromCookie {
		t.Errorf("no cookie: %d, campaign %q from cookie %v", w.Code, b.listQ.Campaign, b.listQ.CampaignFromCookie)
	}
	if ck := cookieNamed(w, campaignCookie); ck != nil {
		t.Errorf("a visit naming no campaign set %v", ck)
	}

	w = h.do(get("/leads?campaign=wp-eol", c))
	ck := cookieNamed(w, campaignCookie)
	if ck == nil || ck.Value != "wp-eol" || !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteLaxMode ||
		ck.Path != "/" || ck.MaxAge != 90*24*60*60 {
		t.Fatalf("naming a campaign set %+v", ck)
	}
	if b.listQ.Campaign != "wp-eol" || b.listQ.CampaignFromCookie {
		t.Errorf("a named campaign reached the backend as %q, from cookie %v", b.listQ.Campaign, b.listQ.CampaignFromCookie)
	}

	w = h.do(get("/leads?status=all", c, remembered("wp-eol")))
	if b.listQ.Campaign != "wp-eol" || !b.listQ.CampaignFromCookie || b.listQ.Status != "all" {
		t.Errorf("the cookie was not applied: %+v", b.listQ)
	}
	if ck := cookieNamed(w, campaignCookie); ck != nil {
		t.Errorf("applying the cookie set it again: %v", ck)
	}
	if body := w.Body.String(); !strings.Contains(body, "campaign=wp-eol") {
		t.Error("the page's links do not carry the remembered campaign")
	}

	// A retired or deleted campaign's slug goes down flagged; the backend drops it silently.
	h.do(get("/leads", c, remembered("closed-campaign")))
	if b.listQ.Campaign != "closed-campaign" || !b.listQ.CampaignFromCookie {
		t.Errorf("a retired slug reached the backend as %+v", b.listQ)
	}

	// A campaign the list does not offer is not remembered; all is.
	if ck := cookieNamed(h.do(get("/leads?campaign=nope", c)), campaignCookie); ck != nil {
		t.Errorf("an unknown campaign was remembered: %v", ck)
	}
	if ck := cookieNamed(h.do(get("/leads?campaign=all", c, remembered("wp-eol"))), campaignCookie); ck == nil || ck.Value != "all" {
		t.Errorf("campaign=all was not remembered: %v", ck)
	}
	if b.listQ.Campaign != "all" || b.listQ.CampaignFromCookie {
		t.Errorf("campaign=all reached the backend as %+v", b.listQ)
	}
}

// TestLeadListCampaignCookieSpans: the leads to label and the outreach order span campaigns,
// so they neither read the remembered campaign nor set it, whatever they name.
func TestLeadListCampaignCookieSpans(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, q := range []string{"to_label=1", "sort=order", "sort=ORDER"} {
		w := h.do(get("/leads?"+q, c, remembered("wp-eol")))
		if w.Code != http.StatusOK || b.listQ.Campaign != "" || b.listQ.CampaignFromCookie {
			t.Errorf("%s: %d, campaign %q from cookie %v", q, w.Code, b.listQ.Campaign, b.listQ.CampaignFromCookie)
		}
		if ck := cookieNamed(h.do(get("/leads?"+q+"&campaign=drupal-eol", c, remembered("wp-eol"))), campaignCookie); ck != nil {
			t.Errorf("%s with a campaign set %v", q, ck)
		}
	}
}

// TestLeadListRefusalIgnoresCookie: a refused action renders the page its form came from (the
// back query), neither reading nor setting the remembered campaign.
func TestLeadListRefusalIgnoresCookie(t *testing.T) {
	b := richBackend("x")
	b.leadErr = fmt.Errorf("%w: It is already skipped.", ErrUserFixable)
	h := newHarness(t, b, false)
	w := h.do(post("/leads/5/skip", url.Values{"back": {"/leads?status=all"}}, h.session(t), remembered("wp-eol")))
	if w.Code != http.StatusUnprocessableEntity || b.listQ.Campaign != "" || b.listQ.CampaignFromCookie {
		t.Errorf("refusal: %d, campaign %q from cookie %v", w.Code, b.listQ.Campaign, b.listQ.CampaignFromCookie)
	}
	if ck := cookieNamed(w, campaignCookie); ck != nil {
		t.Errorf("a refusal set %v", ck)
	}
}

// TestLeadListEveryCampaignOption: the form's "Every campaign" posts campaign=all, so a
// remembered campaign never makes every campaign unreachable.
func TestLeadListEveryCampaignOption(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	if body := h.do(get("/leads", h.session(t))).Body.String(); !strings.Contains(body, `<option value="all">Every campaign</option>`) {
		t.Error(`"Every campaign" does not post campaign=all`)
	}
}

// TestLeadActTriageFetch: a lead action posted by the list's script (X-Triage: 1) gets 204
// with no body, no flash and no redirect, for every triage verb; a refusal gets 422 with the
// refusal's words as plain text, and no flash either. Without the header the form answer
// stands: a redirect with the flash.
func TestLeadActTriageFetch(t *testing.T) {
	b := richBackend("x")
	b.leadOut = LeadOutcome{Message: "Skipped Northwind Trading; no call was saved."}
	h := newHarness(t, b, false)
	c := h.session(t)
	fetch := func(do string) *http.Request {
		r := post("/leads/5/"+do, url.Values{"back": {"/leads?campaign=wp-eol#lead-w-5"}}, c)
		r.Header.Set("X-Triage", "1")
		return r
	}
	for _, do := range []string{LeadSkip, LeadUnskip, LeadGood, LeadRoastery, LeadCafe} {
		w := h.do(fetch(do))
		if w.Code != http.StatusNoContent || w.Body.Len() != 0 || w.Header().Get("Location") != "" {
			t.Errorf("%s: %d %q, location %q", do, w.Code, w.Body.String(), w.Header().Get("Location"))
		}
		if ck := cookieNamed(w, flashCookie); ck != nil {
			t.Errorf("%s set the flash %v", do, ck)
		}
		if b.lastLead.Do != do || b.lastLead.ID != 5 {
			t.Errorf("%s reached the backend as %+v", do, b.lastLead)
		}
	}

	const words = "Lead 5 is already skipped; nothing changed."
	b.leadErr = fmt.Errorf("%w: %s", ErrUserFixable, words)
	w := h.do(fetch(LeadSkip))
	if w.Code != http.StatusUnprocessableEntity || w.Body.String() != words ||
		!strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("refusal: %d %q (%s)", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
	}
	if ck := cookieNamed(w, flashCookie); ck != nil {
		t.Errorf("a fetched refusal set the flash %v", ck)
	}

	b.leadErr = ErrLeadsNotSetUp
	if w := h.do(fetch(LeadGood)); w.Code != http.StatusUnprocessableEntity || strings.Contains(w.Body.String(), "<html") {
		t.Errorf("not set up: %d %q", w.Code, w.Body.String())
	}

	b.leadErr = nil
	w = h.do(post("/leads/5/"+LeadSkip, url.Values{"back": {"/leads"}}, c))
	if w.Code != http.StatusSeeOther || cookieNamed(w, flashCookie) == nil {
		t.Errorf("a form post: %d, flash %v", w.Code, cookieNamed(w, flashCookie))
	}
}

// TestLeadListTabsAndChips: the campaign tabs keep the rest of the filter but drop the town
// and place type, end with All campaigns (current on all or blank) summing their counts, and
// a café campaign's chips link each place type; town and place type round-trip through the
// list's URL and the folded form counts the town but not campaign=all.
func TestLeadListTabsAndChips(t *testing.T) {
	f := LeadListQuery{Campaign: "cafes", Town: "Fictive Town", PlaceType: "cafe", Status: "all"}
	tabs := campaignTabs(f, []CampaignTab{{Slug: "cafes", Name: "Cafes", Open: 7, Cafe: true}, {Slug: "drupal-eol", Name: "Drupal", Open: 3}})
	if len(tabs) != 3 || !tabs[0].Current || tabs[1].Current || tabs[2].Current || tabs[2].Count != 10 || tabs[2].Label != "All campaigns" {
		t.Fatalf("tabs %+v", tabs)
	}
	if tabs[1].URL != "/leads?campaign=drupal-eol&status=all" || tabs[2].URL != "/leads?campaign=all&status=all" {
		t.Errorf("tab URLs %q, %q", tabs[1].URL, tabs[2].URL)
	}
	if all := campaignTabs(LeadListQuery{Campaign: "all"}, []CampaignTab{{Slug: "cafes", Name: "Cafes"}}); !all[1].Current {
		t.Error("All campaigns is not current on campaign=all")
	}
	if campaignTabs(f, nil) != nil || placeTypeChips(f, nil) != nil {
		t.Error("tabs or chips with nothing to show")
	}

	chips := placeTypeChips(f, &PlaceTypeCounts{Cafe: 5, Roastery: 2})
	if len(chips) != 3 || !chips[0].Current || chips[1].Count != 2 || chips[2].Count != 7 ||
		chips[1].URL != "/leads?campaign=cafes&place_type=roastery&status=all&town=Fictive+Town" || chips[2].URL != "/leads?campaign=cafes&place_type=all&status=all&town=Fictive+Town" {
		t.Errorf("chips %+v", chips)
	}

	q := leadListQuery(url.Values{"town": {" Fictive Town "}, "place_type": {"roastery"}})
	if q.Town != "Fictive Town" || q.PlaceType != "roastery" {
		t.Errorf("query %+v", q)
	}
	v := newLeadListView(LeadListPage{Filter: LeadListQuery{Campaign: "all", Town: "Fictive Town"}})
	if v.Filters != 1 {
		t.Errorf("%d filters on, want the town alone", v.Filters)
	}
}
