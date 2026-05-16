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
	DefaultBaseBranch        = "main"
	DefaultSessionPrefix     = "bosun"
	DefaultSuffixPattern     = "-bosun-{N}"
	DefaultSessionCount      = 4
	DefaultIsolateCache      = false
	DefaultLauncherStrategy  = "auto"
	DefaultVerifyCmd         = "make check"
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
	DefaultSuggestAPIKeyEnv = "ANTHROPIC_API_KEY"
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
	if c.Suggest.MaxTokens < 0 {
		return fmt.Errorf("suggest.max_tokens must be ≥ 0, got %d", c.Suggest.MaxTokens)
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
// Example: WorktreeSuffix(3) => "-bosun-3" with the default pattern.
func (c Config) WorktreeSuffix(n int) string {
	return c.WorktreeSuffixForLabel(fmt.Sprintf("%d", n))
}

// WorktreeSuffixForLabel substitutes a session label into the configured
// pattern. `{N}` accepts either form: a bare integer ("3") substitutes
// directly; a "session-N" label substitutes just the integer to stay
// byte-identical with the v0.1 numeric form; any other label substitutes
// the whole label (e.g. "auth" → "-bosun-auth").
func (c Config) WorktreeSuffixForLabel(label string) string {
	sub := label
	if rest, ok := strings.CutPrefix(label, "session-"); ok {
		// Strip the "session-" prefix so a "session-3" label still
		// produces "-bosun-3" rather than "-bosun-session-3" — keeping
		// numeric-mode paths byte-identical with v0.1.
		sub = rest
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
