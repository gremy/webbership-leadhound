package igsignal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

func testLeads() []LeadKey {
	return []LeadKey{
		{LeadID: 1, OrgName: "Cafe Exemplu", Town: "Târgu Exemplu", Domain: "cafe-exemplu.example", Handles: []string{"cafe.exemplu"}},
		{LeadID: 2, OrgName: "Roastery Example SRL", SiteName: "Roastery Example", Town: "Cluj-Napoca", Domain: "roastery-example.example"},
		{LeadID: 3, OrgName: "Cafeneaua Exemplu", Town: "Târgu Exemplu"},
		{LeadID: 4, PlaceName: "Cafeneaua Exemplu", Town: "Arad"},
		{LeadID: 5, PlaceName: "Unique Exemplu", Town: "Iasi"},
		{LeadID: 6, OrgName: "Twin One", Handles: []string{"twin.exemplu"}},
		{LeadID: 7, OrgName: "Twin Two", Handles: []string{"twin.exemplu", "other.exemplu"}},
		{LeadID: 8, OrgName: "Chain A", Domain: "chain-exemplu.example"},
		{LeadID: 9, OrgName: "Chain B", Domain: "chain-exemplu.example"},
		{LeadID: 10, OrgName: "Stampino"},
		// Leads whose stored domain is a host that is no café's own website: never matched by it.
		{LeadID: 11, OrgName: "L11", Domain: "beacons.ai"},
		{LeadID: 12, OrgName: "L12", Domain: "glovoapp.com"},
		{LeadID: 13, OrgName: "L13", Domain: "stampino.ro"},
		{LeadID: 14, OrgName: "L14", Domain: "loyalty-own.example"},
		{LeadID: 15, OrgName: "L15", Domain: "linktr.ee"},
		{LeadID: 16, OrgName: "L16", Domain: "google.com"},
	}
}

func ownConfig(t *testing.T) *cafe.Config {
	t.Helper()
	cfg, err := cafe.DefaultConfig().WithOwnVendor("own loyalty product", []string{"loyalty-own.example"})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestMatch(t *testing.T) {
	m := NewMatcher(ownConfig(t), testLeads())
	for _, c := range []struct {
		name   string
		p      Profile
		tier   string
		ids    []int64
		strong bool
	}{
		{"handle", Profile{Handle: "cafe.exemplu"}, leadstore.IGTierHandle, []int64{1}, true},
		{"handle beats domain and name", Profile{Handle: "cafe.exemplu", FullName: "Unique Exemplu",
			ExternalURL: "https://roastery-example.example/"}, leadstore.IGTierHandle, []int64{1}, true},
		{"domain, from a subdomain and a path", Profile{Handle: "roastery_example",
			ExternalURL: "https://shop.roastery-example.example/beans?x=1"}, leadstore.IGTierDomain, []int64{2}, true},
		{"domain beats name", Profile{Handle: "x_example", FullName: "Unique Exemplu",
			ExternalURL: "http://www.roastery-example.example"}, leadstore.IGTierDomain, []int64{2}, true},
		{"name and town in the bio", Profile{Handle: "x_example", FullName: "Cafeneaua Exemplu", Bio: "Cafea în Târgu Exemplu ☕"},
			leadstore.IGTierName, []int64{3}, false},
		{"name and town in the category", Profile{Handle: "x_example", FullName: "Cafeneaua exemplu!", Category: "Cafe · Arad"},
			leadstore.IGTierName, []int64{4}, false},
		{"town written without its accents", Profile{Handle: "x_example", FullName: "Cafeneaua Exemplu", Bio: "TARGU EXEMPLU"},
			leadstore.IGTierName, []int64{3}, false},
		{"both towns: candidates", Profile{Handle: "x_example", FullName: "Cafeneaua Exemplu", Bio: "Târgu Exemplu și Arad"},
			leadstore.IGTierName, []int64{3, 4}, false},
		{"a town inside a word is no town", Profile{Handle: "x_example", FullName: "Cafeneaua Exemplu", Bio: "Aradul nostru"},
			leadstore.IGTierNone, nil, false},
		{"a name two leads carry, no town: nothing", Profile{Handle: "x_example", FullName: "Cafeneaua Exemplu"},
			leadstore.IGTierNone, nil, false},
		{"a name only one lead carries", Profile{Handle: "x_example", FullName: "Unique  Exemplu"},
			leadstore.IGTierName, []int64{5}, false},
		{"a site name, diacritics folded", Profile{Handle: "x_example", FullName: "Roastery Exâmple", Bio: "Cluj Napoca"},
			leadstore.IGTierName, []int64{2}, false},
		{"name with accents", Profile{Handle: "x_example", FullName: "Café Exemplu"}, leadstore.IGTierName, []int64{1}, false},
		{"handle two leads hold: candidates", Profile{Handle: "twin.exemplu"}, leadstore.IGTierHandle, []int64{6, 7}, false},
		{"domain two leads share: candidates", Profile{Handle: "x_example", ExternalURL: "https://chain-exemplu.example/"},
			leadstore.IGTierDomain, []int64{8, 9}, false},
		{"no match", Profile{Handle: "x_example", FullName: "Nobody Exemplu"}, leadstore.IGTierNone, nil, false},
	} {
		r := m.Match(c.p)
		if r.Tier != c.tier || !slices.Equal(r.LeadIDs, c.ids) || r.Strong() != c.strong {
			t.Errorf("%s: %+v, want %s %v strong %v", c.name, r, c.tier, c.ids, c.strong)
		}
	}
}

// TestMatchExcludedHosts: a link-in-bio page, a social or map page, an ordering or loyalty
// vendor's host and the own product's host are no website of the café's own, whatever a lead
// holds as its domain.
func TestMatchExcludedHosts(t *testing.T) {
	m := NewMatcher(ownConfig(t), testLeads())
	for _, link := range []string{
		"https://beacons.ai/x_example", "https://linktr.ee/x_example", "https://app.glovoapp.com/ro/tm/x/",
		"https://stampino.ro/c/x", "https://cafe.loyalty-own.example/", "https://maps.google.com/?cid=1",
		"https://goo.gl/maps/x", "https://wa.me/0", "https://www.facebook.com/x.example", "https://bio.link/x",
		"https://taplink.cc/x", "https://lnk.bio/x", "https://wolt.com/ro/x", "https://tazz.ro/x", "https://www.ubereats.com/x",
		"not a link at all", "",
	} {
		if d := m.Domain(link); d != "" {
			t.Errorf("Domain(%q) = %q, want none", link, d)
		}
		if r := m.Match(Profile{Handle: "x_example", ExternalURL: link}); r.Tier != leadstore.IGTierNone {
			t.Errorf("%q matched %+v", link, r)
		}
	}
	if d := m.Domain("https://Shop.Roastery-Example.example/x"); d != "roastery-example.example" {
		t.Errorf("Domain = %q", d)
	}
}

// TestMatchNeverByLeadID: a capture's source.lead_id is a hint for the report, never a tier;
// a wrong one attaches nothing and changes no match.
func TestMatchNeverByLeadID(t *testing.T) {
	m := NewMatcher(nil, testLeads())
	if r := m.Match(Profile{Handle: "x_example", Source: Source{Type: SourceLead, LeadID: 5}}); r.Tier != leadstore.IGTierNone || len(r.LeadIDs) != 0 {
		t.Errorf("matched by lead_id alone: %+v", r)
	}
	if r := m.Match(Profile{Handle: "cafe.exemplu", Source: Source{Type: SourceLead, LeadID: 2}}); !slices.Equal(r.LeadIDs, []int64{1}) {
		t.Errorf("a wrong lead_id moved the match: %+v", r)
	}
}

// TestMatchFollow: a following-list account has only a handle and a name, so it matches on
// those tiers only.
func TestMatchFollow(t *testing.T) {
	m := NewMatcher(nil, testLeads())
	if r := m.MatchFollow(Follow{Handle: "other.exemplu"}); r.Tier != leadstore.IGTierHandle || !slices.Equal(r.LeadIDs, []int64{7}) {
		t.Errorf("handle: %+v", r)
	}
	if r := m.MatchFollow(Follow{Handle: "x_example", FullName: "Unique Exemplu"}); r.Tier != leadstore.IGTierName || r.Strong() {
		t.Errorf("name: %+v", r)
	}
	if r := m.MatchFollow(Follow{Handle: "x_example", FullName: "Cafeneaua Exemplu"}); r.Tier != leadstore.IGTierNone {
		t.Errorf("a tied name with no town to tell them apart: %+v", r)
	}
}

// TestMatchVendorsAndOwnCustomers: a vendor's own account matches no lead, even by a name a
// lead carries; a profile showing the operator's own product is flagged a customer, so the
// import never queues it for review, and still matches by its handle.
func TestMatchVendorsAndOwnCustomers(t *testing.T) {
	m := NewMatcher(ownConfig(t), testLeads())
	r := m.Match(Profile{Handle: "stampino.app", FullName: "Stampino"})
	if r.Tier != leadstore.IGTierNone || r.VendorAccount != "stampino" || len(r.LeadIDs) != 0 {
		t.Errorf("vendor account: %+v", r)
	}
	if r := m.MatchFollow(Follow{Handle: "pick.coupons"}); r.VendorAccount != "pick-coupons" {
		t.Errorf("vendor account in a list: %+v", r)
	}
	r = m.Match(Profile{Handle: "cafe.exemplu", Bio: "Card: loyalty-own.example/c/1"})
	if !r.OwnCustomer || !slices.Equal(r.LeadIDs, []int64{1}) {
		t.Errorf("own customer: %+v", r)
	}
	r = m.Match(Profile{Handle: "x_example", ExternalURL: "https://cafe.loyalty-own.example/"})
	if !r.OwnCustomer || r.Tier != leadstore.IGTierNone {
		t.Errorf("own customer with no lead: %+v", r)
	}
	if r := NewMatcher(nil, testLeads()).Match(Profile{Handle: "x_example", ExternalURL: "https://cafe.loyalty-own.example/"}); r.OwnCustomer {
		t.Errorf("the default list knows the own product: %+v", r)
	}
	if r := m.Match(Profile{Handle: "x_example", Bio: "@stampino.app"}); r.OwnCustomer {
		t.Errorf("a rival's customer flagged as own: %+v", r)
	}
}

// TestMatchStore: a result is the ig_match row SetIGMatch takes: one lead, candidates, or none.
func TestMatchStore(t *testing.T) {
	m := NewMatcher(nil, testLeads())
	for _, c := range []struct {
		p     Profile
		lead  int64
		cands []int64
		tier  string
	}{
		{Profile{Handle: "cafe.exemplu"}, 1, nil, leadstore.IGTierHandle},
		{Profile{Handle: "twin.exemplu"}, 0, []int64{6, 7}, leadstore.IGTierHandle},
		{Profile{Handle: "x_example", FullName: "Unique Exemplu"}, 5, nil, leadstore.IGTierName},
		{Profile{Handle: "x_example"}, 0, nil, leadstore.IGTierNone},
	} {
		got := m.Match(c.p).Store(c.p.Handle)
		if got.Handle != c.p.Handle || got.LeadID != c.lead || !slices.Equal(got.Candidates, c.cands) || got.Tier != c.tier ||
			got.Actor != leadstore.ActorSystem {
			t.Errorf("%s: %+v", c.p.Handle, got)
		}
	}
}

// TestNameKeyIsTheDuplicateKey is the contract-drift test between the matcher's name key and
// leadstore's duplicate check (duplicates.go): duplicateKey is unexported and reads the
// database's rows, so the test holds both ends to one function. duplicateKey must still key a
// place's name on cafe.NormalizeName(r.Name), and NameKey must give what cafe.NormalizeName
// gives on the same inputs. Change either normalisation and this fails.
func TestNameKeyIsTheDuplicateKey(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../leadstore/duplicates.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if d, ok := d.(*ast.FuncDecl); ok && d.Name.Name == "duplicateKey" {
			fn = d
		}
	}
	if fn == nil {
		t.Fatal("duplicates.go has no duplicateKey: the duplicate check changed; bring NameKey along")
	}
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		arg, aok := call.Args[0].(*ast.SelectorExpr)
		if ok && aok && sel.Sel.Name == "NormalizeName" && arg.Sel.Name == "Name" {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "cafe" {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Fatal("duplicateKey no longer keys the name on cafe.NormalizeName(r.Name); bring NameKey along")
	}
	for _, in := range []string{"Café  Néro!", "Prăjitoria Şoim", "Cafeneaua Șoim", "CAFE-EXEMPLU", "  ", "☕ Cafe ☕",
		"Cafe Exemplu S.R.L.", "Roastery\tExample", "Ïndrăgostiți 24/7"} {
		if got, want := NameKey(in), cafe.NormalizeName(in); got != want {
			t.Errorf("NameKey(%q) = %q, the duplicate check keys it %q", in, got, want)
		}
	}
}
