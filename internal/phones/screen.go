// Package phones holds CP5's phone-side rules that are not the country table: screening a
// number against the do-not-call registers (file export and import, and the callable
// decision), and phone research for call-step leads as a thin adapter over
// internal/research, which already asks the model and verifies each number on the page it
// cites. The package is pure: the store is reached through the small interfaces defined
// here, which internal/leadstore satisfies, and no model is called from here.
package phones

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/dial"
	"github.com/gremy/webbership-leadhound/internal/enrich"
)

// Screening results, as phone_screening.result holds them.
const (
	Clean  = "clean"
	Listed = "listed"
)

// ExportHeader is the first line of an export file; one E.164 number per line follows.
const ExportHeader = "number"

// Screening is one recorded screening of a number against one register.
type Screening struct {
	Number    string    // E.164: "+" and digits
	Register  string    // the register's code in the country table ("tps", "rpo" …)
	Result    string    // Clean or Listed
	CheckedAt time.Time // when the register was consulted
}

// ScreeningStore is the slice of the store screening needs. internal/leadstore's
// phones.go satisfies it (through a thin adapter where its own row types differ).
type ScreeningStore interface {
	// RecordScreening appends one screening row (phone_screening is append-only).
	// source names the file it came from, actor who imported it.
	RecordScreening(ctx context.Context, s Screening, source, actor string) error
	// LatestScreening is number's most recent screening against register; ok is false
	// when it was never screened there.
	LatestScreening(ctx context.Context, number, register string) (s Screening, ok bool, err error)
	// ScreeningsMissingOrOlder lists the numbers of country's callable leads (stored phone
	// facts, as normalised) whose latest screening against register is missing or was
	// checked before before.
	ScreeningsMissingOrOlder(ctx context.Context, register, country string, before time.Time) ([]string, error)
}

// Cutoff is the oldest a screening against reg may be and still count: now minus the
// register's max_age_days, or, for a register whose research named no age (0 days), the
// campaign's start, so the number is screened before each campaign.
func Cutoff(reg compliance.Register, campaignStart, now time.Time) time.Time {
	if reg.MaxAgeDays > 0 {
		return now.AddDate(0, 0, -reg.MaxAgeDays)
	}
	return campaignStart
}

// Callable decides whether number may be called as far as screening goes: for every
// register in required (compliance.RegistersFor the lead's country) its latest screening
// must be Clean and not older than the register's window (Cutoff). The latest screening
// wins per register, so clean then listed is not callable; a number once listed also
// carries a phone suppression, which only the operator lifts, so listed then clean stays
// out through the suppression check, not here. A screening dated after now is ignored
// (a clock or file error, never trusted). With no registers required it is callable; the
// reason then is "". Otherwise reason says, in plain words, why not.
func Callable(number string, screenings []Screening, required []compliance.Register, campaignStart, now time.Time) (ok bool, reason string) {
	for _, reg := range required {
		var latest Screening
		found := false
		for _, s := range screenings {
			if s.Number != number || s.Register != reg.Code || s.CheckedAt.After(now) {
				continue
			}
			if !found || s.CheckedAt.After(latest.CheckedAt) {
				latest, found = s, true
			}
		}
		name := registerName(reg)
		switch {
		case !found:
			return false, "not screened against " + name
		case latest.Result != Clean:
			return false, "listed on " + name
		}
		if reg.MaxAgeDays > 0 {
			if latest.CheckedAt.Before(Cutoff(reg, campaignStart, now)) {
				return false, fmt.Sprintf("%s screening is older than %d days", name, reg.MaxAgeDays)
			}
			continue
		}
		if campaignStart.IsZero() || latest.CheckedAt.Before(campaignStart) {
			return false, name + " screening is from before this campaign started"
		}
	}
	return true, ""
}

func registerName(r compliance.Register) string {
	if r.Name != "" {
		return r.Name
	}
	return strings.ToUpper(r.Code)
}

// ExportSummary counts what Export wrote and what it left out.
type ExportSummary struct {
	Written    int // numbers written
	Undialable int // stored numbers with no E.164 form for the register's country (left out)
}

// Export writes the screening file for reg: ExportHeader, then each number whose latest
// screening against reg is missing or older than Cutoff, in E.164, sorted, once each. A
// number stored in national form gets the register country's code through dial.Number; one
// that cannot be dialled safely is counted and left out, never guessed. The caller opens
// the file (0600, outside the repo).
func Export(ctx context.Context, st ScreeningStore, reg compliance.Register, campaignStart, now time.Time, w io.Writer) (ExportSummary, error) {
	var sum ExportSummary
	if reg.Code == "" || reg.Country == "" {
		return sum, errors.New("phones: export: register has no code or country")
	}
	if reg.MaxAgeDays == 0 && campaignStart.IsZero() {
		return sum, fmt.Errorf("phones: export: register %s is screened before each campaign; the campaign's start is needed", reg.Code)
	}
	stored, err := st.ScreeningsMissingOrOlder(ctx, reg.Code, reg.Country, Cutoff(reg, campaignStart, now))
	if err != nil {
		return sum, fmt.Errorf("phones: export %s: %w", reg.Code, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range stored {
		e := E164(n, reg.Country)
		if e == "" {
			sum.Undialable++
			continue
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	sort.Strings(out)
	bw := bufio.NewWriter(w)
	if _, err := bw.WriteString(ExportHeader + "\n"); err != nil {
		return sum, fmt.Errorf("phones: export %s: %w", reg.Code, err)
	}
	for _, e := range out {
		if _, err := bw.WriteString(e + "\n"); err != nil {
			return sum, fmt.Errorf("phones: export %s: %w", reg.Code, err)
		}
	}
	if err := bw.Flush(); err != nil {
		return sum, fmt.Errorf("phones: export %s: %w", reg.Code, err)
	}
	sum.Written = len(out)
	return sum, nil
}

// E164 is raw as an E.164 number ("+" and 8 to 15 digits), or "" when it cannot be one
// safely. raw is normalised with enrich.NormalizePhone (so "+44 (0)20 …" drops the "(0)"
// and "0044 …" reads as "+44 …"); a number without its country code is completed with
// country's through dial.Number (trunk prefix dropped where the country has one).
func E164(raw, country string) string {
	_, norm, ok := enrich.NormalizePhone(raw)
	if !ok {
		return ""
	}
	e := dial.Number(norm, country)
	if n := len(e) - 1; n < 8 || n > 15 {
		return ""
	}
	return e
}

// statusWords maps a result file's status text (lower case, "_" and "-" read as spaces,
// spaces collapsed) to a result. Registers' own files differ and are not documented here,
// so the parser accepts the common wordings and refuses anything else.
var statusWords = map[string]string{
	// Clean: the number is not on the register.
	"clean": Clean, "not listed": Clean, "unlisted": Clean, "not registered": Clean,
	"unregistered": Clean, "not on list": Clean, "not on the list": Clean,
	"not on register": Clean, "not on the register": Clean, "no match": Clean,
	"not matched": Clean, "not found": Clean, "ok": Clean, "allowed": Clean, "callable": Clean,
	// Listed: the number is on the register, or opted out of calls.
	"listed": Listed, "registered": Listed, "on list": Listed, "on the list": Listed,
	"on register": Listed, "on the register": Listed, "match": Listed, "matched": Listed,
	"found": Listed, "opted out": Listed, "blocked": Listed, "do not call": Listed,
	"dnc": Listed,
}

// yesNoWords are accepted only in a column whose header asks "is it listed?" (listed,
// registered, on list, the register's code …): yes means Listed, no means Clean.
var yesNoWords = map[string]string{
	"yes": Listed, "y": Listed, "true": Listed, "1": Listed,
	"no": Clean, "n": Clean, "false": Clean, "0": Clean,
}

var numberHeaders = map[string]bool{
	"number": true, "phone": true, "phone number": true, "telephone": true,
	"telephone number": true, "tel": true, "msisdn": true, "e164": true, "e.164": true,
}

var statusHeaders = map[string]bool{"status": true, "result": true, "outcome": true}

var listedHeaders = map[string]bool{
	"listed": true, "registered": true, "on list": true, "on register": true,
	"is listed": true, "is registered": true, "match": true, "matched": true,
}

var dateHeaders = map[string]bool{
	"checked": true, "checked at": true, "checked on": true, "date": true,
	"screened": true, "screened at": true, "screened on": true,
}

// ImportSummary is what ParseResults read: the screenings to record, and how many lines
// were refused because they did not parse (never guessed). Lines lists the refused lines'
// numbers (1-based, the header counted), so the operator can open the file at them; it
// never holds the line's text.
type ImportSummary struct {
	Screenings []Screening
	Refused    int
	Lines      []int
}

// ParseResults reads a register's result file for reg. The file is CSV (comma, semicolon
// or tab, read from the first line), with a number column and a status column:
//
//   - With a header row, the number column is headed number, phone, phone number,
//     telephone, tel, msisdn or e164; the status column status, result or outcome, also
//     after the register's code ("tps status") (the words below), or listed, registered, on list, matched or the register's own code
//     ("tps") (the words below, and also yes/no, y/n, true/false, 1/0, yes meaning listed);
//     an optional date column (checked, checked at, date, screened …) gives each row's
//     check date as 2006-01-02 or RFC 3339.
//   - Without a header (the first line's first cell is a number), column 1 is the number
//     and column 2 the status words.
//
// Status words (case, "_" and "-" ignored): clean, not listed, unlisted, not registered,
// unregistered, not on (the) list, not on (the) register, no match, not matched, not found,
// ok, allowed, callable → clean; listed, registered, on (the) list, on (the) register,
// match, matched, found, opted out, blocked, do not call, dnc → listed.
//
// Numbers go through E164 with the register's country, so "+44 (0)20 7946 0000",
// "0044 20 7946 0000" and "020 7946 0000" (for a GB register) are the same number; a
// leading "'" or "=" spreadsheet guard is dropped. A row whose number, status or date does
// not parse, or whose date is after now, is refused and counted. A row with no date column
// takes checkedAt (when the operator ran the screening). A number twice in the file is
// recorded once: listed if any of its rows says listed (the safe side), else clean with the
// latest date. Blank lines are skipped.
func ParseResults(r io.Reader, reg compliance.Register, checkedAt, now time.Time) (ImportSummary, error) {
	var sum ImportSummary
	if reg.Code == "" || reg.Country == "" {
		return sum, errors.New("phones: import: register has no code or country")
	}
	br := bufio.NewReader(r)
	first, err := br.Peek(4096)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return sum, fmt.Errorf("phones: import %s: %w", reg.Code, err)
	}
	cr := csv.NewReader(br)
	cr.Comma = delimiter(first)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.TrimLeadingSpace = true
	cr.ReuseRecord = false

	numCol, statCol, dateCol := 0, 1, -1
	yesNo := false
	headerDone := false
	byNumber := map[string]Screening{}
	var line int
	refuse := func() { sum.Refused++; sum.Lines = append(sum.Lines, line) }

	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				line = pe.StartLine
				refuse()
				continue
			}
			return sum, fmt.Errorf("phones: import %s: %w", reg.Code, err)
		}
		line, _ = cr.FieldPos(0)
		if blank(rec) {
			continue
		}
		if !headerDone {
			headerDone = true
			if h, ok := header(rec, reg.Code); ok {
				numCol, statCol, dateCol, yesNo = h.num, h.status, h.date, h.yesNo
				continue
			}
		}
		s, ok := row(rec, numCol, statCol, dateCol, yesNo, reg, checkedAt, now)
		if !ok {
			refuse()
			continue
		}
		prev, dup := byNumber[s.Number]
		switch {
		case !dup:
			byNumber[s.Number] = s
		case prev.Result == Listed && s.Result != Listed:
		case s.Result == Listed && prev.Result != Listed:
			byNumber[s.Number] = s
		case s.CheckedAt.After(prev.CheckedAt):
			byNumber[s.Number] = s
		}
	}
	for _, s := range byNumber {
		sum.Screenings = append(sum.Screenings, s)
	}
	sort.Slice(sum.Screenings, func(i, j int) bool { return sum.Screenings[i].Number < sum.Screenings[j].Number })
	return sum, nil
}

// Import records each screening ParseResults read through st. A listed number also needs a
// phone suppression with the register's code as its reason (the plan's rule); the store
// writes that itself or the caller does, from the returned summary's Listed results.
func Import(ctx context.Context, st ScreeningStore, sum ImportSummary, source, actor string) (recorded int, err error) {
	for _, s := range sum.Screenings {
		if err := st.RecordScreening(ctx, s, source, actor); err != nil {
			return recorded, fmt.Errorf("phones: record screening %d of %d: %w", recorded+1, len(sum.Screenings), err)
		}
		recorded++
	}
	return recorded, nil
}

type headerCols struct {
	num, status, date int
	yesNo             bool
}

// header reads rec as a header row when it names a number column and a status column.
func header(rec []string, register string) (headerCols, bool) {
	h := headerCols{num: -1, status: -1, date: -1}
	for i, c := range rec {
		k := words(c)
		switch {
		case numberHeaders[k] && h.num < 0:
			h.num = i
		case (statusHeaders[k] || statusHeaders[strings.TrimPrefix(k, register+" ")] || listedHeaders[k] || k == register) && h.status < 0:
			// "status", "tps status", "tps_result": words; "listed", "tps": also yes/no.
			h.yesNo = !statusHeaders[k] && !statusHeaders[strings.TrimPrefix(k, register+" ")]
			h.status = i
		case dateHeaders[k] && h.date < 0:
			h.date = i
		}
	}
	return h, h.num >= 0 && h.status >= 0
}

func row(rec []string, numCol, statCol, dateCol int, yesNo bool, reg compliance.Register, checkedAt, now time.Time) (Screening, bool) {
	if numCol >= len(rec) || statCol >= len(rec) {
		return Screening{}, false
	}
	num := E164(strings.TrimLeft(strings.TrimSpace(rec[numCol]), "'="), reg.Country)
	if num == "" {
		return Screening{}, false
	}
	st := words(rec[statCol])
	res, ok := statusWords[st]
	if !ok && yesNo {
		res, ok = yesNoWords[st]
	}
	if !ok {
		return Screening{}, false
	}
	at := checkedAt
	if dateCol >= 0 {
		if dateCol >= len(rec) {
			return Screening{}, false
		}
		d, ok := parseDate(rec[dateCol])
		if !ok {
			return Screening{}, false
		}
		at = d
	}
	if at.IsZero() || at.After(now) {
		return Screening{}, false
	}
	return Screening{Number: num, Register: reg.Code, Result: res, CheckedAt: at}, true
}

func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// words is s lower-cased, "_" and "-" read as spaces, spaces collapsed.
func words(s string) string {
	s = strings.NewReplacer("_", " ", "-", " ").Replace(strings.ToLower(s))
	return strings.Join(strings.Fields(s), " ")
}

func blank(rec []string) bool {
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// delimiter is the separator the file's first line uses most: ';', tab, else ','.
func delimiter(head []byte) rune {
	l := string(head)
	if i := strings.IndexAny(l, "\r\n"); i >= 0 {
		l = l[:i]
	}
	best, n := ',', strings.Count(l, ",")
	for _, d := range []rune{';', '\t'} {
		if c := strings.Count(l, string(d)); c > n {
			best, n = d, c
		}
	}
	return best
}
