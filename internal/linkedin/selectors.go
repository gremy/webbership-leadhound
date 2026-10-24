// Package linkedin parses LinkedIn's public guest job pages (search cards and job
// details) into typed structs, and builds the URLs that fetch them.
//
// Every CSS selector lives in the one table below, so a markup change is fixed in one
// place. A page that has cards but lost a title or company returns ErrParserBroke: a
// broken parser must read as "parser broke", never as "no jobs".
package linkedin

import "errors"

// ErrParserBroke means a page had the expected shape (cards, or a detail page) but a
// required field could not be found: the markup has drifted from the selector table.
var ErrParserBroke = errors.New("linkedin: parser broke")

// Apply types, matching the job_observation.apply_type CHECK constraint.
const (
	ApplyOffsite = "offsite"
	ApplyOnsite  = "onsite"
	ApplySimple  = "simple"
)

// selectors is the one selector table (LI2).
type selectors struct {
	// Search cards.
	card         string // the card; its data-entity-urn carries the job ID
	cardSelfLink string // a card that is itself the link to the job view (no cardLink inside)
	jobURN       string // any element carrying a job URN, counted apart from card
	cardLink     string // the full-card link to the job view
	cardTitle    string
	cardCompany  string // the company link inside the subtitle
	cardSubtitle string // the subtitle itself, when the company has no link
	cardLocation string
	cardListDate string // datetime attribute; matches both --new and plain
	cardBenefits string

	// Detail page.
	topCard        string // the region where closed and apply markers count
	detailTitle    string
	detailCompany  string
	detailLocation string
	notLocation    string // excluded from detailLocation
	detailPosted   string
	detailApplied  string
	description    []string // first match wins
	criteriaItem   string
	criteriaLabel  string
	criteriaValue  string
	applyOnsite    string
	applySimple    string
	applyOffsite   string
	closedBanner   string
}

var sel = selectors{
	card:         "li div.base-search-card, li a.base-search-card",
	cardSelfLink: "a.base-search-card",
	jobURN:       `[data-entity-urn^="urn:li:jobPosting:"]`,
	cardLink:     "a.base-card__full-link",
	cardTitle:    "h3.base-search-card__title",
	cardCompany:  "h4.base-search-card__subtitle a",
	cardSubtitle: "h4.base-search-card__subtitle",
	cardLocation: "span.job-search-card__location",
	cardListDate: "time[class^=job-search-card__listdate]",
	cardBenefits: "span.job-posting-benefits__text",

	topCard:        ".top-card-layout",
	detailTitle:    "h2.top-card-layout__title, h1.top-card-layout__title",
	detailCompany:  "a.topcard__org-name-link",
	detailLocation: "span.topcard__flavor.topcard__flavor--bullet",
	notLocation:    ".num-applicants__caption",
	detailPosted:   "span.posted-time-ago__text",
	detailApplied:  ".num-applicants__caption",
	description:    []string{"div.show-more-less-html__markup", "div.description__text"},
	criteriaItem:   "li.description__job-criteria-item",
	criteriaLabel:  "h3.description__job-criteria-subheader",
	criteriaValue:  "span.description__job-criteria-text",
	applyOnsite:    "[data-tracking-control-name=public_jobs_apply-link-onsite]",
	applySimple:    "[data-tracking-control-name=public_jobs_apply-link-simple_onsite], [data-tracking-control-name=public_jobs_apply-link-simple]",
	applyOffsite:   "[data-svg-class-name=apply-button__offsite-apply-icon-svg], [data-tracking-control-name^=public_jobs_apply-link-offsite]",
	closedBanner:   ".closed-job, .closed-job__flavor",
}

// closedText is the banner text a closed posting shows in its top card.
const closedText = "no longer accepting applications"
