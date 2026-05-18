package launcher

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLauncher_RapidFire is the regression test for trial #3c Bug D
// (docs/v0.9-trial-3c-findings.md). Three bosun_spawn calls in ~8
// seconds resulted in zero visible sub-agent windows on macOS, with
// no error surfaced to the parent. The pre-fix code path returned
// success from Launch but the spawned osascript / Ghostty CLI
// invocations were racing each other into a no-op.
//
// The fix is two-part: (1) replace the silent Wait()-goroutine in
// spawnDetached with one that surfaces post-fork stderr, and (2) add
// a 250ms stagger between successive macOS launches so AppleScript /
// Apple Events / Ghostty IPC don't race. This test asserts BOTH:
// every Launch call reaches the spawner (no silent skips), and on
// macOS the launches arrive at the spawner at least 250ms apart.
func TestLauncher_RapidFire(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH stub uses POSIX shebang")
	}

	stubGhosttyOnPath(t)
	restoreSpawnFn := installRecordingSpawn(t)
	defer restoreSpawnFn()

	const n = 5
	startedAt := time.Now()
	for i := 1; i <= n; i++ {
		_, err := Launch(Options{
			Strategy:     StrategyTerminal,
			WorktreePath: t.TempDir(),
			SessionName:  fmt.Sprintf("session-%d", i),
			Command:      "claude",
			Out:          io.Discard,
		})
		if err != nil {
			t.Fatalf("Launch %d returned error: %v (the fake spawner returns nil — Launch should not error)", i, err)
		}
	}

	recordedSpawnsMu.Lock()
	defer recordedSpawnsMu.Unlock()
	if got := len(recordedSpawns); got != n {
		t.Fatalf("recorded %d spawns, want %d — Launch must NEVER silently skip the spawner", got, n)
	}

	// On macOS the stagger must space launches out. On Linux/Windows the
	// stagger is intentionally not applied (per scope: keep non-darwin
	// paths untouched), so we only assert the timing on darwin.
	if runtime.GOOS == "darwin" {
		elapsed := time.Since(startedAt)
		// 5 launches => 4 inter-call gaps. First call doesn't stagger
		// (lastAt is zero), so the floor is (n-1) * stagger.
		wantMin := (n - 1) * int(macOSLaunchStagger/time.Millisecond)
		if elapsed < time.Duration(wantMin)*time.Millisecond {
			t.Errorf("%d macOS launches took %v, want >= %dms (stagger should serialize)", n, elapsed, wantMin)
		}
		for i := 1; i < n; i++ {
			gap := recordedSpawns[i].at.Sub(recordedSpawns[i-1].at)
			// Tolerate 5ms scheduler jitter below the threshold.
			if gap < macOSLaunchStagger-5*time.Millisecond {
				t.Errorf("call %d→%d gap = %v, want >= %v", i-1, i, gap, macOSLaunchStagger)
			}
		}
	}
}

// TestLauncher_SurfacesStartError verifies that when the spawner's
// Start() fails (binary missing, permission denied, etc.), the
// failure is surfaced to opts.Out before falling back to print.
// Launch's contract is "never silent" — even when the terminal
// strategy fails, the operator must see why.
//
// Launch deliberately downgrades a terminal failure to a print
// fallback (so a single broken `--launch` doesn't abort init), but
// the fallback prints the underlying error first. This test pins
// that "loud-fallback" behavior.
func TestLauncher_SurfacesStartError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH stub uses POSIX shebang")
	}
	stubGhosttyOnPath(t)

	prev := spawnFn
	t.Cleanup(func() { spawnFn = prev })
	resetMacOSStaggerForTest()

	wantErr := fmt.Errorf("simulated start failure")
	spawnFn = func(cmd *exec.Cmd, sessionName string, outWriter io.Writer) error {
		return wantErr
	}

	var buf bytes.Buffer
	got, err := Launch(Options{
		Strategy:     StrategyTerminal,
		WorktreePath: "/wt",
		SessionName:  "session-x",
		Command:      "claude",
		Out:          &buf,
	})
	if err != nil {
		t.Fatalf("Launch returned err=%v; expected nil with fall-through to print", err)
	}
	if got != StrategyPrint {
		t.Errorf("Launch resolved to %s, want %s (terminal failure must downgrade to print)", got, StrategyPrint)
	}
	out := buf.String()
	if !strings.Contains(out, "terminal launch failed") {
		t.Errorf("opts.Out missing terminal-failure header; got: %q", out)
	}
	if !strings.Contains(out, wantErr.Error()) {
		t.Errorf("opts.Out missing underlying error %q; got: %q", wantErr, out)
	}
	if !strings.Contains(out, "session-x") {
		t.Errorf("opts.Out missing session name; got: %q", out)
	}
}

// TestLauncher_SurfacesPostForkFailure verifies that when the child
// process exits non-zero AFTER the fork (the exact failure mode
// trial #3c saw — osascript returning an AppleScript error or
// Ghostty's CLI bouncing off a busy IPC channel), the operator gets
// a one-line diagnostic in opts.Out. Pre-fix, the Wait() goroutine
// silently discarded these errors.
func TestLauncher_SurfacesPostForkFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell stub")
	}

	// Stub ghostty as a script that writes to stderr and exits 1.
	dir := t.TempDir()
	stub := filepath.Join(dir, "ghostty")
	script := "#!/bin/sh\necho 'simulated AppleScript boom' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	resetMacOSStaggerForTest()

	buf := &syncBuf{}
	_, err := Launch(Options{
		Strategy:     StrategyTerminal,
		WorktreePath: t.TempDir(),
		SessionName:  "session-x",
		Command:      "claude",
		Out:          buf,
	})
	if err != nil {
		t.Fatalf("Launch returned error from Start(): %v — expected fork to succeed and Wait() to surface the exit code", err)
	}

	// The Wait() goroutine runs asynchronously. Poll briefly for the
	// stderr line to appear.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "simulated AppleScript boom") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	out := buf.String()
	if !strings.Contains(out, "session-x") {
		t.Errorf("post-fork message missing session name; got: %q", out)
	}
	if !strings.Contains(out, "simulated AppleScript boom") {
		t.Errorf("post-fork stderr not surfaced — this is the trial #3c silent-failure regression; got: %q", out)
	}
}

// syncBuf is a goroutine-safe bytes.Buffer wrapper. The spawnDetached
// reaper goroutine writes to opts.Out concurrently with the test
// reading it; without a mutex the race detector trips.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestWaitForMacOSStagger_SpacesCallsOut exercises the stagger
// primitive directly. On darwin the second call must observe a sleep
// of at least macOSLaunchStagger. On other OSes we don't gate
// behavior, but the function is still safe to call (no panic).
func TestWaitForMacOSStagger_SpacesCallsOut(t *testing.T) {
	resetMacOSStaggerForTest()
	waitForMacOSStagger() // primes lastAt
	start := time.Now()
	waitForMacOSStagger()
	gap := time.Since(start)
	if gap < macOSLaunchStagger-5*time.Millisecond {
		t.Errorf("second waitForMacOSStagger returned after %v, want >= %v", gap, macOSLaunchStagger)
	}
}

// TestWaitForMacOSStagger_NoSleepWhenGapAlreadyElapsed verifies that
// when the previous call was long enough ago, the next call returns
// immediately. Avoids an unnecessary 250ms tax on operators who
// `bosun launch` once, do other work, then `bosun launch` again.
func TestWaitForMacOSStagger_NoSleepWhenGapAlreadyElapsed(t *testing.T) {
	resetMacOSStaggerForTest()
	waitForMacOSStagger()
	time.Sleep(macOSLaunchStagger + 20*time.Millisecond)
	start := time.Now()
	waitForMacOSStagger()
	gap := time.Since(start)
	if gap > 50*time.Millisecond {
		t.Errorf("waitForMacOSStagger slept %v despite stagger having already elapsed", gap)
	}
}

// --- helpers ---------------------------------------------------------------

type recordedSpawn struct {
	sessionName string
	argv        []string
	at          time.Time
}

var (
	recordedSpawnsMu sync.Mutex
	recordedSpawns   []recordedSpawn
	recordedSpawnsN  atomic.Int32
)

// installRecordingSpawn replaces spawnFn with a recording fake and
// returns a function to restore the original. Also resets the macOS
// stagger and the record buffer so each test starts fresh.
func installRecordingSpawn(t *testing.T) func() {
	t.Helper()
	prev := spawnFn
	recordedSpawnsMu.Lock()
	recordedSpawns = nil
	recordedSpawnsN.Store(0)
	recordedSpawnsMu.Unlock()
	resetMacOSStaggerForTest()

	spawnFn = func(cmd *exec.Cmd, sessionName string, outWriter io.Writer) error {
		recordedSpawnsMu.Lock()
		recordedSpawns = append(recordedSpawns, recordedSpawn{
			sessionName: sessionName,
			argv:        append([]string(nil), cmd.Args...),
			at:          time.Now(),
		})
		recordedSpawnsMu.Unlock()
		recordedSpawnsN.Add(1)
		return nil
	}
	return func() { spawnFn = prev }
}

// resetMacOSStaggerForTest zeroes the package's lastLaunchAt tracker
// so a test doesn't observe stagger from an earlier test's launch.
func resetMacOSStaggerForTest() {
	macOSStaggerMu.Lock()
	defer macOSStaggerMu.Unlock()
	macOSLastAt = time.Time{}
}

// stubGhosttyOnPath drops a no-op ghostty script onto PATH so
// launchTerminal picks the Ghostty path cross-platform.
func stubGhosttyOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "ghostty")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
