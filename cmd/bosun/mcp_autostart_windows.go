//go:build windows

package main

import "os/exec"

// applyMcpDaemonAttrs is a no-op on Windows. The MCP daemon path itself
// is Unix-socket-bound and not exercised on Windows builds today; this
// stub keeps the package compiling.
func applyMcpDaemonAttrs(*exec.Cmd) {}
