//go:build !windows

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/testrun"
)

// Intent (README, "Borrowing a virtual Mac"): the real borrow CLI waits beside
// jobs, passes the place and its size to the command, preserves stdio and exit
// status, survives a coordinator restart, and cleans the named VM before
// releasing the place. GitHub and the machine agent are isolated stand-ins; no
// real VM is started.
func TestBorrowCLIWaitsRunsAndSurvivesRestart(t *testing.T) {
	e := borrowE2E(t)
	busy, err := lock.Busy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	e.serve()
	command := `read -r input
printf 'place=%s vm=%s cpus=%s memory=%s input=%s\n' "$BORROWED_PLACE" "$BORROWED_VM" "$BORROWED_CPUS" "$BORROWED_MEMORY_MB" "$input"
echo live > "$E2E_DIR/vm-live"
while [ ! -e "$E2E_DIR/borrow-done" ]; do sleep 0.05; done
exit 23`
	client, output := startBorrow(t, e, command)
	e.eventually("borrow reports its queue position", func() bool { return strings.Contains(readBorrowOutput(output), "position 1") })
	busy.Close()
	e.eventually("command starts in borrowed place", func() bool { _, err := os.Stat(filepath.Join(e.dir, "vm-live")); return err == nil })
	if run, _ := e.runIn("box-1"); e.heldBy("box-1") != coordinator.BusyBorrow || run.JobName != "native check" {
		t.Fatalf("box-1 is held for %q by %q, want the borrow \"native check\"\n%s", e.heldBy("box-1"), run.JobName, borrowStatus(t, e))
	}
	if got := e.stop(); got != 0 {
		t.Fatalf("stop=%d", got)
	}
	held, _ := lock.HeldBy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock"))
	if !held {
		t.Fatal("restart lost borrow lock")
	}
	e.serve()
	e.eventually("new service sees borrow", func() bool { return e.heldBy("box-1") == coordinator.BusyBorrow })
	// The command ends; while the VM's clean runs, the place stays held, so
	// no job or borrow can take it before the VM is gone.
	os.WriteFile(filepath.Join(e.dir, "hold-clean"), nil, 0600)
	t.Cleanup(func() { os.WriteFile(filepath.Join(e.dir, "clean-go"), nil, 0600) })
	os.WriteFile(filepath.Join(e.dir, "borrow-done"), nil, 0600)
	e.eventually("the borrowed VM's clean starts", func() bool { _, err := os.Stat(filepath.Join(e.dir, "cleaning")); return err == nil })
	if held, _ := lock.HeldBy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock")); !held {
		t.Fatal("the borrow released its place before its VM was cleaned")
	}
	os.WriteFile(filepath.Join(e.dir, "clean-go"), nil, 0600)
	if err := client.Wait(); err == nil || client.ProcessState.ExitCode() != 23 {
		t.Fatalf("exit=%v output=%s", err, readBorrowOutput(output))
	}
	e.eventually("borrow frees after cleanup", func() bool { held, _ := lock.HeldBy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock")); return !held })
	if _, err := os.Stat(filepath.Join(e.dir, "vm-live")); !os.IsNotExist(err) {
		t.Fatal("leftover VM was not removed")
	}
	transcript := readBorrowOutput(output)
	if !strings.Contains(transcript, "place=box-1 vm=runner-up-box-1 cpus=2 memory=4096 input=hello") {
		t.Fatalf("stdio/environment: %s", transcript)
	}
	t.Logf("CLI transcript:\n%s\nRestart preserved the lock; cleanup removed the stand-in VM; exit=23.\n%s", transcript, borrowStatus(t, e))
}

// Intent (README, "Borrowing a virtual Mac"): stopping a task ends its borrow
// and deletes its named VM; cancelling a queued task must never launch its
// command later.
func TestBorrowCLICancellation(t *testing.T) {
	for _, queued := range []bool{true, false} {
		t.Run(fmt.Sprintf("queued=%v", queued), func(t *testing.T) {
			e := borrowE2E(t)
			var busy *os.File
			if queued {
				var err error
				busy, err = lock.Busy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer busy.Close()
			}
			e.serve()
			client, output := startBorrow(t, e, `echo live > "$E2E_DIR/vm-live"; exec sleep 60`)
			e.eventually("borrow reaches expected state", func() bool {
				if queued {
					return strings.Contains(readBorrowOutput(output), "position 1")
				}
				_, err := os.Stat(filepath.Join(e.dir, "vm-live"))
				return err == nil
			})
			client.Process.Signal(syscall.SIGTERM)
			client.Wait()
			if client.ProcessState.ExitCode() != 130 {
				t.Fatalf("exit=%d output=%s", client.ProcessState.ExitCode(), readBorrowOutput(output))
			}
			if busy != nil {
				busy.Close()
			}
			e.eventually("place is free after cancellation", func() bool { held, _ := lock.HeldBy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock")); return !held })
			if _, err := os.Stat(filepath.Join(e.dir, "vm-live")); !os.IsNotExist(err) {
				t.Fatal("cancelled borrow left a VM")
			}
			// Queue directories are collected on the next scheduling pass,
			// after the caller releases its live lock. Once collected, no
			// later pass can grant the request; until then the place was free
			// for it. A grant starts a borrow supervisor, which keeps a log:
			// the queued request, cancelled before any grant, has none.
			e.eventually("coordinator collects the abandoned request", func() bool {
				entries, err := os.ReadDir(filepath.Join(e.cfg.StateDir, "borrows"))
				return err == nil && len(entries) == 0
			})
			grants, _ := filepath.Glob(filepath.Join(e.cfg.LogsDir(), "borrow-*.log"))
			if want := map[bool]int{true: 0, false: 1}[queued]; len(grants) != want {
				t.Fatalf("the coordinator granted the borrow %d times, want %d: %v\n%s", len(grants), want, grants, e.logs())
			}

			t.Logf("cancellation exit=130; no VM or request remains\n%s", readBorrowOutput(output))
		})
	}
}

// Intent (README, "Borrowing a virtual Mac"): signal termination retains its
// shell-compatible status through the real supervisor, socket protocol and
// caller, and releases the borrowed place.
func TestBorrowCLICommandSignalStatus(t *testing.T) {
	e := borrowE2E(t)
	e.serve()
	client, output := startBorrow(t, e, `printf 'terminating borrowed command with SIGTERM\n'; kill -TERM $$`)
	if err := client.Wait(); err == nil || client.ProcessState.ExitCode() != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit=%v status=%d output=%s", err, client.ProcessState.ExitCode(), readBorrowOutput(output))
	}
	e.eventually("signal-terminated borrow frees its place", func() bool {
		held, _ := lock.HeldBy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock"))
		return !held
	})
	t.Logf("%scommand exit=143; place released\n%s", readBorrowOutput(output), borrowStatus(t, e))
}

func borrowE2E(t *testing.T) *e2e {
	t.Helper()
	e := newE2E(t)
	agent := filepath.Join(e.dir, "bin", "borrow-agent")
	// The fake agent owns only the test's VM marker, exactly where the command
	// makes its stand-in VM. This exercises real agent dispatch and cleanup.
	// While hold-clean exists, a clean says so in cleaning and waits for
	// clean-go, so a test can see what holds the place during the clean.
	script := `#!/bin/sh
case "$2" in
vms) if [ -e "$E2E_DIR/vm-live" ]; then echo '["runner-up-box-1"]'; else echo '[]'; fi ;;
clean)
  echo clean >> "$E2E_DIR/borrow-clean.log"
  if [ -e "$E2E_DIR/hold-clean" ]; then
    : > "$E2E_DIR/cleaning"
    until [ -e "$E2E_DIR/clean-go" ]; do sleep 0.05; done
  fi
  rm -f "$E2E_DIR/vm-live" ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(agent, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	e.cfg.Machines[0].Agent = agent
	e.cfg.Machines[0].Places[0].Provider = config.ProviderTart
	e.cfg.Machines[0].Places[0].Image = "local-macos"
	if err := config.SaveConfig(e.config, e.cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.cfg.PlaceDir("box-1"), 0700); err != nil {
		t.Fatal(err)
	}
	return e
}

func startBorrow(t *testing.T, e *e2e, script string) (*exec.Cmd, string) {
	t.Helper()
	output := filepath.Join(e.dir, "borrow-output")
	out, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	// This executable exists only in the caller's PATH, not the service's.
	bin := filepath.Join(e.dir, "caller-bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "borrow-check"), []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := testrun.Command(t, runnerUp(t), "borrow", "--config", e.config, "--pool", "ru-linux", "--label", "native check", "--minutes", "20", "--", "borrow-check")
	cmd.Env = append(append([]string{}, e.env...), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader("hello\n")
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Signal(syscall.SIGTERM) })
	return cmd, output
}
func readBorrowOutput(path string) string { b, _ := os.ReadFile(path); return string(b) }
func borrowStatus(t *testing.T, e *e2e) string {
	t.Helper()
	b, err := testrun.Command(t, runnerUp(t), "status", "--config", e.config).CombinedOutput()
	if err != nil {
		t.Fatalf("status: %v %s", err, b)
	}
	return string(b)
}

// Intent (README, "Borrowing a virtual Mac"): borrowing requires an explicit
// label, declared positive minutes and a tart pool, rather than silently taking
// a non-VM place. Every case reads a valid setup file, so each refusal is its
// own and no request reaches the coordinator's queue.
func TestBorrowCLIRejectsInvalidInputs(t *testing.T) {
	e := newE2E(t) // Its pool has only a Docker place.
	usage := "borrow needs --pool LABEL --label TEXT --minutes N -- COMMAND"
	for _, tc := range []struct {
		args []string
		why  string
	}{
		{[]string{"--pool", "ru-linux", "--label", "check", "--minutes", "0", "--", "true"}, usage},
		{[]string{"--pool", "ru-linux", "--minutes", "20", "--", "true"}, usage},
		{[]string{"--pool", "ru-linux", "--label", "check", "--minutes", "20"}, usage},
		{[]string{"--pool", "ru-linux", "--label", "check", "--minutes", "20", "--", "true"}, "pool has no tart place"},
	} {
		args := append([]string{"borrow", "--config", e.config}, tc.args...)
		cmd := testrun.Command(t, runnerUp(t), args...)
		b, err := cmd.CombinedOutput()
		if err == nil || cmd.ProcessState.ExitCode() != 2 || strings.TrimSpace(string(b)) != tc.why {
			t.Fatalf("borrow %v: %v %q, want exit 2 and %q", tc.args, err, b, tc.why)
		}
		t.Logf("borrow %v: exit=2, %s", tc.args, b)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.cfg.StateDir, "borrows")); len(entries) != 0 {
		t.Fatalf("a refused borrow left requests %v", entries)
	}
}
