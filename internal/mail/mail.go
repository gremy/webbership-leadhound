// Package mail sends the morning brief: through Resend when a key is set, and otherwise to
// an outbox directory that sends nothing and keeps a readable copy of each message.
//
// Messages carry a plain-text body and an optional minimal HTML copy. Nothing here adds
// tracking pixels, tags or custom headers.
package mail

import (
	"context"
	"errors"
	"net/mail"
)

// Message is one email.
type Message struct {
	To, Subject string
	Text        string // the plain-text body, always sent
	HTML        string // an optional HTML copy of the same content; "" sends text only
	FromName    string // shown as the sender's name
	// IdempotencyKey makes the provider send at most once per key (Resend keeps keys 24 h,
	// up to 256 characters): the brief passes "brief-<day>", so a rerun the same day sends
	// nothing new.
	IdempotencyKey string
}

// Mailer sends messages.
type Mailer interface {
	// Provider names the mailer ("resend" or "outbox").
	Provider() string
	// Send returns the provider's message id (the outbox's file name).
	Send(ctx context.Context, m Message) (string, error)
}

// ErrBadAddress: the recipient is not a single plain address.
var ErrBadAddress = errors.New("mail: invalid recipient")

// Check validates a recipient: one bare address, no display name, no list.
func Check(to string) error {
	if len(to) > 254 {
		return ErrBadAddress
	}
	a, err := mail.ParseAddress(to)
	if err != nil || a.Address != to {
		return ErrBadAddress
	}
	return nil
}
