package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/runner-up/internal/lock"
)

// Intent (a bug a live Hyper-V check found): a finished place left
// its lock behind. A place leaves nothing once its agent lets go, and
// two agents still never hold one place at once.

func TestAFinishedPlaceLeavesNothingBehind(t *testing.T) {
	a := &AgentRun{Place: "ru-e2e-1", StateDir: t.TempDir()}
	unlock, err := LockPlace(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if _, err := os.Stat(a.Dir()); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(a.Dir())
		t.Errorf("the finished place left %s with %v", a.Dir(), entries)
	}
}

func TestAnAgentWaitingForAPlaceTakesItWhenTheOtherLetsGo(t *testing.T) {
	a := &AgentRun{Place: "ru-e2e-1", StateDir: t.TempDir()}
	first, err := LockPlace(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func())
	go func() {
		second, err := LockPlace(context.Background(), a)
		if err != nil {
			t.Error(err)
		}
		got <- second
	}()
	select {
	case <-got:
		t.Fatal("a second agent took a place another holds")
	case <-time.After(1500 * time.Millisecond):
	}
	first()
	var second func()
	select {
	case second = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting agent never took the place")
	}
	// The waiting agent holds the lock file now at the place's path: a
	// third finds the place busy.
	f, err := lock.Busy(filepath.Join(a.Dir(), "agent.lock"))
	if err != nil || f != nil {
		t.Errorf("a third agent could lock the place (%v)", err)
	}
	second()
}
