package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
)

// Apple's Virtualization framework runs at most two macOS virtual machines at
// once on a Mac (macOSVMLimit). A VM runner-up did not start, such as one its
// owner runs by hand, takes one of those two as much as a place's clone does,
// and tart then refuses the next clone. So a free tart place counts as busy
// while its Mac already runs as many macOS VMs as the framework allows: its
// other tart places whose runners are alive, each of which runs or is about
// to run its clone, and the macOS VMs the Mac's agent reports that are no
// clone of those places.

// vmReader names the macOS VMs running on machine m right now.
type vmReader func(ctx context.Context, m config.Machine) ([]string, error)

// vmReadEvery is how often the coordinator asks a Mac with a free tart place
// which macOS VMs it runs. A VM started since is met by the agent, which
// waits for a free one before it clones.
const vmReadEvery = 30 * time.Second

// ReadVMs asks machine m's agent which macOS VMs it runs.
func ReadVMs(ctx context.Context, m config.Machine) ([]string, error) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	argv := AgentCommand(m, "agent", "vms")
	out, err := exec.CommandContext(c, argv[0], argv[1:]...).Output()
	if err != nil {
		return nil, fmt.Errorf("%v %s", err, agent.FirstLine(StderrOf(err), nil))
	}
	var vms []string
	if err := json.Unmarshal(out, &vms); err != nil {
		return nil, fmt.Errorf("unreadable answer: %w", err)
	}
	return vms, nil
}

// refreshVMs reads the running macOS VMs of every Mac with a free tart place,
// at most every vmReadEvery each, outside the queue lock. A Mac whose reading
// fails counts no VM beyond its places' own until a reading succeeds.
func (s *Service) refreshVMs(ctx context.Context) {
	if s.VmReader == nil {
		return
	}
	s.mu.Lock()
	now := s.now()
	var due []config.Machine
	for _, m := range s.Cfg.Machines {
		if now.Sub(s.vmsRead[m.Name]) < vmReadEvery {
			continue
		}
		for _, p := range m.Places {
			if p.Provider == config.ProviderTart && !s.runnerAlive(p.ID) {
				due = append(due, m)
				break
			}
		}
	}
	s.mu.Unlock()
	for _, m := range due {
		vms, err := s.VmReader(ctx, m)
		s.mu.Lock()
		s.vmsRead[m.Name] = now
		if err != nil {
			delete(s.vms, m.Name)
			s.Log.Warn("could not read the Mac's running macOS VMs; counting only its places' clones", "machine", m.Name, "err", err)
		} else {
			s.vms[m.Name] = vms
		}
		s.mu.Unlock()
	}
}

// vmLimitFull says why tart place p cannot start a clone now, or "" when it
// can.
func (s *Service) vmLimitFull(p config.Place) string {
	if p.Provider != config.ProviderTart {
		return ""
	}
	own := map[string]bool{agent.CloneName(p.ID): true}
	clones := 0
	for _, q := range s.Cfg.Machine(p.Machine).Places {
		if q.Provider != config.ProviderTart || q.ID == p.ID || !s.runnerAlive(q.ID) {
			continue
		}
		own[agent.CloneName(q.ID)] = true
		clones++
	}
	var others []string
	for _, vm := range s.vms[p.Machine] {
		if !own[vm] {
			others = append(others, vm)
		}
	}
	if clones+len(others) < agent.MacOSVMLimit {
		return ""
	}
	why := fmt.Sprintf("macOS VM limit: %d of %d running", clones+len(others), agent.MacOSVMLimit)
	if len(others) > 0 {
		why += ", outside runner-up: " + strings.Join(others, ", ")
	}
	return why
}

func StderrOf(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(exit.Stderr)
	}
	return ""
}
