// Package config loads bosun's optional config file at .bosun/config.json,
// returning a Config struct populated with defaults when the file is absent.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jasondillingham/bosun/internal/hooks"
)

const (
	DefaultBaseBranch       = "main"
	DefaultSessionPrefix    = "bosun"
	DefaultSuffixPattern    = "-bosun-{N}"
	DefaultSessionCount     = 4
	DefaultIsolateCache     = false
	DefaultLauncherStrategy = "auto"
	DefaultVerifyCmd        = "make check"
	// DefaultGitOpTimeoutSeconds is the per-operation timeout applied to
	// every `git` subprocess by internal/git.Client. Catches the
	// silent-init-hang case where `git worktree add` blocks indefinitely
	// on a closing fsync while the host is under APFS / Spotlight
	// pressure. Operators can extend or shorten via the
	// `git_op_timeout_seconds` config field.
	DefaultGitOpTimeoutSeconds = 30
	ConfigRelativePath         = ".bosun/config.json"

	// Defaults for the suggest assistant (see internal/suggest and
	// docs/v0.5-suggest-spec.md). Kept here so config and the suggest
	// package can both reference the same constants.
	DefaultSuggestModel     = "claude-sonnet-4-6"
	DefaultSuggestMaxTokens = 8000
	DefaultSuggestAPIKeyEnv = "ANTHROPIC_API_KEY" //nolint:gosec // G101: name of an env var holding a key, not the key itself

	// Defaults for the v0.9 agent_spawn capability. See
	// docs/v0.9-spawn-spec.md for the auth + quota model.
	DefaultAgentSpawnEnabled       = false
	DefaultAgentSpawnMaxConcurrent = 3
	DefaultAgentSpawnMaxDepth      = 1
	// MaxAgentSpawnDepthCeiling is the hard upper bound on spawn depth
	// regardless of what an operator configures. A misconfigured 100
	// gets clamped here so a runaway agent can't fork-bomb.
	MaxAgentSpawnDepthCeiling = 4

	// Defaults for the v1.0 agent_subtask capability. See
	// docs/v1.0-sub-task-spec.md for the auth + quota model. The brief
	// (#11 milestone 1+2) lands the registry-only surface — parent
	// agents register sub-tasks via bosun_subtask and run them
	// themselves; bosun's role is record + audit + display.
	DefaultAgentSubtaskEnabled       = false
	DefaultAgentSubtaskMaxConcurrent = 5

	// Defaults for the liveness gate. See LivenessGate on Config for
	// the trade-off between the two modes.
	LivenessGateAuto     = "auto"
	LivenessGateExternal = "external"
	DefaultLivenessGate  = LivenessGateAuto
)

// Config is the resolved bosun config for a repo.
type Config struct {
	BaseBranch            string `json:"base_branch"`
	SessionPrefix         string `json:"session_prefix"`
	WorktreeSuffixPattern string `json:"worktree_suffix_pattern"`
	DefaultSessionCount   int    `json:"default_session_count"`
	IsolateCacheDefault   bool   `json:"isolate_cache_default"`
	Launcher              string `json:"launcher"`
	// VerifyCmd is the command bosun's brief preamble tells the agent to run
	// before declaring `bosun done`. Default is `make check` (bosun's own
	// convention). Projects with different test workflows set this to e.g.
	// "make test" or "go test ./...".
	VerifyCmd string `json:"verify_cmd"`
	// GitOpTimeoutSeconds caps each `git` subprocess invocation made by
	// internal/git.Client. Zero / unset → DefaultGitOpTimeoutSeconds.
	GitOpTimeoutSeconds int `json:"git_op_timeout_seconds"`
	// Hooks are operator-defined shell commands run at lifecycle moments
	// (see internal/hooks for the runner and the v0.1 event set).
	Hooks []hooks.Hook `json:"hooks,omitempty"`
	// Suggest configures the v0.5 brief-authoring assistant. Read by
	// internal/suggest and cmd/bosun/cmd_suggest.go.
	Suggest SuggestConfig `json:"suggest"`
	// AgentSpawn gates the v0.9 bosun_spawn MCP tool. Off by default;
	// see docs/v0.9-spawn-spec.md for the auth + quota model.
	AgentSpawn AgentSpawnConfig `json:"agent_spawn"`
	// AgentSubtask gates the v1.0 bosun_subtask MCP tool. Off by
	// default; see docs/v1.0-sub-task-spec.md for the registry +
	// per-parent-quota model. Sub-tasks share the parent's worktree
	// (no fork, no branch, no merge), so the blast radius is far
	// smaller than spawn — but the registry still has to record
	// every call for the audit trail #14 set up.
	AgentSubtask AgentSubtaskConfig `json:"agent_subtask"`
	// LivenessGate selects how `bosun status` decides whether a WORKING
	// session has CRASHED:
	//
	//   "auto"     — default; check the attached-pid file first (if
	//                present), otherwise scan the process table for
	//                claude / claude-code / code-cli in the worktree.
	//                Today's behavior plus the explicit-attach refinement.
	//   "external" — skip all CRASHED transitions. RUNNING reports
	//                "external" and the state stays whatever the
	//                operator (or bosun_done) sets. Right for repos
	//                where the workers are exclusively driven by
	//                external orchestrators (Claude Code Task agents,
	//                CI agents, hand-launched terminals) and the
	//                proc-scan would always false-CRASH.
	LivenessGate string `json:"liveness_gate"`
}

// AgentSpawnConfig governs whether (and how much) agent-driven session
// spawning is permitted in this repo. Enforced by internal/mcp's
// bosun_spawn tool. The defaults are deliberately conservative:
// disabled, single-depth, 3 concurrent.
type AgentSpawnConfig struct {
	// Enabled gates the bosun_spawn MCP tool entirely. False by default
	// — agents cannot spawn until the operator opts in.
	Enabled bool `json:"enabled"`
	// MaxConcurrentSubSessions caps the live children a single parent
	// may have. Defaults to DefaultAgentSpawnMaxConcurrent.
	MaxConcurrentSubSessions int `json:"max_concurrent_sub_sessions"`
	// MaxDepth caps how deep a spawn tree may grow. 1 means parents
	// can spawn but their children can't; 2 allows grandchildren.
	// Values above MaxAgentSpawnDepthCeiling are silently clamped.
	MaxDepth int `json:"max_depth"`
	// AllowedForSessions, when non-empty, is a whitelist of session
	// labels permitted to spawn. When empty (the default), any session
	// may spawn. Useful for "this one workflow can fan out, others
	// stay single-shot."
	AllowedForSessions []string `json:"allowed_for_sessions,omitempty"`
}

// AgentSubtaskConfig governs whether (and how much) agent-driven
// sub-task registration is permitted in this repo. Enforced by
// internal/mcp's bosun_subtask tool. Defaults: disabled, 5 concurrent.
type AgentSubtaskConfig struct {
	// Enabled gates the bosun_subtask MCP tool entirely. False by
	// default — agents cannot register sub-tasks until the operator
	// opts in. The brief is intentionally conservative until the
	// v1.0 cancel + observability lanes land.
	Enabled bool `json:"enabled"`
	// MaxConcurrent caps the active sub-tasks a single parent may
	// have registered at once. Defaults to
	// DefaultAgentSubtaskMaxConcurrent. The spec argues for 8;
	// the lane-1 brief pinned 5 because the registry has no
	// completion path yet — sub-tasks linger until the parent
	// agent removes them or the operator nukes the dir.
	MaxConcurrent int `json:"max_concurrent"`
}

// SuggestConfig configures the brief-authoring assistant (`bosun suggest`).
// All fields are overridable via CLI flags on the suggest command.
type SuggestConfig struct {
	// Model is the Claude model ID used by the proposer. Defaults to
	// `claude-sonnet-4-6`.
	Model string `json:"model"`
	// MaxTokens caps the model's response. Defaults to 8000.
	MaxTokens int `json:"max_tokens"`
	// APIKeyEnv names the environment variable holding the Anthropic API
	// key. Defaults to `ANTHROPIC_API_KEY`. Operators can rename it to
	// keep multiple keys segregated.
	APIKeyEnv string `json:"api_key_env"`
}

// Defaults returns a Config populated with the documented defaults.
func Defaults() Config {
	return Config{
		BaseBranch:            DefaultBaseBranch,
		SessionPrefix:         DefaultSessionPrefix,
		WorktreeSuffixPattern: DefaultSuffixPattern,
		DefaultSessionCount:   DefaultSessionCount,
		IsolateCacheDefault:   DefaultIsolateCache,
		Launcher:              DefaultLauncherStrategy,
		VerifyCmd:             DefaultVerifyCmd,
		GitOpTimeoutSeconds:   DefaultGitOpTimeoutSeconds,
		Suggest: SuggestConfig{
			Model:     DefaultSuggestModel,
			MaxTokens: DefaultSuggestMaxTokens,
			APIKeyEnv: DefaultSuggestAPIKeyEnv,
		},
		AgentSpawn: AgentSpawnConfig{
			Enabled:                  DefaultAgentSpawnEnabled,
			MaxConcurrentSubSessions: DefaultAgentSpawnMaxConcurrent,
			MaxDepth:                 DefaultAgentSpawnMaxDepth,
		},
		AgentSubtask: AgentSubtaskConfig{
			Enabled:       DefaultAgentSubtaskEnabled,
			MaxConcurrent: DefaultAgentSubtaskMaxConcurrent,
		},
		LivenessGate: DefaultLivenessGate,
	}
}

// Load reads .bosun/config.json under repoRoot, returning defaults when the
// file is absent. Returns an error if the file exists but cannot be parsed.
func Load(repoRoot string) (Config, error) {
	path := filepath.Join(repoRoot, ConfigRelativePath)
	cfg := Defaults()

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}

	// Decode into an overlay so unset fields keep their defaults.
	var overlay Config
	if err := json.Unmarshal(data, &overlay); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}

	if overlay.BaseBranch != "" {
		cfg.BaseBranch = overlay.BaseBranch
	}
	if overlay.SessionPrefix != "" {
		cfg.SessionPrefix = overlay.SessionPrefix
	}
	if overlay.WorktreeSuffixPattern != "" {
		cfg.WorktreeSuffixPattern = overlay.WorktreeSuffixPattern
	}
	if overlay.DefaultSessionCount > 0 {
		cfg.DefaultSessionCount = overlay.DefaultSessionCount
	}
	if overlay.Launcher != "" {
		cfg.Launcher = overlay.Launcher
	}
	if overlay.VerifyCmd != "" {
		cfg.VerifyCmd = overlay.VerifyCmd
	}
	// Only override when the operator set a positive value — 0 / unset
	// keeps the documented default per DefaultGitOpTimeoutSeconds.
	if overlay.GitOpTimeoutSeconds > 0 {
		cfg.GitOpTimeoutSeconds = overlay.GitOpTimeoutSeconds
	}
	// Bool fields are tri-state-ish; we just adopt the parsed value.
	cfg.IsolateCacheDefault = overlay.IsolateCacheDefault
	cfg.Hooks = overlay.Hooks

	// Suggest sub-config: only override individual fields the user set,
	// so partial config files keep the documented defaults for the rest.
	if overlay.Suggest.Model != "" {
		cfg.Suggest.Model = overlay.Suggest.Model
	}
	if overlay.Suggest.MaxTokens > 0 {
		cfg.Suggest.MaxTokens = overlay.Suggest.MaxTokens
	}
	if overlay.Suggest.APIKeyEnv != "" {
		cfg.Suggest.APIKeyEnv = overlay.Suggest.APIKeyEnv
	}

	// AgentSpawn: bool fields adopt the parsed value (zero is meaningful
	// — false is the default). Numeric fields override when positive so
	// an omitted key keeps the documented defaults. Depth gets clamped
	// to the ceiling regardless of what the operator typed.
	cfg.AgentSpawn.Enabled = overlay.AgentSpawn.Enabled
	if overlay.AgentSpawn.MaxConcurrentSubSessions > 0 {
		cfg.AgentSpawn.MaxConcurrentSubSessions = overlay.AgentSpawn.MaxConcurrentSubSessions
	}
	if overlay.AgentSpawn.MaxDepth > 0 {
		cfg.AgentSpawn.MaxDepth = overlay.AgentSpawn.MaxDepth
	}
	if cfg.AgentSpawn.MaxDepth > MaxAgentSpawnDepthCeiling {
		cfg.AgentSpawn.MaxDepth = MaxAgentSpawnDepthCeiling
	}
	cfg.AgentSpawn.AllowedForSessions = overlay.AgentSpawn.AllowedForSessions

	// AgentSubtask: same shape as AgentSpawn — bool adopts the parsed
	// value (false is meaningful), positive int overrides the default.
	cfg.AgentSubtask.Enabled = overlay.AgentSubtask.Enabled
	if overlay.AgentSubtask.MaxConcurrent > 0 {
		cfg.AgentSubtask.MaxConcurrent = overlay.AgentSubtask.MaxConcurrent
	}

	// LivenessGate: only override when the operator explicitly set a
	// non-empty value, so a partial config file keeps the documented
	// default ("auto").
	if overlay.LivenessGate != "" {
		cfg.LivenessGate = overlay.LivenessGate
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate returns an error if any field has an obviously invalid value.
func (c Config) Validate() error {
	if c.DefaultSessionCount < 1 {
		return fmt.Errorf("default_session_count must be ≥ 1, got %d", c.DefaultSessionCount)
	}
	if c.SessionPrefix == "" {
		return fmt.Errorf("session_prefix must not be empty")
	}
	if strings.ContainsAny(c.SessionPrefix, "/ \t") {
		// `/` would collide with the prefix/label separator in branch names
		// (bosun/<label>); whitespace breaks shell-quoted CLI args downstream.
		return fmt.Errorf("session_prefix must not contain '/' or whitespace, got %q", c.SessionPrefix)
	}
	if !strings.Contains(c.WorktreeSuffixPattern, "{N}") {
		return fmt.Errorf("worktree_suffix_pattern must contain {N}, got %q", c.WorktreeSuffixPattern)
	}
	if strings.HasPrefix(c.WorktreeSuffixPattern, "{N}") {
		// A pattern starting with {N} (e.g. "{N}" or "{N}-bosun") would yield
		// a worktree path like ".../myproj3" — collapsed onto repos whose
		// names happen to end in a digit and impossible to distinguish from
		// a non-bosun sibling directory. Require at least one literal byte
		// before the substitution point.
		return fmt.Errorf("worktree_suffix_pattern must not start with {N}, got %q", c.WorktreeSuffixPattern)
	}
	switch c.Launcher {
	case "auto", "tmux", "terminal", "print":
	default:
		return fmt.Errorf("launcher must be one of auto|tmux|terminal|print, got %q", c.Launcher)
	}
	switch c.LivenessGate {
	case "", LivenessGateAuto, LivenessGateExternal:
	default:
		return fmt.Errorf("liveness_gate must be one of %s|%s, got %q",
			LivenessGateAuto, LivenessGateExternal, c.LivenessGate)
	}
	if c.Suggest.MaxTokens < 0 {
		return fmt.Errorf("suggest.max_tokens must be ≥ 0, got %d", c.Suggest.MaxTokens)
	}
	if c.AgentSubtask.MaxConcurrent < 0 {
		return fmt.Errorf("agent_subtask.max_concurrent must be ≥ 0, got %d", c.AgentSubtask.MaxConcurrent)
	}
	if c.GitOpTimeoutSeconds < 0 {
		return fmt.Errorf("git_op_timeout_seconds must be ≥ 0, got %d", c.GitOpTimeoutSeconds)
	}
	for i, h := range c.Hooks {
		if !hooks.IsKnownEvent(h.Event) {
			return fmt.Errorf("hooks[%d]: unknown event %q (known: %s)", i, h.Event, strings.Join(hooks.KnownEvents, ", "))
		}
		if strings.TrimSpace(h.Command) == "" {
			return fmt.Errorf("hooks[%d]: command must not be empty", i)
		}
		if h.TimeoutSeconds < 0 {
			return fmt.Errorf("hooks[%d]: timeout_seconds must be ≥ 0, got %d", i, h.TimeoutSeconds)
		}
	}
	return nil
}

// WorktreeSuffix substitutes the session number into the configured pattern.
// Example: WorktreeSuffix(3, "") => "-bosun-3" with the default pattern.
// A non-empty roundTimestamp produces `-bosun-<ts>-3` per the scheme-C
// UID-per-worktree design (see docs/uid-worktree-design.md).
func (c Config) WorktreeSuffix(n int, roundTimestamp string) string {
	return c.WorktreeSuffixForLabel(fmt.Sprintf("%d", n), roundTimestamp)
}

// WorktreeSuffixForLabel substitutes a session label into the configured
// pattern. `{N}` accepts either form: a bare integer ("3") substitutes
// directly; a "session-N" label substitutes just the integer to stay
// byte-identical with the v0.1 numeric form; any other label substitutes
// the whole label (e.g. "auth" → "-bosun-auth").
//
// When roundTimestamp is non-empty, the composite value `<timestamp>-<sub>`
// is substituted instead (scheme C from docs/uid-worktree-design.md): a
// "session-3" label with timestamp "20260518-115400" yields
// "-bosun-20260518-115400-3". Empty roundTimestamp preserves the v0.1
// byte-identical numeric form for legacy callers — only init.go's per-
// round invocation passes a non-empty timestamp today; other callers will
// migrate in lane 4 (UID worktree migration).
func (c Config) WorktreeSuffixForLabel(label, roundTimestamp string) string {
	sub := label
	if rest, ok := strings.CutPrefix(label, "session-"); ok {
		// Strip the "session-" prefix so a "session-3" label still
		// produces "-bosun-3" rather than "-bosun-session-3" — keeping
		// numeric-mode paths byte-identical with v0.1.
		sub = rest
	}
	if roundTimestamp != "" {
		sub = roundTimestamp + "-" + sub
	}
	return strings.ReplaceAll(c.WorktreeSuffixPattern, "{N}", sub)
}

// BranchFor returns the bosun branch name for numeric session N.
// Example: BranchFor(2) => "bosun/session-2".
func (c Config) BranchFor(n int) string {
	return c.BranchForLabel(c.SessionName(n))
}

// BranchForLabel returns the bosun branch name for a session label.
// Example: BranchForLabel("auth") => "bosun/auth".
func (c Config) BranchForLabel(label string) string {
	return c.SessionPrefix + "/" + label
}

// SessionName returns the short session name for N: "session-N".
func (c Config) SessionName(n int) string {
	return fmt.Sprintf("session-%d", n)
}
