package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/gremy/webbership-leadhound/internal/console"
)

func init() { commands["console"] = runConsole }

const consoleUsage = "usage: leadhound console hash-password"

// maxPasswordBytes is the longest password bcrypt reads whole.
const maxPasswordBytes = 72

// passwordInput is where hash-password reads the password; tests replace it.
var passwordInput = os.Stdin

// passwordPrompt is where hash-password asks for the password, so the hash alone goes to
// stdout.
var passwordPrompt io.Writer = os.Stderr

// runConsole is `leadhound console hash-password`: it reads the console password and
// prints only its bcrypt hash, for LEADHOUND_CONSOLE_PASSWORD_HASH. From a terminal it
// asks twice without echo; from a pipe it reads one line. A mismatch, fewer than 12
// characters or more than 72 bytes is a usage error and prints nothing.
func runConsole(_ context.Context, args []string) error {
	if len(args) != 1 || args[0] != "hash-password" {
		return usageError("%s", consoleUsage)
	}
	pw, err := readNewPassword(passwordInput)
	if err != nil {
		return err
	}
	switch {
	case utf8.RuneCount(pw) < console.MinPasswordLen:
		return usageError("the password must be at least %d characters", console.MinPasswordLen)
	case len(pw) > maxPasswordBytes:
		return usageError("the password must be at most %d bytes", maxPasswordBytes)
	}
	h, err := console.HashPassword(pw)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, h)
	return err
}

// readNewPassword asks twice without echo when f is a terminal, else reads one line.
func readNewPassword(f *os.File) ([]byte, error) {
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return readPasswordLine(f)
	}
	first, err := promptPassword(fd, "Password: ")
	if err != nil {
		return nil, err
	}
	again, err := promptPassword(fd, "Again: ")
	if err != nil {
		return nil, err
	}
	if string(first) != string(again) {
		return nil, usageError("the two passwords differ")
	}
	return first, nil
}

func promptPassword(fd int, prompt string) ([]byte, error) {
	_, _ = fmt.Fprint(passwordPrompt, prompt)
	pw, err := term.ReadPassword(fd)
	_, _ = fmt.Fprintln(passwordPrompt)
	if err != nil {
		return nil, fmt.Errorf("console: read the password: %w", err)
	}
	return pw, nil
}

// readPasswordLine reads the first line of r without its line ending; spaces stay, as
// they are part of the password.
func readPasswordLine(r io.Reader) ([]byte, error) {
	line, err := bufio.NewReader(r).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("console: read the password: %w", err)
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r")), nil
}
