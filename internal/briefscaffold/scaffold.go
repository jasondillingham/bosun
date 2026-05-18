// Package briefscaffold serves the embedded starter-brief templates that
// `bosun new-brief --pattern <name>` writes to stdout (or to a file).
//
// Each pattern ships as a real, useful starter brief — not a stub. An
// operator runs `bosun new-brief --pattern <name>`, fills a handful of
// `{{placeholder}}` markers, and the result is ready to feed into
// `bosun init --brief <plan>.md`.
//
// Patterns are embedded into the binary via `//go:embed` so the command
// works in an offline / network-isolated install with no external fetch.
package briefscaffold

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
)

//go:embed patterns/*.md
var patternFS embed.FS

// Pattern is one named starter brief. Name is the value passed to
// `--pattern`; Description is the one-line summary surfaced by
// `--list-patterns`; Body is the raw markdown that `new-brief` writes.
type Pattern struct {
	Name        string
	Description string
	Body        string
}

// patterns is the registry surfaced by Patterns / Get / Names. Order
// here drives the order of `--list-patterns` output; intentionally not
// alphabetical so the most common pattern (recipe) is listed first.
var patterns = []patternMeta{
	{
		name:        "recipe",
		filename:    "recipe.md",
		description: "command-mode spawn brief; parent + N sub-sessions, shared-interface-up-front",
	},
	{
		name:        "review",
		filename:    "review.md",
		description: "multi-lane code review; each lane reviews a different path, writes notes-only",
	},
	{
		name:        "audit",
		filename:    "audit.md",
		description: "multi-lane bug hunt; each lane focuses on a different class of issue, notes-only",
	},
	{
		name:        "cleanup",
		filename:    "cleanup.md",
		description: "multi-lane refactor; each lane owns one cross-cutting subsystem, no shared types",
	},
}

type patternMeta struct {
	name        string
	filename    string
	description string
}

// Get returns the pattern named name, or an error listing the available
// names when no such pattern is registered.
func Get(name string) (Pattern, error) {
	for _, m := range patterns {
		if m.name == name {
			body, err := readPattern(m.filename)
			if err != nil {
				return Pattern{}, err
			}
			return Pattern{Name: m.name, Description: m.description, Body: body}, nil
		}
	}
	return Pattern{}, fmt.Errorf("unknown pattern %q (available: %s)", name, joinNames(patternNames()))
}

// Patterns returns every registered pattern with its body loaded.
// Useful for `--list-patterns` and for tests that iterate the set.
func Patterns() ([]Pattern, error) {
	out := make([]Pattern, 0, len(patterns))
	for _, m := range patterns {
		body, err := readPattern(m.filename)
		if err != nil {
			return nil, err
		}
		out = append(out, Pattern{Name: m.name, Description: m.description, Body: body})
	}
	return out, nil
}

// Names returns the registered pattern names in registry order. Cheap —
// doesn't load any pattern bodies.
func Names() []string {
	return patternNames()
}

func patternNames() []string {
	names := make([]string, len(patterns))
	for i, m := range patterns {
		names[i] = m.name
	}
	return names
}

func readPattern(filename string) (string, error) {
	data, err := fs.ReadFile(patternFS, "patterns/"+filename)
	if err != nil {
		return "", fmt.Errorf("read embedded pattern %s: %w", filename, err)
	}
	return string(data), nil
}

// joinNames builds a stable comma-separated list for error messages.
// Sorted so the error text is deterministic even if patterns is
// re-ordered at the call site.
func joinNames(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	out := ""
	for i, n := range sorted {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
