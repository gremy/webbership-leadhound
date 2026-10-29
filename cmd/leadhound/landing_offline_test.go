package main

import "context"

// No test in this package reaches the network for a mailbox domain's website: every check
// lands at https://<domain>/ unless a test swaps landingURL for its own.
func init() {
	landingURL = func(_ context.Context, domain string) (string, error) {
		return "https://" + domain + "/", nil
	}
}
