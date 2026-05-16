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
)

const (
	DefaultBaseBranch        = "main"
	DefaultSessionPrefix     = "bosun"
	DefaultSuffixPattern     = "-bosun-{N}"
	DefaultSessionCount      = 4
	DefaultIsolateCache      = false
	DefaultLauncherStrategy  = "auto"
	DefaultVerifyCmd         = "make check"
	ConfigRelativePath       = ".bosun/config.json"
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
	// Bool fields are tri-state-ish; we just adopt the parsed value.
	cfg.IsolateCacheDefault = overlay.IsolateCacheDefault

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
	if !strings.Contains(c.WorktreeSuffixPattern, "{N}") {
		return fmt.Errorf("worktree_suffix_pattern must contain {N}, got %q", c.WorktreeSuffixPattern)
	}
	switch c.Launcher {
	case "auto", "tmux", "terminal", "print":
	default:
		return fmt.Errorf("launcher must be one of auto|tmux|terminal|print, got %q", c.Launcher)
	}
	return nil
}

// WorktreeSuffix substitutes the session number into the configured pattern.
// Example: WorktreeSuffix(3) => "-bosun-3" with the default pattern.
func (c Config) WorktreeSuffix(n int) string {
	return strings.ReplaceAll(c.WorktreeSuffixPattern, "{N}", fmt.Sprintf("%d", n))
}

// BranchFor returns the bosun branch name for session N.
// Example: BranchFor(2) => "bosun/session-2".
func (c Config) BranchFor(n int) string {
	return fmt.Sprintf("%s/session-%d", c.SessionPrefix, n)
}

// SessionName returns the short session name for N: "session-N".
func (c Config) SessionName(n int) string {
	return fmt.Sprintf("session-%d", n)
}
