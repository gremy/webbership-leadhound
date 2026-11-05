package console

import (
	"cmp"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxFilterValues is the most values one repeatable filter (kind, country, channel) takes
// from a request; the rest are dropped before the backend sees them.
const maxFilterValues = 50

// maxBack is the longest back value a lead action follows; a longer one goes to the list.
const maxBack = 2048

// leadActions are the writes POST /leads/{id}/{action} takes.
var leadActions = []string{LeadStar, LeadUnstar, LeadUp, LeadDown, LeadTop, LeadDefer, LeadSkip, LeadClear, LeadLabel, LeadLookAgain,
	LeadUnskip, LeadGood, LeadRoastery, LeadCafe}

// triageHeader is the request header the list's script sends with a lead action it posts by
// fetch: the answer is then a bare status, 204 on success and 422 with the refusal's words, so
// the page keeps its place and no flash is left for the next page.
const triageHeader = "X-Triage"

// campaignAll is the campaign value that says every campaign outright, so that a remembered
// campaign does not stand in for it.
const campaignAll = "all"

// leadsCampaignTTL is how long the list remembers the campaign it last named.
const leadsCampaignTTL = 90 * 24 * time.Hour

// badBand is the refusal of a call whose band the form did not offer.
const badBand = "Choose strong, likely, maybe, unlikely or no, or Clear; your call was not changed."

// leadListView is what the Leads list renders: the backend's page, the line from the last
// action, and the pager's links, which keep the filter.
type leadListView struct {
	P          LeadListPage
	Message    string
	Error      bool
	Pages      int
	Prev, Next string // "" when there is no such page
	NoLeads    bool   // there are no leads at all
	NoneMatch  bool   // there are leads, but none matches the filter
	Back       string // this page, filter and page kept: where a row's action comes back to
	Order      bool   // sorted by the outreach order: rows carry Up, Down, Pin to top, Push back, Skip
	Tabs       []reachTab
	// Campaigns are the campaign tabs, All campaigns last, and PlaceTypes the chips of a café
	// campaign (coffee shops, roasteries, both); each is nil when the page has none.
	Campaigns, PlaceTypes []reachTab
	// The table's optional columns: State only when the filter is not the default (open
	// leads), Relevance and Order only when a row has one, Your call when a row has one or
	// the list is the leads to label.
	ShowState, ShowRelevance, ShowOrder, ShowCall bool
	ToLabel                                       bool // the list is the leads to label
	Filters                                       int  // how many filters are on, for the folded form's summary
}

// reachTab is one tab of the List: a next action with its number, or All (Counted false). The
// campaign tabs and the place type chips are tabs of the same shape.
type reachTab struct {
	Label, URL       string
	Count            int
	Counted, Current bool
}

// CanLook reports whether a dead-end row offers Look for a contact: not while a search runs,
// when one has already found a contact, or in a country that is off, which would only refuse.
func (leadListView) CanLook(reason string) bool {
	return reason != ReachLooking && reason != ReachFound && reason != ReachCountry
}

// reachTabs are the List's tabs under f, each keeping the rest of the filter and going back
// to the first page; All is f with no next action, so it behaves as the plain list does.
func reachTabs(f LeadListQuery, c ReachCounts) []reachTab {
	tab := func(bucket string) string {
		f := f
		f.Reach = bucket
		return leadListURL(f, 1)
	}
	var tabs []reachTab
	for _, bucket := range ReachTabs {
		tabs = append(tabs, reachTab{Label: ReachWords(bucket), URL: tab(bucket), Count: c.Of(bucket), Counted: true, Current: f.Reach == bucket})
	}
	return append(tabs, reachTab{Label: "All", URL: tab(""), Current: f.Reach == ""})
}

// campaignTabs are the list's campaign tabs under f: each campaign with its open leads, then
// All campaigns with their sum. A tab keeps the rest of the filter but not the town or place
// type, which belong to one campaign, and goes back to the first page. nil with no campaign.
func campaignTabs(f LeadListQuery, tabs []CampaignTab) []reachTab {
	if len(tabs) == 0 {
		return nil
	}
	tab := func(slug string) string {
		f := f
		f.Campaign, f.Town, f.PlaceType = slug, "", ""
		return leadListURL(f, 1)
	}
	out := make([]reachTab, 0, len(tabs)+1)
	all := 0
	for _, c := range tabs {
		out = append(out, reachTab{Label: c.Name, URL: tab(c.Slug), Count: c.Open, Counted: true, Current: f.Campaign == c.Slug})
		all += c.Open
	}
	return append(out, reachTab{Label: "All campaigns", URL: tab(campaignAll), Count: all, Counted: true,
		Current: f.Campaign == "" || f.Campaign == campaignAll})
}

// placeTypeChips are a café campaign's chips under f: coffee shops, roasteries, and both, each
// with its open leads, keeping the rest of the filter and going back to the first page. nil
// when c is nil (no café campaign chosen).
func placeTypeChips(f LeadListQuery, c *PlaceTypeCounts) []reachTab {
	if c == nil {
		return nil
	}
	chip := func(label, placeType string, n int) reachTab {
		g := f
		g.PlaceType = placeType
		return reachTab{Label: label, URL: leadListURL(g, 1), Count: n, Counted: true, Current: f.PlaceType == placeType}
	}
	return []reachTab{chip("Coffee shops", "cafe", c.Cafe), chip("Roasteries", "roastery", c.Roastery),
		chip("Both", campaignAll, c.Cafe+c.Roastery)}
}

// leadDetailView is what a lead's page renders: the lead, its signals grouped by the fetch
// they came from, and the line from the last action.
type leadDetailView struct {
	Data    LeadDetailPage
	Fetches []signalFetch
	Back    string
	Message string
	Error   bool
	AtCall  bool      // Message is about the call: it shows inside the "Your call" block, not at the top
	Seq     []seqRow  // the lead's sequence, worded
	CRM     *crmPanel // the stage, note, deal and timeline panels; nil when the backend has no CRM
}

// callAnchor is the "Your call" block's id: a call goes back to it.
const callAnchor = "your-call"

// signalFetch is one fetch and the signals read from it, in the order the lead's page
// lists them. A signal with no fetch on file sits in a group of its own with no fetch.
type signalFetch struct {
	FetchURL, FinalURL, Status string
	FetchedAt                  *time.Time
	Signals                    []LeadSignal
}

// leadActView is one lead action's form: the lead, the action, the button's words, where it
// comes back to, and the button's words for a screen reader when they differ.
type leadActView struct {
	ID                  int64
	Do, Label, Back, SR string
}

// leadList is GET /leads: the leads under the query's filter, one page of them. A query that
// names no campaign takes the remembered one from the cookie, and one that names a campaign the
// list offers (or all) is remembered, except the leads to label and the outreach order, which
// span campaigns and neither read nor set it.
func (s *server) leadList(w http.ResponseWriter, r *http.Request) {
	q := leadListQuery(r.URL.Query())
	spans := q.ToLabel == "1" || strings.EqualFold(q.Sort, "order")
	named := q.Campaign != ""
	if !named && !spans {
		if c, err := r.Cookie(s.leadsCampaign); err == nil && strings.TrimSpace(c.Value) != "" {
			q.Campaign, q.CampaignFromCookie = strings.TrimSpace(c.Value), true
		}
	}
	msg := s.takeFlash(w, r)
	p, err := s.b.LeadList(r.Context(), q)
	if s.leadsRead(w, r, http.StatusOK, err) {
		return
	}
	if named && !spans && (q.Campaign == campaignAll ||
		slices.ContainsFunc(p.Options.Campaigns, func(o FilterOption) bool { return o.Value == q.Campaign })) {
		s.rememberCampaign(w, q.Campaign)
	}
	s.renderListPage(w, r, http.StatusOK, p, msg, false)
}

// rememberCampaign sets the remembered-campaign cookie to slug, as the session cookie is set
// but SameSite Lax, so a link from elsewhere still opens the list on it.
func (s *server) rememberCampaign(w http.ResponseWriter, slug string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is off only in dev mode, over plain http on loopback
		Name:     s.leadsCampaign,
		Value:    slug,
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.opt.Dev,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(leadsCampaignTTL / time.Second),
	})
}

// renderList shows the list under query q with a line; a refusal comes back through it. It
// neither reads nor sets the remembered campaign: q is the page the form came from.
func (s *server) renderList(w http.ResponseWriter, r *http.Request, code int, q url.Values, msg string, isErr bool) {
	p, err := s.b.LeadList(r.Context(), leadListQuery(q))
	if s.leadsRead(w, r, code, err) {
		return
	}
	s.renderListPage(w, r, code, p, msg, isErr)
}

func (s *server) renderListPage(w http.ResponseWriter, r *http.Request, code int, p LeadListPage, msg string, isErr bool) {
	v := newLeadListView(p)
	v.Message, v.Error = msg, isErr
	s.render(w, r, code, "leads_list", page{Title: "Leads", Active: leadsPath, SignedIn: true, Data: v})
}

// leadDetail is GET /leads/{id}: everything stored about one lead.
func (s *server) leadDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	msg, atCall := s.takeFlashAt(w, r)
	s.renderDetail(w, r, http.StatusOK, id, msg, false, atCall)
}

func (s *server) renderDetail(w http.ResponseWriter, r *http.Request, code int, id int64, msg string, isErr, atCall bool) {
	s.renderDetailCRM(w, r, code, id, msg, isErr, atCall, nil)
}

// renderDetailCRM is renderDetail with the CRM section posted (and refused) kept in its form;
// nil shows the stored values.
func (s *server) renderDetailCRM(w http.ResponseWriter, r *http.Request, code int, id int64, msg string, isErr, atCall bool, posted *crmForm) {
	d, err := s.b.LeadDetail(r.Context(), id)
	if s.leadsRead(w, r, code, err) {
		return
	}
	seq, err := s.b.LeadSequence(r.Context(), id)
	if s.leadsRead(w, r, code, err) {
		return
	}
	crm, err := s.crmPanelFor(r, id, posted)
	if s.leadsRead(w, r, code, err) {
		return
	}
	title := cmp.Or(d.Name, siteDomain(d.Domain), "Lead "+strconv.FormatInt(id, 10))
	s.render(w, r, code, "lead_detail", page{Title: title, Active: leadsPath, SignedIn: true,
		Data: leadDetailView{Data: d, Fetches: signalFetches(d.Signals), Back: leadsPath + "/" + strconv.FormatInt(id, 10),
			Message: msg, Error: isErr, AtCall: atCall, Seq: s.sequenceRows(seq), CRM: crm}})
}

// leadAct is POST /leads/{id}/{action}: star, unstar, up, down, top, defer, skip or clear
// one lead, then back to the page the form names in back (the list when it names none, or
// one that is not a Leads page). A refusal shows that page again with its words.
func (s *server) leadAct(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	do := r.PathValue("action")
	// The CRM writes share this pattern: POST /leads/{id}/crm would conflict with
	// POST /leads/settings/{section} in the mux, so they are dispatched from here.
	switch {
	case ok && do == "crm":
		s.leadCRM(w, r, id)
		return
	case ok && do == "snooze":
		s.leadSnooze(w, r, id)
		return
	case ok && do == "contacts":
		s.leadContactAdd(w, r, id)
		return
	case ok && do == "draft-it":
		s.leadDraftIt(w, r, id)
		return
	case ok && do == "draft-discard":
		s.leadDraftDiscard(w, r, id)
		return
	case ok && do == "sector":
		s.leadSector(w, r, id)
		return
	case ok && do == "proposals":
		s.leadProposalAdd(w, r, id)
		return
	}
	if !ok || !slices.Contains(leadActions, do) {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	back := leadsBack(r.PostForm.Get("back"))
	again := func(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
		if triageFetch(r) {
			triageRefused(w, code, msg)
			return
		}
		if u, err := url.Parse(back); err == nil && u.Path == leadsPath {
			s.renderList(w, r, code, u.Query(), msg, isErr)
			return
		}
		s.renderDetail(w, r, code, id, msg, isErr, do == LeadLabel)
	}
	a := LeadAction{Do: do, ID: id}
	switch do {
	case LeadDefer:
		a.Until = strings.TrimSpace(r.PostForm.Get("until"))
	case LeadLabel:
		a.Value = strings.TrimSpace(r.PostForm.Get("band"))
		if a.Value != LabelClear && !slices.Contains(LabelBands, a.Value) {
			s.leadsLog(do, "refused")
			again(w, r, http.StatusUnprocessableEntity, badBand, true)
			return
		}
		back = atCallBlock(back)
	}
	s.leadsAct(w, r, a, back, again)
}

// triageFetch reports whether r is a lead action the list's script posted by fetch.
func triageFetch(r *http.Request) bool { return r.Header.Get(triageHeader) == "1" }

// triageRefused answers a fetched lead action that was refused: code with the refusal's words
// as plain text, which the script shows as they are.
func triageRefused(w http.ResponseWriter, code int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg))
}

// leadReport is GET /leads/reports/{id}: the lead's upgrade-risk report as a file to save,
// for the signed-in operator only. It writes nothing. A lead with no report shows its page
// again with the reason.
func (s *server) leadReport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	rep, err := s.b.LeadReport(r.Context(), id)
	if errors.Is(err, ErrUserFixable) {
		s.leadsLog("report", "refused")
		s.renderDetail(w, r, http.StatusUnprocessableEntity, id, refusalWords(err), true, false)
		return
	}
	if s.leadsRead(w, r, http.StatusOK, err) {
		return
	}
	s.leadsLog("report", "ok")
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": reportFilename(rep.Filename, id)}))
	h.Set("Content-Length", strconv.Itoa(len(rep.HTML)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rep.HTML)
}

// reportFilename is name kept to letters, digits, dots, dashes and underscores, ending in
// .html; an empty or unusable name is upgrade-risk-lead-<id>.html.
func reportFilename(name string, id int64) string {
	var b strings.Builder
	for _, c := range strings.ToLower(name) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(strings.TrimSuffix(b.String(), ".html"), ".-_")
	if out == "" || len(out) > 120 {
		out = "upgrade-risk-lead-" + strconv.FormatInt(id, 10)
	}
	return out + ".html"
}

// atCallBlock is back pointed at the "Your call" block when back is a lead's own page, in
// place of any fragment it had; the list, which has no call block, is back as it is.
func atCallBlock(back string) string {
	page, _, _ := strings.Cut(back, "#")
	if u, err := url.Parse(page); err != nil || u.Path == leadsPath {
		return back
	}
	return page + "#" + callAnchor
}

// leadsBack is where a lead action goes back to: back when it is a relative path on the
// Leads pages ("/leads" followed by the end, "/", "?" or "#"), clean, with no host, scheme
// or backslash; anything else is the list. What it returns is rebuilt from back's parts,
// each escaped onto the fixed leadsPath (the query in its own order), so the target is a
// path on this site by construction, not by the checks alone; a query that does not
// unescape is the list too.
func leadsBack(back string) string {
	if len(back) > maxBack || !strings.HasPrefix(back, leadsPath) || strings.ContainsRune(back, '\\') {
		return leadsPath
	}
	if rest := back[len(leadsPath):]; rest != "" && !strings.ContainsRune("/?#", rune(rest[0])) {
		return leadsPath
	}
	u, err := url.Parse(back)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil || path.Clean(u.Path) != u.Path {
		return leadsPath
	}
	var b strings.Builder
	b.WriteString(leadsPath)
	if sub := strings.TrimPrefix(u.Path, leadsPath); sub != "" {
		for _, seg := range strings.Split(sub[1:], "/") {
			b.WriteString("/" + url.PathEscape(seg))
		}
	}
	sep := "?"
	for _, pair := range strings.Split(u.RawQuery, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		uk, kerr := url.QueryUnescape(k)
		uv, verr := url.QueryUnescape(v)
		if kerr != nil || verr != nil {
			return leadsPath
		}
		b.WriteString(sep + url.QueryEscape(uk) + "=" + url.QueryEscape(uv))
		sep = "&"
	}
	if u.Fragment != "" {
		b.WriteString("#" + url.PathEscape(u.Fragment))
	}
	return b.String()
}

// signalFetches groups signals by the fetch they came from, each fetch once, in the order
// its first signal comes.
func signalFetches(signals []LeadSignal) []signalFetch {
	var out []signalFetch
	for _, sg := range signals {
		i := slices.IndexFunc(out, func(f signalFetch) bool {
			return f.FetchURL == sg.FetchURL && f.FinalURL == sg.FinalURL && f.Status == sg.Status && sameTime(f.FetchedAt, sg.FetchedAt)
		})
		if i < 0 {
			out = append(out, signalFetch{FetchURL: sg.FetchURL, FinalURL: sg.FinalURL, Status: sg.Status, FetchedAt: sg.FetchedAt})
			i = len(out) - 1
		}
		out[i].Signals = append(out[i].Signals, sg)
	}
	return out
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// leadListQuery reads the list's filter from a query: every value trimmed, empty ones
// dropped, each repeatable filter capped at maxFilterValues.
func leadListQuery(v url.Values) LeadListQuery {
	one := func(k string) string { return strings.TrimSpace(v.Get(k)) }
	return LeadListQuery{
		Kinds:        filterValues(v["kind"]),
		Countries:    filterValues(v["country"]),
		Channels:     filterValues(v["channel"]),
		Starred:      one("starred"),
		HasPhone:     one("has_phone"),
		MinRelevance: one("min_relevance"),
		Status:       one("status"),
		Sort:         one("sort"),
		Page:         one("page"),
		ToLabel:      one("to_label"),
		Campaign:     one("campaign"),
		Town:         one("town"),
		PlaceType:    one("place_type"),
		Reach:        one("reach"),
	}
}

func filterValues(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" && len(out) < maxFilterValues {
			out = append(out, v)
		}
	}
	return out
}

// newLeadListView works out the pager, the empty states and the optional columns for p.
func newLeadListView(p LeadListPage) leadListView {
	v := leadListView{P: p, Back: leadListURL(p.Filter, p.Page), Order: p.Filter.Sort == "order", ToLabel: p.Filter.ToLabel == "1"}
	statusOn := p.Filter.Status != "" && p.Filter.Status != "open"
	v.ShowState = statusOn || p.Filter.Reach != "" // a next action spans states
	v.Tabs = reachTabs(p.Filter, p.ReachCounts)
	v.Campaigns = campaignTabs(p.Filter, p.CampaignTabs)
	v.PlaceTypes = placeTypeChips(p.Filter, p.PlaceTypeCounts)
	v.ShowCall = v.ToLabel
	for _, row := range p.Rows {
		v.ShowRelevance = v.ShowRelevance || row.Relevance != ""
		v.ShowOrder = v.ShowOrder || row.Position != ""
		v.ShowCall = v.ShowCall || row.Call != ""
	}
	for _, on := range []bool{len(p.Filter.Kinds) > 0, len(p.Filter.Countries) > 0, len(p.Filter.Channels) > 0,
		p.Filter.Starred != "", p.Filter.HasPhone != "", p.Filter.MinRelevance != "", statusOn, v.ToLabel,
		p.Filter.Campaign != "" && p.Filter.Campaign != campaignAll, p.Filter.Town != ""} {
		if on {
			v.Filters++
		}
	}
	if p.PerPage > 0 {
		v.Pages = (p.Total + p.PerPage - 1) / p.PerPage
	}
	if p.Page > 1 {
		v.Prev = leadListURL(p.Filter, p.Page-1)
	}
	if p.PerPage > 0 && p.Page*p.PerPage < p.Total {
		v.Next = leadListURL(p.Filter, p.Page+1)
	}
	v.NoLeads = p.TotalLeads == 0
	v.NoneMatch = !v.NoLeads && len(p.Rows) == 0
	return v
}

// leadListURL is the list under f at page n: the filter kept, the page set (left out for
// the first). A next action picks its own states and a blank Status means every state, so the
// default Status (open), which the backend echoes, is left out with one: it would hide leads.
func leadListURL(f LeadListQuery, n int) string {
	if f.Reach != "" && f.Status == "open" {
		f.Status = ""
	}
	q := url.Values{}
	for k, vs := range map[string][]string{"kind": f.Kinds, "country": f.Countries, "channel": f.Channels} {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	for k, v := range map[string]string{"starred": f.Starred, "has_phone": f.HasPhone, "min_relevance": f.MinRelevance,
		"status": f.Status, "sort": f.Sort, "to_label": f.ToLabel, "campaign": f.Campaign, "reach": f.Reach,
		"town": f.Town, "place_type": f.PlaceType} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if n > 1 {
		q.Set("page", strconv.Itoa(n))
	}
	if len(q) == 0 {
		return leadsPath
	}
	return leadsPath + "?" + q.Encode()
}

// webURL reports whether s is an http or https URL with a host: only such a URL becomes a
// link on a lead's page; anything else is shown as text.
func webURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return true
	}
	return false
}
