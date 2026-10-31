//go:build !windows

package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
	"github.com/kunchenguid/runner-up/internal/lock"
)

// Intent (the design): the coordinator hears of a waiting job
// from its scale set, acquires it once a place is free, and runs it there
// through the machine's agent over SSH, handing the runner its registration on
// standard input only; every runner starts with the fork guard; "a place keeps
// running through a coordinator restart and a crashed place is cleaned up".
//
// The coordinator, its place runners and the agent are the real programs. GitHub's
// Actions service is a local stand-in speaking the scale set protocol, ssh is a
// stand-in that runs the agent's command line on this computer, and docker is
// a stand-in whose "container" is a process that prints what the runner
// prints when its job starts and ends.

// actionsStandIn answers the scale set protocol as GitHub's Actions service
// does, for one repository's scale sets, one per pool.
type actionsStandIn struct {
	*httptest.Server
	t *testing.T

	mu       sync.Mutex
	queues   map[string][]queued // messages not yet acknowledged, by scale set name
	nextMsg  int
	sets     map[string]int // scale set ids by name
	opened   int            // sessions opened
	closed   int            // sessions closed
	polls    int            // polls since the last session opened
	acquired []int64
	jits     []string // registrations handed out, in order
	removed  []int64  // runners removed
	stray    []string // requests the stand-in does not know
	// names are the runners registered, by id; busy are those GitHub gave a
	// job, which it refuses to remove; refused are the removals it refused,
	// in order.
	names   map[int64]string
	busy    map[int64]bool
	refused []int64
	// containerUp, when set, says whether the place's container runs; up
	// records its answer at each removal.
	containerUp func() bool
	up          []bool
}

type queued struct {
	id   int
	body string
}

func newActionsStandIn(t *testing.T) *actionsStandIn {
	a := &actionsStandIn{t: t, queues: map[string][]queued{}, sets: map[string]int{}, names: map[int64]string{},
		busy: map[int64]bool{}}
	a.Server = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.Close)
	return a
}

// send queues one message holding jobs, each a scale set job message, for
// the ru-linux scale set every setup here serves.
func (a *actionsStandIn) send(jobs ...any) { a.sendTo("ru-linux", jobs...) }

// sendTo queues one message holding jobs for pool's scale set.
func (a *actionsStandIn) sendTo(pool string, jobs ...any) {
	body, err := json.Marshal(jobs)
	if err != nil {
		a.t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextMsg++
	a.queues[pool] = append(a.queues[pool], queued{a.nextMsg, string(body)})
}

// setPath reads a path under one scale set's address: the set's name and
// what follows its id, or ok false for a set the stand-in did not make.
func (a *actionsStandIn) setPath(path string) (name, rest string, ok bool) {
	tail, found := strings.CutPrefix(path, "/tenant/_apis/runtime/runnerscalesets/")
	if !found {
		return "", "", false
	}
	idText, rest, _ := strings.Cut(tail, "/")
	for n, id := range a.sets {
		if strconv.Itoa(id) == idText {
			return n, rest, true
		}
	}
	return "", "", false
}

// adminToken is the shape of the token the service hands a client: a JWT,
// read for its expiry only.
func adminToken() string {
	enc := base64.RawURLEncoding.EncodeToString
	claims := fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc([]byte(claims)) + "." + enc([]byte("stand-in"))
}

func (a *actionsStandIn) serve(w http.ResponseWriter, r *http.Request) {
	sets := "/tenant/_apis/runtime/runnerscalesets"
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if v != nil {
			json.NewEncoder(w).Encode(v)
		}
	}
	path := r.URL.Path
	a.mu.Lock()
	defer a.mu.Unlock()
	setName, setRest, inSet := a.setPath(path)
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/actions/runners/registration-token"):
		reply(http.StatusCreated, map[string]string{"token": "registration-token"})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/actions/runner-registration"):
		reply(http.StatusOK, map[string]string{"url": a.URL + "/tenant/", "token": adminToken()})
	case r.Method == http.MethodGet && path == sets:
		name := r.URL.Query().Get("name")
		if _, ok := a.sets[name]; !ok {
			reply(http.StatusOK, map[string]any{"count": 0, "value": []any{}})
			return
		}
		reply(http.StatusOK, map[string]any{"count": 1, "value": []any{a.set(name)}})
	case r.Method == http.MethodPost && path == sets:
		var set scaleset.RunnerScaleSet
		json.NewDecoder(r.Body).Decode(&set)
		if _, ok := a.sets[set.Name]; !ok {
			a.sets[set.Name] = 7 + len(a.sets)
		}
		reply(http.StatusOK, a.set(set.Name))
	case r.Method == http.MethodPost && inSet && setRest == "sessions":
		a.opened++
		a.polls = 0
		reply(http.StatusOK, map[string]any{
			"sessionId": fmt.Sprintf("00000000-0000-4000-8000-%012d", a.opened), "ownerName": "runner-up",
			"messageQueueUrl": a.URL + "/queue/" + setName, "messageQueueAccessToken": "queue-token",
			"statistics": map[string]int{},
		})
	case r.Method == http.MethodDelete && inSet && strings.HasPrefix(setRest, "sessions/"):
		a.closed++
		reply(http.StatusNoContent, nil)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/queue/"):
		a.polls++
		last, _ := strconv.Atoi(r.URL.Query().Get("lastMessageId"))
		for _, m := range a.queues[strings.TrimPrefix(path, "/queue/")] {
			if m.id > last {
				reply(http.StatusOK, map[string]any{"messageId": m.id, "messageType": "RunnerScaleSetJobMessages",
					"body": m.body, "statistics": map[string]int{}})
				return
			}
		}
		// GitHub holds a poll open until a message comes; a short wait keeps
		// the stand-in's polls cheap.
		a.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
		a.mu.Lock()
		reply(http.StatusAccepted, nil)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/queue/"):
		name, idText, _ := strings.Cut(strings.TrimPrefix(path, "/queue/"), "/")
		id, _ := strconv.Atoi(idText)
		a.queues[name] = slices.DeleteFunc(a.queues[name], func(m queued) bool { return m.id <= id })
		reply(http.StatusNoContent, nil)
	case r.Method == http.MethodPost && inSet && setRest == "acquirejobs":
		var ids []int64
		json.NewDecoder(r.Body).Decode(&ids)
		a.acquired = append(a.acquired, ids...)
		reply(http.StatusOK, map[string]any{"count": len(ids), "value": ids})
	case r.Method == http.MethodPost && inSet && setRest == "generatejitconfig":
		var setting scaleset.RunnerScaleSetJitRunnerSetting
		json.NewDecoder(r.Body).Decode(&setting)
		jit := fmt.Sprintf("stand-in-registration-%d-%d", len(a.jits)+1, time.Now().UnixNano())
		a.jits = append(a.jits, jit)
		a.names[int64(len(a.jits))] = setting.Name
		reply(http.StatusOK, map[string]any{"runner": map[string]any{"id": len(a.jits), "name": setting.Name},
			"encodedJITConfig": jit})
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/tenant/_apis/distributedtask/pools/0/agents/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(path, "/tenant/_apis/distributedtask/pools/0/agents/"), 10, 64)
		if a.busy[id] {
			// GitHub's answer, as it refused a runner it had given a job.
			a.refused = append(a.refused, id)
			reply(http.StatusBadRequest, map[string]string{"typeName": "JobStillRunningException",
				"message": fmt.Sprintf("Runner %s is currently running a job and cannot be deleted.", a.names[id])})
			return
		}
		a.removed = append(a.removed, id)
		if a.containerUp != nil {
			a.up = append(a.up, a.containerUp())
		}
		reply(http.StatusNoContent, nil)
	default:
		a.stray = append(a.stray, r.Method+" "+r.URL.String())
		reply(http.StatusNotFound, nil)
	}
}

func (a *actionsStandIn) set(name string) map[string]any {
	return map[string]any{"id": a.sets[name], "name": name, "runnerGroupId": 1,
		"labels": []map[string]string{{"type": "System", "name": name}}}
}

// read returns a copy of what the stand-in has seen.
func (a *actionsStandIn) read() actionsStandIn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return actionsStandIn{opened: a.opened, closed: a.closed, polls: a.polls, acquired: slices.Clone(a.acquired),
		jits: slices.Clone(a.jits), removed: slices.Clone(a.removed), stray: slices.Clone(a.stray),
		refused: slices.Clone(a.refused), up: slices.Clone(a.up)}
}

// giveJob has GitHub give runner id a job, so it refuses to remove it.
func (a *actionsStandIn) giveJob(id int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.busy[id] = true
}

// The stand-in ssh runs the agent's command line, its last argument, here.
const fakeSSH = `#!/bin/sh
printf '%s\n' "$*" >> "$E2E_DIR/ssh.log"
for last; do :; done
exec sh -c "$last"
`

// The stand-in docker's "container" for a place is the docker run process
// itself, recorded with its agent in NAME.pid. It prints what a runner prints,
// and its job starts once go.PID exists and ends once done.PID does.
const fakeDocker = `#!/bin/bash
d=$E2E_DIR/docker
printf '%s\n' "$*" >> "$d/docker.log"
name=${!#}
case $1 in
run)
  for a in "$@"; do [ "$prev" = --name ] && name=$a; prev=$a; done
  printf '%s\n' "$ACTIONS_RUNNER_INPUT_JITCONFIG" >> "$d/registrations"
  echo "$$ $PPID" > "$d/$name.pid"
  echo "√ Connected to GitHub"
  echo "Listening for Jobs"
  until [ -e "$d/go.$$" ] || [ -e "$d/stop" ]; do sleep 0.05; done
  echo "Running job: build"
  until [ -e "$d/done.$$" ] || [ -e "$d/stop" ]; do sleep 0.05; done
  echo "Job build completed with result: Succeeded"
  rm -f "$d/$name.pid"
  ;;
rm)
  if [ -e "$d/$name.pid" ]; then
    read pid agent < "$d/$name.pid"
    kill "$pid"
    rm -f "$d/$name.pid"
    echo "$name"
    exit 0
  fi
  echo "Error response from daemon: No such container: $name" >&2
  exit 1
  ;;
esac
`

// e2e is one coordinator setup: a machine reached over (stand-in) SSH with
// one Docker place, serving octo-org/app's ru-linux pool.
type e2e struct {
	t       *testing.T
	dir     string
	config  string
	cfg     *config.Config
	actions *actionsStandIn
	env     []string
	serving *exec.Cmd
	served  int
}

func newE2E(t *testing.T) *e2e {
	if testing.Short() {
		t.Skip("runs the coordinator, place runners and agent as programs")
	}
	dir := t.TempDir()
	e := &e2e{t: t, dir: dir, actions: newActionsStandIn(t)}
	for _, sub := range []string{"bin", "docker", "state", "machine"} {
		os.MkdirAll(filepath.Join(dir, sub), 0o700)
	}
	os.WriteFile(filepath.Join(dir, "bin", "ssh"), []byte(fakeSSH), 0o755)
	os.WriteFile(filepath.Join(dir, "bin", "docker"), []byte(fakeDocker), 0o755)
	e.cfg = &config.Config{
		StateDir:    filepath.Join(dir, "state"),
		GitHub:      config.GitHub{Repos: []string{"octo-org/app"}},
		Pools:       []config.Pool{{Label: "ru-linux"}},
		TickSeconds: 1,
		Machines: []config.Machine{{Name: "box", SSH: []string{"ci@box"}, Agent: runnerUp(t), Cores: 4, MemoryMB: 8192,
			RoomCores: 1, RoomMemoryMB: 1024,
			Places: []config.Place{{ID: "box-1", Provider: config.ProviderDocker, Pools: []string{"ru-linux"}, Cores: 2, MemoryMB: 4096}}}},
	}
	e.config = filepath.Join(dir, "config.json")
	e.save()
	e.env = append(os.Environ(),
		"PATH="+filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"E2E_DIR="+dir,
		// The agent's own folder on the machine.
		"XDG_STATE_HOME="+filepath.Join(dir, "machine"))
	t.Cleanup(e.stopAll)
	return e
}

// save writes the setup file and reads it back, as the coordinator will.
func (e *e2e) save() {
	e.t.Helper()
	if err := config.SaveConfig(e.config, e.cfg); err != nil {
		e.t.Fatal(err)
	}
	cfg, err := config.LoadConfig(e.config)
	if err != nil {
		e.t.Fatal(err)
	}
	e.cfg = cfg
}

// serve starts the coordinator; its output goes to coordinator-N.log.
func (e *e2e) serve() {
	e.served++
	out, err := os.Create(filepath.Join(e.dir, fmt.Sprintf("coordinator-%d.log", e.served)))
	if err != nil {
		e.t.Fatal(err)
	}
	defer out.Close()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(slices.Clone(e.env), serveEnv+"="+e.config, actionsEnv+"="+e.actions.URL, selfEnv+"="+runnerUp(e.t))
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.serving = cmd
}

// stop stops the coordinator as systemd does, and returns its exit code.
func (e *e2e) stop() int {
	e.t.Helper()
	e.serving.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- e.serving.Wait() }()
	select {
	case err := <-done:
		e.serving = nil
		return agent.ExitCode(err)
	case <-time.After(30 * time.Second):
		e.t.Fatal("the coordinator did not stop within 30 seconds")
		return -1
	}
}

// stopAll ends everything a test left running.
func (e *e2e) stopAll() {
	if e.serving != nil {
		e.serving.Process.Kill()
		e.serving.Wait()
	}
	os.WriteFile(filepath.Join(e.dir, "docker", "stop"), nil, 0o600)
	for _, p := range e.cfg.Places() {
		for _, name := range []string{"place.json", "last-place.json"} {
			var run coordinator.PlaceRun
			if config.ReadJSON(filepath.Join(e.cfg.PlaceDir(p.ID), name), &run) == nil && run.PID > 1 {
				// A place runner leads its own process group.
				syscall.Kill(-run.PID, syscall.SIGKILL)
			}
		}
	}
	time.Sleep(200 * time.Millisecond)
}

func (e *e2e) eventually(what string, ok func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			e.t.Fatalf("after 30 seconds: %s\n%s", what, e.logs())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// logs gathers every log the run wrote, for a failure message.
func (e *e2e) logs() string {
	var b strings.Builder
	paths, _ := filepath.Glob(filepath.Join(e.dir, "coordinator-*.log"))
	more, _ := filepath.Glob(filepath.Join(e.cfg.LogsDir(), "*.log"))
	for _, p := range append(append(paths, more...), filepath.Join(e.dir, "ssh.log"), filepath.Join(e.dir, "docker", "docker.log")) {
		data, _ := os.ReadFile(p)
		fmt.Fprintf(&b, "--- %s\n%s", filepath.Base(p), data)
	}
	return b.String()
}

// container is the place's running stand-in container: its pid and its
// agent's, or zeros when none runs.
func (e *e2e) container() (pid, agentPID int) {
	data, err := os.ReadFile(filepath.Join(e.dir, "docker", "runner-up-box-1.pid"))
	if err != nil {
		return 0, 0
	}
	fmt.Sscan(string(data), &pid, &agentPID)
	return pid, agentPID
}

// registrations are what each container found in its runner's variable.
func (e *e2e) registrations() []string {
	data, _ := os.ReadFile(filepath.Join(e.dir, "docker", "registrations"))
	return strings.Fields(string(data))
}

func (e *e2e) startJob(pid int) {
	os.WriteFile(filepath.Join(e.dir, "docker", fmt.Sprintf("go.%d", pid)), nil, 0o600)
}

func (e *e2e) endJob(pid int) {
	os.WriteFile(filepath.Join(e.dir, "docker", fmt.Sprintf("done.%d", pid)), nil, 0o600)
}

func (e *e2e) placeRun() (coordinator.PlaceRun, bool) {
	var run coordinator.PlaceRun
	err := config.ReadJSON(filepath.Join(e.cfg.PlaceDir("box-1"), "place.json"), &run)
	return run, err == nil
}

func (e *e2e) busy() bool {
	held, err := lock.HeldBy(filepath.Join(e.cfg.PlaceDir("box-1"), "busy.lock"))
	return err != nil || held
}

func (e *e2e) lines(path string) []string {
	data, _ := os.ReadFile(path)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// messagesDir holds the scale set messages recorded from GitHub.
const messagesDir = "../internal/coordinator/testdata/messages"

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// verifyJob is the first job of a recorded waiting-jobs message: octo-org/app's
// "verify / linux" in the ru-linux pool, request 9101, queued a minute ago.
func verifyJob(t *testing.T) scaleset.JobMessageBase {
	var f struct {
		Messages []struct {
			Message scaleset.RunnerScaleSetMessage `json:"message"`
		} `json:"messages"`
	}
	b, err := os.ReadFile(filepath.Join(messagesDir, "waiting-jobs.json"))
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if err != nil {
		t.Fatal(err)
	}
	job := f.Messages[0].Message.JobAvailableMessages[0].JobMessageBase
	job.QueueTime = time.Now().Add(-time.Minute).UTC()
	return job
}

func jobMessage(base scaleset.JobMessageBase, kind scaleset.MessageType) scaleset.JobMessageBase {
	base.MessageType = kind
	return base
}

// noSecrets checks no registration appears in any command line or log.
func (e *e2e) noSecrets(jits []string) {
	e.t.Helper()
	logs := e.logs()
	for _, jit := range jits {
		if strings.Contains(logs, jit) {
			e.t.Errorf("a runner registration appeared in a command line or log:\n%s", logs)
		}
	}
}

func TestJobRunsThroughTheAgentAndOutlivesACoordinatorRestart(t *testing.T) {
	e := newE2E(t)
	job := verifyJob(t)
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(job, scaleset.MessageTypeJobAvailable)})
	e.serve()

	e.eventually("a container starts for the job", func() bool { pid, _ := e.container(); return pid > 0 })
	seen := e.actions.read()
	if !slices.Equal(seen.acquired, []int64{9101}) || len(seen.jits) != 1 {
		t.Fatalf("acquired %v with %d registrations, want request 9101 with one", seen.acquired, len(seen.jits))
	}
	if got := e.registrations(); !slices.Equal(got, seen.jits) {
		t.Fatalf("the runner got registration %v, want %v", got, seen.jits)
	}
	pid, agentPID := e.container()
	run, _ := e.placeRun()
	for _, p := range []int{run.PID, agentPID} {
		args, _ := exec.Command("ps", "-ww", "-o", "args=", "-p", strconv.Itoa(p)).Output()
		if len(args) == 0 || strings.Contains(string(args), seen.jits[0]) {
			t.Errorf("process %d's command line is %q", p, args)
		}
	}
	// The agent ran over SSH with every word quoted, and gave the container
	// the place's share and the fork guard as its job-started hook.
	ssh := e.lines(filepath.Join(e.dir, "ssh.log"))
	wantRun := "-T -o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=4 -o ConnectTimeout=10 ci@box '" + runnerUp(t) +
		"' 'agent' 'run' '--provider' 'docker' '--place' 'box-1' '--cores' '2' '--memory' '4096' '--boot-timeout' '300'"
	if !slices.Contains(ssh, wantRun) {
		t.Errorf("ssh ran %q, want %q among them", ssh, wantRun)
	}
	docker := e.lines(filepath.Join(e.dir, "docker", "docker.log"))
	hook := filepath.Join(e.dir, "machine", "runner-up", "agent", "places", "box-1", "fork-guard.sh")
	wantDocker := "run --rm --name runner-up-box-1 --label runner-up.place=box-1 --cpus 2 --memory 4096m" +
		" -e ACTIONS_RUNNER_INPUT_JITCONFIG -v " + runnerUp(t) + ":/opt/runner-up/runner-up:ro -v " + hook +
		":/opt/runner-up/fork-guard.sh:ro -e ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/runner-up/fork-guard.sh" +
		" --entrypoint /home/runner/run.sh " + agent.DefaultDockerImage
	if !slices.Contains(docker, wantDocker) {
		t.Errorf("docker ran %q, want %q among them", docker, wantDocker)
	}
	if b, _ := os.ReadFile(hook); string(b) != agent.ForkGuardHook("/opt/runner-up/runner-up") {
		t.Errorf("the container's job-started hook is %q", b)
	}

	e.startJob(pid)
	e.eventually("the place runner sees the job start", func() bool { run, ok := e.placeRun(); return ok && run.Started })
	out, err := exec.Command(runnerUp(t), "status", "--config", e.config).Output()
	if err != nil || !strings.Contains(string(out), `place box-1 on box (docker, 2 cores, 4096 MB) busy (job): octo-org/app "verify / linux" running`) {
		t.Errorf("status said %q (%v)", out, err)
	}

	// The coordinator stops; the job carries on in its place.
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM\n%s", code, e.logs())
	}
	if seen := e.actions.read(); seen.closed != 1 {
		t.Errorf("the coordinator closed %d sessions on stopping, want 1", seen.closed)
	}
	if !e.busy() || !alive(pid) || !alive(agentPID) {
		t.Fatalf("the place did not outlive the coordinator: busy %v, container %v, agent %v", e.busy(), alive(pid), alive(agentPID))
	}

	// The coordinator starts again: it opens a new session, leaves the busy
	// place alone and starts no second runner.
	e.serve()
	// It places jobs as soon as its session is open, before its first poll,
	// and again after every poll.
	e.eventually("the coordinator polls its new session twice", func() bool {
		seen := e.actions.read()
		return seen.opened == 2 && seen.polls >= 2
	})
	if seen := e.actions.read(); len(seen.jits) != 1 || len(seen.acquired) != 1 {
		t.Fatalf("after the restart the coordinator acquired %v and made %d registrations", seen.acquired, len(seen.jits))
	}
	cleans := 0
	for _, line := range e.lines(filepath.Join(e.dir, "ssh.log")) {
		if strings.Contains(line, "'agent' 'clean'") {
			cleans++
		}
	}
	if cleans != 1 {
		t.Errorf("the agent cleaned the place %d times, want once, at the first start while it was free", cleans)
	}
	if !alive(pid) {
		t.Fatal("the restart stopped the running job")
	}

	// The job ends and GitHub says so; the place frees and its runner, which
	// ran its job, is not removed.
	e.endJob(pid)
	runner := run.RunnerName
	e.actions.send(
		scaleset.JobStarted{RunnerID: 1, RunnerName: runner, JobMessageBase: jobMessage(job, scaleset.MessageTypeJobStarted)},
		scaleset.JobCompleted{Result: "succeeded", RunnerID: 1, RunnerName: runner, JobMessageBase: jobMessage(job, scaleset.MessageTypeJobCompleted)})
	e.eventually("the place frees", func() bool { return !e.busy() })
	e.eventually("the coordinator forgets the finished run", func() bool { _, ok := e.placeRun(); return !ok })
	if alive(pid) || alive(agentPID) {
		t.Errorf("the container or agent outlived the job: %v %v", alive(pid), alive(agentPID))
	}
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM", code)
	}
	seen = e.actions.read()
	if len(seen.removed) != 0 || len(seen.stray) != 0 {
		t.Errorf("removed runners %v, unknown requests %v", seen.removed, seen.stray)
	}
	if len(e.registrations()) != 1 {
		t.Errorf("%d containers ran, want 1", len(e.registrations()))
	}
	e.noSecrets(seen.jits)
}

func TestCrashedPlaceIsCleanedUpAndItsJobPlacedAgain(t *testing.T) {
	e := newE2E(t)
	job := verifyJob(t)
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(job, scaleset.MessageTypeJobAvailable)})
	e.serve()
	e.eventually("a container starts for the job", func() bool { pid, _ := e.container(); return pid > 0 })
	first, _ := e.container()

	// The place runner dies before the job reaches its runner, as when the
	// coordinator's machine loses it or the SSH connection drops: the agent
	// stops the container, the runner nobody used is removed, and the job
	// gets a new runner in the place.
	run, _ := e.placeRun()
	syscall.Kill(run.PID, syscall.SIGKILL)
	e.eventually("a second container starts", func() bool { return len(e.registrations()) == 2 })
	e.eventually("the second container is recorded", func() bool { pid, _ := e.container(); return pid > 0 && pid != first })
	if alive(first) {
		t.Error("the first container outlived its place runner")
	}
	e.eventually("the unused runner is removed", func() bool { return slices.Equal(e.actions.read().removed, []int64{1}) })

	// The agent itself is killed outright and cannot clean up: its container
	// is left running until the next agent in the place removes it first.
	second, agentPID := e.container()
	syscall.Kill(agentPID, syscall.SIGKILL)
	e.eventually("a third container starts", func() bool { return len(e.registrations()) == 3 })
	e.eventually("the third container is recorded", func() bool { pid, _ := e.container(); return pid > 0 && pid != second })
	if alive(second) {
		t.Error("the container a killed agent left behind is still running")
	}
	docker := e.lines(filepath.Join(e.dir, "docker", "docker.log"))
	runs := []int{}
	for i, line := range docker {
		if strings.HasPrefix(line, "run ") {
			runs = append(runs, i)
		}
	}
	if len(runs) != 3 || !slices.Contains(docker[runs[1]:runs[2]], "rm -f runner-up-box-1") {
		t.Errorf("the third agent did not remove what the second left before starting:\n%s", strings.Join(docker, "\n"))
	}
	e.eventually("the second unused runner is removed", func() bool { return slices.Equal(e.actions.read().removed, []int64{1, 2}) })

	// The third runner runs the job.
	third, _ := e.container()
	e.startJob(third)
	e.eventually("the job starts", func() bool { run, ok := e.placeRun(); return ok && run.Started })
	e.endJob(third)
	e.eventually("the place frees", func() bool { return !e.busy() })
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM", code)
	}
	seen := e.actions.read()
	if !slices.Equal(seen.acquired, []int64{9101, 9101, 9101}) || !slices.Equal(seen.removed, []int64{1, 2}) {
		t.Errorf("acquired %v and removed runners %v, want 9101 three times and runners 1 and 2", seen.acquired, seen.removed)
	}
	if !slices.Equal(e.registrations(), seen.jits) {
		t.Errorf("the containers got %v, want %v", e.registrations(), seen.jits)
	}
	e.noSecrets(seen.jits)
}

// Intent (RUU-18: a runner "was powered off by runner-up's idle timer the
// same second GitHub gave it" a job; "GitHub then failed the job with 'lost
// communication'"): GitHub can give a scale set's idle runner a job
// until the runner is removed, so a runner no job reached within the start
// timeout is removed from its scale set while it still runs, and only then
// stopped. Its job gets a new runner.
func TestIdleRunnerIsRemovedFromItsScaleSetBeforeItStops(t *testing.T) {
	e := newE2E(t)
	e.cfg.StartTimeoutSeconds = 2
	e.save()
	e.actions.containerUp = func() bool { pid, _ := e.container(); return alive(pid) }
	job := verifyJob(t)
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(job, scaleset.MessageTypeJobAvailable)})
	e.serve()
	e.eventually("a container starts for the job", func() bool { pid, _ := e.container(); return pid > 0 })
	first, _ := e.container()

	// No job reaches the runner: once the start timeout passes it is removed,
	// with its container still running, and then its container stops.
	e.eventually("the idle runner is removed", func() bool { return len(e.actions.read().removed) > 0 })
	e.eventually("the idle runner's container stops", func() bool { return !alive(first) })
	if seen := e.actions.read(); seen.removed[0] != 1 || !seen.up[0] {
		t.Fatalf("removed runners %v with the container running %v, want runner 1 removed while it ran\n%s",
			seen.removed, seen.up, e.logs())
	}

	// The job waits again and gets a new runner in the place.
	e.eventually("the job gets a second runner", func() bool { return len(e.registrations()) == 2 })
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM", code)
	}
	seen := e.actions.read()
	if slices.Contains(seen.removed[1:], 1) || len(seen.stray) != 0 {
		t.Errorf("removed runners %v, unknown requests %v; want runner 1 removed once", seen.removed, seen.stray)
	}
	if len(seen.acquired) < 2 || !slices.Equal(seen.acquired[:2], []int64{9101, 9101}) {
		t.Errorf("acquired %v, want 9101 for the first runner and again for the second", seen.acquired)
	}
	e.noSecrets(seen.jits)
}

// Intent (RUU-18): a runner GitHub gave a job just as the start timeout
// passed is one GitHub refuses to remove, saying it is running a job. That
// runner keeps running and runs the job; no second runner starts.
func TestRunnerGivenAJobAtTheStartTimeoutKeepsRunning(t *testing.T) {
	e := newE2E(t)
	e.cfg.StartTimeoutSeconds = 2
	e.save()
	e.actions.giveJob(1)
	job := verifyJob(t)
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(job, scaleset.MessageTypeJobAvailable)})
	e.serve()
	e.eventually("a container starts for the job", func() bool { pid, _ := e.container(); return pid > 0 })
	pid, _ := e.container()
	run, _ := e.placeRun()

	// The start timeout passes before the runner says its job started.
	// GitHub refuses the removal; the runner keeps running through the next
	// ticks, and is not asked about again.
	e.eventually("GitHub refuses to remove the runner", func() bool { return len(e.actions.read().refused) > 0 })
	time.Sleep(2 * time.Second)
	if seen := e.actions.read(); !alive(pid) || !slices.Equal(seen.refused, []int64{1}) || len(seen.removed) != 0 {
		t.Fatalf("container running %v, refused removals %v, removed runners %v; want it running, refused once\n%s",
			alive(pid), seen.refused, seen.removed, e.logs())
	}

	// Its job starts and ends; the place frees.
	e.startJob(pid)
	e.eventually("the place runner sees the job start", func() bool { run, ok := e.placeRun(); return ok && run.Started })
	e.endJob(pid)
	e.actions.send(
		scaleset.JobStarted{RunnerID: 1, RunnerName: run.RunnerName, JobMessageBase: jobMessage(job, scaleset.MessageTypeJobStarted)},
		scaleset.JobCompleted{Result: "succeeded", RunnerID: 1, RunnerName: run.RunnerName,
			JobMessageBase: jobMessage(job, scaleset.MessageTypeJobCompleted)})
	e.eventually("the place frees", func() bool { return !e.busy() })
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM", code)
	}
	seen := e.actions.read()
	if len(e.registrations()) != 1 || len(seen.removed) != 0 || !slices.Equal(seen.refused, []int64{1}) || len(seen.stray) != 0 {
		t.Errorf("%d containers ran, removed %v, refused %v, unknown requests %v; want one container, one refusal",
			len(e.registrations()), seen.removed, seen.refused, seen.stray)
	}
	e.noSecrets(seen.jits)
}

// Intent (RUU-16): a place serving two pools runs each pool's job on that
// pool's image - two Ubuntu releases on the same place, picked by the label
// the job asks for - and stays one place with one share of its machine: the
// second job waits for it rather than getting a place of its own.
func TestPlaceRunsEachJobOnItsPoolsImage(t *testing.T) {
	e := newE2E(t)
	e.cfg.Pools = append(e.cfg.Pools, config.Pool{Label: "ru-linux-2604"})
	box := &e.cfg.Machines[0].Places[0]
	box.Pools = []string{"ru-linux", "ru-linux-2604"}
	box.Image = "runner-2404:latest"
	box.PoolImages = map[string]string{"ru-linux-2604": "runner-2604:latest"}
	e.save()
	checked, err := exec.Command(runnerUp(t), "check-config", "--config", e.config).CombinedOutput()
	wantChecked := "ok: 1 targets, 2 pools, 1 machines, 1 places\n" +
		"box (ci@box): box-1 docker 2 cores 4096 MB; places may reserve 3 of 4 cores and 7168 of 8192 MB, all at once\n"
	if err != nil || string(checked) != wantChecked {
		t.Fatalf("check-config said %q (%v), want %q", checked, err, wantChecked)
	}

	newer := verifyJob(t)
	newer.RunnerRequestID, newer.JobDisplayName = 9102, "verify / linux 26.04"
	older := verifyJob(t)
	e.actions.sendTo("ru-linux-2604", scaleset.JobAvailable{JobMessageBase: jobMessage(newer, scaleset.MessageTypeJobAvailable)})
	e.serve()
	e.eventually("a container starts for the 26.04 job", func() bool { pid, _ := e.container(); return pid > 0 })
	first, _ := e.container()
	if run, _ := e.placeRun(); run.Pool != "ru-linux-2604" {
		t.Fatalf("the place runs a job of %q, want ru-linux-2604", run.Pool)
	}

	// The 24.04 job arrives while the only place is busy: it waits.
	e.actions.send(scaleset.JobAvailable{JobMessageBase: jobMessage(older, scaleset.MessageTypeJobAvailable)})
	status := func() string {
		out, _ := exec.Command(runnerUp(t), "status", "--config", e.config).Output()
		return string(out)
	}
	e.eventually("the 24.04 job waits for the place", func() bool {
		return strings.Contains(status(), `waiting octo-org/app ru-linux "verify / linux"`)
	})
	if n := len(e.registrations()); n != 1 {
		t.Fatalf("%d containers ran while the place was busy, want 1", n)
	}
	t.Logf("status while the 26.04 job holds the place:\n%s", status())

	e.startJob(first)
	e.endJob(first)
	run, _ := e.placeRun()
	e.actions.sendTo("ru-linux-2604",
		scaleset.JobStarted{RunnerID: 1, RunnerName: run.RunnerName, JobMessageBase: jobMessage(newer, scaleset.MessageTypeJobStarted)},
		scaleset.JobCompleted{Result: "succeeded", RunnerID: 1, RunnerName: run.RunnerName, JobMessageBase: jobMessage(newer, scaleset.MessageTypeJobCompleted)})
	e.eventually("a container starts for the 24.04 job", func() bool {
		pid, _ := e.container()
		return pid > 0 && pid != first && len(e.registrations()) == 2
	})
	second, _ := e.container()
	if run, _ := e.placeRun(); run.Pool != "ru-linux" {
		t.Fatalf("the place runs a job of %q, want ru-linux", run.Pool)
	}
	e.startJob(second)
	e.endJob(second)
	e.eventually("the place frees", func() bool { return !e.busy() })
	if code := e.stop(); code != 0 {
		t.Fatalf("the coordinator exited %d on SIGTERM", code)
	}

	// Each job's agent and container got its pool's image, in the one place.
	agentRun := func(image string) string {
		return "-T -o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=4 -o ConnectTimeout=10 ci@box '" + runnerUp(t) +
			"' 'agent' 'run' '--provider' 'docker' '--place' 'box-1' '--cores' '2' '--memory' '4096' '--boot-timeout' '300' '--image' '" + image + "'"
	}
	var runs []string
	for _, line := range e.lines(filepath.Join(e.dir, "ssh.log")) {
		if strings.Contains(line, "'agent' 'run'") {
			runs = append(runs, line)
		}
	}
	if want := []string{agentRun("runner-2604:latest"), agentRun("runner-2404:latest")}; !slices.Equal(runs, want) {
		t.Errorf("ssh ran the agent as\n%s\nwant\n%s", strings.Join(runs, "\n"), strings.Join(want, "\n"))
	}
	var images []string
	for _, line := range e.lines(filepath.Join(e.dir, "docker", "docker.log")) {
		if strings.HasPrefix(line, "run ") {
			images = append(images, line[strings.LastIndex(line, " ")+1:])
		}
	}
	if want := []string{"runner-2604:latest", "runner-2404:latest"}; !slices.Equal(images, want) {
		t.Errorf("the containers ran images %v, want %v", images, want)
	}
	logs := e.logs()
	for _, want := range []string{
		`msg="agent started" place=box-1 machine=box provider=docker pool=ru-linux-2604 image=runner-2604:latest`,
		`msg="agent started" place=box-1 machine=box provider=docker pool=ru-linux image=runner-2404:latest`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("no place log says %q\n%s", want, logs)
		}
	}
	seen := e.actions.read()
	if !slices.Equal(seen.acquired, []int64{9102, 9101}) || len(seen.stray) != 0 {
		t.Errorf("acquired %v, unknown requests %v, want 9102 then 9101", seen.acquired, seen.stray)
	}
	e.noSecrets(seen.jits)
	t.Logf("check-config:\n%sagent runs:\n%s\ncontainer images: %v", checked, strings.Join(runs, "\n"), images)
}

// Intent (RUU-16): a place runner started for a pool its place does not
// serve starts no agent, since no image there was chosen for that pool; nor
// does one started with no pool, as a coordinator from before pools had
// images would start it, in a place whose pools have their own images.
func TestPlaceRunnerRefusesAPoolItsPlaceDoesNotServe(t *testing.T) {
	e := newE2E(t)
	e.cfg.Machines[0].Places[0].PoolImages = map[string]string{"ru-linux": "runner-2604:latest"}
	e.save()
	for pool, logged := range map[string]string{"ru-large": "ru-large", "": `""`} {
		args := []string{"run-place", "--config", e.config, "--place", "box-1"}
		if pool != "" {
			args = append(args, "--pool", pool)
		}
		cmd := exec.Command(runnerUp(t), args...)
		cmd.Env = e.env
		cmd.Stdin = strings.NewReader("stand-in-registration\n")
		out, err := cmd.CombinedOutput()
		want := `msg="the place does not serve the pool" place=box-1 pool=` + logged + "\n"
		if code := agent.ExitCode(err); code != agent.ExitFailed || !strings.Contains(string(out), want) {
			t.Errorf("run-place for pool %q exited %d saying %q", pool, code, out)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, "ssh.log")); !os.IsNotExist(err) {
		t.Errorf("a refused place runner reached the machine: %v", err)
	}
}
