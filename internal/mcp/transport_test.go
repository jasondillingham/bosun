package mcp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestReadLineCapped_AcceptsNormalFrame pins the happy path — a
// newline-delimited frame under the cap is returned without the newline.
func TestReadLineCapped_AcceptsNormalFrame(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("hello world\n"))
	got, err := readLineCapped(in, 1024)
	if err != nil {
		t.Fatalf("readLineCapped: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

// TestReadLineCapped_RefusesOversizedFrame is the Bughunt-1 F018
// regression: a frame that exceeds the cap MUST return errFrameTooLarge
// instead of growing the buffer unbounded. Pre-fix bufio.ReadBytes('\n')
// would allocate the full N bytes whatever N was.
func TestReadLineCapped_RefusesOversizedFrame(t *testing.T) {
	// Build a payload of cap+1 bytes WITHOUT a newline. The reader
	// should refuse exactly at cap+1.
	const cap = 32
	payload := bytes.Repeat([]byte("A"), cap+1)
	in := bufio.NewReader(bytes.NewReader(payload))
	got, err := readLineCapped(in, cap)
	if err == nil {
		t.Fatal("expected errFrameTooLarge; got nil")
	}
	if !errors.Is(err, errFrameTooLarge) {
		t.Errorf("got %v, want errFrameTooLarge", err)
	}
	if got != nil {
		t.Errorf("got partial buf %q, want nil on refusal", got)
	}
}

// TestReadLineCapped_AcceptsExactlyAtCap pins the off-by-one: a frame
// of exactly `cap` bytes followed by a newline is fine.
func TestReadLineCapped_AcceptsExactlyAtCap(t *testing.T) {
	const cap = 32
	payload := append(bytes.Repeat([]byte("B"), cap), '\n')
	in := bufio.NewReader(bytes.NewReader(payload))
	got, err := readLineCapped(in, cap)
	if err != nil {
		t.Fatalf("readLineCapped at exact cap: %v", err)
	}
	if len(got) != cap {
		t.Errorf("got len=%d, want %d", len(got), cap)
	}
}

// TestReadLineCapped_ReturnsEOFOnEarlyClose verifies the contract for
// the SDK runtime: when the underlying conn closes mid-frame, we return
// the underlying error (io.EOF) so the MCP runtime can tear down the
// connection cleanly. This matches the pre-fix bufio.ReadBytes behavior
// — we MUST preserve it so error-handling upstream stays the same.
func TestReadLineCapped_ReturnsEOFOnEarlyClose(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("partial"))
	_, err := readLineCapped(in, 1024)
	if !errors.Is(err, io.EOF) {
		t.Errorf("got %v, want io.EOF", err)
	}
}

// TestMaxFrameBytes_Sanity is a static check that the cap stays above
// every realistic legitimate frame size. If a future tool needs a
// >16 MiB request body, this assertion is the place to revisit.
func TestMaxFrameBytes_Sanity(t *testing.T) {
	// The brief-text cap on bosun_spawn is 256 KiB; tools/list output
	// is ~30 KiB. The cap must comfortably exceed both.
	const briefCap = 256 << 10
	if maxFrameBytes < briefCap*8 {
		t.Errorf("maxFrameBytes=%d is too tight against briefCap=%d", maxFrameBytes, briefCap)
	}
}
