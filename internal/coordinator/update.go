package coordinator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/quote"
	releases "github.com/kunchenguid/runner-up/internal/release"
)

// Once the coordinator runs the version the setup's release names, it copies
// that release to every machine it reaches over SSH, as the machine's agent:
// it reads the machine's platform, takes the platform's archive (checked
// against the setup's SHA-256), sends the program over SSH into a folder of
// its version - .local/share/runner-up/<version> in the machine's home, or
// %LOCALAPPDATA%\runner-up\<version> on Windows - where the machine checks
// its SHA-256 before keeping it, and asks it its version. From then on that
// machine's jobs run with it; jobs already running keep the agent they
// started with, in its own version's folder. The coordinator's own machine
// runs the coordinator's program as its agent. agents.json under the state
// folder records each machine's copy; a machine that cannot be reached keeps
// its earlier agent and is tried again later.
//
// A Windows machine with account places also runs runner-up's service, as
// SYSTEM, which only an administrator could replace. It replaces itself, but
// only with a release the release signing key signed (internal/release):
// the coordinator holds the machine's account places from new jobs, waits for
// their jobs to end, and has the machine's agent hand the service the
// release's Windows archive with checksums.txt and its signature. The service
// checks them, installs the program in a folder of its own version, and
// restarts as it; a new service that stops within a minute goes back to the
// one before. Once the service answers with the new version, the places take
// jobs again.

// agentRetry is how long a machine whose copy or service update failed waits
// before the coordinator tries it again.
var agentRetry = 10 * time.Minute

// Updater keeps the coordinator, its machines' agents and their Windows
// services on the setup's release.
type Updater struct {
	Program *Program
	Log     *slog.Logger
	// Self is the place runners' program, which the coordinator's own machine
	// runs as its agent.
	Self string
	// Service is the coordinator's, whose account places a service update
	// holds.
	Service *Service
	// Changed says a machine's agent changed: the setup is to be applied again
	// (Setup). Ready carries a new version to restart into.
	Changed chan struct{}
	Ready   chan *Restart

	mu      sync.Mutex
	agents  map[string]installedAgent
	working map[string]bool
	retry   map[string]time.Time
	// asked is the release last asked of Program.Want, and askAgain when it
	// is asked again after a failure.
	asked    string
	askAgain time.Time
	wanting  bool
}

// installedAgent is a machine's copy of a release.
type installedAgent struct {
	Version string `json:"version"`
	Path    string `json:"path"`
	// Service is the version the machine's runner-up service answered with
	// last, for a Windows machine with account places.
	Service string `json:"service,omitempty"`
}

func NewUpdater(p *Program, self string, log *slog.Logger) *Updater {
	u := &Updater{Program: p, Self: self, Log: log, Changed: make(chan struct{}, 1), Ready: make(chan *Restart, 1),
		agents: map[string]installedAgent{}, working: map[string]bool{}, retry: map[string]time.Time{}}
	config.ReadJSON(u.agentsPath(), &u.agents)
	return u
}

func (u *Updater) agentsPath() string {
	return filepath.Join(filepath.Dir(u.Program.Dir), "agents.json")
}

// Setup is base with each machine's agent the release's: the coordinator's
// program on its own machine, and each other machine's recorded copy. A
// setup that names no release keeps its agents.
func (u *Updater) Setup(base *config.Config) *config.Config {
	if base.Release == nil {
		return base
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	out := *base
	out.Machines = slices.Clone(base.Machines)
	for i, m := range out.Machines {
		if m.Local() {
			out.Machines[i].Agent = u.Self
		} else if a, ok := u.agents[m.Name]; ok {
			out.Machines[i].Agent = a.Path
		}
	}
	return &out
}

// Tick starts whatever the release still needs, each in the background: the
// coordinator's own new version, then each machine's agent and service.
func (u *Updater) Tick(ctx context.Context, base *config.Config) {
	rel := base.Release
	if rel == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	key := rel.Version + " " + rel.SHA256[releases.Platform(runtime.GOOS, runtime.GOARCH)] + " " + rel.URL
	now := time.Now()
	if !u.wanting && (key != u.asked || !u.askAgain.IsZero() && now.After(u.askAgain)) {
		u.asked, u.askAgain, u.wanting = key, time.Time{}, true
		go u.want(ctx, rel, key)
	}
	if rel.Version != u.Program.Running {
		return
	}
	for _, m := range base.Machines {
		if m.Local() || u.working[m.Name] || now.Before(u.retry[m.Name]) {
			continue
		}
		a := u.agents[m.Name]
		var work func(context.Context, config.Machine, *config.Release) error
		switch {
		case a.Version != rel.Version:
			work = u.copyAgent
		case m.Windows() && hasAccountPlaces(m) && a.Service != rel.Version:
			work = u.updateService
		default:
			continue
		}
		u.working[m.Name] = true
		go func() {
			err := work(ctx, m, rel)
			u.mu.Lock()
			delete(u.working, m.Name)
			if err != nil && ctx.Err() == nil {
				u.retry[m.Name] = time.Now().Add(agentRetry)
				u.Log.Error("could not bring the machine to the setup's release; it keeps what it runs and is tried again later",
					"machine", m.Name, "version", rel.Version, "retry", agentRetry, "err", err)
			}
			u.mu.Unlock()
		}()
	}
}

func (u *Updater) want(ctx context.Context, rel *config.Release, key string) {
	r, err := u.Program.Want(ctx, rel)
	u.mu.Lock()
	u.wanting = false
	var refused ErrRefused
	switch {
	case errors.As(err, &refused):
		u.Log.Warn("the setup names a version runner-up refused; it stays on its version", "version", rel.Version,
			"running", u.Program.Running, "err", err)
	case err != nil && ctx.Err() == nil:
		u.askAgain = time.Now().Add(agentRetry)
		u.Log.Error("could not ready the setup's release; runner-up stays on its version and tries again later",
			"version", rel.Version, "running", u.Program.Running, "retry", agentRetry, "err", err)
	}
	u.mu.Unlock()
	if r != nil {
		select {
		case u.Ready <- r:
		case <-ctx.Done():
		}
	}
}

func hasAccountPlaces(m config.Machine) bool {
	return slices.ContainsFunc(m.Places, func(p config.Place) bool { return p.Provider == config.ProviderAccount })
}

// record keeps machine name's copy and says the setup is to be applied again.
func (u *Updater) record(name string, change func(*installedAgent)) {
	u.mu.Lock()
	a := u.agents[name]
	change(&a)
	u.agents[name] = a
	if err := config.WriteJSON(u.agentsPath(), u.agents); err != nil {
		u.Log.Error("could not record the machine's agent", "machine", name, "err", err)
	}
	u.mu.Unlock()
	select {
	case u.Changed <- struct{}{}:
	default:
	}
}

// copyAgent copies rel to machine m and checks it runs there.
func (u *Updater) copyAgent(ctx context.Context, m config.Machine, rel *config.Release) error {
	c, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	platform, err := machinePlatform(c, m)
	if err != nil {
		return err
	}
	archive, err := u.Program.Archive(c, rel, platform)
	if err != nil {
		return err
	}
	prog, err := releases.Program(archive, platform)
	if err != nil {
		return err
	}
	path, err := sendAgent(c, m, rel.Version, prog)
	if err != nil {
		return err
	}
	try := m
	try.Agent = path
	argv := AgentCommand(try, "version")
	out, err := exec.CommandContext(c, argv[0], argv[1:]...).Output()
	if err != nil {
		return fmt.Errorf("the copy at %s does not run: %s", path, agent.FirstLine(StderrOf(err), err))
	}
	if got := strings.TrimSpace(string(out)); got != rel.Version {
		return fmt.Errorf("the copy at %s says it is %q", path, got)
	}
	u.record(m.Name, func(a *installedAgent) { a.Version, a.Path = rel.Version, path })
	u.Log.Info("copied runner-up to the machine as its agent", "machine", m.Name, "version", rel.Version, "platform", platform, "path", path)
	return nil
}

// machinePlatform reads machine m's platform, as a release names it.
func machinePlatform(ctx context.Context, m config.Machine) (string, error) {
	line := "uname -sm"
	if m.Windows() {
		line = powershellLine("[Console]::Out.WriteLine($env:PROCESSOR_ARCHITECTURE)")
	}
	argv := append(SshArgs(m), line)
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	if err != nil {
		return "", fmt.Errorf("could not read the machine's platform: %s", agent.FirstLine(StderrOf(err), err))
	}
	got := strings.TrimSpace(string(out))
	platform := map[string]string{
		"Darwin arm64": "darwin-arm64", "Darwin x86_64": "darwin-amd64",
		"Linux x86_64": "linux-amd64", "Linux aarch64": "linux-arm64", "Linux arm64": "linux-arm64",
		"AMD64": "windows-amd64", "ARM64": "windows-arm64",
	}[got]
	if platform == "" {
		return "", fmt.Errorf("the machine runs %q, which no runner-up release is built for", got)
	}
	return platform, nil
}

// powershellLine is a command line that runs script in Windows PowerShell,
// read the same by cmd and PowerShell: the script travels base64 encoded.
func powershellLine(script string) string {
	return strings.Join(agent.EncodedScript(script), " ")
}

// sendAgent sends prog, version's program, to machine m over SSH, where it
// is kept only when its SHA-256 matches, and returns its path there.
func sendAgent(ctx context.Context, m config.Machine, version string, prog []byte) (string, error) {
	line, path := agentCopy(m, version, releases.SHA256(prog))
	argv := append(SshArgs(m), line)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(prog)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("could not copy runner-up %s to the machine: %s", version, agent.FirstLine(StderrOf(err), err))
	}
	if m.Windows() {
		path = strings.TrimSpace(string(out))
		if err := quote.CheckWinWord(path, true); err != nil {
			return "", fmt.Errorf("the copy's path %w", err)
		}
	}
	return path, nil
}

// agentCopy is the command line that keeps version's program, whose SHA-256
// is sum, from standard input on machine m, and the program's path there,
// which a Windows machine prints instead.
func agentCopy(m config.Machine, version, sum string) (line, path string) {
	if m.Windows() {
		return powershellLine(strings.NewReplacer("@VERSION@", version, "@SHA256@", sum).Replace(windowsAgentCopy)), ""
	}
	path = ".local/share/runner-up/" + version + "/runner-up"
	return "sh -c " + quote.ShQuote(strings.NewReplacer("@DIR@", quote.ShQuote(filepath.Dir(path)), "@SHA256@", sum).Replace(unixAgentCopy)), path
}

// unixAgentCopy keeps standard input as the program in @DIR@, once its
// SHA-256 is @SHA256@. The program's own folder holds only it, so jobs of an
// earlier version keep theirs.
const unixAgentCopy = `set -e
mkdir -p @DIR@
cd @DIR@
cat > runner-up.new
if command -v sha256sum >/dev/null 2>&1; then sum=$(sha256sum < runner-up.new); else sum=$(shasum -a 256 < runner-up.new); fi
case "$sum" in
@SHA256@*) ;;
*) rm -f runner-up.new; echo "the copy's SHA-256 is not the release's" >&2; exit 1 ;;
esac
chmod 0755 runner-up.new
mv -f runner-up.new runner-up`

// windowsAgentCopy is unixAgentCopy for Windows PowerShell, in the runner
// account's %LOCALAPPDATA%. It prints the program's path. A program of the
// same version already there may be running jobs, and stays.
const windowsAgentCopy = `$ErrorActionPreference = 'Stop'
$dir = Join-Path $env:LOCALAPPDATA 'runner-up\@VERSION@'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$new = Join-Path $dir 'runner-up.exe.new'
$exe = Join-Path $dir 'runner-up.exe'
$in = [Console]::OpenStandardInput()
$out = [IO.File]::Create($new)
try { $in.CopyTo($out) } finally { $out.Close() }
$sum = (Get-FileHash -Algorithm SHA256 -LiteralPath $new).Hash.ToLowerInvariant()
if ($sum -ne '@SHA256@') { Remove-Item -LiteralPath $new; throw "the copy's SHA-256 is $sum, not the release's" }
if ((Test-Path -LiteralPath $exe) -and (Get-FileHash -Algorithm SHA256 -LiteralPath $exe).Hash.ToLowerInvariant() -eq $sum) {
    Remove-Item -LiteralPath $new
} else {
    Move-Item -Force -LiteralPath $new -Destination $exe
}
[Console]::Out.WriteLine($exe)`

// serviceWait is how long the coordinator waits for a machine's service to
// answer with its new version after handing it the release.
var serviceWait = 3 * time.Minute

// updateService brings Windows machine m's runner-up service to rel.
func (u *Updater) updateService(ctx context.Context, m config.Machine, rel *config.Release) error {
	running, err := serviceVersion(ctx, m)
	if err != nil {
		return err
	}
	if running == rel.Version {
		u.record(m.Name, func(a *installedAgent) { a.Service = running })
		return nil
	}
	if releases.Compare(running, rel.Version) > 0 {
		return fmt.Errorf("its service runs %s, newer than the setup's %s, and never goes back to an older version", running, rel.Version)
	}
	platform, err := machinePlatform(ctx, m)
	if err != nil {
		return err
	}
	signed, err := u.signedArchive(ctx, rel, platform)
	if err != nil {
		return err
	}
	var ids []string
	for _, p := range m.Places {
		if p.Provider == config.ProviderAccount {
			ids = append(ids, p.ID)
		}
	}
	u.Service.Hold(ids, "runner-up's service on the machine is updating to "+rel.Version)
	defer u.Service.Unhold(ids)
	u.Log.Info("waiting for the machine's account jobs to end to update its service", "machine", m.Name, "service", running, "version", rel.Version)
	stateDir := u.Service.Setup().StateDir
	for slices.ContainsFunc(ids, func(id string) bool {
		held, err := lock.HeldBy(filepath.Join(stateDir, "places", id, "busy.lock"))
		return err != nil || held
	}) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if err := handService(ctx, m, signed); err != nil {
		return err
	}
	deadline := time.Now().Add(serviceWait)
	for {
		got, err := serviceVersion(ctx, m)
		if err == nil && got == rel.Version {
			u.record(m.Name, func(a *installedAgent) { a.Service = got })
			u.Log.Info("the machine's service updated itself", "machine", m.Name, "version", got)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("its service did not answer as %s within %s (last: %q, %v); see service.log on the machine", rel.Version, serviceWait, got, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// signedArchive is rel's archive for platform with its signed checksums,
// checked here as the service will check them.
func (u *Updater) signedArchive(ctx context.Context, rel *config.Release, platform string) (releases.Signed, error) {
	s := releases.Signed{Name: releases.ArchiveName(rel.Version, platform)}
	var err error
	if s.Archive, err = u.Program.Archive(ctx, rel, platform); err != nil {
		return s, err
	}
	if s.Checksums, err = releases.Fetch(ctx, u.Program.Client, rel.ArchiveURL("checksums.txt"), 1<<20); err != nil {
		return s, err
	}
	if s.Signature, err = releases.Fetch(ctx, u.Program.Client, rel.ArchiveURL("checksums.txt.sig"), 1<<10); err != nil {
		return s, fmt.Errorf("the release has no signature: %w", err)
	}
	if _, err := s.Verify(platform, releases.SigningKeys()); err != nil {
		return s, fmt.Errorf("its service accepts only a signed release: %w", err)
	}
	return s, nil
}

// serviceVersion asks machine m's agent the version of its runner-up service.
func serviceVersion(ctx context.Context, m config.Machine) (string, error) {
	c, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	argv := AgentCommand(m, "agent", "service-version")
	out, err := exec.CommandContext(c, argv[0], argv[1:]...).Output()
	if err != nil {
		return "", fmt.Errorf("could not read its service's version: %s", agent.FirstLine(StderrOf(err), err))
	}
	return strings.TrimSpace(string(out)), nil
}

// handService hands machine m's service the signed release through its agent.
func handService(ctx context.Context, m config.Machine, s releases.Signed) error {
	c, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	head, err := json.Marshal(agent.ServiceUpdate{Archive: s.Name, Size: int64(len(s.Archive)),
		Checksums: string(s.Checksums), Signature: base64.StdEncoding.EncodeToString(s.Signature)})
	if err != nil {
		return err
	}
	argv := AgentCommand(m, "agent", "update-service")
	cmd := exec.CommandContext(c, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(slices.Concat(head, []byte("\n"), s.Archive))
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The service's own answer is the last thing the agent says.
		last := ""
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				last = line
			}
		}
		return fmt.Errorf("its service did not take the release: %s", agent.FirstLine(last, err))
	}
	return nil
}
