package status

import (
	"encoding/json"
	"io"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/session"
)

type sessionJSON struct {
	Name        string `json:"name"`
	Number      int    `json:"number"`
	Branch      string `json:"branch"`
	Path        string `json:"path"`
	State       string `json:"state"`
	Ahead       int    `json:"ahead"`
	Dirty       int    `json:"dirty"`
	Claimed     int    `json:"claimed"`
	LastSHA     string `json:"last_sha,omitempty"`
	LastSubject string `json:"last_subject,omitempty"`
	LastRel     string `json:"last_relative,omitempty"`
	LastUnix    int64  `json:"last_unix,omitempty"`
	StateMsg    string `json:"state_message,omitempty"`
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
			Name:     s.Name,
			Number:   s.Number,
			Branch:   s.Branch,
			Path:     s.Path,
			State:    string(s.State),
			Ahead:    s.Ahead,
			Dirty:    s.Dirty,
			Claimed:  s.Claimed,
			StateMsg: s.StateMsg,
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
