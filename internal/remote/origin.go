// Package remote bridges a local bosun host with a remote docker host
// over SSH. Two distinct concerns live here:
//
//   - origin.go: keeps a bare sibling repo at .bosun/remote/repo.git so
//     remote containers can `git clone` the session branch from the
//     bosun host without bind-mounting the worktree.
//   - sshtunnel.go: wraps `ssh -R` so the in-container agent's MCP
//     traffic reverses back to the local bosun MCP daemon's Unix socket.
//
// Both are foundational pieces of Phase 3 (multi-host docker). See
// docs/remote-docker-plan.md sections 2.3 and 2.4 for the design.
package remote

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// remoteOriginEnv is the operator escape hatch for NAT / firewall
// scenarios where the auto-derived `user@hostname:path` SSH URI won't
// reach the bosun host from inside the remote container. The operator
// sets BOSUN_REMOTE_ORIGIN to whatever URI is correct for their
// network (e.g. `ssh://op@public.example.com:/srv/bosun-repos/foo.git`)
// and bosun forwards it verbatim into the container as BOSUN_ORIGIN.
const remoteOriginEnv = "BOSUN_REMOTE_ORIGIN"

// barePathRel is the on-disk location of the bare sibling repo relative
// to the main worktree's repo root. Kept under .bosun/ so it's covered
// by the existing .gitignore rule and doesn't surface in `git status`.
var barePathRel = filepath.Join(".bosun", "remote", "repo.git")

// PreparePushable ensures the local repo is SSH-cloneable from the
// remote docker host. It maintains a bare sibling repo at
// <repoRoot>/.bosun/remote/repo.git and pushes branch into it.
//
// The returned sshURI is what the remote container should pass to
// `git clone` (e.g. `ssh://user@host:/abs/path/to/repo.git`).
//
// Auto-derivation: when the BOSUN_REMOTE_ORIGIN env var is set, that
// value is returned verbatim — operator escape hatch for NAT scenarios
// where `user@hostname` doesn't resolve from the remote container's
// network. Otherwise the URI is composed from `os.Getenv("USER")` +
// `os.Hostname()` + the absolute bare-repo path.
//
// Idempotent: a second call against the same repoRoot/branch reuses
// the existing bare repo and just re-pushes the branch (which is a
// no-op if HEAD hasn't moved).
//
// Failure modes:
//   - repoRoot doesn't exist or isn't a git repo → returns the git
//     error from `git init --bare` or `git push`.
//   - branch doesn't exist locally → git push fails; the wrapped
//     error includes the branch name so operators can spot the typo.
func PreparePushable(repoRoot, branch string) (string, error) {
	if repoRoot == "" {
		return "", fmt.Errorf("remote: PreparePushable: repoRoot is required")
	}
	if branch == "" {
		return "", fmt.Errorf("remote: PreparePushable: branch is required")
	}

	barePath := filepath.Join(repoRoot, barePathRel)

	// Create the bare repo if missing. `git init --bare` is itself
	// idempotent (running it twice on an existing bare repo just
	// re-prints the "Reinitialized existing Git repository" banner
	// and exits 0), but the stat-first short-circuit avoids the
	// noise and is one fewer subprocess on the hot path.
	if _, err := os.Stat(barePath); os.IsNotExist(err) {
		if mkErr := os.MkdirAll(filepath.Dir(barePath), 0o755); mkErr != nil {
			return "", fmt.Errorf("remote: create bare repo parent dir: %w", mkErr)
		}
		if out, gErr := runGit(repoRoot, "init", "--bare", barePath); gErr != nil {
			return "", fmt.Errorf("remote: git init --bare %s: %w\n%s", barePath, gErr, out)
		}
	} else if err != nil {
		return "", fmt.Errorf("remote: stat bare repo: %w", err)
	}

	// Push the branch from the working repo into the bare sibling.
	// `--force` is intentional: the bare repo is bosun-owned scratch
	// space and the local working repo is authoritative. Without
	// --force a non-fast-forward push (e.g. after `git commit --amend`
	// on the local side) would refuse and confuse the operator.
	if out, gErr := runGit(repoRoot, "push", "--force", barePath, branch+":"+branch); gErr != nil {
		return "", fmt.Errorf("remote: push %s to bare sibling: %w\n%s", branch, gErr, out)
	}

	return composeSSHURI(barePath)
}

// composeSSHURI returns the ssh:// URI the in-container `git clone`
// should target. Honours BOSUN_REMOTE_ORIGIN if set; otherwise builds
// one from os.Getenv("USER") + os.Hostname() + absolute bare path.
//
// Split out from PreparePushable so the env-override path is unit-
// testable without spinning up a real git repo.
func composeSSHURI(barePath string) (string, error) {
	if override := strings.TrimSpace(os.Getenv(remoteOriginEnv)); override != "" {
		return override, nil
	}
	user := os.Getenv("USER")
	if user == "" {
		// $USER is universal on POSIX shells but not guaranteed in
		// every spawn context (cron, systemd, …). Fall back to "" —
		// `ssh ://hostname:path` is still a parseable URI and the
		// operator can override via ~/.ssh/config.
		user = ""
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("remote: os.Hostname: %w", err)
	}
	abs, err := filepath.Abs(barePath)
	if err != nil {
		return "", fmt.Errorf("remote: abs bare path: %w", err)
	}
	if user == "" {
		return fmt.Sprintf("ssh://%s:%s", host, abs), nil
	}
	return fmt.Sprintf("ssh://%s@%s:%s", user, host, abs), nil
}

// runGit invokes `git <args...>` with cwd set to dir and returns the
// combined stdout/stderr. Keeps origin.go self-contained — we don't
// pull in internal/git here because PreparePushable is conceptually a
// remote-package primitive (its job is to expose a URI), not a git
// wrapper. A small, focused helper avoids the cross-package coupling.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
