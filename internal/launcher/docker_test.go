package launcher

import (
	"strings"
	"testing"
)

// TestDockerInvocation_Minimal pins the smallest sensible invocation —
// just the required fields. Asserts the docker run shape, the worktree
// bind, the working-directory flag, and the trailing image+command.
func TestDockerInvocation_Minimal(t *testing.T) {
	got, err := dockerInvocation(Options{
		WorktreePath: "/tmp/work",
		SessionName:  "session-1",
		Command:      "claude",
		DockerImage:  "ghcr.io/example/agent:latest",
	})
	if err != nil {
		t.Fatalf("dockerInvocation: %v", err)
	}
	for _, want := range []string{
		"docker run --rm -it",
		"--name bosun-session-1",
		"-v /tmp/work:/work",
		"-w /work",
		"-e BOSUN_MCP_SOCK=/work/.bosun/mcp.sock",
		"ghcr.io/example/agent:latest",
		" claude",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("invocation missing %q\nfull:\n%s", want, got)
		}
	}
}

// TestDockerInvocation_RequiresImage refuses to build a pipeline when
// the image is unset — that catches operator misconfiguration before
// the terminal launcher would surface an opaque `docker run` failure.
func TestDockerInvocation_RequiresImage(t *testing.T) {
	if _, err := dockerInvocation(Options{
		WorktreePath: "/tmp/work",
		SessionName:  "session-1",
		Command:      "claude",
	}); err == nil {
		t.Errorf("dockerInvocation with empty image returned nil; want error")
	}
}

// TestDockerInvocation_RequiresWorktree mirrors the image check for
// the path used as the container's CWD bind mount.
func TestDockerInvocation_RequiresWorktree(t *testing.T) {
	if _, err := dockerInvocation(Options{
		SessionName: "session-1",
		Command:     "claude",
		DockerImage: "ghcr.io/example/agent:latest",
	}); err == nil {
		t.Errorf("dockerInvocation with empty worktree returned nil; want error")
	}
}

// TestDockerInvocation_BindsMCPSocket asserts BOSUN_MCP_SOCK in opts.Env
// is bind-mounted into the container at the rewritten path. Without
// this, MCP tools inside the container can't reach the host daemon.
func TestDockerInvocation_BindsMCPSocket(t *testing.T) {
	got, err := dockerInvocation(Options{
		WorktreePath: "/tmp/work",
		SessionName:  "session-1",
		Command:      "claude",
		DockerImage:  "img",
		Env: map[string]string{
			"BOSUN_MCP_SOCK": "/tmp/wt/.bosun/mcp.sock",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "-v /tmp/wt/.bosun/mcp.sock:/work/.bosun/mcp.sock") {
		t.Errorf("expected MCP socket bind mount, got:\n%s", got)
	}
}

// TestDockerInvocation_ForwardsBosunSession ensures the session label
// crosses the container boundary so the in-container agent's
// self-register / heartbeat / claim calls can identify themselves.
func TestDockerInvocation_ForwardsBosunSession(t *testing.T) {
	got, err := dockerInvocation(Options{
		WorktreePath: "/tmp/work",
		SessionName:  "session-1",
		Command:      "claude",
		DockerImage:  "img",
		Env: map[string]string{
			"BOSUN_SESSION": "session-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "-e BOSUN_SESSION=session-1") {
		t.Errorf("expected BOSUN_SESSION forwarded into container, got:\n%s", got)
	}
}

// TestDockerInvocation_SkipsBosunBin BOSUN_BIN references a host path
// that doesn't exist inside the container. The wrapper would fail to
// find it and the operator would see a confusing "bosun: command not
// found" inside the container.
func TestDockerInvocation_SkipsBosunBin(t *testing.T) {
	got, err := dockerInvocation(Options{
		WorktreePath: "/tmp/work",
		SessionName:  "session-1",
		Command:      "claude",
		DockerImage:  "img",
		Env: map[string]string{
			"BOSUN_BIN": "/Users/op/go/bin/bosun",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "BOSUN_BIN") {
		t.Errorf("BOSUN_BIN should be stripped from in-container env, got:\n%s", got)
	}
}

// TestDockerInvocation_ExtraMountsForwarded passes operator-configured
// mounts through verbatim. Useful for shared caches (Go module cache,
// node_modules, …) and credential dirs.
func TestDockerInvocation_ExtraMountsForwarded(t *testing.T) {
	got, err := dockerInvocation(Options{
		WorktreePath:      "/tmp/work",
		SessionName:       "session-1",
		Command:           "claude",
		DockerImage:       "img",
		DockerExtraMounts: []string{"/Users/op/.claude:/root/.claude", "/opt/cache:/cache"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"-v /Users/op/.claude:/root/.claude",
		"-v /opt/cache:/cache",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected extra mount %q, got:\n%s", want, got)
		}
	}
}

// TestDockerInvocation_EnvPassthroughByName forwards host env vars by
// name only — Docker resolves the value from its parent shell at
// container start time. Lets operators forward ANTHROPIC_API_KEY,
// OLLAMA_HOST, etc. without hardcoding the value in config.
func TestDockerInvocation_EnvPassthroughByName(t *testing.T) {
	got, err := dockerInvocation(Options{
		WorktreePath:         "/tmp/work",
		SessionName:          "session-1",
		Command:              "claude",
		DockerImage:          "img",
		DockerEnvPassthrough: []string{"ANTHROPIC_API_KEY", "OLLAMA_HOST"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// -e NAME (no =value) is the by-name form docker honors.
	for _, want := range []string{"-e ANTHROPIC_API_KEY", "-e OLLAMA_HOST"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected by-name env passthrough %q, got:\n%s", want, got)
		}
	}
}
