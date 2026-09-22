// Command pg-tunnel manages PostgreSQL development connections.
package main

import (
	"context"
	"errors"
	"log"
	"os"

	"github.com/vertti/pg-tunnel/internal/cli"
	"github.com/vertti/pg-tunnel/internal/process"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := process.SignalContext(context.Background())
	defer stop()
	if err := cli.RunContext(ctx, os.Args[1:], os.Stderr); err != nil {
		log.Print(err)
		return process.ExitCode(errors.Join(err, context.Cause(ctx)))
	}
	return 0
}
