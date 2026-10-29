//go:build !windows

// Package proc starts, detaches and ends processes.
package proc

import (
	"os/exec"
	"syscall"
)

// Detach starts cmd in its own session, so it outlives the coordinator.
func Detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// Terminate asks process pid to stop.
func Terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func CloseOnExec(fd int) { syscall.CloseOnExec(fd) }

// CoordinatorRuns is nil where the coordinator can run.
func CoordinatorRuns() error { return nil }
