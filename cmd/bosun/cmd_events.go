package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jasondillingham/bosun/internal/events"
	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
	"github.com/spf13/cobra"
)

// newEventsCmd builds the `bosun events` subcommand. It connects to a
// running `bosun serve` instance, subscribes to the SSE stream at
// /api/events, and pretty-prints events as they arrive — the terminal
// counterpart to the browser dashboard.
func newEventsCmd() *cobra.Command {
	opts := eventsOpts{}

	cmd := &cobra.Command{
		Use:   "events",
		Short: "Tail the event stream from a running `bosun serve`",
		Long: `Connect to the SSE stream exposed by ` + "`bosun serve`" + ` at /api/events
and pretty-print each event with a timestamp + session label.

By default the URL is auto-detected from .bosun/serve.pid (written by
` + "`bosun serve`" + ` on startup). Pass --url to override.

Examples:
  bosun events --tail                          # tail forever
  bosun events --tail --since 5m               # backfill last 5m, then tail
  bosun events --tail --filter session-1       # only events for session-1
  bosun events --tail --json                   # one JSON line per event
  bosun events --once                          # emit one event, exit`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEvents(cmd, opts)
		},
	}

	cmd.Flags().BoolVar(&opts.tail, "tail", false, "stream events as they arrive")
	cmd.Flags().BoolVar(&opts.once, "once", false, "print one event then exit")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit one JSON line per event")
	cmd.Flags().StringVar(&opts.filter, "filter", "", "only show events whose session matches this label")
	cmd.Flags().DurationVar(&opts.since, "since", 0, "drop backfill events older than this duration (e.g. 5m, 1h)")
	cmd.Flags().StringVar(&opts.url, "url", "", "SSE URL (default: read from .bosun/serve.pid)")

	cmd.GroupID = "during"
	return cmd
}

type eventsOpts struct {
	tail    bool
	once    bool
	jsonOut bool
	filter  string
	since   time.Duration
	url     string
}

func runEvents(cmd *cobra.Command, opts eventsOpts) error {
	// --once and --tail are exclusive: --once means "one event then
	// exit"; --tail means "stream forever". Defaulting to --tail when
	// neither is set would surprise scripts that just type `bosun
	// events` expecting a snapshot. Refuse the ambiguous case.
	if opts.once && opts.tail {
		return userErr("--once and --tail are mutually exclusive")
	}
	if !opts.once && !opts.tail {
		return userErr("pass --tail to stream or --once to emit a single event")
	}

	rc, err := loadCtx()
	if err != nil {
		return err
	}

	endpoint, err := resolveEventsURL(rc.repoRoot, opts.url)
	if err != nil {
		return err
	}

	// SIGINT / SIGTERM should end the stream cleanly. The SSE client
	// returns ctx.Err() in that case, which we map to exit 0 — Ctrl-C
	// is the normal way to leave `--tail`.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	client := &events.Client{
		URL:       endpoint,
		Reconnect: opts.tail, // --once never reconnects
	}

	// `since` filters the backfill the server emits when a client
	// connects. The server replays up to ~20 records; we apply the
	// cutoff in the printer so the filter composes with --filter and
	// --json without needing server cooperation.
	var cutoff time.Time
	if opts.since > 0 {
		cutoff = time.Now().Add(-opts.since)
	}

	printer := &eventsPrinter{
		out:       os.Stdout,
		jsonOut:   opts.jsonOut,
		filter:    opts.filter,
		cutoff:    cutoff,
		once:      opts.once,
		cancelCtx: cancel,
	}

	err = client.Stream(ctx, printer.onEvent)
	if err != nil && !errors.Is(err, context.Canceled) {
		return userErr("events stream: %v", err)
	}
	if opts.once && printer.printed == 0 {
		// --once with no event delivered is a user-visible failure —
		// scripts can rely on a non-zero exit to mean "no event."
		return userErr("--once: no event received before stream ended")
	}
	return nil
}

// eventsPrinter formats each Event for stdout. It owns the side state
// (count of events printed, --once cancel hook) so onEvent stays a
// pure callback the SSE client can hand events to.
type eventsPrinter struct {
	out       io.Writer
	jsonOut   bool
	filter    string
	cutoff    time.Time
	once      bool
	cancelCtx context.CancelFunc

	printed int
}

func (p *eventsPrinter) onEvent(ev events.Event) {
	if ev.Data == "" {
		return
	}
	var rec bosunmcp.Event
	if err := json.Unmarshal([]byte(ev.Data), &rec); err != nil {
		// A line we can't decode is almost certainly schema drift or a
		// corrupt record on disk; surfacing it to stderr keeps the
		// stream usable but lets the operator know something's off.
		fmt.Fprintf(os.Stderr, "bosun events: skip undecodable record: %v\n", err)
		return
	}
	if !p.cutoff.IsZero() && rec.At.Before(p.cutoff) {
		return
	}
	if p.filter != "" && rec.Session != p.filter {
		return
	}

	if p.jsonOut {
		// Re-marshal to normalize the line (a single JSON object, no
		// trailing whitespace). The server's `data:` payload is
		// already JSON, but it may have additional fields we want to
		// keep — round-tripping through bosunmcp.Event drops nothing.
		out, err := json.Marshal(rec)
		if err != nil {
			return
		}
		fmt.Fprintln(p.out, string(out))
	} else {
		fmt.Fprintln(p.out, formatEvent(rec))
	}

	p.printed++
	if p.once && p.cancelCtx != nil {
		p.cancelCtx()
	}
}

// formatEvent renders one event as a single human-readable line. Shape
// mirrors the brief's example:
//
//	14:23:11  session-1  STATE_CHANGE  WORKING → DONE
//
// Kind is upper-cased so it lines up with the rest of bosun's CLI
// (claim/done/state are spelled in uppercase in `bosun status` too).
func formatEvent(e bosunmcp.Event) string {
	ts := e.At.Local().Format("15:04:05")
	if e.At.IsZero() {
		ts = "--:--:--"
	}
	kind := strings.ToUpper(e.Kind)
	if kind == "" {
		kind = "EVENT"
	}
	session := e.Session
	if session == "" {
		session = "-"
	}
	msg := e.Message
	return fmt.Sprintf("%s  %-10s  %-12s  %s", ts, session, kind, msg)
}

// resolveEventsURL returns the SSE endpoint to dial. Precedence:
//
//  1. --url, if the caller passed one
//  2. .bosun/serve.pid (written by `bosun serve` on startup)
//
// Anything resolved is normalized to a full http://host:port/api/events
// URL so callers can pass either a bare host:port or the whole thing.
func resolveEventsURL(repoRoot, explicit string) (string, error) {
	if explicit != "" {
		return normalizeEventsURL(explicit)
	}
	addr, err := readServePidfileAddr(repoRoot)
	if err != nil {
		return "", err
	}
	return normalizeEventsURL(addr)
}

// normalizeEventsURL accepts any of `host:port`, `http://host:port`, or
// `http://host:port/api/events` and returns the full SSE URL. Keeps
// `bosun events --url ...` flexible without making the user type the
// full path every time.
func normalizeEventsURL(raw string) (string, error) {
	s := raw
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", userErr("--url %q: %v", raw, err)
	}
	if u.Host == "" {
		return "", userErr("--url %q: missing host", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/api/events"
	}
	return u.String(), nil
}

// readServePidfileAddr reads .bosun/serve.pid in the main worktree and
// returns the address line. The pidfile format is
// `<pid>\n<host:port>\n`, mirroring the MCP pidfile shape. A missing
// file produces a user-facing pointer to `bosun serve` rather than a
// raw os.PathError.
func readServePidfileAddr(repoRoot string) (string, error) {
	path := filepath.Join(repoRoot, servePidfileRelative)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", userErr("no running bosun serve detected (start one with `bosun serve` or pass --url)")
		}
		return "", internalErr("read serve.pid", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 2 {
		return "", userErr("serve.pid is malformed (missing address line); restart `bosun serve`")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid <= 0 {
		return "", userErr("serve.pid has unparseable pid %q", lines[0])
	}
	addr := strings.TrimSpace(lines[1])
	if addr == "" {
		return "", userErr("serve.pid is missing the bound address; restart `bosun serve`")
	}
	return addr, nil
}
