//go:build dragonfly || freebsd || netbsd || openbsd

package memguard

import "golang.org/x/sys/unix"

// NetBSD and OpenBSD publish hw.physmem64 because hw.physmem truncates on a
// machine with more memory than a 32-bit quantity can hold. FreeBSD and
// DragonFly only have hw.physmem, and its width follows the architecture, so
// the 32-bit read is tried after the 64-bit one fails on i386 and arm.
func physicalMemoryBytes() uint64 {
	for _, name := range []string{"hw.physmem64", "hw.physmem"} {
		if n, err := unix.SysctlUint64(name); err == nil && n > 0 {
			return n
		}
		if n, err := unix.SysctlUint32(name); err == nil && n > 0 {
			return uint64(n)
		}
	}
	return 0
}
