package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/hooks"
)

func TestDefaults(t *testing.T) {
	c := Defaults()
	if c.BaseBranch != "main" {
		t.Fatalf("BaseBranch = %q, want main", c.BaseBranch)
	}
	if c.DefaultSessionCount != 4 {
		t.Fatalf("DefaultSessionCount = %d, want 4", c.DefaultSessionCount)
	}
	if c.Launcher != "auto" {
		t.Fatalf("Launcher = %q, want auto", c.Launcher)
	}
	if c.GitOpTimeoutSeconds != DefaultGitOpTimeoutSeconds {
		t.Fatalf("GitOpTimeoutSeconds = %d, want %d", c.GitOpTimeoutSeconds, DefaultGitOpTimeoutSeconds)
	}
	if c.Suggest.Model != DefaultSuggestModel {
		t.Fatalf("Suggest.Model = %q, want %q", c.Suggest.Model, DefaultSuggestModel)
	}
	if c.Suggest.MaxTokens != DefaultSuggestMaxTokens {
		t.Fatalf("Suggest.MaxTokens = %d, want %d", c.Suggest.MaxTokens, DefaultSuggestMaxTokens)
	}
	if c.Suggest.APIKeyEnv != DefaultSuggestAPIKeyEnv {
		t.Fatalf("Suggest.APIKeyEnv = %q, want %q", c.Suggest.APIKeyEnv, DefaultSuggestAPIKeyEnv)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Defaults().Validate() = %v", err)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(missing) error: %v", err)
	}
	if !reflect.DeepEqual(c, Defaults()) {
		t.Fatalf("Load(missing) = %+v, want defaults", c)
	}
}

func TestLoad_VerifyCmdOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"verify_cmd":"make test"}`)
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.VerifyCmd != "make test" {
		t.Fatalf("VerifyCmd = %q, want make test", c.VerifyCmd)
	}
	// Other fields still defaulted.
	if c.BaseBranch != "main" {
		t.Errorf("BaseBranch defaulted lost: %q", c.BaseBranch)
	}
}

func TestLoad_AgentCommandDefault(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentCommand != "claude" {
		t.Errorf("AgentCommand default = %q, want %q", c.AgentCommand, "claude")
	}
}

func TestLoad_AgentCommandOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"agent_command":"./scripts/ollama-claude.sh"}`)
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentCommand != "./scripts/ollama-claude.sh" {
		t.Errorf("AgentCommand = %q, want %q", c.AgentCommand, "./scripts/ollama-claude.sh")
	}
	// Other fields still defaulted — the override is field-scoped.
	if c.VerifyCmd != "make check" {
		t.Errorf("VerifyCmd default lost: %q", c.VerifyCmd)
	}
}

func TestLoad_OverridesOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"base_branch":"develop","default_session_count":8,"isolate_cache_default":true}`)
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q, want develop", c.BaseBranch)
	}
	if c.DefaultSessionCount != 8 {
		t.Errorf("DefaultSessionCount = %d, want 8", c.DefaultSessionCount)
	}
	if !c.IsolateCacheDefault {
		t.Errorf("IsolateCacheDefault = false, want true")
	}
	if c.SessionPrefix != "bosun" {
		t.Errorf("SessionPrefix overridden unexpectedly: %q", c.SessionPrefix)
	}
	if c.Launcher != "auto" {
		t.Errorf("Launcher overridden unexpectedly: %q", c.Launcher)
	}
}

func TestLoad_SuggestOverridesPartial(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Only override the model; max_tokens + api_key_env should keep defaults.
	data := []byte(`{"suggest":{"model":"claude-opus-4-7"}}`)
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Suggest.Model != "claude-opus-4-7" {
		t.Errorf("Suggest.Model = %q, want claude-opus-4-7", c.Suggest.Model)
	}
	if c.Suggest.MaxTokens != DefaultSuggestMaxTokens {
		t.Errorf("Suggest.MaxTokens defaulted lost: %d", c.Suggest.MaxTokens)
	}
	if c.Suggest.APIKeyEnv != DefaultSuggestAPIKeyEnv {
		t.Errorf("Suggest.APIKeyEnv defaulted lost: %q", c.Suggest.APIKeyEnv)
	}
}

func TestLoad_SuggestOverridesFull(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"suggest":{"model":"claude-sonnet-4-6","max_tokens":12000,"api_key_env":"ANTHROPIC_KEY_ALT"}}`)
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Suggest.MaxTokens != 12000 {
		t.Errorf("Suggest.MaxTokens = %d, want 12000", c.Suggest.MaxTokens)
	}
	if c.Suggest.APIKeyEnv != "ANTHROPIC_KEY_ALT" {
		t.Errorf("Suggest.APIKeyEnv = %q, want ANTHROPIC_KEY_ALT", c.Suggest.APIKeyEnv)
	}
}

func TestValidate_SuggestRejectsNegativeMaxTokens(t *testing.T) {
	c := Defaults()
	c.Suggest.MaxTokens = -1
	if err := c.Validate(); err == nil {
		t.Fatal("Validate with negative Suggest.MaxTokens should fail")
	}
}

func TestValidate_GitOpTimeoutRejectsNegative(t *testing.T) {
	c := Defaults()
	c.GitOpTimeoutSeconds = -1
	if err := c.Validate(); err == nil {
		t.Fatal("Validate with negative GitOpTimeoutSeconds should fail")
	}
}

func TestValidate_GitOpTimeoutZeroAllowed(t *testing.T) {
	// Zero is a sentinel meaning "use the default" — Load() leaves the
	// defaulted value in place when the overlay reports 0. Validate must
	// accept it so a defaulted Config stays valid even before Load wires
	// the constant in.
	c := Defaults()
	c.GitOpTimeoutSeconds = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate with zero GitOpTimeoutSeconds: %v", err)
	}
}

func TestLoad_GitOpTimeoutOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), []byte(`{"git_op_timeout_seconds":120}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GitOpTimeoutSeconds != 120 {
		t.Fatalf("GitOpTimeoutSeconds = %d, want 120", c.GitOpTimeoutSeconds)
	}
}

func TestLoad_GitOpTimeoutKeepsDefaultWhenUnset(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A config file that doesn't mention the field must keep the default
	// rather than coercing it to JSON's zero (which would be 0 / unbounded).
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), []byte(`{"base_branch":"develop"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GitOpTimeoutSeconds != DefaultGitOpTimeoutSeconds {
		t.Fatalf("GitOpTimeoutSeconds = %d, want default %d", c.GitOpTimeoutSeconds, DefaultGitOpTimeoutSeconds)
	}
}

func TestLoad_BadJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"defaults ok", func(c *Config) {}, false},
		{"zero sessions", func(c *Config) { c.DefaultSessionCount = 0 }, true},
		{"missing {N}", func(c *Config) { c.WorktreeSuffixPattern = "-bosun" }, true},
		{"unknown launcher", func(c *Config) { c.Launcher = "weird" }, true},
		{"launcher tmux ok", func(c *Config) { c.Launcher = "tmux" }, false},
		{"empty session prefix", func(c *Config) { c.SessionPrefix = "" }, true},
		{"session prefix with slash", func(c *Config) { c.SessionPrefix = "team/bosun" }, true},
		{"session prefix with space", func(c *Config) { c.SessionPrefix = "team bosun" }, true},
		{"session prefix with tab", func(c *Config) { c.SessionPrefix = "team\tbosun" }, true},
		{"suffix starts with {N}", func(c *Config) { c.WorktreeSuffixPattern = "{N}-bosun" }, true},
		{"suffix is only {N}", func(c *Config) { c.WorktreeSuffixPattern = "{N}" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			tc.mutate(&c)
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestValidate_Hooks(t *testing.T) {
	cases := []struct {
		name    string
		hooks   []hooks.Hook
		wantErr bool
	}{
		{"no hooks", nil, false},
		{"known event", []hooks.Hook{{Event: "post-init", Command: "echo hi"}}, false},
		{"all known events", []hooks.Hook{
			{Event: "pre-init", Command: "true"},
			{Event: "post-init", Command: "true"},
			{Event: "post-done", Command: "true"},
			{Event: "pre-merge", Command: "true"},
			{Event: "post-merge", Command: "true"},
		}, false},
		{"unknown event", []hooks.Hook{{Event: "not-a-real-event", Command: "echo"}}, true},
		{"empty event", []hooks.Hook{{Event: "", Command: "echo"}}, true},
		{"empty command", []hooks.Hook{{Event: "pre-init", Command: ""}}, true},
		{"whitespace command", []hooks.Hook{{Event: "pre-init", Command: "   "}}, true},
		{"negative timeout", []hooks.Hook{{Event: "pre-init", Command: "echo", TimeoutSeconds: -1}}, true},
		{"zero timeout ok", []hooks.Hook{{Event: "pre-init", Command: "echo", TimeoutSeconds: 0}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.Hooks = tc.hooks
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate(hooks=%v) err=%v, wantErr=%v", tc.hooks, err, tc.wantErr)
			}
		})
	}
}

func TestLoad_Hooks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"hooks":[{"event":"post-init","command":"echo hi","fail_open":true,"timeout_seconds":5}]}`)
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Hooks) != 1 {
		t.Fatalf("len(Hooks) = %d, want 1", len(c.Hooks))
	}
	got := c.Hooks[0]
	if got.Event != "post-init" || got.Command != "echo hi" || !got.FailOpen || got.TimeoutSeconds != 5 {
		t.Fatalf("Hook parsed wrong: %+v", got)
	}
}

func TestLoad_RejectsUnknownHookEvent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"hooks":[{"event":"not-a-real-event","command":"echo hi"}]}`)
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load with unknown hook event should fail")
	}
}

func TestSuffixAndBranch(t *testing.T) {
	c := Defaults()
	if got := c.WorktreeSuffix(3, ""); got != "-bosun-3" {
		t.Errorf("WorktreeSuffix(3, \"\") = %q, want -bosun-3", got)
	}
	if got := c.BranchFor(7); got != "bosun/session-7" {
		t.Errorf("BranchFor(7) = %q", got)
	}
	if got := c.SessionName(2); got != "session-2" {
		t.Errorf("SessionName(2) = %q", got)
	}
}

func TestBranchForLabel(t *testing.T) {
	c := Defaults()
	cases := map[string]string{
		"auth":      "bosun/auth",
		"session-2": "bosun/session-2",
		"http":      "bosun/http",
	}
	for label, want := range cases {
		if got := c.BranchForLabel(label); got != want {
			t.Errorf("BranchForLabel(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestWorktreeSuffixForLabel(t *testing.T) {
	c := Defaults()
	// Empty round timestamp keeps the v0.1 legacy substitution. Non-empty
	// timestamp triggers the scheme-C UID-per-worktree form: the value
	// substituted for {N} becomes `<timestamp>-<sub>`.
	cases := []struct {
		label     string
		timestamp string
		want      string
	}{
		// Legacy form: empty timestamp.
		{"auth", "", "-bosun-auth"},
		{"3", "", "-bosun-3"},
		{"http", "", "-bosun-http"},
		{"session-3", "", "-bosun-3"}, // "session-N" labels collapse to N for byte-identical numeric paths
		{"session-1", "", "-bosun-1"},
		// UID-per-worktree form: non-empty timestamp.
		{"3", "20260518-115400", "-bosun-20260518-115400-3"},
		{"session-3", "20260518-115400", "-bosun-20260518-115400-3"},
		{"auth", "20260518-115400", "-bosun-20260518-115400-auth"},
		{"session-1", "20260101-000000", "-bosun-20260101-000000-1"},
	}
	for _, tc := range cases {
		if got := c.WorktreeSuffixForLabel(tc.label, tc.timestamp); got != tc.want {
			t.Errorf("WorktreeSuffixForLabel(%q, %q) = %q, want %q", tc.label, tc.timestamp, got, tc.want)
		}
	}
	// BranchFor(n) must equal BranchForLabel(SessionName(n)) — the wrapper
	// contract that keeps numeric callers byte-identical.
	if c.BranchFor(5) != c.BranchForLabel(c.SessionName(5)) {
		t.Errorf("BranchFor wrapper drifted from BranchForLabel(SessionName)")
	}
	// Both forms must produce the same suffix for numeric sessions so
	// existing worktrees on disk keep their paths after the refactor.
	if c.WorktreeSuffix(4, "") != c.WorktreeSuffixForLabel("session-4", "") {
		t.Errorf("WorktreeSuffix wrapper drifted from WorktreeSuffixForLabel(session-N)")
	}
	if c.WorktreeSuffix(4, "") != c.WorktreeSuffixForLabel("4", "") {
		t.Errorf("WorktreeSuffix wrapper drifted from WorktreeSuffixForLabel(N)")
	}
	// Wrapper contract holds under a non-empty round timestamp too.
	ts := "20260518-115400"
	if c.WorktreeSuffix(4, ts) != c.WorktreeSuffixForLabel("session-4", ts) {
		t.Errorf("WorktreeSuffix(n, ts) wrapper drifted from WorktreeSuffixForLabel(session-N, ts)")
	}
}

// TestLoad_DockerHostsOverride exercises round-tripping a Hosts list
// through the on-disk loader (Phase 3 lane 1). Empty defaults to nil
// per docs/remote-docker-plan.md; an operator-supplied list survives
// Load unchanged so cmd_init can pick Hosts[0] as the round default.
func TestLoad_DockerHostsOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"docker":{"hosts":["ssh://thor","tcp://10.0.0.5:2375"]}}`)
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Docker.Hosts) != 2 {
		t.Fatalf("len(Docker.Hosts) = %d, want 2 (%v)", len(c.Docker.Hosts), c.Docker.Hosts)
	}
	if c.Docker.Hosts[0] != "ssh://thor" {
		t.Errorf("Docker.Hosts[0] = %q, want ssh://thor", c.Docker.Hosts[0])
	}
	if c.Docker.Hosts[1] != "tcp://10.0.0.5:2375" {
		t.Errorf("Docker.Hosts[1] = %q, want tcp://10.0.0.5:2375", c.Docker.Hosts[1])
	}
}

// TestLoad_DockerHostsEmptyValid pins the "no hosts configured means
// local docker" contract — empty list parses cleanly and the loader
// returns no error.
func TestLoad_DockerHostsEmptyValid(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"docker":{"hosts":[]}}`)
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Docker.Hosts) != 0 {
		t.Errorf("Docker.Hosts = %v, want empty", c.Docker.Hosts)
	}
}

// TestValidate_DockerHosts is the table-driven validation harness for
// the docker.hosts entries — every supported and rejected shape lives
// here so the JSON loader's behavior stays observable from one place.
func TestValidate_DockerHosts(t *testing.T) {
	cases := []struct {
		name    string
		hosts   []string
		wantErr bool
	}{
		{"empty list ok", nil, false},
		{"empty slice ok", []string{}, false},
		{"single ssh ok", []string{"ssh://thor"}, false},
		{"single tcp ok", []string{"tcp://10.0.0.5:2375"}, false},
		{"ssh with user/port ok", []string{"ssh://operator@thor.lan:2222"}, false},
		{"multiple mixed ok", []string{"ssh://thor", "tcp://10.0.0.5:2375"}, false},
		{"empty entry rejected", []string{""}, true},
		{"whitespace-only entry rejected", []string{"   "}, true},
		{"missing scheme rejected", []string{"thor"}, true},
		{"unix scheme rejected", []string{"unix:///var/run/docker.sock"}, true},
		{"http scheme rejected", []string{"http://thor"}, true},
		{"https scheme rejected", []string{"https://thor"}, true},
		{"scheme without host rejected", []string{"ssh://"}, true},
		{"unparseable url rejected", []string{"ssh://[badport"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.Docker.Hosts = tc.hosts
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate(Docker.Hosts=%v) err=%v, wantErr=%v", tc.hosts, err, tc.wantErr)
			}
		})
	}
}

// TestValidate_MCPTools_TableDriven covers the Phase 5 #61 validation
// surface. Each row is one failure mode plus one happy-path baseline,
// asserting both directions: invalid defs must surface a specific
// error string the operator can act on, and the canonical valid def
// must accept.
func TestValidate_MCPTools_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		defs    []MCPToolDef
		wantErr string // substring; empty means "expect no error"
	}{
		{
			name: "valid def passes",
			defs: []MCPToolDef{{
				Name:        "bosun_lint",
				Description: "Run the repo's lint script",
				Command:     []string{"./scripts/lint.sh"},
			}},
		},
		{
			name: "empty name refused",
			defs: []MCPToolDef{{
				Name: "", Description: "x", Command: []string{"true"},
			}},
			wantErr: "name must not be empty",
		},
		{
			name: "name without bosun_ prefix refused",
			defs: []MCPToolDef{{
				Name: "lint", Description: "x", Command: []string{"true"},
			}},
			wantErr: `must start with "bosun_"`,
		},
		{
			name: "duplicate names refused",
			defs: []MCPToolDef{
				{Name: "bosun_lint", Description: "a", Command: []string{"true"}},
				{Name: "bosun_lint", Description: "b", Command: []string{"true"}},
			},
			wantErr: "duplicate name",
		},
		{
			name: "blank description refused",
			defs: []MCPToolDef{{
				Name: "bosun_lint", Description: "  ", Command: []string{"true"},
			}},
			wantErr: "description must not be empty",
		},
		{
			name: "empty command refused",
			defs: []MCPToolDef{{
				Name: "bosun_lint", Description: "x", Command: nil,
			}},
			wantErr: "command must not be empty",
		},
		{
			name: "blank command[0] refused",
			defs: []MCPToolDef{{
				Name: "bosun_lint", Description: "x", Command: []string{"  "},
			}},
			wantErr: "must not be blank",
		},
		{
			name: "negative timeout refused",
			defs: []MCPToolDef{{
				Name: "bosun_lint", Description: "x", Command: []string{"true"},
				TimeoutSeconds: -1,
			}},
			wantErr: "timeout_seconds must be ≥ 0",
		},
		{
			name: "timeout above ceiling refused",
			defs: []MCPToolDef{{
				Name: "bosun_lint", Description: "x", Command: []string{"true"},
				TimeoutSeconds: MaxCustomToolTimeoutSeconds + 1,
			}},
			wantErr: "exceeds the",
		},
		{
			name: "zero timeout is OK (defaulted at exec time)",
			defs: []MCPToolDef{{
				Name: "bosun_lint", Description: "x", Command: []string{"true"},
				TimeoutSeconds: 0,
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.MCPTools = tc.defs
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate returned %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate returned nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoad_MCPToolsRoundTrip confirms an MCPTools list survives the
// JSON loader — the field appears verbatim in the loaded Config and
// the validate gate runs at Load time so a bad def can't reach
// runtime by hand-editing the file.
func TestLoad_MCPToolsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"mcp_tools":[{"name":"bosun_lint","description":"run the linter","command":["./lint.sh","--fix"],"timeout_seconds":15}]}`
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.MCPTools) != 1 {
		t.Fatalf("MCPTools len = %d, want 1", len(c.MCPTools))
	}
	got := c.MCPTools[0]
	if got.Name != "bosun_lint" || got.Description != "run the linter" ||
		len(got.Command) != 2 || got.Command[0] != "./lint.sh" || got.Command[1] != "--fix" ||
		got.TimeoutSeconds != 15 {
		t.Errorf("loaded def = %+v, mismatch", got)
	}
}

// TestLoad_RejectsBadMCPToolsDef confirms the validate path fires
// from the file loader too — same shape as TestLoad_RejectsBadDockerHost.
func TestLoad_RejectsBadMCPToolsDef(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Name without bosun_ prefix — same gate the unit test covers.
	body := `{"mcp_tools":[{"name":"lint","description":"x","command":["true"]}]}`
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load with a bare-name mcp_tools def should fail")
	}
}

// TestLoad_RejectsBadDockerHost ensures the validate path fires from
// the file loader too — an operator who hand-edits config.json into a
// bad shape gets the same refusal as the table-driven Validate call.
func TestLoad_RejectsBadDockerHost(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"docker":{"hosts":["unix:///var/run/docker.sock"]}}`)
	if err := os.WriteFile(filepath.Join(dir, ".bosun/config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load with unix:// docker host should fail")
	}
}
