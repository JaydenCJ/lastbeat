// Tests for the schedule-duration parser. Cron schedules live in days and
// weeks, so the extended units and strict validation both get exercised.
package duration

import (
	"testing"
	"time"
)

func TestParseSimpleUnits(t *testing.T) {
	cases := map[string]time.Duration{
		"90s": 90 * time.Second,
		"15m": 15 * time.Minute,
		"1h":  time.Hour,
		"1d":  24 * time.Hour,
		"2w":  14 * 24 * time.Hour,
	}
	for in, want := range cases {
		got, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("Parse(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseCompoundAndWhitespace(t *testing.T) {
	got, err := Parse("1d12h30m")
	if err != nil {
		t.Fatal(err)
	}
	if want := 36*time.Hour + 30*time.Minute; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, err := Parse("  5m "); err != nil || got != 5*time.Minute {
		t.Errorf("surrounding whitespace should be trimmed: %v, %v", got, err)
	}
}

func TestParseRejectsMalformedInput(t *testing.T) {
	// "60" is the classic typo (interval = "60"); it must fail loudly
	// instead of guessing a unit, and so must every unknown unit.
	for _, in := range []string{"", "60", "5x", "10ms", "3mo", "d", "m5"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
}

func TestParseRejectsUnitOrderAndRepeats(t *testing.T) {
	// "30m1h" is almost certainly a mistake; require each unit at most
	// once, largest first.
	for _, in := range []string{"30m1h", "1h2h", "1d1w"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
}

func TestParseRejectsNonPositiveValues(t *testing.T) {
	// A check needs a positive interval; zero, negative, fractional and
	// overflow-sized values are all schedule bugs.
	for _, in := range []string{"0s", "0h0m", "-5m", "1.5h", "99999999999d"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
}

func TestFormatRoundTrips(t *testing.T) {
	for _, in := range []string{"90s", "45m", "1h30m", "1d", "1d12h", "2w", "1w2d3h4m5s"} {
		d, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		back, err := Parse(Format(d))
		if err != nil {
			t.Fatalf("Format(%v)=%q did not re-parse: %v", d, Format(d), err)
		}
		if back != d {
			t.Errorf("round trip %q -> %q lost precision", in, Format(d))
		}
	}
}

func TestFormatNormalizes(t *testing.T) {
	d, _ := Parse("90s")
	if got := Format(d); got != "1m30s" {
		t.Errorf("Format(90s) = %q, want 1m30s", got)
	}
	if got := Format(0); got != "0s" {
		t.Errorf("Format(0) = %q, want 0s", got)
	}
}
