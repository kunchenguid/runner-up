// Package ciguard holds tests that guard the repository's CI, release and
// install files. They read those files by repository-relative path, so every
// test runs from the repository root.
package ciguard

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if err := os.Chdir("../.."); err != nil {
		fmt.Fprintln(os.Stderr, "cannot enter repository root:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
