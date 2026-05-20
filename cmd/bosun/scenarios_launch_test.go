package main

// Scenarios for the standalone `bosun launch` initial-prompt resolution.
//
// `bosun init --launch --brief X` already defaults the initial prompt to
// "Read BOSUN_BRIEF.md ..." when the operator doesn't override it. Plain
// `bosun launch session-N` (run later, e.g. to reopen a closed window)
// must match: when BOSUN_BRIEF.md exists in the worktree and no
// --initial-prompt was given, the same default fires. These scenarios
// drive `bosun launch` end-to-end with `launcher=print` so the rendered
// shell command lands in captured output where we can grep it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScenario_LaunchDefaultsToBriefPromptWhenBriefExists(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)
	s.WriteFile("plan.md", "## session-1\nrefactor things\n")
	s.Bosun("init", "1", "--brief", "plan.md")

	// Sanity check: init wrote a brief into the worktree.
	if _, err := os.Stat(filepath.Join(s.WorktreePath(1), "BOSUN_BRIEF.md")); err != nil {
		t.Fatalf("BOSUN_BRIEF.md not present in session-1 worktree: %v", err)
	}

	out := s.Bosun("launch", "session-1")
	s.AssertContainsAll(out, "Launched session-1", "Read BOSUN_BRIEF.md")
}

func TestScenario_LaunchLeavesPromptEmptyWhenNoBrief(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)
	s.Bosun("init", "1")

	// Sanity check: no brief was written, since --brief wasn't passed.
	if _, err := os.Stat(filepath.Join(s.WorktreePath(1), "BOSUN_BRIEF.md")); !os.IsNotExist(err) {
		t.Fatalf("BOSUN_BRIEF.md unexpectedly present (or stat err): %v", err)
	}

	out := s.Bosun("launch", "session-1")
	if strings.Contains(out, "Read BOSUN_BRIEF.md") {
		t.Fatalf("launch without a brief should not inject the default prompt:\n%s", out)
	}
	// The print launcher renders the prompt (when set) as a shell-quoted
	// argument right after `claude`. With no prompt, the line ends at
	// `claude\n`. Anything like `claude '...` would mean a default leaked.
	if strings.Contains(out, "claude '") {
		t.Fatalf("printed command should not include a quoted prompt arg:\n%s", out)
	}
}

func TestScenario_LaunchExplicitPromptBeatsBriefDefault(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)
	s.WriteFile("plan.md", "## session-1\nrefactor things\n")
	s.Bosun("init", "1", "--brief", "plan.md")

	out := s.Bosun("launch", "session-1", "--initial-prompt", "custom kickoff")
	s.AssertContains(out, "'custom kickoff'")
	if strings.Contains(out, "Read BOSUN_BRIEF.md") {
		t.Fatalf("explicit --initial-prompt must override the default:\n%s", out)
	}
}

// TestScenario_InitDockerHostFromBriefPerSession is the round-trip
// scenario for Phase 3 lane 1: two sessions with different per-session
// (host: …) brief clauses must each land DOCKER_HOST=<their endpoint>
// in the launcher's printed env. The print launcher renders env vars
// as `KEY='value' command ...` so the captured stdout is the assertion
// surface — we grep for the per-session value alongside the session
// label.
func TestScenario_InitDockerHostFromBriefPerSession(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)
	plan := "## session-1 (host: ssh://thor)\nremote one\n\n## session-2 (host: ssh://docker-server)\nremote two\n"
	s.WriteFile("plan.md", plan)

	out := s.Bosun("init", "2", "--brief", "plan.md", "--launch")
	// Both sessions should have their host substituted into the launcher
	// env prefix. The print fallback's `bosun: run <session>:` header
	// makes per-session attribution unambiguous.
	if !strings.Contains(out, "DOCKER_HOST='ssh://thor'") {
		t.Errorf("expected DOCKER_HOST=ssh://thor in launch output:\n%s", out)
	}
	if !strings.Contains(out, "DOCKER_HOST='ssh://docker-server'") {
		t.Errorf("expected DOCKER_HOST=ssh://docker-server in launch output:\n%s", out)
	}
}

// TestScenario_InitDockerHostFromCLIFlag pins the second tier of the
// precedence ladder: a `--docker-host` CLI flag applies to every
// session in the round when no brief clause shadows it.
func TestScenario_InitDockerHostFromCLIFlag(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)

	out := s.Bosun("init", "2", "--launch", "--docker-host", "ssh://thor")
	// Both sessions land the same host (the flag is round-wide).
	hostCount := strings.Count(out, "DOCKER_HOST='ssh://thor'")
	if hostCount < 2 {
		t.Errorf("expected DOCKER_HOST set on both sessions, got %d hits:\n%s", hostCount, out)
	}
}

// TestScenario_InitDockerHostBriefBeatsFlag locks in the third
// invariant: the brief clause wins when both are set. session-1
// declares (host: ssh://thor) so it stays on thor even with the
// --docker-host=ssh://docker-server flag in play; session-2 has no
// brief clause and so falls back to the flag.
func TestScenario_InitDockerHostBriefBeatsFlag(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)
	plan := "## session-1 (host: ssh://thor)\nstays on thor\n\n## session-2\nfalls back to flag\n"
	s.WriteFile("plan.md", plan)

	out := s.Bosun("init", "2", "--brief", "plan.md", "--launch", "--docker-host", "ssh://docker-server")
	if !strings.Contains(out, "DOCKER_HOST='ssh://thor'") {
		t.Errorf("session-1 brief clause should win over --docker-host flag:\n%s", out)
	}
	if !strings.Contains(out, "DOCKER_HOST='ssh://docker-server'") {
		t.Errorf("session-2 should fall back to --docker-host flag:\n%s", out)
	}
}

// TestScenario_InitDockerHostFromConfigHosts0 pins the fourth tier:
// with neither a brief clause nor a CLI flag, the first entry of
// config.docker.hosts is used as the round-wide default. Mirrors how
// agent_command's resolution falls back to config.AgentCommand.
func TestScenario_InitDockerHostFromConfigHosts0(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print","docker":{"hosts":["ssh://thor","ssh://docker-server"]}}`)

	out := s.Bosun("init", "1", "--launch")
	if !strings.Contains(out, "DOCKER_HOST='ssh://thor'") {
		t.Errorf("expected DOCKER_HOST defaulted to config.docker.hosts[0] (ssh://thor):\n%s", out)
	}
	// Sanity: hosts[1] must NOT appear — lane 1 deliberately doesn't do
	// load-balancing; that's deferred to lane 5+. Without explicit
	// per-session selection, every session gets hosts[0].
	if strings.Contains(out, "DOCKER_HOST='ssh://docker-server'") {
		t.Errorf("only hosts[0] should be used in lane 1; hosts[1] leaked:\n%s", out)
	}
}

// TestScenario_InitNoDockerHostMeansLocal pins the no-op case: without
// any host source configured, DOCKER_HOST does not appear in the
// launched env — today's local-docker behavior is preserved.
func TestScenario_InitNoDockerHostMeansLocal(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"launcher":"print"}`)

	out := s.Bosun("init", "1", "--launch")
	if strings.Contains(out, "DOCKER_HOST=") {
		t.Errorf("DOCKER_HOST should not appear when no host is configured:\n%s", out)
	}
}

// TestScenario_ConfigSetDockerHostsRejected mirrors the hooks-list
// rejection: docker.hosts is a slice that can't be sensibly typed on
// the scalar `config set` line, so the command must refuse and point
// the operator at the JSON file. Without this, an operator typing
// `config set docker.hosts '["ssh://thor"]'` would silently land a
// string-shaped value where the loader expects a list.
func TestScenario_ConfigSetDockerHostsRejected(t *testing.T) {
	s := newScenario(t)

	out, err := s.BosunErr("config", "set", "docker.hosts", `["ssh://thor"]`)
	if err == nil {
		t.Fatalf("set docker.hosts should fail:\n%s", out)
	}
	if !strings.Contains(out, "docker.hosts") {
		t.Errorf("error should mention docker.hosts: %s", out)
	}
	if !strings.Contains(out, "list") {
		t.Errorf("error should explain it's a list: %s", out)
	}
}

// TestScenario_ConfigListShowsDockerHosts pins `config list` rendering
// of the docker.hosts read-only entry. With hosts configured, the
// value lands as `[ssh://thor, ssh://docker-server]` and the (default)
// marker is dropped. Without hosts, the entry is `[]` and marked
// default. Mirrors the hooks-list contract.
func TestScenario_ConfigListShowsDockerHosts(t *testing.T) {
	s := newScenario(t)

	// Default first: no file, no hosts, line marked default.
	out := s.Bosun("config", "list")
	var hostsLine string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "docker.hosts:") {
			hostsLine = line
			break
		}
	}
	if hostsLine == "" {
		t.Fatalf("config list missing docker.hosts line:\n%s", out)
	}
	if !strings.Contains(hostsLine, "[]") {
		t.Errorf("docker.hosts default line should render as []: %q", hostsLine)
	}
	if !strings.Contains(hostsLine, "(default)") {
		t.Errorf("docker.hosts line should carry (default) marker when unconfigured: %q", hostsLine)
	}

	// Override the file (config set rejects the key per
	// TestScenario_ConfigSetDockerHostsRejected, so hand-write the
	// JSON the way operators would).
	s.WriteFile(".bosun/config.json", `{"docker":{"hosts":["ssh://thor","ssh://docker-server"]}}`)
	out = s.Bosun("config", "list")
	hostsLine = ""
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "docker.hosts:") {
			hostsLine = line
			break
		}
	}
	if hostsLine == "" {
		t.Fatalf("config list missing docker.hosts line after override:\n%s", out)
	}
	if !strings.Contains(hostsLine, "ssh://thor") || !strings.Contains(hostsLine, "ssh://docker-server") {
		t.Errorf("docker.hosts line should include both endpoints: %q", hostsLine)
	}
	if strings.Contains(hostsLine, "(default)") {
		t.Errorf("docker.hosts line should not carry (default) when configured: %q", hostsLine)
	}
}

// TestScenario_ConfigValidateFailsOnBadDockerHost extends the
// existing validate-failure scenarios to cover the new host-list
// rule: a unix:// scheme is invalid because DOCKER_HOST=unix://...
// makes no sense for a remote-targeting feature, and an unparseable
// URL must surface at validate rather than at first launch.
func TestScenario_ConfigValidateFailsOnBadDockerHost(t *testing.T) {
	s := newScenario(t)
	s.WriteFile(".bosun/config.json", `{"docker":{"hosts":["unix:///var/run/docker.sock"]}}`)
	out, err := s.BosunErr("config", "validate")
	if err == nil {
		t.Fatalf("expected validate to fail on unix:// docker host, output:\n%s", out)
	}
	if !strings.Contains(out, "docker.hosts") {
		t.Errorf("error should mention docker.hosts: %s", out)
	}
}
