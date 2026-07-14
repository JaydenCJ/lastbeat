// Package cli implements the lastbeat command-line interface. All
// subcommands run through Run so tests drive the real argument parsing,
// output rendering and exit codes in-process.
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/JaydenCJ/lastbeat/internal/config"
	"github.com/JaydenCJ/lastbeat/internal/version"
)

// Exit codes, documented in the README.
const (
	ExitOK      = 0
	ExitBreach  = 1 // status --fail-on-down found a down check
	ExitUsage   = 2
	ExitRuntime = 3
)

const usage = `lastbeat — dead-man's-switch monitor for cron jobs

Usage:
  lastbeat [-c FILE] <command> [flags] [args]

Commands:
  init [PATH]       write a starter lastbeat.toml (default ./lastbeat.toml)
  serve             listen for pings and sweep for silent checks
  ping NAME         record a heartbeat directly in the state file
  fail NAME         record an explicit job failure
  sweep             evaluate all checks once and fire due alerts
  status            show every check (text or --format json)
  checks            list the configured checks and their schedules
  version           print the version

Command flags:
  ping, fail   --note TEXT      attach a short note to the heartbeat
  sweep        --json           print the transitions as JSON
  status       --format FMT     output format: text (default) or json
               --fail-on-down   exit 1 if any check is down

Global flags:
  -c, --config FILE   config file (default "lastbeat.toml")

Run 'lastbeat <command> --help' for one command's usage.
`

// commandUsage is each subcommand's one-line usage, printed on --help
// (exit 0) and on argument errors (exit 2).
var commandUsage = map[string]string{
	"init":    "usage: lastbeat init [PATH]",
	"serve":   "usage: lastbeat serve",
	"ping":    "usage: lastbeat ping NAME [--note TEXT]",
	"fail":    "usage: lastbeat fail NAME [--note TEXT]",
	"sweep":   "usage: lastbeat sweep [--json]",
	"status":  "usage: lastbeat status [--format text|json] [--fail-on-down]",
	"checks":  "usage: lastbeat checks",
	"version": "usage: lastbeat version",
}

// Run executes the CLI and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	configPath := "lastbeat.toml"
	rest := args
	// Peel global flags off the front so `lastbeat -c x.toml status` works.
	for len(rest) > 0 {
		switch rest[0] {
		case "-c", "--config":
			if len(rest) < 2 {
				fmt.Fprintln(stderr, "error: -c needs a file argument")
				return ExitUsage
			}
			configPath = rest[1]
			rest = rest[2:]
		case "-h", "--help", "help":
			fmt.Fprint(stdout, usage)
			return ExitOK
		case "-V", "--version":
			fmt.Fprintf(stdout, "lastbeat %s\n", version.Version)
			return ExitOK
		default:
			goto commands
		}
	}
commands:
	if len(rest) == 0 {
		fmt.Fprint(stderr, usage)
		return ExitUsage
	}
	cmd, cmdArgs := rest[0], rest[1:]
	if len(cmdArgs) > 0 && (cmdArgs[0] == "-h" || cmdArgs[0] == "--help") {
		if u, ok := commandUsage[cmd]; ok {
			fmt.Fprintln(stdout, u)
			return ExitOK
		}
	}
	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "lastbeat %s\n", version.Version)
		return ExitOK
	case "init":
		return cmdInit(cmdArgs, stdout, stderr)
	case "serve":
		return cmdServe(configPath, cmdArgs, stdout, stderr)
	case "ping":
		return cmdPing(configPath, cmdArgs, false, stdout, stderr)
	case "fail":
		return cmdPing(configPath, cmdArgs, true, stdout, stderr)
	case "sweep":
		return cmdSweep(configPath, cmdArgs, stdout, stderr)
	case "status":
		return cmdStatus(configPath, cmdArgs, stdout, stderr)
	case "checks":
		return cmdChecks(configPath, cmdArgs, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown command %q\n\n%s", cmd, usage)
		return ExitUsage
	}
}

// now returns the wall clock, or the instant in LASTBEAT_NOW (RFC3339)
// when set. The override makes alert pipelines rehearsable: you can replay
// "what happens at 03:00 tomorrow if the backup stays silent" today.
func now(stderr io.Writer) (time.Time, bool) {
	v := os.Getenv("LASTBEAT_NOW")
	if v == "" {
		return time.Now(), true
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		fmt.Fprintf(stderr, "error: LASTBEAT_NOW %q is not RFC3339 (e.g. 2026-07-13T06:00:00Z)\n", v)
		return time.Time{}, false
	}
	return t, true
}

func loadConfig(path string, stderr io.Writer) (*config.Config, bool) {
	cfg, err := config.LoadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return nil, false
	}
	return cfg, true
}

// runtimeErr prints an error and returns the runtime exit code.
func runtimeErr(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "error: %v\n", err)
	return ExitRuntime
}

// parseFlags is a tiny long/short flag scanner for subcommands: flags may
// appear before or after positionals; "--" ends flag parsing.
func parseFlags(args []string, spec map[string]bool) (flags map[string]string, pos []string, err error) {
	flags = map[string]string{}
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			i++
			continue
		}
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			val := name[eq+1:]
			name = name[:eq]
			needsValue, ok := spec[name]
			if !ok {
				return nil, nil, fmt.Errorf("unknown flag --%s", name)
			}
			if !needsValue {
				return nil, nil, fmt.Errorf("flag --%s takes no value", name)
			}
			flags[name] = val
			i++
			continue
		}
		needsValue, ok := spec[name]
		if !ok {
			return nil, nil, fmt.Errorf("unknown flag --%s", name)
		}
		if !needsValue {
			flags[name] = "true"
			i++
			continue
		}
		if i+1 >= len(args) {
			return nil, nil, fmt.Errorf("flag --%s needs a value", name)
		}
		flags[name] = args[i+1]
		i += 2
	}
	return flags, pos, nil
}
