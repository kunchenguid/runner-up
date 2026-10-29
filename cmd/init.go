package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
)

// listFlag gathers a flag given more than once.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// initMain is `runner-up init`: it checks the GitHub command line's sign-in
// can serve what is asked and writes a setup with no machines yet.
func initMain(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fs.String("config", config.DefaultConfigPath(), "setup file to write")
	var repos, orgs listFlag
	fs.Var(&repos, "repo", "a repository to serve, owner/name; repeat for more")
	fs.Var(&orgs, "org", "an organization to serve; repeat for more")
	pool := fs.String("pool", "runner-up", "the label jobs ask for in runs-on")
	stateDir := fs.String("state-dir", config.DefaultStateDir(), "where the coordinator keeps its state")
	group := fs.String("runner-group", "", "the organization runner group; Default when empty")
	gh := fs.String("gh", "gh", "the GitHub command line program")
	force := fs.Bool("force", false, "replace an existing setup")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if _, err := os.Stat(*configPath); err == nil && !*force {
		fmt.Fprintf(os.Stderr, "%s already exists; add machines with runner-up machine add, or pass --force to start over\n", *configPath)
		return 2
	}
	cfg := &config.Config{StateDir: *stateDir, Pools: []config.Pool{{Label: *pool}},
		GitHub: config.GitHub{Repos: repos, Orgs: orgs, RunnerGroup: *group}}
	if *gh != "gh" {
		cfg.GitHub.GH = *gh
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := initSetup(ctx, cfg, coordinator.NewGitHubAPI(coordinator.GhToken(cfg.Gh())), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := config.SaveConfig(*configPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("wrote %s\nnext: runner-up machine add NAME --ssh user@host\n", *configPath)
	return 0
}

// initSetup checks the setup and that the sign-in may create runners for
// every repository and organization in it.
func initSetup(ctx context.Context, cfg *config.Config, api *coordinator.GithubAPI, out io.Writer) error {
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	login, scopes, err := api.SignIn(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed in to GitHub as %s through the GitHub command line\n", login)
	for _, r := range cfg.GitHub.Repos {
		admin, err := api.RepoAdmin(ctx, r)
		if err != nil {
			return fmt.Errorf("%s: %w", r, err)
		}
		if !admin {
			return fmt.Errorf("%s: %s is not an admin of it, which creating its runners needs", r, login)
		}
		fmt.Fprintf(out, "%s: ok\n", r)
	}
	// A classic token lists its scopes; a token with none listed is a
	// fine-grained one, whose permissions show only when used.
	if len(cfg.GitHub.Orgs) > 0 && len(scopes) > 0 && !slices.Contains(scopes, "admin:org") {
		return fmt.Errorf("organization runners need the admin:org scope: run gh auth refresh -h github.com -s admin:org")
	}
	for _, o := range cfg.GitHub.Orgs {
		fmt.Fprintf(out, "%s: ok\n", o)
	}
	return nil
}

// machineAddMain is `runner-up machine add`: it reaches the machine over SSH,
// installs the agent there when it is missing, reads the machine's cores,
// memory and providers, and adds it with as many places as its budget fits.
func machineAddMain(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: runner-up machine add NAME --ssh DESTINATION [flags]")
		return 2
	}
	name := args[0]
	fs := flag.NewFlagSet("machine add", flag.ContinueOnError)
	configPath := fs.String("config", config.DefaultConfigPath(), "setup file")
	sshDest := fs.String("ssh", "", `the machine for ssh, with any options, such as "ci@box" or "-p 2222 ci@10.0.0.5"; empty is this machine`)
	agent := fs.String("agent", "", "runner-up's path on the machine; runner-up on its PATH by default")
	install := fs.String("install", "", "a runner-up build for the machine to copy there when it has none; this program by default when the machine matches it")
	provider := fs.String("provider", "", "docker or kvm on Linux, tart or host on a Mac, account or hyperv on Windows; docker, tart or account by default")
	image := fs.String("image", "", "the runner image (docker), the base cloud image's path on the machine (kvm, hyperv), or the tart image to clone (tart)")
	hvSwitch := fs.String("switch", "", "the Hyper-V switch a hyperv place joins; the Default Switch by default")
	places := fs.Int("places", 0, "how many places; as many as the budget fits by default, at most 2 tart places and 1 host place")
	cores := fs.Int("place-cores", 0, "cores each place gets; 4 for tart, 2 otherwise")
	memory := fs.Int("place-memory", 0, "memory each place gets, in MB; 8192 for tart, 4096 otherwise")
	diskGB := fs.Int("disk-gb", 0, "a KVM or Hyper-V place's disk size in GB, or the size a tart clone grows to")
	roomCores := fs.Int("room-cores", 1, "cores kept for the machine's owner")
	roomMemory := fs.Int("room-memory", 1024, "memory kept for the machine's owner, in MB")
	var pools listFlag
	fs.Var(&pools, "pool", "a pool the places serve; every pool by default")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	m := config.Machine{Name: name, SSH: strings.Fields(*sshDest), Agent: *agent}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if !m.Local() && remoteWindows(ctx, m) {
		m.OS = "windows"
	}
	if err := ensureAgent(ctx, &m, *install, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	pr, err := probeMachine(ctx, m)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if pr.OS == "windows" {
		fmt.Printf("%s: %s/%s, %d cores, %d MB, account: %s, hyperv: %s\n", name, pr.OS, pr.Arch, pr.Cores, pr.MemoryMB, pr.Account, pr.HyperV)
	} else {
		fmt.Printf("%s: %s/%s, %d cores, %d MB, docker: %s, kvm: %s, tart: %s, host: %s\n",
			name, pr.OS, pr.Arch, pr.Cores, pr.MemoryMB, pr.Docker, pr.KVM, pr.Tart, pr.Host)
	}
	opts := placeOptions{Provider: *provider, Image: *image, Switch: *hvSwitch, Places: *places, Cores: *cores, MemoryMB: *memory,
		DiskGB: *diskGB, RoomCores: *roomCores, RoomMemoryMB: *roomMemory, Pools: pools}
	if err := addMachine(cfg, m, pr, opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := config.SaveConfig(*configPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(machineLine(cfg.Machine(name)))
	fmt.Printf("wrote %s; (re)start runner-up serve to use it\n", *configPath)
	return 0
}

type placeOptions struct {
	Provider                string
	Image                   string
	Switch                  string
	Places                  int
	Cores, MemoryMB         int
	DiskGB                  int
	RoomCores, RoomMemoryMB int
	Pools                   []string
}

// addMachine adds machine m, as probed, to cfg with its places.
func addMachine(cfg *config.Config, m config.Machine, pr agent.Probe, o placeOptions) error {
	if slices.ContainsFunc(cfg.Machines, func(x config.Machine) bool { return x.Name == m.Name }) {
		return fmt.Errorf("machine %q is already in the setup", m.Name)
	}
	switch pr.OS {
	case "linux":
		if o.Provider == "" {
			o.Provider = config.ProviderDocker
		}
	case "darwin":
		if o.Provider == "" {
			o.Provider = config.ProviderTart
		}
		if o.Provider == config.ProviderDocker || o.Provider == config.ProviderKVM {
			return fmt.Errorf("%s runs %s; %s places run on Linux machines only: use tart or host", m.Name, pr.OS, o.Provider)
		}
	case "windows":
		m.OS = "windows"
		if o.Provider == "" {
			o.Provider = config.ProviderAccount
		}
	default:
		return fmt.Errorf("%s runs %s; this version runs places on Linux, macOS and Windows only", m.Name, pr.OS)
	}
	if slices.Contains(config.WindowsProviders, o.Provider) && pr.OS != "windows" {
		return fmt.Errorf("%s runs %s; %s places run on Windows machines only", m.Name, pr.OS, o.Provider)
	}
	if o.Cores == 0 {
		o.Cores = 2
		if o.Provider == config.ProviderTart {
			o.Cores = 4
		}
	}
	if o.MemoryMB == 0 {
		o.MemoryMB = 4096
		if o.Provider == config.ProviderTart {
			o.MemoryMB = 8192
		}
	}
	switch o.Provider {
	case config.ProviderTart:
		if pr.Tart != "ok" {
			return fmt.Errorf("%s cannot run tart places: %s", m.Name, pr.Tart)
		}
		if o.Image == "" {
			return fmt.Errorf("a tart place needs --image, a macOS tart image pulled on %s, such as ghcr.io/cirruslabs/macos-sequoia-base:latest", m.Name)
		}
	case config.ProviderHost:
		if pr.Host != "ok" {
			return fmt.Errorf("%s cannot run host places: %s", m.Name, pr.Host)
		}
	case config.ProviderAccount:
		if pr.Account != "ok" {
			return fmt.Errorf("%s cannot run account places: %s", m.Name, pr.Account)
		}
	case config.ProviderHyperV:
		if pr.HyperV != "ok" {
			return fmt.Errorf("%s cannot run Hyper-V places: %s", m.Name, pr.HyperV)
		}
		if o.Image == "" {
			return fmt.Errorf(`a Hyper-V place needs --image, the full path of a VHDX base disk on %s, such as C:\runner-up\noble.vhdx`, m.Name)
		}
	case config.ProviderKVM:
		if pr.KVM != "ok" {
			return fmt.Errorf("%s cannot run KVM places: %s", m.Name, pr.KVM)
		}
		if o.Image == "" {
			return fmt.Errorf("a KVM place needs --image, the path of a cloud image on %s, such as Ubuntu's noble-server-cloudimg", m.Name)
		}
	case config.ProviderDocker:
		if pr.Docker != "ok" {
			return fmt.Errorf("%s cannot run Docker places: %s", m.Name, pr.Docker)
		}
	}
	m.Cores, m.MemoryMB = pr.Cores, pr.MemoryMB
	m.RoomCores, m.RoomMemoryMB = min(o.RoomCores, max(m.Cores-1, 0)), min(o.RoomMemoryMB, max(m.MemoryMB-1, 0))
	b := m.Budget()
	n := o.Places
	if n == 0 {
		n = b.Cores / o.Cores
		if b.MemoryMB > 0 {
			n = min(n, b.MemoryMB/o.MemoryMB)
		}
		if n == 0 {
			return fmt.Errorf("%s has %d cores and %d MB for places, less than one place of %d cores and %d MB",
				m.Name, b.Cores, b.MemoryMB, o.Cores, o.MemoryMB)
		}
		// More tart places than macOS runs VMs would only take turns, and
		// host places share one user's home folder.
		switch o.Provider {
		case config.ProviderTart:
			n = min(n, agent.MacOSVMLimit)
		case config.ProviderHost:
			n = 1
		}
	}
	pools := o.Pools
	if len(pools) == 0 {
		pools = cfg.Labels()
	}
	for i := 1; i <= n; i++ {
		m.Places = append(m.Places, config.Place{ID: m.Name + "-" + strconv.Itoa(i), Provider: o.Provider, Pools: pools,
			Cores: o.Cores, MemoryMB: o.MemoryMB, Image: o.Image, DiskGB: o.DiskGB, Switch: o.Switch})
	}
	cfg.Machines = append(cfg.Machines, m)
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		cfg.Machines = cfg.Machines[:len(cfg.Machines)-1]
		return err
	}
	return nil
}

// installedAgent is where machine add installs runner-up on a machine,
// relative to the home folder ssh starts commands in.
const installedAgent = ".local/bin/runner-up"

// ensureAgent checks runner-up runs on the machine, and installs it in
// ~/.local/bin when it does not: from build, or from this program when the
// machine's system and architecture match it.
func ensureAgent(ctx context.Context, m *config.Machine, build string, out io.Writer) error {
	if m.Windows() {
		argv := coordinator.AgentCommand(*m, "version")
		got, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
		if err != nil {
			return fmt.Errorf("runner-up is not set up on %s, a Windows machine: on it, run runner-up setup as an administrator (see the README), then add it again", m.Name)
		}
		fmt.Fprintf(out, "%s: runner-up %s\n", m.Name, strings.TrimSpace(string(got)))
		return nil
	}
	tries := []string{m.Agent}
	if m.Agent == "" && !m.Local() {
		// An earlier machine add installed it where ssh's shell may not look.
		tries = append(tries, installedAgent)
	}
	var err error
	for _, agent := range tries {
		try := *m
		try.Agent = agent
		argv := coordinator.AgentCommand(try, "version")
		var got []byte
		if got, err = exec.CommandContext(ctx, argv[0], argv[1:]...).Output(); err == nil {
			m.Agent = agent
			fmt.Fprintf(out, "%s: runner-up %s\n", m.Name, strings.TrimSpace(string(got)))
			return nil
		}
		// 127 is the shell's "not found"; anything else is a real failure.
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 127 {
			break
		}
	}
	if m.Local() {
		return fmt.Errorf("runner-up is not on this machine's PATH as %s: pass --agent", m.AgentPath())
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 127 {
		return fmt.Errorf("could not run runner-up on %s over ssh: %v %s", m.Name, err, agent.FirstLine(coordinator.StderrOf(err), nil))
	}
	if build == "" {
		uname := append(coordinator.SshArgs(*m), "uname -sm")
		u, err := exec.CommandContext(ctx, uname[0], uname[1:]...).Output()
		if err != nil {
			return fmt.Errorf("could not read %s's system: %v", m.Name, err)
		}
		if want := unameOf(runtime.GOOS, runtime.GOARCH); strings.TrimSpace(string(u)) != want {
			return fmt.Errorf("runner-up is not on %s, which runs %s and this program is built for %s: build one for it and pass --install FILE",
				m.Name, strings.TrimSpace(string(u)), want)
		}
		if build, err = os.Executable(); err != nil {
			return err
		}
	}
	f, err := os.Open(build)
	if err != nil {
		return err
	}
	defer f.Close()
	cp := append(coordinator.SshArgs(*m), "mkdir -p .local/bin && cat > "+installedAgent+".new && chmod 0755 "+installedAgent+".new && mv "+installedAgent+".new "+installedAgent)
	cmd := exec.CommandContext(ctx, cp[0], cp[1:]...)
	cmd.Stdin = f
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("could not copy runner-up to %s: %s", m.Name, agent.FirstLine(string(b), err))
	}
	m.Agent = installedAgent
	fmt.Fprintf(out, "%s: installed runner-up in ~/.local/bin\n", m.Name)
	return nil
}

// remoteWindows says whether the machine m reaches over SSH runs Windows,
// whose sign-in runs cmd or PowerShell, either of which runs cmd /c ver.
func remoteWindows(ctx context.Context, m config.Machine) bool {
	argv := append(coordinator.SshArgs(m), "cmd /c ver")
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	return err == nil && strings.Contains(string(out), "Microsoft Windows")
}

// unameOf is what uname -sm prints on goos/goarch: Linux calls arm64
// aarch64, and macOS calls it arm64.
func unameOf(goos, goarch string) string {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[goarch]
	if goos == "darwin" && goarch == "arm64" {
		arch = "arm64"
	}
	if arch == "" {
		arch = goarch
	}
	return map[string]string{"linux": "Linux", "darwin": "Darwin"}[goos] + " " + arch
}

func probeMachine(ctx context.Context, m config.Machine) (agent.Probe, error) {
	argv := coordinator.AgentCommand(m, "agent", "probe")
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	var pr agent.Probe
	if err != nil {
		return pr, fmt.Errorf("could not probe %s: %v %s", m.Name, err, agent.FirstLine(coordinator.StderrOf(err), nil))
	}
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&pr); err != nil {
		return pr, fmt.Errorf("%s answered the probe with something unreadable", m.Name)
	}
	return pr, nil
}
