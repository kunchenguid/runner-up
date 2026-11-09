package agent

import (
	"context"
	"fmt"
	"testing"
)

// Intent (README, "Setup file", minAvailableMemoryMB): a Mac's free memory
// is its memory less what it has in use as Activity Monitor counts it, so
// file cache macOS can drop counts free, and a reading above the Mac's
// memory is refused rather than read as none free.
func TestMacMemoryIsWhatIsNotInUse(t *testing.T) {
	// A 16 GB Mac with 16 KB pages: 8 GB app memory (less 1 GB purgeable),
	// 2 GB wired and 1 GB compressed in use.
	const page = 16384
	pages := func(mb int) int { return mb << 20 / page }
	vmStat := fmt.Sprintf(`Mach Virtual Memory Statistics: (page size of %d bytes)
Pages free:                               %d.
Anonymous pages:                          %d.
Pages purgeable:                          %d.
Pages wired down:                         %d.
Pages occupied by compressor:             %d.
`, page, pages(2048), pages(8192), pages(1024), pages(2048), pages(1024))
	run := func(memsize string) tool {
		return func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "/usr/bin/vm_stat" {
				return []byte(vmStat), nil
			}
			return []byte("10\n" + memsize + "\n"), nil
		}
	}
	m, err := readMacMemory(context.Background(), run(fmt.Sprint(16<<30)))
	if err != nil || m.AvailableMB != 16384-10240 {
		t.Fatalf("read %+v, %v; want %d MB free", m, err, 16384-10240)
	}
	if m, err := readMacMemory(context.Background(), run(fmt.Sprint(8<<30))); err == nil {
		t.Fatalf("read %+v from more memory in use than the Mac has", m)
	}
}
