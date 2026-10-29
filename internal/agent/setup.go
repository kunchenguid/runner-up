package agent

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"strings"
)

//go:embed setup.ps1
var setupLibrary string

type setupOptions struct {
	User   string
	Key    string
	SSH    bool
	HyperV bool
}

// setupScript is setup.ps1 after the values it reads.
func setupScript(source string, o setupOptions) string {
	return fmt.Sprintf("$source = %s\n$user = %s\n$key = %s\n$ssh = $%t\n$hyperv = $%t\n",
		psQuote(source), psQuote(o.User), psQuote(o.Key), o.SSH, o.HyperV) + setupLibrary
}

// SetupMain is `runner-up setup`, run once on a Windows machine as an
// administrator.
func SetupMain(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	o := setupOptions{}
	fs.StringVar(&o.User, "user", os.Getenv("USERNAME"), "the account the coordinator signs in as over SSH")
	keyFile := fs.String("ssh-key", "", "a public key file (such as the coordinator's id_ed25519.pub) to let that account sign in with")
	fs.BoolVar(&o.SSH, "ssh", false, "turn on Windows' OpenSSH server")
	fs.BoolVar(&o.HyperV, "hyperv", false, "turn on Hyper-V and let the account make VMs, for Hyper-V places")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.User == "" {
		fmt.Fprintln(os.Stderr, "name the account the coordinator signs in as with --user")
		return 2
	}
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		o.Key = strings.TrimSpace(string(b))
		if strings.ContainsAny(o.Key, "\r\n") || !strings.HasPrefix(o.Key, "ssh-") && !strings.HasPrefix(o.Key, "ecdsa-") {
			fmt.Fprintf(os.Stderr, "%s is not one public key\n", *keyFile)
			return 2
		}
	}
	if err := runSetup(o); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
