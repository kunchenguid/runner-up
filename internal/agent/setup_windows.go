package agent

import (
	"errors"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

func runSetup(o setupOptions) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("runner-up setup changes this machine's settings: run it from a terminal opened with Run as administrator")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command("powershell.exe", encodedScript(setupScript(self, o))...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return errors.New("runner-up setup stopped; the lines above say why. Running it again is harmless")
	}
	return nil
}
