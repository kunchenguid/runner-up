//go:build !windows

package coordinator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (README, "Setup file", minAvailableMemoryMB; issue #56): a Linux
// workstation that is also its owner's daily machine had about 2 GB
// available and most of its swap in use before any VM ran, and a 4 GB VM
// froze it: roomMemoryMB is static, and the owner's use is not. With a floor
// set, a place there starts only while the memory the machine's agent reads
// free, plus what runner-up's own running places use, leaves the floor beside
// every running place's share and the new job's. Below it the place is busy,
// so the job waits at GitHub holding nothing, and status says why. A reading
// that fails keeps the places waiting.

// desk is the issue's workstation: 16 cores and 32 GB, 4 cores and 16 GB kept
// for its owner, two KVM places of 4 cores and 8 GB, and a floor of 4 GB.
// Its budget fits both places at once, so only the floor holds one back.
func desk(dir string) *config.Config {
	kvm := func(id string) config.Place {
		return config.Place{ID: id, Provider: config.ProviderKVM, Pools: []string{"ru-linux", "ru-large"}, Cores: 4, MemoryMB: 8192,
			Image: "/var/lib/runner-up/noble-server-cloudimg-amd64.img"}
	}
	return &config.Config{
		StateDir: dir,
		GitHub:   config.GitHub{Repos: []string{"octo-org/app", "octo-user/tool", "octo-user/wallet"}},
		Pools:    []config.Pool{{Label: "ru-linux"}, {Label: "ru-large"}},
		Machines: []config.Machine{{Name: "desk", SSH: []string{"ci@desk"}, Cores: 16, MemoryMB: 32768, RoomCores: 4, RoomMemoryMB: 16384,
			MinAvailableMemoryMB: 4096, Places: []config.Place{kvm("desk-1"), kvm("desk-2")}}},
	}
}

// deskAgent stands in for the machine's agent answering `agent memory`.
type deskAgent struct {
	mu    sync.Mutex
	mem   agent.Memory
	err   error
	reads int
}

func (a *deskAgent) read(_ context.Context, m config.Machine) (agent.Memory, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	return a.mem, a.err
}

func (a *deskAgent) set(availableMB int, places map[string]int, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mem, a.err = agent.Memory{AvailableMB: availableMB, PlacesMB: places}, err
}

func (a *deskAgent) times() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads
}

// acquiredJobs counts the jobs acquired from every scale set.
func (h *harness) acquiredJobs() int {
	n := 0
	for _, set := range h.sets {
		set.mu.Lock()
		n += len(set.acquired)
		set.mu.Unlock()
	}
	return n
}

// later places again once the floor's reading is due.
func (h *harness) later() {
	h.now = h.now.Add(memReadEvery)
	h.svc.Place(context.Background())
}

func TestFloorHoldsPlacesBelowTheOwnersFreeMemory(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := desk(t.TempDir())
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	mem := &deskAgent{}
	h.svc.MemReader = mem.read
	desk1, desk2 := cfg.Machines[0].Places[0], cfg.Machines[0].Places[1]

	// The owner's own use leaves 2 GB: no place starts, and no job is
	// acquired or runner registered.
	mem.set(2048, nil, nil)
	h.feed(f)
	if got := h.placed(); len(got) != 0 {
		t.Fatalf("places %v run jobs with 2 GB free under a 4 GB floor", got)
	}
	if n, r := h.acquiredJobs(), h.registrations(); n != 0 || r != 0 {
		t.Fatalf("%d jobs acquired and %d runners registered while the floor held every place", n, r)
	}
	if busy, why := h.svc.PlaceBusy(desk1); !busy || why != "free memory 2048 MB, under the floor 4096 MB + 8192 MB for this job" {
		t.Fatalf("desk-1 reads busy=%v %q", busy, why)
	}
	// Every message placed again, but the machine was read once.
	if n := mem.times(); n != 1 {
		t.Fatalf("the machine's free memory was read %d times within %s, want once", n, memReadEvery)
	}

	// The owner closes things: 13 GB is free, room for one place beside the
	// floor but not two.
	mem.set(13000, nil, nil)
	h.later()
	if got := h.placed(); len(got) != 1 {
		t.Fatalf("places %v run jobs with 13000 MB free, want one", got)
	}
	running, waiting := desk1, desk2
	if _, ok := h.placed()[desk2.ID]; ok {
		running, waiting = desk2, desk1
	}

	// The running VM has taken 6000 MB of its 8 GB, out of what was free.
	// Its use counts as free, but the 2 GB it may still take does not.
	mem.set(7000, map[string]int{running.ID: 6000}, nil)
	h.later()
	if got := h.placed(); len(got) != 1 {
		t.Fatalf("places %v run jobs, want one", got)
	}
	want := "free memory 13000 MB with 6000 MB its places use, under the floor 4096 MB + 8192 MB its places reserve + 8192 MB for this job"
	if _, why := h.svc.PlaceBusy(waiting); why != BusyReason(want) {
		t.Fatalf("%s reads %q, want %q", waiting.ID, why, want)
	}

	// The VM uses 8000 MB and the owner leaves 15000 MB: 23000 MB is not the
	// owner's, enough for the floor and both places. Counted as the owner's,
	// the VM's own use would have kept the second place waiting.
	mem.set(15000, map[string]int{running.ID: 8000}, nil)
	h.later()
	if got := h.placed(); len(got) != 2 {
		t.Fatalf("places %v run jobs with 23000 MB not the owner's, want both", got)
	}
}

func TestFloorWaitsWhileTheMachineCannotBeRead(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := desk(t.TempDir())
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	var buf bytes.Buffer
	h.svc.Log = slog.New(slog.NewJSONHandler(&buf, nil))
	h.now = f.Now
	desk1 := cfg.Machines[0].Places[0]

	// Before any reading, nothing starts.
	h.feed(f)
	if got := h.placed(); len(got) != 0 {
		t.Fatalf("places %v run jobs before the machine's free memory was read", got)
	}
	if _, why := h.svc.PlaceBusy(desk1); why != "free memory not read yet" {
		t.Fatalf("desk-1 reads %q", why)
	}

	// An agent too old to answer: the places wait, and the failure is logged
	// once while it fails the same way.
	mem := &deskAgent{}
	mem.set(0, nil, errors.New("exit status 2 unknown agent command memory"))
	h.svc.MemReader = mem.read
	h.later()
	h.later()
	if got := h.placed(); len(got) != 0 {
		t.Fatalf("places %v run jobs while the machine's free memory cannot be read", got)
	}
	if n := h.acquiredJobs(); n != 0 {
		t.Fatalf("%d jobs acquired while the floor could not be read", n)
	}
	if _, why := h.svc.PlaceBusy(desk1); why != "free memory unread: exit status 2 unknown agent command memory" {
		t.Fatalf("desk-1 reads %q", why)
	}
	if n := strings.Count(buf.String(), "could not read the machine's free memory"); n != 1 {
		t.Fatalf("logged the failed reading %d times, want once:\n%s", n, buf.String())
	}

	// It answers: a place starts.
	mem.set(32000, nil, nil)
	h.later()
	if got := h.placed(); len(got) != 2 {
		t.Fatalf("places %v run jobs once 32000 MB reads free, want both", got)
	}
	if !strings.Contains(buf.String(), "the machine's free memory reads again") {
		t.Fatalf("did not log the reading coming back:\n%s", buf.String())
	}
}

// The floor reads a machine only when it matters: every memReadEvery at most,
// while a place there could start, and never a machine without a floor.
func TestFloorReadsOnlyWhileAPlaceCouldStart(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := desk(t.TempDir())
	cfg.Machines = append(cfg.Machines, oneBox("").Machines...)
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	var mu sync.Mutex
	read := map[string]int{}
	h.svc.MemReader = func(_ context.Context, m config.Machine) (agent.Memory, error) {
		mu.Lock()
		defer mu.Unlock()
		read[m.Name]++
		return agent.Memory{AvailableMB: 32000}, nil
	}
	h.feed(f)
	if got := h.placed(); len(got) != 5 {
		t.Fatalf("places %v run jobs, want all five", got)
	}
	// Both of desk's places now run: it is not read again.
	h.later()
	h.later()
	if read["desk"] != 1 || read["box"] != 0 {
		t.Fatalf("read %v, want desk once and box, which has no floor, never", read)
	}
	h.finishAndPlace("desk-1")
	if read["desk"] != 2 {
		t.Fatalf("desk read %d times, want again once a place there was free", read["desk"])
	}
}

// Intent (README, "Changing the setup"): a floor set while the coordinator
// runs holds the next job, and removing it lets the next job start, without
// a restart.
func TestFloorAppliesToTheNextJobWhenTheSetupChanges(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := desk(t.TempDir())
	cfg.Machines[0].MinAvailableMemoryMB = 0
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	mem := &deskAgent{}
	mem.set(2048, nil, nil)
	h.svc.MemReader = mem.read
	floored := desk(t.TempDir())
	floored.StateDir = cfg.StateDir
	floored.Defaults()
	if _, err := h.svc.Reload(floored, h.svc.Rules); err != nil {
		t.Fatal(err)
	}
	h.feed(f)
	if got := h.placed(); len(got) != 0 {
		t.Fatalf("places %v run jobs after a floor of 4 GB was set with 2 GB free", got)
	}
	cfg.Defaults()
	if _, err := h.svc.Reload(cfg, h.svc.Rules); err != nil {
		t.Fatal(err)
	}
	h.later()
	if got := h.placed(); len(got) != 2 {
		t.Fatalf("places %v run jobs once the floor was removed, want both", got)
	}
}
