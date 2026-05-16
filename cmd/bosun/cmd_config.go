package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/spf13/cobra"
)

// configSetKeys is the ordered list of scalar keys `bosun config set` accepts.
// The `hooks` field is intentionally excluded: it's a list of records, not a
// scalar, so editing it through a single key=value CLI would be lossy.
var configSetKeys = []string{
	"base_branch",
	"launcher",
	"verify_cmd",
	"default_session_count",
	"session_prefix",
	"worktree_suffix_pattern",
	"isolate_cache_default",
}

// configListKeys is the order `bosun config list` and `bosun config get` know
// about. It's `configSetKeys` plus the read-only `hooks` summary.
var configListKeys = append(append([]string{}, configSetKeys...), "hooks")

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and edit .bosun/config.json",
		Long: `Read or write bosun's on-disk config.

Without this subcommand, .bosun/config.json must be hand-edited; typos in keys
fail silently and revert to defaults. ` + "`bosun config`" + ` validates keys and types
and writes the file atomically.`,
	}
	cmd.AddCommand(
		newConfigListCmd(),
		newConfigGetCmd(),
		newConfigSetCmd(),
	)
	return cmd
}

func newConfigListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Print every resolved config key (marks defaults)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigList()
		},
	}
}

func newConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print the value for one config key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigGet(args[0])
		},
	}
}

func newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a config key and write .bosun/config.json",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigSet(args[0], args[1])
		},
	}
}

func runConfigList() error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	raw, err := readRawConfig(rc.repoRoot)
	if err != nil {
		return userErr("read config file: %v", err)
	}
	for _, key := range configListKeys {
		marker := ""
		if _, ok := raw[key]; !ok {
			marker = " (default)"
		}
		printf("%s: %s%s\n", key, formatConfigValue(rc.cfg, key), marker)
	}
	return nil
}

func runConfigGet(key string) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	if !isKnownConfigKey(key) {
		return userErr("unknown config key %q (known: %s)", key, strings.Join(configListKeys, ", "))
	}
	printf("%s\n", formatConfigValue(rc.cfg, key))
	return nil
}

func runConfigSet(key, value string) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	if key == "hooks" {
		return userErr("hooks is a list — edit %s directly (config set handles scalars only)", config.ConfigRelativePath)
	}
	if !isSettableConfigKey(key) {
		return userErr("unknown config key %q (settable: %s)", key, strings.Join(configSetKeys, ", "))
	}

	raw, err := readRawConfig(rc.repoRoot)
	if err != nil {
		return userErr("read config file: %v", err)
	}

	encoded, err := encodeConfigValue(key, value)
	if err != nil {
		return userErr("%v", err)
	}
	raw[key] = encoded

	merged, err := configFromRaw(raw)
	if err != nil {
		return userErr("%v", err)
	}
	if err := merged.Validate(); err != nil {
		return userErr("%v", err)
	}

	if err := writeConfigAtomic(rc.repoRoot, raw); err != nil {
		return internalErr("write config", err)
	}
	printf("bosun: set %s = %s\n", key, formatConfigValue(merged, key))
	return nil
}

func isKnownConfigKey(key string) bool {
	for _, k := range configListKeys {
		if k == key {
			return true
		}
	}
	return false
}

func isSettableConfigKey(key string) bool {
	for _, k := range configSetKeys {
		if k == key {
			return true
		}
	}
	return false
}

// formatConfigValue renders a single key's resolved value as a string. For
// `hooks` it emits a count summary so list/get stay scalar even though the
// underlying field is a list.
func formatConfigValue(cfg config.Config, key string) string {
	switch key {
	case "base_branch":
		return cfg.BaseBranch
	case "launcher":
		return cfg.Launcher
	case "verify_cmd":
		return cfg.VerifyCmd
	case "default_session_count":
		return strconv.Itoa(cfg.DefaultSessionCount)
	case "session_prefix":
		return cfg.SessionPrefix
	case "worktree_suffix_pattern":
		return cfg.WorktreeSuffixPattern
	case "isolate_cache_default":
		return strconv.FormatBool(cfg.IsolateCacheDefault)
	case "hooks":
		return fmt.Sprintf("%d hook(s)", len(cfg.Hooks))
	}
	return ""
}

// encodeConfigValue parses the user-supplied string into the type expected on
// disk and returns the JSON-encoded form. Type mismatches surface here as
// user errors rather than silently coercing.
func encodeConfigValue(key, value string) (json.RawMessage, error) {
	switch key {
	case "base_branch", "launcher", "verify_cmd", "session_prefix", "worktree_suffix_pattern":
		return json.Marshal(value)
	case "default_session_count":
		n, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("%s must be an integer, got %q", key, value)
		}
		return json.Marshal(n)
	case "isolate_cache_default":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("%s must be true|false, got %q", key, value)
		}
		return json.Marshal(b)
	}
	return nil, fmt.Errorf("unknown config key %q", key)
}

// readRawConfig reads .bosun/config.json as a key→raw-bytes map. The map is
// empty (not nil) when the file is missing. The raw form lets callers tell
// "key absent from disk" apart from "key present and equal to the default,"
// which the loader can't distinguish for zero-valued scalars.
func readRawConfig(repoRoot string) (map[string]json.RawMessage, error) {
	path := filepath.Join(repoRoot, config.ConfigRelativePath)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, err
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return raw, nil
}

// configFromRaw mirrors config.Load's overlay-on-defaults merge so we can
// validate the proposed file contents before writing.
func configFromRaw(raw map[string]json.RawMessage) (config.Config, error) {
	cfg := config.Defaults()
	if len(raw) == 0 {
		return cfg, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return cfg, fmt.Errorf("re-encode config: %w", err)
	}
	var overlay config.Config
	if err := json.Unmarshal(data, &overlay); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
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
	cfg.IsolateCacheDefault = overlay.IsolateCacheDefault
	cfg.Hooks = overlay.Hooks
	return cfg, nil
}

// writeConfigAtomic marshals raw into pretty-printed JSON and replaces
// .bosun/config.json with a temp-file+rename so a concurrent read never
// observes a half-written file. The .bosun directory is created if missing.
func writeConfigAtomic(repoRoot string, raw map[string]json.RawMessage) error {
	dir := filepath.Join(repoRoot, ".bosun")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	final := filepath.Join(repoRoot, config.ConfigRelativePath)
	if err := os.Rename(tmpPath, final); err != nil {
		cleanup()
		return err
	}
	return nil
}
