package agent

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// Intent (README, "Windows machines" and "Pull requests from forks"; AGENTS.md,
// "Invariants"): an account place's start script unpacks the runner archive only
// when its bytes match the pinned sum, hands the runner the registration it
// read from standard input in its environment, and gives it the fork guard as
// its job-started hook unless the place allows forks. The script runs in
// Windows PowerShell on the command line the service starts it with, in a
// fresh environment block as the service gives it (not the test's own, which
// under PowerShell 7 points Windows PowerShell at the wrong modules); only
// the runner archive is a stand-in, whose run.cmd prints what it was given.
func TestAccountJobStartScriptRunsOnlyThePinnedRunner(t *testing.T) {
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(powershell); err != nil {
		t.Fatalf("no Windows PowerShell: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "actions-runner-win-x64.zip")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("run.cmd")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("@echo off\r\n" +
		"echo registration=%ACTIONS_RUNNER_INPUT_JITCONFIG%\r\n" +
		"echo hook=%ACTIONS_RUNNER_HOOK_JOB_STARTED%\r\n" +
		"echo size=%RUNNER_UP_CORES% %RUNNER_UP_MEMORY_MB%\r\n" +
		"exit /b 7\r\n"))
	zw.Close()
	f.Close()
	env, err := windows.GetCurrentProcessToken().Environ(false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	const registration = "stand-in-registration"

	// run writes the job's files for pin into a fresh folder and runs its
	// start script as the service does, with the registration on its input.
	run := func(pin string, allowForks bool) (folder, out string, code int) {
		folder = t.TempDir()
		for name, body := range accountJobFiles(archive, pin, `C:\Program Files\runner-up\runner-up.exe`, JobRequest{AllowForks: allowForks, Cores: 2, MemoryMB: 4096}) {
			if err := os.WriteFile(filepath.Join(folder, name), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(powershell)
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: jobCommandLine(powershell, folder)}
		cmd.Env = env
		cmd.Stdin = strings.NewReader(registration + "\n")
		b, err := cmd.CombinedOutput()
		if _, exit := err.(*exec.ExitError); err != nil && !exit {
			t.Fatal(err)
		}
		return folder, string(b), cmd.ProcessState.ExitCode()
	}

	// An archive whose bytes are not the pinned ones is never unpacked.
	folder, out, code := run(strings.Repeat("0", 64), false)
	if code == 0 || !strings.Contains(out, "does not match its pin") || strings.Contains(out, "registration=") {
		t.Errorf("a runner archive off its pin gave exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(folder, "runner")); !os.IsNotExist(err) {
		t.Errorf("a runner archive off its pin was unpacked: %v", err)
	}

	// The pinned archive runs, with the registration and the fork guard.
	folder, out, code = run(hex.EncodeToString(sum[:]), false)
	if code != 7 {
		t.Fatalf("the pinned runner gave exit %d, want its own 7:\n%s", code, out)
	}
	lines := map[string]string{}
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			lines[k] = v
		}
	}
	if lines["registration"] != registration || lines["size"] != "2 4096" {
		t.Errorf("the runner got registration %q and size %q:\n%s", lines["registration"], lines["size"], out)
	}
	if hook := lines["hook"]; !strings.EqualFold(filepath.Base(hook), guardFile) {
		t.Errorf("the runner's job-started hook is %q, want the place's %s:\n%s", hook, guardFile, out)
	} else if _, err := os.Stat(hook); err != nil {
		t.Errorf("the runner's job-started hook %q is not the place's: %v", hook, err)
	}

	// A place that allows forks gives the runner no job-started hook.
	_, out, code = run(hex.EncodeToString(sum[:]), true)
	if code != 7 || !strings.Contains(out, "hook=%ACTIONS_RUNNER_HOOK_JOB_STARTED%") {
		t.Errorf("a place that allows forks gave exit %d:\n%s", code, out)
	}
}
