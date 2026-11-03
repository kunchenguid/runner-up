package ciguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/testrun"
)

// Intent (CONTRIBUTING.md, "Before a release"): CI installs no Tart, so the
// tests against real Tart skip there and report passed without running.
// make test-real-tart is the check run before a release instead. It must run
// every test that uses the real-Tart harness and nothing else, and on a
// machine without Tart it must fail each of them rather than skip. The bug it
// catches: a new real-Tart test named outside the check's selection, or a
// check that passes on a machine with no Tart.
func TestRealTartCheckRunsEveryRealTartTestAndFailsWithoutTart(t *testing.T) {
	if testing.Short() {
		t.Skip("runs make and go as programs")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the real-Tart tests build only on Linux and macOS")
	}
	want := realTartTests(t)
	if len(want) == 0 {
		t.Fatal("no test in cmd uses realTartBorrowE2E")
	}

	// A PATH with go and make but no Tart, wherever Tart is installed.
	bin := t.TempDir()
	for _, name := range []string{"go", "make"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("no %s to run the check with", name)
		}
		if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	path := bin + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin"
	if onPath("tart", path) {
		t.Skip("Tart is installed in /usr/bin or /bin")
	}
	cmd := testrun.Command(t, "make", "test-real-tart")
	cmd.Env = append(withoutEnv(os.Environ(), "PATH", "RUNNER_UP_REAL_TART"), "PATH="+path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("make test-real-tart passed on a machine without Tart:\n%s", out)
	}
	ran := matches(`(?m)^=== RUN   (\w+)$`, out)
	failed := matches(`(?m)^--- FAIL: (\w+) `, out)
	if !slices.Equal(ran, want) {
		t.Errorf("the check ran %v, want every real-Tart test %v\n%s", ran, want, out)
	}
	if !slices.Equal(failed, want) {
		t.Errorf("without Tart the check failed %v, want %v\n%s", failed, want, out)
	}
	if n := len(regexp.MustCompile(`Tart is not installed`).FindAll(out, -1)); n != len(want) {
		t.Errorf("%d tests said Tart is not installed, want %d\n%s", n, len(want), out)
	}
}

// realTartTests names, sorted, the cmd tests that call realTartBorrowE2E.
func realTartTests(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("cmd", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	fset := token.NewFileSet()
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "realTartBorrowE2E" {
						names = append(names, fn.Name.Name)
						return false
					}
				}
				return true
			})
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func matches(pattern string, out []byte) []string {
	var names []string
	for _, m := range regexp.MustCompile(pattern).FindAllSubmatch(out, -1) {
		names = append(names, string(m[1]))
	}
	slices.Sort(names)
	return names
}

func withoutEnv(env []string, names ...string) []string {
	var kept []string
	for _, kv := range env {
		drop := false
		for _, name := range names {
			if strings.HasPrefix(kv, name+"=") {
				drop = true
			}
		}
		if !drop {
			kept = append(kept, kv)
		}
	}
	return kept
}

func onPath(name, path string) bool {
	for _, dir := range filepath.SplitList(path) {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}
