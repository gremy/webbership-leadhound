package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadview"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/places"
	"github.com/gremy/webbership-leadhound/internal/probe"
)

// The Leads list and a lead's page: the console's query read into leadstore's filter, the
// list weighed by the profile in force and the relevance trust gate, and every row worded
// with leadview. Both read live (D15): no snapshot.

// relevanceQuestion is the relevance question whose latest eval decides whether the list
// sorts by relevance (the trust gate, D9).
const relevanceQuestion = leadrate.Question

// noteValueRunes is the most of one ignored value a note repeats.
const noteValueRunes = 40

// The list's fixed choices, in the order the form offers them.
var (
	leadChannelOptions = []console.FilterOption{
		{Value: leadstore.ListChannelEmail, Label: "Email address"},
		{Value: leadstore.ListChannelForm, Label: "Contact form"},
		{Value: leadstore.ListChannelPhone, Label: "Phone number"},
	}
	leadStatusOptions = []console.FilterOption{
		{Value: leadstore.ListOpen, Label: "Not contacted yet"},
		{Value: leadstore.ListContacted, Label: "Contacted"},
		{Value: leadstore.ListAll, Label: "All"},
	}
	leadSortOptions = []console.FilterOption{
		{Value: leadstore.SortRelevance, Label: "Relevance"},
		{Value: leadstore.SortNewest, Label: "Newest"},
		{Value: leadstore.SortOrder, Label: "Outreach order (starred only)"},
	}
)

// leadKind is one lead_kind row: its name, label and whether batches draft it.
type leadKind struct {
	Name, Label string
	Outreach    bool
}

// leadKinds reads every lead kind, outreach kinds first in the file's kinds_priority order,
// then by name.
func leadKinds(ctx context.Context, b *backend, cfg leadconfig.Config) ([]leadKind, error) {
	rows, err := b.s.Pool().Query(ctx, `SELECT name, label, outreach FROM lead_kind ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("console: lead kinds: %w", err)
	}
	kinds, err := pgx.CollectRows(rows, pgx.RowToStructByPos[leadKind])
	if err != nil {
		return nil, fmt.Errorf("console: lead kinds: %w", err)
	}
	place := func(name string) int {
		if i := slices.Index(cfg.KindsPriority, name); i >= 0 {
			return i
		}
		return len(cfg.KindsPriority)
	}
	slices.SortStableFunc(kinds, func(a, b leadKind) int {
		if a.Outreach != b.Outreach {
			if a.Outreach {
				return -1
			}
			return 1
		}
		return cmp.Compare(place(a.Name), place(b.Name))
	})
	return kinds, nil
}

// kindLabel is kind's label, or its name when the table has no such kind.
func kindLabel(kinds []leadKind, name string) string {
	for _, k := range kinds {
		if k.Name == name {
			return k.Label
		}
	}
	return name
}

// leadsWeighing is what the list and a lead's page are weighed by: the effective
// profile, the small-site cut and whether relevance passed its trust gate.
type leadsWeighing struct {
	cfg      leadconfig.Config
	profile  leadconfig.Profile
	trusted  bool
	research researchRules // what a star may look for (the star notes)
}

func (b *backend) leadsWeighing(ctx context.Context) (leadsWeighing, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return leadsWeighing{}, err
	}
	w, err := weighLeads(ctx, b.lq(), setup)
	if err != nil {
		return leadsWeighing{}, err
	}
	w.research, err = loadResearchRules(ctx, b.lq(), setup.leads, b.now())
	return w, err
}

// weighLeads is leadsWeighing over lq for the effective config in setup; leads rate orders
// its queue by it too.
func weighLeads(ctx context.Context, lq leadstore.Queries, setup leadsSetup) (leadsWeighing, error) {
	// The fallback the queue reads (CandidateLeads): the file's profile, or kind points
	// from kinds_priority; a saved profile replaces it.
	fallback := setup.leads.PriorityProfile()
	if fallback.IsZero() {
		fallback = leadconfig.Profile{Kinds: leadconfig.KindPoints(setup.leads.KindsPriority)}
	}
	p, _, err := lq.EffectiveProfile(ctx, fallback)
	if err != nil {
		return leadsWeighing{}, err
	}
	e, ok, err := lq.LatestRelevanceEval(ctx, relevanceQuestion)
	if err != nil {
		return leadsWeighing{}, err
	}
	return leadsWeighing{cfg: setup.leads, profile: p, trusted: ok && e.Trusted}, nil
}

// LeadList is one page of the Leads list: the query read into a filter (unknown values
// ignored with a note, numbers clamped), the leads it matches weighed by the profile in
// force, sorted by relevance only once relevance passed its trust gate, and worded for
// the page. A page past the last shows the last.
func (b *backend) LeadList(ctx context.Context, q console.LeadListQuery) (console.LeadListPage, error) {
	w, err := b.leadsWeighing(ctx)
	if err != nil {
		return console.LeadListPage{}, err
	}
	kinds, err := leadKinds(ctx, b, w.cfg)
	if err != nil {
		return console.LeadListPage{}, err
	}
	opts := leadFilterOptions(kinds, w.cfg, w.trusted)
	lq := b.lq()
	campaigns, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return console.LeadListPage{}, err
	}
	for _, c := range campaigns {
		opts.Campaigns = append(opts.Campaigns, console.FilterOption{Value: c.Slug, Label: c.Name})
	}
	f, echo, notes := leadListFilter(q, opts)
	if echo.ToLabel != "" {
		if f.LeadIDs, err = toLabelIDs(ctx, lq); err != nil {
			return console.LeadListPage{}, err
		}
	}
	if err := campaignFilter(ctx, lq, q.Campaign, &f, &echo, &notes); err != nil {
		return console.LeadListPage{}, err
	}
	pf, err := leadstore.ListProfileJSON(w.profile)
	if err != nil {
		return console.LeadListPage{}, err
	}
	lc := leadstore.ListContext{ProfileJSON: pf, SkipRankOver: w.cfg.SkipRankOver, SortByRelevance: w.trusted}
	rows, total, err := lq.LeadList(ctx, f, lc)
	if err != nil {
		return console.LeadListPage{}, err
	}
	per := leadstore.ListPerPageDefault
	if last := max((total+per-1)/per, 1); len(rows) == 0 && f.Page > last {
		notes = append(notes, fmt.Sprintf("There is no page %d; this is the last page.", f.Page))
		f.Page = last
		if rows, total, err = lq.LeadList(ctx, f, lc); err != nil {
			return console.LeadListPage{}, err
		}
	}
	echo.Page = strconv.Itoa(f.Page)
	if f.Page == 1 {
		echo.Page = ""
	}
	p := console.LeadListPage{Total: total, Page: f.Page, PerPage: per, Filter: echo, Options: opts, Notes: notes, LabelGoal: labelGoal}
	if p.TotalLeads, err = leadCount(ctx, b); err != nil {
		return console.LeadListPage{}, err
	}
	if p.Labelled, err = lq.LabelCount(ctx); err != nil {
		return console.LeadListPage{}, err
	}
	order, err := outreachOrder(ctx, b, starredIDs(rows))
	if err != nil {
		return console.LeadListPage{}, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.LeadID)
	}
	calls, err := lq.LatestLabels(ctx, ids)
	if err != nil {
		return console.LeadListPage{}, err
	}
	if w.research, err = w.research.withPhones(ctx, lq, newLeadOrgs(rows...)); err != nil {
		return console.LeadListPage{}, err
	}
	today := b.now().In(b.loc)
	for _, r := range rows {
		row := leadListRow(r, kinds, w, order, today)
		if l := calls[r.LeadID]; l.Band != nil {
			row.Call = *l.Band
		}
		p.Rows = append(p.Rows, row)
	}
	return p, nil
}

// campaignFilter narrows f to campaign slug of the operator's account, echoed so it
// survives paging. An unknown slug lists no lead, with a note: never an error page.
func campaignFilter(ctx context.Context, lq leadstore.Queries, slug string, f *leadstore.ListFilter, echo *console.LeadListQuery, notes *[]string) error {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil
	}
	echo.Campaign = slug
	c, err := lq.CampaignBySlug(ctx, leadstore.DefaultAccountID, slug)
	if errors.Is(err, leadstore.ErrCampaignNotFound) {
		*notes = append(*notes, fmt.Sprintf("No campaign is called %s, so no lead is listed.", noteValue(slug)))
		f.LeadIDs = []int64{}
		return nil
	}
	if err != nil {
		return err
	}
	f.CampaignID = c.ID
	return nil
}

// labelGoal is how many leads with the operator's call the trust gate needs (D9).
const labelGoal = leadrate.TrustMinN

// toLabelIDs are the leads of the To label filter: labelGoal open leads spread over kinds,
// Drupal majors and countries, as a golden draft picks them (spreadSample), and every lead
// that has a call, whatever its state. Never nil, so an empty set lists no lead.
func toLabelIDs(ctx context.Context, lq leadstore.Queries) ([]int64, error) {
	open, err := lq.LeadsToRate(ctx, leadstore.ListContext{})
	if err != nil {
		return nil, err
	}
	labelled, err := lq.LabelledLeadIDs(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, labelGoal+len(labelled))
	for _, l := range spreadSample(open, labelGoal) {
		ids = append(ids, l.LeadID)
	}
	for _, id := range labelled {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// callBands are the bands the "Your call" block offers, best first, each with leadrate's
// plain description as its help.
func callBands() []console.CallBand {
	var out []console.CallBand
	for _, name := range leadrate.Bands() {
		out = append(out, console.CallBand{Value: name, Label: strings.ToUpper(name[:1]) + name[1:], Help: leadrate.Describe(name)})
	}
	return out
}

// leadCall is lead id's "Your call" block: its latest call and when, the bands, and how many
// leads have a call. The row's Call is set from the same read.
func leadCall(ctx context.Context, lq leadstore.Queries, row *console.LeadListRow) (console.LeadCall, error) {
	c := console.LeadCall{Bands: callBands(), LabelGoal: labelGoal}
	calls, err := lq.LatestLabels(ctx, []int64{row.LeadID})
	if err != nil {
		return console.LeadCall{}, err
	}
	if l, ok := calls[row.LeadID]; ok && l.Band != nil {
		row.Call, c.At = *l.Band, &l.CreatedAt
	}
	if c.Labelled, err = lq.LabelCount(ctx); err != nil {
		return console.LeadCall{}, err
	}
	return c, nil
}

// leadFilterOptions are the choices the filter form offers: the outreach kinds, the
// countries leads.yaml has a policy for, and the fixed channels, statuses and sorts. Until
// relevance passes its trust gate, the relevance sort says it sorts by score.
func leadFilterOptions(kinds []leadKind, cfg leadconfig.Config, trusted bool) console.LeadFilterOptions {
	o := console.LeadFilterOptions{Channels: leadChannelOptions, Statuses: leadStatusOptions, Sorts: slices.Clone(leadSortOptions)}
	if !trusted {
		o.Sorts[0].Label = "Relevance (by score for now)"
	}
	for _, k := range kinds {
		if k.Outreach {
			o.Kinds = append(o.Kinds, console.FilterOption{Value: k.Name, Label: k.Label})
		}
	}
	codes := make([]string, 0, len(cfg.Countries))
	for c := range cfg.Countries {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	for _, c := range codes {
		o.Countries = append(o.Countries, console.FilterOption{Value: c, Label: countryName(c)})
	}
	return o
}

// countryName is an ISO 3166-1 alpha-2 code's English name, or the code itself.
func countryName(code string) string {
	r, err := language.ParseRegion(code)
	if err != nil {
		return code
	}
	return cmp.Or(display.English.Regions().Name(r), code)
}

// languageName is an ISO 639-1 code's English name, or the code itself.
func languageName(code string) string {
	b, err := language.ParseBase(code)
	if err != nil {
		return code
	}
	return cmp.Or(display.English.Languages().Name(b), code)
}

// leadListFilter reads q into leadstore's filter against the offered choices. Values the
// form does not offer are ignored, each field's in one note; min relevance is clamped to
// 0..100 and the page to 1 and up. echo is the filter as taken, Page left for the caller.
func leadListFilter(q console.LeadListQuery, o console.LeadFilterOptions) (f leadstore.ListFilter, echo console.LeadListQuery, notes []string) {
	known := func(field string, in []string, opts []console.FilterOption, norm func(string) string) []string {
		var kept, ignored []string
		for _, v := range in {
			v = norm(v)
			switch {
			case slices.ContainsFunc(opts, func(x console.FilterOption) bool { return x.Value == v }):
				if !slices.Contains(kept, v) {
					kept = append(kept, v)
				}
			default:
				ignored = append(ignored, noteValue(v))
			}
		}
		if len(ignored) > 0 {
			notes = append(notes, fmt.Sprintf("Ignored the %s %s: not one the list offers.", field, strings.Join(ignored, ", ")))
		}
		return kept
	}
	same := func(s string) string { return s }
	f.Kinds = known("kind", q.Kinds, o.Kinds, same)
	f.Countries = known("country", q.Countries, o.Countries, strings.ToUpper)
	f.Channels = known("channel", q.Channels, o.Channels, strings.ToLower)
	echo.Kinds, echo.Countries, echo.Channels = f.Kinds, f.Countries, f.Channels

	switch q.Starred {
	case "":
	case "yes":
		f.Starred, echo.Starred = new(true), "yes"
	case "no":
		f.Starred, echo.Starred = new(false), "no"
	default:
		notes = append(notes, fmt.Sprintf("Ignored starred %s: it takes yes or no.", noteValue(q.Starred)))
	}
	switch q.HasPhone {
	case "":
	case "1":
		f.HasPhone, echo.HasPhone = true, "1"
	default:
		notes = append(notes, fmt.Sprintf("Ignored has_phone %s: it takes 1.", noteValue(q.HasPhone)))
	}
	if q.MinRelevance != "" {
		n, err := strconv.Atoi(q.MinRelevance)
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("Ignored the relevance %s: it takes a whole number from 0 to 100.", noteValue(q.MinRelevance)))
		case n > 100:
			f.MinRelevance = 100
			notes = append(notes, "Relevance goes up to 100, so the list shows leads rated 100.")
		case n > 0:
			f.MinRelevance = n
		}
		if f.MinRelevance > 0 {
			echo.MinRelevance = strconv.Itoa(f.MinRelevance)
		}
	}
	statusDefault := leadstore.ListOpen
	switch q.ToLabel {
	case "":
	case "1":
		// The leads to label are every lead with a call, whatever its state.
		echo.ToLabel, statusDefault = "1", leadstore.ListAll
	default:
		notes = append(notes, fmt.Sprintf("Ignored to_label %s: it takes 1.", noteValue(q.ToLabel)))
	}
	f.Status = choose("status", q.Status, statusDefault, o.Statuses, &notes)
	f.Sort = choose("sort", q.Sort, leadstore.SortRelevance, o.Sorts, &notes)
	echo.Status, echo.Sort = f.Status, f.Sort
	f.Page = 1
	if q.Page != "" {
		n, err := strconv.Atoi(q.Page)
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("Ignored the page %s: it takes a whole number from 1.", noteValue(q.Page)))
		case n > 1:
			f.Page = n
		}
	}
	f.PerPage = leadstore.ListPerPageDefault
	return f, echo, notes
}

// choose is v when opts offers it, else def, with a note when v was given.
func choose(field, v, def string, opts []console.FilterOption, notes *[]string) string {
	v = strings.ToLower(v)
	switch {
	case v == "":
		return def
	case slices.ContainsFunc(opts, func(x console.FilterOption) bool { return x.Value == v }):
		return v
	}
	*notes = append(*notes, fmt.Sprintf("Ignored the %s %s: not one the list offers.", field, noteValue(v)))
	return def
}

// noteValue quotes a submitted value for a note, cut to noteValueRunes characters.
func noteValue(v string) string {
	if utf8.RuneCountInString(v) > noteValueRunes {
		v = string([]rune(v)[:noteValueRunes]) + "…"
	}
	return strconv.Quote(v)
}

// leadCount is how many leads there are at all, as the list counts them (selftest leads
// never listed).
func leadCount(ctx context.Context, b *backend) (int, error) {
	var n int
	if err := b.s.Pool().QueryRow(ctx,
		`SELECT count(*)::int FROM lead l JOIN org o ON o.id = l.org_id
		 WHERE l.kind <> 'selftest' AND o.kind <> 'selftest'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("console: lead count: %w", err)
	}
	return n, nil
}

func starredIDs(rows []leadstore.ListRow) []int64 {
	var ids []int64
	for _, r := range rows {
		if r.Starred {
			ids = append(ids, r.LeadID)
		}
	}
	return ids
}

// outreachOrder is the place of each of ids in the outreach order, counted from 1: the
// starred leads not yet taken into outreach (state new), by position then id, as the list
// sorts them. A lead outside the order has no entry.
func outreachOrder(ctx context.Context, b *backend, ids []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := b.s.Pool().Query(ctx,
		`SELECT id, n FROM (
		   SELECT l.id, row_number() OVER (ORDER BY st.position, l.id)::int AS n
		   FROM (SELECT DISTINCT ON (lead_id) lead_id, action, position FROM lead_star ORDER BY lead_id, id DESC) st
		   JOIN lead l ON l.id = st.lead_id
		   JOIN org o ON o.id = l.org_id
		   WHERE st.action <> 'unstar' AND l.state = 'new' AND l.kind <> 'selftest' AND o.kind <> 'selftest'
		 ) x WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("console: outreach order: %w", err)
	}
	var id int64
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&id, &n}, func() error {
		out[id] = n
		return nil
	}); err != nil {
		return nil, fmt.Errorf("console: outreach order: %w", err)
	}
	return out, nil
}

// leadListRow words one row of the list.
func leadListRow(r leadstore.ListRow, kinds []leadKind, w leadsWeighing, order map[int64]int, today time.Time) console.LeadListRow {
	row := console.LeadListRow{
		LeadID: r.LeadID, OrgID: r.OrgID, Name: r.Name, Domain: r.Domain, Country: r.Country,
		Kind:     kindLabel(kinds, r.Kind),
		Campaign: r.CampaignName,
		Finding:  leadview.FindingLine(r.Finding, w.cfg.Facts, today),
		State:    leadStateWords(r.State),
		HasEmail: r.HasEmail, HasForm: r.HasForm, HasPhone: r.HasPhone,
		Starred: r.Starred, SmallSite: r.SmallSite,
		StarNote: starNote(w.research, r.State, w.research.listLead(r)),
	}
	if r.Relevance != nil {
		row.Relevance = leadview.RelevanceWords(r.Relevance, r.Band, w.trusted)
	}
	if n, ok := order[r.LeadID]; ok {
		row.Position = strconv.Itoa(n)
	}
	return row
}

// leadStateWords is a lead's state in plain words.
func leadStateWords(state string) string {
	switch state {
	case leadstore.LeadNew:
		return "not contacted yet"
	case leadstore.LeadBatched:
		return "in a batch"
	case leadstore.LeadOptedOut:
		return "opted out"
	}
	return strings.ReplaceAll(state, "_", " ")
}

// LeadDetail is everything stored about lead id, worded for its page: the lead weighed by
// the profile in force (its points and why), its finding and the fetches behind it, its
// routes, its facts by kind, the agency page, its site, its country's policy, its history,
// its place in the queue, its messages and its relevance. ErrNotFound for no such lead.
func (b *backend) LeadDetail(ctx context.Context, id int64) (console.LeadDetailPage, error) {
	w, err := b.leadsWeighing(ctx)
	if err != nil {
		return console.LeadDetailPage{}, err
	}
	d, err := b.lq().LeadDetail(ctx, id)
	if errors.Is(err, leadstore.ErrLeadNotFound) {
		return console.LeadDetailPage{}, console.ErrNotFound
	}
	if err != nil {
		return console.LeadDetailPage{}, err
	}
	d.Weigh(w.profile, w.cfg.SkipRankOver)
	kinds, err := leadKinds(ctx, b, w.cfg)
	if err != nil {
		return console.LeadDetailPage{}, err
	}
	order, err := outreachOrder(ctx, b, starredIDs([]leadstore.ListRow{d.ListRow}))
	if err != nil {
		return console.LeadDetailPage{}, err
	}
	if w.research, err = w.research.withPhones(ctx, b.lq(), newLeadOrgs(d.ListRow)); err != nil {
		return console.LeadDetailPage{}, err
	}
	p := console.LeadDetailPage{
		LeadListRow:   leadListRow(d.ListRow, kinds, w, order, b.now().In(b.loc)),
		Origin:        d.Origin,
		CountryPolicy: countryPolicy(w, d.Country),
		Why:           leadview.WhyLines(d.Reasons),
	}
	if p.YourCall, err = leadCall(ctx, b.lq(), &p.LeadListRow); err != nil {
		return console.LeadDetailPage{}, err
	}
	lr, err := b.leadResearch(ctx, id)
	if err != nil {
		return console.LeadDetailPage{}, err
	}
	p.Research = consoleResearch(lr, w.research, d.State, w.research.listLead(d.ListRow))
	p.Report = b.reportLink(ctx, w.cfg, id)
	// Every purpose together; 0 (no Claude call on this lead yet) shows nothing.
	if spent, err := b.s.Q().ClaudeSpendForLead(ctx, id, ""); err != nil {
		return console.LeadDetailPage{}, err
	} else if spent > 0 {
		p.ClaudeSpend = console.USD(spent)
	}
	if d.ListRank != nil {
		p.ListRank = strconv.Itoa(*d.ListRank)
	}
	for _, s := range d.Signals {
		name, value := cafeSignalWords(s.Name, s.Value)
		p.Signals = append(p.Signals, console.LeadSignal{Name: name, Value: value, FetchURL: s.URL, FinalURL: s.FinalURL,
			Status: httpStatusWords(s.Status), FetchedAt: s.FetchedAt})
	}
	var addrs []string
	hasMX := false
	for _, r := range d.Routes {
		if r.Kind == leadstore.RouteEmail {
			addrs = append(addrs, r.Value)
			hasMX = hasMX || r.MXOK
		}
	}
	p.SendNowBlock = sendNowBlock(w.cfg, d.ListRow, len(addrs) > 0, hasMX)
	checks, err := b.lq().EmailChecks(ctx, leadstore.DefaultAccountID, addrs, time.Time{})
	if err != nil {
		return console.LeadDetailPage{}, err
	}
	for _, r := range d.Routes {
		lr := console.LeadRoute{Kind: routeKindWords(r.Kind), Value: r.Value, Note: routeNote(r),
			PageURL: r.FinalURL, FoundAt: new(r.FoundAt)}
		if r.Kind == leadstore.RouteEmail {
			lr.Check, lr.CheckedAt = emailCheckLine(checks[strings.ToLower(r.Value)], w.cfg.Verify.CatchAllRule(), b.now(), startOfDay(b.now(), b.loc))
		}
		p.Routes = append(p.Routes, lr)
	}
	for _, f := range d.Facts {
		lf := console.LeadFact{Value: f.Value, PageURL: f.FinalURL, FoundAt: new(f.FoundAt)}
		switch f.Kind {
		case leadstore.FactPhone:
			if f.DerivedBy == places.DerivedBy {
				lf.Label = placeSourceWords
			}
			p.Phones = append(p.Phones, lf)
		case leadstore.FactDescription:
			p.Descriptions = append(p.Descriptions, lf)
		case leadstore.FactSocial:
			lf.Label = socialLabel(f.Value)
			p.Social = append(p.Social, lf)
		}
	}
	if a := d.Agency; a != nil {
		p.Agency = &console.LeadAgency{URL: a.URL, Phrase: a.Phrase}
		if p.Agency.FetchURL, err = fetchURL(ctx, b, a.FetchID); err != nil {
			return console.LeadDetailPage{}, err
		}
	}
	for _, e := range d.Events {
		p.Events = append(p.Events, console.LeadEvent{State: leadStateWords(e.State), Reason: e.Reason, Actor: e.Actor, At: e.At})
	}
	if pr := d.Priority; pr != nil {
		p.Priority = &console.LeadPriority{Words: priorityWords(*pr), Actor: pr.Actor, At: pr.At}
	}
	for _, m := range d.Messages {
		p.Messages = append(p.Messages, console.LeadDetailMessage{BatchID: m.BatchID, Label: detailMessageLabel(m),
			Channel: channelWords(m.Channel), State: strings.ReplaceAll(m.State, "_", " "), Subject: m.Subject, Body: m.Body,
			Due: m.DueAt, Sent: m.SentAt})
	}
	for _, r := range d.RelevanceHistory {
		words := leadview.RelevanceWords(&r.Score, r.Band, w.trusted)
		if r.Model != "" {
			words += " · " + r.Model
		}
		p.RelevanceHistory = append(p.RelevanceHistory, console.LeadRelevance{Words: words, At: r.CreatedAt})
	}
	return p, nil
}

// sendNowBlock is why Send now cannot send lead r, from the lead alone, in the words the
// manual send's refusal uses (manualLeftOut); "" when it may be tried.
func sendNowBlock(cfg leadconfig.Config, r leadstore.ListRow, hasEmail, hasMX bool) string {
	switch {
	case r.State == leadstore.LeadBatched:
		return "It is already in a batch that waits for approval; send that batch instead."
	case r.State != leadstore.LeadNew:
		return "Its state is " + leadStateWords(r.State) + ", and only a new lead can be sent."
	case r.Channel == leadstore.ChannelNone:
		w := noChannelWhy(cfg, r.Country, hasEmail, hasMX)
		return strings.ToUpper(w[:1]) + w[1:] + "."
	}
	return ""
}

// countryPolicy is the line leads.yaml and the profile give the lead's country: never
// contacted, not written to, or the channel, language and business hours it is written in.
func countryPolicy(w leadsWeighing, code string) string {
	if code == "" {
		return ""
	}
	name := countryName(code)
	if slices.Contains(w.profile.ExcludeCountries, code) {
		return name + ": never contacted (on the never-contact list)."
	}
	c, ok := w.cfg.Countries[code]
	if !ok || c.Channel == "" || c.Channel == leadstore.ChannelNone {
		return name + ": not written to; leads.yaml gives it no channel."
	}
	line := name + ": written to by " + channelWords(c.Channel)
	if c.Lang != "" {
		line += " in " + languageName(c.Lang)
	}
	if c.TZ != "" {
		line += ", in business hours on " + c.TZ + " time"
	}
	return line + "."
}

// httpStatusWords is an HTTP status in words, "200 OK"; "" for none.
func httpStatusWords(code int) string {
	if code == 0 {
		return ""
	}
	if t := http.StatusText(code); t != "" {
		return strconv.Itoa(code) + " " + t
	}
	return strconv.Itoa(code)
}

func routeKindWords(kind string) string {
	switch kind {
	case leadstore.RouteEmail:
		return "Email"
	case leadstore.RouteForm:
		return "Contact form"
	}
	return kind
}

// routeNote is what was checked about a route: an email address's mail server.
func routeNote(r leadstore.DetailRoute) string {
	if r.Kind != leadstore.RouteEmail {
		return ""
	}
	note := "no mail server found"
	if r.MXOK {
		note = "mail server found"
	}
	if r.DerivedBy == places.DerivedBy {
		note += " · " + placeSourceWords
	}
	return note
}

// placeSourceWords says a route or phone came from the café's map listing (Overture), not
// from a page of its own.
const placeSourceWords = "from its map listing (Overture)"

// channelWords is a channel in words.
func channelWords(ch string) string {
	switch ch {
	case "email":
		return "email"
	case "form":
		return "contact form"
	case "linkedin":
		return "LinkedIn"
	case leadstore.ChannelPhone:
		return "phone call"
	}
	return strings.ReplaceAll(ch, "_", " ")
}

// socialLabel names a social link by its host, "" when it has none.
func socialLabel(v string) string {
	u, err := url.Parse(v)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// fetchURL is fetch id's address after redirects, else the address asked for.
func fetchURL(ctx context.Context, b *backend, id int64) (string, error) {
	var u string
	err := b.s.Pool().QueryRow(ctx, `SELECT coalesce(nullif(final_url, ''), url) FROM http_fetch WHERE id = $1`, id).Scan(&u)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("console: fetch url: %w", err)
	}
	return u, nil
}

// priorityWords is the lead's latest queue steer in words.
func priorityWords(p leadstore.DetailPriority) string {
	switch p.Action {
	case leadstore.PriorityPin:
		return "Pinned to the top of the queue."
	case leadstore.PriorityDefer:
		if p.Until != nil {
			if d, err := outreach.FormatDate(*p.Until, outreach.LangEnglish); err == nil {
				return "Pushed back through " + d + "."
			}
		}
		return "Pushed back."
	case leadstore.PriorityClear:
		return "Back in its place by score."
	}
	return p.Action
}

// detailMessageLabel names a message on a lead's page: which email, or the manual item,
// its number, and whether its batch was a shadow one.
func detailMessageLabel(m leadstore.DetailMessage) string {
	var l string
	switch m.Channel {
	case "email":
		l = fmt.Sprintf("Step %d · email · message %d", m.Step+1, m.ID)
	case "form":
		l = fmt.Sprintf("Contact form text · message %d", m.ID)
	case "linkedin":
		l = fmt.Sprintf("LinkedIn note · message %d", m.ID)
	case leadstore.ChannelPhone:
		l = fmt.Sprintf("Step %d · call · message %d", m.Step+1, m.ID)
	default:
		l = fmt.Sprintf("Message %d", m.ID)
	}
	if m.Shadow {
		l += " · shadow batch"
	}
	return l
}

// cafeSignalWords words a cafe@1 probe signal for the lead page: "loyalty" with
// "stampme, via script: https://…" (the vendor or phrase id, where on the page, what
// matched), and the language with where it was read. Any other signal stays as stored.
func cafeSignalWords(name, value string) (string, string) {
	switch name {
	case cafe.SigLoyalty, cafe.SigOrdering, cafe.SigInstagram:
		parts := strings.SplitN(value, ":", 4)
		if len(parts) == 4 && parts[0] == name {
			return name, parts[1] + ", via " + parts[2] + ": " + parts[3]
		}
	case probe.SigCafeLang:
		parts := strings.SplitN(value, ":", 3)
		if len(parts) == 3 {
			return "language", parts[0] + ", via " + parts[1] + " (" + parts[2] + ")"
		}
	}
	return name, value
}
