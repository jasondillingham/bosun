package suggest

import (
	"errors"
	"strings"
	"testing"
)

// goodProposal returns a minimal valid proposal — used as the base for
// negative tests that flip one field at a time.
func goodProposal() LaneProposal {
	return LaneProposal{
		Version: "v1",
		Sessions: []Lane{
			{
				Label:      "session-1",
				Scope:      "foundation",
				OwnedFiles: []string{"internal/auth/**"},
				DependsOn:  nil,
			},
			{
				Label:      "session-2",
				Scope:      "storage",
				OwnedFiles: []string{"internal/storage/**"},
				DependsOn:  []string{"session-1"},
			},
			{
				Label:      "session-3",
				Scope:      "frontend",
				OwnedFiles: []string{"web/**"},
				DependsOn:  []string{"session-1"},
			},
		},
	}
}

func TestValidate_Happy(t *testing.T) {
	p := goodProposal()
	warnings, err := Validate(p, 3)
	if err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestValidate_SchemaErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*LaneProposal)
		wantSub string
	}{
		{
			name:    "no sessions",
			mutate:  func(p *LaneProposal) { p.Sessions = nil },
			wantSub: "no sessions",
		},
		{
			name:    "wrong count",
			mutate:  func(p *LaneProposal) { p.Sessions = p.Sessions[:2] },
			wantSub: "want 3",
		},
		{
			name:    "empty label",
			mutate:  func(p *LaneProposal) { p.Sessions[1].Label = "" },
			wantSub: "empty label",
		},
		{
			name:    "whitespace label",
			mutate:  func(p *LaneProposal) { p.Sessions[1].Label = "   " },
			wantSub: "empty label",
		},
		{
			name:    "empty scope",
			mutate:  func(p *LaneProposal) { p.Sessions[1].Scope = "" },
			wantSub: "empty scope",
		},
		{
			name:    "whitespace scope",
			mutate:  func(p *LaneProposal) { p.Sessions[1].Scope = "\t\n" },
			wantSub: "empty scope",
		},
		{
			name:    "duplicate label",
			mutate:  func(p *LaneProposal) { p.Sessions[2].Label = p.Sessions[1].Label },
			wantSub: "duplicate session label",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := goodProposal()
			tt.mutate(&p)
			_, err := Validate(p, 3)
			if err == nil {
				t.Fatalf("Validate = nil, want error containing %q", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Validate = %q, want substring %q", err.Error(), tt.wantSub)
			}
		})
	}
}

func TestValidate_RequestedCountZeroSkipsCheck(t *testing.T) {
	p := goodProposal()
	p.Sessions = p.Sessions[:2]
	if _, err := Validate(p, 0); err != nil {
		t.Fatalf("Validate(_, 0) = %v, want nil", err)
	}
}

func TestValidate_LabelCharset(t *testing.T) {
	tests := []struct {
		label   string
		wantErr bool
	}{
		// Valid labels.
		{"session-1", false},
		{"auth", false},
		{"auth-handler", false},
		{"a", false},
		{"a1", false},
		{"a-b-c", false},
		{"session-42", false},

		// Invalid labels — same rules session.ValidateLabel enforces.
		{"", true},
		{"1session", true},      // starts with digit
		{"session-", true},      // trailing dash
		{"-session", true},      // leading dash
		{"Auth", true},          // uppercase
		{"auth--handler", true}, // consecutive dashes
		{"session_1", true},     // underscore
		{"session.1", true},     // dot
		{"session 1", true},     // space
		{"session/1", true},     // slash
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			p := goodProposal()
			p.Sessions[0].Label = tt.label
			// Adjust depends_on so the swap doesn't dangle.
			for i := range p.Sessions {
				for j, d := range p.Sessions[i].DependsOn {
					if d == "session-1" {
						p.Sessions[i].DependsOn[j] = tt.label
					}
				}
			}
			_, err := Validate(p, 3)
			gotErr := err != nil
			if gotErr != tt.wantErr {
				t.Errorf("Validate label=%q err=%v, wantErr=%v", tt.label, err, tt.wantErr)
			}
			// The empty-label case is caught by the schema check first
			// (which is fine — it's still rejected). Other invalid
			// labels should produce an error mentioning the label or
			// the charset rule.
			if tt.wantErr && err != nil && tt.label != "" {
				if !strings.Contains(err.Error(), "label") {
					t.Errorf("Validate err = %q, want substring %q", err.Error(), "label")
				}
			}
		})
	}
}

func TestValidate_FileOverlap(t *testing.T) {
	tests := []struct {
		name        string
		a, b        []string
		wantOverlap bool
	}{
		{"exact match", []string{"a.go"}, []string{"a.go"}, true},
		{"disjoint files", []string{"a.go"}, []string{"b.go"}, false},
		{"directory contains file", []string{"internal/auth"}, []string{"internal/auth/handler.go"}, true},
		{"different directories", []string{"internal/auth/**"}, []string{"internal/storage/**"}, false},
		{"glob contains concrete", []string{"internal/auth/**"}, []string{"internal/auth/handler.go"}, true},
		{"concrete inside dir glob", []string{"internal/auth/handler.go"}, []string{"internal/auth/**"}, true},
		{"bare dir vs glob covering it", []string{"internal/auth"}, []string{"internal/auth/**"}, true},
		{"glob suffix mismatch", []string{"**/*.go"}, []string{"**/*.md"}, false},
		{"glob suffix match", []string{"**/*.go"}, []string{"**/*_test.go"}, true},
		{"glob anywhere matches dir", []string{"**/*.go"}, []string{"internal/auth/handler.go"}, true},
		{"star segment", []string{"cmd/*"}, []string{"cmd/bosun"}, true},
		{"star segment disjoint", []string{"cmd/*"}, []string{"web/bosun"}, false},
		{"nested glob overlap", []string{"internal/**/*.go"}, []string{"internal/auth/**"}, true},
		{"sibling globs no overlap", []string{"internal/auth/**"}, []string{"internal/storage/handler.go"}, false},
		{"trailing slash normalize", []string{"internal/auth/"}, []string{"internal/auth"}, true},
		{"backslash normalize", []string{`internal\auth\a.go`}, []string{"internal/auth/a.go"}, true},
		{"leading dot-slash", []string{"./a.go"}, []string{"a.go"}, true},
		{"empty pattern ignored", []string{""}, []string{"a.go"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := patternsOverlap(tt.a[0], tt.b[0])
			if got != tt.wantOverlap {
				t.Errorf("patternsOverlap(%q, %q) = %v, want %v",
					tt.a[0], tt.b[0], got, tt.wantOverlap)
			}
			// Cross-check symmetry — overlap is a relation, not a direction.
			rev := patternsOverlap(tt.b[0], tt.a[0])
			if rev != tt.wantOverlap {
				t.Errorf("patternsOverlap(%q, %q) = %v, want %v (asymmetric)",
					tt.b[0], tt.a[0], rev, tt.wantOverlap)
			}
		})
	}
}

func TestValidate_OverlapErrorSurfaceArea(t *testing.T) {
	p := goodProposal()
	// Force an overlap: lane 2 also claims something in internal/auth.
	p.Sessions[1].OwnedFiles = []string{"internal/auth/storage.go"}
	_, err := Validate(p, 3)
	if err == nil {
		t.Fatal("Validate = nil, want OverlapError")
	}
	var oe *OverlapError
	if !errors.As(err, &oe) {
		t.Fatalf("err type = %T, want *OverlapError", err)
	}
	// Either lane order is acceptable — the validator iterates i<j so
	// session-1 should be LaneA. Check both labels are present.
	gotLabels := []string{oe.LaneA, oe.LaneB}
	wantLabels := map[string]bool{"session-1": false, "session-2": false}
	for _, l := range gotLabels {
		if _, ok := wantLabels[l]; !ok {
			t.Errorf("OverlapError lane %q not in proposal", l)
		}
		wantLabels[l] = true
	}
	for l, found := range wantLabels {
		if !found {
			t.Errorf("OverlapError missing lane %q", l)
		}
	}
	if oe.PatternA == "" || oe.PatternB == "" {
		t.Errorf("OverlapError patterns empty: %+v", oe)
	}
}

func TestValidate_CycleDetection(t *testing.T) {
	tests := []struct {
		name    string
		deps    map[string][]string
		wantErr bool
	}{
		{
			name: "no cycle / linear chain",
			deps: map[string][]string{
				"session-1": nil,
				"session-2": {"session-1"},
				"session-3": {"session-2"},
			},
			wantErr: false,
		},
		{
			name: "no cycle / diamond",
			deps: map[string][]string{
				"session-1": nil,
				"session-2": {"session-1"},
				"session-3": {"session-1"},
				"session-4": {"session-2", "session-3"},
			},
			wantErr: false,
		},
		{
			name: "two-node cycle",
			deps: map[string][]string{
				"session-1": {"session-2"},
				"session-2": {"session-1"},
				"session-3": nil,
			},
			wantErr: true,
		},
		{
			name: "three-node cycle",
			deps: map[string][]string{
				"session-1": {"session-2"},
				"session-2": {"session-3"},
				"session-3": {"session-1"},
			},
			wantErr: true,
		},
		{
			name: "self-dependency",
			deps: map[string][]string{
				"session-1": {"session-1"},
				"session-2": nil,
				"session-3": nil,
			},
			wantErr: true,
		},
		{
			name: "back-edge deep in graph",
			deps: map[string][]string{
				"session-1": nil,
				"session-2": {"session-1"},
				"session-3": {"session-2", "session-4"},
				"session-4": {"session-3"},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := LaneProposal{Version: "v1"}
			// Stable label order in p.Sessions doesn't matter for cycle
			// detection but does for the schema "session-N" iteration —
			// build sessions in the order session-1, session-2, …
			labels := make([]string, 0, len(tt.deps))
			for l := range tt.deps {
				labels = append(labels, l)
			}
			// Deterministic order.
			for i := 1; i <= len(labels); i++ {
				lbl := labelFor(i)
				if _, ok := tt.deps[lbl]; !ok {
					continue
				}
				p.Sessions = append(p.Sessions, Lane{
					Label:      lbl,
					Scope:      "test",
					OwnedFiles: []string{filenameFor(i)},
					DependsOn:  tt.deps[lbl],
				})
			}
			_, err := Validate(p, len(p.Sessions))
			gotErr := err != nil
			if gotErr != tt.wantErr {
				t.Errorf("Validate err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr && err != nil {
				var ce *CycleError
				if !errors.As(err, &ce) {
					t.Errorf("err type = %T, want *CycleError", err)
				} else if len(ce.Cycle) < 2 {
					t.Errorf("CycleError.Cycle = %v, want at least 2 entries", ce.Cycle)
				}
			}
		})
	}
}

func TestValidate_UnknownDependency(t *testing.T) {
	p := goodProposal()
	p.Sessions[1].DependsOn = []string{"session-99"}
	_, err := Validate(p, 3)
	if err == nil {
		t.Fatal("Validate = nil, want error")
	}
	if !strings.Contains(err.Error(), "unknown session") {
		t.Errorf("Validate err = %q, want substring %q", err.Error(), "unknown session")
	}
}

func TestValidate_AvoidListWarnings(t *testing.T) {
	tests := []struct {
		name         string
		avoidFor1    []string
		wantWarnSubs []string
	}{
		{
			name:         "useful avoid (no warning)",
			avoidFor1:    []string{"internal/storage/**"},
			wantWarnSubs: nil,
		},
		{
			name:         "avoid file no one owns",
			avoidFor1:    []string{"docs/legacy.md"},
			wantWarnSubs: []string{"avoids", "docs/legacy.md"},
		},
		{
			name:         "mixed useful + noise",
			avoidFor1:    []string{"internal/storage/**", "tmp/unrelated.txt"},
			wantWarnSubs: []string{"tmp/unrelated.txt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := goodProposal()
			p.Sessions[0].AvoidFiles = tt.avoidFor1
			warnings, err := Validate(p, 3)
			if err != nil {
				t.Fatalf("Validate = %v", err)
			}
			if len(tt.wantWarnSubs) == 0 {
				if len(warnings) != 0 {
					t.Errorf("warnings = %v, want none", warnings)
				}
				return
			}
			if len(warnings) == 0 {
				t.Fatalf("warnings empty, want substrings %v", tt.wantWarnSubs)
			}
			joined := strings.Join(warnings, "\n")
			for _, sub := range tt.wantWarnSubs {
				if !strings.Contains(joined, sub) {
					t.Errorf("warnings = %v, missing substring %q", warnings, sub)
				}
			}
		})
	}
}

func TestValidate_AvoidListIgnoresSelfOwned(t *testing.T) {
	// A lane avoiding files it itself owns is also nonsense, but the
	// brief restricts the warning to "no OTHER lane owns it." Make sure
	// we don't false-positive when a lane's own OwnedFiles cover the
	// avoid pattern.
	p := goodProposal()
	p.Sessions[0].AvoidFiles = []string{"internal/auth/**"}
	warnings, err := Validate(p, 3)
	if err != nil {
		t.Fatalf("Validate = %v", err)
	}
	// Should warn — no OTHER lane owns internal/auth/**.
	if len(warnings) == 0 {
		t.Fatal("warnings empty, expected one (self-owned doesn't count)")
	}
}

func TestGlobToRegex(t *testing.T) {
	tests := []struct {
		pattern string
		match   string
		want    bool
	}{
		{"a.go", "a.go", true},
		{"a.go", "ab.go", false},
		{"*.go", "main.go", true},
		{"*.go", "sub/main.go", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/auth/main.go", true},
		{"**/*.go", "internal/auth/main.md", false},
		{"internal/auth/**", "internal/auth/h.go", true},
		{"internal/auth/**", "internal/auth/sub/h.go", true},
		{"internal/auth/**", "internal/auth", false},
		{"internal/auth/**", "internal/storage/h.go", false},
		{"?.go", "a.go", true},
		{"?.go", "ab.go", false},
		{"a[bc].go", "ab.go", true},
		{"a[bc].go", "ad.go", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"/"+tt.match, func(t *testing.T) {
			got := globMatch(tt.pattern, tt.match)
			if got != tt.want {
				t.Errorf("globMatch(%q, %q) = %v, want %v",
					tt.pattern, tt.match, got, tt.want)
			}
		})
	}
}

func TestNormalizePat(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"  internal/auth  ", "internal/auth"},
		{`internal\auth\a.go`, "internal/auth/a.go"},
		{"./a.go", "a.go"},
		{"a.go", "a.go"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := normalizePat(tt.in); got != tt.want {
				t.Errorf("normalizePat(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// labelFor and filenameFor produce deterministic stand-ins for tests
// that synthesize proposals from a depends-on map.
func labelFor(i int) string {
	return "session-" + itoa(i)
}

func filenameFor(i int) string {
	return "f" + itoa(i) + ".go"
}

func itoa(i int) string {
	// Tiny stdlib-free int-to-decimal so the test file has no extra
	// imports beyond what assertions need.
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
