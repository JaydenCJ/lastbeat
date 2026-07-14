// Package state persists everything lastbeat knows between runs in a
// single JSON file: last ping per check, current status, counters. Writes
// are atomic (temp file + rename in the same directory), so a crash or
// power loss mid-write can never leave a truncated state file behind.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Statuses a check can be in. Transitions live in the monitor package.
const (
	StatusWaiting = "waiting" // configured, never pinged yet
	StatusUp      = "up"      // pinged within its interval
	StatusLate    = "late"    // past its deadline, still within grace
	StatusDown    = "down"    // past deadline + grace, or explicitly failed
)

// SchemaVersion is bumped only for incompatible state-file changes.
const SchemaVersion = 1

// CheckState is the persisted record for one check.
type CheckState struct {
	Status     string    `json:"status"`
	LastPing   time.Time `json:"last_ping"`
	LastChange time.Time `json:"last_change"`
	LastEvent  string    `json:"last_event,omitempty"`
	Pings      int64     `json:"pings"`
	Fails      int64     `json:"fails"`
	Note       string    `json:"note,omitempty"`
}

// State is the whole persisted document.
type State struct {
	SchemaVersion int                    `json:"schema_version"`
	UpdatedAt     time.Time              `json:"updated_at"`
	Checks        map[string]*CheckState `json:"checks"`
}

// New returns an empty state document.
func New() *State {
	return &State{SchemaVersion: SchemaVersion, Checks: map[string]*CheckState{}}
}

// Get returns the record for a check, creating a waiting entry on first use.
func (s *State) Get(name string) *CheckState {
	cs, ok := s.Checks[name]
	if !ok {
		cs = &CheckState{Status: StatusWaiting}
		s.Checks[name] = cs
	}
	return cs
}

// Names returns check names in sorted order for deterministic output.
func (s *State) Names() []string {
	names := make([]string, 0, len(s.Checks))
	for n := range s.Checks {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Prune drops state entries for checks no longer in the configuration, so
// renamed or removed jobs do not haunt status output forever. It returns
// the removed names.
func (s *State) Prune(configured map[string]bool) []string {
	var removed []string
	for name := range s.Checks {
		if !configured[name] {
			delete(s.Checks, name)
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	return removed
}

// Load reads a state file. A missing file is not an error — it yields a
// fresh empty state, so first run needs no setup step.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("state file %s is corrupt: %w", path, err)
	}
	if s.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("state file %s has schema_version %d; this lastbeat understands %d", path, s.SchemaVersion, SchemaVersion)
	}
	if s.Checks == nil {
		s.Checks = map[string]*CheckState{}
	}
	return &s, nil
}

// Save writes the state atomically: marshal, write to a temp file next to
// the target, fsync, then rename over the old file.
func (s *State) Save(path string, now time.Time) error {
	s.UpdatedAt = now.UTC()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".lastbeat-state-*")
	if err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}
