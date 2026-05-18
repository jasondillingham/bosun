// Package events is a minimal Server-Sent Events client. `bosun events
// --tail` uses it to consume the /api/events stream `bosun serve`
// exposes; nothing else in the codebase should grow a custom HTTP
// streaming reader.
//
// The parser handles the three line shapes the SSE spec calls out plus
// the bosun server's own keepalive comment:
//
//   - `event: <name>`  → sets the event type for the next record
//   - `data: <payload>` → appended to the event's body (multiple data:
//     lines concatenated with newlines, per the spec)
//   - `:<anything>` → comment (the server sends `: keep-alive` to keep
//     idle connections from being reaped by intervening proxies)
//   - blank line → dispatches the buffered event
//
// We deliberately don't implement `id:` or last-event-id reconnects:
// the bosun events log is content-addressable enough that re-receiving
// a backfill on reconnect is fine and far simpler than tracking IDs.
package events

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Event is one decoded SSE record. Name is the event:-line value (often
// empty — the bosun server doesn't emit event: today), Data is the
// concatenation of one-or-more data: lines.
type Event struct {
	Name string
	Data string
}

// Client streams events from an SSE endpoint. Configure via the public
// fields, then call Stream. The zero value uses sensible defaults.
type Client struct {
	// URL is the SSE endpoint, e.g. "http://127.0.0.1:8765/api/events".
	URL string

	// HTTPClient is the transport used for the streaming GET. nil means
	// http.DefaultClient with no timeout — SSE intentionally holds the
	// connection open, so a Timeout on the client breaks it.
	HTTPClient *http.Client

	// ReconnectInitial is the first backoff delay after a connection
	// loss (server restart, network blip). Defaults to 250ms.
	ReconnectInitial time.Duration

	// ReconnectMax caps the exponential backoff so a long server outage
	// doesn't push delays into the minute range. Defaults to 5s.
	ReconnectMax time.Duration

	// Reconnect controls whether the client reconnects after a stream
	// ends (or fails to start). When false, Stream returns after the
	// first disconnect — used by `bosun events --once` and tests.
	Reconnect bool
}

// defaultReconnectInitial / defaultReconnectMax are the fallbacks Stream
// uses when a Client's fields are left zero. Kept in one place so the
// docstring and the runtime agree.
const (
	defaultReconnectInitial = 250 * time.Millisecond
	defaultReconnectMax     = 5 * time.Second
)

// Stream connects to c.URL and invokes onEvent for every dispatched SSE
// event until ctx is cancelled, the server closes the connection (when
// Reconnect=false), or a non-retryable HTTP error occurs (4xx).
//
// When Reconnect=true a clean EOF (server restart) triggers an
// exponential backoff and reconnects. A 4xx response returns
// immediately — those mean the request itself is wrong, not that the
// server is transiently unavailable.
//
// onEvent is called from Stream's goroutine; if it blocks, the read
// loop blocks with it. Callers that need decoupling should buffer
// inside onEvent.
func (c *Client) Stream(ctx context.Context, onEvent func(Event)) error {
	if c.URL == "" {
		return errors.New("events: URL is empty")
	}
	initial := c.ReconnectInitial
	if initial <= 0 {
		initial = defaultReconnectInitial
	}
	maxBackoff := c.ReconnectMax
	if maxBackoff <= 0 {
		maxBackoff = defaultReconnectMax
	}
	backoff := initial

	for {
		err := c.connectOnce(ctx, onEvent)
		if ctx.Err() != nil {
			// Caller cancelled — surface that, not the read-loop error
			// (which is almost always "context canceled" too).
			return ctx.Err()
		}
		// 4xx is a permanent failure — don't reconnect into the same
		// wrong-URL forever.
		var notRetry *nonRetryableError
		if errors.As(err, &notRetry) {
			return err
		}
		if !c.Reconnect {
			return err
		}
		// Wait with exponential backoff, but bail early if ctx fires.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// nonRetryableError marks failures the caller shouldn't reconnect from
// (404, 401, etc.). Wrapped so callers can errors.As().
type nonRetryableError struct {
	err error
}

func (e *nonRetryableError) Error() string { return e.err.Error() }
func (e *nonRetryableError) Unwrap() error { return e.err }

// connectOnce performs one HTTP GET + read loop. Returns when the
// stream closes for any reason; the caller's reconnect logic in
// Stream decides whether to come back.
func (c *Client) connectOnce(ctx context.Context, onEvent func(Event)) error {
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect %s: %w", c.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return &nonRetryableError{err: fmt.Errorf("server returned %s", resp.Status)}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	return Parse(resp.Body, onEvent)
}

// Parse reads SSE-framed records from r and invokes onEvent once per
// dispatched event. Exposed for tests (so they don't need a real HTTP
// server to exercise the parser) and to let callers consume an
// already-open stream.
//
// Returns nil on a clean EOF — that's the expected outcome when the
// server closes the stream. Any other read error is wrapped and
// returned.
func Parse(r io.Reader, onEvent func(Event)) error {
	sc := bufio.NewScanner(r)
	// A single SSE record (event: + multiple data: lines) can easily
	// exceed bufio's default 64 KiB token. Bosun events are small JSON
	// blobs, but the dashboard could one day stream larger payloads —
	// bumping the buffer to 1 MiB is cheap insurance and matches what
	// other SSE clients in the ecosystem use.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		name string
		data strings.Builder
		any  bool // true once we've seen any field for the current event
	)
	dispatch := func() {
		if !any {
			return
		}
		ev := Event{Name: name, Data: data.String()}
		onEvent(ev)
		name = ""
		data.Reset()
		any = false
	}

	for sc.Scan() {
		line := sc.Text()
		// Per the SSE spec a blank line dispatches the buffered event.
		if line == "" {
			dispatch()
			continue
		}
		// Comments start with ':' (e.g. our `: keep-alive`). Drop them
		// silently — they're framing, not events.
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value := splitField(line)
		switch field {
		case "event":
			name = value
			any = true
		case "data":
			// Multiple data: lines on one event are concatenated with
			// `\n` per the spec.
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			any = true
		default:
			// Unknown fields ("id", "retry", or anything bosun doesn't
			// emit yet) are ignored. Doing nothing matches the SSE
			// spec for `retry:` parse failures and is the safest path
			// for forward compat.
		}
	}
	// Per spec, an EOF after a non-empty buffered event still
	// dispatches it. bosun's server always emits a trailing blank
	// line, but being lenient here keeps Parse usable against other
	// servers and replayed fixtures.
	dispatch()
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read sse stream: %w", err)
	}
	return nil
}

// splitField parses an SSE field line into (field, value). Per the
// spec, the colon is optional (a bare field name is a field with an
// empty value) and exactly one leading space after the colon is
// stripped from the value.
func splitField(line string) (string, string) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return line, ""
	}
	field := line[:idx]
	value := line[idx+1:]
	value = strings.TrimPrefix(value, " ")
	return field, value
}
