package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/git"
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

// configRecognizedKeys is the complete set of top-level JSON keys
// `.bosun/config.json` may contain. `bosun config validate` rejects any
// stray key so typos surface at the gate rather than being silently
// ignored by the loader's permissive json.Unmarshal.
//
// This is a superset of configListKeys: it also includes fields the
// scalar set/get/unset path doesn't expose (git_op_timeout_seconds is
// an int callers usually leave at the default; suggest is a sub-object).
var configRecognizedKeys = []string{
	"base_branch",
	"session_prefix",
	"worktree_suffix_pattern",
	"default_session_count",
	"isolate_cache_default",
	"launcher",
	"verify_cmd",
	"git_op_timeout_seconds",
	"hooks",
	"suggest",
}

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
		newConfigUnsetCmd(),
		newConfigValidateCmd(),
		newConfigInitCmd(),
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

func newConfigUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset <key>",
		Short: "Remove a key from .bosun/config.json (falls back to default)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigUnset(args[0])
		},
	}
}

func newConfigValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check .bosun/config.json parses and every key + value is valid",
		Long: `Read-only verification that .bosun/config.json is well-formed.

Checks the file parses as JSON, every top-level key is one bosun
recognises, every hook event matches the known event set, and every
value passes the loader's validation rules. Exits 0 when clean,
non-zero with a structured error otherwise. Suitable as a pre-commit
hook or CI gate.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigValidate()
		},
	}
}

func newConfigInitCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a stub .bosun/config.json populated with defaults",
		Long: `Create .bosun/config.json with every key set to its documented default.

Also writes .bosun/config.example.json — an annotated, human-only
sibling file with a leading // comment block describing each key.
The example file is documentation; bosun never loads it, so JSON's
no-comments rule doesn't apply.

Refuses to overwrite an existing config.json unless --force is set.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigInit(force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing .bosun/config.json")
	return cmd
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

func runConfigUnset(key string) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	if !isKnownConfigKey(key) {
		return userErr("unknown config key %q (known: %s)", key, strings.Join(configListKeys, ", "))
	}

	raw, err := readRawConfig(rc.repoRoot)
	if err != nil {
		return userErr("read config file: %v", err)
	}
	if _, ok := raw[key]; !ok {
		printf("bosun: %s already at default (no-op)\n", key)
		return nil
	}
	delete(raw, key)

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
	printf("bosun: unset %s (now %s)\n", key, formatConfigValue(merged, key))
	return nil
}

// runConfigValidate inspects .bosun/config.json without modifying it.
// Designed to be wired into a pre-commit hook or CI gate, so it must
// exit non-zero on any structural problem the loader would silently
// tolerate — most importantly typo'd top-level keys, which
// json.Unmarshal ignores by default.
func runConfigValidate() error {
	root, err := repoRootForConfig()
	if err != nil {
		return err
	}

	path := filepath.Join(root, config.ConfigRelativePath)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			printf("bosun: %s absent — using defaults (valid)\n", config.ConfigRelativePath)
			return nil
		}
		return internalErr("stat config", err)
	}

	raw, err := readRawConfig(root)
	if err != nil {
		return userErr("%v", err)
	}

	var unknown []string
	for k := range raw {
		if !isRecognizedConfigKey(k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return userErr("unrecognized key(s) in %s: %s (known: %s)",
			config.ConfigRelativePath,
			strings.Join(unknown, ", "),
			strings.Join(configRecognizedKeys, ", "))
	}

	// Delegate value-level checks (types, ranges, known hook events) to
	// config.Load — it parses and runs Validate. Anything it rejects is
	// surfaced verbatim so the operator sees the same message they'd see
	// running a normal bosun command against the broken file.
	if _, err := config.Load(root); err != nil {
		return userErr("%v", err)
	}

	printf("bosun: %s is valid\n", config.ConfigRelativePath)
	return nil
}

// runConfigInit writes a stub config and an annotated companion file.
// It deliberately skips loadCtx so it works even when the existing
// config.json is broken — `init --force` is the recovery path.
func runConfigInit(force bool) error {
	root, err := repoRootForConfig()
	if err != nil {
		return err
	}

	path := filepath.Join(root, config.ConfigRelativePath)
	if _, statErr := os.Stat(path); statErr == nil {
		if !force {
			return userErr("%s already exists; pass --force to overwrite", config.ConfigRelativePath)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return internalErr("stat config", statErr)
	}

	dir := filepath.Join(root, ".bosun")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return internalErr("mkdir .bosun", err)
	}

	defaults := config.Defaults()
	stub, err := json.MarshalIndent(defaults, "", "  ")
	if err != nil {
		return internalErr("marshal defaults", err)
	}
	stub = append(stub, '\n')
	if err := os.WriteFile(path, stub, 0o644); err != nil {
		return internalErr("write config", err)
	}

	examplePath := filepath.Join(dir, "config.example.json")
	if err := os.WriteFile(examplePath, []byte(buildConfigExample(defaults)), 0o644); err != nil {
		return internalErr("write config example", err)
	}

	printf("bosun: wrote %s\n", config.ConfigRelativePath)
	printf("bosun: wrote .bosun/config.example.json (annotated reference)\n")
	return nil
}

// buildConfigExample emits a human-readable annotated copy of the
// defaults. The file starts with `//`-prefixed lines describing each
// key, then the JSON body. It is not valid JSON and is not loaded by
// bosun — operators copy values out of it into config.json by hand.
func buildConfigExample(c config.Config) string {
	body, _ := json.MarshalIndent(c, "", "  ")
	var b strings.Builder
	b.WriteString("// bosun config — annotated reference. Documentation only;\n")
	b.WriteString("// bosun reads .bosun/config.json, never this file. Copy values\n")
	b.WriteString("// you want into config.json (strip these comment lines first —\n")
	b.WriteString("// JSON forbids comments).\n")
	b.WriteString("//\n")
	b.WriteString("// base_branch:             git branch new sessions branch off of.\n")
	b.WriteString("// session_prefix:          leading segment for bosun branch names (prefix/session-N).\n")
	b.WriteString("// worktree_suffix_pattern: suffix appended to the repo dirname for each worktree path.\n")
	b.WriteString("//                          Must contain {N}; must not start with it.\n")
	b.WriteString("// default_session_count:   how many sessions `bosun init` creates without an explicit count.\n")
	b.WriteString("// isolate_cache_default:   copy node_modules / build cache into each worktree at init time.\n")
	b.WriteString("// launcher:                agent-window strategy: auto | tmux | terminal | print.\n")
	b.WriteString("// verify_cmd:              command the brief preamble tells the agent to run before `bosun done`.\n")
	b.WriteString("// git_op_timeout_seconds:  per-operation cap on each `git` subprocess (0 = built-in default).\n")
	b.WriteString("// hooks:                   list of {event, command, fail_open?, timeout_seconds?} entries.\n")
	b.WriteString("//                          Events: pre-init, post-init, post-done, pre-merge, post-merge,\n")
	b.WriteString("//                          pre-cleanup, post-cleanup.\n")
	b.WriteString("// suggest:                 brief-authoring assistant config (model, max_tokens, api_key_env).\n")
	b.WriteString("//\n")
	b.Write(body)
	b.WriteString("\n")
	return b.String()
}

// repoRootForConfig resolves the main worktree path without going
// through loadCtx so the caller doesn't inherit config.Load failures.
// `validate` and `init` need to run against repos whose config is
// currently broken; loadCtx would refuse before they got a chance.
func repoRootForConfig() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", internalErr("getwd", err)
	}
	root, err := git.New().MainWorktreePath(context.Background(), cwd)
	if err != nil {
		return "", userErr("not inside a git repository (cwd=%s)", cwd)
	}
	return root, nil
}

func isKnownConfigKey(key string) bool {
	for _, k := range configListKeys {
		if k == key {
			return true
		}
	}
	return false
}

func isRecognizedConfigKey(key string) bool {
	for _, k := range configRecognizedKeys {
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
