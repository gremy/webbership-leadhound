package ats

import (
	"encoding/xml"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// teamtailorPageSize is the most items one feed request returns (the feed's default and
// the per_page asked for). A page with fewer items is the last one.
const teamtailorPageSize = 100

// teamtailorPageURL is the public RSS feed of a board, one page of it:
// https://<slug>.teamtailor.com/jobs.rss?offset=N&per_page=100. The wiring asks for
// offset 0, 100, 200, ... until teamtailorLastPage says a page came back short.
func teamtailorPageURL(b Board, offset int) string {
	if offset < 0 {
		offset = 0
	}
	q := url.Values{}
	q.Set("offset", strconv.Itoa(offset))
	q.Set("per_page", strconv.Itoa(teamtailorPageSize))
	return "https://" + url.PathEscape(b.Board) + ".teamtailor.com/jobs.rss?" + q.Encode()
}

// teamtailorLastPage reports whether a parsed page is the last one: it held fewer than
// teamtailorPageSize items. The parser drops no items, so postings count items.
func teamtailorLastPage(page []Posting) bool { return len(page) < teamtailorPageSize }

// teamtailorFeed is GET /jobs.rss: RSS 2.0. The locations and the department, role and
// division sit in the tt namespace (https://teamtailor.com/locations); remoteStatus has
// none. Fields match on the local name, so a change of the namespace URI breaks nothing.
type teamtailorFeed struct {
	XMLName xml.Name `xml:"rss"`
	Channel *struct {
		Title string           `xml:"title"`
		Items []teamtailorItem `xml:"item"`
	} `xml:"channel"`
}

type teamtailorItem struct {
	Title        string `xml:"title"`
	Description  string `xml:"description"` // escaped HTML or CDATA; decoding yields the markup
	PubDate      string `xml:"pubDate"`     // RFC 1123 with a numeric zone
	Link         string `xml:"link"`        // https://<host>/jobs/<number>-<title-slug>
	GUID         string `xml:"guid"`        // a UUID, no isPermaLink
	RemoteStatus string `xml:"remoteStatus"`
	Locations    []struct {
		Name    string `xml:"name"`
		City    string `xml:"city"`
		Country string `xml:"country"`
	} `xml:"locations>location"`
}

// parseTeamtailor maps one page of a Teamtailor board's RSS feed.
//
// ID is the item's guid: it is a UUID that names the job, while the link carries a slug of
// the title that changes when the title does. An item with an empty guid falls back to the
// job number at the start of the link's last segment.
//
// Company is Board.Name, else the slug. The channel title is not used: the one sample read
// names the company, but nothing documents that it always does (a careers site may title
// its feed otherwise).
//
// The feed gives no separate apply URL, so ApplyURL is the link, and no employment type.
func parseTeamtailor(b Board, body []byte) ([]Posting, error) {
	var feed teamtailorFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, broke("bad RSS: %v", err)
	}
	if feed.Channel == nil {
		return nil, broke("no channel")
	}
	out := make([]Posting, 0, len(feed.Channel.Items))
	for i, it := range feed.Channel.Items {
		link := strings.TrimSpace(it.Link)
		p := Posting{
			ID:              strings.TrimSpace(it.GUID),
			Title:           strings.TrimSpace(it.Title),
			Company:         b.company(),
			DescriptionHTML: strings.TrimSpace(it.Description),
			PostedAt:        teamtailorTime(it.PubDate),
			ApplyURL:        link,
			URL:             link,
		}
		if p.ID == "" {
			p.ID = teamtailorIDFromLink(link)
		}
		if p.ID == "" || p.Title == "" {
			return nil, broke("item %d has no id or no title", i)
		}
		locs := make([]string, 0, len(it.Locations))
		for _, l := range it.Locations {
			s := joinNonEmpty(l.City, l.Country)
			if s == "" {
				s = strings.TrimSpace(l.Name)
			}
			if s != "" {
				locs = append(locs, s)
			}
		}
		p.Location = withWorkplace(strings.Join(locs, "; "), teamtailorWorkplace(it.RemoteStatus))
		out = append(out, p)
	}
	return out, nil
}

// teamtailorWorkplace maps remoteStatus to a work* constant. "none" and "hybrid" are seen
// in a live feed; "fully" (and "remote", for safety) mean remote. "temporary" (remote for
// now) and anything else state no workplace, so the gates read the text.
func teamtailorWorkplace(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "none":
		return workOnsite
	case "hybrid":
		return workHybrid
	case "fully", "remote":
		return workRemote
	}
	return ""
}

// teamtailorPubDates are the RSS date forms accepted, numeric zone first, each also with
// a one-digit day.
var teamtailorPubDates = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
}

// teamtailorTime reads pubDate as UTC, falling back to RFC 3339; nil when empty or not
// understood.
func teamtailorTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range teamtailorPubDates {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return parseTime(s)
}

// teamtailorIDFromLink takes the job number from a link's last segment,
// ".../jobs/<number>-<title-slug>"; "" when the segment does not start with digits.
func teamtailorIDFromLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return ""
	}
	seg := path.Base(strings.TrimSuffix(u.Path, "/"))
	num, _, _ := strings.Cut(seg, "-")
	if num == "" {
		return ""
	}
	for _, r := range num {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return num
}
