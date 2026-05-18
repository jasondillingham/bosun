package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EventLogRelative is where bosun writes its append-only events log when
// `bosun mcp` boots inside a repo. Lives under .bosun/ so it's auto-gitignored.
const EventLogRelative = ".bosun/events.log"

// eventBufCap caps the in-memory ring buffer. Old entries are dropped FIFO
// when the cap is hit. Sized to stay cheap to scan while holding the lock —
// 200 announcements is a generous ceiling for a single working session.
const eventBufCap = 200

// Event is one operator-visible signal pushed by an agent via bosun_announce.
// The fields are intentionally flat so the JSONL records on disk stay easy
// to grep with standard CLI tooling.
type Event struct {
	Session string    `json:"session"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// eventBuf is the package-level ring buffer plus the optional JSONL
// persistence path. Package-level state lets tool handlers push events
// without threading a store through every call. Real cross-process
// visibility (CLI-side `bosun status` reading what an MCP-side announce
// wrote) comes from the file; the in-memory buffer is the fast read path
// for callers in the same process as the MCP server.
var eventBuf = struct {
	sync.Mutex
	items []Event
	path  string
}{}

// SetEventsLog configures the JSONL persistence path. Pass "" to disable
// persistence. Safe to call multiple times — tests call it with t.TempDir().
func SetEventsLog(path string) {
	eventBuf.Lock()
	eventBuf.path = path
	eventBuf.Unlock()
}

// Push records e in the in-memory buffer (oldest dropped if cap is hit) and,
// when SetEventsLog was given a non-empty path, appends one JSON line to the
// events log. Returns the persistence error if any — the in-memory write is
// non-failing.
func Push(e Event) error {
	eventBuf.Lock()
	eventBuf.items = append(eventBuf.items, e)
	if len(eventBuf.items) > eventBufCap {
		eventBuf.items = eventBuf.items[len(eventBuf.items)-eventBufCap:]
	}
	path := eventBuf.path
	eventBuf.Unlock()
	if path == "" {
		return nil
	}
	return appendEventLine(path, e)
}

// Recent returns up to n most-recent events from the in-memory buffer in
// chronological order (oldest first). Returns nil when n <= 0 or empty.
func Recent(n int) []Event {
	eventBuf.Lock()
	defer eventBuf.Unlock()
	if n <= 0 || len(eventBuf.items) == 0 {
		return nil
	}
	if n > len(eventBuf.items) {
		n = len(eventBuf.items)
	}
	out := make([]Event, n)
	copy(out, eventBuf.items[len(eventBuf.items)-n:])
	return out
}

// ResetEventsForTest clears the in-memory buffer and persistence path.
// Test-only — keeps unit tests independent of one another when they run
// in the same process.
func ResetEventsForTest() {
	eventBuf.Lock()
	eventBuf.items = nil
	eventBuf.path = ""
	eventBuf.Unlock()
}

func appendEventLine(path string, e Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	// Create the parent directory on demand — .bosun/ may not exist yet on
	// a freshly cloned repo before any claim/state file has been written.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("mkdir events parent: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open events log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

// TailEvents reads up to n most-recent records from a JSONL events log at
// path. Returns chronological order (oldest first). A missing file is not
// an error — returns (nil, nil) so callers can no-op when no MCP server
// has ever run.
func TailEvents(path string, n int) ([]Event, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open events log: %w", err)
	}
	defer f.Close()
	lines, err := tailLines(f, n)
	if err != nil {
		return nil, fmt.Errorf("tail events log: %w", err)
	}
	out := make([]Event, 0, len(lines))
	for _, line := range lines {
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			// Skip corrupt lines rather than failing the whole tail: a
			// partially-written record at the end (e.g. process kill mid-
			// write) shouldn't blind the operator to the previous N events.
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// tailLines returns up to n newline-terminated records from the end of f in
// original (oldest-first) order. Scans backwards in chunks so it doesn't
// have to load the whole log into memory when it's been running a while.
func tailLines(f *os.File, n int) ([][]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size == 0 || n <= 0 {
		return nil, nil
	}

	const chunk int64 = 4096
	var buf []byte
	pos := size
	for pos > 0 {
		readLen := chunk
		if pos < readLen {
			readLen = pos
		}
		pos -= readLen
		b := make([]byte, readLen)
		if _, err := f.ReadAt(b, pos); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		buf = append(b, buf...)
		// > n newlines means we have at least n complete records at the tail
		// (the n+1th newline gives us a known-complete boundary above them).
		if bytes.Count(buf, []byte{'\n'}) > n {
			break
		}
	}

	// Drop trailing newline so Split doesn't yield an empty final element.
	buf = bytes.TrimRight(buf, "\n")
	parts := bytes.Split(buf, []byte{'\n'})
	if len(parts) > n {
		parts = parts[len(parts)-n:]
	}
	out := make([][]byte, 0, len(parts))
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		// Copy so callers can't mutate our scratch buffer.
		cp := make([]byte, len(p))
		copy(cp, p)
		out = append(out, cp)
	}
	return out, nil
}
