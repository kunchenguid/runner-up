package agent

// The measurement wrapper runs only inside a macOS guest. Windows agents
// still honor each pool's configured reservation and provider limits.
func MeasureMain([]string) int { return 2 }
