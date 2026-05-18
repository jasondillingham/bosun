package main

import (
	"fmt"
	"io"
	"os"

	"github.com/jasondillingham/bosun/internal/briefscaffold"
	"github.com/spf13/cobra"
)

func newNewBriefCmd() *cobra.Command {
	var (
		pattern      string
		out          string
		listPatterns bool
	)

	cmd := &cobra.Command{
		Use:   "new-brief",
		Short: "Scaffold a starter plan markdown for a known pattern",
		Long: `Writes a ready-to-fill plan markdown to stdout (or to --out) for one of
four common bosun patterns. The output is a real starter brief — fill the
` + "`{{placeholders}}`" + ` and feed it to ` + "`bosun init --brief <plan>.md`" + `.

Patterns:
  recipe    — parent + spawned sub-sessions, shared-interface-up-front
  review    — multi-lane code review, notes-only
  audit     — multi-lane bug hunt, notes-only
  cleanup   — multi-lane mechanical refactor

Examples:
  bosun new-brief --pattern recipe                     # to stdout
  bosun new-brief --pattern audit --out my-audit.md    # to file
  bosun new-brief --list-patterns                      # show available patterns`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNewBrief(cmd.OutOrStdout(), pattern, out, listPatterns)
		},
	}

	cmd.Flags().StringVar(&pattern, "pattern", "", "pattern to scaffold (recipe|review|audit|cleanup)")
	cmd.Flags().StringVar(&out, "out", "", "write to this file instead of stdout")
	cmd.Flags().BoolVar(&listPatterns, "list-patterns", false, "list available patterns and exit")

	cmd.GroupID = "setup"
	return cmd
}

// runNewBrief is the testable command body. It handles --list-patterns
// first (no --pattern required), then validates --pattern, loads the
// embedded body, and writes to stdout (or --out).
func runNewBrief(w io.Writer, pattern, out string, listPatterns bool) error {
	if listPatterns {
		return runListPatterns(w)
	}
	if pattern == "" {
		return userErr("--pattern is required (or pass --list-patterns to see options)")
	}

	p, err := briefscaffold.Get(pattern)
	if err != nil {
		return userErr("%v", err)
	}

	if out == "" {
		if _, err := io.WriteString(w, p.Body); err != nil {
			return internalErr("write pattern to stdout", err)
		}
		return nil
	}

	if err := os.WriteFile(out, []byte(p.Body), 0o644); err != nil {
		return internalErr("write "+out, err)
	}
	fmt.Fprintf(w, "Wrote %s pattern to %s.\n", p.Name, out)
	fmt.Fprintf(w, "Next: review the plan, fill the {{placeholders}}, then run `bosun init --brief %s`.\n", out)
	return nil
}

func runListPatterns(w io.Writer) error {
	patterns, err := briefscaffold.Patterns()
	if err != nil {
		return internalErr("load patterns", err)
	}
	for _, p := range patterns {
		fmt.Fprintf(w, "%s: %s\n", p.Name, p.Description)
	}
	return nil
}
