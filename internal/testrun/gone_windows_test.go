package testrun

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code Windows reports for a running process.
const stillActive = 259

// childEnds fails t unless process pid, the stand-in's child, is gone within
// Grace.
func childEnds(t *testing.T, pid int) {
	t.Helper()
	end := time.Now().Add(min(Grace, deadline(t)))
	for running(pid) {
		if time.Now().After(end) {
			t.Errorf("the command's child %d outlived it", pid)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func running(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == stillActive
}
