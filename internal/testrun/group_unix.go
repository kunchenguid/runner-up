//go:build !windows

package testrun

import (
	"os"
	"os/exec"
	"syscall"
)

func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killGroup kills every process left in the group p leads.
func killGroup(p *os.Process) { syscall.Kill(-p.Pid, syscall.SIGKILL) }
