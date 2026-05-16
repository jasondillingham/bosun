package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name:        "bosun_check",
			Description: "Check whether any of the given paths are claimed by other bosun sessions before you start editing. Returns a list of conflicts; empty list means safe to proceed.",
		}, s.toolCheck)
	})
}

// CheckArgs is the input schema for bosun_check.
type CheckArgs struct {
	Paths []string `json:"paths" jsonschema:"paths the caller intends to edit (repo-root relative)"`
}

// CheckResult is the structured output for bosun_check.
type CheckResult struct {
	Conflicts []CheckConflict `json:"conflicts"`
}

// CheckConflict represents one path claimed by one or more sessions.
type CheckConflict struct {
	Path     string   `json:"path" jsonschema:"the claimed path"`
	Sessions []string `json:"sessions" jsonschema:"sessions currently claiming this path"`
}

// maxCheckPaths caps the number of paths a single bosun_check call may
// supply. The cost is O(paths × total-claimed-paths) string compares;
// even at the cap it's microseconds, but rejecting absurd inputs early
// is friendlier than letting an agent silently send the equivalent of a
// directory listing.
const maxCheckPaths = 1024

// toolCheck implements bosun_check. Queries the shared claims store and
// reports any caller-supplied path that overlaps an existing claim.
// Overlap rules mirror claims.matches(): equality + directory containment.
func (s *Server) toolCheck(_ context.Context, _ *mcp.CallToolRequest, args CheckArgs) (*mcp.CallToolResult, CheckResult, error) {
	if len(args.Paths) > maxCheckPaths {
		return nil, CheckResult{}, fmt.Errorf("paths length %d exceeds limit %d", len(args.Paths), maxCheckPaths)
	}
	all, err := s.claims.All()
	if err != nil {
		return nil, CheckResult{}, fmt.Errorf("read claims: %w", err)
	}

	conflicts := make([]CheckConflict, 0, len(args.Paths))
	for _, want := range args.Paths {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		// Match the CLI's claims-normalization so MCP callers and CLI
		// callers see the same overlap semantics.
		want = strings.ReplaceAll(want, `\`, "/")
		want = strings.TrimPrefix(want, "./")

		hit := []string{}
		for session, c := range all {
			for _, claimed := range c.Paths {
				if pathsOverlap(want, claimed) {
					hit = append(hit, session)
					break
				}
			}
		}
		if len(hit) > 0 {
			conflicts = append(conflicts, CheckConflict{Path: want, Sessions: hit})
		}
	}

	summary := "no conflicts"
	if len(conflicts) > 0 {
		parts := make([]string, 0, len(conflicts))
		for _, c := range conflicts {
			parts = append(parts, fmt.Sprintf("%s (claimed by %s)", c.Path, strings.Join(c.Sessions, ", ")))
		}
		summary = strings.Join(parts, "; ")
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: summary},
		},
	}, CheckResult{Conflicts: conflicts}, nil
}
