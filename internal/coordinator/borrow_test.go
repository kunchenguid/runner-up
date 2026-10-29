//go:build !windows

package coordinator

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/plan"
)

func (l *fakeLauncher) LaunchBorrow(p config.Place, f *os.File, id string) error {
	return l.Launch(p, f, "borrow:"+id)
}

func enqueueBorrow(t *testing.T, cfg *config.Config, r borrowRequest) *os.File {
	t.Helper()
	dir := borrowDir(cfg, r.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	live, err := lock.Busy(filepath.Join(dir, "live.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	if err := config.WriteJSON(filepath.Join(dir, "request.json"), r); err != nil {
		t.Fatal(err)
	}
	return live
}

// Intent (INF-1823): borrows rank among recorded jobs by the same rules,
// using declared minutes rather than the unknown or learned job estimate.
// They may borrow only tart places, never a host or retired crew place.
func TestBorrowRanksAmongRecordedJobs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		order   []string
		minutes int
		queued  time.Duration
		first   string
	}{
		{"short borrow", []string{"shortest"}, 5, 0, "borrow"},
		{"long borrow", []string{"shortest"}, 30, 0, "job"},
		{"aged job goes first", []string{"waited:60m", "shortest"}, 5, 0, "job"},
		{"oldest borrow", []string{"waited:60m", "shortest"}, 30, -5 * time.Hour, "borrow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := loadFixture(t, "waiting-jobs.json")
			cfg := oneMac(t.TempDir())
			cfg.Order = tc.order
			cfg.Machines[0].Places = cfg.Machines[0].Places[:1]
			l := newFakeLauncher()
			h := newHarness(t, cfg, l, map[string]*fakeSet{})
			h.now = f.Now
			busy, err := lock.Busy(filepath.Join(cfg.PlaceDir("mac-1"), "busy.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer busy.Close()
			h.feedAsMac(f)
			enqueueBorrow(t, cfg, borrowRequest{ID: "request-check", Label: "native check", Pool: "ru-mac", Minutes: tc.minutes, Queued: f.Now.Add(tc.queued)})
			busy.Close()
			h.svc.Place(context.Background())
			var run PlaceRun
			if err := config.ReadJSON(filepath.Join(cfg.PlaceDir("mac-1"), "place.json"), &run); err != nil {
				t.Fatal(err)
			}
			if got := run.BorrowID != ""; got != (tc.first == "borrow") {
				t.Fatalf("first run %+v, want %s", run, tc.first)
			}
			t.Logf("%s: place=%s borrow=%q job=%q", tc.name, "mac-1", run.BorrowID, run.JobName)
			l.finish(t, "mac-1")
			h.svc.Tidy(context.Background())
			h.svc.Place(context.Background())
			var next PlaceRun
			if err := config.ReadJSON(filepath.Join(cfg.PlaceDir("mac-1"), "place.json"), &next); err != nil {
				t.Fatal(err)
			}
			if tc.first == "borrow" && next.BorrowID != "" {
				t.Fatal("job did not take the place after borrow")
			}
			t.Logf("next run on the same place: borrow=%q job=%q", next.BorrowID, next.JobName)
			l.finish(t, "mac-1")
		})
	}
	cfg := oneMac(t.TempDir())
	rules, _ := plan.ParseOrder(nil)
	host := cfg.Machines[0].Places[3]
	if got := plan.Plan([]plan.Job{{BorrowID: "request-host", Pool: "ru-mac"}}, []plan.PlaceView{{Place: host}}, nil, rules, time.Now()); len(got) != 0 {
		t.Fatal("borrow took a host place")
	}
}

// Intent (INF-1823): a borrow survives a service restart and counts its place's
// share and named VM once, even when the agent reports that VM as running.
func TestBorrowCountsOnceAcrossRestart(t *testing.T) {
	cfg := oneMac(t.TempDir())
	cfg.Machines[0].Places = cfg.Machines[0].Places[:2]
	l := newFakeLauncher()
	h := newHarness(t, cfg, l, map[string]*fakeSet{})
	h.now = time.Now()
	enqueueBorrow(t, cfg, borrowRequest{ID: "request-check", Pool: "ru-mac", Label: "native check", Minutes: 20, Queued: h.now})
	h.svc.Place(context.Background())
	defer l.finish(t, "mac-1")
	s, err := NewService(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), l)
	if err != nil {
		t.Fatal(err)
	}
	s.vms["mac"] = []string{agent.CloneName("mac-1")}
	if busy, why := s.PlaceBusy(cfg.Machines[0].Places[1]); busy {
		t.Fatalf("second slot blocked by a double count: %s", why)
	}
	if used := s.inUse("mac", ""); used != (config.Share{Cores: 2, MemoryMB: 4096}) {
		t.Fatalf("share = %+v", used)
	}
	if busy, why := s.PlaceBusy(cfg.Machines[0].Places[0]); !busy || why != "borrow" {
		t.Fatalf("borrow status: %v %q", busy, why)
	}
	s.refreshBorrowsLocked()
	if len(s.waiting) != 0 {
		t.Fatal("granted borrow queued again after restart")
	}
	t.Log("restart: named VM plus held borrow reserves one share and leaves the second VM slot free")
}

// Intent (INF-1823): an abandoned waiter holds nothing and cannot later be
// granted a place. Equal labels still identify independent requests.
func TestBorrowAbandonedWaiterAndEqualLabels(t *testing.T) {
	cfg := oneMac(t.TempDir())
	l := newFakeLauncher()
	h := newHarness(t, cfg, l, map[string]*fakeSet{})
	h.now = time.Now()
	a := enqueueBorrow(t, cfg, borrowRequest{ID: "request-a", Pool: "ru-mac", Label: "check", Minutes: 20, Queued: h.now})
	enqueueBorrow(t, cfg, borrowRequest{ID: "request-b", Pool: "ru-mac", Label: "check", Minutes: 20, Queued: h.now})
	h.svc.refreshBorrowsLocked()
	if len(h.svc.waiting) != 2 {
		t.Fatal("equal labels overwrote a request")
	}
	a.Close()
	h.svc.refreshBorrowsLocked()
	if len(h.svc.waiting) != 1 {
		t.Fatal("abandoned waiter remained queued")
	}
}

// Intent (captain's written answer on INF-1823): max(1 hour, what crew asked).
// The duration feeds the same command cancellation path exercised below.
func TestBorrowLimit(t *testing.T) {
	for _, minutes := range []int{1, 20, 60, 90, 180} {
		if got, want := borrowLimit(minutes), time.Duration(max(60, minutes))*time.Minute; got != want {
			t.Fatalf("%d minutes: %s != %s", minutes, got, want)
		}
	}
}

// Intent (INF-1823): a stopped or expired borrow stops its command and returns
// an explanation, and ordinary command exits retain their exit status.
func TestBorrowCommandExitAndDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "exit", true: "deadline"}[deadline], func(t *testing.T) {
			out, err := os.CreateTemp(t.TempDir(), "output")
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			in, _ := os.Open(os.DevNull)
			defer in.Close()
			ctx := context.Background()
			script := `printf '%s %s %s %s' "$BORROWED_PLACE" "$BORROWED_VM" "$BORROWED_CPUS" "$BORROWED_MEMORY_MB"; exit 23`
			if deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
				script = `echo $$ > "` + filepath.Join(filepath.Dir(out.Name()), "pid") + `"; exec sleep 60`
			}
			code := runBorrowCommand(ctx, config.Place{ID: "mini-vm1", Cores: 4, MemoryMB: 16384}, borrowCommand{Args: []string{"sh", "-c", script}, Env: os.Environ()}, in, out, out)
			want := 23
			if deadline {
				want = 124
			}
			if code != want {
				t.Fatalf("code %d want %d", code, want)
			}
			b, _ := os.ReadFile(out.Name())
			if !deadline && string(b) != "mini-vm1 runner-up-mini-vm1 4 16384" {
				t.Fatalf("environment %q", b)
			}
			if deadline && !strings.Contains(string(b), "deadline exceeded") {
				t.Fatalf("missing timeout explanation: %s", b)
			}
			if deadline {
				pidBytes, err := os.ReadFile(filepath.Join(filepath.Dir(out.Name()), "pid"))
				if err != nil {
					t.Fatal(err)
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
					t.Fatalf("expired command still alive: %d (%v)", pid, err)
				}
			}
			t.Logf("exit=%d output=%q", code, b)
		})
	}
}

// Intent (INF-1823): a borrowed command that a signal ends reports the
// shell-compatible status 128 plus the signal, not a lost status.
func TestBorrowCommandSignalStatus(t *testing.T) {
	out, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	in, _ := os.Open(os.DevNull)
	defer in.Close()
	code := runBorrowCommand(context.Background(), config.Place{ID: "mini-vm1"}, borrowCommand{Args: []string{"sh", "-c", "kill -TERM $$"}, Env: os.Environ()}, in, out, out)
	if want := 128 + int(syscall.SIGTERM); code != want {
		t.Fatalf("code %d want %d", code, want)
	}
}

// Intent (INF-1823): a request enters the line only after its live lock is
// held. A scheduling pass during caller initialization must not delete the
// caller's folder before it can publish the request.
func TestBorrowUnpublishedRequestIsNotCollected(t *testing.T) {
	cfg := oneMac(t.TempDir())
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	dir := borrowDir(cfg, "request-starting")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, withLockFile := range []bool{false, true} {
		if withLockFile {
			if err := os.WriteFile(filepath.Join(dir, "live.lock"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		h.svc.refreshBorrowsLocked()
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("collected unpublished request: %v", err)
		}
	}
	live := enqueueBorrow(t, cfg, borrowRequest{ID: "request-starting", Pool: "ru-mac", Label: "check", Minutes: 20, Queued: time.Now()})
	h.svc.refreshBorrowsLocked()
	if len(h.svc.waiting) != 1 {
		t.Fatal("published request did not enter the line")
	}
	live.Close()
	h.svc.refreshBorrowsLocked()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("abandoned published request not collected: %v", err)
	}
}
