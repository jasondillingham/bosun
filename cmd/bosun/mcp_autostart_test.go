package main

import (
	"os"
	"path/filepath"
	"testing"

	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
)

// TestInheritedSocketBelongsToRepo covers the three paths in the helper:
// the inherited socket equals this repo's default socket path; the
// inherited socket is recorded in this repo's pidfile; the inherited
// socket belongs to a different repo. Cross-repo bug: ensureMcp used to
// blindly trust any live inherited socket, so an agent launched against
// repo A while a parent shell still had repo B's BOSUN_MCP_SOCK set would
// talk to the wrong daemon. The helper rejects that case.
func TestInheritedSocketBelongsToRepo(t *testing.T) {
	repoA := t.TempDir()
	repoB := t.TempDir()

	defaultA := bosunmcp.DefaultSocketPath(repoA)
	defaultB := bosunmcp.DefaultSocketPath(repoB)

	if inheritedSocketBelongsToRepo("", repoA) {
		t.Fatalf("empty socket should not match")
	}
	if !inheritedSocketBelongsToRepo(defaultA, repoA) {
		t.Fatalf("default path for repoA must match repoA")
	}
	if inheritedSocketBelongsToRepo(defaultB, repoA) {
		t.Fatalf("default path for repoB must NOT match repoA")
	}

	// Pidfile-recorded match: write repoA's pidfile pointing at a custom
	// socket and confirm it's accepted for repoA but not repoB.
	customSock := filepath.Join(t.TempDir(), "custom.sock")
	pidfilePath := filepath.Join(repoA, ".bosun", "mcp.pid")
	if err := os.MkdirAll(filepath.Dir(pidfilePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidfilePath, []byte("12345\n"+customSock+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !inheritedSocketBelongsToRepo(customSock, repoA) {
		t.Fatalf("custom socket recorded in repoA's pidfile must match repoA")
	}
	if inheritedSocketBelongsToRepo(customSock, repoB) {
		t.Fatalf("repoA's pidfile must not authorize repoB")
	}
}
