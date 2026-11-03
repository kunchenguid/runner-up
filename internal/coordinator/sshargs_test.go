package coordinator

import (
	"slices"
	"testing"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (README.md, "When a machine is down"): ssh to a machine never asks a
// person for anything, notices a dropped connection within a minute, and gives
// up after ten seconds without an answer (ConnectTimeout=10) unless the
// machine's own ssh options set ConnectTimeout. This test owns ssh's option
// words; tests that see a command line read them from SshArgs.
func TestSshArgsAreBatchKeptAliveAndTimed(t *testing.T) {
	head := []string{"ssh", "-T", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4"}
	for _, tc := range []struct {
		why  string
		ssh  []string
		want []string
	}{
		{"the default timeout", []string{"ci@box"}, append(slices.Clone(head), "-o", "ConnectTimeout=10", "ci@box")},
		{"the machine's own timeout", []string{"-o", "connecttimeout=3", "ci@box"}, append(slices.Clone(head), "-o", "connecttimeout=3", "ci@box")},
	} {
		if got := SshArgs(config.Machine{Name: "box", SSH: tc.ssh}); !slices.Equal(got, tc.want) {
			t.Errorf("with %s ssh runs as %q, want %q", tc.why, got, tc.want)
		}
	}
}
