//go:build !linux

package main

// clearAmbientCaps is a no-op off Linux: ambient capabilities are a Linux
// concept, and only the NixOS module ever grants lab one (issue #106).
func clearAmbientCaps() (uint64, error) { return 0, nil }
