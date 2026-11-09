package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
)

// A machine that is also someone's daily computer may keep a floor of free
// memory for its owner, whose own use changes through the day
// (config.Machine.MinAvailableMemoryMB). Before a place there starts, the
// coordinator holds the machine's latest reading of its free memory against
// the floor: a place starts only while
//
//	available + what its running places use >= floor + what its running places reserve + the job's memoryMB
//
// What runner-up's own running places use counts as free, since it is theirs,
// not the owner's; each counts at most its reservation, and a place the agent
// cannot read counts as using nothing, so its whole share stays held. Below
// the floor the place is busy, so the job waits at GitHub holding nothing. A
// missing, failed or stale reading keeps the machine's places waiting until
// one succeeds: the floor protects the owner. The reading never changes a size.

// memReader reads machine m's free memory and what its places use.
type memReader func(ctx context.Context, m config.Machine) (agent.Memory, error)

// memReadEvery is how often the coordinator reads the free memory of a
// machine with a floor and a free place. The owner's use changes by the
// minute, so it is read about as often as the serve loop ticks by default,
// but only while a place there could start: a machine whose places are all
// busy, or which rests, is not read.
const memReadEvery = 10 * time.Second

// memReadBudget is how long one placement pass waits for the machines it
// reads. A machine that has not answered by then counts as a failed reading.
var memReadBudget = 10 * time.Second

// memReadStale is how old a reading may be and still hold a place to the
// floor. A machine whose places were all busy was not read, and a place there
// that frees during a pass waits for a fresh reading rather than start on the
// owner's use from long ago. It leaves a pass room to read and plan.
func memReadStale() time.Duration { return 2*memReadEvery + memReadBudget }

// MemoryReading is the latest reading of a machine's free memory, kept in
// the state file so runner-up status can say why a place waits.
type MemoryReading struct {
	// At is when the reading was asked for.
	At time.Time `json:"at"`
	agent.Memory
	// Err says why there is no reading.
	Err string `json:"err,omitempty"`
}

// ReadMemory asks machine m's agent its free memory.
func ReadMemory(ctx context.Context, m config.Machine) (agent.Memory, error) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	argv := AgentCommand(m, "agent", "memory")
	out, err := exec.CommandContext(c, argv[0], argv[1:]...).Output()
	if err != nil {
		return agent.Memory{}, fmt.Errorf("%v %s", err, agent.FirstLine(StderrOf(err), nil))
	}
	var mem agent.Memory
	if err := json.Unmarshal(out, &mem); err != nil {
		return agent.Memory{}, fmt.Errorf("unreadable answer: %w", err)
	}
	return mem, nil
}

// refreshMemory reads the free memory of every machine with a floor and a
// free place, at most every memReadEvery each, outside the queue lock, all
// at once within memReadBudget. It logs a reading that fails, once until it
// fails differently or succeeds again.
func (s *Service) refreshMemory(ctx context.Context) {
	if s.MemReader == nil {
		return
	}
	s.mu.Lock()
	// A machine the setup no longer gives a floor keeps no reading.
	maps.DeleteFunc(s.Memory, func(name string, _ MemoryReading) bool { return s.Cfg.Machine(name).MinAvailableMemoryMB == 0 })
	now := s.now()
	var due []config.Machine
	for _, m := range s.Cfg.Machines {
		if m.MinAvailableMemoryMB == 0 || now.Sub(s.Memory[m.Name].At) < memReadEvery || s.restingLocked(m.Name) {
			continue
		}
		if slices.ContainsFunc(m.Places, func(p config.Place) bool { return !s.runnerAlive(p.ID) }) {
			due = append(due, m)
		}
	}
	s.mu.Unlock()
	if len(due) == 0 {
		return
	}
	got := readAtOnce(ctx, due, memReadBudget, s.MemReader)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range due {
		r, last := got[m.Name], s.Memory[m.Name]
		read := MemoryReading{At: now, Memory: r.value}
		switch {
		case r.err != nil:
			read = MemoryReading{At: now, Err: r.err.Error()}
			if read.Err != last.Err {
				s.Log.Warn("could not read the machine's free memory; its places wait until a reading succeeds",
					"machine", m.Name, "err", r.err)
			}
		case last.Err != "":
			s.Log.Info("the machine's free memory reads again", "machine", m.Name, "availableMB", read.AvailableMB)
		}
		s.Memory[m.Name] = read
	}
}

// memoryFloor is machine m's latest reading against its floor, leaving out
// place except.
type memoryFloor struct {
	// free is the machine's available memory plus what its running places
	// use, each at most its reservation; own is that part of it.
	free, own int
	// reserved is what its running places reserve.
	reserved int
	// unread says why there is no reading.
	unread string
}

func (v *snapshot) memoryFloor(m config.Machine, except string) memoryFloor {
	r, ok := v.s.Memory[m.Name]
	switch {
	case !ok:
		return memoryFloor{unread: "free memory not read yet"}
	case r.Err != "":
		return memoryFloor{unread: "free memory unread: " + r.Err}
	case v.s.now != nil && v.s.now().Sub(r.At) > memReadStale():
		return memoryFloor{unread: "free memory not read since " + r.At.Format(time.RFC3339)}
	}
	f := memoryFloor{free: r.AvailableMB}
	for _, q := range v.livePlaces(m.Name, except) {
		sh := v.reservation(q)
		f.reserved += sh.MemoryMB
		// A use read before the place's run started was its last run's.
		st := v.place(q.ID)
		if used, ok := r.PlacesMB[q.ID]; ok && st.hasRun && st.run.LaunchedAt.Before(r.At) {
			f.own += min(max(used, 0), sh.MemoryMB)
		}
	}
	f.free += f.own
	return f
}

// left is what a new job may reserve under the floor; below zero when none
// may, as without a reading.
func (f memoryFloor) left(floor int) int {
	if f.unread != "" {
		return -1
	}
	return f.free - floor - f.reserved
}

// underFloor says why a job of memoryMB does not start on machine m under
// its free-memory floor, beside its places but except, or "" when it may.
func (v *snapshot) underFloor(m config.Machine, except string, memoryMB int) string {
	f := v.memoryFloor(m, except)
	if f.unread != "" {
		return f.unread
	}
	if memoryMB <= f.left(m.MinAvailableMemoryMB) {
		return ""
	}
	why := fmt.Sprintf("free memory %d MB", f.free)
	if f.own > 0 {
		why += fmt.Sprintf(" with %d MB its places use", f.own)
	}
	why += fmt.Sprintf(", under the floor %d MB", m.MinAvailableMemoryMB)
	if f.reserved > 0 {
		why += fmt.Sprintf(" + %d MB its places reserve", f.reserved)
	}
	return why + fmt.Sprintf(" + %d MB for this job", memoryMB)
}
