package suggest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Default Anthropic Messages API endpoint. Overridable per-instance for
// tests (httptest.Server) or per-process via the ANTHROPIC_API_URL env
// (used by the end-to-end scenarios in cmd/bosun — coordinated with
// session-6 of the v0.5 round-1 plan).
const defaultAnthropicEndpoint = "https://api.anthropic.com/v1/messages"

// anthropicVersion pins the API version. Bumping this is an opt-in;
// keep it stable until we verify response shapes against a new release.
const anthropicVersion = "2023-06-01"

// defaultClaudeModel mirrors config.DefaultSuggestModel — duplicated as
// a const so this package doesn't pull internal/config into a cycle.
const defaultClaudeModel = "claude-sonnet-4-6"

// defaultMaxTokens mirrors config.DefaultSuggestMaxTokens.
const defaultMaxTokens = 8000

// systemPrompt establishes the role and the lane-design rules every
// proposed plan must respect.
const systemPrompt = `You are bosun's brief-authoring assistant. Bosun runs N coding agents in parallel, each on its own git worktree, and your job is to propose N lanes of work that those agents can do simultaneously without colliding.

Lane-design rules — every proposal you return MUST obey:

1. Each lane owns files no other lane touches. Overlap on shared types or modules causes merge conflicts; the operator has seen this pain before and will reject overlapping proposals.
2. If foundational changes need to land before the others, put them in a lane labeled "session-1" and have downstream lanes declare it in their "depends_on" field. Bosun's dep-aware merge will order the merges accordingly.
3. Cycles in the dependency graph are forbidden — bosun will refuse them at merge time.
4. Test files belong to the same lane as the production code they cover. Co-locate, don't split.
5. Each lane's "owned_files" and "avoid_files" are glob patterns ("internal/auth/**", "cmd/bosun/cmd_login.go"). Be specific enough that another agent reading the brief can tell if they're encroaching.

Return JSON ONLY, conforming exactly to the schema given in the user prompt. Do not wrap it in prose, do not add fields not in the schema, do not omit required fields.`

// userPromptTemplate is filled with the goal, the requested session
// count, and the RepoIntel JSON. The few-shot examples block stays
// inline (it's trimmed from v0.4-plan.md sections) so the model has a
// concrete shape to mimic. Keep it small — these examples count
// against the input-token budget.
const userPromptTemplate = `Goal: %s

Sessions requested: %d

RepoIntel (compact snapshot of the repository — file shape, recent activity, languages, dependencies):

` + "```json\n%s\n```" + `

<examples>
Here are two trimmed-down sessions from a real bosun plan (v0.4) so you can match the structural format. These are exemplars — NOT what you should propose; your output must respond to the goal above.

Example session A (foundational, no dependencies):
{
  "label": "session-1",
  "scope": "Lifecycle hooks system: pre-init, post-init, post-done callsites",
  "owned_files": ["internal/hooks/**", "internal/config/config.go", "internal/config/config_test.go", "cmd/bosun/cmd_init.go", "cmd/bosun/cmd_done.go"],
  "avoid_files": ["cmd/bosun/cmd_merge.go", "cmd/bosun/cmd_cleanup.go", "internal/web/**"],
  "depends_on": [],
  "rationale": "Foundational scaffolding. Other lanes wire into the hooks system once it lands.",
  "work_to_do": [
    "New package internal/hooks/ with Hook{Event, Command, FailOpen, TimeoutSeconds} and Run().",
    "Add Hooks []hooks.Hook field to internal/config Config struct.",
    "Wire pre-init/post-init callsites in cmd_init.go; post-done in cmd_done.go.",
    "Tests for event matching, env injection, timeout enforcement."
  ],
  "notes": "Validate event names against the known set; unknown events are a config error."
}

Example session B (depends on session-1, small focused diff):
{
  "label": "session-2",
  "scope": "bosun merge --dry-run flag",
  "owned_files": ["cmd/bosun/cmd_merge.go", "cmd/bosun/scenarios_test.go"],
  "avoid_files": ["internal/hooks/**", "cmd/bosun/cmd_init.go"],
  "depends_on": ["session-1"],
  "rationale": "Single-file feature; runs after session-1 lands the hook callsites so the dry-run output matches a real merge.",
  "work_to_do": [
    "Add --dry-run flag in cmd_merge.go.",
    "Build the merge plan as today, but print 'would merge' lines instead of running git merge --squash.",
    "Honor --all and --no-squash in the dry-run path.",
    "Scenarios: clean two-session repo, DONE filtering, named-session arg."
  ],
  "notes": ""
}
</examples>

Required JSON response schema:

` + "```json\n" + `{
  "version": "v1",
  "goal": "<echo of the goal>",
  "sessions": [
    {
      "label": "session-1",
      "scope": "one-line summary of what this lane does",
      "owned_files": ["glob/pattern/**", "..."],
      "avoid_files": ["glob/pattern/**"],
      "depends_on": ["session-1"],
      "rationale": "1-2 sentences for the operator review",
      "work_to_do": ["bullet 1", "bullet 2"],
      "notes": "optional gotchas (empty string if none)"
    }
  ]
}` + "\n```" + `

Return EXACTLY %d sessions. Output JSON only, no prose around it.`

// retryFollowupTemplate is the user message used for the one retry
// attempt after a malformed first response. The validator error is
// echoed so the model can correct course.
const retryFollowupTemplate = `Your previous response failed validation: %s

Please return a corrected JSON object that conforms to the schema. Output JSON only.`

// anthropicRequest mirrors the Messages API request body. Only the
// fields bosun cares about are modeled here; extras would be silently
// ignored by the API.
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicResponse models the subset of the Messages API response we
// read. Content is an array of typed blocks; we concatenate text blocks
// and ignore the rest (e.g. tool_use blocks bosun doesn't request).
type anthropicResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
}

type anthropicErrorResponse struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ClaudeProposer is the production Proposer — talks to the real
// Anthropic Messages API. Construct via NewClaudeProposer; the zero
// value is not usable (no API key).
type ClaudeProposer struct {
	// APIKey is the bearer key sent in the `x-api-key` header. Required.
	APIKey string
	// Model is the Claude model ID. Defaults to claude-sonnet-4-6.
	Model string
	// MaxTokens caps the response length. Defaults to 8000.
	MaxTokens int
	// Endpoint is the Messages API URL. Defaults to api.anthropic.com,
	// overridable per-instance for tests or via ANTHROPIC_API_URL env.
	Endpoint string
	// HTTPClient is the transport. Defaults to a client with a 60s
	// timeout — tests inject httptest.Server's client.
	HTTPClient *http.Client
}

// ClaudeProposerOptions configures NewClaudeProposer. Any zero-valued
// field falls back to a documented default (or env override).
type ClaudeProposerOptions struct {
	APIKey    string
	Model     string
	MaxTokens int
	Endpoint  string
	// APIKeyEnv names the env var to read the API key from when APIKey
	// is empty. Defaults to ANTHROPIC_API_KEY.
	APIKeyEnv  string
	HTTPClient *http.Client
}

// NewClaudeProposer builds a ClaudeProposer with defaults filled in.
// Returns an error if the API key cannot be resolved (neither passed
// nor present in the named env var).
func NewClaudeProposer(opts ClaudeProposerOptions) (*ClaudeProposer, error) {
	keyEnv := opts.APIKeyEnv
	if keyEnv == "" {
		keyEnv = "ANTHROPIC_API_KEY"
	}
	key := opts.APIKey
	if key == "" {
		key = os.Getenv(keyEnv)
	}
	if key == "" {
		return nil, fmt.Errorf("bosun: no Claude API key — set %s or pass APIKey", keyEnv)
	}

	endpoint := opts.Endpoint
	if endpoint == "" {
		// ANTHROPIC_API_URL is the env-level override used by the
		// end-to-end scenarios (cmd/bosun/scenarios_test.go, session-6
		// of the v0.5 round). Production callers leave it unset.
		endpoint = os.Getenv("ANTHROPIC_API_URL")
	}
	if endpoint == "" {
		endpoint = defaultAnthropicEndpoint
	}

	model := opts.Model
	if model == "" {
		model = defaultClaudeModel
	}
	maxTok := opts.MaxTokens
	if maxTok <= 0 {
		maxTok = defaultMaxTokens
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}

	return &ClaudeProposer{
		APIKey:     key,
		Model:      model,
		MaxTokens:  maxTok,
		Endpoint:   endpoint,
		HTTPClient: client,
	}, nil
}

// Propose runs the goal + intel + N through Claude and returns a
// validated LaneProposal. On a malformed or schema-invalid first
// response, retries once with the validation error echoed back to the
// model. After two failed attempts returns a ProposalError.
func (c *ClaudeProposer) Propose(ctx context.Context, goal string, intel RepoIntel, n int) (LaneProposal, error) {
	if strings.TrimSpace(goal) == "" {
		return LaneProposal{}, errors.New("bosun: goal must not be empty")
	}
	if n < 1 {
		return LaneProposal{}, fmt.Errorf("bosun: session count must be ≥ 1, got %d", n)
	}

	intelJSON, err := json.MarshalIndent(intel, "", "  ")
	if err != nil {
		return LaneProposal{}, fmt.Errorf("bosun: marshal RepoIntel: %w", err)
	}

	userPrompt := fmt.Sprintf(userPromptTemplate, goal, n, string(intelJSON), n)

	// First attempt: system prompt + initial user message.
	messages := []anthropicMessage{{Role: "user", Content: userPrompt}}
	firstText, err := c.call(ctx, messages)
	if err != nil {
		return LaneProposal{}, err
	}

	proposal, firstValidationErr := parseAndValidate(firstText, goal, n)
	if firstValidationErr == nil {
		return proposal, nil
	}

	// Retry once. Carry the prior turn as context so the model can see
	// what it produced and the validation error it triggered.
	retryMessages := []anthropicMessage{
		{Role: "user", Content: userPrompt},
		{Role: "assistant", Content: firstText},
		{Role: "user", Content: fmt.Sprintf(retryFollowupTemplate, firstValidationErr.Error())},
	}
	retryText, err := c.call(ctx, retryMessages)
	if err != nil {
		return LaneProposal{}, err
	}
	proposal, retryValidationErr := parseAndValidate(retryText, goal, n)
	if retryValidationErr == nil {
		return proposal, nil
	}

	return LaneProposal{}, &ProposalError{
		FirstError: firstValidationErr,
		RetryError: retryValidationErr,
		Raw:        retryText,
	}
}

// call performs one Messages API round trip and returns the
// concatenated text content. Non-2xx responses become network errors
// carrying the Anthropic error message when one is present.
func (c *ClaudeProposer) call(ctx context.Context, messages []anthropicMessage) (string, error) {
	body, err := json.Marshal(anthropicRequest{
		Model:     c.Model,
		MaxTokens: c.MaxTokens,
		System:    systemPrompt,
		Messages:  messages,
	})
	if err != nil {
		return "", fmt.Errorf("bosun: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("bosun: build request: %w", err)
	}
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("bosun: anthropic request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("bosun: read anthropic response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Try to surface the structured error message; fall back to raw.
		var apiErr anthropicErrorResponse
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Error.Message != "" {
			return "", fmt.Errorf("bosun: anthropic %d: %s", resp.StatusCode, apiErr.Error.Message)
		}
		return "", fmt.Errorf("bosun: anthropic %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("bosun: decode anthropic response: %w (body: %s)", err, truncate(string(raw), 200))
	}

	var text strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() == 0 {
		return "", fmt.Errorf("bosun: anthropic response contained no text content (stop_reason=%q)", parsed.StopReason)
	}
	return text.String(), nil
}

// ProposalError is returned when both the initial response and the
// retry fail validation. Callers (CLI wiring) can present FirstError +
// RetryError to the operator with enough context to narrow the goal.
type ProposalError struct {
	FirstError error
	RetryError error
	// Raw is the unparsed retry text — useful for debugging.
	Raw string
}

func (e *ProposalError) Error() string {
	return fmt.Sprintf("bosun: claude proposer failed twice (first: %v; retry: %v)", e.FirstError, e.RetryError)
}

// Unwrap returns the retry error so errors.Is/As traverse to the most
// recent failure cause.
func (e *ProposalError) Unwrap() error { return e.RetryError }

// parseAndValidate pulls the JSON out of a potentially-prose-wrapped
// response, decodes into LaneProposal, and runs schema validation.
func parseAndValidate(text, goal string, n int) (LaneProposal, error) {
	jsonText, err := extractJSON(text)
	if err != nil {
		return LaneProposal{}, err
	}
	var proposal LaneProposal
	dec := json.NewDecoder(strings.NewReader(jsonText))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&proposal); err != nil {
		return LaneProposal{}, fmt.Errorf("decode JSON: %w", err)
	}
	if err := validateClaudeSchema(proposal, goal, n); err != nil {
		return LaneProposal{}, err
	}
	return proposal, nil
}

// extractJSON returns the largest balanced JSON object embedded in
// text. If the model wraps the JSON in prose ("Here's the plan: { ...
// }"), this walks the runes tracking brace depth and string-literal
// boundaries to find the outermost object.
func extractJSON(text string) (string, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", errors.New("empty response")
	}
	start := strings.Index(trimmed, "{")
	if start < 0 {
		return "", errors.New("no JSON object found in response")
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(trimmed); i++ {
		ch := trimmed[i]
		if escaped {
			escaped = false
			continue
		}
		if inString {
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return trimmed[start : i+1], nil
			}
		}
	}
	return "", errors.New("unbalanced JSON braces in response")
}

// validateSchema checks the proposal matches the v1 schema (every
// required field present, correct session count, no empty labels).
// Lane-level overlap + cycle detection is session-3's territory
// (internal/suggest/validate.go) and runs downstream of this; this
// function only enforces the API contract.
func validateClaudeSchema(p LaneProposal, goal string, n int) error {
	if p.Version == "" {
		return errors.New("missing required field: version")
	}
	if p.Version != "v1" {
		return fmt.Errorf("unsupported version %q, want v1", p.Version)
	}
	if p.Goal == "" {
		return errors.New("missing required field: goal")
	}
	if len(p.Sessions) != n {
		return fmt.Errorf("got %d sessions, want %d", len(p.Sessions), n)
	}
	seen := make(map[string]struct{}, n)
	for i, lane := range p.Sessions {
		if strings.TrimSpace(lane.Label) == "" {
			return fmt.Errorf("sessions[%d]: empty label", i)
		}
		if _, dup := seen[lane.Label]; dup {
			return fmt.Errorf("sessions[%d]: duplicate label %q", i, lane.Label)
		}
		seen[lane.Label] = struct{}{}
		if strings.TrimSpace(lane.Scope) == "" {
			return fmt.Errorf("sessions[%d] (%s): empty scope", i, lane.Label)
		}
		if len(lane.OwnedFiles) == 0 {
			return fmt.Errorf("sessions[%d] (%s): owned_files must not be empty", i, lane.Label)
		}
		if len(lane.WorkToDo) == 0 {
			return fmt.Errorf("sessions[%d] (%s): work_to_do must not be empty", i, lane.Label)
		}
		if strings.TrimSpace(lane.Rationale) == "" {
			return fmt.Errorf("sessions[%d] (%s): empty rationale", i, lane.Label)
		}
	}
	return nil
}

// truncate caps s at n bytes for embedding in error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
