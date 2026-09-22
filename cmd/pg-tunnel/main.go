// Command pg-tunnel manages PostgreSQL development connections.
package main

import (
	"log"
	"os"

	"github.com/vertti/pg-tunnel/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:], os.Stdout); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
