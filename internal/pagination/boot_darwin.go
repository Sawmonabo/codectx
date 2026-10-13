//go:build darwin

package pagination

import "golang.org/x/sys/unix"

// bootIdentity reads the kernel's boot session UUID, generated once per boot
// (sysctl kern.bootsessionuuid). The boot time sysctl is not used: it moves
// when the wall clock is stepped.
func bootIdentity() string {
	id, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return ""
	}
	return id
}
