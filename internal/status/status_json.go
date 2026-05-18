package status

// The JSON output below is the stable contract behind both
// `bosun status --json` and the web `/api/status` endpoint. Both surfaces
// share this one payload — anything shipped here ships everywhere a
// script or dashboard can see it.
//
// Schema (v0.1, stable). Top-level object:
//
//	{
//	  "sessions": [ <session>, ... ],   // always present, may be empty
//	  "overlaps": [ <overlap>, ... ]    // omitted unless requested
//	}
//
// session object (every field present unless marked optional):
//
//	name            string  e.g. "session-1" or "auth"
//	number          int     1-based session number; 0 for named sessions
//	branch          string  e.g. "bosun/session-1"
//	path            string  absolute worktree path
//	state           string  "WORKING" | "DONE" | "STUCK" | "CRASHED"
//	ahead           int     commits ahead of base branch
//	dirty           int     count of uncommitted tracked-file changes
//	claimed         int     count of distinct claimed paths
//	running         bool    true when an agent process lives in the worktree
//	running_pid     int     pid; omitted when running=false (omitempty)
//	running_external bool   true when liveness_gate=external (operator-driven); omitted when false (omitempty)
//	stale           bool    true when WORKING+heartbeat older than 5min; omitted when false (omitempty)
//	heartbeat_unix  int64   unix timestamp of last heartbeat; omitted when no heartbeat recorded
//	last_sha        string  short SHA of the last commit; omitted when ahead=0
//	last_subject    string  subject line of the last commit; omitted when ahead=0
//	last_relative   string  human relative time, e.g. "3 minutes ago"; omitted when ahead=0
//	last_unix       int64   unix timestamp of the last commit; omitted when ahead=0
//	state_message   string  body of the .done/.stuck marker file; omitted when blank
//	parent          string  label of the session that spawned this one; omitted for top-level
//	children        []string labels of sub-sessions spawned by this one; omitted when empty
//	depth           int     0 for top-level, parent.Depth+1 for sub-sessions; omitted when 0
//
// overlap object:
//
//	path      string    repo-relative path claimed by multiple sessions
//	sessions  []string  session names that claim the path
//
// Stability promise:
//   - Keys and types above will not change within the v0.1 line.
//   - New fields may be added (additive); consumers should ignore unknown keys.
//   - `omitempty` keys disappear when zero-valued; a typed-struct consumer
//     gets the zero value, a raw-map consumer must handle absence. Don't
//     change a non-omitempty key to omitempty without bumping the version.
//   - Removing or renaming a key is a breaking change reserved for v0.2+.
//   - `sessions` order matches session.Derive's sort: numeric sessions
//     ascend by number, named sessions follow in label-alphabetical order.

import (
	"encoding/json"
	"io"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/session"
)

// JSONSchemaVersion is the wire-stable version tag emitted by bosun's
// machine-readable JSON outputs (`bosun list --json`, `bosun show --json`).
// Consumers can switch on this string to detect breaking changes; additive
// field changes keep the same version. `bosun status --json` predates this
// constant and intentionally does not surface it — its stability promise
// lives in the doc comment at the top of this file.
const JSONSchemaVersion = "v0.4.0"

// sessionJSON is the per-session row in the public payload. Field tags are
// the stable wire names — see the package doc above before renaming.
type sessionJSON struct {
	Name          string `json:"name"`
	Number        int    `json:"number"`
	Branch        string `json:"branch"`
	Path          string `json:"path"`
	State         string `json:"state"`
	Ahead         int    `json:"ahead"`
	Dirty         int    `json:"dirty"`
	Claimed       int    `json:"claimed"`
	Running         bool `json:"running"`
	RunningPID      int  `json:"running_pid,omitempty"`
	RunningExternal bool `json:"running_external,omitempty"`
	Stale           bool `json:"stale,omitempty"`
	HeartbeatUnix int64  `json:"heartbeat_unix,omitempty"`
	LastSHA       string `json:"last_sha,omitempty"`
	LastSubject   string `json:"last_subject,omitempty"`
	LastRel       string `json:"last_relative,omitempty"`
	LastUnix      int64  `json:"last_unix,omitempty"`
	StateMsg      string `json:"state_message,omitempty"`
	// Parent/Children/Depth surface the spawn-tree shape to consumers
	// that want to render it (web dashboard, TUI). All three are
	// omitempty: top-level sessions with no children produce the same
	// minimal payload as v0.8.
	Parent   string   `json:"parent,omitempty"`
	Children []string `json:"children,omitempty"`
	Depth    int      `json:"depth,omitempty"`
}

type overlapJSON struct {
	Path     string   `json:"path"`
	Sessions []string `json:"sessions"`
}

type statusJSON struct {
	Sessions []sessionJSON `json:"sessions"`
	Overlaps []overlapJSON `json:"overlaps,omitempty"`
}

// RenderJSON writes the status payload as machine-readable JSON.
func RenderJSON(w io.Writer, sessions []session.Session, overlaps []claims.Overlap, withOverlaps bool) error {
	payload := statusJSON{
		Sessions: make([]sessionJSON, 0, len(sessions)),
	}
	for _, s := range sessions {
		row := sessionJSON{
			Name:            s.Name,
			Number:          s.Number,
			Branch:          s.Branch,
			Path:            s.Path,
			State:           string(s.State),
			Ahead:           s.Ahead,
			Dirty:           s.Dirty,
			Claimed:         s.Claimed,
			Running:         s.Running,
			RunningPID:      s.RunningPID,
			RunningExternal: s.RunningExternal,
			Stale:           s.Stale,
			StateMsg:        s.StateMsg,
			Parent:          s.Parent,
			Children:        s.Children,
			Depth:           s.Depth,
		}
		if !s.HeartbeatAt.IsZero() {
			row.HeartbeatUnix = s.HeartbeatAt.Unix()
		}
		if s.Last != nil {
			row.LastSHA = s.Last.ShortSHA
			row.LastSubject = s.Last.Subject
			row.LastRel = s.Last.Relative
			row.LastUnix = s.Last.Unix
		}
		payload.Sessions = append(payload.Sessions, row)
	}
	if withOverlaps {
		payload.Overlaps = make([]overlapJSON, 0, len(overlaps))
		for _, o := range overlaps {
			payload.Overlaps = append(payload.Overlaps, overlapJSON{Path: o.Path, Sessions: o.Sessions})
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}
