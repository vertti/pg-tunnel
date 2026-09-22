// Package cli implements the pg-tunnel command-line entry point.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// ErrNotImplemented indicates that database tunneling is not available yet.
var ErrNotImplemented = errors.New("database tunneling is not implemented yet; use -help for available options")

// Run parses command-line options and writes help or version information.
func Run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("pg-tunnel", flag.ContinueOnError)
	flags.SetOutput(output)
	version := flags.Bool("version", false, "print the development version")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}

		return fmt.Errorf("parse options: %w", err)
	}

	if flags.NArg() > 0 || !*version {
		return ErrNotImplemented
	}

	if _, err := fmt.Fprintln(output, "pg-tunnel dev"); err != nil {
		return fmt.Errorf("write version: %w", err)
	}

	return nil
}
