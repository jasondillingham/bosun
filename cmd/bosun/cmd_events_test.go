package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
)

// TestScenario_EventsTailStreamsLiveEvents is the brief's mandated
// end-to-end check: start `bosun serve`, run `bosun events --tail`
// against it, and assert events flow. We seed two events into
// .bosun/events.log up-front so the SSE backfill has something to
// replay, then append a third while --tail is running to verify the
// live path is wired up too.
func TestScenario_EventsTailStreamsLiveEvents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scenario tests use POSIX shell helpers")
	}

	s := newScenario(t)

	logPath := filepath.Join(s.repo, ".bosun", "events.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir .bosun: %v", err)
	}
	seedEventLog(t, logPath,
		bosunmcp.Event{Session: "session-1", Kind: "progress", Message: "first", At: time.Unix(1700000000, 0).UTC()},
		bosunmcp.Event{Session: "session-2", Kind: "claim", Message: "second", At: time.Unix(1700000001, 0).UTC()},
	)

	port := pickFreePort(t)

	// Start `bosun serve` in the background. The CommandContext timeout
	// is the test-wide ceiling; we Signal SIGINT in t.Cleanup to give
	// it a chance to clean up the pidfile.
	serveCtx, serveCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer serveCancel()

	serve := exec.CommandContext(serveCtx, bosunBin, "serve", "--port", itoa(port), "--bind", "127.0.0.1", "--interval", "1")
	serve.Dir = s.repo
	var serveOut subprocessTail
	serve.Stdout = &serveOut
	serve.Stderr = &serveOut
	if err := serve.Start(); err != nil {
		t.Fatalf("start bosun serve: %v", err)
	}
	t.Cleanup(func() {
		_ = serve.Process.Signal(os.Interrupt)
		_ = serve.Wait()
	})

	// Wait for the pidfile to appear — it's the explicit handshake that
	// auto-detect from .bosun/serve.pid will succeed when --url isn't
	// passed.
	pidfile := filepath.Join(s.repo, ".bosun", "serve.pid")
	if err := waitForFile(pidfile, 5*time.Second); err != nil {
		t.Fatalf("serve.pid never appeared: %v\nserve output:\n%s", err, serveOut.String())
	}

	// --once against the backfill should print the first seeded event
	// then exit 0. This proves auto-detect from .bosun/serve.pid works.
	onceCtx, onceCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer onceCancel()
	onceCmd := exec.CommandContext(onceCtx, bosunBin, "events", "--once")
	onceCmd.Dir = s.repo
	onceOut, err := onceCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bosun events --once: %v\noutput:\n%s\nserve:\n%s", err, onceOut, serveOut.String())
	}
	if !strings.Contains(string(onceOut), "session-1") || !strings.Contains(string(onceOut), "first") {
		t.Fatalf("--once didn't print the seeded event:\n%s", onceOut)
	}

	// --tail streams: run it in a goroutine, append a fresh event mid-
	// stream, and wait until the tail prints it. SIGINT after we've
	// seen enough output ends --tail cleanly.
	tailCtx, tailCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer tailCancel()

	tailCmd := exec.CommandContext(tailCtx, bosunBin, "events", "--tail", "--json")
	tailCmd.Dir = s.repo
	stdoutPipe, err := tailCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	tailCmd.Stderr = &serveOut
	if err := tailCmd.Start(); err != nil {
		t.Fatalf("start bosun events --tail: %v", err)
	}
	t.Cleanup(func() {
		_ = tailCmd.Process.Signal(os.Interrupt)
		_ = tailCmd.Wait()
	})

	// Collector goroutine: drain stdout line-by-line so we can match
	// against incoming events without blocking on a full read.
	lines := make(chan string, 32)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdoutPipe)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	// Drain the backfill — we've seeded 2 events, so the first two
	// JSON lines should mention "first" / "second".
	got := readN(t, lines, 2, 5*time.Second, &serveOut)
	if !strings.Contains(got[0], "first") || !strings.Contains(got[1], "second") {
		t.Fatalf("backfill ordering broken: %#v", got)
	}

	// Append a live event after the backfill drains. The server polls
	// the log on a 1s tick, so we should see it land within a couple
	// of seconds.
	appendEventLog(t, logPath, bosunmcp.Event{
		Session: "session-3", Kind: "commit", Message: "live-tail-event",
		At: time.Now().UTC(),
	})

	live := readN(t, lines, 1, 8*time.Second, &serveOut)
	if !strings.Contains(live[0], "live-tail-event") || !strings.Contains(live[0], "session-3") {
		t.Fatalf("live event never surfaced; got %#v", live)
	}

	// Verify --json produced parseable JSON so downstream scripts can
	// rely on the shape.
	var rec bosunmcp.Event
	if err := json.Unmarshal([]byte(live[0]), &rec); err != nil {
		t.Errorf("--json output not parseable: %v\nline=%s", err, live[0])
	}
}

// TestEvents_NoServeRefusesWithPointer covers the "auto-detect fails"
// path: a fresh repo with no serve running should refuse with a
// pointer to `bosun serve`, not blow up with a raw os.PathError.
func TestEvents_NoServeRefusesWithPointer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scenario tests use POSIX shell helpers")
	}
	s := newScenario(t)

	out, err := s.BosunErr("events", "--once")
	if err == nil {
		t.Fatalf("expected error when no serve is running; got:\n%s", out)
	}
	if !strings.Contains(out, "bosun serve") {
		t.Errorf("error should point at `bosun serve`, got:\n%s", out)
	}
}

// TestEvents_FilterAndSince exercises composition of --filter and
// --since. We seed three events with different sessions + timestamps,
// then run --once with --filter session-2 and confirm only that one
// is printed (the backfill is replayed oldest-first so without the
// filter --once would print session-1 first).
func TestEvents_FilterAndSince(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scenario tests use POSIX shell helpers")
	}
	s := newScenario(t)

	logPath := filepath.Join(s.repo, ".bosun", "events.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir .bosun: %v", err)
	}
	now := time.Now().UTC()
	seedEventLog(t, logPath,
		bosunmcp.Event{Session: "session-1", Kind: "progress", Message: "old-and-filtered-out", At: now.Add(-1 * time.Hour)},
		bosunmcp.Event{Session: "session-2", Kind: "progress", Message: "want-this-one", At: now.Add(-1 * time.Minute)},
		bosunmcp.Event{Session: "session-3", Kind: "progress", Message: "also-not-this", At: now.Add(-30 * time.Second)},
	)

	port := pickFreePort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serve := exec.CommandContext(ctx, bosunBin, "serve", "--port", itoa(port), "--bind", "127.0.0.1", "--interval", "1")
	serve.Dir = s.repo
	var serveOut subprocessTail
	serve.Stdout = &serveOut
	serve.Stderr = &serveOut
	if err := serve.Start(); err != nil {
		t.Fatalf("start bosun serve: %v", err)
	}
	t.Cleanup(func() {
		_ = serve.Process.Signal(os.Interrupt)
		_ = serve.Wait()
	})

	if err := waitForFile(filepath.Join(s.repo, ".bosun", "serve.pid"), 5*time.Second); err != nil {
		t.Fatalf("serve.pid never appeared: %v\n%s", err, serveOut.String())
	}

	onceCtx, onceCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer onceCancel()
	cmd := exec.CommandContext(onceCtx, bosunBin, "events", "--once", "--filter", "session-2", "--since", "5m")
	cmd.Dir = s.repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bosun events: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "want-this-one") {
		t.Errorf("filter+since didn't surface the matching event:\n%s", out)
	}
	if strings.Contains(string(out), "old-and-filtered-out") || strings.Contains(string(out), "also-not-this") {
		t.Errorf("filter+since leaked a non-matching event:\n%s", out)
	}
}

// readN reads up to n lines from the channel within timeout. Fails
// the test on timeout — these tests assert positive flow, so a stall
// is always a bug worth surfacing with the subprocess output.
func readN(t *testing.T, lines <-chan string, n int, timeout time.Duration, log *subprocessTail) []string {
	t.Helper()
	got := make([]string, 0, n)
	deadline := time.After(timeout)
	for len(got) < n {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("events stream closed early; got %d/%d lines: %#v\nsubprocess:\n%s", len(got), n, got, log.String())
			}
			got = append(got, line)
		case <-deadline:
			t.Fatalf("timed out waiting for %d events; got %d: %#v\nsubprocess:\n%s", n, len(got), got, log.String())
		}
	}
	return got
}

// seedEventLog writes the given events as JSONL records to path
// (truncating any prior contents). Mirrors the on-disk format the MCP
// server writes so the SSE backfill picks them up unchanged.
func seedEventLog(t *testing.T, path string, events ...bosunmcp.Event) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open events log: %v", err)
	}
	defer f.Close()
	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

// appendEventLog tacks one event onto an existing JSONL log. Used by
// the live-tail test to push a record after the backfill drains.
func appendEventLog(t *testing.T, path string, e bosunmcp.Event) {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open events log: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// waitForFile polls until path exists or timeout fires. Used to bridge
// the "bosun serve hasn't bound yet" gap before the events client
// tries to auto-detect.
func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("file did not appear: " + path)
}
