// Package cli implements the pg-tunnel command-line entry point.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime/debug"
)

// ErrUsage marks command-line mistakes, which exit with status 2.
var ErrUsage = errors.New("usage")

const help = `pg-tunnel connects PostgreSQL clients to private AWS RDS databases through SSM.

Usage:
  pg-tunnel run [--config PATH] CONNECTION -- COMMAND [ARGS...]
      Open a tunnel, run COMMAND with libpq settings, and clean up when it exits.
  pg-tunnel connect [--config PATH] CONNECTION
      Keep a tunnel open and print settings for separately launched clients.
  pg-tunnel init [--aws-profile PROFILE] [--region REGION] [--config PATH] [--sslrootcert PEM]
      Discover AWS resources, test the connection, and save it.
  pg-tunnel cleanup
      Remove credential files left behind by crashed sessions.
  pg-tunnel --version

CONNECTION is a name saved by init in pg-tunnel.json, not an AWS profile.
Run "pg-tunnel COMMAND --help" for the options of a command.

Options:
`

func usageError(format string, args ...any) error {
	return fmt.Errorf("%w: %s (see pg-tunnel --help)", ErrUsage, fmt.Sprintf(format, args...))
}

// RunContext executes commands until completion or cancellation.
func RunContext(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return usageError("missing command")
	}
	switch args[0] {
	case "run", "connect":
		return runSession(ctx, args[0], args[1:], output)
	case "init":
		return initProfile(ctx, args[1:], output)
	case "cleanup":
		if len(args) != 1 {
			return usageError("cleanup takes no arguments")
		}
		return cleanup(output)
	case "help":
		return rootOptions([]string{"--help"}, output)
	}
	if args[0] == "" || args[0][0] != '-' {
		return usageError("unknown command %q", args[0])
	}
	return rootOptions(args, output)
}

func rootOptions(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("pg-tunnel", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		fmt.Fprint(output, help) //nolint:errcheck // flag.Usage has no error return; normal command output errors are returned separately.
		flags.PrintDefaults()
	}
	version := flags.Bool("version", false, "print version and build commit")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return usageError("unexpected argument %q", flags.Arg(0))
	}
	if !*version {
		return usageError("missing command")
	}

	info, _ := debug.ReadBuildInfo()
	if _, err := fmt.Fprintln(output, buildVersion(info)); err != nil {
		return fmt.Errorf("write version: %w", err)
	}

	return nil
}

func buildVersion(info *debug.BuildInfo) string {
	version := "dev"
	if info == nil {
		return "pg-tunnel " + version
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			return "pg-tunnel " + version + " (" + setting.Value + ")"
		}
	}
	return "pg-tunnel " + version
}
