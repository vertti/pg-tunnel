// Package cli implements the pg-tunnel command-line entry point.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
)

// ErrUsage marks command-line mistakes, which exit with status 2.
var ErrUsage = errors.New("usage")

type usageErr string

func (e usageErr) Error() string { return string(e) }

// Is matches ErrUsage.
func (usageErr) Is(target error) bool { return target == ErrUsage }

const help = `pg-tunnel connects PostgreSQL clients to private AWS RDS and Aurora databases through SSM.

Usage:
  pg-tunnel run [--config PATH] CONNECTION -- COMMAND [ARGS...]
      Open a tunnel, run COMMAND with libpq settings, and clean up when it exits.
  pg-tunnel connect [--config PATH] CONNECTION
      Keep a tunnel open and print settings for separately launched clients.
  pg-tunnel check [--config PATH] CONNECTION
      Verify a saved connection and clean up before exiting.
  pg-tunnel init [--aws-profile PROFILE] [--region REGION] [--config PATH] [--sslrootcert PEM]
      Discover AWS resources, test the connection, and save it.
  pg-tunnel cleanup
      Remove credential files left behind by crashed sessions.
  pg-tunnel --version

CONNECTION is a name saved by init in pg-tunnel.json, not an AWS profile.
Run "pg-tunnel COMMAND --help" for the options of a command.

Options:
`

func usageError(command, format string, args ...any) error {
	return usageErr(fmt.Sprintf(format, args...) + " (see " + strings.TrimSpace("pg-tunnel "+command) + " --help)")
}

// RunContext executes commands until completion or cancellation. Requested help,
// the version, and connect's client settings go to stdout; everything else to stderr.
func RunContext(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError("", "missing command")
	}
	switch args[0] {
	case "run", "connect", "check":
		return runSession(ctx, args[0], args[1:], stdout, stderr)
	case "init":
		return initProfile(ctx, args[1:], stdout, stderr)
	case "cleanup":
		return cleanupCommand(args[1:], stdout, stderr)
	case "help":
		if len(args) > 1 {
			return RunContext(ctx, []string{args[1], "--help"}, stdout, stderr)
		}
		return rootOptions([]string{"--help"}, stdout)
	}
	if !strings.HasPrefix(args[0], "-") {
		return usageError("", "unknown command %q", args[0])
	}
	return rootOptions(args, stdout)
}

// parseFlags prints usage to stdout when help is requested; a flag error becomes
// one line pointing at the command's help.
func parseFlags(flags *flag.FlagSet, args []string, stdout io.Writer) (help bool, _ error) {
	usage := flags.Usage
	flags.Usage = func() {}
	flags.SetOutput(io.Discard)
	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		flags.SetOutput(stdout)
		usage()
		return true, nil
	}
	if err != nil {
		return false, usageError(strings.TrimPrefix(flags.Name(), "pg-tunnel"), "%v", err)
	}
	return false, nil
}

func rootOptions(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("pg-tunnel", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprint(flags.Output(), help) //nolint:errcheck // flag.Usage has no error return.
		flags.PrintDefaults()
	}
	version := flags.Bool("version", false, "print version and build commit")

	if help, err := parseFlags(flags, args, stdout); help || err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return usageError("", "unexpected argument %q", flags.Arg(0))
	}
	if !*version {
		return usageError("", "missing command")
	}

	info, _ := debug.ReadBuildInfo()
	if _, err := fmt.Fprintln(stdout, buildVersion(info)); err != nil {
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
