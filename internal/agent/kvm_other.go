//go:build !linux

package agent

import (
	"context"
	"errors"
	"io"
)

// KVM places run only on Linux.
type kvmProvider struct{}

var errUnsupported = errors.New("KVM places run only on Linux")

func (kvmProvider) run(context.Context, *AgentRun, string, io.Writer) error { return errUnsupported }
func (kvmProvider) clean(context.Context, *AgentRun) error                  { return nil }

func kvmReady() string { return errUnsupported.Error() }
