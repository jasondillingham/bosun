package remote

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain skips the entire sshtunnel test suite when `sleep` isn't
// on PATH. withFakeSSH below stubs the ssh binary with an exec of
// `sleep <duration>` to simulate a long-running tunnel; Windows
// runners have no `sleep` on default PATH. Production sshtunnel
// code is Linux-shaped (the SSH bridge for remote-docker) and not
// load-bearing on Windows operator boxes, so dropping these tests
// on Windows doesn't cost coverage that matters.
//
// Exit 0 keeps the package reported as PASS on windows-latest CI.
// Windows-trial finding 2026-05-22 — see
// docs/windows-trial-2026-05-22.md.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("sleep"); err != nil {
		fmt.Fprintln(os.Stderr, "sshtunnel tests skipped: sleep not on PATH (Windows runner or trimmed env)")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// withFakeSSH swaps execCommand for a factory that runs a stand-in
// process instead of real ssh. The stand-in is a `sleep` whose
// duration the test controls — long enough to look "healthy" past
// startupProbe, short enough to not waste wall-clock when the test
// kills it. Returns the captured argv so tests can pin the shape of
// the ssh invocation that production callers would have made.
func withFakeSSH(t *testing.T, sleepFor string) *[]string {
	t.Helper()
	captured := &[]string{}
	prev := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		*captured = append(*captured, name)
		*captured = append(*captured, args...)
		// Substitute the stand-in: a sleep that lives long enough
		// for the startup probe to consider the tunnel healthy.
		return exec.Command("sleep", sleepFor)
	}
	t.Cleanup(func() { execCommand = prev })
	return captured
}

// TestOpenReverseProxy_BuildsExpectedSSHCommand pins the ssh argv
// shape: -R remote:local, the host, and the keep-alive command.
// Regression-guards against silent flag drift that would break the
// reverse-forward contract.
func TestOpenReverseProxy_BuildsExpectedSSHCommand(t *testing.T) {
	captured := withFakeSSH(t, "2")

	tun, err := OpenReverseProxy("/tmp/host.sock", "/work/.bosun/mcp.sock", "ssh://user@example.com")
	if err != nil {
		t.Fatalf("OpenReverseProxy: %v", err)
	}
	t.Cleanup(func() { _ = tun.Close() })

	joined := strings.Join(*captured, " ")
	for _, want := range []string{
		"ssh",
		"-R /work/.bosun/mcp.sock:/tmp/host.sock",
		// The ssh:// scheme is stripped by parseSSHHost — ssh CLI's
		// positional host arg can't carry a URL.
		"user@example.com",
		"sleep infinity",
		"ExitOnForwardFailure=yes",
		"BatchMode=yes",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh argv missing %q\nfull: %s", want, joined)
		}
	}
	if strings.Contains(joined, "ssh://") {
		t.Errorf("ssh argv should NOT contain raw ssh:// scheme:\nfull: %s", joined)
	}
}

// TestOpenReverseProxy_AddsPortFlagFromURI: ssh URIs with a non-
// default port should produce `-p N` in argv (ssh CLI doesn't
// accept port in the host argument).
func TestOpenReverseProxy_AddsPortFlagFromURI(t *testing.T) {
	captured := withFakeSSH(t, "2")
	tun, err := OpenReverseProxy("/tmp/host.sock", "/work/.bosun/mcp.sock", "ssh://op@example.com:2222")
	if err != nil {
		t.Fatalf("OpenReverseProxy: %v", err)
	}
	t.Cleanup(func() { _ = tun.Close() })

	joined := strings.Join(*captured, " ")
	if !strings.Contains(joined, "-p 2222") {
		t.Errorf("expected -p 2222 in argv, got: %s", joined)
	}
	if !strings.Contains(joined, "op@example.com") || strings.Contains(joined, "op@example.com:2222") {
		t.Errorf("expected host arg as op@example.com (no :port), got: %s", joined)
	}
}

// TestOpenReverseProxy_AcceptsBareHost: legacy bare-host form
// (user@host without ssh:// scheme) still works — parseSSHHost
// passes it through unchanged.
func TestOpenReverseProxy_AcceptsBareHost(t *testing.T) {
	captured := withFakeSSH(t, "2")
	tun, err := OpenReverseProxy("/tmp/host.sock", "/work/.bosun/mcp.sock", "user@example.com")
	if err != nil {
		t.Fatalf("OpenReverseProxy with bare host: %v", err)
	}
	t.Cleanup(func() { _ = tun.Close() })

	joined := strings.Join(*captured, " ")
	if !strings.Contains(joined, "user@example.com") {
		t.Errorf("expected user@example.com in argv, got: %s", joined)
	}
}

// TestOpenReverseProxy_RejectsEmptyInputs catches the operator-side
// "forgot to thread the local socket through" bug before ssh would
// emit a confusing "bad forwarding specification" error.
func TestOpenReverseProxy_RejectsEmptyInputs(t *testing.T) {
	if _, err := OpenReverseProxy("", "/work/x", "host"); err == nil {
		t.Errorf("expected error for empty localSock")
	}
	if _, err := OpenReverseProxy("/tmp/x.sock", "", "host"); err == nil {
		t.Errorf("expected error for empty remotePath")
	}
	if _, err := OpenReverseProxy("/tmp/x.sock", "/work/x", ""); err == nil {
		t.Errorf("expected error for empty host")
	}
}

// TestOpenReverseProxy_StartupFailureSurfacesError: if ssh exits
// inside the startup probe window, OpenReverseProxy should return
// the error rather than handing back a dead tunnel. Simulated by
// swapping in a no-op `true` command (exits 0 immediately).
func TestOpenReverseProxy_StartupFailureSurfacesError(t *testing.T) {
	prev := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		// `true` exits 0 immediately — close enough to "ssh died
		// during connect" for the probe-window logic.
		return exec.Command("true")
	}
	t.Cleanup(func() { execCommand = prev })

	_, err := OpenReverseProxy("/tmp/host.sock", "/work/.bosun/mcp.sock", "ssh://nope")
	if err == nil {
		t.Errorf("expected error when ssh exits during startup probe")
	}
}

// TestTunnel_CloseKillsProcessAndWaits: Close must terminate the
// underlying process and block until it's reaped. Without that, a
// cmd_init that closes tunnels at shutdown could race a still-alive
// ssh child outliving bosun.
func TestTunnel_CloseKillsProcessAndWaits(t *testing.T) {
	withFakeSSH(t, "10")

	tun, err := OpenReverseProxy("/tmp/host.sock", "/work/x", "host")
	if err != nil {
		t.Fatalf("OpenReverseProxy: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- tun.Close() }()

	select {
	case err := <-done:
		// Close itself returns nil regardless of how the child
		// exited — the contract is "process is gone", not "process
		// exited cleanly".
		if err != nil {
			t.Errorf("unexpected Close error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Errorf("Close didn't return within 3s — likely deadlock")
	}
}

// TestTunnel_CloseIsIdempotent: callers that defer Close() AND get
// called from a watchdog path shouldn't panic on double-close.
func TestTunnel_CloseIsIdempotent(t *testing.T) {
	withFakeSSH(t, "5")

	tun, err := OpenReverseProxy("/tmp/host.sock", "/work/x", "host")
	if err != nil {
		t.Fatalf("OpenReverseProxy: %v", err)
	}
	if err := tun.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := tun.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestTunnel_WaitReturnsAfterProcessExit: callers that supervise the
// tunnel (e.g. "kill the docker run when ssh dies") use Wait() as
// the blocking primitive. Verify it unblocks once the process is
// gone — using the stand-in sleep that we kill via Close.
func TestTunnel_WaitReturnsAfterProcessExit(t *testing.T) {
	withFakeSSH(t, "10")

	tun, err := OpenReverseProxy("/tmp/host.sock", "/work/x", "host")
	if err != nil {
		t.Fatalf("OpenReverseProxy: %v", err)
	}

	// Wait in the background; Close should unblock it.
	waited := make(chan struct{})
	go func() {
		_ = tun.Wait()
		close(waited)
	}()

	_ = tun.Close()
	select {
	case <-waited:
		// good
	case <-time.After(3 * time.Second):
		t.Errorf("Wait didn't return after Close within 3s")
	}
}
