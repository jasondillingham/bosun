package web

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jasondillingham/bosun/internal/brief"
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
	mux.HandleFunc("/api/show/", s.handleShow)
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

// showJSON is the per-session detail payload behind GET /api/show/<session>.
// Mirrors the per-row shape from /api/status (name/branch/path/state/etc.)
// and adds two fields the dashboard's brief-preview pane needs: the
// claimed paths and the worktree's BOSUN_BRIEF.md. The full shape will
// converge with `bosun show --json` once session-3 merges; until then
// the field names are chosen to align (snake_case, same keys as
// /api/status where they overlap).
type showJSON struct {
	Name         string   `json:"name"`
	Number       int      `json:"number"`
	Branch       string   `json:"branch"`
	Path         string   `json:"path"`
	State        string   `json:"state"`
	StateMsg     string   `json:"state_message,omitempty"`
	Ahead        int      `json:"ahead"`
	Dirty        int      `json:"dirty"`
	Claimed      int      `json:"claimed"`
	Running      bool     `json:"running"`
	RunningPID   int      `json:"running_pid,omitempty"`
	LastSHA      string   `json:"last_sha,omitempty"`
	LastSubject  string   `json:"last_subject,omitempty"`
	LastRel      string   `json:"last_relative,omitempty"`
	LastUnix     int64    `json:"last_unix,omitempty"`
	ClaimedPaths []string `json:"claimed_paths"`
	Brief        string   `json:"brief"`
}

// handleShow returns the per-session detail payload for the path
// /api/show/<label>. Returns 404 when the label doesn't resolve to a live
// bosun worktree — the dashboard relies on that to render an "unknown
// session" message rather than a stale preview.
func (s *Server) handleShow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := strings.TrimPrefix(r.URL.Path, "/api/show/")
	// Reject sub-paths like /api/show/session-1/extra so the route stays
	// strict — a typo shouldn't silently match the first segment.
	if raw == "" || strings.Contains(raw, "/") {
		http.NotFound(w, r)
		return
	}
	label, err := session.ParseLabel(raw)
	if err != nil {
		http.Error(w, "bosun: "+err.Error(), http.StatusBadRequest)
		return
	}

	sessions, err := session.Derive(r.Context(), s.cfg.Git, s.cfg.Cfg, s.cfg.RepoRoot, s.cfg.State, s.cfg.Claims)
	if err != nil {
		http.Error(w, "bosun: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var found *session.Session
	for i := range sessions {
		if sessions[i].Label == label {
			found = &sessions[i]
			break
		}
	}
	if found == nil {
		http.NotFound(w, r)
		return
	}

	// Claimed-paths file is optional — a session that hasn't called
	// `bosun claim` yet has no file. nil claim is not an error.
	var paths []string
	if c, err := s.cfg.Claims.Read(found.Name); err != nil {
		http.Error(w, "bosun: "+err.Error(), http.StatusInternalServerError)
		return
	} else if c != nil {
		paths = c.Paths
	}
	if paths == nil {
		paths = []string{}
	}

	briefBody, err := brief.ReadFromWorktree(found.Path)
	if err != nil {
		http.Error(w, "bosun: "+err.Error(), http.StatusInternalServerError)
		return
	}

	row := showJSON{
		Name:         found.Name,
		Number:       found.Number,
		Branch:       found.Branch,
		Path:         found.Path,
		State:        string(found.State),
		StateMsg:     found.StateMsg,
		Ahead:        found.Ahead,
		Dirty:        found.Dirty,
		Claimed:      found.Claimed,
		Running:      found.Running,
		RunningPID:   found.RunningPID,
		ClaimedPaths: paths,
		Brief:        briefBody,
	}
	if found.Last != nil {
		row.LastSHA = found.Last.ShortSHA
		row.LastSubject = found.Last.Subject
		row.LastRel = found.Last.Relative
		row.LastUnix = found.Last.Unix
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(row)
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
