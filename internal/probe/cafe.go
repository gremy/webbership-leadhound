package probe

import (
	"slices"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// SigCafeLang is the signal name of a cafe@1 probe's language: its value is
// "<iso>:<via>:<note>" (cafe.Result.Language, LanguageVia, LanguageNote). Every other cafe@1
// signal is named by its kind (cafe.SigLoyalty, SigOrdering, SigInstagram) and its value is
// cafe.Signal.String ("kind:vendor:via:match").
const SigCafeLang = "lang"

// CafeSignals are the site_probe signals of one cafe@1 reading, each citing fetchID (the
// homepage): what CafeResult reads back.
func CafeSignals(r cafe.Result, fetchID int64) []leadstore.Signal {
	out := make([]leadstore.Signal, 0, len(r.Signals)+1)
	for _, s := range r.Signals {
		out = append(out, leadstore.Signal{Signal: s.Kind, Value: s.String(), FetchID: fetchID})
	}
	if r.Language != "" {
		out = append(out, leadstore.Signal{Signal: SigCafeLang, Value: r.Language + ":" + r.LanguageVia + ":" + r.LanguageNote, FetchID: fetchID})
	}
	return out
}

// CafeResult reads a cafe@1 probe's stored signals back into the detector's result, so the
// build classifies from the store (cafe.Classify) exactly as the probe saw the page. Signals
// of other names (the probe's own: redirect, robots …) are ignored. A loyalty signal of the
// own vendor (cafe.OwnVendorID) marks the café a customer.
func CafeResult(sigs []leadstore.Signal) cafe.Result {
	var r cafe.Result
	for _, sg := range sigs {
		if sg.Signal == SigCafeLang {
			parts := strings.SplitN(sg.Value, ":", 3)
			if len(parts) == 3 && parts[0] != "" {
				r.Language, r.LanguageVia, r.LanguageNote = parts[0], parts[1], parts[2]
			}
			continue
		}
		if sg.Signal != cafe.SigLoyalty && sg.Signal != cafe.SigOrdering && sg.Signal != cafe.SigInstagram {
			continue
		}
		parts := strings.SplitN(sg.Value, ":", 4)
		if len(parts) != 4 || parts[0] != sg.Signal {
			continue
		}
		s := cafe.Signal{Kind: parts[0], Vendor: parts[1], Via: parts[2], Match: parts[3]}
		r.Signals = append(r.Signals, s)
		switch s.Kind {
		case cafe.SigLoyalty:
			r.HasLoyalty = true
			if s.Via != cafe.ViaWord && !slices.Contains(r.Vendors, s.Vendor) {
				r.Vendors = append(r.Vendors, s.Vendor)
			}
			if s.Vendor == cafe.OwnVendorID && s.Via != cafe.ViaWord {
				r.OwnCustomer = true
			}
		case cafe.SigOrdering:
			r.HasOrdering = true
		case cafe.SigInstagram:
			r.HasInstagram = true
		}
	}
	return r
}

func cmpCafe(c *cafe.Config) *cafe.Config {
	if c == nil {
		return cafe.DefaultConfig()
	}
	return c
}

func cmpURL(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
