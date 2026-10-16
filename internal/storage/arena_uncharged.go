//go:build !postgres && !illumos && !solaris

package storage

// Kernels that overcommit never make the WASM driver's 256MB per-connection
// reservation resident, so it costs address space and nothing else.
const sqliteReservationCharged = false
