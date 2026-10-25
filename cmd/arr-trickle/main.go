// Command arr-trickle works through the Sonarr/Radarr/Lidarr missing and upgrade backlog a few
// searches at a time, within a daily budget.
package main

import (
	"os"

	"github.com/cstupi/arr-trickle/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr)) }
