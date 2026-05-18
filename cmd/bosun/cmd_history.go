package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jasondillingham/bosun/internal/history"
	"github.com/spf13/cobra"
)

func newHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Inspect archived session records under .bosun/history/",
		Long: `Browse the .bosun/history/ archive — the metadata bosun captures just
before cleanup / remove / merge wipe a session's worktree, claims, and
brief. Use this to answer "what did session-2 do last week" after the
session itself is long gone.

Archives are gitignored (they live under .bosun/) and retained
indefinitely; use ` + "`bosun history prune --older-than 30d`" + ` to trim.`,
	}
	cmd.AddCommand(
		newHistoryListCmd(),
		newHistoryShowCmd(),
		newHistoryGrepCmd(),
		newHistoryPruneCmd(),
	)
	cmd.GroupID = "wiring"
	return cmd
}

func newHistoryListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List every archived session, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHistoryList()
		},
	}
}

func newHistoryShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <label-or-timestamp>",
		Short: "Print one archive's metadata and the contents of its files",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHistoryShow(args[0])
		},
	}
}

func newHistoryGrepCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "grep <pattern>",
		Short: "Search every archive's text contents for a regex pattern",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHistoryGrep(args[0])
		},
	}
}

func newHistoryPruneCmd() *cobra.Command {
	var olderThan string
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete archives older than the given age (e.g. 30d, 12h, 2w)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHistoryPrune(olderThan)
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "", "delete archives older than this duration (e.g. 30d, 12h, 2w)")
	_ = cmd.MarkFlagRequired("older-than")
	return cmd
}

func runHistoryList() error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	entries, err := history.List(rc.repoRoot)
	if err != nil {
		return internalErr("list history", err)
	}
	if len(entries) == 0 {
		println("bosun: no archived history yet")
		return nil
	}
	for _, e := range entries {
		reason := ""
		merge := ""
		if e.Metadata != nil {
			reason = e.Metadata.EndReason
			if e.Metadata.Detail != "" {
				reason = reason + " (" + e.Metadata.Detail + ")"
			}
			if e.Metadata.MergeSHA != "" {
				merge = " merge=" + shortSHA(e.Metadata.MergeSHA)
			}
		}
		printf("%s  %s  %s%s\n",
			e.Timestamp.Format("2006-01-02 15:04:05Z"),
			padLabel(e.Label, 16),
			reason,
			merge,
		)
	}
	return nil
}

func runHistoryShow(identifier string) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	entry, candidates, err := history.Lookup(rc.repoRoot, identifier)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return userErr("no archive matching %q", identifier)
		}
		if len(candidates) > 0 {
			return userErr("%v — disambiguate with the full directory name:\n  %s",
				err, strings.Join(candidates, "\n  "))
		}
		return internalErr("lookup history", err)
	}

	printf("archive: %s\n", entry.DirName)
	printf("path:    %s\n", entry.Path)
	if entry.Metadata != nil {
		m := entry.Metadata
		printf("label:   %s\n", m.Label)
		if m.Branch != "" {
			printf("branch:  %s\n", m.Branch)
		}
		printf("ended:   %s\n", m.EndedAt.Format("2006-01-02 15:04:05Z"))
		if !m.StartedAt.IsZero() {
			printf("started: %s\n", m.StartedAt.Format("2006-01-02 15:04:05Z"))
		}
		printf("reason:  %s\n", m.EndReason)
		if m.Detail != "" {
			printf("detail:  %s\n", m.Detail)
		}
		if m.MergeSHA != "" {
			printf("merge:   %s\n", m.MergeSHA)
		}
	}

	// Then print each file's contents in a stable order.
	for _, name := range []string{"brief.md", "claims.json", "commits.log", "merged.txt"} {
		path := filepath.Join(entry.Path, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		printf("\n----- %s -----\n", name)
		body := string(data)
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		printf("%s", body)
	}
	return nil
}

func runHistoryGrep(pattern string) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	hits, err := history.Grep(rc.ctx, rc.repoRoot, pattern)
	if err != nil {
		return userErr("grep: %v", err)
	}
	if len(hits) == 0 {
		// Exit 0 with no output — matches grep convention well enough
		// and keeps `bosun history grep | wc -l` honest.
		return nil
	}
	// Sort for stable output (ripgrep is already ordered per-file but
	// the Go fallback sorts in its own order; normalize here).
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].DirName != hits[j].DirName {
			return hits[i].DirName < hits[j].DirName
		}
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		return hits[i].Line < hits[j].Line
	})
	for _, h := range hits {
		printf("%s/%s:%d:%s\n", h.DirName, h.File, h.Line, h.Text)
	}
	return nil
}

func runHistoryPrune(olderThan string) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	dur, err := history.ParseDuration(olderThan)
	if err != nil {
		return userErr("--older-than: %v", err)
	}
	deleted, err := history.Prune(rc.repoRoot, dur)
	if err != nil {
		return internalErr("prune history", err)
	}
	if len(deleted) == 0 {
		printf("bosun: no archives older than %s\n", olderThan)
		return nil
	}
	for _, name := range deleted {
		printf("removed %s\n", name)
	}
	printf("bosun: pruned %d archive(s)\n", len(deleted))
	return nil
}

func padLabel(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
