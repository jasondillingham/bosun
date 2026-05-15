package tui

import (
	"strings"
	"testing"
)

func TestColorize_Disabled(t *testing.T) {
	if got := Colorize("hi", Green(), false); got != "hi" {
		t.Fatalf("disabled = %q, want hi", got)
	}
}

func TestColorize_Enabled(t *testing.T) {
	got := Colorize("hi", Green(), true)
	if !strings.HasPrefix(got, "\x1b[32m") || !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("enabled = %q, want ANSI green wrap", got)
	}
}

func TestShouldColor_NoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if ShouldColor(false) {
		t.Fatal("ShouldColor with NO_COLOR set = true, want false")
	}
}

func TestShouldColor_Flag(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if ShouldColor(true) {
		t.Fatal("ShouldColor(true) = true, want false")
	}
}

func TestColorAccessors(t *testing.T) {
	if Green() == "" || Yellow() == "" || Red() == "" || Dim() == "" {
		t.Fatal("color accessor returned empty string")
	}
}
