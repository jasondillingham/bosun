package main

import (
	"sync"

	"github.com/jasondillingham/bosun/internal/remote"
)

// Phase 3 lane 2+3: bosun init keeps SSH reverse-proxy tunnels alive
// for the lifetime of the launched docker run.
//
// Lifetime story (pragmatic v1):
//
// The terminal launcher spawns `docker run` in a separate window and
// returns control immediately. cmd_init exits soon after — but the SSH
// child process is detached at fork time and the OS reparents it to
// init/launchd, so the tunnel survives bosun's exit.
//
// We can't tie tunnel lifetime to the per-window docker run process
// (cmd_init never sees its PID — the terminal app owns it). The
// pragmatic alternative would be a long-lived bosun supervisor that
// watches each docker run and closes the matching tunnel when it
// exits. That's outside the scope of lane 2+3 — the brief explicitly
// accepts "tunnels die when bosun init exits, which means they live
// roughly as long as the docker run because both are short interactive
// sessions in this v1 shape."
//
// The retain*Tunnel slice exists so the Tunnel struct (and the
// goroutine watching the ssh process) isn't garbage-collected mid-run
// while cmd_init is still spinning up subsequent sessions.
//
// Future lane: a `bosun watch` supervisor or per-session Tunnel cleanup
// in cmd_cleanup would close these deterministically.
var (
	retainedTunnelsMu sync.Mutex
	retainedTunnels   []*remote.Tunnel
)

// retainTunnel parks t in the package-level slice so its goroutine
// can't be GC'd before bosun init exits. Safe to call concurrently
// from multiple goroutines (lane 2+3 currently calls sequentially,
// but future per-session-parallel launch would benefit).
func retainTunnel(t *remote.Tunnel) {
	if t == nil {
		return
	}
	retainedTunnelsMu.Lock()
	defer retainedTunnelsMu.Unlock()
	retainedTunnels = append(retainedTunnels, t)
}
