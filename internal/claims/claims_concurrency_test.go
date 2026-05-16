package claims

import (
	"fmt"
	"sort"
	"sync"
	"testing"
)

// TestAdd_ConcurrentSameSessionPreservesAllPaths exercises the read-modify-
// write race in Add. Two goroutines that concurrently append distinct paths
// to the same session must both land on disk; a naive Read→merge→Write loop
// loses one of them when both reads see the same baseline.
func TestAdd_ConcurrentSameSessionPreservesAllPaths(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	const goroutines = 16
	const pathsPerG = 8

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < pathsPerG; i++ {
				p := fmt.Sprintf("g%02d/file%02d.go", g, i)
				if err := s.Add("session-1", []string{p}); err != nil {
					t.Errorf("g%d add %q: %v", g, p, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	c, err := s.Read("session-1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if c == nil {
		t.Fatal("claim file missing after concurrent adds")
	}
	want := goroutines * pathsPerG
	if len(c.Paths) != want {
		// Sort + log first few mismatches to make it obvious which
		// paths got swallowed by lost updates.
		got := append([]string(nil), c.Paths...)
		sort.Strings(got)
		t.Fatalf("paths after concurrent Add = %d, want %d (lost-update race?). first 5 of %d: %v",
			len(c.Paths), want, len(got), firstN(got, 5))
	}
}

// TestAdd_ConcurrentDistinctSessionsAreIndependent confirms that the lock
// added to fix same-session races doesn't accidentally serialize work
// across sessions to the point of losing data — each session-N file should
// end up with exactly its goroutine's contributions.
func TestAdd_ConcurrentDistinctSessionsAreIndependent(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	const sessions = 8
	const pathsPerS = 8

	var wg sync.WaitGroup
	wg.Add(sessions)
	for sid := 0; sid < sessions; sid++ {
		go func(sid int) {
			defer wg.Done()
			name := fmt.Sprintf("session-%d", sid)
			for i := 0; i < pathsPerS; i++ {
				p := fmt.Sprintf("file%02d.go", i)
				if err := s.Add(name, []string{p}); err != nil {
					t.Errorf("%s add: %v", name, err)
					return
				}
			}
		}(sid)
	}
	wg.Wait()

	for sid := 0; sid < sessions; sid++ {
		name := fmt.Sprintf("session-%d", sid)
		c, err := s.Read(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if c == nil || len(c.Paths) != pathsPerS {
			t.Fatalf("%s paths = %v, want %d entries", name, c, pathsPerS)
		}
	}
}

// TestAdd_RemoveInterleavingDoesNotPanic stresses the path where Add and
// Remove run concurrently against the same session — primarily to make sure
// the lock covers Remove too (otherwise we'd see a sometimes-empty path
// list after a successful Add).
func TestAdd_RemoveInterleavingDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	if err := s.Add("session-1", []string{"keep.go"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = s.Add("session-1", []string{fmt.Sprintf("a%d.go", i)})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_, _ = s.Remove("session-1", []string{fmt.Sprintf("a%d.go", i)})
		}
	}()
	wg.Wait()

	// "keep.go" must still be present — the test isn't asserting a precise
	// count from the racing Add/Remove, just that the seeded path survives
	// (proving Remove never wiped the whole file when only one path was
	// requested for removal).
	c, err := s.Read("session-1")
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	found := false
	if c != nil {
		for _, p := range c.Paths {
			if p == "keep.go" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("keep.go vanished during concurrent Add/Remove: %+v", c)
	}
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
