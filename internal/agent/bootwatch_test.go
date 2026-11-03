package agent

import (
	"context"
	"testing"
	"time"
)

// Intent (README, "Macs", "A clone that restarts"): once a job runs in a tart
// clone, the agent reads the clone's boot session every guestCheckSeconds
// and ends the job when a new boot session shows the clone's macOS
// restarted, taking its runner with it. A reading that fails, or is no boot
// session, ends nothing, so a clone too busy to answer is never taken for a
// restarted one.
func TestOnlyANewBootSessionEndsTheJob(t *testing.T) {
	// kern.bootsessionuuid as a clone of macOS 15 prints it.
	const boot = "6C1F5C0B-0A3E-4F3B-9E43-2D9C5D0B1A77"
	const reboot = "0F9A2B4C-7D1E-4C2A-8B5F-3E6D7A8B9C0D"
	for _, tc := range []struct {
		why      string
		readings []string
		want     bool
	}{
		{"the clone restarted", []string{boot, boot, reboot}, true},
		{"too busy to answer at first", []string{"", "", boot, reboot}, true},
		{"failed and odd readings", []string{boot, "", "sysctl: unknown oid 'kern.bootsessionuuid'", boot}, false},
		{"the same boot throughout", []string{boot, boot, boot, boot}, false},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		i := 0
		read := func(context.Context) string {
			if i >= len(tc.readings) {
				cancel()
				return ""
			}
			i++
			return tc.readings[i-1]
		}
		if got := WatchBootSession(ctx, read, time.Millisecond); got != tc.want {
			t.Errorf("%s: the watch said restarted=%v, want %v", tc.why, got, tc.want)
		}
		cancel()
	}
}
