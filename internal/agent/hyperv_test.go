package agent

// Intent (the design: Hyper-V VM places on Windows): cloud-init in the VM
// reads its seed from a disc runner-up writes; the runner's registration
// reaches the VM only over its serial port, never on the disc; the guest runs
// the runner pinned to its published sum; and the fork guard inside the VM,
// which cannot run runner-up's own Windows program, decides every event the
// way runner-up's fork-guard command does.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSeedDiscReadsAsCloudInitsNoCloudSeed(t *testing.T) {
	bsdtar, err := exec.LookPath("bsdtar")
	if err != nil {
		t.Skip("no bsdtar to read the disc with")
	}
	seed, err := hypervSeed("win-pc-1", "x64", false, time.Unix(1793230000, 0))
	if err != nil {
		t.Fatal(err)
	}
	iso, err := seedISO(seed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "seed.iso")
	os.WriteFile(path, iso, 0o600)
	// libarchive reads the Joliet names, as Linux does.
	list, err := exec.Command(bsdtar, "-tf", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range strings.Fields(string(list)) {
		if f != "." {
			got = append(got, f)
		}
	}
	if strings.Join(got, " ") != "meta-data user-data" {
		t.Errorf("the disc holds %q, want meta-data and user-data", got)
	}
	for name, want := range seed {
		got, err := exec.Command(bsdtar, "-xOf", path, name).Output()
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s reads back as %q (%v), want %q", name, got, err, want)
		}
	}
	// cloud-init finds its seed by the volume label.
	if label := strings.TrimSpace(string(iso[16*2048+40 : 16*2048+72])); label != "cidata" {
		t.Errorf("the disc is labelled %q, want cidata", label)
	}
}

func TestSeedHoldsNoSecretAndPinsTheRunner(t *testing.T) {
	seed, err := hypervSeed("win-pc-1", "x64", false, time.Unix(1793230000, 0))
	if err != nil {
		t.Fatal(err)
	}
	user := string(seed["user-data"])
	if !strings.HasPrefix(user, "#cloud-config\n") {
		t.Fatalf("user-data is not cloud-config:\n%s", user)
	}
	m := regexp.MustCompile(`(?m)^    content: (\S+)$`).FindStringSubmatch(user)
	if m == nil {
		t.Fatalf("user-data writes no guest program:\n%s", user)
	}
	guest, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`RUNNER_URL = "https://github.com/actions/runner/releases/download/v` + runnerVersion + `/actions-runner-linux-x64-` + runnerVersion + `.tar.gz"`,
		`RUNNER_SHA256 = "` + runnerSHA256["linux-x64"] + `"`,
		`ALLOW_FORKS = "false" == "true"`,
	} {
		if !bytes.Contains(guest, []byte(want)) {
			t.Errorf("the guest program lacks %s", want)
		}
	}
	if bytes.Contains(guest, []byte("@RUNNER")) || bytes.Contains(guest, []byte("@ALLOW")) {
		t.Errorf("the guest program has a value left unfilled")
	}
	if !strings.Contains(string(seed["meta-data"]), "instance-id: runner-up-win-pc-1-") {
		t.Errorf("meta-data is %q", seed["meta-data"])
	}
	forks, _ := hypervSeed("win-pc-1", "x64", true, time.Unix(1793230000, 0))
	m = regexp.MustCompile(`(?m)^    content: (\S+)$`).FindStringSubmatch(string(forks["user-data"]))
	if g, _ := base64.StdEncoding.DecodeString(m[1]); !bytes.Contains(g, []byte(`ALLOW_FORKS = "true" == "true"`)) {
		t.Errorf("a place that allows forks still runs the fork guard")
	}
}

// fakeGuest plays the VM's side of the serial port: it says it is ready until
// it reads the registration, then prints the runner's output and turns off.
func fakeGuest(t *testing.T, conn net.Conn, got chan<- string) {
	go func() {
		defer conn.Close()
		lines := make(chan string, 1)
		go func() {
			b := make([]byte, 4096)
			var acc []byte
			for {
				n, err := conn.Read(b)
				acc = append(acc, b[:n]...)
				if i := bytes.IndexByte(acc, '\n'); i >= 0 {
					lines <- string(acc[:i])
					return
				}
				if err != nil {
					close(lines)
					return
				}
			}
		}()
		for {
			io.WriteString(conn, guestReady+"\n")
			select {
			case l, ok := <-lines:
				if !ok {
					return
				}
				got <- l
				// One more ready line was already on its way.
				io.WriteString(conn, guestReady+"\n")
				io.WriteString(conn, "runner-up guest: starting the runner\n√ Connected to GitHub\nRunning job: build\n")
				io.WriteString(conn, "runner-up guest: the runner ended with 0\n")
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
}

func TestRegistrationGoesToTheVMOverItsSerialPortOnce(t *testing.T) {
	agent, vm := net.Pipe()
	got := make(chan string, 2)
	fakeGuest(t, vm, got)
	var out bytes.Buffer
	if err := talkToGuest(context.Background(), agent, registration, &out, 5*time.Second); err != nil {
		t.Fatalf("a job that ran to its end failed: %v\n%s", err, out.String())
	}
	if l := <-got; l != registration {
		t.Errorf("the VM read %q, want the registration", l)
	}
	if strings.Contains(out.String(), guestReady) || strings.Contains(out.String(), registration) {
		t.Errorf("the agent's output holds the handshake or the registration:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Running job: build\nrunner-up guest: the runner ended with 0\n") {
		t.Errorf("the runner's output did not reach the agent:\n%s", out.String())
	}
}

func TestAVMThatNeverAnswersFailsAtTheBootTimeout(t *testing.T) {
	agent, vm := net.Pipe()
	defer vm.Close()
	go io.Copy(io.Discard, vm)
	start := time.Now()
	err := talkToGuest(context.Background(), agent, registration, io.Discard, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not start runner-up's guest program within 200ms") {
		t.Errorf("a silent VM gave %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the agent waited %s for a silent VM", time.Since(start))
	}
}

func TestStoppingThePlaceLetsGoOfTheVM(t *testing.T) {
	agent, vm := net.Pipe()
	defer vm.Close()
	go func() {
		io.WriteString(vm, guestReady+"\n")
		io.Copy(io.Discard, vm)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- talkToGuest(ctx, agent, registration, io.Discard, 5*time.Second) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a stopped place gave %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent kept the VM's serial port after the place stopped")
	}
}

func python(t *testing.T) string {
	t.Helper()
	// The guest script runs on the VM's Linux, and needs its termios.
	if runtime.GOOS == "windows" {
		t.Skip("the guest script needs a Unix python")
	}
	p, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	return p
}

func TestGuestForkGuardDecidesLikeRunnerUps(t *testing.T) {
	py := python(t)
	guest := filepath.Join(t.TempDir(), "guest.py")
	os.WriteFile(guest, []byte(hypervGuest), 0o700)
	events, err := filepath.Glob("testdata/events/*")
	if err != nil || len(events) < 5 {
		t.Fatalf("events %v %v", events, err)
	}
	extra := t.TempDir()
	for name, body := range map[string]string{
		"nan.json":       `{"pull_request": NaN}`,
		"float-ids.json": `{"pull_request": {"head": {"repo": {"id": 1.0}}, "base": {"repo": {"id": 1.0}}}}`,
		"empty.json":     ``,
		"pr-null.json":   `{"pull_request": null, "workflow_run": null}`,
		"pr-string.json": `{"pull_request": "x"}`,
	} {
		os.WriteFile(filepath.Join(extra, name), []byte(body), 0o600)
		events = append(events, filepath.Join(extra, name))
	}
	events = append(events, filepath.Join(extra, "missing.json"))
	for _, ev := range events {
		var goOut, goErr bytes.Buffer
		t.Setenv("GITHUB_EVENT_PATH", ev)
		t.Setenv("GITHUB_EVENT_NAME", "pull_request")
		code := RunForkGuard(&goOut, &goErr)
		cmd := exec.Command(py, "-I", guest, "guard")
		var pyOut, pyErr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &pyOut, &pyErr
		cmd.Env = append(os.Environ(), "GITHUB_EVENT_PATH="+ev, "GITHUB_EVENT_NAME=pull_request")
		err := cmd.Run()
		pyCode := 0
		if x, ok := err.(*exec.ExitError); ok {
			pyCode = x.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if pyCode != code || pyOut.String() != goOut.String() || pyErr.String() != goErr.String() {
			t.Errorf("%s: the guest's guard said %d %q %q, runner-up's %d %q %q", filepath.Base(ev),
				pyCode, pyOut.String(), pyErr.String(), code, goOut.String(), goErr.String())
		}
	}
}

// The guest's half of the handshake, on a pseudo-terminal as on the VM's
// serial port: it says it is ready until a line arrives, and takes that line.
func TestGuestWaitsForTheRegistrationOnItsSerialPort(t *testing.T) {
	py := python(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "guest.py"), []byte(hypervGuest), 0o600)
	script := `
import importlib.util, os, sys, threading, time, tty
spec = importlib.util.spec_from_file_location("guest", sys.argv[1])
g = importlib.util.module_from_spec(spec); spec.loader.exec_module(g)
agent, port = os.openpty()
tty.setraw(port); tty.setraw(agent)
def host():
    seen = b""
    while g.READY not in seen:
        seen += os.read(agent, 4096)
    os.write(agent, sys.argv[2].encode() + b"\n")
threading.Thread(target=host, daemon=True).start()
print(g.registration(port))
`
	out, err := exec.Command(py, "-I", "-c", script, filepath.Join(dir, "guest.py"), registration).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != registration {
		t.Errorf("the guest took %q (%v), want the registration", out, err)
	}
}

// Intent (a bug moving a fleet to runner-up found: Linux jobs in Hyper-V VMs
// must find tools such as Node in the job image's tool cache, as on GitHub's
// Ubuntu runners, whose images set RUNNER_TOOL_CACHE in /etc/environment). The
// runner gets what the image's /etc/environment sets, but never a
// registration, hook or identity from it.
func TestGuestRunnerGetsTheImagesEnvironment(t *testing.T) {
	py := python(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "guest.py"), []byte(hypervGuest), 0o600)
	// Ubuntu 24.04's own line, then a job image's.
	etc := filepath.Join(dir, "environment")
	os.WriteFile(etc, []byte(`PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/games:/usr/local/games:/snap/bin"
# set by the job image
RUNNER_TOOL_CACHE=/opt/hostedtoolcache
ACTIONS_RUNNER_INPUT_JITCONFIG=from-the-image
HOME=/root
`), 0o600)
	script := `
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("guest", sys.argv[1])
g = importlib.util.module_from_spec(spec); spec.loader.exec_module(g)
print(json.dumps(g.runner_env(sys.argv[3], g.image_environment(sys.argv[2])), sort_keys=True))
`
	out, err := exec.Command(py, "-I", "-c", script, filepath.Join(dir, "guest.py"), etc, registration).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var env map[string]string
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for k, want := range map[string]string{
		"RUNNER_TOOL_CACHE":               "/opt/hostedtoolcache",
		"PATH":                            "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/games:/usr/local/games:/snap/bin",
		"ACTIONS_RUNNER_INPUT_JITCONFIG":  registration,
		"ACTIONS_RUNNER_HOOK_JOB_STARTED": "/opt/runner-up/fork-guard.sh",
		"HOME":                            "/home/runner",
		"USER":                            "runner",
	} {
		if env[k] != want {
			t.Errorf("the runner gets %s=%q, want %q", k, env[k], want)
		}
	}
	// An image without the file still runs the runner.
	out, err = exec.Command(py, "-I", "-c", script, filepath.Join(dir, "guest.py"), filepath.Join(dir, "missing"), registration).CombinedOutput()
	env = nil
	if err != nil || json.Unmarshal(out, &env) != nil {
		t.Fatalf("with no /etc/environment: %v: %s", err, out)
	}
	if env["ACTIONS_RUNNER_INPUT_JITCONFIG"] != registration || env["PATH"] == "" || env["RUNNER_TOOL_CACHE"] != "" {
		t.Errorf("with no /etc/environment the runner gets %v", env)
	}
}
