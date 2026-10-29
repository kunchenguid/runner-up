//go:build !windows

package cmd

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
)

// Intent: the real borrow supervisor invokes the real Tart agent in a private
// store, deletes a VM left by the caller, and kills stray command descendants.
// Creating an empty Linux VM exercises Tart's storage without an OS download.
func TestBorrowCLIRealTartCleanup(t *testing.T) {
	e := realTartBorrowE2E(t)
	e.serve()
	client, output := startBorrow(t, e, `set -eu
tart create "$BORROWED_VM" --linux --disk-size 1
sleep 120 &
echo $! > "$E2E_DIR/child-pid"
printf 'created=%s cwd=%s\n' "$BORROWED_VM" "$PWD"
exit 23`)
	if err := client.Wait(); err == nil || client.ProcessState.ExitCode() != 23 {
		t.Fatalf("exit=%v output=%s", err, readBorrowOutput(output))
	}
	e.eventually("real agent releases the place", func() bool {
		held, _ := lock.HeldBy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock"))
		return !held
	})
	vms := assertEmptyBorrowTartStore(t, e)
	pid := borrowPID(t, filepath.Join(e.dir, "child-pid"))
	e.eventually("stray child is killed", func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
	t.Logf("%sTart list after real agent cleanup: %s\nstray child stopped; exit=23\n%s", readBorrowOutput(output), vms, borrowStatus(t, e))
}

// Intent: losing the caller without a graceful signal still disconnects its
// private socket, stops the command, cleans the VM, and frees the place.
func TestBorrowCLIRealTartDisconnect(t *testing.T) {
	e := realTartBorrowE2E(t)
	e.serve()
	client, output := startBorrow(t, e, `set -eu
tart create "$BORROWED_VM" --linux --disk-size 1
echo $$ > "$E2E_DIR/disconnect-pid"
exec sleep 120`)
	e.eventually("command starts before caller disconnects", func() bool {
		_, err := os.Stat(filepath.Join(e.dir, "disconnect-pid"))
		return err == nil
	})
	pid := borrowPID(t, filepath.Join(e.dir, "disconnect-pid"))
	if err := client.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	client.Wait()
	e.eventually("disconnect stops command and frees place", func() bool {
		held, _ := lock.HeldBy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock"))
		return !held && syscall.Kill(pid, 0) == syscall.ESRCH
	})
	vms := assertEmptyBorrowTartStore(t, e)
	t.Logf("caller killed without graceful cancellation\n%sTart list after disconnect: %s\ncommand stopped; place free", readBorrowOutput(output), vms)
}

// Intent: a granted borrow's persisted grant time bounds the real supervisor,
// stopping a live foreground command and cleaning its VM before exit 124.
func TestBorrowSupervisorLiveDeadline(t *testing.T) {
	e := realTartBorrowE2E(t)
	// The short socket address also works with macOS's 104-byte limit.
	socketDir, err := os.MkdirTemp("", "ru-deadline-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "s")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	id := "request-live-deadline"
	dir := filepath.Join(e.cfg.StateDir, "borrows", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	busy, err := lock.Busy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	// Use the persisted grant contract rather than waiting a full hour.
	grant := map[string]any{
		"id": id, "pool": "ru-linux", "label": "deadline check", "minutes": 1,
		"place": "box-1", "socket": socket, "granted": time.Now().Add(-time.Hour + 10*time.Second),
	}
	if err := config.WriteJSON(filepath.Join(dir, "granted.json"), grant); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(e.dir, "deadline-output"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	in, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	cmd := exec.Command(runnerUp(t), "run-borrow", "--config", e.config, "--place", "box-1", "--borrow", id)
	cmd.Env, cmd.ExtraFiles, cmd.Stdout, cmd.Stderr = e.env, []*os.File{busy}, out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	listener.SetDeadline(time.Now().Add(10 * time.Second))
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, _, err := conn.WriteMsgUnix([]byte{0}, syscall.UnixRights(int(in.Fd()), int(out.Fd()), int(out.Fd())), nil); err != nil {
		t.Fatal(err)
	}
	script := `set -eu; tart create "$BORROWED_VM" --linux --disk-size 1; echo $$ > "$E2E_DIR/deadline-pid"; exec sleep 120`
	if err := json.NewEncoder(conn).Encode(map[string]any{"Args": []string{"/bin/sh", "-c", script}, "Env": e.env, "Dir": e.dir}); err != nil {
		t.Fatal(err)
	}
	var code int
	if err := json.NewDecoder(conn).Decode(&code); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	if code != 124 || cmd.ProcessState.ExitCode() != 124 {
		t.Fatalf("deadline result=%d exit=%d", code, cmd.ProcessState.ExitCode())
	}
	pid := borrowPID(t, filepath.Join(e.dir, "deadline-pid"))
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("deadline command remains alive: %v", err)
	}
	vms := assertEmptyBorrowTartStore(t, e)
	t.Logf("%sTart list after deadline cleanup: %s\ncommand stopped; exit=124", readBorrowOutput(out.Name()), vms)
}

// Intent: local requests share the real coordinator's queue with GitHub work,
// and declared duration wins over age until the waited rule applies. Unknown
// GitHub jobs use the default 15-minute estimate, after both these borrows.
func TestBorrowCLILiveSharedQueue(t *testing.T) {
	e := borrowE2E(t)
	busy, err := lock.Busy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	job := verifyJob(t)
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(job, scaleset.MessageTypeJobAvailable)})
	e.serve()
	e.eventually("GitHub job enters the queue", func() bool { return strings.Contains(borrowStatus(t, e), job.JobDisplayName) })
	long, longOutput := startLiveBorrow(t, e, "long", 10)
	e.eventually("long request enters queue", func() bool { return strings.Contains(readBorrowOutput(longOutput), "position") })
	short, shortOutput := startLiveBorrow(t, e, "short", 5)
	e.eventually("short borrow ranks ahead of older work", func() bool { return strings.Contains(readBorrowOutput(shortOutput), "position 1") })
	busy.Close()
	borrowStarted(t, e, "short")
	if _, err := os.Stat(filepath.Join(e.dir, "long-started")); !os.IsNotExist(err) {
		t.Fatal("long command ran before short")
	}
	if len(e.actions.read().acquired) != 0 {
		t.Fatal("GitHub job acquired before shortest borrow")
	}
	t.Logf("shortest granted while older borrow and GitHub job wait:\n%s", borrowStatus(t, e))
	finishLiveBorrow(t, e, "short", short)
	borrowStarted(t, e, "long")
	if len(e.actions.read().acquired) != 0 {
		t.Fatal("GitHub job acquired before known borrow")
	}
	finishLiveBorrow(t, e, "long", long)
	e.eventually("GitHub work is acquired after borrows", func() bool { return len(e.actions.read().acquired) > 0 })
	t.Logf("short request:\n%slong request:\n%sGitHub acquired after both commands: %v", readBorrowOutput(shortOutput), readBorrowOutput(longOutput), e.actions.read().acquired)
}

// Intent: two independent borrows with equal labels may run concurrently,
// including across restart and a reported named VM, but a third must wait.
// GitHub and the VM inventory are isolated stand-ins; supervisors are real.
func TestBorrowCLILiveTwoSlotAccounting(t *testing.T) {
	e := borrowE2E(t)
	e.cfg.Machines[0].Cores, e.cfg.Machines[0].MemoryMB = 8, 16384
	first := e.cfg.Machines[0].Places[0]
	for _, id := range []string{"box-2", "box-3"} {
		p := first
		p.ID = id
		e.cfg.Machines[0].Places = append(e.cfg.Machines[0].Places, p)
		if err := os.MkdirAll(e.cfg.PlaceDir(id), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.SaveConfig(e.config, e.cfg); err != nil {
		t.Fatal(err)
	}
	e.serve()
	one, oneOutput := startLiveBorrow(t, e, "one", 20)
	borrowStarted(t, e, "one")
	// The isolated agent reports the first borrow's named VM as running.
	if err := os.WriteFile(filepath.Join(e.dir, "vm-live"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if code := e.stop(); code != 0 {
		t.Fatalf("stop=%d", code)
	}
	e.serve()
	two, twoOutput := startLiveBorrow(t, e, "two", 20)
	borrowStarted(t, e, "two")
	three, threeOutput := startLiveBorrow(t, e, "three", 20)
	e.eventually("third borrow reports the two-VM boundary", func() bool {
		return strings.Contains(readBorrowOutput(threeOutput), "macOS VM limit")
	})
	status := borrowStatus(t, e)
	if strings.Count(status, "busy (borrow)") != 2 {
		t.Fatalf("expected two active borrows: %s", status)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "three-started")); !os.IsNotExist(err) {
		t.Fatal("third borrow exceeded VM limit")
	}
	t.Logf("two equal-label borrows active after restart; third waits:\n%s\n%s", status, readBorrowOutput(threeOutput))
	finishLiveBorrow(t, e, "one", one)
	borrowStarted(t, e, "three")
	finishLiveBorrow(t, e, "two", two)
	finishLiveBorrow(t, e, "three", three)
	t.Logf("one:\n%stwo:\n%sthree after release:\n%s", readBorrowOutput(oneOutput), readBorrowOutput(twoOutput), readBorrowOutput(threeOutput))
}

// Intent: active borrows reserve their share across coordinator restart and
// cannot take the cores or memory explicitly kept for the machine's owner.
func TestBorrowCLILiveOwnerRoom(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cores  int
		memory int
		reason string
	}{
		{"cores", 4, 16384, "cores 2 of 3 in use, 1 kept for the owner"},
		{"memory", 8, 8192, "memory 4096 of 7168 MB in use, 1024 MB kept for the owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := borrowE2E(t)
			e.cfg.Machines[0].Cores, e.cfg.Machines[0].MemoryMB = tc.cores, tc.memory
			second := e.cfg.Machines[0].Places[0]
			second.ID = "box-2"
			e.cfg.Machines[0].Places = append(e.cfg.Machines[0].Places, second)
			if err := os.MkdirAll(e.cfg.PlaceDir(second.ID), 0700); err != nil {
				t.Fatal(err)
			}
			if err := config.SaveConfig(e.config, e.cfg); err != nil {
				t.Fatal(err)
			}
			e.serve()
			one, oneOutput := startLiveBorrow(t, e, "one", 20)
			borrowStarted(t, e, "one")
			if code := e.stop(); code != 0 {
				t.Fatalf("stop=%d", code)
			}
			e.serve()
			two, twoOutput := startLiveBorrow(t, e, "two", 20)
			e.eventually("owner room blocks second borrow after restart", func() bool {
				return strings.Contains(readBorrowOutput(twoOutput), tc.reason)
			})
			if _, err := os.Stat(filepath.Join(e.dir, "two-started")); !os.IsNotExist(err) {
				t.Fatal("second command consumed the owner's room")
			}
			t.Logf("owner room preserved across restart:\n%s\n%s", borrowStatus(t, e), readBorrowOutput(twoOutput))
			finishLiveBorrow(t, e, "one", one)
			borrowStarted(t, e, "two")
			finishLiveBorrow(t, e, "two", two)
			t.Logf("first:\n%ssecond after share released:\n%s", readBorrowOutput(oneOutput), readBorrowOutput(twoOutput))
		})
	}
}

func realTartBorrowE2E(t *testing.T) *e2e {
	t.Helper()
	if _, err := exec.LookPath("tart"); err != nil {
		t.Skip("requires Tart")
	}
	e := borrowE2E(t)
	e.cfg.Machines[0].Agent = runnerUp(t)
	if err := config.SaveConfig(e.config, e.cfg); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(e.dir, "tart")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	e.env = append(e.env, "TART_HOME="+home)
	return e
}

func assertEmptyBorrowTartStore(t *testing.T, e *e2e) string {
	t.Helper()
	list := exec.Command("tart", "list", "--format", "json")
	list.Env = e.env
	b, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("tart list: %v %s", err, b)
	}
	var vms []any
	if err := json.Unmarshal(b, &vms); err != nil || len(vms) != 0 {
		t.Fatalf("leftover VMs: %s (%v)", b, err)
	}
	return string(b)
}

func borrowPID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func borrowStarted(t *testing.T, e *e2e, name string) {
	t.Helper()
	e.eventually(name+" command starts", func() bool {
		_, err := os.Stat(filepath.Join(e.dir, name+"-started"))
		return err == nil
	})
}

func finishLiveBorrow(t *testing.T, e *e2e, name string, cmd *exec.Cmd) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, name+"-done"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func startLiveBorrow(t *testing.T, e *e2e, name string, minutes int) (*exec.Cmd, string) {
	t.Helper()
	output := filepath.Join(e.dir, name+"-output")
	out, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	script := `printf 'place=%s vm=%s\n' "$BORROWED_PLACE" "$BORROWED_VM"; touch "$E2E_DIR/` + name + `-started"; while [ ! -e "$E2E_DIR/` + name + `-done" ]; do sleep 0.05; done`
	cmd := exec.Command(runnerUp(t), "borrow", "--config", e.config, "--pool", "ru-linux", "--label", "same label", "--minutes", strconv.Itoa(minutes), "--", "/bin/sh", "-c", script)
	cmd.Env, cmd.Stdout, cmd.Stderr = e.env, out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Signal(syscall.SIGTERM) })
	return cmd, output
}
