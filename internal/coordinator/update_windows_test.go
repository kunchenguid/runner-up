package coordinator

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/quote"
	releases "github.com/kunchenguid/runner-up/internal/release"
	"golang.org/x/sys/windows"
)

func longPath(t *testing.T, path string) string {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetLongPathName(p, &b[0], uint32(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return windows.UTF16ToString(b[:n])
}

// Intent ("copy agents to the PC"): the
// coordinator sends a release's program to a Windows machine on standard
// input of the line it hands the machine's SSH server, which runs it with
// cmd.exe. The machine keeps the program byte for byte in the account's own
// folder for that version and prints its path, a path the coordinator can run
// over SSH; a copy whose SHA-256 is not the release's is refused, and the
// program already there stays.
func TestAWindowsMachineKeepsTheAgentSentOnItsInput(t *testing.T) {
	// This test program: megabytes of a real Windows program.
	prog, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	// The account's folder, by its long name as Windows gives it to the
	// account: the runner's temporary folder may be named in 8.3 form.
	local := longPath(t, t.TempDir())
	send := func(prog []byte, sum string) (string, error) {
		line, _ := agentCopy(config.Machine{Name: "pc", OS: "windows", SSH: []string{"runner@pc"}}, "v9.0.2", sum)
		cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"))
		// As Windows' OpenSSH server starts a command: cmd.exe /c and the line.
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd.exe /c " + line}
		cmd.Env = append(os.Environ(), "LOCALAPPDATA="+local)
		cmd.Stdin = bytes.NewReader(prog)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return stderr.String(), err
		}
		return strings.TrimSpace(string(out)), nil
	}

	path, err := send(prog, releases.SHA256(prog))
	if err != nil {
		t.Fatalf("the copy failed: %v\n%s", err, path)
	}
	if want := filepath.Join(local, "runner-up", "v9.0.2", "runner-up.exe"); path != want {
		t.Fatalf("the machine printed %q, want %q", path, want)
	}
	if err := quote.CheckWinWord(path, true); err != nil {
		t.Errorf("the coordinator cannot run the copy over SSH: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, prog) {
		t.Fatalf("the copy holds %d bytes (%v), not the %d sent", len(got), err, len(prog))
	}

	other := append(bytes.Clone(prog), 0)
	if out, err := send(other, releases.SHA256(prog)); err == nil || !strings.Contains(out, "not the release's") {
		t.Errorf("a copy with another SHA-256 was not refused: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, prog) {
		t.Errorf("a refused copy changed the program already there (%v)", err)
	}
}
