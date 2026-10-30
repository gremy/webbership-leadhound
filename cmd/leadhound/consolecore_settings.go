package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// The Leads Settings page's side of the backend, and the core `leads settings` shares with
// it (D10, D11). Settings sit on top of leads.yaml: the knobs every reader takes through the
// settings door are saved as a leadconfig.Overrides document in lead_setting, and the two
// that belong to the queue's profile (never-contact countries and the fallback order) as a
// priority_profile version. Nothing here reads or writes a secret: the sender's operator
// addresses, a DSN, the password hash, a key or a mailbox password never reach a page, a
// line or a saved row.

// settingsLockKey is the transaction-level advisory lock every settings write holds from its
// read of the latest saved settings to its row, so two saves (two console tabs, or the console
// and the CLI) never merge onto the same version and lose one another.
const settingsLockKey int64 = 0x4c48_5345_5453 // "LHSETS"

// The relevance trust gate (D9), in the words the Settings page uses: leadrate's gate,
// the one an eval's Trusted applies.
const (
	trustMinLeads     = leadrate.TrustMinN
	trustMinWithinOne = int(leadrate.TrustMinWithinOne * 100) // per cent
	trustMinSpearman  = leadrate.TrustMinSpearman
)

// overrideSections are the Settings sections saved in lead_setting, each with the
// Overrides keys it owns. Who to contact and the fallback order are the priority profile's.
var overrideSections = map[string][]string{
	console.SettingsKinds:      {"kinds_priority", "kinds_off"},
	console.SettingsSmallSites: {"skip_rank_over"},
	console.SettingsBatches:    {"batches_from"},
	console.SettingsDates:      {"facts.d6_eol", "facts.d7_eol", "facts.d8_eol", "facts.d9_eol", "facts.d10_eol"},
	console.SettingsRelevance:  {"relevance.rating", "relevance.leads_daily_calls"},
	console.SettingsEmailCheck: {"verify.catch_all"},
	console.SettingsWindow:     {"schedule.business_hours"},
}

// eolKeys are the end-of-life dates, in the page's order, with the Drupal major each is.
var eolKeys = []struct {
	name  string // the form field and the setting both
	major int
}{{"d6_eol", 6}, {"d7_eol", 7}, {"d8_eol", 8}, {"d9_eol", 9}, {"d10_eol", 10}}

// overrideKeys are the keys o sets, as leads.yaml names them ("facts.d6_eol").
func overrideKeys(o leadconfig.Overrides) map[string]bool {
	k := map[string]bool{
		"kinds_off": o.KindsOff != nil, "kinds_priority": o.KindsPriority != nil,
		"skip_rank_over": o.SkipRankOver != nil, "batches_from": o.BatchesFrom != nil,
	}
	if f := o.Facts; f != nil {
		k["facts.d6_eol"], k["facts.d7_eol"], k["facts.d8_eol"] = f.D6EOL != nil, f.D7EOL != nil, f.D8EOL != nil
		k["facts.d9_eol"], k["facts.d10_eol"] = f.D9EOL != nil, f.D10EOL != nil
	}
	if r := o.Relevance; r != nil {
		k["relevance.rating"], k["relevance.leads_daily_calls"] = r.Rating != nil, r.LeadsDailyCalls != nil
	}
	if v := o.Verify; v != nil {
		k["verify.catch_all"] = v.CatchAll != nil
	}
	if s := o.Schedule; s != nil {
		k["schedule.business_hours"] = s.BusinessHours != nil
	}
	return k
}

// queueFallback is the profile the queue falls back on when no version is saved, as
// CandidateLeads reads it: the file's priority profile, or kind points from kinds_priority.
func queueFallback(cfg leadconfig.Config) leadconfig.Profile {
	p := cfg.PriorityProfile()
	if p.IsZero() {
		p = leadconfig.Profile{Kinds: leadconfig.KindPoints(cfg.KindsPriority)}
	}
	return p
}

// sameOrder reports whether a and b give the same points: every rule but the never-contact
// countries.
func sameOrder(a, b leadconfig.Profile) bool {
	return maps.Equal(a.Kinds, b.Kinds) && maps.Equal(a.Majors, b.Majors) && maps.Equal(a.Countries, b.Countries) &&
		maps.Equal(a.Channels, b.Channels) && slices.Equal(a.RankBands, b.RankBands) && a.ExactVersion == b.ExactVersion
}

// sameExcluded reports whether a and b hold the same never-contact countries, in any order.
func sameExcluded(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// settingsState is the settings in force and where each comes from: the effective config
// (setup.leads, which carries the saved lead_setting row) and the queue's profile.
type settingsState struct {
	setup        leadsSetup
	fallback     leadconfig.Profile // the queue's profile when none is saved (queueFallback)
	profile      leadconfig.Profile // the profile in force
	profileSaved bool               // the latest priority_profile row is a saved version
	profileAt    time.Time          // when that row was saved; zero with none
}

// readSettingsState reads the profile in force over setup (an effective config from
// leadsOpen or backend.leadsSetup).
func readSettingsState(ctx context.Context, db store.DBTX, setup leadsSetup) (settingsState, error) {
	st := settingsState{setup: setup, fallback: queueFallback(setup.leads)}
	var err error
	if st.profile, st.profileSaved, err = leadstore.NewQueries(db).EffectiveProfile(ctx, st.fallback); err != nil {
		return settingsState{}, err
	}
	if st.profileSaved {
		if err := db.QueryRow(ctx, `SELECT at FROM priority_profile ORDER BY id DESC LIMIT 1`).Scan(&st.profileAt); err != nil {
			return settingsState{}, fmt.Errorf("settings: latest profile: %w", err)
		}
	}
	return st, nil
}

// knob is where the saved-settings key comes from: the console, when the applied row sets
// it; else the file.
func (st settingsState) knob(key string) console.SettingSource {
	if st.setup.settings.Applied && overrideKeys(st.setup.settings.Overrides)[key] {
		return console.SettingSource{Console: true, At: st.setup.settings.At}
	}
	return console.SettingSource{}
}

// anyKnob is the console when any of keys is the console's.
func (st settingsState) anyKnob(keys ...string) console.SettingSource {
	for _, k := range keys {
		if s := st.knob(k); s.Console {
			return s
		}
	}
	return console.SettingSource{}
}

// whoSource is where the never-contact countries come from: the console, when a saved
// profile excludes other countries than the file does.
func (st settingsState) whoSource() console.SettingSource {
	if st.profileSaved && !sameExcluded(st.profile.ExcludeCountries, st.fallback.ExcludeCountries) {
		return console.SettingSource{Console: true, At: st.profileAt}
	}
	return console.SettingSource{}
}

// orderSource is where the fallback order's points come from.
func (st settingsState) orderSource() console.SettingSource {
	if st.profileSaved && !sameOrder(st.profile, st.fallback) {
		return console.SettingSource{Console: true, At: st.profileAt}
	}
	return console.SettingSource{}
}

// ignoredNote is the plain line about a saved settings row leads.yaml no longer accepts, or
// "". The problems name only the console's own knobs, never a secret.
func (st settingsState) ignoredNote(loc *time.Location) string {
	ig := st.setup.settings.Ignored
	if len(ig) == 0 {
		return ""
	}
	words := make([]string, len(ig))
	for i, p := range ig {
		words[i] = p.Message
	}
	return fmt.Sprintf("The change saved in the console on %s no longer passes the checks leads.yaml passes, so the file's values apply (%s). The next save or reset replaces it.",
		st.setup.settings.At.In(loc).Format("2 January 2006"), strings.Join(words, "; "))
}

// kindOrderNote says when the fallback order's kind points, not kinds_priority, decide which
// kind comes first, or "".
func (st settingsState) kindOrderNote() string {
	if maps.Equal(st.profile.Kinds, leadconfig.KindPoints(st.setup.leads.KindsPriority)) {
		return ""
	}
	return "The fallback order gives each lead kind its own points, and those points, not the order under Lead kinds, decide which kind a batch draws first."
}

// settingsSetup is the setup the Settings page reads: the whole of leads.yaml checked as
// drafting checks it, so a save can be checked against it (D10). A file that passes only the
// probe checks still shows, with a note that nothing can be saved; one that passes neither
// is ErrLeadsNotSetUp.
func (b *backend) settingsSetup(ctx context.Context) (leadsSetup, string, error) {
	setup, err := b.leadsSetup(ctx, leadsForOutreach)
	if err == nil {
		return setup, "", nil
	}
	if !errors.Is(err, console.ErrLeadsNotSetUp) {
		return leadsSetup{}, "", err
	}
	if setup, err = b.leadsSetup(ctx, leadsForProbe); err != nil {
		return leadsSetup{}, "", err
	}
	return setup, settingsFileNotReady, nil
}

// settingsFileNotReady is the note and the refusal when leads.yaml passes only the probe
// checks: a save is checked against all of it.
const settingsFileNotReady = "The leads settings file does not pass every check drafting needs (its templates, say), so no change can be saved here until it does."

// LeadSettings is the Settings page: the values in force with where each comes from, the
// trust gate in words, and what is shown read-only. It never writes.
func (b *backend) LeadSettings(ctx context.Context) (console.LeadSettingsPage, error) {
	setup, fileNote, err := b.settingsSetup(ctx)
	if err != nil {
		return console.LeadSettingsPage{}, err
	}
	st, err := readSettingsState(ctx, b.s.Pool(), setup)
	if err != nil {
		return console.LeadSettingsPage{}, err
	}
	kinds, err := leadKinds(ctx, b, setup.leads)
	if err != nil {
		return console.LeadSettingsPage{}, err
	}
	eval, evalOK, err := b.lq().LatestRelevanceEval(ctx, relevanceQuestion)
	if err != nil {
		return console.LeadSettingsPage{}, err
	}
	labelled, err := b.lq().LabelCount(ctx)
	if err != nil {
		return console.LeadSettingsPage{}, err
	}
	cfg := setup.leads
	p := console.LeadSettingsPage{
		Values:    settingsValues(st, kinds),
		ServerCap: setup.jevLeadsEnvCap,
		TrustGate: trustGateWords(eval, evalOK, b.loc),
		Labelled:  labelled,
		LabelGoal: labelGoal,
		ReadOnly:  readOnlySettings(cfg, kinds, windowSourceWords(st.knob("schedule.business_hours"), b.loc)),
		Sources: console.SettingsSources{
			Who: st.whoSource(), KindsOn: st.knob("kinds_off"), KindsOrder: st.knob("kinds_priority"),
			SkipRankOver: st.knob("skip_rank_over"), BatchesFrom: st.knob("batches_from"),
			Rating: st.knob("relevance.rating"), LeadsCap: st.knob("relevance.leads_daily_calls"),
			CatchAll: st.knob("verify.catch_all"), Order: st.orderSource(),
		},
	}
	for _, n := range []string{fileNote, st.ignoredNote(b.loc), st.kindOrderNote()} {
		if n != "" {
			p.Notes = append(p.Notes, n)
		}
	}
	return p, nil
}

// settingsValues are the values in force as the page's forms hold them.
func settingsValues(st settingsState, kinds []leadKind) console.LeadSettingsValues {
	cfg := st.setup.leads
	var v console.LeadSettingsValues
	codes := slices.Sorted(maps.Keys(cfg.Countries))
	for _, code := range codes {
		v.Countries = append(v.Countries, console.CountryToggle{Code: code, Label: countryName(code),
			Wanted: !slices.Contains(st.profile.ExcludeCountries, code)})
	}
	var others []string
	for _, c := range st.profile.ExcludeCountries {
		if !slices.Contains(codes, c) {
			others = append(others, c)
		}
	}
	v.NeverContact = strings.Join(others, ", ")
	for _, k := range kinds {
		if k.Outreach && slices.Contains(leadconfig.Kinds, k.Name) {
			v.Kinds = append(v.Kinds, console.KindToggle{Name: k.Name, Label: k.Label, On: !cfg.KindOff(k.Name)})
		}
	}
	v.SkipRankOver = strconv.Itoa(cfg.SkipRankOver)
	v.BatchesFrom = cfg.BatchesFrom()
	for _, d := range eolKeys {
		date, _ := cfg.Facts.EOL(d.major)
		val := ""
		if !date.IsZero() {
			val = date.Format(time.DateOnly)
		}
		v.Dates = append(v.Dates, console.DateSetting{Name: d.name, Label: fmt.Sprintf("Drupal %d end of life", d.major), Value: val,
			Source: st.knob("facts." + d.name)})
	}
	v.Rating = cfg.Relevance.RatingOn()
	v.LeadsCap = strconv.Itoa(cfg.Relevance.LeadsDailyCalls)
	v.CatchAll = cfg.Verify.CatchAllRule()
	v.Order = profileForm(st.profile)
	v.Order.Exclude = "" // never-contact countries are Who to contact's
	return v
}

// trustGateWords say whether the list and the queue sort by relevance yet and what the
// latest check read, the rule in plain words, and the rule's exact thresholds for the fold.
// How many leads carry a call is the page's own line, so it is not said here.
func trustGateWords(e leadstore.RelevanceEval, ok bool, loc *time.Location) console.TrustGate {
	g := console.TrustGate{
		Rule: fmt.Sprintf("Sorting by relevance starts once Jev agrees with your calls on at least %d leads: close to your call on %d in 10 of them, and ranking them in roughly the same order.",
			trustMinLeads, trustMinWithinOne/10),
		Exact: fmt.Sprintf("In exact terms: a check on at least %d labelled leads, where Jev's band is your call or the one next to it on %d%% or more of them, and the two orders agree with a rank correlation (Spearman) of %.1f or more, where 1 is the same order.",
			trustMinLeads, trustMinWithinOne, trustMinSpearman),
	}
	if !ok {
		g.Status = []string{"For now the list and the queue sort leads that are not starred by points: Jev's numbers have not been checked against your calls yet."}
		return g
	}
	measured := fmt.Sprintf("The latest check, on %s, read %d of your calls: Jev was close to your call on %.0f%% of them, and its order matched yours at %.2f.",
		e.At.In(loc).Format("2 January 2006"), e.N, e.WithinOne*100, e.Spearman)
	if e.Trusted {
		g.Status = []string{"The list and the queue sort leads that are not starred by relevance.", measured}
		return g
	}
	g.Status = []string{"For now the list and the queue sort leads that are not starred by points: Jev has not passed its check yet.", measured}
	return g
}

// readOnlySettings are the settings the page shows and never changes (D11): each country's
// policy, the sending pace, the sender's public lines, which languages have an opt-out line,
// the templates by name and the allowlisted environment. The sender's operator addresses
// and every other secret stay out.
func readOnlySettings(cfg leadconfig.Config, kinds []leadKind, windowSource string) console.ReadOnlySettings {
	ro := console.ReadOnlySettings{
		Pace:            paceLines(cfg, windowSource),
		SenderName:      cfg.Sender.Name,
		IdentityLine:    cfg.Sender.IdentityLine,
		SiteURL:         cfg.Sender.SiteURL,
		OptOutLanguages: optOutLanguages(cfg.Sender.OptOutLines),
		Templates:       templateNames(cfg.Templates, kinds),
	}
	for _, code := range slices.Sorted(maps.Keys(cfg.Countries)) {
		c := cfg.Countries[code]
		lang := ""
		if c.Lang != "" {
			lang = languageName(c.Lang)
		}
		ro.Countries = append(ro.Countries, console.CountryPolicy{Code: code, Label: countryName(code), Channel: c.Channel,
			Language: lang, Timezone: c.TZ, Footer: c.Footer})
	}
	for _, name := range console.EnvAllowlist {
		ro.Env = append(ro.Env, console.EnvSetting{Name: name, Value: os.Getenv(name)})
	}
	return ro
}

// optOutLanguages names the languages that have an opt-out line, by name ("English"), in
// the order of their codes. A code with no name is shown as it is.
func optOutLanguages(lines map[string]string) []string {
	codes := slices.Sorted(maps.Keys(lines))
	out := make([]string, len(codes))
	for i, c := range codes {
		out[i] = languageName(c)
	}
	return out
}

// windowSourceWords says where the sending window in force comes from, for a line that names
// it: set on the Controls page (and when), or the leads settings file.
func windowSourceWords(src console.SettingSource, loc *time.Location) string {
	if src.Console {
		return "set on the Controls page on " + src.At.In(loc).Format("2 January 2006")
	}
	return "from the leads settings file"
}

// paceLines are the sending pace and the warm-up ramp, one plain line each; the sending
// window's line says where it comes from (windowSource, windowSourceWords).
func paceLines(cfg leadconfig.Config, windowSource string) []string {
	c := cfg.Caps
	var out []string
	if c.BatchSize > 0 {
		out = append(out, fmt.Sprintf("A batch holds at most %d leads.", c.BatchSize))
	}
	switch {
	case c.StartDate.IsFirstSend():
		out = append(out, "The warm-up ramp counts from each mailbox's own first send.")
	case !c.StartDate.IsZero():
		out = append(out, "The warm-up ramp counts from "+c.StartDate.Format("2 January 2006")+".")
	}
	for _, r := range c.Ramp {
		out = append(out, fmt.Sprintf("From day %d: up to %d emails a mailbox a day.", r.FromDay, r.PerDay))
	}
	out = append(out, fmt.Sprintf("At most %d emails a sending domain a day, and %d mailboxes a sending domain.", c.DomainPerDay, c.MailboxesPerDomain))
	if bh := cfg.SendingWindow(); bh.Start != "" {
		out = append(out, fmt.Sprintf("Emails go in each recipient's business hours, %s to %s on %s (%s; change it on the Controls page).",
			bh.Start, bh.End, strings.Join(bh.Days, ", "), windowSource))
	}
	if sm := cfg.Schedule.SpacingMinutes; len(sm) == 2 {
		out = append(out, fmt.Sprintf("Sends are %d to %d minutes apart.", sm[0], sm[1]))
	}
	if fb := cfg.Schedule.FollowupBusinessDays; len(fb) > 0 {
		days := make([]string, len(fb))
		for i, d := range fb {
			days[i] = strconv.Itoa(d)
		}
		out = append(out, "Follow-ups go "+strings.Join(days, " and ")+" business days after the email before.")
	}
	if bp := cfg.Health.BouncePause; bp.Bounces > 0 {
		out = append(out, fmt.Sprintf("A mailbox pauses itself at %d hard bounces in its last %d sends.", bp.Bounces, bp.WithinSends))
	}
	return out
}

// templateNames are the templates in use by name: each kind and language with the messages
// it has, and each footer. A template's text is never read here.
func templateNames(t leadconfig.Templates, kinds []leadKind) []string {
	keys := slices.SortedFunc(maps.Keys(t.Sets), func(a, b leadconfig.SetKey) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Lang, b.Lang))
	})
	var out []string
	for _, k := range keys {
		set := t.Sets[k]
		var parts []string
		if n := len(set.Steps); n > 0 {
			parts = append(parts, fmt.Sprintf("emails 1 to %d", n))
		}
		if set.Form != nil {
			parts = append(parts, "contact form")
		}
		if set.LinkedIn != nil {
			parts = append(parts, "LinkedIn note")
		}
		out = append(out, fmt.Sprintf("%s in %s: %s", kindLabel(kinds, k.Kind), languageName(k.Lang), strings.Join(parts, ", ")))
	}
	for _, f := range slices.Sorted(maps.Keys(t.Footers)) {
		out = append(out, "Footer "+f)
	}
	return out
}

// SaveLeadSettings saves or resets one section of the Settings page, checked against the
// whole of leads.yaml (D10).
func (b *backend) SaveLeadSettings(ctx context.Context, section string, v console.LeadSettingsValues, reset bool) error {
	setup, fileNote, err := b.settingsSetup(ctx)
	if err != nil {
		return err
	}
	if fileNote != "" {
		return &console.SettingsRefused{Problems: []string{fileNote}}
	}
	_, err = saveLeadSettings(ctx, b.s, setup, section, v, reset)
	return err
}

// saveLeadSettings is one save or reset of a Settings section (a console.Settings* name),
// the console's and `leads settings reset`'s alike, in one transaction under
// settingsLockKey. A lead_setting section merges onto the latest saved settings (the
// others' keys kept; a latest row leads.yaml no longer accepts is dropped, as it is
// ignored), and the merged document must pass the checks leads.yaml passes against setup.file
// before it is saved by the operator; a reset drops the section's keys, and a document left
// with none is a reset row. Who to contact and the fallback order are versions of the
// priority profile, each keeping the other's rules; a profile that gives the file's order is
// a reset. SettingsAll takes a reset only: the settings and the profile both. It reports
// whether a row was written; a refusal is a *console.SettingsRefused.
func saveLeadSettings(ctx context.Context, s *store.Store, setup leadsSetup, section string, v console.LeadSettingsValues, reset bool) (bool, error) {
	tx, err := s.Pool().Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("settings: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, settingsLockKey); err != nil {
		return false, fmt.Errorf("settings: lock: %w", err)
	}
	q := leadstore.NewQueries(tx)
	var wrote bool
	switch {
	case section == console.SettingsAll:
		if !reset {
			return false, &console.SettingsRefused{Problems: []string{"Everything can only be reset to the file."}}
		}
		wrote, err = resetAllSettings(ctx, q, setup)
	case section == console.SettingsWho || section == console.SettingsOrder:
		wrote, err = saveProfileSection(ctx, q, setup, section, v, reset)
	case overrideSections[section] != nil:
		wrote, err = saveOverrideSection(ctx, q, setup, section, v, reset)
	default:
		return false, &console.SettingsRefused{Problems: []string{fmt.Sprintf("%q is not a settings section.", section)}}
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("settings: commit: %w", err)
	}
	return wrote, nil
}

// resetAllSettings puts every setting back to the file: a reset row for the settings and for
// the profile, each only when something is saved.
func resetAllSettings(ctx context.Context, q leadstore.Queries, setup leadsSetup) (bool, error) {
	_, _, _, saved, err := q.LatestSettings(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return false, err
	}
	wrote := false
	if saved {
		if _, err := q.ResetSettings(ctx, leadstore.DefaultAccountID, leadstore.ActorOperator); err != nil {
			return false, err
		}
		wrote = true
	}
	_, profileSaved, err := q.EffectiveProfile(ctx, queueFallback(setup.leads))
	if err != nil {
		return false, err
	}
	if profileSaved {
		if _, err := q.ResetProfile(ctx); err != nil {
			return false, err
		}
		wrote = true
	}
	return wrote, nil
}

// saveOverrideSection saves or resets one lead_setting section.
func saveOverrideSection(ctx context.Context, q leadstore.Queries, setup leadsSetup, section string, v console.LeadSettingsValues, reset bool) (bool, error) {
	raw, _, _, saved, err := q.LatestSettings(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return false, err
	}
	var base leadconfig.Overrides
	if saved {
		if _, stale := effectiveLeads(raw, setup.file); len(stale) == 0 {
			if base, err = leadconfig.DecodeOverrides(raw); err != nil {
				return false, err
			}
		}
	}
	if reset {
		clearSection(&base, section)
	} else if problems := setSection(&base, section, v); len(problems) > 0 {
		return false, &console.SettingsRefused{Problems: problems}
	}
	if base == (leadconfig.Overrides{}) {
		if !saved {
			return false, nil
		}
		_, err := q.ResetSettings(ctx, leadstore.DefaultAccountID, leadstore.ActorOperator)
		return err == nil, err
	}
	doc, err := json.Marshal(base)
	if err != nil {
		return false, fmt.Errorf("settings: encode: %w", err)
	}
	eff, problems := effectiveLeads(doc, setup.file)
	if len(problems) == 0 && section == console.SettingsRelevance && !reset {
		problems = eff.LeadsCapProblems(setup.jevLeadsEnvCap)
	}
	if len(problems) > 0 {
		words := make([]string, len(problems))
		for i, p := range problems {
			words[i] = p.Message
		}
		return false, &console.SettingsRefused{Problems: words}
	}
	if _, err := q.SaveSettings(ctx, leadstore.DefaultAccountID, doc, leadstore.ActorOperator); err != nil {
		return false, err
	}
	return true, nil
}

// clearSection drops section's keys from o.
func clearSection(o *leadconfig.Overrides, section string) {
	switch section {
	case console.SettingsKinds:
		o.KindsOff, o.KindsPriority = nil, nil
	case console.SettingsSmallSites:
		o.SkipRankOver = nil
	case console.SettingsBatches:
		o.BatchesFrom = nil
	case console.SettingsDates:
		o.Facts = nil
	case console.SettingsRelevance:
		o.Relevance = nil
	case console.SettingsEmailCheck:
		o.Verify = nil
	case console.SettingsWindow:
		o.Schedule = nil
	}
}

// setSection puts v's values of section into o and lists what could not be read; the rest
// is checked with leads.yaml's own checks once merged.
func setSection(o *leadconfig.Overrides, section string, v console.LeadSettingsValues) []string {
	whole := func(s string) (int, bool) {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		return n, err == nil
	}
	switch section {
	case console.SettingsKinds:
		order, off := []string{}, []string{}
		for _, k := range v.Kinds {
			order = append(order, k.Name)
			if !k.On {
				off = append(off, k.Name)
			}
		}
		o.KindsPriority, o.KindsOff = &order, &off
	case console.SettingsSmallSites:
		n, ok := whole(v.SkipRankOver)
		if !ok {
			return []string{"Skip sites ranked below takes a whole number, 0 for off."}
		}
		o.SkipRankOver = &n
	case console.SettingsBatches:
		from := strings.TrimSpace(v.BatchesFrom)
		o.BatchesFrom = &from
	case console.SettingsDates:
		f := &leadconfig.FactsOverrides{}
		for _, d := range v.Dates {
			date := leadconfig.ParseDate(strings.TrimSpace(d.Value))
			switch d.Name {
			case "d6_eol":
				f.D6EOL = &date
			case "d7_eol":
				f.D7EOL = &date
			case "d8_eol":
				f.D8EOL = &date
			case "d9_eol":
				f.D9EOL = &date
			case "d10_eol":
				f.D10EOL = &date
			default:
				return []string{"That is not a date the console can change."}
			}
		}
		if *f == (leadconfig.FactsOverrides{}) {
			f = nil
		}
		o.Facts = f
	case console.SettingsRelevance:
		n, ok := whole(v.LeadsCap)
		if !ok {
			return []string{"Most ratings a day takes a whole number, 0 for the server's cap."}
		}
		rating := v.Rating
		o.Relevance = &leadconfig.RelevanceOverrides{Rating: &rating, LeadsDailyCalls: &n}
	case console.SettingsEmailCheck:
		rule := strings.TrimSpace(v.CatchAll)
		o.Verify = &leadconfig.VerifyOverrides{CatchAll: &rule}
	case console.SettingsWindow:
		bh, problems := windowFromForm(v)
		if len(problems) > 0 {
			return problems
		}
		o.Schedule = &leadconfig.ScheduleOverrides{BusinessHours: &bh}
	}
	return nil
}

// windowFromForm is the sending window the Controls form asks for, or what is wrong with it
// in plain words: the rules leads.yaml's schedule.business_hours passes (HH:MM, the end after
// the start, at least one day, each once), the days put in week order.
func windowFromForm(v console.LeadSettingsValues) (leadconfig.BusinessHours, []string) {
	start, end := strings.TrimSpace(v.WindowStart), strings.TrimSpace(v.WindowEnd)
	var problems []string
	ts, errS := time.Parse("15:04", start)
	te, errE := time.Parse("15:04", end)
	switch {
	case errS != nil || errE != nil:
		problems = append(problems, "The start and the end each take a time like 08:30.")
	case !te.After(ts):
		problems = append(problems, "The end must come after the start.")
	}
	var days []string
	for _, d := range leadconfig.Weekdays() {
		if slices.Contains(v.WindowDays, d) {
			days = append(days, d)
		}
	}
	for _, d := range v.WindowDays {
		if !slices.Contains(days, d) {
			problems = append(problems, fmt.Sprintf("%q is not a day of the week.", d))
			break
		}
	}
	if len(days) == 0 {
		problems = append(problems, "Choose at least one day.")
	}
	return leadconfig.BusinessHours{Start: start, End: end, Days: days}, problems
}

// saveProfileSection saves or resets Who to contact or the fallback order as a new version
// of the priority profile, keeping the other section's rules. A profile that gives the file's
// order and never-contact countries is a reset row (or nothing, when none is saved); kind
// points equal to the file's are saved as none, so the queue keeps taking them from the file
// (kinds_priority included).
func saveProfileSection(ctx context.Context, q leadstore.Queries, setup leadsSetup, section string, v console.LeadSettingsValues, reset bool) (bool, error) {
	fallback := queueFallback(setup.leads)
	cur, saved, err := q.EffectiveProfile(ctx, fallback)
	if err != nil {
		return false, err
	}
	var next leadconfig.Profile
	var problems []string
	switch {
	case reset && section == console.SettingsWho:
		next = cur.Clone()
		next.ExcludeCountries = slices.Clone(fallback.ExcludeCountries)
	case reset:
		next = fallback.Clone()
		next.ExcludeCountries = slices.Clone(cur.ExcludeCountries)
	case section == console.SettingsWho:
		next = cur.Clone()
		next.ExcludeCountries, problems = neverContact(setup.leads, cur.ExcludeCountries, v)
	default:
		form := v.Order
		form.Exclude = ""
		next, problems = parseProfileForm(form)
		next.ExcludeCountries = slices.Clone(cur.ExcludeCountries)
	}
	if len(problems) == 0 {
		problems = next.Problems()
	}
	if len(problems) > 0 {
		return false, &console.SettingsRefused{Problems: problems}
	}
	if sameOrder(next, fallback) && sameExcluded(next.ExcludeCountries, fallback.ExcludeCountries) {
		if !saved {
			return false, nil
		}
		_, err := q.ResetProfile(ctx)
		return err == nil, err
	}
	if maps.Equal(next.Kinds, fallback.Kinds) {
		next.Kinds = nil
	}
	if _, err := q.SaveProfile(ctx, next); err != nil {
		return false, err
	}
	return true, nil
}

// neverContact is the never-contact list Who to contact asks for: the countries leads.yaml
// names that were unticked, a country of the file the form did not carry keeping its place in
// cur, then the other codes typed, in capitals, each once.
func neverContact(cfg leadconfig.Config, cur []string, v console.LeadSettingsValues) ([]string, []string) {
	out := []string{}
	var problems []string
	add := func(code string) {
		if !slices.Contains(out, code) {
			out = append(out, code)
		}
	}
	sent := map[string]bool{}
	for _, c := range v.Countries {
		if _, ok := cfg.Countries[c.Code]; !ok {
			problems = append(problems, fmt.Sprintf("%q is not a country of the leads settings file.", c.Code))
			continue
		}
		sent[c.Code] = true
	}
	for _, code := range slices.Sorted(maps.Keys(cfg.Countries)) {
		i := slices.IndexFunc(v.Countries, func(c console.CountryToggle) bool { return c.Code == code })
		switch {
		case i >= 0 && !v.Countries[i].Wanted, i < 0 && slices.Contains(cur, code):
			add(code)
		}
	}
	for _, f := range strings.FieldsFunc(v.NeverContact, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' }) {
		add(strings.ToUpper(f))
	}
	return out, problems
}
