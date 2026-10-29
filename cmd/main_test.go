package cmd

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/actions/scaleset"
	"github.com/kunchenguid/runner-up/internal/config"
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
)

func TestMain(m *testing.M) {
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
	actions := os.Getenv(actionsEnv)
	clients := func(_ context.Context, target string) (*scaleset.Client, error) {
		return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     actions + "/" + target,
			PersonalAccessToken: "local-only",
			SystemInfo:          scaleset.SystemInfo{System: "runner-up", Version: version.Version, Subsystem: "coordinator"},
		})
	}
	return serveWith(cfg, path, os.Getenv(selfEnv), clients, nil)
}
