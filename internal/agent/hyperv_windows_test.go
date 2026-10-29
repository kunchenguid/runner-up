package agent

// Intent (a bug a live Hyper-V check found): a VM job that runs to
// its end failed the place with "No process is on the other end of the pipe"
// once the VM turned off and Hyper-V closed its serial port's pipe. That end
// is the job's normal end.

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// endedPipe is the VM's serial port as Windows reads it: the guest's lines,
// then the error a read gives once Hyper-V has closed the pipe.
type endedPipe struct {
	r      io.Reader
	closed error
	sent   bytes.Buffer
}

func (p *endedPipe) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if err == io.EOF {
		return n, &os.PathError{Op: "read", Path: `\\.\pipe\runner-up-ru-e2e-1`, Err: p.closed}
	}
	return n, err
}
func (p *endedPipe) Write(b []byte) (int, error) { return p.sent.Write(b) }
func (p *endedPipe) Close() error                { return nil }

func TestAVMTurningOffEndsItsJobNormally(t *testing.T) {
	for _, closed := range []error{windows.ERROR_PIPE_NOT_CONNECTED, windows.ERROR_BROKEN_PIPE} {
		p := &endedPipe{closed: closed, r: strings.NewReader(guestReady + "\nrunner-up guest: starting the runner\nrunner-up guest: the runner ended with 0\n")}
		var out bytes.Buffer
		if err := talkToGuest(context.Background(), p, registration, &out, 5*time.Second); err != nil {
			t.Errorf("a VM that turned off after its job failed the place: %v", err)
		}
		if !strings.Contains(out.String(), "the runner ended with 0") || p.sent.String() != registration+"\n" {
			t.Errorf("output %q, sent %q", out.String(), p.sent.String())
		}
	}
}
