package agent

import (
	"bufio"
	"context"
	"crypto/rand"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/quote"
	"github.com/kunchenguid/runner-up/internal/version"

	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// The job host is runner-up's Windows service (`runner-up service`, run as
// SYSTEM), which account places need: a standard account cannot make another
// account or start a program as it. The agent, run over SSH as the machine's
// runner account, sends it one job request on its local pipe and relays the
// job's output; the service makes a fresh standard account rujob-<8 hex>
// with a random password held only in memory, gives it a folder only it may
// write, starts the job's start script as that account in a job object,
// relays its output and deletes everything the account left. When the
// agent's end of the pipe closes - the job was stopped, or the agent or its
// connection went - the service ends the job. It deletes only accounts it
// recorded.
//
// Every Windows call is behind Host, so the order is tested here; the Windows
// calls live in jobhost_windows.go. The service, not the request, writes the
// start script and the fork guard: a request carries only the place, its
// share of the machine and the runner's registration.

// Account is one job account.
type Account struct {
	Name string `json:"name"`
	// SID is empty only between recording the name and the account existing.
	SID   string `json:"sid"`
	Place string `json:"place"`
}

// Files the service writes into a job's folder. The guard keeps a .ps1 name:
// the runner runs a .ps1 start hook with pwsh, or powershell without it.
const (
	startFile = "start.ps1"
	guardFile = "fork-guard.ps1"
)

// Host is what the service needs of Windows.
type Host interface {
	// LookupAccount gives the SID of a local account, or "" when it does not
	// exist.
	LookupAccount(name string) (string, error)
	// CreateAccount makes a standard account and returns its SID. It fails
	// with errAccountExists when the name is taken.
	CreateAccount(name string, password []uint16) (string, error)
	// Prepare puts the account in Users and the job group only, hides it from
	// the sign-in screen and makes its job folder holding files.
	Prepare(a Account, files map[string][]byte) error
	// Start runs the job's start script as the account, contained to its
	// share of the machine, with input on its standard input, from a token
	// that may not shut the machine down. The command line never carries
	// input.
	Start(a Account, password []uint16, input []byte, share config.Share) (Running, error)

	// The cleanup steps, in this order: first nothing may sign in as the
	// account again, so nothing it left can start once its processes end.
	DisableAccount(a Account) error
	RemoveTasks(a Account) error
	EndProcesses(a Account) error
	RemoveSharedFiles(a Account) error
	DeleteProfile(a Account) error
	RemoveFolder(a Account) error
	DeleteAccount(a Account) error
	// Verify checks that nothing of the account is left.
	Verify(a Account) error
}

// Running is a started job.
type Running interface {
	// Output is the start script's standard output and error together; it
	// ends when every process holding them has ended.
	Output() io.Reader
	// Wait waits for the start script to end and gives its exit code.
	Wait() (int, error)
	// Stop ends every process in the job.
	Stop()
	// Close releases the job, its sign-in and its loaded profile.
	Close()
}

var errAccountExists = errors.New("the account already exists")

// JobRequest is one request on the service's pipe: one line of JSON. A run
// request holds the pipe open for the job's whole life.
type JobRequest struct {
	Version string `json:"version"`
	// Op is run, clean (delete whatever the place's earlier jobs left,
	// waiting for a job still cleaning up) or probe (answer that the service
	// is there).
	Op         string `json:"op"`
	Place      string `json:"place,omitempty"`
	Cores      int    `json:"cores,omitempty"`
	MemoryMB   int    `json:"memoryMB,omitempty"`
	AllowForks bool   `json:"allowForks,omitempty"`
	// Registration is the runner's one-time registration, for run only.
	Registration string `json:"registration,omitempty"`
}

const jobRequestVersion = "runner-up/1"

// maxJobRequest bounds a request line, so a broken or hostile sender cannot
// make the service hold an unbounded one.
const maxJobRequest = 128 << 10

func (r JobRequest) check() error {
	if r.Version != jobRequestVersion {
		return fmt.Errorf("not a %s request", jobRequestVersion)
	}
	if r.Op == "probe" {
		return nil
	}
	if r.Op != "run" && r.Op != "clean" {
		return fmt.Errorf("unknown request %q", r.Op)
	}
	if r.Place == "" || len(r.Place) > config.MaxPlaceID || strings.Trim(r.Place, quote.WinPlain) != "" || strings.ContainsAny(r.Place, `\/:`) {
		return fmt.Errorf("place %q is not a plain place id", r.Place)
	}
	if r.Op == "clean" {
		return nil
	}
	if r.Cores < 1 || r.MemoryMB < 512 {
		return fmt.Errorf("a job needs at least 1 core and 512 MB")
	}
	// A JIT registration is base64; anything else is refused before it can
	// reach a script.
	if r.Registration == "" || strings.Trim(r.Registration, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=") != "" {
		return errors.New("the runner registration is not base64")
	}
	return nil
}

// readJobRequest reads exactly one request line from r.
func readJobRequest(r *bufio.Reader) (JobRequest, error) {
	var b []byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			return JobRequest{}, fmt.Errorf("no request: %w", err)
		}
		if c == '\n' {
			break
		}
		if len(b) >= maxJobRequest {
			return JobRequest{}, fmt.Errorf("request longer than %d bytes", maxJobRequest)
		}
		b = append(b, c)
	}
	var req JobRequest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return JobRequest{}, fmt.Errorf("unreadable request: %w", err)
	}
	return req, req.check()
}

func (r JobRequest) encode() ([]byte, error) {
	b, err := json.Marshal(r)
	return append(b, '\n'), err
}

// Lines the service writes back start with servicePrefix; a refusal with
// serviceRefused.
const (
	servicePrefix  = "runner-up service: "
	serviceRefused = servicePrefix + "refused: "
	serviceReady   = servicePrefix + "ready"
)

// Options are the service's settings.
type Options struct {
	Records string
	MaxJob  time.Duration
	// Files gives a job's start script and fork guard.
	Files     func(req JobRequest) (map[string][]byte, error)
	NewSecret func() ([]uint16, error)
	// CleanWait is how long a clean request waits for the place's job to
	// finish cleaning up.
	CleanWait time.Duration
}

// Server runs jobs, one per place at a time.
type Server struct {
	opts  Options
	host  Host
	log   *slog.Logger
	mu    sync.Mutex
	held  map[string]chan struct{}
	dirty map[string]bool
}

func NewServer(opts Options, host Host, log *slog.Logger) *Server {
	if opts.NewSecret == nil {
		opts.NewSecret = newPassword
	}
	if opts.MaxJob == 0 {
		opts.MaxJob = 7 * time.Hour
	}
	if opts.CleanWait == 0 {
		opts.CleanWait = 3 * time.Minute
	}
	return &Server{opts: opts, host: host, log: log, held: map[string]chan struct{}{}, dirty: map[string]bool{}}
}

// hold takes place's turn, waiting up to wait for a job still running or
// cleaning up there. It returns the release, or nil when the place stayed
// busy.
func (s *Server) hold(place string, wait time.Duration) func() {
	s.mu.Lock()
	ch := s.held[place]
	if ch == nil {
		ch = make(chan struct{}, 1)
		s.held[place] = ch
	}
	s.mu.Unlock()
	release := func() { <-ch }
	select {
	case ch <- struct{}{}:
		return release
	default:
	}
	if wait <= 0 {
		return nil
	}
	select {
	case ch <- struct{}{}:
		return release
	case <-time.After(wait):
		return nil
	}
}

// accountName is rujob-<8 hex>: 14 characters, inside the 20 a local account
// name may have.
var accountNameFunc = accountName

func accountName() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "rujob-" + hex.EncodeToString(b), nil
}

// newPassword is 48 random bytes as base64 plus one character of each class
// Windows' complexity rule asks for, as UTF-16 so it can be wiped after use.
func newPassword() ([]uint16, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	p := make([]uint16, 0, 68)
	for i := 0; i+2 < len(b); i += 3 {
		n := uint32(b[i])<<16 | uint32(b[i+1])<<8 | uint32(b[i+2])
		p = append(p, uint16(enc[n>>18&63]), uint16(enc[n>>12&63]), uint16(enc[n>>6&63]), uint16(enc[n&63]))
	}
	wipe(b)
	return append(p, utf16.Encode([]rune("aA1!"))...), nil
}

func wipe[T byte | uint16](b []T) {
	for i := range b {
		b[i] = 0
	}
}

// Records: every account the service made and has not yet deleted, so a
// restart finds what a cut-off job left. Only SYSTEM and Administrators may
// read the file (protectFile).
func (s *Server) load() ([]Account, error) {
	b, err := os.ReadFile(s.opts.Records)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var as []Account
	if err := json.Unmarshal(b, &as); err != nil {
		return nil, fmt.Errorf("%s: %w", s.opts.Records, err)
	}
	return as, nil
}

func (s *Server) save(as []Account) error {
	if as == nil {
		as = []Account{}
	}
	b, err := json.MarshalIndent(as, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.opts.Records), 0o700); err != nil {
		return err
	}
	tmp := s.opts.Records + ".new"
	// Never write into a file someone else left.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := protectFile(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.opts.Records)
}

// record adds or updates a, or removes it when drop is set.
func (s *Server) record(a Account, drop bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	as, err := s.load()
	if err != nil {
		return err
	}
	out := as[:0]
	for _, x := range as {
		if x.Name != a.Name {
			out = append(out, x)
		}
	}
	if !drop {
		out = append(out, a)
	}
	return s.save(out)
}

// Sweep deletes every recorded account of place, or of every place when
// place is empty. A place whose sweep fails stays dirty and takes no job
// until a later sweep succeeds. Accounts the service did not record are
// never touched.
func (s *Server) Sweep(place string) error {
	s.mu.Lock()
	as, err := s.load()
	s.mu.Unlock()
	if err != nil {
		s.mu.Lock()
		if place != "" {
			s.dirty[place] = true
		}
		s.mu.Unlock()
		return err
	}
	failed := map[string]error{}
	for _, a := range as {
		if place != "" && a.Place != place {
			continue
		}
		if err := s.clean(a); err != nil && failed[a.Place] == nil {
			failed[a.Place] = err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if place == "" {
		clear(s.dirty)
	} else {
		delete(s.dirty, place)
	}
	for p := range failed {
		s.dirty[p] = true
	}
	if err := failed[place]; place != "" {
		return err
	}
	for _, err := range failed {
		return err
	}
	return nil
}

// clean runs every cleanup step in order, each even after an earlier one
// failed, then verifies, and forgets the account only when nothing is left.
func (s *Server) clean(a Account) error {
	log := s.log.With("account", a.Name)
	if a.SID == "" {
		// Recorded before the account was made: it is ours if it exists.
		sid, err := s.host.LookupAccount(a.Name)
		if err != nil {
			return err
		}
		if sid == "" {
			return s.record(a, true)
		}
		a.SID = sid
		if err := s.record(a, false); err != nil {
			return err
		}
	}
	var first error
	for _, step := range []struct {
		name string
		do   func(Account) error
	}{
		{"disable account", s.host.DisableAccount},
		{"remove scheduled tasks", s.host.RemoveTasks},
		{"end processes", s.host.EndProcesses},
		{"remove files in shared folders", s.host.RemoveSharedFiles},
		{"delete profile", s.host.DeleteProfile},
		{"remove job folder", s.host.RemoveFolder},
		{"delete account", s.host.DeleteAccount},
		{"verify", s.host.Verify},
	} {
		if err := step.do(a); err != nil {
			log.Error("cleanup step failed", "step", step.name, "err", err)
			if first == nil {
				first = fmt.Errorf("%s: %w", step.name, err)
			}
		}
	}
	if first != nil {
		return first
	}
	log.Info("job account deleted")
	return s.record(a, true)
}

// Serve answers one request read from in and writes what it has to say, and a
// job's output, to out. A run request's job ends when in ends: the agent
// stopped the job, or the agent or its connection went.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	br := bufio.NewReader(in)
	req, err := readJobRequest(br)
	if err != nil {
		return s.refuse(out, err)
	}
	switch req.Op {
	case "probe":
		fmt.Fprintf(out, "%s %s\n", serviceReady, version.Version)
		return nil
	case "clean":
		release := s.hold(req.Place, s.opts.CleanWait)
		if release == nil {
			return s.refuse(out, fmt.Errorf("place %s is still running a job", req.Place))
		}
		defer release()
		if err := s.Sweep(req.Place); err != nil {
			return s.refuse(out, fmt.Errorf("place %s is not clean: %w", req.Place, err))
		}
		fmt.Fprintf(out, "%splace %s is clean\n", servicePrefix, req.Place)
		return nil
	}
	release := s.hold(req.Place, 0)
	if release == nil {
		return s.refuse(out, fmt.Errorf("place %s already runs a job", req.Place))
	}
	defer release()
	if err := s.Sweep(req.Place); err != nil {
		return s.refuse(out, fmt.Errorf("place %s is not clean: %w", req.Place, err))
	}
	gone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, br)
		close(gone)
	}()
	code, err := s.run(ctx, req, out, gone)
	if err != nil {
		fmt.Fprintf(out, "%s%v\n", servicePrefix, err)
	} else {
		fmt.Fprintf(out, "%sstart script ended with %d\n", servicePrefix, code)
	}
	return err
}

func (s *Server) refuse(out io.Writer, err error) error {
	s.log.Warn("refused a request", "err", err)
	fmt.Fprintf(out, "%s%v\n", serviceRefused, err)
	return err
}

func (s *Server) run(ctx context.Context, req JobRequest, out io.Writer, gone <-chan struct{}) (code int, err error) {
	files, err := s.opts.Files(req)
	if err != nil {
		return 0, err
	}
	name, err := accountNameFunc()
	if err != nil {
		return 0, err
	}
	a := Account{Name: name, Place: req.Place}
	log := s.log.With("account", name, "place", req.Place)
	// Record the name before the account exists, so a power cut at any
	// later moment leaves a record the next sweep acts on.
	if err := s.record(a, false); err != nil {
		return 0, err
	}
	password, err := s.opts.NewSecret()
	if err != nil {
		s.record(a, true)
		return 0, err
	}
	defer wipe(password)
	a.SID, err = s.host.CreateAccount(name, password)
	if errors.Is(err, errAccountExists) {
		// Never delete an account this call did not make.
		s.record(a, true)
		return 0, fmt.Errorf("could not make %s: %w", name, err)
	}
	if err != nil {
		// It may exist all the same; the record stays for the next sweep.
		s.markDirty(req.Place, true)
		return 0, fmt.Errorf("could not make %s: %w", name, err)
	}
	if err := s.record(a, false); err != nil {
		s.cleanAfter(a)
		return 0, err
	}
	defer func() {
		if cerr := s.cleanAfter(a); cerr != nil && err == nil {
			err = cerr
		}
	}()
	log.Info("job account made", "sid", a.SID)

	if err := s.host.Prepare(a, files); err != nil {
		return 0, fmt.Errorf("could not prepare %s: %w", name, err)
	}
	job, err := s.host.Start(a, password, []byte(req.Registration+"\n"), config.Share{Cores: req.Cores, MemoryMB: req.MemoryMB})
	wipe(password)
	if err != nil {
		return 0, fmt.Errorf("could not start the job as %s: %w", name, err)
	}
	defer job.Close()
	fmt.Fprintf(out, "%srunning as %s\n", servicePrefix, name)

	relayed := make(chan struct{})
	go func() {
		// A write that fails means the agent has gone.
		if _, err := io.Copy(out, job.Output()); err != nil {
			job.Stop()
		}
		close(relayed)
	}()
	waited := make(chan struct{})
	go func() {
		code, err = job.Wait()
		close(waited)
	}()
	limit := time.NewTimer(s.opts.MaxJob)
	defer limit.Stop()
	select {
	case <-waited:
	case <-gone:
		log.Warn("the agent went away; stopping the job")
		job.Stop()
		<-waited
	case <-ctx.Done():
		log.Warn("the service is stopping; stopping the job")
		job.Stop()
		<-waited
	case <-limit.C:
		log.Warn("the job ran past its limit; stopping it")
		job.Stop()
		<-waited
	}
	// The start script has ended; whatever it left in the job ends too, so
	// the output reaches its end.
	job.Stop()
	select {
	case <-relayed:
	case <-time.After(30 * time.Second):
	}
	log.Info("job ended", "exit", code, "err", err)
	return code, err
}

// cleanAfter deletes the job's account once its job has ended, and marks the
// place dirty if anything is left.
func (s *Server) cleanAfter(a Account) error {
	err := s.clean(a)
	s.markDirty(a.Place, err != nil)
	return err
}

func (s *Server) markDirty(place string, dirty bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dirty {
		s.dirty[place] = true
	} else {
		delete(s.dirty, place)
	}
}

// Dirty reports the places whose last cleanup failed.
func (s *Server) Dirty() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for p := range s.dirty {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// jobCommandLine is the start script's command line: Windows PowerShell,
// which every Windows has, running the script in the job's folder. It is made
// from the folder alone, so no request field can reach it.
func jobCommandLine(powershell, folder string) string {
	return fmt.Sprintf(`"%s" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "%s"`,
		powershell, strings.TrimRight(folder, `\`)+`\`+startFile)
}

//go:embed account-job.ps1
var accountJobScript string

// accountJobFiles are a job's start script, which unpacks the runner archive
// zip only when its SHA-256 is sum, and, unless forks are allowed, the fork
// guard, which runs program's fork-guard command.
func accountJobFiles(zip, sum, program string, allowForks bool) map[string][]byte {
	hook := "$env:ACTIONS_RUNNER_HOOK_JOB_STARTED = Join-Path $dir " + psQuote(guardFile)
	files := map[string][]byte{}
	if allowForks {
		hook = "# Jobs from forks' pull requests may run: no fork guard."
	} else {
		files[guardFile] = []byte(accountGuardScript(program))
	}
	files[startFile] = []byte(strings.NewReplacer(
		"@RUNNER_ZIP@", psQuote(zip), "@RUNNER_SHA256@", psQuote(strings.ToUpper(sum)), "@HOOK@", hook,
	).Replace(accountJobScript))
	return files
}

// accountGuardScript is the fork guard as the runner runs a hook: a
// PowerShell script that runs this program's fork-guard command.
func accountGuardScript(program string) string {
	return "& " + psQuote(program) + " fork-guard\nexit $LASTEXITCODE\n"
}

// psQuote quotes s as a PowerShell single-quoted string.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// The agent's side of an account place. The account provider sends one
// request to the service and relays what comes back; on Windows conn is the
// service's pipe.

// errServiceDone is the end of a run whose start script ran to its end.
var errServiceDone = errors.New("done")

// askService sends req on conn and copies the service's answer to out until
// the service ends it. It closes conn when ctx is done, which is how the
// service learns the agent has gone and stops the job. It returns nil once
// the service said the request was done; a refusal or any other end is an
// error.
func askService(ctx context.Context, conn io.ReadWriteCloser, req JobRequest, out io.Writer) error {
	b, err := req.encode()
	if err != nil {
		conn.Close()
		return err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer func() {
		stop()
		conn.Close()
	}()
	if _, err := conn.Write(b); err != nil {
		return fmt.Errorf("could not reach runner-up's service: %w", err)
	}
	r := bufio.NewReader(conn)
	result := errors.New("runner-up's service ended without an answer")
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			io.WriteString(out, line)
			if !strings.HasSuffix(line, "\n") {
				io.WriteString(out, "\n")
			}
			if l, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), servicePrefix); ok {
				result = serviceAnswer(req.Op, l)
			}
		}
		if err != nil {
			break
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if errors.Is(result, errServiceDone) {
		return nil
	}
	return result
}

// serviceAnswer reads one of the service's own lines: errServiceDone when it
// ends the request well, nil for a line that ends nothing, or the error.
func serviceAnswer(op, line string) error {
	switch {
	case strings.HasPrefix(line, "refused: "):
		return errors.New(strings.TrimPrefix(line, "refused: "))
	case op == "probe" && strings.HasPrefix(line, "ready"):
		return errServiceDone
	case op == "clean" && strings.HasSuffix(line, " is clean"):
		return errServiceDone
	case op == "run" && strings.HasPrefix(line, "start script ended with "):
		return errServiceDone
	case op == "run" && strings.HasPrefix(line, "running as "):
		return errors.New("runner-up's service stopped answering during the job")
	}
	return errors.New(line)
}
