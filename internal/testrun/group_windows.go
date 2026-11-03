package testrun

import (
	"os"
	"os/exec"
)

// Windows has no process groups to kill; Command ends only the program it
// started there.
func ownGroup(*exec.Cmd)    {}
func killGroup(*os.Process) {}
