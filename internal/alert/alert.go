// Package alert delivers monitor events to the configured channels:
// webhooks (JSON POST) and local commands (JSON on stdin, placeholders in
// argv). Delivery is best-effort and bounded by per-alert timeouts —
// a broken alert channel must never take the monitor down with it.
//
// lastbeat performs no other outbound network traffic; webhooks are the
// single user-configured exception to fully-offline operation.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/JaydenCJ/lastbeat/internal/config"
	"github.com/JaydenCJ/lastbeat/internal/monitor"
	"github.com/JaydenCJ/lastbeat/internal/version"
)

// Payload is the JSON document delivered for every event. The schema is
// versioned and append-only within a major version, so receivers can rely
// on these fields.
type Payload struct {
	Tool           string   `json:"tool"`
	SchemaVersion  int      `json:"schema_version"`
	Event          string   `json:"event"`
	Check          string   `json:"check"`
	Status         string   `json:"status"`
	PrevStatus     string   `json:"prev_status"`
	At             string   `json:"at"`
	LastPing       string   `json:"last_ping,omitempty"`
	OverdueSeconds int64    `json:"overdue_seconds,omitempty"`
	Interval       string   `json:"interval"`
	Grace          string   `json:"grace"`
	Tags           []string `json:"tags,omitempty"`
	Note           string   `json:"note,omitempty"`
}

// BuildPayload assembles the wire payload for an event.
func BuildPayload(ev monitor.Event, chk *config.Check) Payload {
	p := Payload{
		Tool:          "lastbeat",
		SchemaVersion: 1,
		Event:         ev.Type,
		Check:         ev.Check,
		Status:        ev.Status,
		PrevStatus:    ev.Prev,
		At:            ev.At.UTC().Format(time.RFC3339),
		Interval:      chk.Interval.String(),
		Grace:         chk.Grace.String(),
		Tags:          chk.Tags,
		Note:          ev.Note,
	}
	if !ev.LastPing.IsZero() {
		p.LastPing = ev.LastPing.UTC().Format(time.RFC3339)
	}
	if ev.OverdueBy > 0 {
		p.OverdueSeconds = int64(ev.OverdueBy / time.Second)
	}
	return p
}

// Dispatcher fans events out to alert channels. The HTTP client is
// injectable so tests exercise real request construction against local
// in-process receivers.
type Dispatcher struct {
	Client *http.Client
}

// New returns a Dispatcher with a default client. Per-request timeouts
// come from each alert's configured timeout via context.
func New() *Dispatcher {
	return &Dispatcher{Client: &http.Client{}}
}

// Dispatch delivers one event to every subscribed channel for its check
// and returns one error per failed delivery. A missing check (removed
// from config mid-flight) delivers to all channels.
func (d *Dispatcher) Dispatch(cfg *config.Config, ev monitor.Event) []error {
	chk := cfg.CheckByName(ev.Check)
	if chk == nil {
		chk = &config.Check{Name: ev.Check}
	}
	var errs []error
	for _, al := range cfg.AlertsFor(chk) {
		if !al.WantsEvent(ev.Type) {
			continue
		}
		if err := d.deliver(al, ev, chk); err != nil {
			errs = append(errs, fmt.Errorf("alert %q: %w", al.Name, err))
		}
	}
	return errs
}

func (d *Dispatcher) deliver(al config.Alert, ev monitor.Event, chk *config.Check) error {
	payload := BuildPayload(ev, chk)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), al.Timeout)
	defer cancel()
	if al.URL != "" {
		return d.postWebhook(ctx, al.URL, body)
	}
	return runCommand(ctx, al.Command, payload, body)
}

func (d *Dispatcher) postWebhook(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "lastbeat/"+version.Version)
	resp, err := d.Client.Do(req)
	if err != nil {
		return fmt.Errorf("post webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}

// runCommand executes an alert command with {check}, {event}, {status} and
// {note} placeholders expanded in every argument, and the full JSON
// payload on stdin for receivers that want structure.
func runCommand(ctx context.Context, argv []string, p Payload, body []byte) error {
	expanded := make([]string, len(argv))
	replacer := strings.NewReplacer(
		"{check}", p.Check,
		"{event}", p.Event,
		"{status}", p.Status,
		"{note}", p.Note,
	)
	for i, a := range argv {
		expanded[i] = replacer.Replace(a)
	}
	cmd := exec.CommandContext(ctx, expanded[0], expanded[1:]...)
	// Newline-terminate stdin so `cat >> alerts.log` receivers get one
	// JSON document per line (webhook bodies stay bare JSON).
	cmd.Stdin = bytes.NewReader(append(append([]byte(nil), body...), '\n'))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("command failed: %v (%s)", err, msg)
		}
		return fmt.Errorf("command failed: %v", err)
	}
	return nil
}
