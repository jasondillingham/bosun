package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jasondillingham/bosun/internal/usage"
	"github.com/spf13/cobra"
)

// newCostCmd builds `bosun cost` — the per-repo rollup over the
// Phase 4 cost ledger. Sibling to `bosun show <session>`'s
// per-session usage block, but works across every session bosun has
// tracked (including merged-but-not-cleaned-yet ones whose ledger
// still lives in .bosun/state/<label>.usage).
//
// Default output is a single line: total cost + token counts +
// turn count. --by-session breaks it down into a table. --by-day
// groups entries by their UTC date. --since filters to entries
// newer than the given duration. --json emits the structured
// payload for piping into a collector.
//
// 2026-05 follow-up grind item #96: closes the Phase 4 gap where
// per-session budgets shipped but no aggregate view existed.
func newCostCmd() *cobra.Command {
	var (
		byKindFlag string // session|day|""
		sinceFlag  string
		jsonOut    bool
		sessionArg string
	)
	cmd := &cobra.Command{
		Use:   "cost",
		Short: "Roll up LLM cost + token usage across sessions",
		Long: `Aggregate the per-session usage ledgers (.bosun/state/<label>.usage)
into one view.

Default shows the round-wide total: cost, tokens in/out, and turn
count across every session bosun has tracked. Use --by=session or
--by=day to break it down; --since to filter to recent activity;
--json for machine-readable output.

Operators tracking spend over time pipe --json into a collector;
operators tracking spend over a session round (the usual case) use
the plain-text default.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := costOpts{
				by:      strings.TrimSpace(byKindFlag),
				since:   strings.TrimSpace(sinceFlag),
				jsonOut: jsonOut,
				session: strings.TrimSpace(sessionArg),
			}
			return runCost(opts)
		},
	}
	cmd.Flags().StringVar(&byKindFlag, "by", "", "breakdown axis: \"session\" (per-label table) or \"day\" (UTC date grouping). Empty = round-wide total only.")
	cmd.Flags().StringVar(&sinceFlag, "since", "", "filter to entries newer than this duration (e.g. \"7d\", \"24h\", \"30m\")")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON for piping; suppresses the human-readable table")
	cmd.Flags().StringVar(&sessionArg, "session", "", "limit to one session label (e.g. session-1, auth)")
	cmd.GroupID = "during"
	return cmd
}

type costOpts struct {
	by      string
	since   string
	jsonOut bool
	session string
}

// costRow is one line of the breakdown — either one session's
// totals (--by=session) or one date's totals (--by=day). The
// default no-breakdown render uses a single roundRow.
type costRow struct {
	Key       string  `json:"key"` // session label OR YYYY-MM-DD date
	CostUSD   float64 `json:"cost_usd"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	TurnCount int     `json:"turn_count"`
}

// costPayload is the JSON shape `--json` emits. Wraps the rows
// with a round-wide total so a collector doesn't have to re-sum.
type costPayload struct {
	By         string    `json:"by,omitempty"`     // "session" | "day" | "" for round-wide-only
	Since      string    `json:"since,omitempty"`  // echo of --since
	GeneratedAt time.Time `json:"generated_at"`
	Total      costRow   `json:"total"`
	Rows       []costRow `json:"rows,omitempty"`
}

func runCost(opts costOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	if opts.by != "" && opts.by != "session" && opts.by != "day" {
		return userErr("--by must be \"session\" or \"day\" (got %q)", opts.by)
	}

	var cutoff time.Time
	if opts.since != "" {
		d, err := parseCostSince(opts.since)
		if err != nil {
			return userErr("%v", err)
		}
		cutoff = time.Now().Add(-d)
	}

	// Determine which sessions to read. --session limits to one;
	// otherwise we pick up every session label with a ledger on
	// disk (including ones that were merged and cleared from
	// state, since .usage is preserved on Clear historically —
	// actually 2026-05 #B3 added heartbeat clearing but usage
	// is also cleared at merge time, so we only see live ones
	// here. That's fine — that's the operator's current round.)
	var labels []string
	if opts.session != "" {
		labels = []string{opts.session}
	} else {
		labels, err = usage.ListSessions(rc.repoRoot)
		if err != nil {
			return internalErr("list usage sessions", err)
		}
	}

	// Pull entries once. We'll re-aggregate them into the requested
	// breakdown shape downstream — for round-wide-only we'd compute
	// less, but the cost (sub-ms for sub-1000 entries) isn't worth
	// the code complexity to avoid.
	type sessionEntries struct {
		label   string
		entries []usage.Entry
	}
	var all []sessionEntries
	for _, label := range labels {
		es, err := usage.Read(rc.repoRoot, label)
		if err != nil {
			return internalErr("read usage "+label, err)
		}
		// Apply --since filter at read time so downstream
		// aggregation doesn't have to know about it.
		if !cutoff.IsZero() {
			filtered := es[:0]
			for _, e := range es {
				if e.Timestamp.After(cutoff) {
					filtered = append(filtered, e)
				}
			}
			es = filtered
		}
		if len(es) > 0 {
			all = append(all, sessionEntries{label: label, entries: es})
		}
	}

	// Build the payload (round-wide total always; rows when --by
	// is set).
	payload := costPayload{
		By:          opts.by,
		Since:       opts.since,
		GeneratedAt: time.Now().UTC(),
	}
	for _, s := range all {
		for _, e := range s.entries {
			payload.Total.CostUSD += e.CostUSD
			payload.Total.TokensIn += e.TokensIn
			payload.Total.TokensOut += e.TokensOut
			payload.Total.TurnCount++
		}
	}
	switch opts.by {
	case "session":
		for _, s := range all {
			var row costRow
			row.Key = s.label
			for _, e := range s.entries {
				row.CostUSD += e.CostUSD
				row.TokensIn += e.TokensIn
				row.TokensOut += e.TokensOut
				row.TurnCount++
			}
			payload.Rows = append(payload.Rows, row)
		}
	case "day":
		// Map of YYYY-MM-DD → aggregated row. Sort by key for a
		// stable, chronological table.
		byDay := map[string]*costRow{}
		for _, s := range all {
			for _, e := range s.entries {
				k := e.Timestamp.UTC().Format("2006-01-02")
				row, ok := byDay[k]
				if !ok {
					row = &costRow{Key: k}
					byDay[k] = row
				}
				row.CostUSD += e.CostUSD
				row.TokensIn += e.TokensIn
				row.TokensOut += e.TokensOut
				row.TurnCount++
			}
		}
		keys := make([]string, 0, len(byDay))
		for k := range byDay {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			payload.Rows = append(payload.Rows, *byDay[k])
		}
	}

	if opts.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}
	return renderCostText(payload)
}

// renderCostText writes the human-readable form. Round-wide total
// always emits; --by-session / --by-day emit a tab-aligned table
// below the total.
func renderCostText(p costPayload) error {
	if p.Total.TurnCount == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "bosun cost: no usage recorded for this filter.")
		_, _ = fmt.Fprintln(os.Stdout, "  (agent runtimes record cost via the bosun_usage MCP tool — see docs/mcp-protocol.md)")
		return nil
	}
	scope := "across all sessions"
	if p.Since != "" {
		scope = "since " + p.Since
	}
	_, _ = fmt.Fprintf(os.Stdout, "Cost %s: $%.4f over %d turn(s) (%d tokens in / %d tokens out)\n",
		scope, p.Total.CostUSD, p.Total.TurnCount, p.Total.TokensIn, p.Total.TokensOut)

	if len(p.Rows) == 0 {
		return nil
	}
	_, _ = fmt.Fprintln(os.Stdout)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	header := "SESSION"
	if p.By == "day" {
		header = "DATE"
	}
	_, _ = fmt.Fprintf(tw, "%s\tCOST\tTURNS\tTOKENS_IN\tTOKENS_OUT\n", header)
	for _, r := range p.Rows {
		_, _ = fmt.Fprintf(tw, "%s\t$%.4f\t%d\t%d\t%d\n",
			r.Key, r.CostUSD, r.TurnCount, r.TokensIn, r.TokensOut)
	}
	return tw.Flush()
}

// parseCostSince accepts a duration string in Go's time.ParseDuration
// shape plus the operator-friendly "Nd" form (days) that time.ParseDuration
// doesn't natively support. "7d" → 168h. Negative values and zero are
// rejected — --since 0 is meaningless and -7d would filter to the
// future. 2026-05 follow-up #96.
func parseCostSince(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("--since requires a duration like \"7d\" or \"24h\"")
	}
	// Handle the "Nd" form first by translating to hours.
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--since: %v (try forms like \"7d\", \"24h\", \"30m\")", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("--since must be a positive duration (got %v)", d)
	}
	return d, nil
}
