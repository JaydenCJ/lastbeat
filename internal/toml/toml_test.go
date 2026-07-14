// Tests for the TOML subset parser: everything a lastbeat.toml can
// contain, plus the explicit rejections for features outside the subset.
package toml

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) *Table {
	t.Helper()
	tbl, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse failed: %v\nsource:\n%s", err, src)
	}
	return tbl
}

func parseErr(t *testing.T, src, wantSubstr string) {
	t.Helper()
	_, err := Parse(src)
	if err == nil {
		t.Fatalf("expected error containing %q, got nil\nsource:\n%s", wantSubstr, src)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("error %q does not contain %q", err.Error(), wantSubstr)
	}
}

func TestParseScalarsAndQuotedKeys(t *testing.T) {
	tbl := mustParse(t, `
name = "backup"
count = 42
big = 1_000_000
neg = -7
enabled = true
disabled = false
raw = 'C:\path\no\escapes'
"weird key" = 1
`)
	if tbl.Values["name"] != "backup" {
		t.Errorf("name = %v", tbl.Values["name"])
	}
	if tbl.Values["count"] != int64(42) || tbl.Values["big"] != int64(1000000) || tbl.Values["neg"] != int64(-7) {
		t.Errorf("ints wrong: %v %v %v", tbl.Values["count"], tbl.Values["big"], tbl.Values["neg"])
	}
	if tbl.Values["enabled"] != true || tbl.Values["disabled"] != false {
		t.Error("bools wrong")
	}
	if tbl.Values["raw"] != `C:\path\no\escapes` {
		t.Errorf("literal string wrong: %v", tbl.Values["raw"])
	}
	if tbl.Values["weird key"] != int64(1) {
		t.Errorf("quoted key = %v", tbl.Values["weird key"])
	}
}

func TestParseStringEscapes(t *testing.T) {
	tbl := mustParse(t, `s = "a\tb\nc\"d\\eé"`)
	if got := tbl.Values["s"]; got != "a\tb\nc\"d\\eé" {
		t.Errorf("escapes decoded wrong: %q", got)
	}
}

func TestCommentsAndBlankLines(t *testing.T) {
	tbl := mustParse(t, `
# full-line comment
key = "value"   # trailing comment
hash = "a # not a comment"  # real comment
`)
	if tbl.Values["key"] != "value" {
		t.Errorf("key = %v", tbl.Values["key"])
	}
	if tbl.Values["hash"] != "a # not a comment" {
		t.Errorf("hash inside string was stripped: %v", tbl.Values["hash"])
	}
}

func TestTablesAndDottedHeaders(t *testing.T) {
	tbl := mustParse(t, `
top = 1
[server]
listen = "127.0.0.1:8377"
[server.tls]
enabled = false
`)
	srv := tbl.Values["server"].(*Table)
	if srv.Values["listen"] != "127.0.0.1:8377" {
		t.Errorf("listen = %v", srv.Values["listen"])
	}
	tls := srv.Values["tls"].(*Table)
	if tls.Values["enabled"] != false {
		t.Error("nested table value wrong")
	}
}

func TestArrayOfTables(t *testing.T) {
	tbl := mustParse(t, `
[[check]]
name = "a"
[[check]]
name = "b"
interval = "1h"
`)
	checks := tbl.Values["check"].([]*Table)
	if len(checks) != 2 {
		t.Fatalf("got %d checks, want 2", len(checks))
	}
	if checks[0].Values["name"] != "a" || checks[1].Values["name"] != "b" {
		t.Error("array-of-tables entries wrong")
	}
	if checks[1].Values["interval"] != "1h" {
		t.Error("second table lost its keys")
	}
}

func TestArraysInlineMultiLineEmpty(t *testing.T) {
	tbl := mustParse(t, `
tags = ["backup", "critical"]
none = []
events = [
  "down",     # when it stops
  "recovered" # when it comes back
]
`)
	tags := tbl.Values["tags"].([]Value)
	if len(tags) != 2 || tags[0] != "backup" || tags[1] != "critical" {
		t.Errorf("inline array = %v", tags)
	}
	if none := tbl.Values["none"].([]Value); len(none) != 0 {
		t.Errorf("empty array = %v", none)
	}
	events := tbl.Values["events"].([]Value)
	if len(events) != 2 || events[0] != "down" || events[1] != "recovered" {
		t.Errorf("multi-line array = %v", events)
	}
}

func TestDuplicatesRejectedWithPosition(t *testing.T) {
	parseErr(t, "a = 1\na = 2\n", "line 2")
	parseErr(t, "a = 1\na = 2\n", `key "a" already set on line 1`)
	parseErr(t, "[t]\na = 1\n[t]\nb = 2\n", "already defined")
}

func TestErrorsCarryLineNumbers(t *testing.T) {
	// The line number is the whole point of hand-rolling the parser:
	// config mistakes must be findable at a glance.
	_, err := Parse("ok = 1\nok2 = 2\nbroken\n")
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("want a line-3 error, got %v", err)
	}
}

func TestUnsupportedFeaturesRejectedExplicitly(t *testing.T) {
	parseErr(t, "pi = 3.14\n", "not supported")
	parseErr(t, "when = 2026-07-13\n", "not supported")
	parseErr(t, "point = { x = 1 }\n", "not supported")
	parseErr(t, "a.b = 1\n", "dotted keys are not supported")
	parseErr(t, "m = [[1], [2]]\n", "nested arrays are not supported")
}

func TestUnterminatedConstructs(t *testing.T) {
	parseErr(t, `s = "no end`, "unterminated string")
	parseErr(t, `s = 'no end`, "unterminated literal string")
	parseErr(t, "a = [1, 2\n", "unterminated array")
	parseErr(t, "[table\n", "missing the closing ]")
}

func TestMalformedLinesRejected(t *testing.T) {
	parseErr(t, `a = 1 extra`, "unexpected trailing")
	parseErr(t, `a = [1] extra`, "unexpected trailing")
	parseErr(t, `a = [,1]`, "unexpected ,")
	parseErr(t, `a = [1 2]`, "expected , or ]")
	parseErr(t, "a =\n", "missing value")
	parseErr(t, "just-a-key\n", "expected `key = value`")
}
