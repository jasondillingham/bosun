package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDockerHostsConfig(t *testing.T, repo string, hosts []string) {
	t.Helper()
	cfgDir := filepath.Join(repo, ".bosun")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir .bosun: %v", err)
	}
	quoted := make([]string, len(hosts))
	for i, h := range hosts {
		quoted[i] = fmt.Sprintf("%q", h)
	}
	body := fmt.Sprintf(`{"docker": {"hosts": [%s]}}`, strings.Join(quoted, ", "))
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestCheckDockerHosts_NoHostsConfigured pins the no-op-on-empty
// shortcut. Without this, every repo without remote-docker config
// would show docker-hosts as a doctor warning every time.
func TestCheckDockerHosts_NoHostsConfigured(t *testing.T) {
	repo := t.TempDir()
	res := CheckDockerHosts(context.Background(), repo)
	if res.Status != Pass {
		t.Errorf("Status = %v, want Pass for empty hosts config", res.Status)
	}
	if !strings.Contains(res.Message, "no remote hosts") {
		t.Errorf("Message should explain the no-config state: %q", res.Message)
	}
}

// TestCheckDockerHosts_AllReachable simulates every configured host
// responding successfully to `docker info`.
func TestCheckDockerHosts_AllReachable(t *testing.T) {
	repo := t.TempDir()
	writeDockerHostsConfig(t, repo, []string{"ssh://thor", "ssh://loki"})
	prev := dockerInfoFn
	dockerInfoFn = func(_ context.Context, host string) error { return nil }
	t.Cleanup(func() { dockerInfoFn = prev })

	res := CheckDockerHosts(context.Background(), repo)
	if res.Status != Pass {
		t.Errorf("Status = %v, want Pass when all hosts reachable", res.Status)
	}
	if !strings.Contains(res.Message, "all 2") {
		t.Errorf("Message should report host count: %q", res.Message)
	}
}

// TestCheckDockerHosts_PartialFailure names exactly which hosts are
// unreachable in the diagnostic — operators can fix individual SSH
// bridges rather than wondering which.
func TestCheckDockerHosts_PartialFailure(t *testing.T) {
	repo := t.TempDir()
	writeDockerHostsConfig(t, repo, []string{"ssh://thor", "ssh://broken", "ssh://loki"})
	prev := dockerInfoFn
	dockerInfoFn = func(_ context.Context, host string) error {
		if host == "ssh://broken" {
			return fmt.Errorf("connection refused")
		}
		return nil
	}
	t.Cleanup(func() { dockerInfoFn = prev })

	res := CheckDockerHosts(context.Background(), repo)
	if res.Status != Fail {
		t.Errorf("Status = %v, want Fail when a host is unreachable", res.Status)
	}
	if !strings.Contains(res.Message, "ssh://broken") {
		t.Errorf("Message should name the unreachable host: %q", res.Message)
	}
	if strings.Contains(res.Message, "ssh://thor") || strings.Contains(res.Message, "ssh://loki") {
		t.Errorf("Message should not list reachable hosts: %q", res.Message)
	}
	if res.Fix == "" {
		t.Errorf("Fix should be populated with a recovery hint")
	}
}
