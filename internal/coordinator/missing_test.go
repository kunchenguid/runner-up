//go:build !windows

package coordinator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/testrun"
	"github.com/kunchenguid/runner-up/internal/version"
)

// Intent (README, "When GitHub cannot find a repository", and the fixed bug:
// a project moved to a new name kept 29 scale set sessions on its old one,
// each tried again every 30 seconds, and their not-found answers used up the
// sign-in's GitHub allowance for over five hours): a session whose target
// GitHub cannot find waits twice as long after each not-found answer in a
// row, then stops and says so, and the next applied setup tries it again.

// goneTarget stands in for GitHub where every target answers not found, as a
// renamed, moved or deleted repository does: GitHub hands out a registration
// token, then refuses the Actions service connection for the target with 404.
type goneTarget struct {
	*httptest.Server
	mu    sync.Mutex
	tries int // the connections GitHub refused
	stray []string
}

func newGoneTarget(t *testing.T) *goneTarget {
	g := &goneTarget{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/runners/registration-token"):
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"token": "registration-token"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/runner-registration"):
			g.tries++
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"Not Found","documentation_url":"https://docs.github.com/rest","status":"404"}`)
		default:
			g.stray = append(g.stray, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *goneTarget) clients(_ context.Context, target string) (*scaleset.Client, error) {
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     g.URL + "/" + target,
		PersonalAccessToken: "local-only",
		SystemInfo:          scaleset.SystemInfo{System: "runner-up", Version: version.Version, Subsystem: "coordinator"},
	})
}

func (g *goneTarget) count() (int, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tries, append([]string(nil), g.stray...)
}

func TestSessionForATargetGitHubCannotFindSlowsThenStops(t *testing.T) {
	g := newGoneTarget(t)
	h := newHarness(t, oneBox(t.TempDir()), newFakeLauncher(), map[string]*fakeSet{})
	var pauses []time.Duration
	pause := func(ctx context.Context, d time.Duration) bool {
		pauses = append(pauses, d)
		return ctx.Err() == nil
	}
	stopped := make(chan bool, 1)
	go func() {
		stopped <- RunSet(context.Background(), h.svc, h.svc.Log, "octo-org/app", "ru-linux", g.clients, pause)
	}()
	if !testrun.Receive(t, stopped, "the session to stop") {
		t.Fatal("the session ended without saying GitHub cannot find its target")
	}
	tries, stray := g.count()
	if tries != missingStop || len(stray) != 0 {
		t.Fatalf("GitHub was asked %d times (and %v), want %d", tries, stray, missingStop)
	}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute}
	if !slices.Equal(pauses, want) {
		t.Fatalf("paused %v between tries, want %v", pauses, want)
	}
}

func TestStoppedSessionIsTriedAgainWhenTheSetupIsApplied(t *testing.T) {
	g := newGoneTarget(t)
	cfg := oneBox(t.TempDir())
	h := newHarness(t, cfg, newFakeLauncher(), map[string]*fakeSet{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ss := NewSessions(ctx, h.svc, slog.New(slog.NewTextHandler(io.Discard, nil)), g.clients)
	ss.pause = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	sessions := len(cfg.Targets()) * len(cfg.Labels())

	ss.Match(cfg)
	testrun.Receive(t, ss.Done(), "every session to stop")
	if tries, _ := g.count(); tries != sessions*missingStop {
		t.Fatalf("GitHub was asked %d times, want %d", tries, sessions*missingStop)
	}

	// The setup is applied again: each stopped session is tried again, and
	// stops again.
	ss.Match(cfg)
	testrun.Receive(t, ss.Done(), "every session to stop again")
	if tries, _ := g.count(); tries != 2*sessions*missingStop {
		t.Fatalf("GitHub was asked %d times after the setup was applied again, want %d", tries, 2*sessions*missingStop)
	}
}
