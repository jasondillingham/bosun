package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// newAuditCmd builds `bosun audit` — a read-only surface over the
// JSON-lines audit logs at .bosun/audit/. Phase 5 #65 polish: until
// now operators had to `cat .bosun/audit/spawn.log | jq` to inspect
// gate decisions, which is fine when you know the file is there and
// know jq, and useless otherwise. This command makes the logs
// discoverable and adds the filters operators actually want at
// inspection time (tail, by-session, refused-only).
//
// Read-only by design: the MCP server is the only writer, so a CLI
// surface that mutates the log would just introduce drift hazards.
func newAuditCmd() *cobra.Command {
	var (
		kindFlag    string
		tailFlag    int
		jsonFlag    bool
		sessionFlag string
		outcomeFlag string
	)
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Read the spawn / sub-task audit logs",
		Long: `Inspect .bosun/audit/spawn.log and .bosun/audit/subtask.log.

Both logs are append-only JSON-lines emitted by the MCP server every
time a bosun_spawn or bosun_subtask call is granted or refused.
Useful for debugging "why didn't my agent spawn?" and for after-the-
fact incident review.

Without --kind, both logs are read and merged in time order. Use
--tail N to limit output to the most recent N entries; combine with
--session or --outcome=refused to focus the slice.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := auditOpts{
				kind:    strings.TrimSpace(kindFlag),
				tail:    tailFlag,
				json:    jsonFlag,
				session: strings.TrimSpace(sessionFlag),
				outcome: strings.TrimSpace(outcomeFlag),
			}
			return runAudit(opts)
		},
	}
	cmd.Flags().StringVar(&kindFlag, "kind", "all", "which log: spawn | subtask | all")
	cmd.Flags().IntVar(&tailFlag, "tail", 0, "show only the last N entries (0 = all)")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit raw JSON lines instead of the human-friendly table")
	cmd.Flags().StringVar(&sessionFlag, "session", "", "filter to entries whose parent or requested_label matches this session label")
	cmd.Flags().StringVar(&outcomeFlag, "outcome", "", "filter to entries with this outcome (e.g. granted, refused)")
	cmd.GroupID = "wiring"
	return cmd
}

type auditOpts struct {
	kind    string
	tail    int
	json    bool
	session string
	outcome string
}

// auditRow is the shared shape we render. Both spawn.log and
// subtask.log already use the same JSON keys (see subtask_audit.go's
// "extend, don't fork" comment), so a single decode target works for
// both. We add Kind to disambiguate when merging.
type auditRow struct {
	Time           string `json:"time"`
	Parent         string `json:"parent"`
	RequestedLabel string `json:"requested_label,omitempty"`
	Outcome        string `json:"outcome"`
	RefusalGate    string `json:"refusal_gate,omitempty"`
	RefusalMessage string `json:"refusal_message,omitempty"`
	Kind           string `json:"kind,omitempty"`
}

func runAudit(opts auditOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	if opts.kind == "" {
		opts.kind = "all"
	}

	var rows []auditRow
	wantSpawn := opts.kind == "all" || opts.kind == "spawn"
	wantSubtask := opts.kind == "all" || opts.kind == "subtask"
	if !wantSpawn && !wantSubtask {
		return userErr("--kind must be one of: spawn, subtask, all (got %q)", opts.kind)
	}

	if wantSpawn {
		r, err := readAuditFile(filepath.Join(rc.repoRoot, ".bosun", "audit", "spawn.log"), "spawn")
		if err != nil {
			return internalErr("read spawn audit", err)
		}
		rows = append(rows, r...)
	}
	if wantSubtask {
		r, err := readAuditFile(filepath.Join(rc.repoRoot, ".bosun", "audit", "subtask.log"), "subtask")
		if err != nil {
			return internalErr("read subtask audit", err)
		}
		rows = append(rows, r...)
	}

	rows = filterAuditRows(rows, opts)

	if opts.tail > 0 && len(rows) > opts.tail {
		rows = rows[len(rows)-opts.tail:]
	}

	if opts.json {
		enc := json.NewEncoder(os.Stdout)
		for _, r := range rows {
			if err := enc.Encode(r); err != nil {
				return internalErr("encode json", err)
			}
		}
		return nil
	}

	if len(rows) == 0 {
		printf("bosun: no audit entries match\n")
		return nil
	}

	// Plain-text columnar layout. Don't reach for text/tabwriter here
	// because the refusal_message column is highly variable and
	// alignment-on-final-column doesn't add much. Fixed-width prefix +
	// free-form tail is easier to grep.
	for _, r := range rows {
		out := fmt.Sprintf("%-20s  %-8s  %-9s  %-15s",
			truncate(r.Time, 19), r.Kind, r.Outcome, truncate(r.Parent, 15))
		if r.RequestedLabel != "" {
			out += "  →" + r.RequestedLabel
		}
		if r.RefusalGate != "" {
			out += "  gate=" + r.RefusalGate
		}
		if r.RefusalMessage != "" {
			out += "  " + r.RefusalMessage
		}
		printf("%s\n", out)
	}
	return nil
}

// readAuditFile decodes a JSON-lines audit log and tags each row with
// the supplied kind. A non-existent log is not an error (an audit
// surface that errors when nothing's happened yet is hostile UX);
// callers see an empty slice.
func readAuditFile(path, kind string) ([]auditRow, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var rows []auditRow
	scanner := bufio.NewScanner(f)
	// Audit lines can be large when refusal_message is verbose. The
	// default 64KB cap is too small for ~10 large lines; bump to 1MB
	// which still fits comfortably in process memory for a tail.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var r auditRow
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			// Skip malformed lines silently rather than failing the
			// whole read. Lines are append-atomic under PIPE_BUF, so
			// a torn line is the only realistic cause and it should
			// be rare; we'd rather show the operator what we have
			// than refuse the whole audit.
			continue
		}
		r.Kind = kind
		rows = append(rows, r)
	}
	if err := scanner.Err(); err != nil {
		return rows, err
	}
	return rows, nil
}

// filterAuditRows applies the --session and --outcome filters in one
// pass and returns the surviving rows in their original order. Time-
// merging across kinds isn't needed: both logs are written in time
// order, and the operator typically reads each kind separately or
// with --tail which gives the most recent regardless of source.
func filterAuditRows(rows []auditRow, opts auditOpts) []auditRow {
	if opts.session == "" && opts.outcome == "" {
		return rows
	}
	out := make([]auditRow, 0, len(rows))
	for _, r := range rows {
		if opts.session != "" && r.Parent != opts.session && r.RequestedLabel != opts.session {
			continue
		}
		if opts.outcome != "" && r.Outcome != opts.outcome {
			continue
		}
		out = append(out, r)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
