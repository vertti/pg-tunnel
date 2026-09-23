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

// ErrUsage indicates an incomplete or unsupported command.
var ErrUsage = errors.New("use pg-tunnel run [--config pg-tunnel.json] PROFILE -- COMMAND, connect PROFILE, init, or cleanup")

// Run parses command-line options and writes help or version information.
func Run(args []string, output io.Writer) error {
	return RunContext(context.Background(), args, output)
}

// RunContext executes commands until completion or cancellation.
func RunContext(ctx context.Context, args []string, output io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "run", "connect":
			return runSession(ctx, args[0], args[1:], output)
		case "init":
			return initProfile(ctx, args[1:], output)
		case "cleanup":
			if len(args) != 1 {
				return ErrUsage
			}
			return cleanup()
		}
	}
	return rootOptions(args, output)
}

func rootOptions(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("pg-tunnel", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: pg-tunnel run [--config pg-tunnel.json] PROFILE -- COMMAND")               //nolint:errcheck // flag.Usage has no error return; normal command output errors are returned separately.
		fmt.Fprintln(output, "       pg-tunnel connect [--config pg-tunnel.json] PROFILE")                      //nolint:errcheck // flag.Usage has no error return.
		fmt.Fprintln(output, "       pg-tunnel cleanup")                                                        //nolint:errcheck // flag.Usage has no error return.
		fmt.Fprintln(output, "       pg-tunnel init [--region REGION] [--aws-profile PROFILE] [--config PATH]") //nolint:errcheck // flag.Usage has no error return.
		flags.PrintDefaults()
	}
	version := flags.Bool("version", false, "print version and build commit")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}

		return fmt.Errorf("parse options: %w", err)
	}

	if flags.NArg() > 0 || !*version {
		return ErrUsage
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
