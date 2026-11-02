package agent

import (
	"encoding/json"
	"math"
)

// Usage is an observation, not a limit or a recommendation. Peaks are sampled;
// RSS sums resident process pages, including shared pages more than once.
// Missing or failed readings are never represented as measured zero use.
type Usage struct {
	Scope        string  `json:"scope"`
	Samples      int     `json:"samples"`
	PeakCores    float64 `json:"peakCores"`
	PeakMemoryMB float64 `json:"peakMemoryMB"`
	Seconds      float64 `json:"seconds"`
	Incomplete   bool    `json:"incomplete,omitempty"`
}

const UsagePrefix = "runner-up usage: "

// ParseUsage accepts only bounded, finite observations. Runner output is not
// trusted: these readings inform a person, never change configured sizes.
func ParseUsage(line string) (Usage, bool) {
	var u Usage
	if len(line) > 4096 || json.Unmarshal([]byte(line), &u) != nil {
		return u, false
	}
	valid := func(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 }
	return u, (u.Scope == "process-group" || u.Scope == "guest-processes") && u.Samples > 0 &&
		valid(u.PeakCores) && valid(u.PeakMemoryMB) && valid(u.Seconds)
}
