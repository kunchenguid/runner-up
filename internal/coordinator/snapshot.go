package coordinator

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// snapshot is one placement pass's reading of the places: whether each
// place's runner or borrow supervisor holds its lock, and the place's
// place.json while it does. A plan asks about a place once for itself and
// again for every place beside it on its machine; the snapshot reads it
// once. The pass forgets a place it claims, to read it afresh. Only places
// the pass claims become busy while it runs, so the rest of a snapshot may
// at most show a place busy that has just become free, never the reverse.
// It is used under mu.
type snapshot struct {
	s      *Service
	places map[string]placeState
}

// placeState is one place as a snapshot read it.
type placeState struct {
	// alive: the place's runner or borrow supervisor holds its lock, or the
	// lock could not be read, which counts alive so a broken reader never
	// starts a second job in a place.
	alive      bool
	unreadable bool
	// run is the place's place.json, read only while it is alive; hasRun
	// says it could be read.
	run    PlaceRun
	hasRun bool
}

func (s *Service) snapshotLocked() *snapshot {
	return &snapshot{s: s, places: map[string]placeState{}}
}

func (v *snapshot) place(id string) placeState {
	if st, ok := v.places[id]; ok {
		return st
	}
	held, err := lock.HeldBy(filepath.Join(v.s.Cfg.PlaceDir(id), "busy.lock"))
	st := placeState{alive: err != nil || held, unreadable: err != nil}
	if st.alive {
		st.run, st.hasRun = readPlaceRun(v.s.Cfg, id)
	}
	v.places[id] = st
	return st
}

// forget drops what the snapshot read of place id.
func (v *snapshot) forget(id string) { delete(v.places, id) }

// runnerAlive reports whether a place's runner or borrow supervisor is alive,
// from its lock alone. A lock that cannot be read counts alive.
func (s *Service) runnerAlive(placeID string) bool {
	held, err := lock.HeldBy(filepath.Join(s.Cfg.PlaceDir(placeID), "busy.lock"))
	return err != nil || held
}

func share(p config.Place) config.Share { return config.Share{Cores: p.Cores, MemoryMB: p.MemoryMB} }

// views reads every place's state for the planner. A place cooling down
// after a failed launch reads as busy for a minute, and a place on a resting
// machine until the machine's agent answers (reach.go). A place a drain
// holds reads as busy until all the drain's places are free, then takes
// only the jobs the drain admits (drain.go).
func (v *snapshot) views() []plan.PlaceView {
	s := v.s
	now := s.now()
	places := s.Cfg.Places()
	out := make([]plan.PlaceView, 0, len(places))
	for _, p := range places {
		// Resource fit depends on the job, not the place's fallback size.
		busy, _ := v.busy(p, false)
		if !busy && (now.Before(s.cooling[p.ID]) || s.restingLocked(p.Machine) || s.held[p.ID] != "") {
			busy = true
		}
		view := plan.PlaceView{Place: p, Busy: busy}
		if d := s.drainOf(p.ID); d != nil {
			view.Drain = d.ID
			view.Busy = busy || d.Drained.IsZero()
		}
		out = append(out, view)
	}
	return out
}

// headroom is what each counted machine's jobs may still reserve.
func (v *snapshot) headroom() map[string]plan.Headroom {
	out := map[string]plan.Headroom{}
	for _, m := range v.s.Cfg.Machines {
		if m.Cores == 0 && m.MemoryMB == 0 {
			continue
		}
		b, used := m.Budget(), v.inUse(m.Name, "")
		cores := b.Cores - used.Cores
		if m.Cores == 0 {
			cores = int(^uint(0) >> 1)
		}
		out[m.Name] = plan.Headroom{Cores: cores, MemoryMB: b.MemoryMB - used.MemoryMB, Memory: b.MemoryMB > 0}
	}
	return out
}

// inUse sums what machine's places reserve while their runner or borrow
// supervisor is alive, leaving out the place except. A runner waiting for its
// job keeps its share, since the job will run there.
// A departed place counts on the machine its runner runs on, not where the
// setup has it now (reload.go).
func (v *snapshot) inUse(machine, except string) config.Share {
	s := v.s
	used := config.Share{}
	places := slices.DeleteFunc(s.Cfg.Places(), func(q config.Place) bool {
		_, departed := s.departed[q.ID]
		return departed
	})
	for _, id := range slices.Sorted(maps.Keys(s.departed)) {
		places = append(places, s.departed[id])
	}
	for _, q := range places {
		if q.ID != except && q.Machine == machine && v.place(q.ID).alive {
			sh := v.reservation(q)
			used.Cores += sh.Cores
			used.MemoryMB += sh.MemoryMB
		}
	}
	return used
}

// inUse is snapshot.inUse read afresh.
func (s *Service) inUse(machine, except string) config.Share {
	return s.snapshotLocked().inUse(machine, except)
}

// reservation is the size held by the running place, frozen in its
// place.json at launch. An unknown live reservation must not be mistaken
// for a smaller place default: until it can be read, the place holds its
// machine's whole counted budget.
func (v *snapshot) reservation(p config.Place) config.Share {
	st := v.place(p.ID)
	if st.hasRun && st.run.Reservation.Cores > 0 && st.run.Reservation.MemoryMB > 0 {
		return st.run.Reservation
	}
	b := v.s.Cfg.Machine(p.Machine).Budget()
	return config.Share{Cores: max(b.Cores, p.Cores), MemoryMB: max(b.MemoryMB, p.MemoryMB)}
}

// Reservation reads the size held by the running place p.
func (s *Service) Reservation(p config.Place) config.Share {
	return s.snapshotLocked().reservation(p)
}

// PlaceBusy reads one place, sized for the pool when checking an assignment.
// Its own lock says whether its runner or borrow supervisor is alive. A
// place is also busy while its size does not fit beside the other runs, within the
// machine's budget less the owner's room. A tart place is
// also busy while its Mac runs as many macOS VMs as it may (vmlimit.go). A
// lock that cannot be read counts busy, so a broken reader never starts a
// second job in a place.
func (s *Service) PlaceBusy(p config.Place) (bool, BusyReason) {
	return s.snapshotLocked().busy(p, true)
}

// BusyReason says why a place is busy: one of the reasons below, or what
// keeps it from fitting on its machine, such as "cores 4 of 6 in use".
type BusyReason string

const (
	// BusyUnreadable: its lock could not be read.
	BusyUnreadable BusyReason = "unreadable"
	// BusyJob: a place runner holds it, for a job.
	BusyJob BusyReason = "job"
	// BusyBorrow: a borrow supervisor holds it.
	BusyBorrow BusyReason = "borrow"
)

// Runs says whether a job or a borrow holds the place.
func (r BusyReason) Runs() bool { return r == BusyJob || r == BusyBorrow }

func (v *snapshot) busy(p config.Place, resources bool) (bool, BusyReason) {
	st := v.place(p.ID)
	if st.unreadable {
		return true, BusyUnreadable
	}
	if st.alive {
		if st.hasRun && st.run.BorrowID != "" {
			return true, BusyBorrow
		}
		return true, BusyJob
	}
	if resources {
		if why := v.tight(p, share(p)); why != "" {
			return true, BusyReason(why)
		}
	}
	if why := v.vmLimitFull(p); why != "" {
		return true, BusyReason(why)
	}
	return false, ""
}

// tight says why a job of size sh does not fit in place p beside its
// machine's other runs, or "" when it fits. A machine without a budget always
// fits.
func (v *snapshot) tight(p config.Place, sh config.Share) string {
	m := v.s.Cfg.Machine(p.Machine)
	if m.Cores == 0 && m.MemoryMB == 0 {
		return ""
	}
	b, used := m.Budget(), v.inUse(p.Machine, p.ID)
	room := func(n int, unit string) string {
		if n == 0 {
			return ""
		}
		return fmt.Sprintf(", %d%s kept for the owner", n, unit)
	}
	if b.Cores > 0 && used.Cores+sh.Cores > b.Cores {
		return fmt.Sprintf("cores %d of %d in use%s", used.Cores, b.Cores, room(m.RoomCores, ""))
	}
	if b.MemoryMB > 0 && used.MemoryMB+sh.MemoryMB > b.MemoryMB {
		return fmt.Sprintf("memory %d of %d MB in use%s", used.MemoryMB, b.MemoryMB, room(m.RoomMemoryMB, " MB"))
	}
	return ""
}
