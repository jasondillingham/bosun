package briefscaffold

import (
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/brief"
)

func TestNames_ContainsAllFour(t *testing.T) {
	got := Names()
	want := []string{"recipe", "review", "audit", "cleanup"}
	if len(got) != len(want) {
		t.Fatalf("Names() len = %d, want %d (got %v)", len(got), len(want), got)
	}
	gotSet := make(map[string]struct{}, len(got))
	for _, n := range got {
		gotSet[n] = struct{}{}
	}
	for _, w := range want {
		if _, ok := gotSet[w]; !ok {
			t.Errorf("Names() missing %q (got %v)", w, got)
		}
	}
}

func TestGet_UnknownPatternErrors(t *testing.T) {
	_, err := Get("nonesuch")
	if err == nil {
		t.Fatal("Get(\"nonesuch\") returned nil error")
	}
	// Error must surface available names so the user can recover.
	for _, want := range []string{"recipe", "review", "audit", "cleanup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %v", want, err)
		}
	}
}

func TestGet_KnownPatternsHaveBody(t *testing.T) {
	for _, name := range []string{"recipe", "review", "audit", "cleanup"} {
		t.Run(name, func(t *testing.T) {
			p, err := Get(name)
			if err != nil {
				t.Fatalf("Get(%q): %v", name, err)
			}
			if p.Name != name {
				t.Errorf("Name = %q, want %q", p.Name, name)
			}
			if p.Description == "" {
				t.Error("Description is empty")
			}
			if len(p.Body) < 500 {
				t.Errorf("Body too short (%d bytes) — patterns must be real starter briefs, not stubs", len(p.Body))
			}
		})
	}
}

// TestAllPatternsParseAsBrief is the most important property test:
// every pattern must parse cleanly through brief.ParseString so an
// operator can pipe `bosun new-brief --pattern X > plan.md && bosun
// init --brief plan.md` without hitting a parser error on a heading
// the regex can't handle.
func TestAllPatternsParseAsBrief(t *testing.T) {
	patterns, err := Patterns()
	if err != nil {
		t.Fatalf("Patterns(): %v", err)
	}
	if len(patterns) != 4 {
		t.Fatalf("len(Patterns()) = %d, want 4", len(patterns))
	}
	for _, p := range patterns {
		t.Run(p.Name, func(t *testing.T) {
			briefs, err := brief.ParseString(p.Body)
			if err != nil {
				t.Fatalf("brief.ParseString(%q body): %v", p.Name, err)
			}
			if len(briefs) == 0 {
				t.Fatalf("%q pattern produced 0 briefs — must have at least one ## session-N heading", p.Name)
			}
		})
	}
}

// TestAllPatternsContainPlaceholders verifies the body has at least
// one {{...}} marker — otherwise the pattern isn't actually a
// "fill-and-go" template.
func TestAllPatternsContainPlaceholders(t *testing.T) {
	patterns, err := Patterns()
	if err != nil {
		t.Fatalf("Patterns(): %v", err)
	}
	for _, p := range patterns {
		t.Run(p.Name, func(t *testing.T) {
			if !strings.Contains(p.Body, "{{") || !strings.Contains(p.Body, "}}") {
				t.Errorf("%q pattern has no {{placeholder}} markers — operators have nothing to fill in", p.Name)
			}
		})
	}
}

// TestRecipePattern_HasSharedInterfaceStep verifies the recipe template
// preserves the load-bearing "write the shared interface FIRST" step
// from docs/brief-recipe-template.md. The whole point of the recipe
// pattern is that the parent commits the shared type BEFORE spawning so
// subs branch from a base that has it available — without this step the
// three subs each invent their own incompatible HealthResult-like type
// and the parent's integration becomes reconciliation work.
func TestRecipePattern_HasSharedInterfaceStep(t *testing.T) {
	p, err := Get("recipe")
	if err != nil {
		t.Fatalf("Get(recipe): %v", err)
	}
	// The step must be FIRST in the recipe, framed as a hard
	// prerequisite. Looking for the literal marker the operator
	// would expect from the template doc.
	body := p.Body
	if !strings.Contains(body, "Shared interface") {
		t.Error("recipe pattern missing the 'Shared interface' contract block")
	}
	if !strings.Contains(body, "shared interface file FIRST") &&
		!strings.Contains(body, "Write the shared interface FIRST") {
		t.Error("recipe pattern missing the 'write shared interface FIRST' recipe step")
	}
	if !strings.Contains(body, "BEFORE spawning") {
		t.Error("recipe pattern missing the 'commit before spawning' framing — that's the load-bearing constraint")
	}
}

// TestMultiLanePatternsHaveMultipleSessions checks that review/audit/
// cleanup actually fan out into multiple ## session-N headings. The
// recipe pattern is intentionally single-heading (sub-sessions spawn
// via MCP, not bosun init).
func TestMultiLanePatternsHaveMultipleSessions(t *testing.T) {
	for _, name := range []string{"review", "audit", "cleanup"} {
		t.Run(name, func(t *testing.T) {
			p, err := Get(name)
			if err != nil {
				t.Fatalf("Get(%q): %v", name, err)
			}
			briefs, err := brief.ParseString(p.Body)
			if err != nil {
				t.Fatalf("brief.ParseString: %v", err)
			}
			if len(briefs) < 2 {
				t.Errorf("%q pattern produced %d briefs, want ≥2 — multi-lane patterns must fan out", name, len(briefs))
			}
		})
	}
}

func TestRecipePatternIsSingleSession(t *testing.T) {
	p, err := Get("recipe")
	if err != nil {
		t.Fatalf("Get(recipe): %v", err)
	}
	briefs, err := brief.ParseString(p.Body)
	if err != nil {
		t.Fatalf("brief.ParseString: %v", err)
	}
	// The recipe parses as exactly one session-1 brief; the
	// sub-session headings (## session-1.{{lane-1-label}}) are
	// dot-prefixed and don't match the heading regex, so they fall
	// inside session-1's body as informational sub-headings.
	if len(briefs) != 1 {
		t.Errorf("recipe pattern produced %d briefs, want 1 (parent only; subs spawn via MCP)", len(briefs))
	}
	if len(briefs) > 0 && briefs[0].Label != "session-1" {
		t.Errorf("recipe pattern first brief label = %q, want session-1", briefs[0].Label)
	}
}
