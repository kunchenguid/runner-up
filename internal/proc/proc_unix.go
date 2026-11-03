//go:build !windows

// Package proc starts, detaches and ends processes.
package proc

import (
	"os/exec"
	"syscall"
)

// Exec replaces this process with the program at path, run with argv and
// env: the same process, so whatever supervises it - launchd, systemd - keeps
// supervising the program it becomes.
func Exec(path string, argv, env []string) error { return syscall.Exec(path, argv, env) }

// Detach starts cmd in its own session, so it outlives the coordinator.
func Detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// Terminate asks process pid to stop.
func Terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func CloseOnExec(fd int) { syscall.CloseOnExec(fd) }

// CoordinatorRuns is nil where the coordinator can run.
func CoordinatorRuns() error { return nil }
