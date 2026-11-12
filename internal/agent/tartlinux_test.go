//go:build !windows

package agent

// Intent (README, "Linux tart images"). A tart-linux place runs one job in a
// fresh tart clone of its Linux image on a Mac, sized to the job and sharing
// nothing of the Mac; the registration reaches the clone only on tart exec's
// standard input, after runner-up's Linux guest program, so no tart command
// line holds it; the place never waits for a macOS VM, since Linux clones do
// not count toward macOS's two; a new Linux boot ID in the clone ends the job;
// a guest that ran no runner fails the place; the clone is deleted after.
//
// tart is a stand-in that keeps its VMs as folders of $TART_DIR/vms, runs a
// "VM" as a process, and for a tart exec of the guest records its arguments
// and standard input, then ends with the status in $TART_DIR/exit once that
// file appears.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
)

const fakeLinuxTart = `#!/bin/bash
t=$TART_DIR
mkdir -p "$t/vms"
printf '%s\n' "$*" >> "$t/tart.log"
cmd=$1; shift
case $cmd in
list)
  sep=""; printf '['
  for v in "$t"/vms/*/; do
    [ -d "$v" ] || continue
    r=false; [ -e "$v/running" ] && r=true
    printf '%s{"Name":"%s","Source":"local","Running":%s}' "$sep" "$(basename "$v")" "$r"; sep=,
  done
  printf ']\n';;
get) printf '{"OS":"%s"}\n' "$(cat "$t/vms/$1/os")";;
clone) mkdir -p "$t/vms/$2"; echo linux > "$t/vms/$2/os"; echo "$1" > "$t/vms/$2/image";;
set) n=$1; shift; echo "$*" > "$t/vms/$n/size";;
run)
  for n; do :; done; v=$t/vms/$n
  echo 6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b > "$v/boot"
  echo $$ > "$v/running"
  trap 'rm -f "$v/running"; exit 0' TERM INT
  while [ -e "$v/running" ]; do sleep 0.05; done;;
exec)
  [ "$1" = -i ] && shift
  v=$t/vms/$1; shift
  [ -e "$v/running" ] || { echo "the VM is not running" >&2; exit 1; }
  case $1 in
  /usr/bin/true) exit 0;;
  /bin/cat) cat "$v/boot";;
  *)
    printf '%s\0' "$@" > "$t/exec-args"
    cat > "$t/exec-stdin.tmp"; mv "$t/exec-stdin.tmp" "$t/exec-stdin"
    echo "runner-up guest: starting the runner"
    until [ -e "$t/exit" ]; do sleep 0.05; done
    exit "$(cat "$t/exit")";;
  esac;;
stop)
  v=$t/vms/${@: -1}
  p=$(cat "$v/running" 2>/dev/null); [ "${p:-0}" -gt 1 ] && kill "$p"
  rm -f "$v/running";;
delete) rm -rf "$t/vms/$1";;
esac
`

const tartLinuxRegistration = "eyJydW5uZXIiOiJydW5uZXItdXAtbGluLTEifQ-lin-1"

const ubuntuImage = "ghcr.io/cirruslabs/ubuntu:latest"

// standInLinuxTart puts the stand-in tart first on PATH, with two macOS VMs
// already running outside runner-up, and returns its folder.
func standInLinuxTart(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("tart-linux places run only on Macs with Apple silicon")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bin"), 0o700)
	os.WriteFile(filepath.Join(dir, "bin", "tart"), []byte(fakeLinuxTart), 0o755)
	for _, vm := range []string{"owners-sequoia", "owners-tahoe"} {
		os.MkdirAll(filepath.Join(dir, "vms", vm), 0o700)
		os.WriteFile(filepath.Join(dir, "vms", vm, "os"), []byte("darwin\n"), 0o600)
		os.WriteFile(filepath.Join(dir, "vms", vm, "running"), []byte("1\n"), 0o600)
	}
	t.Setenv("TART_DIR", dir)
	t.Setenv("PATH", filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func tartLinuxPlace(t *testing.T) *AgentRun {
	return &AgentRun{Provider: config.ProviderTartLinux, Place: "lin-1", Cores: 2, MemoryMB: 4096, Image: ubuntuImage,
		StateDir: t.TempDir(), BootTimeout: 10 * time.Second, GuestCheck: 100 * time.Millisecond}
}

// startTartLinux runs a's job with the stand-in tart until the guest's tart
// exec has its input, and returns a function that waits for the place's
// result.
func startTartLinux(t *testing.T, dir string, a *AgentRun) (result func() error, out *strings.Builder) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out = &strings.Builder{}
	done := make(chan error, 1)
	go func() { done <- tartLinuxProvider{}.run(ctx, a, tartLinuxRegistration, out) }()
	waitFor(t, filepath.Join(dir, "exec-stdin"))
	return func() error {
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("the place did not end")
			return nil
		}
	}, out
}

func TestTartLinuxPlaceGetsTheRegistrationOnlyOnItsInput(t *testing.T) {
	dir := standInLinuxTart(t)
	a := tartLinuxPlace(t)
	result, out := startTartLinux(t, dir, a)
	vm := filepath.Join(dir, "vms", CloneName("lin-1"))

	// The clone is of the place's image, sized to the place, and boots with
	// nothing of the Mac shared, though two macOS VMs already run.
	if b, _ := os.ReadFile(filepath.Join(vm, "image")); strings.TrimSpace(string(b)) != ubuntuImage {
		t.Errorf("the clone is of %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(vm, "size")); strings.TrimSpace(string(b)) != "--cpu 2 --memory 4096" {
		t.Errorf("the clone was sized %q", b)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "tart.log"))
	if !strings.Contains(string(log), "run --no-graphics "+CloneName("lin-1")+"\n") {
		t.Errorf("tart did not boot the clone alone:\n%s", log)
	}
	if strings.Contains(string(log), tartLinuxRegistration) {
		t.Fatalf("the registration appeared in a tart command line:\n%s", log)
	}

	// The guest program starts as root from tart exec's arguments; its input
	// is a length line and that many bytes of program, then the registration
	// on one line.
	b, _ := os.ReadFile(filepath.Join(dir, "exec-args"))
	args := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	if len(args) != 5 || args[0] != "/bin/sh" || args[1] != "-c" || args[2] != tartLinuxStart || args[4] != containerBootstrap(linuxGuestDir) {
		t.Errorf("tart exec ran %q", args)
	}
	in, _ := os.ReadFile(filepath.Join(dir, "exec-stdin"))
	size, rest, _ := strings.Cut(string(in), "\n")
	n, err := strconv.Atoi(size)
	if err != nil || n > len(rest) {
		t.Fatalf("the guest's input starts %q", size)
	}
	if strings.Contains(rest[:n], tartLinuxRegistration) {
		t.Error("the guest's program holds the registration")
	}
	if !strings.Contains(rest[:n], runnerSHA256["linux-"+runnerArch()]) || !strings.Contains(rest[:n], "/opt/runner-up/fork-guard.sh") {
		t.Error("the guest's program does not run the pinned Linux runner with the fork guard")
	}
	if rest[n:] != tartLinuxRegistration+"\n" {
		t.Errorf("after the program the guest reads %q, want the registration", rest[n:])
	}

	// The runner ends well; the place reports no failure, and cleaning up
	// deletes the clone.
	os.WriteFile(filepath.Join(dir, "exit"), []byte("0\n"), 0o600)
	if err := result(); err != nil {
		t.Fatalf("the place reported %v:\n%s", err, out)
	}
	if err := (tartLinuxProvider{}).clean(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if exists(vm) {
		t.Error("the clone outlived the job")
	}
}

func TestTartLinuxPlaceEndsTheJobWhenTheClonesLinuxRestarts(t *testing.T) {
	dir := standInLinuxTart(t)
	result, _ := startTartLinux(t, dir, tartLinuxPlace(t))
	vm := filepath.Join(dir, "vms", CloneName("lin-1"))
	deadline := time.Now().Add(10 * time.Second)
	for {
		log, _ := os.ReadFile(filepath.Join(dir, "tart.log"))
		if strings.Count(string(log), "exec "+CloneName("lin-1")+" /bin/cat /proc/sys/kernel/random/boot_id") >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent never read the clone's boot ID twice:\n%s", log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	os.WriteFile(filepath.Join(vm, "boot"), []byte("0f9a2b4c-7d1e-4c2a-8b5f-3e6d7a8b9c0d\n"), 0o600)
	if err := result(); err == nil || err.Error() != "the clone's Linux restarted during the job" {
		t.Errorf("a restarted clone reported %v", err)
	}
}

func TestTartLinuxPlaceFailsWhenItsGuestRanNoRunner(t *testing.T) {
	dir := standInLinuxTart(t)
	result, _ := startTartLinux(t, dir, tartLinuxPlace(t))
	os.WriteFile(filepath.Join(dir, "exit"), []byte("1\n"), 0o600)
	if err := result(); err == nil || !strings.Contains(err.Error(), "the clone ran no runner (exit status 1)") {
		t.Errorf("a guest that ran no runner reported %v", err)
	}
}
