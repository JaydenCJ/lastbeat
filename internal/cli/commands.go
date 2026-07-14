// Subcommand implementations. Each returns a process exit code and writes
// only to the streams it is handed, keeping everything test-drivable.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/JaydenCJ/lastbeat/internal/alert"
	"github.com/JaydenCJ/lastbeat/internal/config"
	"github.com/JaydenCJ/lastbeat/internal/duration"
	"github.com/JaydenCJ/lastbeat/internal/monitor"
	"github.com/JaydenCJ/lastbeat/internal/server"
	"github.com/JaydenCJ/lastbeat/internal/state"
)

// sampleConfig is what `lastbeat init` writes: a real, working starting
// point that documents itself.
const sampleConfig = `# lastbeat.toml — see https://github.com/JaydenCJ/lastbeat
listen = "127.0.0.1:8377"              # serve mode binds loopback by default
state_file = "lastbeat.state.json"     # relative paths resolve next to this file
sweep_every = "1m"                     # how often serve mode looks for silence
# ping_key = "change-me"               # uncomment to require a shared key on pings

[defaults]
grace = "10m"                          # slack after a missed deadline

[[check]]
name = "nightly-backup"
interval = "24h"                       # must ping at least this often
grace = "45m"                          # backups get extra slack
tags = ["backup", "critical"]

[[check]]
name = "certs-renew"
interval = "7d"

[[alert]]
name = "ops-webhook"
url = "http://127.0.0.1:9090/hooks/lastbeat"
events = ["down", "failed", "recovered"]
`

func cmdInit(args []string, stdout, stderr io.Writer) int {
	path := "lastbeat.toml"
	if len(args) > 1 {
		fmt.Fprintln(stderr, "error: init takes at most one PATH argument")
		return ExitUsage
	}
	if len(args) == 1 {
		path = args[0]
	}
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(stderr, "error: %s already exists; refusing to overwrite\n", path)
		return ExitRuntime
	}
	if err := os.WriteFile(path, []byte(sampleConfig), 0o644); err != nil {
		return runtimeErr(stderr, err)
	}
	fmt.Fprintf(stdout, "wrote %s — edit the checks, then run: lastbeat -c %s serve\n", path, path)
	return ExitOK
}

func cmdPing(configPath string, args []string, failed bool, stdout, stderr io.Writer) int {
	flags, pos, err := parseFlags(args, map[string]bool{"note": true})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitUsage
	}
	if len(pos) != 1 {
		if failed {
			fmt.Fprintln(stderr, commandUsage["fail"])
		} else {
			fmt.Fprintln(stderr, commandUsage["ping"])
		}
		return ExitUsage
	}
	name := pos[0]
	cfg, ok := loadConfig(configPath, stderr)
	if !ok {
		return ExitRuntime
	}
	t, ok := now(stderr)
	if !ok {
		return ExitUsage
	}
	st, err := state.Load(cfg.StateFile)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	var ev *monitor.Event
	if failed {
		ev, err = monitor.RecordFail(cfg, st, name, flags["note"], t)
	} else {
		ev, err = monitor.RecordPing(cfg, st, name, flags["note"], t)
	}
	if err != nil {
		return runtimeErr(stderr, err)
	}
	if err := st.Save(cfg.StateFile, t); err != nil {
		return runtimeErr(stderr, err)
	}
	if ev != nil {
		dispatchEvents(cfg, []monitor.Event{*ev}, stderr)
		fmt.Fprintf(stdout, "%s: %s (%s -> %s)\n", name, ev.Type, ev.Prev, ev.Status)
	} else {
		fmt.Fprintf(stdout, "%s: ok\n", name)
	}
	return ExitOK
}

func cmdSweep(configPath string, args []string, stdout, stderr io.Writer) int {
	flags, pos, err := parseFlags(args, map[string]bool{"json": false})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n%s\n", err, commandUsage["sweep"])
		return ExitUsage
	}
	if len(pos) > 0 {
		fmt.Fprintln(stderr, commandUsage["sweep"])
		return ExitUsage
	}
	cfg, ok := loadConfig(configPath, stderr)
	if !ok {
		return ExitRuntime
	}
	t, ok := now(stderr)
	if !ok {
		return ExitUsage
	}
	st, err := state.Load(cfg.StateFile)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	events := monitor.Sweep(cfg, st, t)
	if err := st.Save(cfg.StateFile, t); err != nil {
		return runtimeErr(stderr, err)
	}
	dispatchEvents(cfg, events, stderr)
	if flags["json"] == "true" {
		if events == nil {
			events = []monitor.Event{} // encode a quiet sweep as [], not null
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(events)
		return ExitOK
	}
	if len(events) == 0 {
		fmt.Fprintln(stdout, "sweep: all quiet, no transitions")
		return ExitOK
	}
	for _, ev := range events {
		if ev.OverdueBy > 0 {
			fmt.Fprintf(stdout, "sweep: %s is %s (%s -> %s, overdue by %s)\n",
				ev.Check, ev.Type, ev.Prev, ev.Status, duration.Format(ev.OverdueBy))
		} else {
			fmt.Fprintf(stdout, "sweep: %s is %s (%s -> %s)\n", ev.Check, ev.Type, ev.Prev, ev.Status)
		}
	}
	return ExitOK
}

func dispatchEvents(cfg *config.Config, events []monitor.Event, stderr io.Writer) {
	d := alert.New()
	for _, ev := range events {
		for _, err := range d.Dispatch(cfg, ev) {
			fmt.Fprintf(stderr, "warning: %v\n", err)
		}
	}
}

func cmdStatus(configPath string, args []string, stdout, stderr io.Writer) int {
	flags, pos, err := parseFlags(args, map[string]bool{"format": true, "fail-on-down": false})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n%s\n", err, commandUsage["status"])
		return ExitUsage
	}
	if len(pos) > 0 {
		fmt.Fprintln(stderr, commandUsage["status"])
		return ExitUsage
	}
	format := flags["format"]
	if format == "" {
		format = "text"
	}
	if format != "text" && format != "json" {
		fmt.Fprintf(stderr, "error: unknown --format %q (text or json)\n", format)
		return ExitUsage
	}
	cfg, ok := loadConfig(configPath, stderr)
	if !ok {
		return ExitRuntime
	}
	t, ok := now(stderr)
	if !ok {
		return ExitUsage
	}
	st, err := state.Load(cfg.StateFile)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	doc := server.BuildStatus(cfg, st, t)
	if format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(doc)
	} else {
		renderStatusText(stdout, doc)
	}
	if flags["fail-on-down"] == "true" && doc.Down > 0 {
		return ExitBreach
	}
	return ExitOK
}

// plural renders a count with its noun ("1 check", "3 checks") so output
// never reads "1 checks" or resorts to "check(s)".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func renderStatusText(w io.Writer, doc server.StatusDoc) {
	fmt.Fprintf(w, "lastbeat status — %s @ %s\n\n", plural(len(doc.Checks), "check"), doc.At)
	nameW, lastW := len("CHECK"), len("LAST PING")
	for _, e := range doc.Checks {
		if len(e.Check) > nameW {
			nameW = len(e.Check)
		}
		if len(lastPingCell(e)) > lastW {
			lastW = len(lastPingCell(e))
		}
	}
	fmt.Fprintf(w, "  %-*s  %-8s  %-*s  %s\n", nameW, "CHECK", "STATUS", lastW, "LAST PING", "DUE")
	for _, e := range doc.Checks {
		due := "—"
		switch {
		case e.Overdue != "":
			due = "overdue by " + e.Overdue
		case e.DueIn != "":
			due = "in " + e.DueIn
		}
		fmt.Fprintf(w, "  %-*s  %-8s  %-*s  %s\n", nameW, e.Check, e.Status, lastW, lastPingCell(e), due)
	}
	if doc.Down > 0 {
		fmt.Fprintf(w, "\n%s down\n", plural(doc.Down, "check"))
	}
}

func lastPingCell(e server.StatusEntry) string {
	if e.LastPing == "" {
		return "never"
	}
	return e.LastPing
}

func cmdChecks(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, commandUsage["checks"])
		return ExitUsage
	}
	cfg, ok := loadConfig(configPath, stderr)
	if !ok {
		return ExitRuntime
	}
	fmt.Fprintf(stdout, "%s, %s\n\n", plural(len(cfg.Checks), "check"), plural(len(cfg.Alerts), "alert channel"))
	for _, chk := range cfg.Checks {
		alerts := "all alerts"
		if len(chk.Alerts) > 0 {
			alerts = fmt.Sprintf("alerts %v", chk.Alerts)
		}
		fmt.Fprintf(stdout, "  %s: every %s (+%s grace) -> %s\n",
			chk.Name, duration.Format(chk.Interval), duration.Format(chk.Grace), alerts)
	}
	return ExitOK
}

func cmdServe(configPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, commandUsage["serve"])
		return ExitUsage
	}
	cfg, ok := loadConfig(configPath, stderr)
	if !ok {
		return ExitRuntime
	}
	logger := log.New(stdout, "", log.LstdFlags)
	srv, err := server.New(cfg, logger, nil)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		return runtimeErr(stderr, err)
	}
	fmt.Fprintln(stdout, "lastbeat: shut down cleanly")
	return ExitOK
}
