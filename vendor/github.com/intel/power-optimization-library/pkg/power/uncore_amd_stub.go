//go:build !linux || !amd64 || !cgo

// Stub for the AMD E‑SMI bridge.
// This file is built and included when the target is not linux/amd64, or when cgo is disabled,
package power

func initAMDUncore() error { return nil }

func (u *uncoreFreq) writeAMD(_ uint) error { return nil }
