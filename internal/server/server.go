// Package server is the heartbeat listener: jobs hit /ping/<name> when
// they finish, and a background sweep declares silence. It binds
// 127.0.0.1 by default, keeps state on disk after every change, and
// exposes read-only status endpoints for dashboards and scripts.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/JaydenCJ/lastbeat/internal/alert"
	"github.com/JaydenCJ/lastbeat/internal/config"
	"github.com/JaydenCJ/lastbeat/internal/monitor"
	"github.com/JaydenCJ/lastbeat/internal/state"
	"github.com/JaydenCJ/lastbeat/internal/version"
)

// maxBody bounds ping request bodies (stored as the check's note).
const maxBody = 4096

// Server wires config, persisted state, the monitor and the alert
// dispatcher behind an http.Handler.
type Server struct {
	cfg        *config.Config
	dispatcher *alert.Dispatcher
	logger     *log.Logger
	now        func() time.Time // injectable clock for tests

	mu sync.Mutex
	st *state.State
}

// New loads state from the config's state file and returns a ready server.
func New(cfg *config.Config, logger *log.Logger, now func() time.Time) (*Server, error) {
	st, err := state.Load(cfg.StateFile)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if now == nil {
		now = time.Now
	}
	return &Server{cfg: cfg, st: st, dispatcher: alert.New(), logger: logger, now: now}, nil
}

// Handler returns the HTTP API:
//
//	GET|POST /ping/<name>       record a heartbeat (body -> note)
//	GET|POST /ping/<name>/fail  record an explicit failure
//	GET      /status            all checks as JSON
//	GET      /status/<name>     one check as JSON
//	GET      /healthz           liveness probe, "ok"
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping/", s.handlePing)
	mux.HandleFunc("/status", s.handleStatusAll)
	mux.HandleFunc("/status/", s.handleStatusOne)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	return mux
}

// Run serves the API on cfg.Listen and sweeps every cfg.SweepEvery until
// ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Listen, err)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	noun := "checks"
	if len(s.cfg.Checks) == 1 {
		noun = "check"
	}
	s.logger.Printf("lastbeat %s listening on http://%s (%d %s, sweep every %s)",
		version.Version, ln.Addr(), len(s.cfg.Checks), noun, s.cfg.SweepEvery)

	ticker := time.NewTicker(s.cfg.SweepEvery)
	defer ticker.Stop()
	s.SweepOnce() // catch anything that went silent while we were not running
	for {
		select {
		case <-ctx.Done():
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutCtx)
			return nil
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
			s.SweepOnce()
		}
	}
}

// SweepOnce evaluates all checks now, dispatches alerts for transitions,
// and persists state. Alert failures are logged, never fatal.
func (s *Server) SweepOnce() []monitor.Event {
	s.mu.Lock()
	now := s.now()
	events := monitor.Sweep(s.cfg, s.st, now)
	s.persistLocked(now)
	s.mu.Unlock()
	s.dispatchAll(events)
	return events
}

func (s *Server) dispatchAll(events []monitor.Event) {
	for _, ev := range events {
		s.logger.Printf("event: check=%s %s -> %s (%s)", ev.Check, ev.Prev, ev.Status, ev.Type)
		for _, err := range s.dispatcher.Dispatch(s.cfg, ev) {
			s.logger.Printf("alert delivery failed: %v", err)
		}
	}
}

func (s *Server) persistLocked(now time.Time) {
	if err := s.st.Save(s.cfg.StateFile, now); err != nil {
		s.logger.Printf("persist state: %v", err)
	}
}

// authorized checks the shared ping key, when one is configured. The key
// may arrive as the X-Lastbeat-Key header or a ?key= query parameter
// (for curl-only jobs that cannot set headers).
func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.PingKey == "" {
		return true
	}
	got := r.Header.Get("X-Lastbeat-Key")
	if got == "" {
		got = r.URL.Query().Get("key")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.PingKey)) == 1
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "missing or wrong ping key", http.StatusForbidden)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/ping/")
	name := rest
	failed := false
	if strings.HasSuffix(rest, "/fail") {
		name = strings.TrimSuffix(rest, "/fail")
		failed = true
	}
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, "bad ping path; use /ping/<check> or /ping/<check>/fail", http.StatusBadRequest)
		return
	}
	note := ""
	if r.Body != nil {
		b, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
		note = strings.TrimSpace(string(b))
	}

	s.mu.Lock()
	now := s.now()
	var ev *monitor.Event
	var err error
	if failed {
		ev, err = monitor.RecordFail(s.cfg, s.st, name, note, now)
	} else {
		ev, err = monitor.RecordPing(s.cfg, s.st, name, note, now)
	}
	if err != nil {
		s.mu.Unlock()
		http.Error(w, fmt.Sprintf("unknown check %q; add it to the config first", name), http.StatusNotFound)
		return
	}
	s.persistLocked(now)
	s.mu.Unlock()

	if ev != nil {
		s.dispatchAll([]monitor.Event{*ev})
	}
	fmt.Fprintln(w, "ok")
}

// StatusEntry is one check in the /status document.
type StatusEntry struct {
	Check    string `json:"check"`
	Status   string `json:"status"`
	LastPing string `json:"last_ping,omitempty"`
	DueIn    string `json:"due_in,omitempty"`
	Overdue  string `json:"overdue_by,omitempty"`
	Pings    int64  `json:"pings"`
	Fails    int64  `json:"fails"`
	Note     string `json:"note,omitempty"`
}

// StatusDoc is the /status response document.
type StatusDoc struct {
	Tool          string        `json:"tool"`
	SchemaVersion int           `json:"schema_version"`
	At            string        `json:"at"`
	Down          int           `json:"down"`
	Checks        []StatusEntry `json:"checks"`
}

// Snapshot builds the status document at the server's current time.
func (s *Server) Snapshot() StatusDoc {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	return BuildStatus(s.cfg, s.st, now)
}

// BuildStatus renders a status document from config + state at an
// explicit instant; shared with the CLI's status subcommand.
func BuildStatus(cfg *config.Config, st *state.State, now time.Time) StatusDoc {
	doc := StatusDoc{Tool: "lastbeat", SchemaVersion: 1, At: now.UTC().Format(time.RFC3339)}
	for _, chk := range cfg.Checks {
		cs := st.Get(chk.Name)
		e := StatusEntry{
			Check:  chk.Name,
			Status: cs.Status,
			Pings:  cs.Pings,
			Fails:  cs.Fails,
			Note:   cs.Note,
		}
		if !cs.LastPing.IsZero() {
			e.LastPing = cs.LastPing.UTC().Format(time.RFC3339)
		}
		if due, ok := monitor.DueIn(&chk, cs, now); ok {
			if due >= 0 {
				e.DueIn = due.Truncate(time.Second).String()
			} else {
				e.Overdue = (-due).Truncate(time.Second).String()
			}
		}
		if cs.Status == state.StatusDown {
			doc.Down++
		}
		doc.Checks = append(doc.Checks, e)
	}
	return doc
}

func (s *Server) handleStatusAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.Snapshot())
}

func (s *Server) handleStatusOne(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/status/")
	doc := s.Snapshot()
	for _, e := range doc.Checks {
		if e.Check == name {
			writeJSON(w, e)
			return
		}
	}
	http.Error(w, fmt.Sprintf("unknown check %q", name), http.StatusNotFound)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
