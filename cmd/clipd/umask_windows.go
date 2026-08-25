//go:build windows

package main

// restrictUmask is a no-op on Windows, which has no umask. The daemon does not
// run there; this exists so the package still builds for cross-compilation
// checks.
func restrictUmask() {}
