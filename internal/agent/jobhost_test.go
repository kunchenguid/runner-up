package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (the design: "the Windows agent
// as a service with Hyper-V VM and fresh-account providers", whose check is
// "the job host's existing tests ported"): each Windows job of an account
// place runs as a brand-new standard account that is deleted after it,
// whatever happened - passed, failed, stopped, or cut off by a restart; the
// service never deletes an account it did not record; a place whose cleanup
// failed takes no job until a later sweep succeeds; the runner's one-time
// registration reaches the job only on its input, never in a file or a
// program's arguments; and the job ends when the agent's end of the pipe
// closes.

// fakeHost stands in for Windows: accounts by name, and a log of every call.
type fakeHost struct {
	mu       sync.Mutex
	accounts map[string]string // name -> SID
	calls    []string
	files    map[string]map[string][]byte
	input    []byte
	share    config.Share
	fail     map[string]int // step -> failures left
	jobs     []*fakeJob
	nextSID  int
	output   string
	exit     int
	block    bool
	// lostSID makes CreateAccount make the account and then fail to read
	// its SID.
	lostSID bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{accounts: map[string]string{}, files: map[string]map[string][]byte{}, fail: map[string]int{},
		output: "√ Connected to GitHub\nCurrent runner version: '2.338.1'\nRunning job: build\n"}
}

func (h *fakeHost) call(step string, a Account) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, step+" "+a.Name)
	if h.fail[step] > 0 {
		h.fail[step]--
		return fmt.Errorf("%s failed", step)
	}
	return nil
}

func (h *fakeHost) LookupAccount(name string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.accounts[name], nil
}

func (h *fakeHost) CreateAccount(name string, password []uint16) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "create "+name)
	if _, ok := h.accounts[name]; ok {
		return "", errAccountExists
	}
	h.nextSID++
	sid := fmt.Sprintf("S-1-5-21-1-2-3-%d", 1000+h.nextSID)
	h.accounts[name] = sid
	if h.lostSID {
		return "", errors.New("made, but not found")
	}
	return sid, nil
}

func (h *fakeHost) Prepare(a Account, files map[string][]byte) error {
	h.mu.Lock()
	h.files[a.Name] = files
	h.mu.Unlock()
	return h.call("prepare", a)
}

func (h *fakeHost) Start(a Account, password []uint16, input []byte, share config.Share) (Running, error) {
	if err := h.call("start", a); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.input = append([]byte(nil), input...)
	h.share = share
	j := &fakeJob{out: strings.NewReader(h.output), exit: h.exit, stopped: make(chan struct{}), block: h.block, host: h, name: a.Name}
	h.jobs = append(h.jobs, j)
	return j, nil
}

func (h *fakeHost) DisableAccount(a Account) error    { return h.call("disable-account", a) }
func (h *fakeHost) EndProcesses(a Account) error      { return h.call("end-processes", a) }
func (h *fakeHost) RemoveTasks(a Account) error       { return h.call("remove-tasks", a) }
func (h *fakeHost) RemoveSharedFiles(a Account) error { return h.call("remove-shared-files", a) }
func (h *fakeHost) DeleteProfile(a Account) error     { return h.call("delete-profile", a) }
func (h *fakeHost) RemoveFolder(a Account) error      { return h.call("remove-folder", a) }
func (h *fakeHost) DeleteAccount(a Account) error {
	if err := h.call("delete-account", a); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.accounts[a.Name] == a.SID {
		delete(h.accounts, a.Name)
	}
	return nil
}
func (h *fakeHost) Verify(a Account) error {
	if err := h.call("verify", a); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.accounts[a.Name] == a.SID {
		return errors.New("the account is still there")
	}
	return nil
}

func (h *fakeHost) steps() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func (h *fakeHost) started() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.jobs)
}

type fakeJob struct {
	out     io.Reader
	exit    int
	block   bool
	stopped chan struct{}
	once    sync.Once
	host    *fakeHost
	name    string
}

func (j *fakeJob) Output() io.Reader { return j.out }
func (j *fakeJob) Wait() (int, error) {
	if j.block {
		<-j.stopped
		return 1, nil
	}
	return j.exit, nil
}
func (j *fakeJob) Stop() {
	j.once.Do(func() {
		j.host.mu.Lock()
		j.host.calls = append(j.host.calls, "stop "+j.name)
		j.host.mu.Unlock()
		close(j.stopped)
	})
}
func (j *fakeJob) Close() {
	j.host.mu.Lock()
	j.host.calls = append(j.host.calls, "close "+j.name)
	j.host.mu.Unlock()
}

// A registration as GitHub's runner scale sets hand one out: base64 JSON.
const registration = "eyJydW5uZXIiOiJ7XCJBZ2VudElkXCI6MTIzfSIsIi5jcmVkZW50aWFscyI6InsuLi59In0="

func runRequest(place string) JobRequest {
	return JobRequest{Version: jobRequestVersion, Op: "run", Place: place, Cores: 2, MemoryMB: 4096, Registration: registration}
}

// serve sends req as the agent does, holding its end open until the service
// has answered, and returns what the service wrote.
func askJobHost(t *testing.T, s *Server, req JobRequest) (string, error) {
	t.Helper()
	b, err := req.encode()
	if err != nil {
		t.Fatal(err)
	}
	in, w := io.Pipe()
	go w.Write(b)
	var out bytes.Buffer
	err = s.Serve(context.Background(), in, &out)
	w.Close()
	return out.String(), err
}

func testFiles(req JobRequest) (map[string][]byte, error) {
	return accountJobFiles(`C:\runner-up\cache\actions-runner-win-x64-2.338.1.zip`, runnerWindowsSHA256["x64"],
		`C:\Program Files\runner-up\runner-up.exe`, req.AllowForks), nil
}

func newTestServer(t *testing.T, h *fakeHost) *Server {
	t.Helper()
	return NewServer(Options{Records: filepath.Join(t.TempDir(), "job-accounts.json"), Files: testFiles, CleanWait: time.Second}, h,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

var cleanupOrder = []string{"disable-account", "remove-tasks", "end-processes", "remove-shared-files", "delete-profile", "remove-folder", "delete-account", "verify"}

func stepsOf(calls []string, name string) []string {
	var out []string
	for _, c := range calls {
		if step, n, _ := strings.Cut(c, " "); n == name {
			out = append(out, step)
		}
	}
	return out
}

func waitStarted(t *testing.T, h *fakeHost) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.started() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the job never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEachJobRunsAsAFreshAccountDeletedAfterwards(t *testing.T) {
	h := newFakeHost()
	s := newTestServer(t, h)
	var out string
	for i := 0; i < 2; i++ {
		o, err := askJobHost(t, s, runRequest("win-pc-1"))
		if err != nil {
			t.Fatalf("job %d: %v\n%s", i, err, o)
		}
		out += o
	}
	var names []string
	for _, c := range h.steps() {
		if strings.HasPrefix(c, "create ") {
			names = append(names, strings.TrimPrefix(c, "create "))
		}
	}
	if len(names) != 2 || names[0] == names[1] {
		t.Fatalf("two jobs made accounts %q, want two different ones", names)
	}
	for _, n := range names {
		if !regexp.MustCompile(`^rujob-[0-9a-f]{8}$`).MatchString(n) || len(n) > 20 {
			t.Errorf("account %q is not rujob-<8 hex> within Windows' 20 characters", n)
		}
		// The job object is ended even after a clean exit, so nothing the
		// job started outlives it.
		want := append([]string{"create", "prepare", "start", "stop", "close"}, cleanupOrder...)
		if got := stepsOf(h.steps(), n); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s went through %q, want %q", n, got, want)
		}
	}
	if len(h.accounts) != 0 {
		t.Errorf("accounts left after the jobs: %v", h.accounts)
	}
	if as, _ := s.load(); len(as) != 0 {
		t.Errorf("records left after the jobs: %v", as)
	}
	if !strings.Contains(out, "Running job: build") || !strings.Contains(out, "start script ended with 0") {
		t.Errorf("the job's output and end did not reach the agent:\n%s", out)
	}
	if h.share != (config.Share{Cores: 2, MemoryMB: 4096}) {
		t.Errorf("the job was contained to %+v, want the request's 2 cores and 4096 MB", h.share)
	}
}

func TestRegistrationReachesTheJobOnlyOnItsInput(t *testing.T) {
	h := newFakeHost()
	s := newTestServer(t, h)
	if _, err := askJobHost(t, s, runRequest("win-pc-2")); err != nil {
		t.Fatal(err)
	}
	if string(h.input) != registration+"\n" {
		t.Fatalf("the start script's input is %q, want the registration alone", h.input)
	}
	for name, files := range h.files {
		for f, b := range files {
			if bytes.Contains(b, []byte(registration)) {
				t.Errorf("%s's %s holds the registration", name, f)
			}
		}
		if len(files[startFile]) == 0 || len(files[guardFile]) == 0 {
			t.Errorf("%s's folder lacks the start script or the guard: %v", name, files)
		}
	}
	line := jobCommandLine(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, `C:\runner-up\jobs\rujob-a1b2c3d4`)
	if want := `"C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "C:\runner-up\jobs\rujob-a1b2c3d4\start.ps1"`; line != want {
		t.Errorf("the start script's command line is %s, want %s", line, want)
	}
}

func TestRequestsThatCouldReachAScriptAreRefused(t *testing.T) {
	h := newFakeHost()
	s := newTestServer(t, h)
	for _, tc := range []struct {
		why string
		req JobRequest
	}{
		{"a registration that is not base64", JobRequest{Version: jobRequestVersion, Op: "run", Place: "win-pc-1", Cores: 2, MemoryMB: 4096, Registration: "abc\"; Remove-Item C:\\ -Recurse"}},
		{"a place that is a path", JobRequest{Version: jobRequestVersion, Op: "run", Place: `..\..\Windows`, Cores: 2, MemoryMB: 4096, Registration: registration}},
		{"another version", JobRequest{Version: "runner-up/0", Op: "run", Place: "win-pc-1", Cores: 2, MemoryMB: 4096, Registration: registration}},
		{"no share", JobRequest{Version: jobRequestVersion, Op: "run", Place: "win-pc-1", Registration: registration}},
	} {
		out, err := askJobHost(t, s, tc.req)
		if err == nil || !strings.HasPrefix(out, serviceRefused) {
			t.Errorf("%s: answered %q, %v; want a refusal", tc.why, out, err)
		}
	}
	// A field the service does not know, such as a start script of the
	// sender's own, is refused too.
	var out bytes.Buffer
	in := strings.NewReader(`{"version":"runner-up/1","op":"run","place":"win-pc-1","cores":2,"memoryMB":4096,"registration":"` + registration + `","start":"calc.exe"}` + "\n")
	if err := s.Serve(context.Background(), in, &out); err == nil || !strings.HasPrefix(out.String(), serviceRefused) {
		t.Errorf("a request with a start script of its own was answered %q, %v", out.String(), err)
	}
	if h.started() != 0 {
		t.Errorf("a refused request started a job")
	}
}

func TestLeftoverAccountIsDeletedBeforeTheNextJob(t *testing.T) {
	h := newFakeHost()
	s := newTestServer(t, h)
	// A power cut during a job: its account and record stay.
	sid, _ := h.CreateAccount("rujob-0a0b0c0d", nil)
	s.record(Account{Name: "rujob-0a0b0c0d", SID: sid, Place: "win-pc-1"}, false)
	// Cut between recording a name and making the account.
	s.record(Account{Name: "rujob-0d0e0f10", Place: "win-pc-2"}, false)
	h.CreateAccount("rujob-0d0e0f10", nil)
	// An account the service never recorded, such as one made by hand.
	h.accounts["rujob-ffffffff"] = "S-1-5-21-9-9-9-1001"

	restarted := NewServer(s.opts, h, s.log)
	if err := restarted.Sweep(""); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"rujob-0a0b0c0d", "rujob-0d0e0f10"} {
		if _, ok := h.accounts[n]; ok {
			t.Errorf("the restart left %s", n)
		}
		if got := stepsOf(h.steps(), n); strings.Join(got[len(got)-len(cleanupOrder):], " ") != strings.Join(cleanupOrder, " ") {
			t.Errorf("%s was cleaned with %q, want %q", n, got, cleanupOrder)
		}
	}
	if h.accounts["rujob-ffffffff"] == "" || len(stepsOf(h.steps(), "rujob-ffffffff")) != 0 {
		t.Errorf("the sweep touched an account it never recorded: %v", stepsOf(h.steps(), "rujob-ffffffff"))
	}
	if as, _ := restarted.load(); len(as) != 0 {
		t.Errorf("records left after the sweep: %v", as)
	}
}

func TestNameTakenByAnotherAccountIsNeverDeleted(t *testing.T) {
	h := newFakeHost()
	s := newTestServer(t, h)
	orig := accountNameFunc
	accountNameFunc = func() (string, error) { return "rujob-12345678", nil }
	defer func() { accountNameFunc = orig }()
	h.accounts["rujob-12345678"] = "S-1-5-21-7-7-7-1001"
	if _, err := askJobHost(t, s, runRequest("win-pc-1")); err == nil {
		t.Fatal("a job ran under a name another account holds")
	}
	if h.accounts["rujob-12345678"] != "S-1-5-21-7-7-7-1001" {
		t.Fatal("the service deleted an account it did not make")
	}
	for _, c := range h.steps() {
		if !strings.HasPrefix(c, "create ") {
			t.Errorf("the service went on to %q after the name was taken", c)
		}
	}
	if as, _ := s.load(); len(as) != 0 {
		t.Errorf("the service kept a record of an account it did not make: %v", as)
	}
}

// An account made whose SID could not be read must not drop out of the
// record, or nothing would ever delete it.
func TestAccountMadeButNotReadIsDeletedByTheNextSweep(t *testing.T) {
	h := newFakeHost()
	h.lostSID = true
	s := newTestServer(t, h)
	if _, err := askJobHost(t, s, runRequest("win-pc-1")); err == nil {
		t.Fatal("a job ran without its account's SID")
	}
	if len(h.accounts) != 1 {
		t.Fatalf("accounts %v, want the one made", h.accounts)
	}
	h.lostSID = false
	if err := s.Sweep("win-pc-1"); err != nil {
		t.Fatal(err)
	}
	if len(h.accounts) != 0 {
		t.Fatalf("the sweep left %v", h.accounts)
	}
}

func TestPlaceWhoseCleanupFailedTakesNoJobUntilASweepSucceeds(t *testing.T) {
	h := newFakeHost()
	h.fail["delete-profile"] = 2
	s := newTestServer(t, h)
	if _, err := askJobHost(t, s, runRequest("win-pc-1")); err == nil {
		t.Fatal("a job whose cleanup failed reported success")
	}
	if d := s.Dirty(); len(d) != 1 || d[0] != "win-pc-1" {
		t.Fatalf("dirty places %v, want [win-pc-1]", d)
	}
	creates := func() int {
		n := 0
		for _, c := range h.steps() {
			if strings.HasPrefix(c, "create ") {
				n++
			}
		}
		return n
	}
	// The next request's sweep fails again: no new account is made.
	out, err := askJobHost(t, s, runRequest("win-pc-1"))
	if err == nil || creates() != 1 {
		t.Fatalf("a dirty place took a job (%d accounts made): %s", creates(), out)
	}
	if !strings.Contains(out, "refused: place win-pc-1 is not clean") {
		t.Errorf("the refusal did not say why: %s", out)
	}
	// The other place is unaffected.
	if _, err := askJobHost(t, s, runRequest("win-pc-2")); err != nil {
		t.Fatalf("place win-pc-2 refused a job: %v", err)
	}
	// Once the sweep succeeds, the place runs again.
	if _, err := askJobHost(t, s, runRequest("win-pc-1")); err != nil {
		t.Fatalf("place win-pc-1 still refused after its leftovers were deleted: %v", err)
	}
	if len(s.Dirty()) != 0 || len(h.accounts) != 0 {
		t.Errorf("dirty %v, accounts %v after a clean sweep", s.Dirty(), h.accounts)
	}
}

func TestJobIsStoppedAndDeletedWhenTheAgentGoes(t *testing.T) {
	h := newFakeHost()
	h.block = true
	s := newTestServer(t, h)
	b, _ := runRequest("win-pc-1").encode()
	in, w := io.Pipe()
	go w.Write(b)
	done := make(chan error)
	go func() { done <- s.Serve(context.Background(), in, io.Discard) }()
	waitStarted(t, h)
	// The agent's end of the pipe closes.
	w.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the job kept running after the agent went")
	}
	name := h.jobs[0].name
	got := strings.Join(stepsOf(h.steps(), name), " ")
	if want := "create prepare start stop close " + strings.Join(cleanupOrder, " "); got != want {
		t.Fatalf("%s went through %q, want %q", name, got, want)
	}
}

func TestRefusesASecondJobInABusyPlace(t *testing.T) {
	h := newFakeHost()
	h.block = true
	s := newTestServer(t, h)
	b, _ := runRequest("win-pc-1").encode()
	in, w := io.Pipe()
	go w.Write(b)
	done := make(chan error)
	go func() { done <- s.Serve(context.Background(), in, io.Discard) }()
	waitStarted(t, h)
	if out, err := askJobHost(t, s, runRequest("win-pc-1")); err == nil || !strings.Contains(out, "already runs a job") {
		t.Errorf("a second job in a busy place was not refused: %v %s", err, out)
	}
	// A clean request waits for the place's turn, then gives up.
	if out, err := askJobHost(t, s, JobRequest{Version: jobRequestVersion, Op: "clean", Place: "win-pc-1"}); err == nil || !strings.Contains(out, "still running a job") {
		t.Errorf("a clean of a busy place did not wait and refuse: %v %s", err, out)
	}
	w.Close()
	<-done
}

// The agent's side against the service itself, as over the pipe: a job runs
// to its end, and closing the agent ends the job and deletes its account.
func TestAgentAndServiceOverThePipe(t *testing.T) {
	h := newFakeHost()
	s := newTestServer(t, h)
	// The service disconnects the pipe once it has answered, as handle does.
	connect := func() net.Conn {
		agent, service := net.Pipe()
		go func() { s.Serve(context.Background(), service, service); service.Close() }()
		return agent
	}
	agent := connect()
	var out bytes.Buffer
	if err := askService(context.Background(), agent, runRequest("win-pc-1"), &out); err != nil {
		t.Fatalf("a job that ran to its end failed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Running job: build") {
		t.Errorf("the job's output did not reach the agent:\n%s", out.String())
	}

	h.block = true
	agent, service := net.Pipe()
	served := make(chan struct{})
	go func() { s.Serve(context.Background(), service, service); service.Close(); close(served) }()
	ctx, cancel := context.WithCancel(context.Background())
	asked := make(chan error)
	go func() { asked <- askService(ctx, agent, runRequest("win-pc-1"), io.Discard) }()
	for h.started() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	// The coordinator stopped the place: the agent's input closed.
	cancel()
	if err := <-asked; err != nil {
		t.Errorf("a stopped job failed the agent: %v", err)
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the service kept the job after the agent closed")
	}
	if len(h.accounts) != 0 {
		t.Errorf("accounts left after the agent stopped the job: %v", h.accounts)
	}

	// A refusal is the agent's error.
	h.fail["delete-profile"] = 1
	h.accounts["rujob-0a0b0c0d"] = "S-1-5-21-1-2-3-9999"
	s.record(Account{Name: "rujob-0a0b0c0d", SID: "S-1-5-21-1-2-3-9999", Place: "win-pc-3"}, false)
	err := askService(context.Background(), connect(), JobRequest{Version: jobRequestVersion, Op: "clean", Place: "win-pc-3"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "place win-pc-3 is not clean") {
		t.Errorf("a failed clean gave the agent %v", err)
	}
}

func TestPasswordsAreLongRandomAndMeetWindowsComplexity(t *testing.T) {
	a, _ := newPassword()
	b, _ := newPassword()
	if len(a) < 64 || utf16ToString(a) == utf16ToString(b) {
		t.Fatalf("passwords %d long, equal %v", len(a), utf16ToString(a) == utf16ToString(b))
	}
	if len(a) > 127 {
		t.Fatalf("password %d long; LogonUser takes at most 127", len(a))
	}
}

func utf16ToString(p []uint16) string {
	r := make([]rune, len(p))
	for i, c := range p {
		r[i] = rune(c)
	}
	return string(r)
}

// The start script checks the runner archive against its pin before
// unpacking it, and the fork guard is the job-started hook unless the place
// allows forks.
func TestAccountJobFiles(t *testing.T) {
	files, _ := testFiles(JobRequest{})
	start := string(files[startFile])
	for _, want := range []string{
		`$zip = 'C:\runner-up\cache\actions-runner-win-x64-2.338.1.zip'`,
		"-ne '" + strings.ToUpper(runnerWindowsSHA256["x64"]) + "'",
		"$env:ACTIONS_RUNNER_HOOK_JOB_STARTED = Join-Path $dir 'fork-guard.ps1'",
		"Remove-Variable registration",
	} {
		if !strings.Contains(start, want) {
			t.Errorf("the start script lacks %q:\n%s", want, start)
		}
	}
	if regexp.MustCompile(`@[A-Z_0-9]+@`).MatchString(start) {
		t.Errorf("the start script has a value left unfilled:\n%s", start)
	}
	if g := string(files[guardFile]); g != "& 'C:\\Program Files\\runner-up\\runner-up.exe' fork-guard\nexit $LASTEXITCODE\n" {
		t.Errorf("the fork guard is %q", g)
	}
	forks, _ := testFiles(JobRequest{AllowForks: true})
	if _, ok := forks[guardFile]; ok || strings.Contains(string(forks[startFile]), "ACTIONS_RUNNER_HOOK_JOB_STARTED") {
		t.Errorf("a place that allows forks still has the fork guard")
	}
}
