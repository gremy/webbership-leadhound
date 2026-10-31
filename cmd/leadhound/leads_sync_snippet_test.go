package main

import (
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
)

// TestClassifyFetchedSnippet: a matched reply keeps its opening words (quotes and signature
// out); an auto-reply, a bounce, an opt-out and an unmatched message keep none.
func TestClassifyFetchedSnippet(t *testing.T) {
	at := "@"
	m := newMatcher([]leadstore.SentRef{{ID: 1, MessageID: "a1" + at + "sender.example", LeadID: 10,
		ToAddress: "info" + at + "acme.example", OrgDomain: "acme.example"}})
	reply := "From: Ana <ana" + at + "mail.acme.example>\r\nSubject: Re: hi\r\nIn-Reply-To: <a1" + at + "sender.example>\r\n"
	cases := []struct {
		name, header, text string
		kind               mailbox.MessageKind
		snippet            string
	}{
		{"matched reply", reply + "\r\n", "Bună, da, sună-mă marți.\r\n-- \r\nAna\r\n\r\nOn Mon, 26 Oct 2026 Sam wrote:\r\n> Hello",
			mailbox.KindReply, "Bună, da, sună-mă marți."},
		{"matched reply by domain", "From: Ana <ana" + at + "mail.acme.example>\r\nSubject: hello\r\n\r\n", "Tell me more.",
			mailbox.KindReply, "Tell me more."},
		{"auto-reply", reply + "Auto-Submitted: auto-replied\r\n\r\n", "I am away until Monday.", mailbox.KindAutoReply, ""},
		{"opt-out", reply + "\r\n", "Please remove me from your list.", mailbox.KindOptOut, ""},
		{"bounce", "From: Daemon <mailer-daemon" + at + "mx.acme.example>\r\nReturn-Path: <>\r\n\r\n", "Delivery failed.",
			mailbox.KindUnmatched, ""},
		{"unmatched", "From: Bo <bo" + at + "other.example>\r\nSubject: hello\r\n\r\n", "A question for you.", mailbox.KindUnmatched, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, _ := classifyFetched(mailbox.Fetched{UID: 9, Header: []byte(c.header), FirstText: []byte(c.text)}, m)
			if in.Kind != c.kind || in.Snippet != c.snippet {
				t.Errorf("got kind %q snippet %q; want %q %q", in.Kind, in.Snippet, c.kind, c.snippet)
			}
		})
	}
}
