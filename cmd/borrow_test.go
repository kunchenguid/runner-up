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
	"github.com/kunchenguid/runner-up/internal/lock"
)

// Intent (INF-1823): the real borrow CLI waits beside jobs, passes the place
// and its size to the command, preserves stdio and exit status, survives a
// coordinator restart, and cleans the named VM before releasing the place.
// GitHub and the machine agent are isolated stand-ins; no real VM is started.
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
	if status := borrowStatus(t, e); !strings.Contains(status, `busy (borrow): borrow "native check"`) {
		t.Fatalf("status: %s", status)
	}
	if got := e.stop(); got != 0 {
		t.Fatalf("stop=%d", got)
	}
	held, _ := lock.HeldBy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock"))
	if !held {
		t.Fatal("restart lost borrow lock")
	}
	e.serve()
	e.eventually("new service sees borrow", func() bool { return strings.Contains(borrowStatus(t, e), "busy (borrow)") })
	os.WriteFile(filepath.Join(e.dir, "borrow-done"), nil, 0600)
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

// Intent (INF-1823): stopping a task ends its borrow and deletes its named VM;
// cancelling a queued task must never launch its command later.
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
			// after the caller releases its live lock. They are never granted.
			e.eventually("coordinator collects the abandoned request", func() bool {
				entries, err := os.ReadDir(filepath.Join(e.cfg.StateDir, "borrows"))
				return err == nil && len(entries) == 0
			})

			t.Logf("cancellation exit=130; no VM or request remains\n%s", readBorrowOutput(output))
		})
	}
}

func borrowE2E(t *testing.T) *e2e {
	t.Helper()
	e := newE2E(t)
	agent := filepath.Join(e.dir, "bin", "borrow-agent")
	// The fake agent owns only the test's VM marker, exactly where the command
	// makes its stand-in VM. This exercises real agent dispatch and cleanup.
	script := `#!/bin/sh
case "$2" in
vms) if [ -e "$E2E_DIR/vm-live" ]; then echo '["runner-up-box-1"]'; else echo '[]'; fi ;;
clean) echo clean >> "$E2E_DIR/borrow-clean.log"; rm -f "$E2E_DIR/vm-live" ;;
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
	cmd := exec.Command(runnerUp(t), "borrow", "--config", e.config, "--pool", "ru-linux", "--label", "native check", "--minutes", "20", "--", "borrow-check")
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
	b, err := exec.Command(runnerUp(t), "status", "--config", e.config).CombinedOutput()
	if err != nil {
		t.Fatalf("status: %v %s", err, b)
	}
	return string(b)
}

// Intent (INF-1823): borrowing requires an explicit label, declared positive
// minutes and a tart pool, rather than silently taking a non-VM place.
func TestBorrowCLIRejectsInvalidInputs(t *testing.T) {
	e := newE2E(t) // Its pool has only a Docker place.
	for _, args := range [][]string{
		{"--pool", "ru-linux", "--label", "check", "--minutes", "0", "--", "true"},
		{"--pool", "ru-linux", "--minutes", "20", "--", "true"},
		{"--pool", "ru-linux", "--label", "check", "--minutes", "20"},
		{"--config", e.config, "--pool", "ru-linux", "--label", "check", "--minutes", "20", "--", "true"},
	} {
		cmd := exec.Command(runnerUp(t), append([]string{"borrow"}, args...)...)
		b, err := cmd.CombinedOutput()
		if err == nil || cmd.ProcessState.ExitCode() != 2 {
			t.Fatalf("borrow %v: %v %s", args, err, b)
		}
		t.Logf("borrow %v: exit=2, %s", args, b)
	}
}
