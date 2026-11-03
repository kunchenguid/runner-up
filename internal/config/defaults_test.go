package config

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// Intent (README.md, "Setup file"): each number the README's table gives as a
// field's default is what a setup that leaves the field out runs with. This
// test owns those defaults; other tests read them from a loaded setup, such
// as cfg.BootTimeoutSeconds, rather than type them.
func TestSetupDefaultsAreTheReadmes(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	c.Defaults()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var applied map[string]any
	if err := json.Unmarshal(b, &applied); err != nil {
		t.Fatal(err)
	}
	rows := regexp.MustCompile("(?m)^\\| `(\\w+)` \\| (\\d+) \\|").FindAllSubmatch(readme, -1)
	if len(rows) == 0 {
		t.Fatal("the README's setup file table gives no number as a default")
	}
	for _, row := range rows {
		field := string(row[1])
		want, _ := strconv.Atoi(string(row[2]))
		if got, ok := applied[field].(float64); !ok || int(got) != want {
			t.Errorf("a setup without %s runs with %v, and the README says %d", field, applied[field], want)
		}
	}
}
