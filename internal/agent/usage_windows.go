package agent

import (
	"fmt"
	"os"

	"github.com/kunchenguid/runner-up/internal/cli"
)

// The measurement wrapper runs only inside a macOS guest. Windows agents
// still honor each pool's configured reservation and provider limits.
func MeasureMain(args []string) int {
	fmt.Fprintln(os.Stderr, "runner-up agent measure runs only inside a macOS guest")
	if cli.Help(args) {
		return 0
	}
	return 2
}
