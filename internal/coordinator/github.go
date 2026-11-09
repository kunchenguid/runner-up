package coordinator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// GhClients makes clients that sign in with the GitHub command line's token,
// read afresh for every session and whenever GitHub refuses a session's
// token, so a new gh sign-in takes effect at the next session.
func GhClients(token tokenSource) ClientFactory {
	return func(ctx context.Context, target string) (*scaleset.Client, error) {
		t, err := token(ctx)
		if err != nil {
			return nil, err
		}
		return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     "https://github.com/" + target,
			PersonalAccessToken: t,
			SystemInfo:          scaleset.SystemInfo{System: "runner-up", Version: version.Version, Subsystem: "coordinator"},
		})
	}
}

// setClient is one scale set's live session.
type setClient struct {
	session *scaleset.MessageSessionClient
	id      int
	// renew makes a client with a token read afresh, for when GitHub refuses
	// the one client was made with.
	renew func(ctx context.Context) (*scaleset.Client, error)
	// end ends the session. Jobs are acquired through the client the session
	// was opened with, which keeps the token it was made with; once GitHub
	// refuses that token, only a new session, opened with a token read
	// afresh, acquires jobs again.
	end context.CancelCauseFunc

	mu     sync.Mutex
	client *scaleset.Client
}

func (c *setClient) AcquireJobs(ctx context.Context, ids []int64) ([]int64, error) {
	got, err := c.session.AcquireJobs(ctx, ids)
	if errors.Is(err, scaleset.UnauthorizedError) && c.end != nil {
		c.end(fmt.Errorf("GitHub refused the session's token: %w", err))
	}
	return got, err
}

func (c *setClient) GenerateJIT(ctx context.Context, name, workFolder string) (string, int, error) {
	var cfg *scaleset.RunnerScaleSetJitRunnerConfig
	err := c.signedIn(ctx, func(client *scaleset.Client) error {
		var err error
		cfg, err = client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: workFolder}, c.id)
		return err
	})
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
	return c.signedIn(ctx, func(client *scaleset.Client) error { return client.RemoveRunner(ctx, id) })
}

// signedIn runs call with the session's client. The message session keeps
// its own token, so a session can outlive the GitHub token its client was
// made with, such as a GitHub App's, which lasts an hour. When GitHub refuses
// that token, signedIn reads the token again through renew and runs call once
// more with the new client, which the session then keeps.
func (c *setClient) signedIn(ctx context.Context, call func(*scaleset.Client) error) error {
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	err := call(client)
	if !errors.Is(err, scaleset.UnauthorizedError) || c.renew == nil {
		return err
	}
	fresh, renewErr := c.renew(ctx)
	if renewErr != nil {
		return fmt.Errorf("%w (reading the GitHub token again: %w)", err, renewErr)
	}
	c.mu.Lock()
	if c.client == client {
		c.client = fresh
	}
	c.mu.Unlock()
	return call(fresh)
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
// it removes; every other session stays open through it.
type Sessions struct {
	ctx     context.Context
	svc     *Service
	log     *slog.Logger
	clients ClientFactory
	wg      sync.WaitGroup
	open    map[sessionKey]context.CancelFunc
}

// sessionKey names one session: a target's scale set for a pool, found in
// the runner group an organization's scale sets join.
type sessionKey struct{ target, pool, group string }

func NewSessions(ctx context.Context, svc *Service, log *slog.Logger, clients ClientFactory) *Sessions {
	return &Sessions{ctx: ctx, svc: svc, log: log, clients: clients, open: map[sessionKey]context.CancelFunc{}}
}

// Match opens a session for each scale set cfg serves that has none, and
// closes each session of a scale set cfg no longer serves. Only one goroutine
// calls it.
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
	for k, cancel := range ss.open {
		if !slices.Contains(want, k) {
			ss.log.Info("the setup no longer serves this scale set; closing its session", "target", k.target, "pool", k.pool)
			cancel()
			delete(ss.open, k)
		}
	}
	for _, k := range want {
		if _, ok := ss.open[k]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(ss.ctx)
		ss.open[k] = cancel
		ss.wg.Add(1)
		go func() {
			defer ss.wg.Done()
			RunSet(ctx, ss.svc, ss.log, k.target, k.pool, ss.clients)
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

// RunSet keeps one scale set's session open and feeds its messages to the
// service, until ctx ends. It never acquires a job itself: the serve loop's
// placement pass does, once a place is chosen, with the serving context.
// Errors restart the session after a pause; the session's messages stay
// queued at GitHub until acknowledged.
func RunSet(ctx context.Context, svc *Service, log *slog.Logger, target, label string, clients ClientFactory) {
	log = log.With("target", target, "pool", label)
	for ctx.Err() == nil {
		err := serveSet(ctx, svc, log, target, label, clients)
		if ctx.Err() != nil {
			return
		}
		log.Warn("scale set session ended; retrying in 30s", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

func serveSet(ctx context.Context, svc *Service, log *slog.Logger, target, label string, clients ClientFactory) error {
	c, err := clients(ctx, target)
	if err != nil {
		return err
	}
	set, err := ensureScaleSet(ctx, c, target, svc.Setup().RunnerGroup(), label)
	if err != nil {
		return fmt.Errorf("scale set: %w", err)
	}
	c.SetSystemInfo(scaleset.SystemInfo{System: "runner-up", Version: version.Version, Subsystem: "coordinator", ScaleSetID: set.ID})
	host, _ := os.Hostname()
	// One session per scale set: a second coordinator waits here until this
	// one lets go.
	session, err := c.MessageSessionClient(ctx, set.ID, "runner-up-"+host)
	if err != nil {
		return fmt.Errorf("session: %w", err)
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
	sessCtx, end := context.WithCancelCause(ctx)
	defer end(nil)
	renew := func(ctx context.Context) (*scaleset.Client, error) {
		c, err := clients(ctx, target)
		if err != nil {
			return nil, err
		}
		c.SetSystemInfo(scaleset.SystemInfo{System: "runner-up", Version: version.Version, Subsystem: "coordinator", ScaleSetID: set.ID})
		log.Info("GitHub refused the session's token; read it again")
		return c, nil
	}
	sc := &setClient{client: c, session: session, id: set.ID, renew: renew, end: end}
	svc.AddSet(target, label, sc)
	// Run before the session closes: no job is acquired through a closing
	// session.
	defer svc.RemoveSet(target, label, sc)

	first := &scaleset.RunnerScaleSetMessage{MessageID: 0, Statistics: session.Session().Statistics}
	svc.Handle(target, label, first)
	last := 0
	for sessCtx.Err() == nil {
		msg, err := session.GetMessage(sessCtx, last, svc.Setup().AnnounceCapacity)
		if sessCtx.Err() != nil {
			break
		}
		if err != nil {
			return fmt.Errorf("get message: %w", err)
		}
		svc.Handle(target, label, msg)
		if msg != nil {
			last = msg.MessageID
			ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
			err := session.DeleteMessage(ackCtx, msg.MessageID)
			cancel()
			if err != nil {
				return fmt.Errorf("ack message %d: %w", msg.MessageID, err)
			}
		}
	}
	return context.Cause(sessCtx)
}
