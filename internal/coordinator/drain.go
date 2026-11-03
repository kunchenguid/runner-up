package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// A drain pauses new placement on named places while the coordinator keeps
// running, so that a step needing those places idle can run there as soon as
// their running jobs finish (`runner-up drain`). Its caller owns it: the
// caller publishes the request and holds its live.lock for as long as the
// drain lasts, so a caller that stops, crashes or reaches its time limit
// always resumes placement. No running job or borrow is ever stopped for a
// drain, and places it does not name place as before.
//
// A drain goes through these phases:
//
//	draining   its places take no new job or borrow; what runs there carries on
//	admitting  once all its places were free at once, they take only the
//	           named jobs, one job for each name
//	running    every named job has run or runs there; the places take nothing
//	           more until those jobs end
//	done       each named job ran in one of its places and that runner ended;
//	           its places take any job again at once
//
// A named job waits for the drain's places while the drain lasts, rather than
// start elsewhere. GitHub picks which of a scale set's jobs a runner takes, so
// a runner started for a named job may run another job of the same pool and
// repository; that job finishes, and the named job is admitted again.
type drainRequest struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Places  []string  `json:"places"`
	Jobs    []string  `json:"jobs"`
	Repo    string    `json:"repo,omitempty"`
	Minutes int       `json:"minutes"`
	Queued  time.Time `json:"queued"`
}

// drainState is the coordinator's side of a drain, kept beside its request
// so that a coordinator restart neither loses nor repeats its progress.
type drainState struct {
	// Drained is when all the drain's places were first free at once.
	Drained time.Time `json:"drained,omitempty"`
	// Ran is the place and runner that ran each named job, by job name.
	Ran  map[string]drainRun `json:"ran,omitempty"`
	Done time.Time           `json:"done,omitempty"`
	// Phase and Detail say where the drain stands, for its caller and status.
	Phase  string `json:"phase,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type drainRun struct {
	Place  string `json:"place,omitempty"`
	Runner string `json:"runner"`
	// Outside: GitHub gave the job to a runner the drain did not start for
	// it, outside its places or before they were drained. The drain no
	// longer waits for it.
	Outside bool `json:"outside,omitempty"`
}

type drain struct {
	drainRequest
	drainState
	saved []byte
}

func drainsDir(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "drains") }

func drainDir(cfg *config.Config, id string) string { return filepath.Join(drainsDir(cfg), id) }

// active says whether the drain still holds its places.
func (d *drain) active() bool { return d.Done.IsZero() }

// names says whether job name, of repository repo, is one the drain admits
// and has not yet run.
func (d *drain) names(name, repo string) bool {
	_, ran := d.Ran[name]
	return !ran && slices.Contains(d.Jobs, name) && (d.Repo == "" || d.Repo == repo)
}

// admitted says whether the drain started place's run for one of its jobs:
// the drain's place, launched once its places were drained.
func (d *drain) admitted(place string, run PlaceRun) bool {
	return slices.Contains(d.Places, place) && run.BorrowID == "" && !d.Drained.IsZero() && !run.LaunchedAt.Before(d.Drained)
}

// listDrains reads every published drain, in the order the drains were
// made, and whether its caller still holds it. A directory whose caller has
// not yet published its request is not a drain.
func listDrains(cfg *config.Config) (live []*drain, gone []string) {
	entries, _ := os.ReadDir(drainsDir(cfg))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := drainDir(cfg, e.Name())
		d := &drain{}
		if config.ReadJSON(filepath.Join(dir, "request.json"), &d.drainRequest) != nil || d.ID != e.Name() {
			continue
		}
		held, err := lock.HeldBy(filepath.Join(dir, "live.lock"))
		if err != nil {
			continue
		}
		if !held {
			gone = append(gone, e.Name())
			continue
		}
		d.saved, _ = os.ReadFile(filepath.Join(dir, "state.json"))
		json.Unmarshal(d.saved, &d.drainState)
		live = append(live, d)
	}
	slices.SortStableFunc(live, func(a, b *drain) int { return a.Queued.Compare(b.Queued) })
	return live, gone
}

// refreshDrainsLocked rereads the drains, forgets those whose callers let
// go, and moves each live one on: a drain whose places are all free starts
// admitting its jobs, and one whose jobs have all run is done.
func (s *Service) refreshDrainsLocked() {
	live, gone := listDrains(s.Cfg)
	for _, id := range gone {
		var r drainRequest
		var st drainState
		config.ReadJSON(filepath.Join(drainDir(s.Cfg, id), "request.json"), &r)
		if config.ReadJSON(filepath.Join(drainDir(s.Cfg, id), "state.json"), &st) != nil || st.Done.IsZero() {
			s.Log.Info("drain ended by its caller; placement resumes", "label", r.Label, "places", strings.Join(r.Places, ","))
		}
		os.RemoveAll(drainDir(s.Cfg, id))
	}
	s.drains = live
	s.advanceDrainsLocked()
}

// advanceDrainsLocked moves each active drain on by what its places run. A
// placement pass calls it before placing, so a drain that just drained admits
// its jobs in that pass, and after, so its caller sees what the pass started.
func (s *Service) advanceDrainsLocked() {
	for _, d := range s.drains {
		if d.active() {
			s.advanceDrainLocked(d)
		}
	}
}

func (s *Service) advanceDrainLocked(d *drain) {
	now := s.now()
	if d.Phase == "" {
		s.Log.Info("drain started", "label", d.Label, "places", strings.Join(d.Places, ","), "jobs", strings.Join(d.Jobs, ","), "repo", d.Repo)
	}
	if d.Ran == nil {
		d.Ran = map[string]drainRun{}
	}
	var running []string
	for _, id := range d.Places {
		dir := s.Cfg.PlaceDir(id)
		// A place's run that ended before this pass is already its last run.
		for _, name := range []string{"place.json", "last-place.json"} {
			var run PlaceRun
			if config.ReadJSON(filepath.Join(dir, name), &run) == nil && d.admitted(id, run) && run.Started && d.names(run.RanJob, run.Repo) {
				d.Ran[run.RanJob] = drainRun{Place: id, Runner: run.RunnerName}
				s.Log.Info("drain: a named job reached its runner", "label", d.Label, "place", id, "job", run.RanJob, "runner", run.RunnerName)
			}
		}
		if s.runnerAlive(id) {
			running = append(running, id+" runs "+s.describeRun(id))
		}
	}
	if d.Drained.IsZero() {
		if len(running) > 0 {
			d.Phase, d.Detail = "draining", strings.Join(running, "; ")
			s.saveDrainLocked(d)
			return
		}
		d.Drained = now
		s.Log.Info("drained; admitting the named jobs", "label", d.Label, "places", strings.Join(d.Places, ","),
			"waited", now.Sub(d.Queued).Round(time.Second).String())
	}
	var states []string
	allRan, finished := true, true
	for _, name := range d.Jobs {
		if r, ok := d.Ran[name]; ok {
			if r.Outside {
				where := "outside the drain"
				if r.Place != "" {
					where = "in " + r.Place + ", " + where
				}
				states = append(states, strconv.Quote(name)+" started "+where)
			} else if s.runnerRuns(r.Place, r.Runner) {
				finished = false
				states = append(states, strconv.Quote(name)+" runs in "+r.Place)
			} else {
				states = append(states, strconv.Quote(name)+" ran in "+r.Place)
			}
			continue
		}
		allRan, finished = false, false
		if place := s.startingFor(d, name); place != "" {
			states = append(states, strconv.Quote(name)+" starting in "+place)
		} else {
			states = append(states, strconv.Quote(name)+" waits")
		}
	}
	d.Phase, d.Detail = "admitting", strings.Join(states, ", ")
	if allRan {
		// Every named job has its place; the places take nothing more until
		// those jobs end.
		d.Phase = "running"
	}
	if finished {
		d.Done = now
		d.Phase, d.Detail = "done", d.Detail+"; placement resumed"
		s.Log.Info("drain done; placement resumes", "label", d.Label, "places", strings.Join(d.Places, ","),
			"took", now.Sub(d.Queued).Round(time.Second).String())
	}
	s.saveDrainLocked(d)
}

func (s *Service) saveDrainLocked(d *drain) {
	b, err := json.MarshalIndent(d.drainState, "", "  ")
	if err != nil || bytes.Equal(bytes.TrimSpace(d.saved), b) {
		return
	}
	if err := config.WriteJSON(filepath.Join(drainDir(s.Cfg, d.ID), "state.json"), d.drainState); err != nil {
		s.Log.Error("could not save a drain's state", "label", d.Label, "err", err)
		return
	}
	d.saved = b
}

// drainStartedLocked records, for each live drain, a named job GitHub gave a
// runner. In a runner the drain started, the job counts as run there; in any
// other runner, the drain stops waiting for it, since that job will not wait
// again. Each runner's place says which runner it runs.
func (s *Service) drainStartedLocked(target, pool string, m *scaleset.JobStarted) {
	live, _ := listDrains(s.Cfg)
	repo := repoOf(target, m.JobMessageBase)
	for _, d := range live {
		if !d.active() || !d.names(m.JobDisplayName, repo) || !s.drainServes(d, pool) {
			continue
		}
		r := drainRun{Runner: m.RunnerName, Outside: true}
		for _, p := range s.Cfg.Places() {
			var run PlaceRun
			if config.ReadJSON(filepath.Join(s.Cfg.PlaceDir(p.ID), "place.json"), &run) == nil && run.RunnerName == m.RunnerName {
				r.Place, r.Outside = p.ID, !d.admitted(p.ID, run)
				break
			}
		}
		if d.Ran == nil {
			d.Ran = map[string]drainRun{}
		}
		d.Ran[m.JobDisplayName] = r
		s.Log.Info("drain: a named job reached a runner", "label", d.Label, "job", m.JobDisplayName, "runner", m.RunnerName,
			"place", r.Place, "outside", r.Outside)
		s.saveDrainLocked(d)
	}
}

// drainServes says whether one of the places the drain holds serves pool.
func (s *Service) drainServes(d *drain, pool string) bool {
	return slices.ContainsFunc(d.Places, func(id string) bool {
		p, ok := s.Cfg.Place(id)
		return ok && slices.Contains(p.Pools, pool)
	})
}

// describeRun names what a busy place runs, for a drain's caller.
func (s *Service) describeRun(placeID string) string {
	var run PlaceRun
	if config.ReadJSON(filepath.Join(s.Cfg.PlaceDir(placeID), "place.json"), &run) != nil {
		return "a job"
	}
	if run.BorrowID != "" {
		return "borrow " + strconv.Quote(run.JobName)
	}
	return run.Repo + " " + strconv.Quote(run.JobName)
}

// runnerRuns says whether place still runs the runner named runner.
func (s *Service) runnerRuns(placeID, runner string) bool {
	var run PlaceRun
	return s.runnerAlive(placeID) && config.ReadJSON(filepath.Join(s.Cfg.PlaceDir(placeID), "place.json"), &run) == nil &&
		run.RunnerName == runner
}

// startingFor names the drain's place whose live runner was started for job
// name and has not yet taken a job, or "" when none has.
func (s *Service) startingFor(d *drain, name string) string {
	for _, id := range d.Places {
		var run PlaceRun
		if s.runnerAlive(id) && config.ReadJSON(filepath.Join(s.Cfg.PlaceDir(id), "place.json"), &run) == nil &&
			run.BorrowID == "" && !run.Started && run.JobName == name && !run.LaunchedAt.Before(d.Drained) {
			return id
		}
	}
	return ""
}

// drainOf is the live drain holding place id, the earliest made when two
// name it, or nil.
func (s *Service) drainOf(id string) *drain {
	for _, d := range s.drains {
		if d.active() && slices.Contains(d.Places, id) {
			return d
		}
	}
	return nil
}

// drainFor says which drain's places a waiting job may take: "" for any
// place no drain holds, a drain's ID for that drain's places once it admits
// the job, or heldForDrain while the job waits for a drain that does not yet
// admit it. A job counts as named only where a place of the drain serves
// its pool.
func (s *Service) drainFor(j plan.Job) string {
	if j.BorrowID != "" {
		return ""
	}
	for _, d := range s.drains {
		if !d.active() || !d.names(j.Name, j.Repo) || !s.drainServes(d, j.Pool) {
			continue
		}
		if d.Drained.IsZero() || s.startingFor(d, j.Name) != "" {
			return heldForDrain
		}
		return d.ID
	}
	return ""
}

// heldForDrain matches no place: the job waits for its drain.
const heldForDrain = "held for a drain"

// DrainStatus is a live drain as `runner-up status` shows it.
type DrainStatus struct {
	Label  string
	Places []string
	Jobs   []string
	Repo   string
	Queued time.Time
	Phase  string
	Detail string
}

// Drains lists the drains whose callers still hold them.
func Drains(cfg *config.Config) []DrainStatus {
	live, _ := listDrains(cfg)
	out := make([]DrainStatus, 0, len(live))
	for _, d := range live {
		out = append(out, DrainStatus{Label: d.Label, Places: d.Places, Jobs: d.Jobs, Repo: d.Repo, Queued: d.Queued,
			Phase: d.Phase, Detail: d.Detail})
	}
	return out
}

// listFlag gathers a repeated flag's values.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// DrainMain pauses new placement on named places until their running jobs
// finish and the named jobs have run there, then resumes it. It runs on the
// coordinator's computer, beside a running coordinator.
func DrainMain(args []string) int {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	path := fs.String("config", config.DefaultConfigPath(), "setup file")
	var places, jobs listFlag
	fs.Var(&places, "place", "a place to drain; repeat for more")
	fs.Var(&jobs, "job", "the display name of a job to admit once the places are drained; repeat for more")
	repo := fs.String("repo", "", "the repository the named jobs belong to, OWNER/NAME")
	label := fs.String("label", "", "name shown in status and logs")
	minutes := fs.Int("minutes", 0, "the longest the drain lasts, in minutes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if len(places) == 0 || len(jobs) == 0 || *label == "" || *minutes <= 0 ||
		int64(*minutes) > int64((1<<63-1)/int64(time.Minute)) || len(fs.Args()) > 0 {
		fmt.Fprintln(os.Stderr, "drain needs --place ID --job NAME --label TEXT --minutes N; repeat --place and --job for more")
		return 2
	}
	cfg, err := config.LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := checkDrain(cfg, places, jobs, *repo); err != nil {
		fmt.Fprintln(os.Stderr, "drain:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	req := drainRequest{Label: *label, Places: places, Jobs: jobs, Repo: *repo, Minutes: *minutes}
	code, err := runDrain(ctx, cfg, req, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "drain:", err)
		return 1
	}
	return code
}

func checkDrain(cfg *config.Config, places, jobs []string, repo string) error {
	for i, id := range places {
		if _, ok := cfg.Place(id); !ok {
			return fmt.Errorf("no place %q in the setup", id)
		}
		if slices.Contains(places[:i], id) {
			return fmt.Errorf("place %q is named twice", id)
		}
	}
	for i, name := range jobs {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("a job name is empty")
		}
		if slices.Contains(jobs[:i], name) {
			return fmt.Errorf("job %q is named twice; each name admits one job", name)
		}
	}
	if owner, name, ok := strings.Cut(repo, "/"); repo != "" && (!ok || owner == "" || name == "" || strings.Contains(name, "/")) {
		return fmt.Errorf("--repo %q is not OWNER/NAME", repo)
	}
	return nil
}

// runDrain publishes the drain, holds it and says where it stands until the
// coordinator reports it done (0), the time limit passes (124) or the caller
// is stopped (130). Every way out lets go of the drain.
func runDrain(ctx context.Context, cfg *config.Config, req drainRequest, out io.Writer) (int, error) {
	root := drainsDir(cfg)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return 1, err
	}
	live, _ := listDrains(cfg)
	for _, d := range live {
		if d.active() {
			for _, id := range req.Places {
				if slices.Contains(d.Places, id) {
					return 1, fmt.Errorf("place %s is already drained for %q", id, d.Label)
				}
			}
		}
	}
	dir, err := os.MkdirTemp(root, "drain-")
	if err != nil {
		return 1, err
	}
	// Only a published request belongs to the coordinator; afterwards the
	// live lock, not removing the directory, withdraws it.
	published := false
	defer func() {
		if !published {
			os.RemoveAll(dir)
		}
	}()
	held, err := lock.Busy(filepath.Join(dir, "live.lock"))
	if err != nil || held == nil {
		return 1, fmt.Errorf("could not hold the drain: %v", err)
	}
	defer held.Close()
	req.ID, req.Queued = filepath.Base(dir), time.Now()
	if err := config.WriteJSON(filepath.Join(dir, "request.json"), req); err != nil {
		return 1, err
	}
	published = true
	limit := time.NewTimer(time.Duration(req.Minutes) * time.Minute)
	defer limit.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	last := ""
	for {
		var st drainState
		message := "waiting for the coordinator"
		if config.ReadJSON(filepath.Join(dir, "state.json"), &st) == nil && st.Phase != "" {
			message = st.Phase
			if st.Detail != "" {
				message += ": " + st.Detail
			}
		}
		if message != last {
			fmt.Fprintln(out, "drain:", message)
			last = message
		}
		if !st.Done.IsZero() {
			return 0, nil
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "drain: stopped; placement resumes, and running jobs carry on")
			return 130, nil
		case <-limit.C:
			fmt.Fprintf(out, "drain: %d minutes passed; placement resumes, and running jobs carry on\n", req.Minutes)
			return 124, nil
		case <-poll.C:
		}
	}
}
