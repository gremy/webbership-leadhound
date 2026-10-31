package main

import (
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// TestOverrideCodesInSync: the store's, the console's and the send's warning codes are one
// closed set, so no code the confirm page shows is one the store or the run refuses, and none
// the run would honour is missing from the page.
func TestOverrideCodesInSync(t *testing.T) {
	var run []string
	for _, c := range outreach.AllOverrideCodes() {
		run = append(run, string(c))
	}
	store, page := slices.Sorted(slices.Values(leadstore.OverrideCodes)), slices.Sorted(slices.Values(console.SendWarningCodes))
	slices.Sort(run)
	if !slices.Equal(store, page) || !slices.Equal(page, run) {
		t.Errorf("store %v, console %v, outreach %v", store, page, run)
	}
	if !slices.Equal(console.SendWarningCodes, leadstore.OverrideCodes) {
		t.Errorf("display order: console %v, store %v", console.SendWarningCodes, leadstore.OverrideCodes)
	}
}

// TestManualChanged: the press matches the plan only with its hash, every warning shown and no
// hard stop the page did not show; a plan that cannot send never matches.
func TestManualChanged(t *testing.T) {
	p := console.SendPlan{HashPrefix: "abcdef012345", CanSend: true,
		Warnings: []console.SendWarning{{Code: console.WarnRamp}, {Code: console.WarnWindow}},
		Leads:    []console.SendPlanLead{{LeadID: 1, OK: true}, {LeadID: 2, Stop: "x"}}}
	c := console.ManualConfirm{HashPrefix: "ABCDEF012345 ", Codes: []string{console.WarnWindow, console.WarnRamp}}
	shown := manualEntry{showned: true, shown: map[int64]bool{2: true}}
	if manualChanged(p, c, shown) {
		t.Error("the honest press changed")
	}
	for name, tc := range map[string]struct {
		p    console.SendPlan
		c    console.ManualConfirm
		seen manualEntry
	}{
		"hash":        {p, console.ManualConfirm{HashPrefix: "abcdef012346", Codes: c.Codes}, shown},
		"code":        {p, console.ManualConfirm{HashPrefix: c.HashPrefix, Codes: []string{console.WarnWindow}}, shown},
		"new stop":    {p, c, manualEntry{showned: true, shown: map[int64]bool{}}},
		"no memory":   {p, c, manualEntry{}},
		"cannot send": {func() console.SendPlan { q := p; q.CanSend = false; return q }(), c, shown},
	} {
		if !manualChanged(tc.p, tc.c, tc.seen) {
			t.Errorf("%s: not changed", name)
		}
	}
	// A warning gone since is no change; its code is simply not recorded.
	q := p
	q.Warnings = q.Warnings[:1]
	if manualChanged(q, c, shown) {
		t.Error("a warning gone since counts as a change")
	}
}
