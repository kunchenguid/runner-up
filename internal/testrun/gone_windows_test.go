package testrun

import "testing"

// childEnds checks nothing on Windows, which has no process groups for
// Command to end; there it ends only the program it started.
func childEnds(*testing.T, int) {}
