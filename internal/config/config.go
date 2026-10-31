// Package config reads, checks and writes runner-up's configuration file.
package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/kunchenguid/runner-up/internal/quote"
)

// Providers make a fresh place for each job on a machine.
const (
	// ProviderDocker runs each job in a new container of a runner image,
	// removed after the job.
	ProviderDocker = "docker"
	// ProviderKVM runs each job in a new KVM virtual machine on a copy-on-write
	// disk over a base cloud image, deleted after the job.
	ProviderKVM = "kvm"
	// ProviderTart runs each job in a new tart clone of a macOS image on a Mac
	// with Apple silicon, deleted after the job.
	ProviderTart = "tart"
	// ProviderHost runs each job on the machine itself, as the agent's user, in
	// a runner folder made for the job and removed after it.
	ProviderHost = "host"
	// ProviderAccount runs each job on a Windows machine itself, as a fresh
	// standard account runner-up's Windows service makes for the job and
	// deletes after it.
	ProviderAccount = "account"
	// ProviderHyperV runs each job in a new Hyper-V Linux virtual machine on a
	// Windows machine, on a differencing disk over a base disk image, deleted
	// after the job.
	ProviderHyperV = "hyperv"
)

var providers = []string{ProviderDocker, ProviderKVM, ProviderTart, ProviderHost, ProviderAccount, ProviderHyperV}

// WindowsProviders run on Windows machines, and only there.
var WindowsProviders = []string{ProviderAccount, ProviderHyperV}

// Config is the whole setup. `runner-up init` and `runner-up machine add`
// write it; a person may edit it too. It never holds a token: the coordinator
// asks the GitHub command line for one each time it opens a session.
type Config struct {
	// StateDir is where the coordinator keeps its state, each place's lock and
	// the place logs.
	StateDir string `json:"stateDir"`
	GitHub   GitHub `json:"github"`
	// Pools are the labels jobs ask for in runs-on. Each pool gets one runner
	// scale set per repository or organization in GitHub.
	Pools    []Pool    `json:"pools"`
	Machines []Machine `json:"machines"`
	// Order is the rules that pick which waiting job a free place takes, in
	// order (internal/plan). Empty means defaultOrder.
	Order []string `json:"order,omitempty"`
	// AllowForkPullRequests lets jobs from another repository's pull request
	// run. Off by default: every runner starts with the fork guard as its
	// job-started hook (internal/agent).
	AllowForkPullRequests bool `json:"allowForkPullRequests,omitempty"`

	// AnnounceCapacity is the capacity each scale set announces with every
	// poll: GitHub assigns the set up to that many waiting jobs and tells the
	// coordinator of each. It stays above the number of places, so the
	// coordinator hears of every waiting job and picks the order, not GitHub.
	AnnounceCapacity    int `json:"announceCapacity,omitempty"`
	StartTimeoutSeconds int `json:"startTimeoutSeconds,omitempty"`
	BootTimeoutSeconds  int `json:"bootTimeoutSeconds,omitempty"`
	// GuestCheckSeconds is how often a tart place reads its clone's boot
	// session while the job runs, to end the job if the clone's macOS
	// restarted.
	GuestCheckSeconds int `json:"guestCheckSeconds,omitempty"`
	MaxJobSeconds     int `json:"maxJobSeconds,omitempty"`
	TickSeconds       int `json:"tickSeconds,omitempty"`
	LogKeepDays       int `json:"logKeepDays,omitempty"`
	// RecordMessagesDir, when set, keeps every scale set message as JSON for
	// test fixtures. Messages carry no token.
	RecordMessagesDir string `json:"recordMessagesDir,omitempty"`
}

// GitHub names what the coordinator serves. The sign-in is the GitHub command
// line's: `gh auth token` gives the coordinator its token.
type GitHub struct {
	// Repos get one scale set per pool each: owner/name. A personal account
	// has no organization-wide runners, so its repositories are listed here.
	Repos []string `json:"repos,omitempty"`
	// Orgs get one scale set per pool each, serving every repository the
	// runner group allows. The sign-in needs the admin:org scope.
	Orgs []string `json:"orgs,omitempty"`
	// RunnerGroup is the organization runner group the scale sets join;
	// "Default" when empty. Repository scale sets always use the default group.
	RunnerGroup string `json:"runnerGroup,omitempty"`
	// GH is the GitHub command line program; "gh" when empty.
	GH string `json:"gh,omitempty"`
}

// Pool is one label jobs ask for.
type Pool struct {
	Label string `json:"label"`
}

// Machine is one computer whose places run jobs. The coordinator reaches it
// over SSH, or runs its agent directly when SSH is empty.
type Machine struct {
	Name string `json:"name"`
	// SSH is the destination and any options for ssh, such as ["you@box"] or
	// ["-p", "2222", "ci@10.0.0.5"]. Empty is the coordinator's own machine.
	SSH []string `json:"ssh,omitempty"`
	// Agent is the runner-up program on the machine; "runner-up" when empty.
	Agent string `json:"agent,omitempty"`
	// OS is "windows" for a Windows machine, whose SSH server runs cmd or
	// PowerShell rather than a POSIX shell; empty for Linux and macOS.
	OS string `json:"os,omitempty"`
	// Cores and MemoryMB, when set, are what the machine's places may reserve
	// together, less the room kept for the machine's owner. A place takes a
	// job only while its share fits beside the shares of the machine's places
	// whose runners are alive. Zero counts nothing.
	Cores    int `json:"cores,omitempty"`
	MemoryMB int `json:"memoryMB,omitempty"`
	// RoomCores and RoomMemoryMB are kept for the machine's owner: places
	// reserve at most Cores-RoomCores and MemoryMB-RoomMemoryMB.
	RoomCores    int     `json:"roomCores,omitempty"`
	RoomMemoryMB int     `json:"roomMemoryMB,omitempty"`
	Places       []Place `json:"places"`
}

// Place is one slot on a machine that runs one job at a time, each in a fresh
// container or virtual machine, or on the machine itself.
type Place struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	// Pools are the labels whose jobs the place takes.
	Pools []string `json:"pools"`
	// Cores and MemoryMB are what the place's container or VM gets, and its
	// share of its machine's budget while it runs a job. A host place's job
	// is not held to them: they are only its share.
	Cores    int `json:"cores"`
	MemoryMB int `json:"memoryMB"`
	// Image is the runner image for a Docker place, the base disk image on
	// the machine for a KVM or Hyper-V place, or the tart image a tart place
	// clones, such as ghcr.io/cirruslabs/macos-sequoia-base:latest or a local
	// VM's name.
	Image string `json:"image,omitempty"`
	// PoolImages gives some of the place's pools their own image, by pool
	// label: a job of a named pool runs on that image, any other job on
	// Image. The place stays one place with one share of its machine,
	// whichever image its job uses.
	PoolImages map[string]string `json:"poolImages,omitempty"`
	// DiskGB is a KVM place's disk size, defaultDiskGB when zero; a tart
	// place's clone or a Hyper-V place's disk grows to it when its image is
	// smaller.
	DiskGB int `json:"diskGB,omitempty"`
	// Switch is the Hyper-V virtual switch a Hyper-V place's virtual machine
	// joins; defaultSwitch when empty.
	Switch string `json:"switch,omitempty"`
	// ToolCache is the runner's tool cache (RUNNER_TOOL_CACHE), where
	// actions such as setup-node keep the tools they download. A host place
	// keeps it between jobs, in its folder under the agent's when empty, so
	// a tool is downloaded once. For a tart place it is a folder inside the
	// clone, such as one the image was set up with; empty leaves the runner's
	// own, under its work folder, which every job starts without.
	ToolCache string `json:"toolCache,omitempty"`
	// WorkFolder is where the runner keeps its jobs' work, relative to the
	// runner's folder or absolute; "_work" when empty. GitHub's Ubuntu
	// runners use /home/runner/work, where a cache saved there restores.
	WorkFolder string `json:"workFolder,omitempty"`

	// Machine is the place's machine's name, filled in on load.
	Machine string `json:"-"`
}

// Share is what a place reserves of its machine while it runs.
type Share struct {
	Cores    int `json:"cores"`
	MemoryMB int `json:"memoryMB"`
}

// GitHub's own runners answer to these labels; a pool that looked like one
// would let them take a job meant for a place, or the other way round.
var hostedLabelPrefixes = []string{"ubuntu-", "windows-", "macos-"}
var reservedLabels = []string{"self-hosted", "linux", "windows", "macos", "x64", "arm64", "arm", "x86"}

const DefaultDiskGB = 40

// DefaultSwitch is the switch Windows makes with Hyper-V, which gives virtual
// machines the internet through the machine's own connection.
const DefaultSwitch = "Default Switch"

// Budget is what a machine's places may reserve together, after the room
// kept for its owner; a zero field counts nothing.
func (m Machine) Budget() Share {
	b := Share{}
	if m.Cores > 0 {
		b.Cores = m.Cores - m.RoomCores
	}
	if m.MemoryMB > 0 {
		b.MemoryMB = m.MemoryMB - m.RoomMemoryMB
	}
	return b
}

// ImageFor is the image a job of pool runs on in the place.
func (p Place) ImageFor(pool string) string {
	if image, ok := p.PoolImages[pool]; ok {
		return image
	}
	return p.Image
}

func (p Place) RunnerWorkFolder() string {
	if p.WorkFolder == "" {
		return "_work"
	}
	return p.WorkFolder
}

func (m Machine) AgentPath() string {
	if m.Agent == "" {
		return "runner-up"
	}
	return m.Agent
}

func (m Machine) Local() bool { return len(m.SSH) == 0 }

func (m Machine) Windows() bool { return m.OS == "windows" }

func DefaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		dir, err = xdg, nil
	}
	if err != nil {
		return "runner-up.json"
	}
	return filepath.Join(dir, "runner-up", "config.json")
}

// winAbs matches a full Windows path on a drive.
var winAbs = regexp.MustCompile(`^[A-Za-z]:\\[^\\]`)

// MaxPlaceID keeps a place's id short enough for the names Windows gives
// what it makes for the place.
const MaxPlaceID = 40

func DefaultStateDir() string {
	if runtime.GOOS == "windows" {
		if dir, err := os.UserCacheDir(); err == nil {
			return filepath.Join(dir, "runner-up")
		}
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "runner-up")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "runner-up")
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Defaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// SaveConfig writes c to path for a person to read and edit.
func SaveConfig(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return WriteJSON(path, c)
}

func (c *Config) Defaults() {
	if c.AnnounceCapacity == 0 {
		c.AnnounceCapacity = 50
	}
	if c.StartTimeoutSeconds == 0 {
		c.StartTimeoutSeconds = 600
	}
	if c.BootTimeoutSeconds == 0 {
		c.BootTimeoutSeconds = 300
	}
	if c.GuestCheckSeconds == 0 {
		c.GuestCheckSeconds = 30
	}
	if c.MaxJobSeconds == 0 {
		c.MaxJobSeconds = 6 * 60 * 60
	}
	if c.TickSeconds == 0 {
		c.TickSeconds = 10
	}
	if c.LogKeepDays == 0 {
		c.LogKeepDays = 30
	}
	for i := range c.Machines {
		for j := range c.Machines[i].Places {
			c.Machines[i].Places[j].Machine = c.Machines[i].Name
		}
	}
}

func (c *Config) Validate() error {
	if !filepath.IsAbs(c.StateDir) {
		return fmt.Errorf("stateDir must be absolute")
	}
	if len(c.GitHub.Repos)+len(c.GitHub.Orgs) == 0 {
		return fmt.Errorf("github needs at least one repository or organization")
	}
	seen := map[string]bool{}
	for _, r := range c.GitHub.Repos {
		owner, name, ok := strings.Cut(r, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") || seen[r] {
			return fmt.Errorf("repository %q must be owner/name, listed once", r)
		}
		seen[r] = true
	}
	for _, o := range c.GitHub.Orgs {
		if o == "" || strings.Contains(o, "/") || seen[o] {
			return fmt.Errorf("organization %q must be a plain name, listed once", o)
		}
		seen[o] = true
	}
	if len(c.Pools) == 0 {
		return fmt.Errorf("at least one pool is needed")
	}
	labels := map[string]bool{}
	for _, p := range c.Pools {
		if err := checkLabel(p.Label); err != nil {
			return err
		}
		if labels[p.Label] {
			return fmt.Errorf("pool %q is listed twice", p.Label)
		}
		labels[p.Label] = true
	}
	// A pool rule that names no pool would order nothing, and say nothing.
	for _, r := range c.Order {
		if l, ok := strings.CutPrefix(r, "pool:"); ok && !labels[l] {
			return fmt.Errorf("order rule %q names no pool of this setup", r)
		}
	}
	machines := map[string]bool{}
	ids := map[string]bool{}
	served := map[string]bool{}
	for _, m := range c.Machines {
		if m.Name == "" || machines[m.Name] || strings.ContainsAny(m.Name, "/ ") {
			return fmt.Errorf("machine names must be unique, set and plain: %q", m.Name)
		}
		machines[m.Name] = true
		if m.Cores < 0 || m.MemoryMB < 0 || m.RoomCores < 0 || m.RoomMemoryMB < 0 {
			return fmt.Errorf("machine %q: cores, memoryMB and their room must not be negative", m.Name)
		}
		if m.RoomCores > 0 && m.RoomCores >= m.Cores || m.RoomMemoryMB > 0 && m.RoomMemoryMB >= m.MemoryMB {
			return fmt.Errorf("machine %q: the room kept for its owner must leave its places some cores and memory", m.Name)
		}
		if m.OS != "" && m.OS != "windows" {
			return fmt.Errorf("machine %q: os must be windows or left out", m.Name)
		}
		if m.Windows() {
			if m.Local() {
				return fmt.Errorf("machine %q: the coordinator runs on Linux or macOS, so a Windows machine needs ssh", m.Name)
			}
			if err := quote.CheckWinWord(m.AgentPath(), true); err != nil {
				return fmt.Errorf("machine %q: agent %w", m.Name, err)
			}
		}
		b := m.Budget()
		for _, p := range m.Places {
			if p.ID == "" || ids[p.ID] || strings.ContainsAny(p.ID, "/ \t\n") || len(p.ID) > MaxPlaceID {
				return fmt.Errorf("place ids must be unique, set, plain and at most %d characters: %q", MaxPlaceID, p.ID)
			}
			ids[p.ID] = true
			if !slices.Contains(providers, p.Provider) {
				return fmt.Errorf("place %q: provider must be one of %v", p.ID, providers)
			}
			if win := slices.Contains(WindowsProviders, p.Provider); win != m.Windows() {
				if win {
					return fmt.Errorf("place %q: %s places run only on Windows machines", p.ID, p.Provider)
				}
				return fmt.Errorf("place %q: %s places do not run on Windows machines; use %v", p.ID, p.Provider, WindowsProviders)
			}
			if m.Windows() {
				// The id names the place's pipe, VM and folders on the machine.
				if strings.Trim(p.ID, quote.WinPlain) != "" || strings.ContainsAny(p.ID, `\:=+`) {
					return fmt.Errorf("place %q: a Windows machine's place id has only letters, digits, -, _ and .", p.ID)
				}
				for _, w := range []string{p.Image, p.Switch} {
					if err := quote.CheckWinWord(w, false); err != nil {
						return fmt.Errorf("place %q: %w", p.ID, err)
					}
				}
			}
			if len(p.Pools) == 0 {
				return fmt.Errorf("place %q: it serves no pool", p.ID)
			}
			for _, l := range p.Pools {
				if !labels[l] {
					return fmt.Errorf("place %q: unknown pool %q", p.ID, l)
				}
				served[l] = true
			}
			if p.Cores <= 0 || p.MemoryMB <= 0 {
				return fmt.Errorf("place %q: it needs cores and memoryMB", p.ID)
			}
			if b.Cores > 0 && p.Cores > b.Cores {
				return fmt.Errorf("place %q: its %d cores never fit its machine's %d, less the owner's room", p.ID, p.Cores, b.Cores)
			}
			if b.MemoryMB > 0 && p.MemoryMB > b.MemoryMB {
				return fmt.Errorf("place %q: its %d MB never fit its machine's %d MB, less the owner's room", p.ID, p.MemoryMB, b.MemoryMB)
			}
			if err := checkImage(p.Provider, p.Image); err != nil {
				return fmt.Errorf("place %q: %w", p.ID, err)
			}
			for _, l := range slices.Sorted(maps.Keys(p.PoolImages)) {
				image := p.PoolImages[l]
				if !slices.Contains(p.Pools, l) {
					return fmt.Errorf("place %q: poolImages names pool %q, which the place does not serve", p.ID, l)
				}
				if image == "" {
					return fmt.Errorf("place %q: poolImages gives pool %q no image", p.ID, l)
				}
				err := checkImage(p.Provider, image)
				if err == nil && m.Windows() {
					err = quote.CheckWinWord(image, false)
				}
				if err != nil {
					return fmt.Errorf("place %q: pool %q's image: %w", p.ID, l, err)
				}
			}
			if p.Switch != "" && p.Provider != ProviderHyperV {
				return fmt.Errorf("place %q: only a Hyper-V place joins a switch", p.ID)
			}
			if p.ToolCache != "" {
				if p.Provider != ProviderHost && p.Provider != ProviderTart {
					return fmt.Errorf("place %q: only a host or tart place sets its tool cache", p.ID)
				}
				if !strings.HasPrefix(p.ToolCache, "/") || strings.ContainsAny(p.ToolCache, "\n\r") {
					return fmt.Errorf("place %q: the tool cache must be an absolute path", p.ID)
				}
			}
			if strings.ContainsAny(p.WorkFolder, "\n\r") {
				return fmt.Errorf("place %q: the work folder must be one line", p.ID)
			}
		}
	}
	for _, p := range c.Pools {
		if len(c.Machines) > 0 && !served[p.Label] {
			return fmt.Errorf("pool %q: no place serves it", p.Label)
		}
	}
	return nil
}

// checkImage refuses an image a place of provider cannot run its jobs on.
func checkImage(provider, image string) error {
	switch {
	case provider == ProviderKVM && !filepath.IsAbs(image):
		return fmt.Errorf("a KVM place needs the absolute path of its base disk image on the machine")
	case provider == ProviderTart && (image == "" || strings.ContainsAny(image, " \t\n")):
		return fmt.Errorf("a tart place needs the tart image it clones, such as ghcr.io/cirruslabs/macos-sequoia-base:latest")
	case provider == ProviderHost && image != "":
		return fmt.Errorf("a host place runs on the machine itself and takes no image")
	case provider == ProviderHyperV && !winAbs.MatchString(image):
		return fmt.Errorf(`a Hyper-V place needs the full path of its base disk image on the machine, such as C:\runner-up\noble.vhdx`)
	case provider == ProviderAccount && image != "":
		return fmt.Errorf("an account place runs on the machine itself and takes no image")
	}
	return nil
}

// checkLabel refuses a pool label GitHub's own runners, or every self-hosted
// runner, would also answer to.
func checkLabel(l string) error {
	if l == "" || strings.ContainsAny(l, " ,\t\n") {
		return fmt.Errorf("pool label %q must be one plain word", l)
	}
	low := strings.ToLower(l)
	if slices.Contains(reservedLabels, low) {
		return fmt.Errorf("pool label %q is one every self-hosted runner has; pick your own", l)
	}
	for _, p := range hostedLabelPrefixes {
		if strings.HasPrefix(low, p) {
			return fmt.Errorf("pool label %q looks like GitHub's own runners (%s*); pick another", l, p)
		}
	}
	return nil
}

// Targets are the repositories and organizations, each with one scale set per
// pool.
func (c *Config) Targets() []string {
	return append(append([]string{}, c.GitHub.Repos...), c.GitHub.Orgs...)
}

func (c *Config) Labels() []string {
	out := make([]string, 0, len(c.Pools))
	for _, p := range c.Pools {
		out = append(out, p.Label)
	}
	return out
}

// Places lists every machine's places, in order.
func (c *Config) Places() []Place {
	var out []Place
	for _, m := range c.Machines {
		out = append(out, m.Places...)
	}
	return out
}

func (c *Config) Machine(name string) Machine {
	for _, m := range c.Machines {
		if m.Name == name {
			return m
		}
	}
	return Machine{Name: name}
}

func (c *Config) Place(id string) (Place, bool) {
	for _, p := range c.Places() {
		if p.ID == id {
			return p, true
		}
	}
	return Place{}, false
}

func (c *Config) Gh() string {
	if c.GitHub.GH == "" {
		return "gh"
	}
	return c.GitHub.GH
}

func (c *Config) RunnerGroup() string {
	if c.GitHub.RunnerGroup == "" {
		return "Default"
	}
	return c.GitHub.RunnerGroup
}

func (c *Config) PlaceDir(id string) string { return filepath.Join(c.StateDir, "places", id) }
func (c *Config) LogsDir() string           { return filepath.Join(c.StateDir, "logs") }

func (c *Config) Seconds(n int) time.Duration { return time.Duration(n) * time.Second }

// HasTart says whether any place runs in tart.
func (c *Config) HasTart() bool {
	for _, p := range c.Places() {
		if p.Provider == ProviderTart {
			return true
		}
	}
	return false
}
