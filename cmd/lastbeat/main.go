// Command lastbeat is a dead-man's-switch monitor for cron jobs: jobs
// ping it after every run, and it alerts you the moment one goes silent.
package main

import (
	"os"

	"github.com/JaydenCJ/lastbeat/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
