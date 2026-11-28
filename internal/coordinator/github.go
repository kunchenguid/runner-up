package coordinator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/version"
)

// A repository's runners belong to its "Default" runner group, id 1.
const defaultRunnerGroupID = 1

// An acknowledgement already in flight may finish after cancellation, then
// the session must close. Bound both so they fit inside shutdownTimeout.
const sessionCleanupTimeout = 5 * time.Second

// ClientFactory makes a scale set client for a repository or organization.
type ClientFactory func(ctx context.Context, target string) (*scaleset.Client, error)

// GhClients makes clients that sign in with the GitHub command line's token.
// gh is asked when a session opens, so a missing sign-in fails it plainly,
// and again each time the client signs in to GitHub, so a token gh renews
// takes effect without a new session.
func GhClients(token tokenSource) ClientFactory {
	return func(ctx context.Context, target string) (*scaleset.Client, error) {
		if _, err := token(ctx); err != nil {
			return nil, err
		}
		return ghClient("https://github.com/"+target, token)
	}
}

// ghTokenPlaceholder is the token a scale set client holds; ghSignIn sends
// gh's token in its place.
const ghTokenPlaceholder = "gh-auth-token"

// ghClient is a scale set client for configURL that signs in with token, read
// afresh at each sign-in.
func ghClient(configURL string, token tokenSource) (*scaleset.Client, error) {
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     configURL,
		PersonalAccessToken: ghTokenPlaceholder,
		SystemInfo:          scaleset.SystemInfo{System: "runner-up", Version: version.Version, Subsystem: "coordinator"},
	}, scaleset.WithHTTPClient(ghSignIn{token: token, next: scaleset.NewHTTPClient(scaleset.HTTPClientConfig{})}))
}

// ghSignIn sends a scale set client's requests, with gh's current token in
// place of the placeholder. The client sends its token only to ask GitHub for
// a runner registration token, which it does when its Actions service token
// nears its end, about hourly. A session's requests ride on that client, so
// a token read once would fail them all once it expired, as a GitHub App
// installation token does after an hour.
type ghSignIn struct {
	token tokenSource
	next  scaleset.HTTPClient
}

func (s ghSignIn) Do(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "Bearer "+ghTokenPlaceholder {
		return s.next.Do(req)
	}
	token, err := s.token(req.Context())
	if err != nil {
		return nil, err
	}
	signed := req.Clone(req.Context())
	signed.Header.Set("Authorization", "Bearer "+token)
	return s.next.Do(signed)
}

// setClient is one scale set's live session.
type setClient struct {
	client  *scaleset.Client
	session *scaleset.MessageSessionClient
	id      int
}

func (c *setClient) AcquireJobs(ctx context.Context, ids []int64) ([]int64, error) {
	return c.session.AcquireJobs(ctx, ids)
}

func (c *setClient) GenerateJIT(ctx context.Context, name, workFolder string) (string, int, error) {
	cfg, err := c.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: workFolder}, c.id)
	if err != nil {
		return "", 0, err
	}
	id := 0
	if cfg.Runner != nil {
		id = cfg.Runner.ID
	}
	return cfg.EncodedJITConfig, id, nil
}

func (c *setClient) RemoveRunner(ctx context.Context, id int64) error {
	return c.client.RemoveRunner(ctx, id)
}

// isOrg reports whether a target is an organization rather than owner/name.
func isOrg(target string) bool { return !strings.Contains(target, "/") }

// ensureScaleSet finds the target's scale set for label, or creates it. The
// scale set is never deleted on shutdown: waiting jobs stay queued for it.
func ensureScaleSet(ctx context.Context, c *scaleset.Client, target, group, label string) (*scaleset.RunnerScaleSet, error) {
	groupID := defaultRunnerGroupID
	if isOrg(target) {
		g, err := c.GetRunnerGroupByName(ctx, group)
		if err != nil {
			return nil, fmt.Errorf("runner group %q: %w", group, err)
		}
		groupID = int(g.ID)
	}
	set, err := c.GetRunnerScaleSet(ctx, groupID, label)
	if err != nil {
		return nil, err
	}
	if set != nil {
		return set, nil
	}
	return c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
		Name:          label,
		RunnerGroupID: groupID,
		Labels:        []scaleset.Label{{Name: label, Type: "System"}},
		// Every place starts a fresh runner of a pinned version; none updates
		// itself.
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	})
}

// Sessions keeps one scale set session open for each repository or
// organization and pool of the setup the coordinator runs. A setup change
// opens the sessions of the scale sets it adds and closes those of the ones
// it removes; every other session stays open through it, except a session
// that stopped because GitHub could not find its repository or organization
// (RunSet), which every applied setup change tries again.
type Sessions struct {
	ctx     context.Context
	svc     *Service
	log     *slog.Logger
	clients ClientFactory
	// pause waits between a session's tries (RunSet).
	pause pauseFunc
	wg    sync.WaitGroup
	open  map[sessionKey]*openSession

	// mu guards missing: the sessions that stopped because GitHub could not
	// find their target.
	mu      sync.Mutex
	missing map[sessionKey]bool
}

// openSession is a session Match started; cancel closes it.
type openSession struct{ cancel context.CancelFunc }

// sessionKey names one session: a target's scale set for a pool, found in
// the runner group an organization's scale sets join.
type sessionKey struct{ target, pool, group string }

func NewSessions(ctx context.Context, svc *Service, log *slog.Logger, clients ClientFactory) *Sessions {
	return &Sessions{ctx: ctx, svc: svc, log: log, clients: clients, pause: sleep,
		open: map[sessionKey]*openSession{}, missing: map[sessionKey]bool{}}
}

// Match opens a session for each scale set cfg serves that has none, or
// whose session stopped because GitHub could not find its target, and closes
// each session of a scale set cfg no longer serves. Only one goroutine calls
// it.
func (ss *Sessions) Match(cfg *config.Config) {
	var want []sessionKey
	for _, target := range cfg.Targets() {
		group := ""
		if isOrg(target) {
			group = cfg.RunnerGroup()
		}
		for _, label := range cfg.Labels() {
			want = append(want, sessionKey{target, label, group})
		}
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for k, o := range ss.open {
		if !slices.Contains(want, k) {
			if !ss.missing[k] {
				ss.log.Info("the setup no longer serves this scale set; closing its session", "target", k.target, "pool", k.pool)
			}
			o.cancel()
			delete(ss.open, k)
			delete(ss.missing, k)
		}
	}
	for _, k := range want {
		if _, ok := ss.open[k]; ok && !ss.missing[k] {
			continue
		}
		if ss.missing[k] {
			ss.log.Info("the setup was applied again; trying the session GitHub could not find again", "target", k.target, "pool", k.pool)
			delete(ss.missing, k)
		}
		ctx, cancel := context.WithCancel(ss.ctx)
		o := &openSession{cancel}
		ss.open[k] = o
		ss.wg.Add(1)
		go func() {
			defer ss.wg.Done()
			if RunSet(ctx, ss.svc, ss.log, k.target, k.pool, ss.clients, ss.pause) {
				ss.mu.Lock()
				// A later Match may have closed it meanwhile.
				if ss.open[k] == o {
					ss.missing[k] = true
				}
				ss.mu.Unlock()
			}
		}()
	}
}

// Done is closed once every session has ended, after the coordinator's
// context ended.
func (ss *Sessions) Done() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		ss.wg.Wait()
		close(done)
	}()
	return done
}

const (
	// sessionRetry is the pause before a session that ended is tried again.
	sessionRetry = 30 * time.Second
	// missingStop is how many times in a row GitHub answers that it cannot
	// find a session's repository or organization before the session stops:
	// with the pauses doubling from sessionRetry between them, about half an
	// hour of answers.
	missingStop = 7
)

// pauseFunc waits d, and reports false when ctx ended first.
type pauseFunc func(ctx context.Context, d time.Duration) bool

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RunSet keeps one scale set's session open and feeds its messages to the
// service, until ctx ends. It never acquires a job itself: the serve loop's
// placement pass does, once a place is chosen, with the serving context.
// Errors restart the session after a pause of sessionRetry; the session's
// messages stay queued at GitHub until acknowledged.
//
// A repository or organization that was renamed, moved or deleted answers
// not found to every try, and every try uses the sign-in's allowance, which
// every scale set and GitHub read shares: 29 sessions on one old name used it
// up for hours. So each not found in a row doubles the pause, and after
// missingStop of them RunSet logs that GitHub cannot find the target and
// returns true, its session stopped. Any session that opens ends the count.
func RunSet(ctx context.Context, svc *Service, log *slog.Logger, target, label string, clients ClientFactory, pause pauseFunc) (missing bool) {
	log = log.With("target", target, "pool", label)
	notFound := 0
	for ctx.Err() == nil {
		opened, err := serveSet(ctx, svc, log, target, label, clients)
		if ctx.Err() != nil {
			return false
		}
		if opened {
			notFound = 0
		}
		wait := sessionRetry
		if errors.Is(err, scaleset.NotFoundError) {
			notFound++
			if notFound >= missingStop {
				log.Error("GitHub cannot find this repository or organization; its session stops until the setup changes or runner-up restarts",
					"answers", notFound, "err", err)
				return true
			}
			wait = sessionRetry << (notFound - 1)
			log.Warn("GitHub cannot find this repository or organization; retrying later", "answers", notFound,
				"retry", wait.String(), "err", err)
		} else {
			notFound = 0
			log.Warn("scale set session ended; retrying in 30s", "err", err)
		}
		if !pause(ctx, wait) {
			return false
		}
	}
	return false
}

// serveSet serves one session of a scale set, and reports whether it opened.
func serveSet(ctx context.Context, svc *Service, log *slog.Logger, target, label string, clients ClientFactory) (opened bool, err error) {
	c, err := clients(ctx, target)
	if err != nil {
		return false, err
	}
	set, err := ensureScaleSet(ctx, c, target, svc.Setup().RunnerGroup(), label)
	if err != nil {
		return false, fmt.Errorf("scale set: %w", err)
	}
	c.SetSystemInfo(scaleset.SystemInfo{System: "runner-up", Version: version.Version, Subsystem: "coordinator", ScaleSetID: set.ID})
	host, _ := os.Hostname()
	// One session per scale set: a second coordinator waits here until this
	// one lets go.
	session, err := c.MessageSessionClient(ctx, set.ID, "runner-up-"+host)
	if err != nil {
		return false, fmt.Errorf("session: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
		defer cancel()
		if err := session.Close(closeCtx); err != nil {
			log.Warn("could not close scale set session; it may remain active", "err", err)
		} else {
			log.Info("scale set session closed", "scaleSetId", set.ID)
		}
	}()
	log.Info("scale set session open", "scaleSetId", set.ID)
	sc := &setClient{client: c, session: session, id: set.ID}
	svc.AddSet(target, label, sc)
	// Run before the session closes: no job is acquired through a closing
	// session.
	defer svc.RemoveSet(target, label, sc)

	first := &scaleset.RunnerScaleSetMessage{MessageID: 0, Statistics: session.Session().Statistics}
	svc.Handle(target, label, first)
	last := 0
	for ctx.Err() == nil {
		msg, err := session.GetMessage(ctx, last, svc.Setup().AnnounceCapacity)
		if err != nil {
			return true, fmt.Errorf("get message: %w", err)
		}
		svc.Handle(target, label, msg)
		if msg != nil {
			last = msg.MessageID
			ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
			err := session.DeleteMessage(ackCtx, msg.MessageID)
			cancel()
			if err != nil {
				return true, fmt.Errorf("ack message %d: %w", msg.MessageID, err)
			}
		}
	}
	return true, ctx.Err()
}
