// Package testbin builds runner-up for the tests that run it as a program: as
// a coordinator, a place runner, an agent or a runner's hook.
package testbin

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	mu     sync.Mutex
	dir    string
	builds = map[string]string{}
)

// Build builds runner-up once per set of linker flags, such as
// "-X github.com/kunchenguid/runner-up/internal/agent.runnerArchive=FILE", and
// returns the program's path.
func Build(t *testing.T, ldflags string) string {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if path, ok := builds[ldflags]; ok {
		return path
	}
	if dir == "" {
		d, err := os.MkdirTemp("", "runner-up-test-")
		if err != nil {
			t.Fatal(err)
		}
		dir = d
	}
	path := filepath.Join(dir, "runner-up-"+string(rune('a'+len(builds))))
	args := []string{"build", "-o", path}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	out, err := exec.Command("go", append(args, "github.com/kunchenguid/runner-up")...).CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %s", out)
	}
	builds[ldflags] = path
	return path
}

// Dir is a folder for files the tests' builds read, removed with them.
func Dir(t *testing.T) string {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if dir == "" {
		d, err := os.MkdirTemp("", "runner-up-test-")
		if err != nil {
			t.Fatal(err)
		}
		dir = d
	}
	return dir
}

// Main runs a package's tests and removes what Build made.
func Main(m *testing.M) {
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}
