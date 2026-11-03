package coordinator

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/lock"
)

// A borrow or a drain is a request its caller owns: a folder of its own
// under the requests' root in the state folder (borrows/ or drains/). The
// caller makes the folder, takes its live.lock and then publishes the
// request by writing request.json. From then on the folder belongs to the
// coordinator, and only the caller letting go of live.lock - by finishing,
// crashing or being stopped - withdraws the request, even while no
// coordinator runs. The coordinator removes the folder of a request whose
// caller let go.

// callerRequest is the caller's side of a request folder.
type callerRequest struct {
	dir       string
	live      *os.File
	published bool
}

// openRequest makes a new request folder under root, its name prefix and a
// random suffix, and takes its live lock. Close it when the request ends.
func openRequest(root, prefix string) (*callerRequest, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, prefix)
	if err != nil {
		return nil, err
	}
	live, err := lock.Busy(filepath.Join(dir, "live.lock"))
	if err == nil && live == nil {
		err = errors.New("another process holds a new request's lock")
	}
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &callerRequest{dir: dir, live: live}, nil
}

// id is the request's name, its folder's.
func (r *callerRequest) id() string { return filepath.Base(r.dir) }

// publish writes the request as request.json, handing it to the coordinator.
func (r *callerRequest) publish(request any) error {
	if err := config.WriteJSON(filepath.Join(r.dir, "request.json"), request); err != nil {
		return err
	}
	r.published = true
	return nil
}

// close lets go of the request. The coordinator ignores a folder that was
// never published, so close removes it; a published one is the
// coordinator's to remove.
func (r *callerRequest) close() {
	r.live.Close()
	if !r.published {
		os.RemoveAll(r.dir)
	}
}

// requestHeld says whether a request's caller still holds it.
func requestHeld(dir string) (bool, error) {
	return lock.HeldBy(filepath.Join(dir, "live.lock"))
}

// scanRequests lists the published request folders under root by name:
// live while their callers hold them, gone once they let go. A folder counts
// as published once one of the files named by published exists; before
// that its caller may still be making it, so it is in neither list. A
// folder whose lock cannot be read is in neither list either.
func scanRequests(root string, published ...string) (live, gone []string) {
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if !anyExists(dir, published) {
			continue
		}
		held, err := requestHeld(dir)
		if err != nil {
			continue
		}
		if held {
			live = append(live, e.Name())
		} else {
			gone = append(gone, e.Name())
		}
	}
	return live, gone
}

func anyExists(dir string, names []string) bool {
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}
