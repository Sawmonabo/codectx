//go:build linux

package pagination

import (
	"os"
	"strings"
)

// bootIdentity reads the kernel's boot id, a random UUID generated once per
// boot (random(4), /proc/sys/kernel/random/boot_id).
func bootIdentity() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
