package coordinator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
// restart, with the place's locked busy.lock as file descriptor 3, the setup
// the coordinator runs (writeSetup) and the pool of the job it was started
// for as arguments, and the runner's one-time registration on standard
// input, never in an argument or an environment. It
// runs the place's machine's agent - over SSH, or directly on the
// coordinator's own machine - with the pool's image, hands it the
// registration, and holds the agent's input open until the runner ends. The
// agent makes a fresh place, runs one job there and removes the place
// whatever happened; when this process sends it "stop", it stops the job and
// cleans up. When ssh loses the agent, the place runner reconnects (watch).

// PlaceRun is a place's place.json for a job or borrow, written by the coordinator
// just before launch. For jobs, the place's runner updates it as the job starts.
type PlaceRun struct {
	BorrowID   string    `json:"borrowId,omitempty"`
	RunnerName string    `json:"runnerName"`
	RunnerID   int       `json:"runnerId"`
	Target     string    `json:"target"`
	Pool       string    `json:"pool"`
	Repo       string    `json:"repo"`
	RequestID  int64     `json:"requestId,omitempty"`
	JobName    string    `json:"jobName"`
	LaunchedAt time.Time `json:"launchedAt"`
	Started    bool      `json:"started"`
	// RanJob is the display name of the job that reached the runner, as the
	// runner printed it. GitHub may give a scale set's runner another of the
	// set's jobs than JobName, the one it was started for.
	RanJob string `json:"ranJob,omitempty"`
	PID    int    `json:"pid,omitempty"`
	// Reservation is frozen at launch, so restarts and setup edits cannot
	// shrink a running job's claim on the machine.
	Reservation config.Share `json:"reservation,omitempty"`
	// Unreachable: the place runner could not reach its machine's agent, or
	// lost it (reach.go).
	Unreachable bool `json:"unreachable,omitempty"`
	// StartFailed: the place's agent said the place failed before a job
	// reached the runner, unasked, or did not start (failedstart.go).
	StartFailed bool `json:"startFailed,omitempty"`
}

// ProcLauncher is the real Launcher.
type ProcLauncher struct {
	self   string
	log    *slog.Logger
	Exited chan string

	// mu guards the setup place runners are started with: the coordinator's,
	// kept at setupPath (writeSetup).
	mu        sync.Mutex
	cfg       *config.Config
	setupPath string
}

// NewProcLauncher starts place runners with self, this program, under cfg.
func NewProcLauncher(cfg *config.Config, self string, log *slog.Logger) *ProcLauncher {
	return &ProcLauncher{cfg: cfg, self: self, log: log, Exited: make(chan string, 16)}
}

// UseSetup keeps cfg for the place runners started from now on to read.
func (l *ProcLauncher) UseSetup(cfg *config.Config) error {
	path, err := writeSetup(cfg)
	if err != nil {
		return fmt.Errorf("could not keep the setup for place runners: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg, l.setupPath = cfg, path
	return nil
}

// setup is the setup place runners start with, and where they read it.
func (l *ProcLauncher) setup() (*config.Config, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.setupPath == "" {
		return nil, "", errors.New("no setup kept for place runners")
	}
	return l.cfg, l.setupPath, nil
}

func (l *ProcLauncher) Launch(p config.Place, pool string, lock *os.File, jit string) error {
	log := fmt.Sprintf("%s-%s.log", p.ID, time.Now().UTC().Format("20060102T150405Z"))
	return l.start(p, lock, log, strings.NewReader(jit+"\n"), "run-place", "--pool", pool)
}

// start runs this program as command, a place's detached supervisor: a
// place runner or a borrow supervisor. It holds place p's locked busy.lock
// as file descriptor 3 and reads the setup the coordinator runs, and its
// output goes to log in the logs folder.
func (l *ProcLauncher) start(p config.Place, busy *os.File, log string, stdin io.Reader, command string, args ...string) error {
	cfg, setupPath, err := l.setup()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(cfg.LogsDir(), log), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd := exec.Command(l.self, append([]string{command, "--config", setupPath, "--place", p.ID}, args...)...)
	cmd.Stdin = stdin
	cmd.Stdout, cmd.Stderr = out, out
	cmd.ExtraFiles = []*os.File{busy}
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := updatePlaceRun(cfg, p.ID, func(run *PlaceRun) { run.PID = cmd.Process.Pid }); err != nil {
		l.log.Warn("could not record the place's supervisor process; stopping it waits for it to end", "place", p.ID, "err", err)
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
	l.mu.Lock()
	cfg := l.cfg
	l.mu.Unlock()
	var run PlaceRun
	if err := config.ReadJSON(filepath.Join(cfg.PlaceDir(placeID), "place.json"), &run); err != nil {
		return err
	}
	held, err := lock.HeldBy(filepath.Join(cfg.PlaceDir(placeID), "busy.lock"))
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

// SshArgs is ssh to the machine, before the remote command. ssh gives up on
// a machine that does not answer within ten seconds, rather than wait the
// system's own connect timeout, unless the machine's own options set
// ConnectTimeout; ssh takes the first value it is given for an option.
func SshArgs(m config.Machine) []string {
	ssh := []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4"}
	if !slices.ContainsFunc(m.SSH, func(w string) bool { return strings.Contains(strings.ToLower(w), "connecttimeout") }) {
		ssh = append(ssh, "-o", "ConnectTimeout=10")
	}
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

// RunArgs are the agent's arguments for running one job of pool in place p,
// on the pool's image there. None is secret: the registration travels on
// standard input only.
func RunArgs(cfg *config.Config, p config.Place, pool string) []string {
	args := []string{"agent", "run", "--provider", p.Provider, "--place", p.ID,
		"--cores", strconv.Itoa(p.Cores), "--memory", strconv.Itoa(p.MemoryMB),
		"--boot-timeout", strconv.Itoa(cfg.BootTimeoutSeconds)}
	if image := p.ImageFor(pool); image != "" {
		args = append(args, "--image", image)
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
	// A Linux guest is the agent's default, which agents older than the
	// flag run too.
	if p.Guest == config.GuestWindows {
		args = append(args, "--guest", p.Guest)
	}
	if p.ToolCache != "" {
		args = append(args, "--tool-cache", p.ToolCache)
	}
	// Given only when set, so agents older than the flags run the places
	// that set neither.
	if p.Arch != "" {
		args = append(args, "--arch", p.Arch)
	}
	for _, c := range p.ContainerArgs {
		args = append(args, "--container-arg", c)
	}
	if cfg.AllowForkPullRequests {
		args = append(args, "--allow-forks")
	}
	// The agent's own wait is the default, which agents older than the
	// flag never needed.
	if cfg.ReconnectSeconds != config.DefaultReconnectSeconds {
		args = append(args, "--reconnect-wait", strconv.Itoa(cfg.ReconnectSeconds))
	}
	return args
}

// reservedPlace is place p sized for a job of pool as the coordinator
// reserved it: the size it wrote into place.json before launch, or the pool's
// size when it wrote none. A concurrent setup edit must not change the
// agent's limits after the reservation was counted.
func reservedPlace(cfg *config.Config, p config.Place, pool string) config.Place {
	p = cfg.SizePlace(p, pool)
	var run PlaceRun
	if config.ReadJSON(filepath.Join(cfg.PlaceDir(p.ID), "place.json"), &run) == nil && run.Reservation.Cores > 0 && run.Reservation.MemoryMB > 0 {
		p.Cores, p.MemoryMB = run.Reservation.Cores, run.Reservation.MemoryMB
	}
	return p
}

// RunPlace is the place runner's main, for a job of pool.
func RunPlace(cfg *config.Config, placeID, pool string) int {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("place", placeID)
	p, ok := cfg.Place(placeID)
	if !ok {
		log.Error("unknown place")
		return agent.ExitFailed
	}
	// The pool picks the job's image, so a pool the place does not serve
	// would run the job on an image chosen for none of its pools.
	if !slices.Contains(p.Pools, pool) {
		log.Error("the place does not serve the pool", "pool", pool)
		return agent.ExitFailed
	}
	p = reservedPlace(cfg, p, pool)
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
	argv := AgentCommand(cfg.Machine(p.Machine), RunArgs(cfg, p, pool)...)
	cmd := exec.Command(argv[0], argv[1:]...)
	r := &placeRunner{cfg: cfg, p: p, pool: pool, log: log}
	return r.watch(ctx, cmd, jit)
}

type placeRunner struct {
	cfg  *config.Config
	p    config.Place
	pool string
	log  *slog.Logger
}

// watch runs the agent, feeding it the registration and holding its input
// open, and watches its output for the job starting. A runner that runs past
// the job limit, or that the coordinator asks to stop, is stopped by sending
// the agent "stop" and closing its input; one that does not stop then is
// killed. A runner no job reached in time is the coordinator's to stop: it
// removes the runner's registration first (Service.giveUp), since GitHub can
// give the runner a job up to that moment. An agent that says the place
// failed before a job reached the runner, unasked, failed to start the
// runner, for the coordinator to rest the place (failedstart.go).
//
// When ssh loses a machine's agent that had answered, the place keeps
// running there for reconnectSeconds (the agent's reconnect.go), and so the
// place runner keeps the place and reconnects with `agent attach` until then.
// Only once it gives up is the machine out of reach.
func (r *placeRunner) watch(ctx context.Context, cmd *exec.Cmd, jit string) int {
	pr, pw := io.Pipe()
	// stopping closes once the runner is to stop: each agent command still
	// running is sent "stop".
	stopping := make(chan struct{})
	current := cmd
	start := func(cmd *exec.Cmd, input string) (<-chan error, error) {
		current = cmd
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		cmd.Stdout, cmd.Stderr = pw, pw
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		done := make(chan error, 1)
		ended := make(chan struct{})
		// Wait closes the agent's input once it has ended.
		go func() {
			io.WriteString(stdin, input)
			select {
			case <-stopping:
				io.WriteString(stdin, agent.StopLine)
				stdin.Close()
			case <-ended:
			}
		}()
		go func() { err := cmd.Wait(); close(ended); done <- err }()
		return done, nil
	}
	done, err := start(cmd, jit+"\n")
	if err != nil {
		r.log.Error("agent did not start", "err", err)
		r.markStartFailed()
		return agent.ExitFailed
	}
	attrs := []any{"machine", r.p.Machine, "provider", r.p.Provider, "pool", r.pool}
	if image := r.p.ImageFor(r.pool); image != "" {
		attrs = append(attrs, "image", image)
	}
	r.log.Info("agent started", attrs...)
	started := make(chan struct{})
	scanned := make(chan struct{})
	// heard is set once the agent itself has spoken, so ssh losing it later
	// lost a running place.
	var heard atomic.Bool
	go func() {
		defer close(scanned)
		once := sync.Once{}
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "runner-up agent: ") {
				heard.Store(true)
			}
			fmt.Fprintln(os.Stdout, line)
			if text, ok := strings.CutPrefix(line, agent.UsagePrefix); ok {
				if usage, valid := agent.ParseUsage(text); valid {
					if err := config.WriteJSON(filepath.Join(r.cfg.PlaceDir(r.p.ID), "usage.json"), usage); err != nil {
						r.log.Warn("could not keep resource readings", "err", err)
					}
				}
			}
			if _, job, ok := strings.Cut(line, "Running job:"); ok {
				once.Do(func() { r.markStarted(strings.TrimSpace(job)); close(started) })
			}
		}
		io.Copy(io.Discard, pr)
	}()

	jobTimer := time.NewTimer(r.cfg.Seconds(r.cfg.MaxJobSeconds))
	defer jobTimer.Stop()
	var kill, retry <-chan time.Time
	var reconnectBy time.Time
	stopped := false
	end := func(why string) {
		if stopped {
			return
		}
		r.log.Warn(why)
		stopped = true
		close(stopping)
		if done != nil {
			kill = time.After(2 * time.Minute)
		}
	}
	gotJob := false
	finish := func(code int, unreachable bool) int {
		pw.Close()
		// Retain the last reading before releasing busy.lock. The agent
		// can print its completion and exit before this select sees started.
		<-scanned
		select {
		case <-started:
			gotJob = true
		default:
		}
		r.log.Info("agent ended", "exit", code, "job", gotJob)
		switch {
		case unreachable:
			r.markUnreachable()
		case !gotJob && code == agent.ExitFailed && !stopped:
			r.markStartFailed()
		}
		if !gotJob {
			return agent.ExitUnstarted
		}
		return agent.ExitDone
	}
	remote := !r.cfg.Machine(r.p.Machine).Local()
	for {
		select {
		case err := <-done:
			done, kill = nil, nil
			code := agent.ExitCode(err)
			switch {
			case code == agent.ExitGone:
				r.log.Warn("the place's agent had already ended")
				return finish(code, false)
			case code != sshFailed || !remote:
				return finish(code, false)
			case stopped || !heard.Load() && reconnectBy.IsZero():
				// Lost while stopping, or never reached: the agent
				// stops the place itself.
				return finish(code, true)
			}
			if reconnectBy.IsZero() {
				reconnectBy = time.Now().Add(r.cfg.Seconds(r.cfg.ReconnectSeconds))
				r.log.Warn("lost the machine's agent; the place keeps running there while this reconnects",
					"machine", r.p.Machine, "until", reconnectBy.Format(time.RFC3339))
			}
			if time.Now().After(reconnectBy) {
				r.log.Warn("could not reconnect to the machine's agent in time")
				return finish(code, true)
			}
			retry = time.After(reconnectEvery)
		case <-retry:
			retry = nil
			argv := AgentCommand(r.cfg.Machine(r.p.Machine), AttachArgs(r.p)...)
			if done, err = start(exec.Command(argv[0], argv[1:]...), ""); err != nil {
				r.log.Warn("could not start reconnecting", "err", err)
				retry = time.After(reconnectEvery)
				done = nil
			}
		case <-started:
			gotJob = true
			started = nil
		case <-jobTimer.C:
			end("the job ran past the limit; stopping it")
			if done == nil {
				return finish(sshFailed, true)
			}
		case <-ctx.Done():
			ctx = context.Background()
			end("asked to stop; stopping the runner")
			if done == nil {
				return finish(sshFailed, true)
			}
		case <-kill:
			r.log.Warn("the agent did not stop; killing it")
			current.Process.Kill()
			kill = nil
		}
	}
}

// reconnectEvery is how long the place runner waits between tries to
// reconnect to its machine's agent.
var reconnectEvery = 5 * time.Second

// AttachArgs are the agent's arguments for reconnecting to place p's run.
func AttachArgs(p config.Place) []string {
	return []string{"agent", "attach", "--place", p.ID}
}

// markStarted records that a job reached the runner, and which: the runner
// prints the job's display name.
func (r *placeRunner) markStarted(job string) {
	r.update(func(run *PlaceRun) { run.Started, run.RanJob = true, job })
	r.log.Info("job reached the runner", "job", job)
}

// markUnreachable records, before this process ends and lets go of the place,
// that ssh could not reach the machine's agent or lost it, for the
// coordinator to rest the machine (reach.go).
func (r *placeRunner) markUnreachable() {
	r.update(func(run *PlaceRun) { run.Unreachable = true })
	r.log.Warn("ssh could not reach the machine's agent, or lost it", "machine", r.p.Machine)
}

// markStartFailed records, before this process ends and lets go of the
// place, that the runner failed to start, for the coordinator to rest the
// place (failedstart.go).
func (r *placeRunner) markStartFailed() {
	r.update(func(run *PlaceRun) { run.StartFailed = true })
	r.log.Warn("the runner failed to start; the place rests")
}

func (r *placeRunner) update(change func(*PlaceRun)) {
	if err := updatePlaceRun(r.cfg, r.p.ID, change); err != nil {
		r.log.Warn("could not record the run in place.json", "err", err)
	}
}

// A place's place.json has two writers: the coordinator, which writes it
// before a launch, records the launched process and retires it once the
// place is free, and the place runner, which records its job starting or its
// machine out of reach. Each write takes the place's place.lock first, so
// one writer's read, change and write never loses the other's change.

// readPlaceRun reads place id's place.json, and says whether it could.
func readPlaceRun(cfg *config.Config, id string) (PlaceRun, bool) {
	var run PlaceRun
	return run, config.ReadJSON(filepath.Join(cfg.PlaceDir(id), "place.json"), &run) == nil
}

// withPlaceRun runs f holding place id's place.lock, with the path of its
// place.json.
func withPlaceRun(cfg *config.Config, id string, f func(path string) error) error {
	dir := cfg.PlaceDir(id)
	l, err := lock.Wait(filepath.Join(dir, "place.lock"))
	if err != nil {
		return err
	}
	defer l.Close()
	return f(filepath.Join(dir, "place.json"))
}

// writePlaceRun replaces place id's place.json with run.
func writePlaceRun(cfg *config.Config, id string, run PlaceRun) error {
	return withPlaceRun(cfg, id, func(path string) error { return config.WriteJSON(path, run) })
}

// updatePlaceRun applies change to place id's place.json.
func updatePlaceRun(cfg *config.Config, id string, change func(*PlaceRun)) error {
	return withPlaceRun(cfg, id, func(path string) error {
		var run PlaceRun
		if err := config.ReadJSON(path, &run); err != nil {
			return err
		}
		change(&run)
		return config.WriteJSON(path, run)
	})
}

// retirePlaceRun keeps place id's place.json as its last-place.json.
func retirePlaceRun(cfg *config.Config, id string) error {
	return withPlaceRun(cfg, id, func(path string) error {
		return os.Rename(path, filepath.Join(filepath.Dir(path), "last-place.json"))
	})
}
