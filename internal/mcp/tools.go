// This file holds helpers shared by multiple tool implementations. Each
// individual tool lives in its own tool_*.go file and registers itself
// via registerTool() in its init().
package mcp

import "strings"

// pathsOverlap mirrors the package-internal claims.matches() rules used
// by `bosun status --with-overlaps` and `bosun_check`. Equality and
// directory containment count; glob handling is intentionally absent
// here in round 0 and added in round 2 when we unify the matcher.
func pathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	if isPrefixDir(a, b) || isPrefixDir(b, a) {
		return true
	}
	return false
}

// isPrefixDir reports whether prefix (as a directory) contains p.
func isPrefixDir(prefix, p string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return false
	}
	return strings.HasPrefix(p, prefix+"/")
}
