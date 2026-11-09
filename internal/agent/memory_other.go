//go:build !linux && !darwin

package agent

import (
	"context"
	"errors"
)

// Free memory is read only on Linux and macOS; the setup refuses a floor on
// a Windows machine.
func readMemory(context.Context, string) (Memory, error) {
	return Memory{}, errors.New("free-memory readings need Linux or macOS")
}
