package coordinator

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
)

// A machine whose agent cannot be reached rests. Its place runner records it
// when ssh to the machine fails, which ssh says by exiting 255: the
// connection was refused, timed out or dropped. While a machine rests its
// free places take no job, so no job is acquired and no runner registered for
// them, and the job the runner was started for, waiting again, goes to
// another place. Every reachAskEvery the coordinator asks the machine's agent
// its version, which costs no call to GitHub; once it answers, the machine's
// places take jobs again.

// sshFailed is ssh's exit code when it could not reach the machine or lost
// it. The agent itself exits 0 to 3.
const sshFailed = 255

// reachAskEvery is how often the coordinator asks a resting machine's agent.
const reachAskEvery = time.Minute

// reacher asks machine m's agent for an answer.
type reacher func(ctx context.Context, m config.Machine) error

// ReachAgent asks machine m's agent its version.
func ReachAgent(ctx context.Context, m config.Machine) error {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	argv := AgentCommand(m, "version")
	if out, err := exec.CommandContext(c, argv[0], argv[1:]...).CombinedOutput(); err != nil {
		return errors.New(agent.FirstLine(string(out), err))
	}
	return nil
}

// restLocked rests machine name from now, unless it rests already. Its agent
// is first asked after reachAskEvery.
func (s *Service) restLocked(name string) {
	if s.restingLocked(name) {
		return
	}
	now := s.now()
	s.unreachable[name] = now
	s.reachAsked[name] = now
	s.Log.Warn("the machine's agent could not be reached; its places take no jobs until it answers", "machine", name)
	s.saveLocked()
}

func (s *Service) restingLocked(name string) bool {
	_, ok := s.unreachable[name]
	return ok
}

// askRestingLocked asks each resting machine's agent, at most every
// reachAskEvery, in the background: Place never waits for an answer, and the
// first Place after one gives the machine's places jobs again. A machine
// still resting after a restart is asked at once.
func (s *Service) askRestingLocked(ctx context.Context) {
	if s.Reach == nil {
		return
	}
	now := s.now()
	for _, m := range s.Cfg.Machines {
		if !s.restingLocked(m.Name) || now.Sub(s.reachAsked[m.Name]) < reachAskEvery {
			continue
		}
		s.reachAsked[m.Name] = now
		s.asking.Add(1)
		go func() {
			defer s.asking.Done()
			err := s.Reach(ctx, m)
			s.mu.Lock()
			defer s.mu.Unlock()
			if err != nil {
				s.Log.Warn("the machine's agent still cannot be reached", "machine", m.Name, "err", err)
				return
			}
			if s.restingLocked(m.Name) {
				delete(s.unreachable, m.Name)
				s.Log.Info("the machine's agent answers again; its places take jobs", "machine", m.Name)
				s.saveLocked()
			}
		}()
	}
}
