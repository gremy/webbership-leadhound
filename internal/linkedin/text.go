package linkedin

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// squash trims s and collapses every run of whitespace to one space.
func squash(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

var applicantsRE = regexp.MustCompile(`(\d[\d,.]*)\s*applicants?`)

// ParseApplicants reads an applicant caption: "116 applicants" → 116, "1 applicant" → 1,
// "Over 200 applicants" → 200, "Be among the first 25 applicants" → 25. It returns nil
// when the text names no count.
func ParseApplicants(text string) *int {
	m := applicantsRE.FindStringSubmatch(strings.ToLower(squash(text)))
	if m == nil {
		return nil
	}
	digits := strings.NewReplacer(",", "", ".", "").Replace(m[1])
	n, err := strconv.Atoi(digits)
	if err != nil {
		return nil
	}
	return &n
}

var postedRE = regexp.MustCompile(`^(?:reposted\s+)?(\d+)\s+(minute|hour|day|week|month|year)s?\s+ago$`)

// ParsePosted estimates when a job was posted from LinkedIn's relative text ("6 days
// ago", "2 hours ago", "Reposted 1 week ago", "Just now"), counted back from fetchedAt.
// Months count as 30 days and years as 365. It returns nil when the text is not
// understood. The result is an instant; bucketing it into a day is the caller's job.
func ParsePosted(text string, fetchedAt time.Time) *time.Time {
	t := strings.ToLower(squash(text))
	if t == "just now" || t == "reposted just now" {
		at := fetchedAt
		return &at
	}
	m := postedRE.FindStringSubmatch(t)
	if m == nil {
		return nil
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return nil
	}
	var at time.Time
	switch m[2] {
	case "minute":
		at = fetchedAt.Add(-time.Duration(n) * time.Minute)
	case "hour":
		at = fetchedAt.Add(-time.Duration(n) * time.Hour)
	case "day":
		at = fetchedAt.AddDate(0, 0, -n)
	case "week":
		at = fetchedAt.AddDate(0, 0, -7*n)
	case "month":
		at = fetchedAt.AddDate(0, 0, -30*n)
	case "year":
		at = fetchedAt.AddDate(0, 0, -365*n)
	}
	return &at
}

// DayIn returns local midnight, in loc, of the day t falls on there.
func DayIn(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}
