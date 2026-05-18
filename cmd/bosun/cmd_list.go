package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/jasondillingham/bosun/internal/status"
	"github.com/spf13/cobra"
	"strings"
)

// listJSON is the stable wire shape behind `bosun list --json`.
//
// Schema (versioned by status.JSONSchemaVersion; additive changes keep the
// same version, key renames or removals are breaking and bump it):
//
//	{
//	  "version":  string,    // status.JSONSchemaVersion
//	  "sessions": [
//	    { "name": string, "branch": string, "state": string }, ...
//	  ]
//	}
//
// `sessions` is always present (possibly empty) and ordered the same as
// `session.Derive`'s sort. `--ready` filters the slice to DONE sessions
// before emitting; the version key is unchanged either way.
type listJSON struct {
	Version  string            `json:"version"`
	Sessions []listSessionJSON `json:"sessions"`
}

type listSessionJSON struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	State  string `json:"state"`
}

func newListCmd() *cobra.Command {
	var (
		ready   bool
		jsonOut bool
		tree    bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Print session names, one per line",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd, ready, jsonOut, tree)
		},
	}

	cmd.Flags().BoolVar(&ready, "ready", false, "only print sessions marked DONE")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	cmd.Flags().BoolVar(&tree, "tree", false, "indent sub-sessions under their parent (default: flat for script consumption)")

	cmd.GroupID = "during"
	return cmd
}

func runList(cmd *cobra.Command, ready, jsonOut, tree bool) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}

	if jsonOut {
		payload := listJSON{
			Version:  status.JSONSchemaVersion,
			Sessions: make([]listSessionJSON, 0, len(sessions)),
		}
		for _, s := range sessions {
			if ready && s.State != session.StateDone {
				continue
			}
			payload.Sessions = append(payload.Sessions, listSessionJSON{
				Name:   s.Name,
				Branch: s.Branch,
				State:  string(s.State),
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			return internalErr("encode json", err)
		}
		return nil
	}

	if tree {
		// Enrich + walk hierarchy so children render under parents.
		// Failures fall through to the flat list — the tree is
		// advisory, not load-bearing for `bosun list`.
		likes := make([]spawntree.SessionLike, len(sessions))
		for i := range sessions {
			likes[i] = &sessions[i]
		}
		_ = spawntree.NewStore(rc.repoRoot).EnrichSessions(likes)
	}

	for _, s := range sessions {
		if ready && s.State != session.StateDone {
			continue
		}
		name := s.Name
		if tree && s.Depth > 0 {
			name = strings.Repeat("  ", s.Depth) + "└─ " + s.Name
		}
		fmt.Fprintln(os.Stdout, name)
	}
	return nil
}
