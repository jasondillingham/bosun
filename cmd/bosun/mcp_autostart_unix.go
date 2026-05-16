//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// applyMcpDaemonAttrs puts the spawned MCP daemon in its own session so
// it survives bosun init's exit and doesn't catch the parent's SIGINT
// when the operator Ctrl-C's the init run.
func applyMcpDaemonAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
