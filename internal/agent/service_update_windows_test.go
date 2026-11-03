package agent

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/release"
	"github.com/kunchenguid/runner-up/internal/testbin"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/kunchenguid/runner-up/internal/testrun"
)

func TestMain(m *testing.M) { testbin.Main(m) }

// Intent (README, "Windows machines" and "Updating runner-up"): runner-up setup
// installs the service from a folder of its version and starts it; run again
// with the same program, it leaves the running service as it is. Handed a newer
// release through the agent, as the coordinator hands it, the service refuses
// one the key it trusts did not sign, and installs a signed one, restarts as it
// and answers with its version. An administrator's setup then installs the
// version before again.
//
// It installs runner-up's service on this machine, so it runs only on a
// GitHub-hosted runner, which is thrown away after the run.
func TestTheServiceSetsUpOnceAndUpdatesItselfFromASignedRelease(t *testing.T) {
	if os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Skip("installs runner-up's service on this machine; runs only on a GitHub-hosted runner")
	}
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	build := func(version string) string {
		return testbin.Build(t, "-X github.com/kunchenguid/runner-up/internal/version.Version="+version+
			" -X github.com/kunchenguid/runner-up/internal/release.signingKeys="+base64.StdEncoding.EncodeToString(public))
	}
	older, newer := build("v9.0.1"), build("v9.0.2")
	user := os.Getenv("USERNAME")
	run := func(stdin []byte, program string, args ...string) (string, error) {
		cmd := testrun.Command(t, program, args...)
		cmd.Stdin = bytes.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	m, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Disconnect()
	t.Cleanup(func() {
		if s, err := m.OpenService(serviceName); err == nil {
			s.Control(svc.Stop)
			s.Delete()
			s.Close()
		}
	})
	// service is the service's program and process.
	service := func() (string, uint32) {
		t.Helper()
		s, err := m.OpenService(serviceName)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		c, err := s.Config()
		if err != nil {
			t.Fatal(err)
		}
		st, err := s.Query()
		if err != nil {
			t.Fatal(err)
		}
		return c.BinaryPathName, st.ProcessId
	}
	installed := func(program, version string) string {
		b, err := os.ReadFile(program)
		if err != nil {
			t.Fatal(err)
		}
		return `"` + filepath.Join(os.Getenv("ProgramFiles"), "runner-up", version+"-"+release.SHA256(b)[:12], "runner-up.exe") + `" service`
	}
	answers := func(version string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for {
			out, err := run(nil, older, "agent", "service-version")
			if err == nil && strings.TrimSpace(out) == version {
				return
			}
			if time.Now().After(deadline) {
				log, _ := os.ReadFile(filepath.Join(os.Getenv("ProgramData"), "runner-up", "service.log"))
				t.Fatalf("the service answers %q (%v), want %s\nservice.log:\n%s", out, err, version, log)
			}
			time.Sleep(time.Second)
		}
	}

	if out, err := run(nil, older, "setup", "--user", user); err != nil || !strings.Contains(out, "runner-up service running v9.0.1.") {
		t.Fatalf("setup failed: %v\n%s", err, out)
	}
	bin, pid := service()
	if want := installed(older, "v9.0.1"); bin != want {
		t.Fatalf("the service runs %s, want %s", bin, want)
	}
	answers("v9.0.1")
	if out, err := run(nil, older, "setup", "--user", user); err != nil || !strings.Contains(out, "runner-up service already runs v9.0.1, unchanged.") {
		t.Fatalf("setup again failed: %v\n%s", err, out)
	}
	if again, samePID := service(); again != bin || samePID != pid {
		t.Fatalf("setup with the same program restarted the service: %s pid %d, was %s pid %d", again, samePID, bin, pid)
	}

	// The release as the release workflow makes it: the program in a zip,
	// listed in checksums.txt, which the private key signs.
	platform := release.Platform("windows", runtime.GOARCH)
	name := release.ArchiveName("v9.0.2", platform)
	program, err := os.ReadFile(newer)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	f, _ := z.Create(release.ProgramName(platform))
	f.Write(program)
	z.Close()
	checksums := fmt.Sprintf("%s  %s\n", release.SHA256(archive.Bytes()), name)
	hand := func(sig []byte) (string, error) {
		head, err := json.Marshal(ServiceUpdate{Archive: name, Size: int64(archive.Len()), Checksums: checksums,
			Signature: base64.StdEncoding.EncodeToString(sig)})
		if err != nil {
			t.Fatal(err)
		}
		return run(append(append(head, '\n'), archive.Bytes()...), older, "agent", "update-service")
	}
	_, other, _ := ed25519.GenerateKey(nil)
	// TestTheServiceInstallsOnlyANewerReleaseTheSigningKeySigned owns the refusal's words;
	// here the service refuses and keeps its program.
	if out, err := hand(ed25519.Sign(other, []byte(checksums))); err == nil {
		t.Fatalf("the service took a release another key signed: %v\n%s", err, out)
	}
	answers("v9.0.1")
	if again, _ := service(); again != bin {
		t.Fatalf("after a release another key signed the service runs %s, want %s", again, bin)
	}
	if out, err := hand(ed25519.Sign(private, []byte(checksums))); err != nil || !strings.Contains(out, serviceUpdatingTo+"v9.0.2") {
		t.Fatalf("the service did not take the signed release: %v\n%s", err, out)
	}
	answers("v9.0.2")
	if bin, _ := service(); bin != installed(newer, "v9.0.2") {
		t.Fatalf("the service runs %s, want %s", bin, installed(newer, "v9.0.2"))
	}

	// An administrator installs any version.
	if out, err := run(nil, older, "setup", "--user", user); err != nil {
		t.Fatalf("setup with the version before failed: %v\n%s", err, out)
	}
	answers("v9.0.1")
	if again, _ := service(); again != bin {
		t.Fatalf("after setup with the version before the service runs %s, want %s", again, bin)
	}
}
