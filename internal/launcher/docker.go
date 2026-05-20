package launcher

import (
	"fmt"
	"strings"
)

// dockerInvocation returns a shell-ready `docker run` pipeline that wraps
// opts.Command. The composed string is what the terminal launcher
// ultimately runs inside its `bash -lc` shell (via shellInvocation).
//
// Mounts:
//
//   - opts.WorktreePath → /work (the agent's CWD)
//   - $BOSUN_MCP_SOCK → /work/.bosun/mcp.sock (only when set; Unix socket
//     works across bind mount on every supported host)
//   - any opts.DockerExtraMounts entries, verbatim
//
// Env passthrough:
//
//   - BOSUN_SESSION + BOSUN_BIN are skipped — they reference host paths
//     (BOSUN_BIN especially) that don't exist inside the container.
//   - BOSUN_MCP_SOCK is rewritten to /work/.bosun/mcp.sock to match the
//     bind mount destination.
//   - Each opts.DockerEnvPassthrough name is forwarded with `-e NAME`
//     (Docker reads the value from the host environment at run time).
//   - Other opts.Env values come through as `-e KEY=VALUE` literals.
//
// Container name: bosun-<session-label>. Operators or `bosun cleanup`
// can target it with `docker stop bosun-<label>` if the host docker CLI
// is unreachable.
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

	args := []string{
		"docker", "run", "--rm", "-it",
		"--name", "bosun-" + opts.SessionName,
		"-v", opts.WorktreePath + ":/work",
		"-w", "/work",
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
	if sock, ok := opts.Env["BOSUN_MCP_SOCK"]; ok && sock != "" {
		args = append(args, "-v", sock+":/work/.bosun/mcp.sock")
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
		args = append(args, "-e", k+"="+v)
	}

	args = append(args, opts.DockerImage)

	// The bare command (e.g. "claude") goes after the image. We don't
	// add the initial prompt here — that's handled by the terminal
	// launcher's shellInvocation, which appends it after the full
	// shell pipeline.
	if opts.Command != "" {
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
