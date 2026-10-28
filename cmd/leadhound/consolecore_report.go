package main

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// The console's Report link (CP6): the lead's upgrade-risk report, built from its stored
// facts by leadReportHTML (the one `leads report` writes), served to the signed-in operator
// as a file to save. It writes nothing and names no contact address: the operator's own
// addresses are secrets in leads.yaml, so the console's report carries none.

// LeadReport is lead id's upgrade-risk report and the file name to save it under,
// upgrade-risk-<domain>-<date>.html. No such lead is console.ErrNotFound; a lead with no
// report (no Drupal site finding, a major with no wording) is the refusal's words.
func (b *backend) LeadReport(ctx context.Context, id int64) (console.LeadReport, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return console.LeadReport{}, err
	}
	lq := b.lq()
	now := b.now()
	html, err := leadReportHTML(ctx, lq, setup.leads, id, "", now)
	if errors.Is(err, leadstore.ErrLeadNotFound) {
		return console.LeadReport{}, console.ErrNotFound
	}
	if err != nil {
		return console.LeadReport{}, err
	}
	name := "upgrade-risk-lead-" + strconv.FormatInt(id, 10)
	if l, err := lq.LeadByID(ctx, id); err == nil {
		if o, err := lq.OrgByID(ctx, l.OrgID); err == nil && o.Domain != "" {
			name = "upgrade-risk-" + o.Domain
		}
	}
	return console.LeadReport{Filename: name + "-" + now.In(b.loc).Format("2006-01-02") + ".html", HTML: html}, nil
}

// reportLink is whether lead id's page offers its report: Ready when the report builds, else
// the refusal's words; a report that fails for another reason is logged by lead id alone
// (the error may quote the page) and said plainly.
func (b *backend) reportLink(ctx context.Context, cfg leadconfig.Config, id int64) console.ReportLink {
	_, err := leadReportHTML(ctx, b.lq(), cfg, id, "", b.now())
	switch {
	case err == nil:
		return console.ReportLink{Ready: true}
	case errors.Is(err, console.ErrUserFixable):
		return console.ReportLink{Why: strings.TrimPrefix(strings.TrimPrefix(err.Error(), console.ErrUserFixable.Error()), ": ")}
	}
	slog.Default().Warn("console report not built", "lead", id)
	return console.ReportLink{Why: "The report could not be built from what is stored for this lead."}
}
