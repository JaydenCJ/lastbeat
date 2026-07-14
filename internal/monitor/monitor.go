// Package monitor is the pure state machine at the heart of lastbeat.
// It never touches the clock, the disk, or the network: callers pass in
// the configuration, the current state, and an explicit "now", and get
// back the events that just happened. That is what makes every transition
// unit-testable to the second.
//
// Status transitions:
//
//	waiting --ping--> up
//	up --deadline passed--> late --grace exhausted--> down
//	up|late --ping--> up          (no event; the job is simply on time-ish)
//	down --ping--> up             (event: recovered)
//	any --fail ping--> down       (event: failed)
//	down --grace exhausted--> down (no repeat event; alerts fire on edges)
package monitor

import (
	"fmt"
	"time"

	"github.com/JaydenCJ/lastbeat/internal/config"
	"github.com/JaydenCJ/lastbeat/internal/state"
)

// Event is one alert-worthy transition observed by the monitor.
type Event struct {
	Check     string        `json:"check"`
	Type      string        `json:"event"` // config.Event*
	Status    string        `json:"status"`
	Prev      string        `json:"prev_status"`
	At        time.Time     `json:"at"`
	LastPing  time.Time     `json:"last_ping"`
	OverdueBy time.Duration `json:"-"`
	Note      string        `json:"note,omitempty"`
}

// RecordPing marks a successful heartbeat for a check. It returns a
// recovered event when the check was down, nil otherwise.
func RecordPing(cfg *config.Config, st *state.State, name, note string, now time.Time) (*Event, error) {
	if cfg.CheckByName(name) == nil {
		return nil, fmt.Errorf("unknown check %q", name)
	}
	cs := st.Get(name)
	prev := cs.Status
	cs.LastPing = now.UTC()
	cs.Pings++
	cs.Note = clipNote(note)
	if cs.Status != state.StatusUp {
		cs.Status = state.StatusUp
		cs.LastChange = now.UTC()
	}
	if prev == state.StatusDown {
		cs.LastEvent = config.EventRecovered
		return &Event{
			Check:    name,
			Type:     config.EventRecovered,
			Status:   state.StatusUp,
			Prev:     prev,
			At:       now.UTC(),
			LastPing: cs.LastPing,
			Note:     cs.Note,
		}, nil
	}
	return nil, nil
}

// RecordFail marks an explicit failure report (the job ran and knows it
// broke). The check goes straight to down and a failed event fires.
func RecordFail(cfg *config.Config, st *state.State, name, note string, now time.Time) (*Event, error) {
	if cfg.CheckByName(name) == nil {
		return nil, fmt.Errorf("unknown check %q", name)
	}
	cs := st.Get(name)
	prev := cs.Status
	cs.LastPing = now.UTC() // the job did make contact; deadlines restart
	cs.Fails++
	cs.Note = clipNote(note)
	cs.Status = state.StatusDown
	cs.LastChange = now.UTC()
	cs.LastEvent = config.EventFailed
	return &Event{
		Check:    name,
		Type:     config.EventFailed,
		Status:   state.StatusDown,
		Prev:     prev,
		At:       now.UTC(),
		LastPing: cs.LastPing,
		Note:     cs.Note,
	}, nil
}

// Sweep evaluates every configured check against now and applies overdue
// transitions. It also creates waiting entries for new checks and prunes
// entries whose checks were removed from the config. Sweep is idempotent:
// re-running it at the same instant produces no additional events.
func Sweep(cfg *config.Config, st *state.State, now time.Time) []Event {
	configured := map[string]bool{}
	var events []Event
	for _, chk := range cfg.Checks {
		configured[chk.Name] = true
		cs := st.Get(chk.Name)
		if cs.Status == state.StatusWaiting || cs.Status == state.StatusDown {
			// waiting: nothing expected yet; down: already alerted on
			// the edge, stay down until a ping recovers it.
			continue
		}
		deadline := cs.LastPing.Add(chk.Interval)
		graceEnd := deadline.Add(chk.Grace)
		switch {
		case now.After(graceEnd):
			prev := cs.Status
			cs.Status = state.StatusDown
			cs.LastChange = now.UTC()
			cs.LastEvent = config.EventDown
			events = append(events, Event{
				Check:     chk.Name,
				Type:      config.EventDown,
				Status:    state.StatusDown,
				Prev:      prev,
				At:        now.UTC(),
				LastPing:  cs.LastPing,
				OverdueBy: now.Sub(deadline),
			})
		case now.After(deadline) && cs.Status == state.StatusUp:
			cs.Status = state.StatusLate
			cs.LastChange = now.UTC()
			cs.LastEvent = config.EventLate
			events = append(events, Event{
				Check:     chk.Name,
				Type:      config.EventLate,
				Status:    state.StatusLate,
				Prev:      state.StatusUp,
				At:        now.UTC(),
				LastPing:  cs.LastPing,
				OverdueBy: now.Sub(deadline),
			})
		}
	}
	st.Prune(configured)
	return events
}

// DueIn reports how long until a check's next deadline at the given
// instant; negative means it is already overdue.
func DueIn(chk *config.Check, cs *state.CheckState, now time.Time) (time.Duration, bool) {
	if cs.Status == state.StatusWaiting || cs.LastPing.IsZero() {
		return 0, false
	}
	return cs.LastPing.Add(chk.Interval).Sub(now), true
}

// maxNote bounds stored ping notes so a chatty job cannot bloat the state
// file; 512 bytes is plenty for an exit summary.
const maxNote = 512

func clipNote(note string) string {
	if len(note) > maxNote {
		return note[:maxNote]
	}
	return note
}
