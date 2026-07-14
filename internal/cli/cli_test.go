// In-process integration tests for the CLI: real argument parsing, real
// config and state files in temp dirs, deterministic time via
// LASTBEAT_NOW, asserted exit codes and output.
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JaydenCJ/lastbeat/internal/version"
)

const testConfig = `
state_file = "state.json"

[[check]]
name = "backup"
interval = "1h"
grace = "10m"

[[check]]
name = "report"
interval = "1d"
`

// run executes the CLI and returns exit code, stdout, stderr.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// writeConfig drops a config into a temp dir and returns its path.
func writeConfig(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lastbeat.toml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVersionSubcommandAndFlag(t *testing.T) {
	want := "lastbeat " + version.Version + "\n"
	for _, args := range [][]string{{"version"}, {"--version"}, {"-V"}} {
		code, out, _ := run(t, args...)
		if code != ExitOK || out != want {
			t.Errorf("%v: code=%d out=%q", args, code, out)
		}
	}
}

func TestNoArgsAndUnknownCommandExit2(t *testing.T) {
	code, _, errOut := run(t)
	if code != ExitUsage || !strings.Contains(errOut, "Usage:") {
		t.Errorf("no args: code=%d err=%q", code, errOut)
	}
	code, _, errOut = run(t, "explode")
	if code != ExitUsage || !strings.Contains(errOut, `unknown command "explode"`) {
		t.Errorf("unknown command: code=%d err=%q", code, errOut)
	}
}

func TestHelpExitsZero(t *testing.T) {
	code, out, _ := run(t, "--help")
	if code != ExitOK || !strings.Contains(out, "dead-man's-switch") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestCommandHelpPrintsUsageAndExitsZero(t *testing.T) {
	// The top-level help promises `lastbeat <command> --help`; every
	// subcommand must honor it, including the ones that take no flags.
	for _, cmd := range []string{"init", "serve", "ping", "fail", "sweep", "status", "checks", "version"} {
		for _, flag := range []string{"-h", "--help"} {
			code, out, errOut := run(t, cmd, flag)
			if code != ExitOK || !strings.Contains(out, "usage: lastbeat "+cmd) {
				t.Errorf("%s %s: code=%d out=%q err=%q", cmd, flag, code, out, errOut)
			}
		}
	}
}

func TestMissingConfigIsRuntimeError(t *testing.T) {
	code, _, errOut := run(t, "-c", filepath.Join(t.TempDir(), "nope.toml"), "status")
	if code != ExitRuntime || !strings.Contains(errOut, "error:") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

func TestInitWritesValidConfigAndRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastbeat.toml")
	code, out, _ := run(t, "init", path)
	if code != ExitOK || !strings.Contains(out, path) {
		t.Fatalf("init failed: code=%d out=%q", code, out)
	}
	// The generated file must load cleanly...
	if code, _, errOut := run(t, "-c", path, "checks"); code != ExitOK {
		t.Fatalf("generated config does not load: %s", errOut)
	}
	// ...and a second init must not clobber user edits.
	code, _, errOut := run(t, "init", path)
	if code != ExitRuntime || !strings.Contains(errOut, "refusing to overwrite") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

func TestPingThenStatusShowsUp(t *testing.T) {
	cfgPath := writeConfig(t, testConfig)
	t.Setenv("LASTBEAT_NOW", "2026-07-13T03:00:00Z")
	code, out, errOut := run(t, "-c", cfgPath, "ping", "backup")
	if code != ExitOK || !strings.Contains(out, "backup: ok") {
		t.Fatalf("ping: code=%d out=%q err=%q", code, out, errOut)
	}
	t.Setenv("LASTBEAT_NOW", "2026-07-13T03:20:00Z")
	code, out, _ = run(t, "-c", cfgPath, "status")
	if code != ExitOK {
		t.Fatalf("status exit %d", code)
	}
	if !strings.Contains(out, "backup") || !strings.Contains(out, "up") || !strings.Contains(out, "in 40m0s") {
		t.Errorf("status output:\n%s", out)
	}
	if !strings.Contains(out, "waiting") || !strings.Contains(out, "never") {
		t.Errorf("unpinged check should show waiting/never:\n%s", out)
	}
	// The state file the ping produced is plain, versioned JSON.
	data, err := os.ReadFile(filepath.Join(filepath.Dir(cfgPath), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("state file is not JSON: %v", err)
	}
	if doc["schema_version"] != float64(1) {
		t.Errorf("schema_version = %v", doc["schema_version"])
	}
}

func TestPingUnknownCheckExits3(t *testing.T) {
	cfgPath := writeConfig(t, testConfig)
	code, _, errOut := run(t, "-c", cfgPath, "ping", "typo")
	if code != ExitRuntime || !strings.Contains(errOut, `unknown check "typo"`) {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

func TestSweepReportsDownWithOverdue(t *testing.T) {
	cfgPath := writeConfig(t, testConfig)
	t.Setenv("LASTBEAT_NOW", "2026-07-13T03:00:00Z")
	run(t, "-c", cfgPath, "ping", "backup")
	t.Setenv("LASTBEAT_NOW", "2026-07-13T06:00:00Z")
	code, out, _ := run(t, "-c", cfgPath, "sweep")
	if code != ExitOK {
		t.Fatalf("sweep exit %d", code)
	}
	if !strings.Contains(out, "backup is down") || !strings.Contains(out, "overdue by 2h") {
		t.Errorf("sweep output: %q", out)
	}
	// Second sweep at the same instant: edge-triggered, so all quiet.
	code, out, _ = run(t, "-c", cfgPath, "sweep")
	if code != ExitOK || !strings.Contains(out, "all quiet") {
		t.Errorf("repeat sweep: code=%d out=%q", code, out)
	}
}

func TestSweepJSONOutput(t *testing.T) {
	cfgPath := writeConfig(t, testConfig)
	t.Setenv("LASTBEAT_NOW", "2026-07-13T03:00:00Z")
	run(t, "-c", cfgPath, "ping", "backup")
	t.Setenv("LASTBEAT_NOW", "2026-07-13T06:00:00Z")
	_, out, _ := run(t, "-c", cfgPath, "sweep", "--json")
	var events []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &events); err != nil {
		t.Fatalf("sweep --json is not JSON: %v\n%s", err, out)
	}
	if len(events) != 1 || events[0]["check"] != "backup" || events[0]["event"] != "down" {
		t.Errorf("events: %v", events)
	}
	// A quiet sweep must still be valid JSON: an empty array, never null,
	// so `jq length` and similar consumers keep working.
	_, out, _ = run(t, "-c", cfgPath, "sweep", "--json")
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("quiet sweep --json = %q, want []", out)
	}
}

func TestStatusJSONAndFailOnDown(t *testing.T) {
	cfgPath := writeConfig(t, testConfig)
	t.Setenv("LASTBEAT_NOW", "2026-07-13T03:00:00Z")
	run(t, "-c", cfgPath, "ping", "backup")
	t.Setenv("LASTBEAT_NOW", "2026-07-13T06:00:00Z")
	run(t, "-c", cfgPath, "sweep")

	code, out, _ := run(t, "-c", cfgPath, "status", "--format", "json")
	if code != ExitOK {
		t.Fatalf("status exit %d", code)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("status --format json is not JSON: %v", err)
	}
	if doc["down"] != float64(1) {
		t.Errorf("down = %v", doc["down"])
	}
	// --fail-on-down turns a down check into exit code 1 for scripts.
	code, _, _ = run(t, "-c", cfgPath, "status", "--fail-on-down")
	if code != ExitBreach {
		t.Errorf("fail-on-down exit = %d, want %d", code, ExitBreach)
	}
}

func TestStatusTextPluralizesCounts(t *testing.T) {
	cfgPath := writeConfig(t, testConfig)
	t.Setenv("LASTBEAT_NOW", "2026-07-13T03:00:00Z")
	run(t, "-c", cfgPath, "ping", "backup")
	t.Setenv("LASTBEAT_NOW", "2026-07-13T06:00:00Z")
	run(t, "-c", cfgPath, "sweep")

	// Exactly one check is down: the footer must read "1 check down",
	// never "1 checks down" or the lazy "1 check(s) down".
	_, out, _ := run(t, "-c", cfgPath, "status")
	if !strings.Contains(out, "2 checks @") || !strings.Contains(out, "\n1 check down\n") {
		t.Errorf("status output:\n%s", out)
	}
}

func TestFailThenPingRecovers(t *testing.T) {
	cfgPath := writeConfig(t, testConfig)
	t.Setenv("LASTBEAT_NOW", "2026-07-13T03:00:00Z")
	code, out, _ := run(t, "-c", cfgPath, "fail", "backup", "--note", "disk full")
	if code != ExitOK || !strings.Contains(out, "backup: failed") {
		t.Fatalf("fail: code=%d out=%q", code, out)
	}
	t.Setenv("LASTBEAT_NOW", "2026-07-13T03:05:00Z")
	code, out, _ = run(t, "-c", cfgPath, "ping", "backup")
	if code != ExitOK || !strings.Contains(out, "recovered") {
		t.Errorf("recovery ping: code=%d out=%q", code, out)
	}
}

func TestSweepFiresCommandAlert(t *testing.T) {
	// Full pipeline through the real CLI: config with a command alert,
	// ping, then a sweep two hours later writes the alert to a file.
	dir := t.TempDir()
	outFile := filepath.Join(dir, "alerts.log")
	cfgPath := filepath.Join(dir, "lastbeat.toml")
	cfgSrc := `
state_file = "state.json"

[[check]]
name = "backup"
interval = "1h"
grace = "10m"

[[alert]]
name = "logfile"
command = ["/bin/sh", "-c", "cat >> ` + outFile + `"]
`
	if err := os.WriteFile(cfgPath, []byte(cfgSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LASTBEAT_NOW", "2026-07-13T03:00:00Z")
	run(t, "-c", cfgPath, "ping", "backup")
	t.Setenv("LASTBEAT_NOW", "2026-07-13T06:00:00Z")
	if code, _, errOut := run(t, "-c", cfgPath, "sweep"); code != ExitOK || errOut != "" {
		t.Fatalf("sweep: code=%d err=%q", code, errOut)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("alert command did not run: %v", err)
	}
	if !strings.Contains(string(data), `"event":"down"`) || !strings.Contains(string(data), `"check":"backup"`) {
		t.Errorf("alert payload: %s", data)
	}
}

func TestChecksListsSchedules(t *testing.T) {
	cfgPath := writeConfig(t, testConfig)
	code, out, _ := run(t, "-c", cfgPath, "checks")
	if code != ExitOK {
		t.Fatalf("checks exit %d", code)
	}
	if !strings.Contains(out, "2 checks") || !strings.Contains(out, "backup: every 1h (+10m grace)") {
		t.Errorf("checks output:\n%s", out)
	}
	if !strings.Contains(out, "report: every 1d") {
		t.Errorf("day-unit schedule missing:\n%s", out)
	}
}

func TestBadInputsExitUsage(t *testing.T) {
	cfgPath := writeConfig(t, testConfig)
	code, _, errOut := run(t, "-c", cfgPath, "status", "--verbose")
	if code != ExitUsage || !strings.Contains(errOut, "usage:") {
		t.Errorf("unknown flag: code=%d err=%q", code, errOut)
	}
	code, _, _ = run(t, "-c", cfgPath, "status", "--format", "yaml")
	if code != ExitUsage {
		t.Errorf("bad --format value: code=%d, want %d", code, ExitUsage)
	}
	t.Setenv("LASTBEAT_NOW", "yesterday-ish")
	code, _, errOut = run(t, "-c", cfgPath, "ping", "backup")
	if code != ExitUsage || !strings.Contains(errOut, "RFC3339") {
		t.Errorf("bad LASTBEAT_NOW: code=%d err=%q", code, errOut)
	}
}
