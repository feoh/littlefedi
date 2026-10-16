package memguard

import "golang.org/x/sys/unix"

// Totalram is uint32 on the 32-bit architectures and uint64 on the others, so
// both it and Unit are widened before the multiplication.
func physicalMemoryBytes() uint64 {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0
	}
	return uint64(info.Totalram) * uint64(info.Unit)
}
