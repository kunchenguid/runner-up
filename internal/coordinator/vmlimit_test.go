//go:build !windows

package coordinator

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (the design: "tart VM and host providers with the
// two-VM limit and the guest reboot watch"; "unit tests for the limit and
// reboot watch"). Apple's Virtualization framework runs at most two macOS
// VMs at once on a Mac, so a Mac's tart places never start a third clone, and
// a macOS VM someone else runs there takes one of the two, as a proof clone
// did to runner-up's predecessor (it refused every clone of a place until the
// boot timeout). Once a job runs in a clone, a new boot session in the clone
// ends the job, and nothing else does.

// oneMac is a Mac with Apple silicon, 10 cores and 24 GB, with three small
// tart places and a host place for ru-mac jobs: its budget fits all four at
// once, so only the VM limit holds a tart place back.
func oneMac(dir string) *config.Config {
	const image = "ghcr.io/cirruslabs/macos-sequoia-base:latest"
	tart := func(id string) config.Place {
		return config.Place{ID: id, Provider: config.ProviderTart, Pools: []string{"ru-mac"}, Cores: 2, MemoryMB: 4096, Image: image}
	}
	return &config.Config{
		StateDir: dir,
		GitHub:   config.GitHub{Repos: []string{"octo-org/app", "octo-user/tool", "octo-user/wallet"}},
		Pools:    []config.Pool{{Label: "ru-mac"}},
		Machines: []config.Machine{{Name: "mac", SSH: []string{"you@mac"}, Cores: 10, MemoryMB: 24576, RoomCores: 1, RoomMemoryMB: 1024,
			Places: []config.Place{tart("mac-1"), tart("mac-2"), tart("mac-3"),
				{ID: "mac-host", Provider: config.ProviderHost, Pools: []string{"ru-mac"}, Cores: 2, MemoryMB: 4096}}}},
	}
}

// feedAsMac hands the service the recorded waiting jobs as if each had asked
// for ru-mac.
func (h *harness) feedAsMac(f fixture) {
	h.t.Helper()
	for _, m := range f.Messages {
		msg := m.Message
		for i := range msg.JobAvailableMessages {
			msg.JobAvailableMessages[i].RequestLabels = []string{"ru-mac"}
		}
		if err := h.svc.Handle(context.Background(), m.Repo, "ru-mac", &msg); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) busyTart() []string {
	var out []string
	for id := range h.placed() {
		if strings.HasPrefix(id, "mac-") && id != "mac-host" {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func TestMacRunsAtMostTwoClonesAtOnce(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := oneMac(t.TempDir())
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	h.feedAsMac(f)
	// Six jobs wait. Two tart places start clones, and the host place, which
	// starts none, takes a job too; the third tart place waits.
	if got := h.busyTart(); !slices.Equal(got, []string{"mac-1", "mac-2"}) {
		t.Fatalf("tart places %v run jobs, want mac-1 and mac-2", got)
	}
	if _, ok := h.placed()["mac-host"]; !ok {
		t.Fatal("the host place was held back by the VM limit")
	}
	if busy, why := h.svc.PlaceBusy(cfg.Machines[0].Places[2]); !busy || why != "macOS VM limit: 2 of 2 running" {
		t.Fatalf("mac-3 reads busy=%v %q, want it held by the VM limit", busy, why)
	}

	// A clone's job ends: one tart place takes the next job, never two.
	h.finishAndPlace("mac-1")
	if got := h.busyTart(); len(got) != 2 {
		t.Fatalf("tart places %v run jobs after one finished, want two", got)
	}
	h.finishAndPlace("mac-2")
	if got := h.busyTart(); len(got) != 2 {
		t.Fatalf("tart places %v run jobs after another finished, want two", got)
	}
}

// vmsOnMac stands in for the Mac's agent answering `agent vms`.
type vmsOnMac struct {
	mu    sync.Mutex
	vms   []string
	reads int
}

func (v *vmsOnMac) read(_ context.Context, m config.Machine) ([]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.reads++
	return slices.Clone(v.vms), nil
}

func (v *vmsOnMac) set(vms ...string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.vms = vms
}

func TestMacOSVMOutsideRunnerUpTakesOneOfTheTwo(t *testing.T) {
	f := loadFixture(t, "waiting-jobs.json")
	cfg := oneMac(t.TempDir())
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	h.now = f.Now
	// A proof clone someone started by hand runs on the Mac, beside a Linux
	// VM, which the agent leaves out of its answer.
	mac := &vmsOnMac{vms: []string{"sshhip-xcode271-proof"}}
	h.svc.VmReader = mac.read
	h.feedAsMac(f)
	if got := h.busyTart(); !slices.Equal(got, []string{"mac-1"}) {
		t.Fatalf("tart places %v run jobs beside the proof clone, want only mac-1", got)
	}
	if _, why := h.svc.PlaceBusy(cfg.Machines[0].Places[1]); why != "macOS VM limit: 2 of 2 running, outside runner-up: sshhip-xcode271-proof" {
		t.Fatalf("mac-2 reads %q", why)
	}
	// Every message placed again, but the Mac was asked once.
	if mac.reads != 1 {
		t.Fatalf("the coordinator asked the Mac %d times within %s, want once", mac.reads, vmReadEvery)
	}

	// mac-1's clone now shows in the Mac's answer too; it counts once.
	mac.set("sshhip-xcode271-proof", agent.CloneName("mac-1"))
	h.now = h.now.Add(vmReadEvery)
	h.svc.Place(context.Background())
	if got := h.busyTart(); !slices.Equal(got, []string{"mac-1"}) {
		t.Fatalf("tart places %v run jobs, want only mac-1", got)
	}

	// The proof clone stops: the next reading frees the second VM.
	mac.set(agent.CloneName("mac-1"))
	h.now = h.now.Add(vmReadEvery)
	h.svc.Place(context.Background())
	if got := h.busyTart(); !slices.Equal(got, []string{"mac-1", "mac-2"}) {
		t.Fatalf("tart places %v run jobs once the proof clone stopped, want mac-1 and mac-2", got)
	}
	if mac.reads != 3 {
		t.Fatalf("the coordinator asked the Mac %d times, want 3", mac.reads)
	}
}

func TestOnlyANewBootSessionEndsTheJob(t *testing.T) {
	// kern.bootsessionuuid as a clone of macOS 15 prints it.
	const boot = "6C1F5C0B-0A3E-4F3B-9E43-2D9C5D0B1A77"
	const reboot = "0F9A2B4C-7D1E-4C2A-8B5F-3E6D7A8B9C0D"
	for _, tc := range []struct {
		why      string
		readings []string
		want     bool
	}{
		{"the clone restarted", []string{boot, boot, reboot}, true},
		{"too busy to answer at first", []string{"", "", boot, reboot}, true},
		{"failed and odd readings", []string{boot, "", "sysctl: unknown oid 'kern.bootsessionuuid'", boot}, false},
		{"the same boot throughout", []string{boot, boot, boot, boot}, false},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		i := 0
		read := func(context.Context) string {
			if i >= len(tc.readings) {
				cancel()
				return ""
			}
			i++
			return tc.readings[i-1]
		}
		if got := agent.WatchBootSession(ctx, read, time.Millisecond); got != tc.want {
			t.Errorf("%s: the watch said restarted=%v, want %v", tc.why, got, tc.want)
		}
		cancel()
	}
}
