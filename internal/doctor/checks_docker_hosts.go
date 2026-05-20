package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jasondillingham/bosun/internal/config"
)

// dockerHostsCheckTimeout caps `docker -H <host> info` per host so a
// single unreachable endpoint doesn't make `bosun doctor` hang for
// the operator. 5 seconds is generous for an SSH-bridged docker
// daemon on a fast LAN; an unreachable host fails noticeably faster
// than that on ECONNREFUSED.
const dockerHostsCheckTimeout = 5 * time.Second

// dockerInfoFn is the test seam for the docker info call.
// Production uses exec.Command via dockerInfoExec; tests substitute
// to assert on argv and simulate unreachable hosts without spawning
// real docker subprocesses.
var dockerInfoFn = dockerInfoExec

func dockerInfoExec(ctx context.Context, host string) error {
	cmd := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+host)
	return cmd.Run()
}

// CheckDockerHosts verifies that every endpoint configured in
// config.docker.hosts is reachable via `docker info`. Skipped when
// docker.hosts is empty (no remote hosts to check).
//
// 2026-05 follow-up grind #100. Surfaces unreachable hosts at
// doctor time so `bosun init --launch` doesn't half-create
// worktrees and then fail at terminal-spawn time on an SSH-bridge
// that's been down since this morning.
//
// The check uses the same `docker -H ssh://...` transport bosun's
// own remote-docker launcher uses, so a host that fails here will
// also fail launch. The reverse isn't guaranteed (SSH agent state,
// transient network issues), but a passing doctor result is a
// strong "you can probably init" signal.
func CheckDockerHosts(ctx context.Context, repoRoot string) Result {
	cfg, err := config.Load(repoRoot)
	if err != nil {
		return Result{
			Name:    "docker-hosts",
			Status:  Fail,
			Message: fmt.Sprintf("load config: %v", err),
		}
	}
	if len(cfg.Docker.Hosts) == 0 {
		return Result{
			Name:    "docker-hosts",
			Status:  Pass,
			Message: "no remote hosts configured (config.docker.hosts is empty)",
		}
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return Result{
			Name:    "docker-hosts",
			Status:  Fail,
			Message: "docker binary not on PATH but config.docker.hosts is non-empty",
			Fix:     "install Docker Desktop (mac/win) or docker-ce (linux), or clear config.docker.hosts",
		}
	}

	var unreachable []string
	for _, host := range cfg.Docker.Hosts {
		probeCtx, cancel := context.WithTimeout(ctx, dockerHostsCheckTimeout)
		err := dockerInfoFn(probeCtx, host)
		cancel()
		if err != nil {
			unreachable = append(unreachable, host)
		}
	}
	if len(unreachable) == 0 {
		return Result{
			Name:    "docker-hosts",
			Status:  Pass,
			Message: fmt.Sprintf("all %d configured docker host(s) reachable", len(cfg.Docker.Hosts)),
		}
	}
	return Result{
		Name:   "docker-hosts",
		Status: Fail,
		Message: fmt.Sprintf("%d of %d docker host(s) unreachable: %s",
			len(unreachable), len(cfg.Docker.Hosts), strings.Join(unreachable, ", ")),
		Fix: "verify SSH connectivity (ssh <user>@<host> 'docker info'), check the remote daemon is running, or remove the unreachable host from config.docker.hosts",
	}
}
