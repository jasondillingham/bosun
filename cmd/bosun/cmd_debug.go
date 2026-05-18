package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jasondillingham/bosun/internal/debug"
	"github.com/jasondillingham/bosun/internal/doctor"
	"github.com/spf13/cobra"
)

// debugIncludeKnown is the set of values valid for `bosun debug --include`.
// "audit" pulls full audit-log contents; "all" turns on every full-content
// expansion (audit + state + claims). Anything else is a user error.
var debugIncludeKnown = map[string]bool{
	"audit": true,
	"all":   true,
}

func newDebugCmd() *cobra.Command {
	var (
		outPath  string
		noRedact bool
		includes []string
	)
	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Produce a self-contained issue-report bundle",
		Long: `Debug gathers everything a maintainer needs to triage a bug report
into a single plain-text bundle: version, doctor output, git state,
config (redacted), recent audit/state/claims/merges summaries, and
OS + git version. The last section is a checklist the operator
should run through before sharing the file.

Examples:
  bosun debug > debug-report.txt
  bosun debug --out debug-report.txt
  bosun debug --no-redact            # disable secret redaction in config
  bosun debug --include audit        # include full audit log contents
  bosun debug --include all          # everything: audit + state + claims`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			includeSet := map[string]bool{}
			for _, v := range includes {
				v = strings.TrimSpace(strings.ToLower(v))
				if v == "" {
					continue
				}
				if !debugIncludeKnown[v] {
					return userErr("unknown --include value %q (known: audit, all)", v)
				}
				includeSet[v] = true
			}

			rc, err := loadCtx()
			if err != nil {
				return err
			}

			var w io.Writer = os.Stdout
			if outPath != "" {
				f, err := os.Create(outPath)
				if err != nil {
					return userErr("create %s: %v", outPath, err)
				}
				defer f.Close()
				w = f
			}

			opts := debugOptions{
				redact:   !noRedact,
				includes: includeSet,
			}
			writeDebugBundle(w, rc, opts, time.Now().UTC())
			return nil
		},
	}
	cmd.Flags().StringVar(&outPath, "out", "", "write bundle to PATH instead of stdout")
	cmd.Flags().BoolVar(&noRedact, "no-redact", false, "disable secret redaction in config.json")
	cmd.Flags().StringSliceVar(&includes, "include", nil, "include full content for: audit, all")
	cmd.GroupID = "wiring"
	return cmd
}

// debugOptions controls what writeDebugBundle expands and what it
// summarizes. redact governs config.json (and audit-log content when
// pulled in via --include audit). includes is the set of expansion
// flags ("audit", "all").
type debugOptions struct {
	redact   bool
	includes map[string]bool
}

func (o debugOptions) includeAudit() bool  { return o.includes["audit"] || o.includes["all"] }
func (o debugOptions) includeState() bool  { return o.includes["all"] }
func (o debugOptions) includeClaims() bool { return o.includes["all"] }

// writeDebugBundle is the bundle's section dispatcher. Each section is
// independently fallible — a missing or unreadable artifact prints an
// inline "(unavailable: <reason>)" instead of aborting the bundle, so
// the maintainer still gets the parts that did work.
func writeDebugBundle(w io.Writer, rc *runCtx, opts debugOptions, now time.Time) {
	bw := bufio.NewWriter(w)
	defer func() { _ = bw.Flush() }()

	writeBanner(bw, fmt.Sprintf("BOSUN DEBUG REPORT — %s", now.Format("2006-01-02 15:04:05 UTC")))
	fmt.Fprintf(bw, "repo: %s\n", rc.repoRoot)
	if opts.redact {
		_, _ = fmt.Fprintln(bw, "redaction: ON (re-run with --no-redact to disable)")
	} else {
		_, _ = fmt.Fprintln(bw, "redaction: OFF (--no-redact)")
	}
	_, _ = fmt.Fprintln(bw)

	writeSection(bw, "bosun --version", func(w io.Writer) {
		fmt.Fprintf(w, "%s\n", version)
	})

	writeSection(bw, "bosun doctor", func(w io.Writer) {
		results := doctor.Run(rc.ctx, rc.repoRoot, doctor.DefaultChecks())
		doctor.WriteReport(w, rc.repoRoot, results)
	})

	writeSection(bw, "git status", func(w io.Writer) {
		writeGitOutput(w, rc.ctx, rc.repoRoot, "status")
	})

	writeSection(bw, "git worktree list --porcelain", func(w io.Writer) {
		writeGitOutput(w, rc.ctx, rc.repoRoot, "worktree", "list", "--porcelain")
	})

	writeSection(bw, ".bosun/config.json", func(w io.Writer) {
		writeConfigSection(w, rc.repoRoot, opts.redact)
	})

	writeSection(bw, "audit logs (.bosun/audit/)", func(w io.Writer) {
		writeAuditSection(w, rc.repoRoot, opts)
	})

	writeSection(bw, ".bosun/spawn-tree.json", func(w io.Writer) {
		writeFileSection(w, filepath.Join(rc.repoRoot, ".bosun", "spawn-tree.json"))
	})

	writeSection(bw, "state (.bosun/state/)", func(w io.Writer) {
		writeDirSection(w, filepath.Join(rc.repoRoot, ".bosun", "state"), opts.includeState())
	})

	writeSection(bw, "claims (.bosun/claims/)", func(w io.Writer) {
		writeDirSection(w, filepath.Join(rc.repoRoot, ".bosun", "claims"), opts.includeClaims())
	})

	writeSection(bw, "merges (.bosun/merges.log, last 50)", func(w io.Writer) {
		writeTailSection(w, filepath.Join(rc.repoRoot, ".bosun", "merges.log"), 50)
	})

	writeSection(bw, "environment", func(w io.Writer) {
		fmt.Fprintf(w, "GOOS:   %s\nGOARCH: %s\n", runtime.GOOS, runtime.GOARCH)
		fmt.Fprintf(w, "go:     %s\n", runtime.Version())
		writeCommandOutput(w, "uname -a", "uname", "-a")
		writeCommandOutput(w, "git --version", "git", "--version")
	})

	writeChecklist(bw)
}

// writeBanner emits a 60-char divider with the title centered between
// the rules. Used at the top of every section and at the report header.
func writeBanner(w io.Writer, title string) {
	const bar = "============================================================"
	fmt.Fprintln(w, bar)
	fmt.Fprintf(w, " %s\n", title)
	fmt.Fprintln(w, bar)
}

// writeSection emits a banner for title and then invokes body to fill
// the section. Always terminates with a trailing blank line so the next
// section is visually separated.
func writeSection(w io.Writer, title string, body func(io.Writer)) {
	writeBanner(w, title)
	body(w)
	fmt.Fprintln(w)
}

// writeGitOutput runs `git <args>` in dir and emits stdout under the
// caller's section, or an "(unavailable: ...)" line on failure.
func writeGitOutput(w io.Writer, ctx context.Context, dir string, args ...string) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(w, "(unavailable: %v)\n", err)
		return
	}
	if len(out) == 0 {
		fmt.Fprintln(w, "(empty)")
		return
	}
	_, _ = w.Write(out)
	if out[len(out)-1] != '\n' {
		fmt.Fprintln(w)
	}
}

// writeCommandOutput runs cmd with args and writes the (label, output)
// pair. Falls back to "(unavailable: ...)" on error, including when the
// binary itself isn't on PATH (the bundle still has to assemble on
// platforms where `uname` is missing, e.g. raw Windows).
func writeCommandOutput(w io.Writer, label, name string, args ...string) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		fmt.Fprintf(w, "%s: (unavailable: %v)\n", label, err)
		return
	}
	s := strings.TrimRight(string(out), "\n")
	if s == "" {
		fmt.Fprintf(w, "%s: (empty)\n", label)
		return
	}
	fmt.Fprintf(w, "%s:\n  %s\n", label, strings.ReplaceAll(s, "\n", "\n  "))
}

// writeConfigSection prints .bosun/config.json verbatim, applying
// redaction to obvious secrets unless the operator opted out.
func writeConfigSection(w io.Writer, repoRoot string, redact bool) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".bosun", "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(w, "(no .bosun/config.json — bosun is using defaults)")
			return
		}
		fmt.Fprintf(w, "(unavailable: %v)\n", err)
		return
	}
	body := string(data)
	if redact {
		body = debug.Redact(body)
	}
	_, _ = fmt.Fprint(w, body)
	if !strings.HasSuffix(body, "\n") {
		fmt.Fprintln(w)
	}
}

// writeAuditSection summarizes each .bosun/audit/*.log: by default the
// last 10 entries (with redaction applied if enabled), or the full file
// when --include audit is set.
func writeAuditSection(w io.Writer, repoRoot string, opts debugOptions) {
	auditDir := filepath.Join(repoRoot, ".bosun", "audit")
	entries, err := os.ReadDir(auditDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(w, "(no .bosun/audit/ directory — no audit events recorded)")
			return
		}
		fmt.Fprintf(w, "(unavailable: %v)\n", err)
		return
	}
	var logs []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			logs = append(logs, e.Name())
		}
	}
	if len(logs) == 0 {
		fmt.Fprintln(w, "(audit directory empty)")
		return
	}
	sort.Strings(logs)
	for _, name := range logs {
		fmt.Fprintf(w, "--- %s ---\n", name)
		path := filepath.Join(auditDir, name)
		if opts.includeAudit() {
			writeFileSectionWithRedact(w, path, opts.redact)
		} else {
			writeLastNLinesRedact(w, path, 10, opts.redact)
		}
		fmt.Fprintln(w)
	}
}

// writeFileSection emits the file's full contents under the caller's
// section, or "(missing)" / "(unavailable: ...)" when the file isn't
// there or can't be read.
func writeFileSection(w io.Writer, path string) {
	writeFileSectionWithRedact(w, path, false)
}

func writeFileSectionWithRedact(w io.Writer, path string, redact bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(w, "(missing)")
			return
		}
		fmt.Fprintf(w, "(unavailable: %v)\n", err)
		return
	}
	body := string(data)
	if redact {
		body = debug.Redact(body)
	}
	if body == "" {
		fmt.Fprintln(w, "(empty)")
		return
	}
	_, _ = fmt.Fprint(w, body)
	if !strings.HasSuffix(body, "\n") {
		fmt.Fprintln(w)
	}
}

// writeDirSection lists every regular file in dir with its size and
// mtime. When expand is true, each file's contents follow its header.
// The size-only default is what keeps a state-heavy repo from blowing
// up the bundle — the brief is explicit that state/claims default to
// summaries, not full content.
func writeDirSection(w io.Writer, dir string, expand bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(w, "(missing)")
			return
		}
		fmt.Fprintf(w, "(unavailable: %v)\n", err)
		return
	}
	var files []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e)
		}
	}
	if len(files) == 0 {
		fmt.Fprintln(w, "(empty)")
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	for _, e := range files {
		info, err := e.Info()
		if err != nil {
			fmt.Fprintf(w, "  %s  (stat failed: %v)\n", e.Name(), err)
			continue
		}
		fmt.Fprintf(w, "  %s  %d bytes  mtime=%s\n",
			e.Name(), info.Size(), info.ModTime().UTC().Format(time.RFC3339))
	}
	if !expand {
		return
	}
	for _, e := range files {
		path := filepath.Join(dir, e.Name())
		fmt.Fprintf(w, "--- %s ---\n", e.Name())
		writeFileSection(w, path)
	}
}

// writeTailSection emits the last n lines of path. Used for the
// merges.log section where the operator wants recent activity, not the
// full history.
func writeTailSection(w io.Writer, path string, n int) {
	writeLastNLinesRedact(w, path, n, false)
}

// writeLastNLinesRedact reads path, keeps the last n non-empty lines,
// applies redaction if requested, and emits them. Missing files render
// "(missing)" so the section is still labeled.
func writeLastNLinesRedact(w io.Writer, path string, n int, redact bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(w, "(missing)")
			return
		}
		fmt.Fprintf(w, "(unavailable: %v)\n", err)
		return
	}
	lines := splitNonEmptyLines(data)
	if len(lines) == 0 {
		fmt.Fprintln(w, "(empty)")
		return
	}
	if len(lines) > n {
		fmt.Fprintf(w, "(%d entries total; showing last %d)\n", len(lines), n)
		lines = lines[len(lines)-n:]
	}
	for _, line := range lines {
		if redact {
			line = debug.Redact(line)
		}
		fmt.Fprintln(w, line)
	}
}

// splitNonEmptyLines returns the non-blank lines of b. Used for the
// tail rendering so a trailing newline doesn't waste one of the n
// slots on an empty string.
func splitNonEmptyLines(b []byte) []string {
	var out []string
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			continue
		}
		out = append(out, string(line))
	}
	return out
}

// writeChecklist is the final section the bundle MUST end with — the
// brief is explicit: it's the operator's "did you skim this before
// pasting it into a public issue tracker?" gate.
func writeChecklist(w io.Writer) {
	writeBanner(w, "BEFORE SHARING THIS FILE")
	fmt.Fprintln(w, "- [ ] Skim for any secrets the auto-redaction missed")
	fmt.Fprintln(w, "- [ ] Skim for personal paths (/Users/<name>/...)")
	fmt.Fprintln(w, "- [ ] Confirm you want to share this publicly (it'll be in a")
	fmt.Fprintln(w, "      GitHub issue or email thread)")
}
