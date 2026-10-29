//go:build linux

package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
	"golang.org/x/crypto/ssh"
)

// kvmProvider runs each job in a new KVM virtual machine. Its disk is a
// copy-on-write layer over the place's base cloud image, which nothing
// changes, and is deleted after the job. The machine is set up at boot by
// cloud-init from a seed the agent serves on the loopback address: a user
// named runner, with passwordless sudo as on GitHub's own runners, who signs
// in only with a key the agent makes for this one job. The agent then copies
// in the runner and starts it over SSH, on a port forwarded from the
// machine's loopback address.
type kvmProvider struct{}

func (kvmProvider) vmDir(a *AgentRun) string { return filepath.Join(a.Dir(), "vm") }

func (p kvmProvider) clean(ctx context.Context, a *AgentRun) error {
	dir := p.vmDir(a)
	if b, err := os.ReadFile(filepath.Join(dir, "qemu.pid")); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		// Only a QEMU started for this place's folder: the pid may be reused.
		if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); pid > 1 && err == nil && bytes.Contains(cmdline, []byte(dir)) {
			syscall.Kill(pid, syscall.SIGKILL)
			for i := 0; i < 100 && syscall.Kill(pid, 0) == nil; i++ {
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
	return os.RemoveAll(dir)
}

func (p kvmProvider) run(ctx context.Context, a *AgentRun, jit string, out io.Writer) error {
	if msg := kvmReady(); msg != "ok" {
		return errors.New(msg)
	}
	dir, err := ensureDir(p.vmDir(a))
	if err != nil {
		return err
	}
	tarball, err := runnerTarball(ctx, filepath.Join(a.StateDir, "cache"))
	if err != nil {
		return err
	}
	disk, err := overlay(ctx, a, dir)
	if err != nil {
		return err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return err
	}
	seed, err := serveSeed(a.Place, signer.PublicKey())
	if err != nil {
		return err
	}
	defer seed.Close()
	port, err := freePort()
	if err != nil {
		return err
	}
	qemuLog, err := os.Create(filepath.Join(dir, "qemu.log"))
	if err != nil {
		return err
	}
	defer qemuLog.Close()
	cmd := exec.Command(qemuBinary(), qemuArgs(a, dir, disk, port, seed.Addr().(*net.TCPAddr).Port)...)
	cmd.Stdout, cmd.Stderr = qemuLog, qemuLog
	// QEMU dies with the agent; clean removes it by its pid file otherwise.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("qemu: %w", err)
	}
	qemuDone := make(chan struct{})
	go func() { cmd.Wait(); close(qemuDone) }()
	defer func() {
		cmd.Process.Kill()
		<-qemuDone
	}()

	client, err := dialVM(ctx, port, signer, a.BootTimeout, qemuDone)
	if err != nil {
		return fmt.Errorf("%w; the console log ends: %s", err, tail(filepath.Join(dir, "console.log"), 400))
	}
	defer client.Close()
	fmt.Fprintf(out, "runner-up agent: virtual machine for place %s is up\n", a.Place)

	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	err = runSession(client, f, nil, "mkdir -p actions-runner && tar xzf - -C actions-runner")
	f.Close()
	if err != nil {
		return fmt.Errorf("copying the runner in: %w", err)
	}
	env := ""
	if !a.AllowForks {
		self, err := os.Open(a.Self)
		if err != nil {
			return err
		}
		err = runSession(client, self, nil, "mkdir -p .runner-up && cat > .runner-up/runner-up && chmod 0755 .runner-up/runner-up")
		self.Close()
		if err == nil {
			err = runSession(client, strings.NewReader(ForkGuardHook("/home/runner/.runner-up/runner-up")), nil,
				"cat > .runner-up/fork-guard.sh && chmod 0755 .runner-up/fork-guard.sh")
		}
		if err != nil {
			return fmt.Errorf("copying the fork guard in: %w", err)
		}
		env = "export ACTIONS_RUNNER_HOOK_JOB_STARTED=/home/runner/.runner-up/fork-guard.sh; "
	}
	// The registration goes in on the session's input, never in a command
	// line; the runner removes the variable from its environment before any
	// job starts. Without ICU, .NET runs in its invariant mode.
	script := "read -r jit; export ACTIONS_RUNNER_INPUT_JITCONFIG=\"$jit\"; unset jit; " + env +
		"/sbin/ldconfig -p 2>/dev/null | grep -q libicuuc || export DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1; " +
		"exec ./actions-runner/run.sh"
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	session.Stdin = strings.NewReader(jit + "\n")
	session.Stdout, session.Stderr = out, out
	if err := session.Start(script); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case <-done:
	case <-qemuDone:
		return fmt.Errorf("the virtual machine stopped while the runner ran")
	case <-ctx.Done():
	}
	return nil
}

// overlay makes the place's disk: a copy-on-write layer over the base image.
func overlay(ctx context.Context, a *AgentRun, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", a.Image).Output()
	if err != nil {
		return "", fmt.Errorf("base image %s unreadable: %w", a.Image, err)
	}
	var info struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(out, &info); err != nil || info.Format == "" {
		return "", fmt.Errorf("base image %s has no format qemu-img knows", a.Image)
	}
	size := a.DiskGB
	if size <= 0 {
		size = config.DefaultDiskGB
	}
	disk := filepath.Join(dir, "disk.qcow2")
	if b, err := exec.CommandContext(ctx, "qemu-img", "create", "-q", "-f", "qcow2", "-F", info.Format, "-b", a.Image,
		disk, strconv.Itoa(size)+"G").CombinedOutput(); err != nil {
		return "", fmt.Errorf("qemu-img create: %s", FirstLine(string(b), err))
	}
	return disk, nil
}

func qemuBinary() string {
	if runtime.GOARCH == "arm64" {
		return "qemu-system-aarch64"
	}
	return "qemu-system-x86_64"
}

// armFirmware are where distributions put the UEFI firmware an arm64 virtual
// machine boots from.
var armFirmware = []string{
	"/usr/share/qemu-efi-aarch64/QEMU_EFI.fd",
	"/usr/share/edk2/aarch64/QEMU_EFI.fd",
	"/usr/share/AAVMF/AAVMF_CODE.fd",
}

func qemuArgs(a *AgentRun, dir, disk string, sshPort, seedPort int) []string {
	args := []string{"-name", "runner-up-" + a.Place, "-nodefaults", "-display", "none",
		"-pidfile", filepath.Join(dir, "qemu.pid"),
		"-cpu", "host", "-smp", strconv.Itoa(a.Cores), "-m", strconv.Itoa(a.MemoryMB),
		"-drive", "if=virtio,format=qcow2,file=" + strings.ReplaceAll(disk, ",", ",,"),
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:22", sshPort),
		"-device", "virtio-net-pci,netdev=net0",
		"-serial", "file:" + filepath.Join(dir, "console.log"),
		// cloud-init reads its seed from the agent: 10.0.2.2 is the machine's
		// loopback address as the virtual machine sees it.
		"-smbios", fmt.Sprintf("type=1,serial=ds=nocloud;s=http://10.0.2.2:%d/", seedPort),
	}
	if runtime.GOARCH == "arm64" {
		args = append(args, "-machine", "virt,accel=kvm,gic-version=host")
		for _, fw := range armFirmware {
			if _, err := os.Stat(fw); err == nil {
				args = append(args, "-bios", fw)
				break
			}
		}
	} else {
		args = append(args, "-machine", "q35,accel=kvm")
	}
	return args
}

// kvmReady says "ok" when this machine can run KVM places, or why not.
func kvmReady() string {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return "no usable /dev/kvm: " + err.Error()
	}
	f.Close()
	for _, bin := range []string{qemuBinary(), "qemu-img"} {
		if _, err := exec.LookPath(bin); err != nil {
			return bin + " is not installed"
		}
	}
	if runtime.GOARCH == "arm64" {
		for _, fw := range armFirmware {
			if _, err := os.Stat(fw); err == nil {
				return "ok"
			}
		}
		return "no arm64 UEFI firmware (install qemu-efi-aarch64)"
	}
	return "ok"
}

// serveSeed serves cloud-init's NoCloud seed on a loopback port. It holds
// nothing secret: the key it hands out is the public half.
func serveSeed(place string, key ssh.PublicKey) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	meta := fmt.Sprintf("instance-id: runner-up-%s-%d\nlocal-hostname: runner-up-%s\n", place, time.Now().UnixNano(), place)
	user := "#cloud-config\n" +
		"users:\n" +
		"  - name: runner\n" +
		"    shell: /bin/bash\n" +
		"    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n" +
		"    lock_passwd: true\n" +
		"    ssh_authorized_keys:\n" +
		"      - " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + "\n" +
		"ssh_pwauth: false\n" +
		"disable_root: true\n"
	mux := http.NewServeMux()
	mux.HandleFunc("/meta-data", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, meta) })
	mux.HandleFunc("/user-data", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, user) })
	mux.HandleFunc("/vendor-data", func(w http.ResponseWriter, _ *http.Request) {})
	go http.Serve(ln, mux)
	return ln, nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// dialVM signs in to the virtual machine once it has booted. The port is
// QEMU's: QEMU stops at once if it cannot take it, and every attempt first
// checks QEMU still runs, so no other program on the machine can answer in
// its place. That is why any host key the machine made at boot is accepted.
func dialVM(ctx context.Context, port int, signer ssh.Signer, timeout time.Duration, qemuDone <-chan struct{}) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{User: "runner", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-qemuDone:
			return nil, fmt.Errorf("the virtual machine stopped while booting")
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		if c, err := dialOnce(addr, cfg); err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the virtual machine did not boot within %s", timeout)
		}
	}
}

// dialOnce bounds the whole handshake: QEMU accepts the connection before the
// machine's SSH server is up.
func dialOnce(addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := net.DialTimeout("tcp", addr, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(cfg.Timeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

func runSession(client *ssh.Client, in io.Reader, out io.Writer, command string) error {
	s, err := client.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	var stderr bytes.Buffer
	s.Stdin, s.Stdout, s.Stderr = in, out, &stderr
	if err := s.Run(command); err != nil {
		return fmt.Errorf("%v: %s", err, FirstLine(stderr.String(), nil))
	}
	return nil
}

// tail is the last n bytes of a file, on one line.
func tail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return "(none)"
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > n {
		f.Seek(-n, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return strings.Join(strings.Fields(string(b)), " ")
}
