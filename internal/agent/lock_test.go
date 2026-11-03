package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

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

// The test runs in a synctest bubble: synctest.Wait returns once the second
// agent has found the place held and waits to try again.
func TestAnAgentWaitingForAPlaceTakesItWhenTheOtherLetsGo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		synctest.Wait()
		select {
		case <-got:
			t.Fatal("a second agent took a place another holds")
		default:
		}
		first()
		second := <-got
		// The waiting agent holds the lock file now at the place's path: a
		// third finds the place busy.
		f, err := lock.Busy(filepath.Join(a.Dir(), "agent.lock"))
		if err != nil || f != nil {
			t.Errorf("a third agent could lock the place (%v)", err)
		}
		second()
	})
}
