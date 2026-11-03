package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/coordinator"
	"github.com/kunchenguid/runner-up/internal/testbin"
	"github.com/kunchenguid/runner-up/internal/version"
)

// runnerUp builds this program for the tests that run it as a program.
func runnerUp(t *testing.T) string { return testbin.Build(t, "") }

// The end-to-end tests run the coordinator as this test program started
// again with serveEnv set: it serves the stand-in for GitHub's Actions service
// at actionsEnv, and its place runners are the built program at selfEnv.
const (
	serveEnv   = "RUNNER_UP_TEST_SERVE_CONFIG"
	actionsEnv = "RUNNER_UP_TEST_ACTIONS"
	selfEnv    = "RUNNER_UP_TEST_SELF"
	// passesEnv names a file the coordinator adds a line to after each pass
	// of its serve loop, for a test to count.
	passesEnv = "RUNNER_UP_TEST_PASSES"
)

// trialEnv shortens a new version's trial (coordinator.TrialWindow) in the
// end-to-end tests of updates.
const trialEnv = "RUNNER_UP_TEST_TRIAL"

func TestMain(m *testing.M) {
	// This test program, built as a release of runner-up, run as runner-up: by
	// the coordinator checking it before it restarts as it, restarting as it,
	// or on a machine as its agent.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-test.") {
		if path := os.Getenv(serveEnv); path != "" && os.Args[1] == "serve" {
			os.Exit(serveStandIn(path))
		}
		Main()
		os.Exit(0)
	}
	if path := os.Getenv(serveEnv); path != "" {
		os.Exit(serveStandIn(path))
	}
	testbin.Main(m)
}

func serveStandIn(path string) int {
	cfg, err := config.LoadConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if d, err := time.ParseDuration(os.Getenv(trialEnv)); err == nil {
		coordinator.TrialWindow = d
	}
	actions := os.Getenv(actionsEnv)
	clients := func(_ context.Context, target string) (*scaleset.Client, error) {
		return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     actions + "/" + target,
			PersonalAccessToken: "local-only",
			SystemInfo:          scaleset.SystemInfo{System: "runner-up", Version: version.Version, Subsystem: "coordinator"},
		})
	}
	var passed func()
	if passes := os.Getenv(passesEnv); passes != "" {
		passed = func() {
			if f, err := os.OpenFile(passes, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				f.WriteString("pass\n")
				f.Close()
			}
		}
	}
	return serveWith(cfg, path, os.Getenv(selfEnv), clients, nil, passed)
}
