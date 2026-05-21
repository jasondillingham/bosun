package mcp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	registerTool(func(s *Server) {
		mcp.AddTool(s.mcp, &mcp.Tool{
			Name: "bosun_attach",
			Description: "Register an explicit liveness PID for a bosun session. " +
				"In-container equivalent of `bosun attach <session> --pid <pid>` — used " +
				"by wrapper scripts that can't shell out to the bosun binary (the binary " +
				"isn't installed inside containers). Writes " +
				".bosun/state/<session>.attached-pid so the liveness gate recognizes " +
				"workers the proc-scan can't see (sub-agents launched via Task, CI " +
				"runners, containerized workers). Refuses unknown sessions and " +
				"non-positive PIDs.",
		}, s.toolAttach)
	})
}

// AttachArgs is the input schema for bosun_attach.
type AttachArgs struct {
	Session string `json:"session" jsonschema:"the bosun session to register the PID against (e.g. session-2 or 2)"`
	PID     int    `json:"pid" jsonschema:"PID of the worker process to register as the live agent for the session — must be a positive integer"`
}

// AttachResult is the structured output for bosun_attach.
type AttachResult struct {
	Session string `json:"session" jsonschema:"canonical session label the PID was registered against"`
	PID     int    `json:"pid" jsonschema:"PID recorded in the attached-pid file"`
}

// toolAttach implements bosun_attach. Validates the session label, refuses
// labels that don't match a bosun-managed session (re-derived live so a
// typo can't create an orphan state file), refuses non-positive PIDs, then
// writes the attached-pid via state.Store.WriteAttachedPID — byte-identical
// to what `bosun attach --pid N` writes from the CLI.
//
// Heartbeat-clear semantics live elsewhere; this tool only writes.
func (s *Server) toolAttach(ctx context.Context, _ *mcp.CallToolRequest, args AttachArgs) (*mcp.CallToolResult, AttachResult, error) {
	raw := strings.TrimSpace(args.Session)
	if raw == "" {
		return errResult(errors.New("session is required")), AttachResult{}, nil
	}
	label, err := session.ParseLabel(raw)
	if err != nil {
		return errResult(err), AttachResult{}, nil
	}
	if args.PID <= 0 {
		return errResult(fmt.Errorf("pid must be a positive integer, got %d", args.PID)), AttachResult{}, nil
	}
	// v0.12 L2: PID 1 is init/launchd on every supported platform —
	// never a real bosun worker, and the exemplar from the security
	// audit (an agent registering PID 1 as its liveness PID gets a
	// permanently-alive false-positive because proc.IsAlive can't
	// disprove PID 1). Refuse explicitly. Higher reserved PIDs vary by
	// platform (kernel threads on Linux, kernel_task at 0 on macOS) so
	// we don't draw a broader floor — the cwd check below catches the
	// rest where the platform supports it.
	if args.PID == 1 {
		return errResult(errors.New("pid 1 is init/launchd, not a bosun worker — refuse")), AttachResult{}, nil
	}

	// Re-validate the session against the live worktree set — the same
	// "no orphan state files" gate cmd_attach.go uses. Without this gate
	// a typo against `bosun_attach session-99` would silently create
	// .bosun/state/session-99.attached-pid that session.Derive will
	// ignore but operators would have to clean up by hand.
	repoRoot := s.state.RepoRoot()
	cfg, err := config.Load(repoRoot)
	if err != nil {
		return errResult(fmt.Errorf("load config: %w", err)), AttachResult{}, nil
	}
	if s.gitClient == nil {
		// Without a git client we can't Derive — refuse rather than
		// silently skipping the auth gate. Production wiring
		// (cmd_mcp.go) always passes a git client; in-process tests
		// that exercise bosun_attach must do the same.
		return errResult(errors.New("bosun_attach requires a git client to validate the session")), AttachResult{}, nil
	}
	sess, err := findSessionByLabel(ctx, s, cfg, repoRoot, label)
	if err != nil {
		return errResult(err), AttachResult{}, nil
	}
	if sess == nil {
		return errResult(fmt.Errorf("%s not found", label)), AttachResult{}, nil
	}

	// v0.12 L2: best-effort cwd validation. If the platform supports
	// reading a process's working directory (Linux today, via
	// /proc/<pid>/cwd), confirm the registered PID is actually running
	// inside the session's worktree. ErrCwdUnsupported (macOS,
	// Windows) is a soft signal — degrade to writing the attached-pid
	// without the cwd check rather than refusing on every non-Linux
	// host. Any other error (PID not alive, permission denied) is also
	// soft — the IsAlive check at liveness-gate time will catch a dead
	// PID; a permission failure says we lack visibility, not that the
	// PID is wrong.
	if s.pidCwdFn != nil {
		if cwd, err := s.pidCwdFn(args.PID); err == nil {
			if !cwdInsideWorktree(cwd, sess.Path) {
				return errResult(fmt.Errorf("pid %d cwd %q is not inside session worktree %q; bosun_attach refuses cross-worktree PID registration", args.PID, cwd, sess.Path)), AttachResult{}, nil
			}
		}
		// errors (including ErrCwdUnsupported) intentionally swallowed
		// — the validation is best-effort by design.
	}

	if err := s.state.WriteAttachedPID(label, args.PID); err != nil {
		return errResult(fmt.Errorf("write attached-pid: %w", err)), AttachResult{}, nil
	}
	summary := fmt.Sprintf("%s attached pid=%d", label, args.PID)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: summary}},
	}, AttachResult{Session: label, PID: args.PID}, nil
}

// cwdInsideWorktree reports whether cwd is at or under worktreePath.
// Both inputs go through filepath.Clean so trailing slashes and
// redundant separators don't break the comparison. EvalSymlinks is
// best-effort — on macOS t.TempDir() roots like /var/folders/...
// symlink to /private/var/folders/... and the kernel-reported cwd
// resolves through the symlink; trying both sides keeps the
// comparison robust without making symlink failures fatal.
func cwdInsideWorktree(cwd, worktreePath string) bool {
	candidates := func(path string) []string {
		cleaned := filepath.Clean(path)
		out := []string{cleaned}
		if resolved, err := filepath.EvalSymlinks(cleaned); err == nil && resolved != cleaned {
			out = append(out, resolved)
		}
		return out
	}
	cwds := candidates(cwd)
	worktrees := candidates(worktreePath)
	for _, c := range cwds {
		for _, w := range worktrees {
			if c == w {
				return true
			}
			// Descendant check: append a separator to w so
			// "/work/session-1" doesn't false-match "/work/session-10".
			if strings.HasPrefix(c, w+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}
