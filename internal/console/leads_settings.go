package console

import (
	"cmp"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// maxSettingsValues is the most values one repeatable settings field (country, wanted, kind,
// on) takes from a request; the rest are dropped before the backend sees them.
const maxSettingsValues = 300

// settingsSection is one editable section of the Settings page: its Settings* name (the
// path segment and the anchor both) and its heading.
type settingsSection struct{ Name, Title string }

// settingsSections are the Settings page's editable sections, in the page's order.
var settingsSections = []settingsSection{
	{SettingsWho, "Who to contact"},
	{SettingsKinds, "Lead kinds"},
	{SettingsSmallSites, "Small sites"},
	{SettingsBatches, "Batches draw from"},
	{SettingsDates, "Dates"},
	{SettingsRelevance, "Jev relevance"},
	{SettingsOrder, "Fallback order"},
}

// batchSources are the choices of Batches draw from, each a value and its words.
var batchSources = []FilterOption{
	{BatchesStarred, "Starred leads only"},
	{BatchesStarredThenRest, "Starred leads first, then the rest of the queue"},
}

// envHelp is one plain line for each name on EnvAllowlist, shown under its value on the
// Settings page's read-only part.
var envHelp = map[string]string{
	"LEADHOUND_OUTREACH":                 "Set to live, approved emails really go out. Anything else writes each email to a folder on the server and sends nothing.",
	"LEADHOUND_LEADS_TIMERS":             "Set to on, the daily jobs that find sites and their contacts run.",
	"LEADHOUND_LEADS_SEND":               "Set to on, with LEADHOUND_OUTREACH at live and LEADHOUND_LEADS_TIMERS at on, the daily jobs that make batches, send the batches you approved and read replies run.",
	"LEADHOUND_PROBE_RATE":               "How many requests a second checking sites may make, across a run.",
	"LEADHOUND_PROBE_CONCURRENCY":        "How many sites are checked at once.",
	"LEADHOUND_TZ":                       "The time zone dates are shown in.",
	"LEADHOUND_JEV_MODEL":                "The model Jev uses.",
	"LEADHOUND_JEV_DAILY_CALL_CAP":       "The most Jev calls a day for scoring jobs, counted apart from leads.",
	"LEADHOUND_JEV_LEADS_DAILY_CALL_CAP": "The most leads Jev rates a day. Most ratings a day, under Jev relevance, can lower it, never raise it.",
}

// dateNames are the end-of-life dates the Dates section posts, as their form fields.
var dateNames = []string{"d6_eol", "d7_eol", "d8_eol", "d9_eol", "d10_eol"}

// settingsView is what the Settings page renders: the backend's page (a refused section's
// values replaced by what was submitted), the line from the last action, and, after a
// refusal, the section refused and its problems.
type settingsView struct {
	P            LeadSettingsPage
	Sections     []settingsSection
	BatchSources []FilterOption
	EnvHelp      map[string]string // envHelp: a line for each allowlisted environment name
	Message      string
	Error        bool
	Section      string // the refused section, or ""
	SectionTitle string
	Problems     []string
}

// settingsSectionName is the Settings* name a path segment names, taken from the table, not
// from the request, so every path and log line built from it is one of ours; false for none.
func settingsSectionName(seg string) (string, bool) {
	if seg == SettingsAll {
		return SettingsAll, true
	}
	for _, s := range settingsSections {
		if s.Name == seg {
			return s.Name, true
		}
	}
	return "", false
}

// settingsTitle is the heading of section, or "" for no such editable section.
func settingsTitle(section string) string {
	for _, s := range settingsSections {
		if s.Name == section {
			return s.Title
		}
	}
	return ""
}

// leadPrioritiesMoved is GET /leads/priorities: the priorities page is the Settings page's
// fallback order now.
func (s *server) leadPrioritiesMoved(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, leadsSetPath+"#"+SettingsOrder, http.StatusSeeOther)
}

// leadSettings is GET /leads/settings.
func (s *server) leadSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, s.takeFlash(w, r), false, nil)
}

// settingsEdit is a refused save: its section, what was submitted (nil for a reset) and
// the problems.
type settingsEdit struct {
	Section  string
	Values   *LeadSettingsValues
	Problems []string
}

func (s *server) renderSettings(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool, edit *settingsEdit) {
	p, err := s.b.LeadSettings(r.Context())
	if s.leadsRead(w, r, code, err) {
		return
	}
	p.ReadOnly.Env = slices.DeleteFunc(slices.Clone(p.ReadOnly.Env), func(e EnvSetting) bool {
		return !slices.Contains(EnvAllowlist, e.Name)
	})
	v := settingsView{P: p, Sections: settingsSections, BatchSources: batchSources, EnvHelp: envHelp, Message: msg, Error: isErr}
	if edit != nil {
		v.Section, v.Problems = edit.Section, edit.Problems
		v.SectionTitle = cmp.Or(settingsTitle(edit.Section), "Reset everything")
		if edit.Values != nil {
			overlaySettings(&v.P.Values, edit.Section, *edit.Values)
		}
	}
	s.render(w, r, code, "leads_settings", page{Title: "Lead settings", Active: leadsPath, SignedIn: true, Data: v})
}

// saveLeadSettings is POST /leads/settings/{section}: do=save saves the section, do=reset
// puts it back to the file, and in Lead kinds do=up:<kind> or do=down:<kind> moves a kind
// one place and saves the section. /leads/settings/all takes do=reset only. Done, it goes
// back to the section with the outcome in the one-time cookie; a refusal shows the page
// with the section as submitted and every problem (422).
func (s *server) saveLeadSettings(w http.ResponseWriter, r *http.Request) {
	section, ok := settingsSectionName(r.PathValue("section"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	logName := "settings-" + section
	do := r.PostForm.Get("do")
	refuse := func(msg string) {
		s.leadsLog(logName, "refused")
		s.renderSettings(w, r, http.StatusUnprocessableEntity, msg, true, nil)
	}
	reset := do == "reset"
	var v LeadSettingsValues
	switch {
	case reset:
	case section == SettingsAll:
		refuse("Everything can only be reset to the file; nothing was changed.")
		return
	case do == "save":
		v = settingsValues(section, r.PostForm)
	case section == SettingsKinds && (strings.HasPrefix(do, "up:") || strings.HasPrefix(do, "down:")):
		v = settingsValues(section, r.PostForm)
		dir, kind, _ := strings.Cut(do, ":")
		if !moveKind(v.Kinds, kind, dir == "up") {
			refuse("That lead kind cannot move that way; nothing was saved.")
			return
		}
	default:
		refuse("Choose save or reset; nothing was changed.")
		return
	}

	err := s.b.SaveLeadSettings(r.Context(), section, v, reset)
	switch {
	case errors.Is(err, ErrLeadsNotSetUp):
		s.leadsLog(logName, "not set up")
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Leads", Active: leadsPath, SignedIn: true})
		return
	case errors.Is(err, ErrUserFixable):
		s.leadsLog(logName, "refused")
		edit := &settingsEdit{Section: section, Problems: settingsProblems(err)}
		if !reset {
			edit.Values = &v
		}
		s.renderSettings(w, r, http.StatusUnprocessableEntity, "", false, edit)
		return
	case err != nil:
		s.leadsLog(logName, "failed")
		s.leadsFail(w, r, err)
		return
	}
	s.leadsLog(logName, "ok")
	next := leadsSetPath + "#" + section
	msg := settingsTitle(section) + " saved."
	switch {
	case section == SettingsAll:
		next, msg = leadsSetPath, "Every setting is back to the file."
	case reset:
		msg = settingsTitle(section) + " is back to the file."
	}
	if section == SettingsWho || section == SettingsAll {
		msg += " A change applies to the queue straight away and to the next new batch; batches already made keep their leads."
	}
	s.setFlash(w, msg)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// settingsProblems are a refusal's problems: a SettingsRefused's list, or the words of any
// other ErrUserFixable as one.
func settingsProblems(err error) []string {
	var sr *SettingsRefused
	if errors.As(err, &sr) && len(sr.Problems) > 0 {
		return sr.Problems
	}
	return []string{refusalWords(err)}
}

// settingsValues reads one section's form. Text is trimmed and kept as typed: the backend
// checks it.
func settingsValues(section string, f url.Values) LeadSettingsValues {
	var v LeadSettingsValues
	switch section {
	case SettingsWho:
		wanted := settingsList(f["wanted"])
		for _, code := range settingsList(f["country"]) {
			v.Countries = append(v.Countries, CountryToggle{Code: code, Wanted: slices.Contains(wanted, code)})
		}
		v.NeverContact = strings.TrimSpace(f.Get("never_contact"))
	case SettingsKinds:
		on := settingsList(f["on"])
		for _, k := range settingsList(f["kind"]) {
			v.Kinds = append(v.Kinds, KindToggle{Name: k, On: slices.Contains(on, k)})
		}
	case SettingsSmallSites:
		v.SkipRankOver = strings.TrimSpace(f.Get("skip_rank_over"))
	case SettingsBatches:
		v.BatchesFrom = strings.TrimSpace(f.Get("batches_from"))
	case SettingsDates:
		for _, name := range dateNames {
			v.Dates = append(v.Dates, DateSetting{Name: name, Value: strings.TrimSpace(f.Get(name))})
		}
	case SettingsRelevance:
		v.Rating = f.Get("rating") == "on"
		v.LeadsCap = strings.TrimSpace(f.Get("leads_cap"))
	case SettingsOrder:
		v.Order = priorityForm(f)
	}
	return v
}

// settingsList is a repeatable field's values: trimmed, empty ones and repeats dropped,
// capped at maxSettingsValues.
func settingsList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) && len(out) < maxSettingsValues {
			out = append(out, s)
		}
	}
	return out
}

// moveKind moves kind one place up or down kinds, and reports false when kind is not there
// or is already at that end.
func moveKind(kinds []KindToggle, kind string, up bool) bool {
	i := slices.IndexFunc(kinds, func(k KindToggle) bool { return k.Name == kind })
	j := i + 1
	if up {
		j = i - 1
	}
	if i < 0 || j < 0 || j >= len(kinds) {
		return false
	}
	kinds[i], kinds[j] = kinds[j], kinds[i]
	return true
}

// overlaySettings puts a refused section's submitted values over the values in force, so
// the page shows what was typed; the words (labels) stay the backend's.
func overlaySettings(dst *LeadSettingsValues, section string, src LeadSettingsValues) {
	switch section {
	case SettingsWho:
		for i := range dst.Countries {
			dst.Countries[i].Wanted = slices.ContainsFunc(src.Countries, func(c CountryToggle) bool {
				return c.Code == dst.Countries[i].Code && c.Wanted
			})
		}
		dst.NeverContact = src.NeverContact
	case SettingsKinds:
		var kinds []KindToggle
		for _, k := range src.Kinds {
			if i := slices.IndexFunc(dst.Kinds, func(d KindToggle) bool { return d.Name == k.Name }); i >= 0 {
				kinds = append(kinds, KindToggle{Name: k.Name, Label: dst.Kinds[i].Label, On: k.On})
			}
		}
		for _, d := range dst.Kinds {
			if !slices.ContainsFunc(kinds, func(k KindToggle) bool { return k.Name == d.Name }) {
				kinds = append(kinds, d)
			}
		}
		dst.Kinds = kinds
	case SettingsSmallSites:
		dst.SkipRankOver = src.SkipRankOver
	case SettingsBatches:
		dst.BatchesFrom = src.BatchesFrom
	case SettingsDates:
		for i := range dst.Dates {
			if j := slices.IndexFunc(src.Dates, func(d DateSetting) bool { return d.Name == dst.Dates[i].Name }); j >= 0 {
				dst.Dates[i].Value = src.Dates[j].Value
			}
		}
	case SettingsRelevance:
		dst.Rating, dst.LeadsCap = src.Rating, src.LeadsCap
	case SettingsOrder:
		for i := range dst.Order.Points {
			if j := slices.IndexFunc(src.Order.Points, func(p PointField) bool { return p.Key == dst.Order.Points[i].Key }); j >= 0 {
				dst.Order.Points[i].Value = src.Order.Points[j].Value
			}
		}
		dst.Order.ExactVersion, dst.Order.Countries, dst.Order.RankBands = src.Order.ExactVersion, src.Order.Countries, src.Order.RankBands
	}
}

// changedWords says when a value was changed on this page, after what it names ("" for
// the whole value): "Changed here on 24 October 2026", "Order changed here on …". A value
// from the file says nothing, so it is "".
func changedWords(what string, src SettingSource, loc *time.Location) string {
	if !src.Console {
		return ""
	}
	words := "Changed here"
	if what != "" {
		words = what + " changed here"
	}
	if src.At.IsZero() {
		return words + "."
	}
	return words + " on " + src.At.In(loc).Format("2 January 2006") + "."
}
