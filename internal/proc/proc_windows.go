package proc

import (
	"errors"
	"os/exec"
)

// The coordinator and its place runners run on Linux or macOS; on Windows
// runner-up is only an agent. These keep the one program building there.

var errCoordinatorOS = errors.New("the coordinator runs on Linux or macOS; on Windows runner-up runs as a machine's agent (see runner-up setup)")

func Detach(*exec.Cmd)    {}
func Terminate(int) error { return errCoordinatorOS }
func CloseOnExec(int)     {}

func CoordinatorRuns() error { return errCoordinatorOS }
