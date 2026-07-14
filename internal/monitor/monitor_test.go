// Tests for the monitor state machine. Time is always explicit, so every
// transition is asserted to the exact second with zero flakiness.
package monitor

import (
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/lastbeat/internal/config"
	"github.com/JaydenCJ/lastbeat/internal/state"
)

var t0 = time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)

// testConfig: one check pinging hourly with 10 minutes of grace.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse(`
[[check]]
name = "backup"
interval = "1h"
grace = "10m"
`)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestFirstPingMovesWaitingToUp(t *testing.T) {
	cfg, st := testConfig(t), state.New()
	ev, err := RecordPing(cfg, st, "backup", "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if ev != nil {
		t.Errorf("first ping should be silent, got event %+v", ev)
	}
	cs := st.Get("backup")
	if cs.Status != state.StatusUp || !cs.LastPing.Equal(t0) || cs.Pings != 1 {
		t.Errorf("state after first ping: %+v", cs)
	}
	// A ping for a check that is not configured must be an error, not a
	// silently created phantom entry.
	if _, err = RecordPing(cfg, st, "typo", "", t0); err == nil || !strings.Contains(err.Error(), `unknown check "typo"`) {
		t.Errorf("want unknown-check error, got %v", err)
	}
}

func TestSweepBeforeDeadlineIsQuiet(t *testing.T) {
	cfg, st := testConfig(t), state.New()
	RecordPing(cfg, st, "backup", "", t0)
	// 59m59s after the ping: still within the interval.
	if evs := Sweep(cfg, st, t0.Add(time.Hour-time.Second)); len(evs) != 0 {
		t.Errorf("no events expected, got %v", evs)
	}
	if st.Get("backup").Status != state.StatusUp {
		t.Error("check should still be up")
	}
}

func TestSweepInGraceGoesLate(t *testing.T) {
	cfg, st := testConfig(t), state.New()
	RecordPing(cfg, st, "backup", "", t0)
	evs := Sweep(cfg, st, t0.Add(time.Hour+time.Minute)) // 1m past deadline
	if len(evs) != 1 || evs[0].Type != config.EventLate {
		t.Fatalf("want one late event, got %v", evs)
	}
	if evs[0].OverdueBy != time.Minute {
		t.Errorf("overdue = %v, want 1m", evs[0].OverdueBy)
	}
	if st.Get("backup").Status != state.StatusLate {
		t.Error("status should be late")
	}
	// A second sweep still in grace must not repeat the late event.
	if evs := Sweep(cfg, st, t0.Add(time.Hour+2*time.Minute)); len(evs) != 0 {
		t.Errorf("late must fire on the edge only, got %v", evs)
	}
}

func TestSweepPastGraceGoesDown(t *testing.T) {
	cfg, st := testConfig(t), state.New()
	RecordPing(cfg, st, "backup", "", t0)
	evs := Sweep(cfg, st, t0.Add(time.Hour+10*time.Minute+time.Second))
	if len(evs) != 1 || evs[0].Type != config.EventDown {
		t.Fatalf("want one down event, got %v", evs)
	}
	if evs[0].OverdueBy != 10*time.Minute+time.Second {
		t.Errorf("overdue = %v", evs[0].OverdueBy)
	}
	if st.Get("backup").Status != state.StatusDown {
		t.Error("status should be down")
	}
}

func TestUpSkipsLateWhenSweepIsSparse(t *testing.T) {
	// If sweeps are infrequent (pure-cron mode), a check can jump
	// straight from up to down without ever being observed late.
	cfg, st := testConfig(t), state.New()
	RecordPing(cfg, st, "backup", "", t0)
	evs := Sweep(cfg, st, t0.Add(5*time.Hour))
	if len(evs) != 1 || evs[0].Type != config.EventDown || evs[0].Prev != state.StatusUp {
		t.Fatalf("want up->down, got %v", evs)
	}
}

func TestDownEventFiresOnlyOnce(t *testing.T) {
	cfg, st := testConfig(t), state.New()
	RecordPing(cfg, st, "backup", "", t0)
	at := t0.Add(2 * time.Hour)
	if evs := Sweep(cfg, st, at); len(evs) != 1 {
		t.Fatalf("first sweep: %v", evs)
	}
	// Re-sweeping at the same instant is idempotent...
	if evs := Sweep(cfg, st, at); len(evs) != 0 {
		t.Errorf("sweep not idempotent: %v", evs)
	}
	// ...and hours later, still silent: no repeats (edge-triggered).
	if evs := Sweep(cfg, st, t0.Add(9*time.Hour)); len(evs) != 0 {
		t.Errorf("down must not re-fire, got %v", evs)
	}
}

func TestPingAfterDownRecovers(t *testing.T) {
	cfg, st := testConfig(t), state.New()
	RecordPing(cfg, st, "backup", "", t0)
	Sweep(cfg, st, t0.Add(2*time.Hour))
	ev, err := RecordPing(cfg, st, "backup", "back!", t0.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil || ev.Type != config.EventRecovered || ev.Prev != state.StatusDown {
		t.Fatalf("want recovered event, got %+v", ev)
	}
	cs := st.Get("backup")
	if cs.Status != state.StatusUp || cs.Note != "back!" {
		t.Errorf("state after recovery: %+v", cs)
	}
}

func TestPingWhileLateIsSilent(t *testing.T) {
	// Late means "within grace" — recovering from it is normal
	// operation and must not spam a recovered alert.
	cfg, st := testConfig(t), state.New()
	RecordPing(cfg, st, "backup", "", t0)
	Sweep(cfg, st, t0.Add(time.Hour+time.Minute))
	ev, _ := RecordPing(cfg, st, "backup", "", t0.Add(time.Hour+2*time.Minute))
	if ev != nil {
		t.Errorf("ping while late should be silent, got %+v", ev)
	}
	if st.Get("backup").Status != state.StatusUp {
		t.Error("status should be up again")
	}
}

func TestRecordFailGoesStraightDown(t *testing.T) {
	cfg, st := testConfig(t), state.New()
	RecordPing(cfg, st, "backup", "", t0)
	ev, err := RecordFail(cfg, st, "backup", "exit 1: disk full", t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil || ev.Type != config.EventFailed || ev.Status != state.StatusDown {
		t.Fatalf("want failed event, got %+v", ev)
	}
	cs := st.Get("backup")
	if cs.Fails != 1 || cs.Note != "exit 1: disk full" {
		t.Errorf("fail not recorded: %+v", cs)
	}
	// The next successful ping recovers from an explicit fail too.
	rev, _ := RecordPing(cfg, st, "backup", "", t0.Add(time.Hour))
	if rev == nil || rev.Type != config.EventRecovered {
		t.Errorf("want recovered after explicit fail, got %+v", rev)
	}
}

func TestFailRestartsDeadline(t *testing.T) {
	// An explicit fail is still contact: the silence clock restarts, so
	// a later sweep inside the new interval stays quiet (still down).
	cfg, st := testConfig(t), state.New()
	RecordFail(cfg, st, "backup", "", t0)
	if evs := Sweep(cfg, st, t0.Add(30*time.Minute)); len(evs) != 0 {
		t.Errorf("no events expected after fail, got %v", evs)
	}
	if st.Get("backup").Status != state.StatusDown {
		t.Error("check should remain down until a successful ping")
	}
}

func TestWaitingCheckNeverAlerts(t *testing.T) {
	// A check that has never pinged has no baseline; alerting on it
	// would fire the moment a config ships, before the first cron run.
	cfg, st := testConfig(t), state.New()
	if evs := Sweep(cfg, st, t0.Add(1000*time.Hour)); len(evs) != 0 {
		t.Errorf("waiting checks must not alert, got %v", evs)
	}
	if st.Get("backup").Status != state.StatusWaiting {
		t.Error("status should stay waiting")
	}
}

func TestSweepPrunesRemovedChecks(t *testing.T) {
	cfg, st := testConfig(t), state.New()
	st.Get("deleted-job").Status = state.StatusDown
	Sweep(cfg, st, t0)
	if _, ok := st.Checks["deleted-job"]; ok {
		t.Error("state for removed checks should be pruned")
	}
	if _, ok := st.Checks["backup"]; !ok {
		t.Error("configured checks should gain a state entry")
	}
}

func TestNoteIsClipped(t *testing.T) {
	cfg, st := testConfig(t), state.New()
	long := strings.Repeat("x", 2000)
	RecordPing(cfg, st, "backup", long, t0)
	if got := len(st.Get("backup").Note); got != maxNote {
		t.Errorf("note length = %d, want %d", got, maxNote)
	}
}

func TestDueIn(t *testing.T) {
	cfg, st := testConfig(t), state.New()
	chk := cfg.CheckByName("backup")
	if _, ok := DueIn(chk, st.Get("backup"), t0); ok {
		t.Error("waiting check has no due time")
	}
	RecordPing(cfg, st, "backup", "", t0)
	due, ok := DueIn(chk, st.Get("backup"), t0.Add(20*time.Minute))
	if !ok || due != 40*time.Minute {
		t.Errorf("due = %v, %v; want 40m", due, ok)
	}
	due, _ = DueIn(chk, st.Get("backup"), t0.Add(90*time.Minute))
	if due != -30*time.Minute {
		t.Errorf("overdue due = %v; want -30m", due)
	}
}
