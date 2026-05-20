package remote

import (
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// parseSSHHost extracts the ssh CLI's positional host argument (and
// optional -p port flag) from a value that may be a bare user@host or
// a full `ssh://user@host:port[/...]` URI. Callers pass whatever the
// operator configured (config.docker.hosts entries are validated as
// ssh:// URIs by config.Validate, so the URI form is the common case;
// the bare-host form is honoured for parity with the `ssh` CLI).
//
// Returns hostArg = `user@host` (or `host` when user is empty) and
// optionally portArgs = ["-p", "<port>"] when the URI had a non-
// default port.
func parseSSHHost(s string) (hostArg string, portArgs []string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil, fmt.Errorf("empty host")
	}
	if !strings.Contains(s, "://") {
		// Bare form: pass through as-is, no port extraction.
		return s, nil, nil
	}
	u, parseErr := url.Parse(s)
	if parseErr != nil {
		return "", nil, fmt.Errorf("parse %q: %w", s, parseErr)
	}
	if u.Scheme != "ssh" {
		return "", nil, fmt.Errorf("unsupported scheme %q (want ssh://)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return "", nil, fmt.Errorf("ssh URI missing host: %q", s)
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	if user != "" {
		hostArg = user + "@" + host
	} else {
		hostArg = host
	}
	if p := u.Port(); p != "" {
		portArgs = []string{"-p", p}
	}
	return hostArg, portArgs, nil
}

// Tunnel owns an `ssh -R` reverse-proxy that exposes a local Unix
// socket to a remote host. Wraps the long-lived ssh child process so
// callers can Close() it deterministically rather than relying on
// parent-exit reaping.
//
// The remote socket path is cleaned up automatically when the SSH
// session dies — modern OpenSSH unlinks reverse-forward sockets on
// session teardown. Callers should not need to scrub the remote side
// themselves.
type Tunnel struct {
	cmd *exec.Cmd

	mu      sync.Mutex
	closed  bool
	exited  chan struct{} // closed when the underlying ssh process exits
	waitErr error         // populated by the watcher goroutine
}

// startupProbe is the post-Start grace period for OpenSSH's banner
// exchange + reverse-forward negotiation. Empirically OpenSSH fails
// fast (connection refused, permission denied, unknown host) within
// a few hundred ms on macOS / Linux. Long enough to catch the common
// "host unreachable" path; short enough that the operator doesn't
// notice the wait on the happy path.
const startupProbe = 500 * time.Millisecond

// execCommand is the package-level indirection that lets tests
// substitute a fake exec.Cmd factory. Production callers get the
// stock os/exec behaviour.
var execCommand = exec.Command

// OpenReverseProxy starts an ssh reverse-forward that exposes
// localSock on host at remotePath. The returned Tunnel holds the ssh
// child process — call Close() to tear it down deterministically.
//
// Implementation: `ssh -R <remotePath>:<localSock> <host> 'sleep
// infinity'`. The `sleep infinity` command keeps the SSH session
// open so the reverse forward stays alive; killing ssh kills the
// remote sleep via SIGHUP propagation.
//
// Sanity check: after Start, we wait startupProbe ms to confirm the
// ssh process didn't immediately exit (auth failure, network error,
// etc.). If it did, the underlying error is returned so the caller
// sees "host unreachable" up front instead of discovering it later
// when the in-container agent's MCP socket connect fails.
func OpenReverseProxy(localSock, remotePath, host string) (*Tunnel, error) {
	if localSock == "" {
		return nil, fmt.Errorf("remote: OpenReverseProxy: localSock is required")
	}
	if remotePath == "" {
		return nil, fmt.Errorf("remote: OpenReverseProxy: remotePath is required")
	}
	if host == "" {
		return nil, fmt.Errorf("remote: OpenReverseProxy: host is required")
	}
	// host may be either a bare user@host or a full ssh:// URI; ssh
	// CLI only accepts the bare form (and -p for port). Normalise
	// before constructing argv so an ssh:// URI from config doesn't
	// reach ssh as a literal "host" arg.
	hostArg, portArgs, err := parseSSHHost(host)
	if err != nil {
		return nil, fmt.Errorf("remote: OpenReverseProxy: %w", err)
	}

	// -o options disable interactive prompts so an SSH key prompt or
	// host-key challenge surfaces as a clean failure rather than
	// blocking the parent bosun process. ExitOnForwardFailure ensures
	// ssh exits non-zero when the reverse forward can't be set up
	// (e.g. remote socket path unwritable) — without it, ssh would
	// silently degrade and the in-container agent would fail later.
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
	}
	args = append(args, portArgs...)
	args = append(args,
		"-R", remotePath+":"+localSock,
		hostArg,
		"sleep infinity",
	)
	cmd := execCommand("ssh", args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("remote: start ssh -R: %w", err)
	}

	t := &Tunnel{cmd: cmd, exited: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		t.mu.Lock()
		t.waitErr = err
		t.mu.Unlock()
		close(t.exited)
	}()

	// Brief startup probe: if the ssh child died inside startupProbe,
	// surface the underlying error so callers see "host unreachable"
	// at OpenReverseProxy-return time rather than at some later MCP
	// connect attempt from inside the container.
	select {
	case <-t.exited:
		t.mu.Lock()
		err := t.waitErr
		t.mu.Unlock()
		if err == nil {
			return nil, fmt.Errorf("remote: ssh -R %s exited immediately with no error", host)
		}
		return nil, fmt.Errorf("remote: ssh -R %s failed during startup: %w", host, err)
	case <-time.After(startupProbe):
		// Healthy: ssh is still running after the probe window.
	}

	return t, nil
}

// Close terminates the ssh process and waits for it to exit. Safe to
// call multiple times — subsequent calls are no-ops.
//
// Close blocks until the ssh process has actually exited (via Wait)
// so callers can sequence cleanup deterministically (e.g. tear down
// the tunnel, then tear down the bare repo).
func (t *Tunnel) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	if t.cmd.Process != nil {
		// Best-effort kill. The watcher goroutine will populate
		// waitErr; we ignore Kill errors (process already exited)
		// because Wait() is the source of truth.
		_ = t.cmd.Process.Kill()
	}
	<-t.exited
	return nil
}

// Wait blocks until the underlying ssh process exits. Used by callers
// that want to tie their own lifecycle to the tunnel's (e.g. supervise
// a long-running container run and tear down the tunnel when it ends).
func (t *Tunnel) Wait() error {
	<-t.exited
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.waitErr
}
