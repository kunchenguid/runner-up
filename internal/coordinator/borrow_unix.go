//go:build !windows

package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/plan"
	"github.com/kunchenguid/runner-up/internal/proc"
)

// The caller owns a request lock while waiting and a private socket while
// running. The coordinator starts a detached supervisor with busy.lock, just
// as it does for a job. Command arguments, environment and stdio go directly
// from caller to supervisor, never into queue files or logs. A coordinator
// restart therefore neither loses the reservation nor stops the command.
type borrowRequest struct {
	ID       string    `json:"id"`
	Pool     string    `json:"pool"`
	Label    string    `json:"label"`
	Minutes  int       `json:"minutes"`
	Queued   time.Time `json:"queued"`
	Socket   string    `json:"socket"`
	Place    string    `json:"place,omitempty"`
	Granted  time.Time `json:"granted,omitempty"`
	Position int       `json:"position"`
	Reason   string    `json:"reason"`
}

type borrowCommand struct {
	Args, Env []string
	Dir       string
}

func borrowDir(cfg *config.Config, id string) string {
	return filepath.Join(cfg.StateDir, "borrows", id)
}
func validBorrowID(id string) bool {
	return id != "" && filepath.Base(id) == id && id != "." && id != ".."
}
func borrowLimit(minutes int) time.Duration {
	return max(time.Hour, time.Duration(minutes)*time.Minute)
}

// BorrowMain runs a command with a tart place reserved for its VM.
// It runs on the coordinator's computer; commands addressing a
// remote Mac must reach it themselves, as they would without a borrow.
func BorrowMain(args []string) int {
	fs := flag.NewFlagSet("borrow", flag.ContinueOnError)
	path := fs.String("config", config.DefaultConfigPath(), "setup file")
	pool := fs.String("pool", "", "pool to borrow a tart place from")
	label := fs.String("label", "", "name shown in the queue and status")
	minutes := fs.Int("minutes", 0, "declared runtime in minutes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *pool == "" || *label == "" || *minutes <= 0 || int64(*minutes) > int64((1<<63-1)/int64(time.Minute)) || len(fs.Args()) == 0 {
		fmt.Fprintln(os.Stderr, "borrow needs --pool LABEL --label TEXT --minutes N -- COMMAND")
		return 2
	}
	cfg, err := config.LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	found := false
	for _, p := range cfg.Places() {
		if p.Provider == config.ProviderTart && slices.Contains(p.Pools, *pool) {
			found = true
		}
	}
	if !found {
		fmt.Fprintln(os.Stderr, "pool has no tart place")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	code, err := borrow(ctx, cfg, *pool, *label, *minutes, fs.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "borrow:", err)
		return 1
	}
	return code
}

func borrow(ctx context.Context, cfg *config.Config, pool, label string, minutes int, args []string) (int, error) {
	// Resolve with the caller's PATH, not the coordinator service's PATH.
	executable, err := exec.LookPath(args[0])
	if err != nil {
		return 1, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return 1, err
	}
	args = append([]string{executable}, args[1:]...)
	root := filepath.Join(cfg.StateDir, "borrows")
	if err := os.MkdirAll(root, 0700); err != nil {
		return 1, err
	}
	dir, err := os.MkdirTemp(root, "request-")
	if err != nil {
		return 1, err
	}
	// Only published directories belong to the coordinator. Before publication
	// it ignores this directory, so early setup errors can remove it safely.
	// Afterwards the live lock, not file removal, withdraws the request.
	published := false
	defer func() {
		if !published {
			os.RemoveAll(dir)
		}
	}()
	live, err := lock.Busy(filepath.Join(dir, "live.lock"))
	if err != nil {
		return 1, err
	}
	defer live.Close()
	// macOS limits Unix socket addresses to 104 bytes; the state folder can be
	// much longer. This private temporary folder contains only the socket.
	sockets, err := os.MkdirTemp("", "ru-b-")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(sockets)
	socket := filepath.Join(sockets, "s")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return 1, err
	}
	defer listener.Close()
	req := borrowRequest{ID: filepath.Base(dir), Pool: pool, Label: label, Minutes: minutes, Queued: time.Now(), Socket: socket}
	if err := config.WriteJSON(filepath.Join(dir, "request.json"), req); err != nil {
		return 1, err
	}
	published = true
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			listener.Close()
		case <-done:
		}
	}()
	listener.SetDeadline(time.Now().Add(time.Second))
	last := ""
	var conn *net.UnixConn
	for {
		conn, err = listener.AcceptUnix()
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return 130, nil
		}
		if e, ok := err.(net.Error); !ok || !e.Timeout() {
			return 1, err
		}
		var granted borrowRequest
		if config.ReadJSON(filepath.Join(dir, "granted.json"), &granted) == nil && granted.Place != "" {
			if held, err := lock.HeldBy(filepath.Join(cfg.PlaceDir(granted.Place), "busy.lock")); err == nil && !held {
				return 1, fmt.Errorf("supervisor ended before connecting")
			}
		}
		var current borrowRequest
		message := "waiting for coordinator"
		if config.ReadJSON(filepath.Join(dir, "request.json"), &current) == nil && current.Position > 0 {
			message = fmt.Sprintf("waiting at position %d: %s", current.Position, current.Reason)
		}
		if message != last {
			fmt.Fprintln(os.Stderr, "borrow:", message)
			last = message
		}
		listener.SetDeadline(time.Now().Add(time.Second))
	}
	defer conn.Close()
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	// Send exactly one byte with the three descriptors, then the command on
	// the same stream. No command or environment is persisted.
	_, _, err = conn.WriteMsgUnix([]byte{0}, syscall.UnixRights(int(os.Stdin.Fd()), int(os.Stdout.Fd()), int(os.Stderr.Fd())), nil)
	if err != nil {
		return 1, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	if err := json.NewEncoder(conn).Encode(borrowCommand{Args: args, Env: os.Environ(), Dir: cwd}); err != nil {
		return 1, err
	}
	var code int
	if err := json.NewDecoder(conn).Decode(&code); err != nil {
		if ctx.Err() != nil {
			return 130, nil
		}
		return 1, fmt.Errorf("supervisor disconnected: %w", err)
	}
	return code, nil
}

func (s *Service) refreshBorrowsLocked() {
	// Local requests are re-read rather than saved as GitHub jobs. Their live
	// locks discard abandoned requests even when the coordinator was stopped.
	for k, j := range s.waiting {
		if j.BorrowID != "" {
			delete(s.waiting, k)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(s.Cfg.StateDir, "borrows"))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := borrowDir(s.Cfg, e.Name())
		// MkdirTemp and opening live.lock precede taking its lock. Nothing is
		// queued until the caller atomically publishes request.json, so never
		// collect an incompletely initialized request directory.
		if _, err := os.Stat(filepath.Join(dir, "request.json")); err != nil {
			if _, err := os.Stat(filepath.Join(dir, "granted.json")); err != nil {
				continue
			}
		}
		held, err := lock.HeldBy(filepath.Join(dir, "live.lock"))
		if err != nil {
			continue
		}
		if !held {
			os.RemoveAll(dir)
			continue
		}
		var r borrowRequest
		if config.ReadJSON(filepath.Join(dir, "request.json"), &r) != nil || r.ID != e.Name() || r.Minutes <= 0 {
			continue
		}
		j := plan.Job{BorrowID: r.ID, Target: "borrow", Pool: r.Pool, Name: r.Label, QueueTime: r.Queued, SeenAt: r.Queued, Estimate: time.Duration(r.Minutes) * time.Minute}
		s.waiting[j.Key()] = j
	}
	// Rank against the very same job views/rules, not a separate FIFO queue.
	ranked := plan.Rank(s.waitingLocked(), s.Rules, s.now())
	for i, j := range ranked {
		if j.BorrowID == "" {
			continue
		}
		path := filepath.Join(borrowDir(s.Cfg, j.BorrowID), "request.json")
		var r borrowRequest
		if config.ReadJSON(path, &r) != nil {
			continue
		}
		r.Position = i + 1
		r.Reason = "waiting behind higher-ranked work"
		for _, p := range s.Cfg.Places() {
			if p.Provider == config.ProviderTart && slices.Contains(p.Pools, j.Pool) {
				if busy, why := s.PlaceBusy(p); busy {
					r.Reason = p.ID + ": " + why
				} else {
					r.Reason = "waiting for placement"
					break
				}
			}
		}
		config.WriteJSON(path, r)
	}
}

func (s *Service) assignBorrowLocked(p config.Place, j plan.Job, busy *os.File) bool {
	l, ok := s.launcher.(interface {
		LaunchBorrow(config.Place, *os.File, string) error
	})
	if !ok {
		return false
	}
	dir := borrowDir(s.Cfg, j.BorrowID)
	live, err := lock.HeldBy(filepath.Join(dir, "live.lock"))
	if err != nil || !live {
		delete(s.waiting, j.Key())
		return true
	}
	var request borrowRequest
	if err := config.ReadJSON(filepath.Join(dir, "request.json"), &request); err != nil {
		return false
	}
	request.Place = p.ID
	request.Granted = s.now()
	if err := config.WriteJSON(filepath.Join(dir, "request.json"), request); err != nil {
		return false
	}
	// Rename before launching: a restarted coordinator must not grant the same
	// request to a second place while the detached supervisor connects.
	if err := os.Rename(filepath.Join(dir, "request.json"), filepath.Join(dir, "granted.json")); err != nil {
		return false
	}
	run := PlaceRun{BorrowID: j.BorrowID, Pool: j.Pool, JobName: j.Name, Started: true, LaunchedAt: s.now()}
	err = config.WriteJSON(filepath.Join(s.Cfg.PlaceDir(p.ID), "place.json"), run)
	if err == nil {
		err = l.LaunchBorrow(p, busy, j.BorrowID)
	}
	if err != nil {
		os.Rename(filepath.Join(dir, "granted.json"), filepath.Join(dir, "request.json"))
		s.Log.Error("borrow launch failed", "place", p.ID, "err", err)
		s.cooling[p.ID] = s.now().Add(time.Minute)
		return false
	}
	delete(s.waiting, j.Key())
	s.Log.Info("borrow placed", "place", p.ID, "label", j.Name, "waited", s.now().Sub(j.QueueTime).Round(time.Second).String())
	return true
}

func (l *ProcLauncher) LaunchBorrow(p config.Place, busy *os.File, id string) error {
	cmd := exec.Command(l.self, "run-borrow", "--config", l.configPath, "--place", p.ID, "--borrow", id)
	out, err := os.OpenFile(filepath.Join(l.cfg.LogsDir(), "borrow-"+id+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd.Stdout, cmd.Stderr = out, out
	cmd.ExtraFiles = []*os.File{busy}
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
	go func() { cmd.Wait(); l.Exited <- p.ID }()
	return nil
}

// RunBorrow holds the place while the command and cleanup run. Only the
// coordinator launches it, with busy.lock as descriptor 3.
func RunBorrow(cfg *config.Config, placeID, id string) int {
	if !validBorrowID(id) {
		return 1
	}
	busy := os.NewFile(3, "busy.lock")
	if busy == nil {
		return 1
	}
	proc.CloseOnExec(3)
	defer busy.Close()
	p, ok := cfg.Place(placeID)
	if !ok || p.Provider != config.ProviderTart {
		return 1
	}
	var r borrowRequest
	if config.ReadJSON(filepath.Join(borrowDir(cfg, id), "granted.json"), &r) != nil {
		return 1
	}
	conn, err := net.DialTimeout("unix", r.Socket, 10*time.Second)
	if err != nil {
		return 1
	}
	defer conn.Close()
	unix := conn.(*net.UnixConn)
	code := superviseBorrow(cfg, p, r, unix)
	json.NewEncoder(conn).Encode(code)
	return code
}

func superviseBorrow(cfg *config.Config, p config.Place, r borrowRequest, conn *net.UnixConn) int {
	started := r.Granted
	if started.IsZero() {
		started = time.Now()
	}
	// Cleanup happens before busy.lock is released, including on cancellation.
	// A supervisor killed outright is covered by the next job's agent cleanup.
	defer func() {
		fmt.Fprintf(os.Stdout, "borrow completed place=%s label=%q borrowedMinutes=%.2f\n", p.ID, r.Label, time.Since(started).Minutes())
	}()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var data [1]byte
	oob := make([]byte, syscall.CmsgSpace(3*4))
	_, n, flags, _, err := conn.ReadMsgUnix(data[:], oob)
	if err != nil || flags&syscall.MSG_CTRUNC != 0 {
		return 1
	}
	messages, err := syscall.ParseSocketControlMessage(oob[:n])
	if err != nil {
		return 1
	}
	var fds []int
	for _, m := range messages {
		rights, err := syscall.ParseUnixRights(&m)
		if err != nil {
			return 1
		}
		fds = append(fds, rights...)
	}
	if len(fds) != 3 {
		for _, fd := range fds {
			syscall.Close(fd)
		}
		return 1
	}
	for _, fd := range fds {
		proc.CloseOnExec(fd)
	}
	in, out, stderr := os.NewFile(uintptr(fds[0]), "stdin"), os.NewFile(uintptr(fds[1]), "stdout"), os.NewFile(uintptr(fds[2]), "stderr")
	// The os.File wrappers, rather than raw descriptor closes, own these fds.
	// Keep them live through Wait and cleanup.
	defer in.Close()
	defer out.Close()
	defer stderr.Close()
	var command borrowCommand
	if err := json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&command); err != nil || len(command.Args) == 0 {
		return 1
	}
	conn.SetReadDeadline(time.Time{})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithDeadline(ctx, started.Add(borrowLimit(r.Minutes)))
	defer cancel()
	go func() { var b [1]byte; conn.Read(b[:]); cancel() }()
	clean := func() error {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		argv := AgentCommand(cfg.Machine(p.Machine), "agent", "clean", "--provider", config.ProviderTart, "--place", p.ID)
		b, err := exec.CommandContext(c, argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			fmt.Fprintln(stderr, "borrow cleanup:", agent.FirstLine(string(b), err))
		}
		return err
	}
	if clean() != nil {
		return 1
	}
	fmt.Fprintf(stderr, "borrow: granted %s (%d cores, %d MB), VM %s; limit %s\n", p.ID, p.Cores, p.MemoryMB, agent.CloneName(p.ID), borrowLimit(r.Minutes))
	code := runBorrowCommand(ctx, p, command, in, out, stderr)

	if clean() != nil {
		return 1
	}
	return code
}

// runBorrowCommand owns a process group so cancellation, deadlines and stray
// children cannot continue using a place after its command finishes.
func runBorrowCommand(ctx context.Context, p config.Place, command borrowCommand, in, out, stderr *os.File) int {
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "borrow: stopping command:", ctx.Err())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 124
		}
		return 130
	}
	cmd := exec.Command(command.Args[0], command.Args[1:]...)
	cmd.Dir = command.Dir
	cmd.Env = append(command.Env, "BORROWED_PLACE="+p.ID, "BORROWED_VM="+agent.CloneName(p.ID), "BORROWED_CPUS="+strconv.Itoa(p.Cores), "BORROWED_MEMORY_MB="+strconv.Itoa(p.MemoryMB))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err := cmd.Start()
	code := 1
	if err == nil {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err = <-done:
			code = agent.ExitCode(err)
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					code = 128 + int(ws.Signal())
				}
			}
		case <-ctx.Done():
			fmt.Fprintln(stderr, "borrow: stopping command:", ctx.Err())
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				code = 124
			} else {
				code = 130
			}
		}
		// A command may leave descendants behind; they must not keep using the
		// borrowed place after its leader has exited.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	} else {
		fmt.Fprintln(stderr, "borrow:", err)
	}
	return code
}
