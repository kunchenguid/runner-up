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

// Intent (README, "When a machine is down"). A machine that takes SSH's
// connection but never answers held its place runner for as long as ssh waited,
// which is for good; one that drops the connection silently held it for the
// system's own connect timeout. ssh to a machine now gives up after ten
// seconds, and says so by exiting 255, which rests the machine (reach.go). A
// machine's own ssh options may set another timeout.
func TestSshGivesUpOnAMachineThatNeverAnswers(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("needs ssh")
	}
	// The machine takes each connection, says it took it, and never says a
	// word to ssh.
	accepted := make(chan struct{}, 2)
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
			accepted <- struct{}{}
		}
	}()
	port := fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	// The default ssh connects first, then the one with the machine's own
	// shorter timeout. That one ends first, while the default one still
	// waits; then the default one ends. Both end before the allowance below
	// would kill them, which makes no exit 255.
	type ended struct {
		name string
		code int
		out  []byte
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	order := make(chan ended, 2)
	for _, tc := range []struct {
		name string
		ssh  []string
	}{
		{"by default", []string{"-F", "/dev/null", "-p", port, "far@127.0.0.1"}},
		{"with its own timeout", []string{"-o", "ConnectTimeout=1", "-F", "/dev/null", "-p", port, "far@127.0.0.1"}},
	} {
		argv := append(SshArgs(config.Machine{Name: "far", SSH: tc.ssh}), "true")
		go func() {
			out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
			order <- ended{tc.name, agent.ExitCode(err), out}
		}()
		select {
		case <-accepted:
		case got := <-order:
			t.Fatalf("ssh %s exited %d before it connected: %s", got.name, got.code, got.out)
		}
	}
	for _, want := range []string{"with its own timeout", "by default"} {
		got := <-order
		if got.name != want || got.code != sshFailed {
			t.Fatalf("ssh %s exited %d, want ssh %s to exit %d next: %s", got.name, got.code, want, sshFailed, got.out)
		}
	}
}
