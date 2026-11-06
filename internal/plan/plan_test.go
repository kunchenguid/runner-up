package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
)

// Intent (README, "Setup file", priority; asked for as "priority 0 the first
// to use, only when 0 is used up then we start to pick 1, then when 1 is all
// full then start to pick 2"): a job takes a free place with room at the
// lowest priority level that has one, and moves up a level only when every
// place below it that serves its pool is full.
//
// levels-fleet.json is a renamed copy of a real fleet's shape: two Macs with
// small check guests (rental, spare) and a big Mac (main) whose own host
// places are set to level 1, so split check jobs run on the guests first.

// fleet reads the fleet, lets change set its priorities, and loads it as the
// coordinator does.
func fleet(t *testing.T, change func(*config.Config)) *config.Config {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "levels-fleet.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The state folder must be absolute on the machine running the test.
	dir, _ := json.Marshal(t.TempDir())
	cfg, err := config.ParseConfig(bytes.ReplaceAll(b, []byte(`"STATE_DIR"`), dir))
	if err != nil {
		t.Fatal(err)
	}
	if change != nil {
		change(cfg)
		cfg.Defaults()
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

// placeLevels plans n split jobs on cfg's places. running maps a place to the
// pool whose job it runs; a place on a resting machine reads busy, as the
// coordinator's view of it does (internal/coordinator, snapshot.go). It
// returns each placed job's place and level, in rank order.
func placeLevels(t *testing.T, cfg *config.Config, running map[string]string, resting string, n int) (ids []string, levels []int) {
	t.Helper()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	var views []PlaceView
	headroom := map[string]Headroom{}
	for _, m := range cfg.Machines {
		h := Headroom{Cores: m.Budget().Cores, MemoryMB: m.Budget().MemoryMB, Memory: m.MemoryMB > 0}
		for _, p := range m.Places {
			pool, busy := running[p.ID]
			if busy {
				sized := cfg.SizePlace(p, pool)
				h = h.less(config.Share{Cores: sized.Cores, MemoryMB: sized.MemoryMB})
			}
			views = append(views, PlaceView{Place: p, Busy: busy || m.Name == resting})
		}
		headroom[m.Name] = h
	}
	var jobs []Job
	for i := range n {
		jobs = append(jobs, Job{Target: "octo-org/app", Repo: "octo-org/app", Pool: "app-mac-split", RequestID: int64(i + 1),
			Name: fmt.Sprintf("check part %d", i+1), QueueTime: now.Add(time.Duration(i-n) * time.Minute), Size: cfg.PoolSize("app-mac-split")})
	}
	rules, err := ParseOrder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range Plan(jobs, views, headroom, rules, now) {
		ids = append(ids, a.Place.ID)
		levels = append(levels, a.Place.Level)
	}
	return ids, levels
}

func TestJobsFillLevelZeroBeforeLevelOne(t *testing.T) {
	cfg := fleet(t, nil)
	for _, tc := range []struct {
		why     string
		running map[string]string
		resting string
		want    []int
	}{
		{why: "an idle fleet starts all three on the level-0 guests", want: []int{0, 0, 0}},
		{why: "with a guest running a phone check, the third starts at level 1",
			running: map[string]string{"rental-vm": "app-mac-phone"}, want: []int{0, 0, 1}},
		{why: "with the spare Mac unreachable, the third starts at level 1",
			resting: "spare", want: []int{0, 0, 1}},
	} {
		ids, levels := placeLevels(t, cfg, tc.running, tc.resting, 3)
		if fmt.Sprint(levels) != fmt.Sprint(tc.want) {
			t.Errorf("%s: the jobs went to %v at levels %v, want levels %v", tc.why, ids, levels, tc.want)
		}
	}
}

func TestLevelOneFillsBeforeLevelTwoTakesAJob(t *testing.T) {
	// The spare Mac's guest moves to level 2, behind the main Mac's host
	// places at level 1, though the spare is listed first and idle.
	cfg := fleet(t, func(c *config.Config) {
		two := 2
		for i := range c.Machines {
			if c.Machines[i].Name == "spare" {
				c.Machines[i].Places[0].Priority = &two
			}
		}
	})
	// The main Mac has room for two split jobs at once, so its third host
	// place counts full and the fifth job goes to level 2.
	ids, levels := placeLevels(t, cfg, nil, "", 5)
	if want := []int{0, 0, 1, 1, 2}; fmt.Sprint(levels) != fmt.Sprint(want) {
		t.Errorf("the jobs went to %v at levels %v, want levels %v", ids, levels, want)
	}
}
