//go:build !postgres && (illumos || solaris)

package storage

// illumos charges an anonymous reservation against max-swap as soon as it is
// mapped, so the WASM driver's 256MB per-connection reservation is real memory
// pressure here rather than address space.
const sqliteReservationCharged = true
