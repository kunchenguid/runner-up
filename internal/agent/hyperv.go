package agent

import (
	"bufio"
	"context"

	"github.com/kunchenguid/runner-up/internal/config"

	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf16"
)

// A Hyper-V place runs its job in a fresh VM on a Windows machine: a
// generation 2 VM on a differencing disk over the place's base disk, which
// nothing changes, joined to a Hyper-V switch (the Default Switch, with its
// own address translation, unless the place names another), with a seed disc
// the agent writes.
//
// A Linux guest boots under secure boot trusting Ubuntu's signed shim, and
// cloud-init sets it up from the seed disc: a user named runner, with
// passwordless sudo as on GitHub's own runners, and runner-up's guest program
// (hyperv-guest.py).
//
// A Windows guest boots under secure boot trusting Windows. Its image signs
// the standard user runner in to its desktop and, at that sign-in, runs
// guest.ps1 from the seed disc, which also holds the agent's own program for
// the fork guard (hyperv-guest.ps1).
//
// Either way, the registration and the runner's output travel over the VM's
// second serial port, which Hyper-V joins to a named pipe on the machine: no
// network path from the machine into the VM is needed. The VM and its folder
// are deleted before and after every job.

//go:embed hyperv.ps1
var hypervLibrary string

//go:embed hyperv-guest.py
var hypervGuest string

//go:embed hyperv-guest.ps1
var hypervWindowsGuest string

// The secure boot templates Hyper-V knows a guest's boot loader by.
const (
	// secureBootLinux trusts the Microsoft UEFI CA, which signs Ubuntu's
	// shim.
	secureBootLinux = "272e7447-90a4-4563-a4b9-8e4ab00526ce"
	// secureBootWindows trusts Microsoft Windows.
	secureBootWindows = "1734c6e8-3154-4dda-ba5f-a874cc483422"
)

// The files on a Windows guest's seed disc: guest.ps1 is the name the
// image's sign-in task looks for in the root of each CD drive.
const (
	windowsGuestFile   = "guest.ps1"
	windowsGuestRunner = "runner-up.exe"
)

// guestReady is the line the guest program says until it has the
// registration.
const guestReady = "runner-up guest: ready"

func hypervVMName(place string) string { return "runner-up-" + place }
func hypervPipe(place string) string   { return `\\.\pipe\runner-up-` + place }

// hypervSeed is the place's cloud-init seed: no secret, since the
// registration comes later over the serial port.
func hypervSeed(place, arch string, l runnerLaunch, now time.Time) (map[string][]byte, error) {
	guest, err := linuxGuest(arch, l)
	if err != nil {
		return nil, err
	}
	user := "#cloud-config\n" +
		"users:\n" +
		"  - name: runner\n" +
		"    shell: /bin/bash\n" +
		"    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n" +
		"    lock_passwd: true\n" +
		"ssh_pwauth: false\n" +
		"disable_root: true\n" +
		"write_files:\n" +
		"  - path: /opt/runner-up/guest.py\n" +
		"    permissions: \"0755\"\n" +
		"    encoding: b64\n" +
		"    content: " + base64.StdEncoding.EncodeToString([]byte(guest)) + "\n" +
		"runcmd:\n" +
		"  - [/usr/bin/python3, /opt/runner-up/guest.py]\n"
	meta := fmt.Sprintf("instance-id: runner-up-%s-%d\nlocal-hostname: runner-up-%s\n", place, now.UnixNano(), place)
	return map[string][]byte{"user-data": []byte(user), "meta-data": []byte(meta)}, nil
}

// linuxGuest is runner-up's guest program for a Linux guest of arch, filled
// in for one job: the pinned runner and the runner's launch l. It holds no
// secret.
func linuxGuest(arch string, l runnerLaunch) (string, error) {
	sum, ok := runnerSHA256["linux-"+arch]
	if !ok {
		return "", fmt.Errorf("no runner release for %s", arch)
	}
	return strings.NewReplacer(
		"@RUNNER_URL@", runnerURL(runnerTarballName(arch)),
		"@RUNNER_SHA256@", sum,
		"@LAUNCH@", guestLaunch(l),
	).Replace(hypervGuest), nil
}

// windowsGuest is runner-up's guest program for a Windows guest of arch,
// filled in for one job: the pinned runner and the runner's launch l. It
// holds no secret.
func windowsGuest(arch string, l runnerLaunch) (string, error) {
	sum, ok := runnerWindowsSHA256[arch]
	if !ok {
		return "", fmt.Errorf("no Windows runner release for %s", arch)
	}
	hook, guard := "''", "''"
	if l.Hook != "" {
		hook, guard = psWord(l.Hook), psQuote(l.Guard)
	}
	return strings.NewReplacer(
		"@RUNNER_URL@", psQuote(runnerURL(runnerZipName(arch))),
		"@RUNNER_SHA256@", psQuote(strings.ToUpper(sum)),
		"@HOOK@", hook,
		"@GUARD@", guard,
		"@LAUNCH@", psLaunch(l),
	).Replace(hypervWindowsGuest), nil
}

// hypervDisc is the seed disc of a's VM, for a guest of arch, as its volume
// label and files. Neither holds the registration, which comes later over
// the serial port.
func hypervDisc(a *AgentRun, arch string, now time.Time) (string, map[string][]byte, error) {
	l := hypervProvider{}.launch(a)
	if a.Guest != config.GuestWindows {
		seed, err := hypervSeed(a.Place, arch, l, now)
		return "cidata", seed, err
	}
	guest, err := windowsGuest(arch, l)
	if err != nil {
		return "", nil, err
	}
	// Windows runs no program built for another system, so the agent's own
	// runs in a Windows guest of the machine's architecture.
	self, err := os.ReadFile(a.Self)
	if err != nil {
		return "", nil, err
	}
	return "RUNNER_UP", map[string][]byte{windowsGuestFile: []byte(guest), windowsGuestRunner: self}, nil
}

// hypervCreateScript makes the place's VM in dir, whose seed disc the agent
// has written, and starts it.
func hypervCreateScript(a *AgentRun, dir string) string {
	size := a.DiskGB
	if size <= 0 {
		size = config.DefaultDiskGB
	}
	template := secureBootLinux
	if a.Guest == config.GuestWindows {
		template = secureBootWindows
	}
	return fmt.Sprintf(`$dir = %[2]s
New-RunnerUpDisk "$dir\disk.vhdx" %[3]s %[4]d
$null = New-RunnerUpVM -Name %[1]s -Dir $dir -Vhd "$dir\disk.vhdx" -Dvd "$dir\seed.iso" -MemoryMB %[5]d -Cpu %[6]d -Switch %[7]s -Pipe %[8]s -SecureBootTemplate %[9]s
Start-RunnerUpVM %[1]s
`, psQuote(hypervVMName(a.Place)), psQuote(dir), psQuote(a.Image), uint64(size)<<30, a.MemoryMB, a.Cores,
		psQuote(orDefault(a.Switch, config.DefaultSwitch)), psQuote(hypervPipe(a.Place)), psQuote(template))
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// hypervRemoveScript turns off and deletes the place's VM.
func hypervRemoveScript(place string) string {
	return "Remove-RunnerUpVM " + psQuote(hypervVMName(place)) + "\n"
}

// encodedPowerShell is the arguments that run script, after the Hyper-V
// functions, in Windows PowerShell.
func encodedPowerShell(script string) []string { return encodedScript(hypervLibrary + "\n" + script) }

// encodedScript is the arguments that run script in Windows PowerShell. It
// travels as -EncodedCommand, one word of base64, so nothing between here and
// PowerShell can split it.
// EncodedScript is a command line that runs script in Windows PowerShell,
// which cmd and PowerShell read the same: the script travels base64 encoded.
func EncodedScript(script string) []string {
	return append([]string{"powershell"}, encodedScript(script)...)
}

func encodedScript(script string) []string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(b)}
}

// guestGone says whether err from reading the serial port is its end: the VM
// turned off and Hyper-V closed the pipe.
func guestGone(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	for _, e := range pipeEnds {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// talkToGuest is the agent's side of the serial port: it waits up to boot
// for the guest program to say it is ready, sends it the registration, and
// copies everything else it says to out until the VM turns off and the pipe
// ends. It closes conn when ctx is done.
func talkToGuest(ctx context.Context, conn io.ReadWriteCloser, jit string, out io.Writer, boot time.Duration) error {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer func() {
		stop()
		conn.Close()
	}()
	timer := time.AfterFunc(boot, func() { conn.Close() })
	defer timer.Stop()
	r := bufio.NewReader(conn)
	sent := false
	for {
		line, err := r.ReadString('\n')
		if strings.TrimRight(line, "\r\n") == guestReady {
			if !sent {
				if !timer.Stop() {
					return fmt.Errorf("the VM did not start runner-up's guest program within %s", boot)
				}
				if _, err := io.WriteString(conn, jit+"\n"); err != nil {
					return fmt.Errorf("could not send the registration to the VM: %w", err)
				}
				sent = true
			}
		} else if line != "" {
			io.WriteString(out, line)
			if !strings.HasSuffix(line, "\n") {
				io.WriteString(out, "\n")
			}
		}
		if err == nil {
			continue
		}
		switch {
		case ctx.Err() != nil:
			return nil
		case !sent:
			return fmt.Errorf("the VM did not start runner-up's guest program within %s", boot)
		case guestGone(err):
			return nil
		}
		return fmt.Errorf("the VM's serial port: %w", err)
	}
}
