package coordinator

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/proc"
	"github.com/kunchenguid/runner-up/internal/quote"
)

// The place runner is this same program run as `runner-up run-place`. The
// coordinator starts it in its own session, so it outlives a coordinator
// restart, with the place's locked busy.lock as file descriptor 3 and the
// runner's one-time registration on standard input, never in an argument or
// an environment. It runs the place's machine's agent - over SSH, or directly
// on the coordinator's own machine - hands it the registration, and holds the
// agent's input open until the runner ends. The agent makes a fresh place,
// runs one job there and removes the place whatever happened; when its input
// closes, because this process ended or gave up on the runner, it stops the
// job and cleans up.

// ProcLauncher is the real Launcher.
type ProcLauncher struct {
	cfg        *config.Config
	configPath string
	self       string
	log        *slog.Logger
	Exited     chan string
}

// NewProcLauncher starts place runners with self, this program.
func NewProcLauncher(cfg *config.Config, configPath, self string, log *slog.Logger) *ProcLauncher {
	return &ProcLauncher{cfg: cfg, configPath: configPath, self: self, log: log, Exited: make(chan string, 16)}
}

func (l *ProcLauncher) Launch(p config.Place, lock *os.File, jit string) error {
	logPath := filepath.Join(l.cfg.LogsDir(), fmt.Sprintf("%s-%s.log", p.ID, time.Now().UTC().Format("20060102T150405Z")))
	out, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd := exec.Command(l.self, "run-place", "--config", l.configPath, "--place", p.ID)
	cmd.Stdin = strings.NewReader(jit + "\n")
	cmd.Stdout, cmd.Stderr = out, out
	cmd.ExtraFiles = []*os.File{lock}
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	var run PlaceRun
	path := filepath.Join(l.cfg.PlaceDir(p.ID), "place.json")
	if config.ReadJSON(path, &run) == nil {
		run.PID = cmd.Process.Pid
		config.WriteJSON(path, run)
	}
	go func() {
		err := cmd.Wait()
		l.log.Info("place finished", "place", p.ID, "exit", agent.ExitCode(err))
		l.Exited <- p.ID
	}()
	return nil
}

// Stop ends a place's runner; the place runner closes the agent's input and
// the agent cleans up. After a coordinator restart the process is no longer a
// child, so its pid comes from place.json, and only while the place's lock is
// still held.
func (l *ProcLauncher) Stop(placeID string) error {
	var run PlaceRun
	if err := config.ReadJSON(filepath.Join(l.cfg.PlaceDir(placeID), "place.json"), &run); err != nil {
		return err
	}
	held, err := lock.HeldBy(filepath.Join(l.cfg.PlaceDir(placeID), "busy.lock"))
	if err != nil || !held || run.PID <= 1 {
		return fmt.Errorf("place %s has no live runner", placeID)
	}
	return proc.Terminate(run.PID)
}

// AgentCommand is the command line that runs the agent on machine m with
// args: the agent itself on the coordinator's own machine, or ssh to it.
// ssh joins its command's words with spaces for the far side's shell, so
// every word is quoted for that shell: a POSIX shell, or on Windows cmd or
// PowerShell, whichever the machine's SSH server starts.
func AgentCommand(m config.Machine, args ...string) []string {
	argv := append([]string{m.AgentPath()}, args...)
	if m.Local() {
		return argv
	}
	return append(SshArgs(m), remoteLine(m, argv))
}

// SshArgs is ssh to the machine, before the remote command.
func SshArgs(m config.Machine) []string {
	ssh := []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4"}
	return append(ssh, m.SSH...)
}

// remoteLine joins argv into the command line m's shell reads as argv.
func remoteLine(m config.Machine, argv []string) string {
	quoteWord := quote.ShQuote
	if m.Windows() {
		quoteWord = quote.WinQuote
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = quoteWord(a)
	}
	return strings.Join(quoted, " ")
}

// RunArgs are the agent's arguments for running one job in place p. None is
// secret: the registration travels on standard input only.
func RunArgs(cfg *config.Config, p config.Place) []string {
	args := []string{"agent", "run", "--provider", p.Provider, "--place", p.ID,
		"--cores", strconv.Itoa(p.Cores), "--memory", strconv.Itoa(p.MemoryMB),
		"--boot-timeout", strconv.Itoa(cfg.BootTimeoutSeconds)}
	if p.Image != "" {
		args = append(args, "--image", p.Image)
	}
	if p.DiskGB > 0 {
		args = append(args, "--disk-gb", strconv.Itoa(p.DiskGB))
	}
	if p.Provider == config.ProviderTart {
		args = append(args, "--guest-check", strconv.Itoa(cfg.GuestCheckSeconds))
	}
	if p.Switch != "" {
		args = append(args, "--switch", p.Switch)
	}
	if p.ToolCache != "" {
		args = append(args, "--tool-cache", p.ToolCache)
	}
	if cfg.AllowForkPullRequests {
		args = append(args, "--allow-forks")
	}
	return args
}

// RunPlace is the place runner's main.
func RunPlace(cfg *config.Config, placeID string) int {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("place", placeID)
	p, ok := cfg.Place(placeID)
	if !ok {
		log.Error("unknown place")
		return agent.ExitFailed
	}
	// fd 3 is the place's lock. Keep it open until this process ends, and out
	// of every process started from here.
	lock := os.NewFile(3, "busy.lock")
	if lock == nil {
		log.Error("no lock handed over")
		return agent.ExitFailed
	}
	proc.CloseOnExec(3)
	defer lock.Close()

	jit, err := bufio.NewReader(os.Stdin).ReadString('\n')
	jit = strings.TrimSpace(jit)
	if jit == "" {
		log.Error("no runner registration on stdin", "err", err)
		return agent.ExitFailed
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	argv := AgentCommand(cfg.Machine(p.Machine), RunArgs(cfg, p)...)
	cmd := exec.Command(argv[0], argv[1:]...)
	r := &placeRunner{cfg: cfg, p: p, log: log}
	return r.watch(ctx, cmd, jit)
}

type placeRunner struct {
	cfg *config.Config
	p   config.Place
	log *slog.Logger
}

// watch runs the agent, feeding it the registration and holding its input
// open, and watches its output for the job starting. A runner that gets no
// job within the start timeout, or runs past the job limit, is stopped by
// closing the agent's input; one that does not stop then is killed.
func (r *placeRunner) watch(ctx context.Context, cmd *exec.Cmd, jit string) int {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		r.log.Error("agent did not start", "err", err)
		return agent.ExitFailed
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		r.log.Error("agent did not start", "err", err)
		return agent.ExitFailed
	}
	r.log.Info("agent started", "machine", r.p.Machine, "provider", r.p.Provider)
	// Wait closes the pipe once the agent has ended.
	go io.WriteString(stdin, jit+"\n")
	started := make(chan struct{})
	go func() {
		once := sync.Once{}
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(os.Stdout, line)
			if strings.Contains(line, "Running job:") {
				once.Do(func() { r.markStarted(); close(started) })
			}
		}
		io.Copy(io.Discard, pr)
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); pw.Close() }()

	startTimer := time.NewTimer(r.cfg.Seconds(r.cfg.StartTimeoutSeconds))
	jobTimer := time.NewTimer(r.cfg.Seconds(r.cfg.MaxJobSeconds))
	defer startTimer.Stop()
	defer jobTimer.Stop()
	var kill <-chan time.Time
	end := func(why string) {
		if kill != nil {
			return
		}
		r.log.Warn(why)
		stdin.Close()
		kill = time.After(2 * time.Minute)
	}
	gotJob := false
	for {
		select {
		case err := <-done:
			r.log.Info("agent ended", "exit", agent.ExitCode(err), "job", gotJob)
			if !gotJob {
				return agent.ExitUnstarted
			}
			return agent.ExitDone
		case <-started:
			gotJob = true
			started = nil
			startTimer.Stop()
		case <-startTimer.C:
			if !gotJob {
				end("no job reached the runner in time; stopping it")
			}
		case <-jobTimer.C:
			end("the job ran past the limit; stopping it")
		case <-ctx.Done():
			ctx = context.Background()
			end("asked to stop; stopping the runner")
		case <-kill:
			r.log.Warn("the agent did not stop; killing it")
			cmd.Process.Kill()
			kill = nil
		}
	}
}

func (r *placeRunner) markStarted() {
	path := filepath.Join(r.cfg.PlaceDir(r.p.ID), "place.json")
	var run PlaceRun
	if config.ReadJSON(path, &run) == nil {
		run.Started = true
		config.WriteJSON(path, run)
	}
	r.log.Info("job reached the runner")
}
