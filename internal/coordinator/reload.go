package coordinator

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// A setup change applies in place (setup.go). Running jobs run on: each place
// runner runs as the setup it started with, and its job's reservation, frozen
// in place.json at launch, keeps counting against its machine. A place the
// change removes, or moves to another machine, while its runner runs is
// departed: it takes no job, it counts against the machine its runner runs on,
// and Tidy looks after its runner as before, until the runner ends. Waiting
// jobs keep their place in line. A job of a pool the change removes waits
// with no session to place it, until the pool comes back or the job expires.

// setupUser is a Launcher that starts place runners with the setup the
// coordinator runs (ProcLauncher.UseSetup).
type setupUser interface {
	UseSetup(cfg *config.Config) error
}

// prepare makes the folders cfg's places and logs need.
func (s *Service) prepare(cfg *config.Config) error {
	for _, p := range cfg.Places() {
		if err := os.MkdirAll(cfg.PlaceDir(p.ID), 0o700); err != nil {
			return err
		}
	}
	return os.MkdirAll(cfg.LogsDir(), 0o700)
}

// useSetup starts the place runners launched from now on with cfg.
func (s *Service) useSetup(cfg *config.Config) error {
	if u, ok := s.launcher.(setupUser); ok {
		return u.UseSetup(cfg)
	}
	return nil
}

// placesLocked is every place the coordinator looks after: the setup's, and
// the departed places the setup no longer has.
func (s *Service) placesLocked() []config.Place {
	places := s.Cfg.Places()
	for _, id := range slices.Sorted(maps.Keys(s.departed)) {
		if _, ok := s.Cfg.Place(id); !ok {
			places = append(places, s.departed[id])
		}
	}
	return places
}

// Reload replaces the setup the coordinator runs with next, checked as
// check-config checks it, and reports whether anything changed. Nothing
// running stops: the caller opens and closes scale set sessions to match
// (Sessions.Match). A setup that moves the state folder is refused, and the
// running setup stays, since the running places hold their locks there.
func (s *Service) Reload(next *config.Config, rules []plan.Rule) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.Cfg
	if reflect.DeepEqual(old, next) {
		return false, nil
	}
	if next.StateDir != old.StateDir {
		return false, fmt.Errorf("stateDir changes only when runner-up serve restarts: running places hold their locks in %s", old.StateDir)
	}
	if err := s.prepare(next); err != nil {
		return false, err
	}
	if err := s.useSetup(next); err != nil {
		return false, err
	}
	for _, p := range old.Places() {
		if q, ok := next.Place(p.ID); ok && q.Machine == p.Machine {
			continue
		}
		// A place departed before is counted where its runner runs already.
		if _, ok := s.departed[p.ID]; ok || !s.runnerAlive(p.ID) {
			continue
		}
		s.departed[p.ID] = p
		s.Log.Info("the setup no longer has this place on this machine; its runner runs on, and its share counts until it ends",
			"place", p.ID, "machine", p.Machine)
	}
	for id, p := range s.departed {
		if q, ok := next.Place(id); ok && q.Machine == p.Machine {
			delete(s.departed, id)
		}
	}
	attrs := describeChange(old, next)
	s.Cfg, s.Rules = next, rules
	for name := range s.unreachable {
		if !slices.ContainsFunc(next.Machines, func(m config.Machine) bool { return m.Name == name }) {
			delete(s.unreachable, name)
			delete(s.reachAsked, name)
		}
	}
	s.saveLocked()
	s.Log.Info("applied the changed setup", attrs...)
	return true, nil
}

// describeChange names what changed from old to next, for the log: each
// repository or organization, pool, machine and place added (+), removed (-)
// or changed (~), the new order, and the other settings changed.
func describeChange(old, next *config.Config) []any {
	var attrs []any
	add := func(name, change string) {
		if change != "" {
			attrs = append(attrs, name, change)
		}
	}
	targets := func(c *config.Config) map[string]bool {
		out := map[string]bool{}
		for _, t := range c.Targets() {
			out[t] = true
		}
		return out
	}
	pools := func(c *config.Config) map[string]config.Pool {
		out := map[string]config.Pool{}
		for _, p := range c.Pools {
			out[p.Label] = p
		}
		return out
	}
	machines := func(c *config.Config) map[string]config.Machine {
		out := map[string]config.Machine{}
		for _, m := range c.Machines {
			m.Places = nil
			out[m.Name] = m
		}
		return out
	}
	places := func(c *config.Config) map[string]config.Place {
		out := map[string]config.Place{}
		for _, p := range c.Places() {
			out[p.ID] = p
		}
		return out
	}
	settings := func(c *config.Config) map[string]any {
		rest := *c
		rest.GitHub.Repos, rest.GitHub.Orgs, rest.Pools, rest.Machines, rest.Order = nil, nil, nil, nil, nil
		out := map[string]any{}
		b, _ := json.Marshal(rest)
		json.Unmarshal(b, &out)
		return out
	}
	add("targets", changes(targets(old), targets(next)))
	add("pools", changes(pools(old), pools(next)))
	add("machines", changes(machines(old), machines(next)))
	add("places", changes(places(old), places(next)))
	if !slices.Equal(old.Order, next.Order) {
		order := strings.Join(next.Order, " ")
		if order == "" {
			order = "default"
		}
		add("order", order)
	}
	add("settings", changes(settings(old), settings(next)))
	return attrs
}

// changes lists the keys after adds (+), changes (~) and drops (-) from
// before, in order.
func changes[T any](before, after map[string]T) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(after)) {
		if b, ok := before[k]; !ok {
			out = append(out, "+"+k)
		} else if !reflect.DeepEqual(b, after[k]) {
			out = append(out, "~"+k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(before)) {
		if _, ok := after[k]; !ok {
			out = append(out, "-"+k)
		}
	}
	return strings.Join(out, " ")
}
