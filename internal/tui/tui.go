// Package tui provides helpers for TTY detection and color enablement.
package tui

import (
	"os"

	"golang.org/x/term"
)

// IsTTY reports whether stdout is a TTY.
func IsTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// ShouldColor reports whether output should be colorized. Honors --no-color
// (passed via the noColor argument) and the NO_COLOR env variable.
func ShouldColor(noColor bool) bool {
	if noColor {
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	return IsTTY()
}

// ANSI color helpers. These are intentionally minimal — we don't pull in a
// color library for v0.1.
const (
	reset  = "\x1b[0m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	red    = "\x1b[31m"
	dim    = "\x1b[2m"
)

// Colorize wraps s in the given color escape if color is enabled.
func Colorize(s, color string, enabled bool) string {
	if !enabled || color == "" {
		return s
	}
	return color + s + reset
}

// Named colors exported for callers that want to pick.
func Green() string  { return green }
func Yellow() string { return yellow }
func Red() string    { return red }
func Dim() string    { return dim }
