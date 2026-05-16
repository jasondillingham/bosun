//go:build windows

package main

// withMcpSpawnLock is a no-op on Windows. The MCP daemon path is bound to
// Unix sockets and isn't exercised on Windows builds today; if/when it
// is, swap this for a LockFileEx-based equivalent of the unix flock.
func withMcpSpawnLock(_ string, fn func() (mcpServerInfo, error)) (mcpServerInfo, error) {
	return fn()
}
