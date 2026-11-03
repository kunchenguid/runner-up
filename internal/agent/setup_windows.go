package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"
)

// setupBootstrap runs the script it reads from standard input. setup.ps1 is
// too long to hand Windows PowerShell on its command line, which Windows caps
// at 32,767 characters.
const setupBootstrap = `$script = New-Object IO.MemoryStream
[Console]::OpenStandardInput().CopyTo($script)
& ([scriptblock]::Create([Text.Encoding]::UTF8.GetString($script.ToArray())))`

func runSetup(o setupOptions) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("runner-up setup changes this machine's settings: run it from a terminal opened with Run as administrator")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command("powershell.exe", encodedScript(setupBootstrap)...)
	cmd.Stdin = strings.NewReader(setupScript(self, o))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = windowsPowerShellEnv()
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return fmt.Errorf("runner-up setup could not start Windows PowerShell: %w", err)
		}
		return errors.New("runner-up setup stopped; the lines above say why. Running it again is harmless")
	}
	return nil
}

// windowsPowerShellEnv is this process's environment for Windows PowerShell.
// PowerShell 7 puts its own modules first in PSModulePath, which Windows
// PowerShell started from it would then load in place of its own and fail
// on, so setup run from PowerShell 7 drops it and Windows PowerShell uses its
// own.
func windowsPowerShellEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "PSMODULEPATH=") {
			env = append(env, kv)
		}
	}
	return env
}
