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

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadview"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// The Leads list and a lead's page: the console's query read into leadstore's filter, the
// list weighed by the profile in force and the relevance trust gate, and every row worded
// with leadview. Both read live (D15): no snapshot.

// relevanceQuestion is the relevance question whose latest eval decides whether the list
// sorts by relevance (the trust gate, D9).
const relevanceQuestion = "relevance@1"

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
	cfg     leadconfig.Config
	profile leadconfig.Profile
	trusted bool
}

func (b *backend) leadsWeighing(ctx context.Context) (leadsWeighing, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return leadsWeighing{}, err
	}
	lq := b.lq()
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
	f, echo, notes := leadListFilter(q, opts)
	pf, err := leadstore.ListProfileJSON(w.profile)
	if err != nil {
		return console.LeadListPage{}, err
	}
	lc := leadstore.ListContext{ProfileJSON: pf, SkipRankOver: w.cfg.SkipRankOver, SortByRelevance: w.trusted}
	lq := b.lq()
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
	p := console.LeadListPage{Total: total, Page: f.Page, PerPage: per, Filter: echo, Options: opts, Notes: notes}
	if p.TotalLeads, err = leadCount(ctx, b); err != nil {
		return console.LeadListPage{}, err
	}
	order, err := outreachOrder(ctx, b, starredIDs(rows))
	if err != nil {
		return console.LeadListPage{}, err
	}
	today := b.now().In(b.loc)
	for _, r := range rows {
		p.Rows = append(p.Rows, leadListRow(r, kinds, w, order, today))
	}
	return p, nil
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
	f.Status = choose("status", q.Status, leadstore.ListOpen, o.Statuses, &notes)
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
		Finding:  leadview.FindingLine(r.Finding, w.cfg.Facts, today),
		State:    leadStateWords(r.State),
		HasEmail: r.HasEmail, HasForm: r.HasForm, HasPhone: r.HasPhone,
		Starred: r.Starred, SmallSite: r.SmallSite,
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
	p := console.LeadDetailPage{
		LeadListRow:   leadListRow(d.ListRow, kinds, w, order, b.now().In(b.loc)),
		Origin:        d.Origin,
		CountryPolicy: countryPolicy(w, d.Country),
		Why:           leadview.WhyLines(d.Reasons),
	}
	if d.ListRank != nil {
		p.ListRank = strconv.Itoa(*d.ListRank)
	}
	for _, s := range d.Signals {
		p.Signals = append(p.Signals, console.LeadSignal{Name: s.Name, Value: s.Value, FetchURL: s.URL, FinalURL: s.FinalURL,
			Status: httpStatusWords(s.Status), FetchedAt: s.FetchedAt})
	}
	for _, r := range d.Routes {
		p.Routes = append(p.Routes, console.LeadRoute{Kind: routeKindWords(r.Kind), Value: r.Value, Note: routeNote(r),
			PageURL: r.FinalURL, FoundAt: new(r.FoundAt)})
	}
	for _, f := range d.Facts {
		lf := console.LeadFact{Value: f.Value, PageURL: f.FinalURL, FoundAt: new(f.FoundAt)}
		switch f.Kind {
		case leadstore.FactPhone:
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
	if r.MXOK {
		return "mail server found"
	}
	return "no mail server found"
}

// channelWords is a channel in words.
func channelWords(ch string) string {
	switch ch {
	case "email":
		return "email"
	case "form":
		return "contact form"
	case "linkedin":
		return "LinkedIn"
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
		l = fmt.Sprintf("Email %d · message %d", m.Step+1, m.ID)
	case "form":
		l = fmt.Sprintf("Contact form text · message %d", m.ID)
	case "linkedin":
		l = fmt.Sprintf("LinkedIn note · message %d", m.ID)
	default:
		l = fmt.Sprintf("Message %d", m.ID)
	}
	if m.Shadow {
		l += " · shadow batch"
	}
	return l
}
