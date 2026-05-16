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
