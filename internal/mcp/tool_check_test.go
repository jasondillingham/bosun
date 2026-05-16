package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestCheck_RejectsOversizedPathsArray guards bosun_check against an agent
// dumping a directory listing into the paths field. The cost per call is
// O(paths × total-claims) string compares — a 10k entry list against even a
// modest claim set chews thousands of needless comparisons.
func TestCheck_RejectsOversizedPathsArray(t *testing.T) {
	_, sess, cancel := newPipedSession(t)
	defer cancel()

	huge := make([]string, maxCheckPaths+1)
	for i := range huge {
		huge[i] = "internal/x.go"
	}
	result, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "bosun_check",
		Arguments: map[string]any{"paths": huge},
	})
	if err == nil && !result.IsError {
		t.Fatalf("oversized paths array should be rejected, got %+v", result)
	}
}
