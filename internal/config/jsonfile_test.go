//go:build !windows

// Two processes write one file only on the coordinator's machine, which runs
// Linux or macOS. Windows refuses a rename onto a file another rename is
// replacing at that moment, so the case this test covers cannot arise there.

package config

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/kunchenguid/runner-up/internal/testrun"
)

// Intent (the coordinator's design, coordinator/place.go): a file two
// processes write, such as a place's place.json, which the coordinator and
// the place runner both rewrite, is always whole: neither writer fails
// because the other renamed its temporary file, and a reader sees one
// writer's file or the other's.
func TestTwoWritersOfOneFileBothSucceed(t *testing.T) {
	type run struct {
		RunnerName string `json:"runnerName"`
		PID        int    `json:"pid,omitempty"`
		Started    bool   `json:"started"`
	}
	path := filepath.Join(t.TempDir(), "place.json")
	writers := []run{{RunnerName: "runner-up-box-1-1792602614", PID: 48213}, {RunnerName: "runner-up-box-1-1792602614", Started: true}}
	var wg sync.WaitGroup
	errs := make(chan error, 2*200)
	for _, w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				errs <- WriteJSON(path, w)
			}
		}()
	}
	testrun.Wait(t, "both writers to finish", wg.Wait)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a writer failed: %v", err)
		}
	}
	var got run
	if err := ReadJSON(path, &got); err != nil {
		t.Fatalf("the file is not whole: %v", err)
	}
	if got != writers[0] && got != writers[1] {
		t.Fatalf("the file holds %+v, neither writer's", got)
	}
	if left, _ := filepath.Glob(path + "*.tmp"); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}
