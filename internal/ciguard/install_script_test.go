package ciguard

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/testrun"
)

// Exercise the installer through sh, stopping at the asset request: no network,
// archive extraction, installation, or sudo is needed to test version resolution.
func TestInstallScriptLatestRedirect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell installer")
	}
	const releaseURL = "https://github.com/kunchenguid/runner-up/releases/"
	for _, tc := range []struct {
		name, effectiveURL, version string
		curlFailure                 bool
	}{
		{name: "versioned tag", effectiveURL: releaseURL + "tag/v3.1.5", version: "v3.1.5"},
		{name: "URL suffixes", effectiveURL: releaseURL + "tag/v3.1.5/?source=latest#asset", version: "v3.1.5"},
		{name: "unprefixed tag", effectiveURL: releaseURL + "tag/3.1.5", version: "3.1.5"},
		{name: "empty result"},
		{name: "empty tag", effectiveURL: releaseURL + "tag/"},
		{name: "no tag redirect", effectiveURL: releaseURL + "latest"},
		{name: "HTTP failure", effectiveURL: releaseURL + "tag/v3.1.5", curlFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "curl.log")
			commands := map[string]string{
				"uname": "#!/bin/sh\ncase \"$1\" in -s) echo Linux ;; -m) echo x86_64 ;; *) exit 99 ;; esac\n",
				"curl": `#!/bin/sh
printf '%s\n' "$*" >> "$CURL_LOG"
if [ "$*" = '-fsSLI -o /dev/null -w %{url_effective} https://github.com/kunchenguid/runner-up/releases/latest' ]; then
  [ "$CURL_FAILURE" = false ] || exit 22
  printf '%s' "$EFFECTIVE_URL"
  exit 0
fi
# Refuse every other request, including the asset: never perform a real install.
exit 42
`,
			}
			for name, script := range commands {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", dir)
			t.Setenv("TMPDIR", dir)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CURL_LOG", log)
			t.Setenv("EFFECTIVE_URL", tc.effectiveURL)
			failure := "false"
			if tc.curlFailure {
				failure = "true"
			}
			t.Setenv("CURL_FAILURE", failure)
			cmd := testrun.Command(t, "sh", filepath.Join("docs", "install.sh"))
			output, err := cmd.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("expected controlled refusal, got %v: %s", err, output)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			requests := strings.Split(strings.TrimSpace(string(data)), "\n")
			if strings.Contains(string(data), "api.github.com") {
				t.Fatalf("must not use the rate-limited API: %s", data)
			}
			if tc.version == "" {
				if len(requests) != 1 || strings.Contains(string(output), "Downloading") {
					t.Fatalf("unresolved version must not request an asset: %s\n%s", data, output)
				}
				wantExit := 1
				if tc.curlFailure {
					wantExit = 22
				} else if !strings.Contains(string(output), "Failed to determine latest version") {
					t.Fatalf("missing empty-version diagnostic: %s", output)
				}
				if exitErr.ExitCode() != wantExit {
					t.Fatalf("exit = %d, want %d: %s", exitErr.ExitCode(), wantExit, output)
				}
				return
			}
			asset := releaseURL + "download/" + tc.version + "/runner-up-v" + strings.TrimPrefix(tc.version, "v") + "-linux-amd64.tar.gz"
			if exitErr.ExitCode() != 42 || len(requests) != 2 || !strings.HasPrefix(requests[1], "-fsSL "+asset+" -o ") {
				t.Fatalf("expected versioned asset request %s, got exit %d:\n%s\n%s", asset, exitErr.ExitCode(), data, output)
			}
		})
	}
}

// releaseFixture writes a release's archive for platform, holding a runner-up
// program, and a checksums.txt as the release workflow's sha256sum writes it,
// listing every platform's archive. listed is the SHA-256 it records for this
// platform's archive; "" records the archive's real one and "-" leaves it out.
func releaseFixture(t *testing.T, dir, version, platform, listed string) (archive, checksums string, program []byte) {
	t.Helper()
	program = []byte("runner-up " + version + " " + platform + "\n")
	var buf bytes.Buffer
	name := "runner-up-" + version + "-" + platform
	if strings.HasPrefix(platform, "windows-") {
		name += ".zip"
		zw := zip.NewWriter(&buf)
		w, err := zw.Create("runner-up.exe")
		if err == nil {
			_, err = w.Write(program)
		}
		if err == nil {
			err = zw.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
	} else {
		name += ".tar.gz"
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		err := tw.WriteHeader(&tar.Header{Name: "runner-up", Mode: 0o755, Size: int64(len(program))})
		if err == nil {
			_, err = tw.Write(program)
		}
		if err == nil {
			err = tw.Close()
		}
		if err == nil {
			err = gz.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	archive = filepath.Join(dir, name)
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if listed == "" {
		sum := sha256.Sum256(buf.Bytes())
		listed = hex.EncodeToString(sum[:])
	}
	var sums strings.Builder
	for _, p := range []string{"darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64", "windows-amd64", "windows-arm64"} {
		ext := ".tar.gz"
		if strings.HasPrefix(p, "windows-") {
			ext = ".zip"
		}
		sum := sha256.Sum256([]byte(p))
		hash := hex.EncodeToString(sum[:])
		if p == platform {
			if listed == "-" {
				continue
			}
			hash = listed
		}
		fmt.Fprintf(&sums, "%s  runner-up-%s-%s%s\n", hash, version, p, ext)
	}
	checksums = filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(checksums, []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return archive, checksums, program
}

// A user installing with docs/install.sh gets only the archive the release's
// checksums.txt lists, byte for byte: a tampered or truncated download, an
// archive the list leaves out, or a missing checksums.txt installs nothing.
// The script runs for real against a stand-in curl serving the files.
func TestInstallScriptChecksum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell installer")
	}
	const release = "https://github.com/kunchenguid/runner-up/releases/"
	other := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name, listed, refusal string
		noChecksums           bool
	}{
		{name: "matching archive"},
		{name: "mismatched archive", listed: other, refusal: "does not match checksums.txt"},
		{name: "unlisted archive", listed: "-", refusal: "checksums.txt does not list runner-up-v0.1.10-linux-amd64.tar.gz"},
		{name: "no checksums.txt", noChecksums: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			installDir := filepath.Join(dir, ".local", "bin")
			for _, d := range []string{bin, installDir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			archive, checksums, program := releaseFixture(t, dir, "v0.1.10", "linux-amd64", tc.listed)
			if tc.noChecksums {
				checksums = filepath.Join(dir, "missing")
			}
			commands := map[string]string{
				"uname": "#!/bin/sh\ncase \"$1\" in -s) echo Linux ;; -m) echo x86_64 ;; *) exit 99 ;; esac\n",
				"curl": `#!/bin/sh
case "$*" in
  "-fsSLI -o /dev/null -w %{url_effective} ` + release + `latest")
    printf '%s' "` + release + `tag/v0.1.10"; exit 0 ;;
  "-fsSL ` + release + `download/v0.1.10/runner-up-v0.1.10-linux-amd64.tar.gz -o "*)
    cp "$ASSET" "$4"; exit 0 ;;
  "-fsSL ` + release + `download/v0.1.10/checksums.txt -o "*)
    [ -f "$CHECKSUMS" ] || exit 22
    cp "$CHECKSUMS" "$4"; exit 0 ;;
esac
exit 42
`,
			}
			for name, script := range commands {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", dir)
			t.Setenv("TMPDIR", dir)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+installDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("ASSET", archive)
			t.Setenv("CHECKSUMS", checksums)
			output, err := testrun.Command(t, "sh", filepath.Join("docs", "install.sh")).CombinedOutput()
			installed, readErr := os.ReadFile(filepath.Join(installDir, "runner-up"))
			if tc.refusal == "" && !tc.noChecksums {
				if err != nil || readErr != nil || !bytes.Equal(installed, program) {
					t.Fatalf("matching archive not installed: %v %v %q\n%s", err, readErr, installed, output)
				}
				return
			}
			if err == nil || !os.IsNotExist(readErr) {
				t.Fatalf("refused archive installed (err %v, installed %q):\n%s", err, installed, output)
			}
			if !strings.Contains(string(output), tc.refusal) {
				t.Fatalf("missing %q in:\n%s", tc.refusal, output)
			}
		})
	}
}

// A user installing with docs/install.ps1 gets only the archive the release's
// checksums.txt lists, and the script finds the latest release through the
// releases page's redirect, never the rate-limited REST API. It runs for real
// in Windows PowerShell and PowerShell 7, whichever are present, with a
// stand-in Invoke-WebRequest serving the files in each shell's response
// shape, and stops at extraction so the test never changes the user's PATH.
func TestInstallPowerShellChecksum(t *testing.T) {
	var shells []string
	for _, s := range []string{"powershell", "pwsh"} {
		if _, err := exec.LookPath(s); err == nil {
			shells = append(shells, s)
		}
	}
	if len(shells) == 0 {
		t.Skip("no PowerShell")
	}
	const release = "https://github.com/kunchenguid/runner-up/releases/"
	other := strings.Repeat("AB", 32)
	for _, shell := range shells {
		for _, tc := range []struct {
			name, listed, effectiveURL, want string
			ps7                              bool
		}{
			{name: "matching archive", effectiveURL: release + "tag/v0.1.10", want: "REACHED EXTRACTION"},
			{name: "matching archive, PowerShell 7 response", effectiveURL: release + "tag/v0.1.10", ps7: true, want: "REACHED EXTRACTION"},
			{name: "mismatched archive", listed: other, effectiveURL: release + "tag/v0.1.10", want: "does not match checksums.txt"},
			{name: "unlisted archive", listed: "-", effectiveURL: release + "tag/v0.1.10", want: "checksums.txt does not list runner-up-v0.1.10-windows-amd64.zip"},
			{name: "no tag redirect", effectiveURL: release + "latest", want: "Failed to determine latest version"},
		} {
			t.Run(shell+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				archive, checksums, _ := releaseFixture(t, dir, "v0.1.10", "windows-amd64", tc.listed)
				log := filepath.Join(dir, "requests.log")
				shape := "@{ ResponseUri = [uri]$env:EFFECTIVE_URL }"
				if tc.ps7 {
					shape = "@{ RequestMessage = [pscustomobject]@{ RequestUri = [uri]$env:EFFECTIVE_URL } }"
				}
				stubs := `
function Invoke-WebRequest { param($Uri, $Method, $OutFile, [switch]$UseBasicParsing)
  Add-Content -Path $env:REQUEST_LOG -Value "$Method $Uri"
  switch ($Uri) {
    "` + release + `latest" { return [pscustomobject]@{ BaseResponse = [pscustomobject]` + shape + ` } }
    "` + release + `download/v0.1.10/runner-up-v0.1.10-windows-amd64.zip" { Copy-Item $env:ASSET $OutFile; return }
    "` + release + `download/v0.1.10/checksums.txt" { Copy-Item $env:CHECKSUMS $OutFile; return }
  }
  throw "refused request $Uri"
}
function Expand-Archive { throw "REACHED EXTRACTION" }
. "` + filepath.Join("docs", "install.ps1") + `"
`
				cmd := testrun.Command(t, shell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", stubs)
				// Each shell finds its own modules: a PSModulePath inherited from
				// PowerShell 7 (as on a CI runner) hides Windows PowerShell's.
				var env []string
				for _, kv := range os.Environ() {
					if !strings.HasPrefix(strings.ToUpper(kv), "PSMODULEPATH=") {
						env = append(env, kv)
					}
				}
				cmd.Env = append(env, "REQUEST_LOG="+log, "ASSET="+archive, "CHECKSUMS="+checksums,
					"EFFECTIVE_URL="+tc.effectiveURL, "PROCESSOR_ARCHITECTURE=AMD64", "LOCALAPPDATA="+dir)
				output, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(output), tc.want) {
					t.Fatalf("want failure with %q, got %v:\n%s", tc.want, err, output)
				}
				data, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "api.github.com") {
					t.Fatalf("must not use the rate-limited API: %s", data)
				}
				if _, err := os.Stat(filepath.Join(dir, "runner-up", "runner-up.exe")); !os.IsNotExist(err) {
					t.Fatalf("installed despite stopping: %v", err)
				}
			})
		}
	}
}
