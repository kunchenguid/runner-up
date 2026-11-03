//go:build !windows && !darwin

package agent

import (
	"context"
	"errors"
)

// Guest readings come from macOS's own tools: only tart guests are read.
func guestReader() func(context.Context) (sample, error) {
	return func(context.Context) (sample, error) {
		return sample{}, errors.New("guest readings need macOS")
	}
}
