package suggest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// validProposalJSON returns a LaneProposal JSON string conforming to the
// v1 schema, with `count` sessions and the given goal. Used by stub
// servers so each test can dial in the response it needs.
func validProposalJSON(goal string, count int) string {
	p := LaneProposal{Version: "v1", Goal: goal, Sessions: make([]Lane, 0, count)}
	for i := 1; i <= count; i++ {
		p.Sessions = append(p.Sessions, Lane{
			Label:      claudeLabelFor(i),
			Scope:      "stub scope",
			OwnedFiles: []string{"internal/stub/" + claudeLabelFor(i) + "/**"},
			AvoidFiles: []string{},
			DependsOn:  []string{},
			Rationale:  "stub rationale",
			WorkToDo:   []string{"do the thing"},
			Notes:      "",
		})
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func claudeLabelFor(i int) string {
	switch i {
	case 1:
		return "session-1"
	case 2:
		return "session-2"
	case 3:
		return "session-3"
	case 4:
		return "session-4"
	case 5:
		return "session-5"
	case 6:
		return "session-6"
	}
	// Fall back to a sprintf-free version to keep imports minimal in tests.
	return "session-x"
}

// anthropicResponseEnvelope wraps a text payload in the API response
// shape ClaudeProposer expects to decode.
func anthropicResponseEnvelope(text string) string {
	envelope := map[string]any{
		"id":   "msg_test",
		"type": "message",
		"role": "assistant",
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
		"stop_reason": "end_turn",
	}
	b, _ := json.Marshal(envelope)
	return string(b)
}

// newStubServer spins up an httptest.Server returning each entry in
// `replies` in order, asserting along the way that bosun sent the
// expected headers + JSON shape.
func newStubServer(t *testing.T, replies []string) (*httptest.Server, *atomic.Int32, *[]string) {
	t.Helper()
	calls := &atomic.Int32{}
	var captured []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := int(calls.Add(1)) - 1
		if r.Header.Get("x-api-key") == "" {
			t.Errorf("missing x-api-key header")
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("anthropic-version = %q, want 2023-06-01", r.Header.Get("anthropic-version"))
		}
		if r.Header.Get("content-type") != "application/json" {
			t.Errorf("content-type = %q, want application/json", r.Header.Get("content-type"))
		}
		body, _ := io.ReadAll(r.Body)
		captured = append(captured, string(body))
		if idx >= len(replies) {
			t.Errorf("server received call %d but only %d replies queued", idx+1, len(replies))
			http.Error(w, "too many calls", http.StatusInternalServerError)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, replies[idx])
	}))
	t.Cleanup(srv.Close)
	return srv, calls, &captured
}

func newTestProposer(t *testing.T, endpoint string) *ClaudeProposer {
	t.Helper()
	p, err := NewClaudeProposer(ClaudeProposerOptions{
		APIKey:    "test-key",
		Endpoint:  endpoint,
		MaxTokens: 1024,
		HTTPClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("NewClaudeProposer: %v", err)
	}
	return p
}

func TestPropose_HappyPath(t *testing.T) {
	srv, calls, captured := newStubServer(t, []string{
		anthropicResponseEnvelope(validProposalJSON("rewrite", 3)),
	})
	p := newTestProposer(t, srv.URL)

	proposal, err := p.Propose(context.Background(), "rewrite", RepoIntel{Root: "/tmp"}, 3)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
	if len(proposal.Sessions) != 3 {
		t.Errorf("got %d sessions, want 3", len(proposal.Sessions))
	}
	if proposal.Goal != "rewrite" {
		t.Errorf("goal = %q, want rewrite", proposal.Goal)
	}
	// Request body should embed the goal + session count.
	if !strings.Contains((*captured)[0], "Goal: rewrite") {
		t.Errorf("request body missing goal: %s", (*captured)[0])
	}
	if !strings.Contains((*captured)[0], "Sessions requested: 3") {
		t.Errorf("request body missing session count: %s", (*captured)[0])
	}
}

func TestPropose_ExtractsJSONFromProse(t *testing.T) {
	// Model wraps the JSON in prose; extractor should still find it.
	body := "Here's the plan you asked for:\n\n" + validProposalJSON("g", 2) + "\n\nLet me know if you want changes."
	srv, _, _ := newStubServer(t, []string{anthropicResponseEnvelope(body)})
	p := newTestProposer(t, srv.URL)

	proposal, err := p.Propose(context.Background(), "g", RepoIntel{}, 2)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(proposal.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(proposal.Sessions))
	}
}

func TestPropose_RetriesOnceOnBadOutput(t *testing.T) {
	// First reply is malformed (truncated JSON); second is valid.
	srv, calls, captured := newStubServer(t, []string{
		anthropicResponseEnvelope(`{"version":"v1","goal":"g","sessions":[`),
		anthropicResponseEnvelope(validProposalJSON("g", 2)),
	})
	p := newTestProposer(t, srv.URL)

	proposal, err := p.Propose(context.Background(), "g", RepoIntel{}, 2)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (retry)", calls.Load())
	}
	if len(proposal.Sessions) != 2 {
		t.Errorf("sessions = %d, want 2", len(proposal.Sessions))
	}
	// Retry body should include both the assistant's bad output and the
	// validation-error follow-up so the model has feedback to act on.
	if len(*captured) < 2 {
		t.Fatalf("captured = %d bodies, want 2", len(*captured))
	}
	if !strings.Contains((*captured)[1], `"role":"assistant"`) {
		t.Errorf("retry body missing assistant turn: %s", (*captured)[1])
	}
	if !strings.Contains((*captured)[1], "failed validation") {
		t.Errorf("retry body missing validation-error follow-up: %s", (*captured)[1])
	}
}

func TestPropose_FailsAfterRetry(t *testing.T) {
	// All replies fail validation (session count mismatch). Phase 5
	// #62 widened the loop to maxProposeAttempts (3) — the test
	// queues three identical bad bodies so the loop exhausts.
	bad := validProposalJSON("g", 1) // requesting 2, returning 1
	srv, calls, _ := newStubServer(t, []string{
		anthropicResponseEnvelope(bad),
		anthropicResponseEnvelope(bad),
		anthropicResponseEnvelope(bad),
	})
	p := newTestProposer(t, srv.URL)

	_, err := p.Propose(context.Background(), "g", RepoIntel{}, 2)
	if err == nil {
		t.Fatal("expected ProposalError after all attempts failed")
	}
	if calls.Load() != int32(maxProposeAttempts) {
		t.Errorf("calls = %d, want %d (maxProposeAttempts)", calls.Load(), maxProposeAttempts)
	}
	var pe *ProposalError
	if !errors.As(err, &pe) {
		t.Fatalf("err type = %T, want *ProposalError", err)
	}
	if pe.FirstError == nil || pe.RetryError == nil {
		t.Errorf("ProposalError should carry both first + last errors: %+v", pe)
	}
}

func TestPropose_RejectsUnknownFields(t *testing.T) {
	// Extra "extra_field" in a session — DisallowUnknownFields catches it.
	body := `{
  "version": "v1",
  "goal": "g",
  "sessions": [
    {"label":"session-1","scope":"s","owned_files":["a/**"],"avoid_files":[],
     "depends_on":[],"rationale":"r","work_to_do":["x"],"notes":"","extra_field":"nope"}
  ]
}`
	// Queue maxProposeAttempts identical bad bodies so the loop
	// exhausts. Phase 5 #62 widened the retry budget; this test
	// proves the unknown-field rejection still surfaces after the
	// model fails to self-correct.
	replies := make([]string, maxProposeAttempts)
	for i := range replies {
		replies[i] = anthropicResponseEnvelope(body)
	}
	srv, _, _ := newStubServer(t, replies)
	p := newTestProposer(t, srv.URL)
	_, err := p.Propose(context.Background(), "g", RepoIntel{}, 1)
	if err == nil {
		t.Fatal("expected error for unknown fields")
	}
}

// overlappingProposalJSON returns a schema-valid proposal where two
// lanes claim overlapping owned_files patterns. Used to exercise the
// Phase 5 #62 overlap-refinement path — the proposal must pass
// parseAndValidate (schema) but fail Validate (lane-level invariants).
func overlappingProposalJSON(goal string) string {
	p := LaneProposal{Version: "v1", Goal: goal, Sessions: []Lane{
		{
			Label: "session-1", Scope: "auth",
			OwnedFiles: []string{"internal/auth/**"}, AvoidFiles: []string{},
			DependsOn: []string{}, Rationale: "r", WorkToDo: []string{"x"},
		},
		{
			Label: "session-2", Scope: "auth (conflict)",
			OwnedFiles: []string{"internal/auth/**"}, AvoidFiles: []string{},
			DependsOn: []string{}, Rationale: "r", WorkToDo: []string{"y"},
		},
	}}
	b, _ := json.Marshal(p)
	return string(b)
}

// TestPropose_RefinesOnOverlap exercises the Phase 5 #62 path: the
// first proposal is schema-valid but lanes overlap, so Propose
// refines once and the second proposal is fully clean. Asserts on
// both the call count and the body of the refinement turn — the
// model gets the overlap detail spelled out so it can edit
// surgically.
func TestPropose_RefinesOnOverlap(t *testing.T) {
	srv, calls, captured := newStubServer(t, []string{
		anthropicResponseEnvelope(overlappingProposalJSON("g")),
		anthropicResponseEnvelope(validProposalJSON("g", 2)),
	})
	p := newTestProposer(t, srv.URL)

	proposal, err := p.Propose(context.Background(), "g", RepoIntel{}, 2)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (one overlap refinement)", calls.Load())
	}
	if len(proposal.Sessions) != 2 {
		t.Errorf("sessions = %d, want 2", len(proposal.Sessions))
	}
	// The refinement body should carry the assistant's first reply
	// AND a refinement turn explaining the overlap.
	if len(*captured) < 2 {
		t.Fatalf("captured %d bodies, want 2", len(*captured))
	}
	refineBody := (*captured)[1]
	if !strings.Contains(refineBody, "schema-valid") {
		t.Errorf("refinement body should describe overlap as schema-valid: %s", refineBody)
	}
	if !strings.Contains(refineBody, "owned_files") {
		t.Errorf("refinement body should mention owned_files: %s", refineBody)
	}
}

// TestPropose_GivesUpAfterOverlapRefinementBudget: if the model
// keeps producing overlapping proposals, Propose exhausts the
// maxProposeAttempts budget and surfaces the overlap as a
// ProposalError. We don't loop forever — three identical-shape
// failures is plenty of signal that the model is stuck.
func TestPropose_GivesUpAfterOverlapRefinementBudget(t *testing.T) {
	replies := make([]string, maxProposeAttempts)
	for i := range replies {
		replies[i] = anthropicResponseEnvelope(overlappingProposalJSON("g"))
	}
	srv, calls, _ := newStubServer(t, replies)
	p := newTestProposer(t, srv.URL)

	_, err := p.Propose(context.Background(), "g", RepoIntel{}, 2)
	if err == nil {
		t.Fatal("expected ProposalError after exhausting refinement budget")
	}
	if calls.Load() != int32(maxProposeAttempts) {
		t.Errorf("calls = %d, want %d", calls.Load(), maxProposeAttempts)
	}
	var pe *ProposalError
	if !errors.As(err, &pe) {
		t.Fatalf("err type = %T, want *ProposalError", err)
	}
	// Both first and last errors should be overlap errors (lane-level
	// invariant failures), not schema errors.
	var overlap *OverlapError
	if !errors.As(pe.RetryError, &overlap) {
		t.Errorf("last error should be *OverlapError, got %T: %v", pe.RetryError, pe.RetryError)
	}
}

func TestPropose_PropagatesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	t.Cleanup(srv.Close)

	p := newTestProposer(t, srv.URL)
	_, err := p.Propose(context.Background(), "g", RepoIntel{}, 2)
	if err == nil {
		t.Fatal("expected API error")
	}
	if !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Errorf("error %q should surface anthropic error message", err.Error())
	}
}

func TestPropose_EmptyGoalRejected(t *testing.T) {
	p := &ClaudeProposer{APIKey: "k", Endpoint: "http://x", HTTPClient: http.DefaultClient}
	if _, err := p.Propose(context.Background(), "   ", RepoIntel{}, 2); err == nil {
		t.Fatal("expected error for empty goal")
	}
}

func TestPropose_InvalidSessionCount(t *testing.T) {
	p := &ClaudeProposer{APIKey: "k", Endpoint: "http://x", HTTPClient: http.DefaultClient}
	if _, err := p.Propose(context.Background(), "g", RepoIntel{}, 0); err == nil {
		t.Fatal("expected error for zero session count")
	}
}

func TestNewClaudeProposer_DefaultsAndKeyResolution(t *testing.T) {
	t.Setenv("BOSUN_TEST_KEY", "from-env")
	p, err := NewClaudeProposer(ClaudeProposerOptions{APIKeyEnv: "BOSUN_TEST_KEY"})
	if err != nil {
		t.Fatalf("NewClaudeProposer: %v", err)
	}
	if p.APIKey != "from-env" {
		t.Errorf("APIKey = %q, want from-env", p.APIKey)
	}
	if p.Model != defaultClaudeModel {
		t.Errorf("Model = %q, want default", p.Model)
	}
	if p.MaxTokens != defaultMaxTokens {
		t.Errorf("MaxTokens = %d, want default", p.MaxTokens)
	}
	if p.Endpoint != defaultAnthropicEndpoint {
		t.Errorf("Endpoint = %q, want default", p.Endpoint)
	}
}

func TestNewClaudeProposer_MissingKey(t *testing.T) {
	// Use a fake env var name that's guaranteed empty.
	t.Setenv("BOSUN_TEST_KEY_MISSING", "")
	_, err := NewClaudeProposer(ClaudeProposerOptions{APIKeyEnv: "BOSUN_TEST_KEY_MISSING"})
	if err == nil {
		t.Fatal("expected error when no API key available")
	}
	if !strings.Contains(err.Error(), "BOSUN_TEST_KEY_MISSING") {
		t.Errorf("error %q should name the env var", err.Error())
	}
}

func TestNewClaudeProposer_AnthropicAPIURLOverride(t *testing.T) {
	t.Setenv("ANTHROPIC_API_URL", "http://stub.local/v1/messages")
	p, err := NewClaudeProposer(ClaudeProposerOptions{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewClaudeProposer: %v", err)
	}
	if p.Endpoint != "http://stub.local/v1/messages" {
		t.Errorf("Endpoint = %q, want override from env", p.Endpoint)
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		hasErr bool
	}{
		{"plain", `{"a":1}`, `{"a":1}`, false},
		{"prose wrap", "Here you go: {\"a\":1} cheers", `{"a":1}`, false},
		{"nested objects", `{"a":{"b":2},"c":3}`, `{"a":{"b":2},"c":3}`, false},
		{"braces in strings", `{"a":"}{","b":2}`, `{"a":"}{","b":2}`, false},
		{"escaped quotes in strings", `{"a":"\"}","b":2}`, `{"a":"\"}","b":2}`, false},
		{"empty", "", "", true},
		{"no object", "no json here", "", true},
		{"unbalanced", `{"a":1`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractJSON(tc.in)
			if (err != nil) != tc.hasErr {
				t.Fatalf("err = %v, hasErr = %v", err, tc.hasErr)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateSchema(t *testing.T) {
	good := LaneProposal{
		Version: "v1",
		Goal:    "g",
		Sessions: []Lane{
			{Label: "session-1", Scope: "s", OwnedFiles: []string{"a/**"}, WorkToDo: []string{"x"}, Rationale: "r"},
		},
	}
	if err := validateClaudeSchema(good, "g", 1); err != nil {
		t.Errorf("good: %v", err)
	}

	type tc struct {
		name string
		mut  func(*LaneProposal)
		n    int
	}
	cases := []tc{
		{"missing version", func(p *LaneProposal) { p.Version = "" }, 1},
		{"wrong version", func(p *LaneProposal) { p.Version = "v2" }, 1},
		{"missing goal", func(p *LaneProposal) { p.Goal = "" }, 1},
		{"session count mismatch", func(p *LaneProposal) {}, 2},
		{"empty label", func(p *LaneProposal) { p.Sessions[0].Label = "" }, 1},
		{"empty scope", func(p *LaneProposal) { p.Sessions[0].Scope = "" }, 1},
		{"empty owned_files", func(p *LaneProposal) { p.Sessions[0].OwnedFiles = nil }, 1},
		{"empty work_to_do", func(p *LaneProposal) { p.Sessions[0].WorkToDo = nil }, 1},
		{"empty rationale", func(p *LaneProposal) { p.Sessions[0].Rationale = "" }, 1},
		{"duplicate label", func(p *LaneProposal) {
			p.Sessions = append(p.Sessions, p.Sessions[0])
		}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Deep-copy the good proposal so mutations don't leak.
			cp := LaneProposal{
				Version:  good.Version,
				Goal:     good.Goal,
				Sessions: append([]Lane{}, good.Sessions...),
			}
			c.mut(&cp)
			if err := validateClaudeSchema(cp, "g", c.n); err == nil {
				t.Errorf("%s: expected error", c.name)
			}
		})
	}
}
