//go:build !windows

package testrun

import (
	"syscall"
	"testing"
	"time"
)

// childEnds fails t unless process pid, the stand-in's child, is gone within
// Grace. Once its parent dies the system reaps it at once.
func childEnds(t *testing.T, pid int) {
	t.Helper()
	end := time.Now().Add(min(Grace, deadline(t)))
	for syscall.Kill(pid, 0) != syscall.ESRCH {
		if time.Now().After(end) {
			t.Errorf("the command's child %d outlived it", pid)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
