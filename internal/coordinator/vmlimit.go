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
// other tart places whose runners or borrow supervisors are alive, each of
// which reserves its named clone, and the macOS VMs the Mac's agent reports
// that are no clone of those places.

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

// vmReadBudget is how long one placement pass waits for the Macs it reads.
// A Mac that has not answered by then counts as a failed reading.
var vmReadBudget = 30 * time.Second

// refreshVMs reads the running macOS VMs of every Mac with a free tart place,
// at most every vmReadEvery each, outside the queue lock. It reads the Macs
// at once, within vmReadBudget for them all, so one Mac slow to answer never
// delays placement by more than that. A Mac whose reading fails counts no VM
// beyond its places' own until a reading succeeds. A resting Mac is not read:
// its places take no job (reach.go).
func (s *Service) refreshVMs(ctx context.Context) {
	if s.VmReader == nil {
		return
	}
	s.mu.Lock()
	now := s.now()
	var due []config.Machine
	for _, m := range s.Cfg.Machines {
		if now.Sub(s.vmsRead[m.Name]) < vmReadEvery || s.restingLocked(m.Name) {
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
	if len(due) == 0 {
		return
	}
	got := readAtOnce(ctx, due, vmReadBudget, s.VmReader)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range due {
		s.vmsRead[m.Name] = now
		if r := got[m.Name]; r.err != nil {
			delete(s.vms, m.Name)
			s.Log.Warn("could not read the Mac's running macOS VMs; counting only its places' clones", "machine", m.Name, "err", r.err)
		} else {
			s.vms[m.Name] = r.value
		}
	}
}

// machineReading is one machine's answer to readAtOnce.
type machineReading[T any] struct {
	value T
	err   error
}

// readAtOnce asks every machine in due at once, within budget for them all,
// so one machine slow to answer never delays placement by more than that. A
// machine that has not answered by then has an error. It is called with mu
// let go.
func readAtOnce[T any](ctx context.Context, due []config.Machine, budget time.Duration,
	read func(context.Context, config.Machine) (T, error)) map[string]machineReading[T] {
	type answer struct {
		machine string
		machineReading[T]
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// Buffered for every reader, so one that answers after the budget never
	// blocks.
	answers := make(chan answer, len(due))
	for _, m := range due {
		go func() {
			v, err := read(ctx, m)
			answers <- answer{m.Name, machineReading[T]{v, err}}
		}()
	}
	got := map[string]machineReading[T]{}
	for len(got) < len(due) && ctx.Err() == nil {
		select {
		case a := <-answers:
			got[a.machine] = a.machineReading
		case <-ctx.Done():
		}
	}
	for _, m := range due {
		if _, ok := got[m.Name]; !ok {
			got[m.Name] = machineReading[T]{err: fmt.Errorf("no answer within %s", budget)}
		}
	}
	return got
}

// vmLimitFull says why tart place p cannot start a clone now, or "" when it
// can.
func (v *snapshot) vmLimitFull(p config.Place) string {
	s := v.s
	if p.Provider != config.ProviderTart {
		return ""
	}
	own := map[string]bool{agent.CloneName(p.ID): true}
	clones := 0
	for _, q := range s.Cfg.Machine(p.Machine).Places {
		if q.Provider != config.ProviderTart || q.ID == p.ID || !v.place(q.ID).alive {
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
