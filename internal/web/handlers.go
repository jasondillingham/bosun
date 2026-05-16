package web

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/status"
)

//go:embed static/index.html
var staticFS embed.FS

// statusCache holds the most-recently-computed snapshot of session state.
// /api/status reuses it within s.cfg.Interval so a chatty browser poll
// can't trigger a fresh `git status` on every refresh.
type statusCache struct {
	sync.Mutex
	at       time.Time
	sessions []session.Session
	overlaps []claims.Overlap
}

func (s *Server) registerHandlers(mux *http.ServeMux) {
	// Serve the embedded index.html at /. We intentionally don't expose
	// the whole static/ directory — there's only one file and serving
	// arbitrary embed contents at / would surprise anyone who later
	// adds a non-public file under static/.
	indexBytes, _ := fs.ReadFile(staticFS, "static/index.html")

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexBytes)
	})

	cache := &statusCache{}
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		s.handleStatus(w, r, cache)
	})
	mux.HandleFunc("/api/events", s.handleEvents)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, cache *statusCache) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessions, overlaps, err := s.snapshot(r.Context(), cache)
	if err != nil {
		http.Error(w, "bosun: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Always include overlaps in the JSON payload — the web dashboard
	// surfaces them when present, and CLI parity (`bosun status --json
	// --with-overlaps`) doesn't matter here since the consumer is the
	// embedded UI, not a script.
	if err := status.RenderJSON(w, sessions, overlaps, true); err != nil {
		// Headers already flushed; nothing useful to say to the client.
		return
	}
}

// snapshot returns the cached session list when it's fresh, otherwise
// recomputes from git/claims/state. Cache TTL is s.cfg.Interval — set
// to 0 to disable caching (tests rely on this).
func (s *Server) snapshot(ctx context.Context, cache *statusCache) ([]session.Session, []claims.Overlap, error) {
	cache.Lock()
	if s.cfg.Interval > 0 && !cache.at.IsZero() && time.Since(cache.at) < s.cfg.Interval {
		sessions, overlaps := cache.sessions, cache.overlaps
		cache.Unlock()
		return sessions, overlaps, nil
	}
	cache.Unlock()

	sessions, err := session.Derive(ctx, s.cfg.Git, s.cfg.Cfg, s.cfg.RepoRoot, s.cfg.State, s.cfg.Claims)
	if err != nil {
		return nil, nil, err
	}
	overlaps, err := s.cfg.Claims.Overlaps()
	if err != nil {
		return nil, nil, err
	}

	cache.Lock()
	cache.sessions = sessions
	cache.overlaps = overlaps
	cache.at = time.Now()
	cache.Unlock()

	return sessions, overlaps, nil
}
