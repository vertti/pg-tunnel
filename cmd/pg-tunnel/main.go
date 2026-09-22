// Command pg-tunnel manages PostgreSQL development connections.
package main

import (
	"context"
	"errors"
	"log"
	"os"

	"github.com/vertti/pg-tunnel/internal/cli"
	"github.com/vertti/pg-tunnel/internal/process"
	"github.com/vertti/pg-tunnel/internal/ssmplugin"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) > 1 && os.Args[1] == ssmplugin.Command {
		if err := ssmplugin.Run(os.Args[2:], os.Stdout); err != nil {
			log.Print(err)
			return 1
		}
		return 0
	}
	ctx, stop := process.SignalContext(context.Background())
	defer stop()
	if err := cli.RunContext(ctx, os.Args[1:], os.Stderr); err != nil {
		log.Print(err)
		return process.ExitCode(errors.Join(err, context.Cause(ctx)))
	}
	return 0
}
