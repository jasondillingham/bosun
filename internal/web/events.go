package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
)

// eventsBackfill is the cap on records replayed to a freshly-connected SSE
// client. Mirrors the TUI's "Recent" tail so a browser opened mid-session
// gets context without slurping the whole log.
const eventsBackfill = 20

// pollInterval is how often the SSE handler scans .bosun/events.log for
// new lines. 1s keeps the dashboard feeling live while staying cheap.
const pollInterval = 1 * time.Second

// sseKeepalive is the cadence of `: keep-alive` comments sent when no
// events have arrived. Browsers (and intervening proxies) drop idle
// connections; ~15s is well under the typical 60s idle timeout.
const sseKeepalive = 15 * time.Second

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	logPath := filepath.Join(s.cfg.RepoRoot, bosunmcp.EventLogRelative)

	// Backfill: emit up to N most-recent records so a fresh browser sees
	// the last few announcements before any new ones arrive. Then seek
	// to EOF so the live loop only sees new lines.
	if recent, err := bosunmcp.TailEvents(logPath, eventsBackfill); err == nil {
		for _, e := range recent {
			if !writeSSEEvent(w, flusher, e) {
				return
			}
		}
	}
	var offset int64
	if fi, err := os.Stat(logPath); err == nil {
		offset = fi.Size()
	}

	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	ka := time.NewTicker(sseKeepalive)
	defer ka.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ka.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
			newEvents, newOffset, err := readEventsSince(logPath, offset)
			if err != nil {
				// Log file may not exist yet or be transiently missing
				// (no MCP server has ever run). Try again next tick.
				continue
			}
			offset = newOffset
			for _, e := range newEvents {
				if !writeSSEEvent(w, flusher, e) {
					return
				}
			}
		}
	}
}

// writeSSEEvent serializes e and writes one SSE record. Returns false if
// the client connection has gone away (write failed) so the caller can
// stop the loop.
func writeSSEEvent(w http.ResponseWriter, f http.Flusher, e bosunmcp.Event) bool {
	data, err := json.Marshal(e)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return false
	}
	f.Flush()
	return true
}

// readEventsSince returns events appended after offset bytes in path, plus
// the new byte offset (always at the end of the last complete line). A
// partial trailing line — common when an MCP push is still flushing — is
// left for the next call so we never parse half a JSON record.
func readEventsSince(path string, offset int64) ([]bosunmcp.Event, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, offset, nil
		}
		return nil, offset, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	size := fi.Size()

	// Truncated or rotated. Start over from the top so we don't miss
	// records — better to re-emit a few than silently drop everything.
	if size < offset {
		offset = 0
	}
	if size == offset {
		return nil, offset, nil
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	data := make([]byte, size-offset)
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, offset, err
	}

	// Only advance to the last complete line. A trailing partial line is
	// retried on the next tick.
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL == -1 {
		return nil, offset, nil
	}
	newOffset := offset + int64(lastNL) + 1

	var events []bosunmcp.Event
	for _, line := range bytes.Split(data[:lastNL], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var e bosunmcp.Event
		if err := json.Unmarshal(line, &e); err != nil {
			// Skip corrupt lines (e.g. a partially-written record from a
			// killed mid-write) rather than blow up the stream.
			continue
		}
		events = append(events, e)
	}
	return events, newOffset, nil
}
