package coordinator

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kunchenguid/runner-up/internal/agent"
	"github.com/kunchenguid/runner-up/internal/config"
)

// UsageRecord retains the reservation and observation of one finished runner.
// A missing reading is explicit, never an observation of zero. GitHub may
// give a runner a different job of its pool; Pool is always authoritative.
type UsageRecord struct {
	Pool        string       `json:"pool"`
	Machine     string       `json:"machine"`
	Provider    string       `json:"provider"`
	Place       string       `json:"place"`
	Runner      string       `json:"runner"`
	StartedAt   time.Time    `json:"startedAt"`
	FinishedAt  time.Time    `json:"finishedAt"`
	Reservation config.Share `json:"reservation"`
	Usage       *agent.Usage `json:"usage,omitempty"`
}

func usageDir(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "usage") }

func (s *Service) recordUsage(p config.Place, run PlaceRun) error {
	if !run.Started || run.BorrowID != "" {
		return nil
	}
	r := UsageRecord{Pool: run.Pool, Machine: p.Machine, Provider: p.Provider, Place: p.ID, Runner: run.RunnerName,
		StartedAt: run.LaunchedAt, FinishedAt: s.now(), Reservation: run.Reservation}
	if r.Reservation.Cores == 0 {
		r.Reservation = share(p)
	}
	var u agent.Usage
	if config.ReadJSON(filepath.Join(s.Cfg.PlaceDir(p.ID), "usage.json"), &u) == nil {
		r.Usage = &u
	}
	if err := os.MkdirAll(usageDir(s.Cfg), 0700); err != nil {
		return err
	}
	// Replaying retirement after a crash rewrites the same record, not a
	// second sample. No job payloads, environment or registrations are stored.
	path := filepath.Join(usageDir(s.Cfg), fmt.Sprintf("%s-%d.json", p.ID, run.LaunchedAt.UnixNano()))
	return config.WriteJSON(path, r)
}

func ReadUsage(cfg *config.Config) ([]UsageRecord, error) {
	files, err := os.ReadDir(usageDir(cfg))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []UsageRecord
	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		var r UsageRecord
		if err := config.ReadJSON(filepath.Join(usageDir(cfg), f.Name()), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}
