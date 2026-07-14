// Tests for the HTTP API, driven through the real handler with an
// injected deterministic clock and a temp-dir state file.
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/lastbeat/internal/config"
	"github.com/JaydenCJ/lastbeat/internal/state"
)

var t0 = time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)

// newTestServer builds a server over a fresh temp state file with a
// controllable clock. Returns the server and a function to advance time.
func newTestServer(t *testing.T, extraCfg string) (*Server, *time.Time) {
	t.Helper()
	// extraCfg is spliced in before the check so top-level keys stay
	// top-level; [[alert]] tables work in either position.
	cfg, err := config.Parse(extraCfg + `
[[check]]
name = "backup"
interval = "1h"
grace = "10m"
`)
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	clock := t0
	srv, err := New(cfg, nil, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	return srv, &clock
}

func do(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPingKnownCheckReturnsOK(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv.Handler(), http.MethodPost, "/ping/backup", "", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
	// Cron one-liners use `curl $URL` with no -X POST; GET must work.
	if rec := do(t, srv.Handler(), http.MethodGet, "/ping/backup", "", nil); rec.Code != http.StatusOK {
		t.Errorf("GET ping returned %d", rec.Code)
	}
}

func TestPingUnknownCheckIs404(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv.Handler(), http.MethodPost, "/ping/typo", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"typo"`) {
		t.Errorf("404 body should name the check: %q", rec.Body.String())
	}
}

func TestPingBadRequests(t *testing.T) {
	srv, _ := newTestServer(t, "")
	for _, path := range []string{"/ping/", "/ping/a/b/c"} {
		if rec := do(t, srv.Handler(), http.MethodPost, path, "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", path, rec.Code)
		}
	}
	if rec := do(t, srv.Handler(), http.MethodDelete, "/ping/backup", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: got %d, want 405", rec.Code)
	}
}

func TestPingBodyBecomesNote(t *testing.T) {
	srv, _ := newTestServer(t, "")
	do(t, srv.Handler(), http.MethodPost, "/ping/backup", "synced 1234 files\n", nil)
	rec := do(t, srv.Handler(), http.MethodGet, "/status/backup", "", nil)
	var e StatusEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Note != "synced 1234 files" {
		t.Errorf("note = %q", e.Note)
	}
}

func TestPingKeyEnforcement(t *testing.T) {
	srv, _ := newTestServer(t, "ping_key = \"sekret\"\n")
	h := srv.Handler()
	if rec := do(t, h, http.MethodPost, "/ping/backup", "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("missing key: got %d, want 403", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/ping/backup", "", map[string]string{"X-Lastbeat-Key": "wrong"}); rec.Code != http.StatusForbidden {
		t.Errorf("wrong key: got %d, want 403", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/ping/backup", "", map[string]string{"X-Lastbeat-Key": "sekret"}); rec.Code != http.StatusOK {
		t.Errorf("header key: got %d, want 200", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/ping/backup?key=sekret", "", nil); rec.Code != http.StatusOK {
		t.Errorf("query key: got %d, want 200", rec.Code)
	}
}

func TestFailEndpointMarksDown(t *testing.T) {
	srv, _ := newTestServer(t, "")
	do(t, srv.Handler(), http.MethodPost, "/ping/backup/fail", "exit 2", nil)
	rec := do(t, srv.Handler(), http.MethodGet, "/status/backup", "", nil)
	var e StatusEntry
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Status != state.StatusDown || e.Fails != 1 || e.Note != "exit 2" {
		t.Errorf("entry after fail: %+v", e)
	}
}

func TestStatusDocumentShape(t *testing.T) {
	srv, clock := newTestServer(t, "")
	do(t, srv.Handler(), http.MethodPost, "/ping/backup", "", nil)
	*clock = t0.Add(20 * time.Minute)
	rec := do(t, srv.Handler(), http.MethodGet, "/status", "", nil)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type: %q", ct)
	}
	var doc StatusDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Tool != "lastbeat" || doc.SchemaVersion != 1 || doc.Down != 0 {
		t.Errorf("envelope: %+v", doc)
	}
	if len(doc.Checks) != 1 || doc.Checks[0].DueIn != "40m0s" {
		t.Errorf("checks: %+v", doc.Checks)
	}
}

func TestStatusUnknownCheckIs404(t *testing.T) {
	srv, _ := newTestServer(t, "")
	if rec := do(t, srv.Handler(), http.MethodGet, "/status/typo", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", rec.Code)
	}
}

func TestSweepOnceTransitionsAndCountsDown(t *testing.T) {
	srv, clock := newTestServer(t, "")
	do(t, srv.Handler(), http.MethodPost, "/ping/backup", "", nil)
	*clock = t0.Add(3 * time.Hour)
	events := srv.SweepOnce()
	if len(events) != 1 || events[0].Type != config.EventDown {
		t.Fatalf("events: %v", events)
	}
	var doc StatusDoc
	rec := do(t, srv.Handler(), http.MethodGet, "/status", "", nil)
	json.Unmarshal(rec.Body.Bytes(), &doc)
	if doc.Down != 1 || doc.Checks[0].Overdue == "" {
		t.Errorf("status after down: %+v", doc)
	}
}

func TestStatePersistsAcrossServerRestarts(t *testing.T) {
	srv, _ := newTestServer(t, "")
	do(t, srv.Handler(), http.MethodPost, "/ping/backup", "", nil)
	// Same config (and thus the same state file), fresh server instance.
	srv2, err := New(srv.cfg, nil, func() time.Time { return t0.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, srv2.Handler(), http.MethodGet, "/status/backup", "", nil)
	var e StatusEntry
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Status != state.StatusUp || e.Pings != 1 {
		t.Errorf("state lost across restart: %+v", e)
	}
}

func TestHealthz(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := do(t, srv.Handler(), http.MethodGet, "/healthz", "", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Errorf("healthz: %d %q", rec.Code, rec.Body.String())
	}
}

func TestPingDispatchesRecoveryWebhook(t *testing.T) {
	// End-to-end inside one process: check goes down via sweep, then a
	// ping arrives and the recovery webhook fires.
	var got []byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
	}))
	defer hook.Close()
	srv, clock := newTestServer(t, `
[[alert]]
name = "hook"
url = "`+hook.URL+`"
events = ["recovered"]
`)
	do(t, srv.Handler(), http.MethodPost, "/ping/backup", "", nil)
	*clock = t0.Add(3 * time.Hour)
	srv.SweepOnce()
	do(t, srv.Handler(), http.MethodPost, "/ping/backup", "", nil)
	if !strings.Contains(string(got), `"event":"recovered"`) {
		t.Errorf("recovery webhook payload: %s", got)
	}
}
