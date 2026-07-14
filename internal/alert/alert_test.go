// Tests for alert delivery: payload construction, webhook POSTs against
// an in-process 127.0.0.1 receiver, command execution with placeholder
// expansion, and event/channel routing.
package alert

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/lastbeat/internal/config"
	"github.com/JaydenCJ/lastbeat/internal/monitor"
)

var t0 = time.Date(2026, 7, 13, 3, 10, 0, 0, time.UTC)

func downEvent() monitor.Event {
	return monitor.Event{
		Check:     "backup",
		Type:      config.EventDown,
		Status:    "down",
		Prev:      "late",
		At:        t0,
		LastPing:  t0.Add(-70 * time.Minute),
		OverdueBy: 10 * time.Minute,
	}
}

func testCheck() *config.Check {
	return &config.Check{
		Name:     "backup",
		Interval: time.Hour,
		Grace:    10 * time.Minute,
		Tags:     []string{"critical"},
	}
}

func TestBuildPayloadFields(t *testing.T) {
	p := BuildPayload(downEvent(), testCheck())
	if p.Tool != "lastbeat" || p.SchemaVersion != 1 {
		t.Errorf("envelope: %+v", p)
	}
	if p.Event != "down" || p.Check != "backup" || p.PrevStatus != "late" {
		t.Errorf("event fields: %+v", p)
	}
	if p.At != "2026-07-13T03:10:00Z" || p.LastPing != "2026-07-13T02:00:00Z" {
		t.Errorf("timestamps: at=%q last=%q", p.At, p.LastPing)
	}
	if p.OverdueSeconds != 600 {
		t.Errorf("overdue = %d, want 600", p.OverdueSeconds)
	}
	if p.Interval != "1h0m0s" || len(p.Tags) != 1 {
		t.Errorf("schedule fields: %+v", p)
	}
	// A never-pinged check has no last_ping and no overdue; those keys
	// must disappear from the JSON rather than encode as zero values.
	ev := downEvent()
	ev.LastPing = time.Time{}
	ev.OverdueBy = 0
	p = BuildPayload(ev, testCheck())
	if p.LastPing != "" || p.OverdueSeconds != 0 {
		t.Errorf("zero fields should stay empty: %+v", p)
	}
	body, _ := json.Marshal(p)
	if strings.Contains(string(body), "last_ping") || strings.Contains(string(body), "overdue_seconds") {
		t.Errorf("zero fields should be omitted from JSON: %s", body)
	}
}

// cfgWith wires one check and the given alerts into a minimal config.
func cfgWith(alerts ...config.Alert) *config.Config {
	return &config.Config{
		Checks: []config.Check{*testCheck()},
		Alerts: alerts,
	}
}

func webhookAlert(url string, events ...string) config.Alert {
	if len(events) == 0 {
		events = []string{config.EventDown, config.EventFailed, config.EventRecovered}
	}
	return config.Alert{Name: "hook", URL: url, Events: events, Timeout: 5 * time.Second}
}

func TestWebhookDeliveryPostsJSON(t *testing.T) {
	var gotBody []byte
	var gotReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotReq = r
	}))
	defer srv.Close()

	d := New()
	errs := d.Dispatch(cfgWith(webhookAlert(srv.URL)), downEvent())
	if len(errs) != 0 {
		t.Fatalf("delivery errors: %v", errs)
	}
	if gotReq.Method != http.MethodPost || gotReq.Header.Get("Content-Type") != "application/json" {
		t.Errorf("request shape: %s %v", gotReq.Method, gotReq.Header)
	}
	if !strings.HasPrefix(gotReq.Header.Get("User-Agent"), "lastbeat/") {
		t.Errorf("user agent: %q", gotReq.Header.Get("User-Agent"))
	}
	var p Payload
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if p.Check != "backup" || p.Event != "down" {
		t.Errorf("payload: %+v", p)
	}
}

func TestWebhookNon2xxIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	errs := New().Dispatch(cfgWith(webhookAlert(srv.URL)), downEvent())
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "500") {
		t.Errorf("want a 500 error, got %v", errs)
	}
}

func TestWebhookConnectionRefusedIsAnErrorNotAPanic(t *testing.T) {
	// Port 1 on loopback is essentially never listening; delivery must
	// come back as an error value the caller can log.
	al := webhookAlert("http://127.0.0.1:1/hook")
	al.Timeout = 2 * time.Second
	errs := New().Dispatch(cfgWith(al), downEvent())
	if len(errs) != 1 {
		t.Fatalf("want exactly one error, got %v", errs)
	}
	if !strings.Contains(errs[0].Error(), `alert "hook"`) {
		t.Errorf("error should name the channel: %v", errs[0])
	}
}

func TestCommandAlertExpandsPlaceholdersAndGetsStdin(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "alert.txt")
	script := filepath.Join(dir, "notify.sh")
	// The script records its argv and stdin so we can assert on both.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$2\" > \""+outFile+"\"\ncat >> \""+outFile+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	al := config.Alert{
		Name:    "cmd",
		Command: []string{script, "{check}", "{event}"},
		Events:  []string{config.EventDown},
		Timeout: 5 * time.Second,
	}
	errs := New().Dispatch(cfgWith(al), downEvent())
	if len(errs) != 0 {
		t.Fatalf("delivery errors: %v", errs)
	}
	out, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "backup down\n") {
		t.Errorf("placeholders not expanded: %q", out)
	}
	if !strings.Contains(string(out), `"schema_version":1`) {
		t.Errorf("stdin payload missing: %q", out)
	}
}

func TestCommandFailureSurfacesStderr(t *testing.T) {
	al := config.Alert{
		Name:    "cmd",
		Command: []string{"/bin/sh", "-c", "echo bad thing >&2; exit 3"},
		Events:  []string{config.EventDown},
		Timeout: 5 * time.Second,
	}
	errs := New().Dispatch(cfgWith(al), downEvent())
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "bad thing") {
		t.Errorf("stderr should be quoted in the error, got %v", errs)
	}
}

func TestDispatchSkipsUnsubscribedEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("webhook should not be called for an unsubscribed event")
	}))
	defer srv.Close()
	al := webhookAlert(srv.URL, config.EventRecovered) // only recovered
	if errs := New().Dispatch(cfgWith(al), downEvent()); len(errs) != 0 {
		t.Errorf("errors: %v", errs)
	}
}

func TestDispatchHonorsPerCheckAlertRouting(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()
	cfg := cfgWith(
		webhookAlert(srv.URL),
		config.Alert{Name: "other", URL: "http://127.0.0.1:1/never", Events: []string{config.EventDown}, Timeout: time.Second},
	)
	cfg.Checks[0].Alerts = []string{"hook"} // route only to the live one
	if errs := New().Dispatch(cfg, downEvent()); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if hits != 1 {
		t.Errorf("hits = %d, want 1", hits)
	}
}

func TestDispatchForRemovedCheckStillDelivers(t *testing.T) {
	// A check can be dropped from config between the event and the
	// dispatch; the alert must still go out to all channels.
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()
	cfg := cfgWith(webhookAlert(srv.URL))
	ev := downEvent()
	ev.Check = "no-longer-configured"
	if errs := New().Dispatch(cfg, ev); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if hits != 1 {
		t.Errorf("hits = %d, want 1", hits)
	}
}
