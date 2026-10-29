//go:build !windows

package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Account and Hyper-V places run only on Windows.
type accountProvider struct{}

var errWindowsOnly = errors.New("runs only on Windows")

func (accountProvider) run(context.Context, *AgentRun, string, io.Writer) error {
	return errWindowsOnly
}
func (accountProvider) clean(context.Context, *AgentRun) error { return nil }

func accountReady() string { return errWindowsOnly.Error() }

func RunService() error { return errors.New("runner-up's service " + errWindowsOnly.Error()) }

// memoryMB is the machine's memory from /proc/meminfo, or a Mac's from
// sysctl, or 0 where neither answers.
func memoryMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		out, err := exec.Command("/usr/sbin/sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		return int(n / (1024 * 1024))
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.Atoi(f[1])
			return kb / 1024
		}
	}
	return 0
}

// protectFile keeps the job account record to its owner.
func protectFile(path string) error { return os.Chmod(path, 0o600) }

func runSetup(setupOptions) error {
	return errors.New("runner-up setup is for Windows machines; on Linux and macOS, machine add installs the agent")
}
