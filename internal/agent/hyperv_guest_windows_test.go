package agent

// Intent (README, "Windows machines" and "Pull requests from forks";
// AGENTS.md, "Invariants"): a Hyper-V place set to a Windows guest boots
// under secure boot trusting Windows, from a seed disc Windows reads, and its
// guest program, as the agent fills it in, says it is ready on the serial
// port until the registration comes, unpacks the runner only when its
// download matches the pinned sum, hands the runner the registration in the
// runner's environment alone, gives it runner-up's own fork guard as its
// job-started hook unless the place allows forks, copies the runner's output
// to the port and turns the VM off whatever happened.
//
// The guest program runs here in Windows PowerShell, as the image's sign-in
// task runs it, with only the serial port, the VM's turning off, the
// download's address and the user's profile standing in. The runner is a
// stand-in whose run.cmd prints what it was given and runs its hook as the
// runner runs a .ps1 hook, on GitHub payloads saved from a real fork's pull
// request (testdata/events).

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/testbin"
	"github.com/kunchenguid/runner-up/internal/testrun"
	"golang.org/x/sys/windows"
)

// windowsPowerShell is Windows PowerShell, which every Windows has, and the
// environment a fresh sign-in gives it: under PowerShell 7 the test's own
// environment points Windows PowerShell at the wrong modules.
func windowsPowerShell(t *testing.T) (string, []string) {
	t.Helper()
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(powershell); err != nil {
		t.Fatalf("no Windows PowerShell: %v", err)
	}
	env, err := windows.GetCurrentProcessToken().Environ(false)
	if err != nil {
		t.Fatal(err)
	}
	return powershell, env
}

// standInRunner is a runner archive whose run.cmd prints the registration,
// the hook and the size it was given, runs the hook as the runner runs a .ps1
// hook on the event files FORK_EVENT and OWN_EVENT name, and ends with 7.
func standInRunner(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("run.cmd")
	if err != nil {
		t.Fatal(err)
	}
	hook := `powershell -NoProfile -NonInteractive -command ". '%ACTIONS_RUNNER_HOOK_JOB_STARTED%'"` + "\r\n"
	w.Write([]byte("@echo off\r\n" +
		"echo registration=%ACTIONS_RUNNER_INPUT_JITCONFIG%\r\n" +
		"echo hook=%ACTIONS_RUNNER_HOOK_JOB_STARTED%\r\n" +
		"echo size=%RUNNER_UP_CORES% %RUNNER_UP_MEMORY_MB%\r\n" +
		"if not defined ACTIONS_RUNNER_HOOK_JOB_STARTED exit /b 7\r\n" +
		"set GITHUB_EVENT_NAME=pull_request\r\n" +
		"set GITHUB_EVENT_PATH=%FORK_EVENT%\r\n" + hook +
		"echo fork=%ERRORLEVEL%\r\n" +
		"set GITHUB_EVENT_PATH=%OWN_EVENT%\r\n" + hook +
		"echo own=%ERRORLEVEL%\r\n" +
		"exit /b 7\r\n"))
	zw.Close()
	f.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// guestDriver runs the guest program at guest as the image's sign-in task
// would, with its serial port, its turning off, the download's address, the
// pin (when pin is set) and the user's profile replaced. It prints the
// pinned download and sum as the program has them, everything the program
// wrote to its port, and the program's own registration variable.
const guestDriver = `$ErrorActionPreference = 'Stop'
$global:sent = New-Object System.Collections.Generic.List[string]
$global:given = $false
. %[1]s
'pinned-url=' + $RunnerUrl
'pinned-sum=' + $RunnerSHA256
$RunnerUrl = %[2]s
if (%[3]s) { $RunnerSHA256 = %[3]s }
$GuestHome = %[4]s
function Open-RunnerUpPort {
    $port = New-Object psobject
    $port | Add-Member ScriptMethod Write { param($text) $global:sent.Add($text) }
    $port | Add-Member ScriptMethod ReadExisting {
        if ($global:sent.Count -gt 0 -and -not $global:given) { $global:given = $true; return %[5]s + [char]10 }
        ''
    }
    $port
}
function Stop-RunnerUpGuest { $global:sent.Add('turned off' + [char]10) }
Invoke-RunnerUpGuest
'own-registration=' + $env:ACTIONS_RUNNER_INPUT_JITCONFIG
'--- port'
-join $global:sent
`

func TestWindowsGuestRunsOnlyThePinnedRunnerWithTheForkGuard(t *testing.T) {
	powershell, env := windowsPowerShell(t)
	program := testbin.Build(t, "")
	runner := standInRunner(t)
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(runner) }))
	defer download.Close()
	sum := sha256.Sum256(runner)
	fork, _ := filepath.Abs(event("pull-request-521-fork.json"))
	own, _ := filepath.Abs(event("pull-request-527-same-repo.json"))
	const registration = "stand-in-registration"

	// run fills in the guest program for a place of 4 cores and 8 GB, puts
	// it on a stand-in disc beside runner-up, and runs it with pin; it
	// returns the user's profile, what the driver printed and what the
	// program wrote to its port.
	run := func(pin string, allowForks bool) (home, printed, port string) {
		a := &AgentRun{Place: "win-pc-1", Guest: config.GuestWindows, Cores: 4, MemoryMB: 8192, AllowForks: allowForks, Self: program}
		_, files, err := hypervDisc(a, runnerArch(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		disc, home := t.TempDir(), t.TempDir()
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(disc, name), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		driver := filepath.Join(t.TempDir(), "driver.ps1")
		script := fmt.Sprintf(guestDriver, psQuote(filepath.Join(disc, windowsGuestFile)), psQuote(download.URL),
			psQuote(pin), psQuote(home), psQuote(registration))
		if err := os.WriteFile(driver, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := testrun.Command(t, powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", driver)
		cmd.Env = append(env, "FORK_EVENT="+fork, "OWN_EVENT="+own)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the guest program did not run: %v\n%s", err, out)
		}
		printed, port, _ = strings.Cut(strings.ReplaceAll(string(out), "\r\n", "\n"), "--- port\n")
		return home, printed, port
	}
	lines := func(s string) map[string]string {
		m := map[string]string{}
		for _, l := range strings.Split(s, "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				m[k] = v
			}
		}
		return m
	}

	// A download whose bytes are not the pinned ones is never unpacked, and
	// the VM still turns off.
	home, printed, port := run(strings.Repeat("0", 64), false)
	if !strings.Contains(port, "runner-up guest: failed: the runner's download does not match its pin\n") ||
		strings.Contains(port, "registration=") || !strings.HasSuffix(port, "turned off\n") {
		t.Errorf("a runner download off its pin gave:\n%s", port)
	}
	if _, err := os.Stat(filepath.Join(home, "actions-runner")); !os.IsNotExist(err) {
		t.Errorf("a runner download off its pin was unpacked: %v", err)
	}
	// The program as the agent fills it in names the pinned runner.
	p := lines(printed)
	if want := runnerURL(runnerZipName(runnerArch())); p["pinned-url"] != want {
		t.Errorf("the guest downloads %q, want %q", p["pinned-url"], want)
	}
	if want := runnerWindowsSHA256[runnerArch()]; !strings.EqualFold(p["pinned-sum"], want) {
		t.Errorf("the guest pins %q, want %q", p["pinned-sum"], want)
	}

	// The pinned download runs: ready first, then the runner with the
	// registration, the place's size and the fork guard, which refuses the
	// fork's job and allows the repository's own; then the VM turns off.
	home, printed, port = run(hex.EncodeToString(sum[:]), false)
	if !strings.HasPrefix(port, guestReady+"\n") {
		t.Errorf("the guest did not say it was ready before taking the registration:\n%s", port)
	}
	got := lines(port)
	hook := filepath.Join(home, "runner-up", guardFile)
	for k, want := range map[string]string{"registration": registration, "hook": hook, "size": "4 8192", "fork": "1", "own": "0"} {
		if got[k] != want {
			t.Errorf("the runner's %s is %q, want %q:\n%s", k, got[k], want, port)
		}
	}
	for _, want := range []string{
		"runner-up guest: starting the runner\n",
		"runner-up refuses this job: pull request from a-contributor/tool.",
		"runner-up: pull_request job from this repository; allowed\n",
		"runner-up guest: the runner ended with 7\nturned off\n",
	} {
		if !strings.Contains(port, want) {
			t.Errorf("the guest's port lacks %q:\n%s", want, port)
		}
	}
	if !strings.HasSuffix(port, "turned off\n") {
		t.Errorf("the VM did not turn off at the end:\n%s", port)
	}
	// Only the runner's environment held the registration: not the guest
	// program's own, nor any file in the profile.
	if v := lines(printed)["own-registration"]; v != "" {
		t.Errorf("the guest program's own environment holds the registration %q", v)
	}
	filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			if b, _ := os.ReadFile(path); strings.Contains(string(b), registration) {
				t.Errorf("%s holds the registration", path)
			}
		}
		return nil
	})

	// A place that allows forks gives the runner no job-started hook.
	home, _, port = run(hex.EncodeToString(sum[:]), true)
	got = lines(port)
	if got["registration"] != registration || got["hook"] != "" || !strings.Contains(port, "the runner ended with 7\n") {
		t.Errorf("a place that allows forks gave:\n%s", port)
	}
	if _, err := os.Stat(filepath.Join(home, "runner-up", guardFile)); !os.IsNotExist(err) {
		t.Errorf("a place that allows forks has a fork guard: %v", err)
	}
}

// The image's sign-in task runs guest.ps1 from the root of a CD drive, so
// Windows itself must read the disc's names and bytes.
func TestWindowsReadsTheGuestsSeedDisc(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("mounting a disc image needs an administrator")
	}
	powershell, env := windowsPowerShell(t)
	a := &AgentRun{Place: "win-pc-1", Guest: config.GuestWindows, Cores: 4, MemoryMB: 8192, Self: testbin.Build(t, "")}
	label, files, err := hypervDisc(a, runnerArch(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	iso, err := seedISO(label, files)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "seed.iso")
	if err := os.WriteFile(path, iso, 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$image = Mount-DiskImage -ImagePath %[1]s -PassThru
try {
    $drive = $null
    for ($i = 0; $i -lt 50 -and -not $drive; $i++) { $drive = ($image | Get-Volume).DriveLetter; if (-not $drive) { Start-Sleep -Milliseconds 200 } }
    Get-ChildItem -LiteralPath "$($drive):\" | ForEach-Object { $_.Name + ' ' + (Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash }
} finally {
    $null = Dismount-DiskImage -ImagePath %[1]s
}
`, psQuote(path))
	cmd := testrun.Command(t, powershell, encodedScript(script)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Windows could not read the disc: %v\n%s", err, out)
	}
	want := map[string]string{}
	for name, b := range files {
		s := sha256.Sum256(b)
		want[name] = strings.ToUpper(hex.EncodeToString(s[:]))
	}
	got := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(strings.ReplaceAll(string(out), "\r", "")), "\n") {
		if name, sum, ok := strings.Cut(l, " "); ok {
			got[name] = sum
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Windows reads the disc as\n%s\nwant %v", out, want)
	}
}

// The agent makes a Windows guest's VM under secure boot trusting Windows,
// and a Linux guest's under the Microsoft UEFI CA, which signs Ubuntu's shim:
// its script binds the template to the Hyper-V functions' own parameter.
func TestWindowsGuestBootsUnderWindowsSecureBoot(t *testing.T) {
	powershell, env := windowsPowerShell(t)
	// The VM steps stand in; New-RunnerUpVM keeps its own parameters.
	const stand = `$param = (Get-Command New-RunnerUpVM).ScriptBlock.Ast.Body.ParamBlock.Extent.Text
function New-RunnerUpDisk { }
function Start-RunnerUpVM { }
Invoke-Expression ('function New-RunnerUpVM { ' + $param + ' [Console]::Out.WriteLine(''template='' + $SecureBootTemplate) }')
`
	for guest, want := range map[string]string{"": secureBootLinux, config.GuestLinux: secureBootLinux, config.GuestWindows: secureBootWindows} {
		a := &AgentRun{Place: "win-pc-1", Guest: guest, Cores: 4, MemoryMB: 8192, Image: `C:\runner-up\job.vhdx`}
		cmd := testrun.Command(t, powershell, encodedPowerShell(stand+hypervCreateScript(a, `C:\runner-up\places\win-pc-1\vm`))...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "template="+want {
			t.Errorf("a %q guest's VM is made with %q (%v), want template=%s", guest, out, err, want)
		}
	}
}
