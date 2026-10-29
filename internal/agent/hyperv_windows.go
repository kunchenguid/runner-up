package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

type hypervProvider struct{}

// pipeEnds are how a read of the serial port's pipe ends once Hyper-V has
// closed it as the VM turned off.
var pipeEnds = []error{windows.ERROR_PIPE_NOT_CONNECTED, windows.ERROR_BROKEN_PIPE}

func (hypervProvider) vmDir(a *AgentRun) string { return filepath.Join(a.Dir(), "vm") }

// powershell runs script after the Hyper-V functions and returns the first
// line of what went wrong.
func powershell(ctx context.Context, script string) error {
	cmd := exec.CommandContext(ctx, "powershell.exe", encodedPowerShell(script)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return errors.New(FirstLine(out.String(), err))
	}
	return nil
}

func (p hypervProvider) clean(ctx context.Context, a *AgentRun) error {
	if err := powershell(ctx, hypervRemoveScript(a.Place)); err != nil {
		return fmt.Errorf("could not delete the VM: %w", err)
	}
	// Hyper-V may hold the disk for a moment after deleting the VM.
	var err error
	for range 30 {
		if err = os.RemoveAll(p.vmDir(a)); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return err
}

func (p hypervProvider) run(ctx context.Context, a *AgentRun, jit string, out io.Writer) error {
	if msg := hypervReady(); msg != "ok" {
		return errors.New(msg)
	}
	dir, err := ensureDir(p.vmDir(a))
	if err != nil {
		return err
	}
	seed, err := hypervSeed(a.Place, runnerArch(), a.AllowForks, time.Now())
	if err != nil {
		return err
	}
	iso, err := seedISO(seed)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.iso"), iso, 0o600); err != nil {
		return err
	}
	if err := powershell(ctx, hypervCreateScript(a, dir)); err != nil {
		return fmt.Errorf("could not make the VM: %w", err)
	}
	fmt.Fprintf(out, "runner-up agent: virtual machine for place %s is starting\n", a.Place)
	conn, err := dialGuest(ctx, hypervPipe(a.Place))
	if err != nil {
		return err
	}
	return talkToGuest(ctx, conn, jit, out, a.BootTimeout)
}

// dialGuest opens the VM's serial port pipe, which Hyper-V makes as the VM
// starts. Before sending the registration it checks Hyper-V made it: another
// account could make a pipe of the same name first. Hyper-V's own pipes
// belong to the VM's account (NT VIRTUAL MACHINE), SYSTEM or Administrators,
// which a standard account cannot give a pipe.
func dialGuest(ctx context.Context, pipe string) (io.ReadWriteCloser, error) {
	name, _ := windows.UTF16PtrFromString(pipe)
	deadline := time.Now().Add(time.Minute)
	var h windows.Handle
	for {
		var err error
		h, err = windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL, 0, nil,
			windows.OPEN_EXISTING, windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PIPE_BUSY) || time.Now().After(deadline) {
			return nil, fmt.Errorf("could not open the VM's serial port %s: %w", pipe, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err := hyperVOwns(h); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("%s: %w", pipe, err)
	}
	return os.NewFile(uintptr(h), pipe), nil
}

func hyperVOwns(h windows.Handle) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	s := owner.String()
	if s == "S-1-5-18" || s == "S-1-5-32-544" || strings.HasPrefix(s, "S-1-5-83-") {
		return nil
	}
	return fmt.Errorf("the pipe belongs to %s, not Hyper-V", s)
}

// hypervReady is "ok" when this account may make Hyper-V VMs here.
func hypervReady() string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := powershell(ctx, "$null = Get-RunnerUpVsms\n"); err != nil {
		return "no Hyper-V for this account (run runner-up setup --hyperv): " + err.Error()
	}
	return "ok"
}
