// Package leadview words a lead for the console in plain English: what was found on its
// site, why it scores what it does, and how relevant it was rated. It only formats; every
// fact comes from leadstore and leadconfig.
package leadview

import (
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// FindingLine is a lead's finding in one line:
//
//	Drupal 10.2, security support ends 9 December 2026
//	Drupal 7, out of support since 5 January 2025
//	Drupal 10 or 11
//	Drupal (version not shown)
//	Agency page sells Drupal upgrades: “…”
//
// The version is the exact version when known, else the major, else the detector's
// narrowed range. The support clause comes from facts for a known major whose date is set:
// "ends" while the date is today or later, "out of support since" once today is past it.
// A subject with nothing in it (a lead with no finding) is "".
func FindingLine(s leadstore.FindingSubject, facts leadconfig.Facts, today time.Time) string {
	if s.Kind == leadstore.LeadAgencyPartner || s.URL != "" || s.Phrase != "" {
		if s.Phrase == "" {
			return "Agency page sells Drupal upgrades"
		}
		return "Agency page sells Drupal upgrades: “" + s.Phrase + "”"
	}
	if s == (leadstore.FindingSubject{}) {
		return ""
	}
	major := s.Major
	if major == 0 && s.ExactVersion != "" {
		major = versionMajor(s.ExactVersion)
	}
	var version string
	switch {
	case s.ExactVersion != "":
		version = "Drupal " + s.ExactVersion
	case major > 0:
		version = "Drupal " + strconv.Itoa(major)
	case s.Ambiguous != "":
		version = "Drupal " + ambiguousWords(s.Ambiguous)
	default:
		return "Drupal (version not shown)"
	}
	if major == 0 {
		return version
	}
	eol, ok := facts.EOL(major)
	if !ok || eol.IsZero() {
		return version
	}
	date, err := outreach.FormatDate(eol.Time, outreach.LangEnglish)
	if err != nil {
		return version
	}
	if dayOf(today).After(dayOf(eol.Time)) {
		return version + ", out of support since " + date
	}
	return version + ", security support ends " + date
}

// ambiguousWords is a detector's narrowed range in words: "10 or 11", "8 or later".
func ambiguousWords(a string) string {
	switch a {
	case drupal.Ambiguous10Or11:
		return "10 or 11"
	case drupal.Ambiguous8Plus:
		return "8 or later"
	case drupal.Ambiguous6Or7:
		return "6 or 7"
	}
	return strings.ReplaceAll(a, "-", " ")
}

// versionMajor is the leading number of a version such as "10.2.3", or 0.
func versionMajor(v string) int {
	head, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(head)
	if err != nil {
		return 0
	}
	return n
}

// dayOf is t's calendar date, so a date compares by day whatever the clock or zone.
func dayOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
