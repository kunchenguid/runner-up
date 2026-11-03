package testrun

import (
	"os"
	"os/exec"
	"strconv"
)

// Windows has no process groups; a program's children are its process tree,
// which taskkill ends while the program still runs.
func ownGroup(*exec.Cmd) {}

// killGroup kills p and every process it started that is still its child.
func killGroup(p *os.Process) {
	exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run()
}

// endLeftovers does nothing: once p has ended Windows no longer links it to
// its children, and its process ID may already name another process.
func endLeftovers(*os.Process) {}
