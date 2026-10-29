package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// Every place runs this version of GitHub's runner. Its scale sets turn
// updates off, so a newer runner is a change here.
const runnerVersion = "2.338.1"

// runnerSHA256 are the published SHA-256 sums of the runner's Linux and macOS
// release archives, from its release page.
var runnerSHA256 = map[string]string{
	"linux-x64":   "08e9ccb3604d2462c24b5e33604dc30810296ca15cc65f23cffd4317eb144c37",
	"linux-arm64": "475e0c149fd3b611610127b9664ba5bacf5c6928795cc448ab520bb5f324bede",
	"osx-x64":     "f06e329ffe90a2a4bda8b460a20b9e26dcb7525a093bf9b4f89125c72bc1f39e",
	"osx-arm64":   "52c3fb801b931fbd454979bae5ec3443ea3da2c4a1b4c44fa546fb630d61a0a9",
}

// runnerArchive, when set at build time (-ldflags -X
// github.com/kunchenguid/runner-up/internal/agent.runnerArchive=FILE), is the
// runner archive every place uses instead of the pinned release. Only the
// tests set it, to a stand-in runner.
var runnerArchive string

// runnerWindowsSHA256 are the same for its Windows release archives, by
// architecture.
var runnerWindowsSHA256 = map[string]string{
	"x64":   "478f533ac2c349040fbaea7e158f7f7d0235085cf35bc81f661af0c715207ff9",
	"arm64": "424fa096d48a7e082f13025fc5c01048520e39164e720a27130490f2dd86701d",
}

// runnerArch is the runner's name for this machine's architecture.
func runnerArch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}

// runnerPlatform names this machine's runner release, such as osx-arm64. A
// tart clone runs the same system and architecture as its Mac.
func runnerPlatform() string {
	system := map[string]string{"darwin": "osx"}[runtime.GOOS]
	if system == "" {
		system = runtime.GOOS
	}
	return system + "-" + runnerArch()
}

// runnerReady says "ok" when a runner release exists for this machine, or
// why not.
func runnerReady() string {
	if _, ok := runnerSHA256[runnerPlatform()]; !ok && runnerArchive == "" {
		return "no runner release for " + runtime.GOOS + "/" + runtime.GOARCH
	}
	return "ok"
}

// runnerTarball returns the runner's release archive for this machine, kept
// in cacheDir after the first download and checked against its published sum
// on every use.
func runnerTarball(ctx context.Context, cacheDir string) (string, error) {
	if runnerArchive != "" {
		return runnerArchive, nil
	}
	platform := runnerPlatform()
	sum, ok := runnerSHA256[platform]
	if !ok {
		return "", fmt.Errorf("no runner release for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return runnerDownload(ctx, cacheDir, fmt.Sprintf("actions-runner-%s-%s.tar.gz", platform, runnerVersion), sum)
}

// runnerZip is the same for the runner's Windows release archive.
func runnerZip(ctx context.Context, cacheDir string) (string, error) {
	arch := runnerArch()
	sum, ok := runnerWindowsSHA256[arch]
	if !ok {
		return "", fmt.Errorf("no Windows runner release for %s", runtime.GOARCH)
	}
	return runnerDownload(ctx, cacheDir, fmt.Sprintf("actions-runner-win-%s-%s.zip", arch, runnerVersion), sum)
}

// runnerURL is where GitHub publishes the runner's release archive name.
func runnerURL(name string) string {
	return fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", runnerVersion, name)
}

// runnerTarballName is the runner's Linux release archive for arch.
func runnerTarballName(arch string) string {
	return fmt.Sprintf("actions-runner-linux-%s-%s.tar.gz", arch, runnerVersion)
}

// runnerDownload returns the runner release archive name, kept in cacheDir
// and checked against its published sum on every use.
func runnerDownload(ctx context.Context, cacheDir, name, sum string) (string, error) {
	path := filepath.Join(cacheDir, name)
	if got, err := fileSHA256(path); err == nil && got == sum {
		return path, nil
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runnerURL(name), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", name, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(cacheDir, ".runner-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return "", fmt.Errorf("download %s: its SHA-256 is %s, not the published %s", name, got, sum)
	}
	return path, os.Rename(tmp.Name(), path)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
