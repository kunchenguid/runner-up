//go:build !windows

package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/release"
)

// Intent (README, "Updating runner-up"): the setup names a runner-up version
// and a per-platform SHA-256; runner-up downloads, verifies and restarts into
// it; it copies agents to its machines; and if a new version stops within a
// minute, runner-up returns to the previous one.
//
// The coordinator, its place runners and the agent are the real programs, and
// GitHub, ssh and docker the stand-ins of e2e_test.go. The releases are this
// test program built as runner-up v9.0.2, which serves GitHub's stand-in as
// the coordinator does here, and a v9.0.3 that passes every check but stops
// as soon as it serves. A stand-in for GitHub's release downloads serves
// both. The machine's home, where its agent goes, is a test folder.

// testRelease builds this test program as runner-up version, packed as that
// release's archive for this computer.
func testRelease(t *testing.T, version string) (program, archive []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner-up")
	out, err := exec.Command("go", "test", "-c", "-o", path,
		"-ldflags", "-X github.com/kunchenguid/runner-up/internal/version.Version="+version, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go test -c: %s", out)
	}
	program, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return program, tarGz(t, program)
}

// tarGz is a release archive holding program as runner-up.
func tarGz(t *testing.T, program []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	w := tar.NewWriter(z)
	if err := w.WriteHeader(&tar.Header{Name: "runner-up", Mode: 0o755, Size: int64(len(program))}); err != nil {
		t.Fatal(err)
	}
	w.Write(program)
	w.Close()
	z.Close()
	return b.Bytes()
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// stopsAtOnce is a release that answers the coordinator's checks - its
// version, and that the setup is good - but stops as soon as it serves.
const stopsAtOnce = `#!/bin/sh
case $1 in
version) echo v9.0.3 ;;
check-config) echo "setup ok" ;;
*) echo "runner-up v9.0.3 cannot serve" >&2; exit 1 ;;
esac
`

// exited waits for the coordinator to stop on its own and returns its exit
// code.
func (e *e2e) exited() int {
	e.t.Helper()
	done := make(chan error, 1)
	go func() { done <- e.serving.Wait() }()
	select {
	case err := <-done:
		e.serving = nil
		return agent.ExitCode(err)
	case <-time.After(60 * time.Second):
		e.t.Fatalf("the coordinator did not stop within 60 seconds\n%s", e.logs())
		return -1
	}
}

// serveAs starts program as the coordinator, as its supervisor does with
// program/current; its output goes to coordinator-N.log.
func (e *e2e) serveAs(program string) {
	e.served++
	out, err := os.Create(filepath.Join(e.dir, fmt.Sprintf("coordinator-%d.log", e.served)))
	if err != nil {
		e.t.Fatal(err)
	}
	defer out.Close()
	cmd := exec.Command(program, "serve", "--config", e.config)
	cmd.Env = append(slices.Clone(e.env), serveEnv+"="+e.config, actionsEnv+"="+e.actions.URL, selfEnv+"="+runnerUp(e.t))
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.serving = cmd
}

// The coordinator restarts as the release its setup names, keeps it after
// its trial and copies it to its machine, whose jobs then run with it. A
// later release that stops at once is refused when the supervisor starts the
// coordinator again: it serves on as the version before, and a job running
// throughout carries on.
func TestCoordinatorRunsTheSetupsReleaseAndGoesBackFromOneThatStops(t *testing.T) {
	e := newE2E(t)
	platform := release.Platform(runtime.GOOS, runtime.GOARCH)
	good, goodArchive := testRelease(t, "v9.0.2")
	badArchive := tarGz(t, []byte(stopsAtOnce))
	files := map[string][]byte{
		"/v9.0.2/" + release.ArchiveName("v9.0.2", platform): goodArchive,
		"/v9.0.3/" + release.ArchiveName("v9.0.3", platform): badArchive,
	}
	downloads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer downloads.Close()
	home := filepath.Join(e.dir, "home")
	os.MkdirAll(home, 0o700)
	e.env = append(e.env, "HOME="+home, trialEnv+"=3s")
	e.cfg.Release = &config.Release{Version: "v9.0.2", SHA256: map[string]string{platform: sha(goodArchive)}, URL: downloads.URL}
	e.save()
	program := filepath.Join(e.cfg.StateDir, "program")

	e.serve()
	e.eventually("the coordinator restarts as v9.0.2", func() bool {
		return e.logged(`msg="runner-up runs another version" version=v9.0.2`) &&
			e.logged(`msg="runner-up starting" version=v9.0.2`)
	})
	e.eventually("v9.0.2 is kept after its trial", func() bool {
		return e.logged(`msg="runner-up kept the new version after its trial" version=v9.0.2`)
	})
	if target, err := os.Readlink(filepath.Join(program, "current")); err != nil || target != "v9.0.2/runner-up" {
		t.Fatalf("program/current points at %q (%v), want v9.0.2/runner-up", target, err)
	}
	if b, err := os.ReadFile(filepath.Join(program, "v9.0.2", "runner-up")); err != nil || !bytes.Equal(b, good) {
		t.Fatalf("program/v9.0.2/runner-up is not the release's program (%v)", err)
	}

	// The machine's agent is the release, in its version's folder in the
	// machine's home, and its jobs run with it.
	copied := ".local/share/runner-up/v9.0.2/runner-up"
	e.eventually("the coordinator copies v9.0.2 to the machine", func() bool {
		return e.logged(`msg="copied runner-up to the machine as its agent" machine=box version=v9.0.2`)
	})
	if b, err := os.ReadFile(filepath.Join(home, copied)); err != nil || !bytes.Equal(b, good) {
		t.Fatalf("the machine's %s is not the release's program (%v)", copied, err)
	}
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(verifyJob(t), scaleset.MessageTypeJobAvailable)})
	e.eventually("a container starts for the job", func() bool { pid, _ := e.container(); return pid > 0 })
	if ssh := e.lines(filepath.Join(e.dir, "ssh.log")); !slices.ContainsFunc(ssh, func(l string) bool {
		return strings.Contains(l, "'"+copied+"' 'agent' 'run'")
	}) {
		t.Errorf("no place ran its job with the copied agent; ssh ran:\n%s", strings.Join(ssh, "\n"))
	}
	pid, _ := e.container()
	e.startJob(pid)
	e.eventually("the job reaches its runner", func() bool { run, ok := e.placeRun(); return ok && run.Started })

	// The setup moves to v9.0.3, which stops as soon as it serves.
	e.cfg.Release = &config.Release{Version: "v9.0.3", SHA256: map[string]string{platform: sha(badArchive)}, URL: downloads.URL}
	e.save()
	if code := e.exited(); code != 1 {
		t.Fatalf("v9.0.3 exited %d, want 1\n%s", code, e.logs())
	}
	if !e.logged(`msg="runner-up runs another version" version=v9.0.3`) || !e.logged("runner-up v9.0.3 cannot serve") {
		t.Fatalf("the coordinator did not restart as v9.0.3\n%s", e.logs())
	}

	// Its supervisor starts program/current again, which is still v9.0.2.
	e.serveAs(filepath.Join(program, "current"))
	e.eventually("v9.0.2 refuses v9.0.3 and serves on", func() bool {
		return e.logged(`msg="runner-up refused a new version and goes back to the version before it" version=v9.0.3 why="it never started serving" before=v9.0.2`) &&
			e.logged(`msg="runner-up starting" version=v9.0.2`) &&
			e.logged(`msg="the setup names a version runner-up refused; it stays on its version" version=v9.0.3 running=v9.0.2`)
	})
	var refused []struct{ Version, SHA256 string }
	if err := config.ReadJSON(filepath.Join(program, "refused.json"), &refused); err != nil ||
		len(refused) != 1 || refused[0].Version != "v9.0.3" || refused[0].SHA256 != sha(badArchive) {
		t.Fatalf("refused.json holds %+v (%v), want v9.0.3 with its archive's SHA-256", refused, err)
	}
	if target, _ := os.Readlink(filepath.Join(program, "current")); target != "v9.0.2/runner-up" {
		t.Fatalf("program/current points at %q after the refusal, want v9.0.2/runner-up", target)
	}

	// The job ran through both restarts, and ends as usual.
	if !alive(pid) {
		t.Fatalf("the job stopped when the coordinator restarted\n%s", e.logs())
	}
	e.endJob(pid)
	e.eventually("the place frees", func() bool { return !e.busy() })
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM\n%s", code, e.logs())
	}
	if seen := e.actions.read(); len(seen.stray) != 0 {
		t.Errorf("unknown requests %v", seen.stray)
	}
}
