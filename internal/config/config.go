// Package config loads and validates lastbeat.toml: the listener address,
// the state file location, the monitored checks, and the alert channels.
// Validation is strict — unknown keys, dangling alert references and
// malformed durations are load-time errors, never runtime surprises.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/JaydenCJ/lastbeat/internal/duration"
	"github.com/JaydenCJ/lastbeat/internal/toml"
)

// Event names an alert-worthy transition. See monitor for semantics.
const (
	EventDown      = "down"      // a check missed its deadline plus grace
	EventLate      = "late"      // a check missed its deadline, still in grace
	EventFailed    = "failed"    // a job reported an explicit failure
	EventRecovered = "recovered" // a down check pinged again
)

// AllEvents lists every event an alert may subscribe to.
var AllEvents = []string{EventDown, EventLate, EventFailed, EventRecovered}

// DefaultGrace applies when neither the check nor [defaults] sets one.
const DefaultGrace = 5 * time.Minute

// Check is one monitored job: it must ping at least every Interval, with
// Grace of slack before it is declared down.
type Check struct {
	Name     string
	Interval time.Duration
	Grace    time.Duration
	Alerts   []string // alert names; empty = every configured alert
	Tags     []string
}

// Alert is one notification channel: exactly one of URL (webhook, JSON
// POST) or Command (local process, JSON on stdin) is set.
type Alert struct {
	Name    string
	URL     string
	Command []string
	Events  []string // subscribed events; default: down, failed, recovered
	Timeout time.Duration
}

// WantsEvent reports whether the alert subscribes to the given event.
func (a Alert) WantsEvent(event string) bool {
	for _, e := range a.Events {
		if e == event {
			return true
		}
	}
	return false
}

// Config is the fully validated configuration.
type Config struct {
	Listen     string
	StateFile  string
	PingKey    string
	SweepEvery time.Duration
	Checks     []Check
	Alerts     []Alert
}

// CheckByName returns the named check, or nil.
func (c *Config) CheckByName(name string) *Check {
	for i := range c.Checks {
		if c.Checks[i].Name == name {
			return &c.Checks[i]
		}
	}
	return nil
}

// AlertsFor resolves the alert channels that should receive events for a
// check, preserving configuration order.
func (c *Config) AlertsFor(chk *Check) []Alert {
	if len(chk.Alerts) == 0 {
		return c.Alerts
	}
	wanted := map[string]bool{}
	for _, n := range chk.Alerts {
		wanted[n] = true
	}
	var out []Alert
	for _, a := range c.Alerts {
		if wanted[a.Name] {
			out = append(out, a)
		}
	}
	return out
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// LoadFile reads and validates a config file. A relative state_file is
// resolved against the config file's directory, so `lastbeat -c
// /etc/lastbeat/lastbeat.toml` works from any cwd.
func LoadFile(path string) (*Config, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !filepath.IsAbs(cfg.StateFile) {
		cfg.StateFile = filepath.Join(filepath.Dir(path), cfg.StateFile)
	}
	return cfg, nil
}

// Parse decodes and validates configuration source text.
func Parse(src string) (*Config, error) {
	root, err := toml.Parse(src)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Listen:     "127.0.0.1:8377",
		StateFile:  "lastbeat.state.json",
		SweepEvery: time.Minute,
	}
	top := newDecoder(root, "top level")

	cfg.Listen = top.str("listen", cfg.Listen)
	cfg.StateFile = top.str("state_file", cfg.StateFile)
	cfg.PingKey = top.str("ping_key", "")
	cfg.SweepEvery = top.dur("sweep_every", cfg.SweepEvery)

	defaultGrace := DefaultGrace
	if dt := top.table("defaults"); dt != nil {
		dd := newDecoder(dt, "[defaults]")
		defaultGrace = dd.dur("grace", defaultGrace)
		dd.finish()
		if dd.err != nil {
			return nil, dd.err
		}
	}

	for i, t := range top.tables("check") {
		d := newDecoder(t, fmt.Sprintf("[[check]] #%d", i+1))
		chk := Check{
			Name:     d.str("name", ""),
			Grace:    d.dur("grace", defaultGrace),
			Alerts:   d.strs("alerts"),
			Tags:     d.strs("tags"),
			Interval: d.dur("interval", 0),
		}
		if d.err == nil && chk.Name == "" {
			d.err = fmt.Errorf("[[check]] #%d: missing required key \"name\"", i+1)
		}
		if d.err == nil && chk.Interval == 0 {
			d.err = fmt.Errorf("check %q: missing required key \"interval\"", chk.Name)
		}
		d.finish()
		if d.err != nil {
			return nil, d.err
		}
		cfg.Checks = append(cfg.Checks, chk)
	}

	for i, t := range top.tables("alert") {
		d := newDecoder(t, fmt.Sprintf("[[alert]] #%d", i+1))
		al := Alert{
			Name:    d.str("name", ""),
			URL:     d.str("url", ""),
			Command: d.strs("command"),
			Events:  d.strs("events"),
			Timeout: d.dur("timeout", 10*time.Second),
		}
		if len(al.Events) == 0 {
			al.Events = []string{EventDown, EventFailed, EventRecovered}
		}
		if d.err == nil && al.Name == "" {
			d.err = fmt.Errorf("[[alert]] #%d: missing required key \"name\"", i+1)
		}
		d.finish()
		if d.err != nil {
			return nil, d.err
		}
		cfg.Alerts = append(cfg.Alerts, al)
	}

	top.finish()
	if top.err != nil {
		return nil, top.err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if len(c.Checks) == 0 {
		return fmt.Errorf("no [[check]] defined; lastbeat has nothing to watch")
	}
	seen := map[string]bool{}
	for _, chk := range c.Checks {
		if !nameRe.MatchString(chk.Name) {
			return fmt.Errorf("check name %q is invalid (letters, digits, . _ - only; must not start with punctuation)", chk.Name)
		}
		if seen[chk.Name] {
			return fmt.Errorf("duplicate check name %q", chk.Name)
		}
		seen[chk.Name] = true
		if chk.Interval < time.Second {
			return fmt.Errorf("check %q: interval must be at least 1s", chk.Name)
		}
	}
	alertNames := map[string]bool{}
	for _, a := range c.Alerts {
		if !nameRe.MatchString(a.Name) {
			return fmt.Errorf("alert name %q is invalid (letters, digits, . _ - only)", a.Name)
		}
		if alertNames[a.Name] {
			return fmt.Errorf("duplicate alert name %q", a.Name)
		}
		alertNames[a.Name] = true
		if (a.URL == "") == (len(a.Command) == 0) {
			return fmt.Errorf("alert %q: set exactly one of \"url\" or \"command\"", a.Name)
		}
		for _, e := range a.Events {
			if !contains(AllEvents, e) {
				return fmt.Errorf("alert %q: unknown event %q (valid: %v)", a.Name, e, AllEvents)
			}
		}
		if a.Timeout < time.Second {
			return fmt.Errorf("alert %q: timeout must be at least 1s", a.Name)
		}
	}
	for _, chk := range c.Checks {
		for _, ref := range chk.Alerts {
			if !alertNames[ref] {
				return fmt.Errorf("check %q references unknown alert %q", chk.Name, ref)
			}
		}
	}
	if c.SweepEvery < time.Second {
		return fmt.Errorf("sweep_every must be at least 1s")
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// decoder pulls typed values out of a toml.Table, accumulating the first
// error and tracking key usage so unknown keys can be reported.
type decoder struct {
	t    *toml.Table
	ctx  string
	used map[string]bool
	err  error
}

func newDecoder(t *toml.Table, ctx string) *decoder {
	return &decoder{t: t, ctx: ctx, used: map[string]bool{}}
}

func (d *decoder) fail(key, format string, args ...interface{}) {
	if d.err == nil {
		line := d.t.Line[key]
		d.err = fmt.Errorf("%s (line %d): %s", d.ctx, line, fmt.Sprintf(format, args...))
	}
}

func (d *decoder) str(key, def string) string {
	d.used[key] = true
	v, ok := d.t.Values[key]
	if !ok {
		return def
	}
	s, ok := v.(string)
	if !ok {
		d.fail(key, "%q must be a string", key)
		return def
	}
	return s
}

func (d *decoder) dur(key string, def time.Duration) time.Duration {
	d.used[key] = true
	v, ok := d.t.Values[key]
	if !ok {
		return def
	}
	s, ok := v.(string)
	if !ok {
		d.fail(key, "%q must be a duration string like \"15m\" or \"1d\"", key)
		return def
	}
	dd, err := duration.Parse(s)
	if err != nil {
		d.fail(key, "%q: %v", key, err)
		return def
	}
	return dd
}

func (d *decoder) strs(key string) []string {
	d.used[key] = true
	v, ok := d.t.Values[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]toml.Value)
	if !ok {
		d.fail(key, "%q must be an array of strings", key)
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			d.fail(key, "%q must contain only strings", key)
			return nil
		}
		out = append(out, s)
	}
	return out
}

func (d *decoder) table(key string) *toml.Table {
	d.used[key] = true
	v, ok := d.t.Values[key]
	if !ok {
		return nil
	}
	t, ok := v.(*toml.Table)
	if !ok {
		d.fail(key, "%q must be a [%s] table", key, key)
		return nil
	}
	return t
}

func (d *decoder) tables(key string) []*toml.Table {
	d.used[key] = true
	v, ok := d.t.Values[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]*toml.Table)
	if !ok {
		d.fail(key, "%q must be a [[%s]] array of tables", key, key)
		return nil
	}
	return arr
}

// finish flags unknown keys — the cheapest way to catch `internal = "1h"`
// style typos before they silently disable a check.
func (d *decoder) finish() {
	if d.err != nil {
		return
	}
	var unknown []string
	for k := range d.t.Values {
		if !d.used[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		d.fail(unknown[0], "unknown key %q", unknown[0])
	}
}
