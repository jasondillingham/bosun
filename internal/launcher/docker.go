package launcher

import (
	"fmt"
	"strings"
)

// isRemoteSSHDockerHost reports whether the resolved DOCKER_HOST points
// at an SSH-bridged daemon. Phase 3 lane 2+3 only rewrites for the SSH
// case — tcp:// + unix:// hosts (including local) keep the bind-mount
// path.
func isRemoteSSHDockerHost(opts Options) bool {
	if h, ok := opts.Env["DOCKER_HOST"]; ok && strings.HasPrefix(h, "ssh://") {
		return true
	}
	return false
}

// dockerInvocation returns a shell-ready `docker run` pipeline that wraps
// opts.Command. The composed string is what the terminal launcher
// ultimately runs inside its `bash -lc` shell (via shellInvocation).
//
// Local-daemon mode (DOCKER_HOST unset or non-ssh):
//
//   - opts.WorktreePath → /work (the agent's CWD)
//   - $BOSUN_MCP_SOCK → /work/.bosun/mcp.sock (only when set; Unix socket
//     works across bind mount on every supported host)
//   - any opts.DockerExtraMounts entries, verbatim
//
// Remote-daemon mode (DOCKER_HOST=ssh://…, Phase 3 lane 2+3):
//
//   - The worktree bind-mount is dropped — the remote docker daemon
//     can't see the local filesystem. Instead, the container's startup
//     command runs `git clone $BOSUN_ORIGIN /work && git checkout
//     $BOSUN_BRANCH && exec <opts.Command>`. BOSUN_ORIGIN is the SSH
//     URI for the bosun host's bare sibling repo (set by cmd_init via
//     remote.PreparePushable); BOSUN_BRANCH is the session branch.
//   - The MCP socket bind is replaced by a reverse-proxied path —
//     cmd_init has already opened an `ssh -R` tunnel that exposes the
//     local MCP socket inside the remote container at
//     /work/.bosun/mcp.sock. BOSUN_MCP_SOCK is still set to that path
//     so the in-container agent code path stays uniform.
//
// Env passthrough (both modes):
//
//   - BOSUN_SESSION + BOSUN_BIN are skipped — they reference host paths
//     (BOSUN_BIN especially) that don't exist inside the container.
//   - BOSUN_MCP_SOCK is rewritten to /work/.bosun/mcp.sock to match the
//     bind mount / tunnel destination.
//   - Each opts.DockerEnvPassthrough name is forwarded with `-e NAME`
//     (Docker reads the value from the host environment at run time).
//   - Other opts.Env values come through as `-e KEY=VALUE` literals.
//
// Container name: bosun-<session-label>. Operators or `bosun cleanup`
// can target it with `docker stop bosun-<label>` if the host docker CLI
// is unreachable. For remote hosts cleanup must set DOCKER_HOST first.
//
// Returns an error only on truly malformed input — the validated
// session label, image, and worktree path are all expected to be safe.
func dockerInvocation(opts Options) (string, error) {
	if opts.DockerImage == "" {
		return "", fmt.Errorf("DockerImage is required")
	}
	if opts.WorktreePath == "" {
		return "", fmt.Errorf("WorktreePath is required")
	}

	remote := isRemoteSSHDockerHost(opts)

	args := []string{
		"docker", "run", "--rm", "-it",
		"--name", "bosun-" + opts.SessionName,
		"-w", "/work",
	}

	// Local-daemon: bind the worktree directly. Remote-daemon: skip
	// the bind (remote docker can't see local fs) and rely on the
	// container's startup git clone instead.
	if !remote {
		args = append(args[:len(args):len(args)],
			"-v", opts.WorktreePath+":/work",
		)
	}

	for _, m := range opts.DockerExtraMounts {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		args = append(args, "-v", m)
	}

	// MCP socket bind. The host's BOSUN_MCP_SOCK is set by bosun's
	// launcher when the daemon is up; rewrite to the container path
	// the agent expects.
	//
	// In remote mode the socket reaches the container via an ssh -R
	// reverse forward (opened by cmd_init), so no -v bind is needed
	// — the path inside the container exists thanks to the tunnel.
	if !remote {
		if sock, ok := opts.Env["BOSUN_MCP_SOCK"]; ok && sock != "" {
			args = append(args, "-v", sock+":/work/.bosun/mcp.sock")
		}
	}

	// Forward useful in-container env.
	args = append(args, "-e", "BOSUN_MCP_SOCK=/work/.bosun/mcp.sock")
	if label, ok := opts.Env["BOSUN_SESSION"]; ok && label != "" {
		args = append(args, "-e", "BOSUN_SESSION="+label)
	}

	// Pass through operator-named host env vars by name only (docker
	// resolves the value from its parent shell at run time).
	for _, name := range opts.DockerEnvPassthrough {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		args = append(args, "-e", name)
	}

	// Forward any remaining opts.Env literals — except the ones we
	// already handled and the host-only BOSUN_BIN (which references a
	// path that doesn't exist inside the container).
	for k, v := range opts.Env {
		switch k {
		case "BOSUN_MCP_SOCK", "BOSUN_SESSION", "BOSUN_BIN":
			continue
		}
		// DOCKER_HOST itself MUST stay on the host side of the
		// docker CLI invocation — it tells the local `docker run`
		// where to send the request. Don't forward it into the
		// container, where it would mis-point any in-container
		// docker CLI at an unreachable ssh:// URL.
		if k == "DOCKER_HOST" {
			continue
		}
		args = append(args, "-e", k+"="+v)
	}

	args = append(args, opts.DockerImage)

	// In remote mode, replace the bare command with a startup shell
	// that clones the session branch from BOSUN_ORIGIN, then execs
	// the agent. The clone replaces the bind mount that would
	// normally seed /work from the host.
	//
	// `exec` at the end is intentional: it makes the agent the
	// container's PID 1 (or close enough), so signals from
	// `docker stop bosun-<label>` propagate cleanly and the agent
	// can shut down gracefully.
	//
	// `sh -lc` because the bosun reference Dockerfile uses a slim
	// image where bash isn't always present. /bin/sh is universal.
	if remote {
		startup := buildRemoteStartup(opts.Command)
		args = append(args, "sh", "-lc", startup)
	} else if opts.Command != "" {
		// The bare command (e.g. "claude") goes after the image. We
		// don't add the initial prompt here — that's handled by the
		// terminal launcher's shellInvocation, which appends it
		// after the full shell pipeline.
		args = append(args, opts.Command)
	}

	// Quote only the argv elements that need it. Flags, image names,
	// session labels stay readable in the pipeline that bosun prints
	// on launcher fallback; only paths with spaces or special chars
	// get wrapped in single quotes.
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, shellQuoteIfNeeded(a))
	}
	return strings.Join(out, " "), nil
}

// buildRemoteStartup composes the in-container shell command that
// clones the session branch from BOSUN_ORIGIN, checks it out, and
// execs the agent. Split out from dockerInvocation so the shape is
// independently unit-testable.
//
// The clone target is always /work because that's the WORKDIR the
// docker invocation set. We pre-create the parent dir and let git
// clone the bare repo into a fresh empty /work — if the image's
// base layer already has /work as a non-empty dir, the operator
// needs a different image (which is a sensible failure mode for a
// remote-clone workflow).
func buildRemoteStartup(command string) string {
	if command == "" {
		command = "claude"
	}
	// Each step is sequenced with && so the first failure aborts
	// the container with a non-zero exit — the operator sees the
	// `docker run` failure rather than an unexplained shell that
	// exec'd into an empty /work.
	//
	// rm -rf /work/* clears any image-baked content (the reference
	// agent image bind-mounts /work but is fine being emptied
	// because it owns no required state there).
	return `set -e; ` +
		`if [ -z "$BOSUN_ORIGIN" ]; then echo "bosun: BOSUN_ORIGIN unset" >&2; exit 1; fi; ` +
		`if [ -z "$BOSUN_BRANCH" ]; then echo "bosun: BOSUN_BRANCH unset" >&2; exit 1; fi; ` +
		`mkdir -p /work && rm -rf /work/* /work/.[!.]* 2>/dev/null || true; ` +
		`git clone --branch "$BOSUN_BRANCH" "$BOSUN_ORIGIN" /work && ` +
		`cd /work && ` +
		`exec ` + command
}
