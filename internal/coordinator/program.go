package coordinator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/proc"
	releases "github.com/kunchenguid/runner-up/internal/release"
)

// When the setup names a release (config.Release), the coordinator runs that
// version of runner-up. It keeps its programs under its state folder, in
// program/:
//
//	<version>/runner-up  each version it ran or installed
//	current              the version it runs and kept, for a supervisor to start
//	archives/            the release archives it fetched, for the agents
//	trial.json           the version on trial, and the version before it
//	refused.json         the versions it went back from, and why
//
// When the setup names another version, the coordinator fetches its archive
// for this platform, checks its SHA-256 against the setup's, unpacks it, and
// asks the new program its version and to check the setup (check-config). If
// all holds, it stops serving as a stop signal would - running jobs carry
// on - and replaces itself with the new program, in the same process, so
// launchd or systemd keeps supervising it.
//
// The new version is on trial for TrialWindow, and current points at it only
// once it has served that long. If it stops sooner - it fails, crashes or
// never starts serving - the supervisor starts the program it always starts,
// the version before, which sees the trial left behind, refuses that version
// and serves on. A refused version stays refused until the setup names
// another version or another SHA-256. A stop signal during the trial ends it
// with no refusal: a person or a restart stopped it.

// TrialWindow is how long a new version must serve before it is kept.
var TrialWindow = time.Minute

// Program is the coordinator's own program and the versions under dir.
type Program struct {
	Dir string
	// Running is the running program's version, and Self its path.
	Running string
	Self    string
	// Setup is the setup file a new program must accept before the
	// coordinator restarts into it.
	Setup  string
	Client *http.Client
	Log    *slog.Logger
	now    func() time.Time
}

// NewProgram is the coordinator's program at self, running version, with its
// versions under stateDir.
func NewProgram(stateDir, setup, self, version string, log *slog.Logger) *Program {
	return &Program{Dir: filepath.Join(stateDir, "program"), Running: version, Self: self, Setup: setup,
		Client: &http.Client{Timeout: 10 * time.Minute}, Log: log, now: time.Now}
}

// Restart is a program to replace the coordinator with. Trial is set when it
// is a new version on trial, and empty when it is the version the
// coordinator goes back to.
type Restart struct {
	Path    string
	Version string
	Trial   *trial
}

type trial struct {
	Version string `json:"version"`
	// SHA256 is the setup's SHA-256 of the archive it came from.
	SHA256 string `json:"sha256"`
	// From is the version it replaced, to go back to.
	From string `json:"from"`
	// Started is when it began serving; zero until it does.
	Started time.Time `json:"started,omitzero"`
}

type refusal struct {
	Version string    `json:"version"`
	SHA256  string    `json:"sha256"`
	Why     string    `json:"why"`
	At      time.Time `json:"at"`
}

func (p *Program) path(version string) string { return filepath.Join(p.Dir, version, "runner-up") }
func (p *Program) trialPath() string          { return filepath.Join(p.Dir, "trial.json") }
func (p *Program) refusedPath() string        { return filepath.Join(p.Dir, "refused.json") }

// Current is the link to the version the coordinator runs, for a supervisor
// to start.
func (p *Program) Current() string { return filepath.Join(p.Dir, "current") }

// Begin reads the trial a restart left, as the coordinator starts. A trial of
// this version that has not started yet starts now. Any other trial stopped
// before it was kept: Begin refuses its version and, unless this is the
// version before it, returns that version to run instead. It returns nil to
// serve on.
func (p *Program) Begin() *Restart {
	var t trial
	if config.ReadJSON(p.trialPath(), &t) != nil {
		return nil
	}
	if t.Version == p.Running && t.Started.IsZero() {
		t.Started = p.now()
		if err := config.WriteJSON(p.trialPath(), t); err != nil {
			p.Log.Error("could not start the trial of the new version", "err", err)
		}
		p.Log.Info("runner-up runs a new version on trial: if it stops within the trial, the version before it runs again",
			"version", t.Version, "before", t.From, "trial", TrialWindow)
		return nil
	}
	why := fmt.Sprintf("it stopped within %s of starting", TrialWindow)
	if t.Started.IsZero() {
		why = "it never started serving"
	}
	p.refuse(t.Version, t.SHA256, why)
	os.Remove(p.trialPath())
	p.Log.Error("runner-up refused a new version and goes back to the version before it", "version", t.Version,
		"why", why, "before", t.From)
	if err := p.point("current", t.From); err != nil {
		p.Log.Error("could not point current back at the version before", "err", err)
	}
	if t.From == p.Running {
		return nil
	}
	return &Restart{Path: p.path(t.From), Version: t.From}
}

// Keep ends a trial of the running version once it has served TrialWindow,
// and points current at it.
func (p *Program) Keep() bool {
	var t trial
	if config.ReadJSON(p.trialPath(), &t) != nil || t.Version != p.Running || t.Started.IsZero() ||
		p.now().Sub(t.Started) < TrialWindow {
		return false
	}
	if err := p.point("current", p.Running); err != nil {
		p.Log.Error("could not point current at the new version; trying again", "err", err)
		return false
	}
	os.Remove(p.trialPath())
	p.Log.Info("runner-up kept the new version after its trial", "version", t.Version, "current", p.Current())
	p.prune()
	return true
}

// onTrial says whether the running version is on trial.
func (p *Program) onTrial() bool {
	var t trial
	return config.ReadJSON(p.trialPath(), &t) == nil && t.Version == p.Running
}

// Stopped ends the running version's trial when a stop signal stops the
// coordinator: nothing was wrong with it.
func (p *Program) Stopped() {
	var t trial
	if config.ReadJSON(p.trialPath(), &t) == nil && t.Version == p.Running && !t.Started.IsZero() {
		os.Remove(p.trialPath())
	}
}

// ErrRefused is a release the coordinator refused before.
type ErrRefused struct{ refusal }

func (e ErrRefused) Error() string {
	return fmt.Sprintf("runner-up refused %s at %s because %s; name another version or SHA-256 in the setup to try again",
		e.Version, e.At.Local().Format(time.RFC3339), e.Why)
}

// Want readies the setup's release rel. When rel is the running version it
// makes current that version's program, fetching it if need be, unless the
// version is on trial (Keep does that), and returns nil. Otherwise it returns
// the checked new program to restart into, unless it refused that release
// before.
func (p *Program) Want(ctx context.Context, rel *config.Release) (*Restart, error) {
	if rel == nil {
		return nil, nil
	}
	platform := releases.Platform(runtime.GOOS, runtime.GOARCH)
	sum, ok := rel.SHA256[platform]
	if !ok {
		return nil, fmt.Errorf("the setup's release names no SHA-256 for %s, the coordinator's platform", platform)
	}
	if rel.Version == p.Running {
		if target, err := os.Readlink(p.Current()); err == nil && target == filepath.Join(p.Running, "runner-up") || p.onTrial() {
			return nil, nil
		}
		if _, err := p.Install(ctx, rel); err != nil {
			return nil, err
		}
		return nil, p.point("current", p.Running)
	}
	if r := p.refused(rel.Version, sum); r != nil {
		return nil, ErrRefused{*r}
	}
	path, err := p.Install(ctx, rel)
	if err != nil {
		return nil, err
	}
	if err := p.preflight(ctx, path, rel.Version); err != nil {
		p.refuse(rel.Version, sum, err.Error())
		return nil, err
	}
	return &Restart{Path: path, Version: rel.Version, Trial: &trial{Version: rel.Version, SHA256: sum, From: p.Running}}, nil
}

// Ready fetches and checks rel's program for this platform and points current
// at it, for a supervisor to start, without restarting anything. It returns
// current's path.
func (p *Program) Ready(ctx context.Context, rel *config.Release) (string, error) {
	platform := releases.Platform(runtime.GOOS, runtime.GOARCH)
	if r := p.refused(rel.Version, rel.SHA256[platform]); r != nil {
		return "", ErrRefused{*r}
	}
	path, err := p.Install(ctx, rel)
	if err != nil {
		return "", err
	}
	if err := p.preflight(ctx, path, rel.Version); err != nil {
		return "", err
	}
	return p.Current(), p.point("current", rel.Version)
}

// Install fetches rel's program for this platform into its version's folder,
// unless it is there already, and returns its path.
func (p *Program) Install(ctx context.Context, rel *config.Release) (string, error) {
	platform := releases.Platform(runtime.GOOS, runtime.GOARCH)
	path := p.path(rel.Version)
	sumFile := filepath.Join(filepath.Dir(path), "archive.sha256")
	if b, err := os.ReadFile(sumFile); err == nil && string(b) == rel.SHA256[platform] {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	archive, err := p.Archive(ctx, rel, platform)
	if err != nil {
		return "", err
	}
	prog, err := releases.Program(archive, platform)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(p.Dir, ".new-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, "runner-up"), prog, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, "archive.sha256"), []byte(rel.SHA256[platform]), 0o600); err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); err == nil {
		// A folder of this version from another archive: place runners may
		// still run its program, which stays readable once moved aside.
		aside := fmt.Sprintf("%s.replaced-%d", dir, p.now().UnixNano())
		if err := os.Rename(dir, aside); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	p.Log.Info("runner-up fetched a release", "version", rel.Version, "platform", platform, "path", path)
	return path, nil
}

// Archive is rel's archive for platform, checked against the setup's
// SHA-256. It is kept under archives/ for the next machine that needs it.
func (p *Program) Archive(ctx context.Context, rel *config.Release, platform string) ([]byte, error) {
	sum, ok := rel.SHA256[platform]
	if !ok {
		return nil, fmt.Errorf("the setup's release names no SHA-256 for %s", platform)
	}
	name := releases.ArchiveName(rel.Version, platform)
	dir := filepath.Join(p.Dir, "archives")
	kept := filepath.Join(dir, name)
	if b, err := os.ReadFile(kept); err == nil && releases.SHA256(b) == sum {
		return b, nil
	}
	url := rel.ArchiveURL(name)
	b, err := releases.Fetch(ctx, p.Client, url, releases.MaxArchive)
	if err != nil {
		return nil, fmt.Errorf("could not fetch %s: %w", name, err)
	}
	if got := releases.SHA256(b); got != sum {
		return nil, fmt.Errorf("%s from %s has SHA-256 %s, not the %s the setup names", name, url, got, sum)
	}
	if err := os.MkdirAll(dir, 0o700); err == nil {
		if err := os.WriteFile(kept+".tmp", b, 0o600); err == nil {
			os.Rename(kept+".tmp", kept)
		}
	}
	// Archives of other versions are no longer needed.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "runner-up-"+rel.Version+"-") {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return b, nil
}

// preflight asks the program at path its version, which must be version,
// and to check the setup file.
func (p *Program) preflight(ctx context.Context, path, version string) error {
	c, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(c, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("runner-up %s does not run here: %s", version, agent.FirstLine(string(out), err))
	}
	if got := strings.TrimSpace(string(out)); got != version {
		return fmt.Errorf("the program in runner-up %s's archive says it is %q", version, got)
	}
	out, err = exec.CommandContext(c, path, "check-config", "--config", p.Setup).CombinedOutput()
	if err != nil {
		return fmt.Errorf("runner-up %s does not accept the setup: %s", version, agent.FirstLine(string(out), err))
	}
	return nil
}

// Exec replaces the coordinator with r's program, serving the setup file.
// For a new version it first keeps the running program as the version to go
// back to and starts the trial. It returns only when the program could not be
// run.
func (p *Program) Exec(r *Restart) error {
	if r.Trial != nil {
		if err := p.keepRunning(); err != nil {
			return fmt.Errorf("could not keep the running program to go back to: %w", err)
		}
		if err := config.WriteJSON(p.trialPath(), r.Trial); err != nil {
			return err
		}
	}
	err := proc.Exec(r.Path, []string{r.Path, "serve", "--config", p.Setup}, os.Environ())
	if r.Trial != nil {
		os.Remove(p.trialPath())
		p.refuse(r.Version, r.Trial.SHA256, fmt.Sprintf("it could not be run: %v", err))
	}
	return err
}

// keepRunning copies the running program into its version's folder, unless
// it runs from there, so the coordinator can go back to it.
func (p *Program) keepRunning() error {
	path := p.path(p.Running)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	in, err := os.Open(p.Self)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// point makes the link name point at version's program.
func (p *Program) point(name, version string) error {
	link := filepath.Join(p.Dir, name)
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return err
	}
	tmp := link + ".new"
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join(version, "runner-up"), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

func (p *Program) refusals() []refusal {
	var rs []refusal
	config.ReadJSON(p.refusedPath(), &rs)
	return rs
}

func (p *Program) refused(version, sum string) *refusal {
	for _, r := range p.refusals() {
		if r.Version == version && r.SHA256 == sum {
			return &r
		}
	}
	return nil
}

func (p *Program) refuse(version, sum, why string) {
	rs := slices.DeleteFunc(p.refusals(), func(r refusal) bool { return r.Version == version && r.SHA256 == sum })
	rs = append(rs, refusal{Version: version, SHA256: sum, Why: why, At: p.now()})
	err := os.MkdirAll(p.Dir, 0o700)
	if err == nil {
		err = config.WriteJSON(p.refusedPath(), rs)
	}
	if err != nil {
		p.Log.Error("could not record the refused version; it may be tried again", "version", version, "err", err)
	}
}

// programKeep is how long a version's folder outlives its last use: place
// runners started by an earlier version run as its program until their job
// ends.
const programKeep = 7 * 24 * time.Hour

// prune removes the folders of versions neither running nor current that
// have not changed for programKeep.
func (p *Program) prune() {
	keep := map[string]bool{p.Running: true}
	if target, err := os.Readlink(p.Current()); err == nil {
		keep[filepath.Dir(target)] = true
	}
	entries, _ := os.ReadDir(p.Dir)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() || keep[e.Name()] || e.Name() == "archives" || strings.HasPrefix(e.Name(), ".") ||
			p.now().Sub(info.ModTime()) < programKeep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(p.Dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			p.Log.Warn("could not remove an old version's program", "version", e.Name(), "err", err)
		}
	}
}
