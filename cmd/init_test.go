package cmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (the design: "init then machine add over SSH runs a job with no
// hand-written setup file"), and the bug a live run found:
// adding a machine a second time, to another setup, failed. machine add had
// installed runner-up in ~/.local/bin, which a command run over ssh does not
// search, and then refused to install it again from a build for another
// system.

// The stand-in ssh runs its command, the last argument, in $HOME, as ssh does
// on the machine.
const sshInHome = `#!/bin/sh
for last; do :; done
cd "$HOME" && exec sh -c "$last"
`

// standInSSH puts the stand-in ssh first on PATH, under home. It is a shell
// script reaching a Unix machine, so it cannot run on Windows.
func standInSSH(t *testing.T, home, bin string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in ssh is a shell script")
	}
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(sshInHome), 0o755)
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
}

func TestMachineAddFindsTheAgentAnEarlierMachineAddInstalled(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o700)
	os.MkdirAll(bin, 0o700)
	installed := filepath.Join(home, installedAgent)
	if err := os.Symlink(runnerUp(t), installed); err != nil {
		t.Fatal(err)
	}
	standInSSH(t, home, bin)

	m := config.Machine{Name: "lima", SSH: []string{"ci@lima"}}
	var out strings.Builder
	if err := ensureAgent(context.Background(), &m, "", &out); err != nil {
		t.Fatalf("machine add did not find the installed runner-up: %v", err)
	}
	if m.Agent != installedAgent || out.String() != "lima: runner-up dev\n" {
		t.Errorf("machine add chose agent %q and said %q", m.Agent, out.String())
	}
	if target, err := os.Readlink(installed); err != nil || target != runnerUp(t) {
		t.Errorf("machine add replaced the installed runner-up: %q %v", target, err)
	}
}

// Intent (the design: "On a clean Mac ... init then join runs a test
// repository's job"), and the bug building it found: machine add
// compared a Mac's uname, which says arm64, with Linux's name for it,
// aarch64, and refused to install itself on a Mac like the one it ran on.
func TestMachineAddInstallsItselfOnAMachineLikeItsOwn(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(home, 0o700)
	os.MkdirAll(bin, 0o700)
	standInSSH(t, home, bin)

	// The machine is this computer, reached over the stand-in ssh, with no
	// runner-up yet: its real uname answers.
	m := config.Machine{Name: "mac", SSH: []string{"you@mac"}}
	var out strings.Builder
	if err := ensureAgent(context.Background(), &m, "", &out); err != nil {
		t.Fatalf("machine add did not install itself on a machine like its own: %v", err)
	}
	if m.Agent != installedAgent || out.String() != "mac: installed runner-up in ~/.local/bin\n" {
		t.Errorf("machine add chose agent %q and said %q", m.Agent, out.String())
	}
	if info, err := os.Stat(filepath.Join(home, installedAgent)); err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("no runnable runner-up was installed: %v", err)
	}
}
