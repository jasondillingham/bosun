// Package suggest implements `bosun suggest` — the brief-authoring
// assistant that takes a high-level goal, inspects the repo, asks
// Claude to propose disjoint per-session lanes, and writes a plan
// markdown the operator can edit then feed to `bosun init`.
//
// This file owns the proposal validator: every LaneProposal flows
// through Validate before it becomes a markdown plan. See the
// v0.5-suggest-spec for the full lane-design contract.
package suggest

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/session"
)

// LaneProposal is the top-level structured output from the proposer.
// Each entry in Sessions describes one bosun session's lane.
//
// LaneProposal and Lane types live in types.go (the canonical shared
// contract per the v0.5-round1 plan). Lane-3 originally duplicated them
// here while developing in isolation; merged out at integration time.

// OverlapError reports two lanes whose OwnedFiles patterns intersect.
// Carries both lane labels and both patterns so the operator can hand-
// edit the plan without re-running the proposer.
type OverlapError struct {
	LaneA, LaneB       string
	PatternA, PatternB string
}

func (e *OverlapError) Error() string {
	return fmt.Sprintf(
		"lane %q pattern %q overlaps lane %q pattern %q",
		e.LaneA, e.PatternA, e.LaneB, e.PatternB,
	)
}

// CycleError reports a dependency cycle. Cycle reads start → … → start
// (first and last entries equal) per brief.FindDependencyCycle's
// convention.
type CycleError struct {
	Cycle []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("dependency cycle: %s", strings.Join(e.Cycle, " → "))
}

// Validate runs every rule the v0.5 spec requires before a LaneProposal
// becomes a markdown plan. Hard violations (schema, file overlaps,
// cycles, bad labels) come back as the error return. Soft observations
// (avoid-list patterns that don't overlap any other lane's owned-files
// pattern) come back as the warnings slice.
//
// requestedCount is the operator's --sessions flag; the proposer must
// return exactly that many lanes. Pass 0 to skip the count check (used
// by ad-hoc validation of hand-edited plans where the operator may
// have added or removed lanes).
func Validate(p LaneProposal, requestedCount int) (warnings []string, err error) {
	if err := validateSchema(p, requestedCount); err != nil {
		return nil, err
	}
	if err := validateLabels(p); err != nil {
		return nil, err
	}
	if err := validateFileOverlap(p); err != nil {
		return nil, err
	}
	if err := validateDependencies(p); err != nil {
		return nil, err
	}
	return collectAvoidWarnings(p), nil
}

func validateSchema(p LaneProposal, requestedCount int) error {
	if len(p.Sessions) == 0 {
		return errors.New("proposal has no sessions")
	}
	if requestedCount > 0 && len(p.Sessions) != requestedCount {
		return fmt.Errorf(
			"proposal has %d sessions, want %d",
			len(p.Sessions), requestedCount,
		)
	}
	seen := map[string]struct{}{}
	for i, lane := range p.Sessions {
		if strings.TrimSpace(lane.Label) == "" {
			return fmt.Errorf("session[%d] has empty label", i)
		}
		if strings.TrimSpace(lane.Scope) == "" {
			return fmt.Errorf("session %q has empty scope", lane.Label)
		}
		if _, dup := seen[lane.Label]; dup {
			return fmt.Errorf("duplicate session label %q", lane.Label)
		}
		seen[lane.Label] = struct{}{}
	}
	return nil
}

func validateLabels(p LaneProposal) error {
	for _, lane := range p.Sessions {
		if err := session.ValidateLabel(lane.Label); err != nil {
			return fmt.Errorf("session label %q: %w", lane.Label, err)
		}
	}
	return nil
}

func validateFileOverlap(p LaneProposal) error {
	for i := range p.Sessions {
		for j := i + 1; j < len(p.Sessions); j++ {
			laneA := &p.Sessions[i]
			laneB := &p.Sessions[j]
			for _, pa := range laneA.OwnedFiles {
				for _, pb := range laneB.OwnedFiles {
					if patternsOverlap(pa, pb) {
						return &OverlapError{
							LaneA:    laneA.Label,
							LaneB:    laneB.Label,
							PatternA: pa,
							PatternB: pb,
						}
					}
				}
			}
		}
	}
	return nil
}

func validateDependencies(p LaneProposal) error {
	known := make(map[string]struct{}, len(p.Sessions))
	for _, lane := range p.Sessions {
		known[lane.Label] = struct{}{}
	}
	depMap := make(map[string][]string, len(p.Sessions))
	for _, lane := range p.Sessions {
		for _, d := range lane.DependsOn {
			if d == lane.Label {
				// Self-dependency. brief.FindDependencyCycle catches it
				// too, but spot-check per the brief — gives a tighter
				// error message ("X depends on itself" vs. a generic
				// cycle path) and never relies on the upstream
				// detector's exact behavior.
				return &CycleError{Cycle: []string{lane.Label, lane.Label}}
			}
			if _, ok := known[d]; !ok {
				return fmt.Errorf(
					"session %q depends on unknown session %q",
					lane.Label, d,
				)
			}
		}
		// Copy to avoid the map aliasing the proposal's underlying slice.
		deps := append([]string(nil), lane.DependsOn...)
		depMap[lane.Label] = deps
	}
	if cycle := brief.FindDependencyCycle(depMap); cycle != nil {
		return &CycleError{Cycle: cycle}
	}
	return nil
}

func collectAvoidWarnings(p LaneProposal) []string {
	var warnings []string
	for i := range p.Sessions {
		lane := &p.Sessions[i]
		for _, av := range lane.AvoidFiles {
			if !someoneOwns(av, p.Sessions, lane.Label) {
				warnings = append(warnings, fmt.Sprintf(
					"lane %q avoids %q but no other lane owns it",
					lane.Label, av,
				))
			}
		}
	}
	return warnings
}

func someoneOwns(pattern string, sessions []Lane, selfLabel string) bool {
	for i := range sessions {
		if sessions[i].Label == selfLabel {
			continue
		}
		for _, owned := range sessions[i].OwnedFiles {
			if patternsOverlap(pattern, owned) {
				return true
			}
		}
	}
	return false
}

// patternsOverlap reports whether any file path could match both globs.
// Heuristic — gives true positives for the common cases and biases
// toward over-reporting overlaps. Rationale: a brief with no real
// overlap that gets flagged costs the operator 30 seconds of manual
// review; a real overlap that ships costs hours of merge pain.
//
// Handled cases:
//   - Exact equality.
//   - Concrete-vs-concrete with directory containment.
//   - Concrete-vs-glob via regex match; also treats a bare path as a
//     directory whose contents the glob may cover.
//   - Glob-vs-glob via literal prefix + literal suffix compatibility.
func patternsOverlap(a, b string) bool {
	a = normalizePat(a)
	b = normalizePat(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	aWild := hasWildcards(a)
	bWild := hasWildcards(b)

	switch {
	case !aWild && !bWild:
		return isPrefixDir(a, b) || isPrefixDir(b, a)
	case !aWild:
		if globMatch(b, a) {
			return true
		}
		return isPrefixDir(a, literalPrefix(b))
	case !bWild:
		if globMatch(a, b) {
			return true
		}
		return isPrefixDir(b, literalPrefix(a))
	}

	// Both have wildcards.
	aPre, aSuf := literalPrefix(a), literalSuffix(a)
	bPre, bSuf := literalPrefix(b), literalSuffix(b)
	return prefixCompatible(aPre, bPre) && suffixCompatible(aSuf, bSuf)
}

func hasWildcards(p string) bool {
	return strings.ContainsAny(p, "*?[")
}

func normalizePat(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, `\`, "/")
	p = strings.TrimPrefix(p, "./")
	return p
}

func isPrefixDir(prefix, p string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return false
	}
	return strings.HasPrefix(p, prefix+"/")
}

func literalPrefix(p string) string {
	i := strings.IndexAny(p, "*?[")
	if i < 0 {
		return p
	}
	return p[:i]
}

func literalSuffix(p string) string {
	i := strings.LastIndexAny(p, "*?]")
	if i < 0 {
		return p
	}
	return p[i+1:]
}

func prefixCompatible(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	if a == b {
		return true
	}
	aT := strings.TrimSuffix(a, "/")
	bT := strings.TrimSuffix(b, "/")
	if aT == bT {
		return true
	}
	if strings.HasPrefix(a, b) {
		return strings.HasSuffix(b, "/") || a[len(b)] == '/'
	}
	if strings.HasPrefix(b, a) {
		return strings.HasSuffix(a, "/") || b[len(a)] == '/'
	}
	return false
}

func suffixCompatible(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	if a == b {
		return true
	}
	return strings.HasSuffix(a, b) || strings.HasSuffix(b, a)
}

func globMatch(pattern, path string) bool {
	re, err := globToRegex(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(path)
}

// globToRegex converts a path glob into a regex. Recognized syntax:
//
//	**/ → zero or more path segments (followed by /)
//	**  → any number of characters incl. /
//	*   → any number of non-slash characters
//	?   → one non-slash character
//	[…] → character class (passed through to regex unchanged)
//
// Everything else is literal. The result is anchored.
func globToRegex(p string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`\A`)
	i := 0
	for i < len(p) {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				if i+2 < len(p) && p[i+2] == '/' {
					b.WriteString(`(?:.*/)?`)
					i += 3
				} else {
					b.WriteString(`.*`)
					i += 2
				}
			} else {
				b.WriteString(`[^/]*`)
				i++
			}
		case '?':
			b.WriteString(`[^/]`)
			i++
		case '[':
			// Pass through character class as-is up to the closing ].
			end := strings.IndexByte(p[i+1:], ']')
			if end < 0 {
				// Unterminated class — treat literally.
				b.WriteString(regexp.QuoteMeta(p[i:]))
				i = len(p)
				continue
			}
			b.WriteString(p[i : i+1+end+1])
			i += 1 + end + 1
		case '.', '+', '(', ')', '|', '{', '}', '^', '$', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	b.WriteString(`\z`)
	return regexp.Compile(b.String())
}
