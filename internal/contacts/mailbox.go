package contacts

import (
	"slices"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// mailboxProviderDomains are personal mailbox providers by registrable domain.
var mailboxProviderDomains = []string{
	"gmail.com", "googlemail.com", "icloud.com", "me.com", "aol.com", "mail.ru", "proton.me", "protonmail.com",
}

// mailboxProviderNames are personal mailbox providers whose domain varies by country
// (yahoo.com, yahoo.ro, hotmail.co.uk …): the registrable domain's first label.
var mailboxProviderNames = []string{"yahoo", "hotmail", "outlook", "live", "gmx", "yandex"}

// MailboxProvider reports whether domain is, or sits under, a personal mailbox provider's
// domain: an address there belongs to whoever holds the mailbox, never to a business by its
// domain. Extract needs no such list (an address off the org's own domain is foreign); a
// caller with no domain to hold an address to (a café with no website of its own) does.
func MailboxProvider(domain string) bool {
	d, err := sitelist.Registrable(domain)
	if err != nil {
		return false
	}
	if slices.Contains(mailboxProviderDomains, d) {
		return true
	}
	name, _, _ := strings.Cut(d, ".")
	return slices.Contains(mailboxProviderNames, name)
}
