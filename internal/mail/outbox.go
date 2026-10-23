package mail

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// htmlMarker separates the text body from the HTML copy in an outbox file.
const htmlMarker = "----- HTML -----"

// maxPerDay bounds the search for a free file name.
const maxPerDay = 10000

// Outbox is the development mailer: nothing leaves the machine. Each message becomes one
// file, <day>-<n>.txt (n = 1, 2, … for that day, never overwriting an existing file), in Dir,
// which is created 0700; files are 0600. A file reads:
//
//	Subject: <subject>
//	Idempotency-Key: <key>      (only when the message has one)
//
//	<text body>
//
//	----- HTML -----            (only when the message has an HTML copy)
//	<html body>
//
// The recipient is checked but not written down: the file holds what the brief says, not
// where it would have gone.
type Outbox struct {
	Dir      string
	Location *time.Location   // the day's time zone; time.Local when nil
	Now      func() time.Time // time.Now when nil
}

// Provider is "outbox".
func (*Outbox) Provider() string { return "outbox" }

// Send writes the message and returns the file's name.
func (o *Outbox) Send(_ context.Context, m Message) (string, error) {
	if err := Check(m.To); err != nil {
		return "", err
	}
	if o.Dir == "" {
		return "", errors.New("mail: outbox has no directory")
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return "", fmt.Errorf("mail: outbox: %w", err)
	}
	now, loc := time.Now, o.Location
	if o.Now != nil {
		now = o.Now
	}
	if loc == nil {
		loc = time.Local
	}
	day := now().In(loc).Format(time.DateOnly)
	body := render(m)
	for n := 1; n <= maxPerDay; n++ {
		name := fmt.Sprintf("%s-%d.txt", day, n)
		// O_EXCL claims the name atomically, so two runs never write the same file.
		f, err := os.OpenFile(filepath.Join(o.Dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- Dir is configured, name is generated
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("mail: outbox: %w", err)
		}
		_, werr := f.WriteString(body)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return "", fmt.Errorf("mail: outbox %s: %w", name, werr)
		}
		return name, nil
	}
	return "", fmt.Errorf("mail: outbox: %d files already for %s", maxPerDay, day)
}

func render(m Message) string {
	var b strings.Builder
	b.WriteString("Subject: " + m.Subject + "\n")
	if m.IdempotencyKey != "" {
		b.WriteString("Idempotency-Key: " + m.IdempotencyKey + "\n")
	}
	b.WriteString("\n" + strings.TrimRight(m.Text, "\n") + "\n")
	if m.HTML != "" {
		b.WriteString("\n" + htmlMarker + "\n" + strings.TrimRight(m.HTML, "\n") + "\n")
	}
	return b.String()
}
