package coordinator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/plan"
)

// A running coordinator applies a changed setup file in place. It reads the
// file every SetupPoll, and once the file has held the same content for
// SetupQuiet after a change, so a pull or an editor that rewrites the file in
// steps is read whole, it checks the content as check-config does and applies
// it (Service.Reload, Sessions.Match). A file it cannot read, or a setup that
// fails the check, leaves the running setup as it is.
//
// The coordinator's place runners never read the setup file itself, which may
// hold a setup the coordinator refused or has not applied yet. Each reads the
// setup the coordinator runs, which the coordinator keeps under its state
// folder (writeSetup).
const (
	SetupPoll  = 250 * time.Millisecond
	SetupQuiet = time.Second
)

// CheckSetup reads a setup file's content and checks it as check-config does.
func CheckSetup(b []byte) (*config.Config, []plan.Rule, error) {
	cfg, err := config.ParseConfig(b)
	if err != nil {
		return nil, nil, err
	}
	rules, err := plan.ParseOrder(cfg.Order)
	if err != nil {
		return nil, nil, err
	}
	return cfg, rules, nil
}

// SetupRead is the setup file's content, or why it could not be read.
type SetupRead struct {
	Data []byte
	Err  error
}

func (r SetupRead) same(o SetupRead) bool {
	if (r.Err == nil) != (o.Err == nil) {
		return false
	}
	if r.Err != nil {
		return r.Err.Error() == o.Err.Error()
	}
	return bytes.Equal(r.Data, o.Data)
}

// WatchSetup reads the file at path every poll and sends what it holds once
// it has held that for quiet after a change, until ctx ends. Its first
// reading counts as a change, so a file changed while the coordinator started
// is not missed; the receiver compares what it is sent with the setup it runs.
func WatchSetup(ctx context.Context, path string, poll, quiet time.Duration) <-chan SetupRead {
	out := make(chan SetupRead)
	read := func() SetupRead {
		b, err := os.ReadFile(path)
		return SetupRead{Data: b, Err: err}
	}
	w := &setupWatch{quiet: quiet}
	w.next(read(), time.Now())
	go func() {
		tick := time.NewTicker(poll)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			r, ok := w.next(read(), time.Now())
			if !ok {
				continue
			}
			select {
			case out <- r:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// setupWatch is WatchSetup's memory of the setup file between readings.
type setupWatch struct {
	quiet   time.Duration
	seen    SetupRead
	changed time.Time
	read    bool // some reading was taken
	sent    bool // seen was sent
}

// next takes a reading of the file at now, and says whether a reading is to
// be sent: the file's content once it has held it for quiet since it last
// changed, once.
func (w *setupWatch) next(r SetupRead, now time.Time) (SetupRead, bool) {
	if !w.read || !r.same(w.seen) {
		w.seen, w.changed, w.read, w.sent = r, now, true, false
	}
	if w.sent || now.Sub(w.changed) < w.quiet {
		return SetupRead{}, false
	}
	w.sent = true
	return w.seen, true
}

// setupKeep is how long a setup the coordinator no longer runs is kept for
// place runners started just before the change, which read it as they start.
const setupKeep = 24 * time.Hour

// writeSetup keeps cfg under stateDir for place runners to read, and returns
// its path. The file is named for its content and never rewritten, so a place
// runner always reads the setup it was started with, even when the setup
// changes as it starts.
func writeSetup(cfg *config.Config) (string, error) {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	sum := sha256.Sum256(b)
	dir := filepath.Join(cfg.StateDir, "setups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, hex.EncodeToString(sum[:8])+".json")
	if _, err := os.Stat(path); err != nil {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, path); err != nil {
			return "", err
		}
	}
	// A setup run again is kept as long as one run for the first time.
	now := time.Now()
	os.Chtimes(path, now, now)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		info, err := e.Info()
		if p := filepath.Join(dir, e.Name()); p != path && err == nil && time.Since(info.ModTime()) > setupKeep {
			os.Remove(p)
		}
	}
	return path, nil
}
