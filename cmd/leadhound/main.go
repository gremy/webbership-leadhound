// Command leadhound collects job listings, scores them and sends a daily brief.
//
// Usage: leadhound <command> [flags]. Each command lives in its own file in this package and
// registers itself in commands from an init function, so adding one never edits this file.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	_ "time/tzdata" // LEADHOUND_TZ resolves on a server image with no zoneinfo

	"github.com/gremy/webbership-leadhound/internal/config"
)

// command runs one subcommand with the arguments after its name.
type command func(ctx context.Context, args []string) error

// commands maps a subcommand name to its function. Each command file adds its own entry from
// init, for example commands["migrate"] = runMigrate.
var commands = map[string]command{}

// errUsage marks an error the caller can fix by changing the command line or the
// environment; main exits 2 for it, 1 for any other error.
var errUsage = errors.New("usage")

// usageError wraps a message so errors.Is(err, errUsage) holds.
func usageError(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, a...))
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// run dispatches args to a command and turns its result into an exit code: 0 ok, 1 failure,
// 2 usage.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmd, ok := commands[args[0]]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "leadhound: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
	err := cmd(ctx, args[1:])
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		_, _ = fmt.Fprintf(stderr, "leadhound %s: %v\n", args[0], err)
		return 2
	default:
		slog.Error("command failed", "command", args[0], "err", err)
		return 1
	}
}

// usage lists the registered commands on w.
func usage(w io.Writer) {
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	slices.Sort(names)
	list := strings.Join(names, ", ")
	if list == "" {
		list = "(none yet)"
	}
	_, _ = fmt.Fprintf(w, "usage: leadhound <command> [flags]\ncommands: %s\n", list)
}

// loadConfig reads the environment and checks the variables a command cannot run without.
// Any problem is a usage error listing every problem at once.
func loadConfig(required ...string) (config.Config, error) {
	cfg, problems := config.Load(os.Getenv)
	problems = append(problems, config.Require(cfg, required...)...)
	if len(problems) > 0 {
		return cfg, usageError("%s", strings.Join(problems, "; "))
	}
	return cfg, nil
}
