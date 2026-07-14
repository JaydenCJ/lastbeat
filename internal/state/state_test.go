// Tests for the single-file state store: atomic persistence, corruption
// handling, and pruning of removed checks.
package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 13, 6, 0, 0, 0, time.UTC)

func TestLoadMissingFileYieldsEmptyState(t *testing.T) {
	st, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing state file must not error: %v", err)
	}
	if len(st.Checks) != 0 || st.SchemaVersion != SchemaVersion {
		t.Errorf("unexpected fresh state: %+v", st)
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := New()
	cs := st.Get("backup")
	cs.Status = StatusUp
	cs.LastPing = t0
	cs.Pings = 3
	cs.Note = "exit 0"
	if err := st.Save(path, t0); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Checks["backup"], cs) {
		t.Errorf("round trip lost data:\n got %+v\nwant %+v", got.Checks["backup"], cs)
	}
	if !got.UpdatedAt.Equal(t0) {
		t.Errorf("UpdatedAt = %v", got.UpdatedAt)
	}
	// State files get cat'ed and diffed by humans; end with a newline.
	data, _ := os.ReadFile(path)
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Error("state file should end with a newline")
	}
}

func TestSaveIsAtomicNoTempLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st := New()
	st.Get("a").Status = StatusUp
	for i := 0; i < 3; i++ { // overwrite repeatedly
		if err := st.Save(path, t0); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("temp files leaked: %v", names)
	}
}

func TestLoadCorruptFileErrorsWithPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "state.json") {
		t.Errorf("corrupt state should error naming the file, got %v", err)
	}
}

func TestLoadRejectsFutureSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"schema_version": 99, "checks": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "schema_version 99") {
		t.Errorf("future schema should be rejected explicitly, got %v", err)
	}
}

func TestGetCreatesWaitingEntry(t *testing.T) {
	st := New()
	cs := st.Get("new-check")
	if cs.Status != StatusWaiting {
		t.Errorf("fresh entry status = %q, want waiting", cs.Status)
	}
	if st.Get("new-check") != cs {
		t.Error("Get should return the same entry on second call")
	}
}

func TestPruneDropsRemovedChecks(t *testing.T) {
	st := New()
	st.Get("keep")
	st.Get("zap-b")
	st.Get("zap-a")
	removed := st.Prune(map[string]bool{"keep": true})
	if !reflect.DeepEqual(removed, []string{"zap-a", "zap-b"}) {
		t.Errorf("removed = %v", removed)
	}
	if len(st.Checks) != 1 || st.Checks["keep"] == nil {
		t.Errorf("prune result: %v", st.Checks)
	}
}

func TestNamesSorted(t *testing.T) {
	st := New()
	for _, n := range []string{"c", "a", "b"} {
		st.Get(n)
	}
	if got := st.Names(); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("Names() = %v", got)
	}
}
