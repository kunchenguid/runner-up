//go:build !windows

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/testbin"
	"github.com/kunchenguid/runner-up/internal/testrun"
)

// Intent (README, "Setup file" and "Macs"): the tart guest's executed startup
// script must expose the configured cores and memory, including when forks are
// allowed, without retaining the registration in the reader's process
// environment. The shared folder and runner archive follow the existing Mac
// fixture; this executes the actual script, not an assertion about source
// strings.
func TestTartGuestReceivesItsSize(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the guest startup and measurement programs")
	}
	binary := testbin.Build(t, "")
	for _, allow := range []bool{false, true} {
		t.Run(strconv.FormatBool(allow), func(t *testing.T) {
			dir := t.TempDir()
			share := filepath.Join(dir, "share")
			os.MkdirAll(share, 0700)
			const registration = "stand-in-registration"
			script := fmt.Sprintf(`#!/bin/sh
set -eu
[ "$ACTIONS_RUNNER_INPUT_JITCONFIG" = %q ]
parent=$(ps eww -p "$PPID" -o command=)
case "$parent" in
  *%q*) echo 'the resource reader retained the registration' >&2; exit 1 ;;
esac
printf 'job-size=%%s,%%s\n' "$RUNNER_UP_CORES" "$RUNNER_UP_MEMORY_MB"
`, registration, registration)
			if err := writeTarGz(filepath.Join(share, "runner.tar.gz"), "run.sh", script); err != nil {
				t.Fatal(err)
			}
			if err := linkOrCopy(binary, filepath.Join(share, "runner-up")); err != nil {
				t.Fatal(err)
			}
			a := AgentRun{Cores: 4, MemoryMB: 16384, AllowForks: allow}
			cmd := testrun.Command(t, "bash", "-c", tartScript(tartProvider{}.launch(&a)), "runner-up", share, "")
			// Only disposable environment values can enter the process reading.
			cmd.Env = []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH"), "LANG=C", "ACTIONS_RUNNER_HOOK_JOB_STARTED="}
			cmd.Stdin = strings.NewReader(registration + "\n")
			out, err := cmd.CombinedOutput()
			want := "job-size=" + strconv.Itoa(a.Cores) + "," + strconv.Itoa(a.MemoryMB)
			if err != nil || !strings.Contains(string(out), want) {
				t.Fatalf("guest startup: %v\n%s", err, out)
			}
			if exists(filepath.Join(dir, ".runner-up", "fork-guard.sh")) == allow {
				t.Fatalf("fork guard state did not follow allowForks=%v", allow)
			}
			t.Logf("tart guest allowForks=%v: %s", allow, out)
		})
	}
}
