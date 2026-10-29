package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kunchenguid/runner-up/internal/version"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// runner-up's Windows service: the job host (jobhost.go) under Windows'
// service manager, as SYSTEM, listening on its pipe. runner-up setup installs
// it and names the runner account, the one the coordinator signs in as over
// SSH, which with SYSTEM and Administrators is the only one that may open the
// pipe.

const (
	serviceName = "runner-up"
	pipeName    = `\\.\pipe\runner-up`
	// Read and write data only: a client cannot make a pipe instance of
	// its own (FILE_CREATE_PIPE_INSTANCE is FILE_APPEND_DATA).
	clientRights = "0x12008b"
)

// serviceSettings is what runner-up setup writes to service.json in the
// service's own folder.
type serviceSettings struct {
	// User is the runner account.
	User string `json:"user"`
}

// stateDir holds the service's settings, the account record and the log.
// Every account may make folders in ProgramData, so the service takes its own
// back at each start: SYSTEM owns it and only SYSTEM and Administrators may
// use it.
func stateDir() string {
	d := os.Getenv("ProgramData")
	if d == "" {
		d = `C:\ProgramData`
	}
	return filepath.Join(d, "runner-up")
}

// jobsRoot holds the job folders and the runner archive, at the root of the
// system drive: Windows' paths stay short there, as GitHub advises for its
// runner.
func jobsRoot() string {
	d := os.Getenv("SystemDrive")
	if d == "" {
		d = "C:"
	}
	return d + `\runner-up`
}

const stateDirSDDL = "O:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// The job folders' access: job accounts may list the folders above their own
// job folder, as the runner needs, and read the runner archive; only SYSTEM
// and Administrators change anything. Each job folder adds its own account.
func jobDirsSDDL(group string) map[string]string {
	root := jobsRoot()
	return map[string]string{
		root:                         fmt.Sprintf("O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;%s)", group),
		filepath.Join(root, "cache"): fmt.Sprintf("O:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;%s)", group),
		filepath.Join(root, "jobs"):  fmt.Sprintf("O:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;;0x1200a9;;;%s)", group),
	}
}

// secureDir makes dir, or takes it back, with the access sddl gives. A
// folder neither SYSTEM nor Administrators own was made by someone else, who
// chose what is in it and may hold it open to change its access later: it is
// moved aside, never trusted, and the service makes its own.
func secureDir(dir, sddl string) error {
	sa, err := securityAttributes(sddl)
	if err != nil {
		return err
	}
	p, _ := windows.UTF16PtrFromString(dir)
	if err := windows.CreateDirectory(p, sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	if mine, err := ownedBySystemAt(dir); err != nil {
		return err
	} else if !mine {
		aside, _ := windows.UTF16PtrFromString(fmt.Sprintf("%s.untrusted-%d", dir, time.Now().UnixNano()))
		if err := windows.MoveFileEx(p, aside, 0); err != nil {
			return fmt.Errorf("%s was not made by the service and cannot be moved aside: %w", dir, err)
		}
		if err := windows.CreateDirectory(p, sa); err != nil {
			return err
		}
		if mine, err := ownedBySystemAt(dir); err != nil || !mine {
			return fmt.Errorf("%s is not the service's own folder", dir)
		}
	}
	h, err := open(dir, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	e, err := describe(h)
	if err != nil {
		return err
	}
	if !e.Dir() {
		return fmt.Errorf("%s is not a plain folder", dir)
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	owner, _, _ := sd.Owner()
	dacl, _, _ := sd.DACL()
	return windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil)
}

// ownedBySystemAt reports whether SYSTEM or Administrators own path itself,
// a link included. No standard account can give either one a folder.
func ownedBySystemAt(path string) (bool, error) {
	h, err := open(path, windows.READ_CONTROL)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(h)
	for _, k := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		trusted, err := windows.CreateWellKnownSid(k)
		if err != nil {
			return false, err
		}
		if ok, err := ownerIs(h, trusted); err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// RunService is `runner-up service`, as Windows' service manager starts it.
func RunService() error {
	is, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !is {
		return errors.New("the service runs only under Windows' service manager, as runner-up; install it with runner-up setup")
	}
	return svc.Run(serviceName, &handler{})
}

type handler struct{}

// prepare makes the service's folders and reads its settings.
func prepare(log *slog.Logger) (serviceSettings, string, error) {
	var st serviceSettings
	b, err := os.ReadFile(filepath.Join(stateDir(), "service.json"))
	if err != nil {
		return st, "", fmt.Errorf("no settings; run runner-up setup: %w", err)
	}
	if err := json.Unmarshal(b, &st); err != nil || st.User == "" {
		return st, "", fmt.Errorf("service.json names no runner account; run runner-up setup")
	}
	q, err := local(jobGroup)
	if err != nil {
		return st, "", err
	}
	group, _, _, err := windows.LookupSID("", q)
	if err != nil {
		return st, "", fmt.Errorf("no %s group; run runner-up setup: %w", jobGroup, err)
	}
	root := jobsRoot()
	sddl := jobDirsSDDL(group.String())
	for _, dir := range []string{root, filepath.Join(root, "cache"), filepath.Join(root, "jobs")} {
		if err := secureDir(dir, sddl[dir]); err != nil {
			return st, "", err
		}
	}
	self, err := os.Executable()
	return st, self, err
}

func (h *handler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending, WaitHint: 180000}
	stateErr := secureDir(stateDir(), stateDirSDDL)
	log, closeLog := openLog(stateErr == nil)
	defer closeLog()
	if stateErr != nil {
		log.Error("cannot make its own folder", "path", stateDir(), "err", stateErr)
		return true, 1
	}
	settings, self, err := prepare(log)
	if err != nil {
		log.Error("cannot start", "err", err)
		return true, 1
	}
	host, err := newWinHost(log)
	if err != nil {
		log.Error("cannot start", "err", err)
		return true, 1
	}
	var download sync.Mutex
	files := func(req JobRequest) (map[string][]byte, error) {
		// One download at a time: a second would replace the archive while
		// a job reads it.
		download.Lock()
		defer download.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		zip, err := runnerZip(ctx, filepath.Join(jobsRoot(), "cache"))
		if err != nil {
			return nil, err
		}
		return accountJobFiles(zip, runnerWindowsSHA256[runnerArch()], self, req.AllowForks), nil
	}
	server := NewServer(Options{Records: filepath.Join(stateDir(), "job-accounts.json"), Files: files}, host, log)
	// After a restart or a power cut, delete whatever a cut-off job left
	// before taking another.
	if err := server.Sweep(""); err != nil {
		log.Error("leftover job accounts could not all be deleted; those places take no job until they are", "err", err)
	}
	ln, err := listen(settings.User)
	if err != nil {
		log.Error("cannot listen", "err", err)
		return true, 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	var jobs sync.WaitGroup
	go ln.serve(ctx, server, log, &jobs)
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	log.Info("runner-up service running", "version", version.Version, "user", settings.User)
	for c := range requests {
		if c.Cmd == svc.Interrogate {
			status <- c.CurrentStatus
			continue
		}
		if c.Cmd == svc.Stop || c.Cmd == svc.Shutdown {
			break
		}
	}
	status <- svc.Status{State: svc.StopPending, WaitHint: 180000}
	cancel()
	ln.close()
	done := make(chan struct{})
	go func() { jobs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(170 * time.Second):
		log.Error("jobs were still cleaning up at stop; the next start sweeps them")
	}
	log.Info("runner-up service stopped")
	return false, 0
}

func openLog(ok bool) (*slog.Logger, func()) {
	if !ok {
		return slog.New(slog.NewTextHandler(os.Stderr, nil)), func() {}
	}
	path := filepath.Join(stateDir(), "service.log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return slog.New(slog.NewTextHandler(os.Stderr, nil)), func() {}
	}
	protectFile(path)
	return slog.New(slog.NewTextHandler(f, nil)), func() { f.Close() }
}

type listener struct {
	sa      *windows.SecurityAttributes
	mu      sync.Mutex
	closed  bool
	pending windows.Handle
}

// listen makes the pipe, which only SYSTEM, Administrators and the
// machine's runner account may open.
func listen(runner string) (*listener, error) {
	q, err := local(runner)
	if err != nil {
		return nil, err
	}
	sid, _, _, err := windows.LookupSID("", q)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", runner, err)
	}
	// SYSTEM owns the pipe, which the agent checks: no other account can
	// make a pipe SYSTEM owns.
	sa, err := securityAttributes(fmt.Sprintf("O:SYD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;%s;;;%s)", clientRights, sid.String()))
	if err != nil {
		return nil, err
	}
	l := &listener{sa: sa}
	// The first instance proves no one else holds the name.
	h, err := l.instance(true)
	if err != nil {
		return nil, err
	}
	l.pending = h
	return l, nil
}

func (l *listener) instance(first bool) (windows.Handle, error) {
	name, _ := windows.UTF16PtrFromString(pipeName)
	mode := uint32(windows.PIPE_ACCESS_DUPLEX)
	if first {
		mode |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	return windows.CreateNamedPipe(name, mode,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES, 64<<10, 64<<10, 0, l.sa)
}

func (l *listener) serve(ctx context.Context, server *Server, log *slog.Logger, jobs *sync.WaitGroup) {
	for {
		l.mu.Lock()
		h := l.pending
		l.mu.Unlock()
		err := windows.ConnectNamedPipe(h, nil)
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			windows.CloseHandle(h)
			return
		}
		// The next client's instance, made before this one is served.
		for wait := 100 * time.Millisecond; ; wait = min(2*wait, 10*time.Second) {
			next, nerr := l.instance(false)
			l.mu.Lock()
			closed = l.closed
			if nerr == nil {
				l.pending = next
			}
			l.mu.Unlock()
			if nerr == nil || closed {
				break
			}
			log.Error("cannot make the next pipe instance; trying again", "err", nerr)
			time.Sleep(wait)
		}
		if closed {
			// The service is stopping: neither this client nor a new
			// instance is served.
			l.mu.Lock()
			if l.pending != h {
				windows.CloseHandle(l.pending)
			}
			l.pending = 0
			l.mu.Unlock()
			windows.CloseHandle(h)
			return
		}
		if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			log.Warn("a client could not connect", "err", err)
			windows.CloseHandle(h)
			continue
		}
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			handle(ctx, h, server, log)
		}()
	}
}

// close stops taking clients: it wakes the waiting ConnectNamedPipe by
// connecting to it.
func (l *listener) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	name, _ := windows.UTF16PtrFromString(pipeName)
	if h, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0); err == nil {
		windows.CloseHandle(h)
	}
}

// handle serves one agent. The agent's end of the pipe closing is how the
// service learns it has gone: it stopped the job, or it or its connection
// went.
func handle(ctx context.Context, h windows.Handle, server *Server, log *slog.Logger) {
	pipe := os.NewFile(uintptr(h), "runner-up pipe")
	defer func() {
		windows.FlushFileBuffers(h)
		windows.DisconnectNamedPipe(h)
		pipe.Close()
	}()
	server.Serve(ctx, pipe, pipe)
}
