// Package web exposes the same data `bosun status` shows over HTTP: a
// JSON snapshot at /api/status and a server-sent-events stream at
// /api/events. A small embedded static page at / consumes both so the
// operator can keep a browser tab open instead of re-running the CLI.
package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/state"
)

// Config bundles the inputs the web server needs from the caller. Mirrors
// the runCtx fields used by `bosun status` so the handlers can rebuild
// the session list each tick without dragging cmd/bosun into the
// dependency graph of this package.
type Config struct {
	RepoRoot string
	Git      *git.Client
	Cfg      config.Config
	Claims   *claims.Store
	State    *state.Store

	// Bind is the address (host or host:port) the server listens on.
	// Defaults are applied by the caller (cmd_serve.go).
	Bind string
	Port int

	// Interval is how often /api/status recomputes its underlying data
	// when called. The handler caches the last payload for this long so
	// rapid polling doesn't fork a `git status` per request.
	Interval time.Duration

	// MaxConnections caps concurrent inbound HTTP connections. Zero or
	// negative means no cap (preserves legacy behaviour). The default
	// (set by cmd_serve.go) is DefaultMaxConnections. 2026-05 bug-hunt
	// pass-2 #9: SSE handlers spawn a goroutine + a per-second poll
	// ticker each, so an attacker (or curious script) opening 10K
	// connections accumulates 10K goroutines and timers. The cap closes
	// that path. Operators exposing the dashboard via --bind
	// non-loopback can set this lower; localhost-only operators rarely
	// need to touch it.
	MaxConnections int
}

// DefaultMaxConnections is the cmd_serve.go default for
// Config.MaxConnections. 64 is generous for personal dev use (one
// browser tab is one SSE + occasional XHR) and small enough to make a
// flood obvious in `bosun serve`'s log.
const DefaultMaxConnections = 64

// maxRequestBytes caps the inbound HTTP body the dashboard handlers
// will read. Every handler today is GET, so this is defense-in-depth
// — a misconfigured client (or a future POST endpoint) can't drive
// unbounded allocator pressure via Content-Length. 1 MiB is far above
// anything legitimate.
const maxRequestBytes = 1 << 20

// maxHeaderBytes caps inbound HTTP header size. Default Go behaviour
// is 1 MiB; the explicit constant matches and pins it against any
// future stdlib default change.
const maxHeaderBytes = 1 << 20

// dashboardCSP is the Content-Security-Policy header value the
// security middleware emits. The embedded index.html uses inline
// <style> and <script> blocks (no external resources), so
// script-src / style-src must allow 'unsafe-inline'. Everything else
// is locked to 'self': no external scripts, no framing, no
// connections to anywhere but the bosun server itself.
//
// 'unsafe-inline' is a real CSP-strength regression compared to
// nonce-based CSP, but bosun's dashboard renders zero untrusted
// content — there's no XSS sink — so the practical risk is bounded.
// frame-ancestors 'none' + X-Frame-Options: DENY are the
// load-bearing protections against the malicious-browser-tab vector
// for an operator who hits bosun serve while another tab is open.
const dashboardCSP = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'"

// securityHeaders is the middleware that fronts every dashboard
// handler. Sets defense-in-depth headers and caps request bodies.
// Applied once in Start so the handler chain stays simple.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", dashboardCSP)
		h.Set("Referrer-Policy", "no-referrer")
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		next.ServeHTTP(w, r)
	})
}

// Server wraps an *http.Server with the bosun-specific handler set and
// owns the listener lifecycle. Start blocks until ctx is cancelled.
type Server struct {
	cfg Config
	srv *http.Server

	// addr is captured at listen time so tests that ask for a random
	// port (":0") can discover the assigned one via Addr(). Guarded by
	// mu because Start writes it from a goroutine that Addr's callers
	// (tests, status-line printers) race against.
	mu   sync.Mutex
	addr string
}

// New returns a Server ready to listen. Call Start to bind and serve.
func New(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// limitListener wraps inner so it accepts at most n connections at
// once. Inlined here instead of pulling in golang.org/x/net/netutil
// (the tree-wide rule is "no third-party deps without strong
// justification" and this is 20 lines of code).
type limitedListener struct {
	net.Listener
	sem chan struct{}
}

func limitListener(inner net.Listener, n int) net.Listener {
	return &limitedListener{Listener: inner, sem: make(chan struct{}, n)}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{} // block until a slot is free
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitedConn{Conn: c, sem: l.sem}, nil
}

// limitedConn releases its semaphore slot on Close so the cap reflects
// "currently open" connections, not "ever opened."
type limitedConn struct {
	net.Conn
	once sync.Once
	sem  chan struct{}
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { <-c.sem })
	return err
}

// Addr returns the network address the server is bound to. Only useful
// after Start has begun listening (i.e. in tests that pass port 0).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Start binds the configured listener and serves until ctx is cancelled.
// Returns nil on a clean shutdown (context cancellation) so the caller
// can translate SIGINT into a zero exit code.
func (s *Server) Start(ctx context.Context) error {
	bind := s.cfg.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	// Allow the caller to pass either a bare host (and a port) or a full
	// host:port via Bind. The "has colon" check is sufficient because
	// IPv6 literals are expected in [bracketed] form.
	addr := bind
	if _, _, err := net.SplitHostPort(bind); err != nil {
		addr = fmt.Sprintf("%s:%d", bind, s.cfg.Port)
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	// Wrap the listener with a connection cap when one is configured.
	// netutil.LimitListener is the canonical Go primitive for this; we
	// inline a tiny equivalent so the wider tree doesn't grow a new
	// third-party dep. Refusing the Accept loop's caller (rather than
	// accepting and immediately closing) is the right behaviour — the
	// client sees ECONNREFUSED instead of a connection that hangs.
	if s.cfg.MaxConnections > 0 {
		lis = limitListener(lis, s.cfg.MaxConnections)
	}
	s.mu.Lock()
	s.addr = lis.Addr().String()
	s.mu.Unlock()

	mux := http.NewServeMux()
	s.registerHandlers(mux)
	s.srv = &http.Server{
		Handler: securityHeaders(mux),
		// SSE clients hold the connection open; no read/write timeouts
		// here on purpose. The handler exits when the request context
		// is cancelled (client disconnect or server shutdown).
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	// Run Serve in a goroutine so we can race it against ctx cancellation.
	errCh := make(chan error, 1)
	go func() {
		err := s.srv.Serve(lis)
		// http.ErrServerClosed is the clean-shutdown signal; surface
		// anything else.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		// Give in-flight requests a brief grace period to finish, then
		// force-close. SSE handlers exit cleanly when their request
		// context is cancelled.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		<-errCh
		return nil
	case err := <-errCh:
		return err
	}
}
