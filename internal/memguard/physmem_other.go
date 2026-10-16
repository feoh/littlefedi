//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package memguard

// Everywhere else, including illumos and Solaris, the host's memory is reported
// as unknown on purpose.
//
// sysconf(_SC_PHYS_PAGES) inside a zone returns the global zone's memory, not
// the zone's own cap, and a ceiling that is too high by an order of magnitude
// is worse than no ceiling at all: it would license the watchdog to raise the
// limit far past anything the zone can hold. Small zones are protected by the
// floor of the calculation, which counts the arena and the cache budget, and
// that part needs no help from the host.
func physicalMemoryBytes() uint64 { return 0 }
