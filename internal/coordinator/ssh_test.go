//go:build !windows

package coordinator

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (RUU-15: "add a connect timeout in SshArgs"). A machine that takes
// SSH's connection but never answers held its place runner for as long as
// ssh waited, which is for good; one that drops the connection silently held
// it for the system's own connect timeout. ssh to a machine now gives up
// after ten seconds, and says so by exiting 255, which rests the machine
// (reach.go). A machine's own ssh options may set another timeout.
func TestSshGivesUpOnAMachineThatNeverAnswers(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("needs ssh")
	}
	// The machine takes the connection and never says a word.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		var held []net.Conn
		for {
			c, err := l.Accept()
			if err != nil {
				for _, c := range held {
					c.Close()
				}
				return
			}
			held = append(held, c)
		}
	}()
	port := fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	for _, tc := range []struct {
		name   string
		ssh    []string
		within time.Duration
	}{
		{"by default", []string{"-F", "/dev/null", "-p", port, "far@127.0.0.1"}, 20 * time.Second},
		{"with its own timeout", []string{"-o", "ConnectTimeout=1", "-F", "/dev/null", "-p", port, "far@127.0.0.1"}, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			argv := append(SshArgs(config.Machine{Name: "far", SSH: tc.ssh}), "true")
			start := time.Now()
			out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
			took := time.Since(start)
			if code := agent.ExitCode(err); code != sshFailed || took > tc.within {
				t.Fatalf("ssh %v exited %d after %s, want %d within %s: %s", argv[1:], code, took.Round(time.Second), sshFailed, tc.within, out)
			}
		})
	}
}
