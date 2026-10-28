//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// orgOf returns lead's org id.
func (f outreachFixture) orgOf(t *testing.T, lead int64) int64 {
	t.Helper()
	var id int64
	if err := f.owner.QueryRow(context.Background(), `SELECT org_id FROM lead WHERE id = $1`, lead).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// setCountry sets lead's org's country (the fixture's .localhost domains have none).
func (f outreachFixture) setCountry(t *testing.T, lead int64, country string) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(), `UPDATE org SET country = $2 WHERE id = $1`, f.orgOf(t, lead), country); err != nil {
		t.Fatal(err)
	}
}

// screen records one screening as the operator.
func (f outreachFixture) screen(t *testing.T, number, register, result string, at time.Time) leadstore.ScreeningRecorded {
	t.Helper()
	r, err := f.q.RecordScreening(context.Background(), 0, leadstore.Screening{
		Number: number, Register: register, Result: result, CheckedAt: at}, "result.csv", leadstore.ActorGremy)
	if err != nil {
		t.Fatalf("record screening %s %s %s: %v", number, register, result, err)
	}
	return r
}

// phoneSuppression returns the reason of the active phone suppression of number's hash; ""
// when there is none.
func (f outreachFixture) phoneSuppression(t *testing.T, number string) string {
	t.Helper()
	var reason string
	err := f.owner.QueryRow(context.Background(),
		`SELECT reason FROM suppression_active WHERE kind = 'phone' AND value = $1 ORDER BY id LIMIT 1`,
		leadstore.PhoneSuppressionValue(number)).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

// TestScreenStatusGrid is the screening status × operation grid: clean leaves the number
// ready to call (no suppression, not due for export); listed suppresses it with the
// register as the reason; clean then listed suppresses it (latest wins); listed then clean
// stays suppressed (only the operator lifts a suppression). A national-form fact is
// suppressed under its own value too, so the batch's phone pick sees it.
func TestScreenStatusGrid(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	const e164, national, nationalE164 = "+441632960101", "01632960102", "+441632960102"
	for _, register := range []string{leadstore.ReasonTPS, leadstore.ReasonCTPS} {
		for _, tc := range []struct {
			name       string
			results    []string // in check order, an hour apart
			latest     string
			suppressed bool
		}{
			{"clean", []string{leadstore.ScreenClean}, leadstore.ScreenClean, false},
			{"listed", []string{leadstore.ScreenListed}, leadstore.ScreenListed, true},
			{"clean then listed", []string{leadstore.ScreenClean, leadstore.ScreenListed}, leadstore.ScreenListed, true},
			{"listed then clean", []string{leadstore.ScreenListed, leadstore.ScreenClean}, leadstore.ScreenClean, true},
		} {
			t.Run(register+" "+tc.name, func(t *testing.T) {
				f := newOutreachFixture(t)
				lead := f.siteLead(t, "screen.localhost", nil, 7, "info@screen.localhost")
				f.setCountry(t, lead, "GB")
				f.phoneFact(t, lead, e164)
				f.phoneFact(t, lead, national)
				for i, res := range tc.results {
					at := now.Add(time.Duration(i-len(tc.results)) * time.Hour)
					f.screen(t, e164, register, res, at)
					f.screen(t, nationalE164, register, res, at)
				}
				for _, n := range []string{e164, nationalE164} {
					s, ok, err := f.q.LatestScreening(ctx, n, register)
					if err != nil || !ok || s.Result != tc.latest || s.Actor != leadstore.ActorOperator || s.Source != "result.csv" {
						t.Errorf("latest %s = %+v %v %v, want %s", n, s, ok, err, tc.latest)
					}
				}
				for _, n := range []string{e164, national, nationalE164} {
					got := f.phoneSuppression(t, n)
					if tc.suppressed && got != register {
						t.Errorf("suppression of %s = %q, want %q", n, got, register)
					}
					if !tc.suppressed && got != "" {
						t.Errorf("suppression of %s = %q, want none", n, got)
					}
				}
				// Callable-ready: a clean, fresh number is not due for export; a suppressed one is
				// never exported either (it is out of reach).
				missing, err := f.q.ScreeningsMissingOrOlder(ctx, 0, register, "GB", now.AddDate(0, 0, -28))
				if err != nil || len(missing) != 0 {
					t.Errorf("missing = %v %v, want none", missing, err)
				}
				set, err := f.q.SuppressionSet(ctx, 0)
				if err != nil {
					t.Fatal(err)
				}
				if got := slices.Contains(set.Phones, leadstore.PhoneSuppressionValue(national)); got != tc.suppressed {
					t.Errorf("suppression set holds the national fact = %v, want %v", got, tc.suppressed)
				}
			})
		}
	}

	t.Run("a register that is not a suppression reason suppresses as manual", func(t *testing.T) {
		f := newOutreachFixture(t)
		r := f.screen(t, "+390212345678", "rpo", leadstore.ScreenListed, now)
		if r.Suppressed != 1 || f.phoneSuppression(t, "+390212345678") != leadstore.ReasonManual {
			t.Errorf("rpo listed: %+v, reason %q; want one manual suppression", r, f.phoneSuppression(t, "+390212345678"))
		}
		// The same result again writes no second suppression.
		if r := f.screen(t, "+390212345678", "rpo", leadstore.ScreenListed, now); r.Suppressed != 0 {
			t.Errorf("second listed: %+v, want no new suppression", r)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		f := newOutreachFixture(t)
		for name, s := range map[string]leadstore.Screening{
			"national number":  {Number: "01632960101", Register: "tps", Result: "clean", CheckedAt: now},
			"unknown register": {Number: e164, Register: "nope", Result: "clean", CheckedAt: now},
			"unknown result":   {Number: e164, Register: "tps", Result: "maybe", CheckedAt: now},
			"no time":          {Number: e164, Register: "tps", Result: "clean"},
		} {
			if _, err := f.q.RecordScreening(ctx, 0, s, "", leadstore.ActorGremy); !errors.Is(err, leadstore.ErrBadScreening) {
				t.Errorf("%s: %v, want ErrBadScreening", name, err)
			}
		}
		ok := leadstore.Screening{Number: e164, Register: "tps", Result: "clean", CheckedAt: now}
		if _, err := f.q.RecordScreening(ctx, 0, ok, "/tmp/result.csv", leadstore.ActorGremy); !errors.Is(err, leadstore.ErrBadScreening) {
			t.Errorf("a path as source: %v, want ErrBadScreening", err)
		}
		if _, err := f.q.RecordScreening(ctx, 0, ok, "", "mallory"); !errors.Is(err, leadstore.ErrUnknownActor) {
			t.Errorf("unknown actor: %v, want ErrUnknownActor", err)
		}
		// The table itself refuses a check time days ahead.
		_, err := f.owner.Exec(ctx, `INSERT INTO phone_screening (number, register, result, checked_at, actor)
			VALUES ($1, 'tps', 'clean', now() + interval '3 days', 'operator')`, e164)
		if sqlState(err) != "23514" {
			t.Errorf("future check time: %v, want 23514", err)
		}
	})
}

// TestScreeningsMissingOrOlder: the export lists a GB lead's numbers never screened or
// screened before the window, in stored form, and leaves out a fresh one (also when the fact
// is stored in national form), a bad one, a suppressed one and another country's.
func TestScreeningsMissingOrOlder(t *testing.T) {
	ctx := context.Background()
	f := newOutreachFixture(t)
	now := time.Now()
	lead := f.siteLead(t, "export.localhost", nil, 7, "info@export.localhost")
	f.setCountry(t, lead, "GB")
	never := "+441632960201"
	old := "+441632960202"
	fresh := "+441632960203"
	nationalFresh := "01632960204"
	bad := f.phoneFact(t, lead, "+441632960205")
	suppressed := "+441632960206"
	nationalNever := "01632960207"
	for _, n := range []string{never, old, fresh, nationalFresh, suppressed, nationalNever} {
		f.phoneFact(t, lead, n)
	}
	f.screen(t, old, "tps", leadstore.ScreenClean, now.AddDate(0, 0, -29))
	f.screen(t, fresh, "tps", leadstore.ScreenClean, now.AddDate(0, 0, -1))
	f.screen(t, "+441632960204", "tps", leadstore.ScreenClean, now.AddDate(0, 0, -1))
	if _, err := f.q.RecordPhoneStatus(ctx, bad, leadstore.PhoneBad, leadstore.PhoneReasonOperator, nil, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.Suppress(ctx, 0, leadstore.SuppressPhone, leadstore.PhoneSuppressionValue(suppressed), leadstore.ReasonDoNotCall, "test"); err != nil {
		t.Fatal(err)
	}
	other := f.siteLead(t, "export-ie.localhost", nil, 7, "info@export-ie.localhost")
	f.setCountry(t, other, "IE")
	f.phoneFact(t, other, "+35312345678")

	got, err := f.q.ScreeningsMissingOrOlder(ctx, 0, "tps", "GB", now.AddDate(0, 0, -28))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{never, old, nationalNever}
	if !slices.Equal(got, want) {
		t.Errorf("missing or older = %v, want %v", got, want)
	}
	// Another register: nothing screened there, so every usable GB number.
	got, err = f.q.ScreeningsMissingOrOlder(ctx, 0, "ctps", "GB", now.AddDate(0, 0, -28))
	if err != nil || len(got) != 5 {
		t.Errorf("ctps missing = %v %v, want the five usable numbers", got, err)
	}
}

// TestPhoneWrongNumberNotUsable: a wrong_number outcome marks the dialled fact bad in the
// same transaction, so the fact is no longer usable for a later batch; the org's other
// number still is. A no_answer leaves the fact alone.
func TestPhoneWrongNumberNotUsable(t *testing.T) {
	ctx := context.Background()
	for _, shadow := range []bool{false, true} {
		f := newOutreachFixture(t)
		s := f.seqBatch(t, shadow, f.siteLead(t, "wrong.localhost", nil, 7, "info@wrong.localhost"))
		second := f.phoneFact(t, s.lead, "+441632960301")
		f.send(t, s, shadow, 0, day(0))
		f.send(t, s, shadow, 1, day(3))
		res, err := f.q.RecordCallOutcome(ctx, 0, s.msg[2], campaign.OutcomeWrongNumber, "", nil, leadstore.ActorGremy, day(5), plainDays)
		if err != nil {
			t.Fatalf("shadow=%v record: %v", shadow, err)
		}
		st, ok, err := f.q.PhoneStatus(ctx, s.phoneFact)
		if err != nil || !ok || st.Status != leadstore.PhoneBad || st.Reason != leadstore.PhoneReasonWrongNumber ||
			st.CallOutcomeID == nil || *st.CallOutcomeID != res.OutcomeID || st.Actor != leadstore.ActorOperator {
			t.Fatalf("shadow=%v status = %+v %v %v, want bad wrong_number naming outcome %d", shadow, st, ok, err, res.OutcomeID)
		}
		if bad, err := f.q.PhoneFactBad(ctx, s.phoneFact); err != nil || !bad {
			t.Errorf("shadow=%v PhoneFactBad = %v %v, want true", shadow, bad, err)
		}
		facts, err := f.q.UsableOrgFacts(ctx, []int64{f.orgOf(t, s.lead)})
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, x := range facts {
			ids = append(ids, x.ID)
		}
		if slices.Contains(ids, s.phoneFact) || !slices.Contains(ids, second) {
			t.Errorf("shadow=%v usable facts = %v, want %d left out and %d kept", shadow, ids, s.phoneFact, second)
		}
	}

	f := newOutreachFixture(t)
	s := f.seqBatch(t, false, f.siteLead(t, "noanswer.localhost", nil, 7, "info@noanswer.localhost"))
	f.send(t, s, false, 0, day(0))
	f.send(t, s, false, 1, day(3))
	if _, err := f.q.RecordCallOutcome(ctx, 0, s.msg[2], campaign.OutcomeNoAnswer, "", nil, leadstore.ActorGremy, day(5), plainDays); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.q.PhoneStatus(ctx, s.phoneFact); err != nil || ok {
		t.Errorf("no_answer wrote a phone status (%v %v)", ok, err)
	}
	if facts, err := f.q.UsableOrgFacts(ctx, []int64{f.orgOf(t, s.lead)}); err != nil || len(facts) != 1 {
		t.Errorf("usable facts after no_answer = %+v %v, want the number", facts, err)
	}
}

// TestPhoneStatusAndResearchRules: the latest status wins; the pairings the table refuses
// are refused before it; a status or research row on a fact that is not a phone (or not
// the org's) writes nothing.
func TestPhoneStatusAndResearchRules(t *testing.T) {
	ctx := context.Background()
	f := newOutreachFixture(t)
	lead := f.siteLead(t, "status.localhost", nil, 7, "info@status.localhost")
	fact := f.phoneFact(t, lead, "+441632960401")
	org := f.orgOf(t, lead)
	if _, err := f.q.InsertFact(ctx, leadstore.FactRow{OrgID: org, Kind: leadstore.FactDescription, Value: "A shop",
		Normalized: "A shop", FetchID: f.fid, DerivedBy: "enrich@1"}); err != nil {
		t.Fatal(err)
	}
	var desc int64
	if err := f.owner.QueryRow(ctx, `SELECT id FROM org_fact WHERE kind = 'description'`).Scan(&desc); err != nil {
		t.Fatal(err)
	}
	one := int64(1)
	for name, c := range map[string]struct {
		status, reason string
		outcome        *int64
		fact           int64
	}{
		"operator with an outcome":   {leadstore.PhoneBad, leadstore.PhoneReasonOperator, &one, fact},
		"wrong number, no outcome":   {leadstore.PhoneBad, leadstore.PhoneReasonWrongNumber, nil, fact},
		"wrong number marked good":   {leadstore.PhoneGood, leadstore.PhoneReasonWrongNumber, &one, fact},
		"corrected marked bad":       {leadstore.PhoneBad, leadstore.PhoneReasonCorrected, &one, fact},
		"bounced call marked good":   {leadstore.PhoneGood, leadstore.PhoneReasonBouncedCall, nil, fact},
		"unknown status":             {"maybe", leadstore.PhoneReasonOperator, nil, fact},
		"a fact that is not a phone": {leadstore.PhoneBad, leadstore.PhoneReasonOperator, nil, desc},
	} {
		if _, err := f.q.RecordPhoneStatus(ctx, c.fact, c.status, c.reason, c.outcome, leadstore.ActorGremy); !errors.Is(err, leadstore.ErrBadPhoneStatus) {
			t.Errorf("%s: %v, want ErrBadPhoneStatus", name, err)
		}
	}
	for _, st := range []string{leadstore.PhoneBad, leadstore.PhoneGood} {
		if _, err := f.q.RecordPhoneStatus(ctx, fact, st, leadstore.PhoneReasonOperator, nil, leadstore.ActorGremy); err != nil {
			t.Fatal(err)
		}
	}
	if bad, err := f.q.PhoneFactBad(ctx, fact); err != nil || bad {
		t.Errorf("bad then good: bad = %v %v, want false (latest wins)", bad, err)
	}
	if _, err := f.q.RecordPhoneStatus(ctx, fact, leadstore.PhoneBad, leadstore.PhoneReasonBouncedCall, nil, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	if bad, _ := f.q.PhoneFactBad(ctx, fact); !bad {
		t.Error("bounced call: not bad")
	}

	cost := int64(1200)
	id, err := f.q.RecordPhoneResearch(ctx, leadstore.PhoneResearchRow{OrgID: org, FactID: &fact, LineKind: leadstore.LineSwitchboard,
		Role: "Reception", Model: "claude-haiku-5-5", CostMicros: &cost})
	if err != nil || id == 0 {
		t.Fatalf("research: %d %v", id, err)
	}
	other := f.siteLead(t, "status-other.localhost", nil, 7, "info@status-other.localhost")
	for name, r := range map[string]leadstore.PhoneResearchRow{
		"another org's fact": {OrgID: f.orgOf(t, other), FactID: &fact, Model: "m"},
		"a description fact": {OrgID: org, FactID: &desc, Model: "m"},
		"unknown line":       {OrgID: org, FactID: &fact, LineKind: "mobile", Model: "m"},
		"long role":          {OrgID: org, FactID: &fact, Role: strings.Repeat("r", leadstore.MaxPhoneRole+1), Model: "m"},
		"no model":           {OrgID: org, FactID: &fact},
	} {
		if _, err := f.q.RecordPhoneResearch(ctx, r); !errors.Is(err, leadstore.ErrBadPhoneResearch) {
			t.Errorf("%s: %v, want ErrBadPhoneResearch", name, err)
		}
	}
}

// dumpLeadTables returns every row of every table in the public schema as JSON text, one
// line per row, each prefixed with its table.
func dumpLeadTables(t *testing.T, owner *pgx.Conn) string {
	t.Helper()
	ctx := context.Background()
	tables, err := pgx.CollectRows(mustQuery(t, owner, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY 1`), pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, tb := range tables {
		var rows string
		q := `SELECT coalesce(string_agg(row_to_json(t)::text, E'\n'), '') FROM ` + pgx.Identifier{tb}.Sanitize() + ` t`
		if err := owner.QueryRow(ctx, q).Scan(&rows); err != nil {
			t.Fatalf("dump %s: %v", tb, err)
		}
		for _, l := range strings.Split(rows, "\n") {
			if l != "" {
				b.WriteString(tb + " " + l + "\n")
			}
		}
	}
	return b.String()
}

func mustQuery(t *testing.T, conn *pgx.Conn, sql string) pgx.Rows {
	t.Helper()
	rows, err := conn.Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestForgetOrgSweep: after ForgetOrg no table holds the org's numbers in clear (the
// hashed suppressions are not a match by design), its lead is suppressed so it is never
// re-collected, another org's phone data is untouched, and a second run counts zero.
func TestForgetOrgSweep(t *testing.T) {
	ctx := context.Background()
	f := newOutreachFixture(t)
	now := time.Now()
	s := f.seqBatch(t, false, f.siteLead(t, "forget.localhost", nil, 7, "info@forget.localhost")) // dials callNumber
	f.setCountry(t, s.lead, "GB")
	org := f.orgOf(t, s.lead)
	national := f.phoneFact(t, s.lead, "01632960002")
	f.screen(t, callNumber, "tps", leadstore.ScreenClean, now)
	f.screen(t, "+441632960002", "ctps", leadstore.ScreenClean, now)
	if _, err := f.q.RecordPhoneResearch(ctx, leadstore.PhoneResearchRow{OrgID: org, FactID: &national,
		LineKind: leadstore.LineDirect, Role: "Head of IT", Model: "claude-haiku-5-5"}); err != nil {
		t.Fatal(err)
	}
	f.send(t, s, false, 0, day(0))
	f.send(t, s, false, 1, day(3))
	if _, err := f.q.RecordCallOutcome(ctx, 0, s.msg[2], campaign.OutcomeWrongNumber, "rang 441632960001, not them",
		nil, leadstore.ActorGremy, day(5), plainDays); err != nil {
		t.Fatal(err)
	}
	keep := f.siteLead(t, "keep.localhost", nil, 7, "info@keep.localhost")
	f.setCountry(t, keep, "GB")
	keepFact := f.phoneFact(t, keep, "+441632960099")
	f.screen(t, "+441632960099", "tps", leadstore.ScreenClean, now)

	secret := []string{"441632960001", "441632960002", "1632960002"}
	before := dumpLeadTables(t, f.owner)
	for _, d := range secret {
		if !strings.Contains(before, d) {
			t.Fatalf("fixture: %s is not stored before the erasure, so the sweep proves nothing", d)
		}
	}

	owner := leadstore.NewQueries(f.owner)
	got, err := owner.ForgetOrg(ctx, org, "erasure request by email")
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	want := leadstore.Forgotten{PhoneFacts: 2, PhoneStatuses: 1, PhoneResearch: 1, Screenings: 2, NotesBlanked: 1,
		Suppressions: 4, LeadsSuppressed: 0, Cancelled: got.Cancelled} // contacted: only new or batched leads move
	if got != want || got.Cancelled < 1 {
		t.Errorf("forget = %+v, want %+v with at least one cancelled", got, want)
	}

	after := dumpLeadTables(t, f.owner)
	for _, d := range secret {
		for _, l := range strings.Split(after, "\n") {
			if strings.Contains(l, d) {
				t.Errorf("after the erasure %s is still stored: %s", d, l)
			}
		}
	}
	if !strings.Contains(after, "441632960099") {
		t.Error("the other org's number went too")
	}
	if _, ok, err := f.q.LatestScreening(ctx, "+441632960099", "tps"); err != nil || !ok {
		t.Errorf("the other org's screening went too (%v %v)", ok, err)
	}
	if facts, err := f.q.OrgFacts(ctx, []int64{f.orgOf(t, keep)}); err != nil || len(facts) != 1 || facts[0].ID != keepFact {
		t.Errorf("the other org's facts = %+v %v", facts, err)
	}
	var state string
	if err := f.owner.QueryRow(ctx, `SELECT state FROM lead WHERE id = $1`, s.lead).Scan(&state); err != nil || state != leadstore.LeadContacted {
		t.Errorf("lead state = %q %v, want contacted (a suppression moves only new or batched leads)", state, err)
	}
	for _, n := range []string{callNumber, "01632960002", "+441632960002"} {
		if r := f.phoneSuppression(t, n); r != leadstore.ReasonManual {
			t.Errorf("phone suppression of %s = %q, want manual", n, r)
		}
	}
	var domains int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM suppression_active WHERE kind = 'domain' AND value = 'forget.localhost'
		AND source = 'forget: erasure request by email'`).Scan(&domains); err != nil || domains != 1 {
		t.Errorf("domain suppressions = %d %v, want 1", domains, err)
	}
	var outcomes int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM call_outcome WHERE message_id = $1 AND note = ''`, s.msg[2]).Scan(&outcomes); err != nil || outcomes != 1 {
		t.Errorf("call outcome kept with a blank note: %d %v", outcomes, err)
	}

	again, err := owner.ForgetOrg(ctx, org, "erasure request by email")
	if err != nil || again != (leadstore.Forgotten{}) {
		t.Errorf("second forget = %+v %v, want all zero", again, err)
	}
	if _, err := owner.ForgetOrg(ctx, 999999, "x"); !errors.Is(err, leadstore.ErrOrgNotFound) {
		t.Errorf("missing org: %v, want ErrOrgNotFound", err)
	}
	for _, r := range []string{"", "  ", "line\nbreak", "ask a@b.example", strings.Repeat("x", leadstore.MaxForgetReason+1)} {
		if _, err := owner.ForgetOrg(ctx, org, r); !errors.Is(err, leadstore.ErrBadForgetReason) {
			t.Errorf("reason %q: %v, want ErrBadForgetReason", r, err)
		}
	}
}

// TestForgetOrgNeedsOwner: under the app role ForgetOrg fails with insufficient_privilege
// and changes nothing (the suppressions it wrote first roll back with it).
func TestForgetOrgNeedsOwner(t *testing.T) {
	ctx := context.Background()
	f := newOutreachFixture(t)
	lead := f.siteLead(t, "forget-app.localhost", nil, 7, "info@forget-app.localhost")
	fact := f.phoneFact(t, lead, "+441632960501")
	app := leadstore.NewQueries(storetest.AppConn(t))
	_, err := app.ForgetOrg(ctx, f.orgOf(t, lead), "erasure request")
	if sqlState(err) != "42501" {
		t.Fatalf("forget as app: %v (SQLSTATE %q), want 42501", err, sqlState(err))
	}
	var facts, sups int
	if err := f.owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM org_fact WHERE id = $1), (SELECT count(*) FROM suppression)`, fact).
		Scan(&facts, &sups); err != nil || facts != 1 || sups != 0 {
		t.Errorf("after a refused forget: facts %d, suppressions %d (%v); want 1 and 0", facts, sups, err)
	}
}

// TestForgetOrgStopsCollectors: once an org is forgotten (its domain suppressed), no
// collector selects it again: enrich (OrgsNeedingEnrich), the contact search
// (OrgsNeedingRoutes) and contact research (ClaimResearch, even for a row queued before the
// erasure). The org next to it is still selected by all three.
func TestForgetOrgStopsCollectors(t *testing.T) {
	ctx := context.Background()
	f := newOutreachFixture(t)
	gone := f.siteLead(t, "forget-collect.localhost", nil, 7, "info@forget-collect.localhost")
	keep := f.siteLead(t, "keep-collect.localhost", nil, 7, "info@keep-collect.localhost")
	goneOrg, keepOrg := f.orgOf(t, gone), f.orgOf(t, keep)
	for _, l := range []int64{gone, keep} {
		if _, _, err := f.q.RequestResearch(ctx, l, leadstore.ResearchCLI, []string{leadstore.WantPhone}, leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(orgs []leadstore.Org) []int64 {
		var out []int64
		for _, o := range orgs {
			out = append(out, o.ID)
		}
		return out
	}
	now := time.Now()
	enrich, err := f.q.OrgsNeedingEnrich(ctx, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ids(enrich), goneOrg) || !slices.Contains(ids(enrich), keepOrg) {
		t.Fatalf("fixture: enrich selects %v, want both %d and %d", ids(enrich), goneOrg, keepOrg)
	}

	if _, err := leadstore.NewQueries(f.owner).ForgetOrg(ctx, goneOrg, "erasure request"); err != nil {
		t.Fatal(err)
	}
	enrich, err = f.q.OrgsNeedingEnrich(ctx, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := f.q.OrgsNeedingRoutes(ctx, 0, now.Add(365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string][]int64{"enrich": ids(enrich), "routes": ids(routes)} {
		if slices.Contains(got, goneOrg) || !slices.Contains(got, keepOrg) {
			t.Errorf("%s selects %v after the forget: want %d, never %d", name, got, keepOrg, goneOrg)
		}
	}
	claimed, err := f.q.ClaimResearch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var orgs []int64
	for _, r := range claimed {
		orgs = append(orgs, r.OrgID)
	}
	if slices.Contains(orgs, goneOrg) || !slices.Contains(orgs, keepOrg) {
		t.Errorf("research claimed orgs %v after the forget: want %d, never %d", orgs, keepOrg, goneOrg)
	}
}

// TestForgetOrgConsentSweep: the address a person gave on a call with their consent is
// theirs: after ForgetOrg no table holds it, the call outcome stays (without its consent),
// and another org's consent is untouched.
func TestForgetOrgConsentSweep(t *testing.T) {
	ctx := context.Background()
	f := newOutreachFixture(t)
	consent := func(domain, addr string) seqLead {
		s := f.seqBatch(t, false, f.siteLead(t, domain, nil, 7, "info@"+domain))
		f.send(t, s, false, 0, day(0))
		f.send(t, s, false, 1, day(3))
		if _, err := f.q.RecordCallOutcome(ctx, 0, s.msg[2], campaign.OutcomeConnected, "spoke to Jane",
			&leadstore.ConsentInput{Address: addr, Wording: "May I send you the report by email?"},
			leadstore.ActorGremy, day(5), plainDays); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := consent("forget-consent.localhost", "Jane.Doe@Personal-Mail.localhost")
	consent("keep-consent.localhost", "owner@keep-consent.localhost")
	const secret = "jane.doe@personal-mail.localhost"
	if before := dumpLeadTables(t, f.owner); !strings.Contains(before, secret) {
		t.Fatal("fixture: the consent address is not stored before the erasure, so the sweep proves nothing")
	}
	got, err := leadstore.NewQueries(f.owner).ForgetOrg(ctx, f.orgOf(t, s.lead), "erasure request by phone")
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if got.Consents != 1 || got.ConsentsBlanked != 1 {
		t.Errorf("forget = %+v, want one consent deleted and one call outcome's consent blanked", got)
	}
	after := dumpLeadTables(t, f.owner)
	for _, l := range strings.Split(after, "\n") {
		if strings.Contains(strings.ToLower(l), secret) || strings.Contains(strings.ToLower(l), "personal-mail") {
			t.Errorf("after the erasure the consent address is still stored: %s", l)
		}
	}
	if !strings.Contains(after, "owner@keep-consent.localhost") {
		t.Error("the other org's consent went too")
	}
	var n int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM call_outcome WHERE message_id = $1 AND outcome = 'connected'
		AND NOT email_consent AND consent_address IS NULL`, s.msg[2]).Scan(&n); err != nil || n != 1 {
		t.Errorf("call outcome kept without its consent: %d %v", n, err)
	}
	if again, err := leadstore.NewQueries(f.owner).ForgetOrg(ctx, f.orgOf(t, s.lead), "erasure request by phone"); err != nil || again != (leadstore.Forgotten{}) {
		t.Errorf("second forget = %+v %v, want all zero", again, err)
	}
}
