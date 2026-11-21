//go:build !windows

package agent

import (
	"io"
	"os"
	"syscall"
	"testing"
)

// Intent: as TestClosedInputDoesNotCleanUpThePlaceBeforeItsRunReturns, for
// the hangup an SSH server may send its session's command as it closes the
// session.
func TestHangupDoesNotCleanUpThePlaceBeforeItsRunReturns(t *testing.T) {
	loseLink(t, func(io.Closer) { syscall.Kill(os.Getpid(), syscall.SIGHUP) })
}
