//go:build !windows

package agent

import (
	"context"
	"io"
)

type hypervProvider struct{}

var pipeEnds []error

func (hypervProvider) run(context.Context, *AgentRun, string, io.Writer) error { return errWindowsOnly }
func (hypervProvider) clean(context.Context, *AgentRun) error                  { return nil }

func hypervReady() string { return errWindowsOnly.Error() }
