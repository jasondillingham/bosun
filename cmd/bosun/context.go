package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jasondillingham/bosun/internal/claims"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/state"
)

// runCtx bundles the repo-derived state that almost every command needs.
type runCtx struct {
	ctx      context.Context
	git      *git.Client
	cfg      config.Config
	repoRoot string
	claims   *claims.Store
	state    *state.Store
}

// loadCtx finds the repo root, reads optional config, and returns a runCtx.
func loadCtx() (*runCtx, error) {
	ctx := context.Background()
	c := git.New()
	cwd, err := os.Getwd()
	if err != nil {
		return nil, internalErr("getwd", err)
	}
	root, err := c.RepoRoot(ctx, cwd)
	if err != nil {
		return nil, userErr("not inside a git repository (cwd=%s)", cwd)
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, userErr("load config: %v", err)
	}
	return &runCtx{
		ctx:      ctx,
		git:      c,
		cfg:      cfg,
		repoRoot: root,
		claims:   claims.NewStore(root),
		state:    state.NewStore(root),
	}, nil
}

// printf is a convenience that writes to stdout via fmt.Fprintf.
func printf(format string, args ...any) {
	fmt.Fprintf(os.Stdout, format, args...)
}

// println writes to stdout.
func println(s string) {
	fmt.Fprintln(os.Stdout, s)
}
