package mailbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// maxPerDay bounds the search for a free file name.
const maxPerDay = 10000

// Outbox is where a shadow send puts messages: nothing leaves the machine. Each message is
// one file, <day>-<n>.eml (n = 1, 2, … for that day, never overwriting a file already
// there), in Dir, created 0700; files are 0600. The file is the complete message Build
// wrote, byte for byte, so a mail client opens it as it would arrive.
type Outbox struct {
	Dir      string
	Location *time.Location   // the day's time zone; time.Local when nil
	Now      func() time.Time // time.Now when nil
}

// Write stores raw and returns the file's name.
func (o *Outbox) Write(raw []byte) (string, error) {
	if o.Dir == "" {
		return "", errors.New("mailbox: outbox has no directory")
	}
	if len(raw) == 0 {
		return "", errors.New("mailbox: outbox: empty message")
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return "", fmt.Errorf("mailbox: outbox: %w", err)
	}
	// A directory that was already there is narrowed to the owner too.
	if err := os.Chmod(o.Dir, 0o700); err != nil { // #nosec G302 -- a directory: 0700 is owner-only, and it needs x
		return "", fmt.Errorf("mailbox: outbox: %w", err)
	}
	now, loc := time.Now, o.Location
	if o.Now != nil {
		now = o.Now
	}
	if loc == nil {
		loc = time.Local
	}
	day := now().In(loc).Format(time.DateOnly)
	for n := 1; n <= maxPerDay; n++ {
		name := fmt.Sprintf("%s-%d.eml", day, n)
		// O_EXCL claims the name atomically, so two runs never write the same file.
		f, err := os.OpenFile(filepath.Join(o.Dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- Dir is configured, name is generated
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("mailbox: outbox: %w", err)
		}
		_, werr := f.Write(raw)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return "", fmt.Errorf("mailbox: outbox %s: %w", name, werr)
		}
		return name, nil
	}
	return "", fmt.Errorf("mailbox: outbox: %d files already for %s", maxPerDay, day)
}
