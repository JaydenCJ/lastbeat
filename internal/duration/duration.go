// Package duration parses and formats human-friendly durations for
// schedules: on top of Go's native units it understands days ("1d") and
// weeks ("2w"), because cron jobs are scheduled in days and weeks, not in
// nanoseconds.
package duration

import (
	"fmt"
	"strings"
	"time"
)

// unit maps a suffix to its length. Days and weeks are civil approximations
// (24h and 168h); lastbeat compares wall-clock instants, so DST shifts only
// move a deadline by at most an hour, which the grace window absorbs.
var units = []struct {
	suffix string
	length time.Duration
}{
	{"w", 7 * 24 * time.Hour},
	{"d", 24 * time.Hour},
	{"h", time.Hour},
	{"m", time.Minute},
	{"s", time.Second},
}

// Parse converts strings like "90s", "15m", "1h30m", "1d", "1d12h" or "2w"
// into a time.Duration. Components must appear in descending unit order,
// each unit at most once, values are non-negative integers, and the total
// must be positive. This is deliberately stricter than time.ParseDuration:
// schedule typos should fail loudly at config load, not at 3 a.m.
func Parse(s string) (time.Duration, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total time.Duration
	rest := in
	lastUnit := -1
	for rest != "" {
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("invalid duration %q: expected a number at %q", in, rest)
		}
		if i > 9 {
			return 0, fmt.Errorf("invalid duration %q: value too large", in)
		}
		var n int64
		for _, c := range rest[:i] {
			n = n*10 + int64(c-'0')
		}
		rest = rest[i:]
		if rest == "" {
			return 0, fmt.Errorf("invalid duration %q: missing unit (use s, m, h, d or w)", in)
		}
		ui := -1
		for idx, u := range units {
			if strings.HasPrefix(rest, u.suffix) {
				ui = idx
				break
			}
		}
		if ui < 0 {
			return 0, fmt.Errorf("invalid duration %q: unknown unit at %q (use s, m, h, d or w)", in, rest)
		}
		if ui <= lastUnit {
			return 0, fmt.Errorf("invalid duration %q: units must appear once, largest first", in)
		}
		lastUnit = ui
		rest = rest[len(units[ui].suffix):]
		total += time.Duration(n) * units[ui].length
	}
	if total <= 0 {
		return 0, fmt.Errorf("invalid duration %q: must be positive", in)
	}
	return total, nil
}

// Format renders a duration the same way Parse reads it: the largest units
// first, zero components omitted ("1d12h", "45m", "90s" -> "1m30s").
// Sub-second remainders are truncated; a zero duration renders as "0s".
func Format(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	var b strings.Builder
	rest := d
	for _, u := range units {
		if n := rest / u.length; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.suffix)
			rest -= n * u.length
		}
	}
	if b.Len() == 0 {
		return "0s"
	}
	return b.String()
}
