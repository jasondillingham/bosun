package config

import (
	"os"
	"path/filepath"
	"testing"
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
	if c != Defaults() {
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

func TestSuffixAndBranch(t *testing.T) {
	c := Defaults()
	if got := c.WorktreeSuffix(3); got != "-bosun-3" {
		t.Errorf("WorktreeSuffix(3) = %q, want -bosun-3", got)
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
	cases := map[string]string{
		"auth":      "-bosun-auth",
		"3":         "-bosun-3",
		"http":      "-bosun-http",
		"session-3": "-bosun-3", // "session-N" labels collapse to N for byte-identical numeric paths
		"session-1": "-bosun-1",
	}
	for label, want := range cases {
		if got := c.WorktreeSuffixForLabel(label); got != want {
			t.Errorf("WorktreeSuffixForLabel(%q) = %q, want %q", label, got, want)
		}
	}
	// BranchFor(n) must equal BranchForLabel(SessionName(n)) — the wrapper
	// contract that keeps numeric callers byte-identical.
	if c.BranchFor(5) != c.BranchForLabel(c.SessionName(5)) {
		t.Errorf("BranchFor wrapper drifted from BranchForLabel(SessionName)")
	}
	// Both forms must produce the same suffix for numeric sessions so
	// existing worktrees on disk keep their paths after the refactor.
	if c.WorktreeSuffix(4) != c.WorktreeSuffixForLabel("session-4") {
		t.Errorf("WorktreeSuffix wrapper drifted from WorktreeSuffixForLabel(session-N)")
	}
	if c.WorktreeSuffix(4) != c.WorktreeSuffixForLabel("4") {
		t.Errorf("WorktreeSuffix wrapper drifted from WorktreeSuffixForLabel(N)")
	}
}
