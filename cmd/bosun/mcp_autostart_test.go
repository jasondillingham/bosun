package main

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
)

// TestInheritedSocketBelongsToRepo covers the three paths in the helper:
// the inherited socket equals this repo's default socket path; the
// inherited socket is recorded in this repo's pidfile; the inherited
// socket belongs to a different repo. Cross-repo bug: ensureMcp used to
// blindly trust any live inherited socket, so an agent launched against
// repo A while a parent shell still had repo B's BOSUN_MCP_SOCK set would
// talk to the wrong daemon. The helper rejects that case.
func TestInheritedSocketBelongsToRepo(t *testing.T) {
	repoA := t.TempDir()
	repoB := t.TempDir()

	defaultA := bosunmcp.DefaultSocketPath(repoA)
	defaultB := bosunmcp.DefaultSocketPath(repoB)

	if inheritedSocketBelongsToRepo("", repoA) {
		t.Fatalf("empty socket should not match")
	}
	if !inheritedSocketBelongsToRepo(defaultA, repoA) {
		t.Fatalf("default path for repoA must match repoA")
	}
	if inheritedSocketBelongsToRepo(defaultB, repoA) {
		t.Fatalf("default path for repoB must NOT match repoA")
	}

	// Pidfile-recorded match: write repoA's pidfile pointing at a custom
	// socket and confirm it's accepted for repoA but not repoB.
	customSock := filepath.Join(t.TempDir(), "custom.sock")
	pidfilePath := filepath.Join(repoA, ".bosun", "mcp.pid")
	if err := os.MkdirAll(filepath.Dir(pidfilePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidfilePath, []byte("12345\n"+customSock+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !inheritedSocketBelongsToRepo(customSock, repoA) {
		t.Fatalf("custom socket recorded in repoA's pidfile must match repoA")
	}
	if inheritedSocketBelongsToRepo(customSock, repoB) {
		t.Fatalf("repoA's pidfile must not authorize repoB")
	}
}

// TestWithMcpSpawnLock_SerializesCallers confirms the flock primitive
// keeps two goroutines from running fn concurrently. Without the lock
// they'd both pass the pidfile-check and both spawn a daemon — the
// second daemon's Listen() would unlink the first's socket and clobber
// the pidfile. The test increments a shared counter inside fn while
// holding the lock and asserts the counter never observed >1 in flight.
func TestWithMcpSpawnLock_SerializesCallers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("withMcpSpawnLock is a no-op on Windows builds")
	}
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".bosun"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const callers = 8
	var inFlight int32
	var maxInFlight int32

	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			_, err := withMcpSpawnLock(repoRoot, func() (mcpServerInfo, error) {
				cur := atomic.AddInt32(&inFlight, 1)
				for {
					prev := atomic.LoadInt32(&maxInFlight)
					if cur <= prev || atomic.CompareAndSwapInt32(&maxInFlight, prev, cur) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond) // simulated spawn work
				atomic.AddInt32(&inFlight, -1)
				return mcpServerInfo{socketPath: "/tmp/test.sock"}, nil
			})
			if err != nil {
				t.Errorf("withMcpSpawnLock: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Fatalf("max concurrent fn calls = %d, want 1 (lock did not serialize)", got)
	}
}

// TestWithMcpSpawnLock_ReuseShortCircuit checks that, once a "daemon" is
// "running" (we just write a pidfile + isProcessAlive returns true for our
// own pid + socketAlive will fail since there's no real socket), the second
// caller's fn re-checks the pidfile under lock and returns the same
// socketPath without re-spawning. The spawn counter must end at 1.
func TestWithMcpSpawnLock_ReuseShortCircuit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("withMcpSpawnLock is a no-op on Windows builds")
	}
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".bosun"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Stand up a real listening Unix socket so isSocketAlive returns true
	// in the reuse-path check below.
	sockPath := filepath.Join("/tmp", "bosun-spawnlock-test.sock")
	_ = os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen socket: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(sockPath)
	})

	var spawnCount int32
	doSpawn := func() (mcpServerInfo, error) {
		// Re-check pidfile — if a prior spawn already ran, return its info
		// without incrementing the counter.
		if info, ok := readMcpPidfile(repoRoot); ok && isProcessAlive(info.pid) && isSocketAlive(info.socketPath) {
			return mcpServerInfo{socketPath: info.socketPath, pid: info.pid}, nil
		}
		atomic.AddInt32(&spawnCount, 1)
		if err := writeMcpPidfile(repoRoot, os.Getpid(), sockPath); err != nil {
			return mcpServerInfo{}, err
		}
		return mcpServerInfo{socketPath: sockPath, pid: os.Getpid(), spawned: true}, nil
	}

	const callers = 6
	results := make([]string, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			info, err := withMcpSpawnLock(repoRoot, doSpawn)
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
				return
			}
			results[i] = info.socketPath
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&spawnCount); got != 1 {
		t.Fatalf("spawn ran %d times, want 1 (reuse path didn't kick in)", got)
	}
	for i, s := range results {
		if s != sockPath {
			t.Errorf("caller %d got socket %q, want %q", i, s, sockPath)
		}
	}
}

// TestWriteMcpPidfile_AtomicNeverTornRead hammers the pidfile with
// concurrent writers and a reader to confirm the atomic temp+rename
// path never lets the reader observe a half-written file. Pre-fix,
// writeMcpPidfile used os.WriteFile (O_TRUNC + write) which can be
// interrupted between the truncate and the body landing on disk;
// readMcpPidfile would then return ok=false on an unparseable
// `<pid>\n<sock>` and a concurrent ensureMcp would fall through to
// a spawn attempt that races on socket bind. The post-fix path uses
// CreateTemp + Rename so readers either see the prior version or
// the new one — never a torn body.
func TestWriteMcpPidfile_AtomicNeverTornRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		// os.Rename over an existing file is not guaranteed atomic on
		// Windows; the writer path is the same code but the OS-level
		// guarantee differs. Skip rather than test a weaker contract.
		t.Skip("atomic rename semantics differ on Windows")
	}
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".bosun"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Seed an initial valid pidfile so the reader has something parseable
	// on the very first read — the test cares about torn writes from the
	// concurrent rewrites that follow, not the cold-start case.
	if err := writeMcpPidfile(repoRoot, 1, "/tmp/initial.sock"); err != nil {
		t.Fatalf("seed pidfile: %v", err)
	}

	const iters = 500
	done := make(chan struct{})

	var torn int32
	go func() {
		defer close(done)
		for i := 0; i < iters; i++ {
			// Read directly — exercise the same parser ensureMcp uses.
			info, ok := readMcpPidfile(repoRoot)
			if !ok {
				atomic.AddInt32(&torn, 1)
				continue
			}
			// Even when the parse succeeds, sanity-check the contents
			// look like one of the values we plan to write.
			if info.pid <= 0 || !strings.HasPrefix(info.socketPath, "/tmp/") {
				atomic.AddInt32(&torn, 1)
			}
		}
	}()

	var wg sync.WaitGroup
	const writers = 4
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				pid := (w+1)*1000 + i
				sock := "/tmp/bosun-" + strconv.Itoa(pid) + ".sock"
				if err := writeMcpPidfile(repoRoot, pid, sock); err != nil {
					t.Errorf("writer %d: %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	<-done

	if got := atomic.LoadInt32(&torn); got != 0 {
		t.Fatalf("readMcpPidfile observed %d torn/invalid reads under concurrent atomic writes (want 0)", got)
	}

	// And the cleanup invariant: no leftover *.tmp-* files in the bosun
	// dir — every CreateTemp must have either succeeded into a rename
	// or been cleaned up on error.
	entries, err := os.ReadDir(filepath.Join(repoRoot, ".bosun"))
	if err != nil {
		t.Fatalf("read .bosun dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file after atomic pidfile writes: %s", e.Name())
		}
	}
}
