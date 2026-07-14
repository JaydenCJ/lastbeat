// Tests for configuration loading and validation. Every rejection case
// here is a real 3 a.m. failure mode someone would otherwise hit.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validSrc = `
listen = "127.0.0.1:9000"
state_file = "beat.json"
sweep_every = "30s"

[defaults]
grace = "10m"

[[check]]
name = "nightly-backup"
interval = "24h"
grace = "45m"
tags = ["backup"]
alerts = ["ops"]

[[check]]
name = "certs-renew"
interval = "7d"

[[alert]]
name = "ops"
url = "http://127.0.0.1:9090/hook"
events = ["down", "recovered"]

[[alert]]
name = "local-log"
command = ["logger", "{check}", "{event}"]
`

func mustParse(t *testing.T, src string) *Config {
	t.Helper()
	cfg, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	return cfg
}

func parseErr(t *testing.T, src, wantSubstr string) {
	t.Helper()
	_, err := Parse(src)
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", wantSubstr)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("error %q does not contain %q", err.Error(), wantSubstr)
	}
}

func TestParseFullConfig(t *testing.T) {
	cfg := mustParse(t, validSrc)
	if cfg.Listen != "127.0.0.1:9000" || cfg.SweepEvery != 30*time.Second {
		t.Errorf("top-level: %+v", cfg)
	}
	if len(cfg.Checks) != 2 || len(cfg.Alerts) != 2 {
		t.Fatalf("got %d checks, %d alerts", len(cfg.Checks), len(cfg.Alerts))
	}
	b := cfg.Checks[0]
	if b.Name != "nightly-backup" || b.Interval != 24*time.Hour || b.Grace != 45*time.Minute {
		t.Errorf("check[0]: %+v", b)
	}
	if len(b.Tags) != 1 || b.Tags[0] != "backup" {
		t.Errorf("tags: %v", b.Tags)
	}
	// certs-renew sets no grace, so [defaults].grace = 10m applies.
	if cfg.Checks[1].Grace != 10*time.Minute {
		t.Errorf("default grace not applied: %v", cfg.Checks[1].Grace)
	}
}

func TestBuiltinDefaults(t *testing.T) {
	cfg := mustParse(t, "[[check]]\nname = \"a\"\ninterval = \"1h\"\n")
	if cfg.Listen != "127.0.0.1:8377" {
		t.Errorf("default listen should bind loopback, got %q", cfg.Listen)
	}
	if cfg.SweepEvery != time.Minute {
		t.Errorf("default sweep_every: %v", cfg.SweepEvery)
	}
	if cfg.Checks[0].Grace != DefaultGrace {
		t.Errorf("built-in grace: %v", cfg.Checks[0].Grace)
	}
}

func TestAlertDefaultEventsExcludeLate(t *testing.T) {
	// "late" is noisy; alerts only get it when explicitly subscribed.
	cfg := mustParse(t, validSrc)
	la := cfg.Alerts[1]
	if la.WantsEvent(EventLate) {
		t.Error("default events must not include late")
	}
	for _, e := range []string{EventDown, EventFailed, EventRecovered} {
		if !la.WantsEvent(e) {
			t.Errorf("default events missing %s", e)
		}
	}
}

func TestMissingPiecesRejected(t *testing.T) {
	parseErr(t, "listen = \"127.0.0.1:1\"\n", "nothing to watch")
	parseErr(t, "[[check]]\ninterval = \"1h\"\n", `missing required key "name"`)
	parseErr(t, "[[check]]\nname = \"a\"\n", `missing required key "interval"`)
}

func TestUnknownKeyIsCaughtWithLine(t *testing.T) {
	// The classic typo: "internal" instead of "interval" must not
	// silently disable a check, and the error must carry the line.
	parseErr(t, "[[check]]\nname = \"a\"\ninterval = \"1h\"\ninternal = \"2h\"\n", `unknown key "internal"`)
	parseErr(t, "[[check]]\nname = \"a\"\ninterval = \"1h\"\nbogus = 1\n", "line 4")
}

func TestDuplicateCheckNameRejected(t *testing.T) {
	parseErr(t, `
[[check]]
name = "a"
interval = "1h"
[[check]]
name = "a"
interval = "2h"
`, "duplicate check name")
}

func TestInvalidNamesRejected(t *testing.T) {
	parseErr(t, "[[check]]\nname = \"has space\"\ninterval = \"1h\"\n", "invalid")
	parseErr(t, "[[check]]\nname = \"-leading\"\ninterval = \"1h\"\n", "invalid")
}

func TestIntervalBounds(t *testing.T) {
	parseErr(t, "[[check]]\nname = \"a\"\ninterval = \"soon\"\n", "line 3")
	// 1s is the documented minimum and must be accepted.
	if _, err := Parse("[[check]]\nname = \"b\"\ninterval = \"1s\"\n"); err != nil {
		t.Fatalf("1s interval should be valid: %v", err)
	}
}

func TestAlertNeedsExactlyOneTransport(t *testing.T) {
	base := "[[check]]\nname = \"a\"\ninterval = \"1h\"\n[[alert]]\nname = \"x\"\n"
	parseErr(t, base, `set exactly one of "url" or "command"`)
	parseErr(t, base+"url = \"http://127.0.0.1/h\"\ncommand = [\"true\"]\n", `set exactly one of "url" or "command"`)
}

func TestUnknownEventRejected(t *testing.T) {
	parseErr(t, `
[[check]]
name = "a"
interval = "1h"
[[alert]]
name = "x"
url = "http://127.0.0.1/h"
events = ["exploded"]
`, `unknown event "exploded"`)
}

func TestDanglingAlertReferenceRejected(t *testing.T) {
	parseErr(t, `
[[check]]
name = "a"
interval = "1h"
alerts = ["ghost"]
`, `references unknown alert "ghost"`)
}

func TestAlertsForFiltersAndDefaults(t *testing.T) {
	cfg := mustParse(t, validSrc)
	withRef := cfg.CheckByName("nightly-backup")
	got := cfg.AlertsFor(withRef)
	if len(got) != 1 || got[0].Name != "ops" {
		t.Errorf("AlertsFor(nightly-backup) = %v", got)
	}
	noRef := cfg.CheckByName("certs-renew")
	if got := cfg.AlertsFor(noRef); len(got) != 2 {
		t.Errorf("check without alerts key should get all channels, got %d", len(got))
	}
}

func TestLoadFileResolvesRelativeStatePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lastbeat.toml")
	if err := os.WriteFile(path, []byte(validSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateFile != filepath.Join(dir, "beat.json") {
		t.Errorf("state file not resolved against config dir: %q", cfg.StateFile)
	}
}

func TestLoadFileErrorsNameTheFile(t *testing.T) {
	if _, err := LoadFile(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Error("missing config file should error")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(path, []byte("broken =\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "bad.toml") {
		t.Errorf("error should name the file: %v", err)
	}
}
