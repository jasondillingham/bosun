package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools wires every bosun tool onto the underlying *mcp.Server.
// Round 0 ships only `bosun_check` (read-only). Round 1 will add claim /
// release / done / stuck / announce in parallel sessions.
func (s *Server) registerTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "bosun_check",
		Description: "Check whether any of the given paths are claimed by other bosun sessions before you start editing. Returns a list of conflicts; empty list means safe to proceed.",
	}, s.toolCheck)
}

// CheckArgs is the input schema for bosun_check. Struct tags drive the
// JSON Schema the SDK exposes to the client.
type CheckArgs struct {
	Paths []string `json:"paths" jsonschema:"paths the caller intends to edit (repo-root relative)"`
}

// CheckResult is the structured output. The SDK auto-builds a JSON Schema
// from struct tags for the response too.
type CheckResult struct {
	Conflicts []CheckConflict `json:"conflicts"`
}

// CheckConflict represents one path claimed by one or more sessions.
type CheckConflict struct {
	Path     string   `json:"path" jsonschema:"the claimed path"`
	Sessions []string `json:"sessions" jsonschema:"sessions currently claiming this path"`
}

// toolCheck implements bosun_check. It queries the shared claims store and
// reports any caller-supplied path that overlaps an existing claim. The
// "what counts as overlap" rule matches claims.matches() — directory
// containment, equality, and glob matching all detect collisions.
func (s *Server) toolCheck(_ context.Context, _ *mcp.CallToolRequest, args CheckArgs) (*mcp.CallToolResult, CheckResult, error) {
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
		// Use the same normalization as claims.Add so overlap detection
		// behaves consistently whether the path comes from CLI or MCP.
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

	// The CallToolResult.Content is a human-readable summary; the typed
	// CheckResult is what the SDK serializes into the structured response
	// the agent's tool-use layer parses.
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

// pathsOverlap mirrors the package-internal claims.matches() rules. We
// duplicate the logic instead of exporting it to keep the claims package's
// public surface narrow during the round-0 spike. Round 1 should fold
// these into a shared helper once we've seen all the call sites.
func pathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	if isPrefixDir(a, b) || isPrefixDir(b, a) {
		return true
	}
	return false
}

func isPrefixDir(prefix, p string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return false
	}
	return strings.HasPrefix(p, prefix+"/")
}

// claimsAll is a thin alias kept here so other tool files (round 1) can
// reuse the same accessor without each touching the underlying store
// directly. Lets us swap the implementation later without churning every
// tool handler.
func (s *Server) claimsAll() (map[string]*claims.Claim, error) {
	return s.claims.All()
}
