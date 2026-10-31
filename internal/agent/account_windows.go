package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// accountProvider runs each job in a fresh Windows account that runner-up's
// service makes and deletes; the agent only asks the service over its pipe.
type accountProvider struct{}

func (accountProvider) run(ctx context.Context, a *AgentRun, jit string, out io.Writer) error {
	conn, err := dialService()
	if err != nil {
		return err
	}
	return askService(ctx, conn, JobRequest{
		Version: jobRequestVersion, Op: "run", Place: a.Place,
		Cores: a.Cores, MemoryMB: a.MemoryMB, AllowForks: a.AllowForks, Registration: jit,
	}, out)
}

func (accountProvider) clean(ctx context.Context, a *AgentRun) error {
	conn, err := dialService()
	if err != nil {
		return err
	}
	return askService(ctx, conn, JobRequest{Version: jobRequestVersion, Op: "clean", Place: a.Place}, io.Discard)
}

// accountReady is "ok" when runner-up's service answers here.
func accountReady() string {
	conn, err := dialService()
	if err != nil {
		return err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := askService(ctx, conn, JobRequest{Version: jobRequestVersion, Op: "probe"}, io.Discard); err != nil {
		return err.Error()
	}
	return "ok"
}

// dialService opens the service's pipe and checks SYSTEM owns it: any
// account may make a pipe of the same name while the service is stopped, to
// take the runner's registration.
func dialService() (io.ReadWriteCloser, error) {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	return dialPipe(pipeName, system)
}

// dialPipe opens the service's pipe at path, waiting up to a minute for a
// free instance, asking only for what the service grants the runner
// account, and checks owner owns it.
func dialPipe(path string, owner *windows.SID) (io.ReadWriteCloser, error) {
	name, _ := windows.UTF16PtrFromString(path)
	deadline := time.Now().Add(time.Minute)
	var h windows.Handle
	for {
		var err error
		h, err = windows.CreateFile(name, clientAccess, 0, nil,
			windows.OPEN_EXISTING, windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			break
		}
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, errors.New("runner-up's service is not running here; run runner-up setup")
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) || time.Now().After(deadline) {
			return nil, fmt.Errorf("could not reach runner-up's service: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	ok, err := ownerIs(h, owner)
	if err == nil && !ok {
		err = errors.New("the pipe named runner-up is not the service's own")
	}
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// memoryMB is the machine's memory.
func memoryMB() int {
	var m struct {
		Length               uint32
		MemoryLoad           uint32
		TotalPhys            uint64
		AvailPhys            uint64
		TotalPageFile        uint64
		AvailPageFile        uint64
		TotalVirtual         uint64
		AvailVirtual         uint64
		AvailExtendedVirtual uint64
	}
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx").Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0
	}
	return int(m.TotalPhys >> 20)
}

// Host places run the job as the agent's own user, which on Windows is the
// SSH sign-in: account places are Windows's way to run on the machine itself.
type hostProvider struct{}

var errHostOnWindows = errors.New("host places do not run on Windows machines; use account places")

func (hostProvider) run(context.Context, *AgentRun, string, io.Writer) error {
	return errHostOnWindows
}
func (hostProvider) clean(context.Context, *AgentRun) error { return nil }
func hostReady() string                                     { return errHostOnWindows.Error() }
